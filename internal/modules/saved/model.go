// Package saved keeps the listings a customer saved ("Saved" in the
// customer activity hub, FR-016). A saved listing stays in the list after
// it stops being public: it is then shown from a snapshot of its card, never
// linked back to the listing or into search.
package saved

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
)

// Errors returned by the Service.
var (
	// ErrUnauthenticated: no session.
	ErrUnauthenticated = errors.New("saved: not signed in")
	// ErrForbidden: an admin-scope session (saving is for customer sessions).
	ErrForbidden = errors.New("saved: not a customer session")
	// ErrListingNotFound: the listing is unknown or not public right now.
	ErrListingNotFound = errors.New("saved: listing not found")
)

// Why a saved listing is unavailable.
const (
	ReasonExpired   = "EXPIRED"
	ReasonLet       = "LET"
	ReasonSold      = "SOLD"
	ReasonWithdrawn = "WITHDRAWN"
)

// Listing statuses the reasons are mapped from (listings.status).
const (
	statusLet  = "LET"
	statusSold = "SOLD"
)

// reasonLabels are the labels of the reasons. There is no reference set
// for listing statuses, so they are fixed here.
var reasonLabels = map[string]string{
	ReasonExpired:   "Listing expired",
	ReasonLet:       "Let",
	ReasonSold:      "Sold",
	ReasonWithdrawn: "No longer listed",
}

// Reasons returns every unavailable reason.
func Reasons() []string {
	return []string{ReasonExpired, ReasonLet, ReasonSold, ReasonWithdrawn}
}

// ReasonLabel is how a reason is shown.
func ReasonLabel(reason string) string { return reasonLabels[reason] }

// ReasonFor maps a listing's effective status (listings.Record.EffectiveStatus)
// to the reason it is not public: EXPIRED, LET and SOLD are themselves;
// everything else (ARCHIVED, CLOSED, PAUSED, a listing back in review, or a
// PUBLISHED one whose advertiser is no longer verified) is WITHDRAWN.
func ReasonFor(status string) string {
	switch status {
	case listings.StatusExpired:
		return ReasonExpired
	case statusLet:
		return ReasonLet
	case statusSold:
		return ReasonSold
	}
	return ReasonWithdrawn
}

// Snapshot is the card of a saved listing: the `listing` object of a
// SavedListing. It is stored with the save and refreshed while the listing
// is public. Slug is kept in storage; it is shown only while the listing is
// available.
type Snapshot struct {
	Slug        *string          `json:"slug"`
	Reference   string           `json:"reference"`
	Title       string           `json:"title"`
	Category    listings.Coded   `json:"category"`
	Price       listings.Money   `json:"price"`
	PricePeriod *string          `json:"price_period"`
	City        listings.Place   `json:"city"`
	Country     listings.Country `json:"country"`
	Photo       *listings.Photo  `json:"photo"`
}

// SnapshotOf builds the card from a listing's public form: its first photo
// only.
func SnapshotOf(l listings.Listing) Snapshot {
	slug := l.Slug
	s := Snapshot{
		Slug: &slug, Reference: l.Reference, Title: l.Title, Category: l.Category, Price: l.Price,
		City: l.City, Country: l.Country,
	}
	if l.PricePeriod != nil {
		period := *l.PricePeriod
		s.PricePeriod = &period
	}
	if l.City.Code != nil {
		code := *l.City.Code
		s.City.Code = &code
	}
	if len(l.Photos) > 0 {
		p := l.Photos[0]
		p.Variants = append([]listings.PhotoVariant{}, p.Variants...)
		s.Photo = &p
	}
	return s
}

// Saved is one stored save.
type Saved struct {
	UserID    uuid.UUID
	ListingID uuid.UUID
	// SavedAt is the first save; saving again does not move it.
	SavedAt time.Time
	// Snapshot is the card as it was last seen public, at SnapshotAt.
	Snapshot   Snapshot
	SnapshotAt time.Time
}

// Item is one SavedListing as the API returns it.
type Item struct {
	ListingID         uuid.UUID `json:"listing_id"`
	SavedAt           time.Time `json:"saved_at"`
	Available         bool      `json:"available"`
	UnavailableReason *string   `json:"unavailable_reason"`
	UnavailableLabel  *string   `json:"unavailable_label"`
	Listing           Snapshot  `json:"listing"`
}

// Page is one page of a customer's saved listings.
type Page struct {
	Items      []Item `json:"items"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	Total      int    `json:"total"`
	TotalPages int    `json:"total_pages"`
}

// SaveResult is the answer of PUT /v1/me/saved-listings/{listingId}.
type SaveResult struct {
	Saved   bool      `json:"saved"`
	SavedAt time.Time `json:"saved_at"`
	Created bool      `json:"created"`
}

// Catalogue is the public catalogue as this module needs it. The listings
// service implements it: whether each listing is public now is the listings
// module's own rule (Record.Public and Record.EffectiveStatus).
type Catalogue interface {
	CardStates(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]listings.CardState, error)
}
