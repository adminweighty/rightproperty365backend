package agreements

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemoryStore is the in-memory Store (development and tests).
type MemoryStore struct {
	mu         sync.Mutex
	agreements map[uuid.UUID]Agreement
	events     []Event
	refs       map[int]int
	jobs       map[string]time.Time
	signatures map[uuid.UUID]SavedSignature
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{agreements: map[uuid.UUID]Agreement{}, refs: map[int]int{}, jobs: map[string]time.Time{}, signatures: map[uuid.UUID]SavedSignature{}}
}

func copyAdopted(a *Adopted) *Adopted {
	if a == nil {
		return nil
	}
	c := *a
	return &c
}

func copyAgreement(a Agreement) Agreement {
	if a.Original != nil {
		d := *a.Original
		a.Original = &d
	}
	if a.Final != nil {
		d := *a.Final
		a.Final = &d
	}
	a.Pages = append([]PageSize{}, a.Pages...)
	rs := make([]Recipient, len(a.Recipients))
	for i, r := range a.Recipients {
		r.CodeHash = append([]byte(nil), r.CodeHash...)
		if len(r.CodeHash) == 0 {
			r.CodeHash = nil
		}
		r.Signature, r.Initials = copyAdopted(r.Signature), copyAdopted(r.Initials)
		rs[i] = r
	}
	a.Recipients = rs
	a.Fields = append([]Field{}, a.Fields...)
	a.Links = append([]Link{}, a.Links...)
	return a
}

// NextReference implements Store.
func (s *MemoryStore) NextReference(_ context.Context, year int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[year]++
	return fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, s.refs[year]), nil
}

// CreateAgreement implements Store.
func (s *MemoryStore) CreateAgreement(_ context.Context, a Agreement, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.agreements {
		if o.Reference == a.Reference {
			return ErrReferenceTaken
		}
	}
	s.agreements[a.ID] = copyAgreement(a)
	s.events = append(s.events, events...)
	return nil
}

// PutAgreement stores an agreement as it is (the development seed).
func (s *MemoryStore) PutAgreement(a Agreement, events ...Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agreements[a.ID] = copyAgreement(a)
	s.events = append(s.events, events...)
}

// Agreement implements Store.
func (s *MemoryStore) Agreement(_ context.Context, id uuid.UUID) (Agreement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agreements[id]
	if !ok {
		return Agreement{}, ErrNotFound
	}
	return copyAgreement(a), nil
}

func (f Filter) matches(a Agreement) bool {
	if f.Status != "" && a.Status != f.Status {
		return false
	}
	if f.All {
		return true
	}
	if a.OrganisationID != nil && slices.Contains(f.Organisations, *a.OrganisationID) {
		return true
	}
	if f.Sender == nil || a.SenderID != *f.Sender {
		return false
	}
	if a.OrganisationID == nil {
		return f.SenderDirect
	}
	return slices.Contains(f.SenderOrganisations, *a.OrganisationID)
}

func sortNewest(list []Agreement) {
	slices.SortStableFunc(list, func(a, b Agreement) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return bytes.Compare(b.ID[:], a.ID[:])
	})
}

// Agreements implements Store.
func (s *MemoryStore) Agreements(_ context.Context, f Filter) ([]Agreement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Agreement{}
	for _, a := range s.agreements {
		if f.matches(a) {
			out = append(out, copyAgreement(a))
		}
	}
	sortNewest(out)
	return out, nil
}

// ChangeAgreement implements Store.
func (s *MemoryStore) ChangeAgreement(_ context.Context, id uuid.UUID, change func(a *Agreement) ([]Event, error)) (Agreement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agreements[id]
	if !ok {
		return Agreement{}, ErrNotFound
	}
	next := copyAgreement(a)
	events, err := change(&next)
	if err != nil {
		return Agreement{}, err
	}
	next.ID = id
	s.agreements[id] = copyAgreement(next)
	s.events = append(s.events, events...)
	return copyAgreement(next), nil
}

// DeleteAgreement implements Store.
func (s *MemoryStore) DeleteAgreement(_ context.Context, id uuid.UUID) (Agreement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.agreements[id]
	if !ok {
		return Agreement{}, ErrNotFound
	}
	if a.Status != StatusDraft {
		return Agreement{}, ErrState
	}
	delete(s.agreements, id)
	kept := s.events[:0]
	for _, e := range s.events {
		if e.AgreementID != id {
			kept = append(kept, e)
		}
	}
	s.events = kept
	return a, nil
}

// LinkByToken implements Store.
func (s *MemoryStore) LinkByToken(_ context.Context, tokenHash []byte) (Link, uuid.UUID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agreements {
		for _, l := range a.Links {
			if bytes.Equal(l.TokenHash, tokenHash) {
				return l, a.ID, nil
			}
		}
	}
	return Link{}, uuid.Nil, ErrLinkNotFound
}

// RecipientAgreements implements Store.
func (s *MemoryStore) RecipientAgreements(_ context.Context, email string) ([]Agreement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Agreement{}
	for _, a := range s.agreements {
		if a.Status != StatusDraft && slices.ContainsFunc(a.Recipients, func(r Recipient) bool { return r.Email == email }) {
			out = append(out, copyAgreement(a))
		}
	}
	sortNewest(out)
	return out, nil
}

// ByReference implements Store.
func (s *MemoryStore) ByReference(_ context.Context, reference string) (Agreement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agreements {
		if a.Reference == reference {
			return copyAgreement(a), nil
		}
	}
	return Agreement{}, ErrNotFound
}

// BySHA256 implements Store.
func (s *MemoryStore) BySHA256(_ context.Context, sha string) (Agreement, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.agreements {
		if a.Status != StatusCompleted {
			continue
		}
		if a.Final != nil && a.Final.SHA256 == sha {
			return copyAgreement(a), "FINAL", nil
		}
		if a.Original != nil && a.Original.SHA256 == sha {
			return copyAgreement(a), "ORIGINAL", nil
		}
	}
	return Agreement{}, "", ErrNotFound
}

// Events implements Store.
func (s *MemoryStore) Events(_ context.Context, id uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Event{}
	for _, e := range s.events {
		if e.AgreementID == id {
			out = append(out, e)
		}
	}
	slices.SortStableFunc(out, func(a, b Event) int { return a.At.Compare(b.At) })
	return out, nil
}

// ClaimJob implements Store.
func (s *MemoryStore) ClaimJob(_ context.Context, key string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[key]; ok {
		return false, nil
	}
	s.jobs[key] = at
	return true, nil
}

// SavedSignature implements Store.
func (s *MemoryStore) SavedSignature(_ context.Context, userID uuid.UUID) (SavedSignature, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.signatures[userID]
	return SavedSignature{Signature: copyAdopted(v.Signature), Initials: copyAdopted(v.Initials), UpdatedAt: v.UpdatedAt}, ok, nil
}

// PutSavedSignature implements Store.
func (s *MemoryStore) PutSavedSignature(_ context.Context, userID uuid.UUID, v SavedSignature) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signatures[userID] = SavedSignature{Signature: copyAdopted(v.Signature), Initials: copyAdopted(v.Initials), UpdatedAt: v.UpdatedAt}
	return nil
}
