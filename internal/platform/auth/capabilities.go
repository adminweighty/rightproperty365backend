package auth

import (
	"slices"

	"github.com/google/uuid"
)

// This file is the ONE place that answers "may this caller manage listings
// or appointments". Routes must ask here instead of looking at organisation
// kinds or roles themselves.
//
// NOTE: there are still no endpoints that create or publish a listing, for
// anyone. These rules say who will be allowed to when they exist, and who may
// manage appointments.

// AccountRoleActive is the status of an account role that has been approved.
const AccountRoleActive = "ACTIVE"

// listingKinds are the organisation kinds that own listings and take
// appointments: agencies, law firms, property managers, developers and hosts.
// A SERVICE_PROVIDER organisation receives enquiries and consultations only.
var listingKinds = []string{KindAgency, KindLawFirm, KindPropertyManager, KindDeveloper, KindHost}

// KindManagesListings reports whether organisations of that kind own
// listings and take appointments.
func KindManagesListings(kind string) bool { return slices.Contains(listingKinds, kind) }

// KindTakesConsultations reports whether organisations of that kind can be
// asked for a consultation: every kind that manages listings, and service
// providers.
func KindTakesConsultations(kind string) bool {
	return KindManagesListings(kind) || kind == KindServiceProvider
}

func (p *Principal) membership(organisationID uuid.UUID) (Membership, bool) {
	for _, m := range p.Memberships {
		if m.OrganisationID == organisationID {
			return m, true
		}
	}
	return Membership{}, false
}

// MayManageListings reports whether the caller may create and edit listings
// of the organisation: an OWNER, MANAGER or AGENT of an organisation whose
// kind manages listings. STAFF support the others and do not.
func (p *Principal) MayManageListings(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindManagesListings(m.OrganisationKind) && slices.Contains([]string{RoleOwner, RoleManager, RoleAgent}, m.Role)
}

// MayManageAppointments reports whether the caller may see and act on
// appointments of the organisation: any member of an organisation whose kind
// manages listings (which of them a given member sees is the inbox's
// scoping), and any member of a service provider for consultations.
func (p *Principal) MayManageAppointments(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindTakesConsultations(m.OrganisationKind)
}

// MayAssignWork reports whether the caller may assign the organisation's
// requests and appointments to its members: OWNER and MANAGER only.
func (p *Principal) MayAssignWork(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && (m.Role == RoleOwner || m.Role == RoleManager)
}

// IsDirectOwner reports whether the caller is an APPROVED individual owner
// (landlord or property seller): they may manage their own listings and the
// appointments for them, without an organisation.
func (p *Principal) IsDirectOwner() bool {
	for _, r := range p.AccountRoles {
		if r.Status == AccountRoleActive && slices.Contains(DirectOwnerRoles(), r.Role) {
			return true
		}
	}
	return false
}

// MembershipRoleLabel is how a membership role is shown: in an agency the
// real estate titles, elsewhere the general ones.
func MembershipRoleLabel(organisationKind, role string) string {
	if organisationKind == KindAgency {
		switch role {
		case RoleOwner:
			return "Agency Principal"
		case RoleManager:
			return "Agency Administrator"
		case RoleAgent:
			return "Estate Agent"
		case RoleStaff:
			return "Agency Staff"
		}
	}
	switch role {
	case RoleOwner:
		return "Principal"
	case RoleManager:
		return "Manager"
	case RoleAgent:
		return "Estate Agent"
	case RoleStaff:
		return "Staff"
	}
	return role
}

// ListingOrganisationIDs returns the organisations whose listings the caller
// may manage (see MayManageListings).
func (p *Principal) ListingOrganisationIDs() []uuid.UUID {
	var out []uuid.UUID
	for _, m := range p.Memberships {
		if p.MayManageListings(m.OrganisationID) {
			out = append(out, m.OrganisationID)
		}
	}
	return out
}

// ManagesListings reports whether the caller may manage any listings at all:
// a member who may manage listings for at least one organisation, or an
// approved direct owner.
func (p *Principal) ManagesListings() bool {
	return len(p.ListingOrganisationIDs()) > 0 || p.IsDirectOwner()
}

// MayManageListing reports whether the caller may manage ONE listing, given
// who it belongs to: a member who may manage listings for the listing's
// organisation, or the approved direct owner who sells or lets it themselves.
func (p *Principal) MayManageListing(organisationID, sellerUserID *uuid.UUID) bool {
	if organisationID != nil && p.MayManageListings(*organisationID) {
		return true
	}
	return sellerUserID != nil && *sellerUserID == p.UserID && p.IsDirectOwner()
}
