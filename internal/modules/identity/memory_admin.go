package identity

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Account administration on the memory store (internal/modules/accounts
// builds its in-memory store on these). Not part of Store: in Postgres the
// accounts module writes the same rows itself, in one transaction.

// Account statuses besides StatusActive (users.status).
const (
	StatusSuspended = "SUSPENDED"
	StatusDeleted   = "DELETED"
)

// AdminAccount is everything account administration reads about a user.
type AdminAccount struct {
	User
	SuspendedAt      *time.Time
	SuspensionReason string
	SuspendedBy      *uuid.UUID
	DeletedAt        *time.Time
	// AllMemberships are the user's memberships in every status.
	AllMemberships []AdminMembership
	// LastSignInAt is when the newest session started (nil: never).
	LastSignInAt *time.Time
}

// AdminMembership is one membership with its organisation.
type AdminMembership struct {
	OrganisationID   uuid.UUID
	OrganisationName string
	OrganisationKind string
	MarketCode       string
	Role             string
	Status           string
}

func (s *MemoryStore) adminAccountLocked(u *memoryUser) AdminAccount {
	a := AdminAccount{User: s.view(u), SuspendedAt: u.SuspendedAt, SuspensionReason: u.SuspensionReason, SuspendedBy: u.SuspendedBy, DeletedAt: u.DeletedAt,
		AllMemberships: []AdminMembership{}}
	for _, m := range s.memberships {
		if m.UserID != u.ID {
			continue
		}
		org := s.organisations[m.OrganisationID]
		status := m.Status
		if status == "" {
			status = MembershipActive
		}
		a.AllMemberships = append(a.AllMemberships, AdminMembership{OrganisationID: m.OrganisationID, OrganisationName: org.Name,
			OrganisationKind: org.Kind, MarketCode: m.MarketCode, Role: m.Role, Status: status})
	}
	for _, sess := range s.sessions {
		if sess.UserID == u.ID && (a.LastSignInAt == nil || sess.CreatedAt.After(*a.LastSignInAt)) {
			at := sess.CreatedAt
			a.LastSignInAt = &at
		}
	}
	return a
}

// AdminAccounts returns every account (without onboarding statuses).
func (s *MemoryStore) AdminAccounts() []AdminAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AdminAccount, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, s.adminAccountLocked(u))
	}
	return out
}

// AdminAccount returns one account, or ErrNotFound.
func (s *MemoryStore) AdminAccount(id uuid.UUID) (AdminAccount, error) {
	s.mu.Lock()
	u, ok := s.users[id]
	if !ok {
		s.mu.Unlock()
		return AdminAccount{}, ErrNotFound
	}
	a := s.adminAccountLocked(u)
	s.mu.Unlock()
	a.User = s.decorate(a.User)
	return a, nil
}

// endAccessLocked revokes every session of the user and voids their sign-in
// challenges and their unused password reset and verification tokens.
// Caller holds the lock.
func (s *MemoryStore) endAccessLocked(id uuid.UUID, at time.Time) int {
	n := 0
	for _, sess := range s.sessions {
		if sess.UserID == id && sess.RevokedAt == nil {
			t := at
			sess.RevokedAt = &t
			n++
		}
	}
	for k, c := range s.challenges {
		if c.UserID == id {
			delete(s.challenges, k)
		}
	}
	for _, tokens := range []map[string]*memoryToken{s.resets, s.verifications} {
		for _, t := range tokens {
			if t.UserID == id && t.UsedAt == nil {
				used := at
				t.UsedAt = &used
			}
		}
	}
	return n
}

// SuspendAccount marks an ACTIVE account SUSPENDED with the reason and ends
// every way into it. It reports the sessions revoked. ErrNotFound for an
// unknown account; an error when it is not ACTIVE.
func (s *MemoryStore) SuspendAccount(id uuid.UUID, reason string, by *uuid.UUID, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return 0, ErrNotFound
	}
	if u.Status != StatusActive {
		return 0, fmt.Errorf("suspend account: status is %s", u.Status)
	}
	t := at
	u.Status, u.SuspendedAt, u.SuspensionReason, u.SuspendedBy = StatusSuspended, &t, reason, by
	return s.endAccessLocked(id, at), nil
}

// ReactivateAccount makes a SUSPENDED account ACTIVE again.
func (s *MemoryStore) ReactivateAccount(id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return ErrNotFound
	}
	if u.Status != StatusSuspended {
		return fmt.Errorf("reactivate account: status is %s", u.Status)
	}
	u.Status, u.SuspendedAt, u.SuspensionReason, u.SuspendedBy = StatusActive, nil, "", nil
	return nil
}

// AnonymiseAccount erases the personal data of an account in place: its
// name and email address become the given placeholders, its password is
// removed, marketing consent withdrawn, every way into it ended, and its
// status becomes DELETED. The row (and so every reference to it) stays.
func (s *MemoryStore) AnonymiseAccount(id uuid.UUID, name, email string, at time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return 0, ErrNotFound
	}
	if u.Status == StatusDeleted {
		return 0, fmt.Errorf("anonymise account: already deleted")
	}
	delete(s.byEmail, u.Email)
	t := at
	u.Name, u.Email, u.PasswordHash, u.MarketingOptIn = name, email, "", false
	u.Status, u.DeletedAt, u.SuspendedAt, u.SuspensionReason, u.SuspendedBy = StatusDeleted, &t, nil, "", nil
	s.byEmail[email] = id
	// The administrator's authenticator and recovery codes go (API 0.40.0).
	s.deleteMFALocked(id)
	return s.endAccessLocked(id, at), nil
}

// RemoveMembershipsOf marks every membership of the user REMOVED (the
// account was deleted) and returns the organisations they were in.
func (s *MemoryStore) RemoveMembershipsOf(id uuid.UUID) []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	var orgs []uuid.UUID
	for i, m := range s.memberships {
		if m.UserID == id && m.Status != MembershipRemoved {
			s.memberships[i].Status = MembershipRemoved
			orgs = append(orgs, m.OrganisationID)
		}
	}
	return orgs
}

// EmitUserEvent writes an outbox event about a user (the accounts module's
// suspension and reactivation notices), to this store's sink.
func (s *MemoryStore) EmitUserEvent(eventType string, userID uuid.UUID, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit(OutboxEvent{Type: eventType, UserID: userID, OccurredAt: at})
}
