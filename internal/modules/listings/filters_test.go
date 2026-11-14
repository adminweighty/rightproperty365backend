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

// fixedPlaces is a small hierarchy: ZW > harare > harare > borrowdale, and
// ZW > matabeleland-north > victoria-falls.
type fixedPlaces struct{}

func (fixedPlaces) Geography(context.Context) (Geography, error) {
	return Geography{
		Countries:     map[string]CountryInfo{"ZW": {Name: "Zimbabwe", Currency: "USD"}, "ZA": {Name: "South Africa", Currency: "ZAR"}},
		RegionCountry: map[string]string{"harare": "ZW", "matabeleland-north": "ZW", "gauteng": "ZA"},
		CityRegion:    map[string]string{"harare": "harare", "victoria-falls": "matabeleland-north", "johannesburg": "gauteng"},
		CityCountry:   map[string]string{"harare": "ZW", "victoria-falls": "ZW", "johannesburg": "ZA"},
		SuburbCity:    map[string]string{"borrowdale": "harare", "sandton": "johannesburg"},
	}, nil
}

// placeLabels knows the codes and labels of the hierarchy above.
type placeLabels struct{}

var placeSets = map[string]map[string]string{
	SetRegions: {"harare": "Harare", "matabeleland-north": "Matabeleland North", "gauteng": "Gauteng"},
	SetCities:  {"harare": "Harare", "victoria-falls": "Victoria Falls", "johannesburg": "Johannesburg"},
	SetSuburbs: {"borrowdale": "Borrowdale", "sandton": "Sandton"},
}

func (placeLabels) Labels(_ context.Context, set string) (map[string]string, error) {
	return placeSets[set], nil
}

func (placeLabels) ActiveCodes(_ context.Context, set string) ([]string, error) {
	var out []string
	for code := range placeSets[set] {
		out = append(out, code)
	}
	return out, nil
}

func filterRecord(n int, mutate func(*Record)) Record {
	r := sampleRecord()
	r.ID = uuid.MustParse("0195c0de-0000-7000-8000-0000000000" + string(rune('0'+n/10)) + string(rune('0'+n%10)))
	r.Slug, r.Reference = "listing-"+r.ID.String()[34:], "RP-ZW-100"+r.ID.String()[34:]
	r.OrganisationID, r.AgentUserID, r.SellerUserID = nil, nil, nil
	mutate(&r)
	return r
}

func placesStore() (*MemoryStore, *Service) {
	store := NewMemoryStore(placeLabels{})
	// 1: the sample house in Borrowdale, Harare (agency, photos, 320 m2 / 2000 m2, 4 bed, 3 bath, 4 days old).
	store.Put(filterRecord(1, func(r *Record) {}))
	// 2: a private seller's flat in Victoria Falls, no photos, small, older.
	store.Put(filterRecord(2, func(r *Record) {
		r.City, r.Suburb = "victoria-falls", "falls-centre"
		r.Advertiser, r.Photos = Advertiser{Type: AdvertiserPrivateSeller, Name: "Grace Mhlanga"}, nil
		r.Bedrooms, r.Bathrooms, r.FloorArea, r.LandArea = intp(2), intp(1), intp(80), nil
		r.PublishedAt = esNow.AddDate(0, 0, -40)
	}))
	// 3: a stand in Johannesburg (another country and currency), no building.
	store.Put(filterRecord(3, func(r *Record) {
		r.City, r.Suburb, r.MarketCode = "johannesburg", "sandton", "ZA"
		r.Price = Money{Amount: "900000.00", Currency: "ZAR"}
		r.Bedrooms, r.Bathrooms, r.FloorArea, r.LandArea = nil, nil, nil, intp(900)
		r.PublishedAt = esNow.AddDate(0, 0, -1)
	}))
	svc := NewService(store, store, placeLabels{})
	svc.SetPlaces(fixedPlaces{})
	svc.SetClock(func() time.Time { return esNow })
	return store, svc
}

func numbers(t *testing.T, svc *Service, raw string) string {
	t.Helper()
	v, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	q, fields := parseQuery(v)
	if len(fields) > 0 {
		t.Fatalf("%s: %v", raw, fields)
	}
	q, fields, err = svc.Resolve(context.Background(), q)
	if err != nil || len(fields) > 0 {
		t.Fatalf("%s: %v %v", raw, fields, err)
	}
	page, err := svc.Find(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range page.Items {
		out = append(out, strings.TrimPrefix(l.Reference, "RP-ZW-1000"))
	}
	return strings.Join(out, ",")
}

// Each new filter alone, then combined, on the memory adapter (newest first: 3, 1, 2).
func TestCountryRegionAndAdvancedFilters(t *testing.T) {
	_, svc := placesStore()
	for raw, want := range map[string]string{
		"":                              "3,1,2",
		"country=ZW":                    "1,2",
		"country=zw":                    "1,2",
		"market=ZA":                     "3",
		"country=ZA&market=za":          "3",
		"region=harare":                 "1",
		"region=matabeleland-north":     "2",
		"country=ZW&region=harare":      "1",
		"region=harare&city=harare":     "1",
		"bedrooms_max=2":                "2",
		"bedrooms_min=3&bedrooms_max=4": "1",
		"bathrooms_max=1":               "2",
		"floor_area_min=100":            "1",
		"floor_area_max=100":            "2",
		"land_area_min=1000":            "1",
		"land_area_max=1000":            "3",
		"listed_within_days=30":         "3,1",
		"listed_within_days=2":          "3",
		"advertiser_type=agency":        "3,1",
		"advertiser_type=private":       "2",
		"reference=rp-zw-10002":         "2",
		"reference=RP-ZW-10001":         "1",
		"has_photos=true":               "3,1",
		"has_photos=false":              "3,1,2",
		"sort=oldest":                   "2,1,3",
		"sort=area_desc":                "1,2,3",
		"country=ZW&advertiser_type=agency&has_photos=true&floor_area_min=200&listed_within_days=30&bedrooms_max=5": "1",
		"country=ZW&region=harare&advertiser_type=private":                                                          "",
	} {
		if got := numbers(t, svc, raw); got != want {
			t.Errorf("?%s returned [%s], want [%s]", raw, got, want)
		}
	}
}

func TestCountrySetsTheDefaultCurrencyAndListingsCarryRegionAndCountry(t *testing.T) {
	_, svc := placesStore()
	resolve := func(q Query) (Query, map[string]string) {
		out, fields, err := svc.Resolve(context.Background(), q)
		if err != nil {
			t.Fatal(err)
		}
		return out, fields
	}
	if q, _ := resolve(Query{Country: "ZA"}); q.Currency != "ZAR" {
		t.Fatalf("currency = %q, want the country's", q.Currency)
	}
	if q, _ := resolve(Query{Country: "ZA", Currency: "USD"}); q.Currency != "USD" {
		t.Fatalf("an explicit currency must win, got %q", q.Currency)
	}
	q, _ := resolve(Query{Country: "ZA", Sort: SortNewest, Page: 1, PageSize: 12})
	page, err := svc.Find(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if page.Facets.Price.Currency != "ZAR" || page.Facets.Price.Min == nil || *page.Facets.Price.Min != "900000.00" {
		t.Fatalf("price facet = %+v", page.Facets.Price)
	}
	l := page.Items[0]
	if l.Region == nil || !l.Region.Is("gauteng") || l.Region.Label != "Gauteng" || l.Country != (Country{Code: "ZA", Name: "South Africa"}) || l.MarketCode != "ZA" {
		t.Fatalf("region / country = %+v / %+v", l.Region, l.Country)
	}
	raw, _ := json.Marshal(l)
	if !strings.Contains(string(raw), `"region":{"code":"gauteng","label":"Gauteng"},"country":{"code":"ZA","name":"South Africa"},"market_code":"ZA"`) {
		t.Fatalf("listing JSON: %s", raw)
	}

	// Facets: country and region ignore their own filter; suburbs name their city.
	q, _ = resolve(Query{Country: "ZW", Region: "harare", Sort: SortNewest, Page: 1, PageSize: 12})
	page, err = svc.Find(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	f := page.Facets
	if len(f.Country) != 1 || f.Country[0] != (FacetCount{Code: "ZW", Label: "Zimbabwe", Count: 1}) {
		t.Fatalf("country facet = %+v (a region of ZW is selected, so only ZW has listings in it)", f.Country)
	}
	if len(f.Region) != 2 || f.Region[0].Code != "harare" || f.Region[0].Count != 1 || f.Region[1].Code != "matabeleland-north" || f.Region[1].Label != "Matabeleland North" {
		t.Fatalf("region facet = %+v", f.Region)
	}
	if len(f.Suburb) != 1 || f.Suburb[0].City == nil || *f.Suburb[0].City != "harare" {
		t.Fatalf("suburb facet = %+v", f.Suburb)
	}
	if len(f.City) != 1 || f.City[0].City != nil {
		t.Fatalf("city facet = %+v", f.City)
	}
	all, err := svc.Find(context.Background(), Query{Sort: SortNewest, Page: 1, PageSize: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Facets.Country) != 2 || len(all.Facets.Region) != 0 {
		// Without Resolve there is no city -> region map: regions need the hierarchy.
		t.Fatalf("unresolved facets = %+v / %+v", all.Facets.Country, all.Facets.Region)
	}
}

func TestLocationHierarchyIsValidated(t *testing.T) {
	_, svc := placesStore()
	for name, tc := range map[string]struct {
		q     Query
		field string
	}{
		"unknown country":            {Query{Country: "XX"}, "country"},
		"unknown region":             {Query{Region: "atlantis"}, "region"},
		"region of another country":  {Query{Country: "ZA", Region: "harare"}, "region"},
		"city of another region":     {Query{Region: "matabeleland-north", City: "harare"}, "city"},
		"city of another country":    {Query{Country: "ZA", City: "harare"}, "city"},
		"suburb of another city":     {Query{City: "victoria-falls", Suburb: "borrowdale"}, "suburb"},
		"suburb of another region":   {Query{Region: "matabeleland-north", Suburb: "borrowdale"}, "suburb"},
		"suburb of another country":  {Query{Country: "ZA", Suburb: "borrowdale"}, "suburb"},
		"unknown city stays unknown": {Query{Region: "harare", City: "atlantis"}, "city"},
	} {
		_, fields, err := svc.Resolve(context.Background(), tc.q)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := fields[tc.field]; !ok || len(fields) != 1 {
			t.Errorf("%s: fields = %v, want only %q", name, fields, tc.field)
		}
	}
	for name, q := range map[string]Query{
		"whole chain": {Country: "ZW", Region: "harare", City: "harare", Suburb: "borrowdale"},
		"suburb only": {Suburb: "borrowdale"},
		"nothing":     {},
	} {
		if _, fields, _ := svc.Resolve(context.Background(), q); len(fields) != 0 {
			t.Errorf("%s: unexpected %v", name, fields)
		}
	}
}

func TestParseTheNewQueryParameters(t *testing.T) {
	for raw, field := range map[string]string{
		"bedrooms_min=4&bedrooms_max=2":         "bedrooms_max",
		"bathrooms_min=3&bathrooms_max=1":       "bathrooms_max",
		"floor_area_min=300&floor_area_max=200": "floor_area_max",
		"land_area_min=2&land_area_max=1":       "land_area_max",
		"floor_area_min=-1":                     "floor_area_min",
		"land_area_max=abc":                     "land_area_max",
		"bedrooms_max=51":                       "bedrooms_max",
		"listed_within_days=0":                  "listed_within_days",
		"listed_within_days=366":                "listed_within_days",
		"advertiser_type=agent":                 "advertiser_type",
		"reference=RP%20ZW":                     "reference",
		"has_photos=yes":                        "has_photos",
		"country=ZWE":                           "country",
		"country=ZW&market=ZA":                  "market",
		"sort=biggest":                          "sort",
	} {
		v, _ := url.ParseQuery(raw)
		if _, fields := parseQuery(v); len(fields) != 1 || fields[field] == "" {
			t.Errorf("?%s: fields = %v, want only %q", raw, fields, field)
		}
	}
	v, _ := url.ParseQuery("country=zw&region=harare&floor_area_min=200&floor_area_max=200&listed_within_days=30&advertiser_type=private&reference=rp-zw-10001&has_photos=true&sort=area_desc")
	q, fields := parseQuery(v)
	if len(fields) != 0 || q.Country != "ZW" || q.Region != "harare" || *q.FloorAreaMin != 200 || *q.FloorAreaMax != 200 || *q.ListedWithinDays != 30 ||
		q.AdvertiserType != AdvertiserFilterPrivate || q.Reference != "rp-zw-10001" || !q.HasPhotos || q.Sort != SortAreaDesc {
		t.Fatalf("parsed = %+v, %v", q, fields)
	}
}

func TestSuggestRegionsAndCountries(t *testing.T) {
	_, svc := placesStore()
	got, err := svc.Suggest(context.Background(), "mata")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (Suggestion{Type: "region", Label: "Matabeleland North, Zimbabwe", Value: "matabeleland-north"}) {
		t.Fatalf("suggest mata = %+v", got)
	}
	// A city, the region of the same name, then listings; a region or a
	// country without public listings is not suggested.
	got, err = svc.Suggest(context.Background(), "hara")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Type != "city" || got[1] != (Suggestion{Type: "region", Label: "Harare, Zimbabwe", Value: "harare"}) {
		t.Fatalf("suggest hara = %+v", got)
	}
	got, _ = svc.Suggest(context.Background(), "zimb")
	if len(got) != 1 || got[0] != (Suggestion{Type: "country", Label: "Zimbabwe", Value: "ZW"}) {
		t.Fatalf("suggest zimb = %+v", got)
	}
	store, _ := placesStore()
	empty := NewService(store, staleIndex{}, placeLabels{}) // a search engine that knows of no listing
	empty.SetPlaces(fixedPlaces{})
	if got, _ := empty.Suggest(context.Background(), "mata"); len(got) != 0 {
		t.Fatalf("a region without listings was suggested: %+v", got)
	}
}

// The database adapter's WHERE clause: every new filter, and each facet
// leaving out its own filter.
func TestPostgresQueryBuilder(t *testing.T) {
	q := Query{
		Country: "ZW", Region: "harare", City: "harare", BedroomsMin: intp(2), BedroomsMax: intp(5), BathroomsMax: intp(4),
		FloorAreaMin: intp(120), FloorAreaMax: intp(600), LandAreaMin: intp(400), LandAreaMax: intp(5000),
		ListedWithinDays: intp(30), AdvertiserType: AdvertiserFilterAgency, Reference: "rp-zw-10001", HasPhotos: true,
	}
	b := where(q, esNow, "")
	sql := b.sql()
	if !strings.HasPrefix(sql, publicCondition) {
		t.Fatalf("the public rule must come first: %s", sql)
	}
	for _, want := range []string{
		`l.city_code = $2`, `mk.code = $3`, `ct.parent_code = $4`, `l.bedrooms >= $`, `l.bedrooms <= $`, `l.bathrooms <= $`,
		`l.floor_area_m2 >= $`, `l.floor_area_m2 <= $`, `l.land_area_m2 >= $`, `l.land_area_m2 <= $`, `l.published_at >= $`,
		`l.organisation_id IS NOT NULL`, `upper(l.reference) = upper($`, `EXISTS (SELECT 1 FROM listing_photos lp WHERE lp.listing_id = l.id)`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("WHERE lacks %q:\n%s", want, sql)
		}
	}
	if strings.Count(sql, "$") != len(b.args) || len(b.args) != 13 {
		t.Fatalf("%d placeholders for %d arguments:\n%s", strings.Count(sql, "$"), len(b.args), sql)
	}
	if b.args[2] != "ZW" || b.args[3] != "harare" || b.args[10].(time.Time) != esNow.AddDate(0, 0, -30) || b.args[11] != "rp-zw-10001" {
		t.Fatalf("arguments = %v", b.args)
	}
	// No user input is ever written into the SQL text.
	for _, value := range []string{"ZW", "harare", "rp-zw-10001"} {
		if strings.Contains(sql, "'"+value+"'") {
			t.Fatalf("%q was inlined into the SQL", value)
		}
	}
	// Facets leave out their own filter and nothing else.
	if s := where(q, esNow, "country").sql(); strings.Contains(s, "mk.code") || !strings.Contains(s, "ct.parent_code") {
		t.Fatalf("country facet: %s", s)
	}
	if s := where(q, esNow, "region").sql(); strings.Contains(s, "ct.parent_code") || !strings.Contains(s, "mk.code") || !strings.Contains(s, "l.city_code") {
		t.Fatalf("region facet: %s", s)
	}
	if s := where(q, esNow, "bedrooms").sql(); strings.Contains(s, "l.bedrooms") {
		t.Fatalf("bedrooms facet: %s", s)
	}
	if s := where(Query{AdvertiserType: AdvertiserFilterPrivate}, esNow, "").sql(); !strings.Contains(s, `l.organisation_id IS NULL`) {
		t.Fatalf("private advertiser: %s", s)
	}
	if !strings.Contains(searchFrom, "LEFT JOIN markets mk ON mk.id = l.market_id") || !strings.Contains(searchFrom, "ct.set_code = 'cities'") {
		t.Fatalf("searchFrom lacks the joins the country and region filters need: %s", searchFrom)
	}
}

func TestElasticSearchAdapterReadsCountryAndRegionFacets(t *testing.T) {
	es := &fakeES{respond: func(string, string, []byte) (int, string) {
		return 200, `{"timed_out":false,"_shards":{"failed":0},"hits":{"total":{"value":3},"hits":[]},"aggregations":{
			"country":{"values":{"buckets":[{"key":"ZW","doc_count":18}]}},
			"region":{"values":{"buckets":[{"key":"harare","doc_count":11},{"key":"matabeleland-north","doc_count":2}]}},
			"price":{"values":{"buckets":[]}}}}`
	}}
	hits, err := NewElasticSearch(es, "rp365-test", time.Second).Search(context.Background(), Query{Country: "ZW", Page: 1, PageSize: 12}, esNow)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Facets.Country["ZW"] != 18 || hits.Facets.Region["harare"] != 11 || hits.Facets.Region["matabeleland-north"] != 2 {
		t.Fatalf("facets = %+v", hits.Facets)
	}
	sent := string(es.requests[0].Body)
	if !strings.Contains(sent, `"post_filter":{"bool":{"filter":[{"term":{"market":"ZW"}}]}}`) {
		t.Fatalf("the country must be a post_filter on the market keyword: %s", sent)
	}
}

func TestMappingVersionAndNewFields(t *testing.T) {
	var mapping struct {
		Mappings struct {
			Meta struct {
				Version int `json:"mapping_version"`
			} `json:"_meta"`
			Properties map[string]struct {
				Type string `json:"type"`
			} `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(IndexMapping, &mapping); err != nil {
		t.Fatal(err)
	}
	if mapping.Mappings.Meta.Version != MappingVersion || MappingVersion != 4 {
		t.Fatalf("mapping _meta version %d, code says %d: raise both together and note the reindex", mapping.Mappings.Meta.Version, MappingVersion)
	}
	for field, want := range map[string]string{
		"market": "keyword", "region": "keyword", "advertiser_type": "keyword", "has_photos": "boolean",
		"floor_area": "integer", "land_area": "integer", "published_at": "date", "city_name": "text", "region_name": "text",
	} {
		if got := mapping.Mappings.Properties[field].Type; got != want {
			t.Errorf("mapping of %s = %q, want %q", field, got, want)
		}
	}
}

// describedLabels also knows the description of the short-stay category.
type describedLabels struct{ placeLabels }

func (describedLabels) Labels(_ context.Context, set string) (map[string]string, error) {
	if set == SetPropertyCategories {
		return map[string]string{CategoryShortStay: "Short stays", "residential": "Residential"}, nil
	}
	return placeSets[set], nil
}

func (describedLabels) Descriptions(_ context.Context, set string) (map[string]string, error) {
	if set == SetPropertyCategories {
		return map[string]string{CategoryShortStay: "Holiday homes and rooms let by the night, including Airbnb-style stays."}, nil
	}
	return nil, nil
}

// "airbnb" is not a category, a type or a label, but it finds the short
// stays: on the memory adapter, in the Elasticsearch query and document, and
// in the database adapter's SQL.
func TestShortStaySearchAndGuests(t *testing.T) {
	store := NewMemoryStore(describedLabels{})
	store.Put(filterRecord(1, func(r *Record) {}))
	store.Put(filterRecord(2, func(r *Record) {
		r.Category, r.Deal, r.PricePeriod, r.PropertyType = CategoryShortStay, DealRent, PricePeriodNight, "lodge"
		r.MaxGuests, r.MinNights = intp(6), intp(2)
	}))
	search := func(q Query) []string {
		q.Sort, q.Page, q.PageSize = SortNewest, 1, 12
		hits, err := store.Search(context.Background(), q, esNow)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, id := range hits.IDs {
			out = append(out, id.String()[34:])
		}
		return out
	}
	for name, tc := range map[string]struct {
		q    Query
		want string
	}{
		"airbnb":           {Query{Q: "airbnb"}, "02"},
		"AirBnB":           {Query{Q: "AirBnB"}, "02"},
		"short stays":      {Query{Q: "short stays"}, "02"},
		"category":         {Query{Category: CategoryShortStay}, "02"},
		"guests_min met":   {Query{GuestsMin: intp(6)}, "02"},
		"guests_min unmet": {Query{GuestsMin: intp(7)}, ""},
		"no filter":        {Query{}, "01,02"},
	} {
		if got := strings.Join(search(tc.q), ","); got != tc.want {
			t.Errorf("%s: [%s], want [%s]", name, got, tc.want)
		}
	}

	// Elasticsearch: the category keywords are indexed and searched; guests are a range.
	labels, err := loadLabels(context.Background(), describedLabels{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	records, _ := store.ByIDs(context.Background(), []uuid.UUID{uuid.MustParse("0195c0de-0000-7000-8000-000000000002")})
	doc := BuildDocument(records[0], labels)
	if !strings.Contains(doc.Keywords, "Airbnb-style") || !strings.HasPrefix(doc.Keywords, "Short stays") || *doc.MaxGuests != 6 || *doc.MinNights != 2 || doc.PricePeriod != "night" {
		t.Fatalf("document = %+v", doc)
	}
	raw, _ := json.Marshal(BuildSearchBody(Query{Q: "airbnb", Category: CategoryShortStay, GuestsMin: intp(4), Sort: SortRelevance, Page: 1, PageSize: 12}, esNow))
	body := string(raw)
	for _, want := range []string{`"keywords"]`, `"query":"airbnb"`, `{"range":{"max_guests":{"gte":4}}}`, `{"term":{"category":"short-stay"}}`} {
		if !strings.Contains(body, want) {
			t.Errorf("search body lacks %s: %s", want, body)
		}
	}
	// The database adapter matches the category's label and description, and filters guests.
	sql := where(Query{Q: "airbnb", GuestsMin: intp(4)}, esNow, "").sql()
	if !strings.Contains(sql, "pc.description ILIKE $2") || !strings.Contains(sql, "pc.label ILIKE $2") || !strings.Contains(sql, "l.max_guests >= $3") ||
		!strings.Contains(searchFrom, "pc.set_code = 'property_categories' AND pc.code = l.category") {
		t.Fatalf("database search: %s", sql)
	}
}
