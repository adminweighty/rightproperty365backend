package agreements

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the production Store (migration 0023). Every change is
// one short transaction.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func jsonOrNil(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	s := string(raw)
	return &s, nil
}

const agreementColumns = `id, reference, name, message, status, source, lease_id, sender_id, sender_name, sender_email, organisation_id,
	organisation_name, expires_at, reminder_frequency, original_object_key, original_sha256, original_size, original_file_name, pages,
	final_object_key, final_sha256, final_size, final_file_name, cancel_reason, created_at, updated_at, sent_at, completed_at, declined_at,
	cancelled_at, expired_at`

func scanAgreement(row pgx.Row) (Agreement, error) {
	var a Agreement
	var sender *uuid.UUID
	var oKey, oSHA, oName, fKey, fSHA, fName *string
	var oSize, fSize *int64
	var pages []byte
	err := row.Scan(&a.ID, &a.Reference, &a.Name, &a.Message, &a.Status, &a.Source, &a.LeaseID, &sender, &a.SenderName, &a.SenderEmail,
		&a.OrganisationID, &a.OrganisationName, &a.ExpiresAt, &a.ReminderFrequency, &oKey, &oSHA, &oSize, &oName, &pages,
		&fKey, &fSHA, &fSize, &fName, &a.CancelReason, &a.CreatedAt, &a.UpdatedAt, &a.SentAt, &a.CompletedAt, &a.DeclinedAt,
		&a.CancelledAt, &a.ExpiredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Agreement{}, ErrNotFound
	}
	if err != nil {
		return Agreement{}, err
	}
	if sender != nil {
		a.SenderID = *sender
	}
	if oKey != nil && oSHA != nil {
		a.Original = &Document{ObjectKey: *oKey, SHA256: *oSHA, Size: deref(oSize), FileName: derefS(oName)}
	}
	if fKey != nil && fSHA != nil {
		a.Final = &Document{ObjectKey: *fKey, SHA256: *fSHA, Size: deref(fSize), FileName: derefS(fName)}
	}
	if err := json.Unmarshal(pages, &a.Pages); err != nil {
		return Agreement{}, fmt.Errorf("read pages: %w", err)
	}
	a.ExpiresAt, a.CreatedAt, a.UpdatedAt = a.ExpiresAt.UTC(), a.CreatedAt.UTC(), a.UpdatedAt.UTC()
	a.SentAt, a.CompletedAt, a.DeclinedAt, a.CancelledAt, a.ExpiredAt = utc(a.SentAt), utc(a.CompletedAt), utc(a.DeclinedAt), utc(a.CancelledAt), utc(a.ExpiredAt)
	return a, nil
}

func deref(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func derefS(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func readStamp(raw []byte) (*Stamp, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s Stamp
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// stampJSON is a stamp for a jsonb column (NULL for nil).
func stampJSON(s *Stamp) (*string, error) {
	if s == nil {
		return nil, nil
	}
	return jsonOrNil(s)
}

func readAdopted(raw []byte) (*Adopted, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var a Adopted
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// children loads the recipients, fields and links of the agreements.
func children(ctx context.Context, q querier, list []Agreement) error {
	if len(list) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, 0, len(list))
	index := map[uuid.UUID]int{}
	for i, a := range list {
		ids = append(ids, a.ID)
		index[a.ID] = i
	}
	rows, err := q.Query(ctx, `SELECT agreement_id, id, signing_order, role, name, email, label, status, activated_at, first_viewed_at,
			completed_at, declined_at, decline_reason, code_hash, code_link_id, code_expires_at, code_attempts, code_sent_at, code_send_count,
			ip, user_agent, verification, signature, initials, stamp
		FROM agreement_recipients WHERE agreement_id = ANY($1) ORDER BY signing_order, name, id`, ids)
	if err != nil {
		return fmt.Errorf("load recipients: %w", err)
	}
	for rows.Next() {
		var aid uuid.UUID
		var r Recipient
		var sig, ini, stamp []byte
		if err := rows.Scan(&aid, &r.ID, &r.Order, &r.Role, &r.Name, &r.Email, &r.Label, &r.Status, &r.ActivatedAt, &r.FirstViewedAt,
			&r.CompletedAt, &r.DeclinedAt, &r.DeclineReason, &r.CodeHash, &r.CodeLinkID, &r.CodeExpiresAt, &r.CodeAttempts, &r.CodeSentAt,
			&r.CodeSendCount, &r.IP, &r.UserAgent, &r.Verification, &sig, &ini, &stamp); err != nil {
			rows.Close()
			return fmt.Errorf("load recipients: %w", err)
		}
		if r.Signature, err = readAdopted(sig); err == nil {
			if r.Initials, err = readAdopted(ini); err == nil {
				r.Stamp, err = readStamp(stamp)
			}
		}
		if err != nil {
			rows.Close()
			return fmt.Errorf("read adopted signature: %w", err)
		}
		r.ActivatedAt, r.FirstViewedAt, r.CompletedAt, r.DeclinedAt = utc(r.ActivatedAt), utc(r.FirstViewedAt), utc(r.CompletedAt), utc(r.DeclinedAt)
		r.CodeExpiresAt, r.CodeSentAt = utc(r.CodeExpiresAt), utc(r.CodeSentAt)
		list[index[aid]].Recipients = append(list[index[aid]].Recipients, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT agreement_id, id, recipient_id, type, page, x, y, w, h, required, label, value
		FROM agreement_fields WHERE agreement_id = ANY($1) ORDER BY position, id`, ids)
	if err != nil {
		return fmt.Errorf("load fields: %w", err)
	}
	for rows.Next() {
		var aid uuid.UUID
		var f Field
		if err := rows.Scan(&aid, &f.ID, &f.RecipientID, &f.Type, &f.Page, &f.X, &f.Y, &f.W, &f.H, &f.Required, &f.Label, &f.Value); err != nil {
			rows.Close()
			return fmt.Errorf("load fields: %w", err)
		}
		list[index[aid]].Fields = append(list[index[aid]].Fields, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = q.Query(ctx, `SELECT agreement_id, id, recipient_id, token_hash, expires_at, revoked_at, created_at
		FROM agreement_links WHERE agreement_id = ANY($1) ORDER BY created_at, id`, ids)
	if err != nil {
		return fmt.Errorf("load links: %w", err)
	}
	for rows.Next() {
		var aid uuid.UUID
		var l Link
		if err := rows.Scan(&aid, &l.ID, &l.RecipientID, &l.TokenHash, &l.ExpiresAt, &l.RevokedAt, &l.CreatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("load links: %w", err)
		}
		l.ExpiresAt, l.CreatedAt, l.RevokedAt = l.ExpiresAt.UTC(), l.CreatedAt.UTC(), utc(l.RevokedAt)
		list[index[aid]].Links = append(list[index[aid]].Links, l)
	}
	rows.Close()
	return rows.Err()
}

func loadMany(ctx context.Context, q querier, sql string, args ...any) ([]Agreement, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("load agreements: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Agreement, error) { return scanAgreement(row) })
	if err != nil {
		return nil, fmt.Errorf("load agreements: %w", err)
	}
	if list == nil {
		list = []Agreement{}
	}
	return list, children(ctx, q, list)
}

func loadOne(ctx context.Context, q querier, lock bool, where string, args ...any) (Agreement, error) {
	sql := `SELECT ` + agreementColumns + ` FROM agreements WHERE ` + where
	if lock {
		sql += ` FOR UPDATE`
	}
	a, err := scanAgreement(q.QueryRow(ctx, sql, args...))
	if err != nil {
		return Agreement{}, err
	}
	list := []Agreement{a}
	if err := children(ctx, q, list); err != nil {
		return Agreement{}, err
	}
	return list[0], nil
}

func agreementArgs(a Agreement) ([]any, error) {
	pages, err := json.Marshal(a.Pages)
	if err != nil {
		return nil, err
	}
	if a.Pages == nil {
		pages = []byte("[]")
	}
	var oKey, oSHA, oName, fKey, fSHA, fName *string
	var oSize, fSize *int64
	if d := a.Original; d != nil {
		oKey, oSHA, oName, oSize = &d.ObjectKey, &d.SHA256, &d.FileName, &d.Size
	}
	if d := a.Final; d != nil {
		fKey, fSHA, fName, fSize = &d.ObjectKey, &d.SHA256, &d.FileName, &d.Size
	}
	var sender *uuid.UUID
	if a.SenderID != uuid.Nil {
		sender = &a.SenderID
	}
	return []any{a.ID, a.Reference, a.Name, a.Message, a.Status, a.Source, a.LeaseID, sender, a.SenderName, a.SenderEmail, a.OrganisationID,
		a.OrganisationName, a.ExpiresAt, a.ReminderFrequency, oKey, oSHA, oSize, oName, string(pages), fKey, fSHA, fSize, fName,
		a.CancelReason, a.CreatedAt, a.UpdatedAt, a.SentAt, a.CompletedAt, a.DeclinedAt, a.CancelledAt, a.ExpiredAt}, nil
}

func saveChildren(ctx context.Context, q querier, a Agreement) error {
	ids := make([]uuid.UUID, 0, len(a.Recipients))
	for _, r := range a.Recipients {
		ids = append(ids, r.ID)
	}
	if _, err := q.Exec(ctx, `DELETE FROM agreement_recipients WHERE agreement_id = $1 AND NOT (id = ANY($2))`, a.ID, ids); err != nil {
		return fmt.Errorf("drop recipients: %w", err)
	}
	for _, r := range a.Recipients {
		sig, err := jsonOrNil(r.Signature)
		if err != nil {
			return err
		}
		if r.Signature == nil {
			sig = nil
		}
		ini, err := jsonOrNil(r.Initials)
		if err != nil {
			return err
		}
		if r.Initials == nil {
			ini = nil
		}
		stamp, err := stampJSON(r.Stamp)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `
			INSERT INTO agreement_recipients (id, agreement_id, signing_order, role, name, email, label, status, activated_at, first_viewed_at,
				completed_at, declined_at, decline_reason, code_hash, code_link_id, code_expires_at, code_attempts, code_sent_at, code_send_count,
				ip, user_agent, verification, signature, initials, stamp)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25)
			ON CONFLICT (id) DO UPDATE SET signing_order = EXCLUDED.signing_order, role = EXCLUDED.role, name = EXCLUDED.name,
				email = EXCLUDED.email, label = EXCLUDED.label, status = EXCLUDED.status, activated_at = EXCLUDED.activated_at,
				first_viewed_at = EXCLUDED.first_viewed_at, completed_at = EXCLUDED.completed_at, declined_at = EXCLUDED.declined_at,
				decline_reason = EXCLUDED.decline_reason, code_hash = EXCLUDED.code_hash, code_link_id = EXCLUDED.code_link_id,
				code_expires_at = EXCLUDED.code_expires_at, code_attempts = EXCLUDED.code_attempts, code_sent_at = EXCLUDED.code_sent_at,
				code_send_count = EXCLUDED.code_send_count, ip = EXCLUDED.ip, user_agent = EXCLUDED.user_agent,
				verification = EXCLUDED.verification, signature = EXCLUDED.signature, initials = EXCLUDED.initials, stamp = EXCLUDED.stamp`,
			r.ID, a.ID, r.Order, r.Role, r.Name, r.Email, r.Label, r.Status, r.ActivatedAt, r.FirstViewedAt, r.CompletedAt, r.DeclinedAt,
			r.DeclineReason, r.CodeHash, r.CodeLinkID, r.CodeExpiresAt, r.CodeAttempts, r.CodeSentAt, r.CodeSendCount, r.IP, r.UserAgent,
			r.Verification, sig, ini, stamp); err != nil {
			return fmt.Errorf("save recipient: %w", err)
		}
	}
	if _, err := q.Exec(ctx, `DELETE FROM agreement_fields WHERE agreement_id = $1`, a.ID); err != nil {
		return fmt.Errorf("replace fields: %w", err)
	}
	for i, f := range a.Fields {
		if _, err := q.Exec(ctx, `INSERT INTO agreement_fields (id, agreement_id, recipient_id, type, page, x, y, w, h, required, label, value, position)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			f.ID, a.ID, f.RecipientID, f.Type, f.Page, f.X, f.Y, f.W, f.H, f.Required, f.Label, f.Value, i); err != nil {
			return fmt.Errorf("save field: %w", err)
		}
	}
	for _, l := range a.Links {
		if _, err := q.Exec(ctx, `INSERT INTO agreement_links (id, agreement_id, recipient_id, token_hash, expires_at, revoked_at, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (id) DO UPDATE SET expires_at = EXCLUDED.expires_at, revoked_at = EXCLUDED.revoked_at
			WHERE agreement_links.expires_at IS DISTINCT FROM EXCLUDED.expires_at OR agreement_links.revoked_at IS DISTINCT FROM EXCLUDED.revoked_at`,
			l.ID, a.ID, l.RecipientID, l.TokenHash, l.ExpiresAt, l.RevokedAt, l.CreatedAt); err != nil {
			return fmt.Errorf("save link: %w", err)
		}
	}
	return nil
}

func insertEvents(ctx context.Context, q querier, events []Event) error {
	for _, e := range events {
		var detail *string
		if e.Detail != nil {
			var err error
			if detail, err = jsonOrNil(e.Detail); err != nil {
				return err
			}
		}
		if _, err := q.Exec(ctx, `INSERT INTO agreement_events (id, agreement_id, action, actor_type, actor_user_id, actor_name, recipient_id,
				ip, user_agent, detail, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			e.ID, e.AgreementID, e.Action, e.ActorType, e.ActorUserID, e.ActorName, e.RecipientID, e.IP, e.UserAgent, detail, e.At); err != nil {
			return fmt.Errorf("insert agreement event: %w", err)
		}
	}
	return nil
}

// NextReference implements Store.
func (s *PostgresStore) NextReference(ctx context.Context, year int) (string, error) {
	var n int64
	if err := s.pool.QueryRow(ctx, `SELECT nextval('agreement_reference_seq')`).Scan(&n); err != nil {
		return "", fmt.Errorf("next agreement reference: %w", err)
	}
	return fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, n), nil
}

// CreateAgreement implements Store.
func (s *PostgresStore) CreateAgreement(ctx context.Context, a Agreement, events []Event) error {
	args, err := agreementArgs(a)
	if err != nil {
		return err
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO agreements (`+agreementColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
			$14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31)`, args...)
		var pg *pgconn.PgError
		if errors.As(err, &pg) && pg.Code == "23505" && pg.ConstraintName == "agreements_reference_key" {
			return ErrReferenceTaken
		}
		if err != nil {
			return fmt.Errorf("insert agreement: %w", err)
		}
		if err := saveChildren(ctx, tx, a); err != nil {
			return err
		}
		return insertEvents(ctx, tx, events)
	})
}

// Agreement implements Store.
func (s *PostgresStore) Agreement(ctx context.Context, id uuid.UUID) (Agreement, error) {
	return loadOne(ctx, s.pool, false, `id = $1`, id)
}

// Agreements implements Store.
func (s *PostgresStore) Agreements(ctx context.Context, f Filter) ([]Agreement, error) {
	orgs, sorgs := f.Organisations, f.SenderOrganisations
	if orgs == nil {
		orgs = []uuid.UUID{}
	}
	if sorgs == nil {
		sorgs = []uuid.UUID{}
	}
	return loadMany(ctx, s.pool, `SELECT `+agreementColumns+` FROM agreements
		WHERE ($1 = '' OR status = $1)
		  AND ($2 OR organisation_id = ANY($3)
		       OR (sender_id = $4 AND (organisation_id = ANY($5) OR ($6 AND organisation_id IS NULL))))
		ORDER BY created_at DESC, id DESC`, f.Status, f.All, orgs, f.Sender, sorgs, f.SenderDirect)
}

// ChangeAgreement implements Store.
func (s *PostgresStore) ChangeAgreement(ctx context.Context, id uuid.UUID, change func(a *Agreement) ([]Event, error)) (Agreement, error) {
	var out Agreement
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		a, err := loadOne(ctx, tx, true, `id = $1`, id)
		if err != nil {
			return err
		}
		events, err := change(&a)
		if err != nil {
			return err
		}
		a.ID = id
		args, err := agreementArgs(a)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agreements SET reference = $2, name = $3, message = $4, status = $5, source = $6, lease_id = $7,
				sender_id = $8, sender_name = $9, sender_email = $10, organisation_id = $11, organisation_name = $12, expires_at = $13,
				reminder_frequency = $14, original_object_key = $15, original_sha256 = $16, original_size = $17, original_file_name = $18,
				pages = $19, final_object_key = $20, final_sha256 = $21, final_size = $22, final_file_name = $23, cancel_reason = $24,
				created_at = $25, updated_at = $26, sent_at = $27, completed_at = $28, declined_at = $29, cancelled_at = $30, expired_at = $31
			WHERE id = $1`, args...); err != nil {
			return fmt.Errorf("update agreement: %w", err)
		}
		if err := saveChildren(ctx, tx, a); err != nil {
			return err
		}
		out = a
		return insertEvents(ctx, tx, events)
	})
	return out, err
}

// DeleteAgreement implements Store.
func (s *PostgresStore) DeleteAgreement(ctx context.Context, id uuid.UUID) (Agreement, error) {
	var out Agreement
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		a, err := loadOne(ctx, tx, true, `id = $1`, id)
		if err != nil {
			return err
		}
		if a.Status != StatusDraft {
			return ErrState
		}
		if _, err := tx.Exec(ctx, `DELETE FROM agreements WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete agreement: %w", err)
		}
		out = a
		return nil
	})
	return out, err
}

// LinkByToken implements Store.
func (s *PostgresStore) LinkByToken(ctx context.Context, tokenHash []byte) (Link, uuid.UUID, error) {
	var l Link
	var aid uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id, recipient_id, token_hash, expires_at, revoked_at, created_at, agreement_id
		FROM agreement_links WHERE token_hash = $1`, tokenHash).Scan(&l.ID, &l.RecipientID, &l.TokenHash, &l.ExpiresAt, &l.RevokedAt, &l.CreatedAt, &aid)
	if errors.Is(err, pgx.ErrNoRows) {
		return Link{}, uuid.Nil, ErrLinkNotFound
	}
	if err != nil {
		return Link{}, uuid.Nil, fmt.Errorf("load signing link: %w", err)
	}
	l.ExpiresAt, l.CreatedAt, l.RevokedAt = l.ExpiresAt.UTC(), l.CreatedAt.UTC(), utc(l.RevokedAt)
	return l, aid, nil
}

// RecipientAgreements implements Store.
func (s *PostgresStore) RecipientAgreements(ctx context.Context, email string) ([]Agreement, error) {
	return loadMany(ctx, s.pool, `SELECT `+agreementColumns+` FROM agreements
		WHERE status <> 'DRAFT' AND id IN (SELECT agreement_id FROM agreement_recipients WHERE email = $1)
		ORDER BY created_at DESC, id DESC`, email)
}

// ByReference implements Store.
func (s *PostgresStore) ByReference(ctx context.Context, reference string) (Agreement, error) {
	return loadOne(ctx, s.pool, false, `reference = $1`, reference)
}

// BySHA256 implements Store.
func (s *PostgresStore) BySHA256(ctx context.Context, sha string) (Agreement, string, error) {
	a, err := loadOne(ctx, s.pool, false, `status = 'COMPLETED' AND final_sha256 = $1 LIMIT 1`, sha)
	if err == nil {
		return a, "FINAL", nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Agreement{}, "", err
	}
	a, err = loadOne(ctx, s.pool, false, `status = 'COMPLETED' AND original_sha256 = $1 ORDER BY completed_at DESC LIMIT 1`, sha)
	if err != nil {
		return Agreement{}, "", err
	}
	return a, "ORIGINAL", nil
}

// Events implements Store.
func (s *PostgresStore) Events(ctx context.Context, id uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, agreement_id, action, actor_type, actor_user_id, actor_name, recipient_id, ip, user_agent, detail, created_at
		FROM agreement_events WHERE agreement_id = $1 ORDER BY created_at, id`, id)
	if err != nil {
		return nil, fmt.Errorf("load agreement events: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var detail []byte
		if err := row.Scan(&e.ID, &e.AgreementID, &e.Action, &e.ActorType, &e.ActorUserID, &e.ActorName, &e.RecipientID, &e.IP, &e.UserAgent,
			&detail, &e.At); err != nil {
			return Event{}, err
		}
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		e.At = e.At.UTC()
		return e, nil
	})
	if err != nil {
		return nil, fmt.Errorf("load agreement events: %w", err)
	}
	if out == nil {
		out = []Event{}
	}
	return out, nil
}

// ClaimJob implements Store.
func (s *PostgresStore) ClaimJob(ctx context.Context, key string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO agreement_jobs (key, created_at) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING`, key, at)
	if err != nil {
		return false, fmt.Errorf("claim job: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// SavedSignature implements Store.
func (s *PostgresStore) SavedSignature(ctx context.Context, userID uuid.UUID) (SavedSignature, bool, error) {
	var sig, ini, stamp []byte
	var out SavedSignature
	err := s.pool.QueryRow(ctx, `SELECT signature, initials, stamp, updated_at FROM user_signatures WHERE user_id = $1`, userID).Scan(&sig, &ini, &stamp, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SavedSignature{}, false, nil
	}
	if err != nil {
		return SavedSignature{}, false, fmt.Errorf("load saved signature: %w", err)
	}
	if out.Signature, err = readAdopted(sig); err != nil {
		return SavedSignature{}, false, err
	}
	if out.Initials, err = readAdopted(ini); err != nil {
		return SavedSignature{}, false, err
	}
	if out.Stamp, err = readStamp(stamp); err != nil {
		return SavedSignature{}, false, err
	}
	return out, true, nil
}

// PutSavedSignature implements Store.
func (s *PostgresStore) PutSavedSignature(ctx context.Context, userID uuid.UUID, v SavedSignature) error {
	var sig, ini *string
	var err error
	if v.Signature != nil {
		if sig, err = jsonOrNil(v.Signature); err != nil {
			return err
		}
	}
	if v.Initials != nil {
		if ini, err = jsonOrNil(v.Initials); err != nil {
			return err
		}
	}
	stamp, err := stampJSON(v.Stamp)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO user_signatures (user_id, signature, initials, stamp, updated_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (user_id) DO UPDATE SET signature = EXCLUDED.signature, initials = EXCLUDED.initials, stamp = EXCLUDED.stamp,
			updated_at = EXCLUDED.updated_at`,
		userID, sig, ini, stamp, v.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save signature: %w", err)
	}
	return nil
}
