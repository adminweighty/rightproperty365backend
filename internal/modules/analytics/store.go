package analytics

import (
	"context"
	"time"
)

// Store persists raw events, late events and the daily rollups (migration
// 0029). Both implementations must give the same answers: the FR-035
// fixture runs against each.
type Store interface {
	// Insert stores ev unless an event with the same ID exists (Duplicate).
	// A listing_detail_view that is not a bot and has a session is refused
	// as SPADuplicate when a non-bot view of the same listing in the same
	// session is stored less than spaWindow before or after it. The check
	// and the write are atomic.
	Insert(ctx context.Context, ev Event, spaWindow time.Duration) (InsertResult, error)

	// RecordLate keeps a late event's identity. Recording the same event
	// again changes nothing.
	RecordLate(ctx context.Context, l LateEvent) error

	// Aggregate works the daily figures out from the raw and late events
	// under the filter, one row per (listing, local day) that has any.
	Aggregate(ctx context.Context, f AggregateFilter) ([]DayRow, error)

	// ActiveMarkets returns the markets with events that occurred, or late
	// events that arrived, at or after since.
	ActiveMarkets(ctx context.Context, since time.Time) ([]string, error)

	// UpsertRollups writes the rows (keyed by listing and day), replacing
	// what was there, with refreshedAt, and records refreshedAt as the time
	// of the last rollup run.
	UpsertRollups(ctx context.Context, rows []DayRow, refreshedAt time.Time) error

	// Rollups returns the stored rows under the filter.
	Rollups(ctx context.Context, f RollupFilter) ([]DayRow, error)

	// LastRefresh returns the time of the last rollup run (nil: never).
	LastRefresh(ctx context.Context) (*time.Time, error)

	// DeleteBefore deletes the raw events that occurred, and the late
	// events that arrived, before cutoff. Rollups are kept.
	DeleteBefore(ctx context.Context, cutoff time.Time) (events, late int64, err error)
}
