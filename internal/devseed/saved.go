package devseed

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/saved"
)

// SavedListingSamples are the saved listings of the sample tenant
// (tenant@example.test): one public sample listing, and the EXPIRED one
// (saved before it expired), so the activity hub's "Saved" tab shows an
// available and an unavailable item.
func SavedListingSamples(now time.Time) []saved.Saved {
	now = now.UTC().Truncate(time.Hour)
	labels := func(set string) map[string]string {
		out := map[string]string{}
		for _, it := range ReferenceItems()[set] {
			out[it.Code] = it.Label
		}
		return out
	}
	categories, cities := labels(listings.SetPropertyCategories), labels(listings.SetCities)
	countries := map[string]string{}
	for _, m := range markets.Seed() {
		countries[m.Code] = m.Name
	}
	snapshot := func(r listings.Record) saved.Snapshot {
		l := listings.Listing{ID: r.ID, Slug: r.Slug, Reference: r.Reference, Title: r.Title,
			Category: listings.Coded{Code: r.Category, Label: categories[r.Category]}, Price: r.Price,
			City: listings.PlaceOf(cities, r.City, r.CityName), Country: listings.Country{Code: r.MarketCode, Name: countries[r.MarketCode]},
			Photos: r.Photos}
		if r.PricePeriod != "" {
			period := r.PricePeriod
			l.PricePeriod = &period
		}
		return saved.SnapshotOf(l)
	}

	var public, expired *listings.Record
	records := ListingRecords(now)
	for i := range records {
		r := &records[i]
		switch {
		case r.ID == ExpiredListingID:
			expired = r
		case public == nil && r.Status == listings.StatusPublished && r.Advertiser.Verified && r.Category != listings.CategoryShortStay:
			public = r
		}
	}
	var out []saved.Saved
	if public != nil {
		at := now.AddDate(0, 0, -2)
		out = append(out, saved.Saved{UserID: TenantID, ListingID: public.ID, SavedAt: at, Snapshot: snapshot(*public), SnapshotAt: at})
	}
	if expired != nil {
		// Saved while it was public, 15 days before it expired.
		at := expired.ExpiresAt.AddDate(0, 0, -15)
		out = append(out, saved.Saved{UserID: TenantID, ListingID: expired.ID, SavedAt: at, Snapshot: snapshot(*expired), SnapshotAt: at})
	}
	return out
}

// SeedMemorySaved stores the saved-listing samples in memory (no events).
func SeedMemorySaved(store *saved.MemoryStore, now time.Time) {
	for _, x := range SavedListingSamples(now) {
		store.Put(x)
	}
}

// seedPostgresSaved stores the same samples (needs migration 0026). A save
// that is already there is left alone; no outbox events are written.
func seedPostgresSaved(ctx context.Context, tx pgx.Tx, now time.Time) error {
	for _, x := range SavedListingSamples(now) {
		snap, err := json.Marshal(x.Snapshot)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO saved_listings (user_id, listing_id, saved_at, snapshot, snapshot_at)
			VALUES ($1, $2, $3, $4::jsonb, $5)
			ON CONFLICT (user_id, listing_id) DO NOTHING`, x.UserID, x.ListingID, x.SavedAt, string(snap), x.SnapshotAt); err != nil {
			return fmt.Errorf("seed saved listing %s: %w", x.ListingID, err)
		}
	}
	return nil
}
