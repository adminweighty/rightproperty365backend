// Package developments is the development and investment showcase (API
// 0.44.0, BR-019, FR-048): new-development projects of developers, with
// their phases, unit types, photos, a YouTube tour and the developer's own
// listings shown as available units.
//
// A project belongs to a developer's directory profile and to that
// profile's organisation, whose OWNER, MANAGER and AGENT edit it (STAFF
// read it). It is reviewed like a listing (DRAFT -> PENDING_REVIEW ->
// PUBLISHED, or REJECTED) and only a PUBLISHED project of a public profile
// is public.
//
// There is no contact detail anywhere in this package: no phone, email or
// street address is stored, accepted or answered, and free text with an
// email address or a phone number is refused. A customer reaches the
// developer only through a business enquiry that quotes the project
// (businessenquiries.DevelopmentResolver).
//
// The investment highlights are the developer's own statements and are
// always answered as such ("stated by the developer", with a disclaimer),
// never as advice.
package developments

import (
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
)

// Construction statuses of a project and of a phase.
const (
	StatusPlanned           = "PLANNED"
	StatusUnderConstruction = "UNDER_CONSTRUCTION"
	StatusSelling           = "SELLING"
	StatusCompleted         = "COMPLETED"
	StatusSoldOut           = "SOLD_OUT"
)

// Statuses returns the construction statuses in their order.
func Statuses() []string {
	return []string{StatusPlanned, StatusUnderConstruction, StatusSelling, StatusCompleted, StatusSoldOut}
}

var statusLabels = map[string]string{
	StatusPlanned: "Planned", StatusUnderConstruction: "Under construction", StatusSelling: "Selling",
	StatusCompleted: "Completed", StatusSoldOut: "Sold out",
}

// StatusLabel is how a construction status is shown.
func StatusLabel(status string) string { return statusLabels[status] }

// Moderation statuses (the review).
const (
	ModerationDraft     = "DRAFT"
	ModerationPending   = "PENDING_REVIEW"
	ModerationPublished = "PUBLISHED"
	ModerationRejected  = "REJECTED"
)

// ModerationStatuses returns the moderation statuses.
func ModerationStatuses() []string {
	return []string{ModerationDraft, ModerationPending, ModerationPublished, ModerationRejected}
}

// KindDevelopers is the directory kind a project's profile must have.
const KindDevelopers = "developers"

// StatedByDeveloper and HighlightsDisclaimer frame the investment
// highlights in every public answer.
const (
	StatedByDeveloper    = "DEVELOPER"
	HighlightsDisclaimer = "Stated by the developer. These figures have not been verified by RightProperty365 and are not financial advice."
)

// Limits.
const (
	MaxNameLength         = 120
	MaxSummaryLength      = 200
	MaxDescriptionLength  = 5000
	MaxCityNameLength     = 80
	MaxPhaseNameLength    = 80
	MaxUnitTypeNameLength = 80
	MaxHighlights         = 6
	MaxHighlightLabel     = 60
	MaxHighlightValue     = 120
	MaxAmenities          = 30
	MaxPhases             = 10
	MaxUnitTypesPerPhase  = 20
	MaxSlugLength         = 80
	MaxRoomCount          = 50
	MaxSizeM2             = 100000
	MaxReasonLength       = 1000
	MaxPhotos             = listings.MaxPhotosPerListing
	MaxEnquiriesShown     = 100
)

// PhotoKeyPrefix is the object-key prefix of the stored photo files.
const PhotoKeyPrefix = "developmentphotos"

// PhotoPath is the API route of one stored size of a project photo, with
// its version (relative to the API origin, like a listing photo).
func PhotoPath(id uuid.UUID, size, checksum string) string {
	return "/v1/development-photos/" + id.String() + "/" + size + "?v=" + listings.LogoVersion(checksum)
}

// Highlight is one investment statement of the developer ("Expected rental
// yield": "8-10% a year").
type Highlight struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// UnitType is a kind of unit of a phase. Prices are decimal strings;
// PriceOnRequest means no price is shown (and none is stored).
type UnitType struct {
	ID             uuid.UUID
	PhaseID        uuid.UUID
	Name           string
	Bedrooms       *int
	Bathrooms      *int
	SizeMin        *int
	SizeMax        *int
	PriceOnRequest bool
	PriceFrom      string
	PriceTo        string
	Currency       string
	UnitsTotal     *int
	UnitsAvailable *int
	Position       int
	CreatedAt      time.Time
}

// Phase is a stage of a project.
type Phase struct {
	ID         uuid.UUID
	Name       string
	Status     string
	Completion string
	Position   int
	CreatedAt  time.Time
	UnitTypes  []UnitType
}

// Link is a listing of the developer shown as an available unit of the
// project, optionally of one of its unit types.
type Link struct {
	ListingID  uuid.UUID
	UnitTypeID *uuid.UUID
	CreatedAt  time.Time
}

// Development is a project with everything it holds.
type Development struct {
	ID             uuid.UUID
	OrganisationID uuid.UUID
	ProfileID      uuid.UUID
	MarketCode     string
	Slug           string
	Name           string
	Summary        string
	Description    string
	// Status is the construction status; Moderation the review.
	Status     string
	Moderation string
	// Completion is "2027-Q3", "2027-06-30" or "".
	Completion string
	// CityCode / SuburbCode are reference codes; CityName is free text for
	// a country without reference cities (never with CityCode).
	CityCode          string
	SuburbCode        string
	CityName          string
	Latitude          *float64
	Longitude         *float64
	LocationPrecision string
	Amenities         []string
	Highlights        []Highlight
	YouTubeVideoID    string
	YouTubeStartSec   *int
	ReviewReason      string
	SubmittedAt       *time.Time
	SubmittedBy       *uuid.UUID
	ReviewedAt        *time.Time
	ReviewedBy        *uuid.UUID
	PublishedAt       *time.Time
	CreatedBy         *uuid.UUID
	CreatedAt         time.Time
	UpdatedAt         time.Time

	Phases []Phase
	Photos []listings.StoredPhoto
	Links  []Link
}

// Locked reports whether the project's own fields and photos are locked:
// it waits for review or is published (withdraw it to change them).
func (d Development) Locked() bool {
	return d.Moderation == ModerationPending || d.Moderation == ModerationPublished
}

// UnitType returns the unit type with that ID and its phase's index, or false.
func (d *Development) UnitType(id uuid.UUID) (*UnitType, int, bool) {
	for i := range d.Phases {
		for j := range d.Phases[i].UnitTypes {
			if d.Phases[i].UnitTypes[j].ID == id {
				return &d.Phases[i].UnitTypes[j], i, true
			}
		}
	}
	return nil, -1, false
}

// PhaseIndex returns the index of a phase, or -1.
func (d Development) PhaseIndex(id uuid.UUID) int {
	return slices.IndexFunc(d.Phases, func(p Phase) bool { return p.ID == id })
}

// UnitTypeIDs returns the IDs of every unit type.
func (d Development) UnitTypeIDs() []uuid.UUID {
	var out []uuid.UUID
	for _, p := range d.Phases {
		for _, u := range p.UnitTypes {
			out = append(out, u.ID)
		}
	}
	return out
}

// Clone returns a deep copy.
func (d Development) Clone() Development {
	c := d
	c.Latitude, c.Longitude, c.YouTubeStartSec = copyFloat(d.Latitude), copyFloat(d.Longitude), copyInt(d.YouTubeStartSec)
	c.SubmittedAt, c.ReviewedAt, c.PublishedAt = copyTime(d.SubmittedAt), copyTime(d.ReviewedAt), copyTime(d.PublishedAt)
	c.SubmittedBy, c.ReviewedBy, c.CreatedBy = copyID(d.SubmittedBy), copyID(d.ReviewedBy), copyID(d.CreatedBy)
	c.Amenities = append([]string{}, d.Amenities...)
	c.Highlights = append([]Highlight{}, d.Highlights...)
	c.Phases = make([]Phase, len(d.Phases))
	for i, p := range d.Phases {
		c.Phases[i] = p
		c.Phases[i].UnitTypes = make([]UnitType, len(p.UnitTypes))
		for j, u := range p.UnitTypes {
			u.Bedrooms, u.Bathrooms, u.SizeMin, u.SizeMax = copyInt(u.Bedrooms), copyInt(u.Bathrooms), copyInt(u.SizeMin), copyInt(u.SizeMax)
			u.UnitsTotal, u.UnitsAvailable = copyInt(u.UnitsTotal), copyInt(u.UnitsAvailable)
			c.Phases[i].UnitTypes[j] = u
		}
	}
	c.Photos = make([]listings.StoredPhoto, len(d.Photos))
	for i, ph := range d.Photos {
		ph.Variants = append([]listings.StoredVariant{}, ph.Variants...)
		ph.ApprovedAt, ph.UploadedBy = copyTime(ph.ApprovedAt), copyID(ph.UploadedBy)
		c.Photos[i] = ph
	}
	c.Links = make([]Link, len(d.Links))
	for i, l := range d.Links {
		l.UnitTypeID = copyID(l.UnitTypeID)
		c.Links[i] = l
	}
	return c
}

func copyInt(p *int) *int {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyFloat(p *float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyTime(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

func copyID(p *uuid.UUID) *uuid.UUID {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// Errors of the use cases, mapped to HTTP by the handler.
var (
	// ErrNotFound: no such project, or one the caller may not see (the same answer).
	ErrNotFound = errors.New("development not found")
	// ErrForbidden: the caller may see the project but may not do this.
	ErrForbidden = errors.New("not allowed")
	// ErrSlugTaken: another project of the market has the slug.
	ErrSlugTaken = errors.New("slug already taken in this market")
	// ErrConflict: the project changed since it was read.
	ErrConflict = errors.New("the development was changed by someone else")
	// ErrLocked: the project's own fields and photos cannot change while it
	// waits for review or is published.
	ErrLocked = errors.New("the development is locked")
	// ErrNoProfile: the organisation has no developer profile.
	ErrNoProfile = errors.New("the organisation has no developer profile")
	// ErrPhaseNotFound / ErrUnitTypeNotFound: no such phase or unit type in the project.
	ErrPhaseNotFound    = errors.New("phase not found")
	ErrUnitTypeNotFound = errors.New("unit type not found")
	// ErrListingNotFound: no such listing of the organisation.
	ErrListingNotFound = errors.New("listing not found")
	// ErrListingLinked: the listing is already in another project.
	ErrListingLinked = errors.New("the listing is already in another development")
	// ErrSelfReview: a reviewer may not decide a project of their own organisation.
	ErrSelfReview = errors.New("a reviewer may not review their own development")
)

// StateError is an action the project's moderation status does not allow (409).
type StateError struct{ Message string }

func (e *StateError) Error() string { return e.Message }

// ValidationError carries per-field messages (400).
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// IncompleteError is a submission that lacks what a review needs (422).
type IncompleteError struct{ Fields map[string]string }

func (e *IncompleteError) Error() string { return "development incomplete" }
