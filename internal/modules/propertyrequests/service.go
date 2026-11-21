package propertyrequests

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

// Service holds the business rules for creating property requests.
// Vocabulary supplies the codes that are accepted right now. In the running
// API it is backed by the reference data, so deactivating a request type or a
// contact method there makes the API refuse it.
type Vocabulary interface {
	RequestTypes(ctx context.Context) ([]string, error)
	ContactMethods(ctx context.Context) ([]string, error)
}

// WithVocabulary validates request types and contact methods against v.
func WithVocabulary(v Vocabulary) Option { return func(s *Service) { s.vocab = v } }

func (s *Service) allowed(ctx context.Context) (allowed, error) {
	if s.vocab == nil {
		return defaultAllowed(), nil
	}
	types, err := s.vocab.RequestTypes(ctx)
	if err != nil {
		return allowed{}, fmt.Errorf("load request types: %w", err)
	}
	methods, err := s.vocab.ContactMethods(ctx)
	if err != nil {
		return allowed{}, fmt.Errorf("load contact methods: %w", err)
	}
	return allowed{requestTypes: types, contactMethods: methods}, nil
}

type Service struct {
	planner AppointmentPlanner
	// stays checks a request to stay against the listing's calendar.
	stays  StayChecker
	vocab  Vocabulary
	store  Store
	now    func() time.Time
	newRef func() (string, error)
	// assignees says who can hold the requests of an organisation (assign.go).
	assignees Assignees
	// routeTargets checks where a routing exception may be reassigned (API 0.49.0).
	routeTargets RouteTargets
}

// Option customises a Service (used by tests).
type Option func(*Service)

// WithClock overrides the time source.
func WithClock(now func() time.Time) Option { return func(s *Service) { s.now = now } }

// WithReferenceGenerator overrides reference generation.
func WithReferenceGenerator(f func() (string, error)) Option {
	return func(s *Service) { s.newRef = f }
}

// WithStayChecker makes STAY_REQUEST available: the checker decides whether
// the stay can be asked for at the listing.
func WithStayChecker(c StayChecker) Option { return func(s *Service) { s.stays = c } }

// WithAppointmentPlanner makes viewing and consultation requests that carry a
// preferred slot create an appointment in the same transaction.
func WithAppointmentPlanner(p AppointmentPlanner) Option { return func(s *Service) { s.planner = p } }

// NewService builds a Service on the given store.
func NewService(store Store, opts ...Option) *Service {
	s := &Service{store: store, now: time.Now, newRef: NewReference}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Create validates, routes and persists a property request.
//
// Idempotency: a repeated key with identical content returns the original
// request (Result.Replayed); a repeated key with different content returns
// ErrIdempotencyConflict. The replay check runs before the listing checks so
// a retry still succeeds after the listing has since expired.
func (s *Service) Create(ctx context.Context, in Input, headerIdempotencyKey string) (Result, error) {
	now := s.now().UTC().Truncate(time.Microsecond) // Postgres timestamptz precision

	ok, err := s.allowed(ctx)
	if err != nil {
		return Result{}, err
	}
	v, fields := validate(in, headerIdempotencyKey, now, ok)
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

	listing, err := s.store.GetListing(ctx, v.listingID)
	if err != nil {
		if errors.Is(err, ErrListingNotFound) {
			return Result{}, ErrListingNotFound
		}
		return Result{}, fmt.Errorf("load listing: %w", err)
	}
	if listing.Status != ListingStatusPublished || (listing.ExpiresAt != nil && !listing.ExpiresAt.After(now)) {
		return Result{}, ErrListingUnavailable
	}

	// A request to stay: the listing must be a short stay that is free for
	// those nights. Nothing is held: the request is an enquiry, and the
	// lister marks the nights BOOKED themselves.
	if v.stay != nil {
		if s.stays == nil {
			return Result{}, &ValidationError{Fields: map[string]string{"request_type": "STAY_REQUEST is not available"}}
		}
		nights, err := s.stays.CheckStay(ctx, listing.ID, v.stay.CheckIn, v.stay.CheckOut, v.stay.Guests)
		if err != nil {
			var verr *ValidationError
			var taken *DatesUnavailableError
			if errors.As(err, &verr) || errors.As(err, &taken) {
				return Result{}, err
			}
			return Result{}, fmt.Errorf("check the stay: %w", err)
		}
		v.stay.Nights = nights
	}

	id, err := uuid.NewV7()
	if err != nil {
		return Result{}, fmt.Errorf("generate request id: %w", err)
	}

	req := NewRequest{
		ID:                     id,
		MarketID:               listing.MarketID,
		ListingID:              listing.ID,
		RequestType:            v.requestType,
		Status:                 StatusReceived,
		Message:                v.message,
		PreferredContactMethod: v.contactMethod,
		PreferredSlot:          v.slot,
		IdempotencyKey:         v.idempotencyKey,
		RequestHash:            hash,
		CustomerUserID:         v.customerUserID,
		GuestName:              v.name,
		GuestEmail:             v.email,
		GuestPhone:             v.phone,
		PrivacyVersion:         v.privacyVersion,
		ConsentAcceptedAt:      now,
		Recipient:              ResolveRecipient(listing),
		CreatedAt:              now,
		Stay:                   v.stay,
	}
	if req.Recipient == nil {
		// Still accepted: the customer gets RECEIVED and operations picks up
		// the exception. We never lose an enquiry because routing failed.
		req.Status = StatusRoutingException
	}

	for range referenceAttempts {
		if req.Reference, err = s.newRef(); err != nil {
			return Result{}, fmt.Errorf("generate reference: %w", err)
		}
		if s.planner != nil {
			// Planned on every attempt, so a reference collision also gets a
			// fresh appointment reference.
			if req.Appointment, err = s.planner.PlanAppointment(ctx, req); err != nil {
				return Result{}, fmt.Errorf("plan appointment: %w", err)
			}
		}
		rec, created, err := s.store.Create(ctx, req)
		switch {
		case errors.Is(err, ErrReferenceCollision):
			continue
		case err != nil:
			return Result{}, fmt.Errorf("persist property request: %w", err)
		case !created:
			// Lost a race against a concurrent request with the same key.
			return replay(rec, hash)
		}
		return result(rec, false), nil
	}
	return Result{}, errors.New("could not allocate a unique reference")
}

// Inbox lists the newest requests visible under scope, at most
// MaxInboxItems, without paging. InboxPage is the paged form.
func (s *Service) Inbox(ctx context.Context, scope InboxScope) ([]InboxItem, error) {
	page, err := s.store.ListInbox(ctx, InboxFilter{Scope: scope, Limit: MaxInboxItems})
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}
	items := make([]InboxItem, 0, len(page.Items))
	for _, r := range page.Items {
		items = append(items, ItemOf(r))
	}
	return items, nil
}

// IsDirectSeller reports whether the user sells at least one listing directly.
func (s *Service) IsDirectSeller(ctx context.Context, userID uuid.UUID) (bool, error) {
	ok, err := s.store.IsDirectSeller(ctx, userID)
	if err != nil {
		return false, fmt.Errorf("check direct seller: %w", err)
	}
	return ok, nil
}

// ResolveRecipient picks who receives a request: the assigned agent, else the
// organisation inbox, else the direct seller. It returns nil when nobody is
// eligible (the request is then stored as a routing exception).
func ResolveRecipient(l Listing) *Recipient {
	switch {
	case l.AgentUserID != nil && l.AgentEligible:
		return &Recipient{Type: RecipientAgent, UserID: l.AgentUserID, OrganisationID: l.OrganisationID}
	case l.OrganisationID != nil && l.OrganisationEligible:
		return &Recipient{Type: RecipientOrganisation, OrganisationID: l.OrganisationID}
	case l.SellerUserID != nil && l.SellerEligible:
		return &Recipient{Type: RecipientSeller, UserID: l.SellerUserID}
	}
	return nil
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
		RequestID:   rec.ID,
		Reference:   rec.Reference,
		Status:      StatusReceived,
		CreatedAt:   rec.CreatedAt.UTC(),
		Appointment: rec.Appointment,
		Replayed:    replayed,
	}
}

// NewReference returns a random human-friendly reference such as RP-7K3M9QXD.
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
	return "RP-" + string(out), nil
}
