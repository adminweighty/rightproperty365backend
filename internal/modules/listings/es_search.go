package listings

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// J is a JSON object under construction.
type J = map[string]any

// Text fields searched by q, with their weights. Title outranks place, which
// outranks property type, which outranks the long text.
var esTextFields = []string{"title^4", "suburb_label^3", "city_label^3", "city_name^3", "region_name^3", "property_type_label^2", "summary", "description", "reference", "keywords"}

// esPublic is the public rule inside Elasticsearch: only PUBLISHED documents
// whose publication has not ended and whose advertiser was not unverified
// when the document was written. The database re-checks every hit as well,
// so a document that is behind (an advertiser unverified since) is dropped.
//
// The advertiser clause excludes `advertiser_verified: false` rather than
// requiring `true`, so the documents of an index built before the field
// existed (mapping version 4) still match until the index is rebuilt: the
// search keeps working during a deployment, and the database check above is
// what guarantees the rule either way.
func esPublic(now time.Time) []any {
	return []any{
		J{"term": J{"status": StatusPublished}},
		J{"range": J{"expires_at": J{"gt": now.UTC().Format(esTimeLayout)}}},
		J{"bool": J{"must_not": J{"term": J{"advertiser_verified": false}}}},
	}
}

// esFacetFilters returns the filters that have a facet, keyed by facet name.
// They go into post_filter (so they narrow the hits but not the
// aggregations), and each facet's aggregation applies all of them except its own.
func esFacetFilters(q Query) map[string][]any {
	out := map[string][]any{}
	term := func(name, field, value string) {
		if value != "" {
			out[name] = []any{J{"term": J{field: value}}}
		}
	}
	term("deal", "deal", q.Deal)
	term("category", "category", q.Category)
	term("property_type", "property_type", q.PropertyType)
	term("city", "city", q.City)
	term("suburb", "suburb", q.Suburb)
	term("country", "market", q.Country)
	term("region", "region", q.Region)
	if bounds := esRange(q.BedroomsMin, q.BedroomsMax); bounds != nil {
		out["bedrooms"] = []any{J{"range": J{"bedrooms": bounds}}}
	}
	if q.priced() {
		bounds := J{}
		if q.MinPrice != nil {
			bounds["gte"], _ = q.MinPrice.Float64()
		}
		if q.MaxPrice != nil {
			bounds["lte"], _ = q.MaxPrice.Float64()
		}
		// Prices are only comparable within one currency.
		out["price"] = []any{J{"term": J{"currency": currencyOf(q)}}, J{"range": J{"price": bounds}}}
	}
	return out
}

var esFacetOrder = []string{"deal", "category", "property_type", "city", "suburb", "country", "region", "bedrooms", "price"}

// esRange builds the bounds of a range filter, or nil when there are none.
func esRange(min, max *int) J {
	if min == nil && max == nil {
		return nil
	}
	bounds := J{}
	if min != nil {
		bounds["gte"] = *min
	}
	if max != nil {
		bounds["lte"] = *max
	}
	return bounds
}

// esAll is the query matching documents that satisfy every filter. With no
// filters it is match_all rather than a bool query with an empty clause list.
func esAll(filters []any) J {
	if len(filters) == 0 {
		return J{"match_all": J{}}
	}
	return J{"bool": J{"filter": filters}}
}

// allExcept returns every facet filter except the named facet's own.
func allExcept(filters map[string][]any, skip string) []any {
	out := []any{}
	for _, name := range esFacetOrder {
		if name != skip {
			out = append(out, filters[name]...)
		}
	}
	return out
}

// BuildSearchBody builds the _search request for a query. The HTTP contract
// of GET /v1/listings is unchanged: this only says the same thing to
// Elasticsearch.
func BuildSearchBody(q Query, now time.Time) J {
	// Filters without a facet apply to hits and aggregations alike.
	filter := esPublic(now)
	if q.Featured != nil {
		filter = append(filter, J{"term": J{"featured": *q.Featured}})
	}
	if q.BathroomsMin != nil {
		filter = append(filter, J{"range": J{"bathrooms": J{"gte": *q.BathroomsMin}}})
	}
	for _, r := range []struct {
		field    string
		min, max *int
	}{
		{"bathrooms", nil, q.BathroomsMax}, {"floor_area", q.FloorAreaMin, q.FloorAreaMax}, {"land_area", q.LandAreaMin, q.LandAreaMax},
	} {
		if bounds := esRange(r.min, r.max); bounds != nil {
			filter = append(filter, J{"range": J{r.field: bounds}})
		}
	}
	if q.ListedWithinDays != nil {
		filter = append(filter, J{"range": J{"published_at": J{"gte": listedSince(now, *q.ListedWithinDays).UTC().Format(esTimeLayout)}}})
	}
	if q.GuestsMin != nil {
		filter = append(filter, J{"range": J{"max_guests": J{"gte": *q.GuestsMin}}})
	}
	if q.AdvertiserType != "" {
		filter = append(filter, J{"term": J{"advertiser_type": q.AdvertiserType}})
	}
	if q.Reference != "" { // exact, whatever the case
		filter = append(filter, J{"term": J{"reference.keyword": J{"value": q.Reference, "case_insensitive": true}}})
	}
	if q.HasPhotos {
		filter = append(filter, J{"term": J{"has_photos": true}})
	}
	for _, f := range q.Features { // every feature must be present
		filter = append(filter, J{"term": J{"features": f}})
	}
	// The map. In the main filter, so hits, the total and every facet are
	// computed with the box applied. The geo_point holds the public
	// coordinates (rounded for APPROXIMATE listings).
	if q.HasLocation {
		filter = append(filter, J{"exists": J{"field": "location"}})
	}
	if b := q.BBox; b != nil {
		filter = append(filter, J{"geo_bounding_box": J{"location": J{
			"top_left":     J{"lat": b.North, "lon": b.West},
			"bottom_right": J{"lat": b.South, "lon": b.East},
		}}})
	}
	// A stay. The index holds no availability data: the listings that are
	// not free were resolved from the database and are excluded by ID. It is
	// part of the main filter, so hits, the total and every facet are
	// computed with it applied.
	if q.Stay != nil {
		// A short stay without a minimum is one night.
		filter = append(filter, J{"bool": J{"should": []any{
			J{"range": J{"min_nights": J{"lte": q.Stay.Nights}}},
			J{"bool": J{"must_not": []any{J{"exists": J{"field": "min_nights"}}}}},
		}, "minimum_should_match": 1}})
		var excluded []any
		if len(q.Stay.ExcludeIDs) > 0 {
			excluded = append(excluded, J{"ids": J{"values": idStrings(q.Stay.ExcludeIDs)}})
		}
		if len(q.Stay.ExcludeCountries) > 0 {
			excluded = append(excluded, J{"terms": J{"market": q.Stay.ExcludeCountries}})
		}
		if len(excluded) > 0 {
			filter = append(filter, J{"bool": J{"must_not": excluded}})
		}
	}
	boolQuery := J{"filter": filter}
	words := terms(q.Q)
	if len(words) > 0 {
		// Every word must match somewhere. Typos are tolerated (fuzziness
		// AUTO: none for 1-2 letters, one edit for 3-5, two beyond), but the
		// first letter must be right, which keeps short words from matching
		// almost anything.
		must := make([]any, 0, len(words))
		for _, w := range words {
			must = append(must, J{"multi_match": J{
				"query": w, "fields": esTextFields, "operator": "and", "fuzziness": "AUTO", "prefix_length": 1,
			}})
		}
		boolQuery["must"] = must
	}

	facets := esFacetFilters(q)
	facetAgg := func(name, field string, size int) J {
		return J{"filter": esAll(allExcept(facets, name)), "aggs": J{"values": J{"terms": J{"field": field, "size": size}}}}
	}
	aggs := J{
		"deal":          facetAgg("deal", "deal", 50),
		"category":      facetAgg("category", "category", 50),
		"property_type": facetAgg("property_type", "property_type", 200),
		"city":          facetAgg("city", "city", 500),
		"suburb":        facetAgg("suburb", "suburb", 1000),
		"country":       facetAgg("country", "market", 300),
		"region":        facetAgg("region", "region", 500),
		"bedrooms":      facetAgg("bedrooms", "bedrooms", 60),
		// Features are counted within the full result (they have no "other choice").
		"features": J{"filter": esAll(allExcept(facets, "")), "aggs": J{"values": J{"terms": J{"field": "features", "size": 1000}}}},
		// Prices per currency, without the price filter: nothing is converted,
		// and the Service decides which currency the search is in.
		// How many of the hits have coordinates (mappable_total).
		"mappable": J{"filter": esAll(append(allExcept(facets, ""), J{"exists": J{"field": "location"}}))},
		"price": J{"filter": esAll(allExcept(facets, "price")), "aggs": J{"values": J{
			"terms": J{"field": "currency", "size": 200},
			"aggs":  J{"min": J{"min": J{"field": "price"}}, "max": J{"max": J{"field": "price"}}},
		}}},
	}

	newest := []any{J{"published_at": "desc"}, J{"id": "asc"}}
	var order []any
	switch q.Sort {
	case SortPriceAsc:
		order = append([]any{J{"price": "asc"}}, newest...)
	case SortPriceDesc:
		order = append([]any{J{"price": "desc"}}, newest...)
	case SortOldest:
		order = []any{J{"published_at": "asc"}, J{"id": "asc"}}
	case SortAreaDesc:
		order = append([]any{J{"floor_area": J{"order": "desc", "missing": "_last"}}}, newest...)
	case SortRelevance:
		if len(words) > 0 {
			order = append(order, "_score")
		}
		order = append(append(order, J{"featured": "desc"}), newest...)
	default:
		order = newest
	}

	body := J{
		// page and page_size are bounded by the API (page_size <= 48), and
		// from + size never exceeds Elasticsearch's default result window
		// (index.max_result_window, 10,000).
		"from": max(0, min((q.Page-1)*q.PageSize, esResultWindow-q.PageSize)), "size": q.PageSize,
		"_source": false, "track_total_hits": true,
		"query": J{"bool": boolQuery},
		"aggs":  aggs,
		"sort":  order,
	}
	// post_filter narrows the hits after the aggregations are calculated.
	if post := allExcept(facets, ""); len(post) > 0 {
		body["post_filter"] = J{"bool": J{"filter": post}}
	}
	return body
}

// esResultWindow is the default index.max_result_window.
const esResultWindow = 10000

// BuildSuggestBody builds the _search request behind GET /v1/listings/suggest.
// It uses the search_as_you_type sub-fields, so "bor" finds "Borrowdale".
func BuildSuggestBody(term string, limit int, now time.Time) J {
	prefix := func(field string) J {
		return J{"multi_match": J{"query": term, "type": "bool_prefix", "fields": []string{field, field + "._2gram", field + "._3gram"}}}
	}
	suburb, city, title := prefix("suburb_label.suggest"), prefix("city_label.suggest"), prefix("title.suggest")
	return J{
		"size": 0,
		"query": J{"bool": J{
			"filter": esPublic(now), "should": []any{suburb, city, title}, "minimum_should_match": 1,
		}},
		"aggs": J{
			"suburbs": J{"filter": suburb, "aggs": J{"values": J{
				"terms": J{"field": "suburb", "size": limit},
				"aggs":  J{"top": J{"top_hits": J{"size": 1, "_source": []string{"suburb_label", "city_label"}}}},
			}}},
			"cities": J{"filter": city, "aggs": J{"values": J{
				"terms": J{"field": "city", "size": limit},
				"aggs":  J{"top": J{"top_hits": J{"size": 1, "_source": []string{"city_label"}}}},
			}}},
			"listings": J{"filter": title, "aggs": J{"top": J{"top_hits": J{
				"size": limit, "_source": []string{"title", "slug"}, "sort": []any{J{"published_at": "desc"}},
			}}}},
		},
	}
}

// ElasticSearch is the Elasticsearch adapter of the Search port.
//
// It only proposes listing IDs, a total and facet counts. The Service
// re-reads every hit from the database, which re-checks status and expiry.
type ElasticSearch struct {
	es      ESTransport
	alias   string
	timeout time.Duration
}

// NewElasticSearch builds the adapter. timeout bounds each request: the
// search target is a p95 of one second, so it should be well under that.
func NewElasticSearch(es ESTransport, prefix string, timeout time.Duration) *ElasticSearch {
	return &ElasticSearch{es: es, alias: IndexAlias(prefix), timeout: timeout}
}

type esBuckets struct {
	Values struct {
		Buckets []struct {
			Key      json.RawMessage `json:"key"`
			DocCount int             `json:"doc_count"`
			Top      *esTopHits      `json:"top"`
		} `json:"buckets"`
	} `json:"values"`
}

type esTopHits struct {
	Hits struct {
		Hits []struct {
			Source map[string]string `json:"_source"`
		} `json:"hits"`
	} `json:"hits"`
}

func bucketCounts(b esBuckets) map[string]int {
	out := map[string]int{}
	for _, bucket := range b.Values.Buckets {
		var s string
		if json.Unmarshal(bucket.Key, &s) != nil {
			s = strings.TrimSpace(string(bucket.Key)) // numeric key
		}
		out[s] = bucket.DocCount
	}
	return out
}

func (e *ElasticSearch) search(ctx context.Context, body J, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	_, resp, err := esDo(ctx, e.es, http.MethodPost, "/"+e.alias+"/_search", raw, "application/json")
	if err != nil {
		return err
	}
	// A search can answer 200 with partial results: it timed out inside
	// Elasticsearch or some shards failed. Totals and facets would be wrong, so
	// that is a failure here and the database answers instead.
	var health struct {
		TimedOut bool `json:"timed_out"`
		Shards   struct {
			Failed int `json:"failed"`
		} `json:"_shards"`
	}
	if err := json.Unmarshal(resp, &health); err != nil {
		return &ESError{Reason: "unreadable response"}
	}
	if health.TimedOut || health.Shards.Failed > 0 {
		return &ESError{Reason: "partial results"}
	}
	if err := json.Unmarshal(resp, out); err != nil {
		return &ESError{Reason: "unreadable response"}
	}
	return nil
}

// Search implements Search.
func (e *ElasticSearch) Search(ctx context.Context, q Query, now time.Time) (Hits, error) {
	var resp struct {
		Hits struct {
			Total struct {
				Value int `json:"value"`
			} `json:"total"`
			Hits []struct {
				ID string `json:"_id"`
			} `json:"hits"`
		} `json:"hits"`
		Aggregations struct {
			Deal, Category, City, Suburb, Bedrooms, Features esBuckets
			Country, Region                                  esBuckets
			PropertyType                                     esBuckets `json:"property_type"`
			Mappable                                         struct {
				DocCount int `json:"doc_count"`
			} `json:"mappable"`
			Price struct {
				Values struct {
					Buckets []struct {
						Key      string `json:"key"`
						DocCount int    `json:"doc_count"`
						Min      struct {
							Value *float64 `json:"value"`
						} `json:"min"`
						Max struct {
							Value *float64 `json:"value"`
						} `json:"max"`
					} `json:"buckets"`
				} `json:"values"`
			} `json:"price"`
		} `json:"aggregations"`
	}
	if err := e.search(ctx, BuildSearchBody(q, now), &resp); err != nil {
		return Hits{}, err
	}
	hits := Hits{IDs: []uuid.UUID{}, Total: resp.Hits.Total.Value, Mappable: resp.Aggregations.Mappable.DocCount}
	for _, h := range resp.Hits.Hits {
		id, err := uuid.Parse(h.ID)
		if err != nil {
			return Hits{}, &ESError{Reason: "unreadable hit id"}
		}
		hits.IDs = append(hits.IDs, id)
	}
	a := resp.Aggregations
	hits.Facets = RawFacets{
		Deal: bucketCounts(a.Deal), Category: bucketCounts(a.Category), PropertyType: bucketCounts(a.PropertyType),
		City: bucketCounts(a.City), Suburb: bucketCounts(a.Suburb), Features: bucketCounts(a.Features), Bedrooms: map[int]int{},
		Country: bucketCounts(a.Country), Region: bucketCounts(a.Region),
	}
	for key, n := range bucketCounts(a.Bedrooms) {
		if bedrooms, err := strconv.Atoi(key); err == nil {
			hits.Facets.Bedrooms[bedrooms] = n
		}
	}
	hits.Facets.Prices = map[string]PriceStat{}
	for _, b := range a.Price.Values.Buckets {
		stat := PriceStat{Count: b.DocCount}
		if b.Min.Value != nil && b.Max.Value != nil {
			stat.Min, stat.Max = new(big.Rat).SetFloat64(*b.Min.Value), new(big.Rat).SetFloat64(*b.Max.Value)
		}
		hits.Facets.Prices[b.Key] = stat
	}
	return hits, nil
}

// Suggest implements Search.
func (e *ElasticSearch) Suggest(ctx context.Context, term string, limit int, now time.Time) ([]Suggestion, error) {
	var resp struct {
		Aggregations struct {
			Suburbs, Cities esBuckets
			Listings        struct {
				Top esTopHits `json:"top"`
			} `json:"listings"`
		} `json:"aggregations"`
	}
	if err := e.search(ctx, BuildSuggestBody(term, limit, now), &resp); err != nil {
		return nil, err
	}
	out := []Suggestion{}
	source := func(top *esTopHits) map[string]string {
		if top == nil || len(top.Hits.Hits) == 0 {
			return map[string]string{}
		}
		return top.Hits.Hits[0].Source
	}
	for _, b := range resp.Aggregations.Suburbs.Values.Buckets {
		var code string
		_ = json.Unmarshal(b.Key, &code)
		src := source(b.Top)
		out = append(out, Suggestion{Type: "suburb", Label: src["suburb_label"] + ", " + src["city_label"], Value: code})
	}
	for _, b := range resp.Aggregations.Cities.Values.Buckets {
		var code string
		_ = json.Unmarshal(b.Key, &code)
		out = append(out, Suggestion{Type: "city", Label: source(b.Top)["city_label"], Value: code})
	}
	for _, h := range resp.Aggregations.Listings.Top.Hits.Hits {
		out = append(out, Suggestion{Type: "listing", Label: h.Source["title"], Value: h.Source["slug"]})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FallbackSearch tries the primary search engine and, when it fails, answers
// the same request from the fallback (the database adapter). Search stays
// available when Elasticsearch is down, slow or misconfigured; the price is
// that those requests are served without typo tolerance and with database
// relevance. Every fallback is logged and counted.
type FallbackSearch struct {
	primary  Search
	fallback Search
	logger   *slog.Logger
	count    atomic.Int64
}

// NewFallbackSearch builds the wrapper.
func NewFallbackSearch(primary, fallback Search, logger *slog.Logger) *FallbackSearch {
	return &FallbackSearch{primary: primary, fallback: fallback, logger: logger}
}

// Fallbacks returns how many requests were answered by the fallback since start.
func (f *FallbackSearch) Fallbacks() int64 { return f.count.Load() }

func (f *FallbackSearch) failed(ctx context.Context, operation string, err error) {
	n := f.count.Add(1)
	f.logger.WarnContext(ctx, "search engine failed; answering from the database instead",
		slog.String("operation", operation), slog.String("error", err.Error()), slog.Int64("search_fallbacks_total", n))
}

// Search implements Search.
func (f *FallbackSearch) Search(ctx context.Context, q Query, now time.Time) (Hits, error) {
	hits, err := f.primary.Search(ctx, q, now)
	if err == nil {
		return hits, nil
	}
	f.failed(ctx, "search", err)
	return f.fallback.Search(ctx, q, now)
}

// Suggest implements Search.
func (f *FallbackSearch) Suggest(ctx context.Context, term string, limit int, now time.Time) ([]Suggestion, error) {
	out, err := f.primary.Suggest(ctx, term, limit, now)
	if err == nil {
		return out, nil
	}
	f.failed(ctx, "suggest", err)
	return f.fallback.Suggest(ctx, term, limit, now)
}
