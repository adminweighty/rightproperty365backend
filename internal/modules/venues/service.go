package venues

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
)

// ListingInfo is a listing as this module needs it, whatever its status.
type ListingInfo struct {
	ID             uuid.UUID
	Slug           string
	Reference      string
	Title          string
	PropertyType   string
	MarketCode     string
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID
	// Public: published, unexpired, from a verified advertiser (now).
	Public bool
}

// Listings reads listings.
type Listings interface {
	// Listing returns a listing in any status, or ErrListingNotFound.
	Listing(ctx context.Context, id uuid.UUID) (ListingInfo, error)
	// AgentListingIDs returns the organisation's listings the member is the
	// assigned agent of.
	AgentListingIDs(ctx context.Context, organisationID, agentUserID uuid.UUID) ([]uuid.UUID, error)
}

// Markets answers a country's IANA zone and currency.
type Markets interface {
	Market(ctx context.Context, code string) (zone, currency string, err error)
}

// HourlyTypes returns the property type codes flagged hourly_bookable in
// the reference data right now.
type HourlyTypes func(ctx context.Context) (map[string]bool, error)

// Deps are the service's collaborators.
type Deps struct {
	Store    Store
	Listings Listings
	Markets  Markets
	Hourly   HourlyTypes
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service holds the hourly rental rules.
type Service struct {
	d      Deps
	newRef func() (string, error)
}

// NewService builds the Service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	return &Service{d: d, newRef: NewReference}
}

func (s *Service) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

func (s *Service) audit(ctx context.Context, event string, attrs ...slog.Attr) {
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx))}
	s.d.Logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

const (
	referenceAlphabet = "23456789ABCDEFGHJKMNPQRSTVWXYZ"
	referenceLength   = 8
	referenceAttempts = 5
	maxMessageRunes   = 1000
	maxReasonRunes    = 500
	maxAttendees      = 10000
	maxPrivacyVersion = 32
)

// NewReference returns a random reference such as VB-7K3M9QXD.
func NewReference() (string, error) {
	out := make([]byte, referenceLength)
	n := big.NewInt(int64(len(referenceAlphabet)))
	for i := range out {
		v, err := rand.Int(rand.Reader, n)
		if err != nil {
			return "", err
		}
		out[i] = referenceAlphabet[v.Int64()]
	}
	return "VB-" + string(out), nil
}

func newID() uuid.UUID {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.New()
	}
	return id
}

// venue is a listing with what is needed to book it.
type venue struct {
	listing  ListingInfo
	settings Settings
	zone     string
	loc      *time.Location
	currency string
	eligible bool
	stored   bool
}

func (s *Service) load(ctx context.Context, listingID uuid.UUID) (venue, error) {
	l, err := s.d.Listings.Listing(ctx, listingID)
	if err != nil {
		return venue{}, err
	}
	zone, currency, err := s.d.Markets.Market(ctx, l.MarketCode)
	if err != nil {
		return venue{}, fmt.Errorf("load market: %w", err)
	}
	if zone == "" {
		zone = "UTC"
	}
	types, err := s.d.Hourly(ctx)
	if err != nil {
		return venue{}, fmt.Errorf("load property types: %w", err)
	}
	set, ok, err := s.d.Store.Settings(ctx, l.ID)
	if err != nil {
		return venue{}, err
	}
	if !ok {
		set = DefaultSettings(l.ID, currency)
	}
	return venue{listing: l, settings: set, zone: zone, loc: location(zone), currency: currency, eligible: types[l.PropertyType], stored: ok}, nil
}

// bookable loads a listing the public may book by the hour right now
// (ErrVenueNotFound otherwise).
func (s *Service) bookable(ctx context.Context, listingID uuid.UUID) (venue, error) {
	v, err := s.load(ctx, listingID)
	if errors.Is(err, ErrListingNotFound) {
		return venue{}, ErrVenueNotFound
	}
	if err != nil {
		return venue{}, err
	}
	if !v.listing.Public || !v.eligible || !v.stored || !v.settings.Enabled {
		return venue{}, ErrVenueNotFound
	}
	return v, nil
}

// ---------------------------------------------------------------------------
// Settings (/v1/me/listings/{id}/hourly)
// ---------------------------------------------------------------------------

// SettingsView is the answer of GET and PUT .../hourly.
type SettingsView struct {
	Settings     Settings
	PropertyType string
	Eligible     bool
	Configured   bool
	TimeZone     string
	Currencies   []string
}

func (v venue) view() SettingsView {
	currencies := []string{}
	if v.currency != "" {
		currencies = append(currencies, v.currency)
	}
	return SettingsView{Settings: v.settings, PropertyType: v.listing.PropertyType, Eligible: v.eligible, Configured: v.stored,
		TimeZone: v.zone, Currencies: currencies}
}

// GetSettings returns a listing's hourly settings for whoever may manage it.
func (s *Service) GetSettings(ctx context.Context, p *auth.Principal, listingID uuid.UUID) (SettingsView, error) {
	if p == nil {
		return SettingsView{}, ErrUnauthenticated
	}
	v, err := s.load(ctx, listingID)
	if err != nil {
		return SettingsView{}, err
	}
	if !p.MayManageListing(v.listing.OrganisationID, v.listing.SellerUserID) {
		return SettingsView{}, ErrListingNotFound
	}
	return v.view(), nil
}

// PutSettings replaces a listing's hourly settings. Whoever may edit the
// listing may; the property type must be hourly_bookable (400 otherwise).
// The settings are operational, like a calendar: no review.
func (s *Service) PutSettings(ctx context.Context, p *auth.Principal, listingID uuid.UUID, in SettingsInput) (SettingsView, error) {
	if p == nil {
		return SettingsView{}, ErrUnauthenticated
	}
	v, err := s.load(ctx, listingID)
	if err != nil {
		return SettingsView{}, err
	}
	l := v.listing
	if !p.MayEditListing(l.OrganisationID, l.AgentUserID, l.SellerUserID) {
		if p.MayManageListing(l.OrganisationID, l.SellerUserID) {
			return SettingsView{}, ErrForbidden
		}
		return SettingsView{}, ErrListingNotFound
	}
	if !v.eligible {
		return SettingsView{}, ErrListingNotEligible
	}
	currencies := v.view().Currencies
	next, err := in.apply(l.ID, currencies)
	if err != nil {
		return SettingsView{}, err
	}
	user := p.UserID
	next.UpdatedBy, next.UpdatedAt = &user, s.now()
	if err := s.d.Store.PutSettings(ctx, next); err != nil {
		return SettingsView{}, err
	}
	s.audit(ctx, "venue_settings.changed", slog.String("listing_id", l.ID.String()), slog.Bool("enabled", next.Enabled),
		slog.String("actor_user_id", p.UserID.String()))
	v.settings, v.stored = next, true
	return v.view(), nil
}

// Summary is a listing's `hourly_booking` (GET /v1/listings, a card).
type Summary struct {
	Enabled          bool
	RatePerHour      string
	Currency         string
	IncrementMinutes int
}

// Summaries returns the hourly booking summary of those listings whose
// property type (types: listing id -> property type code) is
// hourly_bookable and that have settings. Enabled is the lister's switch.
func (s *Service) Summaries(ctx context.Context, types map[uuid.UUID]string) (map[uuid.UUID]Summary, error) {
	out := map[uuid.UUID]Summary{}
	if len(types) == 0 {
		return out, nil
	}
	hourly, err := s.d.Hourly(ctx)
	if err != nil {
		return nil, err
	}
	var ids []uuid.UUID
	for id, t := range types {
		if hourly[t] {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return out, nil
	}
	got, err := s.d.Store.SettingsOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	for id, v := range got {
		out[id] = Summary{Enabled: v.Enabled, RatePerHour: v.RatePerHour, Currency: v.Currency, IncrementMinutes: v.IncrementMinutes}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Availability and quotes (public)
// ---------------------------------------------------------------------------

// AvailabilityView is the answer of GET /v1/venues/{id}/availability.
type AvailabilityView struct {
	ListingID    uuid.UUID
	Settings     Settings
	TimeZone     string
	Location     *time.Location
	BookableFrom time.Time
	BookableTo   time.Time
	Days         []AvailabilityDay
}

// Availability lays out the local dates from..to of a bookable venue.
func (s *Service) Availability(ctx context.Context, listingID uuid.UUID, from, to string) (AvailabilityView, error) {
	fields := map[string]string{}
	fd, err1 := time.Parse(dateLayout, from)
	td, err2 := time.Parse(dateLayout, to)
	if err1 != nil {
		fields["from"] = "is required, a date YYYY-MM-DD"
	}
	if err2 != nil {
		fields["to"] = "is required, a date YYYY-MM-DD"
	}
	if err1 == nil && err2 == nil {
		switch {
		case td.Before(fd):
			fields["to"] = "must be on or after from"
		case td.Sub(fd) >= MaxAvailabilityDays*24*time.Hour:
			fields["to"] = fmt.Sprintf("at most %d days at a time", MaxAvailabilityDays)
		}
	}
	if len(fields) > 0 {
		return AvailabilityView{}, &ValidationError{Fields: fields}
	}
	v, err := s.bookable(ctx, listingID)
	if err != nil {
		return AvailabilityView{}, err
	}
	now := s.now()
	y1, m1, d1 := fd.Date()
	y2, m2, d2 := td.Date()
	windowStart := wallTime(y1, m1, d1, 0, v.loc).Add(-24 * time.Hour)
	windowEnd := wallTime(y2, m2, d2, 24*60, v.loc).Add(24 * time.Hour)
	occupied, err := s.d.Store.Occupied(ctx, listingID, windowStart, windowEnd, nil)
	if err != nil {
		return AvailabilityView{}, err
	}
	bf, bt := v.settings.Window(now, v.loc)
	return AvailabilityView{ListingID: listingID, Settings: v.settings, TimeZone: v.zone, Location: v.loc, BookableFrom: bf, BookableTo: bt,
		Days: v.settings.Availability(from, to, now, v.loc, occupiedOf(occupied))}, nil
}

// QuoteInput is the body of POST /v1/venues/{id}/quotes.
type QuoteInput struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func parseInstant(raw string) (time.Time, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, "is required, an RFC 3339 instant"
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, "must be an RFC 3339 instant such as 2026-12-05T08:00:00Z"
	}
	return t.UTC(), ""
}

// check validates a slot of the venue at now: its shape (400), then
// whether it can be booked (409), leaving out the booking except.
func (s *Service) check(ctx context.Context, v venue, start, end time.Time, except *uuid.UUID) error {
	if f := v.settings.SlotProblem(start, end, v.loc); len(f) > 0 {
		return &ValidationError{Fields: f}
	}
	buffer := time.Duration(v.settings.CleaningBufferMinutes) * time.Minute
	dayBuffer := 24 * time.Hour
	occupied, err := s.d.Store.Occupied(ctx, v.listing.ID, start.Add(-dayBuffer), end.Add(buffer+dayBuffer), except)
	if err != nil {
		return err
	}
	if reason := v.settings.Unavailable(start, end, s.now(), v.loc, occupiedOf(occupied)); reason != "" {
		return &UnavailableError{Reason: reason}
	}
	return nil
}

// CreateQuote prices a slot of a bookable venue and stores the quote for
// 30 minutes.
func (s *Service) CreateQuote(ctx context.Context, listingID uuid.UUID, in QuoteInput) (Quote, error) {
	fields := map[string]string{}
	start, msg := parseInstant(in.Start)
	if msg != "" {
		fields["start"] = msg
	}
	end, msg := parseInstant(in.End)
	if msg != "" {
		fields["end"] = msg
	}
	if len(fields) > 0 {
		return Quote{}, &ValidationError{Fields: fields}
	}
	v, err := s.bookable(ctx, listingID)
	if err != nil {
		return Quote{}, err
	}
	if err := s.check(ctx, v, start, end, nil); err != nil {
		return Quote{}, err
	}
	pricing, err := Price(v.settings, start, end, v.zone)
	if err != nil {
		return Quote{}, err
	}
	now := s.now()
	q := Quote{ID: newID(), ListingID: listingID, Start: start, End: end, BufferMinutes: v.settings.CleaningBufferMinutes, TimeZone: v.zone,
		Pricing: pricing, CreatedAt: now, ExpiresAt: now.Add(QuoteLifetime)}
	if err := s.d.Store.InsertQuote(ctx, q); err != nil {
		return Quote{}, err
	}
	return q, nil
}

// usableQuote loads a quote of the booking's listing that has not expired
// and that no other booking used.
func (s *Service) usableQuote(ctx context.Context, rawID string, listingID uuid.UUID, self *uuid.UUID) (Quote, error) {
	id, err := uuid.Parse(strings.TrimSpace(rawID))
	if err != nil {
		return Quote{}, &ValidationError{Fields: map[string]string{"quote_id": "is required, the quote_id of a quote"}}
	}
	q, err := s.d.Store.Quote(ctx, id)
	if err != nil {
		return Quote{}, err
	}
	if q.ListingID != listingID {
		return Quote{}, ErrQuoteNotFound
	}
	if q.UsedBy != nil && (self == nil || *q.UsedBy != *self) {
		return Quote{}, ErrQuoteUsed
	}
	if !s.now().Before(q.ExpiresAt) {
		return Quote{}, &QuoteExpiredError{ExpiredAt: q.ExpiresAt}
	}
	return q, nil
}
