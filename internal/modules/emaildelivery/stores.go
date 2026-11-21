package emaildelivery

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/mail"
)

func isProblem(state string) bool { return state == StateBounced || state == StateComplained }

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

// MemoryStore keeps everything in memory; the messages are read from the
// memory delivery log.
type MemoryStore struct {
	mu           sync.Mutex
	messages     func() []mail.Record
	events       map[string]Event
	states       map[string]Event // provider message ID -> its latest state event
	suppressions map[string]Suppression
	alerts       []Alert
	audit        *adminaudit.MemoryLog
}

// NewMemoryStore builds the store. messages lists the email log (nil: none).
func NewMemoryStore(messages func() []mail.Record, audit *adminaudit.MemoryLog) *MemoryStore {
	if messages == nil {
		messages = func() []mail.Record { return nil }
	}
	return &MemoryStore{messages: messages, events: map[string]Event{}, states: map[string]Event{}, suppressions: map[string]Suppression{}, audit: audit}
}

func (s *MemoryStore) message(providerID string) *mail.Record {
	if providerID == "" {
		return nil
	}
	for _, r := range s.messages() {
		if r.ProviderMessageID == providerID {
			r := r
			return &r
		}
	}
	return nil
}

// Apply implements Store.
func (s *MemoryStore) Apply(_ context.Context, e Event) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, seen := s.events[e.WebhookID]; seen {
		return false, nil
	}
	if m := s.message(e.ProviderMessageID); m != nil {
		e.RecipientSHA256 = m.RecipientSHA256
	}
	s.events[e.WebhookID] = e
	if e.ProviderMessageID != "" {
		// A bounce or a complaint is not undone by a later "delivered".
		if cur, ok := s.states[e.ProviderMessageID]; !ok || !isProblem(cur.Type) || isProblem(e.Type) {
			s.states[e.ProviderMessageID] = e
		}
	}
	if e.Hard && len(e.RecipientSHA256) == 64 {
		reason := ReasonHardBounce
		if e.Type == StateComplained {
			reason = ReasonComplaint
		}
		cur, ok := s.suppressions[e.RecipientSHA256]
		if !ok {
			cur.CreatedAt = e.ReceivedAt
		}
		cur.RecipientSHA256, cur.Reason, cur.ProviderMessageID, cur.UpdatedAt = e.RecipientSHA256, reason, e.ProviderMessageID, e.ReceivedAt
		s.suppressions[e.RecipientSHA256] = cur
	}
	return true, nil
}

// IsSuppressed implements Store.
func (s *MemoryStore) IsSuppressed(_ context.Context, hash string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.suppressions[hash]
	return ok, nil
}

// Suppressions implements Store.
func (s *MemoryStore) Suppressions(_ context.Context, hash string, limit int) ([]Suppression, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Suppression{}
	for h, v := range s.suppressions {
		if hash == "" || h == hash {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Clear implements Store.
func (s *MemoryStore) Clear(_ context.Context, hash string, _ uuid.UUID, _ time.Time, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.suppressions[hash]; !ok {
		return ErrNotFound
	}
	delete(s.suppressions, hash)
	if s.audit != nil {
		s.audit.Append(audit)
	}
	return nil
}

// Counts implements Store.
func (s *MemoryStore) Counts(_ context.Context, since time.Time) (Counts, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Counts
	for _, r := range s.messages() {
		if r.UpdatedAt.Before(since) {
			continue
		}
		switch {
		case r.Status == mail.StatusProviderAccepted:
			c.Accepted++
		case r.Status == mail.StatusFailed && r.LastError == mail.ReasonSuppressed:
			c.Suppressed++
		case r.Status == mail.StatusFailed:
			c.Failed++
		}
	}
	for _, e := range s.states {
		if e.ReceivedAt.Before(since) {
			continue
		}
		switch e.Type {
		case StateDelivered:
			c.Delivered++
		case StateDelayed:
			c.Delayed++
		case StateBounced:
			c.Bounced++
		case StateComplained:
			c.Complained++
		}
	}
	return c, nil
}

// Problems implements Store.
func (s *MemoryStore) Problems(_ context.Context, limit int) ([]Problem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Problem{}
	for _, r := range s.messages() {
		st := s.states[r.ProviderMessageID]
		if r.Status != mail.StatusFailed && !(r.ProviderMessageID != "" && isProblem(st.Type)) {
			continue
		}
		p := Problem{Kind: r.Kind, Status: r.Status, LastError: r.LastError, RecipientSHA256: r.RecipientSHA256, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
		if r.ProviderMessageID != "" {
			p.DeliveryState, p.BounceType = st.Type, st.BounceType
			if !st.ReceivedAt.IsZero() {
				p.UpdatedAt = st.ReceivedAt
			}
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Window implements Store.
func (s *MemoryStore) Window(_ context.Context, from, to time.Time) (int, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b, c, a int
	for _, e := range s.events {
		if e.ReceivedAt.Before(from) || e.ReceivedAt.After(to) {
			continue
		}
		switch e.Type {
		case StateBounced:
			b++
		case StateComplained:
			c++
		}
	}
	for _, r := range s.messages() {
		if r.Status == mail.StatusProviderAccepted && !r.UpdatedAt.Before(from) && !r.UpdatedAt.After(to) {
			a++
		}
	}
	return b, c, a, nil
}

// AddAlert implements Store.
func (s *MemoryStore) AddAlert(_ context.Context, a Alert) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.alerts {
		if x.Kind == a.Kind && !x.CreatedAt.Before(a.WindowStart) {
			return false, nil
		}
	}
	s.alerts = append(s.alerts, a)
	return true, nil
}

// Alerts implements Store.
func (s *MemoryStore) Alerts(_ context.Context, since time.Time, limit int) ([]Alert, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Alert{}
	for i := len(s.alerts) - 1; i >= 0 && len(out) < limit; i-- {
		if !s.alerts[i].CreatedAt.Before(since) {
			out = append(out, s.alerts[i])
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// PostgresStore uses email_messages and the 0061 tables.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore builds the store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Apply implements Store.
func (s *PostgresStore) Apply(ctx context.Context, e Event) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if e.ProviderMessageID != "" {
		var hash string
		err := tx.QueryRow(ctx, `SELECT recipient_sha256 FROM email_messages WHERE provider_message_id = $1 ORDER BY created_at DESC LIMIT 1`,
			e.ProviderMessageID).Scan(&hash)
		if err == nil {
			e.RecipientSHA256 = hash
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return false, err
		}
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO email_delivery_events (webhook_id, event_type, provider_message_id, recipient_sha256, bounce_type, hard, occurred_at, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (webhook_id) DO NOTHING`,
		e.WebhookID, e.Type, nullable(e.ProviderMessageID), nullable(e.RecipientSHA256), nullable(e.BounceType), e.Hard, e.OccurredAt, e.ReceivedAt)
	if err != nil {
		return false, fmt.Errorf("insert delivery event: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if e.ProviderMessageID != "" {
		// A bounce or a complaint is not undone by a later "delivered".
		if _, err := tx.Exec(ctx, `
			UPDATE email_messages SET delivery_state = $2, delivery_state_at = $3, bounce_type = COALESCE($4, bounce_type)
			WHERE provider_message_id = $1 AND (delivery_state IS NULL OR delivery_state NOT IN ('BOUNCED', 'COMPLAINED') OR $2 IN ('BOUNCED', 'COMPLAINED'))`,
			e.ProviderMessageID, e.Type, e.OccurredAt, nullable(e.BounceType)); err != nil {
			return false, fmt.Errorf("set delivery state: %w", err)
		}
	}
	if e.Hard && len(e.RecipientSHA256) == 64 {
		reason := ReasonHardBounce
		if e.Type == StateComplained {
			reason = ReasonComplaint
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO email_suppressions (recipient_sha256, reason, provider_message_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $4)
			ON CONFLICT (recipient_sha256) DO UPDATE SET reason = EXCLUDED.reason, provider_message_id = EXCLUDED.provider_message_id,
				updated_at = EXCLUDED.updated_at, cleared_at = NULL, cleared_by = NULL,
				created_at = CASE WHEN email_suppressions.cleared_at IS NULL THEN email_suppressions.created_at ELSE EXCLUDED.created_at END`,
			e.RecipientSHA256, reason, nullable(e.ProviderMessageID), e.ReceivedAt); err != nil {
			return false, fmt.Errorf("suppress: %w", err)
		}
	}
	return true, tx.Commit(ctx)
}

// IsSuppressed implements Store.
func (s *PostgresStore) IsSuppressed(ctx context.Context, hash string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM email_suppressions WHERE recipient_sha256 = $1 AND cleared_at IS NULL)`, hash).Scan(&ok)
	return ok, err
}

// Suppressions implements Store.
func (s *PostgresStore) Suppressions(ctx context.Context, hash string, limit int) ([]Suppression, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT recipient_sha256, reason, COALESCE(provider_message_id, ''), created_at, updated_at
		FROM email_suppressions WHERE cleared_at IS NULL AND ($1 = '' OR recipient_sha256 = $1)
		ORDER BY updated_at DESC LIMIT $2`, hash, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suppression{}
	for rows.Next() {
		var v Suppression
		if err := rows.Scan(&v.RecipientSHA256, &v.Reason, &v.ProviderMessageID, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Clear implements Store.
func (s *PostgresStore) Clear(ctx context.Context, hash string, by uuid.UUID, at time.Time, audit adminaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var clearedBy *uuid.UUID
	if by != uuid.Nil {
		clearedBy = &by
	}
	tag, err := tx.Exec(ctx, `UPDATE email_suppressions SET cleared_at = $2, cleared_by = $3 WHERE recipient_sha256 = $1 AND cleared_at IS NULL`, hash, at, clearedBy)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Counts implements Store.
func (s *PostgresStore) Counts(ctx context.Context, since time.Time) (Counts, error) {
	var c Counts
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'PROVIDER_ACCEPTED'),
		       count(*) FILTER (WHERE delivery_state = 'DELIVERED' AND delivery_state_at >= $1),
		       count(*) FILTER (WHERE delivery_state = 'DELAYED' AND delivery_state_at >= $1),
		       count(*) FILTER (WHERE delivery_state = 'BOUNCED' AND delivery_state_at >= $1),
		       count(*) FILTER (WHERE delivery_state = 'COMPLAINED' AND delivery_state_at >= $1),
		       count(*) FILTER (WHERE status = 'FAILED' AND last_error = 'suppressed'),
		       count(*) FILTER (WHERE status = 'FAILED' AND last_error IS DISTINCT FROM 'suppressed')
		FROM email_messages WHERE updated_at >= $1 OR delivery_state_at >= $1`, since).Scan(
		&c.Accepted, &c.Delivered, &c.Delayed, &c.Bounced, &c.Complained, &c.Suppressed, &c.Failed)
	return c, err
}

// Problems implements Store.
func (s *PostgresStore) Problems(ctx context.Context, limit int) ([]Problem, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT kind, status, COALESCE(delivery_state, ''), COALESCE(bounce_type, ''), COALESCE(last_error, ''), recipient_sha256,
		       created_at, GREATEST(updated_at, COALESCE(delivery_state_at, updated_at)) AS at
		FROM email_messages WHERE status = 'FAILED' OR delivery_state IN ('BOUNCED', 'COMPLAINED')
		ORDER BY at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Problem{}
	for rows.Next() {
		var p Problem
		if err := rows.Scan(&p.Kind, &p.Status, &p.DeliveryState, &p.BounceType, &p.LastError, &p.RecipientSHA256, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Window implements Store.
func (s *PostgresStore) Window(ctx context.Context, from, to time.Time) (int, int, int, error) {
	var b, c, a int
	err := s.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM email_delivery_events WHERE event_type = 'BOUNCED' AND received_at >= $1 AND received_at <= $2),
		       (SELECT count(*) FROM email_delivery_events WHERE event_type = 'COMPLAINED' AND received_at >= $1 AND received_at <= $2),
		       (SELECT count(*) FROM email_messages WHERE status = 'PROVIDER_ACCEPTED' AND accepted_at >= $1 AND accepted_at <= $2)`,
		from, to).Scan(&b, &c, &a)
	return b, c, a, err
}

// AddAlert implements Store. The advisory lock makes two sweeps agree.
func (s *PostgresStore) AddAlert(ctx context.Context, a Alert) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('email_delivery_alerts'))`); err != nil {
		return false, err
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM email_delivery_alerts WHERE kind = $1 AND created_at >= $2)`, a.Kind, a.WindowStart).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO email_delivery_alerts (id, kind, window_start, window_end, events, accepted, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, a.ID, a.Kind, a.WindowStart, a.WindowEnd, a.Events, a.Accepted, a.CreatedAt); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Alerts implements Store.
func (s *PostgresStore) Alerts(ctx context.Context, since time.Time, limit int) ([]Alert, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, kind, window_start, window_end, events, accepted, created_at
		FROM email_delivery_alerts WHERE created_at >= $1 ORDER BY created_at DESC LIMIT $2`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alert{}
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.Kind, &a.WindowStart, &a.WindowEnd, &a.Events, &a.Accepted, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
