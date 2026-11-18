package crm

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// PersonRef names a person.
type PersonRef struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
}

// MarketingView is the marketing permission as returned.
type MarketingView struct {
	Granted        bool       `json:"granted"`
	GrantedAt      time.Time  `json:"granted_at"`
	Source         string     `json:"source"`
	RecordedBy     *uuid.UUID `json:"recorded_by"`
	RecordedByName *string    `json:"recorded_by_name"`
}

// ListingView is a linked listing.
type ListingView struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Reference string    `json:"reference"`
	Slug      *string   `json:"slug"`
}

// ContactView is a contact, as listed and in ContactBody.contact.
type ContactView struct {
	ID                  uuid.UUID      `json:"id"`
	Name                string         `json:"name"`
	Emails              []string       `json:"emails"`
	Phones              []string       `json:"phones"`
	Type                string         `json:"type"`
	TypeLabel           string         `json:"type_label"`
	Stage               string         `json:"stage"`
	StageLabel          string         `json:"stage_label"`
	Tags                []string       `json:"tags"`
	OwnerUserID         *uuid.UUID     `json:"owner_user_id"`
	OwnerName           *string        `json:"owner_name"`
	Source              *string        `json:"source"`
	SourceLabel         *string        `json:"source_label"`
	MarketingPermission *MarketingView `json:"marketing_permission"`
	LastContactAt       *time.Time     `json:"last_contact_at"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	// MergedInto is set on a tombstone: the contact this one was merged into.
	MergedInto     *uuid.UUID    `json:"merged_into"`
	LinkedListings []ListingView `json:"linked_listings"`
	// RemindersDue counts the open reminders due now (overdue included);
	// OpenReminders every open one, NextReminderAt the earliest due.
	RemindersDue   int        `json:"reminders_due"`
	OpenReminders  int        `json:"open_reminders"`
	NextReminderAt *time.Time `json:"next_reminder_at"`
	CreatedBy      *uuid.UUID `json:"created_by"`
}

// LinkedItemView is a linked property request, business enquiry or appointment.
type LinkedItemView struct {
	Kind      string     `json:"kind"`
	ID        uuid.UUID  `json:"id"`
	Reference string     `json:"reference"`
	Type      string     `json:"type"`
	Status    string     `json:"status"`
	Title     string     `json:"title"`
	ListingID *uuid.UUID `json:"listing_id"`
	CreatedAt time.Time  `json:"created_at"`
	Auto      bool       `json:"auto"`
	LinkedAt  time.Time  `json:"linked_at"`
}

// ContactBody is the answer of every single-contact endpoint: the contact,
// its linked property requests, enquiries and appointments, and its reminders.
type ContactBody struct {
	Contact     ContactView      `json:"contact"`
	LinkedItems []LinkedItemView `json:"linked_items"`
	Reminders   []ReminderView   `json:"reminders"`
}

// NoteView is a note.
type NoteView struct {
	ID         uuid.UUID  `json:"id"`
	ContactID  uuid.UUID  `json:"contact_id"`
	Text       string     `json:"text"`
	Author     *PersonRef `json:"author"`
	CreatedAt  time.Time  `json:"created_at"`
	MergedFrom *uuid.UUID `json:"merged_from"`
}

// ReminderView is a reminder.
type ReminderView struct {
	ID        uuid.UUID  `json:"id"`
	ContactID uuid.UUID  `json:"contact_id"`
	DueAt     time.Time  `json:"due_at"`
	Text      string     `json:"text"`
	Done      bool       `json:"done"`
	DoneAt    *time.Time `json:"done_at"`
	CreatedBy *PersonRef `json:"created_by"`
	CreatedAt time.Time  `json:"created_at"`
	Overdue   bool       `json:"overdue"`
}

// DueReminderView is one row of GET .../contacts/reminders.
type DueReminderView struct {
	ReminderView
	Contact ContactRef `json:"contact"`
}

// ContactRef names a contact.
type ContactRef struct {
	ID    uuid.UUID  `json:"id"`
	Name  string     `json:"name"`
	Owner *PersonRef `json:"owner"`
}

// TimelineRef is what a timeline entry is about.
type TimelineRef struct {
	Kind      string    `json:"kind"`
	ID        uuid.UUID `json:"id"`
	Reference *string   `json:"reference,omitempty"`
}

// TimelineEntry is one entry of GET .../contacts/{id}/timeline, newest first.
// Kind is NOTE_ADDED, CREATED, EDITED, STAGE_CHANGED, OWNER_CHANGED,
// REMINDER_ADDED, REMINDER_DONE, REMINDER_REOPENED, LISTING_LINKED,
// LISTING_UNLINKED, MERGED, MERGED_INTO, or the kind of a linked item
// (PROPERTY_REQUEST, BUSINESS_ENQUIRY, APPOINTMENT). Label is a sentence to
// show; text a note's, a reminder's or a stage change's text; ref what it
// is about; data the raw details.
type TimelineEntry struct {
	ID        uuid.UUID      `json:"id"`
	Kind      string         `json:"kind"`
	At        time.Time      `json:"at"`
	ActorName *string        `json:"actor_name,omitempty"`
	Actor     *PersonRef     `json:"actor"`
	Label     string         `json:"label,omitempty"`
	Text      *string        `json:"text,omitempty"`
	Ref       *TimelineRef   `json:"ref,omitempty"`
	Data      map[string]any `json:"data"`
}

// Page is a page of contacts. StageCounts counts the contacts matching the
// filters other than stage, per stage.
type Page struct {
	Items       []ContactView  `json:"items"`
	Page        int            `json:"page"`
	PageSize    int            `json:"page_size"`
	Total       int            `json:"total"`
	TotalPages  int            `json:"total_pages"`
	StageCounts map[string]int `json:"stage_counts"`
}

// TagCount is one tag of a book and how many live contacts carry it.
type TagCount struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// Assignee is a member who may own contacts.
type Assignee struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
	Role   string    `json:"role"`
}

// Optional is a JSON field that distinguishes "absent" from null.
type Optional[T any] struct {
	Set   bool
	Value *T
}

// UnmarshalJSON implements json.Unmarshaler.
func (o *Optional[T]) UnmarshalJSON(raw []byte) error {
	o.Set = true
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		o.Value = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	o.Value = &v
	return nil
}

// MarketingInput is the marketing permission sent by a client.
type MarketingInput struct {
	Granted *bool  `json:"granted"`
	Source  string `json:"source"`
}

// CreateInput is the body of POST .../contacts.
type CreateInput struct {
	Name                string          `json:"name"`
	Emails              []string        `json:"emails"`
	Phones              []string        `json:"phones"`
	Type                string          `json:"type"`
	Tags                []string        `json:"tags"`
	OwnerUserID         *uuid.UUID      `json:"owner_user_id"`
	Source              *string         `json:"source"`
	Stage               *string         `json:"stage"`
	MarketingPermission *MarketingInput `json:"marketing_permission"`
	Notes               *string         `json:"notes"`
}

// PatchInput is the body of PATCH .../contacts/{id}. Absent fields are
// unchanged; source, marketing_permission and last_contact_at may be null.
type PatchInput struct {
	Name                *string                  `json:"name"`
	Emails              *[]string                `json:"emails"`
	Phones              *[]string                `json:"phones"`
	Type                *string                  `json:"type"`
	Tags                *[]string                `json:"tags"`
	Source              Optional[string]         `json:"source"`
	MarketingPermission Optional[MarketingInput] `json:"marketing_permission"`
	LastContactAt       Optional[time.Time]      `json:"last_contact_at"`
}

// ValidationError lists the fields that need attention.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// DuplicateRef names a contact a create or edit would duplicate.
type DuplicateRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// DuplicateError is a create or edit that would duplicate other contacts.
type DuplicateError struct{ Contacts []DuplicateRef }

func (e *DuplicateError) Error() string { return "duplicate contact" }
