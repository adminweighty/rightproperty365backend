package auth

import (
	"testing"

	"github.com/google/uuid"
)

// Who may change one listing: the organisation's owners and managers all of
// its listings, an agent their own, staff none; a direct owner their own,
// once approved.
func TestMayEditListing(t *testing.T) {
	org, me, colleague := uuid.New(), uuid.New(), uuid.New()
	member := func(kind, role string) *Principal {
		return &Principal{UserID: me, Memberships: []Membership{{OrganisationID: org, OrganisationKind: kind, Role: role}}}
	}
	cases := []struct {
		name  string
		p     *Principal
		agent *uuid.UUID
		want  bool
	}{
		{"owner, colleague's listing", member(KindAgency, RoleOwner), &colleague, true},
		{"manager, no agent", member(KindAgency, RoleManager), nil, true},
		{"agent, own listing", member(KindAgency, RoleAgent), &me, true},
		{"agent, colleague's listing", member(KindAgency, RoleAgent), &colleague, false},
		{"agent, no agent", member(KindAgency, RoleAgent), nil, false},
		{"staff", member(KindAgency, RoleStaff), &me, false},
		{"host owner", member(KindHost, RoleOwner), nil, true},
		{"developer manager", member(KindDeveloper, RoleManager), nil, true},
		{"service provider owner", member(KindServiceProvider, RoleOwner), nil, false},
	}
	for _, tc := range cases {
		if got := tc.p.MayEditListing(&org, tc.agent, nil); got != tc.want {
			t.Fatalf("%s: %v, want %v", tc.name, got, tc.want)
		}
		if tc.p.MayEditListing(&colleague, tc.agent, nil) {
			t.Fatalf("%s: rights in another organisation", tc.name)
		}
	}
	seller := &Principal{UserID: me, AccountRoles: []AccountRole{{Role: AccountRoleLandlord, Status: AccountRoleActive}}}
	pending := &Principal{UserID: me, AccountRoles: []AccountRole{{Role: AccountRoleLandlord, Status: "PENDING_VERIFICATION"}}}
	if !seller.MayEditListing(nil, nil, &me) || seller.MayEditListing(nil, nil, &colleague) || pending.MayEditListing(nil, nil, &me) {
		t.Fatal("direct owner rule")
	}
	// Creating follows MayManageListings (the dashboard's can_manage_listings).
	if !member(KindAgency, RoleAgent).MayCreateListing(org) || member(KindAgency, RoleStaff).MayCreateListing(org) {
		t.Fatal("create rule")
	}
}

func TestListingReviewMarkets(t *testing.T) {
	admin := func(role, market string) *Principal {
		return &Principal{SessionScope: ScopeAdmin, PlatformRoles: []PlatformRole{{Role: role, MarketCode: market}}}
	}
	for _, role := range []string{PlatformSuperAdmin, PlatformAdmin, PlatformModerator} {
		if _, all, ok := admin(role, "").ListingReviewMarkets(); !all || !ok {
			t.Fatalf("%s reviews everywhere", role)
		}
	}
	zw := admin(PlatformMarketAdmin, "ZW")
	if codes, all, ok := zw.ListingReviewMarkets(); all || !ok || len(codes) != 1 || !zw.MayReviewListing("ZW") || zw.MayReviewListing("GB") {
		t.Fatalf("market admin: %v %v %v", codes, all, ok)
	}
	for _, role := range []string{PlatformSupport, PlatformFinanceAdmin, PlatformContentAdmin} {
		if _, _, ok := admin(role, "").ListingReviewMarkets(); ok {
			t.Fatalf("%s must not review listings", role)
		}
	}
	customer := admin(PlatformModerator, "")
	customer.SessionScope = ScopeCustomer
	if _, _, ok := customer.ListingReviewMarkets(); ok {
		t.Fatal("a platform role on a customer session reviews nothing")
	}
}
