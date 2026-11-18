package app

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/platform/storage"
)

// ApplicationsService builds the questionnaires and applications service
// (the API, the worker and the in-memory development process share it).
func ApplicationsService(store applications.Store, files storage.Storage, listingsSvc *listings.Service, listingStore listings.Store, identityStore identity.Store,
	logger *slog.Logger, now func() time.Time) *applications.Service {
	d := applications.Deps{Store: store, Files: files, Listings: applicationListings{svc: listingsSvc, store: listingStore}, Logger: logger, Now: now}
	if identityStore != nil {
		d.People = applications.People(personOf(identityStore))
	}
	return applications.NewService(d)
}

// applicationListings reads listings for the applications module.
type applicationListings struct {
	svc   *listings.Service
	store listings.Store
}

// Listings implements applications.Listings.
func (l applicationListings) Listings(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]applications.ListingInfo, error) {
	out := map[uuid.UUID]applications.ListingInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	states, err := l.svc.CardStates(ctx, ids)
	if err != nil {
		return nil, err
	}
	records, err := l.store.ByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		st, ok := states[r.ID]
		if !ok {
			continue
		}
		out[r.ID] = applications.ListingInfo{ID: r.ID, Slug: r.Slug, Reference: r.Reference, Title: r.Title, Category: r.Category,
			MarketCode: r.MarketCode, OrganisationID: r.OrganisationID, AgentUserID: r.AgentUserID, SellerUserID: r.SellerUserID,
			Public: st.Public, Card: saved.SnapshotOf(st.Listing)}
	}
	return out, nil
}

// BySlug implements applications.Listings.
func (l applicationListings) BySlug(ctx context.Context, slug string) (applications.ListingInfo, error) {
	r, err := l.svc.PublicRecord(ctx, slug)
	if errors.Is(err, listings.ErrNotFound) || errors.Is(err, listings.ErrUnavailable) {
		return applications.ListingInfo{}, applications.ErrListingNotFound
	}
	if err != nil {
		return applications.ListingInfo{}, err
	}
	got, err := l.Listings(ctx, []uuid.UUID{r.ID})
	if err != nil {
		return applications.ListingInfo{}, err
	}
	info, ok := got[r.ID]
	if !ok {
		return applications.ListingInfo{}, applications.ErrListingNotFound
	}
	return info, nil
}

// AgentListingIDs implements applications.Listings.
func (l applicationListings) AgentListingIDs(ctx context.Context, organisationID, agentUserID uuid.UUID) ([]uuid.UUID, error) {
	records, err := l.svc.Owned(ctx, []uuid.UUID{organisationID}, nil)
	if err != nil {
		return nil, err
	}
	out := []uuid.UUID{}
	for _, r := range records {
		if r.AgentUserID != nil && *r.AgentUserID == agentUserID && r.OrganisationID != nil && *r.OrganisationID == organisationID {
			out = append(out, r.ID)
		}
	}
	return out, nil
}

// applicationIntake gives the listings the `applications` summary.
type applicationIntake struct{ svc *applications.Service }

// ApplicationSummaries implements listings.Intake.
func (a applicationIntake) ApplicationSummaries(ctx context.Context, records []listings.Record) (map[uuid.UUID]listings.ApplicationsSummary, error) {
	ids := make([]uuid.UUID, 0, len(records))
	short := map[uuid.UUID]bool{}
	for _, r := range records {
		ids = append(ids, r.ID)
		short[r.ID] = r.Category == listings.CategoryShortStay
	}
	got, err := a.svc.Summaries(ctx, ids, short)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]listings.ApplicationsSummary, len(got))
	for id, s := range got {
		out[id] = listings.ApplicationsSummary{Open: s.Open, ClosesAt: s.ClosesAt, FormName: s.FormName}
	}
	return out, nil
}

// RunApplicationJobs runs the application retention job in this process
// (the worker's job; the in-memory development API runs it itself).
func RunApplicationJobs(ctx context.Context, svc *applications.Service) { svc.RunJobs(ctx) }

// ApplicationNotices loads the data behind an application email (the
// notification handlers' application source).
func ApplicationNotices(store applications.Store, listingStore listings.Store, ref reference.Store, identityStore identity.Store,
	logger *slog.Logger) func(ctx context.Context, id uuid.UUID) (applications.Notice, error) {
	svc := ApplicationsService(store, nil, listings.NewService(listingStore, nil, reference.NewService(ref, nil)), listingStore, identityStore, logger, nil)
	return svc.Notice
}

// MemoryListingTitles names listings for the in-memory application search (?q=).
func MemoryListingTitles(store listings.Store) func(ids []uuid.UUID) map[uuid.UUID]string {
	return func(ids []uuid.UUID) map[uuid.UUID]string {
		out := map[uuid.UUID]string{}
		records, err := store.ByIDs(context.Background(), ids)
		if err != nil {
			return out
		}
		for _, r := range records {
			out[r.ID] = r.Title
		}
		return out
	}
}
