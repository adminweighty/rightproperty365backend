package identity

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// MarketingState implements Store. Records are newest first by changed_at;
// records with the same time are in reverse order of writing.
func (s *MemoryStore) MarketingState(_ context.Context, userID uuid.UUID, limit int) (MarketingState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return MarketingState{}, ErrNotFound
	}
	out := MarketingState{OptIn: u.MarketingOptIn, History: []MarketingConsentEvent{}}
	for i := len(s.marketing) - 1; i >= 0; i-- {
		if s.marketing[i].UserID == userID {
			out.History = append(out.History, s.marketing[i])
		}
	}
	// Insertion order reversed is the tie-break; a stable sort keeps it.
	sortNewestFirst(out.History)
	if limit >= 0 && len(out.History) > limit {
		out.History = out.History[:limit]
	}
	return out, nil
}

// sortNewestFirst orders records by changed_at, newest first, keeping the
// current order of records with the same time.
func sortNewestFirst(events []MarketingConsentEvent) {
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j].ChangedAt.After(events[j-1].ChangedAt); j-- {
			events[j], events[j-1] = events[j-1], events[j]
		}
	}
}

// ChangeMarketingOptIn implements Store.
func (s *MemoryStore) ChangeMarketingOptIn(_ context.Context, e MarketingConsentEvent) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[e.UserID]
	if !ok {
		return false, ErrNotFound
	}
	if u.MarketingOptIn == e.OptIn {
		return false, nil
	}
	u.MarketingOptIn = e.OptIn
	e.ChangedAt = e.ChangedAt.UTC()
	s.marketing = append(s.marketing, e)
	return true, nil
}

// MarketingEligibility implements Store.
func (s *MemoryStore) MarketingEligibility(_ context.Context, userID uuid.UUID) (MarketingEligibility, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return MarketingEligibility{}, ErrNotFound
	}
	return MarketingEligibility{Status: u.Status, EmailVerified: u.EmailVerifiedAt != nil, OptIn: u.MarketingOptIn}, nil
}

// PutMarketingConsent stores a marketing consent record as given and sets
// the account's current value to the newest record's (the development seed).
// Records already stored with the same id are replaced.
func (s *MemoryStore) PutMarketingConsent(events ...MarketingConsentEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range events {
		u, ok := s.users[e.UserID]
		if !ok {
			return fmt.Errorf("put marketing consent: %w", ErrNotFound)
		}
		s.marketing = slices.DeleteFunc(s.marketing, func(m MarketingConsentEvent) bool { return m.ID == e.ID })
		e.ChangedAt = e.ChangedAt.UTC()
		s.marketing = append(s.marketing, e)
		latest := e
		for _, other := range s.marketing {
			if other.UserID == e.UserID && other.ChangedAt.After(latest.ChangedAt) {
				latest = other
			}
		}
		u.MarketingOptIn = latest.OptIn
	}
	return nil
}

// DeleteMarketingConsents removes every marketing consent record of the user
// (account deletion; AnonymiseAccount sets the opt-in to false).
func (s *MemoryStore) DeleteMarketingConsents(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.marketing = slices.DeleteFunc(s.marketing, func(e MarketingConsentEvent) bool { return e.UserID == userID })
}

// MarketingConsents returns every stored record of the user, in the order
// written (tests).
func (s *MemoryStore) MarketingConsents(userID uuid.UUID) []MarketingConsentEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []MarketingConsentEvent
	for _, e := range s.marketing {
		if e.UserID == userID {
			out = append(out, e)
		}
	}
	return out
}

// MarketingConsentText implements Store. The memory store holds the rows
// migration 0041 inserts (builtInMarketingConsentTexts).
func (s *MemoryStore) MarketingConsentText(_ context.Context, version string) (MarketingConsentText, error) {
	for _, t := range builtInMarketingConsentTexts() {
		if t.Version == version {
			return t, nil
		}
	}
	return MarketingConsentText{}, ErrConsentTextNotFound
}
