package identity

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
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
	consents      []MemoryConsent
}

type memoryUser struct {
	User
	MarketingOptIn bool
}

type memoryMembership struct {
	UserID         uuid.UUID
	OrganisationID uuid.UUID
	MarketCode     string
	Role           string
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
	}
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
		if m.UserID != u.ID {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, taken := s.byEmail[u.Email]; taken {
		return ErrEmailTaken
	}
	s.users[u.ID] = &memoryUser{
		User: User{
			ID: u.ID, Name: u.Name, Email: u.Email, Status: StatusActive,
			PasswordHash: u.PasswordHash, EmailVerifiedAt: u.EmailVerifiedAt,
		},
		MarketingOptIn: u.MarketingOptIn,
	}
	s.byEmail[u.Email] = u.ID
	s.accountRoles[u.ID] = append([]AccountRole(nil), u.AccountRoles...)
	s.consents = append(s.consents, MemoryConsent{UserID: u.ID, ConsentType: ConsentPrivacyNotice, Version: u.PrivacyVersion, AcceptedAt: u.CreatedAt})
	return nil
}

// UpdatePendingRegistration implements Store.
func (s *MemoryStore) UpdatePendingRegistration(_ context.Context, u NewUser) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	user, ok := s.users[u.ID]
	if !ok || user.EmailVerifiedAt != nil {
		return ErrNotFound
	}
	user.Name, user.PasswordHash, user.MarketingOptIn = u.Name, u.PasswordHash, u.MarketingOptIn
	s.accountRoles[u.ID] = append([]AccountRole(nil), u.AccountRoles...)
	s.consents = append(s.consents, MemoryConsent{UserID: u.ID, ConsentType: ConsentPrivacyNotice, Version: u.PrivacyVersion, AcceptedAt: u.CreatedAt})
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
	defer s.mu.Unlock()
	id, ok := s.byEmail[email]
	if !ok {
		return User{}, ErrNotFound
	}
	return s.view(s.users[id]), nil
}

// UserByID implements Store.
func (s *MemoryStore) UserByID(_ context.Context, id uuid.UUID) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return User{}, ErrNotFound
	}
	return s.view(u), nil
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
	}
	return t.UserID, nil
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
	return t.UserID, nil
}

// CreateSession implements Store.
func (s *MemoryStore) CreateSession(_ context.Context, sess Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := sess
	s.sessions[key(sess.TokenHash)] = &cp
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
	s.memberships = append(s.memberships, memoryMembership{UserID: userID, OrganisationID: organisationID, MarketCode: marketCode, Role: role})
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
