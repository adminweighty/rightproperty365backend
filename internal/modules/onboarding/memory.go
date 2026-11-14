package onboarding

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryDeps are the other in-memory stores an approval writes to. In
// Postgres all of this is one database and one transaction; here the stores
// are separate, so an approval is applied step by step under this store's
// lock, with every check done before the first write.
type MemoryDeps struct {
	Identity  *identity.MemoryStore
	Directory *directory.MemoryStore
	Enquiries *businessenquiries.MemoryStore
	// MarketID returns the ID of a market code (enquiry routing needs it).
	MarketID func(code string) (uuid.UUID, bool)
}

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct {
	deps MemoryDeps

	mu           sync.Mutex
	outcomes     []RoleOutcome
	requirements []Requirement
	apps         map[uuid.UUID]*Application
	history      map[uuid.UUID][]HistoryEntry
	outbox       []outbox.Event
	sink         outbox.Sink
}

// NewMemoryStore returns a store holding the baseline outcome and
// requirement tables. It registers itself with the identity store so that
// registration creates DRAFT applications and GET /v1/me can show each
// role's onboarding status.
func NewMemoryStore(deps MemoryDeps) *MemoryStore {
	s := &MemoryStore{
		deps: deps, outcomes: DefaultOutcomes(), requirements: DefaultRequirements(),
		apps: map[uuid.UUID]*Application{}, history: map[uuid.UUID][]HistoryEntry{},
	}
	deps.Identity.SetOnboarding(s.syncDrafts, s.statuses)
	return s
}

// SetOutboxSink registers a receiver for events as they are written.
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
}

// Outbox returns a copy of the outbox events (test helper).
func (s *MemoryStore) Outbox() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event(nil), s.outbox...)
}

// SetRequirement adds or changes a role's document requirement (what an
// administrator's edit of the table would do).
func (s *MemoryStore) SetRequirement(r Requirement) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.requirements {
		if s.requirements[i].Role == r.Role && s.requirements[i].Document == r.Document {
			s.requirements[i] = r
			return
		}
	}
	s.requirements = append(s.requirements, r)
}

// PutApplication inserts or replaces an application as it is, with history
// rows, without events (seed and test helper).
func (s *MemoryStore) PutApplication(a Application, history ...HistoryEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := a
	s.apps[a.ID] = &cp
	s.history[a.ID] = append(s.history[a.ID], history...)
}

// RemoveApplication deletes an application and its history (seed and test helper).
func (s *MemoryStore) RemoveApplication(id uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.apps, id)
	delete(s.history, id)
}

func (s *MemoryStore) emit(event string, id uuid.UUID, at time.Time) {
	ev := outbox.Event{
		ID: uuid.New(), Type: event, AggregateType: AggregateApplication, AggregateID: id,
		Payload:  fmt.Appendf(nil, `{"application_id":%q,"occurred_at":%q}`, id, at.UTC().Format(time.RFC3339Nano)),
		Attempts: 1, CreatedAt: at,
	}
	s.outbox = append(s.outbox, ev)
	if s.sink != nil {
		s.sink(ev)
	}
}

func (s *MemoryStore) hasOutcome(role string) bool {
	return slices.ContainsFunc(s.outcomes, func(o RoleOutcome) bool { return o.Role == role })
}

// newest returns the user's newest application per role. Caller holds the lock.
func (s *MemoryStore) newest(userID uuid.UUID) map[string]*Application {
	out := map[string]*Application{}
	for _, a := range s.apps {
		if a.UserID != userID {
			continue
		}
		if cur, ok := out[a.Role]; !ok || a.CreatedAt.After(cur.CreatedAt) || (a.CreatedAt.Equal(cur.CreatedAt) && a.ID.String() > cur.ID.String()) {
			out[a.Role] = a
		}
	}
	return out
}

// syncDrafts is called by the identity store when an account's roles are set
// at registration: every role waiting for verification gets a DRAFT
// application, and drafts for roles that were dropped are removed.
func (s *MemoryStore) syncDrafts(userID uuid.UUID, roles []identity.AccountRole, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending := map[string]bool{}
	for _, r := range roles {
		if r.Status == identity.AccountRolePendingVerification && s.hasOutcome(r.Role) {
			pending[r.Role] = true
		}
	}
	for id, a := range s.apps {
		if a.UserID == userID && a.Status == StatusDraft && !pending[a.Role] {
			delete(s.apps, id)
			delete(s.history, id)
		}
	}
	current := s.newest(userID)
	for role := range pending {
		if a, ok := current[role]; ok && a.Status != StatusRejected {
			continue
		}
		id, err := uuid.NewV7()
		if err != nil {
			id = uuid.New()
		}
		s.apps[id] = &Application{ID: id, UserID: userID, Role: role, Status: StatusDraft, CreatedAt: now, UpdatedAt: now}
	}
}

// statuses returns the status of the user's newest application per role.
func (s *MemoryStore) statuses(userID uuid.UUID) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for role, a := range s.newest(userID) {
		out[role] = a.Status
	}
	return out
}

// copyOf returns a deep copy with the applicant attached. Caller holds the lock.
func (s *MemoryStore) copyOf(a *Application) Application {
	out := *a
	out.Areas = append([]string(nil), a.Areas...)
	out.Services = append([]string(nil), a.Services...)
	out.Documents = append([]Document(nil), a.Documents...)
	sort.SliceStable(out.Documents, func(i, j int) bool { return out.Documents[i].UploadedAt.After(out.Documents[j].UploadedAt) })
	if u, ok := s.deps.Identity.Account(a.UserID); ok {
		out.Applicant = Applicant{ID: u.ID, Name: u.Name, Email: u.Email, EmailVerified: u.Verified(), CreatedAt: u.CreatedAt}
	}
	return out
}

// Outcomes implements Store.
func (s *MemoryStore) Outcomes(context.Context) ([]RoleOutcome, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.outcomes), nil
}

// Requirements implements Store.
func (s *MemoryStore) Requirements(context.Context) ([]Requirement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requirements), nil
}

// Current implements Store.
func (s *MemoryStore) Current(_ context.Context, userID uuid.UUID) ([]Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Application
	for _, a := range s.newest(userID) {
		out = append(out, s.copyOf(a))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Role < out[j].Role })
	return out, nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id uuid.UUID) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return Application{}, ErrNotFound
	}
	return s.copyOf(a), nil
}

// Reapply implements Store.
func (s *MemoryStore) Reapply(_ context.Context, app Application) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.newest(app.UserID)[app.Role]
	if !ok || cur.Status != StatusRejected {
		return ErrInvalidState
	}
	if err := s.deps.Identity.SetAccountRoleStatus(app.UserID, app.Role, RoleStatusPending); err != nil {
		return err
	}
	cp := app
	cp.Status = StatusDraft
	s.apps[app.ID] = &cp
	return nil
}

func (s *MemoryStore) editable(id uuid.UUID) (*Application, error) {
	a, ok := s.apps[id]
	if !ok {
		return nil, ErrNotFound
	}
	if !a.Editable() {
		return nil, ErrInvalidState
	}
	return a, nil
}

// SaveDetails implements Store.
func (s *MemoryStore) SaveDetails(_ context.Context, id uuid.UUID, d Details, marketCode, agencyName string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.editable(id)
	if err != nil {
		return err
	}
	a.Details, a.MarketCode, a.AgencyName, a.UpdatedAt = d, marketCode, agencyName, now
	return nil
}

// AddDocument implements Store.
func (s *MemoryStore) AddDocument(_ context.Context, doc Document) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.editable(doc.ApplicationID)
	if err != nil {
		return nil, err
	}
	var kept []Document
	var replaced []string
	for _, d := range a.Documents {
		if d.Type == doc.Type {
			replaced = append(replaced, d.ObjectKey)
		} else {
			kept = append(kept, d)
		}
	}
	if len(kept) >= MaxDocumentsPerApp {
		return nil, ErrTooManyDocuments
	}
	a.Documents, a.UpdatedAt = append(kept, doc), doc.UploadedAt
	return replaced, nil
}

// DeleteDocument implements Store.
func (s *MemoryStore) DeleteDocument(_ context.Context, applicationID, documentID uuid.UUID, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, err := s.editable(applicationID)
	if err != nil {
		return "", err
	}
	for i, d := range a.Documents {
		if d.ID == documentID {
			a.Documents = append(a.Documents[:i:i], a.Documents[i+1:]...)
			a.UpdatedAt = now
			return d.ObjectKey, nil
		}
	}
	return "", ErrDocumentNotFound
}

func (s *MemoryStore) record(a *Application, to string, actor uuid.UUID, reason string, at time.Time) {
	name := ""
	if u, ok := s.deps.Identity.Account(actor); ok {
		name = u.Name
	}
	s.history[a.ID] = append(s.history[a.ID], HistoryEntry{From: a.Status, To: to, ActorID: actor, ActorName: name, Reason: reason, At: at})
	a.Status, a.UpdatedAt = to, at
	if to == StatusSubmitted {
		a.SubmittedAt, a.DecisionReason, a.DecidedAt = &at, "", nil
	} else {
		a.DecisionReason, a.DecidedAt = reason, &at
	}
}

// Transition implements Store.
func (s *MemoryStore) Transition(_ context.Context, t Transition) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[t.ApplicationID]
	if !ok {
		return ErrNotFound
	}
	if !slices.Contains(t.From, a.Status) {
		return ErrInvalidState
	}
	if t.AccountRoleStatus != "" {
		if err := s.deps.Identity.SetAccountRoleStatus(a.UserID, a.Role, t.AccountRoleStatus); err != nil {
			return err
		}
	}
	s.record(a, t.To, t.ActorID, t.Reason, t.At)
	s.emit(t.Event, a.ID, t.At)
	return nil
}

// Approve implements Store.
func (s *MemoryStore) Approve(ctx context.Context, p Approval) (Created, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[p.ApplicationID]
	if !ok {
		return Created{}, ErrNotFound
	}
	if a.Status != StatusSubmitted {
		return Created{}, ErrInvalidState
	}
	// Every check first, so nothing is written when the approval cannot complete.
	if p.AgencySlug != "" {
		if _, err := s.deps.Directory.Get(ctx, KindAgencies, p.AgencySlug); err != nil {
			return Created{}, ErrAgencyNotVerified
		}
	}
	marketCode := ""
	if p.Membership != nil {
		marketCode = p.Membership.MarketCode
	}
	var marketID uuid.UUID
	if p.Profile != nil {
		id, ok := s.deps.MarketID(p.Profile.MarketCode)
		if !ok {
			return Created{}, fmt.Errorf("approve onboarding: unknown market %q", p.Profile.MarketCode)
		}
		marketID = id
	}

	var created Created
	if p.Organisation != nil {
		s.deps.Identity.PutOrganisation(identity.MemoryOrganisation{ID: p.Organisation.ID, Name: p.Organisation.Name, Kind: p.Organisation.Kind, MarketCode: p.Organisation.MarketCode})
		id := p.Organisation.ID
		created.OrganisationID = &id
	}
	if p.Membership != nil {
		if err := s.deps.Identity.SetMembership(p.UserID, p.Membership.OrganisationID, marketCode, p.Membership.Role); err != nil {
			return Created{}, err
		}
		role := p.Membership.Role
		created.MembershipRole = &role
	}
	if p.Profile != nil {
		slug := p.Profile.Slug
		for n := 2; s.deps.Directory.Has(p.Profile.Kind, slug); n++ {
			slug = fmt.Sprintf("%s-%d", p.Profile.Slug, n)
		}
		s.deps.Directory.Put(directory.Record{
			ID: p.Profile.ID, Kind: p.Profile.Kind, Slug: slug, Name: p.Profile.Name, Role: p.Profile.RoleTitle,
			Tagline: p.Profile.Tagline, Description: p.Profile.Description, CityCode: p.Profile.CityCode, CityName: p.Profile.CityName,
			MarketCode: p.Profile.MarketCode, TradeCode: p.Profile.TradeCode,
			Areas: append([]string{}, p.Profile.Areas...), Services: append([]string{}, p.Profile.Services...),
			Verified: true, VerifiedAt: &p.At, Status: directory.StatusActive, UpdatedAt: p.At, Specialism: p.Profile.Specialism,
			OrganisationID: p.Profile.OrganisationID, AgentUserID: p.Profile.AgentUserID, Logo: p.Profile.Logo,
		})
		s.deps.Enquiries.PutProfile(businessenquiries.Profile{
			ID: p.Profile.ID, Kind: p.Profile.Kind, Slug: slug, Name: p.Profile.Name, MarketID: marketID,
			Status: businessenquiries.ProfileActive, OrganisationID: p.Profile.OrganisationID, OrganisationEligible: true,
			AgentUserID: p.Profile.AgentUserID, AgentEligible: p.Profile.AgentUserID != nil,
		})
		created.Profile = &ProfileRef{Kind: p.Profile.Kind, Slug: slug}
		a.Profile = created.Profile
	}
	if err := s.deps.Identity.SetAccountRoleStatus(p.UserID, p.Role, RoleStatusActive); err != nil {
		return Created{}, err
	}
	s.record(a, StatusApproved, p.ActorID, p.Note, p.At)
	s.emit(EventApproved, a.ID, p.At)
	return created, nil
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, f ListFilter) ([]Application, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	counts := map[string]int{}
	var found []Application
	q := strings.ToLower(f.Q)
	for _, a := range s.apps {
		if f.Role != "" && a.Role != f.Role {
			continue
		}
		if f.Markets != nil && !slices.Contains(f.Markets, a.MarketCode) {
			continue
		}
		full := s.copyOf(a)
		if q != "" {
			haystack := strings.ToLower(full.Applicant.Name + " " + full.Applicant.Email + " " + deref(full.BusinessName))
			if !strings.Contains(haystack, q) {
				continue
			}
		}
		counts[a.Status]++
		if a.Status == f.Status {
			found = append(found, full)
		}
	}
	key := func(a Application) time.Time {
		if a.SubmittedAt != nil {
			return *a.SubmittedAt
		}
		return a.CreatedAt
	}
	sort.SliceStable(found, func(i, j int) bool {
		if !key(found[i]).Equal(key(found[j])) {
			return key(found[i]).Before(key(found[j]))
		}
		return found[i].ID.String() < found[j].ID.String()
	})
	total := len(found)
	if f.Offset > total {
		f.Offset = total
	}
	found = found[f.Offset:]
	if len(found) > f.Limit {
		found = found[:f.Limit]
	}
	return found, total, counts, nil
}

// History implements Store.
func (s *MemoryStore) History(_ context.Context, id uuid.UUID) ([]HistoryEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]HistoryEntry(nil), s.history[id]...), nil
}
