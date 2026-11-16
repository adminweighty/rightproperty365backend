package reports

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/subscriptions"
)

// PostgresStore computes the reports with one aggregate query each, over
// the tables of the modules that own the data (read-only).
type PostgresStore struct {
	pool *pgxpool.Pool
	now  func() time.Time
	subs *subscriptions.PostgresStore
}

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore {
	return &PostgresStore{pool: pool, now: time.Now, subs: subscriptions.NewPostgresStore(pool)}
}

func markets(r Range) []string {
	if r.Markets == nil {
		return nil
	}
	return append([]string{}, r.Markets...)
}

func (s *PostgresStore) counts(ctx context.Context, what, sql string, args ...any) ([]Count, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("report %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Count, error) {
		var c Count
		err := row.Scan(&c.Group, &c.MarketCode, &c.Count)
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("report %s: %w", what, err)
	}
	if out == nil {
		out = []Count{}
	}
	return out, nil
}

const marketFilter = `($3::text[] IS NULL OR %s = ANY($3))`

func where(col string) string { return fmt.Sprintf(marketFilter, col) }

// NewUsers implements Store.
func (s *PostgresStore) NewUsers(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "new users", `
		WITH nu AS (
			SELECT u.id, COALESCE((SELECT min(mk.code) FROM memberships m JOIN markets mk ON mk.id = m.market_id WHERE m.user_id = u.id), '') AS market
			FROM users u
			WHERE u.created_at >= $1 AND u.created_at < $2
			  AND NOT EXISTS (SELECT 1 FROM platform_role_assignments a WHERE a.user_id = u.id))
		SELECT COALESCE(r.role, 'NONE'), nu.market, count(*)
		FROM nu LEFT JOIN user_account_roles r ON r.user_id = nu.id
		WHERE `+where("nu.market")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r))
}

// Onboarding implements Store.
func (s *PostgresStore) Onboarding(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "onboarding", `
		SELECT status, COALESCE(market_code, ''), count(*) FROM onboarding_applications
		WHERE created_at >= $1 AND created_at < $2 AND `+where("market_code")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r))
}

// Listings implements Store.
func (s *PostgresStore) Listings(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "listings", `
		SELECT CASE WHEN l.status = 'PUBLISHED' AND l.expires_at <= $4 THEN 'EXPIRED' ELSE l.status::text END, mk.code, count(*)
		FROM listings l JOIN markets mk ON mk.id = l.market_id
		WHERE l.created_at >= $1 AND l.created_at < $2 AND `+where("mk.code")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r), s.now())
}

// PropertyRequests implements Store.
func (s *PostgresStore) PropertyRequests(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "property requests", `
		SELECT CASE WHEN p.status = 'ROUTING_EXCEPTION' THEN 'ROUTING_EXCEPTION' ELSE p.stage END, mk.code, count(*)
		FROM property_requests p JOIN markets mk ON mk.id = p.market_id
		WHERE p.created_at >= $1 AND p.created_at < $2 AND `+where("mk.code")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r))
}

// BusinessEnquiries implements Store.
func (s *PostgresStore) BusinessEnquiries(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "business enquiries", `
		SELECT e.status, mk.code, count(*)
		FROM business_enquiries e JOIN markets mk ON mk.id = e.market_id
		WHERE e.created_at >= $1 AND e.created_at < $2 AND `+where("mk.code")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r))
}

// Leases implements Store.
func (s *PostgresStore) Leases(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "leases", `
		SELECT status, market_code, count(*) FROM leases
		WHERE created_at >= $1 AND created_at < $2 AND `+where("market_code")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r))
}

// Agreements implements Store. An agreement has no market: with a market
// filter there is nothing to count.
func (s *PostgresStore) Agreements(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "agreements", `
		SELECT status, '', count(*) FROM agreements
		WHERE created_at >= $1 AND created_at < $2 AND $3::text[] IS NULL
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r))
}

// Subscriptions implements Store.
func (s *PostgresStore) Subscriptions(ctx context.Context, r Range) ([]Count, error) {
	return s.counts(ctx, "subscriptions", `
		SELECT CASE WHEN status = 'ACTIVE' AND ends_at <= $4 THEN 'EXPIRED' ELSE status END, market_code, count(*) FROM subscriptions
		WHERE created_at >= $1 AND created_at < $2 AND `+where("market_code")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, r.From, r.To, markets(r), s.now())
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

// Revenue implements Store.
func (s *PostgresStore) Revenue(ctx context.Context, r Range) ([]Amount, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT currency_code, market_code, round(sum(payment_amount), 2)::text, count(*) FROM subscriptions
		WHERE payment_amount IS NOT NULL AND payment_received_on >= $1::date AND payment_received_on < $2::date AND `+where("market_code")+`
		GROUP BY 1, 2 ORDER BY 1, 2`, day(r.From), day(r.To), markets(r))
	if err != nil {
		return nil, fmt.Errorf("report revenue: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Amount, error) {
		var a Amount
		err := row.Scan(&a.CurrencyCode, &a.MarketCode, &a.Total, &a.Payments)
		return a, err
	})
	if err != nil {
		return nil, fmt.Errorf("report revenue: %w", err)
	}
	if out == nil {
		out = []Amount{}
	}
	return out, nil
}

const userMarket = `COALESCE((SELECT array_agg(DISTINCT mk.code) FROM memberships m JOIN markets mk ON mk.id = m.market_id
	WHERE m.user_id = u.id AND m.status <> 'REMOVED'), '{}')`

// ExportUsers implements Store.
func (s *PostgresStore) ExportUsers(ctx context.Context, r Range, limit int, start func(int), each func(UserRow) error) error {
	cond := `u.created_at >= $1 AND u.created_at < $2 AND ($3::text[] IS NULL OR ` + userMarket + ` && $3)`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM users u WHERE `+cond, r.From, r.To, markets(r)).Scan(&total); err != nil {
		return fmt.Errorf("export users: %w", err)
	}
	start(total)
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.full_name, u.email, u.status, u.email_verified_at IS NOT NULL,
		       COALESCE((SELECT array_agg(x.role ORDER BY x.role) FROM user_account_roles x WHERE x.user_id = u.id), '{}'),
		       COALESCE((SELECT array_agg(DISTINCT a.role) FROM platform_role_assignments a WHERE a.user_id = u.id), '{}'),
		       `+userMarket+`, u.created_at, (SELECT max(se.created_at) FROM sessions se WHERE se.user_id = u.id)
		FROM users u WHERE `+cond+` ORDER BY u.created_at DESC, u.id DESC LIMIT $4`, r.From, r.To, markets(r), limit)
	if err != nil {
		return fmt.Errorf("export users: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x UserRow
		if err := rows.Scan(&x.ID, &x.Name, &x.Email, &x.Status, &x.EmailVerified, &x.AccountRoles, &x.PlatformRoles, &x.MarketCodes,
			&x.CreatedAt, &x.LastSignInAt); err != nil {
			return err
		}
		if err := each(x); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExportListings implements Store.
func (s *PostgresStore) ExportListings(ctx context.Context, r Range, limit int, start func(int), each func(listings.ReportRow) error) error {
	cond := `l.created_at >= $1 AND l.created_at < $2 AND ($3::text[] IS NULL OR mk.code = ANY($3))`
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM listings l JOIN markets mk ON mk.id = l.market_id WHERE `+cond,
		r.From, r.To, markets(r)).Scan(&total); err != nil {
		return fmt.Errorf("export listings: %w", err)
	}
	start(total)
	rows, err := s.pool.Query(ctx, `
		SELECT l.id, COALESCE(l.reference, ''), l.title,
		       CASE WHEN l.status = 'PUBLISHED' AND l.expires_at <= $5 THEN 'EXPIRED' ELSE l.status::text END,
		       mk.code, COALESCE(l.deal, ''), COALESCE(l.category, ''), COALESCE(l.property_type, ''), COALESCE(l.price_amount::text, ''),
		       l.currency_code, l.organisation_id, l.agent_user_id, l.seller_user_id, l.created_at, l.published_at, l.expires_at
		FROM listings l JOIN markets mk ON mk.id = l.market_id
		WHERE `+cond+` ORDER BY l.created_at DESC, l.id DESC LIMIT $4`, r.From, r.To, markets(r), limit, s.now())
	if err != nil {
		return fmt.Errorf("export listings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var x listings.ReportRow
		if err := rows.Scan(&x.ID, &x.Reference, &x.Title, &x.Status, &x.MarketCode, &x.Deal, &x.Category, &x.PropertyType, &x.Price,
			&x.Currency, &x.OrganisationID, &x.AgentUserID, &x.SellerUserID, &x.CreatedAt, &x.PublishedAt, &x.ExpiresAt); err != nil {
			return err
		}
		if err := each(x); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ExportSubscriptions implements Store.
func (s *PostgresStore) ExportSubscriptions(ctx context.Context, r Range, limit int, start func(int), each func(subscriptions.Subscription) error) error {
	list, total, err := s.subs.Subscriptions(ctx, subscriptions.Filter{CreatedFrom: r.From, CreatedTo: r.To, Markets: markets(r), Limit: limit})
	if err != nil {
		return err
	}
	start(total)
	for _, x := range list {
		if err := each(x); err != nil {
			return err
		}
	}
	return nil
}
