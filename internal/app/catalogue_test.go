package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type listingPage struct {
	Items      []listings.Listing `json:"items"`
	Page       int                `json:"page"`
	PageSize   int                `json:"page_size"`
	Total      int                `json:"total"`
	TotalPages int                `json:"total_pages"`
	Facets     listings.Facets    `json:"facets"`
}

func (e *env) search(query string) listingPage {
	e.t.Helper()
	rec := e.get("/v1/listings" + query)
	wantStatus(e.t, rec, http.StatusOK)
	return decode[listingPage](e.t, rec)
}

func slugs(items []listings.Listing) string {
	out := make([]string, 0, len(items))
	for _, l := range items {
		out = append(out, l.Slug)
	}
	return strings.Join(out, ",")
}

func facet(counts []listings.FacetCount) string {
	out := make([]string, 0, len(counts))
	for _, c := range counts {
		out = append(out, fmt.Sprintf("%s=%d", c.Code, c.Count))
	}
	return strings.Join(out, ",")
}

func TestListingsDefaultPageAndPaging(t *testing.T) {
	e := newEnv(t)
	first := e.search("")
	if first.Total != 18 || first.Page != 1 || first.PageSize != 12 || first.TotalPages != 2 || len(first.Items) != 12 {
		t.Fatalf("default page: total=%d page=%d size=%d pages=%d items=%d", first.Total, first.Page, first.PageSize, first.TotalPages, len(first.Items))
	}
	// Newest first by default.
	for i := 1; i < len(first.Items); i++ {
		if first.Items[i].PublishedAt.After(first.Items[i-1].PublishedAt) {
			t.Fatalf("default order is not newest first at %d", i)
		}
	}
	second := e.search("?page=2")
	if len(second.Items) != 6 || second.Page != 2 || second.Total != 18 {
		t.Fatalf("page 2: %d items", len(second.Items))
	}
	seen := map[string]bool{}
	for _, l := range append(first.Items, second.Items...) {
		if seen[l.Slug] {
			t.Fatalf("listing %s appears on two pages", l.Slug)
		}
		seen[l.Slug] = true
	}
	if beyond := e.search("?page=3"); len(beyond.Items) != 0 || beyond.Total != 18 || beyond.TotalPages != 2 {
		t.Fatalf("a page past the end is empty, not an error: %+v", beyond.Page)
	}
	if got := strings.TrimSpace(e.get("/v1/listings?page=3").Body.String()); !strings.HasPrefix(got, `{"items":[],"page":3,"page_size":12,"total":18,"total_pages":2,"facets":{`) {
		t.Fatalf("body = %.120s", got)
	}
	if all := e.search("?page_size=48"); len(all.Items) != 18 || all.TotalPages != 1 {
		t.Fatalf("page_size=48: %d items", len(all.Items))
	}
	if one := e.search("?page_size=1&page=18"); len(one.Items) != 1 || one.TotalPages != 18 {
		t.Fatalf("page_size=1: %+v", one.TotalPages)
	}
	// Every sample listing is there with the web's slug, reference and price.
	for _, l := range devseed.Sample().Listings {
		if !seen[l.Slug] {
			t.Fatalf("sample listing %s is missing", l.Slug)
		}
	}
}

func TestListingFilters(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name, query string
		want        int
	}{
		{"deal sale", "?deal=sale", 11},
		{"deal rent", "?deal=rent", 7},
		{"category residential", "?category=residential", 13},
		{"category land", "?category=land", 3},
		{"category commercial", "?category=commercial", 2},
		{"property type office", "?property_type=office", 2},
		{"property type house", "?property_type=house", 7},
		{"city harare", "?city=harare", 12},
		{"city bulawayo", "?city=bulawayo", 3},
		{"suburb borrowdale", "?suburb=borrowdale", 1},
		{"featured", "?featured=true", 8},
		{"not featured", "?featured=false", 10},
		{"bedrooms_min 4", "?bedrooms_min=4", 5},
		{"bedrooms_min 1", "?bedrooms_min=1", 13},
		{"bedrooms_min 6", "?bedrooms_min=6", 0},
		{"feature borehole", "?features=borehole", 6},
		{"two features, repeated", "?features=borehole&features=solar-backup", 1},
		{"two features, comma separated", "?features=borehole,solar-backup", 1},
		{"min price", "?min_price=500000", 2},
		{"max price", "?max_price=1000", 3},
		{"price band", "?min_price=100000&max_price=300000", 5},
		{"price in another currency matches nothing", "?min_price=1&currency=ZAR", 0},
		{"q in the title", "?q=townhouse", 2},
		{"q in the suburb", "?q=borrowdale", 1},
		{"q in the city", "?q=mutare", 2},
		{"q two words, both must match", "?q=house+bulawayo", 2},
		{"q is case-insensitive", "?q=BORROWDALE", 1},
		{"q by reference", "?q=RP-ZW-10003", 1},
		{"q with no match", "?q=zzzz", 0},
		{"q wildcard characters are literal", "?q=%25", 0},
		{"combined", "?city=harare&deal=sale&category=residential", 5},
		{"combined with a price ceiling", "?city=harare&deal=sale&category=residential&max_price=300000", 3},
		{"combined to nothing", "?city=mutare&category=commercial", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			page := e.search(tt.query + "&page_size=48")
			if page.Total != tt.want || len(page.Items) != tt.want {
				t.Fatalf("%s: total=%d items=%d, want %d (%s)", tt.query, page.Total, len(page.Items), tt.want, slugs(page.Items))
			}
		})
	}

	// bathrooms_min is checked against the data rather than a fixed number.
	want := 0
	for _, l := range devseed.Sample().Listings {
		if l.Bathrooms != nil && *l.Bathrooms >= 3 {
			want++
		}
	}
	if got := e.search("?bathrooms_min=3").Total; got != want || want == 0 {
		t.Fatalf("bathrooms_min=3: %d, want %d", got, want)
	}
}

func TestListingSortOrders(t *testing.T) {
	e := newEnv(t)
	base := "?city=harare&deal=sale&category=residential"
	if got := slugs(e.search(base + "&sort=price_asc").Items); got != "three-bedroom-apartment-belgravia,three-bedroom-townhouse-greendale,four-bedroom-house-highlands,four-bedroom-family-home-borrowdale,five-bedroom-house-glen-lorne" {
		t.Fatalf("price_asc = %s", got)
	}
	if got := slugs(e.search(base + "&sort=price_desc").Items); !strings.HasPrefix(got, "five-bedroom-house-glen-lorne,four-bedroom-family-home-borrowdale,") {
		t.Fatalf("price_desc = %s", got)
	}
	newest := e.search(base + "&sort=newest").Items
	for i := 1; i < len(newest); i++ {
		if newest[i].PublishedAt.After(newest[i-1].PublishedAt) {
			t.Fatal("newest is not ordered by published_at descending")
		}
	}
	// Relevance: a title match outranks a description match; it is the default when q is given.
	rel := e.search("?q=borehole")
	if rel.Total < 2 {
		t.Fatalf("q=borehole matched %d", rel.Total)
	}
	if byDefault, explicit := slugs(rel.Items), slugs(e.search("?q=borehole&sort=relevance").Items); byDefault != explicit {
		t.Fatalf("relevance should be the default order with q:\n%s\n%s", byDefault, explicit)
	}
	// Relevance without q puts featured listings first.
	plain := e.search("?sort=relevance&page_size=48").Items
	for i := 0; i < 8; i++ {
		if !plain[i].Featured {
			t.Fatalf("relevance without q: position %d is not featured", i)
		}
	}
}

func TestListingFacets(t *testing.T) {
	e := newEnv(t)
	all := e.search("").Facets
	if got := facet(all.Deal); got != "sale=11,rent=7" {
		t.Fatalf("deal facet = %s", got)
	}
	if got := facet(all.Category); got != "commercial=2,land=3,residential=13" {
		t.Fatalf("category facet = %s", got)
	}
	if got := facet(all.City); got != "bulawayo=3,harare=12,mutare=2,victoria-falls=1" {
		t.Fatalf("city facet = %s", got)
	}
	if got := facet(all.Bedrooms); got != "1=2,2=1,3=5,4=3,5=2" {
		t.Fatalf("bedrooms facet = %s", got)
	}
	if all.Bedrooms[0].Label != "1 bedroom" || all.Bedrooms[1].Label != "2 bedrooms" || all.Deal[0].Label != "For sale" {
		t.Fatalf("facet labels: %+v %+v", all.Bedrooms[:2], all.Deal)
	}
	if len(all.Suburb) != 18 || len(all.PropertyType) != 7 || len(all.Features) != 59 {
		t.Fatalf("facet sizes: suburb=%d type=%d features=%d", len(all.Suburb), len(all.PropertyType), len(all.Features))
	}
	// Features are ordered by how many listings have them.
	if all.Features[0].Code != "borehole" || all.Features[0].Count != 6 || all.Features[0].Label != "Borehole" {
		t.Fatalf("top feature = %+v", all.Features[0])
	}
	if all.Price.Currency != "USD" || all.Price.Min == nil || *all.Price.Min != "600.00" || *all.Price.Max != "640000.00" {
		t.Fatalf("price facet = %+v", all.Price)
	}

	// A facet counts the listings matching every OTHER filter, so the chosen
	// value's alternatives stay visible.
	sale := e.search("?deal=sale").Facets
	if got := facet(sale.Deal); got != "sale=11,rent=7" {
		t.Fatalf("deal facet with deal=sale = %s", got)
	}
	if got := facet(sale.Category); got != "land=3,residential=8" {
		t.Fatalf("category facet with deal=sale = %s", got)
	}
	harare := e.search("?city=harare&deal=rent").Facets
	if got := facet(harare.City); !strings.Contains(got, "harare=") || !strings.Contains(got, "bulawayo=") {
		t.Fatalf("city facet with city=harare&deal=rent = %s", got)
	}
	if got := facet(harare.Deal); got != "sale=6,rent=6" {
		t.Fatalf("deal facet with city=harare = %s", got)
	}
	// The price facet ignores the price filter but respects the others.
	rent := e.search("?deal=rent&max_price=600").Facets.Price
	if *rent.Min != "600.00" || *rent.Max != "3800.00" {
		t.Fatalf("price facet for rentals = %s..%s", *rent.Min, *rent.Max)
	}
	// Nothing matches: empty arrays and null prices, never missing keys.
	none := e.get("/v1/listings?q=zzzz")
	if body := none.Body.String(); !strings.Contains(body, `"facets":{"deal":[],"category":[],"property_type":[],"city":[],"suburb":[],"bedrooms":[],"features":[],"price":{"currency":"USD","min":null,"max":null},"country":[],"region":[]}`) {
		t.Fatalf("empty facets = %s", body)
	}
}

func TestListingQueryValidation(t *testing.T) {
	e := newEnv(t)
	for query, field := range map[string]string{
		"?page=0": "page", "?page=abc": "page", "?page=-1": "page",
		"?page_size=0": "page_size", "?page_size=49": "page_size", "?page_size=x": "page_size",
		"?sort=cheapest":       "sort",
		"?deal=lease":          "deal",
		"?category=industrial": "category",
		"?property_type=igloo": "property_type",
		"?city=atlantis":       "city",
		"?suburb=nowhere":      "suburb",
		"?features=helipad":    "features",
		"?min_price=abc":       "min_price", "?min_price=-5": "min_price", "?max_price=1e9": "max_price",
		"?min_price=500&max_price=100": "max_price",
		"?currency=usdollars":          "currency",
		"?featured=maybe":              "featured",
		"?bedrooms_min=-1":             "bedrooms_min", "?bathrooms_min=x": "bathrooms_min",
		"?q=" + strings.Repeat("a", 101): "q",
	} {
		env := wantError(t, e.get("/v1/listings"+query), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields[field] == "" {
			t.Fatalf("%s: fields = %v, want %s", query, env.Error.Fields, field)
		}
	}
	// A code retired from the reference data is no longer a valid filter.
	e.reference.SetActive(reference.SetDealTypes, "rent", false)
	wantError(t, e.get("/v1/listings?deal=rent"), http.StatusBadRequest, httpx.CodeValidationFailed)
}

func TestListingBySlug(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/listings/four-bedroom-family-home-borrowdale")
	wantStatus(t, rec, http.StatusOK)
	l := decode[struct {
		Listing listings.Listing `json:"listing"`
	}](t, rec).Listing

	if l.ID != uuid.MustParse("0195c0de-0000-7000-8000-000000000001") || l.Reference != "RP-ZW-10001" || l.Title != "Four-bedroom family home with garden" {
		t.Fatalf("identity: %+v", l)
	}
	if l.Deal != (listings.Coded{Code: "sale", Label: "For sale"}) || l.Category != (listings.Coded{Code: "residential", Label: "Residential"}) ||
		l.PropertyType != (listings.Coded{Code: "house", Label: "House"}) || l.Suburb != (listings.Coded{Code: "borrowdale", Label: "Borrowdale"}) ||
		l.City != (listings.Coded{Code: "harare", Label: "Harare"}) || l.MarketCode != "ZW" {
		t.Fatalf("classification: %+v %+v %+v %+v %+v", l.Deal, l.Category, l.PropertyType, l.Suburb, l.City)
	}
	if l.Price != (listings.Money{Amount: "385000.00", Currency: "USD"}) || l.PricePeriod != nil {
		t.Fatalf("price: %+v %v", l.Price, l.PricePeriod)
	}
	if l.Bedrooms == nil || *l.Bedrooms != 4 || *l.Bathrooms != 3 || *l.FloorArea != 320 || *l.LandArea != 2000 || !l.Featured {
		t.Fatalf("details: %+v", l)
	}
	if len(l.Features) != 5 || l.Features[0] != (listings.Coded{Code: "borehole", Label: "Borehole"}) || l.Features[4].Label != "Walled and gated" {
		t.Fatalf("features: %+v", l.Features)
	}
	if len(l.Photos) != 1 || l.Photos[0].URL != "/assets/images/listing-house-borrowdale.jpg" || l.Photos[0].Width <= 0 || l.Photos[0].Height <= 0 ||
		l.Photos[0].Alt != "White two-storey house with a tiled roof under a blue sky" {
		t.Fatalf("photos: %+v", l.Photos)
	}
	if !strings.HasPrefix(l.Summary, "Single-storey home") || !strings.HasPrefix(l.Description, "A well-kept single-storey home") {
		t.Fatalf("text: %q", l.Summary)
	}
	// Published four days before "now", for a 90-day cycle.
	if want := e.now.Add(-4 * 24 * time.Hour); !l.PublishedAt.Equal(want) || !l.ExpiresAt.Equal(want.AddDate(0, 0, 90)) || l.UpdatedAt.IsZero() {
		t.Fatalf("dates: published=%s expires=%s", l.PublishedAt, l.ExpiresAt)
	}
	// Advertiser: public profile references only.
	a := l.Advertiser
	if a.Type != "agency" || a.Name != "Sample Estates" || !a.Verified ||
		a.Agency == nil || *a.Agency != (listings.ProfileRef{Kind: "agencies", Slug: "sample-estates", Name: "Sample Estates", Verified: true}) ||
		a.Agent == nil || *a.Agent != (listings.ProfileRef{Kind: "agents", Slug: "tendai-ncube", Name: "Tendai Ncube", Verified: true}) {
		t.Fatalf("advertiser: %+v", a)
	}

	// A rental has a period; a plot has no rooms; a private sale has no profiles.
	rental := decode[struct {
		Listing listings.Listing `json:"listing"`
	}](t, e.get("/v1/listings/two-bedroom-garden-apartment-avondale")).Listing
	if rental.PricePeriod == nil || *rental.PricePeriod != "month" || rental.LandArea != nil {
		t.Fatalf("rental: %+v", rental.PricePeriod)
	}
	raw := e.get("/v1/listings/five-bedroom-house-victoria-falls").Body.String()
	if !strings.Contains(raw, `"advertiser":{"type":"private_seller","name":"Private seller","verified":true,"agency":null,"agent":null}`) {
		t.Fatalf("private seller advertiser: %s", raw)
	}
	if !strings.Contains(e.get("/v1/listings/office-suite-eastlea").Body.String(), `"bedrooms":null,"bathrooms":null`) {
		t.Fatal("absent numbers must be null")
	}

	t.Run("not public", func(t *testing.T) {
		wantError(t, e.get("/v1/listings/no-such-listing"), http.StatusNotFound, listings.CodeListingNotFound)
		wantError(t, e.get("/v1/listings/Bad_Slug!"), http.StatusNotFound, listings.CodeListingNotFound)
		// Was public once: 410, so the page can say "no longer available".
		wantError(t, e.get("/v1/listings/"+devseed.ExpiredListingSlug), http.StatusGone, listings.CodeListingUnavailable)
		// Never public: 404, so drafts are not revealed.
		r, _ := e.listings.BySlug(t.Context(), "office-suite-eastlea")
		for _, status := range []string{"DRAFT", "PENDING_REVIEW"} {
			draft := r
			draft.ID, draft.Slug, draft.Reference, draft.Status = uuid.New(), "a-"+strings.ToLower(status), "RP-ZW-2"+status, status
			draft.PublishedAt = time.Time{}
			e.listings.Put(draft)
			wantError(t, e.get("/v1/listings/"+draft.Slug), http.StatusNotFound, listings.CodeListingNotFound)
		}
	})
}

func TestOnlyPublishedUnexpiredListingsAreEverReturned(t *testing.T) {
	e := newEnv(t)
	gone := map[string]string{}
	r, _ := e.listings.BySlug(t.Context(), "office-suite-eastlea")
	for _, status := range []string{"PAUSED", "SOLD", "LET", "CLOSED", "EXPIRED"} {
		x := r
		x.ID, x.Slug, x.Reference, x.Status = uuid.New(), "gone-"+strings.ToLower(status), "RP-ZW-9"+status, status
		e.listings.Put(x)
		gone[x.Slug] = "status " + status
	}
	// PUBLISHED but past its expiry: the status alone is not enough.
	stale := r
	stale.ID, stale.Slug, stale.Reference = uuid.New(), "gone-stale", "RP-ZW-9STALE"
	stale.ExpiresAt = e.now
	e.listings.Put(stale)
	gone[stale.Slug] = "published but expired"
	gone[devseed.ExpiredListingSlug] = "seeded expired listing"

	assertGone := func() {
		t.Helper()
		page := e.search("?page_size=48")
		for _, l := range page.Items {
			if _, bad := gone[l.Slug]; bad {
				t.Fatalf("%s (%s) was returned by search", l.Slug, gone[l.Slug])
			}
		}
		for slug, why := range gone {
			wantError(t, e.get("/v1/listings/"+slug), http.StatusGone, listings.CodeListingUnavailable)
			for _, path := range []string{"/v1/listings?q=" + url.QueryEscape("office suite"), "/v1/listings/suggest?q=office", "/v1/sitemap",
				"/v1/directory/agencies/sample-commercial", "/v1/directory/agents/farai-chikwanha", "/v1/directory/agencies/sample-estates"} {
				if strings.Contains(e.get(path).Body.String(), `"`+slug+`"`) {
					t.Fatalf("%s (%s) leaked through %s", slug, why, path)
				}
			}
		}
	}
	if total := e.search("").Total; total != 18 {
		t.Fatalf("total = %d, want the 18 public listings only", total)
	}
	assertGone()

	// Time passes: a listing that was public expires without anyone touching it.
	e.now = e.now.AddDate(0, 0, 87) // listing 1 was published 4 days before the seed time
	wantError(t, e.get("/v1/listings/four-bedroom-family-home-borrowdale"), http.StatusGone, listings.CodeListingUnavailable)
	if page := e.search("?suburb=borrowdale"); page.Total != 0 {
		t.Fatalf("an expired listing is still in search: %s", slugs(page.Items))
	}
	if strings.Contains(e.get("/v1/directory/agents/tendai-ncube").Body.String(), "four-bedroom-family-home-borrowdale") {
		t.Fatal("an expired listing is still on the agent's profile")
	}
	e.now = e.now.AddDate(0, 0, 30)
	if total := e.search("").Total; total != 0 {
		t.Fatalf("%d listings are still public 117 days after seeding", total)
	}
}

func TestListingSuggest(t *testing.T) {
	e := newEnv(t)
	type suggestions struct {
		Suggestions []listings.Suggestion `json:"suggestions"`
	}
	get := func(q string) []listings.Suggestion {
		rec := e.get("/v1/listings/suggest?q=" + url.QueryEscape(q))
		wantStatus(t, rec, http.StatusOK)
		return decode[suggestions](t, rec).Suggestions
	}
	bor := get("bor")
	if len(bor) < 1 || bor[0] != (listings.Suggestion{Type: "suburb", Label: "Borrowdale, Harare", Value: "borrowdale"}) {
		t.Fatalf("q=bor: %+v", bor)
	}
	for _, s := range bor[1:] {
		if s.Type != "listing" || !strings.Contains(strings.ToLower(s.Label), "bor") {
			t.Fatalf("q=bor: unexpected %+v", s)
		}
		wantStatus(t, e.get("/v1/listings/"+s.Value), http.StatusOK) // a listing suggestion's value is its slug
	}
	if got := get("harare"); len(got) == 0 || got[0] != (listings.Suggestion{Type: "city", Label: "Harare", Value: "harare"}) {
		t.Fatalf("q=harare: %+v", got)
	}
	// Suburbs first, then cities, then listings; names starting with the term first; at most 8.
	house := get("house")
	if len(house) != 8 {
		t.Fatalf("q=house: %d suggestions, want the maximum of 8", len(house))
	}
	order := map[string]int{"suburb": 0, "city": 1, "listing": 2}
	for i := 1; i < len(get("mu")); i++ {
		if g := get("mu"); order[g[i].Type] < order[g[i-1].Type] {
			t.Fatalf("q=mu: types out of order: %+v", g)
		}
	}
	for _, q := range []string{"", "b", " ", "zzzz"} {
		if got := get(q); len(got) != 0 {
			t.Fatalf("q=%q: %+v", q, got)
		}
	}
	if got := strings.TrimSpace(e.get("/v1/listings/suggest").Body.String()); got != `{"suggestions":[]}` {
		t.Fatalf("body = %s", got)
	}
	wantError(t, e.get("/v1/listings/suggest?q="+strings.Repeat("a", 101)), http.StatusBadRequest, httpx.CodeValidationFailed)
}

type directoryPage struct {
	Kind       directory.Kind      `json:"kind"`
	Items      []directory.Profile `json:"items"`
	Page       int                 `json:"page"`
	PageSize   int                 `json:"page_size"`
	Total      int                 `json:"total"`
	TotalPages int                 `json:"total_pages"`
}

func names(items []directory.Profile) string {
	out := make([]string, 0, len(items))
	for _, p := range items {
		out = append(out, p.Slug)
	}
	return strings.Join(out, ",")
}

func TestDirectory(t *testing.T) {
	e := newEnv(t)

	t.Run("kinds with counts", func(t *testing.T) {
		kinds := decode[struct {
			Kinds []directory.KindCount `json:"kinds"`
		}](t, e.get("/v1/directory")).Kinds
		var got []string
		for _, k := range kinds {
			got = append(got, fmt.Sprintf("%s=%d", k.Code, k.Count))
		}
		if strings.Join(got, ",") != "agencies=5,agents=7,developers=3,property-managers=2,solicitors=3,hosts=2,service-providers=10" {
			t.Fatalf("kinds = %v", got)
		}
		if kinds[0].Label != "Real Estate Agencies" || kinds[4].Label != "Solicitors" || kinds[4].SingularLabel != "Solicitor" || kinds[0].SingularLabel != "Real Estate Agency" ||
			kinds[5].Label != "Hosts and Venues" || kinds[6].Label != "Property Services" || kinds[6].SingularLabel != "Property Service Provider" || kinds[0].Description == "" {
			t.Fatalf("kind = %+v", kinds[0])
		}
	})

	t.Run("list: sponsored first, then by name", func(t *testing.T) {
		page := decode[directoryPage](t, e.get("/v1/directory/agencies"))
		if page.Total != 5 || page.Kind.Code != "agencies" || page.Page != 1 || page.PageSize != 12 || page.TotalPages != 1 {
			t.Fatalf("page = %+v", page)
		}
		if got := names(page.Items); got != "sample-estates,sample-realty,sample-commercial,sample-homes-bulawayo,sample-land-agents" {
			t.Fatalf("order = %s", got)
		}
		first := page.Items[0]
		if !first.Sponsored || !first.Verified || first.City != (listings.Coded{Code: "harare", Label: "Harare"}) || first.ListingCount != 6 ||
			first.Tagline == "" || first.Description == "" || len(first.Areas) == 0 || len(first.Services) == 0 || first.Logo != nil || first.Agency != nil || first.Trade != nil {
			t.Fatalf("first = %+v", first)
		}
	})

	t.Run("filters and paging", func(t *testing.T) {
		for query, want := range map[string]string{
			"service-providers?city=bulawayo":                "sample-rapid-plumbing,sample-sparks,sample-sun-power",
			"service-providers?trade=plumbing":               "sample-plumbers,sample-rapid-plumbing",
			"service-providers?trade=plumbing&city=bulawayo": "sample-rapid-plumbing",
			"service-providers?q=geyser":                     "sample-plumbers,sample-solar-installers",
			"service-providers?q=GEYSER+burst":               "sample-plumbers",
			"service-providers?q=zzzz":                       "",
			"agents?q=sample+estates":                        "rudo-moyo",
			"agencies?trade=plumbing":                        "",
			"hosts?page=2":                                   "",
		} {
			if got := names(decode[directoryPage](t, e.get("/v1/directory/"+query)).Items); got != want {
				t.Fatalf("%s = %q, want %q", query, got, want)
			}
		}
		paged := decode[directoryPage](t, e.get("/v1/directory/service-providers?page_size=4&page=3"))
		if paged.Total != 10 || paged.TotalPages != 3 || len(paged.Items) != 2 {
			t.Fatalf("paging: %+v", paged.Total)
		}
		sp := decode[directoryPage](t, e.get("/v1/directory/service-providers?trade=solar-power")).Items
		if len(sp) != 2 || sp[0].Trade == nil || *sp[0].Trade != (listings.Coded{Code: "solar-power", Label: "Solar power"}) || !sp[0].Sponsored || sp[1].Sponsored {
			t.Fatalf("solar: %+v", sp)
		}
		for query, field := range map[string]string{"agencies?city=atlantis": "city", "service-providers?trade=alchemy": "trade", "agencies?page=0": "page", "agencies?page_size=100": "page_size"} {
			env := wantError(t, e.get("/v1/directory/"+query), http.StatusBadRequest, httpx.CodeValidationFailed)
			if env.Error.Fields[field] == "" {
				t.Fatalf("%s: fields = %v", query, env.Error.Fields)
			}
		}
		wantError(t, e.get("/v1/directory/wizards"), http.StatusNotFound, directory.CodeKindNotFound)
	})

	type detail struct {
		Profile directory.Detail `json:"profile"`
	}
	t.Run("agency profile: its agents and its listings", func(t *testing.T) {
		p := decode[detail](t, e.get("/v1/directory/agencies/sample-estates")).Profile
		var agents []string
		for _, a := range p.Agents {
			agents = append(agents, a.Kind+"/"+a.Slug)
		}
		if strings.Join(agents, ",") != "agents/chipo-dube,agents/rudo-moyo,agents/tendai-ncube" {
			t.Fatalf("agents = %v", agents)
		}
		if len(p.Listings) != 6 || p.ListingCount != 6 {
			t.Fatalf("listings = %d", len(p.Listings))
		}
		for i, l := range p.Listings {
			if l.Advertiser.Agency == nil || l.Advertiser.Agency.Slug != "sample-estates" || (i > 0 && l.PublishedAt.After(p.Listings[i-1].PublishedAt)) {
				t.Fatalf("listing %s does not belong here or is out of order", l.Slug)
			}
		}
		if p.Since == nil || p.Name != "Sample Estates" || p.Role == "" {
			t.Fatalf("profile = %+v", p.Profile)
		}
	})

	t.Run("agent profile: the agency and the agent's own listings", func(t *testing.T) {
		p := decode[detail](t, e.get("/v1/directory/agents/tendai-ncube")).Profile
		if p.Agency == nil || *p.Agency != (listings.ProfileRef{Kind: "agencies", Slug: "sample-estates", Name: "Sample Estates", Verified: true}) {
			t.Fatalf("agency = %+v", p.Agency)
		}
		if len(p.Listings) != 3 || len(p.Agents) != 0 {
			t.Fatalf("listings=%d agents=%d", len(p.Listings), len(p.Agents))
		}
		for _, l := range p.Listings {
			if l.Advertiser.Agent == nil || l.Advertiser.Agent.Slug != "tendai-ncube" {
				t.Fatalf("listing %s is not Tendai's", l.Slug)
			}
		}
		// The agency owner has a profile but no listings of her own.
		if rudo := decode[detail](t, e.get("/v1/directory/agents/rudo-moyo")).Profile; len(rudo.Listings) != 0 || rudo.ListingCount != 0 {
			t.Fatalf("rudo listings = %d", len(rudo.Listings))
		}
	})

	t.Run("service provider profile", func(t *testing.T) {
		rec := e.get("/v1/directory/service-providers/sample-plumbers")
		p := decode[detail](t, rec).Profile
		if p.Trade == nil || p.Trade.Code != "plumbing" || len(p.Offerings) != 4 || p.Offerings[0].Name != "Burst pipes and leaks" || p.Offerings[0].Description == "" ||
			len(p.Services) != 4 || len(p.Listings) != 0 || len(p.Agents) != 0 || !p.Sponsored || p.Since == nil || *p.Since != 2012 {
			t.Fatalf("profile = %+v", p)
		}
		if !strings.Contains(rec.Body.String(), `"logo":null,"agency":null`) || !strings.Contains(rec.Body.String(), `"agents":[],"listings":[]`) {
			t.Fatalf("absent values must be null or empty arrays: %s", rec.Body)
		}
	})

	t.Run("not found", func(t *testing.T) {
		wantError(t, e.get("/v1/directory/agencies/no-such-agency"), http.StatusNotFound, directory.CodeProfileNotFound)
		wantError(t, e.get("/v1/directory/agents/sample-estates"), http.StatusNotFound, directory.CodeProfileNotFound) // right slug, wrong kind
		wantError(t, e.get("/v1/directory/wizards/merlin"), http.StatusNotFound, directory.CodeKindNotFound)
		// A suspended profile disappears from every public endpoint.
		r, _ := e.directory.Get(t.Context(), "hosts", "sample-showrooms")
		r.Status = "SUSPENDED"
		e.directory.Put(r)
		wantError(t, e.get("/v1/directory/hosts/sample-showrooms"), http.StatusNotFound, directory.CodeProfileNotFound)
		if got := names(decode[directoryPage](t, e.get("/v1/directory/hosts")).Items); got != "sample-falls-stays" {
			t.Fatalf("hosts = %s", got)
		}
		if strings.Contains(e.get("/v1/sitemap").Body.String(), "sample-showrooms") {
			t.Fatal("a suspended profile is in the sitemap")
		}
		// A directory kind retired from the reference data disappears too.
		e.reference.SetActive(reference.SetDirectoryKinds, "hosts", false)
		wantError(t, e.get("/v1/directory/hosts"), http.StatusNotFound, directory.CodeKindNotFound)
		if strings.Contains(e.get("/v1/directory").Body.String(), `"code":"hosts"`) {
			t.Fatal("a retired kind is still listed")
		}
	})
}

func TestSitemap(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/sitemap")
	wantStatus(t, rec, http.StatusOK)
	s := decode[directory.Sitemap](t, rec)
	if len(s.Listings) != 18 || len(s.Profiles) != 32 {
		t.Fatalf("sitemap: %d listings, %d profiles", len(s.Listings), len(s.Profiles))
	}
	for _, l := range s.Listings {
		if l.Slug == "" || l.UpdatedAt.IsZero() {
			t.Fatalf("entry = %+v", l)
		}
		wantStatus(t, e.get("/v1/listings/"+l.Slug), http.StatusOK)
	}
	for _, p := range s.Profiles {
		if p.UpdatedAt.IsZero() {
			t.Fatalf("entry = %+v", p)
		}
		wantStatus(t, e.get("/v1/directory/"+p.Kind+"/"+p.Slug), http.StatusOK)
	}
}

// publicGETs is every public, unauthenticated GET the catalogue offers,
// including each listing and each profile.
func publicGETs(e *env) []string {
	paths := []string{
		"/v1/reference", "/v1/markets", "/v1/listings?page_size=48", "/v1/listings?q=house&sort=relevance", "/v1/listings/suggest?q=sample",
		"/v1/listings/suggest?q=harare", "/v1/directory", "/v1/sitemap",
		// Country, region and the advanced filters.
		"/v1/listings?country=ZW&region=harare&page_size=48", "/v1/listings?advertiser_type=private", "/v1/listings?has_photos=true&sort=area_desc&page_size=48",
		"/v1/listings/suggest?q=mata", "/v1/listings/suggest?q=zimb", "/v1/reference/regions?market=ZW",
		"/v1/directory/agencies?country=ZW&region=harare", "/v1/directory/solicitors?region=bulawayo",
	}
	for _, set := range reference.Sets() {
		paths = append(paths, "/v1/reference/"+set)
	}
	for _, l := range devseed.Sample().Listings {
		paths = append(paths, "/v1/listings/"+l.Slug)
	}
	for _, k := range devseed.Sample().Kinds {
		paths = append(paths, "/v1/directory/"+k.Code+"?page_size=48", "/v1/directory/"+k.Code+"?q=sample")
	}
	for _, p := range devseed.Sample().Profiles {
		paths = append(paths, "/v1/directory/"+p.Kind+"/"+p.Slug)
	}
	return paths
}

var (
	contactKeyRe = regexp.MustCompile(`(?i)phone|e-?mail|address|contact|whatsapp|mobile|\btel\b`)
	emailValueRe = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// A phone number: an international number, or a ten-digit local one.
	phoneValueRe = regexp.MustCompile(`\+\d[\d\s().-]{6,}\d|\b0[1-9]\d{8}\b|\b\d{3}[\s-]\d{3}[\s-]\d{4}\b`)
)

// walk visits every key and every string value of a decoded JSON document.
func walk(v any, path string, visit func(path, key string, value any)) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			visit(path, k, child)
			walk(child, path+"."+k, visit)
		}
	case []any:
		for i, child := range x {
			walk(child, fmt.Sprintf("%s[%d]", path, i), visit)
		}
	}
}

// No public response carries contact details: no key named like one, and no
// value that looks like an email address or a phone number.
func TestPublicResponsesCarryNoContactDetails(t *testing.T) {
	e := newEnv(t)
	// Make sure there is contact data in the system that COULD leak.
	wantStatus(t, e.post("/v1/property-requests", e.propertyRequest(1)), http.StatusCreated)
	wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("service-providers", "sample-plumbers")), http.StatusCreated)

	// ...including an onboarding application with a private phone number, a
	// registration number and documents, approved into a public profile.
	approved := e.approveSeededProvider()

	paths := append(publicGETs(e), "/v1/directory/service-providers/"+approved, "/v1/onboarding/requirements")
	if len(paths) < 70 {
		t.Fatalf("only %d public paths were walked", len(paths))
	}
	checked := 0
	for _, path := range paths {
		rec := e.get(path)
		wantStatus(t, rec, http.StatusOK)
		body := rec.Body.String()
		var doc any
		if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		walk(doc, "$", func(at, key string, value any) {
			checked++
			// "contact_methods" is the name of a reference set, not contact data.
			if contactKeyRe.MatchString(key) && key != "contact_methods" {
				t.Fatalf("%s: key %q at %s looks like contact data", path, key, at)
			}
			s, ok := value.(string)
			if !ok {
				return
			}
			if emailValueRe.MatchString(s) {
				t.Fatalf("%s: %s.%s contains an email address: %q", path, at, key, s)
			}
			// Reference rows legitimately carry the words EMAIL / PHONE / WHATSAPP as codes.
			if phoneValueRe.MatchString(s) {
				t.Fatalf("%s: %s.%s contains a phone number: %q", path, at, key, s)
			}
		})
		for _, private := range []string{"@example.test", "@example.com", "tariro", "+263", "771234567", "enquiries@", "password",
			"770000001", "sample-0001", "pamela", "pending.provider", "devseed", "object_key", "sha256"} {
			if strings.Contains(strings.ToLower(body), private) {
				t.Fatalf("%s contains %q", path, private)
			}
		}
		// Public and cacheable, the same for everyone.
		if cc := rec.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public, max-age=") && path != "/v1/markets" {
			t.Fatalf("%s Cache-Control = %q", path, cc)
		}
		if vary := strings.ToLower(strings.Join(rec.Header().Values("Vary"), ",")); strings.Contains(vary, "cookie") || rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s varies by cookie", path)
		}
	}
	if checked < 5000 {
		t.Fatalf("only %d values were checked", checked)
	}

	// The checks themselves would catch a leak.
	for _, leak := range []string{"sample-plumbers@example.test", "+263 77 123 4567", "0771234567", "077 123 4567"} {
		if !emailValueRe.MatchString(leak) && !phoneValueRe.MatchString(leak) {
			t.Fatalf("the leak patterns would miss %q", leak)
		}
	}
	for _, fine := range []string{"0195c0de-0000-7000-8000-000000000001", "RP-ZW-10001", "385000.00", "2026-11-01T09:00:00Z", "8 parking bays"} {
		if emailValueRe.MatchString(fine) || phoneValueRe.MatchString(fine) {
			t.Fatalf("the leak patterns flag harmless value %q", fine)
		}
	}
}

func TestCatalogueIsCacheableAndSessionIndependent(t *testing.T) {
	e := newEnv(t)
	cookie := e.loginCookie(devseed.EstatesOwnerEmail, seedPassword)
	for _, path := range []string{"/v1/listings", "/v1/listings/office-suite-eastlea", "/v1/listings/suggest?q=bor", "/v1/directory",
		"/v1/directory/agencies", "/v1/directory/agencies/sample-estates", "/v1/sitemap"} {
		anon := e.get(path)
		etag := anon.Header().Get("ETag")
		if anon.Header().Get("Cache-Control") != "public, max-age=30, stale-while-revalidate=120" || etag == "" {
			t.Fatalf("%s: Cache-Control=%q ETag=%q", path, anon.Header().Get("Cache-Control"), etag)
		}
		wantStatus(t, e.get(path, withHeader("If-None-Match", etag)), http.StatusNotModified)
		signedIn := e.get(path, withCookie(cookie))
		if signedIn.Body.String() != anon.Body.String() || signedIn.Header().Get("ETag") != etag {
			t.Fatalf("%s differs for a signed-in caller", path)
		}
	}
}
