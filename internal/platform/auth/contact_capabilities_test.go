package auth

import (
	"testing"

	"github.com/google/uuid"
)

// The contact book rules: every member of an organisation that takes
// enquiries reads, OWNER / MANAGER / AGENT write, OWNER / MANAGER manage
// (reassign, import, export); another organisation, a consumer and an
// administrator see nothing; an approved direct owner has a personal book.
func TestContactCapabilities(t *testing.T) {
	org, other := uuid.New(), uuid.New()
	cases := []struct {
		name                string
		kind, role          string
		read, write, manage bool
	}{
		{"agency owner", KindAgency, RoleOwner, true, true, true},
		{"agency manager", KindAgency, RoleManager, true, true, true},
		{"agency agent", KindAgency, RoleAgent, true, true, false},
		{"agency staff", KindAgency, RoleStaff, true, false, false},
		{"law firm agent", KindLawFirm, RoleAgent, true, true, false},
		{"property manager owner", KindPropertyManager, RoleOwner, true, true, true},
		{"service provider owner", KindServiceProvider, RoleOwner, true, true, true},
		{"service provider staff", KindServiceProvider, RoleStaff, true, false, false},
		{"legacy landlord organisation", KindLandlord, RoleOwner, false, false, false},
	}
	for _, tc := range cases {
		p := &Principal{UserID: uuid.New(), Memberships: []Membership{{OrganisationID: org, OrganisationKind: tc.kind, Role: tc.role, MarketCode: "ZW"}}}
		if p.MayReadContacts(org) != tc.read || p.MayWriteContacts(org) != tc.write || p.MayManageContacts(org) != tc.manage {
			t.Fatalf("%s: read %v write %v manage %v", tc.name, p.MayReadContacts(org), p.MayWriteContacts(org), p.MayManageContacts(org))
		}
		if p.MayReadContacts(other) || p.MayWriteContacts(other) || p.MayManageContacts(other) {
			t.Fatalf("%s: sees another organisation's book", tc.name)
		}
		if p.OwnsContactBook(org) || p.OwnsContactBook(p.UserID) {
			t.Fatalf("%s: owns a personal book", tc.name)
		}
		if p.MembershipMarket(org) != "ZW" || p.MembershipMarket(other) != "" {
			t.Fatalf("%s: market %q", tc.name, p.MembershipMarket(org))
		}
	}

	consumer := &Principal{UserID: uuid.New(), AccountRoles: []AccountRole{{Role: "CONSUMER", Status: AccountRoleActive}}}
	admin := &Principal{UserID: uuid.New(), SessionScope: ScopeAdmin, PlatformRoles: []PlatformRole{{Role: PlatformSuperAdmin}}}
	for name, p := range map[string]*Principal{"consumer": consumer, "administrator": admin} {
		if p.MayReadContacts(org) || p.MayWriteContacts(org) || p.MayManageContacts(org) || p.OwnsContactBook(org) || p.OwnsContactBook(p.UserID) {
			t.Fatalf("%s sees a contact book", name)
		}
	}

	landlord := &Principal{UserID: uuid.New(), AccountRoles: []AccountRole{{Role: AccountRoleLandlord, Status: AccountRoleActive}}}
	if !landlord.OwnsContactBook(landlord.UserID) || landlord.OwnsContactBook(org) || landlord.MayReadContacts(landlord.UserID) {
		t.Fatal("an approved landlord owns exactly their own book")
	}
	pending := &Principal{UserID: uuid.New(), AccountRoles: []AccountRole{{Role: AccountRolePrivateSeller, Status: "PENDING_VERIFICATION"}}}
	if pending.OwnsContactBook(pending.UserID) {
		t.Fatal("a seller who is not approved has no book")
	}
	var none *Principal
	if none.OwnsContactBook(uuid.New()) {
		t.Fatal("no principal, no book")
	}
}
