package listings

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// globalStore is placesStore plus a short stay in Mauritius: a country
// without reference places, so its city and region are free text, priced in
// its own currency.
func globalStore(t *testing.T) (*MemoryStore, *Service) {
	t.Helper()
	store, _ := placesStore()
	store.Put(filterRecord(4, func(r *Record) {
		r.Title, r.MarketCode = "Beach villa with a pool", "MU"
		r.City, r.Suburb, r.CityName, r.RegionName = "", "", "Grand Baie", "Rivière du Rempart"
		r.Price = Money{Amount: "8500.00", Currency: "MUR"}
		r.PublishedAt = esNow.AddDate(0, 0, -2)
	}))
	svc := NewService(store, store, placeLabels{})
	svc.SetPlaces(globalPlaces{})
	svc.SetClock(func() time.Time { return esNow })
	return store, svc
}

// globalPlaces is fixedPlaces plus two countries without reference places.
type globalPlaces struct{}

func (globalPlaces) Geography(ctx context.Context) (Geography, error) {
	g, err := fixedPlaces{}.Geography(ctx)
	g.Countries["MU"] = CountryInfo{Name: "Mauritius", Currency: "MUR"}
	g.Countries["FR"] = CountryInfo{Name: "France", Currency: "EUR"}
	return g, err
}

func find(t *testing.T, svc *Service, raw string) (Page, map[string]string) {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	q, fields := parseQuery(v)
	if len(fields) > 0 {
		return Page{}, fields
	}
	q, fields, err = svc.Resolve(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) > 0 {
		return Page{}, fields
	}
	page, err := svc.Find(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return page, nil
}

func facetCodes(counts []FacetCount) string {
	var out []string
	for _, c := range counts {
		out = append(out, c.Code)
	}
	return strings.Join(out, ",")
}

// A listing in a country without reference places: its city and region are
// free text with a null code, it has no suburb, it is counted in the country
// facet and found by q, and it adds nothing to the city, suburb and region
// facets (a free-text place is not filterable).
func TestFreeTextPlaces(t *testing.T) {
	_, svc := globalStore(t)
	page, _ := find(t, svc, "country=MU")
	if page.Total != 1 || len(page.Items) != 1 {
		t.Fatalf("country=MU: %+v", page)
	}
	l := page.Items[0]
	if l.City.Code != nil || l.City.Label != "Grand Baie" || l.Suburb != nil || l.Region == nil || l.Region.Code != nil || l.Region.Label != "Rivière du Rempart" ||
		l.Country != (Country{Code: "MU", Name: "Mauritius"}) || l.MarketCode != "MU" || l.Price.Currency != "MUR" {
		t.Fatalf("listing = %+v (region %+v)", l, l.Region)
	}
	if page.Facets.Price == nil || page.Facets.Price.Currency != "MUR" || *page.Facets.Price.Min != "8500.00" {
		t.Fatalf("price facet = %+v", page.Facets.Price)
	}
	raw, _ := json.Marshal(l)
	for _, want := range []string{`"city":{"code":null,"label":"Grand Baie"}`, `"suburb":null`, `"region":{"code":null,"label":"Rivière du Rempart"}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("listing JSON lacks %s: %s", want, raw)
		}
	}
	// A reference place keeps its code.
	page, _ = find(t, svc, "country=ZW&city=harare")
	raw, _ = json.Marshal(page.Items[0])
	for _, want := range []string{`"city":{"code":"harare","label":"Harare"}`, `"suburb":{"code":"borrowdale","label":"Borrowdale"}`, `"region":{"code":"harare","label":"Harare"}`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("listing JSON lacks %s: %s", want, raw)
		}
	}

	all, _ := find(t, svc, "")
	if got := facetCodes(all.Facets.Country); got != "MU,ZA,ZW" || all.Total != 4 {
		t.Fatalf("country facet = %s, total %d", got, all.Total)
	}
	for name, counts := range map[string][]FacetCount{"city": all.Facets.City, "suburb": all.Facets.Suburb, "region": all.Facets.Region} {
		for _, c := range counts {
			if c.Code == "" || c.Label == "Grand Baie" || strings.Contains(c.Label, "Rempart") {
				t.Fatalf("%s facet has a free-text place: %+v", name, counts)
			}
		}
	}
	// Free-text city and region are searchable.
	for _, q := range []string{"q=grand+baie", "q=rempart", "q=GRAND"} {
		if page, _ := find(t, svc, q); page.Total != 1 || page.Items[0].MarketCode != "MU" {
			t.Fatalf("%s found %d", q, page.Total)
		}
	}
	// ...but they are not codes.
	if _, fields := find(t, svc, "city=grand-baie"); fields["city"] == "" {
		t.Fatalf("a free-text city was accepted as a code: %v", fields)
	}
	// Suggestions: the country has listings, so it is offered; its free-text
	// city is not (a suggestion's value is a filter code).
	suggestions, err := svc.Suggest(context.Background(), "mauri")
	if err != nil || len(suggestions) != 1 || suggestions[0] != (Suggestion{Type: "country", Label: "Mauritius", Value: "MU"}) {
		t.Fatalf("suggest mauri = %+v, %v", suggestions, err)
	}
	if suggestions, _ := svc.Suggest(context.Background(), "grand"); len(suggestions) != 0 {
		t.Fatalf("a free-text city was suggested: %+v", suggestions)
	}
	if suggestions, _ := svc.Suggest(context.Background(), "fran"); len(suggestions) != 0 {
		t.Fatalf("a country without listings was suggested: %+v", suggestions)
	}
}

// Any enabled country is a valid filter: one without listings is an empty
// result in that country's currency; an unknown code is refused.
func TestAnyCountryIsAValidFilter(t *testing.T) {
	_, svc := globalStore(t)
	page, fields := find(t, svc, "country=FR")
	if fields != nil || page.Total != 0 || len(page.Items) != 0 || page.Facets.Price == nil || page.Facets.Price.Currency != "EUR" ||
		page.Facets.Price.Min != nil || len(page.Facets.Currencies) != 0 {
		t.Fatalf("country=FR: %+v %v", page, fields)
	}
	// The country facet still offers the countries that have results.
	if got := facetCodes(page.Facets.Country); got != "MU,ZA,ZW" {
		t.Fatalf("country facet under country=FR = %s", got)
	}
	if _, fields := find(t, svc, "country=XX"); !strings.Contains(fields["country"], "not a known country") {
		t.Fatalf("country=XX: %v", fields)
	}
	if _, fields := find(t, svc, "country=FRA"); fields["country"] == "" {
		t.Fatalf("country=FRA: %v", fields)
	}
}

// One currency per search, and it is never guessed.
func TestCurrencyOfASearch(t *testing.T) {
	_, svc := globalStore(t)
	currencies := func(p Page) string {
		var out []string
		for _, c := range p.Facets.Currencies {
			out = append(out, c.Code+"="+string(rune('0'+c.Count)))
		}
		return strings.Join(out, ",")
	}
	// No country, no currency, results in three currencies: no price range,
	// and the currencies to pick from (most listings first, then by code).
	all, _ := find(t, svc, "")
	if all.Facets.Price != nil || currencies(all) != "USD=2,MUR=1,ZAR=1" {
		t.Fatalf("mixed: price %+v, currencies %s", all.Facets.Price, currencies(all))
	}
	raw, _ := json.Marshal(all.Facets)
	if !strings.Contains(string(raw), `"price":null`) || !strings.Contains(string(raw), `"currencies":[{"code":"USD","count":2},{"code":"MUR","count":1},{"code":"ZAR","count":1}]`) {
		t.Fatalf("facets JSON = %s", raw)
	}
	// ...so a price filter has nothing to apply to.
	for _, raw := range []string{"min_price=1000", "max_price=500000", "min_price=1&max_price=2&sort=price_asc"} {
		if _, fields := find(t, svc, raw); fields["currency"] != CurrencyNeededMessage || len(fields) != 1 {
			t.Fatalf("%s: %v", raw, fields)
		}
	}
	// A currency or a country settles it.
	page, fields := find(t, svc, "min_price=1000&currency=USD")
	if fields != nil || page.Total != 2 || page.Facets.Price.Currency != "USD" || page.Facets.Price.Min == nil {
		t.Fatalf("currency=USD: %+v %v", page.Facets.Price, fields)
	}
	page, fields = find(t, svc, "min_price=1000&country=ZA")
	if fields != nil || page.Total != 1 || page.Facets.Price.Currency != "ZAR" || currencies(page) != "ZAR=1" {
		t.Fatalf("country=ZA: %+v %v", page, fields)
	}
	// An explicit currency wins over the country's, as before.
	page, _ = find(t, svc, "country=ZA&currency=USD")
	if page.Total != 1 || page.Facets.Price.Currency != "USD" || page.Facets.Price.Min != nil || currencies(page) != "ZAR=1" {
		t.Fatalf("country=ZA&currency=USD: %+v", page.Facets)
	}
	// Results that share one currency need no choice: the range is in it,
	// and a price filter uses it, whatever that currency is.
	page, fields = find(t, svc, "city=johannesburg")
	if fields != nil || page.Facets.Price == nil || page.Facets.Price.Currency != "ZAR" || *page.Facets.Price.Max != "900000.00" {
		t.Fatalf("city=johannesburg: %+v %v", page.Facets.Price, fields)
	}
	page, fields = find(t, svc, "city=johannesburg&min_price=100&max_price=900000")
	if fields != nil || page.Total != 1 || page.Facets.Price.Currency != "ZAR" {
		t.Fatalf("priced, one currency: %+v %v", page, fields)
	}
	page, fields = find(t, svc, "q=villa&max_price=9000")
	if fields != nil || page.Total != 1 || page.Facets.Price.Currency != "MUR" {
		t.Fatalf("priced, MUR only: %+v %v", page, fields)
	}
	// Nothing matches: an empty range in the nominal default, not an error.
	page, fields = find(t, svc, "q=nothingmatchesthis&min_price=5")
	if fields != nil || page.Total != 0 || page.Facets.Price == nil || page.Facets.Price.Currency != DefaultCurrency || page.Facets.Price.Min != nil {
		t.Fatalf("no results: %+v %v", page.Facets.Price, fields)
	}
}

// The same rules reach Elasticsearch and the database: the free-text places
// are indexed and searched, and prices are aggregated per currency.
func TestFreeTextPlacesAndCurrenciesInTheOtherAdapters(t *testing.T) {
	store, _ := globalStore(t)
	records, _ := store.AllPublished(context.Background(), esNow)
	var mauritius Record
	for _, r := range records {
		if r.MarketCode == "MU" {
			mauritius = r
		}
	}
	if err := CheckRecord(mauritius); err != nil {
		t.Fatal(err)
	}
	labels, err := loadLabels(context.Background(), placeLabels{}, globalPlaces{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(BuildDocument(mauritius, labels))
	doc := string(raw)
	for _, want := range []string{`"city_name":"Grand Baie"`, `"region_name":"Rivière du Rempart"`, `"market":"MU"`, `"currency":"MUR"`} {
		if !strings.Contains(doc, want) {
			t.Fatalf("document lacks %s: %s", want, doc)
		}
	}
	for _, unwanted := range []string{`"city":`, `"suburb":`, `"city_label":`, `"suburb_label":`, `"region":`} {
		if strings.Contains(doc, unwanted) {
			t.Fatalf("a free-text listing's document has %s: %s", unwanted, doc)
		}
	}
	body, _ := json.Marshal(BuildSearchBody(Query{Q: "baie", Sort: SortRelevance, Page: 1, PageSize: 12}, esNow))
	for _, want := range []string{`"city_name^3"`, `"region_name^3"`, `"price":{"aggs":{"values":{"aggs":{"max":{"max":{"field":"price"}},"min":{"min":{"field":"price"}}},"terms":{"field":"currency","size":200}}}`} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("search body lacks %s: %s", want, body)
		}
	}
	sql := where(Query{Q: "baie"}, esNow, "").sql()
	if !strings.Contains(sql, "l.city_name ILIKE $2") || !strings.Contains(sql, "l.region_name ILIKE $2") {
		t.Fatalf("database search does not match free-text places: %s", sql)
	}
	// The price filter always names its currency.
	priced := where(Query{MinPrice: rat("10"), Currency: "MUR"}, esNow, "")
	if !strings.Contains(priced.sql(), "l.currency_code = $2") || priced.args[1] != "MUR" || strings.Contains(where(Query{MinPrice: rat("10"), Currency: "MUR"}, esNow, "price").sql(), "currency_code") {
		t.Fatalf("priced WHERE = %s %v", priced.sql(), priced.args)
	}
}

func TestListingPlaceRules(t *testing.T) {
	ok := sampleRecord()
	if err := CheckRecord(ok); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Record){
		"no country":                func(r *Record) { r.MarketCode = "" },
		"a country name":            func(r *Record) { r.MarketCode = "Zimbabwe" },
		"no city at all":            func(r *Record) { r.City, r.Suburb = "", "" },
		"a code and free text":      func(r *Record) { r.CityName = "Harare" },
		"a region next to a code":   func(r *Record) { r.RegionName = "Harare Province" },
		"a suburb without a city":   func(r *Record) { r.City, r.CityName = "", "Grand Baie" },
		"markup in the city":        func(r *Record) { r.City, r.Suburb, r.CityName = "", "", "<b>Grand Baie</b>" },
		"a city that is too long":   func(r *Record) { r.City, r.Suburb, r.CityName = "", "", strings.Repeat("a", 81) },
		"a region with a new line":  func(r *Record) { r.City, r.Suburb, r.CityName, r.RegionName = "", "", "Grand Baie", "North\nEast" },
		"a city with outer spacing": func(r *Record) { r.City, r.Suburb, r.CityName = "", "", " Grand Baie" },
	} {
		r := sampleRecord()
		mutate(&r)
		if err := CheckRecord(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	free := sampleRecord()
	free.City, free.Suburb, free.CityName, free.RegionName, free.MarketCode = "", "", strings.Repeat("é", 80), "Rivière du Rempart", "MU"
	if err := CheckRecord(free); err != nil {
		t.Fatalf("a free-text place of 80 characters: %v", err)
	}
}

// A stay (check_in / check_out) is resolved from the database into listing
// IDs to leave out. That exclusion reaches the Elasticsearch query and the
// database adapter's SQL as part of the MAIN filter, so hits, the total and
// every facet are computed with it applied; the index holds no availability.
func TestStayExclusionReachesEveryAdapter(t *testing.T) {
	stay := &Stay{Nights: 3, ExcludeCountries: []string{"MU"}, ExcludeIDs: []uuid.UUID{
		uuid.MustParse("0195c0de-0000-7000-8000-000000000022"), uuid.MustParse("0195c0de-0000-7000-8000-000000000023"),
	}}
	q := Query{Category: CategoryShortStay, City: "harare", Stay: stay, Sort: SortNewest, Page: 1, PageSize: 12}
	raw, _ := json.Marshal(BuildSearchBody(q, esNow))
	var body struct {
		Query struct {
			Bool struct {
				Filter []json.RawMessage `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
		PostFilter json.RawMessage `json:"post_filter"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	var main []string
	for _, f := range body.Query.Bool.Filter {
		main = append(main, string(f))
	}
	filter := strings.Join(main, " ")
	for _, want := range []string{
		`{"bool":{"must_not":[{"ids":{"values":["0195c0de-0000-7000-8000-000000000022","0195c0de-0000-7000-8000-000000000023"]}},{"terms":{"market":["MU"]}}]}}`,
		`{"range":{"min_nights":{"lte":3}}}`, `{"bool":{"must_not":[{"exists":{"field":"min_nights"}}]}}`,
	} {
		if !strings.Contains(filter, want) {
			t.Fatalf("the main filter lacks %s:\n%s", want, filter)
		}
	}
	if strings.Contains(string(body.PostFilter), `"ids"`) || !strings.Contains(string(body.PostFilter), `"category"`) {
		t.Fatalf("the exclusion must not be a post_filter (facets would ignore it): %s", body.PostFilter)
	}
	// No availability field is asked of the index.
	for _, field := range []string{"availab", "check_in", "booked", "blocked"} {
		if strings.Contains(strings.ToLower(string(raw)), field) || strings.Contains(strings.ToLower(string(IndexMapping)), field) {
			t.Fatalf("the index or the query knows about %q", field)
		}
	}
	if plain, _ := json.Marshal(BuildSearchBody(Query{Sort: SortNewest, Page: 1, PageSize: 12}, esNow)); strings.Contains(string(plain), `"ids"`) || strings.Contains(string(plain), "min_nights") {
		t.Fatalf("a search without a stay excludes something: %s", plain)
	}
	// The database adapter: also in the WHERE of every facet (whatever it skips).
	for _, skip := range []string{"", "category", "city", "country", "price", "bedrooms"} {
		b := where(q, esNow, skip)
		sql := b.sql()
		for _, want := range []string{`COALESCE(l.min_nights, 1) <= $`, `l.id <> ALL($`, `mk.code <> ALL($`} {
			if !strings.Contains(sql, want) {
				t.Fatalf("WHERE (skip %q) lacks %q:\n%s", skip, want, sql)
			}
		}
		if strings.Count(sql, "$") != len(b.args) {
			t.Fatalf("placeholders and arguments differ: %s %v", sql, b.args)
		}
	}
	// The memory adapter.
	store, svc := globalStore(t)
	two := 2
	store.Put(filterRecord(5, func(r *Record) {
		r.Category, r.Deal, r.PricePeriod, r.MinNights = CategoryShortStay, DealRent, PricePeriodNight, &two
	}))
	all := Query{Category: CategoryShortStay, Sort: SortNewest, Page: 1, PageSize: 12, Stay: &Stay{Nights: 2}}
	page, err := svc.Find(context.Background(), all)
	if err != nil || page.Total != 1 || facetCodes(page.Facets.Country) != "ZW" {
		t.Fatalf("a stay nobody is excluded from: %+v %v", page, err)
	}
	for name, s := range map[string]*Stay{
		"excluded by ID":      {Nights: 2, ExcludeIDs: []uuid.UUID{page.Items[0].ID}},
		"excluded by country": {Nights: 2, ExcludeCountries: []string{"ZW"}},
		"too short a stay":    {Nights: 1},
	} {
		all.Stay = s
		page, err := svc.Find(context.Background(), all)
		// (The category facet leaves out its own filter, as every facet does,
		// so it still offers the other categories; everything else is empty.)
		if err != nil || page.Total != 0 || len(page.Facets.Country) != 0 || len(page.Facets.City) != 0 || len(page.Facets.Currencies) != 0 {
			t.Fatalf("%s: %+v %v", name, page, err)
		}
	}
}
