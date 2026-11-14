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

// IndexMapping is the explicit mapping of a listings index.
//
//go:embed es_mapping.json
var IndexMapping []byte

// Document is what is stored in the search index for one listing. It holds
// ONLY public listing data that search needs: no customer, no contact detail,
// no owner account, not even the advertiser. Hits are identified by ID and
// the listing itself is always re-read from the database.
type Document struct {
	ID                string    `json:"id"`
	Slug              string    `json:"slug"`
	Reference         string    `json:"reference"`
	Title             string    `json:"title"`
	Summary           string    `json:"summary"`
	Description       string    `json:"description"`
	Deal              string    `json:"deal"`
	Category          string    `json:"category"`
	PropertyType      string    `json:"property_type"`
	PropertyTypeLabel string    `json:"property_type_label"`
	City              string    `json:"city"`
	CityLabel         string    `json:"city_label"`
	Suburb            string    `json:"suburb"`
	SuburbLabel       string    `json:"suburb_label"`
	Market            string    `json:"market"`
	Currency          string    `json:"currency"`
	Price             float64   `json:"price"`
	PricePeriod       string    `json:"price_period,omitempty"`
	Bedrooms          *int      `json:"bedrooms,omitempty"`
	Bathrooms         *int      `json:"bathrooms,omitempty"`
	FloorArea         *int      `json:"floor_area,omitempty"`
	LandArea          *int      `json:"land_area,omitempty"`
	Features          []string  `json:"features"`
	Featured          bool      `json:"featured"`
	Status            string    `json:"status"`
	PublishedAt       time.Time `json:"published_at"`
	ExpiresAt         time.Time `json:"expires_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	Version           int64     `json:"version"`
	// Location is reserved for map search; nothing sets it yet.
	Location *[2]float64 `json:"location,omitempty"`
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
	return Document{
		ID: r.ID.String(), Slug: r.Slug, Reference: r.Reference, Title: r.Title, Summary: r.Summary, Description: r.Description,
		Deal: r.Deal, Category: r.Category, PropertyType: r.PropertyType, PropertyTypeLabel: coded(labels[SetPropertyTypes], r.PropertyType).Label,
		City: r.City, CityLabel: coded(labels[SetCities], r.City).Label, Suburb: r.Suburb, SuburbLabel: coded(labels[SetSuburbs], r.Suburb).Label,
		Market: r.MarketCode, Currency: r.Price.Currency, Price: amount, PricePeriod: r.PricePeriod,
		Bedrooms: r.Bedrooms, Bathrooms: r.Bathrooms, FloorArea: r.FloorArea, LandArea: r.LandArea,
		Features: features, Featured: r.Featured, Status: r.Status,
		PublishedAt: r.PublishedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), UpdatedAt: r.UpdatedAt.UTC(), Version: r.Version(),
	}
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
	logger *slog.Logger
	now    func() time.Time
}

// NewIndexer builds an Indexer for the index prefix (for example "rp365-production").
func NewIndexer(es ESTransport, prefix string, store Store, labels Labels, logger *slog.Logger) *Indexer {
	return &Indexer{es: es, alias: IndexAlias(prefix), prefix: prefix, store: store, labels: labels, logger: logger, now: time.Now}
}

// SetClock overrides the time source (tests).
func (ix *Indexer) SetClock(now func() time.Time) { ix.now = now }

func (ix *Indexer) labelSets(ctx context.Context) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, set := range []string{SetPropertyTypes, SetCities, SetSuburbs} {
		labels, err := ix.labels.Labels(ctx, set)
		if err != nil {
			return nil, err
		}
		out[set] = labels
	}
	return out, nil
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
	defer func() {
		if err != nil { // do not leave a half-built index behind
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
		var result struct {
			Errors bool `json:"errors"`
		}
		if json.Unmarshal(raw, &result) != nil || result.Errors {
			err = &ESError{Reason: "bulk indexing reported errors"}
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
		actions = append(actions, map[string]any{"remove": map[string]any{"index": index, "alias": ix.alias}})
	}
	actions = append(actions, map[string]any{"add": map[string]any{"index": name, "alias": ix.alias, "is_write_index": true}})
	body, _ := json.Marshal(map[string]any{"actions": actions})
	if _, _, err = esDo(ctx, ix.es, http.MethodPost, "/_aliases", body, "application/json"); err != nil {
		return "", 0, fmt.Errorf("swap alias: %w", err)
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
