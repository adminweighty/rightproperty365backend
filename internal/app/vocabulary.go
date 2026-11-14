package app

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/auth"
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

// places adapts the reference data and the markets to listings.Places: the
// hierarchy country (market) > region > city > suburb. Every call reads the
// stores, so an administrator's change applies at once.
type places struct {
	ref     *reference.Service
	markets markets.Store
}

// Geography implements listings.Places.
func (p places) Geography(ctx context.Context) (listings.Geography, error) {
	g := listings.Geography{
		Countries: map[string]listings.CountryInfo{}, RegionCountry: map[string]string{},
		CityRegion: map[string]string{}, CityCountry: map[string]string{}, SuburbCity: map[string]string{},
	}
	all, err := p.markets.List(ctx)
	if err != nil {
		return g, err
	}
	for _, m := range all {
		g.Countries[m.Code] = listings.CountryInfo{Name: m.Name, Currency: m.CurrencyCode}
	}
	for set, fill := range map[string]func(reference.Item){
		reference.SetRegions: func(it reference.Item) {
			if it.MarketCode != nil {
				g.RegionCountry[it.Code] = *it.MarketCode
			}
		},
		reference.SetCities: func(it reference.Item) {
			if it.ParentCode != nil {
				g.CityRegion[it.Code] = *it.ParentCode
			}
			if it.MarketCode != nil {
				g.CityCountry[it.Code] = *it.MarketCode
			}
		},
		reference.SetSuburbs: func(it reference.Item) {
			if it.ParentCode != nil {
				g.SuburbCity[it.Code] = *it.ParentCode
			}
		},
	} {
		items, err := p.ref.Items(ctx, set)
		if err != nil {
			return g, err
		}
		for _, it := range items {
			fill(it)
		}
	}
	return g, nil
}

// Places returns the location hierarchy read from the given stores, for the
// binaries that build search documents (the worker and cmd/reindex).
func Places(ref reference.Store, marketStore markets.Store) listings.Places {
	return places{ref: reference.NewService(ref, nil), markets: marketStore}
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

// listingCards adapts the listing store to appointments.Catalogue. It reads a
// listing whatever its status: an appointment keeps the name of its listing.
type listingCards struct{ store listings.Store }

// ListingCard implements appointments.Catalogue.
func (l listingCards) ListingCard(ctx context.Context, id uuid.UUID) (appointments.ListingCard, bool, error) {
	records, err := l.store.ByIDs(ctx, []uuid.UUID{id})
	if err != nil || len(records) == 0 {
		return appointments.ListingCard{}, false, err
	}
	r := records[0]
	card := appointments.ListingCard{Slug: r.Slug, Title: r.Title, Reference: r.Reference}
	// The person the customer meets: the listing's agent, else its agency.
	// A private seller has no public profile.
	if ref := r.Advertiser.Agent; ref != nil {
		card.WithKind, card.WithSlug, card.WithName = ref.Kind, ref.Slug, ref.Name
	} else if ref := r.Advertiser.Agency; ref != nil {
		card.WithKind, card.WithSlug, card.WithName = ref.Kind, ref.Slug, ref.Name
	}
	return card, true, nil
}

// marketZones adapts the markets to appointments.Zones.
type marketZones struct{ markets markets.Store }

// MarketTimezone implements appointments.Zones.
func (z marketZones) MarketTimezone(ctx context.Context, marketID uuid.UUID) (string, error) {
	all, err := z.markets.List(ctx)
	if err != nil {
		return "", err
	}
	for _, m := range all {
		if m.ID == marketID {
			return m.Timezone, nil
		}
	}
	return "", nil
}

// people adapts the identity store to appointments.People.
type people struct{ identity identity.Store }

// Member implements appointments.People.
func (p people) Member(ctx context.Context, organisationID, userID uuid.UUID) (string, bool, error) {
	u, err := p.identity.UserByID(ctx, userID)
	if errors.Is(err, identity.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if u.Status != identity.StatusActive {
		return "", false, nil
	}
	for _, m := range u.Memberships {
		if m.OrganisationID == organisationID {
			return u.Name, true, nil
		}
	}
	return "", false, nil
}

// Name implements appointments.People.
func (p people) Name(ctx context.Context, userID uuid.UUID) (string, error) {
	u, err := p.identity.UserByID(ctx, userID)
	if errors.Is(err, identity.ErrNotFound) {
		return "", nil
	}
	return u.Name, err
}

// inboxCounts counts what is waiting in the two enquiry inboxes, each scoped
// exactly as its own endpoint scopes it. Neither inbox has a "read" or lead
// status yet, so "new" is every received item the caller can see (capped at
// the inbox size, 100).
type inboxCounts struct {
	requests  *propertyrequests.Service
	enquiries *businessenquiries.Service
}

// NewPropertyRequests implements appointments.InboxCounts.
func (c inboxCounts) NewPropertyRequests(ctx context.Context, p *auth.Principal) (int, error) {
	items, err := c.requests.Inbox(ctx, propertyrequests.InboxScope{
		UserID:                 p.UserID,
		ManagedOrganisationIDs: p.OrganisationIDs(auth.RoleOwner, auth.RoleManager),
		MemberOrganisationIDs:  p.OrganisationIDs(auth.RoleAgent, auth.RoleStaff),
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		if it.Status == propertyrequests.StatusReceived {
			n++
		}
	}
	return n, nil
}

// NewBusinessEnquiries implements appointments.InboxCounts.
func (c inboxCounts) NewBusinessEnquiries(ctx context.Context, p *auth.Principal) (int, error) {
	if !p.HasMembership() {
		return 0, nil
	}
	items, err := c.enquiries.Inbox(ctx, businessenquiries.InboxScope{
		UserID:                 p.UserID,
		ManagedOrganisationIDs: p.OrganisationIDs(auth.RoleOwner, auth.RoleManager),
		MemberOrganisationIDs:  p.OrganisationIDs(auth.RoleAgent, auth.RoleStaff),
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		if it.Status == businessenquiries.StatusReceived {
			n++
		}
	}
	return n, nil
}

// onboardingVocabulary adapts the reference data (and the countries) to
// onboarding.Vocabulary. markets may be nil where no country is validated
// (the notification handlers).
type onboardingVocabulary struct {
	ref     *reference.Service
	markets markets.Store
}

// Countries implements onboarding.Vocabulary: every enabled country.
func (v onboardingVocabulary) Countries(ctx context.Context) ([]string, error) {
	if v.markets == nil {
		return nil, nil
	}
	all, err := v.markets.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, m := range all {
		out = append(out, m.Code)
	}
	return out, nil
}

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
	out := &onboarding.Agency{Slug: r.Slug, Name: r.Name, OrganisationID: r.OrganisationID, CityCode: r.CityCode, MarketCode: r.MarketCode}
	if out.MarketCode != "" {
		return out, nil // the agency's own country
	}
	// A profile from before profiles carried a country: the market of its
	// city, else the first market.
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
