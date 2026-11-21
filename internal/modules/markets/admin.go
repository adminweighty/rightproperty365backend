package markets

// Market administration (API 0.56.0, BR-001): administrators provision a
// country (PLANNED, not public), change its currencies, time zone, default
// listing package and notice, and activate or suspend it. Activation is
// refused while the market is not ready (MARKET_NOT_READY). Every change is
// in the administrator audit log.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Admin statuses.
const (
	AdminPlanned   = "PLANNED"
	AdminActive    = "ACTIVE"
	AdminSuspended = "SUSPENDED"
)

// Limits and audit vocabulary.
const (
	MaxExtraCurrencies = 5
	MaxNoticeRunes     = 280
	TargetMarket       = "MARKET"
	ActionCreated      = "MARKET_CREATED"
	ActionUpdated      = "MARKET_UPDATED"
)

// Errors.
var (
	ErrNotFound = errors.New("market not found")
	ErrExists   = errors.New("market already exists")
	// ErrForbidden: only a platform administrator may provision a country.
	ErrForbidden = errors.New("not allowed")
)

// NotReadyError lists why a market cannot be activated.
type NotReadyError struct{ Problems []string }

func (e *NotReadyError) Error() string { return "market not ready: " + strings.Join(e.Problems, "; ") }

// ValidationError carries per-field messages.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// AdminStore reads and writes every market, whatever its status.
type AdminStore interface {
	All(ctx context.Context) ([]Market, error)
	Get(ctx context.Context, code string) (Market, error)
	// Create inserts the market (ErrExists for a taken code) with its audit.
	Create(ctx context.Context, m Market, audit adminaudit.Event) error
	// Update writes the market's editable fields with its audit.
	Update(ctx context.Context, m Market, audit adminaudit.Event) error
}

// Readiness is what activation needs.
type Readiness struct {
	Ready    bool     `json:"ready"`
	Problems []string `json:"problems"`
	Warnings []string `json:"warnings"`
}

// Checker answers the readiness questions outside this module.
type Checker interface {
	// PackageActive reports whether code is an active listing package of the market.
	PackageActive(ctx context.Context, market, code string) (bool, error)
	// LegalSource returns where the market's document of kind resolves from:
	// "MARKET", "GLOBAL", or "" when it has none.
	LegalSource(ctx context.Context, market, kind string) (string, error)
}

// Admin is the market administration service.
type Admin struct {
	store   AdminStore
	checker Checker
	now     func() time.Time
}

// NewAdmin builds the service.
func NewAdmin(store AdminStore, checker Checker, now func() time.Time) *Admin {
	if now == nil {
		now = time.Now
	}
	return &Admin{store: store, checker: checker, now: now}
}

// marketsOf returns the markets the caller administers (all=true for the global roles).
func marketsOf(p *auth.Principal) (codes []string, all, ok bool) {
	if p == nil || p.SessionScope != auth.ScopeAdmin {
		return nil, false, false
	}
	for _, r := range p.PlatformRoles {
		switch r.Role {
		case auth.PlatformSuperAdmin, auth.PlatformAdmin:
			return nil, true, true
		case auth.PlatformMarketAdmin:
			if r.MarketCode != "" && !slices.Contains(codes, r.MarketCode) {
				codes = append(codes, r.MarketCode)
			}
		}
	}
	return codes, false, len(codes) > 0
}

// AdminRoles may use the market administration.
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformMarketAdmin}
}

// List returns the markets the caller administers.
func (a *Admin) List(ctx context.Context, p *auth.Principal, status, q string) ([]Market, error) {
	codes, all, ok := marketsOf(p)
	if !ok {
		return []Market{}, nil
	}
	every, err := a.store.All(ctx)
	if err != nil {
		return nil, err
	}
	q = Fold(q)
	out := []Market{}
	for _, m := range every {
		if (!all && !slices.Contains(codes, m.Code)) || (status != "" && m.AdminStatus != status) {
			continue
		}
		if q != "" && !strings.Contains(Fold(m.Name), q) && !strings.EqualFold(m.Code, q) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// Get returns one market and its readiness.
func (a *Admin) Get(ctx context.Context, p *auth.Principal, code string) (Market, Readiness, error) {
	m, err := a.visible(ctx, p, code)
	if err != nil {
		return Market{}, Readiness{}, err
	}
	r, err := a.readiness(ctx, m)
	return m, r, err
}

func (a *Admin) visible(ctx context.Context, p *auth.Principal, code string) (Market, error) {
	codes, all, ok := marketsOf(p)
	code = strings.ToUpper(strings.TrimSpace(code))
	if !ok || (!all && !slices.Contains(codes, code)) {
		return Market{}, ErrNotFound
	}
	return a.store.Get(ctx, code)
}

var (
	codeRe     = regexp.MustCompile(`^[A-Z]{2}$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
)

func (a *Admin) readiness(ctx context.Context, m Market) (Readiness, error) {
	r := Readiness{Problems: []string{}, Warnings: []string{}}
	if _, err := time.LoadLocation(m.Timezone); err != nil || m.Timezone == "" || m.Timezone == "Local" {
		r.Problems = append(r.Problems, "The time zone is not a valid IANA zone.")
	}
	if !currencyRe.MatchString(m.CurrencyCode) {
		r.Problems = append(r.Problems, "The currency is not an ISO 4217 code.")
	}
	if a.checker != nil {
		if m.DefaultPackageCode == "" {
			r.Problems = append(r.Problems, "Choose a default listing package.")
		} else if ok, err := a.checker.PackageActive(ctx, m.Code, m.DefaultPackageCode); err != nil {
			return r, err
		} else if !ok {
			r.Problems = append(r.Problems, "The default listing package is not an active package of this country.")
		}
		for _, doc := range []struct{ kind, name string }{{"TERMS", "Terms of service"}, {"PRIVACY", "Privacy policy"}} {
			src, err := a.checker.LegalSource(ctx, m.Code, doc.kind)
			if err != nil {
				return r, err
			}
			switch src {
			case "":
				r.Problems = append(r.Problems, "No "+doc.name+" is published for this country or globally.")
			case "GLOBAL":
				r.Warnings = append(r.Warnings, "The "+doc.name+" is the global version; this country has none of its own.")
			}
		}
	}
	r.Ready = len(r.Problems) == 0
	return r, nil
}

// CreateInput is the body of POST /v1/admin/markets.
type CreateInput struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	CurrencyCode string `json:"currency_code"`
	Timezone     string `json:"timezone"`
}

func cleanName(raw string) (string, string) {
	name := strings.Join(strings.Fields(raw), " ")
	switch {
	case name == "":
		return "", "is required"
	case len([]rune(name)) > 80:
		return "", "must be at most 80 characters"
	}
	return name, ""
}

func checkZone(raw string) (string, string) {
	z := strings.TrimSpace(raw)
	if _, err := time.LoadLocation(z); err != nil || z == "" || z == "Local" {
		return "", "must be an IANA time zone such as Africa/Harare"
	}
	return z, ""
}

// Create provisions a country (PLANNED: not public). Global roles only.
func (a *Admin) Create(ctx context.Context, p *auth.Principal, in CreateInput) (Market, error) {
	if _, all, ok := marketsOf(p); !ok || !all {
		return Market{}, ErrForbidden
	}
	f := map[string]string{}
	m := Market{Code: strings.ToUpper(strings.TrimSpace(in.Code)), AdminStatus: AdminPlanned, Status: StatusActive, SortOrder: DefaultSortOrder,
		ExtraCurrencies: []string{}, UpdatedAt: a.now().UTC()}
	if !codeRe.MatchString(m.Code) {
		f["code"] = "must be an ISO 3166-1 alpha-2 code such as BW"
	}
	var msg string
	if m.Name, msg = cleanName(in.Name); msg != "" {
		f["name"] = msg
	}
	if m.CurrencyCode = strings.ToUpper(strings.TrimSpace(in.CurrencyCode)); !currencyRe.MatchString(m.CurrencyCode) {
		f["currency_code"] = "must be an ISO 4217 code such as BWP"
	}
	if m.Timezone, msg = checkZone(in.Timezone); msg != "" {
		f["timezone"] = msg
	}
	if len(f) > 0 {
		return Market{}, &ValidationError{Fields: f}
	}
	m.ID = IDFor(m.Code)
	audit := adminaudit.New(p.UserID, p.Name, ActionCreated, TargetMarket, m.Code, m.Name, "", nil, snapshot(m), m.UpdatedAt)
	if err := a.store.Create(ctx, m, audit); err != nil {
		return Market{}, err
	}
	return m, nil
}

// UpdateInput is the body of PATCH /v1/admin/markets/{code}.
type UpdateInput struct {
	Name               *string   `json:"name"`
	CurrencyCode       *string   `json:"currency_code"`
	ExtraCurrencies    *[]string `json:"extra_currencies"`
	Timezone           *string   `json:"timezone"`
	DefaultPackageCode *string   `json:"default_package_code"`
	Notice             *string   `json:"notice"`
	Status             *string   `json:"status"`
}

func snapshot(m Market) map[string]any {
	notice := ""
	if m.Notice != nil {
		notice = *m.Notice
	}
	return map[string]any{"name": m.Name, "currency_code": m.CurrencyCode, "extra_currencies": m.ExtraCurrencies, "timezone": m.Timezone,
		"default_package_code": m.DefaultPackageCode, "notice": notice, "status": m.AdminStatus}
}

// Update changes a market. A MARKET_ADMIN may change its own market but not
// its status.
func (a *Admin) Update(ctx context.Context, p *auth.Principal, code string, in UpdateInput) (Market, Readiness, error) {
	cur, err := a.visible(ctx, p, code)
	if err != nil {
		return Market{}, Readiness{}, err
	}
	_, all, _ := marketsOf(p)
	next := cur
	next.ExtraCurrencies = append([]string{}, cur.ExtraCurrencies...)
	f := map[string]string{}
	var msg string
	if in.Name != nil {
		if next.Name, msg = cleanName(*in.Name); msg != "" {
			f["name"] = msg
		}
	}
	if in.CurrencyCode != nil {
		if next.CurrencyCode = strings.ToUpper(strings.TrimSpace(*in.CurrencyCode)); !currencyRe.MatchString(next.CurrencyCode) {
			f["currency_code"] = "must be an ISO 4217 code"
		}
	}
	if in.ExtraCurrencies != nil {
		next.ExtraCurrencies = []string{}
		for _, c := range *in.ExtraCurrencies {
			c = strings.ToUpper(strings.TrimSpace(c))
			switch {
			case !currencyRe.MatchString(c):
				f["extra_currencies"] = "must be ISO 4217 codes"
			case c != next.CurrencyCode && !slices.Contains(next.ExtraCurrencies, c):
				next.ExtraCurrencies = append(next.ExtraCurrencies, c)
			}
		}
		if len(next.ExtraCurrencies) > MaxExtraCurrencies {
			f["extra_currencies"] = fmt.Sprintf("at most %d", MaxExtraCurrencies)
		}
		sort.Strings(next.ExtraCurrencies)
	}
	if in.Timezone != nil {
		if next.Timezone, msg = checkZone(*in.Timezone); msg != "" {
			f["timezone"] = msg
		}
	}
	if in.DefaultPackageCode != nil {
		next.DefaultPackageCode = strings.TrimSpace(*in.DefaultPackageCode)
	}
	if in.Notice != nil {
		n := strings.Join(strings.Fields(*in.Notice), " ")
		switch {
		case len([]rune(n)) > MaxNoticeRunes:
			f["notice"] = fmt.Sprintf("must be at most %d characters", MaxNoticeRunes)
		case n == "":
			next.Notice = nil
		default:
			next.Notice = &n
		}
	}
	if in.Status != nil {
		switch {
		case !all:
			f["status"] = "only a platform administrator can change a country's status"
		case !slices.Contains([]string{AdminPlanned, AdminActive, AdminSuspended}, *in.Status):
			f["status"] = "must be PLANNED, ACTIVE or SUSPENDED"
		case *in.Status == AdminPlanned && cur.AdminStatus != AdminPlanned:
			f["status"] = "a country that was public can be SUSPENDED, not PLANNED again"
		default:
			next.AdminStatus = *in.Status
		}
	}
	if len(f) > 0 {
		return Market{}, Readiness{}, &ValidationError{Fields: f}
	}
	r, err := a.readiness(ctx, next)
	if err != nil {
		return Market{}, Readiness{}, err
	}
	if next.AdminStatus == AdminActive && cur.AdminStatus != AdminActive && !r.Ready {
		return Market{}, r, &NotReadyError{Problems: r.Problems}
	}
	next.Enabled = next.AdminStatus == AdminActive
	next.UpdatedAt = a.now().UTC()
	audit := adminaudit.New(p.UserID, p.Name, ActionUpdated, TargetMarket, next.Code, next.Name, "", snapshot(cur), snapshot(next), next.UpdatedAt)
	if err := a.store.Update(ctx, next, audit); err != nil {
		return Market{}, Readiness{}, err
	}
	return next, r, nil
}

// ---- Stores ---------------------------------------------------------------

// SetAudit records the market audit events in log (memory).
func (s *MemoryStore) SetAudit(log *adminaudit.MemoryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audit = log
}

// All implements AdminStore.
func (s *MemoryStore) All(context.Context) ([]Market, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Market, 0, len(s.markets))
	for _, m := range s.markets {
		out = append(out, withCurrencies(m))
	}
	return out, nil
}

// Get implements AdminStore.
func (s *MemoryStore) Get(_ context.Context, code string) (Market, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, m := range s.markets {
		if m.Code == code {
			return withCurrencies(m), nil
		}
	}
	return Market{}, ErrNotFound
}

// Create implements AdminStore.
func (s *MemoryStore) Create(_ context.Context, m Market, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.markets {
		if o.Code == m.Code {
			return ErrExists
		}
	}
	s.markets = append(s.markets, m)
	Order(s.markets)
	if s.audit != nil {
		s.audit.Append(audit)
	}
	return nil
}

// Update implements AdminStore.
func (s *MemoryStore) Update(_ context.Context, m Market, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.markets {
		if s.markets[i].Code == m.Code {
			m.Currencies = nil
			s.markets[i] = m
			if s.audit != nil {
				s.audit.Append(audit)
			}
			return nil
		}
	}
	return ErrNotFound
}

const adminColumns = `id, code, name, currency_code, timezone, sort_order, extra_currencies, notice, status, enabled, COALESCE(default_package_code, ''), updated_at`

func scanAdmin(row pgx.Row) (Market, error) {
	m := Market{Status: StatusActive}
	err := row.Scan(&m.ID, &m.Code, &m.Name, &m.CurrencyCode, &m.Timezone, &m.SortOrder, &m.ExtraCurrencies, &m.Notice, &m.AdminStatus, &m.Enabled,
		&m.DefaultPackageCode, &m.UpdatedAt)
	m.UpdatedAt = m.UpdatedAt.UTC()
	if m.ExtraCurrencies == nil {
		m.ExtraCurrencies = []string{}
	}
	return withCurrencies(m), err
}

// All implements AdminStore.
func (s *PostgresStore) All(ctx context.Context) ([]Market, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+adminColumns+` FROM markets ORDER BY sort_order, name, code`)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Market, error) { return scanAdmin(row) })
	if err != nil {
		return nil, err
	}
	Order(out)
	return out, nil
}

// Get implements AdminStore.
func (s *PostgresStore) Get(ctx context.Context, code string) (Market, error) {
	m, err := scanAdmin(s.pool.QueryRow(ctx, `SELECT `+adminColumns+` FROM markets WHERE code = $1`, code))
	if errors.Is(err, pgx.ErrNoRows) {
		return Market{}, ErrNotFound
	}
	return m, err
}

// Create implements AdminStore.
func (s *PostgresStore) Create(ctx context.Context, m Market, audit adminaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	id := m.ID
	if id == uuid.Nil {
		id = uuid.New()
	}
	tag, err := tx.Exec(ctx, `
		INSERT INTO markets (id, code, name, currency_code, timezone, status, enabled, sort_order, extra_currencies, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'PLANNED', false, $6, '{}', $7)
		ON CONFLICT (code) DO NOTHING`, id, m.Code, m.Name, m.CurrencyCode, m.Timezone, m.SortOrder, m.UpdatedAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrExists
	}
	if err := adminaudit.Insert(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Update implements AdminStore.
func (s *PostgresStore) Update(ctx context.Context, m Market, audit adminaudit.Event) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var pkg *string
	if m.DefaultPackageCode != "" {
		pkg = &m.DefaultPackageCode
	}
	tag, err := tx.Exec(ctx, `
		UPDATE markets SET name = $2, currency_code = $3, extra_currencies = $4, timezone = $5, default_package_code = $6,
			notice = $7, status = $8, enabled = $9, updated_at = $10
		WHERE code = $1`, m.Code, m.Name, m.CurrencyCode, m.ExtraCurrencies, m.Timezone, pkg, m.Notice, m.AdminStatus, m.Enabled, m.UpdatedAt)
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
