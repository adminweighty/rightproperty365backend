package listings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/outbox"
)

var updateGolden = flag.Bool("update", false, "rewrite the golden files in testdata/es")

var esNow = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// fakeES is a stand-in for an Elasticsearch cluster. NOTHING in these tests
// talks to a real one: the adapter has never run against Elasticsearch.
type fakeES struct {
	requests []fakeRequest
	// respond returns the status and body for a request; nil means 200 {}.
	respond func(method, path string, body []byte) (int, string)
	err     error
}

type fakeRequest struct {
	Method, Path, ContentType string
	Body                      []byte
}

func (f *fakeES) Perform(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	f.requests = append(f.requests, fakeRequest{Method: req.Method, Path: path, ContentType: req.Header.Get("Content-Type"), Body: body})
	if f.err != nil {
		return nil, f.err
	}
	status, out := http.StatusOK, "{}"
	if f.respond != nil {
		status, out = f.respond(req.Method, path, body)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(out)), Header: http.Header{}}, nil
}

func (f *fakeES) calls() []string {
	out := make([]string, 0, len(f.requests))
	for _, r := range f.requests {
		out = append(out, r.Method+" "+r.Path)
	}
	return out
}

func intp(n int) *int { return &n }

func rat(s string) *big.Rat {
	r, _ := new(big.Rat).SetString(s)
	return r
}

// Golden files: the exact Elasticsearch request for representative queries.
// Regenerate deliberately with: go test ./internal/modules/listings -update
func TestSearchBodyGoldenFiles(t *testing.T) {
	featured := true
	cases := map[string]J{
		"default": BuildSearchBody(Query{Sort: SortNewest, Page: 1, PageSize: 12}, esNow),
		"text":    BuildSearchBody(Query{Q: "Borowdale house", Sort: SortRelevance, Page: 1, PageSize: 12}, esNow),
		"all-filters": BuildSearchBody(Query{
			Q: "garden", Deal: "sale", Category: "residential", PropertyType: "house", City: "harare", Suburb: "borrowdale",
			MinPrice: rat("100000"), MaxPrice: rat("400000.50"), Currency: "USD", BedroomsMin: intp(3), BathroomsMin: intp(2),
			Features: []string{"borehole", "solar-backup"}, Featured: &featured, Sort: SortPriceAsc, Page: 3, PageSize: 24,
		}, esNow),
		"suggest": BuildSuggestBody("bor", 8, esNow),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := json.MarshalIndent(body, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			got = append(got, '\n')
			path := filepath.Join("testdata", "es", name+".json")
			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("missing golden file (run with -update): %v", err)
			}
			if strings.ReplaceAll(string(want), "\r\n", "\n") != string(got) {
				t.Fatalf("%s differs from %s; if intended, run: go test ./internal/modules/listings -update", name, path)
			}
		})
	}
}

// The rules that matter, checked structurally rather than by snapshot.
func TestSearchBodyRules(t *testing.T) {
	q := Query{Q: "house", Deal: "sale", City: "harare", MinPrice: rat("1000"), BedroomsMin: intp(2), Features: []string{"borehole"}, Sort: SortRelevance, Page: 2, PageSize: 12}
	raw, _ := json.Marshal(BuildSearchBody(q, esNow))
	body := string(raw)

	// FR-006 is in the query itself, for hits and aggregations alike.
	for _, must := range []string{`{"term":{"status":"PUBLISHED"}}`, `{"range":{"expires_at":{"gt":"2026-11-14T09:00:00.000Z"}}}`} {
		if !strings.Contains(body, `"query":{"bool":{"filter":[`+`{"term":{"status":"PUBLISHED"}},{"range":{"expires_at":{"gt":"2026-11-14T09:00:00.000Z"}}}`) || !strings.Contains(body, must) {
			t.Fatalf("the public rule must lead the main query: %s", body)
		}
	}
	var doc struct {
		From       int            `json:"from"`
		Size       int            `json:"size"`
		Source     bool           `json:"_source"`
		PostFilter map[string]any `json:"post_filter"`
		Aggs       map[string]struct {
			Filter struct {
				Bool struct {
					Filter []map[string]any `json:"filter"`
				} `json:"bool"`
			} `json:"filter"`
		} `json:"aggs"`
		Sort []any `json:"sort"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.From != 12 || doc.Size != 12 || doc.Source {
		t.Fatalf("paging/_source: from=%d size=%d source=%v", doc.From, doc.Size, doc.Source)
	}
	has := func(filters []map[string]any, needle string) bool {
		b, _ := json.Marshal(filters)
		return strings.Contains(string(b), needle)
	}
	// Each facet ignores its own filter and keeps the others.
	if has(doc.Aggs["deal"].Filter.Bool.Filter, `"deal"`) || !has(doc.Aggs["deal"].Filter.Bool.Filter, `"city":"harare"`) {
		t.Fatalf("deal facet filter: %+v", doc.Aggs["deal"].Filter.Bool.Filter)
	}
	if has(doc.Aggs["city"].Filter.Bool.Filter, `"city"`) || !has(doc.Aggs["city"].Filter.Bool.Filter, `"deal":"sale"`) {
		t.Fatalf("city facet filter: %+v", doc.Aggs["city"].Filter.Bool.Filter)
	}
	if has(doc.Aggs["bedrooms"].Filter.Bool.Filter, `"bedrooms"`) || has(doc.Aggs["price"].Filter.Bool.Filter, `"price"`) {
		t.Fatal("bedrooms and price facets must ignore their own filter")
	}
	if !has(doc.Aggs["price"].Filter.Bool.Filter, `"currency":"USD"`) || !has(doc.Aggs["features"].Filter.Bool.Filter, `"deal":"sale"`) {
		t.Fatal("price is per currency; features count within the full result")
	}
	// Prices are compared within one currency only.
	post, _ := json.Marshal(doc.PostFilter)
	if !strings.Contains(string(post), `{"term":{"currency":"USD"}},{"range":{"price":{"gte":1000}}}`) {
		t.Fatalf("post_filter = %s", post)
	}
	// Typo tolerance with a fixed first letter; every word must match.
	if !strings.Contains(body, `"fuzziness":"AUTO"`) || !strings.Contains(body, `"prefix_length":1`) || !strings.Contains(body, `"operator":"and"`) {
		t.Fatalf("text query: %s", body)
	}
	if first, ok := doc.Sort[0].(string); !ok || first != "_score" {
		t.Fatalf("relevance with q sorts by _score first: %v", doc.Sort)
	}
	// Deep paging is capped under Elasticsearch's result window.
	deep, _ := json.Marshal(BuildSearchBody(Query{Page: 100000, PageSize: 48, Sort: SortNewest}, esNow))
	if !strings.Contains(string(deep), `"from":9952`) {
		t.Fatalf("deep paging is not capped: %.80s", deep)
	}
	// No text query without q; relevance then starts with featured.
	plain, _ := json.Marshal(BuildSearchBody(Query{Sort: SortRelevance, Page: 1, PageSize: 12}, esNow))
	if strings.Contains(string(plain), "multi_match") || !strings.Contains(string(plain), `"sort":[{"featured":"desc"}`) {
		t.Fatalf("plain relevance: %s", plain)
	}
}

func sampleRecord() Record {
	id := uuid.MustParse("0195c0de-0000-7000-8000-000000000001")
	return Record{
		ID: id, Slug: "four-bedroom-family-home-borrowdale", Reference: "RP-ZW-10001", Title: "Four-bedroom family home with garden",
		Deal: "sale", Category: "residential", PropertyType: "house", Price: Money{Amount: "385000.00", Currency: "USD"},
		Suburb: "borrowdale", City: "harare", MarketCode: "ZW", Bedrooms: intp(4), Bathrooms: intp(3), FloorArea: intp(320), LandArea: intp(2000),
		Summary: "Single-storey home.", Description: "A well-kept home.", Features: []string{"borehole", "solar-backup"}, Featured: true,
		Photos: []Photo{{URL: "/assets/images/x.jpg", Alt: "x", Width: 1, Height: 1}},
		Advertiser: Advertiser{Type: AdvertiserAgency, Name: "Sample Estates", Verified: true,
			Agency: &ProfileRef{Kind: "agencies", Slug: "sample-estates", Name: "Sample Estates"},
			Agent:  &ProfileRef{Kind: "agents", Slug: "tendai-ncube", Name: "Tendai Ncube"}},
		Status: StatusPublished, PublishedAt: esNow.AddDate(0, 0, -4), ExpiresAt: esNow.AddDate(0, 0, 86), UpdatedAt: esNow.AddDate(0, 0, -4),
		OrganisationID: &id, AgentUserID: &id, SellerUserID: &id,
	}
}

var testLabels = map[string]map[string]string{
	SetPropertyTypes: {"house": "House"}, SetCities: {"harare": "Harare"}, SetSuburbs: {"borrowdale": "Borrowdale"},
}

// The index holds public listing data only: nothing about customers,
// contacts, owner accounts or even the advertiser.
func TestDocumentHoldsOnlyPublicListingData(t *testing.T) {
	doc := BuildDocument(sampleRecord(), testLabels)
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	private := regexp.MustCompile(`(?i)phone|e-?mail|address|contact|customer|guest|owner|user|organisation|agent|seller|advertiser|password|token`)
	for key := range fields {
		if private.MatchString(key) {
			t.Fatalf("document field %q looks like private or account data", key)
		}
	}
	for _, private := range []string{"Sample Estates", "Tendai", "tendai-ncube", sampleRecord().OrganisationID.String() + `"}`} {
		if strings.Contains(string(raw), private) && private != sampleRecord().OrganisationID.String()+`"}` {
			t.Fatalf("document contains %q", private)
		}
	}
	if strings.Count(string(raw), sampleRecord().ID.String()) != 1 {
		t.Fatal("the only UUID in a document is the listing's own id (owner ids must not be indexed)")
	}

	// Every field the builder writes exists in the explicit mapping (which is
	// strict: an unmapped field would be rejected by Elasticsearch).
	var mapping struct {
		Mappings struct {
			Dynamic    string                    `json:"dynamic"`
			Properties map[string]map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(IndexMapping, &mapping); err != nil {
		t.Fatal(err)
	}
	props := mapping.Mappings.Properties
	if mapping.Mappings.Dynamic != "strict" {
		t.Fatal("the mapping must be strict")
	}
	for key := range fields {
		if _, ok := props[key]; !ok {
			t.Fatalf("document field %q is not in the mapping", key)
		}
	}
	for field, want := range map[string]string{
		"deal": "keyword", "category": "keyword", "property_type": "keyword", "city": "keyword", "suburb": "keyword", "features": "keyword",
		"market": "keyword", "currency": "keyword", "price": "scaled_float", "bedrooms": "integer", "bathrooms": "integer",
		"published_at": "date", "expires_at": "date", "updated_at": "date", "version": "long", "location": "geo_point", "title": "text",
	} {
		if props[field]["type"] != want {
			t.Fatalf("mapping of %s = %v, want %s", field, props[field]["type"], want)
		}
	}
	for _, field := range []string{"title", "suburb_label", "city_label"} {
		sub, _ := props[field]["fields"].(map[string]any)
		if suggest, _ := sub["suggest"].(map[string]any); suggest["type"] != "search_as_you_type" {
			t.Fatalf("%s lacks its search_as_you_type sub-field", field)
		}
	}

	if doc.Price != 385000 || doc.Currency != "USD" || doc.SuburbLabel != "Borrowdale" || doc.Version != sampleRecord().UpdatedAt.UnixMicro() || doc.Location != nil {
		t.Fatalf("unexpected document: %+v", doc)
	}
}

const searchResponse = `{
  "hits": {"total": {"value": 7, "relation": "eq"}, "hits": [
    {"_id": "0195c0de-0000-7000-8000-000000000001"}, {"_id": "0195c0de-0000-7000-8000-000000000002"}]},
  "aggregations": {
    "deal": {"doc_count": 9, "values": {"buckets": [{"key": "sale", "doc_count": 6}, {"key": "rent", "doc_count": 3}]}},
    "category": {"values": {"buckets": [{"key": "residential", "doc_count": 7}]}},
    "property_type": {"values": {"buckets": [{"key": "house", "doc_count": 5}, {"key": "apartment", "doc_count": 2}]}},
    "city": {"values": {"buckets": [{"key": "harare", "doc_count": 7}]}},
    "suburb": {"values": {"buckets": [{"key": "borrowdale", "doc_count": 1}]}},
    "bedrooms": {"values": {"buckets": [{"key": 3, "doc_count": 4}, {"key": 4, "doc_count": 3}]}},
    "features": {"values": {"buckets": [{"key": "borehole", "doc_count": 5}]}},
    "price": {"min": {"value": 950.0}, "max": {"value": 385000.5}}
  }
}`

func TestElasticSearchAdapterParsesHitsAndAggregations(t *testing.T) {
	es := &fakeES{respond: func(string, string, []byte) (int, string) { return 200, searchResponse }}
	adapter := NewElasticSearch(es, "rp365-test", time.Second)
	hits, err := adapter.Search(context.Background(), Query{Deal: "sale", Sort: SortNewest, Page: 1, PageSize: 12}, esNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(es.requests) != 1 || es.requests[0].Method != http.MethodPost || es.requests[0].Path != "/rp365-test-listings-current/_search" ||
		es.requests[0].ContentType != "application/json" {
		t.Fatalf("request = %+v", es.calls())
	}
	if hits.Total != 7 || len(hits.IDs) != 2 || hits.IDs[0] != uuid.MustParse("0195c0de-0000-7000-8000-000000000001") {
		t.Fatalf("hits = %+v", hits)
	}
	f := hits.Facets
	if f.Deal["sale"] != 6 || f.Deal["rent"] != 3 || f.PropertyType["apartment"] != 2 || f.City["harare"] != 7 || f.Suburb["borrowdale"] != 1 ||
		f.Features["borehole"] != 5 || f.Bedrooms[3] != 4 || f.Bedrooms[4] != 3 || f.Category["residential"] != 7 {
		t.Fatalf("facets = %+v", f)
	}
	if FormatAmount(f.PriceMin) != "950.00" || FormatAmount(f.PriceMax) != "385000.50" {
		t.Fatalf("price facet = %s..%s", FormatAmount(f.PriceMin), FormatAmount(f.PriceMax))
	}

	// Nothing matches.
	es.respond = func(string, string, []byte) (int, string) {
		return 200, `{"hits":{"total":{"value":0},"hits":[]},"aggregations":{"price":{"min":{"value":null},"max":{"value":null}}}}`
	}
	empty, err := adapter.Search(context.Background(), Query{Page: 1, PageSize: 12}, esNow)
	if err != nil || empty.Total != 0 || len(empty.IDs) != 0 || empty.Facets.PriceMin != nil {
		t.Fatalf("empty = %+v, %v", empty, err)
	}
}

func TestElasticSearchAdapterSuggest(t *testing.T) {
	es := &fakeES{respond: func(string, string, []byte) (int, string) {
		return 200, `{"aggregations": {
		  "suburbs": {"values": {"buckets": [{"key": "borrowdale", "doc_count": 1, "top": {"hits": {"hits": [{"_source": {"suburb_label": "Borrowdale", "city_label": "Harare"}}]}}}]}},
		  "cities": {"values": {"buckets": [{"key": "harare", "doc_count": 12, "top": {"hits": {"hits": [{"_source": {"city_label": "Harare"}}]}}}]}},
		  "listings": {"top": {"hits": {"hits": [{"_source": {"title": "Four-bedroom family home with garden", "slug": "four-bedroom-family-home-borrowdale"}}]}}}
		}}`
	}}
	got, err := NewElasticSearch(es, "rp365-test", time.Second).Suggest(context.Background(), "bor", 8, esNow)
	if err != nil {
		t.Fatal(err)
	}
	want := []Suggestion{
		{Type: "suburb", Label: "Borrowdale, Harare", Value: "borrowdale"},
		{Type: "city", Label: "Harare", Value: "harare"},
		{Type: "listing", Label: "Four-bedroom family home with garden", Value: "four-bedroom-family-home-borrowdale"},
	}
	if len(got) != len(want) {
		t.Fatalf("suggestions = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suggestion %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if !strings.Contains(string(es.requests[0].Body), `"type":"bool_prefix"`) || !strings.Contains(string(es.requests[0].Body), `suburb_label.suggest._2gram`) {
		t.Fatalf("suggest must use the search_as_you_type sub-fields: %s", es.requests[0].Body)
	}
}

// If Elasticsearch is down, slow or broken, the request is answered by the
// database adapter instead of failing.
func TestSearchFallsBackToTheDatabaseAdapter(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	r := sampleRecord()
	store.Put(r)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	failures := map[string]*fakeES{
		"unreachable":  {err: errors.New(`dial tcp 10.1.2.3:9200: connection refused (Authorization: ApiKey SECRET-KEY)`)},
		"server error": {respond: func(string, string, []byte) (int, string) { return 503, `{"error":{"type":"cluster_block_exception"}}` }},
		"index missing": {respond: func(string, string, []byte) (int, string) {
			return 404, `{"error":{"type":"index_not_found_exception"}}`
		}},
		"unreadable answer": {respond: func(string, string, []byte) (int, string) { return 200, `<html>proxy error</html>` }},
		"timed out inside Elasticsearch": {respond: func(string, string, []byte) (int, string) {
			return 200, `{"timed_out":true,"_shards":{"total":1,"successful":1,"failed":0},"hits":{"total":{"value":0},"hits":[]}}`
		}},
		"a shard failed": {respond: func(string, string, []byte) (int, string) {
			return 200, `{"timed_out":false,"_shards":{"total":2,"successful":1,"failed":1},"hits":{"total":{"value":0},"hits":[]}}`
		}},
		"bad hit id": {respond: func(string, string, []byte) (int, string) {
			return 200, `{"hits":{"total":{"value":1},"hits":[{"_id":"nope"}]}}`
		}},
	}
	for name, es := range failures {
		t.Run(name, func(t *testing.T) {
			fb := NewFallbackSearch(NewElasticSearch(es, "rp365-test", time.Second), store, logger)
			svc := NewService(store, fb, noLabels{})
			svc.SetClock(func() time.Time { return esNow })
			page, err := svc.Find(context.Background(), Query{Sort: SortNewest, Page: 1, PageSize: 12})
			if err != nil || len(page.Items) != 1 || page.Items[0].ID != r.ID || page.Total != 1 {
				t.Fatalf("fallback result: %+v, %v", page.Items, err)
			}
			if fb.Fallbacks() != 1 {
				t.Fatalf("fallbacks counted = %d, want 1", fb.Fallbacks())
			}
			if name == "bad hit id" {
				return // that response is only wrong for a search
			}
			if got, err := fb.Suggest(context.Background(), "four", 8, esNow); err != nil || len(got) != 1 {
				t.Fatalf("fallback suggest: %+v, %v", got, err)
			}
			if fb.Fallbacks() != 2 {
				t.Fatalf("fallbacks counted = %d, want 2", fb.Fallbacks())
			}
		})
	}
	out := logs.String()
	if !strings.Contains(out, "answering from the database instead") || !strings.Contains(out, `"search_fallbacks_total"`) {
		t.Fatalf("fallbacks must be logged: %s", out)
	}
	// The log carries a status and a reason: never the host, a header or a key.
	for _, private := range []string{"SECRET-KEY", "ApiKey", "Authorization", "10.1.2.3", "proxy error"} {
		if strings.Contains(out, private) {
			t.Fatalf("the fallback log leaks %q: %s", private, out)
		}
	}

	// A request that exceeds the timeout is abandoned, not waited for.
	slow := slowES{delay: 300 * time.Millisecond}
	fb := NewFallbackSearch(NewElasticSearch(slow, "rp365-test", 20*time.Millisecond), store, logger)
	start := time.Now()
	if hits, err := fb.Search(context.Background(), Query{Sort: SortNewest, Page: 1, PageSize: 12}, esNow); err != nil || hits.Total != 1 {
		t.Fatalf("slow search: %+v, %v", hits, err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("the slow request was not abandoned at the timeout (took %s)", elapsed)
	}
}

type slowES struct{ delay time.Duration }

func (s slowES) Perform(req *http.Request) (*http.Response, error) {
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-time.After(s.delay):
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	}
}

// The index can be behind the database. Whatever it returns is re-read from
// the source of truth, so an expired or unpublished listing never comes back.
func TestElasticsearchHitsAreRecheckedAgainstTheDatabase(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	live := sampleRecord()
	store.Put(live)
	expired := sampleRecord()
	expired.ID, expired.Slug, expired.ExpiresAt = uuid.New(), "expired", esNow.Add(-time.Minute)
	store.Put(expired)
	paused := sampleRecord()
	paused.ID, paused.Slug, paused.Status = uuid.New(), "paused", "PAUSED"
	store.Put(paused)
	deleted := uuid.New()

	// A stale index still returns all four.
	es := &fakeES{respond: func(string, string, []byte) (int, string) {
		return 200, `{"hits":{"total":{"value":4},"hits":[{"_id":"` + expired.ID.String() + `"},{"_id":"` + live.ID.String() +
			`"},{"_id":"` + paused.ID.String() + `"},{"_id":"` + deleted.String() + `"}]},"aggregations":{}}`
	}}
	svc := NewService(store, NewElasticSearch(es, "rp365-test", time.Second), noLabels{})
	svc.SetClock(func() time.Time { return esNow })
	page, err := svc.Find(context.Background(), Query{Sort: SortNewest, Page: 1, PageSize: 12})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].ID != live.ID {
		t.Fatalf("items = %+v, want only the live listing", page.Items)
	}
	// The refused hits are taken off the total too.
	if page.Total != 1 {
		t.Fatalf("total = %d, want 1", page.Total)
	}
	// And the query itself asked Elasticsearch for public listings only.
	if body := string(es.requests[0].Body); !strings.Contains(body, `"status":"PUBLISHED"`) || !strings.Contains(body, `"expires_at":{"gt":`) {
		t.Fatalf("the query lacks the public rule: %s", body)
	}
}

func newIndexer(es ESTransport, store Store) *Indexer {
	ix := NewIndexer(es, "rp365-test", store, labelsFor(testLabels), slog.New(slog.NewJSONHandler(io.Discard, nil)))
	ix.SetClock(func() time.Time { return esNow })
	return ix
}

type labelsFor map[string]map[string]string

func (l labelsFor) Labels(_ context.Context, set string) (map[string]string, error) {
	return l[set], nil
}
func (l labelsFor) ActiveCodes(context.Context, string) ([]string, error) { return nil, nil }

func event(eventType string, id uuid.UUID) outbox.Event {
	return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: AggregateListing, AggregateID: id, Attempts: 1}
}

func TestIndexingIsIdempotentAndOrderIndependent(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	r := sampleRecord()
	store.Put(r)
	es := &fakeES{}
	ix := newIndexer(es, store)
	ctx := context.Background()
	docPath := "/rp365-test-listings-current/_doc/" + r.ID.String()

	// Published: the document is written with the record's version.
	if err := ix.Handle(ctx, event(EventListingPublished, r.ID)); err != nil {
		t.Fatal(err)
	}
	want := "PUT " + docPath + "?version_type=external&version=" + big.NewInt(r.Version()).String()
	if got := es.calls(); len(got) != 1 || got[0] != want {
		t.Fatalf("calls = %v, want [%s]", got, want)
	}
	var doc Document
	if err := json.Unmarshal(es.requests[0].Body, &doc); err != nil || doc.ID != r.ID.String() || doc.SuburbLabel != "Borrowdale" || doc.Version != r.Version() {
		t.Fatalf("indexed document = %+v, %v", doc, err)
	}

	// The same event again, and a different event type for the same listing,
	// produce the identical write: the handler reads the current state.
	for _, eventType := range []string{EventListingPublished, EventListingUpdated, EventListingExpired, EventListingUnpublished} {
		if err := ix.Handle(ctx, event(eventType, r.ID)); err != nil {
			t.Fatal(err)
		}
	}
	for i, call := range es.calls() {
		if call != want || !bytes.Equal(es.requests[i].Body, es.requests[0].Body) {
			t.Fatalf("call %d = %s", i, call)
		}
	}

	// Elasticsearch already has this version or a newer one: 409 is success.
	es.requests, es.respond = nil, func(string, string, []byte) (int, string) {
		return 409, `{"error":{"type":"version_conflict_engine_exception"}}`
	}
	if err := ix.Handle(ctx, event(EventListingUpdated, r.ID)); err != nil {
		t.Fatalf("a version conflict must be treated as done: %v", err)
	}

	// The listing stops being public (expired, paused, or deleted): the
	// document is removed, with a version above the last write so that a
	// delayed older write cannot bring it back. An event that arrives late
	// and says "published" still deletes, because the truth is "not public".
	es.requests, es.respond = nil, nil
	expired := r
	expired.ExpiresAt = esNow.Add(-time.Second)
	store.Put(expired)
	if err := ix.Handle(ctx, event(EventListingPublished, r.ID)); err != nil {
		t.Fatal(err)
	}
	if got, want := es.calls()[0], "DELETE "+docPath+"?version_type=external&version="+big.NewInt(r.Version()+1).String(); got != want {
		t.Fatalf("call = %s, want %s", got, want)
	}
	es.requests = nil
	if err := ix.Handle(ctx, event(EventListingUnpublished, uuid.New())); err != nil { // a listing that no longer exists
		t.Fatal(err)
	}
	if got := es.calls()[0]; !strings.HasPrefix(got, "DELETE /rp365-test-listings-current/_doc/") {
		t.Fatalf("call = %s", got)
	}
	// Deleting something that is not there is fine.
	es.respond = func(string, string, []byte) (int, string) { return 404, `{"result":"not_found"}` }
	if err := ix.Handle(ctx, event(EventListingExpired, r.ID)); err != nil {
		t.Fatalf("a missing document is already deleted: %v", err)
	}

	// Failures: a temporary one is retried by the outbox, a malformed request is dead-lettered.
	store.Put(r)
	es.respond = func(string, string, []byte) (int, string) {
		return 503, `{"error":{"type":"unavailable_shards_exception"}}`
	}
	if err := ix.Handle(ctx, event(EventListingUpdated, r.ID)); err == nil || outbox.IsPermanent(err) {
		t.Fatalf("503: err = %v, want a retryable error", err)
	}
	es.respond = func(string, string, []byte) (int, string) {
		return 400, `{"error":{"type":"strict_dynamic_mapping_exception"}}`
	}
	if err := ix.Handle(ctx, event(EventListingUpdated, r.ID)); !outbox.IsPermanent(err) {
		t.Fatalf("400: err = %v, want a permanent error", err)
	}
	if len(IndexEventTypes()) != 4 {
		t.Fatal("four listing events are handled")
	}
}

func TestReindexSwapsTheAliasAtomically(t *testing.T) {
	store := NewMemoryStore(noLabels{})
	r := sampleRecord()
	store.Put(r)
	expired := sampleRecord()
	expired.ID, expired.Slug, expired.ExpiresAt = uuid.New(), "expired", esNow.Add(-time.Hour)
	store.Put(expired)

	const fresh, old = "rp365-test-listings-20261114090000", "rp365-test-listings-20261001000000"
	es := &fakeES{respond: func(method, path string, _ []byte) (int, string) {
		switch {
		case method == http.MethodGet && strings.HasPrefix(path, "/_alias/"):
			return 200, `{"` + old + `":{"aliases":{"rp365-test-listings-current":{}}},"someone-elses-index":{"aliases":{}}}`
		case strings.HasSuffix(path, "/_bulk"):
			return 200, `{"errors":false,"items":[]}`
		}
		return 200, `{"acknowledged":true}`
	}}
	name, count, err := newIndexer(es, store).Reindex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if name != fresh || count != 1 {
		t.Fatalf("reindex = %s, %d; want %s with 1 listing (the expired one is not indexed)", name, count, fresh)
	}
	want := []string{
		"PUT /" + fresh, "POST /" + fresh + "/_bulk", "POST /" + fresh + "/_refresh",
		"GET /_alias/rp365-test-listings-current", "POST /_aliases", "DELETE /" + old,
	}
	if got := es.calls(); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls:\n got %v\nwant %v", got, want)
	}
	// The index is created with the explicit mapping.
	if !bytes.Equal(es.requests[0].Body, IndexMapping) {
		t.Fatal("the new index must be created with the explicit mapping")
	}
	// Bulk: an action line and a source line per listing, external versioning.
	bulk := es.requests[1]
	lines := strings.Split(strings.TrimRight(string(bulk.Body), "\n"), "\n")
	if bulk.ContentType != "application/x-ndjson" || len(lines) != 2 || !strings.Contains(lines[0], `"version_type":"external"`) ||
		!strings.Contains(lines[0], r.ID.String()) || strings.Contains(string(bulk.Body), expired.ID.String()) {
		t.Fatalf("bulk body = %s", bulk.Body)
	}
	// One request removes the old index from the alias and adds the new one.
	var swap struct {
		Actions []map[string]map[string]any `json:"actions"`
	}
	if err := json.Unmarshal(es.requests[4].Body, &swap); err != nil || len(swap.Actions) != 2 {
		t.Fatalf("alias actions = %s", es.requests[4].Body)
	}
	if swap.Actions[0]["remove"]["must_exist"] != true || swap.Actions[0]["remove"]["index"] != old || swap.Actions[1]["add"]["index"] != fresh || swap.Actions[1]["add"]["is_write_index"] != true ||
		swap.Actions[1]["add"]["alias"] != "rp365-test-listings-current" {
		t.Fatalf("alias actions = %+v", swap.Actions)
	}
	// The old index is deleted only AFTER the swap; other indices are left alone.
	if es.calls()[5] != "DELETE /"+old || strings.Contains(strings.Join(es.calls(), " "), "someone-elses-index") {
		t.Fatalf("deletes = %v", es.calls())
	}

	// The aliases API answers 200 with "errors": true when an action failed.
	// That is not a swap: nothing is deleted afterwards, and the new index is
	// removed only when its own "add" is known to have failed.
	t.Run("the alias request reports failed actions", func(t *testing.T) {
		for name, tc := range map[string]struct {
			response  string
			deleteNew bool
		}{
			"remove failed, add applied": {`{"acknowledged":true,"errors":true,"action_results":[{"action":{"type":"remove"},"status":404,"error":{"type":"aliases_not_found_exception"}},{"action":{"type":"add"},"status":200}]}`, false},
			"add failed":                 {`{"acknowledged":true,"errors":true,"action_results":[{"action":{"type":"remove"},"status":200},{"action":{"type":"add"},"status":400,"error":{"type":"illegal_state_exception"}}]}`, true},
			"no per-action results":      {`{"acknowledged":true,"errors":true}`, false},
			"unreadable":                 {`<html>proxy</html>`, false},
		} {
			es := &fakeES{respond: func(method, path string, _ []byte) (int, string) {
				switch {
				case method == http.MethodGet && strings.HasPrefix(path, "/_alias/"):
					return 200, `{"` + old + `":{"aliases":{"rp365-test-listings-current":{}}}}`
				case strings.HasSuffix(path, "/_bulk"):
					return 200, `{"errors":false,"items":[]}`
				case path == "/_aliases":
					return 200, tc.response
				}
				return 200, `{"acknowledged":true}`
			}}
			if _, _, err := newIndexer(es, store).Reindex(context.Background()); err == nil {
				t.Fatalf("%s: expected an error", name)
			}
			calls := strings.Join(es.calls(), "|")
			if strings.Contains(calls, "DELETE /"+old) {
				t.Fatalf("%s: the old index must not be deleted: %s", name, calls)
			}
			if got := strings.HasSuffix(calls, "DELETE /"+fresh); got != tc.deleteNew {
				t.Fatalf("%s: new index deleted = %v, want %v: %s", name, got, tc.deleteNew, calls)
			}
		}
	})

	// A bulk response is 200 even when items were rejected; the item's own
	// status and error type are reported, and a response without the
	// "errors" flag is not taken as success.
	t.Run("bulk item errors", func(t *testing.T) {
		for response, want := range map[string]string{
			`{"errors":true,"items":[{"index":{"_id":"x","status":400,"error":{"type":"strict_dynamic_mapping_exception","reason":"private text"}}}]}`: "HTTP 400 bulk item rejected: strict_dynamic_mapping_exception",
			`{}`: "unreadable bulk response",
		} {
			es := &fakeES{respond: func(_, path string, _ []byte) (int, string) {
				if strings.HasSuffix(path, "/_bulk") {
					return 200, response
				}
				return 200, `{}`
			}}
			_, _, err := newIndexer(es, store).Reindex(context.Background())
			if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "private text") {
				t.Fatalf("bulk error = %v, want %q", err, want)
			}
		}
	})

	t.Run("first run: no alias yet", func(t *testing.T) {
		es := &fakeES{respond: func(method, path string, _ []byte) (int, string) {
			if method == http.MethodGet {
				return 404, `{"error":"alias missing","status":404}`
			}
			if strings.HasSuffix(path, "/_bulk") {
				return 200, `{"errors":false}`
			}
			return 200, `{"acknowledged":true}`
		}}
		if _, _, err := newIndexer(es, store).Reindex(context.Background()); err != nil {
			t.Fatal(err)
		}
		calls := es.calls()
		if calls[len(calls)-1] != "POST /_aliases" || strings.Contains(string(es.requests[len(calls)-1].Body), "remove") {
			t.Fatalf("calls = %v", calls)
		}
	})

	t.Run("a failure before the swap leaves the alias alone and removes the new index", func(t *testing.T) {
		for failAt, why := range map[string]string{"/_bulk": "bulk rejected", "/_refresh": "refresh failed", "/_alias/": "alias unreadable"} {
			es := &fakeES{respond: func(method, path string, _ []byte) (int, string) {
				switch {
				case failAt == "/_bulk" && strings.HasSuffix(path, "/_bulk"):
					return 200, `{"errors":true,"items":[{"index":{"status":400}}]}`
				case strings.Contains(path, failAt) && failAt != "/_bulk":
					return 500, `{"error":{"type":"boom"}}`
				case strings.HasSuffix(path, "/_bulk"):
					return 200, `{"errors":false}`
				}
				return 200, `{}`
			}}
			if _, _, err := newIndexer(es, store).Reindex(context.Background()); err == nil {
				t.Fatalf("%s: expected an error", why)
			}
			calls := strings.Join(es.calls(), "|")
			if strings.Contains(calls, "POST /_aliases") || !strings.HasSuffix(calls, "DELETE /"+fresh) {
				t.Fatalf("%s: calls = %s", why, calls)
			}
		}
	})
}

func TestESErrorsAreSafeToLog(t *testing.T) {
	es := &fakeES{err: errors.New(`Get "https://elastic:hunter2@search.internal:9200/x": Authorization: ApiKey SECRET-KEY`)}
	_, _, err := esDo(context.Background(), es, http.MethodGet, "/x", nil, "")
	if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "SECRET-KEY") || strings.Contains(err.Error(), "search.internal") {
		t.Fatalf("err = %v", err)
	}
	es = &fakeES{respond: func(string, string, []byte) (int, string) {
		return 401, `{"error":{"type":"security_exception","reason":"invalid ApiKey SECRET-KEY for https://search.internal:9200"}}`
	}}
	_, _, err = esDo(context.Background(), es, http.MethodGet, "/x", nil, "")
	var esErr *ESError
	if !errors.As(err, &esErr) || esErr.Status != 401 || err.Error() != "elasticsearch: HTTP 401 security_exception" {
		t.Fatalf("err = %v", err)
	}
}
