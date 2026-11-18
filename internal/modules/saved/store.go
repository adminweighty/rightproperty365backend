package saved

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Outbox names shared by both stores. The aggregate is the listing; the
// payload carries identifiers only: {user_id, listing_id, at}. Analytics
// counts saves from these events; nothing is emailed.
const (
	EventListingSaved   = "LISTING_SAVED"
	EventListingUnsaved = "LISTING_UNSAVED"
	AggregateListing    = "listing"
)

// EventTypes returns the outbox events this module writes (the worker
// acknowledges them until a consumer exists).
func EventTypes() []string { return []string{EventListingSaved, EventListingUnsaved} }

// MaxIDs caps GET /v1/me/saved-listings/ids.
const MaxIDs = 1000

// Store persists saved listings.
type Store interface {
	// Save stores s unless the user already saved the listing. created
	// reports whether a row was written; when it was, the LISTING_SAVED
	// event is written with it (one transaction in Postgres). The stored row
	// is returned either way, so an existing save keeps its first saved_at.
	Save(ctx context.Context, s Saved) (stored Saved, created bool, err error)

	// Remove deletes the user's save of the listing. removed reports whether
	// there was one; only then is LISTING_UNSAVED written with the delete.
	Remove(ctx context.Context, userID, listingID uuid.UUID, at time.Time) (removed bool, err error)

	// List returns one page of the user's saves, newest saved_at first (then
	// by listing ID), and how many there are in all.
	List(ctx context.Context, userID uuid.UUID, limit, offset int) ([]Saved, int, error)

	// ListingIDs returns the IDs of every listing the user saved, newest first.
	ListingIDs(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error)

	// UpdateSnapshot replaces the snapshot of one save (a listing seen public
	// again). A save that no longer exists is left alone.
	UpdateSnapshot(ctx context.Context, userID, listingID uuid.UUID, snap Snapshot, at time.Time) error
}

// eventPayload is the body of LISTING_SAVED and LISTING_UNSAVED.
func eventPayload(userID, listingID uuid.UUID, at time.Time) []byte {
	raw, _ := json.Marshal(map[string]string{
		"user_id": userID.String(), "listing_id": listingID.String(), "at": at.UTC().Format(time.RFC3339Nano),
	})
	return raw
}
