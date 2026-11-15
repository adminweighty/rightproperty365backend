package auth

import (
	"slices"

	"github.com/google/uuid"
)

// This file is the ONE place that answers "may this caller manage listings,
// appointments or a directory profile". Routes must ask here instead of looking at organisation
// kinds or roles themselves.
//
// Listings are created and edited through /v1/me/listings (MayCreateListing,
// MayEditListing) and reviewed through /v1/admin/listings
// (ListingReviewMarkets).

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

// MayManageProfile reports whether the caller may change a directory
// profile's public presentation (its logo today), given who the profile
// belongs to:
//
//   - an agent profile (agentUserID set): only the agent themselves, while
//     they are still a member of the organisation;
//   - any other profile (an agency's, a law firm's, a host's, ...): an OWNER
//     or MANAGER of the organisation that owns it. An AGENT or STAFF member
//     may not change the business's profile.
func (p *Principal) MayManageProfile(organisationID uuid.UUID, agentUserID *uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	if !ok {
		return false
	}
	if agentUserID != nil {
		return *agentUserID == p.UserID
	}
	return m.Role == RoleOwner || m.Role == RoleManager
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
// It decides who SEES a listing in GET /v1/me/listings (and edits a short
// stay's calendar); who may change its content is MayEditListing.
func (p *Principal) MayManageListing(organisationID, sellerUserID *uuid.UUID) bool {
	if organisationID != nil && p.MayManageListings(*organisationID) {
		return true
	}
	return sellerUserID != nil && *sellerUserID == p.UserID && p.IsDirectOwner()
}

// MayCreateListing reports whether the caller may create a listing for the
// organisation: an OWNER, MANAGER or AGENT of an organisation whose kind
// manages listings (MayManageListings, the dashboard's can_manage_listings).
// An approved direct owner creates listings for themselves (IsDirectOwner).
func (p *Principal) MayCreateListing(organisationID uuid.UUID) bool {
	return p.MayManageListings(organisationID)
}

// MayEditListing reports whether the caller may change ONE listing's
// content, photos and lifecycle (edit, submit, archive, relist, delete):
//
//   - an OWNER or MANAGER of the organisation: every listing of it;
//   - an AGENT of the organisation: only the listings they are the assigned
//     agent of (they may see the others, read-only);
//   - STAFF: none (STAFF do not manage listings at all);
//   - the approved direct owner (landlord, property seller): their own.
func (p *Principal) MayEditListing(organisationID, agentUserID, sellerUserID *uuid.UUID) bool {
	if organisationID != nil {
		m, ok := p.membership(*organisationID)
		if !ok || !KindManagesListings(m.OrganisationKind) {
			return false
		}
		switch m.Role {
		case RoleOwner, RoleManager:
			return true
		case RoleAgent:
			return agentUserID != nil && *agentUserID == p.UserID
		}
		return false
	}
	return sellerUserID != nil && *sellerUserID == p.UserID && p.IsDirectOwner()
}

// ListingReviewRoles are the platform roles that review listings: the
// moderators and the platform and super administrators everywhere, a market
// administrator in its own markets (the scope of the listings section of
// GET /v1/admin/overview).
func ListingReviewRoles() []string {
	return []string{PlatformSuperAdmin, PlatformAdmin, PlatformModerator, PlatformMarketAdmin}
}

// ListingReviewMarkets returns the countries whose listings the caller may
// review: all (all=true) for SUPER_ADMIN, PLATFORM_ADMIN and MODERATOR, the
// market codes of their MARKET_ADMIN roles otherwise. ok is false for anyone
// who may review none, including a platform role on a customer session.
func (p *Principal) ListingReviewMarkets() (codes []string, all, ok bool) {
	if p == nil || p.SessionScope != ScopeAdmin {
		return nil, false, false
	}
	for _, r := range p.PlatformRoles {
		switch r.Role {
		case PlatformSuperAdmin, PlatformAdmin, PlatformModerator:
			return nil, true, true
		case PlatformMarketAdmin:
			if r.MarketCode != "" && !slices.Contains(codes, r.MarketCode) {
				codes = append(codes, r.MarketCode)
			}
		}
	}
	return codes, false, len(codes) > 0
}

// MayReviewListing reports whether the caller may review a listing in that country.
func (p *Principal) MayReviewListing(marketCode string) bool {
	codes, all, ok := p.ListingReviewMarkets()
	return ok && (all || slices.Contains(codes, marketCode))
}
