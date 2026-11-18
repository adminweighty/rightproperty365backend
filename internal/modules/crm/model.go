// Package crm is the agent CRM (API 0.31.0, BR-004, FR-014): the private
// contact book of an organisation, or of an approved direct owner.
//
// A contact has emails and phones (normalised: lower-cased emails, E.164
// phones), a type and a source (reference sets contact_types and
// contact_sources), a lead stage, a relationship owner, tags, a marketing
// permission, notes (append-only), reminders, linked listings and an
// activity log. Property requests, business enquiries and appointments that
// reach the book are linked to the contacts whose email or phone they share
// (exact match after normalisation, same book only): when they arrive
// (outbox events, in the worker) and when a contact is created or edited.
//
// Who may do what is decided in internal/platform/auth/capabilities.go
// (MayReadContacts, MayWriteContacts, MayManageContacts, OwnsContactBook).
// A book that is not the caller's is 404 CONTACT_NOT_FOUND, never data.
// Reassignments, merges, imports and exports are written to the
// organisation's audit trail (orgaudit) in the same transaction.
package crm

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Lead stages (FSD section 3). Any stage may move to any other.
const (
	StageNew              = "NEW"
	StageContacted        = "CONTACTED"
	StageViewingRequested = "VIEWING_REQUESTED"
	StageViewingConfirmed = "VIEWING_CONFIRMED"
	StageNegotiating      = "NEGOTIATING"
	StageClosed           = "CLOSED"
	StageUnsuccessful     = "UNSUCCESSFUL"
)

var stageLabels = map[string]string{
	StageNew:              "New",
	StageContacted:        "Contacted",
	StageViewingRequested: "Viewing requested",
	StageViewingConfirmed: "Viewing confirmed",
	StageNegotiating:      "Negotiating",
	StageClosed:           "Closed",
	StageUnsuccessful:     "Unsuccessful",
}

// Stages returns every lead stage in pipeline order.
func Stages() []string {
	return []string{StageNew, StageContacted, StageViewingRequested, StageViewingConfirmed, StageNegotiating, StageClosed, StageUnsuccessful}
}

// StageLabel is the wording shown for a stage.
func StageLabel(stage string) string {
	if l, ok := stageLabels[stage]; ok {
		return l
	}
	return stage
}

// Reference sets (see the reference module).
const (
	SetContactTypes   = "contact_types"
	SetContactSources = "contact_sources"
)

// Codes the API itself uses.
const (
	TypeOther     = "other"
	SourceEnquiry = "enquiry"
	SourceImport  = "import"
)

// Kinds of linked item.
const (
	KindPropertyRequest = "PROPERTY_REQUEST"
	KindBusinessEnquiry = "BUSINESS_ENQUIRY"
	KindAppointment     = "APPOINTMENT"
	// KindApplication is a submitted application to a listing (API 0.32.0).
	KindApplication = "APPLICATION"
	KindListing     = "LISTING"
)

// ItemKinds are the kinds linked automatically (and that /from-request accepts).
func ItemKinds() []string {
	return []string{KindPropertyRequest, KindBusinessEnquiry, KindAppointment, KindApplication}
}

// Activity log entries (contact_events.kind).
const (
	EventCreated         = "CREATED"
	EventEdited          = "EDITED"
	EventStageChanged    = "STAGE_CHANGED"
	EventOwnerChanged    = "OWNER_CHANGED"
	EventListingLinked   = "LISTING_LINKED"
	EventListingUnlinked = "LISTING_UNLINKED"
	EventItemLinked      = "ITEM_LINKED"
	EventMerged          = "MERGED"
	EventMergedInto      = "MERGED_INTO"
	EventDeleted         = "DELETED"
	EventReminderAdded   = "REMINDER_ADDED"
	EventReminderDone    = "REMINDER_DONE"
	EventReminderReopen  = "REMINDER_REOPENED"
	EventImported        = "IMPORTED"
)

// EventKinds returns every activity log kind.
func EventKinds() []string {
	return []string{EventCreated, EventEdited, EventStageChanged, EventOwnerChanged, EventListingLinked, EventListingUnlinked,
		EventItemLinked, EventMerged, EventMergedInto, EventDeleted, EventReminderAdded, EventReminderDone, EventReminderReopen, EventImported}
}

// Outbox.
const (
	// EventReminderDue is written by the worker when a reminder falls due.
	// Nothing sends an email for it in 0.31.0; the dashboard reads
	// GET .../contacts/reminders.
	EventReminderDue = "CONTACT_REMINDER_DUE"
	AggregateContact = "contact"
)

// OutboxEventTypes are the outbox events that link items to contacts.
func OutboxEventTypes() []string {
	return []string{"PROPERTY_REQUEST_CREATED", "BUSINESS_ENQUIRY_CREATED", "APPOINTMENT_REQUESTED", "APPLICATION_SUBMITTED"}
}

// Limits.
const (
	MaxNameLength      = 200
	MaxEmails          = 10
	MaxPhones          = 10
	MaxTags            = 20
	MaxTagLength       = 40
	MaxNoteLength      = 5000
	MaxReminderLength  = 500
	MaxMarketingSource = 120
	MaxMergeIDs        = 20
	// MaxImportRows and MaxImportBytes cap POST .../contacts/import.
	MaxImportRows  = 5000
	MaxImportBytes = 2 << 20
	// MaxImportBodyBytes is the request body limit of the import (the file
	// and the multipart framing).
	MaxImportBodyBytes = MaxImportBytes + 64<<10
	// MaxExportRows caps GET .../contacts/export.csv.
	MaxExportRows = 50000
)

// Errors.
var (
	ErrUnauthenticated  = errors.New("not signed in")
	ErrNotFound         = errors.New("contact not found")
	ErrForbidden        = errors.New("not allowed")
	ErrMerged           = errors.New("contact was merged")
	ErrConflict         = errors.New("contact changed concurrently")
	ErrReminderNotFound = errors.New("reminder not found")
	ErrListingNotFound  = errors.New("listing not found")
	ErrItemNotFound     = errors.New("request not found")
)

// Book is whose contact book: an organisation's, or a direct owner's
// personal one. Exactly one field is set.
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

// Marketing is the recorded marketing permission.
type Marketing struct {
	Granted    bool       `json:"granted"`
	GrantedAt  time.Time  `json:"granted_at"`
	Source     string     `json:"source"`
	RecordedBy *uuid.UUID `json:"recorded_by"`
}

// Contact is one contact of a book.
type Contact struct {
	ID            uuid.UUID
	Book          Book
	Name          string
	Type          string
	Stage         string
	OwnerUserID   *uuid.UUID
	Source        string // "" when none
	Marketing     *Marketing
	LastContactAt *time.Time
	CreatedBy     *uuid.UUID
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeletedAt     *time.Time
	MergedInto    *uuid.UUID
	// Emails (lower-cased) and Phones (E.164), in the order given.
	Emails []string
	Phones []string
	// Tags are lower-cased, in the order given.
	Tags []string

	// Read-only, computed by the stores: open reminders, the earliest due,
	// the open ones due at ListFilter.Now (List only; the service counts it
	// otherwise) and the linked listings.
	OpenReminders  int
	NextReminderAt *time.Time
	DueReminders   int
	ListingIDs     []uuid.UUID
}

// Live reports whether the contact is neither deleted nor merged.
func (c Contact) Live() bool { return c.DeletedAt == nil && c.MergedInto == nil }

// Note is an internal note: append-only.
type Note struct {
	ID           uuid.UUID
	ContactID    uuid.UUID
	Text         string
	AuthorUserID *uuid.UUID
	AuthorName   string
	CreatedAt    time.Time
	// MergedFrom is the contact the note was written on before a merge.
	MergedFrom *uuid.UUID
}

// Reminder is a follow-up reminder.
type Reminder struct {
	ID            uuid.UUID
	ContactID     uuid.UUID
	DueAt         time.Time
	Text          string
	Done          bool
	DoneAt        *time.Time
	DoneBy        *uuid.UUID
	CreatedBy     *uuid.UUID
	CreatedByName string
	CreatedAt     time.Time
	NotifiedAt    *time.Time
}

// Event is one entry of a contact's activity log.
type Event struct {
	ID          uuid.UUID
	ContactID   uuid.UUID
	Kind        string
	ActorUserID *uuid.UUID
	ActorName   string
	Data        map[string]any
	At          time.Time
}

// Link ties a contact to a listing, property request, business enquiry or appointment.
type Link struct {
	ContactID uuid.UUID
	Kind      string
	ItemID    uuid.UUID
	// Auto is true for a link made by matching an email or phone.
	Auto     bool
	LinkedBy *uuid.UUID
	LinkedAt time.Time
}

// ListFilter narrows GET .../contacts.
type ListFilter struct {
	Q     string
	Type  string
	Stage string
	Tag   string
	// Owner keeps the contacts of that relationship owner; Unowned the ones without.
	Owner   *uuid.UUID
	Unowned bool
	// ReminderDueBefore keeps contacts with an open reminder due before it.
	ReminderDueBefore *time.Time
	// Sort is SortLastContact, SortName or SortCreated.
	Sort string
	// Limit 0 means no limit.
	Limit, Offset int
	// Now is the time Contact.DueReminders is counted at.
	Now time.Time
}

// Sorts of the list.
const (
	SortLastContact = "last_contact"
	SortName        = "name"
	SortCreated     = "created"
)

// Merge moves everything of From onto Keep (notes, reminders, links and the activity log).
type Merge struct {
	Keep uuid.UUID
	From []uuid.UUID
}

// Change is one atomic write. Stores apply it in this order: check Expect
// (the contacts' updated_at), upsert Contacts, apply Merge, insert Notes,
// upsert Reminders, insert Events, insert Links (a link that exists is left
// alone), delete Unlinks, insert Audit.
type Change struct {
	Expect    map[uuid.UUID]time.Time
	Contacts  []Contact
	Merge     *Merge
	Notes     []Note
	Reminders []Reminder
	Events    []Event
	Links     []Link
	Unlinks   []Link
	Audit     []orgaudit.Event
}

// DueReminder is an open reminder with its contact.
type DueReminder struct {
	Reminder
	ContactName string
	OwnerUserID *uuid.UUID
}

// Store persists contact books.
type Store interface {
	// Get returns a contact whatever its state (the service hides deleted
	// ones), or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Contact, error)
	// List returns one page of the live contacts of a book and the total.
	List(ctx context.Context, book Book, f ListFilter) ([]Contact, int, error)
	// StageCounts counts the live contacts of a book that pass the filter
	// (its Stage, Sort, Limit and Offset ignored), per stage.
	StageCounts(ctx context.Context, book Book, f ListFilter) (map[string]int, error)
	// Tags returns the tags of the live contacts of a book with their counts,
	// most used first.
	Tags(ctx context.Context, book Book) ([]TagCount, error)
	// Matching returns the live contacts of a book that have one of the
	// emails or phones (already normalised).
	Matching(ctx context.Context, book Book, emails, phones []string) ([]Contact, error)
	// Apply writes a Change atomically. ErrConflict when an expectation fails.
	Apply(ctx context.Context, ch Change) error
	Notes(ctx context.Context, contactID uuid.UUID) ([]Note, error)
	Reminders(ctx context.Context, contactID uuid.UUID) ([]Reminder, error)
	Events(ctx context.Context, contactID uuid.UUID) ([]Event, error)
	Links(ctx context.Context, contactID uuid.UUID) ([]Link, error)
	// DueReminders returns the open reminders of live contacts of the book
	// due before before, soonest first.
	DueReminders(ctx context.Context, book Book, before time.Time) ([]DueReminder, error)
	// ClaimDueReminders marks up to limit open reminders due at or before now
	// that were not yet notified, writes one CONTACT_REMINDER_DUE outbox event
	// for each, and returns them.
	ClaimDueReminders(ctx context.Context, now time.Time, limit int) ([]Reminder, error)
}

// Item is a property request, business enquiry, appointment or submitted
// application as the CRM
// sees it: who it reached and how to reach its customer.
type Item struct {
	Kind      string
	ID        uuid.UUID
	Reference string
	// Type is the request type, the enquiry's service or the appointment type.
	Type   string
	Status string
	// Title names what it is about: a listing's title, a profile's name.
	Title     string
	ListingID *uuid.UUID
	// Who it reached: an organisation (AssignedUserID is the member it is
	// assigned to, nil for the organisation inbox), or a direct owner.
	OrganisationID    *uuid.UUID
	AssignedUserID    *uuid.UUID
	DirectOwnerUserID *uuid.UUID
	MarketCode        string
	// The customer as entered, and their account's email when verified.
	Name         string
	Email        string
	Phone        string
	AccountEmail string
	CreatedAt    time.Time
}

// Book returns the book an item belongs to (ok false when none: a routing exception).
func (it Item) Book() (Book, bool) {
	if it.OrganisationID != nil {
		return OrganisationBook(*it.OrganisationID), true
	}
	if it.DirectOwnerUserID != nil {
		return PersonalBook(*it.DirectOwnerUserID), true
	}
	return Book{}, false
}

// Items reads property requests, business enquiries and appointments.
type Items interface {
	// Item returns one item of a kind, or ErrItemNotFound.
	Item(ctx context.Context, kind string, id uuid.UUID) (Item, error)
	// Candidates returns the items of a book whose entered email or verified
	// account email is one of emails (lower-cased), or whose phone ends with
	// one of the digit strings in phoneTails. The caller checks the exact
	// match after normalisation.
	Candidates(ctx context.Context, book Book, emails, phoneTails []string) ([]Item, error)
}

// ListingRef is a listing as the CRM shows it.
type ListingRef struct {
	ID             uuid.UUID
	Title          string
	Reference      string
	Slug           string
	OrganisationID *uuid.UUID
	SellerUserID   *uuid.UUID
	MarketCode     string
}

// Listings looks listings up.
type Listings interface {
	// Listings returns the listings among ids (any status).
	Listings(ctx context.Context, ids []uuid.UUID) ([]ListingRef, error)
	// OwnerMarket returns the market of a direct owner's most recent listing ("" when none).
	OwnerMarket(ctx context.Context, userID uuid.UUID) (string, error)
}

// People names people and says who can own an organisation's contacts.
type People interface {
	// Assignable returns the name of an active member of the organisation
	// who may hold leads (OWNER, MANAGER, AGENT); ok false otherwise.
	Assignable(ctx context.Context, organisationID, userID uuid.UUID) (name string, ok bool, err error)
	// Name returns a person's name ("" when unknown).
	Name(ctx context.Context, userID uuid.UUID) (string, error)
}

// Members lists an organisation's members (for the assignees).
type Members func(ctx context.Context, organisationID uuid.UUID) ([]Assignee, error)

// Vocabulary supplies the active codes and labels of a reference set.
type Vocabulary interface {
	Labels(ctx context.Context, set string) (map[string]string, error)
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

func validStage(s string) bool { return slices.Contains(Stages(), s) }
