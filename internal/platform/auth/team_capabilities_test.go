package auth

import (
	"slices"
	"testing"

	"github.com/google/uuid"
)

// The team rules: an OWNER grants MANAGER, AGENT and STAFF; a MANAGER only
// AGENT and STAFF; nobody grants OWNER; AGENT and STAFF manage no team.
func TestTeamCapabilities(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	cases := []struct {
		role       string
		manage     bool
		grantable  []string
		holdsLeads bool
	}{
		{RoleOwner, true, []string{RoleManager, RoleAgent, RoleStaff}, true},
		{RoleManager, true, []string{RoleAgent, RoleStaff}, true},
		{RoleAgent, false, nil, true},
		{RoleStaff, false, nil, false},
	}
	for _, tc := range cases {
		p := &Principal{Memberships: []Membership{{OrganisationID: org, OrganisationKind: KindAgency, Role: tc.role}}}
		if p.MayManageTeam(org) != tc.manage || p.MayManageTeam(other) {
			t.Fatalf("%s: MayManageTeam = %v (other %v)", tc.role, p.MayManageTeam(org), p.MayManageTeam(other))
		}
		for _, role := range []string{RoleOwner, RoleManager, RoleAgent, RoleStaff} {
			if want := slices.Contains(tc.grantable, role); p.MayGrantRole(org, role) != want || p.MayGrantRole(other, role) {
				t.Fatalf("%s granting %s = %v, want %v", tc.role, role, p.MayGrantRole(org, role), want)
			}
		}
		if RoleHoldsLeads(tc.role) != tc.holdsLeads {
			t.Fatalf("%s holds leads = %v", tc.role, RoleHoldsLeads(tc.role))
		}
		if p.MembershipRole(org) != tc.role || p.MembershipRole(other) != "" {
			t.Fatalf("%s: MembershipRole = %q", tc.role, p.MembershipRole(org))
		}
	}
}
