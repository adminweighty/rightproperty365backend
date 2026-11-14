// Package identity implements accounts, email verification, password reset,
// sessions and the role model (organisation memberships and platform roles).
//
// Security rules that hold everywhere in this package:
//   - passwords are stored only as Argon2id hashes;
//   - session, verification and reset tokens are random 32-byte values handed
//     to the client once and stored only as SHA-256 digests;
//   - responses never reveal whether an email address has an account.
package identity

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// Lifetimes.
const (
	// SessionAbsoluteTTL is the hard limit on a session's life.
	SessionAbsoluteTTL = 30 * 24 * time.Hour
	// SessionIdleTTL ends a session that has not been used for this long.
	SessionIdleTTL = 14 * 24 * time.Hour
	// SessionTouchInterval bounds how often last_seen is written: a request
	// only refreshes it when the stored value is at least this old.
	SessionTouchInterval = time.Minute

	// Administrator sessions are deliberately short. Defaults; configurable
	// with ADMIN_SESSION_TTL and ADMIN_SESSION_IDLE_TTL.
	DefaultAdminSessionTTL     = 12 * time.Hour
	DefaultAdminSessionIdleTTL = 30 * time.Minute

	VerificationTokenTTL = 24 * time.Hour
	ResetTokenTTL        = time.Hour
)

// Token delivery modes for login.
const (
	DeliveryCookie = "cookie"
	DeliveryBearer = "bearer"
)

// User statuses (users.status).
const StatusActive = "ACTIVE"

// Account roles a person may choose for themselves at registration.
//
// An account role is a SELF-DECLARED statement of how someone intends to use
// the platform. It is not an authorisation: it creates no organisation and no
// membership, grants no access to any organisation's data, and is never
// consulted by auth.RequireMembership or auth.RequirePlatformRole. Platform
// roles (SUPER_ADMIN etc.) are a separate vocabulary and can never be chosen.
const (
	AccountRoleConsumer        = "CONSUMER"
	AccountRoleTenant          = "TENANT"
	AccountRolePrivateSeller   = auth.AccountRolePrivateSeller
	AccountRoleLandlord        = auth.AccountRoleLandlord
	AccountRoleSalesAgent      = "SALES_AGENT"
	AccountRoleLettingsAgent   = "LETTINGS_AGENT"
	AccountRolePropertyManager = "PROPERTY_MANAGER"
	AccountRoleSolicitor       = "SOLICITOR"
	AccountRoleHost            = "HOST"
	AccountRoleDeveloper       = "DEVELOPER"
	AccountRoleServiceProvider = "SERVICE_PROVIDER"
	AccountRoleAgent           = "AGENT"
	AccountRoleAgencyStaff     = "AGENCY_STAFF"
	AccountRoleAgencyAdmin     = "AGENCY_ADMIN"
	AccountRoleAgencyOwner     = "AGENCY_OWNER"
)

// Account role statuses. A professional role starts as PENDING_VERIFICATION
// and becomes ACTIVE or REJECTED when a platform administrator decides its
// onboarding application (internal/modules/onboarding). The database also
// allows REVOKED, which nothing sets yet.
const (
	AccountRoleActive              = "ACTIVE"
	AccountRolePendingVerification = "PENDING_VERIFICATION"
	AccountRoleRejected            = "REJECTED"
)

// SelectableRole is an account role that may be chosen at sign-up.
type SelectableRole struct {
	Code string
	// RequiresVerification: choosing it only records a request
	// (PENDING_VERIFICATION) instead of an ACTIVE role.
	RequiresVerification bool
}

// RoleCatalogue supplies the roles that may be chosen right now. In the
// running API it is backed by the reference data, so an administrator can
// deactivate a role without a release.
type RoleCatalogue interface {
	SelectableRoles(ctx context.Context) ([]SelectableRole, error)
}

// DefaultSelectableRoles is the built-in list, used when no catalogue is
// configured (unit tests) and as the baseline the reference data is seeded from.
func DefaultSelectableRoles() []SelectableRole {
	out := make([]SelectableRole, 0, len(selectableAccountRoles))
	for _, code := range selectableAccountRoles {
		out = append(out, SelectableRole{Code: code, RequiresVerification: initialAccountRoleStatus(code) == AccountRolePendingVerification})
	}
	return out
}

// selectableAccountRoles is the built-in allowlist and the order in which the
// API returns account roles.
//
// The order follows the groups of the sign-up form: property seekers, real
// estate professionals, real estate businesses, property owners, other
// property businesses.
var selectableAccountRoles = []string{
	AccountRoleConsumer, AccountRoleTenant,
	AccountRoleAgent, AccountRoleSalesAgent, AccountRoleLettingsAgent, AccountRolePropertyManager, AccountRoleSolicitor,
	AccountRoleAgencyOwner, AccountRoleAgencyAdmin, AccountRoleAgencyStaff,
	AccountRoleLandlord, AccountRolePrivateSeller,
	AccountRoleHost, AccountRoleDeveloper, AccountRoleServiceProvider,
}

// IsSeekerRole reports whether a role belongs to the Property Seekers group
// (Prospective Buyer, Prospective Tenant). An account that registers as a
// property seeker can hold no role from any other group, and the reverse.
func IsSeekerRole(role string) bool {
	return role == AccountRoleConsumer || role == AccountRoleTenant
}

// SelectableAccountRoles returns the registration allowlist in API order.
func SelectableAccountRoles() []string { return slices.Clone(selectableAccountRoles) }

// initialAccountRoleStatus is the status a self-selected role starts in:
// CONSUMER and TENANT need no vetting and are ACTIVE (the account still cannot
// sign in until its email is verified); every professional role is only a
// request and waits as PENDING_VERIFICATION.
func initialAccountRoleStatus(role string) string {
	if role == AccountRoleConsumer || role == AccountRoleTenant {
		return AccountRoleActive
	}
	return AccountRolePendingVerification
}

// AccountRole is one self-declared role and its status.
type AccountRole struct {
	Role   string
	Status string
	// OnboardingStatus is the status of the role's newest onboarding
	// application ("" when the role has none: CONSUMER, TENANT, and roles
	// granted before onboarding existed). Filled by the stores on reads.
	OnboardingStatus string
}

// sortAccountRoles orders roles as in the allowlist (unknown values last).
func sortAccountRoles(roles []AccountRole) {
	rank := func(role string) int {
		if i := slices.Index(selectableAccountRoles, role); i >= 0 {
			return i
		}
		return len(selectableAccountRoles)
	}
	sort.SliceStable(roles, func(i, j int) bool { return rank(roles[i].Role) < rank(roles[j].Role) })
}

// Outbox events written by this module, in the same transaction as the change
// they describe. The worker turns them into notification emails. Payloads
// carry identifiers and a timestamp only.
const (
	// EventUserEmailVerified: an email address was verified for the first time.
	EventUserEmailVerified = "USER_EMAIL_VERIFIED"
	// EventPasswordChanged: an account's password was replaced.
	EventPasswordChanged = "PASSWORD_CHANGED"
	// EventAdminSignedIn: an administrator session was started.
	EventAdminSignedIn = "ADMIN_SIGNED_IN"

	AggregateUser = "user"
)

// OutboxEvent is an event to store atomically with a change.
type OutboxEvent struct {
	Type       string
	UserID     uuid.UUID
	OccurredAt time.Time
}

// Payload is the JSON stored in the outbox: identifiers and a time, nothing personal.
func (e OutboxEvent) Payload() []byte {
	return fmt.Appendf(nil, `{"user_id":%q,"occurred_at":%q}`, e.UserID, e.OccurredAt.UTC().Format(time.RFC3339Nano))
}

// SortedAccountRoles returns roles in allowlist order.
func SortedAccountRoles(roles []AccountRole) []AccountRole {
	out := slices.Clone(roles)
	sortAccountRoles(out)
	return out
}

// Consent types recorded in user_consents.
const ConsentPrivacyNotice = "PRIVACY_NOTICE"

// Domain errors mapped to HTTP statuses by the handler.
var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrEmailNotVerified   = errors.New("email not verified")
	ErrInvalidToken       = errors.New("token is unknown, expired or already used")

	// ErrNotFound is returned by stores when a user or session does not exist.
	ErrNotFound = errors.New("not found")
	// ErrEmailTaken is returned by Store.CreateUser when the email is in use.
	ErrEmailTaken = errors.New("email already registered")
	// ErrMarketNotFound is returned when a market code does not exist.
	ErrMarketNotFound = errors.New("market not found")
	// ErrNotPlatformAdmin is returned when an administrator-only operation
	// names an account that holds no platform role.
	ErrNotPlatformAdmin = errors.New("account holds no platform role")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct {
	Fields map[string]string
}

func (e *ValidationError) Error() string { return "validation failed" }

// User is an account with its roles.
type User struct {
	ID              uuid.UUID
	Name            string
	Email           string // stored lower-cased
	Status          string
	PasswordHash    string // "" when the account has no password yet
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
	PlatformRoles   []auth.PlatformRole
	Memberships     []auth.Membership
	// AccountRoles are self-declared and informational only (see AccountRole*).
	AccountRoles []AccountRole
}

// Verified reports whether the email address has been verified.
func (u User) Verified() bool { return u.EmailVerifiedAt != nil }

// NewUser is everything a Store needs to create an account atomically with
// its registration consent record.
type NewUser struct {
	ID             uuid.UUID
	Name           string
	Email          string
	PasswordHash   string
	MarketingOptIn bool
	PrivacyVersion string
	// AccountRoles is the validated role selection with initial statuses.
	AccountRoles []AccountRole
	// EmailVerifiedAt is nil for normal registration (set by seeds only).
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
}

// NewPlatformAdmin is an administrator account created by cmd/adminctl (or a
// seed): already verified, holding exactly one platform role. Platform roles
// are never obtainable through the public API.
type NewPlatformAdmin struct {
	ID           uuid.UUID
	Name         string
	Email        string
	PasswordHash string
	Role         string
	MarketCode   string // required for MARKET_ADMIN, empty otherwise
	CreatedAt    time.Time
}

// Session is a server-side session. Only the SHA-256 of its token is stored.
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TokenHash []byte
	// Scope is auth.ScopeCustomer or auth.ScopeAdmin.
	Scope      string
	Delivery   string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time // absolute expiry
	RevokedAt  *time.Time
}

// UserView is the `user` object of the API (GET /v1/me, login, verify-email).
type UserView struct {
	ID            uuid.UUID          `json:"id"`
	Name          string             `json:"name"`
	Email         string             `json:"email"`
	EmailVerified bool               `json:"email_verified"`
	PlatformRoles []PlatformRoleView `json:"platform_roles"`
	Memberships   []MembershipView   `json:"memberships"`
	AccountRoles  []AccountRoleView  `json:"account_roles"`
}

// AccountRoleView is one self-declared account role. It conveys no access.
type AccountRoleView struct {
	Role   string `json:"role"`
	Status string `json:"status"`
	// OnboardingStatus is the status of the role's onboarding application:
	// DRAFT, SUBMITTED, CHANGES_REQUESTED, APPROVED or REJECTED; null for a
	// role that has no onboarding.
	OnboardingStatus *string `json:"onboarding_status"`
}

// PlatformRoleView is one platform role; market_code is null unless the role
// is market-scoped.
type PlatformRoleView struct {
	Role       string  `json:"role"`
	MarketCode *string `json:"market_code"`
}

// MembershipView is one organisation membership.
type MembershipView struct {
	OrganisationID   uuid.UUID `json:"organisation_id"`
	OrganisationName string    `json:"organisation_name"`
	OrganisationKind string    `json:"organisation_kind"`
	MarketCode       string    `json:"market_code"`
	Role             string    `json:"role"`
	// RoleLabel is how Role is shown: "Agency Principal", "Estate Agent", ...
	RoleLabel string `json:"role_label"`
}

// View builds the API representation. Arrays are always present (never null).
func (u User) View() UserView {
	v := UserView{
		ID: u.ID, Name: u.Name, Email: u.Email, EmailVerified: u.Verified(),
		PlatformRoles: make([]PlatformRoleView, 0, len(u.PlatformRoles)),
		Memberships:   make([]MembershipView, 0, len(u.Memberships)),
		AccountRoles:  make([]AccountRoleView, 0, len(u.AccountRoles)),
	}
	roles := slices.Clone(u.AccountRoles)
	sortAccountRoles(roles)
	for _, r := range roles {
		view := AccountRoleView{Role: r.Role, Status: r.Status}
		if r.OnboardingStatus != "" {
			status := r.OnboardingStatus
			view.OnboardingStatus = &status
		}
		v.AccountRoles = append(v.AccountRoles, view)
	}
	for _, r := range u.PlatformRoles {
		pr := PlatformRoleView{Role: r.Role}
		if r.MarketCode != "" {
			code := r.MarketCode
			pr.MarketCode = &code
		}
		v.PlatformRoles = append(v.PlatformRoles, pr)
	}
	for _, m := range u.Memberships {
		v.Memberships = append(v.Memberships, MembershipView{
			OrganisationID: m.OrganisationID, OrganisationName: m.OrganisationName,
			OrganisationKind: m.OrganisationKind, MarketCode: m.MarketCode, Role: m.Role,
			RoleLabel: auth.MembershipRoleLabel(m.OrganisationKind, m.Role),
		})
	}
	return v
}

// AuthResult is the outcome of a successful login or email verification.
type AuthResult struct {
	User      User
	Token     string // raw session token; handed to the client exactly once
	Delivery  string
	Scope     string
	ExpiresAt time.Time
	// TTL is the session's absolute lifetime (cookie Max-Age).
	TTL time.Duration
	// NoSession is true when the operation succeeded but deliberately did not
	// sign the caller in (email verification of a platform-role account).
	NoSession bool
}
