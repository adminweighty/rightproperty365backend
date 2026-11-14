package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/storage"
)

// vocabulary adapts the reference data to the small ports the other modules
// validate against. Every call reads the store, so a row deactivated by an
// administrator stops being accepted immediately; nothing is cached here.
type vocabulary struct{ ref *reference.Service }

// RequestTypes implements propertyrequests.Vocabulary.
func (v vocabulary) RequestTypes(ctx context.Context) ([]string, error) {
	return v.ref.ActiveCodes(ctx, reference.SetRequestTypes)
}

// ContactMethods implements propertyrequests.Vocabulary and businessenquiries.Vocabulary.
func (v vocabulary) ContactMethods(ctx context.Context) ([]string, error) {
	return v.ref.ActiveCodes(ctx, reference.SetContactMethods)
}

// DirectoryKinds implements businessenquiries.Vocabulary.
func (v vocabulary) DirectoryKinds(ctx context.Context) ([]string, error) {
	return v.ref.ActiveCodes(ctx, reference.SetDirectoryKinds)
}

// SelectableRoles implements identity.RoleCatalogue. Platform roles are
// refused by identity itself before this list is consulted.
func (v vocabulary) SelectableRoles(ctx context.Context) ([]identity.SelectableRole, error) {
	items, err := v.ref.Items(ctx, reference.SetAccountRoles)
	if err != nil {
		return nil, err
	}
	out := make([]identity.SelectableRole, 0, len(items))
	for _, it := range items {
		// A role whose verification flag is missing is treated as needing verification.
		out = append(out, identity.SelectableRole{Code: it.Code, RequiresVerification: it.RequiresVerification == nil || *it.RequiresVerification})
	}
	return out, nil
}

// DirectoryKindList implements directory.Reference: the active directory
// kinds in display order.
func (v vocabulary) DirectoryKindList(ctx context.Context) ([]directory.Kind, error) {
	items, err := v.ref.Items(ctx, reference.SetDirectoryKinds)
	if err != nil {
		return nil, err
	}
	out := make([]directory.Kind, 0, len(items))
	for _, it := range items {
		k := directory.Kind{Code: it.Code, Label: it.Label, SingularLabel: it.Label}
		if it.SingularLabel != nil {
			k.SingularLabel = *it.SingularLabel
		}
		if it.Description != nil {
			k.Description = *it.Description
		}
		out = append(out, k)
	}
	return out, nil
}

// Labels implements directory.Reference.
func (v vocabulary) Labels(ctx context.Context, set string) (map[string]string, error) {
	return v.ref.Labels(ctx, set)
}

// listingNames adapts the listings service to propertyrequests.ListingNames.
type listingNames struct{ svc *listings.Service }

// ListingNames implements propertyrequests.ListingNames.
func (l listingNames) ListingNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]propertyrequests.InboxListing, error) {
	summaries, err := l.svc.Summaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]propertyrequests.InboxListing, len(summaries))
	for id, s := range summaries {
		out[id] = propertyrequests.InboxListing{Slug: s.Slug, Title: s.Title, Reference: s.Reference}
	}
	return out, nil
}

// onboardingVocabulary adapts the reference data to onboarding.Vocabulary.
type onboardingVocabulary struct{ ref *reference.Service }

// RoleLabels implements onboarding.Vocabulary.
func (v onboardingVocabulary) RoleLabels(ctx context.Context) (map[string]string, error) {
	return v.ref.Labels(ctx, reference.SetAccountRoles)
}

// CityMarkets implements onboarding.Vocabulary.
func (v onboardingVocabulary) CityMarkets(ctx context.Context) (map[string]string, error) {
	items, err := v.ref.Items(ctx, reference.SetCities)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(items))
	for _, it := range items {
		out[it.Code] = ""
		if it.MarketCode != nil {
			out[it.Code] = *it.MarketCode
		}
	}
	return out, nil
}

// TradeCodes implements onboarding.Vocabulary.
func (v onboardingVocabulary) TradeCodes(ctx context.Context) ([]string, error) {
	return v.ref.ActiveCodes(ctx, reference.SetServiceTrades)
}

// DocumentTypes implements onboarding.Vocabulary.
func (v onboardingVocabulary) DocumentTypes(ctx context.Context) ([]onboarding.DocumentType, error) {
	items, err := v.ref.Items(ctx, reference.SetOnboardingDocuments)
	if err != nil {
		return nil, err
	}
	out := make([]onboarding.DocumentType, 0, len(items))
	for _, it := range items {
		t := onboarding.DocumentType{Code: it.Code, Label: it.Label}
		if it.Description != nil {
			t.Description = *it.Description
		}
		out = append(out, t)
	}
	return out, nil
}

// agencyDirectory adapts the public directory to onboarding.Agencies: only a
// published, verified agency profile can be joined.
type agencyDirectory struct {
	directory directory.Store
	ref       *reference.Service
	markets   markets.Store
}

// VerifiedAgency implements onboarding.Agencies.
func (a agencyDirectory) VerifiedAgency(ctx context.Context, slug string) (*onboarding.Agency, error) {
	// The directory store only returns published AND verified profiles.
	r, err := a.directory.Get(ctx, directory.KindAgencies, slug)
	if errors.Is(err, directory.ErrProfileNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &onboarding.Agency{Slug: r.Slug, Name: r.Name, OrganisationID: r.OrganisationID, CityCode: r.CityCode}
	// The agency's market is the market of its city; an agency whose city
	// applies to every market falls back to the first market.
	cities, err := onboardingVocabulary{ref: a.ref}.CityMarkets(ctx)
	if err != nil {
		return nil, err
	}
	out.MarketCode = cities[r.CityCode]
	if out.MarketCode == "" {
		all, err := a.markets.List(ctx)
		if err != nil {
			return nil, err
		}
		if len(all) > 0 {
			out.MarketCode = all[0].Code
		}
	}
	return out, nil
}

// OnboardingNotifications returns the function the notification handlers use
// to load the data behind an onboarding event: the applicant, the role's
// label, the decision's reason and the profile an approval created. It reads
// no document (files may be nil).
func OnboardingNotifications(store onboarding.Store, files storage.Storage, ref reference.Store, logger *slog.Logger) func(ctx context.Context, id uuid.UUID) (onboarding.NotificationInfo, error) {
	svc := onboarding.NewService(store, files, onboardingVocabulary{ref: reference.NewService(ref, nil)}, nil, logger)
	return svc.Notification
}
