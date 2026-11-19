package reviews

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// PostgresStore keeps reviews in provider_reviews and
// provider_review_reports (migration 0039).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres adapter.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const reviewColumns = `r.id, r.profile_id, r.provider_organisation_id, r.provider_kind, r.provider_slug, r.provider_name,
	r.reviewer_kind, r.reviewer_user_id, r.reviewer_organisation_id, r.reviewer_name, r.author_name,
	r.basis_kind, r.basis_id, r.basis_reference, r.rating, r.body, r.status,
	r.moderation_reason, r.moderated_at, r.moderated_by, r.moderated_by_name, r.published_at,
	r.reply_body, r.reply_at, r.reply_updated_at, r.reply_by, r.reply_by_name, r.reply_removed_at, r.reply_removed_reason,
	r.version, r.created_at, r.updated_at,
	(SELECT count(*) FROM provider_review_reports rp WHERE rp.review_id = r.id AND rp.status = 'OPEN')`

func scanReview(row pgx.CollectableRow) (Review, error) {
	var r Review
	var replyBody *string
	var replyAt, replyUpdated *time.Time
	var replyBy *uuid.UUID
	var replyByName string
	if err := row.Scan(&r.ID, &r.ProfileID, &r.ProviderOrganisationID, &r.ProviderKind, &r.ProviderSlug, &r.ProviderName,
		&r.ReviewerKind, &r.ReviewerUserID, &r.ReviewerOrganisationID, &r.ReviewerName, &r.AuthorName,
		&r.BasisKind, &r.BasisID, &r.BasisReference, &r.Rating, &r.Text, &r.Status,
		&r.ModerationReason, &r.ModeratedAt, &r.ModeratedBy, &r.ModeratedByName, &r.PublishedAt,
		&replyBody, &replyAt, &replyUpdated, &replyBy, &replyByName, &r.ReplyRemovedAt, &r.ReplyRemovedReason,
		&r.Version, &r.CreatedAt, &r.UpdatedAt, &r.OpenReports); err != nil {
		return Review{}, err
	}
	if replyBody != nil && replyAt != nil {
		reply := &Reply{Text: *replyBody, At: replyAt.UTC(), UpdatedAt: replyAt.UTC(), ByUserID: replyBy, ByName: replyByName}
		if replyUpdated != nil {
			reply.UpdatedAt = replyUpdated.UTC()
		}
		r.Reply = reply
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	r.ModeratedAt, r.PublishedAt, r.ReplyRemovedAt = utc(r.ModeratedAt), utc(r.PublishedAt), utc(r.ReplyRemovedAt)
	return r, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (s *PostgresStore) query(ctx context.Context, sql string, args ...any) ([]Review, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanReview)
}

// Review implements Store.
func (s *PostgresStore) Review(ctx context.Context, id uuid.UUID) (Review, error) {
	list, err := s.query(ctx, `SELECT `+reviewColumns+` FROM provider_reviews r WHERE r.id = $1`, id)
	if err != nil {
		return Review{}, err
	}
	if len(list) == 0 {
		return Review{}, ErrNotFound
	}
	return list[0], nil
}

// where builds the conditions of a Filter.
func (f Filter) where() (string, []any) {
	conds, args := []string{"true"}, []any{}
	add := func(format string, v any) {
		args = append(args, v)
		conds = append(conds, fmt.Sprintf(format, len(args)))
	}
	if f.ProfileID != nil {
		add(`r.profile_id = $%d`, *f.ProfileID)
	}
	if f.ProviderOrganisationID != nil {
		add(`r.provider_organisation_id = $%d`, *f.ProviderOrganisationID)
	}
	if f.ReviewerKind != "" {
		add(`r.reviewer_kind = $%d`, f.ReviewerKind)
	}
	if f.ReviewerUserID != nil {
		add(`r.reviewer_user_id = $%d`, *f.ReviewerUserID)
	}
	if f.ReviewerOrganisationID != nil {
		add(`r.reviewer_organisation_id = $%d`, *f.ReviewerOrganisationID)
	}
	if len(f.Statuses) > 0 {
		add(`r.status = ANY($%d::text[])`, f.Statuses)
	}
	openReport := `EXISTS (SELECT 1 FROM provider_review_reports rp WHERE rp.review_id = r.id AND rp.status = 'OPEN')`
	if f.Queue {
		conds = append(conds, `(r.status = 'PENDING' OR `+openReport+`)`)
	}
	if f.Reported {
		conds = append(conds, openReport)
	}
	if f.BasisKind != "" {
		add(`r.basis_kind = $%d`, f.BasisKind)
	}
	if f.Rating != 0 {
		add(`r.rating = $%d`, f.Rating)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		add(`(r.provider_name || ' ' || r.body) ILIKE $%d`, "%"+likeEscaper.Replace(q)+"%")
	}
	return strings.Join(conds, " AND "), args
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// Reviews implements Store.
func (s *PostgresStore) Reviews(ctx context.Context, f Filter) ([]Review, int, error) {
	where, args := f.where()
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM provider_reviews r WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count reviews: %w", err)
	}
	orderBy := ` ORDER BY r.created_at DESC, r.id DESC`
	switch f.Order {
	case "published":
		orderBy = ` ORDER BY COALESCE(r.published_at, r.created_at) DESC, r.id DESC`
	case "queue":
		orderBy = ` ORDER BY r.created_at, r.id`
	}
	tail := orderBy
	if f.Limit > 0 {
		args = append(args, f.Limit, max(f.Offset, 0))
		tail += fmt.Sprintf(` LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	}
	list, err := s.query(ctx, `SELECT `+reviewColumns+` FROM provider_reviews r WHERE `+where+tail, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list reviews: %w", err)
	}
	return list, total, nil
}

// ByBasis implements Store.
func (s *PostgresStore) ByBasis(ctx context.Context, kind string, ids []uuid.UUID) (map[uuid.UUID]Review, error) {
	out := map[uuid.UUID]Review{}
	if len(ids) == 0 {
		return out, nil
	}
	list, err := s.query(ctx, `SELECT `+reviewColumns+` FROM provider_reviews r WHERE r.basis_kind = $1 AND r.basis_id = ANY($2)`, kind, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		out[r.BasisID] = r
	}
	return out, nil
}

// Reports implements Store.
func (s *PostgresStore) Reports(ctx context.Context, reviewID uuid.UUID) ([]Report, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, review_id, reporter_user_id, reason, status, created_at, resolved_at, resolved_by, resolution
		FROM provider_review_reports WHERE review_id = $1 ORDER BY created_at DESC, id DESC`, reviewID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Report, error) {
		var rp Report
		err := row.Scan(&rp.ID, &rp.ReviewID, &rp.ReporterUserID, &rp.Reason, &rp.Status, &rp.CreatedAt, &rp.ResolvedAt, &rp.ResolvedBy,
			&rp.Resolution)
		rp.CreatedAt, rp.ResolvedAt = rp.CreatedAt.UTC(), utc(rp.ResolvedAt)
		return rp, err
	})
}

// Ratings implements Store.
func (s *PostgresStore) Ratings(ctx context.Context, profileIDs []uuid.UUID) (map[uuid.UUID]Rating, error) {
	out := map[uuid.UUID]Rating{}
	if len(profileIDs) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT profile_id, count(*), COALESCE(sum(rating), 0) FROM provider_reviews
		WHERE status = 'PUBLISHED' AND profile_id = ANY($1) GROUP BY profile_id`, profileIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var r Rating
		if err := rows.Scan(&id, &r.Count, &r.Sum); err != nil {
			return nil, err
		}
		out[id] = r
	}
	return out, rows.Err()
}

// QueueCounts implements Store.
func (s *PostgresStore) QueueCounts(ctx context.Context) (int, int, error) {
	var pending, reported int
	err := s.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM provider_reviews WHERE status = 'PENDING'),
		(SELECT count(DISTINCT review_id) FROM provider_review_reports WHERE status = 'OPEN')`).Scan(&pending, &reported)
	return pending, reported, err
}

// Apply implements Store.
func (s *PostgresStore) Apply(ctx context.Context, ch Change) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := ApplyTx(ctx, tx, ch); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func uniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == constraint
}

func replyColumns(r Review) (body *string, at, updated any, by *uuid.UUID, byName string) {
	if r.Reply == nil {
		return nil, nil, nil, nil, ""
	}
	text := r.Reply.Text
	return &text, r.Reply.At, r.Reply.UpdatedAt, r.Reply.ByUserID, r.Reply.ByName
}

// ApplyTx applies a Change inside the caller's transaction (the seed).
func ApplyTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	if c := ch.Create; c != nil {
		version := max(c.Version, 1)
		body, at, updated, by, byName := replyColumns(*c)
		_, err := tx.Exec(ctx, `
			INSERT INTO provider_reviews (id, profile_id, provider_organisation_id, provider_kind, provider_slug, provider_name,
				reviewer_kind, reviewer_user_id, reviewer_organisation_id, reviewer_name, author_name,
				basis_kind, basis_id, basis_reference, rating, body, status,
				moderation_reason, moderated_at, moderated_by, moderated_by_name, published_at,
				reply_body, reply_at, reply_updated_at, reply_by, reply_by_name, reply_removed_at, reply_removed_reason,
				version, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22,
				$23, $24, $25, $26, $27, $28, $29, $30, $31, $32)`,
			c.ID, c.ProfileID, c.ProviderOrganisationID, c.ProviderKind, c.ProviderSlug, c.ProviderName,
			c.ReviewerKind, c.ReviewerUserID, c.ReviewerOrganisationID, c.ReviewerName, c.AuthorName,
			c.BasisKind, c.BasisID, c.BasisReference, c.Rating, c.Text, c.Status,
			c.ModerationReason, c.ModeratedAt, c.ModeratedBy, c.ModeratedByName, c.PublishedAt,
			body, at, updated, by, byName, c.ReplyRemovedAt, c.ReplyRemovedReason,
			version, c.CreatedAt, c.UpdatedAt)
		if uniqueViolation(err, "provider_reviews_one_per_basis") {
			return ErrBasisReviewed
		}
		if err != nil {
			return fmt.Errorf("insert review: %w", err)
		}
	}
	if u := ch.Update; u != nil {
		body, at, updated, by, byName := replyColumns(*u)
		tag, err := tx.Exec(ctx, `
			UPDATE provider_reviews SET rating = $3, body = $4, status = $5, reviewer_user_id = $6, reviewer_name = $7, author_name = $8,
				moderation_reason = $9, moderated_at = $10, moderated_by = $11, moderated_by_name = $12, published_at = $13,
				reply_body = $14, reply_at = $15, reply_updated_at = $16, reply_by = $17, reply_by_name = $18,
				reply_removed_at = $19, reply_removed_reason = $20, updated_at = $21, version = version + 1
			WHERE id = $1 AND version = $2`,
			u.ID, ch.ExpectVersion, u.Rating, u.Text, u.Status, u.ReviewerUserID, u.ReviewerName, u.AuthorName,
			u.ModerationReason, u.ModeratedAt, u.ModeratedBy, u.ModeratedByName, u.PublishedAt,
			body, at, updated, by, byName, u.ReplyRemovedAt, u.ReplyRemovedReason, u.UpdatedAt)
		if err != nil {
			return fmt.Errorf("update review: %w", err)
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM provider_reviews WHERE id = $1)`, u.ID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return ErrNotFound
			}
			return ErrConflict
		}
	}
	if ch.Delete != nil {
		if _, err := tx.Exec(ctx, `DELETE FROM provider_reviews WHERE id = $1`, *ch.Delete); err != nil {
			return fmt.Errorf("delete review: %w", err)
		}
	}
	if res := ch.Resolve; res != nil {
		if _, err := tx.Exec(ctx, `UPDATE provider_review_reports SET status = 'RESOLVED', resolved_at = $2, resolved_by = $3, resolution = $4
			WHERE review_id = $1 AND status = 'OPEN'`, res.ReviewID, res.At, res.By, res.Resolution); err != nil {
			return fmt.Errorf("resolve review reports: %w", err)
		}
	}
	for _, rp := range ch.Reports {
		var status string
		err := tx.QueryRow(ctx, `SELECT status FROM provider_reviews WHERE id = $1 FOR UPDATE`, rp.ReviewID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != StatusPublished) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO provider_review_reports (id, review_id, reporter_user_id, reason, status, created_at, resolved_at, resolved_by, resolution)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			rp.ID, rp.ReviewID, rp.ReporterUserID, rp.Reason, rp.Status, rp.CreatedAt, rp.ResolvedAt, rp.ResolvedBy, rp.Resolution)
		if uniqueViolation(err, "provider_review_reports_one_open") {
			return ErrAlreadyReported
		}
		if err != nil {
			return fmt.Errorf("insert review report: %w", err)
		}
	}
	for _, e := range ch.Audit {
		if err := adminaudit.Insert(ctx, tx, e); err != nil {
			return err
		}
	}
	for _, n := range ch.Notices {
		payload, err := json.Marshal(n)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`,
			uuid.New(), OutboxModerated, AggregateReview, n.ReviewID, string(payload), n.OccurredAt); err != nil {
			return fmt.Errorf("insert outbox event: %w", err)
		}
	}
	return nil
}

// ForgetUserTx handles a deleted account's rows inside the account
// deletion's transaction (the users row is anonymised, so nothing
// cascades): as MemoryStore.ForgetUser, plus the enquiries the person
// marked as replied to.
func ForgetUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID, deletedName string) error {
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE provider_reviews SET reviewer_user_id = NULL, author_name = $2 WHERE reviewer_user_id = $1 AND reviewer_kind = 'ORGANISATION'`,
			[]any{userID, deletedName}},
		{`UPDATE provider_reviews SET reviewer_user_id = NULL, reviewer_name = $2, author_name = '' WHERE reviewer_user_id = $1`,
			[]any{userID, FormerCustomer}},
		{`UPDATE provider_reviews SET moderated_by = NULL WHERE moderated_by = $1`, []any{userID}},
		{`UPDATE provider_reviews SET reply_by = NULL WHERE reply_by = $1`, []any{userID}},
		{`UPDATE provider_review_reports SET reporter_user_id = NULL WHERE reporter_user_id = $1`, []any{userID}},
		{`UPDATE provider_review_reports SET resolved_by = NULL WHERE resolved_by = $1`, []any{userID}},
		{`UPDATE business_enquiries SET replied_by = NULL WHERE replied_by = $1`, []any{userID}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			return fmt.Errorf("forget the deleted account's review rows: %w", err)
		}
	}
	return nil
}
