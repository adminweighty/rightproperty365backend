package tenancies

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// ---------------------------------------------------------------------------
// The schedule
// ---------------------------------------------------------------------------

// resync adds to ch what makes the tenancy's schedule match its rule after
// the change (t is the tenancy as it will be): missing dues, and dues after
// the end with no allocation and no waiver removed.
func (s *Service) resync(ctx context.Context, ch *Change, t Tenancy) error {
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return err
	}
	add, remove := planSchedule(t, dues, s.today(), s.clock())
	ch.Dues, ch.DeleteDues = append(ch.Dues, add...), append(ch.DeleteDues, remove...)
	return nil
}

// ExtendSchedules re-runs the rule of every ACTIVE tenancy (the worker,
// daily): a rolling schedule gains the dues the horizon now reaches, and a
// tenancy activated before part b gets its schedule. Idempotent. It
// returns the number of dues added.
func (s *Service) ExtendSchedules(ctx context.Context) (int, error) {
	list, err := s.store.ActiveTenancies(ctx)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, t := range list {
		var ch Change
		if err := s.resync(ctx, &ch, t); err != nil {
			return added, err
		}
		if len(ch.Dues) == 0 && len(ch.DeleteDues) == 0 {
			continue
		}
		ch.ExpectStatus = map[uuid.UUID]string{t.ID: StatusActive}
		err := s.store.Apply(ctx, ch)
		if errors.Is(err, ErrConflict) {
			continue // ended meanwhile: ending re-ran the rule
		}
		if err != nil {
			return added, err
		}
		added += len(ch.Dues)
	}
	return added, nil
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func daysBetween(from, to string) int {
	return int(parseDay(to).Sub(parseDay(from)).Hours() / 24)
}

func zero(cur string) string { return money.Format(new(big.Rat), cur) }

func dueCore(t Tenancy, d Due, today string) DueCore {
	st := d.state(forever)
	out := DueCore{ID: d.ID, TenancyID: d.TenancyID, Sequence: d.Sequence, DueDate: d.DueDate,
		Period: PeriodView{From: d.DueDate, To: PeriodEnd(t, d.Sequence, d.DueDate)}, Amount: d.Amount, Currency: d.Currency,
		MinorUnit: money.MinorUnit(d.Currency), Paid: money.Format(st.paid, d.Currency), Waived: money.Format(st.waived, d.Currency),
		Outstanding: money.Format(st.outstanding, d.Currency), Status: st.status, StatusLabel: DueStatusLabel(st.status)}
	if d.DueDate < today && st.outstanding.Sign() > 0 {
		out.Overdue, out.DaysOverdue = true, daysBetween(d.DueDate, today)
	}
	return out
}

func dueView(a access, t Tenancy, d Due, today string) DueView {
	core := dueCore(t, d, today)
	v := DueView{DueCore: core, Can: DueCan{Waive: a.write && (core.Status == DueDue || core.Status == DuePartPaid)}}
	if d.WaivedAt != nil {
		v.Waiver = &WaiverView{At: *d.WaivedAt, By: PersonRef{UserID: d.WaivedBy, Name: d.WaivedByName}, Reason: d.WaiveReason}
	}
	return v
}

func myDueView(t Tenancy, d Due, today string) MyDueView {
	v := MyDueView{DueCore: dueCore(t, d, today)}
	if d.WaivedAt != nil {
		v.Waiver = &MyWaiverView{At: *d.WaivedAt}
	}
	return v
}

// summary computes a schedule's figures today; next is the index of the
// next due (-1 when none).
func summary(t Tenancy, dues []Due, today string) (SummaryCore, int) {
	cur := t.RentCurrency
	charged, paid, waived, overdue, advance := new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat), new(big.Rat)
	count, next := 0, -1
	for i, d := range dues {
		st := d.state(forever)
		switch {
		case d.DueDate <= today:
			charged.Add(charged, money.Rat(d.Amount))
			paid.Add(paid, st.paid)
			waived.Add(waived, st.waived)
			if d.DueDate < today && st.outstanding.Sign() > 0 {
				overdue.Add(overdue, st.outstanding)
				count++
			}
		default:
			advance.Add(advance, st.paid)
		}
		if next < 0 && d.DueDate >= today && st.outstanding.Sign() > 0 {
			next = i
		}
	}
	outstanding := new(big.Rat).Sub(charged, paid)
	outstanding.Sub(outstanding, waived)
	if outstanding.Sign() < 0 {
		outstanding.SetInt64(0)
	}
	return SummaryCore{Currency: cur, MinorUnit: money.MinorUnit(cur), Charged: money.Format(charged, cur), Paid: money.Format(paid, cur),
		Waived: money.Format(waived, cur), Outstanding: money.Format(outstanding, cur), Overdue: money.Format(overdue, cur), OverdueCount: count,
		PaidInAdvance: money.Format(advance, cur)}, next
}

func scheduleView(t Tenancy, dues []Due, today string) *ScheduleView {
	if t.Status == StatusDraft {
		return nil
	}
	through, rolling := ScheduleThrough(t, today)
	return &ScheduleView{Rolling: rolling, Through: through, Count: len(dues), Rule: RuleText(t.RentFrequency, t.RentDueDay)}
}

// DueFilter narrows the items of GET .../dues.
type DueFilter struct {
	Status   string
	From, To string
}

func (f DueFilter) match(core DueCore) bool {
	switch {
	case f.Status != "" && core.Status != f.Status,
		f.From != "" && core.DueDate < f.From,
		f.To != "" && core.DueDate > f.To:
		return false
	}
	return true
}

func (v *viewer) brief(ctx context.Context, t Tenancy) (TenancyBrief, error) {
	prop := v.properties[t.PropertyID]
	out := TenancyBrief{ID: t.ID, Status: t.Status, StatusLabel: StatusLabel(t.Status),
		Property: BriefProperty{ID: t.PropertyID, Name: prop.Name, Archived: prop.DeletedAt != nil}}
	if t.UnitID != nil {
		u, _ := prop.Unit(*t.UnitID)
		out.Unit = &TenancyUnitRef{ID: *t.UnitID, Label: u.Label, Archived: u.DeletedAt != nil}
	}
	var err error
	out.Tenants, err = v.parties(ctx, t, RoleTenant)
	return out, err
}

func methodLabel(m string) string { return ledger.MethodLabel(m) }

func allocationViews(r Receipt, dates map[uuid.UUID]string) []AllocationView {
	out := make([]AllocationView, 0, len(r.Allocations))
	for _, a := range r.Allocations {
		out = append(out, AllocationView{DueID: a.DueID, DueDate: dates[a.DueID], Amount: a.Amount, Currency: r.Currency,
			MinorUnit: money.MinorUnit(r.Currency)})
	}
	return out
}

func receiptStatus(r Receipt) string {
	if r.Reversed() {
		return ReceiptReversed
	}
	return ReceiptRecorded
}

// mayReverse: the ledger's rule for reversing the receipt's entry.
func (a access) mayReverse(r Receipt) bool {
	if r.Reversed() || !a.write {
		return false
	}
	if a.book.OrganisationID == nil {
		return true
	}
	return a.p.MayCorrectLedgerEntry(*a.book.OrganisationID, r.LedgerAuthor, r.LedgerStatus == ledger.StatusReconciled)
}

func (v *viewer) receipt(ctx context.Context, a access, t Tenancy, r Receipt, dates map[uuid.UUID]string) (ReceiptView, error) {
	status := receiptStatus(r)
	ls := r.LedgerStatus
	if ls == "" {
		ls = ledger.StatusUnverified
	}
	out := ReceiptView{ID: r.ID, TenancyID: r.TenancyID, Amount: r.Amount, Currency: r.Currency, MinorUnit: money.MinorUnit(r.Currency),
		Method: r.Method, MethodLabel: methodLabel(r.Method), ReceivedOn: r.ReceivedOn, Reference: r.Reference, Status: status,
		StatusLabel: receiptLabels[status], Allocations: allocationViews(r, dates), LedgerEntryID: r.LedgerEntryID, LedgerStatus: ls,
		LedgerStatusLabel: ledger.StatusLabel(ls), RecordedBy: PersonRef{UserID: r.RecordedBy, Name: r.RecordedByName}, RecordedAt: r.RecordedAt,
		Can: ReceiptCan{Reverse: a.mayReverse(r)}}
	if r.Reversed() && r.ReversalEntryID != nil {
		out.Reversal = &ReceiptReversalView{LedgerEntryID: *r.ReversalEntryID, ReversedOn: r.ReversedOn, At: *r.ReversedAt}
	}
	if r.ContactID != nil {
		ref := ContactRef{ID: *r.ContactID, Name: r.ContactName}
		c, err := v.contact(ctx, t.Book, *r.ContactID)
		if err != nil {
			return ReceiptView{}, err
		}
		if c != nil {
			ref.Name, ref.Live = c.Name, true
		}
		out.Tenant = &ref
	}
	return out, nil
}

func myReceipt(r Receipt, dates map[uuid.UUID]string) MyReceiptView {
	status := receiptStatus(r)
	return MyReceiptView{ID: r.ID, Amount: r.Amount, Currency: r.Currency, MinorUnit: money.MinorUnit(r.Currency), Method: r.Method,
		MethodLabel: methodLabel(r.Method), ReceivedOn: r.ReceivedOn, Reference: r.Reference, Status: status, StatusLabel: receiptLabels[status],
		Allocations: allocationViews(r, dates), ReversedOn: strPtr(r.ReversedOn)}
}

func dueDates(dues []Due) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, d := range dues {
		out[d.ID] = d.DueDate
	}
	return out
}

// ---------------------------------------------------------------------------
// Dues
// ---------------------------------------------------------------------------

func (s *Service) duesPage(a access, t Tenancy, dues []Due, f DueFilter) DuesPage {
	today := s.today()
	core, next := summary(t, dues, today)
	out := DuesPage{Items: []DueView{}, Summary: DueSummary{SummaryCore: core}, Schedule: scheduleView(t, dues, today)}
	for i, d := range dues {
		v := dueView(a, t, d, today)
		if i == next {
			nv := v
			out.Summary.NextDue = &nv
		}
		if f.match(v.DueCore) {
			out.Items = append(out.Items, v)
		}
	}
	return out
}

// ListDues serves GET .../tenancies/{tenancyId}/dues.
func (s *Service) ListDues(ctx context.Context, p *auth.Principal, bookID, tenancyID uuid.UUID, f DueFilter) (DuesPage, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return DuesPage{}, err
	}
	t, err := s.tenancy(ctx, a, tenancyID)
	if err != nil {
		return DuesPage{}, err
	}
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return DuesPage{}, err
	}
	return s.duesPage(a, t, dues, f), nil
}

// WaiveInput is the body of POST .../dues/{dueId}/waive.
type WaiveInput struct {
	Reason string `json:"reason"`
}

// WaiveDue serves POST .../dues/{dueId}/waive: what is outstanding is forgiven.
func (s *Service) WaiveDue(ctx context.Context, p *auth.Principal, bookID, dueID uuid.UUID, in WaiveInput) (DueView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return DueView{}, err
	}
	d, err := s.store.Due(ctx, dueID)
	if err != nil {
		return DueView{}, err
	}
	t, err := s.tenancy(ctx, a, d.TenancyID)
	if errors.Is(err, ErrTenancyNotFound) {
		return DueView{}, ErrDueNotFound
	}
	if err != nil {
		return DueView{}, err
	}
	st := d.state(forever)
	if st.status == DuePaid || st.status == DueWaived {
		return DueView{}, &NotWaivableError{Status: st.status}
	}
	f := map[string]string{}
	reason := text(f, "reason", in.Reason, 1, MaxWaiveReason)
	if err := invalid(f); err != nil {
		return DueView{}, err
	}
	prop, err := s.store.Property(ctx, t.PropertyID)
	if err != nil {
		return DueView{}, err
	}
	now := s.clock()
	before := map[string]any{"due_id": d.ID.String(), "due_date": d.DueDate, "amount": d.Amount, "currency": d.Currency,
		"outstanding": money.Format(st.outstanding, d.Currency), "status": st.status}
	after := map[string]any{"status": DueWaived, "waived": money.Format(st.outstanding, d.Currency), "currency": d.Currency, "reason": reason}
	ch := Change{ExpectStatus: map[uuid.UUID]string{t.ID: t.Status},
		Waive: &Waiver{DueID: d.ID, At: now, By: a.actor(), ByName: a.p.Name, Reason: reason},
		Audit: s.event(ctx, a, orgaudit.ActionRentDueWaived, t.ID, Label(prop, t), before, after)}
	if err := s.store.Apply(ctx, ch); err != nil {
		return DueView{}, err
	}
	got, err := s.store.Due(ctx, d.ID)
	if err != nil {
		return DueView{}, err
	}
	return dueView(a, t, got, s.today()), nil
}

// ---------------------------------------------------------------------------
// Receipts
// ---------------------------------------------------------------------------

// AllocationInput is one line of a receipt body.
type AllocationInput struct {
	DueID  *uuid.UUID `json:"due_id"`
	Amount *string    `json:"amount"`
}

// ReceiptInput is the body of POST .../tenancies/{tenancyId}/receipts.
type ReceiptInput struct {
	Amount          *string           `json:"amount"`
	Currency        string            `json:"currency"`
	Method          string            `json:"method"`
	ReceivedOn      string            `json:"received_on"`
	Reference       *string           `json:"reference"`
	TenantContactID *uuid.UUID        `json:"tenant_contact_id"`
	Allocations     []AllocationInput `json:"allocations"`
}

// allocate spreads a receipt over the dues: the explicit lines, or oldest
// due first. Nothing is allocated above a due's outstanding amount.
func allocate(f map[string]string, dues []Due, total *big.Rat, cur string, lines []AllocationInput) []Allocation {
	byID := map[uuid.UUID]Due{}
	left := map[uuid.UUID]*big.Rat{}
	all := new(big.Rat)
	for _, d := range dues {
		byID[d.ID] = d
		st := d.state(forever)
		left[d.ID] = st.outstanding
		if st.status != DueWaived {
			all.Add(all, st.outstanding)
		}
	}
	var out []Allocation
	if len(lines) == 0 {
		if total.Cmp(all) > 0 {
			f["amount"] = fmt.Sprintf("is more than the %s outstanding on this tenancy's schedule", money.Format(all, cur))
			return nil
		}
		rest := new(big.Rat).Set(total)
		for _, d := range dues { // oldest first (the stores order by due date)
			if rest.Sign() <= 0 {
				break
			}
			o := left[d.ID]
			if o.Sign() <= 0 {
				continue
			}
			take := new(big.Rat).Set(o)
			if rest.Cmp(o) < 0 {
				take.Set(rest)
			}
			rest.Sub(rest, take)
			out = append(out, Allocation{DueID: d.ID, Amount: money.Format(take, cur)})
		}
		return out
	}
	if len(lines) > MaxAllocations {
		f["allocations"] = fmt.Sprintf("may hold at most %d lines", MaxAllocations)
		return nil
	}
	sum := new(big.Rat)
	seen := map[uuid.UUID]bool{}
	for i, l := range lines {
		key := fmt.Sprintf("allocations[%d]", i)
		if l.DueID == nil {
			f[key+".due_id"] = "is required"
			continue
		}
		d, ok := byID[*l.DueID]
		switch {
		case !ok:
			f[key+".due_id"] = "is not a due of this tenancy"
			continue
		case seen[d.ID]:
			f[key+".due_id"] = "is listed twice"
			continue
		case d.state(forever).status == DueWaived:
			f[key+".due_id"] = "is waived"
			continue
		}
		seen[d.ID] = true
		amt := amount(f, key+".amount", l.Amount, cur)
		if amt == "" {
			continue
		}
		r := money.Rat(amt)
		if r.Cmp(left[d.ID]) > 0 {
			f[key+".amount"] = fmt.Sprintf("is more than the %s outstanding on this due", money.Format(left[d.ID], cur))
			continue
		}
		sum.Add(sum, r)
		out = append(out, Allocation{DueID: d.ID, Amount: amt})
	}
	if len(f) == 0 && sum.Cmp(total) != 0 {
		f["allocations"] = fmt.Sprintf("must add up to the amount (%s)", money.Format(total, cur))
	}
	// Oldest due first, as the stores read them.
	sort.SliceStable(out, func(i, j int) bool { return byID[out[i].DueID].DueDate < byID[out[j].DueID].DueDate })
	return out
}

// ledgerFields renames the ledger's validation fields for the receipt body.
func ledgerFields(err error) error {
	var lv *ledger.ValidationError
	switch {
	case errors.As(err, &lv):
		out := map[string]string{}
		for k, v := range lv.Fields {
			out["ledger."+k] = v
		}
		return &ValidationError{Fields: out}
	case errors.Is(err, ledger.ErrForbidden):
		return ErrForbidden
	case errors.Is(err, ledger.ErrBookNotFound):
		return ErrBookNotFound
	case errors.Is(err, ledger.ErrUnauthenticated):
		return ErrUnauthenticated
	}
	return err
}

// RecordReceipt serves POST .../tenancies/{tenancyId}/receipts: the receipt,
// its allocations and its RENT_RECEIVED ledger entry in one transaction.
func (s *Service) RecordReceipt(ctx context.Context, p *auth.Principal, bookID, tenancyID uuid.UUID, in ReceiptInput) (ReceiptResult, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return ReceiptResult{}, err
	}
	t, err := s.tenancy(ctx, a, tenancyID)
	if err != nil {
		return ReceiptResult{}, err
	}
	if t.Status == StatusDraft {
		return ReceiptResult{}, &StateError{Status: t.Status}
	}
	if s.ledger == nil {
		return ReceiptResult{}, ErrNoLedger
	}
	f := map[string]string{}
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	switch {
	case cur == "":
		f["currency"] = "is required"
	case cur != t.RentCurrency:
		f["currency"] = "must be the rent currency (" + t.RentCurrency + ")"
	}
	amt := amount(f, "amount", in.Amount, t.RentCurrency)
	method := strings.TrimSpace(in.Method)
	if !slices.Contains(ledger.Methods(), method) {
		f["method"] = "must be CASH, BANK, MOBILE_MONEY or OTHER"
	}
	today := s.today()
	received := strings.TrimSpace(in.ReceivedOn)
	switch {
	case !validDate(received):
		f["received_on"] = "must be a date (YYYY-MM-DD)"
	case received < t.StartDate:
		f["received_on"] = "must be on or after the tenancy's start date (" + t.StartDate + ")"
	case received > today:
		f["received_on"] = "may not be in the future"
	}
	ref := ""
	if in.Reference != nil {
		ref = text(f, "reference", *in.Reference, 0, MaxReceiptReference)
	}
	if in.TenantContactID != nil && !t.HasTenant(*in.TenantContactID) {
		f["tenant_contact_id"] = "is not a tenant of this tenancy"
	}
	if err := invalid(f); err != nil {
		return ReceiptResult{}, err
	}
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return ReceiptResult{}, err
	}
	allocs := allocate(f, dues, money.Rat(amt), t.RentCurrency, in.Allocations)
	if err := invalid(f); err != nil {
		return ReceiptResult{}, err
	}

	// The tenant contact (a live one for the ledger link).
	v, err := s.viewer(ctx, a.book, []Tenancy{t})
	if err != nil {
		return ReceiptResult{}, err
	}
	var contactID *uuid.UUID
	contactName := ""
	for _, party := range t.Parties {
		if party.Role != RoleTenant || (in.TenantContactID != nil && party.ContactID != *in.TenantContactID) {
			continue
		}
		c, err := v.contact(ctx, t.Book, party.ContactID)
		if err != nil {
			return ReceiptResult{}, err
		}
		if c == nil {
			if in.TenantContactID != nil {
				f["tenant_contact_id"] = "is no longer a live contact of these books (merged or deleted)"
			}
			continue
		}
		id := party.ContactID
		contactID, contactName = &id, c.Name
		break
	}
	if err := invalid(f); err != nil {
		return ReceiptResult{}, err
	}
	prop := v.properties[t.PropertyID]
	var listingID, leaseID *uuid.UUID
	if prop.ListingID != nil && s.links != nil {
		if _, ok, err := s.links.Listing(ctx, a.book, *prop.ListingID); err != nil {
			return ReceiptResult{}, err
		} else if ok {
			listingID = prop.ListingID
		}
	}
	if t.LeaseID != nil && s.links != nil {
		if _, ok, err := s.links.Lease(ctx, a.book, *t.LeaseID); err != nil {
			return ReceiptResult{}, err
		} else if ok {
			leaseID = t.LeaseID
		}
	}
	label := Label(prop, t)
	lch, entry, err := s.ledger.PrepareRentReceipt(ctx, p, bookID, ledger.RentReceipt{Amount: amt, Currency: t.RentCurrency, Method: method,
		EffectiveDate: received, Reference: ref, Description: "Rent: " + label, TenancyID: t.ID, ContactID: contactID, ListingID: listingID,
		LeaseID: leaseID})
	if err != nil {
		return ReceiptResult{}, ledgerFields(err)
	}
	now := s.clock()
	rec := Receipt{ID: newID(), TenancyID: t.ID, Amount: amt, Currency: t.RentCurrency, Method: method, ReceivedOn: received, Reference: ref,
		ContactID: contactID, ContactName: contactName, LedgerEntryID: entry.ID, RecordedBy: a.actor(), RecordedByName: a.p.Name, RecordedAt: now,
		Allocations: allocs}
	lines := make([]map[string]any, 0, len(allocs))
	for _, x := range allocs {
		lines = append(lines, map[string]any{"due_id": x.DueID.String(), "amount": x.Amount})
	}
	after := map[string]any{"receipt_id": rec.ID.String(), "amount": amt, "currency": rec.Currency, "method": method, "received_on": received,
		"ledger_entry_id": entry.ID.String(), "allocations": lines}
	ch := Change{ExpectStatus: map[uuid.UUID]string{t.ID: t.Status}, Ledger: &lch, Receipts: []Receipt{rec},
		Audit: s.event(ctx, a, orgaudit.ActionRentReceiptRecorded, t.ID, label, nil, after)}
	if err := s.store.Apply(ctx, ch); err != nil {
		return ReceiptResult{}, err
	}
	return s.receiptResult(ctx, a, t, v, rec.ID)
}

func (s *Service) receiptResult(ctx context.Context, a access, t Tenancy, v *viewer, id uuid.UUID) (ReceiptResult, error) {
	recs, err := s.store.Receipts(ctx, t.ID)
	if err != nil {
		return ReceiptResult{}, err
	}
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return ReceiptResult{}, err
	}
	i := slices.IndexFunc(recs, func(r Receipt) bool { return r.ID == id })
	if i < 0 {
		return ReceiptResult{}, ErrReceiptNotFound
	}
	rv, err := v.receipt(ctx, a, t, recs[i], dueDates(dues))
	if err != nil {
		return ReceiptResult{}, err
	}
	out := ReceiptResult{Receipt: rv, Dues: []DueView{}}
	today := s.today()
	for _, d := range dues {
		if slices.ContainsFunc(recs[i].Allocations, func(x Allocation) bool { return x.DueID == d.ID }) {
			out.Dues = append(out.Dues, dueView(a, t, d, today))
		}
	}
	return out, nil
}

// ListReceipts serves GET .../tenancies/{tenancyId}/receipts.
func (s *Service) ListReceipts(ctx context.Context, p *auth.Principal, bookID, tenancyID uuid.UUID) ([]ReceiptView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return nil, err
	}
	t, err := s.tenancy(ctx, a, tenancyID)
	if err != nil {
		return nil, err
	}
	recs, err := s.store.Receipts(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return nil, err
	}
	v, err := s.viewer(ctx, a.book, []Tenancy{t})
	if err != nil {
		return nil, err
	}
	dates := dueDates(dues)
	out := make([]ReceiptView, 0, len(recs))
	for _, r := range recs {
		rv, err := v.receipt(ctx, a, t, r, dates)
		if err != nil {
			return nil, err
		}
		out = append(out, rv)
	}
	return out, nil
}

// GetReceipt serves GET .../tenancies/{tenancyId}/receipts/{receiptId}.
func (s *Service) GetReceipt(ctx context.Context, p *auth.Principal, bookID, tenancyID, id uuid.UUID) (ReceiptView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return ReceiptView{}, err
	}
	t, err := s.tenancy(ctx, a, tenancyID)
	if err != nil {
		return ReceiptView{}, err
	}
	v, err := s.viewer(ctx, a.book, []Tenancy{t})
	if err != nil {
		return ReceiptView{}, err
	}
	res, err := s.receiptResult(ctx, a, t, v, id)
	if err != nil {
		return ReceiptView{}, err
	}
	return res.Receipt, nil
}

// ---------------------------------------------------------------------------
// Arrears
// ---------------------------------------------------------------------------

// ArrearsQuery is the query of GET .../arrears.
type ArrearsQuery struct {
	AsOf       string
	Currency   string
	PropertyID *uuid.UUID
}

// arrear is one overdue due as of a date.
type arrear struct {
	t    Tenancy
	d    Due
	st   dueState
	days int
}

// Ageing buckets (as the ledger's).
var bucketNames = []string{"0-30", "31-60", "61-90", "90+"}

// BucketOf returns the ageing bucket of a number of days overdue (1-30 is "0-30").
func BucketOf(days int) string {
	switch {
	case days <= 30:
		return "0-30"
	case days <= 60:
		return "31-60"
	case days <= 90:
		return "61-90"
	}
	return "90+"
}

// bookArrears returns the overdue dues of a book's tenancies (not DRAFT) as
// of asOf: dated before it, not waived by it, with something outstanding
// counting the receipts received by it and not reversed by it.
func (s *Service) bookArrears(ctx context.Context, book Book, asOf string) ([]arrear, *viewer, error) {
	list, _, _, err := s.store.Tenancies(ctx, book, TenancyFilter{})
	if err != nil {
		return nil, nil, err
	}
	byID := map[uuid.UUID]Tenancy{}
	var ids []uuid.UUID
	var kept []Tenancy
	for _, t := range list {
		if t.Status == StatusDraft {
			continue
		}
		byID[t.ID] = t
		ids = append(ids, t.ID)
		kept = append(kept, t)
	}
	v, err := s.viewer(ctx, book, kept)
	if err != nil {
		return nil, nil, err
	}
	if len(ids) == 0 {
		return nil, v, nil
	}
	dues, err := s.store.Dues(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	var out []arrear
	for _, d := range dues {
		if d.DueDate >= asOf {
			continue
		}
		st := d.state(asOf)
		if st.status == DueWaived || st.outstanding.Sign() <= 0 {
			continue
		}
		out = append(out, arrear{t: byID[d.TenancyID], d: d, st: st, days: daysBetween(d.DueDate, asOf)})
	}
	return out, v, nil
}

func emptyBuckets(cur string) []BucketView {
	out := make([]BucketView, 0, len(bucketNames))
	for _, b := range bucketNames {
		out = append(out, BucketView{Bucket: b, Currency: cur, MinorUnit: money.MinorUnit(cur), Outstanding: zero(cur)})
	}
	return out
}

func addBucket(list []BucketView, days int, v *big.Rat, sums map[string]*big.Rat) {
	b := BucketOf(days)
	for i := range list {
		if list[i].Bucket != b {
			continue
		}
		if sums[b] == nil {
			sums[b] = new(big.Rat)
		}
		sums[b].Add(sums[b], v)
		list[i].Outstanding = money.Format(sums[b], list[i].Currency)
		list[i].Count++
	}
}

// Arrears serves GET .../arrears: the ageing of the book's overdue rent, per
// currency, by tenancy.
func (s *Service) Arrears(ctx context.Context, p *auth.Principal, bookID uuid.UUID, q ArrearsQuery) (Arrears, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Arrears{}, err
	}
	f := map[string]string{}
	if q.AsOf == "" {
		q.AsOf = s.today()
	} else if !validDate(q.AsOf) {
		f["as_of"] = "must be a date (YYYY-MM-DD)"
	}
	q.Currency = strings.ToUpper(strings.TrimSpace(q.Currency))
	if q.Currency != "" && !money.ValidCurrencyCode(q.Currency) {
		f["currency"] = "must be an ISO 4217 currency code, such as USD"
	}
	if err := invalid(f); err != nil {
		return Arrears{}, err
	}
	list, v, err := s.bookArrears(ctx, a.book, q.AsOf)
	if err != nil {
		return Arrears{}, err
	}
	out := Arrears{AsOf: q.AsOf, Currencies: []ArrearsCurrency{}}
	type group struct {
		cur     *ArrearsCurrency
		sums    map[string]*big.Rat
		total   *big.Rat
		rows    map[uuid.UUID]*ArrearsTenancy
		rowSums map[uuid.UUID]map[string]*big.Rat
		rowTot  map[uuid.UUID]*big.Rat
	}
	groups := map[string]*group{}
	var codes []string
	sort.SliceStable(list, func(i, j int) bool { return list[i].d.DueDate < list[j].d.DueDate })
	for _, x := range list {
		cur := x.d.Currency
		if (q.Currency != "" && cur != q.Currency) || (q.PropertyID != nil && x.t.PropertyID != *q.PropertyID) {
			continue
		}
		g := groups[cur]
		if g == nil {
			g = &group{cur: &ArrearsCurrency{Currency: cur, MinorUnit: money.MinorUnit(cur), Buckets: emptyBuckets(cur)}, sums: map[string]*big.Rat{},
				total: new(big.Rat), rows: map[uuid.UUID]*ArrearsTenancy{}, rowSums: map[uuid.UUID]map[string]*big.Rat{}, rowTot: map[uuid.UUID]*big.Rat{}}
			groups[cur] = g
			codes = append(codes, cur)
		}
		addBucket(g.cur.Buckets, x.days, x.st.outstanding, g.sums)
		g.total.Add(g.total, x.st.outstanding)
		g.cur.Count++
		row := g.rows[x.t.ID]
		if row == nil {
			brief, err := v.brief(ctx, x.t)
			if err != nil {
				return Arrears{}, err
			}
			row = &ArrearsTenancy{Tenancy: brief, Currency: cur, MinorUnit: money.MinorUnit(cur), OldestDueDate: x.d.DueDate, DaysOverdue: x.days,
				Buckets: emptyBuckets(cur)}
			g.rows[x.t.ID], g.rowSums[x.t.ID], g.rowTot[x.t.ID] = row, map[string]*big.Rat{}, new(big.Rat)
		}
		addBucket(row.Buckets, x.days, x.st.outstanding, g.rowSums[x.t.ID])
		g.rowTot[x.t.ID].Add(g.rowTot[x.t.ID], x.st.outstanding)
		row.Outstanding = money.Format(g.rowTot[x.t.ID], cur)
		row.Count++
	}
	slices.Sort(codes)
	for _, cur := range codes {
		g := groups[cur]
		g.cur.Outstanding = money.Format(g.total, cur)
		g.cur.Tenancies = []ArrearsTenancy{}
		for _, r := range g.rows {
			g.cur.Tenancies = append(g.cur.Tenancies, *r)
		}
		sort.SliceStable(g.cur.Tenancies, func(i, j int) bool {
			a, b := g.cur.Tenancies[i], g.cur.Tenancies[j]
			if a.DaysOverdue != b.DaysOverdue {
				return a.DaysOverdue > b.DaysOverdue
			}
			if c := money.Rat(a.Outstanding).Cmp(money.Rat(b.Outstanding)); c != 0 {
				return c > 0
			}
			return a.Tenancy.ID.String() < b.Tenancy.ID.String()
		})
		out.Currencies = append(out.Currencies, *g.cur)
	}
	return out, nil
}

// LedgerArrears feeds the ledger's receivables report (ledger.Deps
// RentArrears): the book's rent arrears as of a date.
func (s *Service) LedgerArrears(ctx context.Context, book ledger.Book, asOf string) ([]ledger.RentArrear, error) {
	list, v, err := s.bookArrears(ctx, Book{OrganisationID: book.OrganisationID, UserID: book.UserID}, asOf)
	if err != nil {
		return nil, err
	}
	out := make([]ledger.RentArrear, 0, len(list))
	for _, x := range list {
		brief, err := v.brief(ctx, x.t)
		if err != nil {
			return nil, err
		}
		cur := x.d.Currency
		out = append(out, ledger.RentArrear{Tenancy: brief, Tenants: x.t.PartyIDs(RoleTenant), DueID: x.d.ID, DueDate: x.d.DueDate, Currency: cur,
			Amount: x.d.Amount, Settled: money.Format(x.st.paid, cur), Outstanding: money.Format(x.st.outstanding, cur)})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The tenant's own dues and receipts
// ---------------------------------------------------------------------------

func (s *Service) myTenancy(ctx context.Context, p *auth.Principal, id uuid.UUID) (Tenancy, error) {
	list, err := s.mine(ctx, p)
	if err != nil {
		return Tenancy{}, err
	}
	for _, t := range list {
		if t.ID == id {
			return t, nil
		}
	}
	return Tenancy{}, ErrTenancyNotFound
}

// MyDues serves GET /v1/me/tenancies/{id}/dues.
func (s *Service) MyDues(ctx context.Context, p *auth.Principal, id uuid.UUID, f DueFilter) (MyDuesPage, error) {
	t, err := s.myTenancy(ctx, p, id)
	if err != nil {
		return MyDuesPage{}, err
	}
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return MyDuesPage{}, err
	}
	today := s.today()
	core, next := summary(t, dues, today)
	out := MyDuesPage{Items: []MyDueView{}, Summary: MyDueSummary{SummaryCore: core}, Schedule: scheduleView(t, dues, today)}
	for i, d := range dues {
		v := myDueView(t, d, today)
		if i == next {
			nv := v
			out.Summary.NextDue = &nv
		}
		if f.match(v.DueCore) {
			out.Items = append(out.Items, v)
		}
	}
	return out, nil
}

// MyReceipts serves GET /v1/me/tenancies/{id}/receipts.
func (s *Service) MyReceipts(ctx context.Context, p *auth.Principal, id uuid.UUID) ([]MyReceiptView, error) {
	t, err := s.myTenancy(ctx, p, id)
	if err != nil {
		return nil, err
	}
	recs, err := s.store.Receipts(ctx, t.ID)
	if err != nil {
		return nil, err
	}
	dues, err := s.store.Dues(ctx, []uuid.UUID{t.ID})
	if err != nil {
		return nil, err
	}
	dates := dueDates(dues)
	out := make([]MyReceiptView, 0, len(recs))
	for _, r := range recs {
		out = append(out, myReceipt(r, dates))
	}
	return out, nil
}

// clockDay is the date part of a time, in UTC.
func clockDay(t time.Time) string { return t.UTC().Format(time.DateOnly) }
