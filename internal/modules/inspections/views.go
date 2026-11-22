package inspections

import (
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
)

// The JSON of the contract 0.36 "Response shapes (d)".

// CodeLabel is a code and its wording.
type CodeLabel struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// TemplateRef is the template an inspection was copied from.
type TemplateRef struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// PhotoCan is what the caller may do with a photo.
type PhotoCan struct {
	Delete bool `json:"delete"`
}

// PhotoView is a photo as the managers see it.
type PhotoView struct {
	ID          uuid.UUID           `json:"id"`
	ItemID      uuid.UUID           `json:"item_id"`
	URL         string              `json:"url"`
	ContentType string              `json:"content_type"`
	Width       int                 `json:"width"`
	Height      int                 `json:"height"`
	Size        int64               `json:"size"`
	UploadedBy  tenancies.PersonRef `json:"uploaded_by"`
	UploadedAt  time.Time           `json:"uploaded_at"`
	Can         PhotoCan            `json:"can"`
}

// BaselineView is the move-in finding of an item of a move-out.
type BaselineView struct {
	Status      *string `json:"status"`
	StatusLabel *string `json:"status_label"`
	Note        string  `json:"note"`
}

// BaselineRef is the move-in a move-out is compared with.
type BaselineRef struct {
	ID          uuid.UUID `json:"id"`
	Reference   string    `json:"reference"`
	CompletedAt time.Time `json:"completed_at"`
}

// ItemView is one item as the managers see it.
type ItemView struct {
	ID          uuid.UUID     `json:"id"`
	Label       string        `json:"label"`
	Position    int           `json:"position"`
	Status      *string       `json:"status"`
	StatusLabel *string       `json:"status_label"`
	Note        string        `json:"note"`
	Photos      []PhotoView   `json:"photos"`
	Baseline    *BaselineView `json:"baseline"`
}

// AreaView is one area as the managers see it.
type AreaView struct {
	ID       uuid.UUID  `json:"id"`
	Label    string     `json:"label"`
	Position int        `json:"position"`
	Items    []ItemView `json:"items"`
}

// SummaryProperty is the property of a summary.
type SummaryProperty struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Archived bool      `json:"archived"`
}

// FullProperty is the property of a full inspection.
type FullProperty struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Address    string    `json:"address"`
	MarketCode string    `json:"market_code"`
	Archived   bool      `json:"archived"`
}

// UnitRef is the unit of an inspection.
type UnitRef struct {
	ID       uuid.UUID `json:"id"`
	Label    string    `json:"label"`
	Archived bool      `json:"archived"`
}

// Summary is an inspection in a list.
type Summary struct {
	ID           uuid.UUID  `json:"id"`
	Reference    string     `json:"reference"`
	Type         string     `json:"type"`
	TypeLabel    string     `json:"type_label"`
	Status       string     `json:"status"`
	StatusLabel  string     `json:"status_label"`
	Property     any        `json:"property"`
	Unit         *UnitRef   `json:"unit"`
	TenancyID    *uuid.UUID `json:"tenancy_id"`
	ScheduledFor time.Time  `json:"scheduled_for"`
	// TimeZone is the property market's IANA zone: scheduled_for is shown
	// in it first (API 0.62.0).
	TimeZone    string              `json:"time_zone"`
	Inspector   tenancies.PersonRef `json:"inspector"`
	Template    TemplateRef         `json:"template"`
	Progress    Progress            `json:"progress"`
	PhotoCount  int                 `json:"photo_count"`
	StartedAt   *time.Time          `json:"started_at"`
	CompletedAt *time.Time          `json:"completed_at"`
	CancelledAt *time.Time          `json:"cancelled_at"`
	CreatedAt   time.Time           `json:"created_at"`
	UpdatedAt   time.Time           `json:"updated_at"`
}

// SignOff is the typed sign-off names.
type SignOff struct {
	TenantName *string `json:"tenant_name"`
	AgentName  *string `json:"agent_name"`
}

// InspectionCan is what the caller may do with an inspection.
type InspectionCan struct {
	Edit           bool `json:"edit"`
	EditChecklist  bool `json:"edit_checklist"`
	RecordFindings bool `json:"record_findings"`
	AddPhoto       bool `json:"add_photo"`
	Start          bool `json:"start"`
	Complete       bool `json:"complete"`
	Cancel         bool `json:"cancel"`
}

// InspectionView is one inspection as the managers see it.
type InspectionView struct {
	Summary
	Tenancy      *tenancies.TenancyBrief `json:"tenancy"`
	Areas        []AreaView              `json:"areas"`
	SummaryText  string                  `json:"summary"`
	SignOff      SignOff                 `json:"sign_off"`
	CancelReason *string                 `json:"cancel_reason"`
	Baseline     *BaselineRef            `json:"baseline"`
	CreatedBy    tenancies.PersonRef     `json:"created_by"`
	CompletedBy  *tenancies.PersonRef    `json:"completed_by"`
	CancelledBy  *tenancies.PersonRef    `json:"cancelled_by"`
	ReportURL    string                  `json:"report_url"`
	Can          InspectionCan           `json:"can"`
}

// Page is GET .../inspections.
type Page struct {
	Items        []Summary      `json:"items"`
	Total        int            `json:"total"`
	Page         int            `json:"page"`
	PageSize     int            `json:"page_size"`
	StatusCounts map[string]int `json:"status_counts"`
}

// PhotoRules are the upload rules.
type PhotoRules struct {
	MaxPhotosPerItem int      `json:"max_photos_per_item"`
	MaxPhotos        int      `json:"max_photos"`
	MaxBytes         int      `json:"max_bytes"`
	Types            []string `json:"types"`
}

// Limits are the checklist limits.
type Limits struct {
	MaxAreas        int `json:"max_areas"`
	MaxItemsPerArea int `json:"max_items_per_area"`
	MaxItems        int `json:"max_items"`
	MaxLabel        int `json:"max_label"`
	MaxNote         int `json:"max_note"`
	MaxSummary      int `json:"max_summary"`
	MaxSignOff      int `json:"max_sign_off"`
}

// Permissions is the caller's access.
type Permissions struct {
	Write bool `json:"write"`
}

// Options is GET .../inspections/options.
type Options struct {
	Types           []CodeLabel         `json:"types"`
	Statuses        []CodeLabel         `json:"statuses"`
	FindingStatuses []CodeLabel         `json:"finding_statuses"`
	Templates       []Template          `json:"templates"`
	Inspectors      []Person            `json:"inspectors"`
	Transitions     map[string][]string `json:"transitions"`
	PhotoRules      PhotoRules          `json:"photo_rules"`
	Limits          Limits              `json:"limits"`
	Permissions     Permissions         `json:"permissions"`
}

// The tenant's view.

// NameOnly is a name.
type NameOnly struct {
	Name string `json:"name"`
}

// LabelOnly is a label.
type LabelOnly struct {
	Label string `json:"label"`
}

// MyPhoto is a photo as the tenant sees it.
type MyPhoto struct {
	ID          uuid.UUID `json:"id"`
	URL         string    `json:"url"`
	ContentType string    `json:"content_type"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
}

// MyItem is an item as the tenant sees it.
type MyItem struct {
	ID          uuid.UUID     `json:"id"`
	Label       string        `json:"label"`
	Status      *string       `json:"status"`
	StatusLabel *string       `json:"status_label"`
	Note        string        `json:"note"`
	Photos      []MyPhoto     `json:"photos"`
	Baseline    *BaselineView `json:"baseline"`
}

// MyArea is an area as the tenant sees it.
type MyArea struct {
	ID    uuid.UUID `json:"id"`
	Label string    `json:"label"`
	Items []MyItem  `json:"items"`
}

// MyBaselineRef is the move-in a move-out is compared with, for the tenant.
type MyBaselineRef struct {
	Reference   string    `json:"reference"`
	CompletedAt time.Time `json:"completed_at"`
}

// MyInspection is an inspection as the tenant sees it.
type MyInspection struct {
	ID           uuid.UUID      `json:"id"`
	Reference    string         `json:"reference"`
	Type         string         `json:"type"`
	TypeLabel    string         `json:"type_label"`
	Status       string         `json:"status"`
	StatusLabel  string         `json:"status_label"`
	Property     NameOnly       `json:"property"`
	Unit         *LabelOnly     `json:"unit"`
	ScheduledFor time.Time      `json:"scheduled_for"`
	TimeZone     string         `json:"time_zone"`
	CompletedAt  *time.Time     `json:"completed_at"`
	Inspector    NameOnly       `json:"inspector"`
	Template     LabelOnly      `json:"template"`
	Progress     Progress       `json:"progress"`
	Areas        []MyArea       `json:"areas"`
	Summary      string         `json:"summary"`
	SignOff      SignOff        `json:"sign_off"`
	Baseline     *MyBaselineRef `json:"baseline"`
	ReportURL    string         `json:"report_url"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
