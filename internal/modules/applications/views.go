package applications

import (
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/saved"
)

// FormView is a form as its organisation sees it.
type FormView struct {
	ID              uuid.UUID  `json:"id"`
	FamilyID        uuid.UUID  `json:"family_id"`
	Version         int        `json:"version"`
	Status          string     `json:"status"`
	OrganisationID  *uuid.UUID `json:"organisation_id"`
	OwnerUserID     *uuid.UUID `json:"owner_user_id"`
	Name            string     `json:"name"`
	MarketCode      string     `json:"market_code"`
	Purpose         string     `json:"purpose"`
	PrivacyNotice   string     `json:"privacy_notice"`
	RetentionDays   int        `json:"retention_days"`
	ExpiresAt       *time.Time `json:"expires_at"`
	Sections        []Section  `json:"sections"`
	Warnings        []Warning  `json:"warnings"`
	SubmissionCount int        `json:"submission_count"`
	Locked          bool       `json:"locked"`
	CanEdit         bool       `json:"can_edit"`
	CreatedByName   string     `json:"created_by_name"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
	PublishedAt     *time.Time `json:"published_at"`
	RetiredAt       *time.Time `json:"retired_at"`
}

func sectionsOrEmpty(s []Section) []Section {
	if s == nil {
		return []Section{}
	}
	out := make([]Section, len(s))
	for i, x := range s {
		if x.Fields == nil {
			x.Fields = []Field{}
		}
		out[i] = x
	}
	return out
}

func formView(f Form, canEdit bool) FormView {
	return FormView{ID: f.ID, FamilyID: f.FamilyID, Version: f.Version, Status: f.Status, OrganisationID: f.OrganisationID,
		OwnerUserID: f.OwnerUserID, Name: f.Name, MarketCode: f.MarketCode, Purpose: f.Purpose, PrivacyNotice: f.PrivacyNotice,
		RetentionDays: f.RetentionDays, ExpiresAt: utcPtr(f.ExpiresAt), Sections: sectionsOrEmpty(f.Sections), Warnings: Warnings(f.Sections),
		SubmissionCount: f.Submissions, Locked: f.Submissions > 0, CanEdit: canEdit && f.Status != FormRetired, CreatedByName: f.CreatedByName,
		CreatedAt: f.CreatedAt.UTC(), UpdatedAt: f.UpdatedAt.UTC(), PublishedAt: utcPtr(f.PublishedAt), RetiredAt: utcPtr(f.RetiredAt)}
}

// VersionRef is one version of a form family.
type VersionRef struct {
	ID              uuid.UUID  `json:"id"`
	Version         int        `json:"version"`
	Status          string     `json:"status"`
	PublishedAt     *time.Time `json:"published_at"`
	SubmissionCount int        `json:"submission_count"`
}

// PublicForm is a form as an applicant sees it: no warnings, counts or owner.
type PublicForm struct {
	ID            uuid.UUID  `json:"id"`
	FamilyID      uuid.UUID  `json:"family_id"`
	Version       int        `json:"version"`
	Name          string     `json:"name"`
	MarketCode    string     `json:"market_code"`
	Purpose       string     `json:"purpose"`
	PrivacyNotice string     `json:"privacy_notice"`
	RetentionDays int        `json:"retention_days"`
	ExpiresAt     *time.Time `json:"expires_at"`
	Sections      []Section  `json:"sections"`
}

func publicForm(f Form) PublicForm {
	return PublicForm{ID: f.ID, FamilyID: f.FamilyID, Version: f.Version, Name: f.Name, MarketCode: f.MarketCode, Purpose: f.Purpose,
		PrivacyNotice: f.PrivacyNotice, RetentionDays: f.RetentionDays, ExpiresAt: utcPtr(f.ExpiresAt), Sections: sectionsOrEmpty(f.Sections)}
}

// FormRef names a form version in lists.
type FormRef struct {
	ID       uuid.UUID `json:"id"`
	FamilyID uuid.UUID `json:"family_id"`
	Version  int       `json:"version"`
	Name     string    `json:"name"`
}

// IntakeView is GET/PUT /v1/me/listings/{id}/applications-intake.
type IntakeView struct {
	ListingID    uuid.UUID  `json:"listing_id"`
	FormID       *uuid.UUID `json:"form_id"`
	FormVersion  *int       `json:"form_version"`
	FormName     *string    `json:"form_name"`
	Status       string     `json:"status"`
	StoredStatus string     `json:"stored_status"`
	ClosesAt     *time.Time `json:"closes_at"`
	ClosedReason *string    `json:"closed_reason"`
	Eligible     bool       `json:"eligible"`
	UpdatedAt    *time.Time `json:"updated_at"`
}

// IntakeState is the intake of the public form preview.
type IntakeState struct {
	Status       string     `json:"status"`
	ClosesAt     *time.Time `json:"closes_at"`
	ClosedReason *string    `json:"closed_reason"`
}

// Summary is the `applications` object of a public listing.
type Summary struct {
	Open     bool       `json:"open"`
	ClosesAt *time.Time `json:"closes_at"`
	FormName *string    `json:"form_name"`
}

// FileView is one uploaded file.
type FileView struct {
	ID          uuid.UUID `json:"id"`
	Field       string    `json:"field"`
	FileName    string    `json:"file_name"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	UploadedAt  time.Time `json:"uploaded_at"`
	// ScanStatus is the malware scan status (API 0.41.0): PENDING while
	// the file is being checked (it cannot be downloaded yet), CLEAN,
	// INFECTED (removed) or LEGACY_UNSCANNED. Omitted when scanning is off.
	ScanStatus string `json:"scan_status,omitempty"`
}

func fileView(f File) FileView {
	return FileView{ID: f.ID, Field: f.FieldKey, FileName: f.FileName, ContentType: f.ContentType, Size: f.Size, UploadedAt: f.UploadedAt.UTC(),
		ScanStatus: f.ScanStatus}
}

// TimelineEntry is one entry of an application's timeline.
type TimelineEntry struct {
	ID        uuid.UUID `json:"id"`
	Kind      string    `json:"kind"`
	Status    *string   `json:"status"`
	Label     string    `json:"label"`
	Message   *string   `json:"message"`
	At        time.Time `json:"at"`
	ActorName *string   `json:"actor_name,omitempty"`
}

// InfoRequest is the open request for more information.
type InfoRequest struct {
	Message     string    `json:"message"`
	RequestedAt time.Time `json:"requested_at"`
}

// ApplicantView is an application as its applicant sees it. In lists Form
// is a FormRef and Answers, Files and MissingRequired are left out (nil);
// they are interfaces so that an empty map or list is still written.
type ApplicantView struct {
	ID               uuid.UUID       `json:"id"`
	Reference        *string         `json:"reference"`
	Status           string          `json:"status"`
	StatusLabel      string          `json:"status_label"`
	ListingID        uuid.UUID       `json:"listing_id"`
	Listing          saved.Snapshot  `json:"listing"`
	ListingAvailable bool            `json:"listing_available"`
	Form             any             `json:"form"`
	Answers          any             `json:"answers,omitempty"`
	Files            any             `json:"files,omitempty"`
	MissingRequired  any             `json:"missing_required,omitempty"`
	InfoRequest      *InfoRequest    `json:"info_request"`
	Timeline         []TimelineEntry `json:"timeline"`
	CanEdit          bool            `json:"can_edit"`
	CanSubmit        bool            `json:"can_submit"`
	CanWithdraw      bool            `json:"can_withdraw"`
	Erased           bool            `json:"erased"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
	SubmittedAt      *time.Time      `json:"submitted_at"`
	DecidedAt        *time.Time      `json:"decided_at"`
}

// Applicant names who applied (to the receiving side).
type Applicant struct {
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
	Email  *string    `json:"email"`
}

// RenderedField is one answer laid out as its form version asked it.
type RenderedField struct {
	Key     string     `json:"key"`
	Label   string     `json:"label"`
	Type    string     `json:"type"`
	Visible bool       `json:"visible"`
	Answer  any        `json:"answer"`
	Display *string    `json:"display"`
	Files   []FileView `json:"files"`
}

// RenderedSection is one section of the rendered answers.
type RenderedSection struct {
	Key         string          `json:"key"`
	Title       string          `json:"title"`
	Description *string         `json:"description"`
	Fields      []RenderedField `json:"fields"`
}

// NoteView is one internal note.
type NoteView struct {
	ID           uuid.UUID  `json:"id"`
	Text         string     `json:"text"`
	AuthorUserID *uuid.UUID `json:"author_user_id"`
	AuthorName   string     `json:"author_name"`
	CreatedAt    time.Time  `json:"created_at"`
}

// AgentView is an application as the receiving side sees it. In lists Form
// is a FormRef and Answers, Rendered, Files and Notes are left out.
type AgentView struct {
	ApplicantView
	Applicant           Applicant  `json:"applicant"`
	Rendered            any        `json:"rendered,omitempty"`
	Notes               any        `json:"notes,omitempty"`
	AllowedTransitions  []string   `json:"allowed_transitions"`
	AssignedAgentUserID *uuid.UUID `json:"assigned_agent_user_id"`
}

// MinePage is GET /v1/me/applications.
type MinePage struct {
	Items      []ApplicantView `json:"items"`
	Page       int             `json:"page"`
	PageSize   int             `json:"page_size"`
	Total      int             `json:"total"`
	TotalPages int             `json:"total_pages"`
}

// ReceivedPage is GET /v1/agent/organisations/{orgId}/applications.
type ReceivedPage struct {
	Items        []AgentView    `json:"items"`
	Page         int            `json:"page"`
	PageSize     int            `json:"page_size"`
	Total        int            `json:"total"`
	TotalPages   int            `json:"total_pages"`
	StatusCounts map[string]int `json:"status_counts"`
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func totalPages(total, size int) int {
	if total == 0 || size == 0 {
		return 0
	}
	return (total + size - 1) / size
}
