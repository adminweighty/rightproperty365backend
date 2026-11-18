package analytics

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is the in-memory Store (development and tests).
type MemoryStore struct {
	mu          sync.Mutex
	events      []Event
	ids         map[uuid.UUID]bool
	late        map[uuid.UUID]LateEvent
	rollups     map[rollupKey]DayRow
	lastRefresh *time.Time
}

type rollupKey struct {
	listing uuid.UUID
	day     string
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{ids: map[uuid.UUID]bool{}, late: map[uuid.UUID]LateEvent{}, rollups: map[rollupKey]DayRow{}}
}

// Events returns a copy of the stored raw events (test helper).
func (s *MemoryStore) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Event(nil), s.events...)
}

// Insert implements Store.
func (s *MemoryStore) Insert(_ context.Context, ev Event, spaWindow time.Duration) (InsertResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids[ev.ID] {
		return Duplicate, nil
	}
	if ev.Type == TypeDetailView && !ev.Bot && ev.Session != "" {
		for _, x := range s.events {
			if x.Type == TypeDetailView && !x.Bot && x.Session == ev.Session && x.ListingID == ev.ListingID &&
				absDuration(x.OccurredAt.Sub(ev.OccurredAt)) < spaWindow {
				return SPADuplicate, nil
			}
		}
	}
	s.ids[ev.ID] = true
	s.events = append(s.events, ev)
	return Inserted, nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// RecordLate implements Store.
func (s *MemoryStore) RecordLate(_ context.Context, l LateEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.late[l.EventID]; !ok {
		s.late[l.EventID] = l
	}
	return nil
}

// Aggregate implements Store.
func (s *MemoryStore) Aggregate(_ context.Context, f AggregateFilter) ([]DayRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []Event
	for _, ev := range s.events {
		if matchesCodes(f.MarketCodes, ev.MarketCode) && matchesIDs(f.ListingIDs, ev.ListingID) &&
			(f.FromDay == "" || ev.LocalDay >= f.FromDay) && (f.Since.IsZero() || !ev.OccurredAt.Before(f.Since)) {
			events = append(events, ev)
		}
	}
	var late []LateEvent
	for _, l := range s.late {
		if matchesCodes(f.MarketCodes, l.MarketCode) && matchesIDs(f.ListingIDs, l.ListingID) &&
			(f.FromDay == "" || l.LocalDay >= f.FromDay) && (f.Since.IsZero() || !l.ReceivedAt.Before(f.Since)) {
			late = append(late, l)
		}
	}
	return aggregateEvents(events, late), nil
}

// aggregateEvents is the metric contract over raw events (the Postgres
// store's query computes the same): views are valid listing_detail_view
// events; unique views are distinct sessions per listing and local day,
// where an event without a session counts once on its own; bots are only
// counted as filtered.
func aggregateEvents(events []Event, late []LateEvent) []DayRow {
	rows := map[rollupKey]*DayRow{}
	sessions := map[rollupKey]map[string]bool{}
	row := func(listing uuid.UUID, day, market string) *DayRow {
		k := rollupKey{listing, day}
		r := rows[k]
		if r == nil {
			r = &DayRow{ListingID: listing, Day: day, MarketCode: market, Sources: map[string]int{}, Devices: map[string]int{}}
			rows[k] = r
		}
		return r
	}
	for _, ev := range events {
		r := row(ev.ListingID, ev.LocalDay, ev.MarketCode)
		if ev.Bot {
			r.FilteredBotEvents++
			continue
		}
		r.count(ev.Type)
		if ev.Type != TypeDetailView {
			continue
		}
		r.Sources[orUnknown(ev.Source)]++
		r.Devices[orUnknown(ev.Device)]++
		if ev.Session == "" {
			r.UniqueViews++
			continue
		}
		k := rollupKey{ev.ListingID, ev.LocalDay}
		if sessions[k] == nil {
			sessions[k] = map[string]bool{}
		}
		if !sessions[k][ev.Session] {
			sessions[k][ev.Session] = true
			r.UniqueViews++
		}
	}
	for _, l := range late {
		row(l.ListingID, l.LocalDay, l.MarketCode).LateEventsDropped++
	}
	out := make([]DayRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sortRows(out)
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return Unknown
	}
	return s
}

func sortRows(rows []DayRow) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Day != rows[j].Day {
			return rows[i].Day < rows[j].Day
		}
		return rows[i].ListingID.String() < rows[j].ListingID.String()
	})
}

// ActiveMarkets implements Store.
func (s *MemoryStore) ActiveMarkets(_ context.Context, since time.Time) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	for _, ev := range s.events {
		if !ev.OccurredAt.Before(since) {
			seen[ev.MarketCode] = true
		}
	}
	for _, l := range s.late {
		if !l.ReceivedAt.Before(since) {
			seen[l.MarketCode] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// UpsertRollups implements Store.
func (s *MemoryStore) UpsertRollups(_ context.Context, rows []DayRow, refreshedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rows {
		r.RefreshedAt = refreshedAt
		r.Sources, r.Devices = copyCounts(r.Sources), copyCounts(r.Devices)
		s.rollups[rollupKey{r.ListingID, r.Day}] = r
	}
	at := refreshedAt
	s.lastRefresh = &at
	return nil
}

func copyCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Rollups implements Store.
func (s *MemoryStore) Rollups(_ context.Context, f RollupFilter) ([]DayRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []DayRow{}
	for _, r := range s.rollups {
		if matchesCodes(f.MarketCodes, r.MarketCode) && matchesIDs(f.ListingIDs, r.ListingID) &&
			(f.FromDay == "" || r.Day >= f.FromDay) && (f.ToDay == "" || r.Day <= f.ToDay) {
			r.Sources, r.Devices = copyCounts(r.Sources), copyCounts(r.Devices)
			out = append(out, r)
		}
	}
	sortRows(out)
	return out, nil
}

// LastRefresh implements Store.
func (s *MemoryStore) LastRefresh(context.Context) (*time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRefresh == nil {
		return nil, nil
	}
	at := *s.lastRefresh
	return &at, nil
}

// DeleteBefore implements Store.
func (s *MemoryStore) DeleteBefore(_ context.Context, cutoff time.Time) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var events, late int64
	kept := s.events[:0]
	for _, ev := range s.events {
		if ev.OccurredAt.Before(cutoff) {
			delete(s.ids, ev.ID)
			events++
			continue
		}
		kept = append(kept, ev)
	}
	s.events = kept
	for id, l := range s.late {
		if l.ReceivedAt.Before(cutoff) {
			delete(s.late, id)
			late++
		}
	}
	return events, late, nil
}
