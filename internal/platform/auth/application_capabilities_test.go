package auth

import (
	"testing"

	"github.com/google/uuid"
)

// Forms: OWNER / MANAGER manage, AGENT reads, STAFF nothing. Received
// applications: OWNER / MANAGER all, an AGENT those of their own listings.
func TestApplicationCapabilities(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	cases := []struct {
		name                            string
		kind, role                      string
		readForms, manageForms, seesAll bool
		ownListing, otherListing        bool
	}{
		{"agency owner", KindAgency, RoleOwner, true, true, true, true, true},
		{"agency manager", KindAgency, RoleManager, true, true, true, true, true},
		{"agency agent", KindAgency, RoleAgent, true, false, false, true, false},
		{"agency staff", KindAgency, RoleStaff, false, false, false, false, false},
		{"service provider owner", KindServiceProvider, RoleOwner, false, false, false, false, false},
	}
	for _, tc := range cases {
		p := &Principal{UserID: uuid.New(), Memberships: []Membership{{OrganisationID: org, OrganisationKind: tc.kind, Role: tc.role}}}
		mine, someone := p.UserID, uuid.New()
		if p.MayReadForms(org) != tc.readForms || p.MayManageForms(org) != tc.manageForms || p.MaySeeAllApplications(org) != tc.seesAll {
			t.Fatalf("%s: forms read %v manage %v all %v", tc.name, p.MayReadForms(org), p.MayManageForms(org), p.MaySeeAllApplications(org))
		}
		if p.MayReadApplication(org, &mine) != tc.ownListing || p.MayReadApplication(org, &someone) != tc.otherListing {
			t.Fatalf("%s: own listing %v, other listing %v", tc.name, p.MayReadApplication(org, &mine), p.MayReadApplication(org, &someone))
		}
		if p.MayReadForms(other) || p.MayManageForms(other) || p.MayReadApplication(other, &mine) {
			t.Fatalf("%s: sees another organisation's forms or applications", tc.name)
		}
	}
}
