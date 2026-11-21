package availability

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // every country's zone must load on any machine

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Listings is what the calendar needs from the listings module.
type Listings interface {
	// Owned returns listings of the organisations or of the direct owner, whatever their status.
	Owned(ctx context.Context, organisationIDs []uuid.UUID, sellerUserID *uuid.UUID) ([]listings.Record, error)
	// Record returns one listing whatever its status, or listings.ErrNotFound.
	Record(ctx context.Context, id uuid.UUID) (listings.Record, error)
	// PublicRecord returns a public listing, or listings.ErrNotFound / ErrUnavailable.
	PublicRecord(ctx context.Context, slug string) (listings.Record, error)
	// Present turns records into their public form (labels, places, country).
	Present(ctx context.Context, records []listings.Record) ([]listings.Listing, error)
	// SetStayRules changes min_nights and / or max_guests.
	SetStayRules(ctx context.Context, id uuid.UUID, minNights, maxGuests *int) (listings.Record, error)
}

// Zones supplies the default IANA time zone of every enabled country, by
// country code. A listing's nights are dates in its country's zone.
type Zones interface {
	CountryZones(ctx context.Context) (map[string]string, error)
}

// Service implements the availability calendar.
type Service struct {
	store    Store
	listings Listings
	zones    Zones
	now      func() time.Time

	mu        sync.Mutex
	locations map[string]*time.Location
}

// NewService builds a Service.
func NewService(store Store, source Listings, zones Zones) *Service {
	return &Service{store: store, listings: source, zones: zones, now: time.Now, locations: map[string]*time.Location{}}
}

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// location returns the zone by name, UTC when it does not load.
func (s *Service) location(zone string) *time.Location {
	s.mu.Lock()
	defer s.mu.Unlock()
	if loc, ok := s.locations[zone]; ok {
		return loc
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		loc = time.UTC
	}
	s.locations[zone] = loc
	return loc
}

// zoneOf returns the IANA zone name of a country ("UTC" when unknown).
func zoneOf(zones map[string]string, country string) string {
	if zone := zones[country]; zone != "" {
		return zone
	}
	return "UTC"
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// ListingRef names the listing a calendar belongs to.
type ListingRef struct {
	ID        uuid.UUID `json:"id"`
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Reference string    `json:"reference"`
}

// Day is one night of the lister's calendar.
type Day struct {
	Date      Date `json:"date"`
	Available bool `json:"available"`
	// Reason is BLOCKED or BOOKED for a night the lister marked, null
	// otherwise (also for a night that is unavailable only because it is in
	// the past or beyond the window).
	Reason *string `json:"reason"`
	Note   *string `json:"note"`
}

// RangeView is one stored range of unavailable nights.
type RangeView struct {
	ID         uuid.UUID `json:"id"`
	FirstNight Date      `json:"first_night"`
	LastNight  Date      `json:"last_night"`
	Reason     string    `json:"reason"`
	Note       *string   `json:"note"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Window is the booking window: today .. today+WindowDays in the listing's zone.
type Window struct {
	From Date `json:"from"`
	To   Date `json:"to"`
}

// CalendarView is the lister's calendar: private reasons and notes included.
type CalendarView struct {
	Listing               ListingRef  `json:"listing"`
	Timezone              string      `json:"timezone"`
	Window                Window      `json:"window"`
	MinNights             int         `json:"min_nights"`
	MaxGuests             *int        `json:"max_guests"`
	From                  Date        `json:"from"`
	To                    Date        `json:"to"`
	Days                  []Day       `json:"days"`
	Ranges                []RangeView `json:"ranges"`
	AvailabilityUpdatedAt *time.Time  `json:"availability_updated_at"`
}

// PublicDay is one night as the public sees it: free or not, nothing else.
type PublicDay struct {
	Date      Date `json:"date"`
	Available bool `json:"available"`
}

// PublicListing names the listing of a public calendar.
type PublicListing struct {
	Slug string `json:"slug"`
}

// PublicCalendar is the calendar as the public sees it. It has no reason,
// note, range or author.
type PublicCalendar struct {
	Listing               PublicListing `json:"listing"`
	Timezone              string        `json:"timezone"`
	MinNights             int           `json:"min_nights"`
	MaxGuests             *int          `json:"max_guests"`
	From                  Date          `json:"from"`
	To                    Date          `json:"to"`
	Days                  []PublicDay   `json:"days"`
	AvailabilityUpdatedAt *time.Time    `json:"availability_updated_at"`
}

// ListingSummary is one of the caller's listings.
type ListingSummary struct {
	ID                    uuid.UUID        `json:"id"`
	Slug                  string           `json:"slug"`
	Reference             string           `json:"reference"`
	Title                 string           `json:"title"`
	Category              listings.Coded   `json:"category"`
	PropertyType          listings.Coded   `json:"property_type"`
	Status                string           `json:"status"`
	Price                 listings.Money   `json:"price"`
	PricePeriod           *string          `json:"price_period"`
	City                  listings.Place   `json:"city"`
	Country               listings.Country `json:"country"`
	MaxGuests             *int             `json:"max_guests"`
	MinNights             *int             `json:"min_nights"`
	AvailabilityUpdatedAt *time.Time       `json:"availability_updated_at"`
	Photo                 *listings.Photo  `json:"photo"`
	// Expiry is the publication period (API 0.45.0); null for a listing that
	// was never published. Status is the effective status.
	Expiry *listings.Expiry `json:"expiry"`
	// CanEdit: the caller may edit the listing (auth.MayEditListing), and so
	// draft a lease for it. An AGENT sees colleagues' listings read-only.
	CanEdit bool `json:"can_edit"`
}

// ListingsPage is one page of the caller's listings.
type ListingsPage struct {
	Items      []ListingSummary `json:"items"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	Total      int              `json:"total"`
	TotalPages int              `json:"total_pages"`
}

func minNights(r listings.Record) int {
	if r.MinNights != nil && *r.MinNights > 0 {
		return *r.MinNights
	}
	return 1
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ---------------------------------------------------------------------------
// The lister's side
// ---------------------------------------------------------------------------

// summaries turns records into listing summaries, with the calendars' last change.
func (s *Service) summaries(ctx context.Context, records []listings.Record) ([]ListingSummary, error) {
	presented, err := s.listings.Present(ctx, records)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	states, err := s.store.States(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]ListingSummary, 0, len(records))
	now := s.clock()
	for i, r := range records {
		l := presented[i]
		item := ListingSummary{
			ID: r.ID, Slug: r.Slug, Reference: r.Reference, Title: r.Title, Category: l.Category, PropertyType: l.PropertyType,
			Status: r.EffectiveStatus(now), Price: r.Price, PricePeriod: l.PricePeriod, City: l.City, Country: l.Country,
			MaxGuests: l.MaxGuests, MinNights: l.MinNights, Expiry: listings.ExpiryOf(r, now),
		}
		if r.Category == listings.CategoryShortStay {
			item.AvailabilityUpdatedAt = states[r.ID].UpdatedAt
		}
		if len(r.Photos) > 0 {
			photo := r.Photos[0]
			item.Photo = &photo
		}
		out = append(out, item)
	}
	return out, nil
}

// MyListings returns the listings the caller may manage: those of the
// organisations they may manage listings for, and their own as an approved
// direct owner. ErrForbidden when the caller manages no listings at all.
// status matches the effective status (a PUBLISHED listing past its period
// is EXPIRED) and expiry the state of the publication period (API 0.45.0);
// "" matches everything.
func (s *Service) MyListings(ctx context.Context, p *auth.Principal, category, status, expiry string, page, size int) (ListingsPage, error) {
	if !p.ManagesListings() {
		return ListingsPage{}, ErrForbidden
	}
	var seller *uuid.UUID
	if p.IsDirectOwner() {
		id := p.UserID
		seller = &id
	}
	records, err := s.listings.Owned(ctx, p.ListingOrganisationIDs(), seller)
	if err != nil {
		return ListingsPage{}, fmt.Errorf("load listings: %w", err)
	}
	// A member pinned to a branch sees the listings without a branch and
	// those of their own branch (API 0.57.0).
	records = slices.DeleteFunc(records, func(r listings.Record) bool {
		return r.OrganisationID != nil && !p.MayAccessBranch(*r.OrganisationID, r.BranchID)
	})
	now := s.clock()
	var kept []listings.Record
	for _, r := range records {
		if expiry != "" {
			if e := listings.ExpiryOf(r, now); e == nil || e.State != expiry {
				continue
			}
		}
		if (category == "" || r.Category == category) && (status == "" || r.EffectiveStatus(now) == status) &&
			p.MayManageListing(r.OrganisationID, r.SellerUserID) {
			kept = append(kept, r)
		}
	}
	out := ListingsPage{Items: []ListingSummary{}, Page: page, PageSize: size, Total: len(kept), TotalPages: (len(kept) + size - 1) / size}
	start := min((page-1)*size, len(kept))
	end := min(start+size, len(kept))
	if out.Items, err = s.summaries(ctx, kept[start:end]); err != nil {
		return ListingsPage{}, err
	}
	for i, r := range kept[start:end] {
		out.Items[i].CanEdit = p.MayEditListing(r.OrganisationID, r.AgentUserID, r.SellerUserID)
	}
	return out, nil
}

// managed returns a listing the caller may manage. A listing that does not
// exist and one that belongs to somebody else are the same answer.
func (s *Service) managed(ctx context.Context, p *auth.Principal, id uuid.UUID) (listings.Record, error) {
	r, err := s.listings.Record(ctx, id)
	if errors.Is(err, listings.ErrNotFound) {
		return listings.Record{}, ErrNotFound
	}
	if err != nil {
		return listings.Record{}, err
	}
	if !p.MayManageListing(r.OrganisationID, r.SellerUserID) {
		return listings.Record{}, ErrNotFound
	}
	return r, nil
}

// shortStay is managed plus the category check.
func (s *Service) shortStay(ctx context.Context, p *auth.Principal, id uuid.UUID) (listings.Record, error) {
	r, err := s.managed(ctx, p, id)
	if err != nil {
		return listings.Record{}, err
	}
	if r.Category != listings.CategoryShortStay {
		return listings.Record{}, ErrNotShortStay
	}
	return r, nil
}

// zone returns the zone name and location of a listing's country.
func (s *Service) zone(ctx context.Context, r listings.Record) (string, *time.Location, error) {
	zones, err := s.zones.CountryZones(ctx)
	if err != nil {
		return "", nil, fmt.Errorf("load time zones: %w", err)
	}
	name := zoneOf(zones, r.MarketCode)
	return name, s.location(name), nil
}

// span reads from / to (YYYY-MM-DD; default today .. today+DefaultSpanDays).
func span(fromRaw, toRaw string, today Date) (from, to Date, fields map[string]string) {
	fields = map[string]string{}
	from, to = today, today+DefaultSpanDays
	var err error
	if fromRaw != "" {
		if from, err = ParseDate(fromRaw); err != nil {
			fields["from"] = "must be a date such as " + today.String()
		} else if toRaw == "" {
			to = from + DefaultSpanDays
		}
	}
	if toRaw != "" {
		if to, err = ParseDate(toRaw); err != nil {
			fields["to"] = "must be a date such as " + (today + DefaultSpanDays).String()
		}
	}
	if len(fields) > 0 {
		return from, to, fields
	}
	switch {
	case to < from:
		fields["to"] = "must not be before from"
	case int(to-from)+1 > MaxSpanNights:
		fields["to"] = fmt.Sprintf("the span is at most %d nights", MaxSpanNights)
	}
	return from, to, fields
}

func (s *Service) view(r listings.Record, zone string, today, from, to Date, state State) CalendarView {
	out := CalendarView{
		Listing:  ListingRef{ID: r.ID, Slug: r.Slug, Title: r.Title, Reference: r.Reference},
		Timezone: zone, Window: Window{From: today, To: today + WindowDays}, MinNights: minNights(r), MaxGuests: r.MaxGuests,
		From: from, To: to, Days: make([]Day, 0, int(to-from)+1), Ranges: make([]RangeView, 0, len(state.Ranges)),
		AvailabilityUpdatedAt: state.UpdatedAt,
	}
	for night := from; night <= to; night++ {
		day := Day{Date: night, Available: Available(state.Ranges, night, today)}
		if marked, ok := find(state.Ranges, night); ok {
			reason := marked.Reason
			day.Reason, day.Note = &reason, nullable(marked.Note)
		}
		out.Days = append(out.Days, day)
	}
	for _, rg := range state.Ranges {
		out.Ranges = append(out.Ranges, RangeView{ID: rg.ID, FirstNight: rg.First, LastNight: rg.Last, Reason: rg.Reason,
			Note: nullable(rg.Note), UpdatedAt: rg.UpdatedAt.UTC()})
	}
	return out
}

// Calendar returns the lister's calendar of a short stay.
func (s *Service) Calendar(ctx context.Context, p *auth.Principal, id uuid.UUID, fromRaw, toRaw string) (CalendarView, error) {
	r, err := s.shortStay(ctx, p, id)
	if err != nil {
		return CalendarView{}, err
	}
	zone, loc, err := s.zone(ctx, r)
	if err != nil {
		return CalendarView{}, err
	}
	today := Today(s.now(), loc)
	from, to, fields := span(fromRaw, toRaw, today)
	if len(fields) > 0 {
		return CalendarView{}, &ValidationError{Fields: fields}
	}
	states, err := s.store.States(ctx, []uuid.UUID{r.ID})
	if err != nil {
		return CalendarView{}, err
	}
	return s.view(r, zone, today, from, to, states[r.ID]), nil
}

// ChangeInput is one change as the client sends it.
type ChangeInput struct {
	From   string  `json:"from"`
	To     string  `json:"to"`
	Status string  `json:"status"`
	Note   *string `json:"note"`
}

// cleanNote validates a private note: plain text on one line.
func cleanNote(raw *string) (string, string) {
	if raw == nil {
		return "", ""
	}
	note := strings.TrimSpace(*raw)
	switch {
	case len([]rune(note)) > MaxNoteLength:
		return "", fmt.Sprintf("must be at most %d characters", MaxNoteLength)
	case strings.ContainsAny(note, "<>"):
		return "", "must be plain text, without HTML"
	}
	for _, c := range note {
		if c < 0x20 || c == 0x7f {
			return "", "must be plain text on one line"
		}
	}
	return note, ""
}

// parseChanges validates the changes of one PUT against today in the
// listing's zone. Field paths are changes[i].from and so on.
func parseChanges(inputs []ChangeInput, today Date) ([]Change, map[string]string) {
	fields := map[string]string{}
	if len(inputs) < 1 || len(inputs) > MaxChanges {
		fields["changes"] = fmt.Sprintf("must have 1 to %d items", MaxChanges)
		return nil, fields
	}
	out := make([]Change, 0, len(inputs))
	for i, in := range inputs {
		at := func(name string) string { return fmt.Sprintf("changes[%d].%s", i, name) }
		var c Change
		var errFrom, errTo error
		if c.From, errFrom = ParseDate(in.From); errFrom != nil {
			fields[at("from")] = "must be a date such as " + today.String()
		}
		if c.To, errTo = ParseDate(in.To); errTo != nil {
			fields[at("to")] = "must be a date such as " + today.String()
		}
		switch in.Status {
		case StatusAvailable, ReasonBlocked, ReasonBooked:
			c.Status = in.Status
		default:
			fields[at("status")] = "must be one of AVAILABLE, BLOCKED, BOOKED"
		}
		note, problem := cleanNote(in.Note)
		switch {
		case problem != "":
			fields[at("note")] = problem
		case note != "" && in.Status == StatusAvailable:
			fields[at("note")] = "is only for BLOCKED and BOOKED nights"
		default:
			c.Note = note
		}
		if errFrom == nil && errTo == nil {
			switch {
			case c.From < today:
				fields[at("from")] = "must not be in the past (today is " + today.String() + " there)"
			case c.To < c.From:
				fields[at("to")] = "must not be before from"
			case int(c.To-c.From)+1 > MaxSpanNights:
				fields[at("to")] = fmt.Sprintf("a change covers at most %d nights", MaxSpanNights)
			case c.To > today+WindowDays:
				fields[at("to")] = "must be within the booking window, which ends on " + (today + WindowDays).String()
			}
		}
		out = append(out, c)
	}
	return out, fields
}

// Change applies changes to a short stay's calendar, in order and atomically:
// either all of them are applied or none is. It returns the calendar for the
// default span.
func (s *Service) Change(ctx context.Context, p *auth.Principal, id uuid.UUID, inputs []ChangeInput) (CalendarView, error) {
	r, err := s.shortStay(ctx, p, id)
	if err != nil {
		return CalendarView{}, err
	}
	zone, loc, err := s.zone(ctx, r)
	if err != nil {
		return CalendarView{}, err
	}
	at := s.clock()
	today := Today(at, loc)
	changes, fields := parseChanges(inputs, today)
	if len(fields) > 0 {
		return CalendarView{}, &ValidationError{Fields: fields}
	}
	newID := func() uuid.UUID {
		if id, err := uuid.NewV7(); err == nil {
			return id
		}
		return uuid.New()
	}
	state, err := s.store.Update(ctx, r.ID, at, func(current State) ([]Range, []Audit, error) {
		ranges := current.Ranges
		audits := make([]Audit, 0, len(changes))
		for _, c := range changes {
			ranges = Apply(ranges, c, p.UserID, at, newID)
			audits = append(audits, Audit{ID: newID(), ListingID: r.ID, ActorID: p.UserID, From: c.From, To: c.To, Status: c.Status, Note: c.Note, At: at})
		}
		return ranges, audits, nil
	})
	if err != nil {
		return CalendarView{}, fmt.Errorf("update availability: %w", err)
	}
	return s.view(r, zone, today, today, today+DefaultSpanDays, state), nil
}

// SetStayRules changes the shortest stay and / or the number of guests of a
// short stay the caller may manage, and returns its summary.
func (s *Service) SetStayRules(ctx context.Context, p *auth.Principal, id uuid.UUID, minNightsIn, maxGuestsIn *int) (ListingSummary, error) {
	r, err := s.shortStay(ctx, p, id)
	if err != nil {
		return ListingSummary{}, err
	}
	fields := map[string]string{}
	if minNightsIn == nil && maxGuestsIn == nil {
		fields["min_nights"] = "send min_nights, max_guests or both"
	}
	if minNightsIn != nil && (*minNightsIn < 1 || *minNightsIn > MaxMinNights) {
		fields["min_nights"] = fmt.Sprintf("must be a whole number from 1 to %d", MaxMinNights)
	}
	if maxGuestsIn != nil && (*maxGuestsIn < 1 || *maxGuestsIn > MaxGuests) {
		fields["max_guests"] = fmt.Sprintf("must be a whole number from 1 to %d", MaxGuests)
	}
	if len(fields) > 0 {
		return ListingSummary{}, &ValidationError{Fields: fields}
	}
	updated, err := s.listings.SetStayRules(ctx, r.ID, minNightsIn, maxGuestsIn)
	if errors.Is(err, listings.ErrNotFound) {
		return ListingSummary{}, ErrNotFound
	}
	if err != nil {
		return ListingSummary{}, fmt.Errorf("set stay rules: %w", err)
	}
	out, err := s.summaries(ctx, []listings.Record{updated})
	if err != nil {
		return ListingSummary{}, err
	}
	return out[0], nil
}

// ---------------------------------------------------------------------------
// The public side
// ---------------------------------------------------------------------------

// Public returns the public calendar of a public short stay: available or
// not per night, and nothing about why. The listing errors are the listing's
// own (listings.ErrNotFound, listings.ErrUnavailable).
func (s *Service) Public(ctx context.Context, slug, fromRaw, toRaw string) (PublicCalendar, error) {
	r, err := s.listings.PublicRecord(ctx, slug)
	if err != nil {
		return PublicCalendar{}, err
	}
	if r.Category != listings.CategoryShortStay {
		return PublicCalendar{}, ErrNotShortStay
	}
	zone, loc, err := s.zone(ctx, r)
	if err != nil {
		return PublicCalendar{}, err
	}
	today := Today(s.now(), loc)
	from, to, fields := span(fromRaw, toRaw, today)
	if len(fields) > 0 {
		return PublicCalendar{}, &ValidationError{Fields: fields}
	}
	states, err := s.store.States(ctx, []uuid.UUID{r.ID})
	if err != nil {
		return PublicCalendar{}, err
	}
	state := states[r.ID]
	out := PublicCalendar{
		Listing: PublicListing{Slug: r.Slug}, Timezone: zone, MinNights: minNights(r), MaxGuests: r.MaxGuests,
		From: from, To: to, Days: make([]PublicDay, 0, int(to-from)+1), AvailabilityUpdatedAt: state.UpdatedAt,
	}
	for night := from; night <= to; night++ {
		out.Days = append(out.Days, PublicDay{Date: night, Available: Available(state.Ranges, night, today)})
	}
	return out, nil
}

// Summaries implements listings.Calendar: updated_at and next_available of
// each short stay among records.
func (s *Service) Summaries(ctx context.Context, records []listings.Record, now time.Time) (map[uuid.UUID]listings.AvailabilitySummary, error) {
	var ids []uuid.UUID
	for _, r := range records {
		if r.Category == listings.CategoryShortStay {
			ids = append(ids, r.ID)
		}
	}
	out := map[uuid.UUID]listings.AvailabilitySummary{}
	if len(ids) == 0 {
		return out, nil
	}
	states, err := s.store.States(ctx, ids)
	if err != nil {
		return nil, err
	}
	zones, err := s.zones.CountryZones(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range records {
		if r.Category != listings.CategoryShortStay {
			continue
		}
		state := states[r.ID]
		summary := listings.AvailabilitySummary{UpdatedAt: state.UpdatedAt}
		if night, ok := NextAvailable(state.Ranges, Today(now, s.location(zoneOf(zones, r.MarketCode)))); ok {
			text := night.String()
			summary.NextAvailable = &text
		}
		out[r.ID] = summary
	}
	return out, nil
}

// stayDates parses check_in and check_out and applies the rules that do not
// depend on a listing: order and length.
func stayDates(checkInRaw, checkOutRaw string) (checkIn, checkOut Date, fields map[string]string) {
	fields = map[string]string{}
	var err error
	if checkIn, err = ParseDate(checkInRaw); err != nil {
		fields["check_in"] = "must be a date such as 2026-12-24"
	}
	if checkOut, err = ParseDate(checkOutRaw); err != nil {
		fields["check_out"] = "must be a date such as 2026-12-27"
	}
	if len(fields) > 0 {
		return checkIn, checkOut, fields
	}
	switch {
	case checkOut <= checkIn:
		fields["check_out"] = "must be after check_in"
	case Nights(checkIn, checkOut) > MaxStayNights:
		fields["check_out"] = fmt.Sprintf("a stay is at most %d nights", MaxStayNights)
	}
	return checkIn, checkOut, fields
}

// Stay implements listings.Calendar. The stay must lie inside the booking
// window somewhere in the world; the countries where it does not (today
// differs by time zone) are excluded, and so is every listing with an
// unavailable night in it.
func (s *Service) Stay(ctx context.Context, checkInRaw, checkOutRaw string, now time.Time) (listings.Stay, map[string]string, error) {
	checkIn, checkOut, fields := stayDates(checkInRaw, checkOutRaw)
	if len(fields) > 0 {
		return listings.Stay{}, fields, nil
	}
	zones, err := s.zones.CountryZones(ctx)
	if err != nil {
		return listings.Stay{}, nil, err
	}
	lastNight := checkOut - 1
	stay := listings.Stay{Nights: Nights(checkIn, checkOut)}
	earliest, latest := Today(now, time.UTC), Today(now, time.UTC)
	for country, zone := range zones {
		today := Today(now, s.location(zone))
		earliest, latest = min(earliest, today), max(latest, today)
		if checkIn < today || lastNight > today+WindowDays {
			stay.ExcludeCountries = append(stay.ExcludeCountries, country)
		}
	}
	switch {
	case checkIn < earliest:
		fields["check_in"] = "must not be in the past"
	case lastNight > latest+WindowDays:
		fields["check_out"] = fmt.Sprintf("must be within the booking window (%d days from today)", WindowDays)
	}
	if len(fields) > 0 {
		return listings.Stay{}, fields, nil
	}
	sort.Strings(stay.ExcludeCountries)
	if stay.ExcludeIDs, err = s.store.Overlapping(ctx, checkIn, lastNight); err != nil {
		return listings.Stay{}, nil, err
	}
	return stay, nil, nil
}

// StayRequest is a stay somebody asks for, checked against one listing.
type StayRequest struct {
	CheckIn  Date
	CheckOut Date
	Nights   int
	Guests   int
}

// CheckStay validates a request to stay at a listing: the listing is a short
// stay, the dates are inside its booking window, the stay is at least its
// min_nights, the guests fit, and every night is available right now. Rule
// violations are a *ValidationError (keyed check_in, check_out, guests or
// request_type); nights that are taken are an *UnavailableError naming the
// first one. It changes nothing: asking to stay does not block the dates.
func (s *Service) CheckStay(ctx context.Context, listingID uuid.UUID, checkInRaw, checkOutRaw string, guests int) (StayRequest, error) {
	r, err := s.listings.Record(ctx, listingID)
	if err != nil {
		return StayRequest{}, err
	}
	if r.Category != listings.CategoryShortStay {
		return StayRequest{}, &ValidationError{Fields: map[string]string{"request_type": "STAY_REQUEST is only for short-stay listings"}}
	}
	checkIn, checkOut, fields := stayDates(checkInRaw, checkOutRaw)
	if len(fields) > 0 {
		return StayRequest{}, &ValidationError{Fields: fields}
	}
	_, loc, err := s.zone(ctx, r)
	if err != nil {
		return StayRequest{}, err
	}
	today, nights := Today(s.now(), loc), Nights(checkIn, checkOut)
	switch {
	case checkIn < today:
		fields["check_in"] = "must not be in the past (today is " + today.String() + " there)"
	case checkOut-1 > today+WindowDays:
		fields["check_out"] = "must be within the booking window, which ends on " + (today + WindowDays).String()
	case nights < minNights(r):
		fields["check_out"] = fmt.Sprintf("the shortest stay here is %d nights", minNights(r))
	}
	if r.MaxGuests != nil && guests > *r.MaxGuests {
		fields["guests"] = fmt.Sprintf("this place sleeps at most %d guests", *r.MaxGuests)
	}
	if len(fields) > 0 {
		return StayRequest{}, &ValidationError{Fields: fields}
	}
	states, err := s.store.States(ctx, []uuid.UUID{r.ID})
	if err != nil {
		return StayRequest{}, err
	}
	if night, taken := FirstUnavailable(states[r.ID].Ranges, checkIn, checkOut-1, today); taken {
		return StayRequest{}, &UnavailableError{Night: night}
	}
	return StayRequest{CheckIn: checkIn, CheckOut: checkOut, Nights: nights, Guests: guests}, nil
}
