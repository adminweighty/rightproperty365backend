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

	"github.com/google/uuid"
)

// SessionCookieName is the web session cookie.
const SessionCookieName = "rp_session"

// How the session token reached the API.
const (
	ViaCookie = "cookie"
	ViaBearer = "bearer"
)

// Organisation kinds.
const (
	KindAgency          = "AGENCY"
	KindLandlord        = "LANDLORD"
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

	// SessionID identifies the session that authenticated this request and Via
	// says whether its token arrived in the cookie or as a bearer token.
	SessionID uuid.UUID
	Via       string
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

// HasPlatformRole reports whether the principal holds one of roles in any
// market. SUPER_ADMIN satisfies every platform-role check. Handlers acting on
// market data must additionally check MarketCode for MARKET_ADMIN.
func (p *Principal) HasPlatformRole(roles ...string) bool {
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
