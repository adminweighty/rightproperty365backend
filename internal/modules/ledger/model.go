// Package ledger is the agent ledger / books (API 0.34.0, BR-012, FR-026 to
// FR-029, FSD section 6): an organisation's (or an approved direct owner's)
// own cashbook. It is not the platform's revenue ledger and it is not
// double-entry (decision D-05).
//
// Rules:
//
//   - Amounts are exact decimals (math/big via internal/platform/money) with
//     an ISO 4217 currency and at most its minor-unit decimals. Nothing ever
//     adds two currencies: every total is per currency.
//   - A posted entry is never edited or deleted. A REVERSAL entry (the same
//     amount, the other direction, reversal_of the original) undoes it; a
//     correction is a reversal plus a new entry (correction_of the
//     original), in one transaction. The database refuses UPDATE of what
//     was posted and every DELETE (migration 0033).
//   - Manual entries are USER_RECORDED_UNVERIFIED. RECONCILED means "the
//     organisation marked it reconciled against its own statement", never a
//     bank verification. GATEWAY_CONFIRMED is reserved: there is no gateway,
//     nothing sets it, and the database refuses it without a gateway
//     reference.
//   - A TRANSFER is an OUT / IN pair (between methods or branches) linked
//     both ways. An OPENING_BALANCE is allowed once per currency, method and
//     branch while it is not reversed. RECEIVABLE and PAYABLE are memo
//     entries (no method, never in a cashbook balance) that cash entries
//     settle.
//
// Who may do what is decided in internal/platform/auth/capabilities.go
// (MayReadLedger, MayWriteLedger, MayManageLedger, MayCorrectLedgerEntry,
// OwnsLedgerBook). A book that is not the caller's is 404 BOOK_NOT_FOUND.
// Every post, reversal, correction, reconciliation, attachment, attachment
// download and export is written to the organisation's audit trail in the
// same transaction (a direct owner's book: the API log).
package ledger

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Entry types.
const (
	TypeIncome          = "INCOME"
	TypeExpense         = "EXPENSE"
	TypeRentReceived    = "RENT_RECEIVED"
	TypeCommission      = "COMMISSION"
	TypeDepositHeld     = "DEPOSIT_HELD"
	TypeDepositReturned = "DEPOSIT_RETURNED"
	TypeTransfer        = "TRANSFER"
	TypeOpeningBalance  = "OPENING_BALANCE"
	TypeReceivable      = "RECEIVABLE"
	TypePayable         = "PAYABLE"
	// TypeReversal is posted by the API only (reverse, correct).
	TypeReversal = "REVERSAL"
)

// Directions.
const (
	In  = "IN"
	Out = "OUT"
)

// Methods.
const (
	MethodCash        = "CASH"
	MethodBank        = "BANK"
	MethodMobileMoney = "MOBILE_MONEY"
	MethodOther       = "OTHER"
)

// Statuses (FR-028).
const (
	StatusUnverified = "USER_RECORDED_UNVERIFIED"
	StatusReconciled = "RECONCILED"
	// StatusGatewayConfirmed is reserved: never set by the API.
	StatusGatewayConfirmed = "GATEWAY_CONFIRMED"
)

// Category kinds.
const (
	KindIn  = "IN"
	KindOut = "OUT"
	KindAny = "ANY"
)

// TypeInfo describes an entry type.
type TypeInfo struct {
	Code  string
	Label string
	// Direction is the fixed direction ("" when the body chooses or for a transfer).
	Direction string
	// Cash is false for the memo types (receivable, payable).
	Cash bool
	// Category is the default category code ("" when none).
	Category string
}

var types = []TypeInfo{
	{TypeIncome, "Income", In, true, "other-income"},
	{TypeExpense, "Expense", Out, true, "other-expense"},
	{TypeRentReceived, "Rent received", In, true, "rent"},
	{TypeCommission, "Commission", In, true, "commission"},
	{TypeDepositHeld, "Deposit held", In, true, "deposit"},
	{TypeDepositReturned, "Deposit returned", Out, true, "deposit"},
	{TypeTransfer, "Transfer", "", true, ""},
	{TypeOpeningBalance, "Opening balance", "", true, ""},
	{TypeReceivable, "Receivable (owed to you)", In, false, ""},
	{TypePayable, "Payable (you owe)", Out, false, ""},
}

var reversalInfo = TypeInfo{TypeReversal, "Reversal", "", true, ""}

// Types returns the types a body may carry, in form order.
func Types() []TypeInfo { return slices.Clone(types) }

// typeInfo returns the description of a type (REVERSAL included).
func typeInfo(code string) (TypeInfo, bool) {
	if code == TypeReversal {
		return reversalInfo, true
	}
	for _, t := range types {
		if t.Code == code {
			return t, true
		}
	}
	return TypeInfo{}, false
}

// IsCash reports whether entries of the type move a cashbook balance.
func IsCash(typ string) bool {
	t, ok := typeInfo(typ)
	return ok && t.Cash
}

var methodLabels = map[string]string{MethodCash: "Cash", MethodBank: "Bank", MethodMobileMoney: "Mobile money", MethodOther: "Other"}

// Methods returns every payment method in form order.
func Methods() []string { return []string{MethodCash, MethodBank, MethodMobileMoney, MethodOther} }

// MethodLabel is the wording of a method.
func MethodLabel(m string) string {
	if l, ok := methodLabels[m]; ok {
		return l
	}
	return m
}

// Status labels: never anything that sounds like a bank verification.
const (
	LabelUnverified       = "Recorded by you, not verified"
	LabelReconciled       = "Marked reconciled by your team"
	LabelGatewayConfirmed = "Confirmed by a payment gateway (not available)"
)

// StatusLabel is the wording of a status.
func StatusLabel(s string) string {
	switch s {
	case StatusReconciled:
		return LabelReconciled
	case StatusGatewayConfirmed:
		return LabelGatewayConfirmed
	}
	return LabelUnverified
}

// Statuses returns every status.
func Statuses() []string { return []string{StatusUnverified, StatusReconciled, StatusGatewayConfirmed} }

// Limits.
const (
	MaxDescription = 500
	MaxReference   = 120
	MaxBranch      = 80
	MaxReason      = 500
	MaxNote        = 5000
	MaxReconcile   = 500
	MaxLabel       = 80
	MaxCategories  = 200
	// MaxFileBytes is the largest attachment (as onboarding documents).
	MaxFileBytes = 10 << 20
	// MaxUploadBodyBytes is the request body limit of an attachment upload.
	MaxUploadBodyBytes = MaxFileBytes + 64<<10
	// MaxAttachments per entry.
	MaxAttachments = 20
	// MaxExportRows caps an export.
	MaxExportRows = 50000
	// AttachmentKeyPrefix is the object storage prefix of attachments.
	AttachmentKeyPrefix = "ledger"
)

// Errors.
var (
	ErrUnauthenticated      = errors.New("not signed in")
	ErrBookNotFound         = errors.New("book not found")
	ErrEntryNotFound        = errors.New("ledger entry not found")
	ErrAttachmentNotFound   = errors.New("ledger attachment not found")
	ErrCategoryNotFound     = errors.New("ledger category not found")
	ErrForbidden            = errors.New("not allowed")
	ErrAlreadyReversed      = errors.New("ledger entry already reversed")
	ErrNotReversible        = errors.New("a reversal cannot be reversed")
	ErrOpeningBalanceExists = errors.New("opening balance exists")
	ErrCategoryExists       = errors.New("ledger category exists")
	ErrOverSettled          = errors.New("settles more than is outstanding")
	ErrUnsupportedFile      = errors.New("unsupported file type")
	ErrFileTooLarge         = errors.New("file too large")
	ErrImmutable            = errors.New("posted ledger entries are immutable")
	// ErrRentReceipt refuses a correction of a rent receipt's entry: reverse
	// it and record a new receipt on the tenancy.
	ErrRentReceipt = errors.New("ledger entry is a rent receipt")
)

// Book is whose books: an organisation's, or a direct owner's personal
// ones. Exactly one field is set.
type Book struct {
	OrganisationID *uuid.UUID
	UserID         *uuid.UUID
}

// OrganisationBook returns the book of an organisation.
func OrganisationBook(id uuid.UUID) Book { return Book{OrganisationID: &id} }

// PersonalBook returns the personal book of a direct owner.
func PersonalBook(userID uuid.UUID) Book { return Book{UserID: &userID} }

// ID is the organisation ID or the owner's user ID.
func (b Book) ID() uuid.UUID {
	if b.OrganisationID != nil {
		return *b.OrganisationID
	}
	if b.UserID != nil {
		return *b.UserID
	}
	return uuid.Nil
}

// Same reports whether two books are the same.
func (b Book) Same(o Book) bool {
	return ptrEq(b.OrganisationID, o.OrganisationID) && ptrEq(b.UserID, o.UserID)
}

func ptrEq(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Entry is one posted entry. Amount is the positive decimal with exactly the
// currency's minor-unit decimals; dates are "YYYY-MM-DD".
type Entry struct {
	ID             uuid.UUID
	Book           Book
	Type           string
	Direction      string
	Category       string // "" when none
	Method         string // "" for receivables and payables
	Amount         string
	Currency       string
	EffectiveDate  string
	RecordedAt     time.Time
	AuthorUserID   *uuid.UUID
	AuthorName     string
	Status         string
	ReconciledAt   *time.Time
	ReconciledBy   *uuid.UUID
	ReconciledName string
	ReconcileNote  string
	ReversalOf     *uuid.UUID
	ReversedBy     *uuid.UUID
	CorrectionOf   *uuid.UUID
	CorrectedBy    *uuid.UUID
	TransferPair   *uuid.UUID
	Settles        *uuid.UUID
	Reason         string
	ContactID      *uuid.UUID
	ListingID      *uuid.UUID
	LeaseID        *uuid.UUID
	// TenancyID is set on the RENT_RECEIVED entry of a rent receipt (and
	// its reversal) only: the tenancies module posts those (API 0.36.1).
	TenancyID *uuid.UUID
	// PropertyID is set on the EXPENSE entry of a maintenance cost (and its
	// reversal) only: the maintenance module posts those (API 0.36.2).
	PropertyID  *uuid.UUID
	Branch      string // "" when none
	Description string
	Reference   string

	// Read-only, computed by the stores.
	NoteCount       int
	AttachmentCount int
}

// Reversed reports whether the entry was reversed.
func (e Entry) Reversed() bool { return e.ReversedBy != nil }

// Note is a remark on an entry: append-only.
type Note struct {
	ID           uuid.UUID
	EntryID      uuid.UUID
	Text         string
	AuthorUserID *uuid.UUID
	AuthorName   string
	CreatedAt    time.Time
}

// Attachment is a file kept with an entry in the private object storage.
type Attachment struct {
	ID             uuid.UUID
	EntryID        uuid.UUID
	FileName       string
	ContentType    string
	Size           int64
	SHA256         string
	ObjectKey      string
	UploadedBy     *uuid.UUID
	UploadedByName string
	UploadedAt     time.Time
	// ScanStatus is the malware scan status (storage.Scan*), read from the
	// scan records when a view is built; "" when it has none. Never stored.
	ScanStatus string
}

// Category is one category of a book.
type Category struct {
	ID        uuid.UUID
	Book      Book
	Code      string
	Label     string
	Kind      string
	Active    bool
	Default   bool
	SortOrder int
	CreatedBy *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
	// Used is computed by the stores: an entry carries the code.
	Used bool
}

// DefaultCategories are the categories every book starts with.
func DefaultCategories() []Category {
	rows := []struct{ code, label, kind string }{
		{"rent", "Rent", KindIn}, {"commission", "Commission", KindIn}, {"deposit", "Deposit", KindAny},
		{"maintenance", "Maintenance", KindOut}, {"utilities", "Utilities", KindOut}, {"advertising", "Advertising", KindOut},
		{"salaries", "Salaries", KindOut}, {"fees", "Fees", KindAny}, {"other-income", "Other income", KindIn},
		{"other-expense", "Other expense", KindOut},
	}
	out := make([]Category, 0, len(rows))
	for i, r := range rows {
		out = append(out, Category{Code: r.code, Label: r.label, Kind: r.kind, Active: true, Default: true, SortOrder: (i + 1) * 10})
	}
	return out
}

// Filter narrows a list of entries. Dates are inclusive "YYYY-MM-DD" ("" = open).
type Filter struct {
	Currency  string
	Type      string
	Category  string
	Method    string
	Status    string
	From, To  string
	Branch    *string
	ContactID *uuid.UUID
	ListingID *uuid.UUID
	LeaseID   *uuid.UUID
	Q         string
	// ExcludeReversed hides reversed entries and REVERSAL entries.
	ExcludeReversed bool
	// Ascending sorts oldest first (effective date, then recorded_at, then id).
	Ascending bool
	// Limit 0 means no limit.
	Limit, Offset int
}

// LinkSet is a set-once link written on an existing entry.
type LinkSet struct {
	EntryID      uuid.UUID
	ReversedBy   *uuid.UUID
	CorrectedBy  *uuid.UUID
	TransferPair *uuid.UUID
}

// Reconciliation changes an entry's reconciliation status.
type Reconciliation struct {
	EntryID uuid.UUID
	// Status is StatusReconciled or StatusUnverified.
	Status string
	At     time.Time
	By     *uuid.UUID
	ByName string
	Note   string
}

// Change is one atomic write. Stores apply it in this order: lock the
// entries of NotReversed and fail with ErrAlreadyReversed when one is
// reversed; insert Reversals; apply Links; insert Entries (an opening
// balance that already has a live one: ErrOpeningBalanceExists; an entry
// that settles more than the outstanding of its receivable or payable:
// ErrOverSettled); apply PostLinks; apply Reconcile; insert Notes and
// Attachments; insert Audit.
type Change struct {
	NotReversed []uuid.UUID
	Reversals   []Entry
	Links       []LinkSet
	Entries     []Entry
	PostLinks   []LinkSet
	Reconcile   *Reconciliation
	Notes       []Note
	Attachments []Attachment
	Audit       []orgaudit.Event
}

// Reversal is one entry reversed by a Change: the entry, its REVERSAL entry
// and the reversal's effective date and time.
type Reversal struct {
	EntryID       uuid.UUID
	ReversalID    uuid.UUID
	EffectiveDate string
	At            time.Time
}

// reversals lists the entries a Change reverses (its Links with ReversedBy).
func (ch Change) reversals() []Reversal {
	var out []Reversal
	for _, l := range ch.Links {
		if l.ReversedBy == nil {
			continue
		}
		r := Reversal{EntryID: l.EntryID, ReversalID: *l.ReversedBy}
		for _, e := range ch.Reversals {
			if e.ID == *l.ReversedBy {
				r.EffectiveDate, r.At = e.EffectiveDate, e.RecordedAt
			}
		}
		out = append(out, r)
	}
	return out
}

// Store persists books.
type Store interface {
	// Entry returns one entry, or ErrEntryNotFound.
	Entry(ctx context.Context, id uuid.UUID) (Entry, error)
	// Entries returns one page of a book's entries that pass the filter, and the total.
	Entries(ctx context.Context, book Book, f Filter) ([]Entry, int, error)
	// Related returns the entries of a book that point at id (reversal_of,
	// correction_of, transfer_pair, settles).
	Related(ctx context.Context, book Book, id uuid.UUID) ([]Entry, error)
	// Settled returns, per receivable or payable among ids, the live cash
	// entries settling it (any date).
	Settled(ctx context.Context, book Book, ids []uuid.UUID) (map[uuid.UUID][]Entry, error)
	// OpeningBalance returns the live opening balance of a book for a
	// currency, method and branch (case-insensitive), ok false when none.
	OpeningBalance(ctx context.Context, book Book, currency, method, branch string) (Entry, bool, error)
	// Branches returns the branch labels used in a book, sorted.
	Branches(ctx context.Context, book Book) ([]string, error)
	Notes(ctx context.Context, entryID uuid.UUID) ([]Note, error)
	Attachments(ctx context.Context, entryID uuid.UUID) ([]Attachment, error)
	// Categories returns a book's categories (Used computed).
	Categories(ctx context.Context, book Book) ([]Category, error)
	// EnsureCategories adds the categories a book does not have yet (by code).
	EnsureCategories(ctx context.Context, book Book, defaults []Category) error
	// CreateCategory adds a category (ErrCategoryExists when the code is taken).
	CreateCategory(ctx context.Context, c Category) error
	// UpdateCategory replaces a category's label, kind, active and updated_at.
	UpdateCategory(ctx context.Context, c Category) error
	// Apply writes a Change atomically.
	Apply(ctx context.Context, ch Change) error
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

// Cash reports whether the entry moves a cashbook balance: not a receivable
// or payable, nor the reversal of one (which has no method either).
func (e Entry) Cash() bool {
	if e.Type == TypeReversal {
		return e.Method != ""
	}
	return IsCash(e.Type)
}

// stamped fills a category's missing ID and times.
func stamped(c Category) Category {
	if c.ID == uuid.Nil {
		c.ID = newID()
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC().Truncate(time.Microsecond)
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = c.CreatedAt
	}
	return c
}
