package appointments

// Availability (API 0.51.0, FR-020..022): a professional's bookable hours,
// the free consultation services they offer, and the slots a customer may
// choose from. Wall-clock rules (a local date's hours, exceptions, DST) are
// the venues' (venues.Settings.Day), so both behave the same.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/venues"
)

// Limits of the settings.
const (
	DefaultTimezone      = "Africa/Harare"
	MaxWorkingRanges     = 21
	MaxExceptions        = 100
	MaxActiveServices    = 20
	MaxSlotDays          = 31
	maxServiceNameRunes  = 80
	maxServiceNoteRunes  = 500
	maxServicePlaceRunes = 200
	dateLayout           = "2006-01-02"
)

// Consultation channels.
const (
	ChannelInPerson = "IN_PERSON"
	ChannelVideo    = "VIDEO"
	ChannelPhone    = "PHONE"
)

// Channels returns every consultation channel.
func Channels() []string { return []string{ChannelInPerson, ChannelVideo, ChannelPhone} }

// Availability errors.
var (
	ErrServiceNotFound = errors.New("consultation service not found")
	ErrTooManyServices = errors.New("too many active consultation services")
)

// Settings are one professional's bookable hours.
type Settings struct {
	UserID        uuid.UUID
	Enabled       bool
	Timezone      string
	WorkingHours  []venues.OpeningRange
	Exceptions    []venues.Exception
	SlotMinutes   int
	BufferMinutes int
	LeadTimeHours int
	HorizonDays   int
	MaxPerDay     int
	UpdatedAt     *time.Time
}

// DefaultSettings are what a professional who never saved any has: off.
func DefaultSettings(userID uuid.UUID) Settings {
	return Settings{UserID: userID, Timezone: DefaultTimezone, WorkingHours: []venues.OpeningRange{}, Exceptions: []venues.Exception{},
		SlotMinutes: 30, LeadTimeHours: 24, HorizonDays: 30}
}

// ConsultationService is a free consultation a professional offers.
type ConsultationService struct {
	ID              uuid.UUID
	UserID          uuid.UUID
	Name            string
	DurationMinutes int
	Channel         string
	Location        string
	Notes           string
	Active          bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Busy is a span of the professional's time that is taken.
type Busy struct{ Start, End time.Time }

// AvailabilityStore persists settings and services, and answers which of a
// professional's time is taken.
type AvailabilityStore interface {
	// Settings returns the user's settings, or ok=false when never saved.
	Settings(ctx context.Context, userID uuid.UUID) (s Settings, ok bool, err error)
	SaveSettings(ctx context.Context, s Settings) error
	// Services returns the user's services, active first then newest first.
	Services(ctx context.Context, userID uuid.UUID) ([]ConsultationService, error)
	// Service returns one service, or ErrServiceNotFound.
	Service(ctx context.Context, id uuid.UUID) (ConsultationService, error)
	// SaveService inserts or updates a service. On insert of an active
	// service it returns ErrTooManyServices when the user already has
	// MaxActiveServices active ones (checked under a lock).
	SaveService(ctx context.Context, c ConsultationService, insert bool) error
	// BusyFor returns the open appointments (REQUESTED, CONFIRMED,
	// RESCHEDULE_REQUESTED) assigned to the user that overlap [from, to):
	// their times, and a pending proposed time too, except the appointment
	// `except` (uuid.Nil for none).
	BusyFor(ctx context.Context, userID uuid.UUID, from, to time.Time, except uuid.UUID) ([]Busy, error)
}

// ---------------------------------------------------------------------------
// Settings input
// ---------------------------------------------------------------------------

// SettingsInput is the JSON body of PUT /v1/me/appointment-settings.
type SettingsInput struct {
	Enabled       bool                  `json:"enabled"`
	Timezone      string                `json:"timezone"`
	WorkingHours  []venues.OpeningRange `json:"working_hours"`
	Exceptions    []venues.Exception    `json:"exceptions"`
	SlotMinutes   *int                  `json:"slot_minutes"`
	BufferMinutes *int                  `json:"buffer_minutes"`
	LeadTimeHours *int                  `json:"lead_time_hours"`
	HorizonDays   *int                  `json:"horizon_days"`
	MaxPerDay     *int                  `json:"max_per_day"`
}

// clock reads "HH:MM" (00:00 to 24:00) as minutes after midnight.
func clock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' {
		return 0, false
	}
	h, err1 := strconv.Atoi(s[:2])
	m, err2 := strconv.Atoi(s[3:])
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 || (h == 24 && m != 0) {
		return 0, false
	}
	return h*60 + m, true
}

// apply validates the input into settings for userID, at now.
func (in SettingsInput) apply(userID uuid.UUID, now time.Time) (Settings, error) {
	f := map[string]string{}
	s := DefaultSettings(userID)
	s.Enabled = in.Enabled

	zone := strings.TrimSpace(in.Timezone)
	if zone == "" {
		f["timezone"] = "is required"
	} else if _, err := time.LoadLocation(zone); err != nil || zone == "Local" {
		f["timezone"] = "must be an IANA time zone such as Africa/Harare"
	} else {
		s.Timezone = zone
	}

	if len(in.WorkingHours) > MaxWorkingRanges {
		f["working_hours"] = fmt.Sprintf("at most %d ranges", MaxWorkingRanges)
	} else {
		type span struct{ a, b int }
		byDay := map[int][]span{}
		for i, r := range in.WorkingHours {
			path := fmt.Sprintf("working_hours[%d]", i)
			a, ok1 := clock(r.Opens)
			b, ok2 := clock(r.Closes)
			switch {
			case r.Weekday < 1 || r.Weekday > 7:
				f[path+".weekday"] = "must be 1 (Monday) to 7 (Sunday)"
			case !ok1:
				f[path+".opens"] = "must be a time HH:MM"
			case !ok2:
				f[path+".closes"] = "must be a time HH:MM (up to 24:00)"
			case a >= b:
				f[path+".closes"] = "must be after opens"
			default:
				for _, o := range byDay[r.Weekday] {
					if a < o.b && o.a < b {
						f[path] = "overlaps another range on the same day"
					}
				}
				byDay[r.Weekday] = append(byDay[r.Weekday], span{a, b})
			}
		}
		s.WorkingHours = append([]venues.OpeningRange{}, in.WorkingHours...)
		sort.SliceStable(s.WorkingHours, func(i, j int) bool {
			if s.WorkingHours[i].Weekday != s.WorkingHours[j].Weekday {
				return s.WorkingHours[i].Weekday < s.WorkingHours[j].Weekday
			}
			return s.WorkingHours[i].Opens < s.WorkingHours[j].Opens
		})
	}

	if len(in.Exceptions) > MaxExceptions {
		f["exceptions"] = fmt.Sprintf("at most %d dates", MaxExceptions)
	} else {
		seen := map[string]bool{}
		today := now.Add(-48 * time.Hour).Format(dateLayout)
		last := now.AddDate(2, 0, 0).Format(dateLayout)
		s.Exceptions = []venues.Exception{}
		for i, e := range in.Exceptions {
			path := fmt.Sprintf("exceptions[%d]", i)
			d, err := time.Parse(dateLayout, e.Date)
			switch {
			case err != nil || d.Format(dateLayout) != e.Date:
				f[path+".date"] = "must be a date YYYY-MM-DD"
				continue
			case e.Date < today || e.Date > last:
				f[path+".date"] = "must be within the next two years"
				continue
			case seen[e.Date]:
				f[path+".date"] = "is listed twice"
				continue
			}
			seen[e.Date] = true
			out := venues.Exception{Date: e.Date, Closed: e.Closed}
			if !e.Closed {
				var a, b int
				ok1, ok2 := false, false
				if e.Opens != nil {
					a, ok1 = clock(*e.Opens)
				}
				if e.Closes != nil {
					b, ok2 = clock(*e.Closes)
				}
				switch {
				case !ok1:
					f[path+".opens"] = "is required (HH:MM) unless the day is closed"
				case !ok2:
					f[path+".closes"] = "is required (HH:MM) unless the day is closed"
				case a >= b:
					f[path+".closes"] = "must be after opens"
				default:
					out.Opens, out.Closes = e.Opens, e.Closes
				}
			}
			if e.Note != nil {
				note, msg := plainText(*e.Note, 120, false)
				if msg != "" {
					f[path+".note"] = msg
				} else if note != "" {
					out.Note = &note
				}
			}
			s.Exceptions = append(s.Exceptions, out)
		}
		sort.SliceStable(s.Exceptions, func(i, j int) bool { return s.Exceptions[i].Date < s.Exceptions[j].Date })
	}

	ints := func(path string, v *int, dst *int, lo, hi, step int) {
		if v == nil {
			return
		}
		switch {
		case *v < lo || *v > hi:
			f[path] = fmt.Sprintf("must be between %d and %d", lo, hi)
		case step > 1 && *v%step != 0:
			f[path] = fmt.Sprintf("must be a multiple of %d", step)
		default:
			*dst = *v
		}
	}
	ints("slot_minutes", in.SlotMinutes, &s.SlotMinutes, 15, 240, 15)
	ints("buffer_minutes", in.BufferMinutes, &s.BufferMinutes, 0, 120, 1)
	ints("lead_time_hours", in.LeadTimeHours, &s.LeadTimeHours, 0, 336, 1)
	ints("horizon_days", in.HorizonDays, &s.HorizonDays, 1, 180, 1)
	ints("max_per_day", in.MaxPerDay, &s.MaxPerDay, 0, 50, 1)

	if s.Enabled && len(in.WorkingHours) == 0 && f["working_hours"] == "" {
		f["working_hours"] = "add at least one range before switching availability on"
	}
	if len(f) > 0 {
		return Settings{}, &ValidationError{Fields: f}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// Service input
// ---------------------------------------------------------------------------

// ServiceInput is the JSON body of POST and PATCH /v1/me/consultation-services.
type ServiceInput struct {
	Name            *string `json:"name"`
	DurationMinutes *int    `json:"duration_minutes"`
	Channel         *string `json:"channel"`
	Location        *string `json:"location"`
	Notes           *string `json:"notes"`
	Active          *bool   `json:"active"`
	// Price is refused: paid consultations are not enabled (FR-022).
	Price *string `json:"price"`
}

func (in ServiceInput) apply(c *ConsultationService, create bool) error {
	f := map[string]string{}
	if in.Price != nil {
		f["price"] = "paid consultations are not available yet; services are free"
	}
	if in.Name != nil || create {
		raw := ""
		if in.Name != nil {
			raw = *in.Name
		}
		name, msg := plainText(raw, maxServiceNameRunes, false)
		switch {
		case msg != "":
			f["name"] = msg
		case name == "":
			f["name"] = "is required"
		default:
			c.Name = name
		}
	}
	if in.DurationMinutes != nil || create {
		d := 0
		if in.DurationMinutes != nil {
			d = *in.DurationMinutes
		}
		if msg := checkDuration(d); msg != "" {
			f["duration_minutes"] = msg
		} else {
			c.DurationMinutes = d
		}
	}
	if in.Channel != nil || create {
		ch := ""
		if in.Channel != nil {
			ch = strings.TrimSpace(*in.Channel)
		}
		if !slices.Contains(Channels(), ch) {
			f["channel"] = "must be one of " + strings.Join(Channels(), ", ")
		} else {
			c.Channel = ch
		}
	}
	if in.Location != nil {
		v, msg := plainText(*in.Location, maxServicePlaceRunes, false)
		if msg != "" {
			f["location"] = msg
		} else {
			c.Location = v
		}
	}
	if in.Notes != nil {
		v, msg := plainText(*in.Notes, maxServiceNoteRunes, true)
		if msg != "" {
			f["notes"] = msg
		} else {
			c.Notes = v
		}
	}
	if in.Active != nil {
		c.Active = *in.Active
	}
	if len(f) > 0 {
		return &ValidationError{Fields: f}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Slots (pure)
// ---------------------------------------------------------------------------

// Slot is one bookable start.
type Slot struct{ Start, End time.Time }

// SlotDay is a local date's slots.
type SlotDay struct {
	Date  string
	Slots []Slot
}

// window is when a customer may book at now: after the lead time, before
// the horizon.
func (s Settings) window(now time.Time) (from, until time.Time) {
	return now.Add(time.Duration(s.LeadTimeHours) * time.Hour), now.Add(time.Duration(s.HorizonDays) * 24 * time.Hour)
}

// ComputeSlots returns the slots of `days` local dates from `from` (a local
// date in the settings' zone), each `length` minutes long, stepped by the
// slot length inside the working ranges, skipping busy time (widened by the
// buffer), times outside the booking window, and full days (max_per_day,
// counting the busy appointments that start on that date).
func ComputeSlots(s Settings, from string, days, length int, busy []Busy, now time.Time) []SlotDay {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		loc = time.UTC
	}
	start, err := time.ParseInLocation(dateLayout, from, loc)
	if err != nil {
		return []SlotDay{}
	}
	step := time.Duration(max(s.SlotMinutes, 15)) * time.Minute
	size := time.Duration(length) * time.Minute
	buffer := time.Duration(s.BufferMinutes) * time.Minute
	earliest, latest := s.window(now)
	cal := venues.Settings{OpeningHours: s.WorkingHours, Exceptions: s.Exceptions}

	out := make([]SlotDay, 0, days)
	for i := range days {
		date := start.AddDate(0, 0, i).Format(dateLayout)
		day := SlotDay{Date: date, Slots: []Slot{}}
		taken := 0
		if s.MaxPerDay > 0 {
			for _, b := range busy {
				if b.Start.In(loc).Format(dateLayout) == date {
					taken++
				}
			}
		}
		if s.MaxPerDay == 0 || taken < s.MaxPerDay {
			for _, r := range cal.Day(date, loc).Ranges {
				for t := r.Start; !t.Add(size).After(r.End); t = t.Add(step) {
					if t.Before(earliest) || t.After(latest) {
						continue
					}
					slot := Slot{Start: t.UTC(), End: t.Add(size).UTC()}
					free := true
					for _, b := range busy {
						if slot.Start.Before(b.End.Add(buffer)) && b.Start.Add(-buffer).Before(slot.End) {
							free = false
							break
						}
					}
					if free {
						day.Slots = append(day.Slots, slot)
					}
				}
			}
		}
		out = append(out, day)
	}
	return out
}

// offers reports whether start is the start of one of the slots.
func offers(days []SlotDay, start time.Time) bool {
	for _, d := range days {
		for _, s := range d.Slots {
			if s.Start.Equal(start) {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Service methods
// ---------------------------------------------------------------------------

func (s *Service) availability() (AvailabilityStore, bool) {
	return s.d.Availability, s.d.Availability != nil
}

// MySettings returns the caller's settings (defaults when never saved).
func (s *Service) MySettings(ctx context.Context, userID uuid.UUID) (Settings, error) {
	store, ok := s.availability()
	if !ok {
		return DefaultSettings(userID), nil
	}
	got, found, err := store.Settings(ctx, userID)
	if err != nil {
		return Settings{}, err
	}
	if !found {
		return DefaultSettings(userID), nil
	}
	return got, nil
}

// SaveSettings validates and stores the caller's settings.
func (s *Service) SaveSettings(ctx context.Context, userID uuid.UUID, in SettingsInput) (Settings, error) {
	store, ok := s.availability()
	if !ok {
		return Settings{}, errors.New("availability is not configured")
	}
	now := s.now()
	next, err := in.apply(userID, now)
	if err != nil {
		return Settings{}, err
	}
	next.UpdatedAt = &now
	if err := store.SaveSettings(ctx, next); err != nil {
		return Settings{}, fmt.Errorf("save appointment settings: %w", err)
	}
	return next, nil
}

// MyServices returns the caller's consultation services.
func (s *Service) MyServices(ctx context.Context, userID uuid.UUID) ([]ConsultationService, error) {
	store, ok := s.availability()
	if !ok {
		return []ConsultationService{}, nil
	}
	return store.Services(ctx, userID)
}

// CreateService adds a consultation service for the caller.
func (s *Service) CreateService(ctx context.Context, userID uuid.UUID, in ServiceInput) (ConsultationService, error) {
	store, ok := s.availability()
	if !ok {
		return ConsultationService{}, errors.New("availability is not configured")
	}
	now := s.now()
	c := ConsultationService{UserID: userID, Active: true, CreatedAt: now, UpdatedAt: now}
	if err := in.apply(&c, true); err != nil {
		return ConsultationService{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return ConsultationService{}, err
	}
	c.ID = id
	if err := store.SaveService(ctx, c, true); err != nil {
		return ConsultationService{}, err
	}
	return c, nil
}

// UpdateService changes one of the caller's services (ErrServiceNotFound
// for another user's).
func (s *Service) UpdateService(ctx context.Context, userID, id uuid.UUID, in ServiceInput) (ConsultationService, error) {
	store, ok := s.availability()
	if !ok {
		return ConsultationService{}, ErrServiceNotFound
	}
	c, err := store.Service(ctx, id)
	if err != nil {
		return ConsultationService{}, err
	}
	if c.UserID != userID {
		return ConsultationService{}, ErrServiceNotFound
	}
	wasActive := c.Active
	if err := in.apply(&c, false); err != nil {
		return ConsultationService{}, err
	}
	if c.Active && !wasActive {
		all, err := store.Services(ctx, userID)
		if err != nil {
			return ConsultationService{}, err
		}
		active := 0
		for _, x := range all {
			if x.Active {
				active++
			}
		}
		if active >= MaxActiveServices {
			return ConsultationService{}, ErrTooManyServices
		}
	}
	c.UpdatedAt = s.now()
	if err := store.SaveService(ctx, c, false); err != nil {
		return ConsultationService{}, err
	}
	return c, nil
}

// ArchiveService switches one of the caller's services off.
func (s *Service) ArchiveService(ctx context.Context, userID, id uuid.UUID) error {
	off := false
	_, err := s.UpdateService(ctx, userID, id, ServiceInput{Active: &off})
	return err
}

// SlotQuery is GET /v1/appointments/slots.
type SlotQuery struct {
	Type      string
	ListingID string
	Kind      string
	Slug      string
	ServiceID string
	From      string
	Days      int
}

// SlotAnswer is what the slots endpoint returns.
type SlotAnswer struct {
	Configured  bool
	Timezone    string
	SlotMinutes int
	Length      int
	Services    []ConsultationService
	Days        []SlotDay
}

// professionalOf returns the single user who would confirm an appointment
// for the target (nil when it is an organisation with nobody assigned), and
// the market's zone. It applies the same checks as Create.
func (s *Service) professionalOf(ctx context.Context, listingID *uuid.UUID, kind, slug string, now time.Time) (*uuid.UUID, uuid.UUID, error) {
	if listingID != nil {
		listing, err := s.d.Listings.GetListing(ctx, *listingID)
		if errors.Is(err, propertyrequests.ErrListingNotFound) {
			return nil, uuid.Nil, ErrListingNotFound
		}
		if err != nil {
			return nil, uuid.Nil, fmt.Errorf("load listing: %w", err)
		}
		if listing.Status != propertyrequests.ListingStatusPublished || (listing.ExpiresAt != nil && !listing.ExpiresAt.After(now)) {
			return nil, uuid.Nil, ErrListingUnavailable
		}
		r := propertyrequests.ResolveRecipient(listing)
		if r == nil {
			return nil, uuid.Nil, ErrListingUnavailable
		}
		return r.UserID, listing.MarketID, nil
	}
	profile, err := s.d.Profiles.GetProfile(ctx, kind, slug)
	if errors.Is(err, businessenquiries.ErrRecipientNotFound) {
		return nil, uuid.Nil, ErrRecipientNotFound
	}
	if err != nil {
		return nil, uuid.Nil, fmt.Errorf("load profile: %w", err)
	}
	r := businessenquiries.ResolveRecipient(profile)
	if profile.Status != businessenquiries.ProfileActive || r == nil {
		return nil, uuid.Nil, ErrRecipientUnavailable
	}
	return r.UserID, profile.MarketID, nil
}

// calendar returns the professional's settings when availability is ON.
func (s *Service) calendar(ctx context.Context, user *uuid.UUID) (Settings, bool, error) {
	store, ok := s.availability()
	if !ok || user == nil {
		return Settings{}, false, nil
	}
	got, found, err := store.Settings(ctx, *user)
	if err != nil {
		return Settings{}, false, fmt.Errorf("load appointment settings: %w", err)
	}
	return got, found && got.Enabled, nil
}

// pickService returns the active service of the professional named by raw
// ("" for none), or a field message.
func (s *Service) pickService(ctx context.Context, user *uuid.UUID, raw string) (*ConsultationService, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, "", nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, "must be a UUID", nil
	}
	store, ok := s.availability()
	if !ok || user == nil {
		return nil, "is not offered here", nil
	}
	c, err := store.Service(ctx, id)
	if errors.Is(err, ErrServiceNotFound) || (err == nil && (c.UserID != *user || !c.Active)) {
		return nil, "is not offered here", nil
	}
	if err != nil {
		return nil, "", err
	}
	return &c, "", nil
}

// Slots answers GET /v1/appointments/slots.
func (s *Service) Slots(ctx context.Context, q SlotQuery) (SlotAnswer, error) {
	now := s.now()
	f := map[string]string{}
	if q.Type != TypeViewing && q.Type != TypeConsultation {
		f["type"] = "must be VIEWING or CONSULTATION"
	}
	var listingID *uuid.UUID
	hasListing := strings.TrimSpace(q.ListingID) != ""
	switch {
	case hasListing && q.Slug != "":
		f["recipient_slug"] = "send either listing_id or recipient_kind and recipient_slug"
	case hasListing:
		id, err := uuid.Parse(strings.TrimSpace(q.ListingID))
		if err != nil {
			f["listing_id"] = "must be a UUID"
		}
		listingID = &id
	case q.Kind == "" || q.Slug == "":
		f["listing_id"] = "either listing_id or recipient_kind and recipient_slug is required"
	case q.Type == TypeViewing:
		f["listing_id"] = "is required for a VIEWING"
	}
	days := q.Days
	if days == 0 {
		days = 14
	}
	if days < 1 || days > MaxSlotDays {
		f["days"] = fmt.Sprintf("must be between 1 and %d", MaxSlotDays)
	}
	if q.From != "" {
		if d, err := time.Parse(dateLayout, q.From); err != nil || d.Format(dateLayout) != q.From {
			f["from"] = "must be a date YYYY-MM-DD"
		}
	}
	if len(f) > 0 {
		return SlotAnswer{}, &ValidationError{Fields: f}
	}
	user, marketID, err := s.professionalOf(ctx, listingID, q.Kind, q.Slug, now)
	if err != nil {
		return SlotAnswer{}, err
	}
	zone, err := s.timezone(ctx, marketID)
	if err != nil {
		return SlotAnswer{}, err
	}
	ans := SlotAnswer{Timezone: zone, Services: []ConsultationService{}, Days: []SlotDay{}}
	settings, on, err := s.calendar(ctx, user)
	if err != nil || !on {
		return ans, err
	}
	ans.Configured, ans.Timezone, ans.SlotMinutes, ans.Length = true, settings.Timezone, settings.SlotMinutes, settings.SlotMinutes
	if q.Type == TypeConsultation {
		all, err := s.d.Availability.Services(ctx, *user)
		if err != nil {
			return SlotAnswer{}, err
		}
		for _, c := range all {
			if c.Active {
				ans.Services = append(ans.Services, c)
			}
		}
		if q.ServiceID != "" {
			c, msg, err := s.pickService(ctx, user, q.ServiceID)
			if err != nil {
				return SlotAnswer{}, err
			}
			if msg != "" {
				return SlotAnswer{}, &ValidationError{Fields: map[string]string{"service_id": msg}}
			}
			ans.Length = c.DurationMinutes
		}
	}
	from := q.From
	loc, _ := time.LoadLocation(settings.Timezone)
	if from == "" {
		from = now.In(loc).Format(dateLayout)
	}
	ans.Days, err = s.slotsFor(ctx, *user, settings, from, days, ans.Length, uuid.Nil, now)
	return ans, err
}

func (s *Service) slotsFor(ctx context.Context, user uuid.UUID, st Settings, from string, days, length int, except uuid.UUID, now time.Time) ([]SlotDay, error) {
	loc, err := time.LoadLocation(st.Timezone)
	if err != nil {
		loc = time.UTC
	}
	start, err := time.ParseInLocation(dateLayout, from, loc)
	if err != nil {
		return []SlotDay{}, nil
	}
	busy, err := s.d.Availability.BusyFor(ctx, user, start.Add(-24*time.Hour), start.AddDate(0, 0, days+1), except)
	if err != nil {
		return nil, fmt.Errorf("load busy time: %w", err)
	}
	return ComputeSlots(st, from, days, length, busy, now), nil
}

// checkOffered is the FR-020 rule: when the professional's availability is
// on, start must be the start of an offered slot of the given length. It
// returns a field message ("" when fine) and the length to book (0 when the
// calendar is off, so the caller keeps its own).
func (s *Service) checkOffered(ctx context.Context, user *uuid.UUID, start time.Time, length int, except uuid.UUID) (string, int, error) {
	settings, on, err := s.calendar(ctx, user)
	if err != nil || !on {
		return "", 0, err
	}
	if length == 0 {
		length = settings.SlotMinutes
	}
	loc, err := time.LoadLocation(settings.Timezone)
	if err != nil {
		loc = time.UTC
	}
	day := start.In(loc).Format(dateLayout)
	days, err := s.slotsFor(ctx, *user, settings, day, 1, length, except, s.now())
	if err != nil {
		return "", 0, err
	}
	if !offers(days, start) {
		return "is not one of the offered times; choose another", length, nil
	}
	return "", length, nil
}

// bookable applies the availability rules to a new appointment: the chosen
// consultation service (its length, name and channel), and, when the
// professional's calendar is on, an offered slot (a 400 on field): the
// slot length then replaces a duration the customer chose.
func (s *Service) bookable(ctx context.Context, a *Appointment, rawService string, field string) error {
	if a.Type != TypeConsultation && strings.TrimSpace(rawService) != "" {
		return &ValidationError{Fields: map[string]string{"service_id": "is only for consultations"}}
	}
	svc, msg, err := s.pickService(ctx, a.AssignedUserID, rawService)
	if err != nil {
		return err
	}
	if msg != "" {
		return &ValidationError{Fields: map[string]string{"service_id": msg}}
	}
	length := 0
	if svc != nil {
		id := svc.ID
		a.ServiceID, a.ServiceName, a.ServiceChannel = &id, svc.Name, svc.Channel
		a.DurationMinutes, length = svc.DurationMinutes, svc.DurationMinutes
		if svc.Location != "" && a.LocationNote == "" {
			a.LocationNote = svc.Location
		}
	}
	msg, booked, err := s.checkOffered(ctx, a.AssignedUserID, a.StartUTC, length, uuid.Nil)
	if err != nil {
		return err
	}
	if msg != "" {
		return &ValidationError{Fields: map[string]string{field: msg}}
	}
	if booked > 0 && svc == nil {
		a.DurationMinutes = booked
	}
	return nil
}
