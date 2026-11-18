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

func floatp(f float64) *float64 { return &f }

// The coordinates of a PATCH: types, ranges, decimals, both-or-neither,
// (0, 0) and the precision values.
func TestLocationValidation(t *testing.T) {
	cases := []struct {
		name   string
		before Content
		patch  string
		// want maps a field to its message; empty means valid.
		want map[string]string
	}{
		{"both", Content{}, `{"latitude": -17.8252, "longitude": 31.0335}`, nil},
		{"both with six decimals", Content{}, `{"latitude": -17.825201, "longitude": 31.033512}`, nil},
		{"trailing zeros are fine", Content{}, `{"latitude": -17.8250000000, "longitude": 31.03}`, nil},
		{"exponent within six decimals", Content{}, `{"latitude": 1.5e1, "longitude": 3e-6}`, nil},
		{"edges", Content{}, `{"latitude": -90, "longitude": 180}`, nil},
		{"other edges", Content{}, `{"latitude": 90, "longitude": -180}`, nil},
		{"latitude 0 alone is fine", Content{}, `{"latitude": 0, "longitude": 31.1}`, nil},
		{"longitude 0 alone is fine", Content{}, `{"latitude": 51.5, "longitude": 0}`, nil},
		{"neither", Content{}, `{"latitude": null, "longitude": null}`, nil},
		{"clear both", Content{Latitude: floatp(1), Longitude: floatp(2)}, `{"latitude": null, "longitude": null}`, nil},
		{"move one coordinate", Content{Latitude: floatp(1), Longitude: floatp(2)}, `{"latitude": 1.5}`, nil},
		{"approximate", Content{}, `{"latitude": 1, "longitude": 2, "location_precision": "APPROXIMATE"}`, nil},
		{"exact", Content{}, `{"location_precision": "EXACT"}`, nil},
		{"precision null is exact", Content{}, `{"location_precision": null}`, nil},
		{"precision without coordinates", Content{}, `{"location_precision": "APPROXIMATE"}`, nil},

		{"latitude too high", Content{}, `{"latitude": 90.000001, "longitude": 1}`, map[string]string{"latitude": MsgLatitudeRange}},
		{"latitude too low", Content{}, `{"latitude": -91, "longitude": 1}`, map[string]string{"latitude": MsgLatitudeRange}},
		{"longitude too high", Content{}, `{"latitude": 1, "longitude": 180.5}`, map[string]string{"longitude": MsgLongitudeRange}},
		{"longitude too low", Content{}, `{"latitude": 1, "longitude": -181}`, map[string]string{"longitude": MsgLongitudeRange}},
		{"latitude only", Content{}, `{"latitude": 1}`, map[string]string{"longitude": MsgCoordinatePair}},
		{"longitude only", Content{}, `{"longitude": 1}`, map[string]string{"latitude": MsgCoordinatePair}},
		{"clear one of two", Content{Latitude: floatp(1), Longitude: floatp(2)}, `{"longitude": null}`, map[string]string{"longitude": MsgCoordinatePair}},
		{"null island", Content{}, `{"latitude": 0, "longitude": 0}`, map[string]string{"latitude": MsgNullIsland}},
		{"null island with decimals", Content{}, `{"latitude": 0.0, "longitude": -0.000000}`, map[string]string{"latitude": MsgNullIsland}},
		{"seven decimals", Content{}, `{"latitude": -17.8252011, "longitude": 31.03}`, map[string]string{"latitude": MsgCoordinateDigits}},
		{"seven decimals by exponent", Content{}, `{"latitude": 1, "longitude": 1e-7}`, map[string]string{"longitude": MsgCoordinateDigits}},
		{"a string", Content{}, `{"latitude": "-17.8", "longitude": 31}`, map[string]string{"latitude": MsgCoordinateNumber}},
		{"a bool", Content{}, `{"latitude": 1, "longitude": true}`, map[string]string{"longitude": MsgCoordinateNumber}},
		{"an object", Content{}, `{"latitude": {"value": 1}, "longitude": 1}`, map[string]string{"latitude": MsgCoordinateNumber}},
		{"huge", Content{}, `{"latitude": 1e400, "longitude": 1}`, map[string]string{"latitude": MsgCoordinateNumber}},
		{"precision lower case", Content{}, `{"location_precision": "approximate"}`, map[string]string{"location_precision": MsgPrecision}},
		{"precision unknown", Content{}, `{"location_precision": "CITY"}`, map[string]string{"location_precision": MsgPrecision}},
		{"precision empty", Content{}, `{"location_precision": ""}`, map[string]string{"location_precision": MsgPrecision}},
		{"precision a number", Content{}, `{"location_precision": 2}`, map[string]string{"location_precision": MsgPrecision}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(c.patch), &raw); err != nil {
				t.Fatal(err)
			}
			fields := map[string]string{}
			content := c.before
			applyPatch(&content, decodePatch(raw, fields), fields)
			if len(fields) == 0 {
				validateLocation(content, fields)
			}
			if len(c.want) == 0 {
				if len(fields) != 0 {
					t.Fatalf("fields = %v, want none", fields)
				}
				return
			}
			for k, v := range c.want {
				if fields[k] != v {
					t.Fatalf("fields = %v, want %s = %q", fields, k, v)
				}
			}
		})
	}
}

func TestRoundCoordinate(t *testing.T) {
	for _, c := range []struct {
		in   float64
		dec  int
		want float64
	}{
		{-18.951234, 2, -18.95}, {32.657891, 2, 32.66}, {1.005, 2, 1.01}, {-1.005, 2, -1.01}, {1.004999, 2, 1.0},
		{0.004999, 2, 0}, {-0.005, 2, -0.01}, {89.999, 2, 90}, {-179.996, 2, -180}, {51.5073519, 6, 51.507352},
		{-17.825201, 6, -17.825201},
	} {
		if got := RoundCoordinate(c.in, c.dec); got != c.want {
			t.Errorf("RoundCoordinate(%v, %d) = %v, want %v", c.in, c.dec, got, c.want)
		}
	}
}

func TestPublicLocation(t *testing.T) {
	if (Record{}).PublicLocation() != nil || (Record{Latitude: floatp(1)}).PublicLocation() != nil {
		t.Fatal("no coordinates, no location")
	}
	exact := Record{Latitude: floatp(-17.825201), Longitude: floatp(31.033512)}
	if l := exact.PublicLocation(); l == nil || *l != (Location{-17.825201, 31.033512, PrecisionExact}) {
		t.Fatalf("exact: %+v", l)
	}
	approx := Record{Latitude: floatp(-18.951234), Longitude: floatp(32.657891), LocationPrecision: PrecisionApproximate}
	if l := approx.PublicLocation(); l == nil || *l != (Location{-18.95, 32.66, PrecisionApproximate}) {
		t.Fatalf("approximate: %+v", l)
	}
}

// approximateRecord is a public APPROXIMATE listing whose exact coordinates
// have digits that would show in any output that leaked them.
func approximateRecord() Record {
	r := sampleRecord()
	r.Latitude, r.Longitude, r.LocationPrecision = floatp(-17.783456), floatp(31.067891), PrecisionApproximate
	return r
}

// leaks reports whether raw carries a digit run of the exact coordinates.
func leaks(raw string) bool {
	for _, s := range []string{"783456", "067891", "-17.783", "31.067", "17.7834", "31.0678"} {
		if strings.Contains(raw, s) {
			return true
		}
	}
	return false
}

// An APPROXIMATE listing never shows its exact coordinates publicly: not on
// the listing, not on a search card (either adapter), not in the index
// document, not in a directory listing; only the manage view has them.
func TestApproximateLocationNeverLeaks(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	r := approximateRecord()
	store.Put(r)
	svc := NewService(store, store, noLabels{})
	svc.SetClock(func() time.Time { return esNow })
	ctx := context.Background()

	l, err := svc.BySlug(ctx, r.Slug)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(l)
	if leaks(string(raw)) || !strings.Contains(string(raw), `"location":{"latitude":-17.78,"longitude":31.07,"precision":"APPROXIMATE"}`) {
		t.Fatalf("detail: %s", raw)
	}

	for _, q := range []Query{
		{Sort: SortNewest, Page: 1, PageSize: 12},
		{Sort: SortNewest, Page: 1, PageSize: 12, HasLocation: true},
		{Sort: SortNewest, Page: 1, PageSize: 12, BBox: &BBox{West: 31, South: -18, East: 31.1, North: -17.7}},
	} {
		page, err := svc.Find(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(page)
		if len(page.Items) != 1 || leaks(string(raw)) || !strings.Contains(string(raw), `"latitude":-17.78`) {
			t.Fatalf("search %+v: %s", q, raw)
		}
	}

	es := &fakeES{respond: func(string, string, []byte) (int, string) {
		return 200, `{"hits":{"total":{"value":1},"hits":[{"_id":"` + r.ID.String() + `"}]},"aggregations":{"mappable":{"doc_count":1}}}`
	}}
	elastic := NewService(store, NewElasticSearch(es, "rp365-test", time.Second), noLabels{})
	elastic.SetClock(func() time.Time { return esNow })
	page, err := elastic.Find(ctx, Query{Sort: SortNewest, Page: 1, PageSize: 12})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(page)
	if leaks(string(raw)) || page.MappableTotal != 1 {
		t.Fatalf("elasticsearch: %s", raw)
	}

	doc, _ := json.Marshal(BuildDocument(r, testLabels))
	if leaks(string(doc)) || !strings.Contains(string(doc), `"location":[31.07,-17.78]`) {
		t.Fatalf("index document: %s", doc)
	}

	presented, err := svc.Present(ctx, []Record{r})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(presented)
	if leaks(string(raw)) {
		t.Fatalf("present: %s", raw)
	}

	// The lister's own view keeps the exact values.
	view, _ := json.Marshal(contentView(ContentOf(r)))
	if !strings.Contains(string(view), `"latitude":-17.783456`) || !strings.Contains(string(view), `"location_precision":"APPROXIMATE"`) {
		t.Fatalf("manage view: %s", view)
	}
}

func TestParseBBox(t *testing.T) {
	box, problem := ParseBBox("30.9,-17.9,31.2,-17.7")
	if problem != "" || *box != (BBox{West: 30.9, South: -17.9, East: 31.2, North: -17.7}) {
		t.Fatalf("%+v %q", box, problem)
	}
	if box, problem := ParseBBox(" -180 , -90 , 180 , 90 "); problem != "" || *box != (BBox{-180, -90, 180, 90}) {
		t.Fatalf("whole world: %+v %q", box, problem)
	}
	for _, bad := range []string{
		"", "1,2,3", "1,2,3,4,5", "a,b,c,d", "1,,3,4", "NaN,1,2,3", "Inf,1,2,3", "1,2,3,4;", "0x1,2,3,4",
		"-181,0,10,10", "0,0,181,10", "0,-91,10,10", "0,0,10,91",
		"0,10,10,5",     // south > north
		"0,10,10,10",    // south = north
		"170,0,-170,10", // across the antimeridian
		"10,0,10,5",     // west = east
	} {
		if box, problem := ParseBBox(bad); problem == "" || box != nil {
			t.Errorf("ParseBBox(%q) = %+v, want an error", bad, box)
		}
	}
	// Through the handler: a 400 field error on bbox.
	for _, bad := range []string{"1,2,3", "0,10,10,5", "170,0,-170,10", "x,0,1,1"} {
		_, fields := parseQuery(url.Values{"bbox": {bad}})
		if fields["bbox"] == "" {
			t.Errorf("bbox=%s: fields %v", bad, fields)
		}
	}
	q, fields := parseQuery(url.Values{"bbox": {"30.9,-17.9,31.2,-17.7"}, "has_location": {"true"}})
	if len(fields) != 0 || q.BBox == nil || !q.HasLocation {
		t.Fatalf("parsed: %+v %v", q, fields)
	}
	if _, fields := parseQuery(url.Values{"has_location": {"yes"}}); fields["has_location"] == "" {
		t.Fatalf("has_location=yes: %v", fields)
	}
	if q, fields := parseQuery(url.Values{"bbox": {""}}); len(fields) != 0 || q.BBox != nil {
		t.Fatalf("an empty bbox is no box: %+v %v", q.BBox, fields)
	}
}

// mapRecords are public listings at known places: Harare (exact), an
// APPROXIMATE one just inside a box edge only by its rounded position,
// Bulawayo (exact) and one without coordinates.
func mapRecords() (harare, approx, bulawayo, none Record) {
	harare = sampleRecord()
	harare.Latitude, harare.Longitude = floatp(-17.80), floatp(31.05)

	approx = sampleRecord()
	approx.ID, approx.Slug, approx.Reference = uuid.New(), "approximate-plot", "RP-ZW-20002"
	// Exact -17.696 rounds to -17.70: inside a box whose north edge is -17.70.
	approx.Latitude, approx.Longitude, approx.LocationPrecision = floatp(-17.696), floatp(31.1), PrecisionApproximate
	approx.Deal = "rent"
	approx.PricePeriod = PricePeriodMonth

	bulawayo = sampleRecord()
	bulawayo.ID, bulawayo.Slug, bulawayo.Reference = uuid.New(), "bulawayo-house", "RP-ZW-20003"
	bulawayo.Latitude, bulawayo.Longitude = floatp(-20.15), floatp(28.58)

	none = sampleRecord()
	none.ID, none.Slug, none.Reference = uuid.New(), "no-coordinates", "RP-ZW-20004"
	return
}

func TestMemorySearchBBoxHasLocationAndMappableTotal(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	harare, approx, bulawayo, none := mapRecords()
	for _, r := range []Record{harare, approx, bulawayo, none} {
		store.Put(r)
	}
	svc := NewService(store, store, noLabels{})
	svc.SetClock(func() time.Time { return esNow })
	ctx := context.Background()
	find := func(q Query) Page {
		t.Helper()
		q.Sort, q.Page, q.PageSize = SortNewest, 1, 12
		page, err := svc.Find(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		return page
	}
	slugs := func(p Page) map[string]bool {
		out := map[string]bool{}
		for _, l := range p.Items {
			out[l.Slug] = true
		}
		return out
	}

	all := find(Query{})
	if all.Total != 4 || all.MappableTotal != 3 {
		t.Fatalf("all: total %d, mappable %d", all.Total, all.MappableTotal)
	}
	for _, l := range all.Items {
		if (l.Location == nil) != (l.Slug == none.Slug) {
			t.Fatalf("%s: location %+v", l.Slug, l.Location)
		}
	}

	mappable := find(Query{HasLocation: true})
	if mappable.Total != 3 || mappable.MappableTotal != 3 || slugs(mappable)[none.Slug] {
		t.Fatalf("has_location: %+v", slugs(mappable))
	}

	// Harare's box: the exact listing, and the APPROXIMATE one by its
	// public (rounded) position -17.70 on the north edge; its exact -17.696
	// is outside, which must not matter.
	box := &BBox{West: 30.9, South: -17.9, East: 31.2, North: -17.70}
	harareBox := find(Query{BBox: box})
	if got := slugs(harareBox); len(got) != 2 || !got[harare.Slug] || !got[approx.Slug] || harareBox.MappableTotal != 2 {
		t.Fatalf("harare box: %v, mappable %d", got, harareBox.MappableTotal)
	}
	// A box that holds the APPROXIMATE listing's exact position but not
	// its public one finds nothing: the public position is what counts.
	if got := find(Query{BBox: &BBox{West: 31.09, South: -17.699, East: 31.11, North: -17.69}}); got.Total != 0 {
		t.Fatalf("exact-only box: %+v", slugs(got))
	}
	// Facets reflect the box: Bulawayo is not counted.
	if harareBox.Facets.Deal == nil {
		t.Fatal("no deal facet")
	}
	deals := map[string]int{}
	for _, f := range harareBox.Facets.Deal {
		deals[f.Code] = f.Count
	}
	if deals["sale"] != 1 || deals["rent"] != 1 {
		t.Fatalf("deal facet in the box: %v", deals)
	}
	// Other filters still apply inside the box.
	if got := find(Query{BBox: box, Deal: "rent"}); got.Total != 1 || !slugs(got)[approx.Slug] {
		t.Fatalf("box and deal: %v", slugs(got))
	}
	// Bulawayo only.
	if got := find(Query{BBox: &BBox{West: 28, South: -21, East: 29, North: -20}}); got.Total != 1 || !slugs(got)[bulawayo.Slug] {
		t.Fatalf("bulawayo: %v", slugs(got))
	}
}

// The Elasticsearch adapter: geo_bounding_box on location and exists for
// has_location in the main filter (so facets reflect them), and
// mappable_total from the "mappable" aggregation.
func TestElasticSearchMapQuery(t *testing.T) {
	body := BuildSearchBody(Query{
		BBox: &BBox{West: 30.9, South: -17.9, East: 31.2, North: -17.7}, HasLocation: true, Deal: "sale",
		Sort: SortNewest, Page: 1, PageSize: 12,
	}, esNow)
	raw, _ := json.Marshal(body)
	var parsed struct {
		Query struct {
			Bool struct {
				Filter []map[string]json.RawMessage `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
		Aggs map[string]struct {
			Filter json.RawMessage `json:"filter"`
		} `json:"aggs"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	var geo, exists string
	for _, f := range parsed.Query.Bool.Filter {
		if v, ok := f["geo_bounding_box"]; ok {
			geo = string(v)
		}
		if v, ok := f["exists"]; ok {
			exists = string(v)
		}
	}
	if geo != `{"location":{"bottom_right":{"lat":-17.9,"lon":31.2},"top_left":{"lat":-17.7,"lon":30.9}}}` {
		t.Fatalf("geo_bounding_box = %s", geo)
	}
	if exists != `{"field":"location"}` {
		t.Fatalf("exists = %s", exists)
	}
	mappable, ok := parsed.Aggs["mappable"]
	if !ok || !strings.Contains(string(mappable.Filter), `"exists":{"field":"location"}`) || !strings.Contains(string(mappable.Filter), `"deal":"sale"`) {
		t.Fatalf("mappable aggregation: %s", mappable.Filter)
	}
	// Without a box there is no geo filter, but the mappable count is there.
	plain, _ := json.Marshal(BuildSearchBody(Query{Sort: SortNewest, Page: 1, PageSize: 12}, esNow))
	if strings.Contains(string(plain), "geo_bounding_box") || !strings.Contains(string(plain), `"mappable"`) {
		t.Fatalf("plain: %s", plain)
	}

	es := &fakeES{respond: func(_, _ string, body []byte) (int, string) {
		if !strings.Contains(string(body), "geo_bounding_box") {
			return 500, `{}`
		}
		return 200, `{"hits":{"total":{"value":20},"hits":[]},"aggregations":{"mappable":{"doc_count":12}}}`
	}}
	hits, err := NewElasticSearch(es, "rp365-test", time.Second).Search(context.Background(), Query{
		BBox: &BBox{West: 30.9, South: -17.9, East: 31.2, North: -17.7}, Sort: SortNewest, Page: 1, PageSize: 12,
	}, esNow)
	if err != nil || hits.Total != 20 || hits.Mappable != 12 {
		t.Fatalf("hits %+v, %v", hits, err)
	}

	// The index document: [lon, lat], public coordinates; none without them.
	harare, _, _, none := mapRecords()
	if d := BuildDocument(harare, testLabels); d.Location == nil || *d.Location != [2]float64{31.05, -17.80} {
		t.Fatalf("document location: %v", d.Location)
	}
	if d := BuildDocument(none, testLabels); d.Location != nil {
		t.Fatalf("no coordinates: %v", d.Location)
	}
}

// The revision content keeps the coordinates; an older revision without
// them reads as none, EXACT.
func TestContentCarriesTheLocation(t *testing.T) {
	r := Record{Latitude: floatp(-17.5), Longitude: floatp(31.25), LocationPrecision: PrecisionApproximate}
	c := ContentOf(r)
	raw, _ := json.Marshal(c)
	var back Content
	if err := json.Unmarshal(raw, &back); err != nil || back.Latitude == nil || *back.Latitude != -17.5 || back.LocationPrecision != PrecisionApproximate {
		t.Fatalf("round trip: %s %+v %v", raw, back, err)
	}
	*c.Latitude = 9
	if *r.Latitude != -17.5 {
		t.Fatal("ContentOf must copy the coordinates")
	}
	var old Content
	if err := json.Unmarshal([]byte(`{"title":"An older revision"}`), &old); err != nil || old.Latitude != nil {
		t.Fatalf("old revision: %+v %v", old, err)
	}
	var out Record
	old.ApplyTo(&out)
	if out.Latitude != nil || out.LocationPrecision != PrecisionExact {
		t.Fatalf("ApplyTo old: %+v", out)
	}
	if v := contentView(old); v.LocationPrecision != PrecisionExact || v.Latitude != nil {
		t.Fatalf("view: %+v", v)
	}
	// A location change shows in the reviewer's diff.
	changes := diff(Content{}, Content{Latitude: floatp(1), Longitude: floatp(2), LocationPrecision: PrecisionApproximate})
	got := map[string]bool{}
	for _, ch := range changes {
		got[ch.Field] = true
	}
	if !got["latitude"] || !got["longitude"] || !got["location_precision"] {
		t.Fatalf("diff: %+v", changes)
	}
	for _, m := range completeness(Content{}, nil, nil, Geography{}).Missing {
		if strings.Contains(m.Field, "latitude") || strings.Contains(m.Field, "location") {
			t.Fatalf("completeness asks for a location: %+v", m)
		}
	}
}
