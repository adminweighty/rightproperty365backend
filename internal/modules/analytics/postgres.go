package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production Store (migration 0029).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func nullText(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// Insert implements Store. A detail view with a session takes a
// transaction-scoped advisory lock on (session, listing) so that two
// concurrent requests cannot both pass the SPA check.
func (s *PostgresStore) Insert(ctx context.Context, ev Event, spaWindow time.Duration) (InsertResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM analytics_events WHERE event_id = $1)`, ev.ID).Scan(&exists); err != nil {
		return 0, err
	}
	if exists {
		return Duplicate, nil
	}
	if ev.Type == TypeDetailView && !ev.Bot && ev.Session != "" {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 29))`, ev.Session+"|"+ev.ListingID.String()); err != nil {
			return 0, err
		}
		var seen bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM analytics_events
			WHERE event_type = 'listing_detail_view' AND NOT bot AND session_id = $1 AND listing_id = $2
			  AND occurred_at > $3 AND occurred_at < $4)`,
			ev.Session, ev.ListingID, ev.OccurredAt.Add(-spaWindow), ev.OccurredAt.Add(spaWindow)).Scan(&seen); err != nil {
			return 0, err
		}
		if seen {
			return SPADuplicate, nil
		}
	}
	var network *string
	if ev.Network != "" {
		network = &ev.Network
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO analytics_events (event_id, event_type, origin, occurred_at, received_at, local_day, listing_id, market_code,
			placement, rank, source, device, session_id, network, bot, bot_reason, utm_source, utm_medium, utm_campaign)
		VALUES ($1, $2, $3, $4, $5, $6::date, $7, $8, $9, $10, $11, $12, $13, $14::cidr, $15, $16, $17, $18, $19)
		ON CONFLICT (event_id) DO NOTHING`,
		ev.ID, ev.Type, ev.Origin, ev.OccurredAt, ev.ReceivedAt, ev.LocalDay, ev.ListingID, ev.MarketCode,
		nullText(ev.Placement), ev.Rank, nullText(ev.Source), nullText(ev.Device), nullText(ev.Session), network, ev.Bot, nullText(ev.BotReason),
		nullText(ev.Campaign.Source), nullText(ev.Campaign.Medium), nullText(ev.Campaign.Campaign))
	if err != nil {
		return 0, fmt.Errorf("insert analytics event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Duplicate, nil
	}
	return Inserted, nil
}

// RecordLate implements Store.
func (s *PostgresStore) RecordLate(ctx context.Context, l LateEvent) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO analytics_late_events (event_id, event_type, listing_id, market_code, occurred_at, received_at, local_day)
		VALUES ($1, $2, $3, $4, $5, $6, $7::date)
		ON CONFLICT (event_id) DO NOTHING`,
		l.EventID, l.Type, l.ListingID, l.MarketCode, l.OccurredAt, l.ReceivedAt, l.LocalDay)
	return err
}

// where builds the condition of a filter; timeColumn is occurred_at for raw
// events and received_at for late ones.
func where(f AggregateFilter, timeColumn string) (string, []any) {
	conds, args := []string{"true"}, []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "$?", fmt.Sprintf("$%d", len(args))))
	}
	if f.MarketCodes != nil {
		add("market_code = ANY($?::text[])", f.MarketCodes)
	}
	if f.ListingIDs != nil {
		add("listing_id = ANY($?::uuid[])", idStrings(f.ListingIDs))
	}
	if f.FromDay != "" {
		add("local_day >= $?::date", f.FromDay)
	}
	if !f.Since.IsZero() {
		add(timeColumn+" >= $?", f.Since)
	}
	return strings.Join(conds, " AND "), args
}

// Aggregate implements Store: the metric contract in SQL (the memory
// store's aggregateEvents computes the same).
func (s *PostgresStore) Aggregate(ctx context.Context, f AggregateFilter) ([]DayRow, error) {
	if (f.MarketCodes != nil && len(f.MarketCodes) == 0) || (f.ListingIDs != nil && len(f.ListingIDs) == 0) {
		return []DayRow{}, nil
	}
	rows := map[rollupKey]*DayRow{}
	row := func(listing uuid.UUID, day, market string) *DayRow {
		k := rollupKey{listing, day}
		r := rows[k]
		if r == nil {
			r = &DayRow{ListingID: listing, Day: day, MarketCode: market, Sources: map[string]int{}, Devices: map[string]int{}}
			rows[k] = r
		}
		return r
	}

	cond, args := where(f, "occurred_at")
	const valid = `NOT bot AND event_type = `
	q := `
		SELECT listing_id, local_day::text, min(market_code),
			count(*) FILTER (WHERE ` + valid + `'listing_detail_view'),
			count(DISTINCT session_id) FILTER (WHERE ` + valid + `'listing_detail_view')
				+ count(*) FILTER (WHERE ` + valid + `'listing_detail_view' AND session_id IS NULL),
			count(*) FILTER (WHERE ` + valid + `'property_card_impression'),
			count(*) FILTER (WHERE ` + valid + `'contact_click'),
			count(*) FILTER (WHERE ` + valid + `'youtube_play_intent'),
			count(*) FILTER (WHERE ` + valid + `'listing_save'),
			count(*) FILTER (WHERE ` + valid + `'enquiry_submit'),
			count(*) FILTER (WHERE ` + valid + `'viewing_request'),
			count(*) FILTER (WHERE ` + valid + `'appointment_confirm'),
			count(*) FILTER (WHERE ` + valid + `'listing_relist'),
			count(*) FILTER (WHERE bot)
		FROM analytics_events WHERE ` + cond + `
		GROUP BY listing_id, local_day`
	res, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	for res.Next() {
		var id uuid.UUID
		var day, market string
		var t Totals
		var filtered int
		if err := res.Scan(&id, &day, &market, &t.Views, &t.UniqueViews, &t.Impressions, &t.ContactClicks, &t.VideoPlays, &t.Saves,
			&t.Enquiries, &t.ViewingRequests, &t.AppointmentsConfirmed, &t.Relists, &filtered); err != nil {
			res.Close()
			return nil, err
		}
		r := row(id, day, market)
		r.Totals, r.FilteredBotEvents = t, filtered
	}
	res.Close()
	if err := res.Err(); err != nil {
		return nil, err
	}

	// Source and device breakdowns of the valid views.
	res, err = s.pool.Query(ctx, `
		SELECT listing_id, local_day::text, min(market_code), 'source', COALESCE(source, 'unknown'), count(*)
		FROM analytics_events WHERE `+cond+` AND NOT bot AND event_type = 'listing_detail_view'
		GROUP BY listing_id, local_day, COALESCE(source, 'unknown')
		UNION ALL
		SELECT listing_id, local_day::text, min(market_code), 'device', COALESCE(device, 'unknown'), count(*)
		FROM analytics_events WHERE `+cond+` AND NOT bot AND event_type = 'listing_detail_view'
		GROUP BY listing_id, local_day, COALESCE(device, 'unknown')`, args...)
	if err != nil {
		return nil, err
	}
	for res.Next() {
		var id uuid.UUID
		var day, market, dim, value string
		var n int
		if err := res.Scan(&id, &day, &market, &dim, &value, &n); err != nil {
			res.Close()
			return nil, err
		}
		r := row(id, day, market)
		if dim == "source" {
			r.Sources[value] = n
		} else {
			r.Devices[value] = n
		}
	}
	res.Close()
	if err := res.Err(); err != nil {
		return nil, err
	}

	lateCond, lateArgs := where(f, "received_at")
	res, err = s.pool.Query(ctx, `
		SELECT listing_id, local_day::text, min(market_code), count(*)
		FROM analytics_late_events WHERE `+lateCond+`
		GROUP BY listing_id, local_day`, lateArgs...)
	if err != nil {
		return nil, err
	}
	for res.Next() {
		var id uuid.UUID
		var day, market string
		var n int
		if err := res.Scan(&id, &day, &market, &n); err != nil {
			res.Close()
			return nil, err
		}
		row(id, day, market).LateEventsDropped = n
	}
	res.Close()
	if err := res.Err(); err != nil {
		return nil, err
	}

	out := make([]DayRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, *r)
	}
	sortRows(out)
	return out, nil
}

// ActiveMarkets implements Store.
func (s *PostgresStore) ActiveMarkets(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT market_code FROM analytics_events WHERE occurred_at >= $1
		UNION
		SELECT DISTINCT market_code FROM analytics_late_events WHERE received_at >= $1
		ORDER BY 1`, since)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// UpsertRollups implements Store: the rows and the run time in one
// transaction.
func (s *PostgresStore) UpsertRollups(ctx context.Context, rows []DayRow, refreshedAt time.Time) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	batch := &pgx.Batch{}
	for _, r := range rows {
		sources, err := json.Marshal(nonNil(r.Sources))
		if err != nil {
			return err
		}
		devices, err := json.Marshal(nonNil(r.Devices))
		if err != nil {
			return err
		}
		batch.Queue(`
			INSERT INTO analytics_listing_daily (listing_id, day, market_code, views, unique_views, impressions, contact_clicks,
				video_plays, saves, enquiries, viewing_requests, appointments_confirmed, relists, filtered_bot_events,
				late_events_dropped, sources, devices, refreshed_at)
			VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb, $17::jsonb, $18)
			ON CONFLICT (listing_id, day) DO UPDATE SET
				market_code = EXCLUDED.market_code, views = EXCLUDED.views, unique_views = EXCLUDED.unique_views,
				impressions = EXCLUDED.impressions, contact_clicks = EXCLUDED.contact_clicks, video_plays = EXCLUDED.video_plays,
				saves = EXCLUDED.saves, enquiries = EXCLUDED.enquiries, viewing_requests = EXCLUDED.viewing_requests,
				appointments_confirmed = EXCLUDED.appointments_confirmed, relists = EXCLUDED.relists,
				filtered_bot_events = EXCLUDED.filtered_bot_events, late_events_dropped = EXCLUDED.late_events_dropped,
				sources = EXCLUDED.sources, devices = EXCLUDED.devices, refreshed_at = EXCLUDED.refreshed_at`,
			r.ListingID, r.Day, r.MarketCode, r.Views, r.UniqueViews, r.Impressions, r.ContactClicks, r.VideoPlays, r.Saves,
			r.Enquiries, r.ViewingRequests, r.AppointmentsConfirmed, r.Relists, r.FilteredBotEvents, r.LateEventsDropped,
			string(sources), string(devices), refreshedAt)
	}
	batch.Queue(`
		INSERT INTO analytics_rollup_runs (id, refreshed_at) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET refreshed_at = EXCLUDED.refreshed_at`, refreshedAt)
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("upsert rollups: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func nonNil(m map[string]int) map[string]int {
	if m == nil {
		return map[string]int{}
	}
	return m
}

// Rollups implements Store.
func (s *PostgresStore) Rollups(ctx context.Context, f RollupFilter) ([]DayRow, error) {
	if (f.MarketCodes != nil && len(f.MarketCodes) == 0) || (f.ListingIDs != nil && len(f.ListingIDs) == 0) {
		return []DayRow{}, nil
	}
	conds, args := []string{"true"}, []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.ReplaceAll(cond, "$?", fmt.Sprintf("$%d", len(args))))
	}
	if f.MarketCodes != nil {
		add("market_code = ANY($?::text[])", f.MarketCodes)
	}
	if f.ListingIDs != nil {
		add("listing_id = ANY($?::uuid[])", idStrings(f.ListingIDs))
	}
	if f.FromDay != "" {
		add("day >= $?::date", f.FromDay)
	}
	if f.ToDay != "" {
		add("day <= $?::date", f.ToDay)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT listing_id, day::text, market_code, views, unique_views, impressions, contact_clicks, video_plays, saves,
			enquiries, viewing_requests, appointments_confirmed, relists, filtered_bot_events, late_events_dropped,
			sources::text, devices::text, refreshed_at
		FROM analytics_listing_daily WHERE `+strings.Join(conds, " AND ")+`
		ORDER BY day, listing_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayRow{}
	for rows.Next() {
		var r DayRow
		var sources, devices string
		if err := rows.Scan(&r.ListingID, &r.Day, &r.MarketCode, &r.Views, &r.UniqueViews, &r.Impressions, &r.ContactClicks,
			&r.VideoPlays, &r.Saves, &r.Enquiries, &r.ViewingRequests, &r.AppointmentsConfirmed, &r.Relists,
			&r.FilteredBotEvents, &r.LateEventsDropped, &sources, &devices, &r.RefreshedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(sources), &r.Sources); err != nil {
			return nil, fmt.Errorf("decode rollup sources: %w", err)
		}
		if err := json.Unmarshal([]byte(devices), &r.Devices); err != nil {
			return nil, fmt.Errorf("decode rollup devices: %w", err)
		}
		r.RefreshedAt = r.RefreshedAt.UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortRows(out)
	return out, nil
}

// LastRefresh implements Store.
func (s *PostgresStore) LastRefresh(ctx context.Context) (*time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `SELECT refreshed_at FROM analytics_rollup_runs WHERE id = 1`).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	at = at.UTC()
	return &at, nil
}

// DeleteBefore implements Store.
func (s *PostgresStore) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, int64, error) {
	events, err := s.pool.Exec(ctx, `DELETE FROM analytics_events WHERE occurred_at < $1`, cutoff)
	if err != nil {
		return 0, 0, err
	}
	late, err := s.pool.Exec(ctx, `DELETE FROM analytics_late_events WHERE received_at < $1`, cutoff)
	if err != nil {
		return events.RowsAffected(), 0, err
	}
	return events.RowsAffected(), late.RowsAffected(), nil
}
