// Package invoices is the finance administrators' renewal invoices (API
// 0.47.0, BR-015, FR-037, FR-038, FR-039/040, Handover section 10).
//
// A finance administrator drafts an invoice for the renewal of a
// subscription, then issues it: the invoice gets its number (INV-ZW-2026-
// 000001, gap-free per market and year), becomes an immutable document and
// is emailed to the payer with a link to the subscription page, where it is
// paid online (when a gateway is configured) or offline. Issuing creates the
// renewal subscription (PENDING_PAYMENT) the payment pays; settlement
// activates it through the subscriptions module (the plan's package is
// granted; listings are never republished).
//
//	DRAFT ──issue──> ISSUED ──payment──> PART_PAID ──payment──> PAID
//	  │                 │ └──due date passed──> OVERDUE ──payment──> ...
//	  └─discard         └──void (nothing paid)──> VOID ──reissue──> new DRAFT
//	ISSUED / PART_PAID / OVERDUE / PAID ──dispute──> DISPUTED ──resolve──> back
//
// Corrections of an issued invoice are credit notes (CRN-...) or void and
// reissue. Every payment gets a receipt (RCT-...). Every change is in the
// invoice's history and the administrators' audit.
package invoices

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Statuses.
const (
	StatusDraft    = "DRAFT"
	StatusIssued   = "ISSUED"
	StatusPartPaid = "PART_PAID"
	StatusPaid     = "PAID"
	StatusOverdue  = "OVERDUE"
	StatusVoid     = "VOID"
	StatusDisputed = "DISPUTED"
)

// Statuses returns every status, in lifecycle order.
func Statuses() []string {
	return []string{StatusDraft, StatusIssued, StatusPartPaid, StatusPaid, StatusOverdue, StatusVoid, StatusDisputed}
}

var statusLabels = map[string]string{
	StatusDraft: "Draft", StatusIssued: "Issued", StatusPartPaid: "Part paid", StatusPaid: "Paid",
	StatusOverdue: "Overdue", StatusVoid: "Void", StatusDisputed: "Disputed",
}

// StatusLabel is a status's wording.
func StatusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// payable: a payment can be recorded.
func payable(status string) bool {
	return status == StatusIssued || status == StatusPartPaid || status == StatusOverdue
}

// Payment methods.
const (
	MethodBankTransfer = "BANK_TRANSFER"
	MethodCash         = "CASH"
	MethodCardOffline  = "CARD_OFFLINE"
	MethodGateway      = "GATEWAY"
	MethodOther        = "OTHER"
)

// OfflineMethods are the methods an administrator records.
func OfflineMethods() []string {
	return []string{MethodBankTransfer, MethodCash, MethodCardOffline, MethodOther}
}

// History actions (invoice_events.action).
const (
	ActionCreated         = "CREATED"
	ActionEdited          = "EDITED"
	ActionIssued          = "ISSUED"
	ActionResent          = "RESENT"
	ActionPaymentRecorded = "PAYMENT_RECORDED"
	ActionCreditNote      = "CREDIT_NOTE"
	ActionOverdue         = "OVERDUE"
	ActionVoided          = "VOIDED"
	ActionDisputed        = "DISPUTED"
	ActionDisputeResolved = "DISPUTE_RESOLVED"
	ActionReissued        = "REISSUED"
	ActionPaid            = "PAID"
)

// Actor types of the history.
const (
	ActorAdmin   = "ADMIN"
	ActorSystem  = "SYSTEM"
	ActorGateway = "GATEWAY"
)

// Admin audit actions (target INVOICE, labelled with the number).
const (
	AuditCreated         = "INVOICE_CREATED"
	AuditEdited          = "INVOICE_EDITED"
	AuditDiscarded       = "INVOICE_DISCARDED"
	AuditIssued          = "INVOICE_ISSUED"
	AuditResent          = "INVOICE_RESENT"
	AuditPaymentRecorded = "INVOICE_PAYMENT_RECORDED"
	AuditCreditNote      = "INVOICE_CREDIT_NOTE"
	AuditVoided          = "INVOICE_VOIDED"
	AuditReissued        = "INVOICE_REISSUED"
	AuditDisputed        = "INVOICE_DISPUTED"
	AuditDisputeResolved = "INVOICE_DISPUTE_RESOLVED"
)

// TargetInvoice is the admin audit target of an invoice.
const TargetInvoice = "INVOICE"

// Outbox events (aggregate "invoice"; payload invoice_id, the kind or the
// payment, occurred_at). The worker emails the payer.
const (
	// EventSent is an issue or a resend (payload kind ISSUED / RESENT).
	EventSent = "INVOICE_SENT"
	// EventOverdue is the due date passing (once per invoice).
	EventOverdue = "INVOICE_OVERDUE"
	// EventPaymentRecorded is a payment and its receipt (payload payment_id).
	EventPaymentRecorded = "INVOICE_PAYMENT_RECORDED"

	AggregateInvoice = "invoice"
)

// EventTypes lists the outbox events of this module.
func EventTypes() []string { return []string{EventSent, EventOverdue, EventPaymentRecorded} }

// Number kinds.
const (
	KindInvoice = "INV"
	KindReceipt = "RCT"
	KindCredit  = "CRN"
)

// FormatNumber is a document number: INV-ZW-2026-000001.
func FormatNumber(kind, market string, year, n int) string {
	return fmt.Sprintf("%s-%s-%04d-%06d", kind, market, year, n)
}

// Limits and defaults.
const (
	DefaultDueDays  = 14
	MaxDueDays      = 365
	MaxLines        = 20
	MaxLineText     = 300
	MaxTermsRunes   = 2000
	MaxReasonRunes  = 1000
	MaxReferenceLen = 100
	DefaultPageSize = 20
	MaxPageSize     = 100
	DayLayout       = "2006-01-02"
	// ResendInterval is the shortest time between two sends of an invoice.
	ResendInterval = 10 * time.Minute
)

// DefaultTerms is the terms text of a new invoice.
const DefaultTerms = "Payment is due by the due date, quoting the invoice number. Your subscription renews when the payment is confirmed. " +
	"Listings that have expired are not republished automatically: relist them from your listings."

// Error codes.
const (
	CodeNotFound        = "INVOICE_NOT_FOUND"
	CodeState           = "INVOICE_STATE_CONFLICT"
	CodeRenewalExists   = "RENEWAL_EXISTS"
	CodeNotInvoiceable  = "SUBSCRIPTION_NOT_INVOICEABLE"
	CodeOverpayment     = "INVOICE_OVERPAYMENT"
	CodeHasPayments     = "INVOICE_HAS_PAYMENTS"
	CodeResendTooSoon   = "INVOICE_RESEND_TOO_SOON"
	CodePriceNotDefined = "PRICE_NOT_AVAILABLE"
)

// Errors.
var (
	ErrNotFound       = errors.New("invoice not found")
	ErrState          = errors.New("the invoice's status does not allow this")
	ErrRenewalExists  = errors.New("the subscriber already has a renewal waiting or paid")
	ErrNotInvoiceable = errors.New("the subscription cannot be renewed by an invoice")
	ErrOverpayment    = errors.New("the amount is more than what is due")
	ErrHasPayments    = errors.New("an invoice with payments cannot be voided; credit it first")
	ErrResendTooSoon  = errors.New("the invoice was sent less than 10 minutes ago")
	ErrPriceMissing   = errors.New("the plan has no price for this currency and period")
)

// ValidationError lists the fields that need attention.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Line is one line of an invoice.
type Line struct {
	ID          uuid.UUID `json:"id"`
	Description string    `json:"description"`
	Quantity    int       `json:"quantity"`
	UnitAmount  string    `json:"unit_amount"`
	Amount      string    `json:"amount"`
}

// Tax is the tax line of an invoice: the market's rule for subscriptions
// when one is in force at issue, else none.
type Tax struct {
	Name      string `json:"name"`
	Rate      string `json:"rate"` // percent, e.g. "15.0000"
	Inclusive bool   `json:"inclusive"`
	Amount    string `json:"amount"`
	IsSample  bool   `json:"is_sample"`
}

// Invoice is one invoice.
type Invoice struct {
	ID                   uuid.UUID
	Number               string // "" while DRAFT
	Status               string
	StatusBeforeDispute  string
	MarketCode           string
	CurrencyCode         string
	SubscriberKind       string
	SubscriberID         uuid.UUID
	OrganisationID       *uuid.UUID
	PayerName            string
	RenewsSubscriptionID uuid.UUID
	SubscriptionID       *uuid.UUID
	PlanID               uuid.UUID
	PlanName             string
	Period               string
	ServiceFrom          time.Time // dates, UTC midnight
	ServiceTo            time.Time
	IssueOn              *time.Time
	DueOn                time.Time
	Terms                string
	Lines                []Line
	Subtotal             string
	Tax                  *Tax
	Total                string
	AmountPaid           string
	AmountCredited       string
	VoidReason           string
	DisputeReason        string
	ReplacesInvoiceID    *uuid.UUID
	SentAt               *time.Time
	LastSentAt           *time.Time
	SendCount            int
	OverdueNotifiedAt    *time.Time
	CreatedBy            *uuid.UUID
	CreatedByName        string
	IssuedBy             *uuid.UUID
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// Balance is what remains due.
func (inv Invoice) Balance() string {
	return Sub(inv.Total, Sum(inv.AmountPaid, inv.AmountCredited))
}

// Payment is one payment of an invoice, with its receipt.
type Payment struct {
	ID             uuid.UUID  `json:"id"`
	InvoiceID      uuid.UUID  `json:"invoice_id"`
	ReceiptNumber  string     `json:"receipt_number"`
	Amount         string     `json:"amount"`
	Method         string     `json:"method"`
	Reference      string     `json:"reference"`
	ReceivedOn     time.Time  `json:"-"`
	SourceKey      string     `json:"-"`
	RecordedBy     *uuid.UUID `json:"-"`
	RecordedByName string     `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
}

// CreditNote is one credit note of an invoice.
type CreditNote struct {
	ID            uuid.UUID  `json:"id"`
	InvoiceID     uuid.UUID  `json:"invoice_id"`
	Number        string     `json:"number"`
	Amount        string     `json:"amount"`
	Reason        string     `json:"reason"`
	CreatedBy     *uuid.UUID `json:"-"`
	CreatedByName string     `json:"-"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Event is one entry of an invoice's history.
type Event struct {
	ID          uuid.UUID
	InvoiceID   uuid.UUID
	Action      string
	ActorType   string
	ActorUserID *uuid.UUID
	ActorName   string
	Note        string
	At          time.Time
}

// NewEvent builds a history entry.
func NewEvent(invoiceID uuid.UUID, action, actorType string, actorID uuid.UUID, actorName, note string, at time.Time) Event {
	var actor *uuid.UUID
	if actorID != uuid.Nil {
		a := actorID
		actor = &a
	}
	return Event{ID: uuid.New(), InvoiceID: invoiceID, Action: action, ActorType: actorType, ActorUserID: actor, ActorName: actorName, Note: note, At: at}
}

// OutboxEvent is one event a change writes.
type OutboxEvent struct {
	Type    string
	Payload map[string]any
}

// Change is what a change of an invoice writes with it, in one transaction.
type Change struct {
	Events []Event
	Audit  []adminaudit.Event
	Outbox []OutboxEvent
	// Number: assign the invoice's number now (issue).
	Number bool
	// Lines: replace the lines with the invoice's (draft edits).
	Lines bool
	// Payment is stored with a receipt number assigned by the store.
	Payment *Payment
	// Credit is stored with a number assigned by the store.
	Credit *CreditNote
}

// Filter selects invoices. Empty fields match everything.
type Filter struct {
	Statuses []string
	Market   string
	// Subscribers limits to these subscriber ids (a customer's own).
	Subscribers []uuid.UUID
	// RenewsSubscription limits to the invoices of one subscription.
	RenewsSubscription *uuid.UUID
	// Q is matched against the number and the payer's name.
	Q             string
	ExcludeDrafts bool
	Limit         int
	Offset        int
}

// Store is the persistence of invoices.
type Store interface {
	// Create stores a new DRAFT with its lines and history entry.
	Create(ctx context.Context, inv Invoice, ev Event, audit adminaudit.Event) error
	// Get returns one invoice with its lines, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Invoice, error)
	// List returns one page, newest first, and the total.
	List(ctx context.Context, f Filter) ([]Invoice, int, error)
	// Change locks the invoice, calls change with a copy and saves it with
	// what change returns (numbers assigned under lock, gap-free).
	Change(ctx context.Context, id uuid.UUID, change func(inv *Invoice) (Change, error)) (Invoice, error)
	// Delete removes a DRAFT (with its lines and history) after the audit.
	Delete(ctx context.Context, id uuid.UUID, audit adminaudit.Event) error
	Payments(ctx context.Context, id uuid.UUID) ([]Payment, error)
	Credits(ctx context.Context, id uuid.UUID) ([]CreditNote, error)
	Events(ctx context.Context, id uuid.UUID) ([]Event, error)
	// BySubscription returns the invoice whose renewal subscription is id,
	// or ErrNotFound.
	BySubscription(ctx context.Context, subscriptionID uuid.UUID) (Invoice, error)
	// DueCandidates returns the ids of ISSUED and PART_PAID invoices due on
	// or before day (the caller checks each market's local date).
	DueCandidates(ctx context.Context, day time.Time) ([]uuid.UUID, error)
}

// ---------------------------------------------------------------------------
// Money: decimal strings with two decimals, exact.
// ---------------------------------------------------------------------------

var amountRe = regexp.MustCompile(`^[0-9]{1,10}(\.[0-9]{1,2})?$`)

// ParseAmount checks a positive amount with at most two decimals and
// returns it with exactly two.
func ParseAmount(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !amountRe.MatchString(raw) {
		return "", false
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok || r.Sign() <= 0 {
		return "", false
	}
	return r.FloatString(2), true
}

func rat(s string) *big.Rat {
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return new(big.Rat)
	}
	return r
}

// Sum adds amounts.
func Sum(amounts ...string) string {
	total := new(big.Rat)
	for _, a := range amounts {
		total.Add(total, rat(a))
	}
	return total.FloatString(2)
}

// Sub is a - b.
func Sub(a, b string) string { return new(big.Rat).Sub(rat(a), rat(b)).FloatString(2) }

// Mul is quantity times an amount.
func Mul(q int, a string) string {
	return new(big.Rat).Mul(big.NewRat(int64(q), 1), rat(a)).FloatString(2)
}

// Compare compares two amounts (-1, 0, 1).
func Compare(a, b string) int { return rat(a).Cmp(rat(b)) }

// roundHalfUp rounds r to cents, halves away from zero.
func roundHalfUp(r *big.Rat) string {
	scaled := new(big.Rat).Mul(r, big.NewRat(100, 1))
	num, den := scaled.Num(), scaled.Denom()
	q, m := new(big.Int).QuoRem(num, den, new(big.Int))
	if new(big.Int).Mul(m, big.NewInt(2)).CmpAbs(den) >= 0 {
		if num.Sign() >= 0 {
			q.Add(q, big.NewInt(1))
		} else {
			q.Sub(q, big.NewInt(1))
		}
	}
	return new(big.Rat).SetFrac(q, big.NewInt(100)).FloatString(2)
}

// ApplyTax returns the tax amount and the total of a subtotal under a rate
// (percent). Inclusive: the subtotal already holds the tax (total =
// subtotal, tax = subtotal * rate / (100 + rate)); exclusive: it is added.
// Rounded half-up to cents once.
func ApplyTax(subtotal, ratePercent string, inclusive bool) (tax, total string) {
	sub, rate := rat(subtotal), rat(ratePercent)
	if inclusive {
		t := new(big.Rat).Quo(new(big.Rat).Mul(sub, rate), new(big.Rat).Add(big.NewRat(100, 1), rate))
		return roundHalfUp(t), sub.FloatString(2)
	}
	t := roundHalfUp(new(big.Rat).Quo(new(big.Rat).Mul(sub, rate), big.NewRat(100, 1)))
	return t, Sum(subtotal, t)
}

// day is t's UTC date at midnight.
func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// LocalDay is t's date in the IANA zone (UTC when the zone is unknown).
func LocalDay(t time.Time, zone string) time.Time {
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		loc = time.UTC
	}
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
}
