package tenancies

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/ledger"
)

// rentEntry is a receipt's RENT_RECEIVED ledger entry, as the ledger service builds it.
func rentEntry(fx fixtures, tenancyID uuid.UUID, amount, date string, now time.Time) ledger.Entry {
	contact, tenancy, author := fx.contacts[0], tenancyID, fx.user
	return ledger.Entry{ID: newID(), Book: ledger.OrganisationBook(fx.org), Type: ledger.TypeRentReceived, Direction: ledger.In, Category: "rent",
		Method: ledger.MethodBank, Amount: amount, Currency: "USD", EffectiveDate: date, RecordedAt: now, AuthorUserID: &author, AuthorName: "Tester",
		Status: ledger.StatusUnverified, ContactID: &contact, TenancyID: &tenancy, Description: "Rent: Avondale Court"}
}

// rentStoreContract runs the same rent checks on both store pairs.
func rentStoreContract(t *testing.T, s Store, books ledger.Store, fx fixtures, forget func(uuid.UUID)) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := ConnectLedger(s, books); err != nil {
		t.Fatal(err)
	}
	book := OrganisationBook(fx.org)
	prop := Property{ID: newID(), Book: book, Name: "Avondale Court", Address: "12 King George Road", MarketCode: "ZW", CreatedAt: now, UpdatedAt: now}
	ten := Tenancy{ID: newID(), Book: book, BookName: "Test Estates", PropertyID: prop.ID, RentAmount: "950.00", RentCurrency: "USD",
		RentFrequency: FrequencyMonthly, RentDueDay: 1, StartDate: "2026-09-01", Status: StatusActive, ActivatedAt: &now, ActivatedBy: &fx.user,
		CreatedBy: &fx.user, CreatedAt: now, UpdatedAt: now, Parties: []Party{{ContactID: fx.contacts[0], Role: RoleTenant, Name: "Tia"}}}
	other := ten
	other.ID, other.Parties = newID(), []Party{{ContactID: fx.contacts[1], Role: RoleTenant, Name: "Tom"}}
	otherProp := prop
	otherProp.ID = newID()
	other.PropertyID = otherProp.ID
	if err := s.Apply(ctx, Change{Properties: []Property{prop, otherProp}, Tenancies: []Tenancy{ten, other}}); err != nil {
		t.Fatal(err)
	}
	mkDues := func(tid uuid.UUID, dates ...string) []Due {
		var out []Due
		for i, d := range dates {
			out = append(out, Due{ID: newID(), TenancyID: tid, Sequence: i + 1, DueDate: d, Amount: "950.00", Currency: "USD", CreatedAt: now})
		}
		return out
	}
	dues := mkDues(ten.ID, "2026-09-01", "2026-10-01", "2026-11-01", "2026-12-01")
	foreign := mkDues(other.ID, "2026-09-01")
	if err := s.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{ten.ID: StatusActive}, Dues: append(dues, foreign...)}); err != nil {
		t.Fatal(err)
	}
	// Generation is idempotent: a due date the tenancy has is skipped.
	if err := s.Apply(ctx, Change{Dues: mkDues(ten.ID, "2026-09-01", "2026-10-01")}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Dues(ctx, []uuid.UUID{ten.ID})
	if err != nil || len(got) != 4 || got[0].ID != dues[0].ID || got[3].DueDate != "2026-12-01" || got[0].Amount != "950.00" || len(got[0].Allocations) != 0 {
		t.Fatalf("dues: %+v %v", got, err)
	}
	if _, err := s.Due(ctx, uuid.New()); !errors.Is(err, ErrDueNotFound) {
		t.Fatalf("unknown due: %v", err)
	}
	if list, err := s.ActiveTenancies(ctx); err != nil || !containsTenancy(list, ten.ID) {
		t.Fatalf("active tenancies: %v", err)
	}

	// A receipt and its ledger entry, in one transaction.
	e1 := rentEntry(fx, ten.ID, "1000.00", "2026-11-02", now)
	r1 := Receipt{ID: newID(), TenancyID: ten.ID, Amount: "1000.00", Currency: "USD", Method: ledger.MethodBank, ReceivedOn: "2026-11-02",
		Reference: "FNB 1", ContactID: &fx.contacts[0], ContactName: "Tia", LedgerEntryID: e1.ID, RecordedBy: &fx.user, RecordedByName: "Tester",
		RecordedAt: now, Allocations: []Allocation{{DueID: dues[1].ID, Amount: "50.00"}, {DueID: dues[0].ID, Amount: "950.00"}}}
	if err := s.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{ten.ID: StatusActive}, Ledger: &ledger.Change{Entries: []ledger.Entry{e1}},
		Receipts: []Receipt{r1}}); err != nil {
		t.Fatal(err)
	}
	recs, err := s.Receipts(ctx, ten.ID)
	if err != nil || len(recs) != 1 || recs[0].Amount != "1000.00" || recs[0].LedgerStatus != ledger.StatusUnverified || recs[0].LedgerAuthor == nil ||
		len(recs[0].Allocations) != 2 || recs[0].Allocations[0].DueID != dues[0].ID || recs[0].Reversed() || recs[0].ContactName != "Tia" {
		t.Fatalf("receipts: %+v %v", recs, err)
	}
	if e, err := books.Entry(ctx, e1.ID); err != nil || e.TenancyID == nil || *e.TenancyID != ten.ID || e.Type != ledger.TypeRentReceived {
		t.Fatalf("ledger entry: %+v %v", e, err)
	}
	d0, _ := s.Due(ctx, dues[0].ID)
	if st := d0.state(forever); st.status != DuePaid || len(d0.Allocations) != 1 || d0.Allocations[0].ReceivedOn != "2026-11-02" {
		t.Fatalf("paid due: %+v %+v", d0, st)
	}

	// A failure inside the transaction keeps neither the receipt nor its entry:
	// a line above the due's outstanding (October: 900.00 left) ...
	e2 := rentEntry(fx, ten.ID, "901.00", "2026-11-03", now)
	r2 := Receipt{ID: newID(), TenancyID: ten.ID, Amount: "901.00", Currency: "USD", Method: ledger.MethodCash, ReceivedOn: "2026-11-03",
		LedgerEntryID: e2.ID, RecordedAt: now, Allocations: []Allocation{{DueID: dues[1].ID, Amount: "901.00"}}}
	if err := s.Apply(ctx, Change{Ledger: &ledger.Change{Entries: []ledger.Entry{e2}}, Receipts: []Receipt{r2}}); !errors.Is(err, ErrDueChanged) {
		t.Fatalf("over-allocation: %v", err)
	}
	// ... and a due of another tenancy.
	r2.Allocations = []Allocation{{DueID: foreign[0].ID, Amount: "901.00"}}
	if err := s.Apply(ctx, Change{Ledger: &ledger.Change{Entries: []ledger.Entry{e2}}, Receipts: []Receipt{r2}}); !errors.Is(err, ErrDueChanged) {
		t.Fatalf("another tenancy's due: %v", err)
	}
	if _, err := books.Entry(ctx, e2.ID); !errors.Is(err, ledger.ErrEntryNotFound) {
		t.Fatalf("the refused receipt's ledger entry was kept: %v", err)
	}
	if recs, _ := s.Receipts(ctx, ten.ID); len(recs) != 1 {
		t.Fatalf("the refused receipt was kept: %d", len(recs))
	}

	// Waive November; twice is refused.
	w := &Waiver{DueID: dues[2].ID, At: now, By: &fx.user, ByName: "Tester", Reason: "Storm damage"}
	if err := s.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{ten.ID: StatusActive}, Waive: w}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, Change{Waive: w}); !errors.Is(err, ErrDueChanged) {
		t.Fatalf("waived twice: %v", err)
	}
	if err := s.Apply(ctx, Change{Waive: &Waiver{DueID: dues[0].ID, At: now, Reason: "paid"}}); !errors.Is(err, ErrDueChanged) {
		t.Fatalf("a paid due waived: %v", err)
	}
	d2, _ := s.Due(ctx, dues[2].ID)
	if d2.WaivedAt == nil || d2.WaiveReason != "Storm damage" || d2.WaivedByName != "Tester" || d2.state(forever).status != DueWaived {
		t.Fatalf("waived: %+v", d2)
	}
	// Removing dues skips those with an allocation or a waiver.
	if err := s.Apply(ctx, Change{DeleteDues: []uuid.UUID{dues[0].ID, dues[2].ID, dues[3].ID}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Dues(ctx, []uuid.UUID{ten.ID}); len(got) != 3 || got[2].ID != dues[2].ID {
		t.Fatalf("after removal: %+v", got)
	}

	// A ledger reversal of the entry marks the receipt reversed in its own transaction.
	orig, rev := e1.ID, newID()
	reversal := e1
	reversal.ID, reversal.Type, reversal.Direction, reversal.ReversalOf, reversal.EffectiveDate = rev, ledger.TypeReversal, ledger.Out, &orig, "2026-11-05"
	reversal.Category, reversal.Reason = "rent", "Bounced"
	if err := books.Apply(ctx, ledger.Change{NotReversed: []uuid.UUID{orig}, Reversals: []ledger.Entry{reversal},
		Links: []ledger.LinkSet{{EntryID: orig, ReversedBy: &rev}}}); err != nil {
		t.Fatal(err)
	}
	recs, _ = s.Receipts(ctx, ten.ID)
	if len(recs) != 1 || !recs[0].Reversed() || recs[0].ReversalEntryID == nil || *recs[0].ReversalEntryID != rev || recs[0].ReversedOn != "2026-11-05" {
		t.Fatalf("reversed receipt: %+v", recs)
	}
	d0, _ = s.Due(ctx, dues[0].ID)
	if st := d0.state(forever); st.status != DueDue || d0.Allocations[0].ReversedOn != "2026-11-05" {
		t.Fatalf("the allocation still counts: %+v %+v", d0, st)
	}
	if st := d0.state("2026-11-04"); st.status != DuePaid {
		t.Fatalf("as of before the reversal: %+v", st)
	}
	// The due may be paid again.
	e3 := rentEntry(fx, ten.ID, "950.00", "2026-11-06", now)
	r3 := Receipt{ID: newID(), TenancyID: ten.ID, Amount: "950.00", Currency: "USD", Method: ledger.MethodBank, ReceivedOn: "2026-11-06",
		LedgerEntryID: e3.ID, RecordedBy: &fx.user, RecordedByName: "Tester", RecordedAt: now, Allocations: []Allocation{{DueID: dues[0].ID, Amount: "950.00"}}}
	if err := s.Apply(ctx, Change{Ledger: &ledger.Change{Entries: []ledger.Entry{e3}}, Receipts: []Receipt{r3}}); err != nil {
		t.Fatal(err)
	}

	// Account deletion unlinks who recorded and who waived.
	forget(fx.user)
	recs, _ = s.Receipts(ctx, ten.ID)
	d2, _ = s.Due(ctx, dues[2].ID)
	if recs[0].RecordedBy != nil || recs[0].RecordedByName != "Tester" || d2.WaivedBy != nil {
		t.Fatalf("not unlinked: %+v %+v", recs[0], d2)
	}
}

func containsTenancy(list []Tenancy, id uuid.UUID) bool {
	for _, t := range list {
		if t.ID == id {
			return true
		}
	}
	return false
}

func TestMemoryRentStoreContract(t *testing.T) {
	s := NewMemoryStore()
	rentStoreContract(t, s, ledger.NewMemoryStore(), newFixtures(), func(u uuid.UUID) { s.ForgetUser(u, "Deleted account") })
}

func TestPostgresRentStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	fx := newFixtures()
	seedRows(t, pool, fx)
	rentStoreContract(t, NewPostgresStore(pool), ledger.NewPostgresStore(pool), fx, func(u uuid.UUID) { forgetTx(t, pool, u) })
}

func forgetTx(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, user, "Deleted account"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// The database's own rent guards (migration 0036).
func TestPostgresRentGuards(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := newFixtures()
	seedRows(t, pool, fx)
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	book := OrganisationBook(fx.org)
	prop := Property{ID: newID(), Book: book, Name: "Cottage", Address: "1 Lane", MarketCode: "ZW", CreatedAt: now, UpdatedAt: now}
	ten := Tenancy{ID: newID(), Book: book, PropertyID: prop.ID, RentAmount: "700.00", RentCurrency: "USD", RentFrequency: FrequencyMonthly,
		RentDueDay: 5, StartDate: "2026-09-01", Status: StatusActive, ActivatedAt: &now, CreatedAt: now, UpdatedAt: now,
		Parties: []Party{{ContactID: fx.contacts[0], Role: RoleTenant, Name: "Tia"}}}
	due := Due{ID: newID(), TenancyID: ten.ID, Sequence: 1, DueDate: "2026-09-05", Amount: "700.00", Currency: "USD", CreatedAt: now}
	if err := s.Apply(ctx, Change{Properties: []Property{prop}, Tenancies: []Tenancy{ten}, Dues: []Due{due}}); err != nil {
		t.Fatal(err)
	}
	// tenancy_id on a ledger entry: only rent received and its reversals.
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_entries (id, organisation_id, type, direction, method, amount, currency, effective_date, tenancy_id)
		VALUES ($1, $2, 'EXPENSE', 'OUT', 'CASH', 1, 'USD', '2026-11-01', $3)`, newID(), fx.org, ten.ID); err == nil {
		t.Fatal("an expense took a tenancy_id")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO ledger_entries (id, organisation_id, type, direction, method, amount, currency, effective_date, tenancy_id)
		VALUES ($1, $2, 'RENT_RECEIVED', 'IN', 'CASH', 1, 'USD', '2026-11-01', $3)`, newID(), fx.org, uuid.New()); err == nil {
		t.Fatal("a ledger entry referenced an unknown tenancy")
	}
	refused := map[string]string{
		"two dues on one date":         `INSERT INTO rent_dues (id, tenancy_id, sequence, due_date, amount, currency) VALUES (gen_random_uuid(), $1, 2, '2026-09-05', 1, 'USD')`,
		"a zero due":                   `INSERT INTO rent_dues (id, tenancy_id, sequence, due_date, amount, currency) VALUES (gen_random_uuid(), $1, 2, '2026-10-05', 0, 'USD')`,
		"a waiver without a reason":    `UPDATE rent_dues SET waived_at = now() WHERE tenancy_id = $1`,
		"a receipt without its entry":  `INSERT INTO rent_receipts (id, tenancy_id, amount, currency, method, received_on, ledger_entry_id) VALUES (gen_random_uuid(), $1, 1, 'USD', 'CASH', '2026-11-01', gen_random_uuid())`,
		"an allocation without a due":  `INSERT INTO rent_allocations (receipt_id, tenancy_id, due_id, amount) VALUES (gen_random_uuid(), $1, gen_random_uuid(), 1)`,
		"a tenancy with dues deleted":  `DELETE FROM tenancies WHERE id = $1`,
		"a reversed receipt half-done": `UPDATE rent_receipts SET reversed_at = now() WHERE tenancy_id = $1`,
	}
	// A receipt for the last guard.
	entry := rentEntry(fx, ten.ID, "700.00", "2026-11-01", now)
	if err := s.Apply(ctx, Change{Ledger: &ledger.Change{Entries: []ledger.Entry{entry}}, Receipts: []Receipt{{ID: newID(), TenancyID: ten.ID,
		Amount: "700.00", Currency: "USD", Method: ledger.MethodCash, ReceivedOn: "2026-11-01", LedgerEntryID: entry.ID, RecordedAt: now,
		Allocations: []Allocation{{DueID: due.ID, Amount: "700.00"}}}}}); err != nil {
		t.Fatal(err)
	}
	for name, sql := range refused {
		if _, err := pool.Exec(ctx, sql, ten.ID); err == nil {
			t.Fatalf("%s: the database accepted it", name)
		}
	}
	var old bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'ledger_entries_tenancy_check')`).Scan(&old); err != nil || old {
		t.Fatalf("the CHECK that kept tenancy_id NULL is still there: %v %v", old, err)
	}
}
