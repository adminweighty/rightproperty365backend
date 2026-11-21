package appointments

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// ListingSource supplies the routing state of a listing (the property
// request store).
type ListingSource interface {
	GetListing(ctx context.Context, id uuid.UUID) (propertyrequests.Listing, error)
}

// ProfileSource supplies the routing state of a directory profile (the
// business enquiry store).
type ProfileSource interface {
	GetProfile(ctx context.Context, kind, slug string) (businessenquiries.Profile, error)
}

// Catalogue names a listing and its advertiser's public profile. ok is false
// when the listing has no public record.
type Catalogue interface {
	ListingCard(ctx context.Context, id uuid.UUID) (card ListingCard, ok bool, err error)
}

// Zones returns the IANA time zone of a market ("" when unknown).
type Zones interface {
	MarketTimezone(ctx context.Context, marketID uuid.UUID) (string, error)
}

// People answers questions about accounts.
type People interface {
	// Member returns the name of an ACTIVE user who is a member of the
	// organisation; ok is false otherwise.
	Member(ctx context.Context, organisationID, userID uuid.UUID) (name string, ok bool, err error)
	// Name returns a user's display name ("" when unknown).
	Name(ctx context.Context, userID uuid.UUID) (string, error)
}

// Vocabulary supplies the codes accepted right now (reference data).
type Vocabulary interface {
	ContactMethods(ctx context.Context) ([]string, error)
	DirectoryKinds(ctx context.Context) ([]string, error)
}

// Deps is everything the Service needs.
type Deps struct {
	Store      Store
	Listings   ListingSource
	Profiles   ProfileSource
	Catalogue  Catalogue
	Zones      Zones
	People     People
	Vocabulary Vocabulary
	// Availability (API 0.51.0): settings, services and busy time. Nil:
	// no slots are offered anywhere.
	Availability AvailabilityStore
	Now          func() time.Time
}

// Service holds the appointment rules.
type Service struct {
	d      Deps
	newRef func() (string, error)
}

// NewService builds the Service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d, newRef: NewReference}
}

func (s *Service) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

const (
	referenceAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"
	referenceLength   = 8
	referenceAttempts = 5

	maxNoteRunes      = 1000
	maxLocationRunes  = 200
	maxReasonRunes    = 500
	minIdempotencyKey = 8
	maxIdempotencyKey = 128
	maxPrivacyVersion = 32
)

var (
	htmlTagRe = regexp.MustCompile(`<[/!?]?[A-Za-z][^>]*>|<!--`)
	e164Re    = regexp.MustCompile(`^\+[1-9][0-9]{6,14}$`)
)

// NewReference returns a random human-friendly reference such as AP-7K3M9QXD.
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
	return "AP-" + string(out), nil
}

// plainText validates free text: trimmed, at most max characters, no control
// characters and no HTML. It returns the cleaned text and "" or a message.
func plainText(raw string, max int, multiline bool) (string, string) {
	v := strings.TrimSpace(strings.ReplaceAll(raw, "\r\n", "\n"))
	switch {
	case utf8.RuneCountInString(v) > max:
		return v, fmt.Sprintf("must be at most %d characters", max)
	case !utf8.ValidString(v) || hasControlChars(v, multiline):
		return v, "contains characters that are not allowed"
	case htmlTagRe.MatchString(v):
		return v, "must be plain text without HTML"
	}
	return v, ""
}

func hasControlChars(s string, allowWhitespace bool) bool {
	for _, r := range s {
		if allowWhitespace && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func isPrintableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// parseStart validates a start time: RFC 3339, in the future and at most
// MaxAdvance ahead. It returns the time in UTC, or a message.
func parseStart(raw string, now time.Time) (time.Time, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, "is required"
	}
	t, err := time.Parse(time.RFC3339, raw)
	switch {
	case err != nil:
		return time.Time{}, "must be an RFC 3339 timestamp"
	case !t.After(now):
		return time.Time{}, "must be in the future"
	case t.After(now.Add(MaxAdvance)):
		return time.Time{}, "must be within the next 12 months"
	}
	return t.UTC().Truncate(time.Second), ""
}

func checkDuration(minutes int) string {
	if minutes < MinDurationMinutes || minutes > MaxDurationMinutes {
		return fmt.Sprintf("must be between %d and %d minutes", MinDurationMinutes, MaxDurationMinutes)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Creation
// ---------------------------------------------------------------------------

// Customer is the signed-in user creating or changing an appointment.
type Customer struct {
	UserID uuid.UUID
	Name   string
	Email  string
}

// RecipientRef names a directory profile.
type RecipientRef struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
}

// Consent records acceptance of a privacy notice version.
type Consent struct {
	PrivacyVersion string `json:"privacy_version"`
	Accepted       bool   `json:"accepted"`
}

// Input is the JSON body of POST /v1/appointments.
type Input struct {
	Type                   string        `json:"type"`
	ListingID              *string       `json:"listing_id"`
	Recipient              *RecipientRef `json:"recipient"`
	StartUTC               string        `json:"start_utc"`
	DurationMinutes        *int          `json:"duration_minutes"`
	Note                   string        `json:"note"`
	PreferredContactMethod string        `json:"preferred_contact_method"`
	Phone                  string        `json:"phone"`
	IdempotencyKey         string        `json:"idempotency_key"`
	Consent                *Consent      `json:"consent"`
	// ServiceID is a consultation service of the professional (API 0.51.0).
	ServiceID string `json:"service_id"`

	// Customer is the authenticated caller, set by the handler from the
	// session and never read from the body.
	Customer Customer `json:"-"`
}

// Create validates, routes and persists an appointment requested by a
// signed-in customer. replayed is true when the idempotency key was already
// used for the same content (the original appointment is returned).
func (s *Service) Create(ctx context.Context, in Input, headerKey string) (a Appointment, replayed bool, err error) {
	now := s.now()
	fields := map[string]string{}

	key := strings.TrimSpace(in.IdempotencyKey)
	if key == "" {
		key = strings.TrimSpace(headerKey)
	}
	switch {
	case key == "":
		fields["idempotency_key"] = "is required, in the body or the Idempotency-Key header"
	case len(key) < minIdempotencyKey || len(key) > maxIdempotencyKey:
		fields["idempotency_key"] = "must be between 8 and 128 characters"
	case !isPrintableASCII(key):
		fields["idempotency_key"] = "must contain only printable ASCII characters without spaces"
	}

	switch {
	case in.Type == "":
		fields["type"] = "is required"
	case !slices.Contains(Types(), in.Type):
		fields["type"] = "must be one of " + strings.Join(Types(), ", ")
	}

	// Exactly one target.
	var listingID uuid.UUID
	hasListing := in.ListingID != nil && strings.TrimSpace(*in.ListingID) != ""
	var kind, slug string
	switch {
	case hasListing && in.Recipient != nil:
		fields["recipient"] = "send either listing_id or recipient, not both"
	case !hasListing && in.Recipient == nil:
		fields["listing_id"] = "either listing_id or recipient is required"
	case hasListing:
		raw := strings.TrimSpace(*in.ListingID)
		id, err := uuid.Parse(raw)
		if err != nil || len(raw) != 36 {
			fields["listing_id"] = "must be a UUID"
		}
		listingID = id
	default:
		kinds, err := s.d.Vocabulary.DirectoryKinds(ctx)
		if err != nil {
			return Appointment{}, false, fmt.Errorf("load directory kinds: %w", err)
		}
		kind, slug = strings.TrimSpace(in.Recipient.Kind), strings.TrimSpace(in.Recipient.Slug)
		switch {
		case kind == "":
			fields["recipient.kind"] = "is required"
		case !slices.Contains(kinds, kind):
			fields["recipient.kind"] = "must be one of " + strings.Join(kinds, ", ")
		}
		if slug == "" || len(slug) > 120 {
			fields["recipient.slug"] = "is required"
		}
		if in.Type == TypeViewing {
			fields["listing_id"] = "is required for a VIEWING"
		}
	}

	start, msg := parseStart(in.StartUTC, now)
	if msg != "" {
		fields["start_utc"] = msg
	}
	duration := DefaultDurationMinutes
	if in.DurationMinutes != nil {
		duration = *in.DurationMinutes
		if msg := checkDuration(duration); msg != "" {
			fields["duration_minutes"] = msg
		}
	}
	note, msg := plainText(in.Note, maxNoteRunes, true)
	if msg != "" {
		fields["note"] = msg
	}

	methods, err := s.d.Vocabulary.ContactMethods(ctx)
	if err != nil {
		return Appointment{}, false, fmt.Errorf("load contact methods: %w", err)
	}
	switch {
	case in.PreferredContactMethod == "":
		fields["preferred_contact_method"] = "is required"
	case !slices.Contains(methods, in.PreferredContactMethod):
		fields["preferred_contact_method"] = "must be one of " + strings.Join(methods, ", ")
	}
	phone := strings.TrimSpace(in.Phone)
	switch {
	case phone != "" && !e164Re.MatchString(phone):
		fields["phone"] = "must be in E.164 format, for example +263771234567"
	case phone == "" && (in.PreferredContactMethod == propertyrequests.ContactPhone || in.PreferredContactMethod == propertyrequests.ContactWhatsApp):
		fields["phone"] = "is required when the preferred contact method is PHONE or WHATSAPP"
	}

	privacyVersion := ""
	if in.Consent == nil {
		fields["consent"] = "is required"
	} else {
		privacyVersion = strings.TrimSpace(in.Consent.PrivacyVersion)
		switch {
		case privacyVersion == "":
			fields["consent.privacy_version"] = "is required"
		case len(privacyVersion) > maxPrivacyVersion || !isPrintableASCII(privacyVersion):
			fields["consent.privacy_version"] = "is not a valid privacy notice version"
		}
		if !in.Consent.Accepted {
			fields["consent.accepted"] = "must be true"
		}
	}
	if len(fields) > 0 {
		return Appointment{}, false, &ValidationError{Fields: fields}
	}

	// The hash answers "is this the same request?"; the customer is part of
	// it, so one customer can never replay another's key.
	target := kind + "/" + slug
	if hasListing {
		target = listingID.String()
	}
	canonical, _ := json.Marshal([]string{
		in.Type, target, start.Format(time.RFC3339), fmt.Sprint(duration), note, in.PreferredContactMethod, phone,
		privacyVersion, in.Customer.UserID.String(), strings.TrimSpace(in.ServiceID),
	})
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])

	// The replay check runs before the target checks, so a retry still
	// succeeds after the listing has since expired.
	existing, err := s.d.Store.FindByIdempotencyKey(ctx, key)
	switch {
	case err == nil:
		if existing.RequestHash != hash {
			return Appointment{}, false, ErrIdempotencyConflict
		}
		return existing, true, nil
	case !errors.Is(err, ErrNotFound):
		return Appointment{}, false, fmt.Errorf("look up idempotency key: %w", err)
	}

	customer := in.Customer.UserID
	a = Appointment{
		Type: in.Type, Status: StatusRequested,
		CustomerUserID: &customer, CustomerName: strings.TrimSpace(in.Customer.Name),
		CustomerEmail: strings.ToLower(strings.TrimSpace(in.Customer.Email)), CustomerPhone: phone,
		PreferredContactMethod: in.PreferredContactMethod, CustomerNote: note,
		StartUTC: start, DurationMinutes: duration,
		IdempotencyKey: key, RequestHash: hash, PrivacyVersion: privacyVersion, ConsentAcceptedAt: &now,
		CreatedAt: now, UpdatedAt: now,
	}

	if hasListing {
		listing, err := s.d.Listings.GetListing(ctx, listingID)
		if errors.Is(err, propertyrequests.ErrListingNotFound) {
			return Appointment{}, false, ErrListingNotFound
		}
		if err != nil {
			return Appointment{}, false, fmt.Errorf("load listing: %w", err)
		}
		if listing.Status != propertyrequests.ListingStatusPublished || (listing.ExpiresAt != nil && !listing.ExpiresAt.After(now)) {
			return Appointment{}, false, ErrListingUnavailable
		}
		recipient := propertyrequests.ResolveRecipient(listing)
		if recipient == nil {
			// Nobody could confirm it: refuse instead of promising a time.
			return Appointment{}, false, ErrListingUnavailable
		}
		a.MarketID, a.ListingID = listing.MarketID, &listing.ID
		a.RecipientType, a.AssignedUserID, a.OrganisationID = recipient.Type, recipient.UserID, recipient.OrganisationID
		if err := s.nameListing(ctx, &a); err != nil {
			return Appointment{}, false, err
		}
	} else {
		profile, err := s.d.Profiles.GetProfile(ctx, kind, slug)
		if errors.Is(err, businessenquiries.ErrRecipientNotFound) {
			return Appointment{}, false, ErrRecipientNotFound
		}
		if err != nil {
			return Appointment{}, false, fmt.Errorf("load profile: %w", err)
		}
		recipient := businessenquiries.ResolveRecipient(profile)
		if profile.Status != businessenquiries.ProfileActive || recipient == nil {
			return Appointment{}, false, ErrRecipientUnavailable
		}
		org := recipient.OrganisationID
		a.MarketID, a.OrganisationID, a.AssignedUserID = profile.MarketID, &org, recipient.UserID
		a.RecipientType = RecipientOrganisation
		if recipient.UserID != nil {
			a.RecipientType = RecipientAgent
		}
		a.ProfileKind, a.ProfileSlug = profile.Kind, profile.Slug
		a.WithKind, a.WithSlug, a.WithName = profile.Kind, profile.Slug, profile.Name
	}
	if a.Timezone, err = s.timezone(ctx, a.MarketID); err != nil {
		return Appointment{}, false, err
	}
	// Availability (API 0.51.0): the service, and an offered slot when the
	// professional's calendar is on.
	if err := s.bookable(ctx, &a, in.ServiceID, "start_utc"); err != nil {
		return Appointment{}, false, err
	}
	if a.ID, err = uuid.NewV7(); err != nil {
		return Appointment{}, false, fmt.Errorf("generate appointment id: %w", err)
	}

	for range referenceAttempts {
		if a.Reference, err = s.newRef(); err != nil {
			return Appointment{}, false, fmt.Errorf("generate reference: %w", err)
		}
		stored, created, err := s.d.Store.Create(ctx, a, requestedEntry(a, PartyCustomer))
		switch {
		case errors.Is(err, ErrReferenceCollision):
			continue
		case err != nil:
			return Appointment{}, false, fmt.Errorf("persist appointment: %w", err)
		case !created:
			// Lost a race against a concurrent request with the same key.
			if stored.RequestHash != hash {
				return Appointment{}, false, ErrIdempotencyConflict
			}
			return stored, true, nil
		}
		return stored, false, nil
	}
	return Appointment{}, false, errors.New("could not allocate a unique reference")
}

func (s *Service) timezone(ctx context.Context, marketID uuid.UUID) (string, error) {
	zone, err := s.d.Zones.MarketTimezone(ctx, marketID)
	if err != nil {
		return "", fmt.Errorf("load market time zone: %w", err)
	}
	if zone == "" {
		zone = "UTC"
	}
	return zone, nil
}

// nameListing snapshots the listing's public name and its advertiser's
// public profile onto the appointment.
func (s *Service) nameListing(ctx context.Context, a *Appointment) error {
	card, ok, err := s.d.Catalogue.ListingCard(ctx, *a.ListingID)
	if err != nil {
		return fmt.Errorf("load listing name: %w", err)
	}
	if ok {
		a.ListingSlug, a.ListingTitle, a.ListingReference = card.Slug, card.Title, card.Reference
		a.WithKind, a.WithSlug, a.WithName = card.WithKind, card.WithSlug, card.WithName
	}
	return nil
}

func requestedEntry(a Appointment, actor string) TimelineEntry {
	start := a.StartUTC
	return TimelineEntry{
		ID: uuid.New(), AppointmentID: a.ID, Event: TimelineRequested, ActorType: actor, ActorUserID: a.CustomerUserID,
		ToStatus: StatusRequested, StartUTC: &start, At: a.CreatedAt,
	}
}

// Linked is the appointment a property request carries to its store
// (propertyrequests.LinkedAppointment.Data).
type Linked struct {
	Appointment Appointment
	Entry       TimelineEntry
}

// PlanAppointment implements propertyrequests.AppointmentPlanner: a viewing
// or consultation request with a preferred slot gets an appointment in status
// REQUESTED, routed exactly as the request was. It returns nil when the
// request has no slot, is of another type, or could not be routed (there
// would be nobody to confirm it).
func (s *Service) PlanAppointment(ctx context.Context, req propertyrequests.NewRequest) (*propertyrequests.LinkedAppointment, error) {
	if req.PreferredSlot == nil || req.Recipient == nil {
		return nil, nil
	}
	var kind string
	switch req.RequestType {
	case propertyrequests.TypeViewingRequest:
		kind = TypeViewing
	case propertyrequests.TypeConsultationRequest:
		kind = TypeConsultation
	default:
		return nil, nil
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("generate appointment id: %w", err)
	}
	reference, err := s.newRef()
	if err != nil {
		return nil, fmt.Errorf("generate appointment reference: %w", err)
	}
	requestID, listingID := req.ID, req.ListingID
	a := Appointment{
		ID: id, Reference: reference, Type: kind, Status: StatusRequested, MarketID: req.MarketID,
		ListingID: &listingID, PropertyRequestID: &requestID, LinkedReference: req.Reference,
		CustomerUserID: req.CustomerUserID, CustomerName: req.GuestName, CustomerEmail: req.GuestEmail, CustomerPhone: req.GuestPhone,
		PreferredContactMethod: req.PreferredContactMethod, CustomerNote: req.Message,
		RecipientType: req.Recipient.Type, OrganisationID: req.Recipient.OrganisationID, AssignedUserID: req.Recipient.UserID,
		StartUTC: req.PreferredSlot.UTC().Truncate(time.Second), DurationMinutes: DefaultDurationMinutes,
		CreatedAt: req.CreatedAt, UpdatedAt: req.CreatedAt,
	}
	if a.Timezone, err = s.timezone(ctx, a.MarketID); err != nil {
		return nil, err
	}
	if err := s.bookable(ctx, &a, req.ServiceID, "preferred_slot_utc"); err != nil {
		var verr *ValidationError
		if errors.As(err, &verr) {
			return nil, &propertyrequests.ValidationError{Fields: verr.Fields}
		}
		return nil, err
	}
	if err := s.nameListing(ctx, &a); err != nil {
		return nil, err
	}
	return &propertyrequests.LinkedAppointment{ID: a.ID, Reference: a.Reference, Data: Linked{Appointment: a, Entry: requestedEntry(a, PartyCustomer)}}, nil
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Mine returns the customer's own appointment, or ErrNotFound (also for
// somebody else's).
func (s *Service) Mine(ctx context.Context, userID, id uuid.UUID) (Appointment, error) {
	a, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Appointment{}, err
	}
	if a.CustomerUserID == nil || *a.CustomerUserID != userID {
		return Appointment{}, ErrNotFound
	}
	return a, nil
}

// Received returns an appointment visible under scope, or ErrNotFound.
func (s *Service) Received(ctx context.Context, scope Scope, id uuid.UUID) (Appointment, error) {
	a, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Appointment{}, err
	}
	if !scope.Sees(a) {
		return Appointment{}, ErrNotFound
	}
	return a, nil
}

// Timeline returns the appointment's timeline, oldest first.
func (s *Service) Timeline(ctx context.Context, id uuid.UUID) ([]TimelineEntry, error) {
	return s.d.Store.Timeline(ctx, id)
}

// List returns one page under the filter.
func (s *Service) List(ctx context.Context, f ListFilter) ([]Appointment, int, error) {
	f.Now = s.now()
	return s.d.Store.List(ctx, f)
}

// Count returns the badge counts under scope.
func (s *Service) Count(ctx context.Context, scope Scope) (Counts, error) {
	if scope.Empty() {
		return Counts{}, nil
	}
	return s.d.Store.Count(ctx, scope, s.now())
}

// AssigneeName returns the display name of a user ("" when unknown).
func (s *Service) AssigneeName(ctx context.Context, userID uuid.UUID) (string, error) {
	return s.d.People.Name(ctx, userID)
}

// ---------------------------------------------------------------------------
// Transitions
//
//	REQUESTED ──confirm──> CONFIRMED ──complete──> COMPLETED
//	    │                     │  └────no-show───> NO_SHOW
//	    └──── propose a new time ────> RESCHEDULE_REQUESTED ──the other party accepts──> CONFIRMED
//	REQUESTED | CONFIRMED | RESCHEDULE_REQUESTED ──cancel──> CANCELLED
// ---------------------------------------------------------------------------

// errStartPassed is an ErrInvalidState with its own message.
var errStartPassed = fmt.Errorf("%w: that time has already passed", ErrInvalidState)

// actor is who performs a transition.
type actor struct {
	party  string
	userID uuid.UUID
	// allowed reports whether the actor may touch the appointment at all;
	// false is answered as "not found".
	allowed func(Appointment) bool
}

func customerActor(userID uuid.UUID) actor {
	return actor{party: PartyCustomer, userID: userID, allowed: func(a Appointment) bool {
		return a.CustomerUserID != nil && *a.CustomerUserID == userID
	}}
}

func professionalActor(scope Scope) actor {
	return actor{party: PartyProfessional, userID: scope.UserID, allowed: scope.Sees}
}

func (s *Service) update(ctx context.Context, who actor, id uuid.UUID, change func(cur Appointment, now time.Time) (Mutation, error)) (Appointment, error) {
	now := s.now()
	return s.d.Store.Update(ctx, id, func(cur Appointment) (Mutation, error) {
		if !who.allowed(cur) {
			return Mutation{}, ErrNotFound
		}
		m, err := change(cur, now)
		if err != nil {
			return Mutation{}, err
		}
		m.Next.UpdatedAt = now
		user := who.userID
		m.Entry.ID, m.Entry.AppointmentID, m.Entry.At = uuid.New(), cur.ID, now
		m.Entry.ActorType, m.Entry.ActorUserID = who.party, &user
		m.Entry.FromStatus, m.Entry.ToStatus = cur.Status, m.Next.Status
		return m, nil
	})
}

// propose records a new time proposed by one party; the other must accept it.
func propose(cur Appointment, by string, start time.Time, note string) (Mutation, error) {
	switch {
	case cur.Status == StatusRequested || cur.Status == StatusConfirmed:
	case cur.Status == StatusRescheduleRequested && cur.ProposedBy != by:
		// A counter-proposal to the other party's time.
	default:
		return Mutation{}, ErrInvalidState
	}
	next := cur
	next.Status, next.ProposedStartUTC, next.ProposedBy = StatusRescheduleRequested, &start, by
	return Mutation{Next: next, Entry: TimelineEntry{Event: TimelineRescheduleRequested, Note: note, StartUTC: &start}, Event: EventRescheduleRequested}, nil
}

// confirmAt confirms the appointment at start.
func confirmAt(cur Appointment, start time.Time, now time.Time) (Mutation, error) {
	if !start.After(now) {
		return Mutation{}, errStartPassed
	}
	next := cur
	next.Status, next.StartUTC, next.ProposedStartUTC, next.ProposedBy = StatusConfirmed, start, nil, ""
	return Mutation{Next: next, Entry: TimelineEntry{Event: TimelineConfirmed, StartUTC: &start}, Event: EventConfirmed}, nil
}

func cancel(cur Appointment, by, reason string) (Mutation, error) {
	if !cur.Open() {
		return Mutation{}, ErrInvalidState
	}
	next := cur
	next.Status, next.CancelledBy, next.CancelReason = StatusCancelled, by, reason
	next.ProposedStartUTC, next.ProposedBy = nil, ""
	return Mutation{Next: next, Entry: TimelineEntry{Event: TimelineCancelled, Note: reason}, Event: EventCancelled}, nil
}

// RescheduleInput is the body of both reschedule endpoints.
type RescheduleInput struct {
	StartUTC string `json:"start_utc"`
	Note     string `json:"note"`
}

func (s *Service) reschedule(ctx context.Context, who actor, id uuid.UUID, in RescheduleInput) (Appointment, error) {
	fields := map[string]string{}
	start, msg := parseStart(in.StartUTC, s.now())
	if msg != "" {
		fields["start_utc"] = msg
	}
	note, msg := plainText(in.Note, maxReasonRunes, true)
	if msg != "" {
		fields["note"] = msg
	}
	if len(fields) > 0 {
		return Appointment{}, &ValidationError{Fields: fields}
	}
	return s.update(ctx, who, id, func(cur Appointment, _ time.Time) (Mutation, error) {
		if cur.Open() && cur.StartUTC.Equal(start) && cur.Status != StatusRescheduleRequested {
			return Mutation{}, &ValidationError{Fields: map[string]string{"start_utc": "must differ from the current start time"}}
		}
		return propose(cur, who.party, start, note)
	})
}

// CustomerCancel cancels the customer's own appointment.
func (s *Service) CustomerCancel(ctx context.Context, userID, id uuid.UUID, reason string) (Appointment, error) {
	reason, msg := plainText(reason, maxReasonRunes, true)
	if msg != "" {
		return Appointment{}, &ValidationError{Fields: map[string]string{"reason": msg}}
	}
	return s.update(ctx, customerActor(userID), id, func(cur Appointment, _ time.Time) (Mutation, error) {
		return cancel(cur, PartyCustomer, reason)
	})
}

// CustomerReschedule proposes a new time on behalf of the customer. When the
// professional's calendar is on, it must be an offered slot (API 0.51.0).
func (s *Service) CustomerReschedule(ctx context.Context, userID, id uuid.UUID, in RescheduleInput) (Appointment, error) {
	if start, msg := parseStart(in.StartUTC, s.now()); msg == "" {
		cur, err := s.Mine(ctx, userID, id)
		if err != nil {
			return Appointment{}, err
		}
		msg, _, err := s.checkOffered(ctx, cur.AssignedUserID, start, cur.DurationMinutes, cur.ID)
		if err != nil {
			return Appointment{}, err
		}
		if msg != "" {
			return Appointment{}, &ValidationError{Fields: map[string]string{"start_utc": msg}}
		}
	}
	return s.reschedule(ctx, customerActor(userID), id, in)
}

// CustomerAccept accepts the time the professional proposed.
func (s *Service) CustomerAccept(ctx context.Context, userID, id uuid.UUID) (Appointment, error) {
	return s.update(ctx, customerActor(userID), id, func(cur Appointment, now time.Time) (Mutation, error) {
		if cur.Status != StatusRescheduleRequested || cur.ProposedBy != PartyProfessional || cur.ProposedStartUTC == nil {
			return Mutation{}, ErrInvalidState
		}
		return confirmAt(cur, *cur.ProposedStartUTC, now)
	})
}

// ConfirmInput is the (optional) body of the professional's confirm.
type ConfirmInput struct {
	StartUTC        *string `json:"start_utc"`
	DurationMinutes *int    `json:"duration_minutes"`
	LocationNote    *string `json:"location_note"`
}

// Confirm confirms the requested time, or the time the customer proposed.
// When a DIFFERENT start_utc is given nothing is confirmed: the appointment
// becomes RESCHEDULE_REQUESTED with proposed_by PROFESSIONAL and waits for
// the customer to accept.
func (s *Service) Confirm(ctx context.Context, scope Scope, id uuid.UUID, in ConfirmInput) (Appointment, error) {
	fields := map[string]string{}
	var start *time.Time
	if in.StartUTC != nil && strings.TrimSpace(*in.StartUTC) != "" {
		t, msg := parseStart(*in.StartUTC, s.now())
		if msg != "" {
			fields["start_utc"] = msg
		}
		start = &t
	}
	if in.DurationMinutes != nil {
		if msg := checkDuration(*in.DurationMinutes); msg != "" {
			fields["duration_minutes"] = msg
		}
	}
	var location *string
	if in.LocationNote != nil {
		v, msg := plainText(*in.LocationNote, maxLocationRunes, false)
		if msg != "" {
			fields["location_note"] = msg
		}
		location = &v
	}
	if len(fields) > 0 {
		return Appointment{}, &ValidationError{Fields: fields}
	}
	return s.update(ctx, professionalActor(scope), id, func(cur Appointment, now time.Time) (Mutation, error) {
		// The time on the table: the requested one, or the customer's proposal.
		var offered time.Time
		switch {
		case cur.Status == StatusRequested:
			offered = cur.StartUTC
		case cur.Status == StatusRescheduleRequested && cur.ProposedBy == PartyCustomer && cur.ProposedStartUTC != nil:
			offered = *cur.ProposedStartUTC
		default:
			return Mutation{}, ErrInvalidState
		}
		if in.DurationMinutes != nil {
			cur.DurationMinutes = *in.DurationMinutes
		}
		if location != nil {
			cur.LocationNote = *location
		}
		if start != nil && !start.Equal(offered) {
			return propose(cur, PartyProfessional, *start, "")
		}
		return confirmAt(cur, offered, now)
	})
}

// Reschedule proposes a new time on behalf of the professional.
func (s *Service) Reschedule(ctx context.Context, scope Scope, id uuid.UUID, in RescheduleInput) (Appointment, error) {
	return s.reschedule(ctx, professionalActor(scope), id, in)
}

// Cancel cancels on behalf of the professional; a reason is required.
func (s *Service) Cancel(ctx context.Context, scope Scope, id uuid.UUID, reason string) (Appointment, error) {
	reason, msg := plainText(reason, maxReasonRunes, true)
	if msg == "" && reason == "" {
		msg = "is required"
	}
	if msg != "" {
		return Appointment{}, &ValidationError{Fields: map[string]string{"reason": msg}}
	}
	return s.update(ctx, professionalActor(scope), id, func(cur Appointment, _ time.Time) (Mutation, error) {
		return cancel(cur, PartyProfessional, reason)
	})
}

// finish closes a CONFIRMED appointment whose start time has passed.
func (s *Service) finish(ctx context.Context, scope Scope, id uuid.UUID, status, entry, event string) (Appointment, error) {
	return s.update(ctx, professionalActor(scope), id, func(cur Appointment, now time.Time) (Mutation, error) {
		if cur.Status != StatusConfirmed || now.Before(cur.StartUTC) {
			return Mutation{}, ErrInvalidState
		}
		next := cur
		next.Status = status
		return Mutation{Next: next, Entry: TimelineEntry{Event: entry}, Event: event}, nil
	})
}

// Complete marks a confirmed appointment as having taken place.
func (s *Service) Complete(ctx context.Context, scope Scope, id uuid.UUID) (Appointment, error) {
	return s.finish(ctx, scope, id, StatusCompleted, TimelineCompleted, EventCompleted)
}

// NoShow marks a confirmed appointment the customer did not attend.
func (s *Service) NoShow(ctx context.Context, scope Scope, id uuid.UUID) (Appointment, error) {
	return s.finish(ctx, scope, id, StatusNoShow, TimelineNoShow, EventNoShow)
}

// Assign gives the appointment to a member of its organisation. Only an
// OWNER or MANAGER of that organisation may (ErrForbidden otherwise), only
// while the appointment is open, and only to an active member (ErrNotAMember).
// Assigning a CONFIRMED appointment is refused with ErrSlotTaken when the new
// assignee already has a confirmed appointment at that time.
func (s *Service) Assign(ctx context.Context, scope Scope, id uuid.UUID, rawUserID string) (Appointment, error) {
	rawUserID = strings.TrimSpace(rawUserID)
	userID, err := uuid.Parse(rawUserID)
	if rawUserID == "" {
		return Appointment{}, &ValidationError{Fields: map[string]string{"user_id": "is required"}}
	}
	if err != nil {
		return Appointment{}, &ValidationError{Fields: map[string]string{"user_id": "must be a UUID"}}
	}
	check := func(cur Appointment) error {
		switch {
		case !scope.Sees(cur):
			return ErrNotFound
		case !scope.Manages(cur):
			return ErrForbidden
		case !cur.Open():
			return ErrInvalidState
		}
		return nil
	}
	cur, err := s.d.Store.Get(ctx, id)
	if err != nil {
		return Appointment{}, err
	}
	if err := check(cur); err != nil {
		return Appointment{}, err
	}
	name, ok, err := s.d.People.Member(ctx, *cur.OrganisationID, userID)
	if err != nil {
		return Appointment{}, fmt.Errorf("check membership: %w", err)
	}
	if !ok {
		return Appointment{}, ErrNotAMember
	}
	return s.update(ctx, professionalActor(scope), id, func(cur Appointment, _ time.Time) (Mutation, error) {
		if err := check(cur); err != nil {
			return Mutation{}, err
		}
		next := cur
		next.AssignedUserID, next.RecipientType = &userID, RecipientAgent
		return Mutation{Next: next, Entry: TimelineEntry{Event: TimelineAssigned, Note: "Assigned to " + name}, Event: EventAssigned}, nil
	})
}
