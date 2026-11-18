package venues

import (
	"time"

	"github.com/google/uuid"
)

// The JSON shapes are the contract's "Response shapes" (API 0.33.0).

type settingsJSON struct {
	ListingID             uuid.UUID      `json:"listing_id"`
	PropertyType          string         `json:"property_type"`
	Eligible              bool           `json:"eligible"`
	Configured            bool           `json:"configured"`
	TimeZone              string         `json:"time_zone"`
	Currencies            []string       `json:"currencies"`
	Mode                  string         `json:"mode"`
	Enabled               bool           `json:"enabled"`
	Currency              string         `json:"currency"`
	RatePerHour           *string        `json:"rate_per_hour"`
	IncrementMinutes      int            `json:"increment_minutes"`
	MinMinutes            int            `json:"min_minutes"`
	MaxMinutes            int            `json:"max_minutes"`
	CleaningBufferMinutes int            `json:"cleaning_buffer_minutes"`
	Capacity              int            `json:"capacity"`
	Deposit               *string        `json:"deposit"`
	OpeningHours          []OpeningRange `json:"opening_hours"`
	Exceptions            []Exception    `json:"exceptions"`
	Fees                  []feeJSON      `json:"fees"`
	CancellationPolicy    string         `json:"cancellation_policy"`
	FreeCancellationHours int            `json:"free_cancellation_hours"`
	LeadTimeHours         int            `json:"lead_time_hours"`
	HorizonDays           int            `json:"horizon_days"`
	UpdatedAt             *time.Time     `json:"updated_at"`
}

type feeJSON struct {
	Code          string  `json:"code"`
	Label         string  `json:"label"`
	Kind          string  `json:"kind"`
	Amount        string  `json:"amount"`
	EffectiveFrom string  `json:"effective_from"`
	EffectiveTo   *string `json:"effective_to"`
}

func settingsView(v SettingsView) settingsJSON {
	s := v.Settings
	out := settingsJSON{ListingID: s.ListingID, PropertyType: v.PropertyType, Eligible: v.Eligible, Configured: v.Configured,
		TimeZone: v.TimeZone, Currencies: v.Currencies, Mode: ModeEnquiry, Enabled: s.Enabled, Currency: s.Currency,
		IncrementMinutes: s.IncrementMinutes, MinMinutes: s.MinMinutes, MaxMinutes: s.MaxMinutes, CleaningBufferMinutes: s.CleaningBufferMinutes,
		Capacity: s.Capacity, Deposit: s.Deposit, OpeningHours: s.OpeningHours, Exceptions: s.Exceptions, Fees: []feeJSON{},
		CancellationPolicy: s.CancellationPolicy, FreeCancellationHours: s.FreeCancellationHours, LeadTimeHours: s.LeadTimeHours,
		HorizonDays: s.HorizonDays}
	if s.RatePerHour != "" {
		r := s.RatePerHour
		out.RatePerHour = &r
	}
	if out.OpeningHours == nil {
		out.OpeningHours = []OpeningRange{}
	}
	if out.Exceptions == nil {
		out.Exceptions = []Exception{}
	}
	for _, f := range s.Fees {
		out.Fees = append(out.Fees, feeJSON{Code: f.Code, Label: f.Label, Kind: f.Kind, Amount: f.Amount, EffectiveFrom: f.EffectiveFrom, EffectiveTo: f.EffectiveTo})
	}
	if v.Configured && !s.UpdatedAt.IsZero() {
		t := s.UpdatedAt.UTC()
		out.UpdatedAt = &t
	}
	return out
}

type rangeJSON struct {
	Start      time.Time `json:"start"`
	End        time.Time `json:"end"`
	LocalStart string    `json:"local_start"`
	LocalEnd   string    `json:"local_end"`
}

func ranges(list []Interval, loc *time.Location) []rangeJSON {
	out := make([]rangeJSON, 0, len(list))
	for _, r := range list {
		out = append(out, rangeJSON{Start: r.Start.UTC(), End: r.End.UTC(), LocalStart: localISO(r.Start, loc), LocalEnd: localISO(r.End, loc)})
	}
	return out
}

type dayJSON struct {
	Date       string      `json:"date"`
	Weekday    int         `json:"weekday"`
	Closed     bool        `json:"closed"`
	Note       *string     `json:"note"`
	OpenRanges []rangeJSON `json:"open_ranges"`
	Booked     []rangeJSON `json:"booked"`
}

type availabilityJSON struct {
	ListingID             uuid.UUID `json:"listing_id"`
	TimeZone              string    `json:"time_zone"`
	Currency              string    `json:"currency"`
	RatePerHour           string    `json:"rate_per_hour"`
	Deposit               *string   `json:"deposit"`
	DepositNote           string    `json:"deposit_note"`
	IncrementMinutes      int       `json:"increment_minutes"`
	MinMinutes            int       `json:"min_minutes"`
	MaxMinutes            int       `json:"max_minutes"`
	CleaningBufferMinutes int       `json:"cleaning_buffer_minutes"`
	Capacity              int       `json:"capacity"`
	LeadTimeHours         int       `json:"lead_time_hours"`
	HorizonDays           int       `json:"horizon_days"`
	BookableFrom          time.Time `json:"bookable_from"`
	BookableUntil         time.Time `json:"bookable_until"`
	CancellationPolicy    string    `json:"cancellation_policy"`
	FreeCancellationHours int       `json:"free_cancellation_hours"`
	Mode                  string    `json:"mode"`
	Notice                string    `json:"notice"`
	Days                  []dayJSON `json:"days"`
}

func availabilityView(v AvailabilityView) availabilityJSON {
	s := v.Settings
	out := availabilityJSON{ListingID: v.ListingID, TimeZone: v.TimeZone, Currency: s.Currency, RatePerHour: s.RatePerHour, Deposit: s.Deposit,
		DepositNote: DepositNote, IncrementMinutes: s.IncrementMinutes, MinMinutes: s.MinMinutes, MaxMinutes: s.MaxMinutes,
		CleaningBufferMinutes: s.CleaningBufferMinutes, Capacity: s.Capacity, LeadTimeHours: s.LeadTimeHours, HorizonDays: s.HorizonDays,
		BookableFrom: v.BookableFrom, BookableUntil: v.BookableTo, CancellationPolicy: s.CancellationPolicy,
		FreeCancellationHours: s.FreeCancellationHours, Mode: ModeEnquiry, Notice: Notice, Days: []dayJSON{}}
	for _, d := range v.Days {
		out.Days = append(out.Days, dayJSON{Date: d.Date, Weekday: d.Weekday, Closed: d.Closed, Note: d.Note,
			OpenRanges: ranges(d.OpenRanges, v.Location), Booked: ranges(d.Booked, v.Location)})
	}
	return out
}

type quoteJSON struct {
	QuoteID               uuid.UUID `json:"quote_id"`
	ListingID             uuid.UUID `json:"listing_id"`
	ExpiresAt             time.Time `json:"expires_at"`
	Currency              string    `json:"currency"`
	Lines                 []Line    `json:"lines"`
	Subtotal              string    `json:"subtotal"`
	Deposit               *string   `json:"deposit"`
	DepositNote           string    `json:"deposit_note"`
	TotalExcludingDeposit string    `json:"total_excluding_deposit"`
	Start                 time.Time `json:"start"`
	End                   time.Time `json:"end"`
	DurationMinutes       int       `json:"duration_minutes"`
	TimeZone              string    `json:"time_zone"`
	LocalStart            string    `json:"local_start"`
	LocalEnd              string    `json:"local_end"`
	Mode                  string    `json:"mode"`
	Notice                string    `json:"notice"`
}

func quoteView(q Quote) quoteJSON {
	loc := location(q.TimeZone)
	return quoteJSON{QuoteID: q.ID, ListingID: q.ListingID, ExpiresAt: q.ExpiresAt.UTC(), Currency: q.Pricing.Currency, Lines: q.Pricing.Lines,
		Subtotal: q.Pricing.Subtotal, Deposit: q.Pricing.Deposit, DepositNote: DepositNote, TotalExcludingDeposit: q.Pricing.TotalExcludingDeposit,
		Start: q.Start.UTC(), End: q.End.UTC(), DurationMinutes: q.DurationMinutes(), TimeZone: q.TimeZone,
		LocalStart: localISO(q.Start, loc), LocalEnd: localISO(q.End, loc), Mode: ModeEnquiry, Notice: Notice}
}

type listingRefJSON struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Reference string    `json:"reference"`
}

type pricingJSON struct {
	QuoteID               uuid.UUID `json:"quote_id"`
	Currency              string    `json:"currency"`
	Lines                 []Line    `json:"lines"`
	Subtotal              string    `json:"subtotal"`
	Deposit               *string   `json:"deposit"`
	TotalExcludingDeposit string    `json:"total_excluding_deposit"`
}

func pricingView(id uuid.UUID, p Pricing) pricingJSON {
	lines := p.Lines
	if lines == nil {
		lines = []Line{}
	}
	return pricingJSON{QuoteID: id, Currency: p.Currency, Lines: lines, Subtotal: p.Subtotal, Deposit: p.Deposit, TotalExcludingDeposit: p.TotalExcludingDeposit}
}

type policyJSON struct {
	Text                  string    `json:"text"`
	FreeCancellationHours int       `json:"free_cancellation_hours"`
	FreeUntil             time.Time `json:"free_until"`
}

type proposalJSON struct {
	ProposedBy      string      `json:"proposed_by"`
	Start           time.Time   `json:"start"`
	End             time.Time   `json:"end"`
	LocalStart      string      `json:"local_start"`
	LocalEnd        string      `json:"local_end"`
	DurationMinutes int         `json:"duration_minutes"`
	Quote           pricingJSON `json:"quote"`
}

type cancellationJSON struct {
	By     string    `json:"by"`
	Reason *string   `json:"reason"`
	At     time.Time `json:"at"`
	Free   bool      `json:"free"`
}

type customerJSON struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type eventJSON struct {
	ID         uuid.UUID  `json:"id"`
	Action     string     `json:"action"`
	FromStatus *string    `json:"from_status"`
	ToStatus   string     `json:"to_status"`
	Actor      string     `json:"actor"`
	ActorName  string     `json:"actor_name"`
	Reason     *string    `json:"reason"`
	Start      *time.Time `json:"start"`
	End        *time.Time `json:"end"`
	At         time.Time  `json:"at"`
}

type bookingJSON struct {
	ID                 uuid.UUID         `json:"id"`
	Reference          string            `json:"reference"`
	ServiceType        string            `json:"service_type"`
	Status             string            `json:"status"`
	StatusLabel        string            `json:"status_label"`
	Listing            listingRefJSON    `json:"listing"`
	Start              time.Time         `json:"start"`
	End                time.Time         `json:"end"`
	DurationMinutes    int               `json:"duration_minutes"`
	TimeZone           string            `json:"time_zone"`
	LocalStart         string            `json:"local_start"`
	LocalEnd           string            `json:"local_end"`
	Attendees          *int              `json:"attendees"`
	Message            *string           `json:"message"`
	Quote              pricingJSON       `json:"quote"`
	Mode               string            `json:"mode"`
	Notice             string            `json:"notice"`
	DepositNote        string            `json:"deposit_note"`
	CancellationPolicy policyJSON        `json:"cancellation_policy"`
	Proposal           *proposalJSON     `json:"proposal"`
	Cancellation       *cancellationJSON `json:"cancellation"`
	DeclineReason      *string           `json:"decline_reason"`
	Actions            []string          `json:"actions"`
	CreatedAt          time.Time         `json:"created_at"`
	UpdatedAt          time.Time         `json:"updated_at"`
	Customer           *customerJSON     `json:"customer,omitempty"`
	Events             []eventJSON       `json:"events,omitempty"`
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// bookingView renders a booking for party (CUSTOMER or HOST) at now; the
// host's view names the customer. events are added for a single booking.
func bookingView(b Booking, party string, now time.Time, events []Event) bookingJSON {
	loc := location(b.TimeZone)
	out := bookingJSON{ID: b.ID, Reference: b.Reference, ServiceType: ServiceType, Status: b.Status, StatusLabel: StatusLabel(b.Status),
		Listing: listingRefJSON{ID: b.ListingID, Slug: b.ListingSlug, Title: b.ListingTitle, Reference: b.ListingReference},
		Start:   b.Start.UTC(), End: b.End.UTC(), DurationMinutes: b.DurationMinutes(), TimeZone: b.TimeZone,
		LocalStart: localISO(b.Start, loc), LocalEnd: localISO(b.End, loc), Attendees: b.Attendees, Message: strOrNil(b.Message),
		Quote: pricingView(b.QuoteID, b.Pricing), Mode: ModeEnquiry, Notice: Notice, DepositNote: DepositNote,
		CancellationPolicy: policyJSON{Text: b.PolicyText, FreeCancellationHours: b.FreeCancellationHours, FreeUntil: b.FreeUntil().UTC()},
		DeclineReason:      strOrNil(b.DeclineReason), Actions: Allowed(b, party, now), CreatedAt: b.CreatedAt.UTC(), UpdatedAt: b.UpdatedAt.UTC()}
	if p := b.Proposal; p != nil {
		out.Proposal = &proposalJSON{ProposedBy: p.By, Start: p.Start.UTC(), End: p.End.UTC(), LocalStart: localISO(p.Start, loc),
			LocalEnd: localISO(p.End, loc), DurationMinutes: int(p.End.Sub(p.Start) / time.Minute), Quote: pricingView(p.QuoteID, p.Pricing)}
	}
	if b.Status == StatusCancelled && b.CancelledAt != nil {
		free := b.CancelFree != nil && *b.CancelFree
		out.Cancellation = &cancellationJSON{By: b.CancelledBy, Reason: strOrNil(b.CancelReason), At: b.CancelledAt.UTC(), Free: free}
	}
	if party == PartyHost {
		out.Customer = &customerJSON{Name: b.CustomerName, Email: b.CustomerEmail}
	}
	if events != nil {
		out.Events = make([]eventJSON, 0, len(events))
		for _, e := range events {
			out.Events = append(out.Events, eventJSON{ID: e.ID, Action: e.Action, FromStatus: strOrNil(e.FromStatus), ToStatus: e.ToStatus,
				Actor: e.ActorType, ActorName: e.ActorName, Reason: strOrNil(e.Reason), Start: e.Start, End: e.End, At: e.At.UTC()})
		}
	}
	return out
}
