// Package trust serves the numbers behind the site's claim that every
// listing comes from a verified advertiser.
//
// The claim itself is not made here: it is ENFORCED where listings are read
// (listings.Record.Public and the Postgres public condition). This package
// only counts, so the site can show real figures, and so anyone can check
// that the listings shown and the listings of verified advertisers are the
// same number.
package trust

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Cache lifetimes of GET /v1/trust/summary.
const (
	CacheMaxAge = time.Minute
	CacheStale  = 5 * time.Minute
)

// Sources are where the numbers come from.
type Sources struct {
	// Listings counts the public listings.
	Listings func(ctx context.Context) (listings.PublicCounts, error)
	// Organisations counts the organisations with a public, verified profile.
	Organisations func(ctx context.Context) (int, error)
	// DirectOwners counts the accounts with an approved PRIVATE_SELLER or
	// LANDLORD role.
	DirectOwners func(ctx context.Context) (int, error)
	Now          func() time.Time
}

// VerifiedAdvertisers counts who may advertise.
type VerifiedAdvertisers struct {
	Organisations int `json:"organisations"`
	DirectOwners  int `json:"direct_owners"`
}

// Summary is the body of GET /v1/trust/summary.
type Summary struct {
	VerifiedAdvertisers VerifiedAdvertisers `json:"verified_advertisers"`
	// PublicListings is every listing the API shows right now.
	PublicListings int `json:"public_listings"`
	// ListingsFromVerifiedAdvertisers is counted separately from
	// PublicListings; the two are equal because an unverified advertiser's
	// listing is never public.
	ListingsFromVerifiedAdvertisers int `json:"listings_from_verified_advertisers"`
	// ListingsReviewed is how many of the public listings were themselves
	// reviewed by a platform administrator or moderator.
	ListingsReviewed int       `json:"listings_reviewed"`
	GeneratedAt      time.Time `json:"generated_at"`
}

// Handler serves GET /v1/trust/summary.
type Handler struct {
	src    Sources
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(src Sources, logger *slog.Logger) *Handler {
	if src.Now == nil {
		src.Now = time.Now
	}
	return &Handler{src: src, logger: logger}
}

// Summary handles GET /v1/trust/summary. Public and cacheable.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts, err := h.src.Listings(ctx)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	organisations, err := h.src.Organisations(ctx)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	owners, err := h.src.DirectOwners(ctx)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, Summary{
		VerifiedAdvertisers: VerifiedAdvertisers{Organisations: organisations, DirectOwners: owners},
		PublicListings:      counts.Public, ListingsFromVerifiedAdvertisers: counts.VerifiedAdvertiser, ListingsReviewed: counts.Reviewed,
		// To the minute: the body, and so its ETag, is stable while it is cached.
		GeneratedAt: h.src.Now().UTC().Truncate(time.Minute),
	}, CacheMaxAge, CacheStale)
}
