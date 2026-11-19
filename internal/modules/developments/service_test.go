package developments

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/auth"
)

type fakeProfiles struct{ p Profile }

func (f fakeProfiles) DeveloperOf(_ context.Context, org uuid.UUID) (Profile, error) {
	return f.p, nil
}

func (f fakeProfiles) PublicRef(_ context.Context, id uuid.UUID) (*listings.ProfileRef, error) {
	return &listings.ProfileRef{Kind: KindDevelopers, Slug: f.p.Slug, Name: f.p.Name, Verified: true}, nil
}

type fakeListings struct{}

func (fakeListings) Record(context.Context, uuid.UUID) (listings.Record, error) {
	return listings.Record{}, listings.ErrNotFound
}

func (fakeListings) PublicByIDs(context.Context, []uuid.UUID) ([]listings.Listing, error) {
	return []listings.Listing{}, nil
}

type fakeLabels struct{}

func (fakeLabels) Labels(_ context.Context, set string) (map[string]string, error) {
	switch set {
	case reference.SetCities:
		return map[string]string{"harare": "Harare"}, nil
	case reference.SetSuburbs:
		return map[string]string{"borrowdale": "Borrowdale"}, nil
	case reference.SetDevelopmentAmenities:
		return map[string]string{"clubhouse": "Clubhouse"}, nil
	}
	return map[string]string{}, nil
}

type fakePlaces struct{}

func (fakePlaces) Geography(context.Context) (listings.Geography, error) {
	return listings.Geography{Countries: map[string]listings.CountryInfo{"ZW": {Name: "Zimbabwe", Currency: "USD"}},
		RegionCountry: map[string]string{"harare-province": "ZW"}, CityRegion: map[string]string{"harare": "harare-province"},
		CityCountry: map[string]string{}, SuburbCity: map[string]string{"borrowdale": "harare"}}, nil
}

type fakeInbox struct {
	scopes []businessenquiries.InboxScope
}

func (f *fakeInbox) Inbox(_ context.Context, s businessenquiries.InboxScope) ([]businessenquiries.InboxItem, error) {
	f.scopes = append(f.scopes, s)
	return []businessenquiries.InboxItem{}, nil
}

func principalWith(org uuid.UUID, role string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Name: role, Memberships: []auth.Membership{{OrganisationID: org, OrganisationKind: auth.KindDeveloper,
		MarketCode: "ZW", Role: role}}}
}

func TestRolesOnAProject(t *testing.T) {
	ctx := context.Background()
	org := uuid.New()
	inbox := &fakeInbox{}
	svc := NewService(Deps{Store: NewMemoryStore(), Listings: fakeListings{}, Profiles: fakeProfiles{p: Profile{ID: uuid.New(), Kind: KindDevelopers,
		Slug: "dev", Name: "Dev", MarketCode: "ZW"}}, Labels: fakeLabels{}, Places: fakePlaces{}, Inbox: inbox,
		Now: func() time.Time { return time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC) }})
	owner, agent, staff := principalWith(org, auth.RoleOwner), principalWith(org, auth.RoleAgent), principalWith(org, auth.RoleStaff)
	outsider := principalWith(uuid.New(), auth.RoleOwner)

	d, err := svc.Create(ctx, owner, org, []byte(`{"name": "Harare Gardens", "city": "harare", "suburb": "borrowdale", "amenities": ["clubhouse"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if d.Slug != "harare-gardens" || d.Location.Region == nil || d.Location.Region.Label != "harare-province" || len(d.Actions) == 0 {
		t.Fatalf("created: %+v", d)
	}
	// An AGENT edits; STAFF reads only; anybody else learns nothing.
	if _, err := svc.Update(ctx, agent, org, d.ID, []byte(`{"summary": "By the agent."}`)); err != nil {
		t.Fatalf("agent: %v", err)
	}
	read, err := svc.Get(ctx, staff, org, d.ID)
	if err != nil || len(read.Actions) != 0 {
		t.Fatalf("staff read: %+v %v", read.Actions, err)
	}
	if list, err := svc.List(ctx, staff, org); err != nil || len(list) != 1 {
		t.Fatalf("staff list: %v", err)
	}
	for name, run := range map[string]func() error{
		"update":    func() error { _, err := svc.Update(ctx, staff, org, d.ID, []byte(`{"summary": "x"}`)); return err },
		"phase":     func() error { _, err := svc.AddPhase(ctx, staff, org, d.ID, []byte(`{"name": "P1"}`)); return err },
		"create":    func() error { _, err := svc.Create(ctx, staff, org, []byte(`{"name": "x"}`)); return err },
		"delete":    func() error { return svc.Delete(ctx, staff, org, d.ID) },
		"submit":    func() error { _, err := svc.Submit(ctx, staff, org, d.ID); return err },
		"photo":     func() error { _, err := svc.AddPhoto(ctx, staff, org, d.ID, []byte("x")); return err },
		"link":      func() error { _, err := svc.LinkListing(ctx, staff, org, d.ID, uuid.New(), nil); return err },
		"enquiries": func() error { _, err := svc.Enquiries(ctx, staff, org, d.ID); return err },
	} {
		if err := run(); !errors.Is(err, ErrForbidden) {
			t.Fatalf("staff %s: %v", name, err)
		}
	}
	if _, err := svc.Enquiries(ctx, agent, org, d.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent enquiries: %v", err)
	}
	if _, err := svc.Enquiries(ctx, owner, org, d.ID); err != nil || len(inbox.scopes) != 1 || inbox.scopes[0].DevelopmentID == nil ||
		*inbox.scopes[0].DevelopmentID != d.ID || len(inbox.scopes[0].MemberOrganisationIDs) != 0 {
		t.Fatalf("owner enquiries: %v %+v", err, inbox.scopes)
	}
	for _, p := range []*auth.Principal{outsider, nil} {
		if _, err := svc.Get(ctx, p, org, d.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("outsider: %v", err)
		}
		if _, err := svc.List(ctx, p, org); !errors.Is(err, ErrNotFound) {
			t.Fatalf("outsider list: %v", err)
		}
	}
	// A project of another organisation is not found on this one's path.
	if _, err := svc.Get(ctx, outsider, outsider.Memberships[0].OrganisationID, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other path: %v", err)
	}
}

func TestValidationRules(t *testing.T) {
	for text, want := range map[string]bool{
		"Call 0771 234 567": true, "+263 77 123 4567": true, "sales@example.com": true, "wa.me/263771234567": true,
		"Stands from 18000-26000 USD": false, "Phase 2 in 2027": false, "1250000.00": false, "Plot 12, 400 m2": false,
	} {
		if got := HasContactDetails(text); got != want {
			t.Fatalf("HasContactDetails(%q) = %v", text, got)
		}
	}
	for v, label := range map[string]string{"2027-Q3": "Q3 2027", "2027-06-30": "30 June 2027"} {
		if c, ok := ParseCompletion(v); !ok || c.Label != label {
			t.Fatalf("%s: %+v", v, c)
		}
	}
	for _, v := range []string{"2027", "2027-Q5", "2027-02-30", "soon"} {
		if _, ok := ParseCompletion(v); ok {
			t.Fatalf("%s accepted", v)
		}
	}
	if Slugify("  Borrowdale Heights — Phase 1!") != "borrowdale-heights-phase-1" {
		t.Fatal(Slugify("Borrowdale Heights — Phase 1!"))
	}
}
