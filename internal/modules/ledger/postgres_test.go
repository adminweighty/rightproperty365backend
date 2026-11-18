package ledger

import (
	"context"
	"errors"
	"os"
	"slices"
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
// Ledger rows can never be deleted: every run uses a fresh organisation.
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

// storeContract runs the same checks on both stores. org is an organisation
// and user a person who acts in it (real rows for Postgres).
func storeContract(t *testing.T, s Store, org, user uuid.UUID, audits func() int) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	book := OrganisationBook(org)
	mk := func(typ, dir, method, amount, cur, date string) Entry {
		return Entry{ID: newID(), Book: book, Type: typ, Direction: dir, Method: method, Amount: amount, Currency: cur, EffectiveDate: date,
			RecordedAt: now, AuthorUserID: &user, AuthorName: "Tester", Status: StatusUnverified, Description: "Sample " + typ}
	}
	if err := s.EnsureCategories(ctx, book, withClock(DefaultCategories(), now)); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureCategories(ctx, book, withClock(DefaultCategories(), now)); err != nil {
		t.Fatal("ensure is idempotent: ", err)
	}
	open := mk(TypeOpeningBalance, In, MethodCash, "100.00", "USD", "2026-10-01")
	rent := mk(TypeRentReceived, In, MethodBank, "700.00", "USD", "2026-11-02")
	rent.Category, rent.Branch, rent.Reference = "rent", "Harare", "RCPT-1"
	jpy := mk(TypeIncome, In, MethodCash, "5000", "JPY", "2026-11-03")
	kwd := mk(TypeExpense, Out, MethodBank, "1.250", "KWD", "2026-11-04")
	recv := mk(TypeReceivable, In, "", "300.00", "USD", "2026-10-15")
	recv.Category = "rent"
	audit := orgaudit.New(org, user, "Tester", orgaudit.ActionLedgerEntryCreated, orgaudit.TargetLedgerEntry, rent.ID, Label(rent), nil, entryAudit(rent), now)
	before := audits()
	if err := s.Apply(ctx, Change{Entries: []Entry{open, rent, jpy, kwd, recv}, Audit: []orgaudit.Event{audit},
		Notes: []Note{{ID: newID(), EntryID: rent.ID, Text: "Paid by EFT", AuthorUserID: &user, AuthorName: "Tester", CreatedAt: now}}}); err != nil {
		t.Fatal(err)
	}
	if audits()-before != 1 {
		t.Fatal("audit not written")
	}
	got, err := s.Entry(ctx, kwd.ID)
	if err != nil || got.Amount != "1.250" || got.Currency != "KWD" || got.EffectiveDate != "2026-11-04" || got.Method != MethodBank {
		t.Fatalf("kwd: %+v %v", got, err)
	}
	if got, _ := s.Entry(ctx, jpy.ID); got.Amount != "5000" {
		t.Fatalf("jpy: %s", got.Amount)
	}
	if got, _ := s.Entry(ctx, rent.ID); got.NoteCount != 1 || got.Branch != "Harare" || got.Category != "rent" {
		t.Fatalf("rent: %+v", got)
	}
	if _, err := s.Entry(ctx, uuid.New()); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	// Opening balance once (case-insensitive branch).
	dup := mk(TypeOpeningBalance, In, MethodCash, "1.00", "USD", "2026-10-01")
	if err := s.Apply(ctx, Change{Entries: []Entry{dup}}); !errors.Is(err, ErrOpeningBalanceExists) {
		t.Fatalf("second opening: %v", err)
	}
	if e, ok, err := s.OpeningBalance(ctx, book, "USD", MethodCash, ""); err != nil || !ok || e.ID != open.ID {
		t.Fatalf("opening lookup: %v %v", ok, err)
	}
	// Transfer pair: the first leg gets its pair once the second exists.
	out := mk(TypeTransfer, Out, MethodCash, "20.00", "USD", "2026-11-05")
	in := mk(TypeTransfer, In, MethodBank, "20.00", "USD", "2026-11-05")
	in.TransferPair = &out.ID
	if err := s.Apply(ctx, Change{Entries: []Entry{out, in}, PostLinks: []LinkSet{{EntryID: out.ID, TransferPair: &in.ID}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Entry(ctx, out.ID); got.TransferPair == nil || *got.TransferPair != in.ID {
		t.Fatalf("pair: %+v", got.TransferPair)
	}
	// Reversal: the original is locked and marked once.
	rev := mk(TypeReversal, Out, MethodCash, "100.00", "USD", "2026-10-01")
	rev.ReversalOf, rev.Reason = &open.ID, "wrong"
	if err := s.Apply(ctx, Change{NotReversed: []uuid.UUID{open.ID}, Reversals: []Entry{rev}, Links: []LinkSet{{EntryID: open.ID, ReversedBy: &rev.ID}}}); err != nil {
		t.Fatal(err)
	}
	rev2 := mk(TypeReversal, Out, MethodCash, "100.00", "USD", "2026-10-01")
	rev2.ReversalOf = &open.ID
	if err := s.Apply(ctx, Change{NotReversed: []uuid.UUID{open.ID}, Reversals: []Entry{rev2}}); !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("second reversal: %v", err)
	}
	// Now a new opening balance is allowed (the index ignores reversed ones).
	if err := s.Apply(ctx, Change{Entries: []Entry{dup}}); err != nil {
		t.Fatalf("opening after reversal: %v", err)
	}
	// Settlement: at most the outstanding.
	pay := mk(TypeRentReceived, In, MethodCash, "200.00", "USD", "2026-11-06")
	pay.Settles = &recv.ID
	if err := s.Apply(ctx, Change{Entries: []Entry{pay}}); err != nil {
		t.Fatal(err)
	}
	over := mk(TypeRentReceived, In, MethodCash, "100.01", "USD", "2026-11-07")
	over.Settles = &recv.ID
	if err := s.Apply(ctx, Change{Entries: []Entry{over}}); !errors.Is(err, ErrOverSettled) {
		t.Fatalf("over-settled: %v", err)
	}
	settled, err := s.Settled(ctx, book, []uuid.UUID{recv.ID})
	if err != nil || len(settled[recv.ID]) != 1 {
		t.Fatalf("settled: %v %v", settled, err)
	}
	rel, err := s.Related(ctx, book, open.ID)
	if err != nil || len(rel) != 1 || rel[0].ID != rev.ID {
		t.Fatalf("related: %v %v", rel, err)
	}
	// Reconcile and back.
	if err := s.Apply(ctx, Change{Reconcile: &Reconciliation{EntryID: rent.ID, Status: StatusReconciled, At: now, By: &user, ByName: "Tester", Note: "ok"}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Entry(ctx, rent.ID); got.Status != StatusReconciled || got.ReconciledAt == nil || got.ReconcileNote != "ok" {
		t.Fatalf("reconciled: %+v", got)
	}
	// Lists and filters.
	all, total, err := s.Entries(ctx, book, Filter{})
	if err != nil || total != 10 || len(all) != 10 {
		t.Fatalf("all: %d %v", total, err)
	}
	if all[0].EffectiveDate < all[len(all)-1].EffectiveDate {
		t.Fatal("default order is newest first")
	}
	asc, _, _ := s.Entries(ctx, book, Filter{Ascending: true, Limit: 2, Offset: 1})
	if len(asc) != 2 || asc[0].EffectiveDate > asc[1].EffectiveDate {
		t.Fatalf("asc page: %+v", asc)
	}
	branch := "harare"
	checks := map[string]struct {
		f    Filter
		want int
	}{
		"currency":         {Filter{Currency: "USD"}, 8},
		"type":             {Filter{Type: TypeTransfer}, 2},
		"category":         {Filter{Category: "rent"}, 2},
		"method":           {Filter{Method: MethodBank}, 3},
		"status":           {Filter{Status: StatusReconciled}, 1},
		"from-to":          {Filter{From: "2026-11-02", To: "2026-11-04"}, 3},
		"branch":           {Filter{Branch: &branch}, 1},
		"q":                {Filter{Q: "rcpt"}, 1},
		"exclude reversed": {Filter{ExcludeReversed: true}, 8},
	}
	for name, c := range checks {
		if _, n, err := s.Entries(ctx, book, c.f); err != nil || n != c.want {
			t.Fatalf("%s: %d, want %d (%v)", name, n, c.want, err)
		}
	}
	if list, _ := s.Branches(ctx, book); !slices.Equal(list, []string{"Harare"}) {
		t.Fatalf("branches: %v", list)
	}
	if _, n, _ := s.Entries(ctx, OrganisationBook(uuid.New()), Filter{}); n != 0 {
		t.Fatal("another book sees entries")
	}
	// Notes and attachments.
	att := Attachment{ID: newID(), EntryID: rent.ID, FileName: "r.pdf", ContentType: "application/pdf", Size: 10,
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000", ObjectKey: "ledger/" + uuid.NewString()[:8],
		UploadedBy: &user, UploadedByName: "Tester", UploadedAt: now}
	if err := s.Apply(ctx, Change{Attachments: []Attachment{att}}); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.Attachments(ctx, rent.ID); len(list) != 1 || list[0].ObjectKey != att.ObjectKey {
		t.Fatalf("attachments: %+v", list)
	}
	if list, _ := s.Notes(ctx, rent.ID); len(list) != 1 || list[0].Text != "Paid by EFT" {
		t.Fatalf("notes: %+v", list)
	}
	// Categories.
	cats, err := s.Categories(ctx, book)
	if err != nil || len(cats) != 10 || cats[0].Code != "rent" || !cats[0].Used || cats[3].Used {
		t.Fatalf("categories: %+v %v", cats, err)
	}
	c := Category{ID: newID(), Book: book, Code: "office", Label: "Office", Kind: KindOut, Active: true, SortOrder: 200, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateCategory(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateCategory(ctx, c); !errors.Is(err, ErrCategoryExists) {
		t.Fatalf("duplicate category: %v", err)
	}
	c.Active, c.Label = false, "Office costs"
	if err := s.UpdateCategory(ctx, c); err != nil {
		t.Fatal(err)
	}
	cats, _ = s.Categories(ctx, book)
	if last := cats[len(cats)-1]; last.Code != "office" || last.Active || last.Label != "Office costs" {
		t.Fatalf("updated: %+v", last)
	}
	c.Code = "nope"
	if err := s.UpdateCategory(ctx, c); !errors.Is(err, ErrCategoryNotFound) {
		t.Fatalf("unknown category: %v", err)
	}
}

func TestMemoryStoreContract(t *testing.T) {
	s := NewMemoryStore()
	log := &orgaudit.MemoryLog{}
	s.SetAuditSink(log.Append)
	storeContract(t, s, uuid.New(), uuid.New(), func() int { return len(log.All()) })
}

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	org, user := seedOrg(t, pool)
	storeContract(t, NewPostgresStore(pool), org, user, func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM organisation_audit_events WHERE organisation_id = $1`, org).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})
}

func seedOrg(t *testing.T, pool *pgxpool.Pool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	org, user := uuid.New(), uuid.New()
	suffix := org.String()[:8]
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
			[]any{org, "Ledger Test " + suffix}},
		{`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Ledger Tester', 'ACTIVE', now())`,
			[]any{user, "ledger." + suffix + "@example.test"}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return org, user
}

// The database itself refuses to change or delete a posted entry, and
// refuses GATEWAY_CONFIRMED without a gateway reference (migration 0033).
func TestPostgresGuards(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	org, user := seedOrg(t, pool)
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	e := Entry{ID: newID(), Book: OrganisationBook(org), Type: TypeRentReceived, Direction: In, Category: "rent", Method: MethodCash, Amount: "70.00",
		Currency: "USD", EffectiveDate: "2026-11-02", RecordedAt: now, AuthorUserID: &user, AuthorName: "Tester", Status: StatusUnverified}
	rev := Entry{ID: newID(), Book: e.Book, Type: TypeReversal, Direction: Out, Category: "rent", Method: MethodCash, Amount: "70.00",
		Currency: "USD", EffectiveDate: "2026-11-02", RecordedAt: now, AuthorUserID: &user, AuthorName: "Tester", Status: StatusUnverified,
		ReversalOf: &e.ID, Reason: "typo"}
	if err := s.Apply(ctx, Change{Entries: []Entry{e}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, Change{NotReversed: []uuid.UUID{e.ID}, Reversals: []Entry{rev}, Links: []LinkSet{{EntryID: e.ID, ReversedBy: &rev.ID}}}); err != nil {
		t.Fatal(err)
	}
	refused := map[string]string{
		"amount":         `UPDATE ledger_entries SET amount = 700 WHERE id = $1`,
		"currency":       `UPDATE ledger_entries SET currency = 'ZAR' WHERE id = $1`,
		"type":           `UPDATE ledger_entries SET type = 'INCOME' WHERE id = $1`,
		"direction":      `UPDATE ledger_entries SET direction = 'OUT' WHERE id = $1`,
		"effective_date": `UPDATE ledger_entries SET effective_date = '2026-01-01' WHERE id = $1`,
		"method":         `UPDATE ledger_entries SET method = 'BANK' WHERE id = $1`,
		"author":         `UPDATE ledger_entries SET author_name = 'Someone else' WHERE id = $1`,
		"reversed_by":    `UPDATE ledger_entries SET reversed_by = NULL WHERE id = $1`,
		"reversal_of":    `UPDATE ledger_entries SET reversal_of = NULL WHERE reversal_of = $1`,
		"delete":         `DELETE FROM ledger_entries WHERE id = $1`,
		"delete all":     `DELETE FROM ledger_entries WHERE organisation_id = (SELECT organisation_id FROM ledger_entries WHERE id = $1)`,
		"gateway status": `UPDATE ledger_entries SET status = 'GATEWAY_CONFIRMED' WHERE id = $1`,
	}
	for name, sql := range refused {
		if _, err := pool.Exec(ctx, sql, e.ID); err == nil {
			t.Fatalf("%s: the database accepted it", name)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `TRUNCATE ledger_entries CASCADE`); err == nil {
		t.Fatal("truncate accepted")
	}
	_ = tx.Rollback(ctx)
	if got, err := s.Entry(ctx, e.ID); err != nil || got.Amount != "70.00" || got.ReversedBy == nil || *got.ReversedBy != rev.ID {
		t.Fatalf("after the refused writes: %+v %v", got, err)
	}
	// GATEWAY_CONFIRMED needs a gateway reference (checked in a rolled-back transaction).
	gw := e
	gw.ID, gw.Status = newID(), StatusGatewayConfirmed
	if err := s.Apply(ctx, Change{Entries: []Entry{gw}}); err == nil {
		t.Fatal("GATEWAY_CONFIRMED without a gateway reference was stored")
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyTx(ctx, tx, Change{Entries: []Entry{gw}}); err == nil {
		t.Fatal("refused inside a transaction too")
	}
	_ = tx.Rollback(ctx)
	// What may change: reconciliation and the links.
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET status = 'RECONCILED', reconciled_at = now() WHERE id = $1`, e.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET contact_id = NULL WHERE id = $1`, e.ID); err != nil {
		t.Fatalf("link: %v", err)
	}
	// The set-once links may be set from NULL, never changed.
	other := Entry{ID: newID(), Book: e.Book, Type: TypeIncome, Direction: In, Category: "fees", Method: MethodCash, Amount: "1.00", Currency: "USD",
		EffectiveDate: "2026-11-03", RecordedAt: now, AuthorUserID: &user, AuthorName: "Tester", Status: StatusUnverified}
	if err := s.Apply(ctx, Change{Entries: []Entry{other}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET corrected_by = $2 WHERE id = $1`, other.ID, e.ID); err != nil {
		t.Fatalf("set once: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ledger_entries SET corrected_by = $2 WHERE id = $1`, other.ID, rev.ID); err == nil {
		t.Fatal("changed a set-once link")
	}
}
