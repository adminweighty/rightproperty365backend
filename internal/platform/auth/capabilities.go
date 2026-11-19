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

// MembershipRole returns the caller's role in the organisation, or "" when
// the caller is not an active member of it.
func (p *Principal) MembershipRole(organisationID uuid.UUID) string {
	m, _ := p.membership(organisationID)
	return m.Role
}

// MayManageTeam reports whether the caller may manage the organisation's
// team: see its members, invite people, decide join requests, change roles,
// suspend and remove members, and read the team's audit trail. OWNER and
// MANAGER only (the same people who assign work); what each of them may do
// to whom is MayGrantRole.
func (p *Principal) MayManageTeam(organisationID uuid.UUID) bool {
	return p.MayAssignWork(organisationID)
}

// GrantableRoles returns the membership roles a member with callerRole may
// give (by invitation, by approving a join request or by changing a member's
// role), and so the roles of the members they may act on:
//
//   - an OWNER: MANAGER, AGENT and STAFF;
//   - a MANAGER: AGENT and STAFF (only an OWNER makes someone a MANAGER);
//   - anybody else: none.
//
// Nobody can grant OWNER, and nobody can act on an OWNER.
func GrantableRoles(callerRole string) []string {
	switch callerRole {
	case RoleOwner:
		return []string{RoleManager, RoleAgent, RoleStaff}
	case RoleManager:
		return []string{RoleAgent, RoleStaff}
	}
	return nil
}

// MayGrantRole reports whether the caller may give role to someone in the
// organisation, or act on a member who holds it (see GrantableRoles).
func (p *Principal) MayGrantRole(organisationID uuid.UUID, role string) bool {
	return slices.Contains(GrantableRoles(p.MembershipRole(organisationID)), role)
}

// RoleHoldsLeads reports whether a member with that role can have property
// requests assigned to them: OWNER, MANAGER and AGENT. STAFF support the
// others and hold no leads (as they manage no listings).
func RoleHoldsLeads(role string) bool {
	return role == RoleOwner || role == RoleManager || role == RoleAgent
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

// Contact books (the agent CRM, API 0.31.0). A book belongs to an
// organisation, or (a personal book) to an approved direct owner, whose book
// is addressed with their own user ID in place of an organisation ID.
//
//   - read: any member (STAFF included) of an organisation whose kind takes
//     enquiries or consultations (KindTakesConsultations: the kinds that
//     manage listings, and service providers);
//   - write (create, edit, notes, reminders, stage, listings, merge): its
//     OWNER, MANAGER and AGENT;
//   - manage (reassign any contact, import, export): its OWNER and MANAGER.
//
// The direct owner reads and writes their own book; nobody else sees it.
// Everybody else, platform administrators included, sees no book at all.

// MayReadContacts reports whether the caller may read the organisation's contact book.
func (p *Principal) MayReadContacts(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindTakesConsultations(m.OrganisationKind)
}

// MayWriteContacts reports whether the caller may change the organisation's contact book.
func (p *Principal) MayWriteContacts(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindTakesConsultations(m.OrganisationKind) && RoleHoldsLeads(m.Role)
}

// MayManageContacts reports whether the caller may reassign any contact of
// the organisation, and import and export its book.
func (p *Principal) MayManageContacts(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindTakesConsultations(m.OrganisationKind) && (m.Role == RoleOwner || m.Role == RoleManager)
}

// OwnsContactBook reports whether bookID is the caller's personal contact
// book: their own user ID, while they are an approved direct owner.
func (p *Principal) OwnsContactBook(bookID uuid.UUID) bool {
	return p != nil && bookID == p.UserID && p.IsDirectOwner()
}

// Questionnaires and applications (API 0.32.0). Forms and received
// applications belong to an organisation whose kind manages listings, or to
// an approved direct owner (addressed with their own user ID, as contact
// books are: OwnsContactBook).
//
//   - forms: its OWNER and MANAGER create, edit, publish and retire them;
//     its AGENT reads them; STAFF may not (403); anybody else sees none (404).
//   - received applications: its OWNER and MANAGER see every one; an AGENT
//     those of the listings they are the assigned agent of; nobody else any
//     (404), STAFF included.
//
// The direct owner manages their own forms and sees the applications to
// their own listings.

// MayReadForms reports whether the caller may read the organisation's forms.
func (p *Principal) MayReadForms(organisationID uuid.UUID) bool {
	return p.MayManageListings(organisationID)
}

// MayManageForms reports whether the caller may create, edit, publish and
// retire the organisation's forms.
func (p *Principal) MayManageForms(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindManagesListings(m.OrganisationKind) && (m.Role == RoleOwner || m.Role == RoleManager)
}

// MaySeeAllApplications reports whether the caller sees every application
// the organisation received (OWNER and MANAGER).
func (p *Principal) MaySeeAllApplications(organisationID uuid.UUID) bool {
	return p.MayManageForms(organisationID)
}

// MayReadApplication reports whether the caller may read one application
// the organisation received, given the listing's assigned agent: OWNER and
// MANAGER always, an AGENT when they are that agent.
func (p *Principal) MayReadApplication(organisationID uuid.UUID, agentUserID *uuid.UUID) bool {
	if p.MaySeeAllApplications(organisationID) {
		return true
	}
	m, ok := p.membership(organisationID)
	return ok && KindManagesListings(m.OrganisationKind) && m.Role == RoleAgent && agentUserID != nil && *agentUserID == p.UserID
}

// MembershipMarket returns the market code of the caller's membership of the
// organisation, or "" when the caller is not a member of it.
func (p *Principal) MembershipMarket(organisationID uuid.UUID) string {
	m, _ := p.membership(organisationID)
	return m.MarketCode
}

// Hourly venue bookings (API 0.33.0, FR-025). The bookings of an
// organisation's listings are handled by its OWNER and MANAGER (every one)
// and by a listing's assigned AGENT (that listing's); nobody else sees any,
// STAFF included (404). The direct owner (landlord, property seller)
// handles the bookings of their own listings. The hourly settings of a
// listing are managed by whoever may edit it (MayEditListing).

// MaySeeAllVenueBookings reports whether the caller handles every hourly
// booking of the organisation's listings (OWNER and MANAGER).
func (p *Principal) MaySeeAllVenueBookings(organisationID uuid.UUID) bool {
	return p.MayManageForms(organisationID)
}

// MayHandleVenueBooking reports whether the caller may see and act on one
// hourly booking of the organisation, given its listing's assigned agent.
func (p *Principal) MayHandleVenueBooking(organisationID uuid.UUID, agentUserID *uuid.UUID) bool {
	return p.MayReadApplication(organisationID, agentUserID)
}

// Agent ledgers / books (API 0.34.0, BR-012, FR-026 to FR-029). A book
// belongs to an organisation whose kind takes enquiries or consultations
// (as contact books do), or (a personal book) to an approved direct owner,
// addressed with their own user ID (OwnsContactBook).
//
//   - read (entries, reports, attachments, categories): any member, STAFF
//     included;
//   - write (record entries; reverse, correct, note and attach to the
//     entries they recorded themselves while unreconciled): its OWNER,
//     MANAGER and AGENT;
//   - manage (reverse and correct any entry, reconcile, export, manage
//     categories): its OWNER and MANAGER.
//
// The direct owner reads, writes and manages their own book. Anybody else,
// platform administrators included, sees no book (404 BOOK_NOT_FOUND).

// MayReadLedger reports whether the caller may read the organisation's books.
func (p *Principal) MayReadLedger(organisationID uuid.UUID) bool {
	return p.MayReadContacts(organisationID)
}

// MayWriteLedger reports whether the caller may record entries in the
// organisation's books.
func (p *Principal) MayWriteLedger(organisationID uuid.UUID) bool {
	return p.MayWriteContacts(organisationID)
}

// MayManageLedger reports whether the caller may reverse and correct any
// entry of the organisation's books, reconcile, export and manage categories.
func (p *Principal) MayManageLedger(organisationID uuid.UUID) bool {
	return p.MayManageContacts(organisationID)
}

// MayCorrectLedgerEntry reports whether the caller may reverse or correct
// one entry given who recorded it and whether it is reconciled: a manager
// any entry; an AGENT only their own unreconciled ones.
func (p *Principal) MayCorrectLedgerEntry(organisationID uuid.UUID, authorUserID *uuid.UUID, reconciled bool) bool {
	if p.MayManageLedger(organisationID) {
		return true
	}
	return p.MayWriteLedger(organisationID) && authorUserID != nil && *authorUserID == p.UserID && !reconciled
}

// OwnsLedgerBook reports whether bookID is the caller's personal book: their
// own user ID, while they are an approved direct owner.
func (p *Principal) OwnsLedgerBook(bookID uuid.UUID) bool { return p.OwnsContactBook(bookID) }

// Properties, units and tenancies (API 0.36.0, BR-019, FR-047). They belong
// to an organisation whose kind manages listings (agencies, property
// managers and the like), or (a personal book) to an approved direct owner,
// addressed with their own user ID (OwnsContactBook), the same books as the
// CRM and the ledger.
//
//   - read: any member, STAFF included;
//   - write (properties, units, tenancies, activate, end): its OWNER,
//     MANAGER and AGENT.
//
// The direct owner reads and writes their own book. Anybody else, platform
// administrators included, sees none (404 BOOK_NOT_FOUND). A tenant reads
// their own tenancies through /v1/me/tenancies (matched by verified email),
// never through these rights.

// MayReadProperties reports whether the caller may read the organisation's
// properties and tenancies.
func (p *Principal) MayReadProperties(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindManagesListings(m.OrganisationKind)
}

// MayWriteProperties reports whether the caller may change the
// organisation's properties and tenancies.
func (p *Principal) MayWriteProperties(organisationID uuid.UUID) bool {
	m, ok := p.membership(organisationID)
	return ok && KindManagesListings(m.OrganisationKind) && RoleHoldsLeads(m.Role)
}

// OwnsPropertyBook reports whether bookID is the caller's personal book:
// their own user ID, while they are an approved direct owner.
func (p *Principal) OwnsPropertyBook(bookID uuid.UUID) bool { return p.OwnsContactBook(bookID) }
