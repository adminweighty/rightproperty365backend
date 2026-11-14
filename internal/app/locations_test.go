package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// seeded returns the public sample listings, to derive expectations from the
// data instead of repeating it.
func (e *env) seeded() []listings.Record {
	var out []listings.Record
	for _, r := range devseed.ListingRecords(e.now) {
		if r.Public(e.now) {
			out = append(out, r)
		}
	}
	return out
}

func count(records []listings.Record, keep func(listings.Record) bool) int {
	n := 0
	for _, r := range records {
		if keep(r) {
			n++
		}
	}
	return n
}

func TestReferenceRegionsAndCityParents(t *testing.T) {
	e := newEnv(t)
	type item struct {
		Code       string  `json:"code"`
		Label      string  `json:"label"`
		MarketCode *string `json:"market_code"`
		ParentCode *string `json:"parent_code"`
	}
	type set struct {
		Set   string `json:"set"`
		Items []item `json:"items"`
	}
	rec := e.get("/v1/reference/regions?market=ZW")
	wantStatus(t, rec, http.StatusOK)
	regions := decode[set](t, rec)
	want := []string{"harare", "bulawayo", "manicaland", "mashonaland-central", "mashonaland-east", "mashonaland-west", "masvingo", "matabeleland-north", "matabeleland-south", "midlands"}
	if len(regions.Items) != len(want) {
		t.Fatalf("%d regions, want %d", len(regions.Items), len(want))
	}
	for i, it := range regions.Items {
		if it.Code != want[i] || it.MarketCode == nil || *it.MarketCode != "ZW" || it.ParentCode != nil || it.Label != devseed.ZimbabweProvinces[i] {
			t.Fatalf("regions[%d] = %+v", i, it)
		}
	}
	// Planned markets have no regions yet.
	if got := decode[set](t, e.get("/v1/reference/regions?market=ZA")); len(got.Items) != 0 {
		t.Fatalf("ZA has %d regions, want none", len(got.Items))
	}
	// Every city names its region; every suburb still names its city.
	cities := decode[set](t, e.get("/v1/reference/cities"))
	for _, c := range cities.Items {
		if c.MarketCode == nil || *c.MarketCode != "ZW" {
			continue // the United Kingdom's cities are checked in markets_test.go
		}
		if c.ParentCode == nil || *c.ParentCode != devseed.CityRegions[c.Code] {
			t.Fatalf("city %s parent = %v, want %s", c.Code, c.ParentCode, devseed.CityRegions[c.Code])
		}
	}
	if got := devseed.CityRegions["mutare"] + "/" + devseed.CityRegions["victoria-falls"]; got != "manicaland/matabeleland-north" {
		t.Fatalf("city regions = %s", got)
	}
	known := map[string]bool{}
	for _, c := range cities.Items {
		known[c.Code] = true
	}
	for _, s := range decode[set](t, e.get("/v1/reference/suburbs")).Items {
		if s.ParentCode == nil || !known[*s.ParentCode] {
			t.Fatalf("suburb %s parent = %v", s.Code, s.ParentCode)
		}
	}
}

func TestListingsByCountryAndRegion(t *testing.T) {
	e := newEnv(t)
	all := e.seeded()
	inRegion := func(region string) func(listings.Record) bool {
		return func(r listings.Record) bool { return devseed.CityRegions[r.City] == region }
	}
	sale := func(r listings.Record) bool { return r.Deal == "sale" }

	zw := count(all, func(r listings.Record) bool { return r.MarketCode == "ZW" })
	if got := e.search("?country=ZW&page_size=48"); got.Total != zw || zw == len(all) {
		t.Fatalf("country=ZW: %d, want the %d Zimbabwean listings of %d", got.Total, zw, len(all))
	}
	if got := e.search("?market=zw&page_size=48"); got.Total != zw {
		t.Fatalf("market=zw (alias, any case): %d, want %d", got.Total, zw)
	}
	// A planned market exists but has no listings.
	if got := e.search("?country=ZA"); got.Total != 0 || got.Facets.Price.Currency != "ZAR" {
		t.Fatalf("country=ZA: total %d, price currency %s", got.Total, got.Facets.Price.Currency)
	}
	for _, region := range []string{"harare", "bulawayo", "manicaland", "matabeleland-north", "midlands"} {
		want := count(all, inRegion(region))
		got := e.search("?region=" + region + "&page_size=48")
		if got.Total != want {
			t.Fatalf("region=%s: %d, want %d", region, got.Total, want)
		}
		for _, l := range got.Items {
			if l.Region == nil || l.Region.Code != region || l.Country != (listings.Country{Code: "ZW", Name: "Zimbabwe"}) || l.MarketCode != "ZW" {
				t.Fatalf("region=%s returned %s with region %+v country %+v", region, l.Slug, l.Region, l.Country)
			}
		}
	}
	want := count(all, func(r listings.Record) bool { return inRegion("harare")(r) && sale(r) })
	got := e.search("?country=ZW&region=harare&deal=sale&page_size=48")
	if got.Total != want || want == 0 {
		t.Fatalf("country=ZW&region=harare&deal=sale: %d, want %d", got.Total, want)
	}

	// Facets: each ignores its own filter. With a region selected, the region
	// facet still offers the others, and the city facet only that region's cities.
	regionFacet := map[string]int{}
	for _, f := range got.Facets.Region {
		regionFacet[f.Code] = f.Count
	}
	for _, region := range []string{"harare", "bulawayo", "matabeleland-north"} {
		if want := count(all, func(r listings.Record) bool { return inRegion(region)(r) && sale(r) }); regionFacet[region] != want {
			t.Fatalf("region facet %s = %d, want %d (%s)", region, regionFacet[region], want, facet(got.Facets.Region))
		}
	}
	for _, f := range got.Facets.Region {
		if f.Label == f.Code || f.City != nil {
			t.Fatalf("region facet value %+v", f)
		}
	}
	if len(got.Facets.Country) != 1 || got.Facets.Country[0].Code != "ZW" || got.Facets.Country[0].Label != "Zimbabwe" || got.Facets.Country[0].Count != want {
		t.Fatalf("country facet = %+v", got.Facets.Country)
	}
	for _, f := range got.Facets.City {
		if devseed.CityRegions[f.Code] != "harare" {
			t.Fatalf("city facet offers %s, which is not in the selected region", f.Code)
		}
	}
	for _, f := range got.Facets.Suburb {
		if f.City == nil || *f.City != "harare" {
			t.Fatalf("suburb facet value %s lacks its city: %+v", f.Code, f.City)
		}
	}
	// Existing fields are unchanged: the old facets are all still there.
	body := e.get("/v1/listings").Body.String()
	for _, key := range []string{`"deal":[`, `"category":[`, `"property_type":[`, `"city":[`, `"suburb":[`, `"bedrooms":[`, `"features":[`, `"price":{`, `"country":[`, `"region":[`, `"market_code":"ZW"`, `"country":{"code":"ZW","name":"Zimbabwe"}`, `"region":{"code":`} {
		if !strings.Contains(body, key) {
			t.Fatalf("GET /v1/listings lacks %s", key)
		}
	}
	// A listing by slug carries them too.
	one := e.get("/v1/listings/" + all[0].Slug).Body.String()
	if !strings.Contains(one, `"country":{"code":"ZW","name":"Zimbabwe"}`) || !strings.Contains(one, `"region":{"code":"`+devseed.CityRegions[all[0].City]+`"`) {
		t.Fatalf("listing detail lacks region / country: %s", one)
	}
}

func TestListingsLocationHierarchyErrors(t *testing.T) {
	e := newEnv(t)
	for query, field := range map[string]string{
		"?region=bulawayo&city=harare":           "city",
		"?country=ZA&region=harare":              "region",
		"?country=ZA&city=harare":                "city",
		"?city=bulawayo&suburb=borrowdale":       "suburb",
		"?region=bulawayo&suburb=borrowdale":     "suburb",
		"?country=ZA&suburb=borrowdale":          "suburb",
		"?region=atlantis":                       "region",
		"?country=XX":                            "country",
		"?country=Zimbabwe":                      "country",
		"?country=ZW&market=ZA":                  "market",
		"?floor_area_min=500&floor_area_max=100": "floor_area_max",
		"?bedrooms_min=5&bedrooms_max=2":         "bedrooms_max",
		"?listed_within_days=0":                  "listed_within_days",
		"?advertiser_type=owner":                 "advertiser_type",
		"?has_photos=1":                          "has_photos",
		"?sort=cheapest":                         "sort",
	} {
		env := wantError(t, e.get("/v1/listings"+query), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields[field]; !ok || len(env.Error.Fields) != 1 {
			t.Fatalf("%s: fields = %v, want only %q", query, env.Error.Fields, field)
		}
	}
	// The whole chain, consistent: fine.
	if got := e.search("?country=ZW&region=harare&city=harare&suburb=borrowdale"); got.Total < 1 {
		t.Fatalf("a consistent chain returned %d", got.Total)
	}
}

func TestListingsAdvancedFilters(t *testing.T) {
	e := newEnv(t)
	all := e.seeded()
	check := func(query string, keep func(listings.Record) bool) {
		t.Helper()
		want := count(all, keep)
		got := e.search(query + "&page_size=48")
		if got.Total != want || len(got.Items) != want {
			t.Fatalf("%s: %d listings, want %d", query, got.Total, want)
		}
		byID := map[string]listings.Record{}
		for _, r := range all {
			byID[r.ID.String()] = r
		}
		for _, l := range got.Items {
			if !keep(byID[l.ID.String()]) {
				t.Fatalf("%s returned %s, which does not match", query, l.Slug)
			}
		}
	}
	atMost := func(v *int, max int) bool { return v != nil && *v <= max }
	atLeast := func(v *int, min int) bool { return v != nil && *v >= min }
	check("?bedrooms_max=2", func(r listings.Record) bool { return atMost(r.Bedrooms, 2) })
	check("?bathrooms_max=1", func(r listings.Record) bool { return atMost(r.Bathrooms, 1) })
	check("?floor_area_min=200", func(r listings.Record) bool { return atLeast(r.FloorArea, 200) })
	check("?floor_area_max=150", func(r listings.Record) bool { return atMost(r.FloorArea, 150) })
	check("?land_area_min=1000", func(r listings.Record) bool { return atLeast(r.LandArea, 1000) })
	check("?land_area_max=1000", func(r listings.Record) bool { return atMost(r.LandArea, 1000) })
	check("?advertiser_type=agency", func(r listings.Record) bool { return r.Advertiser.Type == listings.AdvertiserAgency })
	check("?advertiser_type=private", func(r listings.Record) bool { return r.Advertiser.Type == listings.AdvertiserPrivateSeller })
	check("?has_photos=true", func(r listings.Record) bool { return len(r.Photos) > 0 })
	for _, days := range []int{3, 10, 30, 365} {
		since := e.now.Add(-time.Duration(days) * 24 * time.Hour)
		check(fmt.Sprintf("?listed_within_days=%d", days), func(r listings.Record) bool { return !r.PublishedAt.Before(since) })
	}
	since := e.now.Add(-30 * 24 * time.Hour)
	check("?floor_area_min=200&listed_within_days=30", func(r listings.Record) bool {
		return atLeast(r.FloorArea, 200) && !r.PublishedAt.Before(since)
	})
	check("?country=ZW&region=harare&deal=sale&bedrooms_min=3&bedrooms_max=5&floor_area_min=150&advertiser_type=agency&has_photos=true", func(r listings.Record) bool {
		return devseed.CityRegions[r.City] == "harare" && r.Deal == "sale" && atLeast(r.Bedrooms, 3) && atMost(r.Bedrooms, 5) &&
			atLeast(r.FloorArea, 150) && r.Advertiser.Type == listings.AdvertiserAgency && len(r.Photos) > 0
	})
	if count(all, func(r listings.Record) bool { return r.Advertiser.Type == listings.AdvertiserPrivateSeller }) != 1 {
		t.Fatal("the sample data should have exactly one private seller listing")
	}

	// reference: exact, whatever the case.
	ref := all[3].Reference
	for _, raw := range []string{ref, strings.ToLower(ref)} {
		got := e.search("?reference=" + raw)
		if got.Total != 1 || got.Items[0].Reference != ref {
			t.Fatalf("reference=%s: %d listings", raw, got.Total)
		}
	}
	if got := e.search("?reference=" + ref[:len(ref)-1]); got.Total != 0 {
		t.Fatalf("a partial reference matched %d listings", got.Total)
	}

	// New sort orders.
	oldest := e.search("?sort=oldest&page_size=48").Items
	for i := 1; i < len(oldest); i++ {
		if oldest[i].PublishedAt.Before(oldest[i-1].PublishedAt) {
			t.Fatal("sort=oldest is not oldest first")
		}
	}
	area := e.search("?sort=area_desc&page_size=48").Items
	for i := 1; i < len(area); i++ {
		prev, cur := area[i-1].FloorArea, area[i].FloorArea
		if prev == nil && cur != nil || prev != nil && cur != nil && *cur > *prev {
			t.Fatalf("sort=area_desc: %v before %v", prev, cur)
		}
	}
}

func TestSuggestRegionAndCountry(t *testing.T) {
	e := newEnv(t)
	type suggestions struct {
		Suggestions []listings.Suggestion `json:"suggestions"`
	}
	get := func(q string) []listings.Suggestion {
		rec := e.get("/v1/listings/suggest?q=" + q)
		wantStatus(t, rec, http.StatusOK)
		return decode[suggestions](t, rec).Suggestions
	}
	// Matabeleland North has listings (Victoria Falls); Matabeleland South has none.
	mata := get("mata")
	if len(mata) != 1 || mata[0] != (listings.Suggestion{Type: "region", Label: "Matabeleland North, Zimbabwe", Value: "matabeleland-north"}) {
		t.Fatalf("q=mata: %+v", mata)
	}
	if got := e.search("?region=" + mata[0].Value); got.Total < 1 {
		t.Fatal("the suggested region has no listings")
	}
	zim := get("zimb")
	if len(zim) < 1 || zim[0] != (listings.Suggestion{Type: "country", Label: "Zimbabwe", Value: "ZW"}) {
		t.Fatalf("q=zimb: %+v", zim)
	}
	// A city, then the region of the same name, before any listing.
	bul := get("bulaw")
	if len(bul) < 2 || bul[0].Type != "city" || bul[1] != (listings.Suggestion{Type: "region", Label: "Bulawayo, Zimbabwe", Value: "bulawayo"}) {
		t.Fatalf("q=bulaw: %+v", bul)
	}
	if got := get("mashona"); len(got) != 0 {
		t.Fatalf("regions without listings were suggested: %+v", got)
	}
}

func TestDirectoryByCountryAndRegion(t *testing.T) {
	e := newEnv(t)
	page := func(query string) directoryPage {
		t.Helper()
		rec := e.get("/v1/directory/" + query)
		wantStatus(t, rec, http.StatusOK)
		return decode[directoryPage](t, rec)
	}
	all := page("agencies?country=ZW&page_size=48")
	byRegion := map[string]int{}
	for _, p := range all.Items {
		region := devseed.CityRegions[p.City.Code]
		byRegion[region]++
		if p.Region == nil || p.Region.Code != region || p.Region.Label == "" || p.Country == nil || *p.Country != (listings.Country{Code: "ZW", Name: "Zimbabwe"}) {
			t.Fatalf("profile %s: region %+v country %+v", p.Slug, p.Region, p.Country)
		}
	}
	if len(byRegion) < 2 {
		t.Fatalf("the sample agencies should be in more than one region: %v", byRegion)
	}
	for region, want := range byRegion {
		got := page("agencies?region=" + region + "&page_size=48")
		if got.Total != want {
			t.Fatalf("agencies?region=%s: %d, want %d", region, got.Total, want)
		}
		for _, p := range got.Items {
			if p.Region.Code != region {
				t.Fatalf("agencies?region=%s returned %s in %s", region, p.Slug, p.Region.Code)
			}
		}
	}
	if got := page("agencies?country=zw&page_size=48"); got.Total != all.Total {
		t.Fatalf("country=zw: %d, want %d", got.Total, all.Total)
	}
	if got := page("agencies?market=ZW&region=harare&city=harare"); got.Total != byRegion["harare"] {
		t.Fatalf("country + region + city: %d, want %d", got.Total, byRegion["harare"])
	}
	if got := page("agencies?country=ZA"); got.Total != 0 {
		t.Fatalf("country=ZA: %d agencies, want none", got.Total)
	}
	if got := page("agencies?region=midlands"); got.Total != 0 {
		t.Fatalf("region=midlands: %d agencies, want none", got.Total)
	}
	// The profile detail carries them too.
	one := e.get("/v1/directory/agencies/" + all.Items[0].Slug).Body.String()
	if !strings.Contains(one, `"country":{"code":"ZW","name":"Zimbabwe"}`) || !strings.Contains(one, `"region":{"code":"`) {
		t.Fatalf("profile detail lacks region / country: %s", one)
	}
	for query, field := range map[string]string{
		"agencies?region=atlantis":             "region",
		"agencies?country=XX":                  "country",
		"agencies?country=ZA&region=harare":    "region",
		"agencies?region=bulawayo&city=harare": "city",
		"agencies?country=ZW&market=ZA":        "market",
	} {
		env := wantError(t, e.get("/v1/directory/"+query), http.StatusBadRequest, httpx.CodeValidationFailed)
		if _, ok := env.Error.Fields[field]; !ok {
			t.Fatalf("%s: fields = %v, want %q", query, env.Error.Fields, field)
		}
	}
}
