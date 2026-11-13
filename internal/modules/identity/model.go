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
	"errors"
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
	PlatformRoles   []auth.PlatformRole
	Memberships     []auth.Membership
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
	// EmailVerifiedAt is nil for normal registration (set by seeds only).
	EmailVerifiedAt *time.Time
	CreatedAt       time.Time
}

// Session is a server-side session. Only the SHA-256 of its token is stored.
type Session struct {
	ID         uuid.UUID
	UserID     uuid.UUID
	TokenHash  []byte
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
}

// View builds the API representation. Arrays are always present (never null).
func (u User) View() UserView {
	v := UserView{
		ID: u.ID, Name: u.Name, Email: u.Email, EmailVerified: u.Verified(),
		PlatformRoles: make([]PlatformRoleView, 0, len(u.PlatformRoles)),
		Memberships:   make([]MembershipView, 0, len(u.Memberships)),
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
		})
	}
	return v
}

// AuthResult is the outcome of a successful login or email verification.
type AuthResult struct {
	User      User
	Token     string // raw session token; handed to the client exactly once
	Delivery  string
	ExpiresAt time.Time
}
