package tenancies

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/ledger/ledgertest"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// rentFixture is the tenancies fixture with the ledger connected.
type rentFixture struct {
	*fixture
	books *ledger.MemoryStore
	ledg  *ledger.Service
}

func newRentFixture(t *testing.T) *rentFixture {
	t.Helper()
	f := &rentFixture{fixture: newFixture(t), books: ledger.NewMemoryStore()}
	f.books.SetAuditSink(f.audit.Append)
	if err := ConnectLedger(f.store, f.books); err != nil {
		t.Fatal(err)
	}
	f.ledg = ledger.NewService(ledger.Deps{Store: f.books,
		Currencies: func(context.Context) ([]string, error) { return []string{"USD", "ZAR", "JPY"}, nil },
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return f.now }})
	f.ledg.SetRentArrears(f.svc.LedgerArrears)
	f.svc.SetLedger(f.ledg)
	return f
}

// active creates and activates a whole-property tenancy of f.tenant.
func (f *rentFixture) active(start, rent string) TenancyView {
	f.t.Helper()
	p := f.property()
	in := body[TenancyInput](f.t, `{"property_id":"`+p.ID.String()+`","tenants":["`+f.tenant.String()+`"],"rent":`+rent+`,"start_date":"`+start+`"}`)
	ten, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in)
	if err != nil {
		f.t.Fatal(err)
	}
	if ten, err = f.svc.Activate(f.ctx, f.agent, f.org, ten.ID); err != nil {
		f.t.Fatal(err)
	}
	return ten
}

func (f *rentFixture) dues(id uuid.UUID) DuesPage {
	f.t.Helper()
	page, err := f.svc.ListDues(f.ctx, f.staff, f.org, id, DueFilter{})
	if err != nil {
		f.t.Fatal(err)
	}
	return page
}

func (f *rentFixture) receipt(id uuid.UUID, raw string) ReceiptResult {
	f.t.Helper()
	res, err := f.svc.RecordReceipt(f.ctx, f.agent, f.org, id, body[ReceiptInput](f.t, raw))
	if err != nil {
		f.t.Fatalf("%s: %v", raw, err)
	}
	return res
}

func (f *rentFixture) entries() []ledger.Entry {
	f.t.Helper()
	list, _, err := f.books.Entries(f.ctx, ledger.OrganisationBook(f.org), ledger.Filter{Ascending: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return list
}

func statuses(items []DueView, n int) string {
	var out []string
	for _, d := range items[:n] {
		out = append(out, d.Status)
	}
	return strings.Join(out, ",")
}

func perCurrency(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledgertest.PerCurrencyProblem(raw); err != nil {
		t.Fatalf("%v: %s", err, raw)
	}
}

const monthly950 = `{"amount":"950.00","currency":"USD","frequency":"MONTHLY","due_day":1}`

func TestScheduleOnActivationEndAndWorker(t *testing.T) {
	f := newRentFixture(t)
	ten := f.active("2026-09-01", monthly950)
	page := f.dues(ten.ID)
	// September 2026 to November 2027 (rolling: the end of the month 12 months ahead of 2026-11-19).
	if len(page.Items) != 15 || page.Items[0].DueDate != "2026-09-01" || page.Items[14].DueDate != "2027-11-01" || page.Schedule == nil ||
		!page.Schedule.Rolling || page.Schedule.Through != "2027-11-30" || page.Schedule.Count != 15 || page.Schedule.Rule != "Monthly on day 1" {
		t.Fatalf("schedule: %d %+v", len(page.Items), page.Schedule)
	}
	if d := page.Items[0]; d.Status != DueDue || !d.Overdue || d.DaysOverdue != 79 || d.Period.To != "2026-09-30" || d.Outstanding != "950.00" ||
		d.Can.Waive {
		t.Fatalf("first due: %+v", d)
	}
	s := page.Summary
	if s.Charged != "2850.00" || s.Outstanding != "2850.00" || s.Overdue != "2850.00" || s.OverdueCount != 3 || s.Paid != "0.00" ||
		s.NextDue == nil || s.NextDue.DueDate != "2026-12-01" || s.Currency != "USD" {
		t.Fatalf("summary: %+v", s)
	}
	perCurrency(t, page)
	if agent, _ := f.svc.ListDues(f.ctx, f.agent, f.org, ten.ID, DueFilter{Status: DueDue, To: "2026-10-31"}); len(agent.Items) != 2 ||
		!agent.Items[0].Can.Waive || agent.Summary.Charged != "2850.00" {
		t.Fatalf("the agent's filtered view: %+v", agent)
	}
	// The worker: nothing to do the same month; one due more when the month turns.
	if n, err := f.svc.ExtendSchedules(f.ctx); err != nil || n != 0 {
		t.Fatalf("same month: %d %v", n, err)
	}
	f.now = time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)
	if n, err := f.svc.ExtendSchedules(f.ctx); err != nil || n != 1 {
		t.Fatalf("month turned: %d %v", n, err)
	}
	if n, _ := f.svc.ExtendSchedules(f.ctx); n != 0 {
		t.Fatal("the worker is not idempotent")
	}
	if page := f.dues(ten.ID); len(page.Items) != 16 || page.Items[15].DueDate != "2027-12-01" || page.Items[15].Sequence != 16 {
		t.Fatalf("extended: %d", len(page.Items))
	}
	// A paid due after the end stays; the unpaid later ones go.
	f.receipt(ten.ID, `{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-12-01","allocations":[{"due_id":"`+
		f.dues(ten.ID).Items[5].ID.String()+`","amount":"950.00"}]}`)
	if _, err := f.svc.End(f.ctx, f.agent, f.org, ten.ID, EndInput{EndDate: "2026-12-15", Reason: "Moved out"}); err != nil {
		t.Fatal(err)
	}
	page = f.dues(ten.ID)
	if len(page.Items) != 5 || page.Items[4].DueDate != "2027-02-01" || page.Items[4].Status != DuePaid || page.Schedule.Rolling ||
		page.Schedule.Through != "2026-12-15" || page.Items[3].Period.To != "2026-12-15" {
		t.Fatalf("after the end: %d %+v", len(page.Items), page.Schedule)
	}
	// An agreed end date: generated through it; a later PATCH of it re-runs the rule.
	g := newRentFixture(t)
	fixed := g.active("2026-11-01", `{"amount":"100","currency":"USD","frequency":"QUARTERLY","due_day":31}`)
	if _, err := g.svc.UpdateTenancy(g.ctx, g.agent, g.org, fixed.ID, body[TenancyInput](t, `{"end_date":"2027-06-30"}`)); err != nil {
		t.Fatal(err)
	}
	page = g.dues(fixed.ID)
	if len(page.Items) != 3 || page.Items[0].DueDate != "2026-11-30" || page.Items[1].DueDate != "2027-02-28" || page.Items[2].DueDate != "2027-05-31" ||
		page.Schedule.Rolling || page.Schedule.Rule != "Quarterly on day 31 (the last day in shorter months)" {
		t.Fatalf("quarterly fixed term: %+v", page.Items)
	}
	// A DRAFT has none.
	draft, err := g.svc.CreateTenancy(g.ctx, g.agent, g.org, g.tenancyBody(g.property().ID, nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if page := g.dues(draft.ID); len(page.Items) != 0 || page.Schedule != nil || page.Summary.Charged != "0.00" {
		t.Fatalf("draft: %+v", page)
	}
}

func TestReceiptsAllocation(t *testing.T) {
	f := newRentFixture(t)
	ten := f.active("2026-09-01", monthly950)
	dues := f.dues(ten.ID).Items
	// Oldest first by default: September paid, October part paid.
	res := f.receipt(ten.ID, `{"amount":"1000.00","currency":"usd","method":"BANK","received_on":"2026-11-02","reference":"FNB 4471"}`)
	r := res.Receipt
	if r.Status != ReceiptRecorded || r.Amount != "1000.00" || len(r.Allocations) != 2 || r.Allocations[0].DueID != dues[0].ID ||
		r.Allocations[0].Amount != "950.00" || r.Allocations[1].Amount != "50.00" || r.Allocations[1].DueDate != "2026-10-01" ||
		r.Tenant == nil || r.Tenant.ID != f.tenant || !r.Tenant.Live || r.LedgerStatus != ledger.StatusUnverified || !r.Can.Reverse ||
		r.MethodLabel != "Bank" || r.RecordedBy.Name != "Ada Agent" {
		t.Fatalf("receipt: %+v", r)
	}
	if len(res.Dues) != 2 || res.Dues[0].Status != DuePaid || res.Dues[1].Status != DuePartPaid || res.Dues[1].Outstanding != "900.00" {
		t.Fatalf("dues: %+v", res.Dues)
	}
	perCurrency(t, res)
	// Exactly one ledger entry, linked to the tenancy and the tenant contact, unverified.
	entries := f.entries()
	if len(entries) != 1 {
		t.Fatalf("ledger entries: %d", len(entries))
	}
	e := entries[0]
	if e.ID != r.LedgerEntryID || e.Type != ledger.TypeRentReceived || e.Direction != ledger.In || e.Amount != "1000.00" || e.Currency != "USD" ||
		e.TenancyID == nil || *e.TenancyID != ten.ID || e.ContactID == nil || *e.ContactID != f.tenant || e.Status != ledger.StatusUnverified ||
		e.Category != "rent" || e.Method != ledger.MethodBank || e.EffectiveDate != "2026-11-02" || e.Reference != "FNB 4471" ||
		e.Description != "Rent: Avondale Court" {
		t.Fatalf("ledger entry: %+v", e)
	}
	// Explicit allocations: any dues of the tenancy, adding up to the amount.
	res = f.receipt(ten.ID, `{"amount":"1050.00","currency":"USD","method":"CASH","received_on":"2026-11-19",
		"allocations":[{"due_id":"`+dues[3].ID.String()+`","amount":"950.00"},{"due_id":"`+dues[2].ID.String()+`","amount":"100.00"}]}`)
	if len(res.Receipt.Allocations) != 2 || res.Receipt.Allocations[0].DueID != dues[2].ID || res.Dues[1].Status != DuePaid {
		t.Fatalf("explicit: %+v", res.Receipt.Allocations)
	}
	if s := f.dues(ten.ID).Summary; s.Paid != "1100.00" || s.Outstanding != "1750.00" || s.PaidInAdvance != "950.00" {
		t.Fatalf("summary: %+v", s)
	}
	// Refusals: nothing is written.
	refused := []struct {
		raw, field string
	}{
		{`{"amount":"950.00","currency":"ZAR","method":"BANK","received_on":"2026-11-02"}`, "currency"},
		{`{"amount":"950.001","currency":"USD","method":"BANK","received_on":"2026-11-02"}`, "amount"},
		{`{"amount":"950.00","currency":"USD","method":"CHEQUE","received_on":"2026-11-02"}`, "method"},
		{`{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-11-20"}`, "received_on"},
		{`{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-08-31"}`, "received_on"},
		{`{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-11-02","tenant_contact_id":"` + f.landlord.String() + `"}`, "tenant_contact_id"},
		// More than the whole schedule's outstanding (15 x 950 - 2050 = 12200).
		{`{"amount":"12200.01","currency":"USD","method":"BANK","received_on":"2026-11-02"}`, "amount"},
		// A line above its due's outstanding (October: 900.00 left).
		{`{"amount":"901.00","currency":"USD","method":"BANK","received_on":"2026-11-02","allocations":[{"due_id":"` + dues[1].ID.String() + `","amount":"901.00"}]}`, "allocations[0].amount"},
		{`{"amount":"500.00","currency":"USD","method":"BANK","received_on":"2026-11-02","allocations":[{"due_id":"` + dues[1].ID.String() + `","amount":"400.00"}]}`, "allocations"},
		{`{"amount":"500.00","currency":"USD","method":"BANK","received_on":"2026-11-02","allocations":[{"due_id":"` + uuid.NewString() + `","amount":"500.00"}]}`, "allocations[0].due_id"},
		{`{"amount":"500.00","currency":"USD","method":"BANK","received_on":"2026-11-02","allocations":[{"due_id":"` + dues[4].ID.String() + `","amount":"250.00"},{"due_id":"` + dues[4].ID.String() + `","amount":"250.00"}]}`, "allocations[1].due_id"},
	}
	for _, c := range refused {
		_, err := f.svc.RecordReceipt(f.ctx, f.agent, f.org, ten.ID, body[ReceiptInput](t, c.raw))
		if got := fields(t, err); got[c.field] == "" {
			t.Fatalf("%s: fields %v", c.raw, got)
		}
	}
	if s := f.dues(ten.ID).Summary; s.Paid != "1100.00" || len(f.entries()) != 2 {
		t.Fatalf("a refused receipt wrote something: %+v %d", s, len(f.entries()))
	}
	// The whole remaining schedule may be paid, not a cent more.
	f.receipt(ten.ID, `{"amount":"12200.00","currency":"USD","method":"BANK","received_on":"2026-11-19"}`)
	if _, err := f.svc.RecordReceipt(f.ctx, f.agent, f.org, ten.ID, body[ReceiptInput](t,
		`{"amount":"0.01","currency":"USD","method":"BANK","received_on":"2026-11-19"}`)); fields(t, err)["amount"] == "" {
		t.Fatal("over-allocated")
	}
	// Rights and states.
	if _, err := f.svc.RecordReceipt(f.ctx, f.staff, f.org, ten.ID, ReceiptInput{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff: %v", err)
	}
	for _, who := range []*auth.Principal{f.outsider, f.consumer, f.admin} {
		if _, err := f.svc.RecordReceipt(f.ctx, who, f.org, ten.ID, ReceiptInput{}); !errors.Is(err, ErrBookNotFound) {
			t.Fatalf("wrong book: %v", err)
		}
		if _, err := f.svc.ListReceipts(f.ctx, who, f.org, ten.ID); !errors.Is(err, ErrBookNotFound) {
			t.Fatalf("wrong book list: %v", err)
		}
	}
	if _, err := f.svc.ListReceipts(f.ctx, f.outsider, f.other, ten.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("another book's tenancy: %v", err)
	}
	draft, _ := f.svc.CreateTenancy(f.ctx, f.agent, f.org, f.tenancyBody(f.property().ID, nil, ""))
	var state *StateError
	if _, err := f.svc.RecordReceipt(f.ctx, f.agent, f.org, draft.ID, body[ReceiptInput](t,
		`{"amount":"1","currency":"USD","method":"BANK","received_on":"2026-11-19"}`)); !errors.As(err, &state) {
		t.Fatalf("draft: %v", err)
	}
	// Audited.
	n := 0
	for _, ev := range f.audit.All() {
		if ev.Action == orgaudit.ActionRentReceiptRecorded && ev.TargetID == ten.ID {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("receipt audit records: %d", n)
	}
}

// A receipt and its ledger entry are one transaction: a failure in the
// tenancies part keeps neither.
func TestReceiptAndLedgerEntryAtomic(t *testing.T) {
	f := newRentFixture(t)
	ten := f.active("2026-09-01", monthly950)
	f.store.failNext = errors.New("the disk is on fire")
	_, err := f.svc.RecordReceipt(f.ctx, f.agent, f.org, ten.ID, body[ReceiptInput](t,
		`{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-11-02"}`))
	if err == nil || !strings.Contains(err.Error(), "on fire") {
		t.Fatalf("want the failure, got %v", err)
	}
	if len(f.entries()) != 0 {
		t.Fatal("the ledger entry was kept without its receipt")
	}
	if list, _ := f.svc.ListReceipts(f.ctx, f.agent, f.org, ten.ID); len(list) != 0 {
		t.Fatal("the receipt was kept")
	}
	if s := f.dues(ten.ID).Summary; s.Paid != "0.00" {
		t.Fatalf("allocations kept: %+v", s)
	}
	// A failure of the ledger part (an entry with that ID exists) keeps neither either.
	res := f.receipt(ten.ID, `{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-11-02"}`)
	if len(f.entries()) != 1 || res.Receipt.Allocations[0].DueID != f.dues(ten.ID).Items[0].ID {
		t.Fatal("control")
	}
	lch := ledger.Change{Entries: []ledger.Entry{{ID: res.Receipt.LedgerEntryID, Book: ledger.OrganisationBook(f.org), Type: ledger.TypeRentReceived,
		Direction: ledger.In, Method: ledger.MethodCash, Amount: "1.00", Currency: "USD", EffectiveDate: "2026-11-02", Status: ledger.StatusUnverified}}}
	dup := Receipt{ID: newID(), TenancyID: ten.ID, Amount: "1.00", Currency: "USD", Method: ledger.MethodCash, ReceivedOn: "2026-11-02",
		LedgerEntryID: res.Receipt.LedgerEntryID, Allocations: []Allocation{{DueID: f.dues(ten.ID).Items[1].ID, Amount: "1.00"}}}
	if err := f.store.Apply(f.ctx, Change{Ledger: &lch, Receipts: []Receipt{dup}}); !errors.Is(err, ledger.ErrImmutable) {
		t.Fatalf("duplicate entry: %v", err)
	}
	if list, _ := f.svc.ListReceipts(f.ctx, f.agent, f.org, ten.ID); len(list) != 1 {
		t.Fatal("the receipt was kept without its entry")
	}
	// Over-allocation found inside the transaction (a concurrent receipt) keeps neither.
	over := Receipt{ID: newID(), TenancyID: ten.ID, Amount: "960.00", Currency: "USD", Method: ledger.MethodCash, ReceivedOn: "2026-11-02",
		LedgerEntryID: newID(), Allocations: []Allocation{{DueID: f.dues(ten.ID).Items[1].ID, Amount: "960.00"}}}
	lch.Entries[0].ID = over.LedgerEntryID
	if err := f.store.Apply(f.ctx, Change{Ledger: &lch, Receipts: []Receipt{over}}); !errors.Is(err, ErrDueChanged) {
		t.Fatalf("over-allocation in the store: %v", err)
	}
	if len(f.entries()) != 1 {
		t.Fatal("the entry of a refused receipt was kept")
	}
}

func TestReversalUndoesAllocations(t *testing.T) {
	f := newRentFixture(t)
	ten := f.active("2026-09-01", monthly950)
	res := f.receipt(ten.ID, `{"amount":"1400.00","currency":"USD","method":"BANK","received_on":"2026-11-02"}`)
	if st := statuses(f.dues(ten.ID).Items, 3); st != "PAID,PART_PAID,DUE" {
		t.Fatalf("before: %s", st)
	}
	// The staff member may not reverse; the agent who recorded it may.
	if list, _ := f.svc.ListReceipts(f.ctx, f.staff, f.org, ten.ID); list[0].Can.Reverse {
		t.Fatal("staff may reverse")
	}
	// A correction of a receipt's entry is refused: reverse it instead.
	if _, err := f.ledg.Correct(f.ctx, f.owner, f.org, res.Receipt.LedgerEntryID, ledger.CorrectInput{Reason: "typo"}); !errors.Is(err, ledger.ErrRentReceipt) {
		t.Fatalf("correct: %v", err)
	}
	date := "2026-11-05"
	posted, err := f.ledg.Reverse(f.ctx, f.agent, f.org, res.Receipt.LedgerEntryID, ledger.ReverseInput{Reason: "Bounced", EffectiveDate: &date})
	if err != nil {
		t.Fatal(err)
	}
	if posted.Entry.TenancyID == nil || *posted.Entry.TenancyID != ten.ID || posted.Entry.Type != ledger.TypeReversal {
		t.Fatalf("reversal entry: %+v", posted.Entry)
	}
	page := f.dues(ten.ID)
	if st := statuses(page.Items, 3); st != "DUE,DUE,DUE" || page.Summary.Paid != "0.00" || page.Summary.Outstanding != "2850.00" {
		t.Fatalf("after: %s %+v", st, page.Summary)
	}
	list, err := f.svc.ListReceipts(f.ctx, f.owner, f.org, ten.ID)
	if err != nil || len(list) != 1 {
		t.Fatal(err)
	}
	r := list[0]
	if r.Status != ReceiptReversed || r.Reversal == nil || r.Reversal.ReversedOn != "2026-11-05" || r.Reversal.LedgerEntryID != posted.Entry.ID ||
		r.Can.Reverse || len(r.Allocations) != 2 {
		t.Fatalf("reversed receipt: %+v", r)
	}
	// The dues may be paid again.
	f.receipt(ten.ID, `{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-11-06"}`)
	if st := statuses(f.dues(ten.ID).Items, 2); st != "PAID,DUE" {
		t.Fatalf("paid again: %s", st)
	}
	// As of a date before the reversal, the first receipt still counted
	// (October's 500.00 and November's 950.00 were overdue).
	arr, err := f.svc.Arrears(f.ctx, f.staff, f.org, ArrearsQuery{AsOf: "2026-11-04"})
	if err != nil || len(arr.Currencies) != 1 || arr.Currencies[0].Outstanding != "1450.00" || arr.Currencies[0].Tenancies[0].DaysOverdue != 34 {
		t.Fatalf("arrears before the reversal: %+v %v", arr, err)
	}
}

func TestWaive(t *testing.T) {
	f := newRentFixture(t)
	ten := f.active("2026-09-01", monthly950)
	dues := f.dues(ten.ID).Items
	f.receipt(ten.ID, `{"amount":"1300.00","currency":"USD","method":"BANK","received_on":"2026-11-02"}`)
	// Validation, rights and the wrong book first.
	if _, err := f.svc.WaiveDue(f.ctx, f.agent, f.org, dues[1].ID, WaiveInput{Reason: " "}); fields(t, err)["reason"] == "" {
		t.Fatal("empty reason")
	}
	if _, err := f.svc.WaiveDue(f.ctx, f.staff, f.org, dues[1].ID, WaiveInput{Reason: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff: %v", err)
	}
	if _, err := f.svc.WaiveDue(f.ctx, f.outsider, f.org, dues[1].ID, WaiveInput{Reason: "x"}); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("wrong book: %v", err)
	}
	if _, err := f.svc.WaiveDue(f.ctx, f.outsider, f.other, dues[1].ID, WaiveInput{Reason: "x"}); !errors.Is(err, ErrDueNotFound) {
		t.Fatalf("another book's due: %v", err)
	}
	// October is part paid (350.00): the rest is forgiven.
	got, err := f.svc.WaiveDue(f.ctx, f.agent, f.org, dues[1].ID, WaiveInput{Reason: "Storm damage"})
	if err != nil || got.Status != DueWaived || got.Outstanding != "0.00" || got.Waived != "600.00" || got.Paid != "350.00" || got.Overdue ||
		got.Waiver == nil || got.Waiver.Reason != "Storm damage" || got.Waiver.By.Name != "Ada Agent" || got.Can.Waive {
		t.Fatalf("waived: %+v %v", got, err)
	}
	var nw *NotWaivableError
	if _, err := f.svc.WaiveDue(f.ctx, f.agent, f.org, dues[1].ID, WaiveInput{Reason: "again"}); !errors.As(err, &nw) || nw.Status != DueWaived {
		t.Fatalf("waived twice: %v", err)
	}
	if _, err := f.svc.WaiveDue(f.ctx, f.agent, f.org, dues[0].ID, WaiveInput{Reason: "paid"}); !errors.As(err, &nw) || nw.Status != DuePaid {
		t.Fatalf("a paid due: %v", err)
	}
	// A waived due takes no allocation; oldest first skips it.
	if _, err := f.svc.RecordReceipt(f.ctx, f.agent, f.org, ten.ID, body[ReceiptInput](t, `{"amount":"1","currency":"USD","method":"BANK",
		"received_on":"2026-11-19","allocations":[{"due_id":"`+dues[1].ID.String()+`","amount":"1"}]}`)); fields(t, err)["allocations[0].due_id"] == "" {
		t.Fatal("allocated to a waived due")
	}
	res := f.receipt(ten.ID, `{"amount":"10.00","currency":"USD","method":"CASH","received_on":"2026-11-19"}`)
	if res.Receipt.Allocations[0].DueID != dues[2].ID {
		t.Fatalf("oldest first did not skip the waived due: %+v", res.Receipt.Allocations)
	}
	s := f.dues(ten.ID).Summary
	if s.Waived != "600.00" || s.Paid != "1310.00" || s.Outstanding != "940.00" || s.OverdueCount != 1 {
		t.Fatalf("summary: %+v", s)
	}
	found := false
	for _, ev := range f.audit.All() {
		found = found || (ev.Action == orgaudit.ActionRentDueWaived && ev.TargetID == ten.ID && ev.After["reason"] == "Storm damage")
	}
	if !found {
		t.Fatal("waiver not audited")
	}
}

func TestStatementTotals(t *testing.T) {
	f := newRentFixture(t)
	ten := f.active("2026-09-01", monthly950)
	dues := f.dues(ten.ID).Items
	first := f.receipt(ten.ID, `{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-09-02","reference":"=HYPERLINK(1)"}`)
	f.now = time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	f.receipt(ten.ID, `{"amount":"500.00","currency":"USD","method":"MOBILE_MONEY","received_on":"2026-10-03"}`)
	date := "2026-10-10"
	if _, err := f.ledg.Reverse(f.ctx, f.owner, f.org, first.Receipt.LedgerEntryID, ledger.ReverseInput{Reason: "Bounced", EffectiveDate: &date}); err != nil {
		t.Fatal(err)
	}
	f.now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	if _, err := f.svc.WaiveDue(f.ctx, f.owner, f.org, dues[2].ID, WaiveInput{Reason: "Goodwill"}); err != nil {
		t.Fatal(err)
	}
	st, err := f.svc.TenancyStatement(f.ctx, f.staff, f.org, ten.ID, "2026-10-01", "2026-11-30")
	if err != nil || len(st.Currencies) != 1 {
		t.Fatalf("statement: %+v %v", st, err)
	}
	c := st.Currencies[0]
	// Opening: September's charge less its payment. Then October's charge, a
	// payment of 500, the reversal of September's payment, November's charge and its waiver.
	if c.Currency != "USD" || c.OpeningBalance != "0.00" || c.Charged != "1900.00" || c.Paid != "500.00" || c.Reversed != "950.00" ||
		c.Waived != "950.00" || c.ClosingBalance != "1400.00" || c.Overdue != "1400.00" || len(c.Lines) != 5 {
		t.Fatalf("totals: %+v", c)
	}
	var kinds []string
	for _, l := range c.Lines {
		kinds = append(kinds, l.Date+" "+l.Type+" "+l.Balance)
	}
	if strings.Join(kinds, "|") != "2026-10-01 CHARGE 950.00|2026-10-03 PAYMENT 450.00|2026-10-10 PAYMENT_REVERSED 1400.00|"+
		"2026-11-01 CHARGE 2350.00|2026-11-19 WAIVER 1400.00" {
		t.Fatalf("lines: %v", kinds)
	}
	if c.Lines[0].Description != "Rent 2026-10-01 to 2026-10-31" || c.Lines[1].Description != "Payment (Mobile money)" {
		t.Fatalf("descriptions: %+v", c.Lines[:2])
	}
	perCurrency(t, st)
	// The whole history: opening 0, every line.
	all, err := f.svc.TenancyStatement(f.ctx, f.staff, f.org, ten.ID, "", "")
	if err != nil || all.From != "2026-09-01" || all.To != "2026-11-19" || len(all.Currencies[0].Lines) != 7 || all.Currencies[0].ClosingBalance != "1400.00" {
		t.Fatalf("default range: %+v %v", all, err)
	}
	if _, err := f.svc.TenancyStatement(f.ctx, f.staff, f.org, ten.ID, "2026-11-30", "2026-10-01"); fields(t, err)["to"] == "" {
		t.Fatal("to before from")
	}
	// CSV (formula-guarded) and PDF.
	raw, err := StatementCSV(all)
	if err != nil || !strings.HasPrefix(string(raw), "currency,row,date,type,description,reference,debit,credit,balance\n") ||
		!strings.Contains(string(raw), "USD,OPENING,2026-09-01") || !strings.Contains(string(raw), "USD,CLOSING,2026-11-19,,Closing balance,,,,1400.00") ||
		!strings.Contains(string(raw), "'=HYPERLINK(1)") {
		t.Fatalf("csv: %s %v", raw, err)
	}
	pdf, err := StatementPDF(all)
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-")) || len(pdf) < 2000 {
		t.Fatalf("pdf: %d %v", len(pdf), err)
	}
	if name := StatementFileName(all, FormatPDF); name != "statement-"+ten.ID.String()[:8]+"-2026-09-01-2026-11-19.pdf" {
		t.Fatalf("file name %s", name)
	}
	// An activated tenancy with nothing yet in its window still lists its currency; one without dues has none.
	g := newRentFixture(t)
	draft, _ := g.svc.CreateTenancy(g.ctx, g.agent, g.org, g.tenancyBody(g.property().ID, nil, ""))
	if st, err := g.svc.TenancyStatement(g.ctx, g.staff, g.org, draft.ID, "", "2026-12-31"); err != nil || len(st.Currencies) != 0 {
		t.Fatalf("draft statement: %+v %v", st, err)
	}
}

func TestArrearsBuckets(t *testing.T) {
	f := newRentFixture(t)
	usd := f.active("2026-01-01", `{"amount":"100","currency":"USD","frequency":"WEEKLY","due_day":1}`)
	zar := f.active("2026-01-01", `{"amount":"70","currency":"ZAR","frequency":"WEEKLY","due_day":1}`)
	// Replace the generated schedules with dues 0, 1, 30, 31, 60, 61, 90 and 91 days before 2026-11-19.
	for _, id := range []uuid.UUID{usd.ID, zar.ID} {
		f.store.mu.Lock()
		for k, d := range f.store.dues {
			if d.TenancyID == id {
				delete(f.store.dues, k)
			}
		}
		f.store.mu.Unlock()
	}
	asOf := parseDay("2026-11-19")
	var put []Due
	for i, days := range []int{0, 1, 30, 31, 60, 61, 90, 91} {
		put = append(put, Due{ID: newID(), TenancyID: usd.ID, Sequence: i + 1, DueDate: fmtDay(asOf.AddDate(0, 0, -days)), Amount: "100.00", Currency: "USD"})
	}
	put = append(put, Due{ID: newID(), TenancyID: zar.ID, Sequence: 1, DueDate: "2026-11-01", Amount: "70.00", Currency: "ZAR"})
	// A receipt received after the as-of date does not count as of it.
	late := Receipt{ID: newID(), TenancyID: usd.ID, Amount: "40.00", Currency: "USD", Method: ledger.MethodCash, ReceivedOn: "2026-11-19",
		LedgerEntryID: newID(), Allocations: []Allocation{{DueID: put[1].ID, Amount: "40.00"}}}
	f.store.Put(Change{Dues: put, Receipts: []Receipt{late}})

	got, err := f.svc.Arrears(f.ctx, f.staff, f.org, ArrearsQuery{AsOf: "2026-11-19"})
	if err != nil || len(got.Currencies) != 2 || got.Currencies[0].Currency != "USD" || got.Currencies[1].Currency != "ZAR" {
		t.Fatalf("arrears: %+v %v", got, err)
	}
	u := got.Currencies[0]
	want := map[string][2]string{"0-30": {"160.00", "2"}, "31-60": {"200.00", "2"}, "61-90": {"200.00", "2"}, "90+": {"100.00", "1"}}
	if u.Outstanding != "660.00" || u.Count != 7 || len(u.Buckets) != 4 || len(u.Tenancies) != 1 || u.Tenancies[0].DaysOverdue != 91 ||
		u.Tenancies[0].OldestDueDate != "2026-08-20" || u.Tenancies[0].Outstanding != "660.00" {
		t.Fatalf("USD: %+v", u)
	}
	for i, b := range u.Buckets {
		if b.Bucket != bucketNames[i] || b.Outstanding != want[b.Bucket][0] || b.Currency != "USD" || want[b.Bucket][1] != string(rune('0'+b.Count)) {
			t.Fatalf("bucket %+v", b)
		}
	}
	if z := got.Currencies[1]; z.Outstanding != "70.00" || z.Buckets[0].Count != 1 {
		t.Fatalf("ZAR: %+v", z)
	}
	perCurrency(t, got)
	// The day after, the late receipt counts.
	if got, _ := f.svc.Arrears(f.ctx, f.staff, f.org, ArrearsQuery{AsOf: "2026-11-20", Currency: "usd"}); len(got.Currencies) != 1 ||
		got.Currencies[0].Outstanding != "760.00" || got.Currencies[0].Count != 8 {
		t.Fatalf("next day (the 0-day due is now overdue, the late receipt counts): %+v", got)
	}
	// Filters and validation.
	if got, _ := f.svc.Arrears(f.ctx, f.staff, f.org, ArrearsQuery{AsOf: "2026-11-19", PropertyID: &zar.Property.ID}); len(got.Currencies) != 1 ||
		got.Currencies[0].Currency != "ZAR" {
		t.Fatalf("property filter: %+v", got)
	}
	if _, err := f.svc.Arrears(f.ctx, f.staff, f.org, ArrearsQuery{AsOf: "2026-02-30"}); fields(t, err)["as_of"] == "" {
		t.Fatal("bad as_of")
	}
	if _, err := f.svc.Arrears(f.ctx, f.outsider, f.org, ArrearsQuery{}); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("wrong book: %v", err)
	}
	// The ledger's receivables report includes them, per currency.
	rec, err := f.ledg.Receivables(f.ctx, f.staff, f.org, ledger.ReportQuery{AsOf: "2026-11-19"})
	if err != nil || len(rec.Currencies) != 2 || rec.Currencies[0].RentOutstanding != "660.00" || rec.Currencies[0].Outstanding != "660.00" ||
		len(rec.Currencies[0].RentItems) != 7 || rec.Currencies[0].RentItems[0].DaysOverdue != 91 || rec.Currencies[0].RentItems[0].Bucket != "90+" ||
		rec.Currencies[0].Count != 7 || rec.Currencies[0].Buckets[1].Outstanding != "200.00" || rec.Currencies[1].RentOutstanding != "70.00" {
		t.Fatalf("receivables: %+v %v", rec, err)
	}
	perCurrency(t, rec)
	if rec, _ := f.ledg.Receivables(f.ctx, f.staff, f.org, ledger.ReportQuery{AsOf: "2026-11-19", Currency: "ZAR"}); len(rec.Currencies) != 1 {
		t.Fatalf("receivables currency filter: %+v", rec)
	}
	other := uuid.New()
	if rec, _ := f.ledg.Receivables(f.ctx, f.staff, f.org, ledger.ReportQuery{AsOf: "2026-11-19", ContactID: &other}); len(rec.Currencies) != 0 {
		t.Fatalf("receivables contact filter: %+v", rec)
	}
	branch := "Harare"
	if rec, _ := f.ledg.Receivables(f.ctx, f.staff, f.org, ledger.ReportQuery{AsOf: "2026-11-19", Branch: &branch}); len(rec.Currencies) != 0 {
		t.Fatalf("a branch filter has no rent: %+v", rec)
	}
	if rec, _ := f.ledg.Receivables(f.ctx, f.staff, f.org, ledger.ReportQuery{AsOf: "2026-11-19", Side: ledger.TypePayable}); len(rec.Currencies) != 0 {
		t.Fatalf("payables have no rent: %+v", rec)
	}
}

func TestTenantRentViews(t *testing.T) {
	f := newRentFixture(t)
	f.accounts[f.consumer.UserID] = account{email: "tia@example.test", verified: true}
	mine := f.active("2026-09-01", monthly950)
	f.receipt(mine.ID, `{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-09-02","reference":"FNB 1"}`)
	// Somebody else's tenancy of the same book.
	p := f.property()
	in := body[TenancyInput](t, `{"property_id":"`+p.ID.String()+`","tenants":["`+f.tenant2.String()+`"],"rent":`+monthly950+`,"start_date":"2026-09-01"}`)
	theirs, _ := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in)
	if _, err := f.svc.Activate(f.ctx, f.agent, f.org, theirs.ID); err != nil {
		t.Fatal(err)
	}
	dues, err := f.svc.MyDues(f.ctx, f.consumer, mine.ID, DueFilter{Status: DueDue})
	if err != nil || len(dues.Items) != 14 || dues.Items[0].DueDate != "2026-10-01" || dues.Summary.Paid != "950.00" || dues.Schedule == nil {
		t.Fatalf("my dues: %+v %v", dues, err)
	}
	recs, err := f.svc.MyReceipts(f.ctx, f.consumer, mine.ID)
	if err != nil || len(recs) != 1 || recs[0].Reference != "FNB 1" || recs[0].Allocations[0].DueDate != "2026-09-01" || recs[0].ReversedOn != nil {
		t.Fatalf("my receipts: %+v %v", recs, err)
	}
	raw, _ := json.Marshal(map[string]any{"dues": dues, "receipts": recs})
	for _, leak := range []string{`"can"`, "ledger_entry_id", "recorded_by", f.tenant.String(), "Ada Agent"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("the tenant view leaks %q: %s", leak, raw)
		}
	}
	st, err := f.svc.MyStatement(f.ctx, f.consumer, mine.ID, "", "")
	if err != nil || len(st.Currencies) != 1 || st.Currencies[0].ClosingBalance != "1900.00" || st.Tenancy.Tenants[0].Name != "Tia Tenant" {
		t.Fatalf("my statement: %+v %v", st, err)
	}
	// Another tenant's tenancy, an unverified account and a DRAFT are 404.
	for _, id := range []uuid.UUID{theirs.ID, uuid.New()} {
		if _, err := f.svc.MyDues(f.ctx, f.consumer, id, DueFilter{}); !errors.Is(err, ErrTenancyNotFound) {
			t.Fatalf("dues of another tenancy: %v", err)
		}
		if _, err := f.svc.MyReceipts(f.ctx, f.consumer, id); !errors.Is(err, ErrTenancyNotFound) {
			t.Fatalf("receipts of another tenancy: %v", err)
		}
		if _, err := f.svc.MyStatement(f.ctx, f.consumer, id, "", ""); !errors.Is(err, ErrTenancyNotFound) {
			t.Fatalf("statement of another tenancy: %v", err)
		}
	}
	f.accounts[f.consumer.UserID] = account{email: "tia@example.test", verified: false}
	if _, err := f.svc.MyDues(f.ctx, f.consumer, mine.ID, DueFilter{}); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("unverified: %v", err)
	}
	if _, err := f.svc.MyDues(f.ctx, nil, mine.ID, DueFilter{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("signed out: %v", err)
	}
}

// Account deletion unlinks the person as who recorded a receipt or waived a due.
func TestForgetUserRent(t *testing.T) {
	f := newRentFixture(t)
	ten := f.active("2026-09-01", monthly950)
	res := f.receipt(ten.ID, `{"amount":"950.00","currency":"USD","method":"BANK","received_on":"2026-09-02"}`)
	due := f.dues(ten.ID).Items[1]
	if _, err := f.svc.WaiveDue(f.ctx, f.agent, f.org, due.ID, WaiveInput{Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	f.store.ForgetUser(f.agent.UserID, "Deleted account")
	list, _ := f.svc.ListReceipts(f.ctx, f.owner, f.org, ten.ID)
	got, _ := f.store.Due(f.ctx, due.ID)
	if list[0].ID != res.Receipt.ID || list[0].RecordedBy.UserID != nil || list[0].RecordedBy.Name != "Ada Agent" || got.WaivedBy != nil {
		t.Fatalf("not unlinked: %+v %+v", list[0].RecordedBy, got)
	}
}

// TestMarketDateAfterLocalMidnight: at 22:30 UTC it is already the next day
// in Harare, so a receipt dated that day is not "in the future", the day
// after still is, and the arrears default to the market's date.
func TestMarketDateAfterLocalMidnight(t *testing.T) {
	f := newRentFixture(t)
	f.svc.zone = func(_ context.Context, code string) string {
		if code == "ZW" {
			return "Africa/Harare"
		}
		return ""
	}
	ten := f.active("2026-09-01", monthly950)
	f.now = time.Date(2026, 11, 21, 22, 30, 0, 0, time.UTC)
	res := f.receipt(ten.ID, `{"amount":"10.00","currency":"USD","method":"CASH","received_on":"2026-11-22"}`)
	if res.Receipt.ReceivedOn != "2026-11-22" {
		t.Fatalf("received_on: %+v", res.Receipt)
	}
	_, err := f.svc.RecordReceipt(f.ctx, f.agent, f.org, ten.ID,
		body[ReceiptInput](t, `{"amount":"10.00","currency":"USD","method":"CASH","received_on":"2026-11-23"}`))
	var v *ValidationError
	if !errors.As(err, &v) || v.Fields["received_on"] != "may not be in the future" {
		t.Fatalf("day after: %v", err)
	}
	if got := f.svc.PropertyToday(f.ctx, ten.Property.ID); got != "2026-11-22" {
		t.Fatalf("PropertyToday = %s", got)
	}
	ar, err := f.svc.Arrears(f.ctx, f.agent, f.org, ArrearsQuery{})
	if err != nil || ar.AsOf != "2026-11-22" {
		t.Fatalf("arrears as_of %q: %v", ar.AsOf, err)
	}
	// Without a zone lookup the UTC date still applies.
	f.svc.zone = nil
	if got := f.svc.PropertyToday(f.ctx, ten.Property.ID); got != "2026-11-21" {
		t.Fatalf("UTC fallback = %s", got)
	}
}
