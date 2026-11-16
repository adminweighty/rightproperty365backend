// Package accounts is the platform administrators' view of the people on
// the platform: the user list and search, a user's detail, suspension and
// reactivation, and deleting an account — by an administrator or by the
// person themselves (DELETE /v1/me).
//
// Suspension blocks the account at once: every session is revoked, sign-in
// codes and reset links stop working, and identity refuses an account that
// is not ACTIVE at sign-in and on every request. Nothing is unpublished: a
// direct owner's listings stay PUBLISHED but leave the public catalogue
// while the account is suspended, because the public rule already requires
// the owner's account to be ACTIVE (listings/account_admin.go). An
// organisation's listings are the organisation's and stay public.
//
// Deletion anonymises the account in place (the row stays so that audit
// records, signed agreements and leases keep their references), ends every
// session, removes the person from every team (their open requests and
// appointments go back to the organisation), withdraws (archives) the
// listings they own directly, removes them as the assigned agent of
// organisation listings, hides their agent profile, cancels their own
// subscriptions and deletes their saved signature. It is refused while they
// are the only owner of an organisation that has other members.
package accounts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Account statuses (users.status).
const (
	StatusActive    = "ACTIVE"
	StatusSuspended = "SUSPENDED"
	StatusDeleted   = "DELETED"
)

// Outbox events (aggregate "user"), turned into emails by the worker. Their
// payload is identity's: the user ID and a time.
const (
	EventAccountSuspended   = "ACCOUNT_SUSPENDED"
	EventAccountReactivated = "ACCOUNT_REACTIVATED"
)

// EventTypes lists the events of this module.
func EventTypes() []string { return []string{EventAccountSuspended, EventAccountReactivated} }

// Limits.
const (
	MaxReasonRunes  = 1000
	MinReasonRunes  = 3
	DefaultPageSize = 20
	MaxPageSize     = 100
	// DeletedName is the name an anonymised account keeps.
	DeletedName = "Deleted account"
	// DeletedEmailDomain is the (reserved, never deliverable) domain of an
	// anonymised account's address: deleted-<id>@deleted.invalid.
	DeletedEmailDomain = "deleted.invalid"
)

// DeletedEmail is the placeholder address of a deleted account.
func DeletedEmail(id uuid.UUID) string { return "deleted-" + id.String() + "@" + DeletedEmailDomain }

// Error codes of this module's endpoints.
const (
	CodeUserNotFound          = "USER_NOT_FOUND"
	CodeUserState             = "USER_STATE_CONFLICT"
	CodeSelfActionForbidden   = "SELF_ACTION_FORBIDDEN"
	CodeAdminAccountProtected = "ADMIN_ACCOUNT_PROTECTED"
	CodeSoleOwner             = "SOLE_OWNER"
	CodeInvalidPassword       = "INVALID_PASSWORD"
	CodeAdminSelfDelete       = "ADMIN_ACCOUNT_CANNOT_SELF_DELETE"
)

// Errors, mapped to HTTP by the handler.
var (
	ErrNotFound        = errors.New("user not found")
	ErrState           = errors.New("the account's status does not allow this")
	ErrSelf            = errors.New("administrators cannot suspend or delete their own account")
	ErrAdminProtected  = errors.New("only a super administrator may act on an administrator account")
	ErrInvalidPassword = errors.New("the password is wrong")
	ErrAdminSelfDelete = errors.New("an administrator account cannot delete itself")
)

// SoleOwnerError: the person is the only active owner of organisations that
// have other members. Another owner must be named, or the members removed,
// first.
type SoleOwnerError struct{ Organisations []string }

func (e *SoleOwnerError) Error() string {
	return fmt.Sprintf("sole owner of %s", strings.Join(e.Organisations, ", "))
}

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Actor is who makes a change.
type Actor struct {
	UserID uuid.UUID
	Name   string
}

// Filter selects users. Empty fields match everything.
type Filter struct {
	// Q matches the name or email address (case-insensitive substring).
	Q string
	// Role is an account role (AGENT, LANDLORD, ...) in any status, or a
	// platform role (SUPER_ADMIN, ...).
	Role string
	// Status is ACTIVE, SUSPENDED or DELETED.
	Status string
	// Market is a market code: accounts with a membership in that market.
	Market        string
	Limit, Offset int
}

// Membership is one membership in any status.
type Membership struct {
	OrganisationID   uuid.UUID
	OrganisationName string
	OrganisationKind string
	MarketCode       string
	Role             string
	Status           string
}

// AccountRole is a self-declared role with its status and onboarding state.
type AccountRole struct {
	Role             string
	Status           string
	OnboardingStatus string
}

// Summary is one row of the user list.
type Summary struct {
	ID            uuid.UUID
	Name          string
	Email         string
	Status        string
	EmailVerified bool
	CreatedAt     time.Time
	LastSignInAt  *time.Time
	AccountRoles  []string
	PlatformRoles []string
	MarketCodes   []string
}

// Account is a user's detail.
type Account struct {
	ID               uuid.UUID
	Name             string
	Email            string
	Status           string
	EmailVerified    bool
	CreatedAt        time.Time
	LastSignInAt     *time.Time
	SuspendedAt      *time.Time
	SuspensionReason string
	DeletedAt        *time.Time
	PlatformRoles    []auth.PlatformRole
	AccountRoles     []AccountRole
	Memberships      []Membership
	// ListingCounts counts, by status, the listings the user owns directly
	// or is the assigned agent of.
	ListingCounts map[string]int
	// PasswordHash is used by self-service deletion only; never in a view.
	PasswordHash string
}

// Administrator reports whether the account holds a platform role.
func (a Account) Administrator() bool { return len(a.PlatformRoles) > 0 }

// Suspension is what suspending an account writes, in one transaction.
type Suspension struct {
	UserID uuid.UUID
	Reason string
	Actor  Actor
	At     time.Time
	Audit  adminaudit.Event
}

// Reactivation is what reactivating an account writes.
type Reactivation struct {
	UserID uuid.UUID
	Actor  Actor
	At     time.Time
	Audit  adminaudit.Event
}

// Deletion is what deleting an account writes, in one transaction.
type Deletion struct {
	UserID uuid.UUID
	Actor  Actor
	// SelfService: the person deleted their own account.
	SelfService bool
	Reason      string
	At          time.Time
	Audit       adminaudit.Event
	// TeamAudit builds the MEMBER_REMOVED record of each organisation the
	// person leaves.
	TeamAudit func(organisationID uuid.UUID, released map[string]any) orgaudit.Event
}

// Deleted says what a deletion did.
type Deleted struct {
	SessionsRevoked       int
	OrganisationsLeft     int
	ListingsWithdrawn     int
	ListingsUnassigned    int
	SubscriptionsCanceled int
}

// Store persists account administration.
type Store interface {
	// Search returns one page of accounts, newest first, and the total.
	Search(ctx context.Context, f Filter) ([]Summary, int, error)
	// Account returns one account, or ErrNotFound.
	Account(ctx context.Context, id uuid.UUID) (Account, error)
	// Suspend makes an ACTIVE account SUSPENDED (ErrState otherwise), ends
	// every session and sign-in, refreshes the owner's listings, and writes
	// the audit record and the ACCOUNT_SUSPENDED event.
	Suspend(ctx context.Context, s Suspension) (Account, error)
	// Reactivate makes a SUSPENDED account ACTIVE (ErrState otherwise).
	Reactivate(ctx context.Context, r Reactivation) (Account, error)
	// Delete anonymises an account that is not DELETED (ErrState
	// otherwise), as the package documentation describes. *SoleOwnerError
	// when the person is the only owner of an organisation with other
	// members; nothing is changed then.
	Delete(ctx context.Context, d Deletion) (Deleted, error)
}

// Mailer sends the deletion notice from the request path: once the account
// is anonymised there is no address left to read at send time.
type Mailer interface {
	SendAccountDeleted(ctx context.Context, to, name string) error
}
