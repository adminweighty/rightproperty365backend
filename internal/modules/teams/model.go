// Package teams lets an organisation's OWNER and MANAGER run their own team:
// invite people with a role, see and decide the requests of people who asked
// to join (onboarding's join requests), change members' roles, suspend and
// remove members, and read the team's audit trail. Assigning a request to a
// member is in propertyrequests (assign.go).
//
// The rules (auth.GrantableRoles): an OWNER grants and acts on MANAGER, AGENT
// and STAFF; a MANAGER only on AGENT and STAFF; nobody grants OWNER, nobody
// changes or removes an OWNER, and nobody acts on themselves.
//
// Effect is immediate: authorisation reads the caller's ACTIVE memberships on
// every request (identity), so a suspended or removed member has no access
// from their next request on, with the same session.
package teams

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Invitation statuses.
const (
	InvitationPending   = "PENDING"
	InvitationAccepted  = "ACCEPTED"
	InvitationCancelled = "CANCELLED"
	// InvitationReplaced: the same address was invited again; only the
	// newest link works.
	InvitationReplaced = "REPLACED"
)

// Membership statuses a PATCH may set, and the one a removal sets.
const (
	MemberActive    = "ACTIVE"
	MemberSuspended = "SUSPENDED"
	MemberRemoved   = "REMOVED"
)

// Limits.
const (
	// InvitationTTL is how long an invitation link works.
	InvitationTTL    = 7 * 24 * time.Hour
	MaxJobTitleRunes = 80
	DefaultPageSize  = 20
	MaxPageSize      = 100
)

// Error codes of this module's endpoints.
const (
	CodeOrganisationNotFound    = "ORGANISATION_NOT_FOUND"
	CodeMemberNotFound          = "MEMBER_NOT_FOUND"
	CodeRoleNotAllowed          = "ROLE_NOT_ALLOWED"
	CodeOwnerProtected          = "OWNER_PROTECTED"
	CodeSelfChangeForbidden     = "SELF_CHANGE_FORBIDDEN"
	CodeLastOwner               = "LAST_OWNER"
	CodeAlreadyAMember          = "ALREADY_A_MEMBER"
	CodeInvitationNotFound      = "INVITATION_NOT_FOUND"
	CodeInvitationExpired       = "INVITATION_EXPIRED"
	CodeInvitationNoLongerValid = "INVITATION_NO_LONGER_VALID"
	CodeInvitationEmailMismatch = "INVITATION_EMAIL_MISMATCH"
	CodeSeekerAccount           = "SEEKER_ACCOUNT_CANNOT_JOIN"
	CodeJoinRequestNotFound     = "JOIN_REQUEST_NOT_FOUND"
	CodeJoinRequestDecided      = "JOIN_REQUEST_ALREADY_DECIDED"
	CodeAgencyNotVerified       = "AGENCY_NOT_VERIFIED"
)

// Errors, mapped to HTTP by the handler.
var (
	ErrOrganisationNotFound = errors.New("organisation not found")
	ErrForbidden            = errors.New("not allowed to manage this team")
	ErrMemberNotFound       = errors.New("member not found")
	ErrRoleNotAllowed       = errors.New("the caller may not give or act on that role")
	ErrOwnerProtected       = errors.New("an owner cannot be changed or removed")
	ErrSelfChange           = errors.New("nobody changes their own membership")
	ErrLastOwner            = errors.New("the last owner cannot be removed")
	ErrAlreadyMember        = errors.New("already a member")
	ErrInvitationNotFound   = errors.New("invitation not found")
	ErrInvitationExpired    = errors.New("invitation expired")
	ErrInvitationUsed       = errors.New("invitation already accepted, cancelled or replaced")
	ErrEmailMismatch        = errors.New("the invitation is for another email address")
	ErrSeekerAccount        = errors.New("a property seeker account cannot join an organisation")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Actor is who makes a change.
type Actor struct {
	UserID uuid.UUID
	Name   string
}

// Member is one membership that was not removed.
type Member struct {
	UserID     uuid.UUID
	Name       string
	Email      string
	Role       string
	MarketCode string
	// Status is the membership's: ACTIVE or SUSPENDED.
	Status string
	// AccountActive is false when the person's account itself is not active.
	AccountActive bool
	JobTitle      string
	JoinedAt      *time.Time
}

// EffectiveStatus is SUSPENDED when the membership is suspended or the
// account is not active, otherwise ACTIVE.
func (m Member) EffectiveStatus() string {
	if m.Status == MemberSuspended || !m.AccountActive {
		return MemberSuspended
	}
	return MemberActive
}

// Invitation is an invitation to join an organisation with a role.
type Invitation struct {
	ID               uuid.UUID
	OrganisationID   uuid.UUID
	OrganisationName string
	OrganisationKind string
	Email            string // lower-cased
	Role             string
	JobTitle         string
	Status           string
	// TokenHash is the SHA-256 of the emailed token (never the token).
	TokenHash     []byte
	InvitedBy     *uuid.UUID
	InvitedByName string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	AcceptedAt    *time.Time
	CancelledAt   *time.Time
}

// Expired reports whether a pending invitation's link stopped working.
func (i Invitation) Expired(now time.Time) bool { return !now.Before(i.ExpiresAt) }

// Account is the signed-in person accepting an invitation.
type Account struct {
	ID    uuid.UUID
	Name  string
	Email string // lower-cased
}

// AccountState is what the store knows about the accepting account inside
// the acceptance's critical section.
type AccountState struct {
	// Seeker: the account holds a property seeker role (CONSUMER, TENANT).
	Seeker bool
	// Membership is the account's membership of the organisation that was
	// not removed (nil when there is none).
	Membership *Member
}

// Acceptance is everything accepting an invitation does, applied by the
// store in ONE critical section (one transaction in Postgres): the
// invitation becomes ACCEPTED, the membership ACTIVE (created, or a removed
// one reinstated), the account role ACTIVE, an agent profile is created for
// an agency's AGENT, and the audit record is written.
type Acceptance struct {
	TokenHash []byte
	Account   Account
	At        time.Time
	// Check is called with the locked invitation and the account's state;
	// an error stops everything and is returned as is.
	Check func(inv Invitation, state AccountState) error
	// Plan decides, from the locked invitation, what the acceptance creates.
	Plan func(inv Invitation) AcceptPlan
}

// AcceptPlan is what accepting one invitation creates.
type AcceptPlan struct {
	// AccountRole becomes ACTIVE for the account ("" for none).
	AccountRole string
	// AgentProfile: create a public agent profile linked to the agency (when
	// the account has none for it), with ProfileTitle as its role title.
	AgentProfile bool
	ProfileTitle string
	Audit        orgaudit.Event
}

// ProfileRef is a public profile address.
type ProfileRef struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
}

// Accepted is the outcome of an acceptance.
type Accepted struct {
	Invitation Invitation
	// Profile is the agent profile the acceptance created or reactivated.
	Profile *ProfileRef
}

// Release says which open work a member change gives back to the
// organisation inbox.
type Release int

// Release values.
const (
	ReleaseNothing Release = iota
	// ReleaseRequests: the open requests assigned to the member (a member
	// who can no longer hold leads).
	ReleaseRequests
	// ReleaseAll: the open requests and appointments (removed or suspended).
	ReleaseAll
)

// MemberChange is what a change to one membership does. Nil fields stay.
type MemberChange struct {
	Role     *string
	Status   *string
	JobTitle *string // "" clears it
	Remove   bool
	Release  Release
	Actor    Actor
	At       time.Time
	// Events are the audit records. The store adds the released counts
	// (released_requests, released_appointments) to the After of each.
	Events []orgaudit.Event
}

// Released counts the work a change gave back to the organisation.
type Released struct {
	Requests     int
	Appointments int
}

// Store persists the team.
type Store interface {
	// Members returns the organisation's memberships that were not removed.
	Members(ctx context.Context, organisationID uuid.UUID) ([]Member, error)
	// MemberByEmail returns the membership (not removed) of the account with
	// that email address, or nil.
	MemberByEmail(ctx context.Context, organisationID uuid.UUID, email string) (*Member, error)
	// OpenLeads counts, per member, the organisation's open requests
	// assigned to them.
	OpenLeads(ctx context.Context, organisationID uuid.UUID) (map[uuid.UUID]int, error)

	// CreateInvitation stores a PENDING invitation and its audit record. A
	// pending invitation of the same organisation and address becomes
	// REPLACED in the same transaction.
	CreateInvitation(ctx context.Context, inv Invitation, audit orgaudit.Event) error
	// Invitations returns the organisation's PENDING invitations, newest first.
	Invitations(ctx context.Context, organisationID uuid.UUID) ([]Invitation, error)
	// Invitation returns one invitation of the organisation, or ErrInvitationNotFound.
	Invitation(ctx context.Context, organisationID, id uuid.UUID) (Invitation, error)
	// InvitationByToken returns the invitation with that token hash (any
	// status), or ErrInvitationNotFound.
	InvitationByToken(ctx context.Context, tokenHash []byte) (Invitation, error)
	// CancelInvitation marks a PENDING invitation CANCELLED after allow
	// accepted it, with its audit record. ErrInvitationUsed when it is no
	// longer pending.
	CancelInvitation(ctx context.Context, organisationID, id uuid.UUID, by Actor, at time.Time,
		allow func(Invitation) error, audit func(Invitation) orgaudit.Event) (Invitation, error)
	// AcceptInvitation applies an acceptance (see Acceptance).
	// ErrInvitationNotFound for an unknown token.
	AcceptInvitation(ctx context.Context, a Acceptance) (Accepted, error)

	// ChangeMember changes one membership atomically: it reads the member
	// (and the number of ACTIVE owners) under a lock, calls decide, and
	// applies what it returns, the release of open work and the audit
	// records together. ErrMemberNotFound when there is no such membership
	// (or it was removed).
	ChangeMember(ctx context.Context, organisationID, userID uuid.UUID, decide func(target Member, activeOwners int) (MemberChange, error)) (Member, Released, error)

	// Audit returns one page of the organisation's audit trail, newest first.
	Audit(ctx context.Context, organisationID uuid.UUID, limit, offset int) ([]orgaudit.Event, int, error)
}

// InvitationMail is what the invitation email needs. Token is the secret
// part of the link: it is only rendered into the message.
type InvitationMail struct {
	Token            string
	OrganisationName string
	RoleLabel        string
	JobTitle         string
	InviterName      string
	ExpiresAt        time.Time
}

// Mailer sends the invitation email from the request path (the link is a
// credential and never goes through the outbox). Sending to an address on a
// reserved test domain is skipped by the mail service, as for every email.
type Mailer interface {
	SendInvitation(ctx context.Context, to string, m InvitationMail) error
}
