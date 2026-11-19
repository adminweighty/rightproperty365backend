package developments

import (
	"context"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// PublicFilter selects public projects.
type PublicFilter struct {
	Market string
	City   string
	Status string
	// Q matches the name, summary, description and free-text city; QPlaces
	// are the city and suburb codes whose label matches it (the service
	// resolves them from the reference data).
	Q       string
	QPlaces []string
	Limit   int
	Offset  int
}

// QueueFilter selects the review queue.
type QueueFilter struct {
	// Moderation is the moderation status (PENDING_REVIEW by default).
	Moderation string
	// Markets limits the queue to those countries; nil is every country.
	Markets []string
	// Market narrows to one country.
	Market string
	Limit  int
	Offset int
}

// Entry is one public project of the sitemap.
type Entry struct {
	Market    string
	Slug      string
	UpdatedAt time.Time
}

// Store holds the projects. A project is read and written whole: its
// phases, unit types, photos and listing links with it.
//
// "Public" below means: moderation PUBLISHED, its developer profile ACTIVE
// and verified, and its organisation ACTIVE.
type Store interface {
	// Create stores a new project: ErrSlugTaken when the market has the slug,
	// ErrListingLinked when a link names a listing of another project.
	Create(ctx context.Context, d Development) error
	// Get returns a project whatever its status, or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Development, error)
	// Save writes a project changed from the version whose updated_at was
	// expect (ErrConflict when it changed since), with its children. Child
	// rows that are not in d any more are deleted; the photos deleted are
	// returned so that their files can be removed. audit, when set, is
	// written in the same transaction (a review decision).
	Save(ctx context.Context, d Development, expect time.Time, audit *adminaudit.Event) ([]listings.StoredPhoto, error)
	// Delete removes a project (expect as for Save) and returns its photos.
	Delete(ctx context.Context, id uuid.UUID, expect time.Time) ([]listings.StoredPhoto, error)
	// ByOrganisation returns an organisation's projects, newest change first.
	ByOrganisation(ctx context.Context, organisationID uuid.UUID) ([]Development, error)
	// Public returns one page of public projects, most recently published
	// first, and the number of matches.
	Public(ctx context.Context, f PublicFilter) ([]Development, int, error)
	// PublicBySlug returns a public project, or ErrNotFound.
	PublicBySlug(ctx context.Context, market, slug string) (Development, error)
	// PublicByProfile returns the public projects of a profile, most
	// recently published first.
	PublicByProfile(ctx context.Context, profileID uuid.UUID) ([]Development, error)
	// PublicEntries returns every public project's address (sitemap).
	PublicEntries(ctx context.Context) ([]Entry, error)
	// Queue returns one page of the review queue, oldest submission first.
	Queue(ctx context.Context, f QueueFilter) ([]Development, int, error)
	// PhotoByID returns a photo and its project's ID, or listings.ErrPhotoNotFound.
	PhotoByID(ctx context.Context, photoID uuid.UUID) (uuid.UUID, listings.StoredPhoto, error)
}
