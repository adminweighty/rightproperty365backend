package listings

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Listing management: advertisers create and edit their own listings, with
// photos, and platform reviewers approve them before they are public.
//
// The lifecycle (listings.status):
//
//	DRAFT ──submit──> PENDING_REVIEW ──approve──> PUBLISHED ──(publication period over)──> EXPIRED (at query time)
//	                       │  └──request changes──> CHANGES_REQUESTED ──edit, submit──> PENDING_REVIEW
//	                       └──reject──> REJECTED (final: the listing can only be deleted)
//	DRAFT | PENDING_REVIEW | CHANGES_REQUESTED | PUBLISHED | EXPIRED | PAUSED ──archive──> ARCHIVED
//	ARCHIVED | EXPIRED ──relist──> PENDING_REVIEW (was published before) or DRAFT (never was)
//
// A PUBLISHED listing is edited through a REVISION: the changes (and new
// photos) are kept beside the live listing, which stays public with its
// reviewed content until a reviewer approves them (see Revision).

// Statuses added by listing management (StatusPublished is in model.go).
const (
	StatusDraft            = "DRAFT"
	StatusPendingReview    = "PENDING_REVIEW"
	StatusChangesRequested = "CHANGES_REQUESTED"
	StatusRejected         = "REJECTED"
	StatusArchived         = "ARCHIVED"
	StatusPaused           = "PAUSED"
)

// Statuses returns every listing status, in lifecycle order.
func Statuses() []string {
	return []string{StatusDraft, StatusPendingReview, StatusChangesRequested, StatusRejected, StatusPublished,
		StatusExpired, StatusArchived, StatusPaused, "SOLD", "LET", "CLOSED"}
}

// StatusLabel is how a status is shown ("Pending review").
func StatusLabel(status string) string {
	if status == "" {
		return ""
	}
	words := strings.ToLower(strings.ReplaceAll(status, "_", " "))
	return strings.ToUpper(words[:1]) + words[1:]
}

// Statuses of a revision (the pending changes of a published listing).
const (
	RevisionEditing          = "EDITING"
	RevisionPendingReview    = StatusPendingReview
	RevisionChangesRequested = StatusChangesRequested
	RevisionRejected         = StatusRejected
)

// What a reviewer is asked to look at.
const (
	// ReviewKindNew is a listing that is not public: its first publication,
	// or a relist.
	ReviewKindNew = "NEW"
	// ReviewKindEdit is a change to a published listing.
	ReviewKindEdit = "EDIT"
)

// PublicationPeriod is how long an approval publishes a listing for (FR-006).
const PublicationPeriod = 90 * 24 * time.Hour

// MaxReasonLength bounds a reviewer's reason.
const MaxReasonLength = 1000

// Content is everything an advertiser edits: the listing's text, its codes,
// its place and its numbers. It is the body of a revision and the working
// copy of the editor. Codes are reference-data codes; "" is "not set".
type Content struct {
	Title        string   `json:"title"`
	Summary      string   `json:"summary"`
	Description  string   `json:"description"`
	Deal         string   `json:"deal"`
	Category     string   `json:"category"`
	PropertyType string   `json:"property_type"`
	PriceAmount  string   `json:"price_amount"`
	Currency     string   `json:"currency"`
	PricePeriod  string   `json:"price_period"`
	MarketCode   string   `json:"country"`
	City         string   `json:"city"`
	Suburb       string   `json:"suburb"`
	CityName     string   `json:"city_name"`
	RegionName   string   `json:"region_name"`
	Bedrooms     *int     `json:"bedrooms"`
	Bathrooms    *int     `json:"bathrooms"`
	FloorArea    *int     `json:"floor_area"`
	LandArea     *int     `json:"land_area"`
	Features     []string `json:"features"`
	MaxGuests    *int     `json:"max_guests"`
	MinNights    *int     `json:"min_nights"`
}

// ContentOf returns the editable content of a record.
func ContentOf(r Record) Content {
	return Content{
		Title: r.Title, Summary: r.Summary, Description: r.Description, Deal: r.Deal, Category: r.Category, PropertyType: r.PropertyType,
		PriceAmount: r.Price.Amount, Currency: r.Price.Currency, PricePeriod: r.PricePeriod, MarketCode: r.MarketCode,
		City: r.City, Suburb: r.Suburb, CityName: r.CityName, RegionName: r.RegionName,
		Bedrooms: copyInt(r.Bedrooms), Bathrooms: copyInt(r.Bathrooms), FloorArea: copyInt(r.FloorArea), LandArea: copyInt(r.LandArea),
		Features: append([]string{}, r.Features...), MaxGuests: copyInt(r.MaxGuests), MinNights: copyInt(r.MinNights),
	}
}

// ApplyTo writes the content onto a record.
func (c Content) ApplyTo(r *Record) {
	r.Title, r.Summary, r.Description, r.Deal, r.Category, r.PropertyType = c.Title, c.Summary, c.Description, c.Deal, c.Category, c.PropertyType
	r.Price = Money{Amount: c.PriceAmount, Currency: c.Currency}
	r.PricePeriod, r.MarketCode, r.City, r.Suburb, r.CityName, r.RegionName = c.PricePeriod, c.MarketCode, c.City, c.Suburb, c.CityName, c.RegionName
	r.Bedrooms, r.Bathrooms, r.FloorArea, r.LandArea = copyInt(c.Bedrooms), copyInt(c.Bathrooms), copyInt(c.FloorArea), copyInt(c.LandArea)
	r.Features, r.MaxGuests, r.MinNights = append([]string{}, c.Features...), copyInt(c.MaxGuests), copyInt(c.MinNights)
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Revision is the pending change of a PUBLISHED listing. While it exists the
// public sees the listing as it was last approved; the advertiser edits the
// revision. Approving it writes Content and PhotoAlts onto the listing and
// makes its new photos public; rejecting it drops Content and the new photos
// and keeps the reason to show.
type Revision struct {
	Status string `json:"status"`
	// Content is the whole edited content (nil after a rejection).
	Content *Content `json:"content,omitempty"`
	// PhotoAlts are new alternative texts of photos that are already public.
	PhotoAlts   map[string]string `json:"photo_alts,omitempty"`
	Reason      string            `json:"reason,omitempty"`
	SubmittedAt *time.Time        `json:"submitted_at,omitempty"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// StoredVariant is one stored size of an uploaded photo. Key is an object key
// of the private storage and never leaves the API.
type StoredVariant struct {
	Size   string `json:"size"`
	Key    string `json:"key"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int64  `json:"bytes"`
}

// StoredPhoto is a photo of a listing as stored: a bundled sample image (URL
// set) or an upload (Variants set).
type StoredPhoto struct {
	ID       uuid.UUID
	Position int
	// URL is the site-relative path of a bundled sample image; "" for an upload.
	URL      string
	Alt      string
	Width    int
	Height   int
	Checksum string
	Variants []StoredVariant
	// ApprovedAt is when a reviewer approved the photo (with its listing);
	// nil while it waits. Only approved photos are public.
	ApprovedAt *time.Time
	UploadedBy *uuid.UUID
	CreatedAt  time.Time
}

// Uploaded reports whether the photo was uploaded (not a bundled sample).
func (p StoredPhoto) Uploaded() bool { return p.URL == "" }

// Variant returns the stored size, or false.
func (p StoredPhoto) Variant(size string) (StoredVariant, bool) {
	for _, v := range p.Variants {
		if v.Size == size {
			return v, true
		}
	}
	return StoredVariant{}, false
}

// Keys returns the distinct object keys of the photo.
func (p StoredPhoto) Keys() []string {
	var out []string
	for _, v := range p.Variants {
		if v.Key != "" && !slices.Contains(out, v.Key) {
			out = append(out, v.Key)
		}
	}
	return out
}

// PhotoPath is the API route of one stored size of a photo, with the photo's
// version (the first characters of its checksum) so that it can be cached for
// a year. Relative to the API origin, like a logo URL.
func PhotoPath(id uuid.UUID, size, checksum string) string {
	return "/v1/listing-photos/" + id.String() + "/" + size + "?v=" + LogoVersion(checksum)
}

// publicPhoto is the public form of an approved photo.
func publicPhoto(p StoredPhoto) Photo {
	if !p.Uploaded() {
		return Photo{URL: p.URL, Alt: p.Alt, Width: p.Width, Height: p.Height, Variants: []PhotoVariant{}}
	}
	out := Photo{Alt: p.Alt, Width: p.Width, Height: p.Height, Variants: []PhotoVariant{}}
	if large, ok := p.Variant(PhotoSizeLarge); ok {
		out.URL, out.Width, out.Height = PhotoPath(p.ID, PhotoSizeLarge, p.Checksum), large.Width, large.Height
	}
	seen := map[int]bool{}
	for _, size := range PhotoSizes() {
		if v, ok := p.Variant(size); ok && !seen[v.Width] {
			seen[v.Width] = true
			out.Variants = append(out.Variants, PhotoVariant{Width: v.Width, Height: v.Height, URL: PhotoPath(p.ID, size, p.Checksum)})
		}
	}
	return out
}

// PublicPhotos returns the public photos among stored ones: the approved
// ones, in order.
func PublicPhotos(photos []StoredPhoto) []Photo {
	out := []Photo{}
	for _, p := range sortedPhotos(photos) {
		if p.ApprovedAt != nil {
			out = append(out, publicPhoto(p))
		}
	}
	return out
}

func sortedPhotos(photos []StoredPhoto) []StoredPhoto {
	out := append([]StoredPhoto(nil), photos...)
	slices.SortStableFunc(out, func(a, b StoredPhoto) int {
		if a.Position != b.Position {
			return a.Position - b.Position
		}
		return a.CreatedAt.Compare(b.CreatedAt)
	})
	return out
}

// ManagedListing is a listing with everything its management needs.
type ManagedListing struct {
	Record
	CreatedAt   time.Time
	CreatedBy   *uuid.UUID
	SubmittedAt *time.Time
	SubmittedBy *uuid.UUID
	// ReviewReason is the reviewer's reason of the last request for changes
	// or rejection of the listing itself ("" when there is none).
	ReviewReason string
	Revision     *Revision
	// AllPhotos are every photo, approved or not, in order.
	AllPhotos []StoredPhoto
}

// EffectiveStatus is the status as the advertiser sees it: a PUBLISHED
// listing whose publication period is over is EXPIRED.
func (m ManagedListing) EffectiveStatus(now time.Time) string {
	if m.Status == StatusPublished && !m.ExpiresAt.After(now) {
		return StatusExpired
	}
	return m.Status
}

// WorkingContent is what the editor shows: the revision's content for a
// published listing that has one, else the listing's own.
func (m ManagedListing) WorkingContent() Content {
	if m.Revision != nil && m.Revision.Content != nil {
		c := *m.Revision.Content
		c.Features = append([]string{}, c.Features...)
		return c
	}
	return ContentOf(m.Record)
}

// ReviewKind says what a review of the listing is about, "" when it waits
// for none.
func (m ManagedListing) ReviewKind() string {
	switch {
	case m.Status == StatusPendingReview:
		return ReviewKindNew
	case m.Revision != nil && m.Revision.Status == RevisionPendingReview:
		return ReviewKindEdit
	}
	return ""
}

// Actors of a timeline entry.
const (
	ActorAdvertiser = "ADVERTISER"
	ActorReviewer   = "REVIEWER"
)

// Timeline actions.
const (
	ActionCreated          = "CREATED"
	ActionSubmitted        = "SUBMITTED"
	ActionApproved         = "APPROVED"
	ActionChangesRequested = "CHANGES_REQUESTED"
	ActionRejected         = "REJECTED"
	ActionArchived         = "ARCHIVED"
	ActionRelisted         = "RELISTED"
)

// TimelineEntry is one recorded step of a listing's lifecycle.
type TimelineEntry struct {
	ID          uuid.UUID
	ListingID   uuid.UUID
	ActorUserID *uuid.UUID
	ActorKind   string
	Action      string
	FromStatus  string
	ToStatus    string
	// ReviewKind is NEW or EDIT for the submissions and decisions.
	ReviewKind string
	Reason     string
	CreatedAt  time.Time
}

// Outbox events of the review, for the advertiser's emails. Their payload
// is ReviewEventPayload.
const (
	EventListingApproved         = "LISTING_APPROVED"
	EventListingChangesRequested = "LISTING_CHANGES_REQUESTED"
	EventListingRejected         = "LISTING_REJECTED"
)

// ReviewEventTypes lists the review events.
func ReviewEventTypes() []string {
	return []string{EventListingApproved, EventListingChangesRequested, EventListingRejected}
}

// ReviewEventPayload is the payload of a review event. It names the listing
// and carries the reviewer's reason (moderation text, not personal data).
type ReviewEventPayload struct {
	ListingID  uuid.UUID `json:"listing_id"`
	ReviewKind string    `json:"review_kind"`
	Reason     string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ChangeEvent is an outbox event written with a change.
type ChangeEvent struct {
	Type    string
	Payload json.RawMessage
}

// Change is what is recorded with a write: at most one timeline entry and
// the outbox events, in the same transaction.
type Change struct {
	Entry  *TimelineEntry
	Events []ChangeEvent
	// NewCycle records a new publication cycle (an approval that publishes
	// the listing): its published_at and expires_at are the listing's.
	NewCycle bool
}

// indexEvent is an event that tells the search index about a listing.
func indexEvent(eventType string, id uuid.UUID) ChangeEvent {
	raw, _ := json.Marshal(map[string]string{"listing_id": id.String()})
	return ChangeEvent{Type: eventType, Payload: raw}
}

// ReviewFilter selects the review queue.
type ReviewFilter struct {
	// Status matches the listing's status OR its revision's (PENDING_REVIEW
	// by default, so edits of published listings are in the same queue).
	Status string
	// Markets limits the queue to those countries; nil is every country.
	Markets []string
	// Kind is NEW, EDIT or "" for both (only meaningful for PENDING_REVIEW).
	Kind string
	// Country narrows to one country.
	Country string
	// Q matches the title or the reference.
	Q        string
	Page     int
	PageSize int
}

// Errors of listing management.
var (
	// ErrConflict: the listing changed since it was read.
	ErrConflict = errors.New("the listing was changed by someone else")
	// ErrDuplicate: the slug or reference is taken.
	ErrDuplicate = errors.New("slug or reference already taken")
	// ErrPhotoLimit: the listing has MaxPhotosPerListing photos.
	ErrPhotoLimit = errors.New("the listing has the maximum number of photos")
	// ErrPhotoNotFound: no such photo on the listing.
	ErrPhotoNotFound = errors.New("photo not found")
)

// ManageStore is the storage of listing management. The memory and Postgres
// adapters implement it beside Store.
type ManageStore interface {
	// CreateListing stores a new listing (photos, if any, included).
	CreateListing(ctx context.Context, m ManagedListing, ch Change) error
	// Managed returns one listing with everything its management needs, or
	// ErrNotFound.
	Managed(ctx context.Context, id uuid.UUID) (ManagedListing, error)
	// SaveListing writes the listing's columns, features, review fields and
	// revision, and the alt text, position and approval of its photos. A
	// photo that is not in m.AllPhotos any more is deleted and returned, so
	// its stored files can be removed. expect is the updated_at the change
	// was based on: ErrConflict when the listing changed since.
	SaveListing(ctx context.Context, m ManagedListing, expect time.Time, ch Change) ([]StoredPhoto, error)
	// DeleteListing removes a listing and its photos and returns the photos.
	DeleteListing(ctx context.Context, id uuid.UUID, expect time.Time) ([]StoredPhoto, error)
	// AddPhoto adds a photo at the end, unless the listing already has limit
	// photos (ErrPhotoLimit, decided atomically), and sets the listing's
	// updated_at to now. expect as for SaveListing.
	AddPhoto(ctx context.Context, listingID uuid.UUID, p StoredPhoto, limit int, expect, now time.Time, ch Change) error
	// PhotoByID returns a photo and its listing's ID, or ErrPhotoNotFound.
	PhotoByID(ctx context.Context, photoID uuid.UUID) (uuid.UUID, StoredPhoto, error)
	// ReviewQueue returns one page of the review queue, oldest submission first.
	ReviewQueue(ctx context.Context, f ReviewFilter) ([]ManagedListing, int, error)
	// Timeline returns a listing's timeline, oldest first.
	Timeline(ctx context.Context, listingID uuid.UUID) ([]TimelineEntry, error)
}
