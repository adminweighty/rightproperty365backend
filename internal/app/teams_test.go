package app

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// The people of Sample Estates (and an outsider), as the team tests use them.
const (
	ownerEmail    = devseed.EstatesOwnerEmail // OWNER, Rudo Moyo
	managerEmail  = devseed.AgencyAdminEmail  // MANAGER, Ruvimbo Chari
	agentEmail    = identity.SeedAgentEmail   // AGENT, Tendai Ncube
	agent2Email   = devseed.EstatesAgent2Email
	staffEmail    = devseed.AgencyStaffEmail // STAFF, Kuda Staff
	outsiderEmail = identity.SeedAgentBEmail // AGENT of Second Street Realty
)

func teamPath(suffix string) string {
	return "/v1/agent/organisations/" + devseed.SampleEstates.ID.String() + suffix
}

type teamMember struct {
	UserID       string   `json:"user_id"`
	Name         string   `json:"name"`
	Role         string   `json:"role"`
	RoleLabel    string   `json:"role_label"`
	JobTitle     *string  `json:"job_title"`
	Status       string   `json:"status"`
	JoinedAt     *string  `json:"joined_at"`
	OpenLeads    int      `json:"open_leads"`
	IsYou        bool     `json:"is_you"`
	CanChange    bool     `json:"can_change"`
	AllowedRoles []string `json:"allowed_roles"`
	CanHoldLeads bool     `json:"can_hold_leads"`
}

type teamMembers struct {
	Items []teamMember `json:"items"`
}

func (m teamMembers) find(userID uuid.UUID) *teamMember {
	for i := range m.Items {
		if m.Items[i].UserID == userID.String() {
			return &m.Items[i]
		}
	}
	return nil
}

type invitationBody struct {
	Invitation struct {
		ID        string `json:"id"`
		Email     string `json:"email"`
		Role      string `json:"role"`
		Status    string `json:"status"`
		Expired   bool   `json:"expired"`
		CanCancel bool   `json:"can_cancel"`
	} `json:"invitation"`
}

type publicInvitationBody struct {
	Invitation struct {
		Organisation struct {
			Name string `json:"name"`
		} `json:"organisation"`
		Role          string `json:"role"`
		InvitedByName string `json:"invited_by_name"`
		EmailMasked   string `json:"email_masked"`
	} `json:"invitation"`
}

type receivedBody struct {
	Request struct {
		RequestID  string `json:"request_id"`
		Stage      string `json:"stage"`
		CanAssign  bool   `json:"can_assign"`
		AssignedTo *struct {
			UserID string `json:"user_id"`
			Name   string `json:"name"`
		} `json:"assigned_to"`
		Timeline []struct {
			Event string `json:"event"`
			Label string `json:"label"`
		} `json:"timeline"`
	} `json:"request"`
}

type auditBody struct {
	Items []struct {
		Action string         `json:"action"`
		After  map[string]any `json:"after"`
		Actor  struct {
			Name string `json:"name"`
		} `json:"actor"`
		Target struct {
			Type  string `json:"type"`
			Label string `json:"label"`
		} `json:"target"`
	} `json:"items"`
	Total int `json:"total"`
}

func (a auditBody) actions() []string {
	var out []string
	for _, it := range a.Items {
		out = append(out, it.Action)
	}
	return out
}

func (e *env) members(as reqOpt) teamMembers {
	e.t.Helper()
	rec := e.get(teamPath("/members"), as)
	wantStatus(e.t, rec, http.StatusOK)
	return decode[teamMembers](e.t, rec)
}

// invite invites email as role and returns the token of the emailed link.
func (e *env) invite(as reqOpt, email, role string) (invitationBody, string) {
	e.t.Helper()
	rec := e.post(teamPath("/invitations"), obj{"email": email, "role": role}, as)
	wantStatus(e.t, rec, http.StatusCreated)
	sent := e.mail.lastInvitation(e.t)
	if sent.to != strings.ToLower(email) {
		e.t.Fatalf("invitation emailed to %q, want %q", sent.to, email)
	}
	return decode[invitationBody](e.t, rec), sent.mail.Token
}

// Who may see and change the team: OWNER and MANAGER (within their limits),
// never AGENT or STAFF; an outsider gets the same 404 as for nothing.
func TestTeamPermissionMatrix(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)

	for _, tc := range []struct {
		email  string
		status int
		code   string
	}{
		{ownerEmail, http.StatusOK, ""},
		{managerEmail, http.StatusOK, ""},
		{agentEmail, http.StatusForbidden, httpx.CodeForbidden},
		{staffEmail, http.StatusForbidden, httpx.CodeForbidden},
		{outsiderEmail, http.StatusNotFound, teams.CodeOrganisationNotFound},
	} {
		for _, path := range []string{"/members", "/invitations", "/join-requests", "/audit"} {
			rec := e.get(teamPath(path), c.as(tc.email))
			if tc.code == "" {
				wantStatus(t, rec, tc.status)
				if rec.Header().Get("Cache-Control") != "private, no-store" {
					t.Fatalf("%s %s: Cache-Control %q", tc.email, path, rec.Header().Get("Cache-Control"))
				}
				continue
			}
			wantError(t, rec, tc.status, tc.code)
		}
	}
	wantError(t, e.get(teamPath("/members")), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// The members list: what the caller may do with each row.
	owner := e.members(c.as(ownerEmail))
	self := owner.find(devseed.EstatesOwnerID)
	if self == nil || !self.IsYou || self.CanChange || self.Role != auth.RoleOwner || self.RoleLabel != "Agency Principal" {
		t.Fatalf("the owner's own row: %+v", self)
	}
	if m := owner.find(devseed.AgencyAdminID); m == nil || !m.CanChange || strings.Join(m.AllowedRoles, ",") != "MANAGER,AGENT,STAFF" {
		t.Fatalf("the manager as the owner sees them: %+v", m)
	}
	if m := owner.find(identity.SeedAgentUserID); m == nil || !m.CanHoldLeads || m.JoinedAt == nil {
		t.Fatalf("the agent row: %+v", m)
	}
	if m := owner.find(devseed.AgencyStaffID); m == nil || m.CanHoldLeads {
		t.Fatalf("staff hold no leads: %+v", m)
	}
	manager := e.members(c.as(managerEmail))
	if m := manager.find(devseed.EstatesOwnerID); m == nil || m.CanChange {
		t.Fatalf("a manager cannot change the owner: %+v", m)
	}
	if m := manager.find(identity.SeedAgentUserID); m == nil || !m.CanChange || strings.Join(m.AllowedRoles, ",") != "AGENT,STAFF" {
		t.Fatalf("the agent as the manager sees them: %+v", m)
	}

	// Inviting: the role limits.
	for _, tc := range []struct {
		email, role string
		status      int
		code        string
	}{
		{ownerEmail, auth.RoleManager, http.StatusCreated, ""},
		{ownerEmail, auth.RoleAgent, http.StatusCreated, ""},
		{ownerEmail, auth.RoleStaff, http.StatusCreated, ""},
		{ownerEmail, auth.RoleOwner, http.StatusForbidden, teams.CodeRoleNotAllowed},
		{managerEmail, auth.RoleAgent, http.StatusCreated, ""},
		{managerEmail, auth.RoleStaff, http.StatusCreated, ""},
		{managerEmail, auth.RoleManager, http.StatusForbidden, teams.CodeRoleNotAllowed},
		{managerEmail, auth.RoleOwner, http.StatusForbidden, teams.CodeRoleNotAllowed},
		{agentEmail, auth.RoleAgent, http.StatusForbidden, httpx.CodeForbidden},
		{staffEmail, auth.RoleStaff, http.StatusForbidden, httpx.CodeForbidden},
		{outsiderEmail, auth.RoleAgent, http.StatusNotFound, teams.CodeOrganisationNotFound},
	} {
		e.seq++
		rec := e.post(teamPath("/invitations"), obj{"email": "matrix" + string(rune('a'+e.seq%26)) + tc.role + "@example.com", "role": tc.role}, c.as(tc.email))
		if tc.code == "" {
			wantStatus(t, rec, tc.status)
			continue
		}
		wantError(t, rec, tc.status, tc.code)
	}
	wantError(t, e.post(teamPath("/invitations"), obj{"email": "not-an-email", "role": "BOSS"}, c.as(ownerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)

	// Changing members: the role limits, the owner, oneself.
	agent := teamPath("/members/" + identity.SeedAgentUserID.String())
	wantError(t, e.patch(teamPath("/members/"+devseed.EstatesOwnerID.String()), obj{"role": "MANAGER"}, c.as(managerEmail)), http.StatusConflict, teams.CodeLastOwner)
	wantError(t, e.deleteReq(teamPath("/members/"+devseed.EstatesOwnerID.String()), c.as(managerEmail)), http.StatusConflict, teams.CodeLastOwner)
	wantError(t, e.patch(teamPath("/members/"+devseed.EstatesOwnerID.String()), obj{"status": "SUSPENDED"}, c.as(ownerEmail)), http.StatusForbidden, teams.CodeSelfChangeForbidden)
	wantError(t, e.patch(teamPath("/members/"+devseed.AgencyAdminID.String()), obj{"job_title": "Boss"}, c.as(managerEmail)), http.StatusForbidden, teams.CodeSelfChangeForbidden)
	wantError(t, e.patch(agent, obj{"role": "MANAGER"}, c.as(managerEmail)), http.StatusForbidden, teams.CodeRoleNotAllowed)
	wantError(t, e.patch(agent, obj{"role": "OWNER"}, c.as(ownerEmail)), http.StatusForbidden, teams.CodeRoleNotAllowed)
	wantError(t, e.patch(teamPath("/members/"+devseed.AgencyAdminID.String()), obj{"status": "SUSPENDED"}, c.as(managerEmail)), http.StatusForbidden, teams.CodeSelfChangeForbidden)
	wantError(t, e.patch(agent, obj{"status": "SUSPENDED"}, c.as(staffEmail)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.patch(agent, obj{"status": "SUSPENDED"}, c.as(outsiderEmail)), http.StatusNotFound, teams.CodeOrganisationNotFound)
	wantError(t, e.patch(agent, obj{"status": "GONE"}, c.as(ownerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.patch(teamPath("/members/"+identity.SeedAgentBUserID.String()), obj{"role": "STAFF"}, c.as(ownerEmail)), http.StatusNotFound, teams.CodeMemberNotFound)
	// The owner makes Chipo a MANAGER; the other manager may then neither
	// change nor remove her.
	wantStatus(t, e.patch(teamPath("/members/"+devseed.EstatesAgent2ID.String()), obj{"role": "MANAGER"}, c.as(ownerEmail)), http.StatusOK)
	wantError(t, e.patch(teamPath("/members/"+devseed.EstatesAgent2ID.String()), obj{"role": "AGENT"}, c.as(managerEmail)), http.StatusForbidden, teams.CodeRoleNotAllowed)
	wantError(t, e.deleteReq(teamPath("/members/"+devseed.EstatesAgent2ID.String()), c.as(managerEmail)), http.StatusForbidden, teams.CodeRoleNotAllowed)

	// With a second owner, an owner is protected rather than "the last one".
	if err := e.identity.AddMembership(devseed.CommercialAgentID, devseed.SampleEstates.ID, devseed.MarketCode, auth.RoleOwner); err != nil {
		t.Fatal(err)
	}
	wantError(t, e.deleteReq(teamPath("/members/"+devseed.CommercialAgentID.String()), c.as(ownerEmail)), http.StatusForbidden, teams.CodeOwnerProtected)

	// Manager changes an agent's role and job title; the owner sees it.
	rec := e.patch(agent, obj{"role": "STAFF", "job_title": "Viewing assistant"}, c.as(managerEmail))
	wantStatus(t, rec, http.StatusOK)
	changed := decode[struct {
		Member teamMember `json:"member"`
	}](t, rec).Member
	if changed.Role != auth.RoleStaff || changed.JobTitle == nil || *changed.JobTitle != "Viewing assistant" || changed.CanHoldLeads {
		t.Fatalf("after the change: %+v", changed)
	}
	if acts := decode[auditBody](t, e.get(teamPath("/audit"), c.as(ownerEmail))).actions(); !strings.Contains(strings.Join(acts, ","), "MEMBER_ROLE_CHANGED,MEMBER_JOB_TITLE_CHANGED") &&
		!strings.Contains(strings.Join(acts, ","), "MEMBER_JOB_TITLE_CHANGED,MEMBER_ROLE_CHANGED") {
		t.Fatalf("audit after the change: %v", acts)
	}
}

// An invitation: emailed once, shown to its holder, accepted only by the
// invited address, single use, replaced by a new one, cancelled, expired.
func TestInvitations(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)

	inv, token := e.invite(c.as(ownerEmail), "New.Agent@Example.com", auth.RoleAgent)
	if inv.Invitation.Email != "new.agent@example.com" || inv.Invitation.Status != teams.InvitationPending || !inv.Invitation.CanCancel {
		t.Fatalf("invitation: %+v", inv)
	}
	if strings.Contains(e.logs.String(), token) {
		t.Fatal("the token was written to the logs")
	}
	// Anyone with the link sees what it is for, the address masked.
	rec := e.get("/v1/invitations/" + token)
	wantStatus(t, rec, http.StatusOK)
	pub := decode[publicInvitationBody](t, rec).Invitation
	if pub.Organisation.Name != "Sample Estates" || pub.Role != auth.RoleAgent || pub.InvitedByName != "Rudo Moyo" || strings.Contains(pub.EmailMasked, "new.agent") {
		t.Fatalf("public invitation: %+v", pub)
	}
	wantError(t, e.get("/v1/invitations/"+strings.Repeat("a", 43)), http.StatusNotFound, teams.CodeInvitationNotFound)
	wantError(t, e.get("/v1/invitations/short"), http.StatusNotFound, teams.CodeInvitationNotFound)

	// Accepting needs a session...
	wantError(t, e.post("/v1/invitations/"+token+"/accept", nil), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// ...of the invited address.
	other := e.selfRegister("someone.else@example.com", identity.AccountRoleAgent)
	wantError(t, e.post("/v1/invitations/"+token+"/accept", nil, browser(other)), http.StatusForbidden, teams.CodeInvitationEmailMismatch)
	// The invited person signs up first (as the web does), then accepts.
	invited := e.selfRegister("new.agent@example.com", identity.AccountRoleAgent)
	wantError(t, e.post("/v1/invitations/"+token+"/accept", nil, withCookie(invited)), http.StatusForbidden, httpx.CodeCSRFOrigin)
	rec = e.post("/v1/invitations/"+token+"/accept", nil, browser(invited))
	wantStatus(t, rec, http.StatusOK)
	accepted := decode[struct {
		Role    string `json:"role"`
		Profile *struct {
			Kind string `json:"kind"`
			Slug string `json:"slug"`
		} `json:"profile"`
	}](t, rec)
	if accepted.Role != auth.RoleAgent || accepted.Profile == nil || accepted.Profile.Kind != "agents" {
		t.Fatalf("accepted: %+v", accepted)
	}
	// The membership counts from the next request: the inbox opens, the
	// AGENT account role is active, the public agent profile exists.
	me, _ := e.me(invited)
	if len(me.Memberships) != 1 || me.Memberships[0].Role != auth.RoleAgent || me.Memberships[0].OrganisationID != devseed.SampleEstates.ID.String() {
		t.Fatalf("memberships after accepting: %+v", me.Memberships)
	}
	for _, r := range me.AccountRoles {
		if r.Role == identity.AccountRoleAgent && r.Status != identity.AccountRoleActive {
			t.Fatalf("the AGENT role is %s", r.Status)
		}
	}
	wantStatus(t, e.get("/v1/agent/property-requests", withCookie(invited)), http.StatusOK)
	wantStatus(t, e.get("/v1/directory/agents/"+accepted.Profile.Slug), http.StatusOK)
	// Single use.
	wantError(t, e.post("/v1/invitations/"+token+"/accept", nil, browser(invited)), http.StatusGone, teams.CodeInvitationNoLongerValid)
	wantError(t, e.get("/v1/invitations/"+token), http.StatusGone, teams.CodeInvitationNoLongerValid)
	// The link was opened and accepted several times: the access log never
	// holds the token, only its route.
	if logs := e.logs.String(); strings.Contains(logs, token) || !strings.Contains(logs, `"path":"/v1/invitations/{token}/accept"`) {
		t.Fatal("the access log holds an invitation token (or no invitation line at all)")
	}
	// Already a member now.
	wantError(t, e.post(teamPath("/invitations"), obj{"email": "new.agent@example.com", "role": "AGENT"}, c.as(ownerEmail)), http.StatusConflict, teams.CodeAlreadyAMember)
	wantError(t, e.post(teamPath("/invitations"), obj{"email": agentEmail, "role": "STAFF"}, c.as(ownerEmail)), http.StatusConflict, teams.CodeAlreadyAMember)

	// Inviting the same address again replaces the pending invitation.
	_, first := e.invite(c.as(managerEmail), "twice@example.com", auth.RoleStaff)
	_, second := e.invite(c.as(managerEmail), "twice@example.com", auth.RoleStaff)
	wantError(t, e.get("/v1/invitations/"+first), http.StatusGone, teams.CodeInvitationNoLongerValid)
	wantStatus(t, e.get("/v1/invitations/"+second), http.StatusOK)
	list := decode[struct {
		Items []struct {
			ID    string `json:"id"`
			Email string `json:"email"`
		} `json:"items"`
	}](t, e.get(teamPath("/invitations"), c.as(managerEmail)))
	n := 0
	cancelID := ""
	for _, it := range list.Items {
		if it.Email == "twice@example.com" {
			n++
			cancelID = it.ID
		}
	}
	if n != 1 {
		t.Fatalf("%d pending invitations for the address, want 1", n)
	}
	// Cancelling: the link stops working. A manager cannot cancel a
	// MANAGER invitation (only an owner could have sent it).
	_, managerToken := e.invite(c.as(ownerEmail), "future.manager@example.com", auth.RoleManager)
	managerInv := decode[struct {
		Items []struct {
			ID        string `json:"id"`
			Email     string `json:"email"`
			CanCancel bool   `json:"can_cancel"`
		} `json:"items"`
	}](t, e.get(teamPath("/invitations"), c.as(managerEmail)))
	for _, it := range managerInv.Items {
		if it.Email == "future.manager@example.com" {
			if it.CanCancel {
				t.Fatal("a manager cannot cancel a MANAGER invitation")
			}
			wantError(t, e.deleteReq(teamPath("/invitations/"+it.ID), c.as(managerEmail)), http.StatusForbidden, teams.CodeRoleNotAllowed)
		}
	}
	wantStatus(t, e.deleteReq(teamPath("/invitations/"+cancelID), c.as(managerEmail)), http.StatusNoContent)
	wantError(t, e.get("/v1/invitations/"+second), http.StatusGone, teams.CodeInvitationNoLongerValid)
	wantError(t, e.deleteReq(teamPath("/invitations/"+cancelID), c.as(managerEmail)), http.StatusGone, teams.CodeInvitationNoLongerValid)
	wantError(t, e.deleteReq(teamPath("/invitations/"+uuid.NewString()), c.as(managerEmail)), http.StatusNotFound, teams.CodeInvitationNotFound)

	// A property seeker account cannot join.
	_, seekerToken := e.invite(c.as(ownerEmail), identity.SeedConsumerEmail, auth.RoleStaff)
	wantError(t, e.post("/v1/invitations/"+seekerToken+"/accept", nil, c.as(identity.SeedConsumerEmail)), http.StatusConflict, teams.CodeSeekerAccount)

	// Seven days later the links stop working.
	e.now = e.now.Add(teams.InvitationTTL + time.Minute)
	wantError(t, e.get("/v1/invitations/"+managerToken), http.StatusGone, teams.CodeInvitationExpired)
	future := e.selfRegister("future.manager@example.com", identity.AccountRoleAgencyAdmin)
	wantError(t, e.post("/v1/invitations/"+managerToken+"/accept", nil, browser(future)), http.StatusGone, teams.CodeInvitationExpired)

	// Every step is in the audit trail, newest first, never the token.
	audit := decode[auditBody](t, e.get(teamPath("/audit?page_size=100"), c.as(ownerEmail)))
	joined := strings.Join(audit.actions(), ",")
	for _, want := range []string{orgaudit.ActionInvitationCreated, orgaudit.ActionInvitationAccepted, orgaudit.ActionInvitationCancelled} {
		if !strings.Contains(joined, want) {
			t.Fatalf("audit lacks %s: %s", want, joined)
		}
	}
	if audit.Items[0].Action != orgaudit.ActionInvitationCreated || audit.Items[0].Target.Label != identity.SeedConsumerEmail {
		t.Fatalf("newest audit record: %+v", audit.Items[0])
	}
	if raw := e.get(teamPath("/audit?page_size=100"), c.as(ownerEmail)).Body.String(); strings.Contains(raw, token) || strings.Contains(raw, second) {
		t.Fatal("an audit record carries a token")
	}
	wantError(t, e.get(teamPath("/audit?page_size=1000"), c.as(ownerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// Suspending or removing a member takes effect on their very next request
// with the same session, and gives their open work back to the organisation.
func TestSuspendAndRemoveAreImmediate(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)

	// A request for Tendai's listing is assigned to Tendai by routing; an
	// appointment for it too.
	reqID := e.sent(e.propertyRequest(1))
	cast := newCast(e)
	appt := cast.book(identity.SeedConsumerEmail, e.appointmentInput(1, 72*time.Hour))
	if got := decode[apptOne](t, e.get("/v1/inbox/appointments/"+appt.ID, c.as(ownerEmail))).Appointment; got.AssignedTo == nil ||
		got.AssignedTo.UserID != identity.SeedAgentUserID.String() {
		t.Fatalf("the appointment is not Tendai's: %+v", got.AssignedTo)
	}

	tendai := c.as(agentEmail) // one session, used throughout
	wantStatus(t, e.get("/v1/agent/property-requests/"+reqID, tendai), http.StatusOK)
	if m := e.members(c.as(ownerEmail)).find(identity.SeedAgentUserID); m == nil || m.OpenLeads != 1 {
		t.Fatalf("Tendai's open leads: %+v", m)
	}

	// Suspended: the same session no longer opens anything of the agency.
	rec := e.patch(teamPath("/members/"+identity.SeedAgentUserID.String()), obj{"status": "SUSPENDED"}, c.as(managerEmail))
	wantStatus(t, rec, http.StatusOK)
	wantError(t, e.get("/v1/agent/property-requests", tendai), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/agent/property-requests/"+reqID, tendai), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get(teamPath("/members"), tendai), http.StatusNotFound, teams.CodeOrganisationNotFound)
	if me, _ := e.me(e.loginCookie(agentEmail, seedPassword)); len(me.Memberships) != 0 {
		t.Fatalf("a suspended member still has memberships: %+v", me.Memberships)
	}
	// Still listed, as suspended; their work went back to the organisation inbox.
	if m := e.members(c.as(ownerEmail)).find(identity.SeedAgentUserID); m == nil || m.Status != teams.MemberSuspended || m.OpenLeads != 0 || m.CanHoldLeads {
		t.Fatalf("suspended member row: %+v", m)
	}
	detail := decode[receivedBody](t, e.get("/v1/agent/property-requests/"+reqID, c.as(ownerEmail)))
	if detail.Request.AssignedTo != nil || detail.Request.Timeline[len(detail.Request.Timeline)-1].Event != propertyrequests.TimelineUnassigned {
		t.Fatalf("the request after the suspension: %+v", detail.Request)
	}
	// Another agent of the agency now sees it in the organisation inbox.
	wantStatus(t, e.get("/v1/agent/property-requests/"+reqID, c.as(agent2Email)), http.StatusOK)
	got := cast.mustAct(ownerEmail, "/v1/inbox/appointments/"+appt.ID+"/confirm", nil)
	if got.AssignedTo != nil {
		t.Fatalf("the appointment is still assigned: %+v", got.AssignedTo)
	}
	// And a suspended member cannot be given work.
	wantError(t, e.post("/v1/agent/property-requests/"+reqID+"/assign", obj{"user_id": identity.SeedAgentUserID.String()}, c.as(ownerEmail)),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	audit := decode[auditBody](t, e.get(teamPath("/audit"), c.as(ownerEmail)))
	if audit.Items[0].Action != orgaudit.ActionMemberSuspended || audit.Items[0].After["released_requests"] != float64(1) ||
		audit.Items[0].After["released_appointments"] != float64(1) || audit.Items[0].Actor.Name != "Ruvimbo Chari" {
		t.Fatalf("suspension audit: %+v", audit.Items[0])
	}

	// Reactivated: access comes back on the next request.
	wantStatus(t, e.patch(teamPath("/members/"+identity.SeedAgentUserID.String()), obj{"status": "ACTIVE"}, c.as(managerEmail)), http.StatusOK)
	wantStatus(t, e.get("/v1/agent/property-requests", tendai), http.StatusOK)

	// Removed: no access, no longer listed, cannot be changed again.
	wantStatus(t, e.deleteReq(teamPath("/members/"+identity.SeedAgentUserID.String()), c.as(ownerEmail)), http.StatusNoContent)
	wantError(t, e.get("/v1/agent/property-requests", tendai), http.StatusForbidden, httpx.CodeForbidden)
	if m := e.members(c.as(ownerEmail)).find(identity.SeedAgentUserID); m != nil {
		t.Fatalf("a removed member is still listed: %+v", m)
	}
	wantError(t, e.deleteReq(teamPath("/members/"+identity.SeedAgentUserID.String()), c.as(ownerEmail)), http.StatusNotFound, teams.CodeMemberNotFound)
	// A removed manager loses the team pages at once too.
	manager := c.as(managerEmail)
	wantStatus(t, e.get(teamPath("/members"), manager), http.StatusOK)
	wantStatus(t, e.deleteReq(teamPath("/members/"+devseed.AgencyAdminID.String()), c.as(ownerEmail)), http.StatusNoContent)
	wantError(t, e.get(teamPath("/members"), manager), http.StatusNotFound, teams.CodeOrganisationNotFound)
	if acts := decode[auditBody](t, e.get(teamPath("/audit"), c.as(ownerEmail))).actions(); acts[0] != orgaudit.ActionMemberRemoved || acts[1] != orgaudit.ActionMemberRemoved {
		t.Fatalf("removal audit: %v", acts)
	}
	// A removed person can be invited again and come back.
	_, token := e.invite(c.as(ownerEmail), agentEmail, auth.RoleAgent)
	wantStatus(t, e.post("/v1/invitations/"+token+"/accept", nil, tendai), http.StatusOK)
	wantStatus(t, e.get("/v1/agent/property-requests", tendai), http.StatusOK)
}

// Assigning a request: OWNER and MANAGER only, to an active member who can
// hold leads; the assignee sees it, the previous one no longer does.
func TestAssignPropertyRequest(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	id := e.sent(e.propertyRequest(7)) // Chipo Dube's listing
	assign := func(as reqOpt, userID any) *receivedBody {
		e.t.Helper()
		rec := e.post("/v1/agent/property-requests/"+id+"/assign", obj{"user_id": userID}, as)
		if rec.Code != http.StatusOK {
			return nil
		}
		b := decode[receivedBody](t, rec)
		return &b
	}

	// Only OWNER and MANAGER.
	wantError(t, e.post("/v1/agent/property-requests/"+id+"/assign", obj{"user_id": identity.SeedAgentUserID.String()}, c.as(agent2Email)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post("/v1/agent/property-requests/"+id+"/assign", obj{"user_id": identity.SeedAgentUserID.String()}, c.as(staffEmail)), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	wantError(t, e.post("/v1/agent/property-requests/"+id+"/assign", obj{"user_id": identity.SeedAgentUserID.String()}, c.as(outsiderEmail)), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	// Only to someone who can hold leads in this organisation.
	for _, who := range []string{devseed.AgencyStaffID.String(), identity.SeedAgentBUserID.String(), uuid.NewString()} {
		wantError(t, e.post("/v1/agent/property-requests/"+id+"/assign", obj{"user_id": who}, c.as(ownerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)
	}
	wantError(t, e.post("/v1/agent/property-requests/"+id+"/assign", obj{}, c.as(ownerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post("/v1/agent/property-requests/"+id+"/assign", obj{"user_id": "nope"}, c.as(ownerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)

	got := assign(c.as(managerEmail), identity.SeedAgentUserID.String())
	if got == nil || got.Request.AssignedTo == nil || got.Request.AssignedTo.Name != "Tendai Ncube" || got.Request.Stage != propertyrequests.StageAssigned || !got.Request.CanAssign {
		t.Fatalf("after assigning: %+v", got)
	}
	events := ""
	for _, en := range got.Request.Timeline {
		events += en.Event + ":" + en.Label + ";"
	}
	if !strings.Contains(events, "ASSIGNED:Assigned to Tendai Ncube;") || !strings.Contains(events, "STAGE_CHANGED:Moved to Assigned;") {
		t.Fatalf("timeline: %s", events)
	}
	// The assignee sees it; the agent it was routed to no longer does.
	if detail := decode[receivedBody](t, e.get("/v1/agent/property-requests/"+id, c.as(agentEmail))); detail.Request.CanAssign {
		t.Fatal("an agent cannot assign")
	}
	wantError(t, e.get("/v1/agent/property-requests/"+id, c.as(agent2Email)), http.StatusNotFound, propertyrequests.CodeRequestNotFound)
	inbox := decode[struct {
		Items []struct {
			RequestID  string `json:"request_id"`
			AssignedTo *struct {
				UserID string `json:"user_id"`
			} `json:"assigned_to"`
		} `json:"items"`
	}](t, e.get("/v1/agent/property-requests", c.as(agentEmail)))
	found := false
	for _, it := range inbox.Items {
		if it.RequestID == id && it.AssignedTo != nil && it.AssignedTo.UserID == identity.SeedAgentUserID.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("the assignee's inbox does not show the request")
	}
	// The assignee gets an email: one outbox event.
	if n := countEvents(e.requests.Outbox(), propertyrequests.EventRequestAssigned); n != 1 {
		t.Fatalf("%d assignment events, want 1", n)
	}
	// Assigning to the same person again changes nothing.
	if again := assign(c.as(ownerEmail), identity.SeedAgentUserID.String()); again == nil || len(again.Request.Timeline) != len(got.Request.Timeline) {
		t.Fatal("re-assigning to the same person must change nothing")
	}
	// To the owner, then back to the organisation inbox.
	if got := assign(c.as(ownerEmail), devseed.EstatesOwnerID.String()); got == nil || got.Request.AssignedTo == nil || got.Request.AssignedTo.Name != "Rudo Moyo" {
		t.Fatalf("assigned to the owner: %+v", got)
	}
	back := assign(c.as(ownerEmail), nil)
	if back == nil || back.Request.AssignedTo != nil || back.Request.Stage != propertyrequests.StageAssigned {
		t.Fatalf("unassigned: %+v", back)
	}
	wantStatus(t, e.get("/v1/agent/property-requests/"+id, c.as(agent2Email)), http.StatusOK)
	if n := countEvents(e.requests.Outbox(), propertyrequests.EventRequestAssigned); n != 2 {
		t.Fatalf("%d assignment events, want 2 (no email for an unassignment)", n)
	}
	acts := decode[auditBody](t, e.get(teamPath("/audit"), c.as(managerEmail))).actions()
	if strings.Join(acts, ",") != "REQUEST_UNASSIGNED,REQUEST_ASSIGNED,REQUEST_ASSIGNED" {
		t.Fatalf("assignment audit: %v", acts)
	}
	// A direct seller's request cannot be assigned.
	sellerReq := e.sent(e.propertyRequest(6))
	wantError(t, e.post("/v1/agent/property-requests/"+sellerReq+"/assign", obj{"user_id": nil}, c.as(devseed.SellerEmail)), http.StatusForbidden, httpx.CodeForbidden)
}

func countEvents(events []propertyrequests.MemoryOutboxEvent, eventType string) int {
	n := 0
	for _, ev := range events {
		if ev.EventType == eventType {
			n++
		}
	}
	return n
}

// Join requests: the agency's OWNER (and MANAGER for agents and staff)
// decides them as a platform reviewer would; whoever is first wins.
func TestJoinRequests(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	if err := devseed.SeedMemoryTeams(e.t.Context(), e.identity, e.onboard, e.team, e.files, identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}, seedPassword, e.now); err != nil {
		t.Fatal(err)
	}
	type joinList struct {
		Items []struct {
			ID             string `json:"id"`
			Role           string `json:"role"`
			MembershipRole string `json:"membership_role"`
			CanDecide      bool   `json:"can_decide"`
			Applicant      struct {
				Name        string `json:"name"`
				EmailMasked string `json:"email_masked"`
			} `json:"applicant"`
		} `json:"items"`
	}
	list := decode[joinList](t, e.get(teamPath("/join-requests"), c.as(managerEmail)))
	if len(list.Items) != 1 || list.Items[0].ID != devseed.JoiningAgentApplicationID.String() || !list.Items[0].CanDecide ||
		list.Items[0].MembershipRole != auth.RoleAgent || strings.Contains(list.Items[0].Applicant.EmailMasked, "joining") {
		t.Fatalf("join requests: %+v", list)
	}
	// Another agency sees none of it.
	other := "/v1/agent/organisations/" + devseed.SampleCommercial.ID.String() + "/join-requests"
	if l := decode[joinList](t, e.get(other, c.as(devseed.CommercialAgentEmail))); len(l.Items) != 0 {
		t.Fatalf("another agency sees %+v", l)
	}
	wantError(t, e.post(other+"/"+devseed.JoiningAgentApplicationID.String()+"/approve", nil, c.as(devseed.CommercialAgentEmail)), http.StatusNotFound, teams.CodeJoinRequestNotFound)
	wantError(t, e.post(teamPath("/join-requests/"+devseed.JoiningAgentApplicationID.String()+"/approve"), nil, c.as(agentEmail)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(teamPath("/join-requests/"+devseed.JoiningAgentApplicationID.String()+"/decline"), obj{"reason": "no"}, c.as(managerEmail)), http.StatusBadRequest, httpx.CodeValidationFailed)

	// The manager approves: membership, agent profile, active role, email.
	joiner := c.as(devseed.JoiningAgentEmail)
	wantError(t, e.get("/v1/agent/property-requests", joiner), http.StatusForbidden, httpx.CodeForbidden)
	rec := e.post(teamPath("/join-requests/"+devseed.JoiningAgentApplicationID.String()+"/approve"), nil, c.as(managerEmail))
	wantStatus(t, rec, http.StatusOK)
	if s := decode[struct {
		JoinRequest struct {
			Status    string `json:"status"`
			CanDecide bool   `json:"can_decide"`
		} `json:"join_request"`
	}](t, rec).JoinRequest; s.Status != onboarding.StatusApproved || s.CanDecide {
		t.Fatalf("after approving: %+v", s)
	}
	wantStatus(t, e.get("/v1/agent/property-requests", joiner), http.StatusOK)
	if m := e.members(c.as(ownerEmail)).find(devseed.JoiningAgentID); m == nil || m.Role != auth.RoleAgent || m.JobTitle == nil || *m.JobTitle != "Negotiator" {
		t.Fatalf("the new member: %+v", m)
	}
	if !hasEvent(e.onboard, onboarding.EventApproved) {
		t.Fatal("the applicant is not emailed about the approval")
	}
	// The platform reviewer comes second: 409, and the other way round too.
	admin := e.adminLogin(identity.SeedAdminEmail, seedPassword)
	wantError(t, e.post("/v1/admin/onboarding/"+devseed.JoiningAgentApplicationID.String()+"/approve", nil, browser(admin)), http.StatusConflict, onboarding.CodeInvalidState)
	wantError(t, e.post(teamPath("/join-requests/"+devseed.JoiningAgentApplicationID.String()+"/approve"), nil, c.as(ownerEmail)), http.StatusConflict, teams.CodeJoinRequestDecided)
	if l := decode[joinList](t, e.get(teamPath("/join-requests"), c.as(ownerEmail))); len(l.Items) != 0 {
		t.Fatalf("a decided join request is still listed: %+v", l)
	}

	// A request to become MANAGER: only the owner may decide it; the owner
	// declines it with a reason, and the platform reviewer then gets 409.
	appID := e.submittedJoinRequest("joining.admin@example.com", identity.AccountRoleAgencyAdmin)
	if l := decode[joinList](t, e.get(teamPath("/join-requests"), c.as(managerEmail))); len(l.Items) != 1 || l.Items[0].CanDecide {
		t.Fatalf("a manager cannot decide a MANAGER join request: %+v", l)
	}
	wantError(t, e.post(teamPath("/join-requests/"+appID.String()+"/approve"), nil, c.as(managerEmail)), http.StatusForbidden, teams.CodeRoleNotAllowed)
	rec = e.post(teamPath("/join-requests/"+appID.String()+"/decline"), obj{"reason": "We do not know this person."}, c.as(ownerEmail))
	wantStatus(t, rec, http.StatusOK)
	wantError(t, e.post("/v1/admin/onboarding/"+appID.String()+"/reject", obj{"reason": "Not verified here"}, browser(admin)), http.StatusConflict, onboarding.CodeInvalidState)
	// And when the reviewer is first, the agency gets 409.
	third := e.submittedJoinRequest("joining.staff@example.com", identity.AccountRoleAgencyStaff)
	wantStatus(t, e.post("/v1/admin/onboarding/"+third.String()+"/reject", obj{"reason": "Documents unreadable"}, browser(admin)), http.StatusOK)
	wantError(t, e.post(teamPath("/join-requests/"+third.String()+"/approve"), nil, c.as(managerEmail)), http.StatusConflict, teams.CodeJoinRequestDecided)

	audit := decode[auditBody](t, e.get(teamPath("/audit"), c.as(ownerEmail)))
	if strings.Join(audit.actions(), ",") != "JOIN_REQUEST_DECLINED,JOIN_REQUEST_DECLINED,JOIN_REQUEST_APPROVED" {
		t.Fatalf("join audit: %v", audit.actions())
	}
	if audit.Items[0].After["decided_by"] != "PLATFORM" || audit.Items[1].After["decided_by"] != "ORGANISATION" {
		t.Fatalf("who decided: %+v / %+v", audit.Items[0].After, audit.Items[1].After)
	}
}

func hasEvent(store *onboarding.MemoryStore, eventType string) bool {
	for _, ev := range store.Outbox() {
		if ev.Type == eventType {
			return true
		}
	}
	return false
}

// submittedJoinRequest creates an account whose application for role (an
// agency member role) to join Sample Estates is SUBMITTED.
func (e *env) submittedJoinRequest(email, role string) uuid.UUID {
	e.t.Helper()
	userID := uuid.New()
	verified := e.now
	if err := e.identity.CreateUser(e.t.Context(), identity.NewUser{
		ID: userID, Name: "Applicant " + role, Email: email, PasswordHash: "x", PrivacyVersion: "2026-01",
		AccountRoles: []identity.AccountRole{{Role: role, Status: identity.AccountRolePendingVerification}}, EmailVerifiedAt: &verified, CreatedAt: e.now,
	}); err != nil {
		e.t.Fatal(err)
	}
	apps, err := e.onboard.Current(e.t.Context(), userID)
	if err != nil || len(apps) != 1 {
		e.t.Fatalf("no draft application: %v", err)
	}
	app := apps[0]
	slug, phone, at := "sample-estates", "+263770000009", e.now
	app.Status, app.AgencySlug, app.ContactPhone, app.MarketCode, app.SubmittedAt = onboarding.StatusSubmitted, &slug, &phone, devseed.MarketCode, &at
	e.onboard.PutApplication(app)
	return app.ID
}

// A reviewer cannot decide a listing of their own organisation, or one they
// sell, are the agent of, created or submitted.
func TestListingSelfReviewIsForbidden(t *testing.T) {
	e := newEnv(t)
	pending := devseed.PendingListingID.String()
	moderator := e.platformAdmin("moderator.self@example.test", auth.PlatformModerator, "")
	// The moderator also works for Sample Estates (whose listing it is).
	u, err := e.identity.UserByEmail(e.t.Context(), "moderator.self@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.identity.AddMembership(u.ID, devseed.SampleEstates.ID, devseed.MarketCode, auth.RoleStaff); err != nil {
		t.Fatal(err)
	}
	for _, decision := range []string{"approve", "request-changes", "reject"} {
		wantError(t, e.post("/v1/admin/listings/"+pending+"/"+decision, obj{"reason": "Looks fine to me"}, browser(moderator)),
			http.StatusForbidden, listings.CodeSelfReviewForbidden)
	}
	// Once they leave the organisation (removed by its owner), they may.
	wantStatus(t, e.deleteReq(teamPath("/members/"+u.ID.String()), newCast(e).as(ownerEmail)), http.StatusNoContent)
	wantStatus(t, e.post("/v1/admin/listings/"+pending+"/approve", nil, browser(moderator)), http.StatusOK)
}

// The dashboard says who may manage the team.
func TestDashboardCanManageTeam(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	for email, want := range map[string]bool{ownerEmail: true, managerEmail: true, agentEmail: false, staffEmail: false} {
		d := decode[struct {
			Organisations []struct {
				CanManageTeam bool `json:"can_manage_team"`
			} `json:"organisations"`
		}](t, e.get("/v1/me/dashboard", c.as(email)))
		if len(d.Organisations) != 1 || d.Organisations[0].CanManageTeam != want {
			t.Fatalf("%s: %+v", email, d.Organisations)
		}
	}
}
