package teams

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// MemoryDeps are the other in-memory stores a team change writes to. In
// Postgres it is all one database and one transaction; here a change is
// applied step by step under this store's lock, every check first.
type MemoryDeps struct {
	Identity     *identity.MemoryStore
	Directory    *directory.MemoryStore
	Enquiries    *businessenquiries.MemoryStore
	Requests     *propertyrequests.MemoryStore
	Appointments *appointments.MemoryStore
	// Onboarding, when set, sends its join-decision audit records here.
	Onboarding *onboarding.MemoryStore
	// MarketID returns the ID of a market code (enquiry routing needs it).
	MarketID func(code string) (uuid.UUID, bool)
}

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct {
	deps MemoryDeps
	log  *orgaudit.MemoryLog

	mu          sync.Mutex
	invitations map[uuid.UUID]*Invitation
}

// NewMemoryStore returns an empty store. It receives the audit records the
// request and onboarding stores write (assignments, join decisions).
func NewMemoryStore(deps MemoryDeps) *MemoryStore {
	s := &MemoryStore{deps: deps, log: &orgaudit.MemoryLog{}, invitations: map[uuid.UUID]*Invitation{}}
	if deps.Requests != nil {
		deps.Requests.SetAuditSink(s.log.Append)
	}
	if deps.Onboarding != nil {
		deps.Onboarding.SetAuditSink(s.log.Append)
	}
	return s
}

// AuditLog returns the audit records (test helper).
func (s *MemoryStore) AuditLog() []orgaudit.Event { return s.log.All() }

// PutInvitation stores an invitation as it is, without an audit record (seed helper).
func (s *MemoryStore) PutInvitation(inv Invitation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := inv
	s.invitations[inv.ID] = &cp
}

func (s *MemoryStore) member(m identity.MemoryMembership) (Member, bool) {
	u, ok := s.deps.Identity.Account(m.UserID)
	if !ok || m.Status == identity.MembershipRemoved {
		return Member{}, false
	}
	out := Member{
		UserID: u.ID, Name: u.Name, Email: u.Email, Role: m.Role, MarketCode: m.MarketCode, Status: m.Status,
		AccountActive: u.Status == identity.StatusActive, JobTitle: m.JobTitle,
	}
	if !m.JoinedAt.IsZero() {
		joined := m.JoinedAt.UTC()
		out.JoinedAt = &joined
	}
	return out, true
}

// Members implements Store.
func (s *MemoryStore) Members(_ context.Context, organisationID uuid.UUID) ([]Member, error) {
	out := []Member{}
	for _, m := range s.deps.Identity.OrganisationMemberships(organisationID) {
		if member, ok := s.member(m); ok {
			out = append(out, member)
		}
	}
	return out, nil
}

// MemberByEmail implements Store.
func (s *MemoryStore) MemberByEmail(ctx context.Context, organisationID uuid.UUID, email string) (*Member, error) {
	u, err := s.deps.Identity.UserByEmail(ctx, email)
	if err != nil {
		return nil, nil //nolint:nilerr // no such account: no member
	}
	m, ok := s.deps.Identity.MembershipOf(organisationID, u.ID)
	if !ok {
		return nil, nil
	}
	member, ok := s.member(m)
	if !ok {
		return nil, nil
	}
	return &member, nil
}

// OpenLeads implements Store.
func (s *MemoryStore) OpenLeads(ctx context.Context, organisationID uuid.UUID) (map[uuid.UUID]int, error) {
	return s.deps.Requests.OpenLeads(ctx, organisationID)
}

func (s *MemoryStore) withOrganisation(inv Invitation) Invitation {
	if o, ok := s.deps.Identity.Organisation(inv.OrganisationID); ok {
		inv.OrganisationName, inv.OrganisationKind = o.Name, o.Kind
	}
	inv.TokenHash = slices.Clone(inv.TokenHash)
	return inv
}

// CreateInvitation implements Store.
func (s *MemoryStore) CreateInvitation(_ context.Context, inv Invitation, audit orgaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, old := range s.invitations {
		if old.OrganisationID == inv.OrganisationID && old.Email == inv.Email && old.Status == InvitationPending {
			old.Status = InvitationReplaced
		}
	}
	cp := inv
	s.invitations[inv.ID] = &cp
	s.log.Append(audit)
	return nil
}

// Invitations implements Store.
func (s *MemoryStore) Invitations(_ context.Context, organisationID uuid.UUID) ([]Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Invitation{}
	for _, inv := range s.invitations {
		if inv.OrganisationID == organisationID && inv.Status == InvitationPending {
			out = append(out, s.withOrganisation(*inv))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Invitation implements Store.
func (s *MemoryStore) Invitation(_ context.Context, organisationID, id uuid.UUID) (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invitations[id]
	if !ok || inv.OrganisationID != organisationID {
		return Invitation{}, ErrInvitationNotFound
	}
	return s.withOrganisation(*inv), nil
}

func (s *MemoryStore) byToken(tokenHash []byte) (*Invitation, bool) {
	for _, inv := range s.invitations {
		if bytes.Equal(inv.TokenHash, tokenHash) {
			return inv, true
		}
	}
	return nil, false
}

// InvitationByToken implements Store.
func (s *MemoryStore) InvitationByToken(_ context.Context, tokenHash []byte) (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.byToken(tokenHash)
	if !ok {
		return Invitation{}, ErrInvitationNotFound
	}
	return s.withOrganisation(*inv), nil
}

// CancelInvitation implements Store.
func (s *MemoryStore) CancelInvitation(_ context.Context, organisationID, id uuid.UUID, by Actor, at time.Time,
	allow func(Invitation) error, audit func(Invitation) orgaudit.Event) (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invitations[id]
	if !ok || inv.OrganisationID != organisationID {
		return Invitation{}, ErrInvitationNotFound
	}
	if err := allow(*inv); err != nil {
		return Invitation{}, err
	}
	if inv.Status != InvitationPending {
		return Invitation{}, ErrInvitationUsed
	}
	inv.Status, inv.CancelledAt = InvitationCancelled, &at
	s.log.Append(audit(*inv))
	return s.withOrganisation(*inv), nil
}

// AcceptInvitation implements Store.
func (s *MemoryStore) AcceptInvitation(_ context.Context, a Acceptance) (Accepted, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.byToken(a.TokenHash)
	if !ok {
		return Accepted{}, ErrInvitationNotFound
	}
	inv := s.withOrganisation(*stored)
	user, ok := s.deps.Identity.Account(a.Account.ID)
	if !ok {
		return Accepted{}, fmt.Errorf("accept invitation: %w", identity.ErrNotFound)
	}
	state := AccountState{}
	for _, r := range user.AccountRoles {
		state.Seeker = state.Seeker || identity.IsSeekerRole(r.Role)
	}
	if m, ok := s.deps.Identity.MembershipOf(inv.OrganisationID, user.ID); ok {
		if member, ok := s.member(m); ok {
			state.Membership = &member
		}
	}
	if err := a.Check(inv, state); err != nil {
		return Accepted{}, err
	}
	plan := a.Plan(inv)
	org, ok := s.deps.Identity.Organisation(inv.OrganisationID)
	if !ok {
		return Accepted{}, ErrInvitationNotFound
	}
	var marketID uuid.UUID
	if plan.AgentProfile {
		if marketID, ok = s.deps.MarketID(org.MarketCode); !ok {
			return Accepted{}, fmt.Errorf("accept invitation: unknown market %q", org.MarketCode)
		}
	}

	if err := s.deps.Identity.SetMembershipWithTitle(user.ID, org.ID, org.MarketCode, inv.Role, inv.JobTitle, a.At); err != nil {
		return Accepted{}, err
	}
	if plan.AccountRole != "" {
		if err := s.deps.Identity.GrantAccountRole(user.ID, plan.AccountRole, identity.AccountRoleActive); err != nil {
			return Accepted{}, err
		}
	}
	out := Accepted{}
	if plan.AgentProfile {
		out.Profile = s.agentProfile(org, user, plan.ProfileTitle, marketID, a.At)
	}
	s.setEligible(org.ID, user.ID, true)
	stored.Status, stored.AcceptedAt = InvitationAccepted, &a.At
	s.log.Append(plan.Audit)
	out.Invitation = s.withOrganisation(*stored)
	return out, nil
}

// agentProfile gives the new agent a public profile linked to the agency (or
// reactivates the one they had), as an approved onboarding application does.
func (s *MemoryStore) agentProfile(org identity.MemoryOrganisation, user identity.User, title string, marketID uuid.UUID, at time.Time) *ProfileRef {
	if existing, ok := s.deps.Directory.AgentProfile(user.ID); ok && existing.OrganisationID == org.ID {
		existing.Status, existing.UpdatedAt = directory.StatusActive, at
		s.deps.Directory.Put(existing)
		return &ProfileRef{Kind: existing.Kind, Slug: existing.Slug}
	}
	city := ""
	if agency, ok := s.deps.Directory.OwnProfile(org.ID); ok {
		city = agency.CityCode
	}
	base := onboarding.Slugify(user.Name)
	slug := base
	for n := 2; s.deps.Directory.Has(onboarding.KindAgents, slug); n++ {
		slug = fmt.Sprintf("%s-%d", base, n)
	}
	id, err := uuid.NewV7()
	if err != nil {
		id = uuid.New()
	}
	agent := user.ID
	s.deps.Directory.Put(directory.Record{
		ID: id, Kind: onboarding.KindAgents, Slug: slug, Name: user.Name, Role: title, CityCode: city, MarketCode: org.MarketCode,
		Areas: []string{}, Services: []string{}, Verified: true, VerifiedAt: &at, Status: directory.StatusActive, UpdatedAt: at,
		OrganisationID: org.ID, AgentUserID: &agent,
	})
	s.deps.Enquiries.PutProfile(businessenquiries.Profile{
		ID: id, Kind: onboarding.KindAgents, Slug: slug, Name: user.Name, MarketID: marketID,
		Status: businessenquiries.ProfileActive, OrganisationID: org.ID, OrganisationEligible: true,
		AgentUserID: &agent, AgentEligible: true,
	})
	return &ProfileRef{Kind: onboarding.KindAgents, Slug: slug}
}

// ChangeMember implements Store.
func (s *MemoryStore) ChangeMember(_ context.Context, organisationID, userID uuid.UUID, decide func(Member, int) (MemberChange, error)) (Member, Released, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.deps.Identity.MembershipOf(organisationID, userID)
	if !ok {
		return Member{}, Released{}, ErrMemberNotFound
	}
	target, ok := s.member(raw)
	if !ok {
		return Member{}, Released{}, ErrMemberNotFound
	}
	owners := 0
	for _, m := range s.deps.Identity.OrganisationMemberships(organisationID) {
		if member, ok := s.member(m); ok && member.Role == auth.RoleOwner && member.EffectiveStatus() == MemberActive {
			owners++
		}
	}
	change, err := decide(target, owners)
	if err != nil {
		return Member{}, Released{}, err
	}
	next := raw
	if change.Role != nil {
		next.Role = *change.Role
	}
	if change.Status != nil {
		next.Status = *change.Status
	}
	if change.JobTitle != nil {
		next.JobTitle = *change.JobTitle
	}
	if change.Remove {
		next.Status = identity.MembershipRemoved
	}
	if err := s.deps.Identity.UpdateMembership(next); err != nil {
		return Member{}, Released{}, err
	}
	actor := propertyrequests.Actor{UserID: change.Actor.UserID, Name: change.Actor.Name}
	var released Released
	if change.Release >= ReleaseRequests {
		released.Requests = s.deps.Requests.ReleaseMember(organisationID, userID, actor, change.At)
	}
	if change.Release >= ReleaseAll {
		released.Appointments = s.deps.Appointments.ReleaseMember(organisationID, userID, change.Actor.UserID, change.At)
	}
	// New requests for the member's listings go to the organisation inbox
	// while they cannot receive them (Postgres reads this from the membership).
	s.setEligible(organisationID, userID, next.Status == identity.MembershipActive)
	for _, e := range change.Events {
		s.log.Append(withReleased(e, change.Release, released))
	}
	out, _ := s.member(next)
	if change.Remove {
		out = target
		out.Status = MemberRemoved
	}
	return out, released, nil
}

// setEligible says whether the member can receive the requests and enquiries
// routed to them personally (their listings, their agent profile).
func (s *MemoryStore) setEligible(organisationID, userID uuid.UUID, eligible bool) {
	s.deps.Requests.SetAgentEligible(organisationID, userID, eligible)
	s.deps.Enquiries.SetAgentEligible(organisationID, userID, eligible)
}

// withReleased records in an audit event how much work went back to the
// organisation.
func withReleased(e orgaudit.Event, release Release, r Released) orgaudit.Event {
	if release == ReleaseNothing {
		return e
	}
	after := map[string]any{}
	for k, v := range e.After {
		after[k] = v
	}
	after["released_requests"] = r.Requests
	if release == ReleaseAll {
		after["released_appointments"] = r.Appointments
	}
	e.After = after
	return e
}

// Audit implements Store.
func (s *MemoryStore) Audit(_ context.Context, organisationID uuid.UUID, limit, offset int) ([]orgaudit.Event, int, error) {
	items, total := s.log.List(organisationID, limit, offset)
	return items, total, nil
}
