package tenancies

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
// Every run uses a fresh organisation, user and contacts.
func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres store is only exercised against a throwaway database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.NewRunner(conn, migs).Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// fixtures are the rows the stores' records point at (real rows for Postgres).
type fixtures struct {
	org, user uuid.UUID
	contacts  [3]uuid.UUID
	// owner is a direct owner with a personal book and one contact in it.
	owner, ownerContact uuid.UUID
}

func newFixtures() fixtures {
	return fixtures{org: uuid.New(), user: uuid.New(), contacts: [3]uuid.UUID{uuid.New(), uuid.New(), uuid.New()}, owner: uuid.New(),
		ownerContact: uuid.New()}
}

// storeContract runs the same checks on both stores.
func storeContract(t *testing.T, s Store, fx fixtures, audits func() int) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	book := OrganisationBook(fx.org)
	prop := Property{ID: newID(), Book: book, Name: "Avondale Court", Address: "12 King George Road", MarketCode: "ZW", CreatedBy: &fx.user,
		CreatedByName: "Tester", CreatedAt: now, UpdatedAt: now}
	whole := Property{ID: newID(), Book: book, Name: "Borrowdale Cottage", Address: "4 Pendennis Road", MarketCode: "ZW", CreatedAt: now.Add(time.Second),
		UpdatedAt: now}
	u1 := Unit{ID: newID(), PropertyID: prop.ID, Label: "Flat 1", Position: 1, CreatedAt: now, UpdatedAt: now}
	u2 := Unit{ID: newID(), PropertyID: prop.ID, Label: "Flat 2", Position: 2, CreatedAt: now, UpdatedAt: now}
	if err := s.Apply(ctx, Change{Properties: []Property{prop, whole}, Units: []Unit{u1, u2}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Property(ctx, prop.ID)
	if err != nil || got.Name != "Avondale Court" || len(got.Units) != 2 || got.Units[0].Label != "Flat 1" || got.CreatedBy == nil || !got.Book.Same(book) {
		t.Fatalf("property: %+v %v", got, err)
	}
	if _, err := s.Property(ctx, uuid.New()); !errors.Is(err, ErrPropertyNotFound) {
		t.Fatalf("unknown property: %v", err)
	}
	dup := Unit{ID: newID(), PropertyID: prop.ID, Label: "flat 1", Position: 3, CreatedAt: now, UpdatedAt: now}
	if err := s.Apply(ctx, Change{Units: []Unit{dup}}); !errors.Is(err, ErrUnitLabelTaken) {
		t.Fatalf("duplicate label: %v", err)
	}
	list, total, err := s.Properties(ctx, book, PropertyFilter{Q: "cottage"})
	if err != nil || total != 1 || list[0].ID != whole.ID || len(list[0].Units) != 0 {
		t.Fatalf("list q: %+v %d %v", list, total, err)
	}
	if list, total, _ := s.Properties(ctx, book, PropertyFilter{Limit: 1}); total != 2 || len(list) != 1 || list[0].ID != whole.ID {
		t.Fatalf("list page (newest first): %+v %d", list, total)
	}

	// Tenancies.
	mk := func(p uuid.UUID, unit *uuid.UUID, tenant uuid.UUID, created time.Time) Tenancy {
		return Tenancy{ID: newID(), Book: book, BookName: "Test Estates", PropertyID: p, UnitID: unit, RentAmount: "950.00", RentCurrency: "USD",
			RentFrequency: FrequencyMonthly, RentDueDay: 1, DepositAmount: "950.50", DepositCurrency: "USD", StartDate: "2026-12-01",
			EndDate: "2027-11-30", Status: StatusDraft, Notes: "n", CreatedBy: &fx.user, CreatedByName: "Tester", CreatedAt: created, UpdatedAt: created,
			Parties: []Party{{ContactID: tenant, Role: RoleTenant, Name: "Tia"}, {ContactID: fx.contacts[2], Role: RoleLandlord, Name: "Lars"}}}
	}
	t1 := mk(prop.ID, &u1.ID, fx.contacts[0], now)
	t2 := mk(prop.ID, &u1.ID, fx.contacts[1], now.Add(time.Second))
	t3 := mk(whole.ID, nil, fx.contacts[0], now.Add(2*time.Second))
	before := audits()
	ev := orgaudit.New(fx.org, fx.user, "Tester", orgaudit.ActionTenancyCreated, orgaudit.TargetTenancy, t1.ID, "Avondale Court Flat 1", nil, nil, now)
	if err := s.Apply(ctx, Change{Tenancies: []Tenancy{t1, t2, t3}, Audit: []orgaudit.Event{ev}}); err != nil {
		t.Fatal(err)
	}
	if audits()-before != 1 {
		t.Fatal("audit not written")
	}
	got1, err := s.Tenancy(ctx, t1.ID)
	if err != nil || got1.RentAmount != "950.00" || got1.RentCurrency != "USD" || got1.DepositAmount != "950.50" || got1.RentDueDay != 1 ||
		got1.StartDate != "2026-12-01" || got1.EndDate != "2027-11-30" || got1.EndedOn != "" || len(got1.Parties) != 2 ||
		got1.Parties[0].Role != RoleTenant || got1.Parties[1].Name != "Lars" || got1.UnitID == nil || *got1.UnitID != u1.ID || got1.BookName != "Test Estates" {
		t.Fatalf("tenancy: %+v %v", got1, err)
	}
	if _, err := s.Tenancy(ctx, uuid.New()); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("unknown tenancy: %v", err)
	}
	// A unit of another property is refused.
	wrong := mk(whole.ID, &u2.ID, fx.contacts[0], now)
	if err := s.Apply(ctx, Change{Tenancies: []Tenancy{wrong}}); !errors.Is(err, ErrUnitNotFound) {
		t.Fatalf("unit of another property: %v", err)
	}
	// Activate t1; t2 on the same unit cannot be ACTIVE too.
	act := got1
	act.Status, act.ActivatedAt, act.ActivatedBy, act.ActivatedByName = StatusActive, &now, &fx.user, "Tester"
	if err := s.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{t1.ID: StatusDraft}, Tenancies: []Tenancy{act}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{t1.ID: StatusDraft}, Tenancies: []Tenancy{act}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale status: %v", err)
	}
	act2 := t2
	act2.Status, act2.ActivatedAt = StatusActive, &now
	if err := s.Apply(ctx, Change{Tenancies: []Tenancy{act2}}); !errors.Is(err, ErrUnitOccupied) {
		t.Fatalf("second active: %v", err)
	}
	// Uses, in-use guards.
	uses, err := s.Uses(ctx, []uuid.UUID{prop.ID, whole.ID})
	if err != nil || len(uses) != 3 {
		t.Fatalf("uses: %+v %v", uses, err)
	}
	if err := s.Apply(ctx, Change{PropertyFree: []uuid.UUID{prop.ID}}); !errors.Is(err, ErrPropertyInUse) {
		t.Fatalf("property in use: %v", err)
	}
	if err := s.Apply(ctx, Change{UnitFree: []uuid.UUID{u1.ID}}); !errors.Is(err, ErrUnitInUse) {
		t.Fatalf("unit in use: %v", err)
	}
	gone := u2
	gone.DeletedAt = &now
	if err := s.Apply(ctx, Change{UnitFree: []uuid.UUID{u2.ID}, Units: []Unit{gone}}); err != nil {
		t.Fatalf("archive a free unit: %v", err)
	}
	if p, _ := s.Property(ctx, prop.ID); len(p.Units) != 2 || len(p.LiveUnits()) != 1 {
		t.Fatalf("archived unit: %+v", p.Units)
	}
	// Delete the draft t2; end t1.
	if err := s.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{t2.ID: StatusDraft}, DeleteTenancies: []uuid.UUID{t2.ID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tenancy(ctx, t2.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	end := act
	end.Status, end.EndedOn, end.EndedAt, end.EndedBy, end.EndReason = StatusTerminated, "2027-06-30", &now, &fx.user, "Relocating"
	if err := s.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{t1.ID: StatusActive}, Tenancies: []Tenancy{end}}); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.Tenancy(ctx, t1.ID); g.Status != StatusTerminated || g.EndedOn != "2027-06-30" || g.EndReason != "Relocating" || g.EndedAt == nil {
		t.Fatalf("ended: %+v", g)
	}
	// Lists and counts.
	ts, n, counts, err := s.Tenancies(ctx, book, TenancyFilter{Status: StatusTerminated})
	if err != nil || n != 1 || len(ts) != 1 || ts[0].ID != t1.ID || counts[StatusTerminated] != 1 || counts[StatusDraft] != 1 || len(ts[0].Parties) != 2 {
		t.Fatalf("tenancies: %+v %d %v %v", ts, n, counts, err)
	}
	if _, n, _, _ := s.Tenancies(ctx, book, TenancyFilter{ContactID: &fx.contacts[2]}); n != 2 {
		t.Fatalf("contact filter: %d", n)
	}
	if _, n, _, _ := s.Tenancies(ctx, book, TenancyFilter{PropertyID: &whole.ID}); n != 1 {
		t.Fatalf("property filter: %d", n)
	}
	if _, n, _, _ := s.Tenancies(ctx, OrganisationBook(uuid.New()), TenancyFilter{}); n != 0 {
		t.Fatal("another book sees tenancies")
	}
	// The tenant lookup: not DRAFT, tenants only.
	mine, err := s.TenanciesOfTenants(ctx, []uuid.UUID{fx.contacts[0], fx.contacts[2]})
	if err != nil || len(mine) != 1 || mine[0].ID != t1.ID {
		t.Fatalf("tenants: %+v %v", mine, err)
	}
	// An archived property takes no open tenancy.
	archived := whole
	archived.DeletedAt = &now
	if err := s.Apply(ctx, Change{PropertyFree: []uuid.UUID{whole.ID}, Properties: []Property{archived}}); !errors.Is(err, ErrPropertyInUse) {
		t.Fatalf("archive in use: %v", err)
	}
	if err := s.Apply(ctx, Change{DeleteTenancies: []uuid.UUID{t3.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, Change{PropertyFree: []uuid.UUID{whole.ID}, Properties: []Property{archived}}); err != nil {
		t.Fatalf("archive free: %v", err)
	}
	if err := s.Apply(ctx, Change{Tenancies: []Tenancy{mk(whole.ID, nil, fx.contacts[0], now)}}); !errors.Is(err, ErrPropertyNotFound) {
		t.Fatalf("tenancy on an archived property: %v", err)
	}
	if _, total, _ := s.Properties(ctx, book, PropertyFilter{}); total != 1 {
		t.Fatalf("archived listed: %d", total)
	}
	if ps, _ := s.PropertiesByIDs(ctx, []uuid.UUID{whole.ID, prop.ID}); len(ps) != 2 {
		t.Fatalf("by ids (archived included): %d", len(ps))
	}
}

func TestMemoryStoreContract(t *testing.T) {
	s := NewMemoryStore()
	log := &orgaudit.MemoryLog{}
	s.SetAuditSink(log.Append)
	storeContract(t, s, newFixtures(), func() int { return len(log.All()) })
}

// seedRows inserts every row the fixtures' foreign keys reference: the
// organisation, the acting user, the direct owner, and the contacts.
func seedRows(t *testing.T, pool *pgxpool.Pool, fx fixtures) {
	t.Helper()
	ctx := context.Background()
	suffix := fx.org.String()[:8]
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
			[]any{fx.org, "Tenancy Test " + suffix}},
		{`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Tenancy Tester', 'ACTIVE', now())`,
			[]any{fx.user, "tenancy." + suffix + "@example.test"}},
		{`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Tenancy Owner', 'ACTIVE', now())`,
			[]any{fx.owner, "tenancy.owner." + suffix + "@example.test"}},
		{`INSERT INTO contacts (id, organisation_id, name, type) VALUES ($1, $4, 'Tia', 'tenant'), ($2, $4, 'Tom', 'tenant'), ($3, $4, 'Lars', 'landlord')`,
			[]any{fx.contacts[0], fx.contacts[1], fx.contacts[2], fx.org}},
		{`INSERT INTO contacts (id, book_user_id, name, type) VALUES ($1, $2, 'Owner Tenant', 'tenant')`, []any{fx.ownerContact, fx.owner}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := newFixtures()
	seedRows(t, pool, fx)
	storeContract(t, NewPostgresStore(pool), fx, func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM organisation_audit_events WHERE organisation_id = $1`, fx.org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}

// The database's own guards (migration 0035), and the account deletion step.
func TestPostgresGuardsAndForgetUser(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := newFixtures()
	seedRows(t, pool, fx)
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	book := PersonalBook(fx.owner)
	prop := Property{ID: newID(), Book: book, Name: "Cottage", Address: "1 Lane", MarketCode: "GB", CreatedBy: &fx.owner, CreatedByName: "Owner",
		CreatedAt: now, UpdatedAt: now}
	mk := func(status string) Tenancy {
		t := Tenancy{ID: newID(), Book: book, BookName: "Tenancy Owner", PropertyID: prop.ID, RentAmount: "1200.00", RentCurrency: "GBP",
			RentFrequency: FrequencyWeekly, RentDueDay: 5, StartDate: "2026-12-01", Status: status, CreatedBy: &fx.owner, CreatedByName: "Owner",
			CreatedAt: now, UpdatedAt: now, Parties: []Party{{ContactID: fx.ownerContact, Role: RoleTenant, Name: "Owner Tenant"}}}
		if status != StatusDraft {
			t.ActivatedAt, t.ActivatedBy = &now, &fx.owner
		}
		return t
	}
	active, draft := mk(StatusActive), mk(StatusDraft)
	if err := s.Apply(ctx, Change{Properties: []Property{prop}, Tenancies: []Tenancy{active, draft}}); err != nil {
		t.Fatal(err)
	}
	refused := map[string]string{
		"weekly due day 8":         `UPDATE tenancies SET rent_due_day = 8 WHERE id = $1`,
		"end before start":         `UPDATE tenancies SET end_date = '2026-01-01' WHERE id = $1`,
		"deposit without currency": `UPDATE tenancies SET deposit_amount = 10 WHERE id = $1`,
		"deposit other currency":   `UPDATE tenancies SET deposit_amount = 10, deposit_currency = 'USD' WHERE id = $1`,
		"ended without ended_on":   `UPDATE tenancies SET status = 'ENDED', ended_at = now() WHERE id = $1`,
		"active without activated": `UPDATE tenancies SET activated_at = NULL WHERE id = $1`,
		"zero rent":                `UPDATE tenancies SET rent_amount = 0 WHERE id = $1`,
		"lower-case currency":      `UPDATE tenancies SET rent_currency = 'gbp' WHERE id = $1`,
	}
	for name, sql := range refused {
		if _, err := pool.Exec(ctx, sql, active.ID); err == nil {
			t.Fatalf("%s: the database accepted it", name)
		}
	}
	// The ledger's tenancy_id now references tenancies.
	var fk bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ledger_entries_tenancy_ref_fkey')`).Scan(&fk); err != nil || !fk {
		t.Fatalf("ledger foreign key: %v %v", fk, err)
	}
	// Account deletion: drafts go, the manager name is anonymised, the person unlinked.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, fx.owner, "Deleted account"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Tenancy(ctx, draft.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("draft kept: %v", err)
	}
	got, err := s.Tenancy(ctx, active.ID)
	if err != nil || got.BookName != "Deleted account" || got.CreatedBy != nil || got.ActivatedBy != nil || got.RentDueDay != 5 || got.RentAmount != "1200.00" {
		t.Fatalf("forgotten: %+v %v", got, err)
	}
	if p, _ := s.Property(ctx, prop.ID); p.CreatedBy != nil {
		t.Fatal("property creator kept")
	}
}
