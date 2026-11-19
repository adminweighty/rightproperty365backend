// Package auth carries the authenticated caller (the Principal) through the
// request context and provides the middleware every module uses to require
// authentication, an organisation membership or a platform role.
//
// The package knows nothing about how sessions are stored: the identity module
// supplies a SessionResolver. Authorisation is always decided on the server
// from the Principal; nothing sent by a client (headers, body) can widen it.
package auth

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

// SessionCookieName is the web session cookie.
const SessionCookieName = "rp_session"

// How the session token reached the API.
const (
	ViaCookie = "cookie"
	ViaBearer = "bearer"
)

// Session scopes. A customer session is what the public login issues. An
// admin session is issued only by the administrator login, only to accounts
// holding a platform role, only as a cookie, and is much shorter-lived.
// Platform-role checks require an admin session.
const (
	ScopeCustomer = "customer"
	ScopeAdmin    = "admin"
)

// Organisation kinds.
const (
	KindAgency = "AGENCY"
	// KindLandlord is no longer created: a landlord is an individual owner
	// (an account role), and the businesses that used this kind are
	// KindPropertyManager since migration 0010.
	KindLandlord        = "LANDLORD"
	KindPropertyManager = "PROPERTY_MANAGER"
	// KindLawFirm is a solicitor's firm or practice. It can own listings on the
	// same terms as an agency.
	KindLawFirm         = "LAW_FIRM"
	KindHost            = "HOST"
	KindDeveloper       = "DEVELOPER"
	KindServiceProvider = "SERVICE_PROVIDER"
)

// Membership roles, scoped to one organisation and market.
const (
	RoleOwner   = "OWNER"
	RoleManager = "MANAGER"
	RoleAgent   = "AGENT"
	RoleStaff   = "STAFF"
)

// Platform roles, held by the user. MARKET_ADMIN is scoped to one market; the
// others are global.
const (
	PlatformSuperAdmin   = "SUPER_ADMIN"
	PlatformAdmin        = "PLATFORM_ADMIN"
	PlatformMarketAdmin  = "MARKET_ADMIN"
	PlatformFinanceAdmin = "FINANCE_ADMIN"
	PlatformContentAdmin = "CONTENT_ADMIN"
	PlatformModerator    = "MODERATOR"
	PlatformSupport      = "SUPPORT"
)

// AllPlatformRoles returns every platform role.
func AllPlatformRoles() []string {
	return []string{
		PlatformSuperAdmin, PlatformAdmin, PlatformMarketAdmin, PlatformFinanceAdmin,
		PlatformContentAdmin, PlatformModerator, PlatformSupport,
	}
}

// IsPlatformRole reports whether role is a platform role.
func IsPlatformRole(role string) bool { return slices.Contains(AllPlatformRoles(), role) }

// AccountRolePrivateSeller is the self-declared "I sell my own property"
// account role. It is the only account role any route looks at, and only to
// let its holder open their (possibly empty) seller inbox.
const AccountRolePrivateSeller = "PRIVATE_SELLER"

// AccountRoleLandlord is the "I own property that I let" account role. Like
// the property seller, a landlord is an individual who can be the direct
// owner of listings, and the role is looked at for the same single purpose:
// to let its holder open their (possibly empty) inbox.
const AccountRoleLandlord = "LANDLORD"

// DirectOwnerRoles are the account roles whose holders own listings
// directly, without an organisation.
func DirectOwnerRoles() []string { return []string{AccountRolePrivateSeller, AccountRoleLandlord} }

// DeclaresDirectOwner reports whether the principal declared one of the
// direct-owner roles (in any status). See AccountRole: not an authorisation.
func (p *Principal) DeclaresDirectOwner() bool {
	for _, role := range DirectOwnerRoles() {
		if p.HasAccountRole(role) {
			return true
		}
	}
	return false
}

// AccountRole is a role the user chose for themselves at registration, with
// its status. It is carried on the Principal for display and for the one
// documented use above. It is NOT an authorisation: it must never select or
// widen the data a route returns. Use Memberships and PlatformRoles for that.
type AccountRole struct {
	Role   string
	Status string
}

// ErrNoSession is returned by a SessionResolver when the token is unknown,
// expired or revoked. The caller is then treated as anonymous.
var ErrNoSession = errors.New("no valid session")

// PlatformRole is one platform role assignment.
type PlatformRole struct {
	Role string
	// MarketCode is set only for market-scoped roles (MARKET_ADMIN).
	MarketCode string
}

// Membership is the caller's role in one organisation and market.
type Membership struct {
	OrganisationID   uuid.UUID
	OrganisationName string
	OrganisationKind string
	MarketCode       string
	Role             string
}

// Principal is the authenticated caller. Everyone is implicitly a consumer;
// extra capabilities come only from Memberships and PlatformRoles.
type Principal struct {
	UserID        uuid.UUID
	Name          string
	Email         string
	PlatformRoles []PlatformRole
	Memberships   []Membership
	// AccountRoles are self-declared and grant no access; see AccountRole.
	AccountRoles []AccountRole

	// SessionID identifies the session that authenticated this request and Via
	// says whether its token arrived in the cookie or as a bearer token.
	SessionID uuid.UUID
	Via       string
	// SessionScope is ScopeCustomer or ScopeAdmin; SessionExpiresAt is the
	// session's absolute expiry.
	SessionScope     string
	SessionExpiresAt time.Time
	// MFAEnrolmentOnly marks an admin-scope session that may only set up an
	// authenticator (ADMIN_MFA_REQUIRED is on and the administrator has none).
	// It grants no platform role: HasPlatformRole is false, and every
	// /v1/admin route outside /v1/admin/auth answers 403
	// MFA_ENROLMENT_REQUIRED (RefuseMFAEnrolmentOnly).
	MFAEnrolmentOnly bool
}

// HasMembership reports whether the principal holds at least one membership
// with one of roles (any membership when roles is empty).
func (p *Principal) HasMembership(roles ...string) bool {
	for _, m := range p.Memberships {
		if len(roles) == 0 || slices.Contains(roles, m.Role) {
			return true
		}
	}
	return false
}

// HasAccountRole reports whether the principal declared role, in any status.
// See AccountRole: this is not an authorisation check.
func (p *Principal) HasAccountRole(role string) bool {
	for _, r := range p.AccountRoles {
		if r.Role == role {
			return true
		}
	}
	return false
}

// OrganisationIDs returns the organisations in which the principal holds one
// of roles (every organisation when roles is empty). Handlers use it to scope
// queries: data of any other organisation must never be returned.
func (p *Principal) OrganisationIDs(roles ...string) []uuid.UUID {
	var ids []uuid.UUID
	for _, m := range p.Memberships {
		if (len(roles) == 0 || slices.Contains(roles, m.Role)) && !slices.Contains(ids, m.OrganisationID) {
			ids = append(ids, m.OrganisationID)
		}
	}
	return ids
}

// HasPlatformRole reports whether the principal may ACT with one of roles: it
// must hold the role (in any market; SUPER_ADMIN satisfies every check) AND be
// signed in through the administrator login (admin-scope session). A platform
// role on a customer-scope session grants nothing. Neither does an
// enrolment-only session (MFAEnrolmentOnly). Handlers acting on market
// data must additionally check MarketCode for MARKET_ADMIN.
func (p *Principal) HasPlatformRole(roles ...string) bool {
	if p.SessionScope != ScopeAdmin || p.MFAEnrolmentOnly {
		return false
	}
	for _, r := range p.PlatformRoles {
		if r.Role == PlatformSuperAdmin || slices.Contains(roles, r.Role) {
			return true
		}
	}
	return false
}

// SessionResolver turns a raw session token into a Principal. It returns
// ErrNoSession for an unknown, expired or revoked token.
type SessionResolver interface {
	ResolveSession(ctx context.Context, token string) (*Principal, error)
}

type principalKey struct{}

// WithPrincipal returns a context carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the authenticated caller, or nil for an anonymous request.
func FromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}
