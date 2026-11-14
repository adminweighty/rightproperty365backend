package businessenquiries

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
)

// referenceAlphabet omits characters that are easily confused when read
// aloud or typed (0/O, 1/I/L, U).
const referenceAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"

const (
	referenceLength   = 8
	referenceAttempts = 5
)

// Vocabulary supplies the codes that are accepted right now (reference data).
type Vocabulary interface {
	DirectoryKinds(ctx context.Context) ([]string, error)
	ContactMethods(ctx context.Context) ([]string, error)
}

// WithVocabulary validates directory kinds and contact methods against v.
func WithVocabulary(v Vocabulary) Option { return func(s *Service) { s.vocab = v } }

func (s *Service) allowed(ctx context.Context) (allowed, error) {
	if s.vocab == nil {
		return defaultAllowed(), nil
	}
	kinds, err := s.vocab.DirectoryKinds(ctx)
	if err != nil {
		return allowed{}, fmt.Errorf("load directory kinds: %w", err)
	}
	methods, err := s.vocab.ContactMethods(ctx)
	if err != nil {
		return allowed{}, fmt.Errorf("load contact methods: %w", err)
	}
	return allowed{kinds: kinds, contactMethods: methods}, nil
}

// Service holds the business rules for business enquiries.
type Service struct {
	vocab  Vocabulary
	store  Store
	now    func() time.Time
	newRef func() (string, error)
}

// Option customises a Service (used by tests).
type Option func(*Service)

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithReferenceGenerator overrides reference generation.
func WithReferenceGenerator(f func() (string, error)) Option {
	return func(s *Service) { s.newRef = f }
}

// NewService builds a Service on the given store.
func NewService(store Store, opts ...Option) *Service {
	s := &Service{store: store, now: time.Now, newRef: NewReference}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create validates, routes and persists an enquiry.
//
// Idempotency: a repeated key with identical content returns the original
// enquiry (Result.Replayed); a repeated key with different content returns
// ErrIdempotencyConflict. The replay check runs before the profile checks so
// a retry still succeeds after the profile has since been suspended.
func (s *Service) Create(ctx context.Context, in Input, headerIdempotencyKey string) (Result, error) {
	now := s.now().UTC().Truncate(time.Microsecond) // Postgres timestamptz precision

	ok, err := s.allowed(ctx)
	if err != nil {
		return Result{}, err
	}
	v, fields := validate(in, headerIdempotencyKey, ok)
	if len(fields) > 0 {
		return Result{}, &ValidationError{Fields: fields}
	}
	hash := v.hash()

	existing, err := s.store.FindByIdempotencyKey(ctx, v.idempotencyKey)
	switch {
	case err == nil:
		return replay(existing, hash)
	case !errors.Is(err, ErrNotFound):
		return Result{}, fmt.Errorf("look up idempotency key: %w", err)
	}

	profile, err := s.store.GetProfile(ctx, v.kind, v.slug)
	if err != nil {
		if errors.Is(err, ErrRecipientNotFound) {
			return Result{}, ErrRecipientNotFound
		}
		return Result{}, fmt.Errorf("load profile: %w", err)
	}
	if profile.Status != ProfileActive {
		return Result{}, ErrRecipientUnavailable
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Result{}, fmt.Errorf("generate enquiry id: %w", err)
	}

	enq := NewEnquiry{
		ID:                     id,
		MarketID:               profile.MarketID,
		ProfileID:              profile.ID,
		ProfileKind:            profile.Kind,
		ProfileSlug:            profile.Slug,
		ProfileName:            profile.Name,
		Status:                 StatusReceived,
		Service:                v.service,
		Message:                v.message,
		PreferredContactMethod: v.contactMethod,
		IdempotencyKey:         v.idempotencyKey,
		RequestHash:            hash,
		CustomerUserID:         v.customerUserID,
		GuestName:              v.name,
		GuestEmail:             v.email,
		GuestPhone:             v.phone,
		PrivacyVersion:         v.privacyVersion,
		ConsentAcceptedAt:      now,
		Recipient:              ResolveRecipient(profile),
		CreatedAt:              now,
	}
	if enq.Recipient == nil {
		// Still accepted: the customer gets RECEIVED and operations picks up
		// the exception. We never lose an enquiry because routing failed.
		enq.Status = StatusRoutingException
	}

	for range referenceAttempts {
		if enq.Reference, err = s.newRef(); err != nil {
			return Result{}, fmt.Errorf("generate reference: %w", err)
		}
		rec, created, err := s.store.Create(ctx, enq)
		switch {
		case errors.Is(err, ErrReferenceCollision):
			continue
		case err != nil:
			return Result{}, fmt.Errorf("persist business enquiry: %w", err)
		case !created:
			// Lost a race against a concurrent request with the same key.
			return replay(rec, hash)
		}
		return result(rec, false), nil
	}
	return Result{}, errors.New("could not allocate a unique reference")
}

// ResolveRecipient picks who receives an enquiry.
//
//   - An agent profile goes to the named agent within their agency (the
//     agency's OWNER/MANAGER see it too, through the organisation).
//   - Every other profile — and an agent profile whose agent can no longer
//     receive it — goes to the owning organisation's inbox.
//
// It returns nil when nobody is eligible (the enquiry is then stored as a
// routing exception).
func ResolveRecipient(p Profile) *Recipient {
	switch {
	case p.Kind == KindAgents && p.AgentUserID != nil && p.AgentEligible:
		return &Recipient{OrganisationID: p.OrganisationID, UserID: p.AgentUserID}
	case p.OrganisationEligible:
		return &Recipient{OrganisationID: p.OrganisationID}
	}
	return nil
}

// Inbox lists the enquiries visible under scope, newest first, capped at
// MaxInboxItems. There is no pagination yet.
func (s *Service) Inbox(ctx context.Context, scope InboxScope) ([]InboxItem, error) {
	if scope.Limit <= 0 || scope.Limit > MaxInboxItems {
		scope.Limit = MaxInboxItems
	}
	if len(scope.ManagedOrganisationIDs) == 0 && len(scope.MemberOrganisationIDs) == 0 {
		return []InboxItem{}, nil
	}
	items, err := s.store.ListInbox(ctx, scope)
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}
	if items == nil {
		items = []InboxItem{}
	}
	return items, nil
}

func replay(rec Record, hash string) (Result, error) {
	if rec.RequestHash != hash {
		return Result{}, ErrIdempotencyConflict
	}
	return result(rec, true), nil
}

// result builds the customer-facing view. The public status is always
// RECEIVED, whatever the internal status is.
func result(rec Record, replayed bool) Result {
	return Result{
		EnquiryID: rec.ID,
		Reference: rec.Reference,
		Status:    StatusReceived,
		CreatedAt: rec.CreatedAt.UTC(),
		Replayed:  replayed,
	}
}

// NewReference returns a random human-friendly reference such as BE-7K3M9QXD.
func NewReference() (string, error) {
	out := make([]byte, referenceLength)
	max := big.NewInt(int64(len(referenceAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = referenceAlphabet[n.Int64()]
	}
	return "BE-" + string(out), nil
}
