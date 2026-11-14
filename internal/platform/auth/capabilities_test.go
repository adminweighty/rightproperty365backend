package auth

import (
	"testing"

	"github.com/google/uuid"
)

func TestCapabilities(t *testing.T) {
	org := uuid.New()
	other := uuid.New()
	cases := []struct {
		kind, role                         string
		listings, appointments, assignWork bool
	}{
		{KindAgency, RoleOwner, true, true, true},
		{KindAgency, RoleManager, true, true, true},
		{KindAgency, RoleAgent, true, true, false},
		{KindAgency, RoleStaff, false, true, false},
		// A verified solicitor in a law firm has the same rights as an agency's people.
		{KindLawFirm, RoleOwner, true, true, true},
		{KindLawFirm, RoleAgent, true, true, false},
		{KindPropertyManager, RoleOwner, true, true, true},
		{KindDeveloper, RoleManager, true, true, true},
		{KindHost, RoleOwner, true, true, true},
		// Service providers: enquiries and consultations only, never listings.
		{KindServiceProvider, RoleOwner, false, true, true},
		{KindServiceProvider, RoleAgent, false, true, false},
		// The legacy kind manages nothing.
		{KindLandlord, RoleOwner, false, false, true},
	}
	for _, tc := range cases {
		p := &Principal{Memberships: []Membership{{OrganisationID: org, OrganisationKind: tc.kind, Role: tc.role}}}
		if p.MayManageListings(org) != tc.listings || p.MayManageAppointments(org) != tc.appointments || p.MayAssignWork(org) != tc.assignWork {
			t.Fatalf("%s %s: listings %v appointments %v assign %v", tc.kind, tc.role, p.MayManageListings(org), p.MayManageAppointments(org), p.MayAssignWork(org))
		}
		// Never for an organisation the caller is not a member of.
		if p.MayManageListings(other) || p.MayManageAppointments(other) || p.MayAssignWork(other) {
			t.Fatalf("%s %s has rights in another organisation", tc.kind, tc.role)
		}
		if p.IsDirectOwner() {
			t.Fatalf("%s %s is not a direct owner", tc.kind, tc.role)
		}
	}

	// Individual owners: only once the role is ACTIVE (approved).
	for role, want := range map[string]bool{AccountRoleLandlord: true, AccountRolePrivateSeller: true, "AGENT": false, "CONSUMER": false, "SOLICITOR": false} {
		active := &Principal{AccountRoles: []AccountRole{{Role: role, Status: AccountRoleActive}}}
		if active.IsDirectOwner() != want {
			t.Fatalf("ACTIVE %s: direct owner = %v", role, active.IsDirectOwner())
		}
		for _, status := range []string{"PENDING_VERIFICATION", "REJECTED", "REVOKED"} {
			if (&Principal{AccountRoles: []AccountRole{{Role: role, Status: status}}}).IsDirectOwner() {
				t.Fatalf("%s %s must not be a direct owner", status, role)
			}
		}
		if active.MayManageListings(org) || active.MayManageAppointments(org) {
			t.Fatalf("%s has organisation rights without a membership", role)
		}
	}
	// A platform role gives none of this.
	admin := &Principal{SessionScope: ScopeAdmin, PlatformRoles: []PlatformRole{{Role: PlatformSuperAdmin}}}
	if admin.MayManageListings(org) || admin.MayManageAppointments(org) || admin.IsDirectOwner() {
		t.Fatal("a platform role must not manage listings or appointments")
	}

	for _, kind := range []string{KindAgency, KindLawFirm, KindPropertyManager, KindDeveloper, KindHost} {
		if !KindManagesListings(kind) || !KindTakesConsultations(kind) {
			t.Fatalf("%s must manage listings and take consultations", kind)
		}
	}
	if KindManagesListings(KindServiceProvider) || !KindTakesConsultations(KindServiceProvider) {
		t.Fatal("service providers take consultations only")
	}

	labels := map[[2]string]string{
		{KindAgency, RoleOwner}: "Agency Principal", {KindAgency, RoleManager}: "Agency Administrator", {KindAgency, RoleAgent}: "Estate Agent",
		{KindAgency, RoleStaff}: "Agency Staff", {KindLawFirm, RoleOwner}: "Principal", {KindHost, RoleManager}: "Manager",
		{KindLawFirm, RoleAgent}: "Estate Agent", {KindDeveloper, RoleStaff}: "Staff",
	}
	for k, want := range labels {
		if got := MembershipRoleLabel(k[0], k[1]); got != want {
			t.Fatalf("label of %v = %q, want %q", k, got, want)
		}
	}
}

// Who may change a directory profile's public presentation (its logo).
func TestMayManageProfile(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	me, colleague := uuid.New(), uuid.New()
	for _, tc := range []struct {
		role            string
		business, mine  bool
		colleaguesPhoto bool
	}{
		{RoleOwner, true, true, false},
		{RoleManager, true, true, false},
		// An agent changes their own photo, never the business's logo.
		{RoleAgent, false, true, false},
		{RoleStaff, false, true, false},
	} {
		p := &Principal{UserID: me, Memberships: []Membership{{OrganisationID: org, OrganisationKind: KindAgency, Role: tc.role}}}
		if p.MayManageProfile(org, nil) != tc.business || p.MayManageProfile(org, &me) != tc.mine || p.MayManageProfile(org, &colleague) != tc.colleaguesPhoto {
			t.Fatalf("%s: business %v, own %v, a colleague's %v", tc.role, p.MayManageProfile(org, nil), p.MayManageProfile(org, &me), p.MayManageProfile(org, &colleague))
		}
		// Never in an organisation the caller is not a member of — not even
		// their own agent profile once they have left it.
		if p.MayManageProfile(other, nil) || p.MayManageProfile(other, &me) {
			t.Fatalf("%s manages a profile of another organisation", tc.role)
		}
	}
	if (&Principal{UserID: me}).MayManageProfile(org, &me) {
		t.Fatal("a caller without a membership manages a profile")
	}
}
