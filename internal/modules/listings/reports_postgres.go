package listings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

const reportColumns = `r.id, r.listing_id, r.reason, COALESCE(r.note, ''), r.reporter_user_id, r.status, r.decided_by, COALESCE(r.decided_by_name, ''),
	r.decided_at, COALESCE(r.decision_note, ''), r.created_at, l.slug, l.title, COALESCE(l.reference, ''), l.status::text, m.code,
	(SELECT count(*) FROM listing_reports o WHERE o.listing_id = r.listing_id AND o.status = 'OPEN')`

const reportFrom = ` FROM listing_reports r JOIN listings l ON l.id = r.listing_id JOIN markets m ON m.id = l.market_id`

func scanReport(row pgx.Row) (ListingReport, error) {
	var r ListingReport
	err := row.Scan(&r.ID, &r.ListingID, &r.Reason, &r.Note, &r.ReporterUserID, &r.Status, &r.DecidedBy, &r.DecidedByName,
		&r.DecidedAt, &r.DecisionNote, &r.CreatedAt, &r.ListingSlug, &r.ListingTitle, &r.ListingReference, &r.ListingStatus, &r.MarketCode, &r.OpenReports)
	r.CreatedAt = r.CreatedAt.UTC()
	if r.DecidedAt != nil {
		t := r.DecidedAt.UTC()
		r.DecidedAt = &t
	}
	return r, err
}

// CreateReport implements ReportStore: a second OPEN report of the same
// signed-in reporter hits the partial unique index and writes nothing.
func (s *PostgresStore) CreateReport(ctx context.Context, r ListingReport) (bool, error) {
	var note *string
	if r.Note != "" {
		note = &r.Note
	}
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO listing_reports (id, listing_id, reason, note, reporter_user_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, 'OPEN', $6)
		ON CONFLICT (listing_id, reporter_user_id) WHERE status = 'OPEN' AND reporter_user_id IS NOT NULL DO NOTHING`,
		r.ID, r.ListingID, r.Reason, note, r.ReporterUserID, r.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return false, nil
		}
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ListReports implements ReportStore.
func (s *PostgresStore) ListReports(ctx context.Context, f ReportFilter) ([]ListingReport, int, error) {
	where, args := []string{"true"}, []any{}
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if f.Status != "" {
		add("r.status = $%d", f.Status)
	}
	if f.Markets != nil {
		add("m.code = ANY($%d)", f.Markets)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*)`+reportFrom+` WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	size := max(f.PageSize, 1)
	args = append(args, size, max(f.Page-1, 0)*size)
	rows, err := s.pool.Query(ctx, `SELECT `+reportColumns+reportFrom+` WHERE `+cond+
		fmt.Sprintf(` ORDER BY r.created_at DESC, r.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ListingReport, error) { return scanReport(row) })
	if out == nil {
		out = []ListingReport{}
	}
	return out, total, err
}

// GetReport implements ReportStore.
func (s *PostgresStore) GetReport(ctx context.Context, id uuid.UUID) (ListingReport, error) {
	r, err := scanReport(s.pool.QueryRow(ctx, `SELECT `+reportColumns+reportFrom+` WHERE r.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ListingReport{}, ErrReportNotFound
	}
	return r, err
}

// DecideReports implements ReportStore.
func (s *PostgresStore) DecideReports(ctx context.Context, id uuid.UUID, allOpen bool, status string, by uuid.UUID, byName, note string, audit adminaudit.Event, now time.Time) (ListingReport, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return ListingReport{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var listing uuid.UUID
	var cur string
	err = tx.QueryRow(ctx, `SELECT listing_id, status FROM listing_reports WHERE id = $1 FOR UPDATE`, id).Scan(&listing, &cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return ListingReport{}, ErrReportNotFound
	}
	if err != nil {
		return ListingReport{}, err
	}
	if cur != ReportOpen {
		return ListingReport{}, ErrReportDecided
	}
	if _, err := tx.Exec(ctx, `
		UPDATE listing_reports SET status = $3, decided_by = $4, decided_by_name = $5, decided_at = $6, decision_note = $7
		WHERE id = $1 OR ($2 AND listing_id = $8 AND status = 'OPEN')`,
		id, allOpen, status, by, byName, now, note, listing); err != nil {
		return ListingReport{}, err
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return ListingReport{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ListingReport{}, err
	}
	return s.GetReport(ctx, id)
}
