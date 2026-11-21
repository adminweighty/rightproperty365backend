package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/attribution"
)

// ListingInfo is what analytics needs to know about a listing: who may see
// its figures, its market, and whether the public can see it right now.
type ListingInfo struct {
	ID        uuid.UUID
	Title     string
	Reference string
	// Slug is the public page's slug (shown only while Public).
	Slug           string
	MarketCode     string
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID
	Public         bool
}

// Listings is the listings module as analytics sees it.
type Listings interface {
	// Listings returns the listings among ids that exist, whatever their status.
	Listings(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ListingInfo, error)
	// OrganisationListings returns every listing of the organisation,
	// whatever its status.
	OrganisationListings(ctx context.Context, organisationID uuid.UUID) ([]ListingInfo, error)
}

// Zones gives every market's IANA time zone, keyed by market code.
type Zones interface {
	CountryZones(ctx context.Context) (map[string]string, error)
}

// Deps is what the Service needs.
type Deps struct {
	Store    Store
	Listings Listings
	Zones    Zones
	Logger   *slog.Logger
	// Now nil means time.Now.
	Now func() time.Time
	// RawRetentionDays is ANALYTICS_RAW_RETENTION_DAYS (0: the default, 90).
	RawRetentionDays int
}

// Service ingests, records, rolls up and reports.
type Service struct {
	store     Store
	listings  Listings
	zones     Zones
	logger    *slog.Logger
	now       func() time.Time
	retention int
	rates     *rateDetector

	mu        sync.Mutex
	zoneCache map[string]string
	zoneAt    time.Time
	locations map[string]*time.Location
}

// NewService builds the Service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.RawRetentionDays <= 0 {
		d.RawRetentionDays = DefaultRawRetentionDays
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{store: d.Store, listings: d.Listings, zones: d.Zones, logger: d.Logger, now: d.Now,
		retention: d.RawRetentionDays, rates: newRateDetector(RateLimit, RateWindow), locations: map[string]*time.Location{}}
}

// ---------------------------------------------------------------------------
// Market-local days
// ---------------------------------------------------------------------------

const zoneCacheTTL = 5 * time.Minute

// zoneMap returns the markets' time zones, cached briefly.
func (s *Service) zoneMap(ctx context.Context) (map[string]string, error) {
	now := s.now()
	s.mu.Lock()
	if s.zoneCache != nil && now.Sub(s.zoneAt) < zoneCacheTTL && !now.Before(s.zoneAt) {
		z := s.zoneCache
		s.mu.Unlock()
		return z, nil
	}
	s.mu.Unlock()
	z, err := s.zones.CountryZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("load market time zones: %w", err)
	}
	s.mu.Lock()
	s.zoneCache, s.zoneAt = z, now
	s.mu.Unlock()
	return z, nil
}

// location returns a market's time zone, UTC when it has none or it does
// not load.
func (s *Service) location(zones map[string]string, market string) *time.Location {
	name := zones[market]
	if name == "" {
		return time.UTC
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if loc, ok := s.locations[name]; ok {
		return loc
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		s.logger.Warn("analytics: market time zone does not load, using UTC", slog.String("market", market), slog.String("zone", name))
		loc = time.UTC
	}
	s.locations[name] = loc
	return loc
}

const dayLayout = "2006-01-02"

// LocalDay is the calendar day of t in loc (YYYY-MM-DD).
func LocalDay(t time.Time, loc *time.Location) string { return t.In(loc).Format(dayLayout) }

func addDays(day string, n int) string {
	t, err := time.Parse(dayLayout, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format(dayLayout)
}

// ---------------------------------------------------------------------------
// Ingestion (browser)
// ---------------------------------------------------------------------------

// Client is what is read from the request: never stored as such.
type Client struct {
	UserAgent string
	IP        string
	// Untracked: Do Not Track or Global Privacy Control is on. The
	// browser sends nothing then; an event that comes anyway keeps no
	// campaign (API 0.58.0).
	Untracked bool
}

// IngestResult is the 202 body of POST /v1/analytics/events.
type IngestResult struct {
	Accepted   int `json:"accepted"`
	Duplicates int `json:"duplicates"`
	Rejected   int `json:"rejected"`
}

// browserEvent is one event as the browser sends it.
type browserEvent struct {
	EventID    string  `json:"event_id"`
	Type       string  `json:"type"`
	OccurredAt string  `json:"occurred_at"`
	ListingID  string  `json:"listing_id"`
	Placement  *string `json:"placement"`
	Rank       *int    `json:"rank"`
	Source     *string `json:"source"`
	Device     *string `json:"device"`
	Session    *string `json:"session"`
	// Campaign is the campaign the visit came from (API 0.58.0), cleaned
	// and never a reason to reject the event.
	Campaign *attribution.Input `json:"campaign"`
	Consent  *struct {
		Analytics *bool `json:"analytics"`
	} `json:"consent"`
}

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

const maxRank = 10000

// parseBrowserEvent validates one event. ok is false for an event that is
// rejected (including every server-only type).
func parseBrowserEvent(raw json.RawMessage) (Event, bool) {
	var in browserEvent
	if err := json.Unmarshal(raw, &in); err != nil {
		return Event{}, false
	}
	id, err := uuid.Parse(in.EventID)
	if err != nil || id == uuid.Nil {
		return Event{}, false
	}
	if !slices.Contains(BrowserTypes(), in.Type) {
		return Event{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, in.OccurredAt)
	if err != nil {
		return Event{}, false
	}
	listing, err := uuid.Parse(in.ListingID)
	if err != nil {
		return Event{}, false
	}
	if in.Consent != nil && in.Consent.Analytics != nil && !*in.Consent.Analytics {
		return Event{}, false
	}
	ev := Event{ID: id, Type: in.Type, OccurredAt: at.UTC(), ListingID: listing, Origin: OriginBrowser}
	if in.Type == TypeCardImpression {
		if in.Placement != nil {
			if !slices.Contains(Placements(), *in.Placement) {
				return Event{}, false
			}
			ev.Placement = *in.Placement
		}
		if in.Rank != nil {
			if *in.Rank < 0 || *in.Rank > maxRank {
				return Event{}, false
			}
			r := *in.Rank
			ev.Rank = &r
		}
	}
	if in.Source != nil && *in.Source != "" {
		if !slices.Contains(Sources(), *in.Source) {
			return Event{}, false
		}
		ev.Source = *in.Source
	}
	if in.Device != nil && *in.Device != "" {
		if !slices.Contains(Devices(), *in.Device) {
			return Event{}, false
		}
		ev.Device = *in.Device
	}
	if in.Session != nil && *in.Session != "" {
		if !sessionRe.MatchString(*in.Session) {
			return Event{}, false
		}
		ev.Session = *in.Session
	}
	ev.Campaign = attribution.From(in.Campaign)
	return ev, true
}

// Ingest handles one browser batch. A request-level problem (schema, number
// of events) is a *ValidationError; a problem with one event only rejects
// that event. Do Not Track is not read here: the browser does not send
// events under it.
func (s *Service) Ingest(ctx context.Context, c Client, schema int, raw []json.RawMessage) (IngestResult, error) {
	fields := map[string]string{}
	if schema != SchemaVersion {
		fields["schema"] = fmt.Sprintf("must be %d", SchemaVersion)
	}
	if len(raw) < 1 || len(raw) > MaxEventsPerBatch {
		fields["events"] = fmt.Sprintf("must hold 1 to %d events", MaxEventsPerBatch)
	}
	if len(fields) > 0 {
		return IngestResult{}, &ValidationError{Fields: fields}
	}
	now := s.now().UTC()
	var res IngestResult
	events := make([]Event, 0, len(raw))
	ids := []uuid.UUID{}
	for _, r := range raw {
		ev, ok := parseBrowserEvent(r)
		if !ok {
			res.Rejected++
			continue
		}
		if c.Untracked {
			ev.Campaign = attribution.Campaign{}
		}
		events = append(events, ev)
		if !slices.Contains(ids, ev.ListingID) {
			ids = append(ids, ev.ListingID)
		}
	}
	if len(events) == 0 {
		return res, nil
	}
	known, err := s.listings.Listings(ctx, ids)
	if err != nil {
		return IngestResult{}, fmt.Errorf("load listings: %w", err)
	}
	zones, err := s.zoneMap(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	network := TruncateIP(c.IP)
	uaBot := IsBotUserAgent(c.UserAgent)
	for _, ev := range events {
		info, ok := known[ev.ListingID]
		if !ok {
			res.Rejected++
			continue
		}
		loc := s.location(zones, info.MarketCode)
		if now.Sub(ev.OccurredAt) > LateAfter {
			if err := s.recordLate(ctx, ev.ID, ev.Type, info, ev.OccurredAt, now, loc); err != nil {
				return IngestResult{}, err
			}
			res.Rejected++
			continue
		}
		if now.Sub(ev.OccurredAt) > PastWindow || ev.OccurredAt.Sub(now) > FutureWindow || !info.Public {
			res.Rejected++
			continue
		}
		if ev.OccurredAt.After(now) {
			ev.OccurredAt = now
		}
		ev.ReceivedAt, ev.MarketCode, ev.LocalDay, ev.Network = now, info.MarketCode, LocalDay(ev.OccurredAt, loc), network
		sessionKey := ""
		if ev.Session != "" {
			sessionKey = "s:" + ev.Session
		}
		networkKey := ""
		if network != "" {
			networkKey = "n:" + network
		}
		rateBot := s.rates.over(now, sessionKey, networkKey)
		switch {
		case uaBot:
			ev.Bot, ev.BotReason = true, BotUserAgent
		case rateBot:
			ev.Bot, ev.BotReason = true, BotRate
		}
		out, err := s.store.Insert(ctx, ev, SPAWindow)
		if err != nil {
			return IngestResult{}, fmt.Errorf("store analytics event: %w", err)
		}
		if out == Inserted {
			// A bot's event is accepted too: it is stored, flagged and
			// excluded, and the client is not told.
			res.Accepted++
		} else {
			res.Duplicates++
		}
	}
	return res, nil
}

func (s *Service) recordLate(ctx context.Context, id uuid.UUID, eventType string, info ListingInfo, occurred, now time.Time, loc *time.Location) error {
	if err := s.store.RecordLate(ctx, LateEvent{EventID: id, Type: eventType, ListingID: info.ID, MarketCode: info.MarketCode,
		OccurredAt: occurred, ReceivedAt: now, LocalDay: LocalDay(now, loc)}); err != nil {
		return fmt.Errorf("record late analytics event: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Server-side events
// ---------------------------------------------------------------------------

// serverNamespace derives a server-side event's event_id from the ID of
// what caused it (an outbox event), so that recording it again is a no-op
// and it can never collide with a browser's random UUID.
var serverNamespace = uuid.MustParse("8f2d0a8e-7c1b-5b5e-9a63-30a0c0de0030")

// ServerEventID is the event_id of the server-side event caused by source.
func ServerEventID(source uuid.UUID) uuid.UUID { return uuid.NewSHA1(serverNamespace, source[:]) }

// ServerEvent is an action the API performed that analytics counts.
type ServerEvent struct {
	// SourceID identifies the cause (the outbox event's ID); the event_id is
	// derived from it.
	SourceID   uuid.UUID
	Type       string
	ListingID  uuid.UUID
	OccurredAt time.Time
	// Campaign is the campaign of the visit that caused it (API 0.58.0).
	Campaign attribution.Campaign
}

// RecordServer records a server-side event. A listing that does not exist
// is skipped (nil): there is nothing to count it against. Recording the same
// SourceID again changes nothing.
func (s *Service) RecordServer(ctx context.Context, e ServerEvent) error {
	if !slices.Contains(ServerTypes(), e.Type) {
		return fmt.Errorf("analytics: %q is not a server-side event type", e.Type)
	}
	known, err := s.listings.Listings(ctx, []uuid.UUID{e.ListingID})
	if err != nil {
		return fmt.Errorf("load listing: %w", err)
	}
	info, ok := known[e.ListingID]
	if !ok {
		s.logger.Info("analytics: server-side event for an unknown listing skipped", slog.String("type", e.Type), slog.String("listing_id", e.ListingID.String()))
		return nil
	}
	zones, err := s.zoneMap(ctx)
	if err != nil {
		return err
	}
	loc := s.location(zones, info.MarketCode)
	now := s.now().UTC()
	at := e.OccurredAt.UTC()
	if at.IsZero() || at.After(now) {
		at = now
	}
	id := ServerEventID(e.SourceID)
	if now.Sub(at) > LateAfter {
		return s.recordLate(ctx, id, e.Type, info, at, now, loc)
	}
	ev := Event{ID: id, Type: e.Type, OccurredAt: at, ReceivedAt: now, LocalDay: LocalDay(at, loc), ListingID: info.ID,
		MarketCode: info.MarketCode, Origin: OriginServer, Campaign: e.Campaign}
	if _, err := s.store.Insert(ctx, ev, SPAWindow); err != nil {
		return fmt.Errorf("store analytics event: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Worker jobs
// ---------------------------------------------------------------------------

// RollupResult says what a rollup run did.
type RollupResult struct {
	Markets int
	Rows    int
}

// Rollup recomputes the last RollupDays market-local days (today included)
// of every market with recent events and upserts them: running it again
// gives the same rows. Events that arrive later than that are dropped as
// late at ingestion, so a day is final once it leaves the window.
func (s *Service) Rollup(ctx context.Context) (RollupResult, error) {
	now := s.now().UTC()
	zones, err := s.zoneMap(ctx)
	if err != nil {
		return RollupResult{}, err
	}
	markets, err := s.store.ActiveMarkets(ctx, now.Add(-time.Duration(RollupDays+1)*24*time.Hour))
	if err != nil {
		return RollupResult{}, fmt.Errorf("active analytics markets: %w", err)
	}
	var rows []DayRow
	for _, m := range markets {
		from := addDays(LocalDay(now, s.location(zones, m)), -(RollupDays - 1))
		got, err := s.store.Aggregate(ctx, AggregateFilter{MarketCodes: []string{m}, FromDay: from})
		if err != nil {
			return RollupResult{}, fmt.Errorf("aggregate analytics events (%s): %w", m, err)
		}
		rows = append(rows, got...)
	}
	if err := s.store.UpsertRollups(ctx, rows, now); err != nil {
		return RollupResult{}, fmt.Errorf("upsert analytics rollups: %w", err)
	}
	return RollupResult{Markets: len(markets), Rows: len(rows)}, nil
}

// RetentionCutoff is the time before which raw events are deleted.
func (s *Service) RetentionCutoff() time.Time {
	return s.now().UTC().Add(-time.Duration(s.retention) * 24 * time.Hour)
}

// PurgeRaw deletes the raw and late events older than the retention
// (ANALYTICS_RAW_RETENTION_DAYS). Rollups are kept.
func (s *Service) PurgeRaw(ctx context.Context) (events, late int64, err error) {
	events, late, err = s.store.DeleteBefore(ctx, s.RetentionCutoff())
	if err != nil {
		return 0, 0, fmt.Errorf("delete old analytics events: %w", err)
	}
	return events, late, nil
}
