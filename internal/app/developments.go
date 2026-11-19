package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/developments"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// developerProfiles reads the developers' directory profiles for the
// development showcase (API 0.44.0).
type developerProfiles struct{ store directory.Store }

// DeveloperOf implements developments.Profiles.
func (d developerProfiles) DeveloperOf(ctx context.Context, organisationID uuid.UUID) (developments.Profile, error) {
	records, err := d.store.Managed(ctx, []uuid.UUID{organisationID}, uuid.Nil)
	if err != nil {
		return developments.Profile{}, err
	}
	for _, r := range records {
		if r.Kind == developments.KindDevelopers && r.OrganisationID == organisationID {
			return developments.Profile{ID: r.ID, Kind: r.Kind, Slug: r.Slug, Name: r.Name, MarketCode: r.MarketCode}, nil
		}
	}
	return developments.Profile{}, developments.ErrNoProfile
}

// PublicRef implements developments.Profiles: only a public profile
// (ACTIVE and verified) has a card, and the card carries no contact detail.
func (d developerProfiles) PublicRef(ctx context.Context, profileID uuid.UUID) (*listings.ProfileRef, error) {
	r, err := d.store.ByID(ctx, profileID)
	if errors.Is(err, directory.ErrProfileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ref := directory.RefOf(r)
	if !ref.Verified {
		return nil, nil
	}
	return &ref, nil
}

// DevelopmentsDeps is what the showcase is built from.
type DevelopmentsDeps struct {
	Store     developments.Store
	Files     storage.Storage
	Listings  developments.ListingSource
	Directory directory.Store
	Labels    developments.Labels
	Places    listings.Places
	Inbox     developments.Inbox
	// Audit and Enquiries are the memory stores the memory store reports to
	// (nil with Postgres, which does it in SQL).
	Audit     adminaudit.Log
	Enquiries businessenquiries.Store
	Logger    *slog.Logger
	Now       func() time.Time
}

// DevelopmentsService builds the showcase. A nil Store is an empty memory
// store. A memory store learns the profile visibility rule, the
// administrators' audit log and the enquiries to unlink.
func DevelopmentsService(d DevelopmentsDeps) *developments.Service {
	store := d.Store
	if store == nil {
		store = developments.NewMemoryStore()
	}
	profiles := developerProfiles{store: d.Directory}
	if mem, ok := store.(*developments.MemoryStore); ok {
		mem.SetVisibility(func(ctx context.Context, dev developments.Development) bool {
			ref, err := profiles.PublicRef(ctx, dev.ProfileID)
			return err == nil && ref != nil
		})
		if log, ok := d.Audit.(*adminaudit.MemoryLog); ok {
			mem.SetAuditLog(log)
		}
		if enq, ok := d.Enquiries.(*businessenquiries.MemoryStore); ok {
			mem.SetEnquiries(enq)
		}
	}
	return developments.NewService(developments.Deps{Store: store, Files: d.Files, Listings: d.Listings, Profiles: profiles,
		Labels: d.Labels, Places: d.Places, Inbox: d.Inbox, Logger: d.Logger, Now: d.Now})
}

// developmentProjects adapts the showcase to the directory's profile page
// and sitemap.
func developmentProjects(svc *developments.Service) (directory.ProjectSource, directory.DevelopmentEntries) {
	projects := func(ctx context.Context, profileID uuid.UUID) (any, error) {
		cards, err := svc.ProfileProjects(ctx, profileID)
		if err != nil {
			return nil, err
		}
		return cards, nil
	}
	entries := func(ctx context.Context) ([]directory.DevelopmentEntry, error) {
		list, err := svc.SitemapEntries(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]directory.DevelopmentEntry, 0, len(list))
		for _, e := range list {
			out = append(out, directory.DevelopmentEntry{Market: e.Market, Slug: e.Slug, UpdatedAt: e.UpdatedAt})
		}
		return out, nil
	}
	return projects, entries
}
