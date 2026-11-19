package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/modules/tenancies"
)

// ReviewsDeps is what the reviews service is built from.
type ReviewsDeps struct {
	Store       reviews.Store
	Tenancies   *tenancies.Service
	Directory   directory.Store
	Maintenance maintenance.Store
	Enquiries   businessenquiries.Store
	Logger      *slog.Logger
	Now         func() time.Time
}

// ReviewsService builds the reviews of service providers (API 0.36.4): the
// books of the tenancies service, public profiles of the directory, and the
// maintenance requests and business enquiries reviews rest on.
func ReviewsService(d ReviewsDeps) *reviews.Service {
	return reviews.NewService(reviews.Deps{Store: d.Store, Tenancies: d.Tenancies, Profiles: reviewProfiles{d.Directory},
		Bases: reviewBases{maintenance: d.Maintenance, enquiries: d.Enquiries}, Logger: d.Logger, Now: d.Now})
}

// DirectoryRatings is the directory's rating source: the published reviews.
func DirectoryRatings(store reviews.Store) directory.RatingSource {
	return func(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]directory.Rating, error) {
		got, err := store.Ratings(ctx, ids)
		if err != nil {
			return nil, err
		}
		out := make(map[uuid.UUID]directory.Rating, len(got))
		for id, r := range got {
			out[id] = directory.Rating{Average: reviews.Average(r), Count: r.Count}
		}
		return out, nil
	}
}

// reviewProfiles reads public profiles from the directory.
type reviewProfiles struct{ store directory.Store }

func profileOf(r directory.Record) reviews.Profile {
	return reviews.Profile{ID: r.ID, Kind: r.Kind, Slug: r.Slug, Name: r.Name, OrganisationID: r.OrganisationID}
}

func (p reviewProfiles) ByID(ctx context.Context, id uuid.UUID) (reviews.Profile, error) {
	r, err := p.store.ByID(ctx, id)
	if errors.Is(err, directory.ErrProfileNotFound) {
		return reviews.Profile{}, reviews.ErrProfileNotFound
	}
	if err != nil {
		return reviews.Profile{}, err
	}
	return profileOf(r), nil
}

func (p reviewProfiles) Public(ctx context.Context, kind, slug string) (reviews.Profile, error) {
	r, err := p.store.Get(ctx, kind, slug)
	if errors.Is(err, directory.ErrProfileNotFound) {
		return reviews.Profile{}, reviews.ErrProfileNotFound
	}
	if err != nil {
		return reviews.Profile{}, err
	}
	return profileOf(r), nil
}

// reviewBases reads maintenance requests and enquiries through their stores.
type reviewBases struct {
	maintenance maintenance.Store
	enquiries   businessenquiries.Store
}

func jobOf(r maintenance.Request) reviews.MaintenanceJob {
	job := reviews.MaintenanceJob{ID: r.ID, Reference: r.Reference, Title: r.Title, Book: r.Book, Status: r.Status}
	if r.Provider != nil && r.Provider.ProfileID != nil {
		id := *r.Provider.ProfileID
		job.ProviderProfileID = &id
	}
	switch {
	case r.ResolvedAt != nil:
		job.CompletedAt = r.ResolvedAt
	case r.Status == maintenance.StatusClosed:
		job.CompletedAt = r.ClosedAt
	}
	return job
}

func (b reviewBases) MaintenanceJob(ctx context.Context, id uuid.UUID) (reviews.MaintenanceJob, error) {
	r, err := b.maintenance.Request(ctx, id)
	if errors.Is(err, maintenance.ErrNotFound) {
		return reviews.MaintenanceJob{}, reviews.ErrNotFound
	}
	if err != nil {
		return reviews.MaintenanceJob{}, err
	}
	return jobOf(r), nil
}

func (b reviewBases) MaintenanceJobs(ctx context.Context, book tenancies.Book, profileID uuid.UUID) ([]reviews.MaintenanceJob, error) {
	list, _, _, err := b.maintenance.Requests(ctx, book, maintenance.Filter{})
	if err != nil {
		return nil, err
	}
	var out []reviews.MaintenanceJob
	for _, r := range list {
		if r.Provider != nil && r.Provider.ProfileID != nil && *r.Provider.ProfileID == profileID && r.Status != maintenance.StatusCancelled {
			out = append(out, jobOf(r))
		}
	}
	return out, nil
}

func (b reviewBases) Enquiry(ctx context.Context, id uuid.UUID) (businessenquiries.CustomerEnquiry, error) {
	return b.enquiries.Enquiry(ctx, id)
}

func (b reviewBases) Enquiries(ctx context.Context, userID, profileID uuid.UUID) ([]businessenquiries.CustomerEnquiry, error) {
	return b.enquiries.CustomerEnquiries(ctx, userID, profileID)
}
