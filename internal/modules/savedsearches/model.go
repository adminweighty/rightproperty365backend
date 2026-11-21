// Package savedsearches implements saved searches and their alerts (API
// 0.52.0, FR-042): a signed-in customer saves a listing search (its query
// string, as GET /v1/listings takes it) with a frequency; the worker runs it
// against the database search for listings published since the last check
// and, when there are new ones, writes one SAVED_SEARCH_MATCHED outbox event
// in the same transaction as the new watermark. The notification handlers
// send the email and the in-app notice, respecting the preferences; each
// email has a one-click unsubscribe link for that search.
package savedsearches

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

// Frequencies.
const (
	FrequencyInstant = "INSTANT"
	FrequencyDaily   = "DAILY"
	FrequencyWeekly  = "WEEKLY"
	FrequencyOff     = "OFF"
)

// Frequencies returns every frequency.
func Frequencies() []string {
	return []string{FrequencyInstant, FrequencyDaily, FrequencyWeekly, FrequencyOff}
}

// Interval is how often a frequency is checked (0 for OFF).
func Interval(frequency string) time.Duration {
	switch frequency {
	case FrequencyInstant:
		return 10 * time.Minute
	case FrequencyDaily:
		return 24 * time.Hour
	case FrequencyWeekly:
		return 7 * 24 * time.Hour
	}
	return 0
}

// Limits.
const (
	MaxPerUser     = 25
	MaxNameRunes   = 80
	MaxQueryLength = 2000
	// MaxLines is how many listings one alert names.
	MaxLines = 10
	// SweepInterval is how often the worker looks for due searches.
	SweepInterval = 2 * time.Minute
	// claimLease keeps a claimed search from being claimed again while it
	// is being matched.
	claimLease = 10 * time.Minute
	sweepBatch = 100
)

// EventMatched is the outbox event of an alert.
const (
	AggregateSavedSearch = "saved_search"
	EventMatched         = "SAVED_SEARCH_MATCHED"
)

// EventTypes lists the outbox events this module writes.
func EventTypes() []string { return []string{EventMatched} }

// Errors.
var (
	ErrNotFound     = errors.New("saved search not found")
	ErrLimit        = errors.New("too many saved searches")
	ErrInvalidToken = errors.New("unknown unsubscribe token")
)

// ValidationError carries per-field messages for a 400.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// SavedSearch is one saved search.
type SavedSearch struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	Name      string
	Query     string
	Frequency string
	// UnsubscribeToken is the capability in the alert emails' unsubscribe
	// link: it can only switch this search's alerts off.
	UnsubscribeToken string
	// LastCheckedAt is the watermark: listings published after it are new.
	LastCheckedAt time.Time
	// NextCheckAt is when the worker next runs it (nil when OFF).
	NextCheckAt *time.Time
	LastAlertAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Line is one listing named in an alert: public catalogue data only.
type Line struct {
	ListingID uuid.UUID `json:"listing_id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Place     string    `json:"place"`
	Price     string    `json:"price"`
}

// Alert is what one run found.
type Alert struct {
	Lines []Line
	// Total is how many new listings matched (Lines holds the first ones).
	Total int
	// More is true when there may be even more than Total.
	More bool
}

// EventPayload is the body of SAVED_SEARCH_MATCHED.
type EventPayload struct {
	SavedSearchID uuid.UUID `json:"saved_search_id"`
	UserID        uuid.UUID `json:"user_id"`
	Total         int       `json:"total"`
	More          bool      `json:"more"`
	Lines         []Line    `json:"lines"`
	OccurredAt    time.Time `json:"occurred_at"`
}
