package listings

import (
	"encoding/json"
	"reflect"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/packages"
)

// What the management endpoints return. Codes are returned as codes: the
// editor's drop-downs come from GET /v1/reference, like the search filters.

// PriceView is the price of the editor; amount is null until one is entered.
type PriceView struct {
	Amount   *string `json:"amount"`
	Currency *string `json:"currency"`
}

// ContentView is the editable content in the shape a PATCH sends back.
type ContentView struct {
	Title        string    `json:"title"`
	Summary      string    `json:"summary"`
	Description  string    `json:"description"`
	Deal         *string   `json:"deal"`
	Category     *string   `json:"category"`
	PropertyType *string   `json:"property_type"`
	Price        PriceView `json:"price"`
	PricePeriod  *string   `json:"price_period"`
	Country      *string   `json:"country"`
	City         *string   `json:"city"`
	Suburb       *string   `json:"suburb"`
	CityName     *string   `json:"city_name"`
	RegionName   *string   `json:"region_name"`
	Bedrooms     *int      `json:"bedrooms"`
	Bathrooms    *int      `json:"bathrooms"`
	FloorArea    *int      `json:"floor_area"`
	LandArea     *int      `json:"land_area"`
	Features     []string  `json:"features"`
	MaxGuests    *int      `json:"max_guests"`
	MinNights    *int      `json:"min_nights"`
	// YouTubeURL is the canonical watch link of the video tour
	// (https://www.youtube.com/watch?v=ID, with &t=Ns when it starts later),
	// never the link as typed; null without a video. YouTubeVideoID and
	// YouTubeStartSeconds are what is stored.
	YouTubeURL          *string `json:"youtube_url"`
	YouTubeVideoID      *string `json:"youtube_video_id"`
	YouTubeStartSeconds *int    `json:"youtube_start_seconds"`
	// Latitude and Longitude are the EXACT coordinates (the lister's own
	// view; the public sees rounded ones for APPROXIMATE), null for none.
	// LocationPrecision is EXACT or APPROXIMATE, never null.
	Latitude          *float64 `json:"latitude"`
	Longitude         *float64 `json:"longitude"`
	LocationPrecision string   `json:"location_precision"`
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func contentView(c Content) ContentView {
	features := append([]string{}, c.Features...)
	var video *string
	var start *int
	if c.YouTubeVideoID != "" {
		link := youTubeEditURL(c.YouTubeVideoID, c.YouTubeStartSeconds)
		video, start = &link, copyInt(c.YouTubeStartSeconds)
	}
	return ContentView{
		YouTubeURL: video, YouTubeVideoID: opt(c.YouTubeVideoID), YouTubeStartSeconds: start,
		Latitude: copyFloat(c.Latitude), Longitude: copyFloat(c.Longitude), LocationPrecision: NormalPrecision(c.LocationPrecision),
		Title: c.Title, Summary: c.Summary, Description: c.Description, Deal: opt(c.Deal), Category: opt(c.Category),
		PropertyType: opt(c.PropertyType), Price: PriceView{Amount: opt(c.PriceAmount), Currency: opt(c.Currency)},
		PricePeriod: opt(c.PricePeriod), Country: opt(c.MarketCode), City: opt(c.City), Suburb: opt(c.Suburb),
		CityName: opt(c.CityName), RegionName: opt(c.RegionName), Bedrooms: c.Bedrooms, Bathrooms: c.Bathrooms,
		FloorArea: c.FloorArea, LandArea: c.LandArea, Features: features, MaxGuests: c.MaxGuests, MinNights: c.MinNights,
	}
}

// Photo statuses.
const (
	PhotoApproved      = "APPROVED"
	PhotoPendingReview = "PENDING_REVIEW"
)

// ManagedPhotoVariant is one stored size of a photo in the editor.
type ManagedPhotoVariant struct {
	Size   string `json:"size"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	URL    string `json:"url"`
}

// ManagedPhoto is a photo as its managers and reviewers see it.
type ManagedPhoto struct {
	ID       uuid.UUID `json:"id"`
	Position int       `json:"position"`
	Alt      string    `json:"alt"`
	Width    int       `json:"width"`
	Height   int       `json:"height"`
	// Status is APPROVED (public with the listing) or PENDING_REVIEW (shown
	// only to the listing's managers and reviewers until an approval).
	Status string `json:"status"`
	// URL is the largest size: an API path for an upload (served to the
	// public only when the listing and the photo are public, otherwise only
	// to a signed-in manager or reviewer), a web path for a bundled sample.
	URL       string                `json:"url"`
	Variants  []ManagedPhotoVariant `json:"variants"`
	CreatedAt time.Time             `json:"created_at"`
}

func managedPhotos(photos []StoredPhoto, alts map[string]string) []ManagedPhoto {
	out := []ManagedPhoto{}
	for i, p := range sortedPhotos(photos) {
		alt := p.Alt
		if a, ok := alts[p.ID.String()]; ok {
			alt = a
		}
		mp := ManagedPhoto{ID: p.ID, Position: i, Alt: alt, Width: p.Width, Height: p.Height, Status: PhotoPendingReview,
			URL: p.URL, Variants: []ManagedPhotoVariant{}, CreatedAt: p.CreatedAt.UTC()}
		if p.ApprovedAt != nil {
			mp.Status = PhotoApproved
		}
		if p.Uploaded() {
			mp.URL = PhotoPath(p.ID, PhotoSizeLarge, p.Checksum)
			for _, size := range PhotoSizes() {
				if v, ok := p.Variant(size); ok {
					mp.Variants = append(mp.Variants, ManagedPhotoVariant{Size: size, Width: v.Width, Height: v.Height, URL: PhotoPath(p.ID, size, p.Checksum)})
				}
			}
		}
		out = append(out, mp)
	}
	return out
}

// ReviewState is the last review of the listing itself.
type ReviewState struct {
	SubmittedAt *time.Time `json:"submitted_at"`
	ReviewedAt  *time.Time `json:"reviewed_at"`
	// Reason is the reviewer's reason of a request for changes or a
	// rejection; null otherwise.
	Reason *string `json:"reason"`
}

// PendingChanges is the revision of a published listing.
type PendingChanges struct {
	Status      string     `json:"status"`
	StatusLabel string     `json:"status_label"`
	Reason      *string    `json:"reason"`
	SubmittedAt *time.Time `json:"submitted_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// NewPhotos counts the photos that wait for the approval.
	NewPhotos int `json:"new_photos"`
}

// OwnerView says who the listing belongs to.
type OwnerView struct {
	// Type is "organisation" or "direct_owner".
	Type           string     `json:"type"`
	OrganisationID *uuid.UUID `json:"organisation_id"`
	AgentUserID    *uuid.UUID `json:"agent_user_id"`
}

// TimelineView is one step of the listing's history.
type TimelineView struct {
	Action     string    `json:"action"`
	FromStatus *string   `json:"from_status"`
	ToStatus   *string   `json:"to_status"`
	ReviewKind *string   `json:"review_kind"`
	Reason     *string   `json:"reason"`
	Actor      string    `json:"actor"`
	At         time.Time `json:"at"`
	// ActorUserID is shown to reviewers only.
	ActorUserID *uuid.UUID `json:"actor_user_id,omitempty"`
}

func timelineViews(entries []TimelineEntry, withActors bool) []TimelineView {
	out := []TimelineView{}
	for _, e := range entries {
		v := TimelineView{Action: e.Action, FromStatus: opt(e.FromStatus), ToStatus: opt(e.ToStatus), ReviewKind: opt(e.ReviewKind),
			Reason: opt(e.Reason), Actor: e.ActorKind, At: e.CreatedAt.UTC()}
		if withActors {
			v.ActorUserID = e.ActorUserID
		}
		out = append(out, v)
	}
	return out
}

// Owner actions a client may offer (ManagedView.Actions).
const (
	ActEdit    = "edit"
	ActSubmit  = "submit"
	ActArchive = "archive"
	ActRelist  = "relist"
	ActDelete  = "delete"
)

// ManagedView is GET /v1/me/listings/{id}: the full editable listing.
type ManagedView struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Reference string    `json:"reference"`
	// Status is the lifecycle status; a PUBLISHED listing past its
	// publication period is EXPIRED.
	Status      string `json:"status"`
	StatusLabel string `json:"status_label"`
	// Public is true while the public sees the listing.
	Public         bool            `json:"public"`
	Review         ReviewState     `json:"review"`
	PendingChanges *PendingChanges `json:"pending_changes"`
	// Content is the working copy: the pending changes of a published
	// listing when it has some, else the listing itself.
	Content      ContentView    `json:"content"`
	Photos       []ManagedPhoto `json:"photos"`
	PhotoLimit   int            `json:"photo_limit"`
	Completeness Completeness   `json:"completeness"`
	Owner        OwnerView      `json:"owner"`
	Advertiser   Advertiser     `json:"advertiser"`
	// CanEdit is false for a member who may see the listing but not change
	// it (an agent and another agent's listing). Actions are what the caller
	// may do now.
	CanEdit     bool           `json:"can_edit"`
	Actions     []string       `json:"actions"`
	PublishedAt *time.Time     `json:"published_at"`
	ExpiresAt   *time.Time     `json:"expires_at"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	Timeline    []TimelineView `json:"timeline"`
	// PackageUsage is the listing package and active listings of the
	// listing's account in its country (null without a country, API 0.37.0);
	// QuotaWarning is set when that account is at its limit and the listing
	// is not live.
	PackageUsage *packages.MarketUsage `json:"package_usage"`
	QuotaWarning *string               `json:"quota_warning"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// FieldChange is one field a pending edit changes: its live value and the
// proposed one, in the shape of ContentView.
type FieldChange struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// diff compares two contents field by field.
func diff(before, after Content) []FieldChange {
	out := []FieldChange{}
	a, b := contentMap(contentView(before)), contentMap(contentView(after))
	for _, field := range []string{"title", "summary", "description", "deal", "category", "property_type", "price", "price_period",
		"country", "city", "suburb", "city_name", "region_name", "bedrooms", "bathrooms", "floor_area", "land_area",
		"features", "max_guests", "min_nights", "youtube_url", "latitude", "longitude", "location_precision"} {
		if !reflect.DeepEqual(a[field], b[field]) {
			out = append(out, FieldChange{Field: field, Before: a[field], After: b[field]})
		}
	}
	return out
}

func contentMap(v ContentView) map[string]any {
	raw, _ := json.Marshal(v)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}

// PhotoChanges sums up what a pending edit does to the photos.
type PhotoChanges struct {
	// Added are the photos that wait for the approval.
	Added int `json:"added"`
	// AltChanged are the public photos whose description is to change.
	AltChanged int `json:"alt_changed"`
}

// AdminView is GET /v1/admin/listings/{id}: what a reviewer decides on.
type AdminView struct {
	ManagedView
	// ReviewKind is NEW (the listing is not public yet) or EDIT (changes to a
	// published listing); null when nothing waits for review.
	ReviewKind *string `json:"review_kind"`
	// Live is the public content while an edit waits (EDIT only).
	Live *ContentView `json:"live"`
	// Changes are the fields the edit changes (EDIT only; empty otherwise).
	Changes      []FieldChange `json:"changes"`
	PhotoChanges PhotoChanges  `json:"photo_changes"`
	// Decisions are what the reviewer may do now: approve, request_changes, reject.
	Decisions []string `json:"decisions"`
}

// QueueItem is one row of the review queue.
type QueueItem struct {
	ID             uuid.UUID  `json:"id"`
	Reference      string     `json:"reference"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	RevisionStatus *string    `json:"revision_status"`
	ReviewKind     *string    `json:"review_kind"`
	Country        Country    `json:"country"`
	SubmittedAt    *time.Time `json:"submitted_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	Advertiser     struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Verified bool   `json:"verified"`
	} `json:"advertiser"`
	PhotoCount        int `json:"photo_count"`
	PendingPhotoCount int `json:"pending_photo_count"`
}

// QueuePage is one page of the review queue.
type QueuePage struct {
	Items      []QueueItem `json:"items"`
	Page       int         `json:"page"`
	PageSize   int         `json:"page_size"`
	Total      int         `json:"total"`
	TotalPages int         `json:"total_pages"`
}
