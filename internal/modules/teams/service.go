package teams

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Service implements the team use cases.
type Service struct {
	store  Store
	mailer Mailer
	logger *slog.Logger
	now    func() time.Time
}

// NewService builds a Service. mailer may be nil (no invitation email is sent).
func NewService(store Store, mailer Mailer, logger *slog.Logger) *Service {
	return &Service{store: store, mailer: mailer, logger: logger, now: time.Now}
}

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// audit writes one log-only audit line (audit=true); the durable record is
// the organisation audit event written with the change.
func (s *Service) audit(ctx context.Context, event string, attrs ...slog.Attr) {
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx))}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

// Team returns the caller's membership of an organisation whose team they
// manage: ErrOrganisationNotFound when the caller is not an active member of
// it (the same as for one that does not exist), ErrForbidden when they are
// but may not manage its team (AGENT, STAFF).
func Team(p *auth.Principal, organisationID uuid.UUID) (auth.Membership, error) {
	for _, m := range p.Memberships {
		if m.OrganisationID == organisationID {
			if !p.MayManageTeam(organisationID) {
				return auth.Membership{}, ErrForbidden
			}
			return m, nil
		}
	}
	return auth.Membership{}, ErrOrganisationNotFound
}

// ---------------------------------------------------------------------------
// Members
// ---------------------------------------------------------------------------

// MemberView is one person in the team list. No email address or phone.
type MemberView struct {
	UserID      uuid.UUID  `json:"user_id"`
	Name        string     `json:"name"`
	Role        string     `json:"role"`
	RoleLabel   string     `json:"role_label"`
	JobTitle    *string    `json:"job_title"`
	Status      string     `json:"status"`
	StatusLabel string     `json:"status_label"`
	MarketCode  string     `json:"market_code"`
	JoinedAt    *time.Time `json:"joined_at"`
	// OpenLeads is the number of open requests assigned to the member.
	OpenLeads int  `json:"open_leads"`
	IsYou     bool `json:"is_you"`
	// CanChange: the caller may change this member's role or status, or
	// remove them. AllowedRoles are the roles the caller may give them.
	CanChange    bool     `json:"can_change"`
	AllowedRoles []string `json:"allowed_roles"`
	// CanHoldLeads: requests can be assigned to this member (an ACTIVE
	// OWNER, MANAGER or AGENT).
	CanHoldLeads bool `json:"can_hold_leads"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// actable says whether the caller may change or remove target, and if not why.
func actable(p *auth.Principal, organisationID uuid.UUID, target Member, activeOwners int) error {
	switch {
	case target.UserID == p.UserID:
		return ErrSelfChange
	case target.Role == auth.RoleOwner && activeOwners <= 1:
		return ErrLastOwner
	case target.Role == auth.RoleOwner:
		return ErrOwnerProtected
	case !p.MayGrantRole(organisationID, target.Role):
		return ErrRoleNotAllowed
	}
	return nil
}

func (s *Service) memberView(p *auth.Principal, team auth.Membership, m Member, leads map[uuid.UUID]int, owners int) MemberView {
	status := m.EffectiveStatus()
	v := MemberView{
		UserID: m.UserID, Name: m.Name, Role: m.Role, RoleLabel: auth.MembershipRoleLabel(team.OrganisationKind, m.Role),
		JobTitle: optional(m.JobTitle), Status: status, StatusLabel: identity.MemberStatusLabel(status), MarketCode: m.MarketCode,
		JoinedAt: m.JoinedAt, OpenLeads: leads[m.UserID], IsYou: m.UserID == p.UserID, AllowedRoles: []string{},
		CanHoldLeads: status == MemberActive && auth.RoleHoldsLeads(m.Role),
	}
	if actable(p, team.OrganisationID, m, owners) == nil {
		v.CanChange = true
		v.AllowedRoles = append(v.AllowedRoles, auth.GrantableRoles(team.Role)...)
	}
	return v
}

func activeOwners(members []Member) int {
	n := 0
	for _, m := range members {
		if m.Role == auth.RoleOwner && m.EffectiveStatus() == MemberActive {
			n++
		}
	}
	return n
}

// Members returns the team, ordered by role (OWNER first) then name.
func (s *Service) Members(ctx context.Context, p *auth.Principal, organisationID uuid.UUID) (auth.Membership, []MemberView, error) {
	team, err := Team(p, organisationID)
	if err != nil {
		return auth.Membership{}, nil, err
	}
	members, err := s.store.Members(ctx, organisationID)
	if err != nil {
		return auth.Membership{}, nil, err
	}
	leads, err := s.store.OpenLeads(ctx, organisationID)
	if err != nil {
		return auth.Membership{}, nil, err
	}
	owners := activeOwners(members)
	sortMembers(members)
	out := make([]MemberView, 0, len(members))
	for _, m := range members {
		out = append(out, s.memberView(p, team, m, leads, owners))
	}
	return team, out, nil
}

var roleRank = map[string]int{auth.RoleOwner: 0, auth.RoleManager: 1, auth.RoleAgent: 2, auth.RoleStaff: 3}

func sortMembers(members []Member) {
	slices.SortStableFunc(members, func(a, b Member) int {
		if ra, rb := roleRank[a.Role], roleRank[b.Role]; ra != rb {
			return ra - rb
		}
		return strings.Compare(a.Name, b.Name)
	})
}

// MemberPatch is the body of PATCH .../members/{userId}. Nil fields stay.
type MemberPatch struct {
	Role     *string `json:"role"`
	Status   *string `json:"status"`
	JobTitle *string `json:"job_title"`
}

// cleanJobTitle trims a job title: plain text, one line, at most 80
// characters ("" clears it).
func cleanJobTitle(raw string) (string, string) {
	v := strings.TrimSpace(raw)
	switch {
	case !utf8.ValidString(v):
		return "", "contains characters that are not allowed"
	case utf8.RuneCountInString(v) > MaxJobTitleRunes:
		return "", fmt.Sprintf("must be at most %d characters", MaxJobTitleRunes)
	case strings.ContainsAny(v, "<>"):
		return "", "must be plain text"
	}
	for _, r := range v {
		if unicode.IsControl(r) {
			return "", "contains characters that are not allowed"
		}
	}
	return v, ""
}

func memberRoles() []string {
	return []string{auth.RoleOwner, auth.RoleManager, auth.RoleAgent, auth.RoleStaff}
}

// ChangeMember changes a member's role, status (ACTIVE or SUSPENDED) or job
// title. A suspended member loses access at once and their open requests
// and appointments go back to the organisation inbox; a member who becomes
// STAFF gives back their open requests.
func (s *Service) ChangeMember(ctx context.Context, p *auth.Principal, organisationID, userID uuid.UUID, in MemberPatch) (MemberView, error) {
	team, err := Team(p, organisationID)
	if err != nil {
		return MemberView{}, err
	}
	fields := map[string]string{}
	var title *string
	if in.Role != nil && !slices.Contains(memberRoles(), *in.Role) {
		fields["role"] = "must be one of MANAGER, AGENT, STAFF"
	}
	if in.Status != nil && *in.Status != MemberActive && *in.Status != MemberSuspended {
		fields["status"] = "must be ACTIVE or SUSPENDED"
	}
	if in.JobTitle != nil {
		v, problem := cleanJobTitle(*in.JobTitle)
		if problem != "" {
			fields["job_title"] = problem
		}
		title = &v
	}
	if in.Role == nil && in.Status == nil && in.JobTitle == nil {
		fields["role"] = "send at least one of role, status, job_title"
	}
	if len(fields) > 0 {
		return MemberView{}, &ValidationError{Fields: fields}
	}
	actor := Actor{UserID: p.UserID, Name: p.Name}
	now := s.clock()
	member, released, err := s.store.ChangeMember(ctx, organisationID, userID, func(target Member, owners int) (MemberChange, error) {
		if err := actable(p, organisationID, target, owners); err != nil {
			return MemberChange{}, err
		}
		change := MemberChange{Actor: actor, At: now}
		event := func(action string, before, after map[string]any) {
			change.Events = append(change.Events, orgaudit.New(organisationID, actor.UserID, actor.Name, action,
				orgaudit.TargetMember, target.UserID, target.Name, before, after, now))
		}
		if in.Role != nil && *in.Role != target.Role {
			if !p.MayGrantRole(organisationID, *in.Role) {
				return MemberChange{}, ErrRoleNotAllowed
			}
			change.Role = in.Role
			event(orgaudit.ActionMemberRoleChanged, map[string]any{"role": target.Role}, map[string]any{"role": *in.Role})
			if !auth.RoleHoldsLeads(*in.Role) {
				change.Release = ReleaseRequests
			}
		}
		if in.Status != nil && *in.Status != target.Status {
			change.Status = in.Status
			if *in.Status == MemberSuspended {
				change.Release = ReleaseAll
				event(orgaudit.ActionMemberSuspended, map[string]any{"status": target.Status}, map[string]any{"status": MemberSuspended})
			} else {
				event(orgaudit.ActionMemberReactivated, map[string]any{"status": target.Status}, map[string]any{"status": MemberActive})
			}
		}
		if title != nil && *title != target.JobTitle {
			change.JobTitle = title
			event(orgaudit.ActionMemberJobTitleChanged, map[string]any{"job_title": optional(target.JobTitle)}, map[string]any{"job_title": optional(*title)})
		}
		return change, nil
	})
	if err != nil {
		return MemberView{}, err
	}
	s.audit(ctx, "team.member_changed", slog.String("organisation_id", organisationID.String()), slog.String("member_user_id", userID.String()),
		slog.String("actor_user_id", p.UserID.String()), slog.String("role", member.Role), slog.String("status", member.Status),
		slog.Int("released_requests", released.Requests), slog.Int("released_appointments", released.Appointments))
	members, err := s.store.Members(ctx, organisationID)
	if err != nil {
		return MemberView{}, err
	}
	leads, err := s.store.OpenLeads(ctx, organisationID)
	if err != nil {
		return MemberView{}, err
	}
	return s.memberView(p, team, member, leads, activeOwners(members)), nil
}

// RemoveMember removes a member: the membership becomes REMOVED (no longer
// listed, no access from the next request on) and their open requests and
// appointments go back to the organisation inbox.
func (s *Service) RemoveMember(ctx context.Context, p *auth.Principal, organisationID, userID uuid.UUID) (Released, error) {
	if _, err := Team(p, organisationID); err != nil {
		return Released{}, err
	}
	actor := Actor{UserID: p.UserID, Name: p.Name}
	now := s.clock()
	_, released, err := s.store.ChangeMember(ctx, organisationID, userID, func(target Member, owners int) (MemberChange, error) {
		if err := actable(p, organisationID, target, owners); err != nil {
			return MemberChange{}, err
		}
		return MemberChange{Remove: true, Release: ReleaseAll, Actor: actor, At: now, Events: []orgaudit.Event{
			orgaudit.New(organisationID, actor.UserID, actor.Name, orgaudit.ActionMemberRemoved, orgaudit.TargetMember, target.UserID, target.Name,
				map[string]any{"role": target.Role, "status": target.Status}, map[string]any{"status": MemberRemoved}, now),
		}}, nil
	})
	if err != nil {
		return Released{}, err
	}
	s.audit(ctx, "team.member_removed", slog.String("organisation_id", organisationID.String()), slog.String("member_user_id", userID.String()),
		slog.String("actor_user_id", p.UserID.String()), slog.Int("released_requests", released.Requests), slog.Int("released_appointments", released.Appointments))
	return released, nil
}

// ---------------------------------------------------------------------------
// Invitations
// ---------------------------------------------------------------------------

// InvitationView is a pending invitation as the team's managers see it.
type InvitationView struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	RoleLabel string    `json:"role_label"`
	JobTitle  *string   `json:"job_title"`
	Status    string    `json:"status"`
	InvitedBy struct {
		UserID *uuid.UUID `json:"user_id"`
		Name   string     `json:"name"`
	} `json:"invited_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Expired: the link no longer works; invite again to send a new one.
	Expired bool `json:"expired"`
	// CanCancel: the caller may cancel it (their role may give its role).
	CanCancel bool `json:"can_cancel"`
}

func (s *Service) invitationView(p *auth.Principal, team auth.Membership, inv Invitation) InvitationView {
	v := InvitationView{
		ID: inv.ID, Email: inv.Email, Role: inv.Role, RoleLabel: auth.MembershipRoleLabel(team.OrganisationKind, inv.Role),
		JobTitle: optional(inv.JobTitle), Status: inv.Status, CreatedAt: inv.CreatedAt, ExpiresAt: inv.ExpiresAt,
		Expired: inv.Expired(s.clock()), CanCancel: inv.Status == InvitationPending && p.MayGrantRole(team.OrganisationID, inv.Role),
	}
	v.InvitedBy.UserID, v.InvitedBy.Name = inv.InvitedBy, inv.InvitedByName
	return v
}

// InviteInput is the body of POST .../invitations.
type InviteInput struct {
	Email    string  `json:"email"`
	Role     string  `json:"role"`
	JobTitle *string `json:"job_title"`
}

var emailRe = regexp.MustCompile(`^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>".]+$`)

// newToken returns a random 32-byte URL-safe token and its SHA-256.
func newToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("generate invitation token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	return token, hashToken(token), nil
}

func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// plausibleToken rejects anything that cannot be one of our tokens before a
// lookup: 43 characters of the URL-safe base64 alphabet.
func plausibleToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	for _, r := range token {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// Invite invites someone to the team with a role. An OWNER may invite a
// MANAGER, AGENT or STAFF, a MANAGER an AGENT or STAFF (ErrRoleNotAllowed
// otherwise; nobody invites an OWNER). Someone who already belongs to the
// team (active or suspended) is ErrAlreadyMember. Inviting an address that
// has a pending invitation replaces it: only the new link works. The email
// is sent from here (its link is a credential); a failed send is logged,
// and inviting again sends a new link.
func (s *Service) Invite(ctx context.Context, p *auth.Principal, organisationID uuid.UUID, in InviteInput) (InvitationView, error) {
	team, err := Team(p, organisationID)
	if err != nil {
		return InvitationView{}, err
	}
	fields := map[string]string{}
	email := identity.NormalizeEmail(in.Email)
	if email == "" {
		fields["email"] = "is required"
	} else if len(email) > 254 || !emailRe.MatchString(email) {
		fields["email"] = "must be a valid email address"
	}
	if !slices.Contains(memberRoles(), in.Role) {
		fields["role"] = "must be one of MANAGER, AGENT, STAFF"
	}
	title := ""
	if in.JobTitle != nil {
		var problem string
		if title, problem = cleanJobTitle(*in.JobTitle); problem != "" {
			fields["job_title"] = problem
		}
	}
	if len(fields) > 0 {
		return InvitationView{}, &ValidationError{Fields: fields}
	}
	if !p.MayGrantRole(organisationID, in.Role) {
		return InvitationView{}, ErrRoleNotAllowed
	}
	existing, err := s.store.MemberByEmail(ctx, organisationID, email)
	if err != nil {
		return InvitationView{}, err
	}
	if existing != nil {
		return InvitationView{}, ErrAlreadyMember
	}
	token, hash, err := newToken()
	if err != nil {
		return InvitationView{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return InvitationView{}, err
	}
	now := s.clock()
	inviter := p.UserID
	inv := Invitation{
		ID: id, OrganisationID: organisationID, OrganisationName: team.OrganisationName, OrganisationKind: team.OrganisationKind,
		Email: email, Role: in.Role, JobTitle: title, Status: InvitationPending, TokenHash: hash,
		InvitedBy: &inviter, InvitedByName: p.Name, CreatedAt: now, ExpiresAt: now.Add(InvitationTTL),
	}
	after := map[string]any{"email": email, "role": in.Role, "expires_at": inv.ExpiresAt}
	if title != "" {
		after["job_title"] = title
	}
	audit := orgaudit.New(organisationID, p.UserID, p.Name, orgaudit.ActionInvitationCreated, orgaudit.TargetInvitation, id, email, nil, after, now)
	if err := s.store.CreateInvitation(ctx, inv, audit); err != nil {
		return InvitationView{}, err
	}
	s.audit(ctx, "team.invitation_created", slog.String("organisation_id", organisationID.String()), slog.String("invitation_id", id.String()),
		slog.String("role", in.Role), slog.String("actor_user_id", p.UserID.String()))
	if s.mailer != nil {
		if err := s.mailer.SendInvitation(ctx, email, InvitationMail{
			Token: token, OrganisationName: team.OrganisationName, RoleLabel: auth.MembershipRoleLabel(team.OrganisationKind, in.Role),
			JobTitle: title, InviterName: p.Name, ExpiresAt: inv.ExpiresAt,
		}); err != nil {
			s.logger.ErrorContext(ctx, "invitation email failed", slog.String("invitation_id", id.String()), slog.String("error", err.Error()))
		}
	}
	return s.invitationView(p, team, inv), nil
}

// Invitations returns the team's pending invitations, newest first
// (expired ones included, marked expired).
func (s *Service) Invitations(ctx context.Context, p *auth.Principal, organisationID uuid.UUID) ([]InvitationView, error) {
	team, err := Team(p, organisationID)
	if err != nil {
		return nil, err
	}
	invs, err := s.store.Invitations(ctx, organisationID)
	if err != nil {
		return nil, err
	}
	out := make([]InvitationView, 0, len(invs))
	for _, inv := range invs {
		out = append(out, s.invitationView(p, team, inv))
	}
	return out, nil
}

// CancelInvitation cancels a pending invitation (its link stops working).
func (s *Service) CancelInvitation(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) error {
	if _, err := Team(p, organisationID); err != nil {
		return err
	}
	now := s.clock()
	actor := Actor{UserID: p.UserID, Name: p.Name}
	_, err := s.store.CancelInvitation(ctx, organisationID, id, actor, now, func(inv Invitation) error {
		if !p.MayGrantRole(organisationID, inv.Role) {
			return ErrRoleNotAllowed
		}
		return nil
	}, func(inv Invitation) orgaudit.Event {
		return orgaudit.New(organisationID, actor.UserID, actor.Name, orgaudit.ActionInvitationCancelled, orgaudit.TargetInvitation, inv.ID, inv.Email,
			map[string]any{"status": InvitationPending}, map[string]any{"status": InvitationCancelled}, now)
	})
	if err != nil {
		return err
	}
	s.audit(ctx, "team.invitation_cancelled", slog.String("organisation_id", organisationID.String()), slog.String("invitation_id", id.String()),
		slog.String("actor_user_id", p.UserID.String()))
	return nil
}

// PublicInvitation is an invitation as its link shows it, to anyone holding
// the link: no organisation ID, the address masked.
type PublicInvitation struct {
	Organisation struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	} `json:"organisation"`
	Role          string    `json:"role"`
	RoleLabel     string    `json:"role_label"`
	JobTitle      *string   `json:"job_title"`
	InvitedByName string    `json:"invited_by_name"`
	EmailMasked   string    `json:"email_masked"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// usable returns the invitation behind a token while it can be accepted:
// ErrInvitationNotFound, ErrInvitationUsed or ErrInvitationExpired otherwise.
func usable(inv Invitation, now time.Time) error {
	switch {
	case inv.Status != InvitationPending:
		return ErrInvitationUsed
	case inv.Expired(now):
		return ErrInvitationExpired
	}
	return nil
}

func (s *Service) byToken(ctx context.Context, token string) (Invitation, error) {
	if !plausibleToken(token) {
		return Invitation{}, ErrInvitationNotFound
	}
	inv, err := s.store.InvitationByToken(ctx, hashToken(token))
	if err != nil {
		return Invitation{}, err
	}
	return inv, usable(inv, s.clock())
}

// Invitation returns what the invitation page shows.
func (s *Service) Invitation(ctx context.Context, token string) (PublicInvitation, error) {
	inv, err := s.byToken(ctx, token)
	if err != nil {
		return PublicInvitation{}, err
	}
	var out PublicInvitation
	out.Organisation.Name, out.Organisation.Kind = inv.OrganisationName, inv.OrganisationKind
	out.Role, out.RoleLabel, out.JobTitle = inv.Role, auth.MembershipRoleLabel(inv.OrganisationKind, inv.Role), optional(inv.JobTitle)
	out.InvitedByName, out.EmailMasked, out.ExpiresAt = inv.InvitedByName, identity.MaskEmail(inv.Email), inv.ExpiresAt
	return out, nil
}

// AcceptedView is the answer to an acceptance.
type AcceptedView struct {
	Organisation struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
		Kind string    `json:"kind"`
	} `json:"organisation"`
	Role      string      `json:"role"`
	RoleLabel string      `json:"role_label"`
	Profile   *ProfileRef `json:"profile"`
}

// agencyAccountRoles are the account roles an agency membership stands for.
var agencyAccountRoles = map[string]string{
	auth.RoleManager: identity.AccountRoleAgencyAdmin,
	auth.RoleAgent:   identity.AccountRoleAgent,
	auth.RoleStaff:   identity.AccountRoleAgencyStaff,
}

// ErrAdminSession: an administrator's session cannot join an organisation.
var ErrAdminSession = errors.New("an administrator session cannot accept an invitation")

// Accept accepts an invitation for the signed-in account. The account's
// email address must be the invited one (ErrEmailMismatch); a property
// seeker account cannot hold a professional role (ErrSeekerAccount); someone
// who already belongs to the team gets ErrAlreadyMember. In an agency the
// matching account role becomes ACTIVE (AGENCY_ADMIN, AGENT or AGENCY_STAFF)
// and an AGENT gets a public agent profile linked to the agency, as an
// approved onboarding application gives.
func (s *Service) Accept(ctx context.Context, p *auth.Principal, token string) (AcceptedView, error) {
	if p.SessionScope != auth.ScopeCustomer {
		return AcceptedView{}, ErrAdminSession
	}
	if _, err := s.byToken(ctx, token); err != nil {
		return AcceptedView{}, err
	}
	email := identity.NormalizeEmail(p.Email)
	now := s.clock()
	accepted, err := s.store.AcceptInvitation(ctx, Acceptance{
		TokenHash: hashToken(token), Account: Account{ID: p.UserID, Name: p.Name, Email: email}, At: now,
		Check: func(inv Invitation, state AccountState) error {
			if err := usable(inv, now); err != nil {
				return err
			}
			switch {
			case inv.Email != email:
				return ErrEmailMismatch
			case state.Seeker:
				return ErrSeekerAccount
			case state.Membership != nil:
				return ErrAlreadyMember
			}
			return nil
		},
		Plan: func(inv Invitation) AcceptPlan {
			plan := AcceptPlan{}
			if inv.OrganisationKind == auth.KindAgency {
				plan.AccountRole = agencyAccountRoles[inv.Role]
				plan.AgentProfile = inv.Role == auth.RoleAgent
				plan.ProfileTitle = inv.JobTitle
				if plan.ProfileTitle == "" {
					plan.ProfileTitle = auth.MembershipRoleLabel(inv.OrganisationKind, inv.Role)
				}
			}
			plan.Audit = orgaudit.New(inv.OrganisationID, p.UserID, p.Name, orgaudit.ActionInvitationAccepted, orgaudit.TargetInvitation, inv.ID, inv.Email,
				map[string]any{"status": InvitationPending}, map[string]any{"status": InvitationAccepted, "role": inv.Role, "user_id": p.UserID.String(), "name": p.Name}, now)
			return plan
		},
	})
	if err != nil {
		return AcceptedView{}, err
	}
	inv := accepted.Invitation
	s.audit(ctx, "team.invitation_accepted", slog.String("organisation_id", inv.OrganisationID.String()), slog.String("invitation_id", inv.ID.String()),
		slog.String("role", inv.Role), slog.String("member_user_id", p.UserID.String()))
	var out AcceptedView
	out.Organisation.ID, out.Organisation.Name, out.Organisation.Kind = inv.OrganisationID, inv.OrganisationName, inv.OrganisationKind
	out.Role, out.RoleLabel, out.Profile = inv.Role, auth.MembershipRoleLabel(inv.OrganisationKind, inv.Role), accepted.Profile
	return out, nil
}

// ---------------------------------------------------------------------------
// Audit trail
// ---------------------------------------------------------------------------

// AuditView is one record of the team's audit trail.
type AuditView struct {
	ID          uuid.UUID `json:"id"`
	Action      string    `json:"action"`
	ActionLabel string    `json:"action_label"`
	Actor       struct {
		UserID *uuid.UUID `json:"user_id"`
		Name   string     `json:"name"`
	} `json:"actor"`
	Target struct {
		Type  string    `json:"type"`
		ID    uuid.UUID `json:"id"`
		Label string    `json:"label"`
	} `json:"target"`
	Before map[string]any `json:"before"`
	After  map[string]any `json:"after"`
	At     time.Time      `json:"at"`
}

// AuditPage is one page of the audit trail.
type AuditPage struct {
	Items      []AuditView `json:"items"`
	Page       int         `json:"page"`
	PageSize   int         `json:"page_size"`
	Total      int         `json:"total"`
	TotalPages int         `json:"total_pages"`
}

// Audit returns one page of the team's audit trail, newest first.
func (s *Service) Audit(ctx context.Context, p *auth.Principal, organisationID uuid.UUID, page, size int) (AuditPage, error) {
	if _, err := Team(p, organisationID); err != nil {
		return AuditPage{}, err
	}
	events, total, err := s.store.Audit(ctx, organisationID, size, (page-1)*size)
	if err != nil {
		return AuditPage{}, err
	}
	out := AuditPage{Items: make([]AuditView, 0, len(events)), Page: page, PageSize: size, Total: total}
	if total > 0 {
		out.TotalPages = (total + size - 1) / size
	}
	for _, e := range events {
		v := AuditView{ID: e.ID, Action: e.Action, ActionLabel: orgaudit.ActionLabel(e.Action), Before: e.Before, After: e.After, At: e.At.UTC()}
		v.Actor.UserID, v.Actor.Name = e.ActorUserID, e.ActorName
		v.Target.Type, v.Target.ID, v.Target.Label = e.TargetType, e.TargetID, e.TargetLabel
		out.Items = append(out.Items, v)
	}
	return out, nil
}
