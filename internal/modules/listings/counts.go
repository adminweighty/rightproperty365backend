package listings

import (
	"context"
	"slices"
	"time"
)

// StatusExpired is what a PUBLISHED listing counts as once its publication
// period is over, whether or not anything has rewritten its stored status.
const StatusExpired = "EXPIRED"

// CountByStatus implements Store.
func (s *MemoryStore) CountByStatus(_ context.Context, marketCodes []string, now time.Time) (map[string]int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]int{}
	for _, r := range s.records {
		if marketCodes != nil && !slices.Contains(marketCodes, r.MarketCode) {
			continue
		}
		status := r.Status
		if status == StatusPublished && !r.ExpiresAt.After(now) {
			status = StatusExpired
		}
		out[status]++
	}
	return out, nil
}

// CountByStatus implements Store.
//
// NOTE: compiled but not yet exercised against a real database.
func (s *PostgresStore) CountByStatus(ctx context.Context, marketCodes []string, now time.Time) (map[string]int, error) {
	// A nil slice is sent as NULL: every country.
	rows, err := s.pool.Query(ctx, `
		SELECT CASE WHEN l.status = 'PUBLISHED' AND l.expires_at IS NOT NULL AND l.expires_at <= $2
		            THEN 'EXPIRED' ELSE l.status::text END, count(*)
		FROM listings l JOIN markets m ON m.id = l.market_id
		WHERE $1::text[] IS NULL OR m.code = ANY($1::text[])
		GROUP BY 1`, marketCodes, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}
