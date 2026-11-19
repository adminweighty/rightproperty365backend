package tenancies

import (
	"time"

	"github.com/google/uuid"
)

// The JSON shapes of rent (contract 0.36, "Response shapes (b)"). Every
// object holding an amount also holds its currency.

// BriefProperty is the property of a TenancyBrief.
type BriefProperty struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Archived bool      `json:"archived"`
}

// TenancyBrief names a tenancy in lists of dues, arrears and receivables.
type TenancyBrief struct {
	ID          uuid.UUID       `json:"id"`
	Status      string          `json:"status"`
	StatusLabel string          `json:"status_label"`
	Property    BriefProperty   `json:"property"`
	Unit        *TenancyUnitRef `json:"unit"`
	Tenants     []ContactRef    `json:"tenants"`
}

// PeriodView is the period a due pays for.
type PeriodView struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// DueCore is what the agent and the tenant both see of a due.
type DueCore struct {
	ID          uuid.UUID  `json:"id"`
	TenancyID   uuid.UUID  `json:"tenancy_id"`
	Sequence    int        `json:"sequence"`
	DueDate     string     `json:"due_date"`
	Period      PeriodView `json:"period"`
	Amount      string     `json:"amount"`
	Currency    string     `json:"currency"`
	MinorUnit   int        `json:"minor_unit"`
	Paid        string     `json:"paid"`
	Waived      string     `json:"waived"`
	Outstanding string     `json:"outstanding"`
	Status      string     `json:"status"`
	StatusLabel string     `json:"status_label"`
	Overdue     bool       `json:"overdue"`
	DaysOverdue int        `json:"days_overdue"`
}

// WaiverView says who waived a due, when and why.
type WaiverView struct {
	At     time.Time `json:"at"`
	By     PersonRef `json:"by"`
	Reason string    `json:"reason"`
}

// DueCan is what the caller may do with a due now.
type DueCan struct {
	Waive bool `json:"waive"`
}

// DueView is a due as the agent API shows it.
type DueView struct {
	DueCore
	Waiver *WaiverView `json:"waiver"`
	Can    DueCan      `json:"can"`
}

// MyWaiverView is a waiver as the tenant sees it.
type MyWaiverView struct {
	At time.Time `json:"at"`
}

// MyDueView is a due as its tenant sees it.
type MyDueView struct {
	DueCore
	Waiver *MyWaiverView `json:"waiver"`
}

// SummaryCore is the figures of a schedule today.
type SummaryCore struct {
	Currency      string `json:"currency"`
	MinorUnit     int    `json:"minor_unit"`
	Charged       string `json:"charged"`
	Paid          string `json:"paid"`
	Waived        string `json:"waived"`
	Outstanding   string `json:"outstanding"`
	Overdue       string `json:"overdue"`
	OverdueCount  int    `json:"overdue_count"`
	PaidInAdvance string `json:"paid_in_advance"`
}

// DueSummary is the agent's summary of a schedule.
type DueSummary struct {
	SummaryCore
	NextDue *DueView `json:"next_due"`
}

// MyDueSummary is the tenant's summary of a schedule.
type MyDueSummary struct {
	SummaryCore
	NextDue *MyDueView `json:"next_due"`
}

// ScheduleView describes a tenancy's schedule.
type ScheduleView struct {
	Rolling bool   `json:"rolling"`
	Through string `json:"through"`
	Count   int    `json:"count"`
	Rule    string `json:"rule"`
}

// DuesPage is GET .../tenancies/{tenancyId}/dues.
type DuesPage struct {
	Items    []DueView     `json:"items"`
	Summary  DueSummary    `json:"summary"`
	Schedule *ScheduleView `json:"schedule"`
}

// MyDuesPage is GET /v1/me/tenancies/{id}/dues.
type MyDuesPage struct {
	Items    []MyDueView   `json:"items"`
	Summary  MyDueSummary  `json:"summary"`
	Schedule *ScheduleView `json:"schedule"`
}

// AllocationView is one line of a receipt.
type AllocationView struct {
	DueID     uuid.UUID `json:"due_id"`
	DueDate   string    `json:"due_date"`
	Amount    string    `json:"amount"`
	Currency  string    `json:"currency"`
	MinorUnit int       `json:"minor_unit"`
}

// ReceiptReversalView is the ledger reversal that undid a receipt.
type ReceiptReversalView struct {
	LedgerEntryID uuid.UUID `json:"ledger_entry_id"`
	ReversedOn    string    `json:"reversed_on"`
	At            time.Time `json:"at"`
}

// ReceiptCan is what the caller may do with a receipt now.
type ReceiptCan struct {
	Reverse bool `json:"reverse"`
}

// ReceiptView is a receipt as the agent API shows it.
type ReceiptView struct {
	ID                uuid.UUID            `json:"id"`
	TenancyID         uuid.UUID            `json:"tenancy_id"`
	Amount            string               `json:"amount"`
	Currency          string               `json:"currency"`
	MinorUnit         int                  `json:"minor_unit"`
	Method            string               `json:"method"`
	MethodLabel       string               `json:"method_label"`
	ReceivedOn        string               `json:"received_on"`
	Reference         string               `json:"reference"`
	Tenant            *ContactRef          `json:"tenant"`
	Status            string               `json:"status"`
	StatusLabel       string               `json:"status_label"`
	Allocations       []AllocationView     `json:"allocations"`
	LedgerEntryID     uuid.UUID            `json:"ledger_entry_id"`
	LedgerStatus      string               `json:"ledger_status"`
	LedgerStatusLabel string               `json:"ledger_status_label"`
	Reversal          *ReceiptReversalView `json:"reversal"`
	RecordedBy        PersonRef            `json:"recorded_by"`
	RecordedAt        time.Time            `json:"recorded_at"`
	Can               ReceiptCan           `json:"can"`
}

// MyReceiptView is a receipt as its tenant sees it.
type MyReceiptView struct {
	ID          uuid.UUID        `json:"id"`
	Amount      string           `json:"amount"`
	Currency    string           `json:"currency"`
	MinorUnit   int              `json:"minor_unit"`
	Method      string           `json:"method"`
	MethodLabel string           `json:"method_label"`
	ReceivedOn  string           `json:"received_on"`
	Reference   string           `json:"reference"`
	Status      string           `json:"status"`
	StatusLabel string           `json:"status_label"`
	Allocations []AllocationView `json:"allocations"`
	ReversedOn  *string          `json:"reversed_on"`
}

// ReceiptResult is the answer of POST .../receipts.
type ReceiptResult struct {
	Receipt ReceiptView `json:"receipt"`
	Dues    []DueView   `json:"dues"`
}

// StatementProperty is the property on a statement.
type StatementProperty struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// StatementTenancy is the tenancy on a statement (agent and tenant alike).
type StatementTenancy struct {
	ID          uuid.UUID         `json:"id"`
	Status      string            `json:"status"`
	StatusLabel string            `json:"status_label"`
	ManagedBy   ManagerRef        `json:"managed_by"`
	Property    StatementProperty `json:"property"`
	Unit        *LabelRef         `json:"unit"`
	Tenants     []NameRef         `json:"tenants"`
}

// Statement line types.
const (
	LineCharge          = "CHARGE"
	LinePayment         = "PAYMENT"
	LinePaymentReversed = "PAYMENT_REVERSED"
	LineWaiver          = "WAIVER"
)

// StatementLine is one line of a statement.
type StatementLine struct {
	Date        string     `json:"date"`
	Type        string     `json:"type"`
	Description string     `json:"description"`
	Reference   string     `json:"reference"`
	Currency    string     `json:"currency"`
	Debit       string     `json:"debit"`
	Credit      string     `json:"credit"`
	Balance     string     `json:"balance"`
	DueID       *uuid.UUID `json:"due_id"`
	ReceiptID   *uuid.UUID `json:"receipt_id"`
}

// StatementCurrency is a statement in one currency.
type StatementCurrency struct {
	Currency       string          `json:"currency"`
	MinorUnit      int             `json:"minor_unit"`
	OpeningBalance string          `json:"opening_balance"`
	Charged        string          `json:"charged"`
	Paid           string          `json:"paid"`
	Reversed       string          `json:"reversed"`
	Waived         string          `json:"waived"`
	ClosingBalance string          `json:"closing_balance"`
	Overdue        string          `json:"overdue"`
	Lines          []StatementLine `json:"lines"`
}

// Statement is GET .../tenancies/{tenancyId}/statement.
type Statement struct {
	Tenancy     StatementTenancy    `json:"tenancy"`
	From        string              `json:"from"`
	To          string              `json:"to"`
	GeneratedAt time.Time           `json:"generated_at"`
	Currencies  []StatementCurrency `json:"currencies"`
}

// BucketView is one ageing bucket in one currency.
type BucketView struct {
	Bucket      string `json:"bucket"`
	Currency    string `json:"currency"`
	MinorUnit   int    `json:"minor_unit"`
	Outstanding string `json:"outstanding"`
	Count       int    `json:"count"`
}

// ArrearsTenancy is one tenancy's arrears in one currency.
type ArrearsTenancy struct {
	Tenancy       TenancyBrief `json:"tenancy"`
	Currency      string       `json:"currency"`
	MinorUnit     int          `json:"minor_unit"`
	Outstanding   string       `json:"outstanding"`
	Count         int          `json:"count"`
	OldestDueDate string       `json:"oldest_due_date"`
	DaysOverdue   int          `json:"days_overdue"`
	Buckets       []BucketView `json:"buckets"`
}

// ArrearsCurrency is the arrears in one currency.
type ArrearsCurrency struct {
	Currency    string           `json:"currency"`
	MinorUnit   int              `json:"minor_unit"`
	Outstanding string           `json:"outstanding"`
	Count       int              `json:"count"`
	Buckets     []BucketView     `json:"buckets"`
	Tenancies   []ArrearsTenancy `json:"tenancies"`
}

// Arrears is GET .../arrears.
type Arrears struct {
	AsOf       string            `json:"as_of"`
	Currencies []ArrearsCurrency `json:"currencies"`
}
