// Package agreements is electronic signing in the style of Adobe Acrobat
// Sign (FR-032): an agreement wraps one PDF (a generated lease, or a PDF the
// sender uploads), an ordered list of recipients and the fields placed on the
// document for each of them.
//
//   - Recipients: SIGNER, APPROVER or CC, each with an order. Recipients
//     with the same order act in parallel; those of order n are emailed only
//     when everybody of the earlier orders has finished. A CC is never asked
//     to act and gets the completed copy.
//   - Fields: SIGNATURE, INITIALS, DATE_SIGNED (filled when they sign), NAME
//     (filled), TEXT, CHECKBOX and STAMP (a stamp the server generates from
//     the text the signer types), in PDF points from the page's top-left.
//   - Signing by link: every recipient gets their own link (only the
//     token's SHA-256 is stored), valid until the agreement expires and,
//     read-only, for 90 days after it is completed. Before submitting, the
//     recipient proves they hold the email address with a six-digit code.
//   - Completion: every field value is drawn onto the original pages, an
//     audit report is appended, and the SHA-256 of the result is recorded so
//     that a copy can be checked later (GET /v1/verify/{reference},
//     POST /v1/verify).
//
// What it is not: a qualified or advanced electronic signature, a digital
// seal (no platform certificate is applied), witnessing, stamping or
// registration, or legal advice. Nothing may call an agreement legally
// executed.
package agreements

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Agreement statuses.
const (
	StatusDraft             = "DRAFT"
	StatusOutForSignature   = "OUT_FOR_SIGNATURE"
	StatusCompleted         = "COMPLETED"
	StatusDeclined          = "DECLINED"
	StatusCancelled         = "CANCELLED"
	StatusExpired           = "EXPIRED"
	SourceUpload            = "UPLOAD"
	SourceLease             = "LEASE"
	ReminderNone            = "NONE"
	ReminderDaily           = "DAILY"
	ReminderEvery3Days      = "EVERY_3_DAYS"
	ReminderWeekly          = "WEEKLY"
	DefaultExpiry           = 30 * 24 * time.Hour
	MaxExpiry               = 365 * 24 * time.Hour
	ReadOnlyTTL             = 90 * 24 * time.Hour
	MaxRecipients           = 20
	MaxFields               = 500
	MaxPDFBytes             = 20 << 20
	MaxPDFPages             = 50
	MaxNameRunes            = 200
	MaxMessageRunes         = 2000
	MaxTextValueRunes       = 500
	MaxDrawnBytes           = 200 << 10
	MaxUploadedBytes        = 500 << 10
	CodeTTL                 = 10 * time.Minute
	CodeLength              = 6
	CodeMaxAttempts         = 5
	CodeResendInterval      = 30 * time.Second
	CodeMaxSends            = 10
	DefaultPageSize         = 20
	MaxPageSize             = 100
	ReferencePrefix         = "RPA"
	DocumentKeyPrefix       = "agreements"
	SignatureKeyPrefix      = "agreementsignatures"
	SavedSignatureKeyPrefix = "usersignatures"
	ConsentText             = "I agree to use electronic records and signatures, and to sign this document electronically."
)

// Recipient roles and statuses.
const (
	RoleSigner   = "SIGNER"
	RoleApprover = "APPROVER"
	RoleCC       = "CC"

	// RecipientWaiting: an earlier order has not finished.
	RecipientWaiting = "WAITING"
	// RecipientActive: emailed; may act now.
	RecipientActive   = "ACTIVE"
	RecipientSigned   = "SIGNED"
	RecipientApproved = "APPROVED"
	RecipientDeclined = "DECLINED"
	// RecipientCC: a CC; gets the completed copy.
	RecipientCC = "CC"
)

// Field types.
const (
	FieldSignature  = "SIGNATURE"
	FieldInitials   = "INITIALS"
	FieldDateSigned = "DATE_SIGNED"
	FieldName       = "NAME"
	FieldText       = "TEXT"
	FieldCheckbox   = "CHECKBOX"
	// FieldStamp takes the signer's generated stamp (stamp.go), dated the
	// day they sign. SIGNER recipients only.
	FieldStamp = "STAMP"
)

// Adopted signature kinds and how an email address was verified.
const (
	AdoptTyped    = "TYPED"
	AdoptDrawn    = "DRAWN"
	AdoptUploaded = "UPLOADED"

	VerifiedByEmailCode = "EMAIL_CODE"
	VerifiedByDemoCode  = "DEMO_CODE"
)

// Audit actions (agreement_events).
const (
	EventCreated           = "CREATED"
	EventDocumentAdded     = "DOCUMENT_ADDED"
	EventFieldsChanged     = "FIELDS_CHANGED"
	EventSent              = "SENT"
	EventEmailed           = "EMAILED"
	EventViewed            = "VIEWED"
	EventCodeSent          = "CODE_SENT"
	EventEmailVerified     = "EMAIL_VERIFIED"
	EventSigned            = "SIGNED"
	EventApproved          = "APPROVED"
	EventDeclined          = "DECLINED"
	EventReminded          = "REMINDED"
	EventRecipientReplaced = "RECIPIENT_REPLACED"
	EventCancelled         = "CANCELLED"
	EventExpired           = "EXPIRED"
	EventCompleted         = "COMPLETED"
	EventCopySent          = "COPY_SENT"

	ActorUser      = "USER"
	ActorRecipient = "RECIPIENT"
	ActorSystem    = "SYSTEM"
)

// List views of GET /v1/me/agreements (Adobe's Manage page).
const (
	ViewWaitingForYou    = "WAITING_FOR_YOU"
	ViewWaitingForOthers = "WAITING_FOR_OTHERS"
	ViewCompleted        = "COMPLETED"
	ViewDrafts           = "DRAFTS"
)

// Error codes.
const (
	CodeAgreementNotFound = "AGREEMENT_NOT_FOUND"
	CodeAgreementState    = "AGREEMENT_STATE_CONFLICT"
	CodeRecipientNotFound = "RECIPIENT_NOT_FOUND"
	CodeDocumentMissing   = "AGREEMENT_DOCUMENT_REQUIRED"
	CodeDocumentInvalid   = "UNSUPPORTED_FILE_TYPE"
	CodeFileTooLarge      = "FILE_TOO_LARGE"
	CodeFieldsIncomplete  = "AGREEMENT_FIELDS_INCOMPLETE"
	CodeLinkNotFound      = "SIGNING_LINK_NOT_FOUND"
	CodeLinkExpired       = "SIGNING_LINK_EXPIRED"
	CodeLinkNoLongerValid = "SIGNING_LINK_NO_LONGER_VALID"
	CodeNotYourTurn       = "NOT_OPEN_FOR_SIGNING"
	CodeAlreadyDone       = "ALREADY_SIGNED"
	CodeCodeExpired       = "CODE_EXPIRED"
	CodeNotFoundVerify    = "AGREEMENT_NOT_FOUND"
)

// Errors.
var (
	ErrForbidden         = errors.New("not allowed")
	ErrNotFound          = errors.New("agreement not found")
	ErrState             = errors.New("the agreement is not in a state that allows this")
	ErrRecipientNotFound = errors.New("recipient not found")
	ErrDocumentMissing   = errors.New("the agreement has no document")
	ErrDocumentInvalid   = errors.New("the file is not a PDF that can be read")
	ErrFileTooLarge      = errors.New("the file is too large")
	ErrLinkNotFound      = errors.New("signing link not found")
	ErrLinkExpired       = errors.New("signing link expired")
	ErrLinkNoLongerValid = errors.New("signing link replaced, or the agreement was cancelled")
	ErrNotYourTurn       = errors.New("the recipient cannot act now")
	ErrAlreadyDone       = errors.New("the recipient already signed, approved or declined")
	ErrCodeExpired       = errors.New("no live code")
	ErrReferenceTaken    = errors.New("reference already used")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct {
	Code   string
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "validation failed" }

// ResendLaterError says when another code may be sent.
type ResendLaterError struct {
	After     time.Duration
	Exhausted bool
}

func (e *ResendLaterError) Error() string { return "code refused: too soon or too many" }

// PageSize is a page's size in PDF points.
type PageSize struct {
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

// Document is a stored PDF of an agreement.
type Document struct {
	ObjectKey string
	SHA256    string
	Size      int64
	FileName  string
}

// Adopted is a signature (or initials) a recipient adopted.
type Adopted struct {
	Kind     string `json:"kind"`
	Text     string `json:"text,omitempty"`
	Font     string `json:"font,omitempty"`
	ImageKey string `json:"image_key,omitempty"`
}

// Recipient is one person an agreement is sent to.
type Recipient struct {
	ID    uuid.UUID
	Order int
	Role  string
	Name  string
	Email string // lower-cased
	// Label is how the document names them ("Landlord 1"), may be "".
	Label         string
	Status        string
	ActivatedAt   *time.Time
	FirstViewedAt *time.Time
	CompletedAt   *time.Time
	DeclinedAt    *time.Time
	DeclineReason string
	// The live code: HMAC-SHA256 keyed with the raw link token.
	CodeHash      []byte
	CodeLinkID    *uuid.UUID
	CodeExpiresAt *time.Time
	CodeAttempts  int
	CodeSentAt    *time.Time
	CodeSendCount int
	// Set when they signed or approved.
	IP           string
	UserAgent    string
	Verification string
	Signature    *Adopted
	Initials     *Adopted
	// Stamp is the stamp they applied (when they had a STAMP field).
	Stamp *Stamp
}

// Done reports whether the recipient finished (signed, approved or declined).
func (r Recipient) Done() bool {
	return r.Status == RecipientSigned || r.Status == RecipientApproved || r.Status == RecipientDeclined
}

// Field is one field placed on the document for a recipient.
type Field struct {
	ID          uuid.UUID
	RecipientID uuid.UUID
	Type        string
	Page        int
	X, Y, W, H  float64
	Required    bool
	Label       string
	Value       string
}

// Link is a recipient's signing link (only the token's SHA-256).
type Link struct {
	ID          uuid.UUID
	RecipientID uuid.UUID
	TokenHash   []byte
	ExpiresAt   time.Time
	RevokedAt   *time.Time
	CreatedAt   time.Time
}

// Agreement is the aggregate the store loads and saves whole.
type Agreement struct {
	ID                uuid.UUID
	Reference         string
	Name              string
	Message           string
	Status            string
	Source            string
	LeaseID           *uuid.UUID
	SenderID          uuid.UUID
	SenderName        string
	SenderEmail       string
	OrganisationID    *uuid.UUID
	OrganisationName  string
	ExpiresAt         time.Time
	ReminderFrequency string
	Original          *Document
	Pages             []PageSize
	Final             *Document
	Recipients        []Recipient
	Fields            []Field
	Links             []Link
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SentAt            *time.Time
	CompletedAt       *time.Time
	DeclinedAt        *time.Time
	CancelledAt       *time.Time
	ExpiredAt         *time.Time
	CancelReason      string
}

// Recipient returns the recipient with that ID.
func (a *Agreement) Recipient(id uuid.UUID) *Recipient {
	for i := range a.Recipients {
		if a.Recipients[i].ID == id {
			return &a.Recipients[i]
		}
	}
	return nil
}

// Event is one record of an agreement's audit trail. Immutable.
type Event struct {
	ID          uuid.UUID
	AgreementID uuid.UUID
	Action      string
	ActorType   string
	ActorUserID *uuid.UUID
	ActorName   string
	RecipientID *uuid.UUID
	IP          string
	UserAgent   string
	Detail      map[string]any
	At          time.Time
}

// Filter selects agreements (see leases.LeaseFilter for the scope rules).
type Filter struct {
	All                 bool
	Organisations       []uuid.UUID
	Sender              *uuid.UUID
	SenderOrganisations []uuid.UUID
	SenderDirect        bool
	Status              string
	Statuses            []string
}

// SavedSignature is a signed-in user's adopted signature and initials, and
// their stamp.
type SavedSignature struct {
	Signature *Adopted
	Initials  *Adopted
	Stamp     *Stamp
	UpdatedAt time.Time
}

// Store persists agreements.
type Store interface {
	NextReference(ctx context.Context, year int) (string, error)
	CreateAgreement(ctx context.Context, a Agreement, events []Event) error
	Agreement(ctx context.Context, id uuid.UUID) (Agreement, error)
	Agreements(ctx context.Context, f Filter) ([]Agreement, error)
	// ChangeAgreement locks the agreement, calls change with a copy and
	// saves the whole aggregate with the events. The callback must not call
	// the store.
	ChangeAgreement(ctx context.Context, id uuid.UUID, change func(a *Agreement) ([]Event, error)) (Agreement, error)
	DeleteAgreement(ctx context.Context, id uuid.UUID) (Agreement, error)
	LinkByToken(ctx context.Context, tokenHash []byte) (Link, uuid.UUID, error)
	// RecipientAgreements returns the agreements (not drafts) with a
	// recipient of that email address.
	RecipientAgreements(ctx context.Context, email string) ([]Agreement, error)
	ByReference(ctx context.Context, reference string) (Agreement, error)
	// BySHA256 returns the completed agreement whose original or final
	// document has that hash, and which of the two it is.
	BySHA256(ctx context.Context, sha string) (Agreement, string, error)
	Events(ctx context.Context, agreementID uuid.UUID) ([]Event, error)
	// ClaimJob records a job key once: true the first time (reminders).
	ClaimJob(ctx context.Context, key string, at time.Time) (bool, error)
	SavedSignature(ctx context.Context, userID uuid.UUID) (SavedSignature, bool, error)
	PutSavedSignature(ctx context.Context, userID uuid.UUID, s SavedSignature) error
}

// SigningMail is the email with a recipient's link (a request or a reminder).
type SigningMail struct {
	Token         string
	Name          string
	RoleLabel     string
	AgreementName string
	Reference     string
	SenderName    string
	Message       string
	ExpiresAt     time.Time
	Reminder      bool
}

// CodeMail is a recipient's six-digit code.
type CodeMail struct {
	Name      string
	Code      string
	Reference string
}

// CompletedMail tells a recipient (Token: their read-only link) or the
// sender (AgreementID: their agreement page) that the agreement is complete.
type CompletedMail struct {
	Name           string
	AgreementName  string
	Reference      string
	Token          string
	AgreementID    uuid.UUID
	CompletedAt    time.Time
	AvailableUntil time.Time
}

// DeclinedMail tells the sender that a recipient declined.
type DeclinedMail struct {
	Name          string
	AgreementName string
	Reference     string
	RecipientName string
	Reason        string
	AgreementID   uuid.UUID
}

// Mailer sends the agreement emails from the request path (links and codes
// are credentials; they never go through the outbox).
type Mailer interface {
	SendSigning(ctx context.Context, to string, m SigningMail) error
	SendCode(ctx context.Context, to string, m CodeMail) error
	SendCompleted(ctx context.Context, to string, m CompletedMail) error
	SendDeclined(ctx context.Context, to string, m DeclinedMail) error
}
