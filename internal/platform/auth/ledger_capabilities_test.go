package auth

import (
	"testing"

	"github.com/google/uuid"
)

// The ledger rules: every member reads, OWNER / MANAGER / AGENT record,
// OWNER / MANAGER manage (correct any entry, reconcile, export, categories);
// an AGENT corrects only their own unreconciled entries; another
// organisation, a consumer and an administrator see no book; an approved
// direct owner has a personal one.
func TestLedgerCapabilities(t *testing.T) {
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
		{"host agent", KindHost, RoleAgent, true, true, false},
		{"service provider owner", KindServiceProvider, RoleOwner, true, true, true},
		{"legacy landlord organisation", KindLandlord, RoleOwner, false, false, false},
	}
	for _, tc := range cases {
		me := uuid.New()
		someone := uuid.New()
		p := &Principal{UserID: me, Memberships: []Membership{{OrganisationID: org, OrganisationKind: tc.kind, Role: tc.role}}}
		if p.MayReadLedger(org) != tc.read || p.MayWriteLedger(org) != tc.write || p.MayManageLedger(org) != tc.manage {
			t.Fatalf("%s: read %v write %v manage %v", tc.name, p.MayReadLedger(org), p.MayWriteLedger(org), p.MayManageLedger(org))
		}
		if p.MayReadLedger(other) || p.MayWriteLedger(other) || p.MayManageLedger(other) || p.MayCorrectLedgerEntry(other, &me, false) {
			t.Fatalf("%s: sees another organisation's book", tc.name)
		}
		if p.OwnsLedgerBook(org) || p.OwnsLedgerBook(me) {
			t.Fatalf("%s: owns a personal book", tc.name)
		}
		// Correcting: managers any entry; writers their own unreconciled ones.
		if got := p.MayCorrectLedgerEntry(org, &me, false); got != tc.write {
			t.Fatalf("%s: own unreconciled %v", tc.name, got)
		}
		if got := p.MayCorrectLedgerEntry(org, &me, true); got != tc.manage {
			t.Fatalf("%s: own reconciled %v", tc.name, got)
		}
		if got := p.MayCorrectLedgerEntry(org, &someone, false); got != tc.manage {
			t.Fatalf("%s: someone else's %v", tc.name, got)
		}
		if got := p.MayCorrectLedgerEntry(org, nil, false); got != tc.manage {
			t.Fatalf("%s: no author %v", tc.name, got)
		}
	}
	consumer := &Principal{UserID: uuid.New(), AccountRoles: []AccountRole{{Role: "CONSUMER", Status: AccountRoleActive}}}
	admin := &Principal{UserID: uuid.New(), SessionScope: ScopeAdmin, PlatformRoles: []PlatformRole{{Role: PlatformSuperAdmin}}}
	for name, p := range map[string]*Principal{"consumer": consumer, "administrator": admin} {
		if p.MayReadLedger(org) || p.MayWriteLedger(org) || p.MayManageLedger(org) || p.OwnsLedgerBook(org) || p.OwnsLedgerBook(p.UserID) {
			t.Fatalf("%s sees a book", name)
		}
	}
	landlord := &Principal{UserID: uuid.New(), AccountRoles: []AccountRole{{Role: AccountRoleLandlord, Status: AccountRoleActive}}}
	if !landlord.OwnsLedgerBook(landlord.UserID) || landlord.OwnsLedgerBook(org) || landlord.MayReadLedger(landlord.UserID) {
		t.Fatal("an approved landlord owns exactly their own book")
	}
	pending := &Principal{UserID: uuid.New(), AccountRoles: []AccountRole{{Role: AccountRolePrivateSeller, Status: "PENDING_VERIFICATION"}}}
	if pending.OwnsLedgerBook(pending.UserID) {
		t.Fatal("a seller who is not approved has no book")
	}
}
