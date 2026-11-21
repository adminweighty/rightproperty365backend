package identity

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is a mutex-guarded in-memory Store for development and tests
// only. Data is lost on restart.
type MemoryStore struct {
	mu            sync.Mutex
	users         map[uuid.UUID]*memoryUser
	byEmail       map[string]uuid.UUID
	organisations map[uuid.UUID]MemoryOrganisation
	memberships   []memoryMembership
	platformRoles map[uuid.UUID][]auth.PlatformRole
	accountRoles  map[uuid.UUID][]AccountRole
	sessions      map[string]*Session // key: hex token hash
	verifications map[string]*memoryToken
	resets        map[string]*memoryToken
	// emailChanges are the pending changes of address (security.go).
	emailChanges map[string]*memoryEmailChange
	captchas     map[string]*memoryCaptcha  // key: captcha id
	challenges   map[string]*LoginChallenge // key: hex id hash
	consents     []MemoryConsent
	marketing    []MarketingConsentEvent // in insertion order
	outbox       []outbox.Event
	sink         outbox.Sink

	// Administrator MFA (mfa_memory.go), keyed by user.
	mfaTOTP     map[uuid.UUID]*AdminMFA
	mfaRecovery map[uuid.UUID][]RecoveryCode
	// adminAudit receives the administrators' audit records this store
	// writes (MFA); nil drops them.
	adminAudit *adminaudit.MemoryLog

	// Onboarding hooks (see SetOnboarding). Never called with mu held.
	syncDrafts         func(userID uuid.UUID, roles []AccountRole, now time.Time)
	onboardingStatuses func(userID uuid.UUID) map[string]string
}

// SetOnboarding connects the in-memory onboarding store: sync is told the
// roles of an account whenever registration sets them (it creates the DRAFT
// applications, which the Postgres store does in the same transaction), and
// statuses supplies each role's onboarding status for reads.
func (s *MemoryStore) SetOnboarding(sync func(userID uuid.UUID, roles []AccountRole, now time.Time), statuses func(userID uuid.UUID) map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncDrafts, s.onboardingStatuses = sync, statuses
}

// decorate attaches the onboarding status of each account role. It is called
// WITHOUT the lock: the onboarding store reads this store in turn.
func (s *MemoryStore) decorate(u User) User {
	s.mu.Lock()
	statuses := s.onboardingStatuses
	s.mu.Unlock()
	if statuses == nil {
		return u
	}
	byRole := statuses(u.ID)
	for i := range u.AccountRoles {
		u.AccountRoles[i].OnboardingStatus = byRole[u.AccountRoles[i].Role]
	}
	return u
}

func (s *MemoryStore) rolesChanged(userID uuid.UUID, roles []AccountRole, now time.Time) {
	s.mu.Lock()
	sync := s.syncDrafts
	s.mu.Unlock()
	if sync != nil {
		sync(userID, append([]AccountRole(nil), roles...), now)
	}
}

// Account returns a user without onboarding statuses (for the onboarding
// store itself, which must not be called back).
func (s *MemoryStore) Account(id uuid.UUID) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, false
	}
	return s.view(u), true
}

// SetAccountRoleStatus changes the status of one of the user's account roles.
func (s *MemoryStore) SetAccountRoleStatus(userID uuid.UUID, role, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, r := range s.accountRoles[userID] {
		if r.Role == role {
			s.accountRoles[userID][i].Status = status
			return nil
		}
	}
	return fmt.Errorf("set account role status: %w", ErrNotFound)
}

// SetMembership makes the user an ACTIVE member of an organisation with the
// given role, replacing an existing membership of the same organisation (a
// removed or suspended one becomes active again).
func (s *MemoryStore) SetMembership(userID, organisationID uuid.UUID, marketCode, role string) error {
	return s.SetMembershipWithTitle(userID, organisationID, marketCode, role, "", time.Time{})
}

// SetMembershipWithTitle is SetMembership with a job title ("" for none) and
// the time it happened (the zero time keeps the original joining time).
func (s *MemoryStore) SetMembershipWithTitle(userID, organisationID uuid.UUID, marketCode, role, jobTitle string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("set membership: %w", ErrNotFound)
	}
	if _, ok := s.organisations[organisationID]; !ok {
		return fmt.Errorf("set membership: organisation %s: %w", organisationID, ErrNotFound)
	}
	for i, m := range s.memberships {
		if m.UserID == userID && m.OrganisationID == organisationID {
			if m.Status != MembershipActive && !at.IsZero() {
				s.memberships[i].JoinedAt = at
			}
			s.memberships[i].Role, s.memberships[i].MarketCode, s.memberships[i].Status = role, marketCode, MembershipActive
			if jobTitle != "" {
				s.memberships[i].JobTitle = jobTitle
			}
			return nil
		}
	}
	s.memberships = append(s.memberships, memoryMembership{UserID: userID, OrganisationID: organisationID, MarketCode: marketCode, Role: role,
		Status: MembershipActive, JobTitle: jobTitle, JoinedAt: at})
	return nil
}

// Membership statuses (memberships.status). Only an ACTIVE membership gives
// access; a SUSPENDED one is kept (it can be reactivated) and a REMOVED one is
// no longer listed.
const (
	MembershipActive    = "ACTIVE"
	MembershipSuspended = "SUSPENDED"
	MembershipRemoved   = "REMOVED"
)

// MemoryMembership is one membership row (for the team store).
type MemoryMembership struct {
	UserID         uuid.UUID
	OrganisationID uuid.UUID
	MarketCode     string
	Role           string
	Status         string
	JobTitle       string
	JoinedAt       time.Time
}

// MembershipOf returns the user's membership of an organisation, in any status.
func (s *MemoryStore) MembershipOf(organisationID, userID uuid.UUID) (MemoryMembership, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.memberships {
		if m.UserID == userID && m.OrganisationID == organisationID {
			return MemoryMembership(m), true
		}
	}
	return MemoryMembership{}, false
}

// OrganisationMemberships returns every membership of an organisation, in any status.
func (s *MemoryStore) OrganisationMemberships(organisationID uuid.UUID) []MemoryMembership {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MemoryMembership
	for _, m := range s.memberships {
		if m.OrganisationID == organisationID {
			out = append(out, MemoryMembership(m))
		}
	}
	return out
}

// UpdateMembership replaces the role, status and job title of an existing
// membership. ErrNotFound when there is none.
func (s *MemoryStore) UpdateMembership(m MemoryMembership) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, cur := range s.memberships {
		if cur.UserID == m.UserID && cur.OrganisationID == m.OrganisationID {
			s.memberships[i].Role, s.memberships[i].Status, s.memberships[i].JobTitle = m.Role, m.Status, m.JobTitle
			return nil
		}
	}
	return fmt.Errorf("update membership: %w", ErrNotFound)
}

// GrantAccountRole sets one of the user's account roles to status, adding the
// role when the user does not have it.
func (s *MemoryStore) GrantAccountRole(userID uuid.UUID, role, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("grant account role: %w", ErrNotFound)
	}
	for i, r := range s.accountRoles[userID] {
		if r.Role == role {
			s.accountRoles[userID][i].Status = status
			return nil
		}
	}
	s.accountRoles[userID] = append(s.accountRoles[userID], AccountRole{Role: role, Status: status})
	return nil
}

type memoryUser struct {
	User
	MarketingOptIn bool
	// Account administration (memory_admin.go).
	SuspendedAt      *time.Time
	SuspensionReason string
	SuspendedBy      *uuid.UUID
	DeletedAt        *time.Time
}

type memoryMembership struct {
	UserID         uuid.UUID
	OrganisationID uuid.UUID
	MarketCode     string
	Role           string
	Status         string
	JobTitle       string
	JoinedAt       time.Time
}

type memoryToken struct {
	UserID    uuid.UUID
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// MemoryOrganisation mirrors the organisations columns identity needs.
type MemoryOrganisation struct {
	ID         uuid.UUID
	Name       string
	Kind       string
	MarketCode string
}

// MemoryConsent mirrors a user_consents row.
type MemoryConsent struct {
	UserID      uuid.UUID
	ConsentType string
	Version     string
	AcceptedAt  time.Time
	// MarketCode and Context are new in 0.43.0 (nil for older records).
	MarketCode *string
	Context    *string
}

// registrationConsents are the consent records a registration writes: the
// privacy notice, and the terms when a version was sent (0.43.0).
func registrationConsents(u NewUser) []MemoryConsent {
	var market *string
	if u.LegalMarket != "" {
		m := u.LegalMarket
		market = &m
	}
	ctx := ConsentContextRegistration
	out := []MemoryConsent{{UserID: u.ID, ConsentType: ConsentPrivacyNotice, Version: u.PrivacyVersion, AcceptedAt: u.CreatedAt, MarketCode: market, Context: &ctx}}
	if u.TermsVersion != "" {
		out = append(out, MemoryConsent{UserID: u.ID, ConsentType: ConsentTerms, Version: u.TermsVersion, AcceptedAt: u.CreatedAt, MarketCode: market, Context: &ctx})
	}
	return out
}

// AddConsent appends a consent record (the legal module's acceptances in
// development and tests).
func (s *MemoryStore) AddConsent(c MemoryConsent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.consents = append(s.consents, c)
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		users:         make(map[uuid.UUID]*memoryUser),
		byEmail:       make(map[string]uuid.UUID),
		organisations: make(map[uuid.UUID]MemoryOrganisation),
		platformRoles: make(map[uuid.UUID][]auth.PlatformRole),
		accountRoles:  make(map[uuid.UUID][]AccountRole),
		sessions:      make(map[string]*Session),
		verifications: make(map[string]*memoryToken),
		resets:        make(map[string]*memoryToken),
		captchas:      make(map[string]*memoryCaptcha),
		challenges:    make(map[string]*LoginChallenge),
		mfaTOTP:       make(map[uuid.UUID]*AdminMFA),
		mfaRecovery:   make(map[uuid.UUID][]RecoveryCode),
	}
}

type memoryCaptcha struct {
	Captcha
	UsedAt *time.Time
}

// CreateCaptcha implements Store.
func (s *MemoryStore) CreateCaptcha(_ context.Context, c Captcha) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, old := range s.captchas {
		if old.ExpiresAt.Before(c.CreatedAt) {
			delete(s.captchas, id)
		}
	}
	c.AnswerHash = slices.Clone(c.AnswerHash)
	s.captchas[c.ID] = &memoryCaptcha{Captcha: c}
	return nil
}

// ConsumeCaptcha implements Store.
func (s *MemoryStore) ConsumeCaptcha(_ context.Context, id string, now time.Time) (Captcha, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.captchas[id]
	if !ok || c.UsedAt != nil {
		return Captcha{}, ErrNotFound
	}
	c.UsedAt = &now
	return c.Captcha, nil
}

// CreateLoginChallenge implements Store.
func (s *MemoryStore) CreateLoginChallenge(_ context.Context, c LoginChallenge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, old := range s.challenges {
		if old.ExpiresAt.Before(c.CreatedAt) {
			delete(s.challenges, k)
		}
	}
	c.IDHash, c.CodeHash = slices.Clone(c.IDHash), slices.Clone(c.CodeHash)
	s.challenges[key(c.IDHash)] = &c
	return nil
}

// LoginChallenge implements Store.
func (s *MemoryStore) LoginChallenge(_ context.Context, idHash []byte) (LoginChallenge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[key(idHash)]
	if !ok {
		return LoginChallenge{}, ErrNotFound
	}
	out := *c
	out.CodeHash = slices.Clone(c.CodeHash)
	return out, nil
}

// FailLoginChallenge implements Store.
func (s *MemoryStore) FailLoginChallenge(_ context.Context, idHash []byte, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[key(idHash)]
	if !ok || c.ConsumedAt != nil {
		return 0, ErrNotFound
	}
	c.Attempts++
	return c.Attempts, nil
}

// ConsumeLoginChallenge implements Store.
func (s *MemoryStore) ConsumeLoginChallenge(_ context.Context, idHash []byte, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[key(idHash)]
	if !ok || !c.usable(now) {
		return ErrNotFound
	}
	c.ConsumedAt = &now
	return nil
}

// ResendLoginChallenge implements Store.
func (s *MemoryStore) ResendLoginChallenge(_ context.Context, idHash, codeHash []byte, expiresAt, now, sentBefore time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[key(idHash)]
	if !ok || !c.usable(now) || c.CodeVerifiedAt != nil || c.ResendCount >= SignInCodeMaxResends || c.LastSentAt.After(sentBefore) {
		return ErrNotFound
	}
	c.CodeHash, c.ExpiresAt, c.LastSentAt = slices.Clone(codeHash), expiresAt, now
	c.ResendCount++
	return nil
}

// LoginChallengeCount returns how many sign-in challenges are held (tests).
func (s *MemoryStore) LoginChallengeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.challenges)
}

// CaptchaCount returns how many captchas are held, used or not (tests).
func (s *MemoryStore) CaptchaCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captchas)
}

func key(hash []byte) string { return hex.EncodeToString(hash) }

// view returns a copy of the user with roles attached. Caller holds the lock.
func (s *MemoryStore) view(u *memoryUser) User {
	out := u.User
	if u.EmailVerifiedAt != nil {
		t := *u.EmailVerifiedAt
		out.EmailVerifiedAt = &t
	}
	out.PlatformRoles = append([]auth.PlatformRole(nil), s.platformRoles[u.ID]...)
	out.AccountRoles = append([]AccountRole(nil), s.accountRoles[u.ID]...)
	sortAccountRoles(out.AccountRoles)
	out.Memberships = nil
	for _, m := range s.memberships {
		// Only an ACTIVE membership gives access (and so appears on the
		// Principal): a suspended or removed member has none, from their
		// next request on.
		if m.UserID != u.ID || m.Status != MembershipActive {
			continue
		}
		org := s.organisations[m.OrganisationID]
		out.Memberships = append(out.Memberships, auth.Membership{
			OrganisationID: org.ID, OrganisationName: org.Name, OrganisationKind: org.Kind,
			MarketCode: m.MarketCode, Role: m.Role,
		})
	}
	sort.SliceStable(out.Memberships, func(i, j int) bool {
		return out.Memberships[i].OrganisationName < out.Memberships[j].OrganisationName
	})
	return out
}

// CreateUser implements Store.
func (s *MemoryStore) CreateUser(_ context.Context, u NewUser) error {
	if err := s.createUser(u); err != nil {
		return err
	}
	s.rolesChanged(u.ID, u.AccountRoles, u.CreatedAt)
	return nil
}

func (s *MemoryStore) createUser(u NewUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.byEmail[u.Email]; taken {
		return ErrEmailTaken
	}
	s.users[u.ID] = &memoryUser{
		User: User{
			ID: u.ID, Name: u.Name, Email: u.Email, Status: StatusActive,
			PasswordHash: u.PasswordHash, EmailVerifiedAt: u.EmailVerifiedAt, CreatedAt: u.CreatedAt,
		},
		MarketingOptIn: u.MarketingOptIn,
	}
	s.byEmail[u.Email] = u.ID
	s.accountRoles[u.ID] = append([]AccountRole(nil), u.AccountRoles...)
	s.consents = append(s.consents, registrationConsents(u)...)
	if u.MarketingOptIn {
		e, err := registrationMarketingEvent(u)
		if err != nil {
			return err
		}
		s.marketing = append(s.marketing, e)
	}
	return nil
}

// UpdatePendingRegistration implements Store.
func (s *MemoryStore) UpdatePendingRegistration(_ context.Context, u NewUser) error {
	if err := s.updatePendingRegistration(u); err != nil {
		return err
	}
	s.rolesChanged(u.ID, u.AccountRoles, u.CreatedAt)
	return nil
}

func (s *MemoryStore) updatePendingRegistration(u NewUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[u.ID]
	if !ok || user.EmailVerifiedAt != nil {
		return ErrNotFound
	}
	if user.MarketingOptIn != u.MarketingOptIn {
		e, err := registrationMarketingEvent(u)
		if err != nil {
			return err
		}
		s.marketing = append(s.marketing, e)
	}
	user.Name, user.PasswordHash, user.MarketingOptIn = u.Name, u.PasswordHash, u.MarketingOptIn
	s.accountRoles[u.ID] = append([]AccountRole(nil), u.AccountRoles...)
	s.consents = append(s.consents, registrationConsents(u)...)
	for _, t := range s.verifications {
		if t.UserID == u.ID && t.UsedAt == nil {
			used := u.CreatedAt
			t.UsedAt = &used
		}
	}
	return nil
}

// UserByEmail implements Store.
func (s *MemoryStore) UserByEmail(_ context.Context, email string) (User, error) {
	s.mu.Lock()
	id, ok := s.byEmail[email]
	s.mu.Unlock()
	if !ok {
		return User{}, ErrNotFound
	}
	return s.UserByID(context.Background(), id)
}

// UserByID implements Store.
func (s *MemoryStore) UserByID(_ context.Context, id uuid.UUID) (User, error) {
	u, ok := s.Account(id)
	if !ok {
		return User{}, ErrNotFound
	}
	return s.decorate(u), nil
}

// CreateVerificationToken implements Store.
func (s *MemoryStore) CreateVerificationToken(_ context.Context, userID uuid.UUID, tokenHash []byte, expiresAt, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.verifications[key(tokenHash)] = &memoryToken{UserID: userID, ExpiresAt: expiresAt}
	return nil
}

// ConsumeVerificationToken implements Store.
func (s *MemoryStore) ConsumeVerificationToken(_ context.Context, tokenHash []byte, now time.Time) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.verifications[key(tokenHash)]
	if !ok || t.UsedAt != nil || !now.Before(t.ExpiresAt) {
		return uuid.Nil, ErrInvalidToken
	}
	user, ok := s.users[t.UserID]
	if !ok {
		return uuid.Nil, ErrInvalidToken
	}
	t.UsedAt = &now
	if user.EmailVerifiedAt == nil {
		user.EmailVerifiedAt = &now
		s.emit(OutboxEvent{Type: EventUserEmailVerified, UserID: user.ID, OccurredAt: now})
	}
	return t.UserID, nil
}

// emit appends an outbox event and hands it to the sink. Caller holds the lock.
func (s *MemoryStore) emit(e OutboxEvent) {
	ev := outbox.Event{
		ID: uuid.New(), Type: e.Type, AggregateType: AggregateUser, AggregateID: e.UserID,
		Payload: e.Payload(), Attempts: 1, CreatedAt: e.OccurredAt,
	}
	s.outbox = append(s.outbox, ev)
	if s.sink != nil {
		s.sink(ev)
	}
}

// SetOutboxSink registers a receiver for events as they are written
// (development without a database). The sink must not call back into the
// store synchronously.
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

// CreatePasswordResetToken implements Store.
func (s *MemoryStore) CreatePasswordResetToken(_ context.Context, userID uuid.UUID, tokenHash []byte, expiresAt, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resets[key(tokenHash)] = &memoryToken{UserID: userID, ExpiresAt: expiresAt}
	return nil
}

// ResetPassword implements Store. Holding the mutex for the whole call gives
// the same all-or-nothing behaviour as the Postgres transaction.
func (s *MemoryStore) ResetPassword(_ context.Context, tokenHash []byte, passwordHash string, now time.Time) (uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.resets[key(tokenHash)]
	if !ok || t.UsedAt != nil || !now.Before(t.ExpiresAt) {
		return uuid.Nil, ErrInvalidToken
	}
	user, ok := s.users[t.UserID]
	if !ok {
		return uuid.Nil, ErrInvalidToken
	}
	user.PasswordHash = passwordHash
	if user.EmailVerifiedAt == nil {
		user.EmailVerifiedAt = &now
	}
	for _, other := range s.resets {
		if other.UserID == t.UserID && other.UsedAt == nil {
			other.UsedAt = &now
		}
	}
	for _, sess := range s.sessions {
		if sess.UserID == t.UserID && sess.RevokedAt == nil {
			sess.RevokedAt = &now
		}
	}
	s.emit(OutboxEvent{Type: EventPasswordChanged, UserID: t.UserID, OccurredAt: now})
	return t.UserID, nil
}

// CreateSession implements Store.
func (s *MemoryStore) CreateSession(_ context.Context, sess Session, events ...OutboxEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := sess
	s.sessions[key(sess.TokenHash)] = &cp
	for _, e := range events {
		s.emit(e)
	}
	return nil
}

// SessionByTokenHash implements Store.
func (s *MemoryStore) SessionByTokenHash(_ context.Context, tokenHash []byte) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[key(tokenHash)]
	if !ok {
		return Session{}, ErrNotFound
	}
	return *sess, nil
}

// TouchSession implements Store.
func (s *MemoryStore) TouchSession(_ context.Context, id uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ID == id {
			sess.LastSeenAt = now
		}
	}
	return nil
}

// RevokeSession implements Store.
func (s *MemoryStore) RevokeSession(_ context.Context, id uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ID == id && sess.RevokedAt == nil {
			sess.RevokedAt = &now
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Seed and test helpers (not part of Store)
// ---------------------------------------------------------------------------

// ApprovedDirectOwners implements Store.
func (s *MemoryStore) ApprovedDirectOwners(context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, u := range s.users {
		if u.Status != StatusActive {
			continue
		}
		for _, r := range s.accountRoles[id] {
			if r.Status == AccountRoleActive && slices.Contains(auth.DirectOwnerRoles(), r.Role) {
				n++
				break
			}
		}
	}
	return n, nil
}

// Organisation returns an organisation (the listings' advertiser resolver
// needs its name and kind).
func (s *MemoryStore) Organisation(id uuid.UUID) (MemoryOrganisation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.organisations[id]
	return o, ok
}

// PutOrganisation adds or replaces an organisation.
func (s *MemoryStore) PutOrganisation(o MemoryOrganisation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.organisations[o.ID] = o
}

// AddMembership makes the user a member of an organisation in a market.
func (s *MemoryStore) AddMembership(userID, organisationID uuid.UUID, marketCode, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("add membership: %w", ErrNotFound)
	}
	if _, ok := s.organisations[organisationID]; !ok {
		return fmt.Errorf("add membership: organisation %s: %w", organisationID, ErrNotFound)
	}
	s.memberships = append(s.memberships, memoryMembership{UserID: userID, OrganisationID: organisationID, MarketCode: marketCode, Role: role,
		Status: MembershipActive, JoinedAt: s.users[userID].CreatedAt})
	return nil
}

// AddPlatformRole grants a platform role (marketCode only for MARKET_ADMIN).
func (s *MemoryStore) AddPlatformRole(userID uuid.UUID, role, marketCode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[userID]; !ok {
		return fmt.Errorf("add platform role: %w", ErrNotFound)
	}
	s.platformRoles[userID] = append(s.platformRoles[userID], auth.PlatformRole{Role: role, MarketCode: marketCode})
	return nil
}

// CreatePlatformAdmin creates a verified administrator account holding one
// platform role (used by cmd/adminctl's tests; the real target is Postgres).
func (s *MemoryStore) CreatePlatformAdmin(_ context.Context, a NewPlatformAdmin) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.byEmail[a.Email]; taken {
		return ErrEmailTaken
	}
	verified := a.CreatedAt
	s.users[a.ID] = &memoryUser{User: User{
		ID: a.ID, Name: a.Name, Email: a.Email, Status: StatusActive,
		PasswordHash: a.PasswordHash, EmailVerifiedAt: &verified, CreatedAt: a.CreatedAt,
	}}
	s.byEmail[a.Email] = a.ID
	s.platformRoles[a.ID] = []auth.PlatformRole{{Role: a.Role, MarketCode: a.MarketCode}}
	return nil
}

// SetPlatformAdminPassword replaces an administrator's password, invalidates
// their reset tokens and revokes all their sessions.
func (s *MemoryStore) SetPlatformAdminPassword(_ context.Context, email, passwordHash string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.byEmail[email]
	if !ok {
		return ErrNotFound
	}
	if len(s.platformRoles[id]) == 0 {
		return ErrNotPlatformAdmin
	}
	s.users[id].PasswordHash = passwordHash
	for _, t := range s.resets {
		if t.UserID == id && t.UsedAt == nil {
			t.UsedAt = &now
		}
	}
	for _, sess := range s.sessions {
		if sess.UserID == id && sess.RevokedAt == nil {
			sess.RevokedAt = &now
		}
	}
	s.emit(OutboxEvent{Type: EventPasswordChanged, UserID: id, OccurredAt: now})
	return nil
}

// OrganisationManagers returns the active OWNER and MANAGER members of an
// organisation (the people who read its inbox).
func (s *MemoryStore) OrganisationManagers(organisationID uuid.UUID) []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []User
	for _, m := range s.memberships {
		if m.OrganisationID != organisationID || m.Status != MembershipActive || (m.Role != auth.RoleOwner && m.Role != auth.RoleManager) {
			continue
		}
		if u, ok := s.users[m.UserID]; ok && u.Status == StatusActive {
			out = append(out, s.view(u))
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Email < out[j].Email })
	return out
}

// UserCount returns the number of accounts (test helper).
func (s *MemoryStore) UserCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.users)
}

// Consents returns a copy of the consent records (test helper).
func (s *MemoryStore) Consents() []MemoryConsent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]MemoryConsent(nil), s.consents...)
}

// Sessions returns a copy of all sessions (test helper).
func (s *MemoryStore) Sessions() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, *sess)
	}
	return out
}
