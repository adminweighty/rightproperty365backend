package tenancies

import (
	"errors"
	"math/big"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/money"
)

// Rent (API 0.36.1, part b): the schedule of dues of an activated tenancy,
// the receipts against it and their allocations. What a due has been paid
// is never stored on it: it is the sum of the allocations of the receipts
// that are not reversed. A receipt writes one RENT_RECEIVED ledger entry in
// its own transaction; a ledger reversal of that entry marks the receipt
// reversed in the reversal's transaction (the ledger stores' reversal hook,
// ConnectLedger), so its allocations stop counting.

// Due statuses.
const (
	DueDue      = "DUE"
	DuePartPaid = "PART_PAID"
	DuePaid     = "PAID"
	DueWaived   = "WAIVED"
)

// DueStatuses returns every due status.
func DueStatuses() []string { return []string{DueDue, DuePartPaid, DuePaid, DueWaived} }

var dueLabels = map[string]string{DueDue: "Due", DuePartPaid: "Part paid", DuePaid: "Paid", DueWaived: "Waived"}

// DueStatusLabel is the wording of a due status.
func DueStatusLabel(s string) string { return dueLabels[s] }

// Receipt statuses.
const (
	ReceiptRecorded = "RECORDED"
	ReceiptReversed = "REVERSED"
)

var receiptLabels = map[string]string{ReceiptRecorded: "Recorded", ReceiptReversed: "Reversed"}

// Rent limits.
const (
	MaxReceiptReference = 120
	MaxAllocations      = 100
	MaxWaiveReason      = 500
)

// forever is the as-of date of the current state: every receipt received,
// every reversal counted.
const forever = "9999-12-31"

// Rent errors.
var (
	ErrDueNotFound     = errors.New("rent due not found")
	ErrReceiptNotFound = errors.New("rent receipt not found")
	ErrDueChanged      = errors.New("a rent due changed meanwhile")
	ErrNoLedger        = errors.New("tenancies: no ledger service")
)

// NotWaivableError is 409 RENT_DUE_NOT_WAIVABLE.
type NotWaivableError struct{ Status string }

func (e *NotWaivableError) Error() string { return "rent due cannot be waived: " + e.Status }

// Due is one line of a tenancy's rent schedule. Amount has exactly the
// currency's minor-unit decimals; DueDate is "YYYY-MM-DD".
type Due struct {
	ID           uuid.UUID
	TenancyID    uuid.UUID
	Sequence     int
	DueDate      string
	Amount       string
	Currency     string
	WaivedAt     *time.Time
	WaivedBy     *uuid.UUID
	WaivedByName string
	WaiveReason  string
	CreatedAt    time.Time

	// Allocations is read-only, filled by the stores: every allocation to
	// the due (reversed receipts' included), with its receipt's dates.
	Allocations []DueAllocation
}

// DueAllocation is one allocation to a due as the due sees it.
type DueAllocation struct {
	ReceiptID  uuid.UUID
	Amount     string
	ReceivedOn string
	// ReversedOn is the reversal's effective date ("" while the receipt stands).
	ReversedOn string
}

// counts reports whether the allocation counts on date asOf: received by
// then and not reversed by then.
func (a DueAllocation) counts(asOf string) bool {
	return a.ReceivedOn <= asOf && (a.ReversedOn == "" || a.ReversedOn > asOf)
}

// dueState is a due's figures on one date.
type dueState struct {
	paid, waived, outstanding *big.Rat
	status                    string
}

// state computes a due's paid, waived and outstanding amounts and its
// status as of asOf (forever: now).
func (d Due) state(asOf string) dueState {
	paid := new(big.Rat)
	for _, a := range d.Allocations {
		if a.counts(asOf) {
			paid.Add(paid, money.Rat(a.Amount))
		}
	}
	amount := money.Rat(d.Amount)
	rest := new(big.Rat).Sub(amount, paid)
	if rest.Sign() < 0 {
		rest.SetInt64(0)
	}
	st := dueState{paid: paid, waived: new(big.Rat), outstanding: rest}
	switch {
	case d.waivedBy(asOf):
		st.waived, st.outstanding, st.status = rest, new(big.Rat), DueWaived
	case rest.Sign() == 0:
		st.status = DuePaid
	case paid.Sign() > 0:
		st.status = DuePartPaid
	default:
		st.status = DueDue
	}
	return st
}

// waivedBy reports whether the due was waived on or before asOf.
func (d Due) waivedBy(asOf string) bool {
	return d.WaivedAt != nil && d.WaivedAt.UTC().Format(time.DateOnly) <= asOf
}

// Allocation is one line of a receipt.
type Allocation struct {
	DueID  uuid.UUID
	Amount string
}

// Receipt is money received for a tenancy, in its rent currency.
type Receipt struct {
	ID              uuid.UUID
	TenancyID       uuid.UUID
	Amount          string
	Currency        string
	Method          string
	ReceivedOn      string
	Reference       string
	ContactID       *uuid.UUID
	ContactName     string
	LedgerEntryID   uuid.UUID
	RecordedBy      *uuid.UUID
	RecordedByName  string
	RecordedAt      time.Time
	ReversedAt      *time.Time
	ReversalEntryID *uuid.UUID
	ReversedOn      string
	Allocations     []Allocation

	// Read-only, filled by the stores: the ledger entry's current status
	// and author ("" / nil when unknown).
	LedgerStatus string
	LedgerAuthor *uuid.UUID
}

// Reversed reports whether the receipt's ledger entry was reversed.
func (r Receipt) Reversed() bool { return r.ReversedAt != nil }

// Waiver waives one due.
type Waiver struct {
	DueID  uuid.UUID
	At     time.Time
	By     *uuid.UUID
	ByName string
	Reason string
}
