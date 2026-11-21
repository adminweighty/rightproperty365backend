package ledger

import (
	"time"

	"github.com/google/uuid"
)

// The JSON shapes of the API (contract 0.34.0, "Response shapes").

// PersonRef names a person.
type PersonRef struct {
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
}

// ContactRef is a linked contact.
type ContactRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// ListingRef is a linked listing.
type ListingRef struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Reference string    `json:"reference"`
}

// LeaseRef is a linked lease.
type LeaseRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
}

// ReconciliationView says who marked an entry reconciled.
type ReconciliationView struct {
	At   time.Time `json:"at"`
	By   PersonRef `json:"by"`
	Note *string   `json:"note"`
}

// Can is what the caller may do with an entry now.
type Can struct {
	Reverse   bool `json:"reverse"`
	Correct   bool `json:"correct"`
	Reconcile bool `json:"reconcile"`
	Note      bool `json:"note"`
	Attach    bool `json:"attach"`
}

// EntryView is an entry as the API shows it.
type EntryView struct {
	ID             uuid.UUID           `json:"id"`
	Type           string              `json:"type"`
	TypeLabel      string              `json:"type_label"`
	Direction      string              `json:"direction"`
	Category       *string             `json:"category"`
	CategoryLabel  *string             `json:"category_label"`
	Method         *string             `json:"method"`
	MethodLabel    *string             `json:"method_label"`
	Amount         string              `json:"amount"`
	SignedAmount   string              `json:"signed_amount"`
	Currency       string              `json:"currency"`
	MinorUnit      int                 `json:"minor_unit"`
	EffectiveDate  string              `json:"effective_date"`
	RecordedAt     time.Time           `json:"recorded_at"`
	Author         PersonRef           `json:"author"`
	Status         string              `json:"status"`
	StatusLabel    string              `json:"status_label"`
	Reconciliation *ReconciliationView `json:"reconciliation"`
	Description    string              `json:"description"`
	Reference      string              `json:"reference"`
	Branch         *string             `json:"branch"`
	// BranchID is the organisation branch the entry belongs to (API 0.57.0).
	BranchID        *uuid.UUID  `json:"branch_id"`
	Contact         *ContactRef `json:"contact"`
	Listing         *ListingRef `json:"listing"`
	Lease           *LeaseRef   `json:"lease"`
	TenancyID       *uuid.UUID  `json:"tenancy_id"`
	PropertyID      *uuid.UUID  `json:"property_id"`
	ReversalOf      *uuid.UUID  `json:"reversal_of"`
	ReversedBy      *uuid.UUID  `json:"reversed_by"`
	CorrectionOf    *uuid.UUID  `json:"correction_of"`
	CorrectedBy     *uuid.UUID  `json:"corrected_by"`
	TransferPair    *uuid.UUID  `json:"transfer_pair"`
	Settles         *uuid.UUID  `json:"settles"`
	Reason          *string     `json:"reason"`
	IsReversed      bool        `json:"is_reversed"`
	Cash            bool        `json:"cash"`
	NoteCount       int         `json:"note_count"`
	AttachmentCount int         `json:"attachment_count"`
	Can             Can         `json:"can"`
}

// NoteView is a note.
type NoteView struct {
	ID        uuid.UUID `json:"id"`
	Text      string    `json:"text"`
	Author    PersonRef `json:"author"`
	CreatedAt time.Time `json:"created_at"`
}

// AttachmentView is an attachment.
type AttachmentView struct {
	ID           uuid.UUID `json:"id"`
	FileName     string    `json:"file_name"`
	ContentType  string    `json:"content_type"`
	Size         int64     `json:"size"`
	UploadedBy   PersonRef `json:"uploaded_by"`
	UploadedAt   time.Time `json:"uploaded_at"`
	DownloadPath string    `json:"download_path"`
	// ScanStatus is the malware scan status (API 0.41.0): PENDING while
	// the file is being checked (it cannot be downloaded yet), CLEAN,
	// INFECTED (removed) or LEGACY_UNSCANNED. Omitted when scanning is off.
	ScanStatus string `json:"scan_status,omitempty"`
}

// CategoryView is a category.
type CategoryView struct {
	Code      string `json:"code"`
	Label     string `json:"label"`
	Kind      string `json:"kind"`
	Active    bool   `json:"active"`
	Default   bool   `json:"default"`
	Used      bool   `json:"used"`
	SortOrder int    `json:"sort_order"`
}

// Page is GET .../entries.
type Page struct {
	Items      []EntryView `json:"items"`
	Page       int         `json:"page"`
	PageSize   int         `json:"page_size"`
	Total      int         `json:"total"`
	TotalPages int         `json:"total_pages"`
}

// EntryDetail is GET .../entries/{id}.
type EntryDetail struct {
	Entry       EntryView        `json:"entry"`
	Notes       []NoteView       `json:"notes"`
	Attachments []AttachmentView `json:"attachments"`
	Related     []EntryView      `json:"related"`
}

// Posted is the answer of a create, a reversal and a correction.
type Posted struct {
	Entry   EntryView   `json:"entry"`
	Created []EntryView `json:"created"`
	Updated []EntryView `json:"updated,omitempty"`
}

// TypeOption is one type of GET .../options.
type TypeOption struct {
	Code      string  `json:"code"`
	Label     string  `json:"label"`
	Direction *string `json:"direction"`
	Cash      bool    `json:"cash"`
	Category  *string `json:"category"`
}

// CodeLabel is a code with its wording.
type CodeLabel struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// StatusOption is one status of GET .../options.
type StatusOption struct {
	Code     string `json:"code"`
	Label    string `json:"label"`
	Settable bool   `json:"settable"`
}

// CurrencyOption is one currency of GET .../options.
type CurrencyOption struct {
	Code      string `json:"code"`
	MinorUnit int    `json:"minor_unit"`
}

// Permissions are the caller's rights in the book.
type Permissions struct {
	Write  bool `json:"write"`
	Manage bool `json:"manage"`
	Export bool `json:"export"`
}

// Options is GET .../options.
type Options struct {
	Types       []TypeOption     `json:"types"`
	Methods     []CodeLabel      `json:"methods"`
	Statuses    []StatusOption   `json:"statuses"`
	Currencies  []CurrencyOption `json:"currencies"`
	Categories  []CategoryView   `json:"categories"`
	Branches    []string         `json:"branches"`
	Permissions Permissions      `json:"permissions"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
