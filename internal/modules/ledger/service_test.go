package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger/ledgertest"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// fakeLinks knows contacts, listings and leases per book.
type fakeLinks struct {
	contacts map[uuid.UUID]uuid.UUID // contact -> book ID
}

func (f fakeLinks) Contact(_ context.Context, book Book, id uuid.UUID) (ContactRef, bool, error) {
	if b, ok := f.contacts[id]; ok && b == book.ID() {
		return ContactRef{ID: id, Name: "Contact " + id.String()[:4]}, true, nil
	}
	return ContactRef{}, false, nil
}

func (f fakeLinks) Listing(context.Context, Book, uuid.UUID) (ListingRef, bool, error) {
	return ListingRef{}, false, nil
}

func (f fakeLinks) Lease(context.Context, Book, uuid.UUID) (LeaseRef, bool, error) {
	return LeaseRef{}, false, nil
}

type fixture struct {
	t                                                                         *testing.T
	svc                                                                       *Service
	store                                                                     *MemoryStore
	audit                                                                     *orgaudit.MemoryLog
	now                                                                       time.Time
	org                                                                       uuid.UUID
	other                                                                     uuid.UUID
	contact                                                                   uuid.UUID
	foreign                                                                   uuid.UUID
	owner, manager, agent, agent2, staff, outsider, consumer, admin, landlord *auth.Principal
}

func member(org uuid.UUID, role, name string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Name: name, Memberships: []auth.Membership{{OrganisationID: org, OrganisationKind: auth.KindAgency, Role: role}}}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, store: NewMemoryStore(), audit: &orgaudit.MemoryLog{}, now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC),
		org: uuid.New(), other: uuid.New(), contact: uuid.New(), foreign: uuid.New()}
	f.store.SetAuditSink(f.audit.Append)
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.svc = NewService(Deps{Store: f.store, Files: files, Links: fakeLinks{contacts: map[uuid.UUID]uuid.UUID{f.contact: f.org, f.foreign: f.other}},
		Currencies: func(context.Context) ([]string, error) {
			return []string{"USD", "ZAR", "JPY", "KWD", "EUR", "USD"}, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return f.now }})
	f.owner, f.manager = member(f.org, auth.RoleOwner, "Olive Owner"), member(f.org, auth.RoleManager, "Mo Manager")
	f.agent, f.agent2 = member(f.org, auth.RoleAgent, "Ada Agent"), member(f.org, auth.RoleAgent, "Abe Agent")
	f.staff, f.outsider = member(f.org, auth.RoleStaff, "Sam Staff"), member(f.other, auth.RoleOwner, "Other Owner")
	f.consumer = &auth.Principal{UserID: uuid.New(), AccountRoles: []auth.AccountRole{{Role: "CONSUMER", Status: auth.AccountRoleActive}}}
	f.admin = &auth.Principal{UserID: uuid.New(), SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformSuperAdmin}}}
	f.landlord = &auth.Principal{UserID: uuid.New(), Name: "Lan Lord", AccountRoles: []auth.AccountRole{{Role: auth.AccountRoleLandlord, Status: auth.AccountRoleActive}}}
	return f
}

func sp(s string) *string { return &s }

func entry(typ, method, amount, currency, date string) EntryInput {
	in := EntryInput{Type: typ, Amount: sp(amount), Currency: currency, EffectiveDate: date}
	if method != "" {
		in.Method = sp(method)
	}
	return in
}

func (f *fixture) post(p *auth.Principal, in EntryInput) Posted {
	f.t.Helper()
	out, err := f.svc.Create(context.Background(), p, f.org, in)
	if err != nil {
		f.t.Fatalf("create %s: %v", in.Type, fieldsOf(err))
	}
	return out
}

func fieldsOf(err error) any {
	var v *ValidationError
	if errors.As(err, &v) {
		return v.Fields
	}
	return err
}

func wantField(t *testing.T, err error, field string) {
	t.Helper()
	var v *ValidationError
	if !errors.As(err, &v) || v.Fields[field] == "" {
		t.Fatalf("want a %q field error, got %v", field, fieldsOf(err))
	}
}

// FR-027 acceptance: a corrected posted payment keeps its predecessor (and
// the reversal and the new entry), and the audit trail shows who did what.
func TestCorrectedPaymentKeepsPredecessorAndActor(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := entry(TypeRentReceived, MethodCash, "70", "USD", "2026-11-02")
	in.ContactID = &f.contact
	orig := f.post(f.agent, in).Entry
	if orig.Amount != "70.00" || orig.Category == nil || *orig.Category != "rent" || orig.Direction != In {
		t.Fatalf("posted: %+v", orig)
	}
	fixed := entry(TypeRentReceived, MethodCash, "700", "USD", "2026-11-02")
	fixed.ContactID = &f.contact
	out, err := f.svc.Correct(ctx, f.owner, f.org, orig.ID, CorrectInput{EntryInput: fixed, Reason: "Typed 70 instead of 700"})
	if err != nil {
		t.Fatal(fieldsOf(err))
	}
	if len(out.Created) != 2 || len(out.Updated) != 1 {
		t.Fatalf("created %d updated %d", len(out.Created), len(out.Updated))
	}
	rev, next, was := out.Created[0], out.Entry, out.Updated[0]
	if rev.Type != TypeReversal || rev.Direction != Out || rev.Amount != "70.00" || rev.ReversalOf == nil || *rev.ReversalOf != orig.ID ||
		rev.Reason == nil || rev.EffectiveDate != orig.EffectiveDate {
		t.Fatalf("reversal: %+v", rev)
	}
	if next.Amount != "700.00" || next.CorrectionOf == nil || *next.CorrectionOf != orig.ID || next.ID != out.Created[1].ID {
		t.Fatalf("correction: %+v", next)
	}
	if was.ID != orig.ID || !was.IsReversed || *was.ReversedBy != rev.ID || *was.CorrectedBy != next.ID || was.Amount != "70.00" {
		t.Fatalf("original: %+v", was)
	}
	all, err := f.svc.List(ctx, f.staff, f.org, Filter{}, 1, 50)
	if err != nil || all.Total != 3 {
		t.Fatalf("all three stay: %d %v", all.Total, err)
	}
	live, _ := f.svc.List(ctx, f.staff, f.org, Filter{ExcludeReversed: true}, 1, 50)
	if live.Total != 1 || live.Items[0].ID != next.ID {
		t.Fatalf("live: %+v", live.Items)
	}
	d, err := f.svc.Get(ctx, f.staff, f.org, orig.ID)
	if err != nil || len(d.Related) != 2 {
		t.Fatalf("related of the original: %+v %v", d.Related, err)
	}
	// The audit trail: who recorded, who corrected.
	events, _ := f.audit.List(f.org, 0, 0)
	find := func(action string, target uuid.UUID) orgaudit.Event {
		for _, e := range events {
			if e.Action == action && e.TargetID == target {
				return e
			}
		}
		t.Fatalf("no %s for %s in %+v", action, target, events)
		return orgaudit.Event{}
	}
	created := find(orgaudit.ActionLedgerEntryCreated, orig.ID)
	if *created.ActorUserID != f.agent.UserID || created.ActorName != "Ada Agent" {
		t.Fatalf("created by %+v", created)
	}
	corrected := find(orgaudit.ActionLedgerEntryCorrected, orig.ID)
	if *corrected.ActorUserID != f.owner.UserID || corrected.ActorName != "Olive Owner" || corrected.Before["amount"] != "70.00" ||
		corrected.After["corrected_by"] != next.ID.String() || corrected.After["reversal_id"] != rev.ID.String() {
		t.Fatalf("corrected: %+v", corrected)
	}
	if e := find(orgaudit.ActionLedgerEntryCreated, next.ID); *e.ActorUserID != f.owner.UserID {
		t.Fatalf("new entry by %+v", e)
	}
	// Once reversed, never again; a reversal is not reversible.
	_, err = f.svc.Reverse(ctx, f.owner, f.org, orig.ID, ReverseInput{Reason: "again"})
	var re *ReversedError
	if !errors.As(err, &re) || re.ReversedBy != rev.ID {
		t.Fatalf("reverse twice: %v", err)
	}
	if _, err := f.svc.Correct(ctx, f.owner, f.org, orig.ID, CorrectInput{EntryInput: fixed, Reason: "again"}); !errors.Is(err, ErrAlreadyReversed) {
		t.Fatalf("correct twice: %v", err)
	}
	if _, err := f.svc.Reverse(ctx, f.owner, f.org, rev.ID, ReverseInput{Reason: "undo"}); !errors.Is(err, ErrNotReversible) {
		t.Fatalf("reverse a reversal: %v", err)
	}
	// A reason is required.
	other := f.post(f.owner, entry(TypeIncome, MethodBank, "5", "USD", "2026-11-03"))
	_ = other
}

// FR-028: GATEWAY_CONFIRMED can never be set; manual entries are
// USER_RECORDED_UNVERIFIED; RECONCILED is the organisation's own mark.
func TestStatusNeverClaimsVerification(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := entry(TypeRentReceived, MethodBank, "100", "USD", "2026-11-01")
	in.Status = sp(StatusGatewayConfirmed)
	_, err := f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "status")
	in.Status = sp(StatusReconciled)
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "status")
	in.Status, in.GatewayReference = nil, sp("gw_123")
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "gateway_reference")
	in.GatewayReference = nil
	e := f.post(f.agent, in).Entry
	if e.Status != StatusUnverified || e.StatusLabel != "Recorded by you, not verified" || e.Reconciliation != nil {
		t.Fatalf("manual entry: %s %q", e.Status, e.StatusLabel)
	}
	opts, err := f.svc.Options(ctx, f.staff, f.org)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range opts.Statuses {
		if s.Settable {
			t.Fatalf("status %s is settable", s.Code)
		}
	}
	if slices.ContainsFunc(opts.Types, func(o TypeOption) bool { return o.Code == TypeReversal }) {
		t.Fatal("REVERSAL is offered")
	}
	yes := true
	got, err := f.svc.Reconcile(ctx, f.manager, f.org, e.ID, ReconcileInput{Reconciled: &yes, Note: sp("Matched to the statement")})
	if err != nil || got.Status != StatusReconciled || got.StatusLabel != "Marked reconciled by your team" || got.Reconciliation == nil ||
		*got.Reconciliation.By.UserID != f.manager.UserID {
		t.Fatalf("reconciled: %+v %v", got, err)
	}
	for _, label := range []string{StatusLabel(StatusUnverified), StatusLabel(StatusReconciled)} {
		if strings.Contains(strings.ToLower(label), "bank") || strings.Contains(strings.ToLower(label), "verified ") {
			t.Fatalf("label %q sounds like a bank verification", label)
		}
	}
	no := false
	got, _ = f.svc.Reconcile(ctx, f.manager, f.org, e.ID, ReconcileInput{Reconciled: &no})
	if got.Status != StatusUnverified || got.Reconciliation != nil {
		t.Fatalf("unreconciled: %+v", got)
	}
	events, _ := f.audit.List(f.org, 0, 0)
	n := 0
	for _, ev := range events {
		if ev.Action == orgaudit.ActionLedgerEntryReconciled || ev.Action == orgaudit.ActionLedgerEntryUnreconciled {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d reconcile audit events", n)
	}
	// The store refuses a gateway-confirmed entry outright (the database CHECK's mirror).
	bad := Entry{ID: newID(), Book: OrganisationBook(f.org), Type: TypeIncome, Direction: In, Method: MethodBank, Amount: "1.00", Currency: "USD",
		EffectiveDate: "2026-11-01", Status: StatusGatewayConfirmed}
	if err := f.store.Apply(ctx, Change{Entries: []Entry{bad}}); err == nil {
		t.Fatal("the store accepted GATEWAY_CONFIRMED")
	}
}

// FR-029 acceptance: a book with USD and ZAR entries answers per-currency
// totals only, in every report.
func TestReportsNeverAddUSDAndZAR(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	opening := entry(TypeOpeningBalance, MethodCash, "10", "USD", "2026-10-01")
	opening.Direction = sp(In)
	f.post(f.owner, opening)
	zopen := entry(TypeOpeningBalance, MethodCash, "2000", "ZAR", "2026-10-01")
	zopen.Direction = sp(In)
	f.post(f.owner, zopen)
	for _, in := range []EntryInput{
		entry(TypeRentReceived, MethodBank, "100", "USD", "2026-11-03"),
		entry(TypeRentReceived, MethodBank, "30000", "ZAR", "2026-11-03"),
	} {
		in.ContactID = &f.contact
		f.post(f.agent, in)
	}
	exp := entry(TypeExpense, MethodCash, "1", "USD", "2026-11-04")
	exp.Category = sp("utilities")
	f.post(f.agent, exp)
	zexp := entry(TypeExpense, MethodCash, "400", "ZAR", "2026-11-04")
	zexp.Category = sp("utilities")
	f.post(f.agent, zexp)
	for cur, amt := range map[string]string{"USD": "7", "ZAR": "50000"} {
		r := entry(TypeReceivable, "", amt, cur, "2026-10-01")
		r.Category, r.ContactID = sp("rent"), &f.contact
		f.post(f.owner, r)
	}
	q := ReportQuery{From: "2026-11-01", To: "2026-11-30"}
	reports := map[string]func() (any, error){
		"cashbook":        func() (any, error) { return f.svc.Cashbook(ctx, f.staff, f.org, q) },
		"cashbook method": func() (any, error) { q := q; q.By = "method"; return f.svc.Cashbook(ctx, f.staff, f.org, q) },
		"cashbook branch": func() (any, error) { q := q; q.By = "branch"; return f.svc.Cashbook(ctx, f.staff, f.org, q) },
		"categories":      func() (any, error) { return f.svc.CategoriesReport(ctx, f.staff, f.org, q) },
		"statement": func() (any, error) {
			q := q
			q.ContactID = &f.contact
			return f.svc.Statement(ctx, f.staff, f.org, q)
		},
		"receivables": func() (any, error) { return f.svc.Receivables(ctx, f.staff, f.org, ReportQuery{AsOf: "2026-11-30"}) },
		"entries":     func() (any, error) { return f.svc.List(ctx, f.staff, f.org, Filter{}, 1, 100) },
	}
	for name, run := range reports {
		out, err := run()
		if err != nil {
			t.Fatalf("%s: %v", name, fieldsOf(err))
		}
		raw, _ := json.Marshal(out)
		if name != "entries" && !strings.Contains(string(raw), `"currency":"ZAR"`) {
			t.Fatalf("%s has no ZAR section: %s", name, raw)
		}
		if err := ledgertest.PerCurrencyProblem(raw); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Cashbook arithmetic per currency: opening + in - out = closing; a
// reversal counts against its original's column.
func TestCashbookArithmetic(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	op := entry(TypeOpeningBalance, MethodCash, "1000.00", "USD", "2026-10-01")
	op.Direction = sp(In)
	f.post(f.owner, op)
	f.post(f.owner, entry(TypeIncome, MethodCash, "50", "USD", "2026-10-15")) // before the period: opening
	in1 := f.post(f.owner, entry(TypeRentReceived, MethodCash, "200.10", "USD", "2026-11-05")).Entry
	ex := entry(TypeExpense, MethodBank, "30.05", "USD", "2026-11-06")
	ex.Category = sp("maintenance")
	f.post(f.owner, ex)
	if _, err := f.svc.Reverse(ctx, f.owner, f.org, in1.ID, ReverseInput{Reason: "bounced"}); err != nil {
		t.Fatal(err)
	}
	f.post(f.owner, entry(TypeRentReceived, MethodCash, "5000", "JPY", "2026-11-07"))
	cb, err := f.svc.Cashbook(ctx, f.staff, f.org, ReportQuery{From: "2026-11-01", To: "2026-11-30", By: "method"})
	if err != nil {
		t.Fatal(fieldsOf(err))
	}
	if len(cb.Currencies) != 2 || cb.Currencies[0].Currency != "JPY" || cb.Currencies[1].Currency != "USD" {
		t.Fatalf("currencies: %+v", cb.Currencies)
	}
	jpy, usd := cb.Currencies[0], cb.Currencies[1]
	if jpy.OpeningBalance != "0" || jpy.In != "5000" || jpy.ClosingBalance != "5000" || jpy.MinorUnit != 0 {
		t.Fatalf("JPY: %+v", jpy)
	}
	// In: 200.10 - 200.10 (reversal) = 0.00; out 30.05; opening 1050.00; closing 1019.95.
	if usd.OpeningBalance != "1050.00" || usd.In != "0.00" || usd.Out != "30.05" || usd.Net != "-30.05" || usd.ClosingBalance != "1019.95" ||
		usd.EntryCount != 3 {
		t.Fatalf("USD: %+v", usd)
	}
	for _, c := range cb.Currencies {
		o, i, out, cl := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
		o.SetString(c.OpeningBalance)
		i.SetString(c.In)
		out.SetString(c.Out)
		cl.SetString(c.ClosingBalance)
		if new(big.Rat).Sub(new(big.Rat).Add(o, i), out).Cmp(cl) != 0 {
			t.Fatalf("%s: %s + %s - %s != %s", c.Currency, c.OpeningBalance, c.In, c.Out, c.ClosingBalance)
		}
		if last := c.Entries[len(c.Entries)-1]; last.RunningBalance != c.ClosingBalance {
			t.Fatalf("%s: running %s closing %s", c.Currency, last.RunningBalance, c.ClosingBalance)
		}
	}
	// The reversal shows its negative amount in the column of the original.
	rev := usd.Entries[1]
	if rev.Entry.Type != TypeReversal || rev.In != "-200.10" || rev.Out != "0.00" {
		t.Fatalf("reversal line: %+v", rev)
	}
	// By method: CASH 1050 opening, BANK -30.05.
	if len(usd.Groups) != 2 || *usd.Groups[0].Key != MethodCash || usd.Groups[0].ClosingBalance != "1050.00" || usd.Groups[1].ClosingBalance != "-30.05" {
		t.Fatalf("groups: %+v", usd.Groups)
	}
	cats, _ := f.svc.CategoriesReport(ctx, f.staff, f.org, ReportQuery{From: "2026-11-01", To: "2026-11-30", Currency: "USD"})
	if len(cats.Currencies) != 1 || cats.Currencies[0].Total.Out != "30.05" || cats.Currencies[0].Total.In != "0.00" {
		t.Fatalf("categories: %+v", cats)
	}
}

// A transfer is an OUT / IN pair linked both ways; reversing one leg
// reverses both.
func TestTransferPairs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := entry(TypeTransfer, MethodCash, "50", "USD", "2026-11-05")
	_, err := f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "transfer_to")
	in.TransferTo = &TransferTarget{Method: sp(MethodCash)}
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "transfer_to")
	in.Direction = sp(In)
	in.TransferTo = &TransferTarget{Method: sp(MethodBank)}
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "direction")
	in.Direction = nil
	out := f.post(f.agent, in)
	if len(out.Created) != 2 {
		t.Fatalf("%d legs", len(out.Created))
	}
	a, b := out.Created[0], out.Created[1]
	if a.Direction != Out || *a.Method != MethodCash || b.Direction != In || *b.Method != MethodBank || *a.TransferPair != b.ID || *b.TransferPair != a.ID {
		t.Fatalf("legs: %+v / %+v", a, b)
	}
	// Between branches, same method.
	br := entry(TypeTransfer, MethodBank, "10", "USD", "2026-11-05")
	br.Branch, br.TransferTo = sp("Harare"), &TransferTarget{Branch: sp("Bulawayo")}
	legs := f.post(f.owner, br).Created
	if *legs[0].Branch != "Harare" || *legs[1].Branch != "Bulawayo" || *legs[1].Method != MethodBank {
		t.Fatalf("branch legs: %+v", legs)
	}
	cb, _ := f.svc.Cashbook(ctx, f.staff, f.org, ReportQuery{From: "2026-11-01", To: "2026-11-30", By: "method"})
	usd := cb.Currencies[0]
	if usd.ClosingBalance != "0.00" || usd.In != "60.00" || usd.Out != "60.00" || *usd.Groups[0].Key != MethodCash || usd.Groups[0].ClosingBalance != "-50.00" {
		t.Fatalf("cashbook: %+v", usd)
	}
	rev, err := f.svc.Reverse(ctx, f.agent, f.org, b.ID, ReverseInput{Reason: "wrong account"})
	if err != nil {
		t.Fatal(fieldsOf(err))
	}
	if len(rev.Created) != 2 || len(rev.Updated) != 2 || !rev.Updated[0].IsReversed || !rev.Updated[1].IsReversed || *rev.Entry.ReversalOf != b.ID {
		t.Fatalf("reversed pair: %+v", rev)
	}
}

// One live opening balance per currency, method and branch.
func TestOpeningBalanceOnce(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	mk := func(method, cur, branch string) EntryInput {
		in := entry(TypeOpeningBalance, method, "100", cur, "2026-10-01")
		in.Direction = sp(In)
		if branch != "" {
			in.Branch = sp(branch)
		}
		return in
	}
	first := f.post(f.owner, mk(MethodCash, "USD", "")).Entry
	_, err := f.svc.Create(ctx, f.owner, f.org, mk(MethodCash, "USD", ""))
	var oe *OpeningExistsError
	if !errors.As(err, &oe) || oe.Existing != first.ID || oe.Currency != "USD" || oe.Method != MethodCash {
		t.Fatalf("second opening: %v", err)
	}
	f.post(f.owner, mk(MethodBank, "USD", ""))
	f.post(f.owner, mk(MethodCash, "ZAR", ""))
	hre := f.post(f.owner, mk(MethodCash, "USD", "Harare")).Entry
	if _, err := f.svc.Create(ctx, f.owner, f.org, mk(MethodCash, "USD", "harare")); !errors.As(err, &oe) || oe.Existing != hre.ID {
		t.Fatalf("branch is case-insensitive: %v", err)
	}
	noDir := mk(MethodCash, "EUR", "")
	noDir.Direction = nil
	_, err = f.svc.Create(ctx, f.owner, f.org, noDir)
	wantField(t, err, "direction")
	// Correct it (reverse + new in one go), or reverse it and post again.
	fixed := mk(MethodCash, "USD", "")
	fixed.Amount = sp("150")
	if _, err := f.svc.Correct(ctx, f.owner, f.org, first.ID, CorrectInput{EntryInput: fixed, Reason: "counted again"}); err != nil {
		t.Fatalf("correct an opening balance: %v", fieldsOf(err))
	}
	if _, err := f.svc.Create(ctx, f.owner, f.org, mk(MethodCash, "USD", "")); !errors.As(err, &oe) {
		t.Fatalf("the corrected one is live: %v", err)
	}
	if _, err := f.svc.Reverse(ctx, f.owner, f.org, hre.ID, ReverseInput{Reason: "wrong"}); err != nil {
		t.Fatal(err)
	}
	f.post(f.owner, mk(MethodCash, "USD", "Harare"))
	// The store enforces it too (as the database's unique index).
	dup := Entry{ID: newID(), Book: OrganisationBook(f.org), Type: TypeOpeningBalance, Direction: In, Method: MethodBank, Amount: "1.00",
		Currency: "USD", EffectiveDate: "2026-10-01", Status: StatusUnverified}
	if err := f.store.Apply(ctx, Change{Entries: []Entry{dup}}); !errors.Is(err, ErrOpeningBalanceExists) {
		t.Fatalf("store: %v", err)
	}
}

// Ageing buckets of open receivables, per currency.
func TestReceivablesAgeing(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	recv := func(amount, cur, date string) EntryView {
		in := entry(TypeReceivable, "", amount, cur, date)
		in.Category, in.ContactID = sp("rent"), &f.contact
		return f.post(f.owner, in).Entry
	}
	// As of 2026-11-30.
	a := recv("100", "USD", "2026-11-20")    // 10 days: 0-30
	b := recv("200", "USD", "2026-10-15")    // 46 days: 31-60
	recv("300", "USD", "2026-09-15")         // 76 days: 61-90
	recv("400", "USD", "2026-07-01")         // 152 days: 90+
	recv("50", "USD", "2026-12-15")          // not yet due on the date: left out
	recv("1000", "ZAR", "2026-10-31")        // 30 days: 0-30
	gone := recv("999", "USD", "2026-11-01") // reversed: left out
	if _, err := f.svc.Reverse(ctx, f.owner, f.org, gone.ID, ReverseInput{Reason: "waived"}); err != nil {
		t.Fatal(err)
	}
	pay := entry(TypeRentReceived, MethodCash, "60", "USD", "2026-11-25")
	pay.Settles = &a.ID
	f.post(f.owner, pay)
	full := entry(TypeRentReceived, MethodBank, "200", "USD", "2026-11-26")
	full.Settles = &b.ID
	f.post(f.owner, full)
	over := entry(TypeRentReceived, MethodCash, "40.01", "USD", "2026-11-27")
	over.Settles = &a.ID
	_, err := f.svc.Create(ctx, f.owner, f.org, over)
	wantField(t, err, "amount")
	wrong := entry(TypeRentReceived, MethodCash, "1", "ZAR", "2026-11-27")
	wrong.Settles = &a.ID
	_, err = f.svc.Create(ctx, f.owner, f.org, wrong)
	wantField(t, err, "settles")
	rep, err := f.svc.Receivables(ctx, f.staff, f.org, ReportQuery{AsOf: "2026-11-30"})
	if err != nil {
		t.Fatal(fieldsOf(err))
	}
	if len(rep.Currencies) != 2 || rep.Side != TypeReceivable {
		t.Fatalf("currencies: %+v", rep)
	}
	usd := rep.Currencies[0]
	want := map[string][2]string{"0-30": {"40.00", "1"}, "31-60": {"0.00", "0"}, "61-90": {"300.00", "1"}, "90+": {"400.00", "1"}}
	for _, bk := range usd.Buckets {
		if w := want[bk.Bucket]; bk.Outstanding != w[0] || itoa(bk.Count) != w[1] || bk.Currency != "USD" {
			t.Fatalf("bucket %s: %+v", bk.Bucket, bk)
		}
	}
	if usd.Outstanding != "740.00" || usd.Count != 3 {
		t.Fatalf("USD outstanding %s (%d)", usd.Outstanding, usd.Count)
	}
	if it := usd.Items[len(usd.Items)-1]; it.Settled != "60.00" || it.Outstanding != "40.00" || it.DaysOverdue != 10 {
		t.Fatalf("partly settled item: %+v", it)
	}
	zar := rep.Currencies[1]
	if zar.Currency != "ZAR" || zar.Outstanding != "1000.00" || zar.Buckets[0].Count != 1 {
		t.Fatalf("ZAR: %+v", zar)
	}
	// Earlier as_of: the settlement of 2026-11-25 had not happened yet.
	early, _ := f.svc.Receivables(ctx, f.staff, f.org, ReportQuery{AsOf: "2026-11-24", Currency: "USD"})
	if early.Currencies[0].Outstanding != "1000.00" {
		t.Fatalf("as of 24 Nov: %s", early.Currencies[0].Outstanding)
	}
	if bucketOf(0) != "0-30" || bucketOf(30) != "0-30" || bucketOf(31) != "31-60" || bucketOf(60) != "31-60" || bucketOf(61) != "61-90" ||
		bucketOf(90) != "61-90" || bucketOf(91) != "90+" {
		t.Fatal("bucket edges")
	}
	// A cashbook never counts the memo entries.
	cb, _ := f.svc.Cashbook(ctx, f.staff, f.org, ReportQuery{From: "2026-11-01", To: "2026-11-30", Currency: "USD"})
	if cb.Currencies[0].In != "260.00" {
		t.Fatalf("cash in %s", cb.Currencies[0].In)
	}
	st, err := f.svc.Statement(ctx, f.staff, f.org, ReportQuery{From: "2026-11-01", To: "2026-11-30", ContactID: &f.contact})
	if err != nil || len(st.Currencies) != 2 || st.Currencies[0].ReceivableOutstanding != "740.00" {
		t.Fatalf("statement: %+v %v", st, err)
	}
}

func itoa(n int) string { return big.NewInt(int64(n)).String() }

// Amounts: positive, at most the currency's minor-unit decimals, written
// with exactly that many.
func TestMinorUnits(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	ok := map[string][2]string{"JPY": {"1500", "1500"}, "KWD": {"1.25", "1.250"}, "USD": {"10.5", "10.50"}}
	for cur, c := range ok {
		e := f.post(f.owner, entry(TypeIncome, MethodBank, c[0], cur, "2026-11-01"))
		_ = e
	}
	list, _ := f.svc.List(ctx, f.staff, f.org, Filter{}, 1, 10)
	for _, it := range list.Items {
		if it.Amount != ok[it.Currency][1] {
			t.Fatalf("%s: %s", it.Currency, it.Amount)
		}
	}
	bad := []struct{ amount, cur, msg string }{
		{"1500.5", "JPY", "whole number"}, {"1500.0", "JPY", "whole number"}, {"1.2345", "KWD", "at most 3"}, {"10.005", "USD", "at most 2"},
		{"0", "USD", "more than zero"}, {"-5", "USD", "positive"}, {"1e3", "USD", "positive"}, {"12,50", "USD", "positive"},
	}
	for _, b := range bad {
		_, err := f.svc.Create(ctx, f.owner, f.org, entry(TypeIncome, MethodBank, b.amount, b.cur, "2026-11-01"))
		var v *ValidationError
		if !errors.As(err, &v) || !strings.Contains(v.Fields["amount"], b.msg) {
			t.Fatalf("%s %s: %v", b.amount, b.cur, fieldsOf(err))
		}
	}
	_, err := f.svc.Create(ctx, f.owner, f.org, entry(TypeIncome, MethodBank, "1", "XYZ", "2026-11-01"))
	wantField(t, err, "currency")
	_, err = f.svc.Create(ctx, f.owner, f.org, entry(TypeIncome, MethodBank, "1", "USD", "2026-02-30"))
	wantField(t, err, "effective_date")
	in := entry(TypeIncome, MethodBank, "1", "USD", "2026-11-01")
	in.Amount = nil
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "amount")
}

// Links must belong to the same book; tenancies do not exist yet; the
// method rules of the memo types.
func TestLinksAndShapes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	in := entry(TypeRentReceived, MethodBank, "1", "USD", "2026-11-01")
	in.ContactID = &f.foreign
	_, err := f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "contact_id")
	in.ContactID, in.TenancyID = nil, &f.contact
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "tenancy_id")
	lid := uuid.New()
	in.TenancyID, in.ListingID = nil, &lid
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "listing_id")
	in.ListingID, in.LeaseID = nil, &lid
	_, err = f.svc.Create(ctx, f.owner, f.org, in)
	wantField(t, err, "lease_id")
	r := entry(TypeReceivable, MethodCash, "1", "USD", "2026-11-01")
	r.Category = sp("rent")
	_, err = f.svc.Create(ctx, f.owner, f.org, r)
	wantField(t, err, "method")
	_, err = f.svc.Create(ctx, f.owner, f.org, entry(TypeIncome, "", "1", "USD", "2026-11-01"))
	wantField(t, err, "method")
	exp := entry(TypeExpense, MethodCash, "1", "USD", "2026-11-01")
	exp.Category = sp("rent")
	_, err = f.svc.Create(ctx, f.owner, f.org, exp)
	wantField(t, err, "category")
	exp.Direction = sp(In)
	exp.Category = sp("utilities")
	_, err = f.svc.Create(ctx, f.owner, f.org, exp)
	wantField(t, err, "direction")
	_, err = f.svc.Create(ctx, f.owner, f.org, EntryInput{Type: TypeReversal, Amount: sp("1"), Currency: "USD", EffectiveDate: "2026-11-01", Method: sp(MethodCash)})
	wantField(t, err, "type")
	// The first note goes with the entry.
	with := entry(TypeIncome, MethodBank, "1", "USD", "2026-11-01")
	with.Category, with.Notes = sp("fees"), sp("Paid at the counter")
	e := f.post(f.owner, with).Entry
	if e.NoteCount != 1 {
		t.Fatalf("notes: %d", e.NoteCount)
	}
}

// The permission matrix: OWNER / MANAGER full; AGENT records and corrects
// their own unreconciled entries; STAFF reads; another organisation, a
// consumer and an administrator get ErrBookNotFound everywhere.
func TestPermissionMatrix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	rent := func() EntryInput { return entry(TypeRentReceived, MethodCash, "10", "USD", "2026-11-01") }
	if _, err := f.svc.Create(ctx, f.staff, f.org, rent()); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff creates: %v", err)
	}
	mine := f.post(f.agent, rent()).Entry
	theirs := f.post(f.agent2, rent()).Entry
	if !mine.Can.Reverse || !mine.Can.Note || mine.Can.Reconcile {
		t.Fatalf("agent's can on own: %+v", mine.Can)
	}
	if _, err := f.svc.Reverse(ctx, f.agent, f.org, theirs.ID, ReverseInput{Reason: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent reverses another's: %v", err)
	}
	if _, err := f.svc.Correct(ctx, f.agent, f.org, theirs.ID, CorrectInput{EntryInput: rent(), Reason: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent corrects another's: %v", err)
	}
	if _, err := f.svc.AddNote(ctx, f.agent, f.org, theirs.ID, "hi"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent notes another's: %v", err)
	}
	yes := true
	if _, err := f.svc.Reconcile(ctx, f.agent, f.org, mine.ID, ReconcileInput{Reconciled: &yes}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent reconciles: %v", err)
	}
	if _, err := f.svc.Reconcile(ctx, f.owner, f.org, mine.ID, ReconcileInput{Reconciled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Correct(ctx, f.agent, f.org, mine.ID, CorrectInput{EntryInput: rent(), Reason: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent corrects own reconciled: %v", err)
	}
	if _, err := f.svc.ExportCSV(ctx, f.agent, f.org, ReportCashbook, ReportQuery{}, Filter{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent exports: %v", err)
	}
	if _, err := f.svc.ExportCSV(ctx, f.staff, f.org, ReportEntries, ReportQuery{}, Filter{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff exports: %v", err)
	}
	if _, err := f.svc.CreateCategory(ctx, f.agent, f.org, CategoryInput{Label: sp("Bonus"), Kind: sp(KindIn)}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent creates a category: %v", err)
	}
	if _, err := f.svc.AddNote(ctx, f.staff, f.org, mine.ID, "hi"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff notes: %v", err)
	}
	// The agent corrects their own unreconciled entry; the manager any.
	own := f.post(f.agent, rent()).Entry
	if _, err := f.svc.Correct(ctx, f.agent, f.org, own.ID, CorrectInput{EntryInput: rent(), Reason: "fix"}); err != nil {
		t.Fatal(fieldsOf(err))
	}
	if _, err := f.svc.Reverse(ctx, f.manager, f.org, theirs.ID, ReverseInput{Reason: "fix"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Reverse(ctx, f.manager, f.org, mine.ID, ReverseInput{Reason: "reconciled, still reversible by a manager"}); err != nil {
		t.Fatal(err)
	}
	// Readers read.
	for _, p := range []*auth.Principal{f.staff, f.agent, f.manager} {
		if _, err := f.svc.List(ctx, p, f.org, Filter{}, 1, 10); err != nil {
			t.Fatal(err)
		}
		if _, err := f.svc.Cashbook(ctx, p, f.org, ReportQuery{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.svc.ExportCSV(ctx, f.manager, f.org, ReportEntries, ReportQuery{}, Filter{}); err != nil {
		t.Fatal(err)
	}
	// Nobody else sees the book, nor an entry of it.
	for name, p := range map[string]*auth.Principal{"another organisation": f.outsider, "consumer": f.consumer, "administrator": f.admin, "landlord": f.landlord} {
		if err := f.svc.Check(p, f.org); !errors.Is(err, ErrBookNotFound) {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := f.svc.Get(ctx, p, f.org, mine.ID); !errors.Is(err, ErrBookNotFound) {
			t.Fatalf("%s get: %v", name, err)
		}
		if _, err := f.svc.Create(ctx, p, f.org, rent()); !errors.Is(err, ErrBookNotFound) {
			t.Fatalf("%s create: %v", name, err)
		}
	}
	// An entry of one book is not found through another.
	if _, err := f.svc.Get(ctx, f.outsider, f.other, mine.ID); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("cross-book entry: %v", err)
	}
	// A direct owner has full rights in their personal book, and no organisation audit.
	before := len(f.audit.All())
	pin := entry(TypeRentReceived, MethodBank, "500", "USD", "2026-11-01")
	got, err := f.svc.Create(ctx, f.landlord, f.landlord.UserID, pin)
	if err != nil {
		t.Fatal(fieldsOf(err))
	}
	if _, err := f.svc.Reconcile(ctx, f.landlord, f.landlord.UserID, got.Entry.ID, ReconcileInput{Reconciled: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ExportCSV(ctx, f.landlord, f.landlord.UserID, ReportCashbook, ReportQuery{}, Filter{}); err != nil {
		t.Fatal(err)
	}
	if len(f.audit.All()) != before {
		t.Fatal("a personal book wrote organisation audit records")
	}
	if _, err := f.svc.Get(ctx, f.owner, f.org, got.Entry.ID); !errors.Is(err, ErrEntryNotFound) {
		t.Fatalf("the personal entry leaks into the organisation: %v", err)
	}
}

// The export is OWNER / MANAGER only and audited.
func TestExportAudited(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.post(f.owner, entry(TypeRentReceived, MethodCash, "10", "USD", "2026-11-01"))
	f.post(f.owner, entry(TypeRentReceived, MethodCash, "20", "ZAR", "2026-11-01"))
	for _, report := range []string{ReportCashbook, ReportEntries, ReportCategories} {
		out, err := f.svc.ExportCSV(ctx, f.manager, f.org, report, ReportQuery{From: "2026-11-01", To: "2026-11-30"}, Filter{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out.FileName, "ledger-"+report) || !slices.Contains(out.Rows[0], "currency") {
			t.Fatalf("%s: %s %v", report, out.FileName, out.Rows[0])
		}
	}
	_, err := f.svc.ExportCSV(ctx, f.manager, f.org, ReportStatement, ReportQuery{}, Filter{})
	wantField(t, err, "contact_id")
	_, err = f.svc.ExportCSV(ctx, f.manager, f.org, "everything", ReportQuery{}, Filter{})
	wantField(t, err, "report")
	n := 0
	for _, e := range f.audit.All() {
		if e.Action == orgaudit.ActionLedgerExported {
			n++
			if *e.ActorUserID != f.manager.UserID || e.TargetType != orgaudit.TargetLedgerBook || e.TargetID != f.org {
				t.Fatalf("export audit: %+v", e)
			}
		}
	}
	if n != 3 {
		t.Fatalf("%d export audit records", n)
	}
	// Formula injection is neutralised in text cells.
	in := entry(TypeIncome, MethodCash, "1", "USD", "2026-11-02")
	in.Category, in.Description = sp("fees"), sp("=HYPERLINK(\"x\")")
	f.post(f.owner, in)
	out, _ := f.svc.ExportCSV(ctx, f.owner, f.org, ReportEntries, ReportQuery{}, Filter{})
	if !slices.ContainsFunc(out.Rows, func(r []string) bool { return slices.Contains(r, "'=HYPERLINK(\"x\")") }) {
		t.Fatal("formula not escaped")
	}
}

// Categories: seeded defaults, created and deactivated, never deleted; kind
// fixed once used.
func TestCategories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	list, err := f.svc.Categories(ctx, f.staff, f.org, nil)
	if err != nil || len(list) != 10 || list[0].Code != "rent" || !list[0].Default {
		t.Fatalf("defaults: %+v %v", list, err)
	}
	c, err := f.svc.CreateCategory(ctx, f.owner, f.org, CategoryInput{Label: sp("Office rent"), Kind: sp(KindOut)})
	if err != nil || c.Code != "office-rent" {
		t.Fatalf("create: %+v %v", c, err)
	}
	if _, err := f.svc.CreateCategory(ctx, f.owner, f.org, CategoryInput{Label: sp("Office rent"), Kind: sp(KindOut)}); !errors.Is(err, ErrCategoryExists) {
		t.Fatalf("duplicate: %v", err)
	}
	exp := entry(TypeExpense, MethodBank, "1", "USD", "2026-11-01")
	exp.Category = sp("office-rent")
	f.post(f.owner, exp)
	_, err = f.svc.UpdateCategory(ctx, f.owner, f.org, "office-rent", CategoryInput{Kind: sp(KindIn)})
	wantField(t, err, "kind")
	no := false
	c, err = f.svc.UpdateCategory(ctx, f.manager, f.org, "office-rent", CategoryInput{Active: &no, Label: sp("Office lease")})
	if err != nil || c.Active || !c.Used || c.Label != "Office lease" {
		t.Fatalf("deactivate: %+v %v", c, err)
	}
	_, err = f.svc.Create(ctx, f.owner, f.org, exp)
	wantField(t, err, "category")
	if _, err := f.svc.UpdateCategory(ctx, f.owner, f.org, "nope", CategoryInput{}); !errors.Is(err, ErrCategoryNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	active := true
	got, _ := f.svc.Categories(ctx, f.staff, f.org, &active)
	if slices.ContainsFunc(got, func(c CategoryView) bool { return c.Code == "office-rent" }) {
		t.Fatal("inactive listed as active")
	}
}

// Attachments: sniffed types only, audited downloads.
func TestAttachments(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	e := f.post(f.agent, entry(TypeRentReceived, MethodCash, "10", "USD", "2026-11-01")).Entry
	pdf := []byte("%PDF-1.4\n1 0 obj << >> endobj\ntrailer << >>\n%%EOF\n")
	if _, err := f.svc.AddAttachment(ctx, f.agent, f.org, e.ID, "receipt.exe", []byte("MZ\x90\x00 not a pdf")); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("exe: %v", err)
	}
	if _, err := f.svc.AddAttachment(ctx, f.agent2, f.org, e.ID, "r.pdf", pdf); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another agent attaches: %v", err)
	}
	a, err := f.svc.AddAttachment(ctx, f.agent, f.org, e.ID, "../../receipt.png", pdf)
	if err != nil || a.ContentType != "application/pdf" || a.FileName != "receipt.pdf" {
		t.Fatalf("attach: %+v %v", a, err)
	}
	fc, err := f.svc.OpenAttachment(ctx, f.staff, f.org, e.ID, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(fc.Content)
	_ = fc.Content.Close()
	if string(body) != string(pdf) {
		t.Fatal("content")
	}
	if _, err := f.svc.OpenAttachment(ctx, f.staff, f.org, e.ID, uuid.New()); !errors.Is(err, ErrAttachmentNotFound) {
		t.Fatalf("unknown attachment: %v", err)
	}
	viewed := 0
	for _, ev := range f.audit.All() {
		if ev.Action == orgaudit.ActionLedgerAttachmentViewed && *ev.ActorUserID == f.staff.UserID {
			viewed++
		}
	}
	if viewed != 1 {
		t.Fatalf("%d download audits", viewed)
	}
}
