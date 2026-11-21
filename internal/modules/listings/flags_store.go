package listings

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// notCompared are the statuses the duplicate check leaves out.
var notCompared = []string{StatusArchived, "REJECTED", "CLOSED"}

func compared(status string) bool {
	for _, s := range notCompared {
		if s == status {
			return false
		}
	}
	return true
}

// SimilarListings implements FlagStore.
func (s *MemoryStore) SimilarListings(_ context.Context, q SimilarQuery) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []string{}
	for i, r := range s.records {
		if len(out) == 3 {
			break
		}
		if r.ID == q.ExceptID || r.MarketCode != q.MarketCode || !compared(r.Status) {
			continue
		}
		c := s.managedLocked(i).WorkingContent()
		sameTitle := q.Title != "" && normalTitle(c.Title) == q.Title
		samePlace := q.PriceAmount != "" && q.City != "" && q.Bedrooms != nil && c.Bedrooms != nil && c.PriceAmount == q.PriceAmount &&
			c.Currency == q.Currency && c.City == q.City && c.Suburb == q.Suburb && *c.Bedrooms == *q.Bedrooms
		if sameTitle || samePlace {
			out = append(out, r.Reference)
		}
	}
	return out, nil
}

// CreatedSince implements FlagStore.
func (s *MemoryStore) CreatedSince(_ context.Context, organisationID, sellerUserID *uuid.UUID, since time.Time) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for i, r := range s.records {
		m := s.managedLocked(i)
		if m.CreatedAt.Before(since) {
			continue
		}
		if (organisationID != nil && r.OrganisationID != nil && *r.OrganisationID == *organisationID) ||
			(organisationID == nil && sellerUserID != nil && r.OrganisationID == nil && r.SellerUserID != nil && *r.SellerUserID == *sellerUserID) {
			n++
		}
	}
	return n, nil
}

// SimilarListings implements FlagStore. Listings' working titles live on
// the row (or a revision's content); the row is compared.
func (s *PostgresStore) SimilarListings(ctx context.Context, q SimilarQuery) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT COALESCE(l.reference, '') FROM listings l JOIN markets m ON m.id = l.market_id
		WHERE l.id <> $1 AND m.code = $2 AND l.status::text <> ALL($3)
		  AND ((($4 <> '') AND lower(regexp_replace(trim(l.title), '\s+', ' ', 'g')) = $4)
		    OR ($5 <> '' AND $7 <> '' AND $9::int IS NOT NULL AND l.price_amount = $5::text::numeric AND l.currency_code = $6
		        AND l.city_code = $7 AND COALESCE(l.suburb_code, '') = $8 AND l.bedrooms = $9))
		ORDER BY l.updated_at DESC LIMIT 3`,
		q.ExceptID, q.MarketCode, notCompared, q.Title, q.PriceAmount, q.Currency, q.City, q.Suburb, q.Bedrooms)
	if err != nil {
		return nil, err
	}
	var out []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// CreatedSince implements FlagStore.
func (s *PostgresStore) CreatedSince(ctx context.Context, organisationID, sellerUserID *uuid.UUID, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM listings
		WHERE created_at >= $3 AND (($1::uuid IS NOT NULL AND organisation_id = $1)
		   OR ($1::uuid IS NULL AND $2::uuid IS NOT NULL AND organisation_id IS NULL AND seller_user_id = $2))`,
		organisationID, sellerUserID, since).Scan(&n)
	return n, err
}
