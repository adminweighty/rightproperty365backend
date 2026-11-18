package listings

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/outbox"
)

// Outbox events that mean "this listing's public state may have changed".
// They carry the listing ID only; the handler reads the current state from
// the database, so the events are idempotent and their order does not matter.
const (
	EventListingPublished   = "LISTING_PUBLISHED"
	EventListingUpdated     = "LISTING_UPDATED"
	EventListingExpired     = "LISTING_EXPIRED"
	EventListingUnpublished = "LISTING_UNPUBLISHED"

	AggregateListing = "listing"
)

// IndexEventTypes lists the events the search-index handler consumes.
func IndexEventTypes() []string {
	return []string{EventListingPublished, EventListingUpdated, EventListingExpired, EventListingUnpublished}
}

// ESTransport performs a request against Elasticsearch. The official client
// (github.com/elastic/go-elasticsearch) satisfies it; tests use a fake. The
// request URL holds only a path and query: the transport supplies the host
// and the credentials, which therefore never pass through this package.
type ESTransport interface {
	Perform(*http.Request) (*http.Response, error)
}

// MappingVersion counts the changes of the index mapping. It is recorded in
// the mapping's _meta. The mapping is strict, so after a release that raises
// it the index must be rebuilt with cmd/reindex: until then a document with
// the new fields is refused by the old index.
//
//	1  the first mapping
//	2  region, advertiser_type, has_photos (country / region and advanced filters)
//	3  max_guests, min_nights, keywords (short stays; price_period gains "night")
//	4  city_name, region_name (free-text places of a listing in a country
//	   without reference places; city and suburb may now be absent)
//	5  advertiser_verified (only listings of a verified advertiser are public)
//	6  has_video (the listing has a YouTube video tour)
//	7  location is now set (the reserved geo_point: the PUBLIC coordinates,
//	   already rounded for an APPROXIMATE listing). The field itself is
//	   unchanged; the version is raised so that operators reindex, without
//	   which existing documents have no location and no bbox finds them.
const MappingVersion = 7

// IndexMapping is the explicit mapping of a listings index.
//
//go:embed es_mapping.json
var IndexMapping []byte

// Document is what is stored in the search index for one listing. It holds
// ONLY public listing data that search needs: no customer, no contact detail,
// no owner account, not even the advertiser. Hits are identified by ID and
// the listing itself is always re-read from the database.
type Document struct {
	ID                string `json:"id"`
	Slug              string `json:"slug"`
	Reference         string `json:"reference"`
	Title             string `json:"title"`
	Summary           string `json:"summary"`
	Description       string `json:"description"`
	Deal              string `json:"deal"`
	Category          string `json:"category"`
	PropertyType      string `json:"property_type"`
	PropertyTypeLabel string `json:"property_type_label"`
	// City and Suburb are reference codes; both are absent for a listing
	// whose city is free text (CityName), so they never form a facet value.
	City        string `json:"city,omitempty"`
	CityLabel   string `json:"city_label,omitempty"`
	Suburb      string `json:"suburb,omitempty"`
	SuburbLabel string `json:"suburb_label,omitempty"`
	// CityName and RegionName are the free-text city and region of a listing
	// in a country without reference places: searchable by q, not filterable.
	CityName   string `json:"city_name,omitempty"`
	RegionName string `json:"region_name,omitempty"`
	Market     string `json:"market"`
	// Region is the region of the city (reference data), "" when it has none.
	Region string `json:"region,omitempty"`
	// AdvertiserType is "agency" or "private": the kind of advertiser, never who.
	AdvertiserType string `json:"advertiser_type"`
	// AdvertiserVerified says the advertiser was verified when the document
	// was written: a single boolean, never who. Searches filter on it so the
	// totals and facets are right; the database still has the last word on
	// every hit.
	AdvertiserVerified bool `json:"advertiser_verified"`
	HasPhotos          bool `json:"has_photos"`
	// HasVideo says the listing has a YouTube video tour (never which).
	HasVideo bool `json:"has_video"`
	// MaxGuests and MinNights are set for short stays only.
	MaxGuests *int `json:"max_guests,omitempty"`
	MinNights *int `json:"min_nights,omitempty"`
	// Keywords are the label and description of the listing's category, so
	// free text finds a category by what people call it ("airbnb" finds the
	// short stays). Reference data, not listing data.
	Keywords    string   `json:"keywords,omitempty"`
	Currency    string   `json:"currency"`
	Price       float64  `json:"price"`
	PricePeriod string   `json:"price_period,omitempty"`
	Bedrooms    *int     `json:"bedrooms,omitempty"`
	Bathrooms   *int     `json:"bathrooms,omitempty"`
	FloorArea   *int     `json:"floor_area,omitempty"`
	LandArea    *int     `json:"land_area,omitempty"`
	Features    []string `json:"features"`
	Featured    bool     `json:"featured"`
	Status      string   `json:"status"`
	PublishedAt ESTime   `json:"published_at"`
	ExpiresAt   ESTime   `json:"expires_at"`
	UpdatedAt   ESTime   `json:"updated_at"`
	Version     int64    `json:"version"`
	// Location is the geo_point of map search, as [longitude, latitude]:
	// the PUBLIC coordinates (rounded for an APPROXIMATE listing), so the
	// index never holds an exact position the public may not see. Absent
	// for a listing without coordinates.
	Location *[2]float64 `json:"location,omitempty"`
}

// esTimeLayout is how dates are sent to Elasticsearch: UTC with exactly three
// fractional digits. That is the form the reference shows for the default
// date format (strict_date_optional_time, yyyy-MM-dd'T'HH:mm:ss.SSSZ); a date
// field stores milliseconds anyway.
const esTimeLayout = "2006-01-02T15:04:05.000Z"

// ESTime is a time written in esTimeLayout.
type ESTime time.Time

// MarshalJSON implements json.Marshaler.
func (t ESTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format(esTimeLayout) + `"`), nil
}

// UnmarshalJSON implements json.Unmarshaler.
func (t *ESTime) UnmarshalJSON(b []byte) error {
	parsed, err := time.Parse(`"`+esTimeLayout+`"`, string(b))
	if err != nil {
		return err
	}
	*t = ESTime(parsed)
	return nil
}

// BuildDocument turns a record into its index document.
func BuildDocument(r Record, labels map[string]map[string]string) Document {
	price, _ := new(big.Rat).SetString(r.Price.Amount)
	amount := 0.0
	if price != nil {
		amount, _ = price.Float64()
	}
	features := r.Features
	if features == nil {
		features = []string{}
	}
	advertiser := AdvertiserFilterAgency
	if r.Advertiser.Type == AdvertiserPrivateSeller {
		advertiser = AdvertiserFilterPrivate
	}
	place := func(set, code string) string {
		if code == "" {
			return ""
		}
		return coded(labels[set], code).Label
	}
	return Document{
		Region: labels[keyCityRegion][r.City], AdvertiserType: advertiser, AdvertiserVerified: r.Advertiser.Verified, HasPhotos: len(r.Photos) > 0,
		HasVideo: ValidYouTubeID(r.YouTubeVideoID),
		CityName: r.CityName, RegionName: r.RegionName,
		MaxGuests: r.MaxGuests, MinNights: r.MinNights, Keywords: labels[keyCategoryKeywords][r.Category],
		ID: r.ID.String(), Slug: r.Slug, Reference: r.Reference, Title: r.Title, Summary: r.Summary, Description: r.Description,
		Deal: r.Deal, Category: r.Category, PropertyType: r.PropertyType, PropertyTypeLabel: coded(labels[SetPropertyTypes], r.PropertyType).Label,
		City: r.City, CityLabel: place(SetCities, r.City), Suburb: r.Suburb, SuburbLabel: place(SetSuburbs, r.Suburb),
		Market: r.MarketCode, Currency: r.Price.Currency, Price: amount, PricePeriod: r.PricePeriod,
		Bedrooms: r.Bedrooms, Bathrooms: r.Bathrooms, FloorArea: r.FloorArea, LandArea: r.LandArea,
		Features: features, Featured: r.Featured, Status: r.Status,
		PublishedAt: ESTime(r.PublishedAt), ExpiresAt: ESTime(r.ExpiresAt), UpdatedAt: ESTime(r.UpdatedAt), Version: r.Version(),
		Location: esLocation(r),
	}
}

// esLocation is the geo_point of a record ([lon, lat], the public
// coordinates), nil without coordinates.
func esLocation(r Record) *[2]float64 {
	l := r.PublicLocation()
	if l == nil {
		return nil
	}
	return &[2]float64{l.Longitude, l.Latitude}
}

// IndexAlias is the alias searches and writes go through: <prefix>-listings-current.
func IndexAlias(prefix string) string { return prefix + "-listings-current" }

// ESError is a failed Elasticsearch call. Its text carries a status and a
// short reason only: never a URL with a host, a header or an API key.
type ESError struct {
	Status int
	Reason string
}

func (e *ESError) Error() string {
	if e.Status == 0 {
		return "elasticsearch: " + e.Reason
	}
	return fmt.Sprintf("elasticsearch: HTTP %d %s", e.Status, e.Reason)
}

// esDo performs one request and returns the body of a 2xx response. Any
// other status in `accept` is returned without an error.
func esDo(ctx context.Context, t ESTransport, method, path string, body []byte, contentType string, accept ...int) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, reader)
	if err != nil {
		return 0, nil, &ESError{Reason: "invalid request"}
	}
	if body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := t.Perform(req)
	if err != nil {
		// The transport error can name the host; keep only its kind.
		reason := "request failed"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			reason = "timeout"
		}
		return 0, nil, &ESError{Reason: reason}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp.StatusCode, raw, nil
	}
	for _, code := range accept {
		if resp.StatusCode == code {
			return resp.StatusCode, raw, nil
		}
	}
	var e struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	reason := "error"
	if json.Unmarshal(raw, &e) == nil && e.Error.Type != "" && len(e.Error.Type) < 80 {
		reason = e.Error.Type
	}
	return resp.StatusCode, nil, &ESError{Status: resp.StatusCode, Reason: reason}
}

// Indexer keeps the search index in step with the database.
type Indexer struct {
	es     ESTransport
	alias  string
	prefix string
	store  Store
	labels Labels
	places Places
	logger *slog.Logger
	now    func() time.Time
}

// NewIndexer builds an Indexer for the index prefix (for example "rp365-production").
func NewIndexer(es ESTransport, prefix string, store Store, labels Labels, logger *slog.Logger) *Indexer {
	return &Indexer{es: es, alias: IndexAlias(prefix), prefix: prefix, store: store, labels: labels, logger: logger, now: time.Now}
}

// SetClock overrides the time source (tests).
func (ix *Indexer) SetClock(now func() time.Time) { ix.now = now }

// SetPlaces gives the Indexer the location hierarchy, from which a document
// gets its region. Without it documents have no region.
func (ix *Indexer) SetPlaces(p Places) { ix.places = p }

func (ix *Indexer) labelSets(ctx context.Context) (map[string]map[string]string, error) {
	return loadLabels(ctx, ix.labels, ix.places)
}

// Sync makes the index reflect the current state of one listing: its
// document is written when the listing is public and removed when it is not
// (or no longer exists).
//
// It is idempotent and order-independent. It always reads the CURRENT state
// from the database, so replaying an old event writes today's truth, and both
// writes and deletes use external versioning (the record's version), so
// Elasticsearch itself refuses a write that is older than what it holds; that
// refusal (409) is success here.
func (ix *Indexer) Sync(ctx context.Context, id uuid.UUID) error {
	records, err := ix.store.ByIDs(ctx, []uuid.UUID{id})
	if err != nil {
		return fmt.Errorf("load listing: %w", err)
	}
	now := ix.now().UTC()
	path := "/" + ix.alias + "/_doc/" + id.String()
	if len(records) == 0 || !records[0].Public(now) {
		version := now.UnixMicro() // a listing that is gone has no version of its own
		if len(records) == 1 {
			version = records[0].Version() + 1
		}
		_, _, err := esDo(ctx, ix.es, http.MethodDelete, path+"?version_type=external&version="+fmt.Sprint(version), nil, "",
			http.StatusNotFound, http.StatusConflict)
		return err
	}
	labels, err := ix.labelSets(ctx)
	if err != nil {
		return err
	}
	doc := BuildDocument(records[0], labels)
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	_, _, err = esDo(ctx, ix.es, http.MethodPut, path+"?version_type=external&version="+fmt.Sprint(doc.Version), body, "application/json",
		http.StatusConflict)
	return err
}

// Handle is the outbox handler for listing events.
func (ix *Indexer) Handle(ctx context.Context, ev outbox.Event) error {
	if err := ix.Sync(ctx, ev.AggregateID); err != nil {
		var esErr *ESError
		// A request Elasticsearch rejects as malformed will never succeed.
		if errors.As(err, &esErr) && esErr.Status == http.StatusBadRequest {
			return outbox.Permanent(err)
		}
		return err
	}
	return nil
}

// Register attaches the handler to a dispatcher for every listing event.
func (ix *Indexer) Register(d *outbox.Dispatcher) {
	for _, t := range IndexEventTypes() {
		d.Register(t, outbox.HandlerFunc(ix.Handle))
	}
}

// Reindex builds a NEW index from the database, then moves the alias to it in
// one atomic action, and only after that deletes the indices the alias used
// to point to. Searches keep working on the old index until the swap, and a
// failure before the swap leaves the alias untouched (the half-built index is
// removed).
func (ix *Indexer) Reindex(ctx context.Context) (name string, count int, err error) {
	now := ix.now().UTC()
	name = fmt.Sprintf("%s-listings-%s", ix.prefix, now.Format("20060102150405"))
	if _, _, err = esDo(ctx, ix.es, http.MethodPut, "/"+name, IndexMapping, "application/json"); err != nil {
		return "", 0, fmt.Errorf("create index: %w", err)
	}
	created := name // the named result is cleared on an error return
	keep := false   // set when the alias may already point to the new index
	defer func() {
		if err != nil && !keep { // do not leave a half-built index behind
			_, _, _ = esDo(context.WithoutCancel(ctx), ix.es, http.MethodDelete, "/"+created, nil, "", http.StatusNotFound)
		}
	}()

	records, err := ix.store.AllPublished(ctx, now)
	if err != nil {
		return "", 0, fmt.Errorf("load listings: %w", err)
	}
	labels, err := ix.labelSets(ctx)
	if err != nil {
		return "", 0, err
	}
	const batch = 500
	for start := 0; start < len(records); start += batch {
		var bulk bytes.Buffer
		for _, r := range records[start:min(start+batch, len(records))] {
			doc := BuildDocument(r, labels)
			action, _ := json.Marshal(map[string]any{"index": map[string]any{"_id": doc.ID, "version": doc.Version, "version_type": "external"}})
			source, mErr := json.Marshal(doc)
			if mErr != nil {
				err = mErr
				return "", 0, err
			}
			bulk.Write(action)
			bulk.WriteByte('\n')
			bulk.Write(source)
			bulk.WriteByte('\n')
		}
		var raw []byte
		if _, raw, err = esDo(ctx, ix.es, http.MethodPost, "/"+name+"/_bulk", bulk.Bytes(), "application/x-ndjson"); err != nil {
			return "", 0, fmt.Errorf("bulk index: %w", err)
		}
		// The bulk API answers 200 even when items fail: `errors` says so and
		// each item carries its own status and error type.
		var result struct {
			Errors *bool `json:"errors"`
			Items  []map[string]struct {
				Status int `json:"status"`
				Error  struct {
					Type string `json:"type"`
				} `json:"error"`
			} `json:"items"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Errors == nil {
			err = &ESError{Reason: "unreadable bulk response"}
			return "", 0, err
		}
		if *result.Errors {
			err = &ESError{Reason: "bulk indexing reported errors"}
			for _, item := range result.Items {
				if it, ok := item["index"]; ok && it.Status >= 300 {
					reason := it.Error.Type
					if reason == "" || len(reason) >= 80 {
						reason = "error"
					}
					err = &ESError{Status: it.Status, Reason: "bulk item rejected: " + reason}
					break
				}
			}
			return "", 0, err
		}
	}
	if _, _, err = esDo(ctx, ix.es, http.MethodPost, "/"+name+"/_refresh", nil, ""); err != nil {
		return "", 0, fmt.Errorf("refresh: %w", err)
	}

	// Which indices does the alias point to now?
	var old []string
	status, raw, err := esDo(ctx, ix.es, http.MethodGet, "/_alias/"+url.PathEscape(ix.alias), nil, "", http.StatusNotFound)
	if err != nil {
		return "", 0, fmt.Errorf("read alias: %w", err)
	}
	if status == http.StatusOK {
		var current map[string]json.RawMessage
		if err = json.Unmarshal(raw, &current); err != nil {
			return "", 0, &ESError{Reason: "unreadable alias response"}
		}
		for index := range current {
			if index != name && strings.HasPrefix(index, ix.prefix+"-listings-") {
				old = append(old, index)
			}
		}
		sort.Strings(old)
	}

	// One atomic action list: readers never see zero or two indices.
	actions := []map[string]any{}
	for _, index := range old {
		actions = append(actions, map[string]any{"remove": map[string]any{"index": index, "alias": ix.alias, "must_exist": true}})
	}
	actions = append(actions, map[string]any{"add": map[string]any{"index": name, "alias": ix.alias, "is_write_index": true}})
	body, _ := json.Marshal(map[string]any{"actions": actions})
	if _, raw, err = esDo(ctx, ix.es, http.MethodPost, "/_aliases", body, "application/json"); err != nil {
		return "", 0, fmt.Errorf("swap alias: %w", err)
	}
	// The aliases API can answer 200 with "errors": true and one result per
	// action when some actions failed. That is not a swap: nothing is deleted
	// (the alias may point to the old index, the new one or both) and an
	// operator has to look. The new index is removed only when its own "add"
	// is known to have failed, so the alias cannot be pointing at it.
	var swap struct {
		Errors        bool `json:"errors"`
		ActionResults []struct {
			Status int `json:"status"`
		} `json:"action_results"`
	}
	if json.Unmarshal(raw, &swap) != nil {
		keep = true
		err = &ESError{Reason: "unreadable alias response; check the alias before retrying"}
		return "", 0, err
	}
	if swap.Errors {
		added := len(swap.ActionResults) != len(actions) || swap.ActionResults[len(actions)-1].Status < 300
		keep = added
		err = &ESError{Reason: "alias swap reported failed actions; check the alias before retrying"}
		return "", 0, err
	}

	// Only now are the old indices unused. Failing to delete one is not a
	// failure of the reindex: it is logged and left for an operator.
	for _, index := range old {
		if _, _, dErr := esDo(ctx, ix.es, http.MethodDelete, "/"+index, nil, "", http.StatusNotFound); dErr != nil {
			ix.logger.WarnContext(ctx, "old search index could not be deleted", slog.String("index", index), slog.String("error", dErr.Error()))
		}
	}
	return name, len(records), nil
}
