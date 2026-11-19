package legal

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Store holds the document versions.
type Store interface {
	// Versions returns the matching versions, newest first (SortVersions).
	Versions(ctx context.Context, f Filter) ([]Version, error)
	// Version returns one version, or ErrNotFound.
	Version(ctx context.Context, id uuid.UUID) (Version, error)
	// Create adds a version with its audit record (ErrDuplicate).
	Create(ctx context.Context, v Version, audit adminaudit.Event) error
	// Change locks the version, calls change with a copy and saves it with
	// the audit record. ErrImmutable when the change touches a frozen row.
	Change(ctx context.Context, id uuid.UUID, change func(v *Version) (adminaudit.Event, error)) (Version, error)
	// Delete removes a DRAFT (check decides and returns the audit record).
	Delete(ctx context.Context, id uuid.UUID, check func(v Version) (adminaudit.Event, error)) error
}

// AcceptanceStore holds the acceptances (user_consents).
type AcceptanceStore interface {
	// Acceptances returns the person's acceptances, newest first.
	Acceptances(ctx context.Context, userID uuid.UUID) ([]Acceptance, error)
	// Accept records one.
	Accept(ctx context.Context, a Acceptance) error
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

// MemoryStore keeps versions in memory (development and tests).
type MemoryStore struct {
	mu       sync.Mutex
	versions []Version
	audit    *adminaudit.MemoryLog
}

// NewMemoryStore returns a store holding versions (SeedVersions in
// development); audit receives the administrators' records (nil: none).
func NewMemoryStore(versions []Version, audit *adminaudit.MemoryLog) *MemoryStore {
	s := &MemoryStore{audit: audit}
	for _, v := range versions {
		s.versions = append(s.versions, v.Clone())
	}
	return s
}

// Versions implements Store.
func (s *MemoryStore) Versions(_ context.Context, f Filter) ([]Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Version{}
	for _, v := range s.versions {
		if f.Matches(v) {
			out = append(out, v.Clone())
		}
	}
	SortVersions(out)
	return out, nil
}

// Version implements Store.
func (s *MemoryStore) Version(_ context.Context, id uuid.UUID) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.versions {
		if v.ID == id {
			return v.Clone(), nil
		}
	}
	return Version{}, ErrNotFound
}

// Create implements Store.
func (s *MemoryStore) Create(_ context.Context, v Version, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, have := range s.versions {
		if have.Kind == v.Kind && have.MarketCode == v.MarketCode && have.Lang == v.Lang && have.Label == v.Label {
			return ErrDuplicate
		}
	}
	s.versions = append(s.versions, v.Clone())
	if s.audit != nil {
		s.audit.Append(audit)
	}
	return nil
}

// Change implements Store.
func (s *MemoryStore) Change(_ context.Context, id uuid.UUID, change func(v *Version) (adminaudit.Event, error)) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.versions {
		if have.ID != id {
			continue
		}
		next := have.Clone()
		audit, err := change(&next)
		if err != nil {
			return Version{}, err
		}
		if FrozenChange(have, next) {
			return Version{}, ErrImmutable
		}
		for j, other := range s.versions {
			if j != i && other.Kind == next.Kind && other.MarketCode == next.MarketCode && other.Lang == next.Lang && other.Label == next.Label {
				return Version{}, ErrDuplicate
			}
		}
		s.versions[i] = next.Clone()
		if s.audit != nil {
			s.audit.Append(audit)
		}
		return next, nil
	}
	return Version{}, ErrNotFound
}

// Delete implements Store.
func (s *MemoryStore) Delete(_ context.Context, id uuid.UUID, check func(v Version) (adminaudit.Event, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, have := range s.versions {
		if have.ID != id {
			continue
		}
		audit, err := check(have.Clone())
		if err != nil {
			return err
		}
		if have.Status != StatusDraft {
			return ErrImmutable
		}
		s.versions = append(s.versions[:i], s.versions[i+1:]...)
		if s.audit != nil {
			s.audit.Append(audit)
		}
		return nil
	}
	return ErrNotFound
}

// ForgetUser unlinks a deleted account (creator, publisher) and replaces its
// name, as ForgetUserTx does on Postgres.
func (s *MemoryStore) ForgetUser(userID uuid.UUID, deletedName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.versions {
		v := &s.versions[i]
		if v.CreatedBy != nil && *v.CreatedBy == userID {
			v.CreatedBy, v.CreatedByName = nil, deletedName
		}
		if v.PublishedBy != nil && *v.PublishedBy == userID {
			v.PublishedBy, v.PublishedByName = nil, deletedName
		}
	}
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// PostgresStore reads and writes legal_document_versions and user_consents
// (migration 0045). It implements Store and AcceptanceStore.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const versionColumns = `id, kind, market_code, lang, version_label, effective_from, status, body_md, summary_of_changes,
	requires_reacceptance, draft_notice, created_by, created_by_name, published_by, COALESCE(published_by_name, ''), published_at,
	retired_at, created_at, updated_at`

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	err := row.Scan(&v.ID, &v.Kind, &v.MarketCode, &v.Lang, &v.Label, &v.EffectiveFrom, &v.Status, &v.Body, &v.Summary,
		&v.RequiresReacceptance, &v.DraftNotice, &v.CreatedBy, &v.CreatedByName, &v.PublishedBy, &v.PublishedByName, &v.PublishedAt,
		&v.RetiredAt, &v.CreatedAt, &v.UpdatedAt)
	if err != nil {
		return Version{}, err
	}
	v.EffectiveFrom, v.CreatedAt, v.UpdatedAt = v.EffectiveFrom.UTC(), v.CreatedAt.UTC(), v.UpdatedAt.UTC()
	if v.PublishedAt != nil {
		t := v.PublishedAt.UTC()
		v.PublishedAt = &t
	}
	if v.RetiredAt != nil {
		t := v.RetiredAt.UTC()
		v.RetiredAt = &t
	}
	return v, nil
}

// Versions implements Store.
func (s *PostgresStore) Versions(ctx context.Context, f Filter) ([]Version, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+versionColumns+` FROM legal_document_versions
		WHERE ($1 = '' OR kind = $1) AND ($2 = '' OR market_code = $2) AND ($3 = '' OR lang = $3) AND ($4 = '' OR status = $4)
		ORDER BY effective_from DESC, created_at DESC, id DESC`, f.Kind, f.Market, f.Lang, f.Status)
	if err != nil {
		return nil, fmt.Errorf("list legal versions: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Version, error) { return scanVersion(row) })
	if err != nil {
		return nil, fmt.Errorf("list legal versions: %w", err)
	}
	if out == nil {
		out = []Version{}
	}
	SortVersions(out)
	return out, nil
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func pgVersion(ctx context.Context, q rowQuerier, id uuid.UUID, lock bool) (Version, error) {
	sql := `SELECT ` + versionColumns + ` FROM legal_document_versions WHERE id = $1`
	if lock {
		sql += ` FOR UPDATE`
	}
	v, err := scanVersion(q.QueryRow(ctx, sql, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Version{}, ErrNotFound
	}
	if err != nil {
		return Version{}, fmt.Errorf("load legal version: %w", err)
	}
	return v, nil
}

// Version implements Store.
func (s *PostgresStore) Version(ctx context.Context, id uuid.UUID) (Version, error) {
	return pgVersion(ctx, s.pool, id, false)
}

func mapWriteError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "23505" && pgErr.ConstraintName == "legal_document_versions_label_unique":
			return ErrDuplicate
		case pgErr.ConstraintName == "legal_document_versions_frozen":
			return ErrImmutable
		}
	}
	return err
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Create implements Store.
func (s *PostgresStore) Create(ctx context.Context, v Version, audit adminaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO legal_document_versions (id, kind, market_code, lang, version_label, effective_from, status, body_md, summary_of_changes,
			requires_reacceptance, draft_notice, created_by, created_by_name, published_by, published_by_name, published_at, retired_at,
			created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`,
		v.ID, v.Kind, v.MarketCode, v.Lang, v.Label, v.EffectiveFrom, v.Status, v.Body, v.Summary, v.RequiresReacceptance, v.DraftNotice,
		v.CreatedBy, v.CreatedByName, v.PublishedBy, nullable(v.PublishedByName), v.PublishedAt, v.RetiredAt, v.CreatedAt, v.UpdatedAt); err != nil {
		if mapped := mapWriteError(err); mapped != err {
			return mapped
		}
		return fmt.Errorf("insert legal version: %w", err)
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Change implements Store.
func (s *PostgresStore) Change(ctx context.Context, id uuid.UUID, change func(v *Version) (adminaudit.Event, error)) (Version, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Version{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	cur, err := pgVersion(ctx, tx, id, true)
	if err != nil {
		return Version{}, err
	}
	next := cur.Clone()
	audit, err := change(&next)
	if err != nil {
		return Version{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE legal_document_versions SET version_label = $2, effective_from = $3, status = $4, body_md = $5, summary_of_changes = $6,
			requires_reacceptance = $7, draft_notice = $8, published_by = $9, published_by_name = $10, published_at = $11, retired_at = $12,
			updated_at = $13
		WHERE id = $1`,
		id, next.Label, next.EffectiveFrom, next.Status, next.Body, next.Summary, next.RequiresReacceptance, next.DraftNotice,
		next.PublishedBy, nullable(next.PublishedByName), next.PublishedAt, next.RetiredAt, next.UpdatedAt); err != nil {
		if mapped := mapWriteError(err); mapped != err {
			return Version{}, mapped
		}
		return Version{}, fmt.Errorf("update legal version: %w", err)
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return Version{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Version{}, err
	}
	return next, nil
}

// Delete implements Store.
func (s *PostgresStore) Delete(ctx context.Context, id uuid.UUID, check func(v Version) (adminaudit.Event, error)) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	cur, err := pgVersion(ctx, tx, id, true)
	if err != nil {
		return err
	}
	audit, err := check(cur)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM legal_document_versions WHERE id = $1`, id); err != nil {
		if mapped := mapWriteError(err); mapped != err {
			return mapped
		}
		return fmt.Errorf("delete legal version: %w", err)
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Acceptances implements AcceptanceStore.
func (s *PostgresStore) Acceptances(ctx context.Context, userID uuid.UUID) ([]Acceptance, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT user_id, consent_type, version, market_code, context, accepted_at
		FROM user_consents WHERE user_id = $1
		ORDER BY accepted_at DESC, created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list acceptances: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Acceptance, error) {
		var a Acceptance
		var consentType string
		if err := row.Scan(&a.UserID, &consentType, &a.Version, &a.MarketCode, &a.Context, &a.AcceptedAt); err != nil {
			return Acceptance{}, err
		}
		a.Kind, a.AcceptedAt = KindOfConsent(consentType), a.AcceptedAt.UTC()
		return a, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list acceptances: %w", err)
	}
	if out == nil {
		out = []Acceptance{}
	}
	return out, nil
}

// Accept implements AcceptanceStore.
func (s *PostgresStore) Accept(ctx context.Context, a Acceptance) error {
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO user_consents (id, user_id, consent_type, version, accepted_at, created_at, market_code, context)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7)`,
		uuid.New(), a.UserID, ConsentType(a.Kind), a.Version, a.AcceptedAt, a.MarketCode, a.Context); err != nil {
		return fmt.Errorf("insert acceptance: %w", err)
	}
	return nil
}

// Execer is a pgx transaction (or pool).
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// ForgetUserTx unlinks a deleted account from the versions it created or
// published and replaces its name (account deletion anonymises users rows,
// so ON DELETE SET NULL never fires). The legal_document_versions_frozen
// trigger allows exactly this on published rows. Acceptances are KEPT.
func ForgetUserTx(ctx context.Context, tx Execer, userID uuid.UUID, deletedName string) error {
	if _, err := tx.Exec(ctx, `UPDATE legal_document_versions SET created_by = NULL, created_by_name = $2 WHERE created_by = $1`,
		userID, deletedName); err != nil {
		return fmt.Errorf("unlink legal version creator: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE legal_document_versions SET published_by = NULL, published_by_name = $2 WHERE published_by = $1`,
		userID, deletedName); err != nil {
		return fmt.Errorf("unlink legal version publisher: %w", err)
	}
	return nil
}
