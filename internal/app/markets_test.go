package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/mail"
)

func TestMarketsListTheUnitedKingdomAfterZimbabwe(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/markets")
	wantStatus(t, rec, http.StatusOK)
	got := decode[struct {
		Data []markets.Market `json:"data"`
	}](t, rec).Data
	var order []string
	for _, m := range got {
		order = append(order, m.Code+":"+m.Status)
	}
	if strings.Join(order, ",") != "ZW:ACTIVE,GB:ACTIVE,AE:PLANNED,BW:PLANNED,ZA:PLANNED" {
		t.Fatalf("markets = %v", order)
	}
	gb := got[1]
	if gb.Name != "United Kingdom" || gb.CurrencyCode != "GBP" || gb.Timezone != "Europe/London" || gb.ID != markets.UnitedKingdomID {
		t.Fatalf("GB = %+v", gb)
	}
}

func TestUnitedKingdomReferenceDataAndHierarchy(t *testing.T) {
	e := newEnv(t)
	type item struct {
		Code        string  `json:"code"`
		Label       string  `json:"label"`
		Description *string `json:"description"`
		MarketCode  *string `json:"market_code"`
		ParentCode  *string `json:"parent_code"`
	}
	set := func(path string) map[string]item {
		rec := e.get("/v1/reference/" + path)
		wantStatus(t, rec, http.StatusOK)
		out := map[string]item{}
		for _, it := range decode[struct {
			Items []item `json:"items"`
		}](t, rec).Items {
			out[it.Code] = it
		}
		return out
	}
	regions := set("regions?market=GB")
	if len(regions) != 4 || regions["england"].Label != "England" || regions["northern-ireland"].Label != "Northern Ireland" || regions["scotland"].Label == "" || regions["wales"].Label == "" {
		t.Fatalf("GB regions = %+v", regions)
	}
	cities := set("cities?market=GB")
	for city, region := range map[string]string{
		"london": "england", "manchester": "england", "birmingham": "england", "leeds": "england", "liverpool": "england",
		"bristol": "england", "edinburgh": "scotland", "glasgow": "scotland", "cardiff": "wales", "belfast": "northern-ireland",
	} {
		if c, ok := cities[city]; !ok || c.ParentCode == nil || *c.ParentCode != region || *c.MarketCode != "GB" {
			t.Fatalf("city %s = %+v, want region %s", city, c, region)
		}
	}
	if len(cities) != 10 {
		t.Fatalf("%d GB cities, want 10", len(cities))
	}
	suburbs := set("suburbs?market=GB")
	for _, code := range []string{"camden", "greenwich", "kensington", "shoreditch", "wimbledon"} {
		if s, ok := suburbs[code]; !ok || *s.ParentCode != "london" {
			t.Fatalf("suburb %s = %+v", code, s)
		}
	}
	if len(set("regions?market=ZW")) != 10 || len(set("cities?market=ZW")) != 4 {
		t.Fatal("the Zimbabwean sets must not gain the United Kingdom's rows")
	}

	// The hierarchy is validated across markets.
	for query, field := range map[string]string{
		"?country=GB&region=harare":       "region",
		"?country=ZW&region=england":      "region",
		"?country=GB&city=harare":         "city",
		"?region=scotland&city=london":    "city",
		"?country=ZW&suburb=camden":       "suburb",
		"?city=manchester&suburb=camden":  "suburb",
		"?region=wales&suburb=shoreditch": "suburb",
	} {
		env := wantError(t, e.get("/v1/listings"+query), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields[field]; !ok || len(env.Error.Fields) != 1 {
			t.Fatalf("%s: fields = %v, want only %q", query, env.Error.Fields, field)
		}
	}
	if got := e.search("?country=GB&region=england&city=london&suburb=kensington"); got.Total != 1 || got.Items[0].Reference != "RP-GB-10001" {
		t.Fatalf("the consistent UK chain returned %d", got.Total)
	}

	// Categories, short-stay types and price periods are reference data.
	categories := set("property_categories")
	short := categories["short-stay"]
	if short.Label != "Short stays" || short.Description == nil || *short.Description != devseed.ShortStayDescription {
		t.Fatalf("short-stay category = %+v", short)
	}
	for code, it := range categories {
		// No code or label is named after another company's trade mark.
		if strings.Contains(strings.ToLower(code+it.Label), "airbnb") {
			t.Fatalf("category %s is named after a trade mark", code)
		}
	}
	types := set("property_types")
	for code, label := range map[string]string{"holiday-home": "Holiday home", "serviced-apartment": "Serviced apartment", "guest-house": "Guest house", "private-room": "Private room", "lodge": "Lodge"} {
		if it := types[code]; it.Label != label || it.ParentCode == nil || *it.ParentCode != "short-stay" {
			t.Fatalf("property type %s = %+v", code, it)
		}
	}
	if types["house"].ParentCode == nil || *types["house"].ParentCode != "residential" {
		t.Fatal("existing property types must keep their category")
	}
	periods := set("price_periods")
	if len(periods) != 2 || periods["month"].Label != "per month" || periods["night"].Label != "per night" {
		t.Fatalf("price periods = %+v", periods)
	}
}

func TestUnitedKingdomSearchAndCurrencies(t *testing.T) {
	e := newEnv(t)
	gb := e.search("?country=GB")
	if gb.Total != 3 || gb.Facets.Price.Currency != "GBP" || gb.Facets.Price.Min == nil || *gb.Facets.Price.Min != "185.00" || *gb.Facets.Price.Max != "1250000.00" {
		t.Fatalf("country=GB: total %d, price facet %+v", gb.Total, gb.Facets.Price)
	}
	for _, l := range gb.Items {
		if l.Price.Currency != "GBP" || l.Country != (listings.Country{Code: "GB", Name: "United Kingdom"}) || l.MarketCode != "GB" ||
			!strings.HasPrefix(l.Reference, "RP-GB-1") || l.Region == nil || l.Region.Code != "england" ||
			l.Advertiser.Agency == nil || l.Advertiser.Agency.Slug != "sample-homes-london" || len(l.Photos) != 1 {
			t.Fatalf("GB listing %s: %+v", l.Slug, l)
		}
	}
	if len(gb.Facets.Region) != 1 || gb.Facets.Region[0].Code != "england" || gb.Facets.Region[0].Count != 3 {
		t.Fatalf("GB region facet = %+v", gb.Facets.Region)
	}
	// The price filter with a country uses that country's currency.
	if got := e.search("?country=GB&max_price=2000"); got.Total != 2 {
		t.Fatalf("country=GB&max_price=2000: %d, want the two GBP listings under 2000", got.Total)
	}
	if got := e.search("?country=GB&min_price=1&currency=USD"); got.Total != 0 {
		t.Fatalf("an explicit USD price filter matched %d GBP listings", got.Total)
	}

	// No country: the default is unchanged. Every listing is returned, each
	// with its own currency; the price facet and filter use USD only, and
	// never mix currencies.
	all := e.search("?page_size=48")
	currencies := map[string]int{}
	for _, l := range all.Items {
		currencies[l.Price.Currency]++
	}
	if all.Total != 23 || currencies["GBP"] != 3 || currencies["USD"] != 20 || all.Facets.Price.Currency != "USD" || *all.Facets.Price.Max == "1250000.00" {
		t.Fatalf("mixed results: total %d, currencies %v, price facet %+v", all.Total, currencies, all.Facets.Price)
	}
	for _, l := range e.search("?max_price=2000&page_size=48").Items {
		if l.Price.Currency != "USD" {
			t.Fatalf("the default price filter returned %s priced in %s", l.Slug, l.Price.Currency)
		}
	}
	if got := e.search("?currency=GBP&max_price=2000"); got.Total != 2 || got.Facets.Price.Currency != "GBP" {
		t.Fatalf("currency=GBP: %d", got.Total)
	}
	countries := facet(all.Facets.Country)
	if countries != "GB=3,ZW=20" {
		t.Fatalf("country facet = %s", countries)
	}
	// Directories and suggestions follow.
	if got := decode[directoryPage](t, e.get("/v1/directory/agencies?country=GB")); got.Total != 1 || got.Items[0].Slug != "sample-homes-london" ||
		got.Items[0].Region.Code != "england" || got.Items[0].Country.Name != "United Kingdom" || got.Items[0].ListingCount != 3 {
		t.Fatalf("GB agencies = %+v", got.Items)
	}
	if got := decode[directoryPage](t, e.get("/v1/directory/hosts?region=scotland")); got.Total != 1 || got.Items[0].Slug != "sample-stays-edinburgh" {
		t.Fatalf("hosts in Scotland = %d", got.Total)
	}
	if body := e.get("/v1/listings/suggest?q=united").Body.String(); !strings.Contains(body, `{"type":"country","label":"United Kingdom","value":"GB"}`) {
		t.Fatalf("suggest united: %s", body)
	}
}

func TestShortStays(t *testing.T) {
	e := newEnv(t)
	short := e.search("?category=short-stay")
	if short.Total != 3 {
		t.Fatalf("category=short-stay: %d, want 3", short.Total)
	}
	bySlug := map[string]listings.Listing{}
	for _, l := range short.Items {
		bySlug[l.Slug] = l
		if l.Category != (listings.Coded{Code: "short-stay", Label: "Short stays"}) || l.Deal.Code != "rent" || l.PricePeriod == nil || *l.PricePeriod != "night" ||
			l.MaxGuests == nil || l.MinNights == nil || l.PropertyType.Label == l.PropertyType.Code {
			t.Fatalf("short stay %s: %+v", l.Slug, l)
		}
	}
	falls, harare, london := bySlug["safari-lodge-victoria-falls"], bySlug["garden-guest-house-avondale"], bySlug["serviced-apartment-shoreditch"]
	if *falls.MaxGuests != 8 || *falls.MinNights != 2 || falls.Price != (listings.Money{Amount: "320.00", Currency: "USD"}) || falls.PropertyType.Code != "lodge" ||
		falls.Advertiser.Agency == nil || falls.Advertiser.Agency.Kind != "hosts" || falls.Advertiser.Agency.Slug != "sample-falls-stays" {
		t.Fatalf("Victoria Falls lodge: %+v", falls)
	}
	if *harare.MaxGuests != 4 || *harare.MinNights != 1 || harare.Advertiser.Agency.Slug != "sample-showrooms" || harare.PropertyType.Code != "guest-house" {
		t.Fatalf("Harare guest house: %+v", harare)
	}
	if *london.MaxGuests != 2 || london.Price.Currency != "GBP" || london.Country.Code != "GB" || london.PropertyType.Code != "serviced-apartment" {
		t.Fatalf("London serviced apartment: %+v", london)
	}

	// guests_min: short stays that sleep at least that many.
	for query, want := range map[string]int{"?guests_min=1": 3, "?guests_min=3": 2, "?guests_min=4": 2, "?guests_min=5": 1, "?guests_min=9": 0, "?category=short-stay&guests_min=4&country=ZW": 2, "?category=residential&guests_min=1": 0} {
		if got := e.search(query); got.Total != want {
			t.Fatalf("%s: %d, want %d", query, got.Total, want)
		}
	}
	for _, bad := range []string{"?guests_min=0", "?guests_min=101", "?guests_min=many"} {
		env := wantError(t, e.get("/v1/listings"+bad), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields["guests_min"]; !ok {
			t.Fatalf("%s: %v", bad, env.Error.Fields)
		}
	}

	// The brand name is not a category, but searching for it finds the short stays.
	for _, q := range []string{"airbnb", "Airbnb", "AIRBNB lodge"} {
		got := e.search("?q=" + strings.ReplaceAll(q, " ", "+"))
		want := 3
		if strings.Contains(q, "lodge") {
			want = 1
		}
		if got.Total != want {
			t.Fatalf("q=%s: %d, want %d", q, got.Total, want)
		}
		for _, l := range got.Items {
			if l.Category.Code != "short-stay" {
				t.Fatalf("q=%s returned %s", q, l.Slug)
			}
		}
	}
	if got := e.search("?q=airbnb&category=residential"); got.Total != 0 {
		t.Fatal("q=airbnb must not match other categories")
	}

	// One short stay by slug: the JSON carries the new fields; other listings carry nulls.
	one := e.get("/v1/listings/safari-lodge-victoria-falls").Body.String()
	for _, want := range []string{`"price_period":"night"`, `"max_guests":8`, `"min_nights":2`, `"category":{"code":"short-stay","label":"Short stays"}`, `"deal":{"code":"rent"`} {
		if !strings.Contains(one, want) {
			t.Fatalf("short-stay JSON lacks %s: %s", want, one)
		}
	}
	other := e.get("/v1/listings/" + devseed.Sample().Listings[0].Slug).Body.String()
	if !strings.Contains(other, `"max_guests":null,"min_nights":null`) {
		t.Fatalf("a listing that is not a short stay must carry null max_guests / min_nights: %s", other)
	}
	// The category facet offers it like the others.
	if got := facet(e.search("").Facets.Category); !strings.Contains(got, "short-stay=3") {
		t.Fatalf("category facet = %s", got)
	}
}

// A short stay must be deal rent priced per night; nothing else may be.
func TestShortStayRules(t *testing.T) {
	if err := devseed.CheckListings(); err != nil {
		t.Fatal(err)
	}
	two := 2
	valid := listings.Record{Slug: "x", Category: listings.CategoryShortStay, Deal: "rent", PricePeriod: "night", MaxGuests: &two, MinNights: &two}
	if err := listings.CheckRecord(valid); err != nil {
		t.Fatal(err)
	}
	zero := 0
	for name, mutate := range map[string]func(*listings.Record){
		"a short stay for sale":         func(r *listings.Record) { r.Deal = "sale" },
		"a short stay per month":        func(r *listings.Record) { r.PricePeriod = "month" },
		"a short stay without a period": func(r *listings.Record) { r.PricePeriod = "" },
		"no guests":                     func(r *listings.Record) { r.MaxGuests = &zero },
		"no nights":                     func(r *listings.Record) { r.MinNights = &zero },
		"a house per night":             func(r *listings.Record) { r.Category = "residential" },
		"an unknown period":             func(r *listings.Record) { r.PricePeriod = "week" },
		"guests on a house": func(r *listings.Record) {
			r.Category, r.PricePeriod = "residential", "month"
		},
	} {
		r := valid
		mutate(&r)
		if err := listings.CheckRecord(r); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := listings.CheckRecord(listings.Record{Slug: "y", Category: "residential", Deal: "rent", PricePeriod: "month"}); err != nil {
		t.Fatal(err)
	}
}

// Appointment emails name the United Kingdom's time zone by season.
func TestLondonTimeZoneNaming(t *testing.T) {
	winter := time.Date(2027, 1, 12, 10, 0, 0, 0, time.UTC)
	if got := mail.LocalDateTime(winter, "Europe/London"); got != "Tuesday 12 January 2027, 10:00 (Greenwich Mean Time)" {
		t.Fatalf("winter = %q", got)
	}
	summer := time.Date(2027, 7, 13, 10, 0, 0, 0, time.UTC)
	if got := mail.LocalDateTime(summer, "Europe/London"); got != "Tuesday 13 July 2027, 11:00 (British Summer Time)" {
		t.Fatalf("summer = %q", got)
	}
	if got := mail.LocalDateTime(summer, "Africa/Harare"); got != "Tuesday 13 July 2027, 12:00 (Central Africa Time)" {
		t.Fatalf("Harare = %q", got)
	}

	// An appointment for a UK listing carries the UK time zone.
	e := newEnv(t)
	c := newCast(e)
	a := c.book("consumer@example.test", e.appointmentInput(19, 48*time.Hour))
	if a.Timezone != "Europe/London" || a.With == nil || a.With.Slug != "sample-homes-london" {
		t.Fatalf("UK appointment: timezone %s, with %+v", a.Timezone, a.With)
	}
	if got := c.inboxIDs(devseed.SampleHomesLondonOwnerEmail, ""); len(got) != 1 || got[0] != a.ID {
		t.Fatalf("the UK agency owner sees %v", got)
	}
}
