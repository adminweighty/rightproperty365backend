package onboarding

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// This file is the agency's side of a JOIN REQUEST: an application for a role
// whose outcome is AGENCY_MEMBER (AGENT, SALES_AGENT, LETTINGS_AGENT,
// AGENCY_ADMIN, AGENCY_STAFF) names the agency the applicant wants to join,
// and that agency's OWNER — or its MANAGER, for the roles that become AGENT
// or STAFF — may approve or decline it, as a platform reviewer may. Whoever
// decides first wins; the other gets ErrInvalidState (409).

// ErrJoinRequestNotFound: no submitted application to join this agency with
// that ID (also for another agency's application).
var ErrJoinRequestNotFound = errors.New("join request not found")

// JoinApplicant is the person behind a join request. The email address is
// masked: the agency confirms the person by name and job title.
type JoinApplicant struct {
	UserID      uuid.UUID `json:"user_id"`
	Name        string    `json:"name"`
	EmailMasked string    `json:"email_masked"`
}

// JoinRequestView is a join request as the agency sees it.
type JoinRequestView struct {
	ID uuid.UUID `json:"id"`
	// Role is the account role applied for; MembershipRole the membership
	// an approval creates.
	Role                string        `json:"role"`
	RoleLabel           string        `json:"role_label"`
	MembershipRole      string        `json:"membership_role"`
	MembershipRoleLabel string        `json:"membership_role_label"`
	Status              string        `json:"status"`
	StatusLabel         string        `json:"status_label"`
	Applicant           JoinApplicant `json:"applicant"`
	JobTitle            *string       `json:"job_title"`
	DocumentCount       int           `json:"document_count"`
	SubmittedAt         *time.Time    `json:"submitted_at"`
	DecisionReason      *string       `json:"decision_reason"`
	DecidedAt           *time.Time    `json:"decided_at"`
	// CanDecide: the caller may approve or decline it (an OWNER any, a
	// MANAGER only those that become AGENT or STAFF).
	CanDecide bool `json:"can_decide"`
}

// joinAudit builds the agency's audit record of a decision on a join request.
func joinAudit(p *auth.Principal, app Application, organisationID uuid.UUID, action, to, membershipRole, reason string, at time.Time) *orgaudit.Event {
	after := map[string]any{"status": to, "membership_role": membershipRole}
	if reason != "" {
		after["reason"] = reason
	}
	if p.SessionScope == auth.ScopeAdmin {
		after["decided_by"] = "PLATFORM"
	} else {
		after["decided_by"] = "ORGANISATION"
	}
	e := orgaudit.New(organisationID, p.UserID, p.Name, action, orgaudit.TargetJoinRequest, app.ID, app.Applicant.Name,
		map[string]any{"status": StatusSubmitted}, after, at)
	return &e
}

// joinDecisionAudit returns the audit record of a decision to reject or ask
// for changes when the application is a join request of a still-verified
// agency, and nil otherwise.
func (s *Service) joinDecisionAudit(ctx context.Context, p *auth.Principal, app Application, to, reason string, at time.Time) (*orgaudit.Event, error) {
	if app.AgencySlug == nil {
		return nil, nil
	}
	c, err := s.catalogue(ctx)
	if err != nil {
		return nil, err
	}
	outcome := c.outcomes[app.Role]
	if outcome.Outcome != OutcomeAgencyMember {
		return nil, nil
	}
	agency, err := s.agencies.VerifiedAgency(ctx, *app.AgencySlug)
	if err != nil || agency == nil {
		return nil, err
	}
	action := orgaudit.ActionJoinRequestDeclined
	if to == StatusChangesRequested {
		action = orgaudit.ActionJoinRequestChangesRequired
	}
	return joinAudit(p, app, agency.OrganisationID, action, to, outcome.MembershipRole, reason, at), nil
}

// joinView builds the agency's view of an application.
func (c *catalogue) joinView(p *auth.Principal, organisationID uuid.UUID, a Application) JoinRequestView {
	outcome := c.outcomes[a.Role]
	kind := auth.KindAgency
	return JoinRequestView{
		ID: a.ID, Role: a.Role, RoleLabel: c.roleLabel(a.Role),
		MembershipRole: outcome.MembershipRole, MembershipRoleLabel: auth.MembershipRoleLabel(kind, outcome.MembershipRole),
		Status: a.Status, StatusLabel: StatusLabel(a.Status),
		Applicant:     JoinApplicant{UserID: a.UserID, Name: a.Applicant.Name, EmailMasked: identity.MaskEmail(a.Applicant.Email)},
		JobTitle:      a.JobTitle,
		DocumentCount: len(a.Documents), SubmittedAt: utcPtr(a.SubmittedAt),
		DecisionReason: nullable(a.DecisionReason), DecidedAt: utcPtr(a.DecidedAt),
		CanDecide: a.Status == StatusSubmitted && p.MayGrantRole(organisationID, outcome.MembershipRole),
	}
}

// agencyOf returns the verified agency of an organisation the caller manages
// the team of: ErrForbidden when the caller may not, ErrJoinRequestNotFound
// when the organisation has no verified agency profile (nobody can ask to
// join it).
func (s *Service) agencyOf(ctx context.Context, p *auth.Principal, organisationID uuid.UUID) (*Agency, error) {
	if !p.MayManageTeam(organisationID) {
		return nil, ErrForbidden
	}
	agency, err := s.agencies.AgencyOf(ctx, organisationID)
	if err != nil {
		return nil, err
	}
	if agency == nil {
		return nil, ErrJoinRequestNotFound
	}
	return agency, nil
}

// JoinRequests returns the submitted join requests of an agency, oldest
// first (at most 100). An organisation that is not an agency has none.
func (s *Service) JoinRequests(ctx context.Context, p *auth.Principal, organisationID uuid.UUID) ([]JoinRequestView, error) {
	agency, err := s.agencyOf(ctx, p, organisationID)
	if errors.Is(err, ErrJoinRequestNotFound) {
		return []JoinRequestView{}, nil
	}
	if err != nil {
		return nil, err
	}
	c, err := s.catalogue(ctx)
	if err != nil {
		return nil, err
	}
	apps, _, _, err := s.store.List(ctx, ListFilter{Status: StatusSubmitted, AgencySlug: agency.Slug, Limit: 100})
	if err != nil {
		return nil, err
	}
	out := make([]JoinRequestView, 0, len(apps))
	for _, a := range apps {
		if c.outcomes[a.Role].Outcome == OutcomeAgencyMember {
			out = append(out, c.joinView(p, organisationID, a))
		}
	}
	return out, nil
}

// joinRequest loads an application to join the organisation's agency that
// the caller may decide: ErrJoinRequestNotFound for one that is not a join
// request of this agency, ErrForbidden when the caller's role may not grant
// the membership it asks for, ErrInvalidState when it was already decided.
func (s *Service) joinRequest(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) (*catalogue, Application, error) {
	agency, err := s.agencyOf(ctx, p, organisationID)
	if err != nil {
		return nil, Application{}, err
	}
	app, err := s.store.Get(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return nil, Application{}, ErrJoinRequestNotFound
	}
	if err != nil {
		return nil, Application{}, err
	}
	c, err := s.catalogue(ctx)
	if err != nil {
		return nil, Application{}, err
	}
	outcome := c.outcomes[app.Role]
	if outcome.Outcome != OutcomeAgencyMember || app.AgencySlug == nil || *app.AgencySlug != agency.Slug ||
		!slices.Contains([]string{StatusSubmitted, StatusApproved, StatusRejected, StatusChangesRequested}, app.Status) {
		return nil, Application{}, ErrJoinRequestNotFound
	}
	if !p.MayGrantRole(organisationID, outcome.MembershipRole) {
		return nil, Application{}, ErrForbidden
	}
	if app.Status != StatusSubmitted {
		return nil, Application{}, ErrInvalidState
	}
	return c, app, nil
}

func (s *Service) reloadJoin(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) (JoinRequestView, error) {
	app, err := s.store.Get(ctx, id)
	if err != nil {
		return JoinRequestView{}, err
	}
	c, err := s.catalogue(ctx)
	if err != nil {
		return JoinRequestView{}, err
	}
	return c.joinView(p, organisationID, app), nil
}

// AgencyApprove approves a join request on behalf of the agency: the same
// approval a platform reviewer gives (membership, agent profile, ACTIVE
// account role, the applicant's email), recorded in the agency's audit trail.
func (s *Service) AgencyApprove(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) (JoinRequestView, error) {
	_, app, err := s.joinRequest(ctx, p, organisationID, id)
	if err != nil {
		return JoinRequestView{}, err
	}
	if _, err := s.approve(ctx, p, app, ""); err != nil {
		return JoinRequestView{}, err
	}
	return s.reloadJoin(ctx, p, organisationID, id)
}

// AgencyDecline declines a join request on behalf of the agency, with a
// reason that is emailed to the applicant. The account role becomes REJECTED,
// as after a platform reviewer's rejection; the applicant may apply again.
func (s *Service) AgencyDecline(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID, rawReason string) (JoinRequestView, error) {
	text, fields := reason(rawReason, true)
	if fields != nil {
		return JoinRequestView{}, &ValidationError{Fields: fields}
	}
	c, app, err := s.joinRequest(ctx, p, organisationID, id)
	if err != nil {
		return JoinRequestView{}, err
	}
	now := s.clock()
	t := Transition{
		ApplicationID: id, From: []string{StatusSubmitted}, To: StatusRejected, ActorID: p.UserID, Reason: text, At: now,
		Event: EventRejected, AccountRoleStatus: RoleStatusRejected,
		Audit: joinAudit(p, app, organisationID, orgaudit.ActionJoinRequestDeclined, StatusRejected, c.outcomes[app.Role].MembershipRole, text, now),
	}
	if err := s.store.Transition(ctx, t); err != nil {
		return JoinRequestView{}, err
	}
	s.audit(ctx, "onboarding.decided", slog.String("application_id", id.String()), slog.String("role", app.Role),
		slog.String("from", StatusSubmitted), slog.String("to", StatusRejected), slog.String("actor_user_id", p.UserID.String()),
		slog.String("applicant_user_id", app.UserID.String()), slog.String("organisation_id", organisationID.String()))
	return s.reloadJoin(ctx, p, organisationID, id)
}
