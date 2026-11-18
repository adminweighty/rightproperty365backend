package app

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/mail"
)

type countryBody struct {
	ID           string `json:"id"`
	Code         string `json:"code"`
	Name         string `json:"name"`
	CurrencyCode string `json:"currency_code"`
	Timezone     string `json:"timezone"`
	Status       string `json:"status"`
	ListingCount *int   `json:"listing_count"`
	HasListings  *bool  `json:"has_listings"`
}

func (e *env) countries(query string) []countryBody {
	e.t.Helper()
	rec := e.get("/v1/countries" + query)
	wantStatus(e.t, rec, http.StatusOK)
	return decode[struct {
		Data []countryBody `json:"data"`
	}](e.t, rec).Data
}

func countryCodes(list []countryBody) string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		out = append(out, c.Code)
	}
	return strings.Join(out, ",")
}

// The platform is global: every ISO country is listed and usable, and
// /v1/markets is the same data under its older name.
func TestCountriesAreGlobal(t *testing.T) {
	e := newEnv(t)
	all := e.countries("")
	if len(all) != markets.CountryCount || len(all) != len(markets.Seed()) {
		t.Fatalf("%d countries, want %d", len(all), markets.CountryCount)
	}
	byCode := map[string]countryBody{}
	for _, c := range all {
		byCode[c.Code] = c
		if c.Status != "ACTIVE" || c.ListingCount == nil || c.HasListings == nil || *c.HasListings != (*c.ListingCount > 0) || c.ID == "" {
			t.Fatalf("country %+v", c)
		}
	}
	for code, want := range map[string][3]string{
		"GB": {"United Kingdom", "GBP", "Europe/London"}, "US": {"United States", "USD", "America/New_York"},
		"AE": {"United Arab Emirates", "AED", "Asia/Dubai"}, "ZA": {"South Africa", "ZAR", "Africa/Johannesburg"},
		"IN": {"India", "INR", "Asia/Kolkata"}, "AU": {"Australia", "AUD", "Australia/Sydney"},
		"MU": {"Mauritius", "MUR", "Indian/Mauritius"}, "KE": {"Kenya", "KES", "Africa/Nairobi"},
	} {
		c := byCode[code]
		if got := [3]string{c.Name, c.CurrencyCode, c.Timezone}; got != want {
			t.Fatalf("%s = %v, want %v", code, got, want)
		}
	}
	if *byCode["ZW"].ListingCount != 21 || *byCode["GB"].ListingCount != 3 || *byCode["MU"].ListingCount != 1 || *byCode["FR"].ListingCount != 0 || *byCode["FR"].HasListings {
		t.Fatalf("counts: ZW %d GB %d MU %d FR %d", *byCode["ZW"].ListingCount, *byCode["GB"].ListingCount, *byCode["MU"].ListingCount, *byCode["FR"].ListingCount)
	}
	// The older name returns the same bytes.
	if a, b := e.get("/v1/markets").Body.String(), e.get("/v1/countries").Body.String(); a != b {
		t.Fatal("/v1/markets and /v1/countries differ")
	}

	// What the public is shown: the countries with listings, most first.
	if got := countryCodes(e.countries("?has_listings=true")); got != "ZW,GB,MU" {
		t.Fatalf("has_listings=true = %s", got)
	}
	if got := e.countries("?has_listings=false"); len(got) != markets.CountryCount {
		t.Fatalf("has_listings=false returned %d", len(got))
	}
	// An expired or unpublished listing does not count: pause the one in Mauritius.
	for _, r := range e.seeded() {
		if r.MarketCode == devseed.MarketMauritius {
			r.Status = "PAUSED"
			e.listings.Put(r)
		}
	}
	if got := countryCodes(e.countries("?has_listings=true")); got != "ZW,GB" {
		t.Fatalf("has_listings=true after pausing = %s", got)
	}

	// q: a name or a code, without case or diacritics.
	for q, want := range map[string]string{
		"king": "GB", "KING": "GB", "gb": "GB", "reunion": "RE", "R%C3%A9union": "RE", "cote+d": "CI", "zzzz": "",
		"united": "GB,AE,US,UM",
	} {
		if got := countryCodes(e.countries("?q=" + q)); got != want {
			t.Fatalf("q=%s = %s, want %s", q, got, want)
		}
	}
	if got := countryCodes(e.countries("?q=united&has_listings=true")); got != "GB" {
		t.Fatalf("q and has_listings = %s", got)
	}
	for _, bad := range []string{"?has_listings=yes", "?q=" + strings.Repeat("a", 101)} {
		wantError(t, e.get("/v1/countries"+bad), http.StatusBadRequest, httpx.CodeValidationFailed)
	}

	// Cacheable like the reference data, with an ETag.
	rec := e.get("/v1/countries?has_listings=true")
	etag := rec.Header().Get("ETag")
	if rec.Header().Get("Cache-Control") != "public, max-age=60, stale-while-revalidate=300" || etag == "" {
		t.Fatalf("cache headers: %v", rec.Header())
	}
	again := e.get("/v1/countries?has_listings=true", func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatalf("revalidation: %d", again.Code)
	}

	// Reference data can be asked for any country; most have no places yet.
	rec = e.get("/v1/reference/cities?market=MU")
	wantStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("cities of MU: %s", rec.Body.String())
	}
	wantError(t, e.get("/v1/reference/cities?market=XX"), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// The sample listing in a country without reference places: free-text city
// and region, in the country facet, found by q, and served by slug.
func TestFreeTextLocationListing(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/listings/beach-villa-grand-baie")
	wantStatus(t, rec, http.StatusOK)
	body := rec.Body.String()
	for _, want := range []string{
		`"city":{"code":null,"label":"Grand Baie"}`, `"suburb":null`, `"region":{"code":null,"label":"Rivière du Rempart"}`,
		`"country":{"code":"MU","name":"Mauritius"}`, `"market_code":"MU"`, `"price":{"amount":"8500.00","currency":"MUR"}`, `"price_period":"night"`,
		`"max_guests":6,"min_nights":3`, `"reference":"RP-MU-10001"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("listing lacks %s:\n%s", want, body)
		}
	}
	l := decode[struct {
		Listing listings.Listing `json:"listing"`
	}](t, rec).Listing
	if l.Advertiser.Agency == nil || l.Advertiser.Agency.Slug != "sample-falls-stays" || l.Advertiser.Agency.Kind != "hosts" {
		t.Fatalf("advertiser = %+v", l.Advertiser)
	}

	mu := e.search("?country=MU")
	if mu.Total != 1 || mu.Items[0].Slug != "beach-villa-grand-baie" || mu.Facets.Price == nil || mu.Facets.Price.Currency != "MUR" ||
		*mu.Facets.Price.Min != "8500.00" || len(mu.Facets.City) != 0 || len(mu.Facets.Suburb) != 0 || len(mu.Facets.Region) != 0 {
		t.Fatalf("country=MU: %+v", mu)
	}
	if got := facet(e.search("").Facets.Country); got != "MU=1,GB=3,ZW=21" {
		t.Fatalf("country facet = %s", got)
	}
	for _, q := range []string{"?q=grand+baie", "?q=rempart", "?q=mauritius+villa"} {
		got := e.search(q)
		if q == "?q=mauritius+villa" { // the summary names the country
			if got.Total != 1 {
				t.Fatalf("%s: %d", q, got.Total)
			}
			continue
		}
		if got.Total != 1 || got.Items[0].Slug != "beach-villa-grand-baie" {
			t.Fatalf("%s: %d", q, got.Total)
		}
	}
	// A free-text city is not a filter code, and a hierarchy check applies
	// only to codes.
	env := wantError(t, e.get("/v1/listings?city=grand-baie"), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["city"] == "" {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	env = wantError(t, e.get("/v1/listings?country=MU&city=harare"), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["city"] != "is not a city of that country" {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	// Suggestions: the country, because it has listings.
	if body := e.get("/v1/listings/suggest?q=mauri").Body.String(); !strings.Contains(body, `{"type":"country","label":"Mauritius","value":"MU"}`) {
		t.Fatalf("suggest mauri: %s", body)
	}
	if body := e.get("/v1/listings/suggest?q=franc").Body.String(); strings.Contains(body, `"type":"country"`) {
		t.Fatalf("a country without listings was suggested: %s", body)
	}
	// A request for it reaches its owner's inbox, as for any listing.
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(24)), http.StatusCreated)
	if got := e.inboxListings("sample-falls-stays@example.test"); len(got) != 1 || got[0] != 24 {
		t.Fatalf("the host's inbox = %v", got)
	}
}

// Any enabled country is a valid filter; the currency of a search is never
// guessed.
func TestCountryFilterAndCurrencyRule(t *testing.T) {
	e := newEnv(t)
	fr := e.search("?country=FR")
	if fr.Total != 0 || len(fr.Items) != 0 || fr.Facets.Price == nil || fr.Facets.Price.Currency != "EUR" || fr.Facets.Price.Min != nil || len(fr.Facets.Currencies) != 0 {
		t.Fatalf("country=FR: %+v", fr)
	}
	if got := e.search("?country=fr&min_price=100"); got.Total != 0 || got.Facets.Price.Currency != "EUR" {
		t.Fatalf("country=fr&min_price: %+v", got.Facets.Price)
	}
	env := wantError(t, e.get("/v1/listings?country=XX"), http.StatusBadRequest, httpx.CodeValidationFailed)
	if !strings.Contains(env.Error.Fields["country"], "not a known country") {
		t.Fatalf("country=XX: %v", env.Error.Fields)
	}

	// No country, no currency, results in three currencies.
	rec := e.get("/v1/listings")
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"price":null,"currencies":[{"code":"USD","count":21},{"code":"GBP","count":3},{"code":"MUR","count":1}]`) {
		t.Fatalf("mixed-currency facets:\n%s", body[strings.Index(body, `"facets"`):])
	}
	for _, q := range []string{"?min_price=1000", "?max_price=2000", "?min_price=1&max_price=2&deal=rent"} {
		env := wantError(t, e.get("/v1/listings"+q), http.StatusBadRequest, httpx.CodeValidationFailed)
		if len(env.Error.Fields) != 1 || env.Error.Fields["currency"] != "Choose a currency or a country to filter by price." {
			t.Fatalf("%s: %v", q, env.Error.Fields)
		}
	}
	// One currency among the results: used without asking.
	london := e.search("?city=london&max_price=2000")
	if london.Total != 1 || london.Facets.Price.Currency != "GBP" || london.Items[0].Price.Currency != "GBP" {
		t.Fatalf("city=london&max_price=2000: %+v", london)
	}
	sales := e.search("?deal=sale&category=land")
	if sales.Facets.Price == nil || sales.Facets.Price.Currency != "USD" || len(sales.Facets.Currencies) != 1 {
		t.Fatalf("one currency: %+v %+v", sales.Facets.Price, sales.Facets.Currencies)
	}
	// A country or a currency settles it.
	if got := e.search("?country=ZW&min_price=1000"); got.Facets.Price.Currency != "USD" || got.Total == 0 {
		t.Fatalf("country=ZW&min_price=1000: %+v", got.Facets.Price)
	}
	if got := e.search("?currency=MUR&min_price=1000"); got.Total != 1 || got.Facets.Price.Currency != "MUR" {
		t.Fatalf("currency=MUR: %d", got.Total)
	}
	// The web's 18 listings alone share one currency: nothing changes there.
	web := newWebEnv(t)
	if got := web.search("?max_price=600&deal=rent"); got.Facets.Price == nil || got.Facets.Price.Currency != "USD" || got.Total == 0 {
		t.Fatalf("single-currency data set: %+v", got.Facets.Price)
	}
}

// A switched-off country is not listed and not accepted.
func TestDisabledCountry(t *testing.T) {
	e := newEnv(t)
	if !e.marketStore.SetEnabled("FR", false) {
		t.Fatal("FR is not a country")
	}
	if got := e.countries(""); len(got) != markets.CountryCount-1 || strings.Contains(","+countryCodes(got)+",", ",FR,") {
		t.Fatalf("%d countries listed", len(got))
	}
	wantError(t, e.get("/v1/listings?country=FR"), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.get("/v1/directory/agencies?country=FR"), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// Directories take any country: the profile's own.
func TestDirectoryCountryFilterForAnyCountry(t *testing.T) {
	e := newEnv(t)
	for path, want := range map[string]int{
		"hosts?country=GB": 1, "hosts?country=gb": 1, "agencies?country=GB": 1, "agencies?country=ZW": 5,
		"agencies?country=FR": 0, "hosts?country=MU": 0, "solicitors?market=ZW": 3, "agencies?country=KE": 0,
	} {
		got := decode[directoryPage](t, e.get("/v1/directory/"+path))
		if got.Total != want {
			t.Fatalf("%s: %d, want %d", path, got.Total, want)
		}
		for _, p := range got.Items {
			if p.Country == nil || !strings.EqualFold(p.Country.Code, path[strings.Index(path, "=")+1:]) {
				t.Fatalf("%s returned %s in %+v", path, p.Slug, p.Country)
			}
		}
	}
	for _, bad := range []string{"agencies?country=XX", "agencies?country=ZW&market=GB", "agencies?country=GB&city=harare", "agencies?country=ZW&region=england"} {
		wantError(t, e.get("/v1/directory/"+bad), http.StatusBadRequest, httpx.CodeValidationFailed)
	}
}

// A business in a country without reference cities: its city is free text;
// the application belongs to that country (which scopes a market
// administrator) and approval creates a profile there.
func TestOnboardingInAnyCountry(t *testing.T) {
	e := newEnv(t)
	c := e.selfRegister("host.mu@example.com", "HOST")
	save := func(body obj) applicationBody {
		t.Helper()
		rec := e.put("/v1/me/onboarding/HOST", body, browser(c))
		wantStatus(t, rec, http.StatusOK)
		return decode[oneApplication](t, rec).Application
	}
	refuse := func(body obj, field string) {
		t.Helper()
		env := wantError(t, e.put("/v1/me/onboarding/HOST", body, browser(c)), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[field] == "" {
			t.Fatalf("%v: fields = %v, want %s", body, env.Error.Fields, field)
		}
	}
	app := save(obj{"business_name": "Sample Villas Mauritius", "description": "Holiday villas.", "contact_phone": "+23052500000"})
	if got := strings.Join(app.Missing.Fields, ","); got != "country,city" {
		t.Fatalf("missing = %s", got)
	}
	refuse(obj{"country": "XX"}, "country")
	refuse(obj{"country": "Mauritius"}, "country")
	app = save(obj{"country": "mu"})
	if app.Details["country"] != "MU" || strings.Join(app.Missing.Fields, ",") != "city_name" {
		t.Fatalf("after country: %v, missing %v", app.Details["country"], app.Missing.Fields)
	}
	// No reference cities in Mauritius: a code is refused, free text is taken.
	refuse(obj{"country": "MU", "city": "harare"}, "city")
	refuse(obj{"city_name": "<b>Grand Baie</b>"}, "city_name")
	refuse(obj{"city_name": strings.Repeat("a", 81)}, "city_name")
	refuse(obj{"city": "london", "city_name": "Grand Baie"}, "city_name")
	app = save(obj{"city_name": "  Grand Baie "})
	if app.Details["city_name"] != "Grand Baie" || app.Details["city"] != nil || len(app.Missing.Fields) != 0 {
		t.Fatalf("details = %v, missing %v", app.Details, app.Missing.Fields)
	}
	// A country that has reference cities takes a code, not free text...
	refuse(obj{"country": "GB", "city_name": "Oxford"}, "city_name")
	// ...and changing the country drops a city that no longer applies.
	app = save(obj{"country": "GB"})
	if app.Details["country"] != "GB" || app.Details["city_name"] != nil || strings.Join(app.Missing.Fields, ",") != "city" {
		t.Fatalf("after moving to GB: %v, missing %v", app.Details, app.Missing.Fields)
	}
	refuse(obj{"country": "GB", "city": "harare"}, "city") // a city of another country
	app = save(obj{"country": "GB", "city": "london"})
	if app.Details["city"] != "london" || app.Details["country"] != "GB" {
		t.Fatalf("details = %v", app.Details)
	}
	// A city alone still names its country (clients that only know `city`).
	app = save(obj{"city": "bulawayo"})
	if app.Details["country"] != "ZW" || app.Details["city"] != "bulawayo" {
		t.Fatalf("city alone: %v", app.Details)
	}
	// Back to Mauritius, submit and approve.
	app = save(obj{"country": "MU", "city_name": "Grand Baie"})
	if app.Details["country"] != "MU" || app.Details["city"] != nil || app.Details["city_name"] != "Grand Baie" {
		t.Fatalf("details = %v", app.Details)
	}
	for _, doc := range requiredDocuments("HOST") {
		wantStatus(t, e.upload(c, "HOST", doc, "scan.pdf", testPDF()), http.StatusCreated)
	}
	rec := e.post("/v1/me/onboarding/HOST/submit", nil, browser(c))
	wantStatus(t, rec, http.StatusOK)
	id := decode[oneApplication](t, rec).Application.ID

	// Market administrators are scoped by the application's country.
	zw, mu := e.platformAdmin("zw.admin@example.com", "MARKET_ADMIN", "ZW"), e.platformAdmin("mu.admin@example.com", "MARKET_ADMIN", "MU")
	wantStatus(t, e.get("/v1/admin/onboarding/"+id, browser(zw)), http.StatusNotFound)
	rec = e.get("/v1/admin/onboarding/"+id, browser(mu))
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"market_code":"MU"`) || !strings.Contains(body, `"city_name":"Grand Baie"`) {
		t.Fatalf("admin view: %s", body)
	}
	out := e.approve(mu, id)
	if out.Created.Profile == nil {
		t.Fatal("no profile was created")
	}
	rec = e.get("/v1/directory/hosts/" + out.Created.Profile.Slug)
	wantStatus(t, rec, http.StatusOK)
	if body := rec.Body.String(); !strings.Contains(body, `"city":{"code":null,"label":"Grand Baie"}`) || !strings.Contains(body, `"region":null`) ||
		!strings.Contains(body, `"country":{"code":"MU","name":"Mauritius"}`) {
		t.Fatalf("profile: %s", body)
	}
	if got := decode[directoryPage](t, e.get("/v1/directory/hosts?country=MU")); got.Total != 1 || got.Items[0].Slug != out.Created.Profile.Slug {
		t.Fatalf("hosts in MU = %d", got.Total)
	}
	if got := decode[directoryPage](t, e.get("/v1/directory/hosts?q=grand+baie")); got.Total != 1 {
		t.Fatalf("hosts found by free-text city = %d", got.Total)
	}
	// The new organisation is in Mauritius.
	user, _ := e.me(c)
	if len(user.Memberships) != 1 || user.Memberships[0].MarketCode != "MU" {
		t.Fatalf("memberships = %+v", user.Memberships)
	}
}

// Every country's default zone is named in an email, by a name or by its
// offset, and an appointment for a listing abroad carries that zone.
func TestEveryCountryZoneIsNamed(t *testing.T) {
	at := time.Date(2027, 7, 13, 10, 0, 0, 0, time.UTC)
	for _, m := range markets.Seed() {
		got := mail.LocalDateTime(at, m.Timezone)
		if strings.Contains(got, "(Coordinated Universal Time)") != (m.Timezone == "UTC") || strings.HasSuffix(got, "()") {
			t.Fatalf("%s (%s): %q", m.Code, m.Timezone, got)
		}
	}
	e := newEnv(t)
	c := newCast(e)
	a := c.book("consumer@example.test", e.appointmentInput(24, 48*time.Hour))
	if a.Timezone != "Indian/Mauritius" {
		t.Fatalf("appointment for the Mauritius listing: timezone %s", a.Timezone)
	}
}

// Migration 0014 upserts exactly the countries of the Go table, so the
// memory store, cmd/seed and a migrated database agree.
func TestMigrationSeedsEveryCountry(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0014_global_countries.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, m := range markets.Seed() {
		if !strings.Contains(sql, markets.MigrationRow(m)+"\n") {
			t.Fatalf("migration 0014 lacks the row of %s: %s", m.Code, markets.MigrationRow(m))
		}
	}
	if n := strings.Count(sql, "INSERT INTO markets "); n != markets.CountryCount {
		t.Fatalf("migration 0014 inserts %d countries, the table has %d", n, markets.CountryCount)
	}
	for _, want := range []string{
		"ALTER TABLE markets ADD COLUMN enabled boolean NOT NULL DEFAULT true;",
		"ADD COLUMN city_name", "ADD COLUMN region_name", "ADD COLUMN country_code",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("migration 0014 lacks %q", want)
		}
	}
	// An upsert never switches a country back on, and never moves its ID.
	if strings.Contains(sql, "enabled = EXCLUDED") || strings.Contains(sql, "SET id =") || strings.Contains(sql, "DELETE FROM markets") {
		t.Fatal("migration 0014 must not re-enable, re-identify or delete a country")
	}
	// The sample listings fit the place rules of the migration.
	if err := devseed.CheckListings(); err != nil {
		t.Fatal(err)
	}
}
