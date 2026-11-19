package maintenance

import (
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
)

// The JSON of the contract 0.36 "Response shapes (c)".

// CodeLabel is a code and its wording.
type CodeLabel struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// ActorView is who raised or did something.
type ActorView struct {
	Kind   string     `json:"kind"`
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
}

// ProviderView is a directory profile.
type ProviderView struct {
	ID   *uuid.UUID `json:"id"`
	Kind string     `json:"kind"`
	Slug string     `json:"slug"`
	Name string     `json:"name"`
}

// EnquiryRef is the business enquiry sent to a provider.
type EnquiryRef struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
}

// AssignmentView is the current assignment.
type AssignmentView struct {
	Provider   ProviderView        `json:"provider"`
	Enquiry    *EnquiryRef         `json:"enquiry"`
	AssignedAt time.Time           `json:"assigned_at"`
	AssignedBy tenancies.PersonRef `json:"assigned_by"`
}

// PhotoCan is what the caller may do with a photo.
type PhotoCan struct {
	Delete bool `json:"delete"`
}

// PhotoView is a photo as the managers see it.
type PhotoView struct {
	ID          uuid.UUID `json:"id"`
	URL         string    `json:"url"`
	ContentType string    `json:"content_type"`
	Width       int       `json:"width"`
	Height      int       `json:"height"`
	Size        int64     `json:"size"`
	UploadedBy  ActorView `json:"uploaded_by"`
	UploadedAt  time.Time `json:"uploaded_at"`
	Can         PhotoCan  `json:"can"`
}

// CostView is a cost.
type CostView struct {
	ID                uuid.UUID           `json:"id"`
	Amount            string              `json:"amount"`
	Currency          string              `json:"currency"`
	MinorUnit         int                 `json:"minor_unit"`
	Description       string              `json:"description"`
	Method            string              `json:"method"`
	MethodLabel       string              `json:"method_label"`
	IncurredOn        string              `json:"incurred_on"`
	LedgerEntryID     uuid.UUID           `json:"ledger_entry_id"`
	LedgerStatus      string              `json:"ledger_status"`
	LedgerStatusLabel string              `json:"ledger_status_label"`
	Reversed          bool                `json:"reversed"`
	RecordedBy        tenancies.PersonRef `json:"recorded_by"`
	RecordedAt        time.Time           `json:"recorded_at"`
}

// CostTotal is the sum of a request's live costs in one currency.
type CostTotal struct {
	Currency  string `json:"currency"`
	MinorUnit int    `json:"minor_unit"`
	Amount    string `json:"amount"`
	Count     int    `json:"count"`
}

// TimelineItem is one timeline item as the managers see it.
type TimelineItem struct {
	ID         uuid.UUID      `json:"id"`
	Type       string         `json:"type"`
	At         time.Time      `json:"at"`
	Actor      ActorView      `json:"actor"`
	FromStatus *string        `json:"from_status"`
	ToStatus   *string        `json:"to_status"`
	Text       *string        `json:"text"`
	Internal   bool           `json:"internal"`
	Details    map[string]any `json:"details"`
}

// SummaryProperty is the property of a summary.
type SummaryProperty struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Archived bool      `json:"archived"`
}

// UnitRef is the unit of a request.
type UnitRef struct {
	ID       uuid.UUID `json:"id"`
	Label    string    `json:"label"`
	Archived bool      `json:"archived"`
}

// Summary is a request in a list.
type Summary struct {
	ID              uuid.UUID     `json:"id"`
	Reference       string        `json:"reference"`
	Status          string        `json:"status"`
	StatusLabel     string        `json:"status_label"`
	Category        CodeLabel     `json:"category"`
	Priority        CodeLabel     `json:"priority"`
	Title           string        `json:"title"`
	Property        any           `json:"property"`
	Unit            *UnitRef      `json:"unit"`
	TenancyID       *uuid.UUID    `json:"tenancy_id"`
	RaisedBy        ActorView     `json:"raised_by"`
	Provider        *ProviderView `json:"provider"`
	PhotoCount      int           `json:"photo_count"`
	CommentCount    int           `json:"comment_count"`
	CostTotals      []CostTotal   `json:"cost_totals"`
	CreatedAt       time.Time     `json:"created_at"`
	UpdatedAt       time.Time     `json:"updated_at"`
	StatusChangedAt time.Time     `json:"status_changed_at"`
}

// RequestProperty is the property of a full request.
type RequestProperty struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Address    string    `json:"address"`
	MarketCode string    `json:"market_code"`
	Archived   bool      `json:"archived"`
}

// RequestCan is what the caller may do with a request.
type RequestCan struct {
	Edit        bool     `json:"edit"`
	Transitions []string `json:"transitions"`
	Assign      bool     `json:"assign"`
	AddCost     bool     `json:"add_cost"`
	Comment     bool     `json:"comment"`
	AddPhoto    bool     `json:"add_photo"`
}

// RequestView is one request as the managers see it.
type RequestView struct {
	Summary
	Description string                  `json:"description"`
	Tenancy     *tenancies.TenancyBrief `json:"tenancy"`
	Assignment  *AssignmentView         `json:"assignment"`
	Photos      []PhotoView             `json:"photos"`
	Costs       []CostView              `json:"costs"`
	Timeline    []TimelineItem          `json:"timeline"`
	ResolvedAt  *time.Time              `json:"resolved_at"`
	ClosedAt    *time.Time              `json:"closed_at"`
	Can         RequestCan              `json:"can"`
}

// Page is GET .../maintenance.
type Page struct {
	Items        []Summary      `json:"items"`
	Total        int            `json:"total"`
	Page         int            `json:"page"`
	PageSize     int            `json:"page_size"`
	StatusCounts map[string]int `json:"status_counts"`
}

// CurrencyOption is a currency a cost may use.
type CurrencyOption struct {
	Code      string `json:"code"`
	MinorUnit int    `json:"minor_unit"`
}

// PhotoRules are the upload rules.
type PhotoRules struct {
	MaxPhotos int      `json:"max_photos"`
	MaxBytes  int      `json:"max_bytes"`
	Types     []string `json:"types"`
}

// Permissions is the caller's access.
type Permissions struct {
	Write bool `json:"write"`
}

// Options is GET .../maintenance/options.
type Options struct {
	Categories  []CodeLabel         `json:"categories"`
	Priorities  []CodeLabel         `json:"priorities"`
	Statuses    []CodeLabel         `json:"statuses"`
	Transitions map[string][]string `json:"transitions"`
	Currencies  []CurrencyOption    `json:"currencies"`
	Methods     []CodeLabel         `json:"methods"`
	PhotoRules  PhotoRules          `json:"photo_rules"`
	Permissions Permissions         `json:"permissions"`
}

// MyOptions is GET /v1/me/tenancies/{id}/maintenance/options.
type MyOptions struct {
	Categories []CodeLabel `json:"categories"`
	Priorities []CodeLabel `json:"priorities"`
	PhotoRules PhotoRules  `json:"photo_rules"`
}

// ProviderOption is one provider of GET .../maintenance/providers.
type ProviderOption struct {
	ID       uuid.UUID  `json:"id"`
	Kind     string     `json:"kind"`
	Slug     string     `json:"slug"`
	Name     string     `json:"name"`
	Trade    *CodeLabel `json:"trade"`
	City     *string    `json:"city"`
	Verified bool       `json:"verified"`
}

// AssignResult is POST .../assign.
type AssignResult struct {
	Request RequestView     `json:"request"`
	Enquiry AssignedEnquiry `json:"enquiry"`
}

// AssignedEnquiry is the enquiry an assignment sent.
type AssignedEnquiry struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Replayed  bool      `json:"replayed"`
}

// CostResult is POST .../costs.
type CostResult struct {
	Cost    CostView    `json:"cost"`
	Request RequestView `json:"request"`
}

// The tenant's view.

// MyRaisedBy is who raised a request, as the tenant sees it.
type MyRaisedBy struct {
	Kind string `json:"kind"`
	You  bool   `json:"you"`
}

// MyActor is who did something, as the tenant sees it.
type MyActor struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	You  bool   `json:"you"`
}

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
	UploadedAt  time.Time `json:"uploaded_at"`
}

// MyTimelineItem is a timeline item as the tenant sees it.
type MyTimelineItem struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	At         time.Time `json:"at"`
	Actor      MyActor   `json:"actor"`
	FromStatus *string   `json:"from_status"`
	ToStatus   *string   `json:"to_status"`
	Text       *string   `json:"text"`
}

// MyCan is what the tenant may do.
type MyCan struct {
	Comment  bool `json:"comment"`
	AddPhoto bool `json:"add_photo"`
}

// MyRequest is a request as the tenant sees it.
type MyRequest struct {
	ID              uuid.UUID        `json:"id"`
	Reference       string           `json:"reference"`
	Status          string           `json:"status"`
	StatusLabel     string           `json:"status_label"`
	Category        CodeLabel        `json:"category"`
	Priority        CodeLabel        `json:"priority"`
	Title           string           `json:"title"`
	Description     string           `json:"description"`
	Property        NameOnly         `json:"property"`
	Unit            *LabelOnly       `json:"unit"`
	RaisedBy        MyRaisedBy       `json:"raised_by"`
	Provider        *NameOnly        `json:"provider"`
	Photos          []MyPhoto        `json:"photos"`
	Timeline        []MyTimelineItem `json:"timeline"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	StatusChangedAt time.Time        `json:"status_changed_at"`
	Can             MyCan            `json:"can"`
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
