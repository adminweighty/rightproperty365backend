// Package analytics is first-party listing measurement (BR-006, FR-033 to
// FR-036, API 0.30.0): the events the browser sends and the ones the API
// records itself, bot filtering, daily rollups per (listing, market-local
// day) and the owner-scoped reports.
//
// Privacy rules kept here, whatever the caller:
//
//   - the raw IP is never stored, only its network (IPv4 /24, IPv6 /48);
//   - the user agent is only read to recognise bots, never stored;
//   - the referrer is never received, only its class (source);
//   - the session is an opaque pseudonym chosen by the browser; server-side
//     events carry none;
//   - raw events are never returned by any endpoint, only aggregates.
//
// These are not Google Analytics figures: see README "Listing analytics".
package analytics

import (
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the only accepted value of the ingestion body's "schema".
const SchemaVersion = 1

// Browser event types: the only ones POST /v1/analytics/events accepts.
const (
	TypeCardImpression = "property_card_impression"
	TypeDetailView     = "listing_detail_view"
	TypeContactClick   = "contact_click"
	TypeVideoPlay      = "youtube_play_intent"
)

// Server-side event types: recorded by the API from what it did (outbox
// events), and rejected when a browser sends them.
const (
	TypeListingSave        = "listing_save"
	TypeEnquirySubmit      = "enquiry_submit"
	TypeViewingRequest     = "viewing_request"
	TypeAppointmentConfirm = "appointment_confirm"
	TypeListingRelist      = "listing_relist"
	// Reserved for later features; accepted from server code only.
	TypeApplicationSubmit  = "application_submit"
	TypeBookingConfirm     = "booking_confirm"
	TypeInvoicePaymentDone = "invoice_payment_confirmed"
)

// BrowserTypes returns the event types the browser may send.
func BrowserTypes() []string {
	return []string{TypeCardImpression, TypeDetailView, TypeContactClick, TypeVideoPlay}
}

// ServerTypes returns the event types only the API records.
func ServerTypes() []string {
	return []string{TypeListingSave, TypeEnquirySubmit, TypeViewingRequest, TypeAppointmentConfirm, TypeListingRelist,
		TypeApplicationSubmit, TypeBookingConfirm, TypeInvoicePaymentDone}
}

// Placements of a card impression.
func Placements() []string { return []string{"search", "rail", "similar", "home", "saved"} }

// Sources are the referrer classes the browser works out (never the referrer).
func Sources() []string {
	return []string{"direct", "search_engine", "social", "internal", "email", "other"}
}

// Devices are the device classes, from the viewport.
func Devices() []string { return []string{"mobile", "tablet", "desktop"} }

// Unknown is the breakdown key of a view sent without a source or device.
const Unknown = "unknown"

// Origins of a stored event.
const (
	OriginBrowser = "browser"
	OriginServer  = "server"
)

// Why an event was marked as a bot.
const (
	BotUserAgent = "user_agent"
	BotRate      = "rate"
)

// Limits and windows.
const (
	// MaxEventsPerBatch caps one POST /v1/analytics/events.
	MaxEventsPerBatch = 50
	// MaxBodyBytes caps its body (50 events of about 400 bytes, with room).
	MaxBodyBytes = 32 << 10
	// PastWindow and FutureWindow bound a browser event's occurred_at around
	// the time it is received; anything outside is rejected. A time in the
	// (short) future is clamped to the receive time.
	PastWindow   = 24 * time.Hour
	FutureWindow = 5 * time.Minute
	// LateAfter: an event older than this when it arrives (browser or
	// server) is dropped as late and counted. It matches the rollup's
	// recompute window of 3 market-local days.
	LateAfter = 72 * time.Hour
	// RollupDays is how many market-local days (today included) every
	// rollup run recomputes.
	RollupDays = 3
	// SPAWindow: a second listing_detail_view of the same listing in the
	// same session less than this apart is a duplicate (SPA navigation).
	SPAWindow = 30 * time.Minute
	// Rate-based bot detection: more than RateLimit events from one session
	// or one network within RateWindow marks the excess as bot.
	RateLimit  = 300
	RateWindow = 10 * time.Minute
	// DefaultRawRetentionDays is ANALYTICS_RAW_RETENTION_DAYS' default.
	DefaultRawRetentionDays = 90
	// MaxCustomDays caps a custom report period.
	MaxCustomDays = 366
	// TopListings is the length of the top-listings lists.
	TopListings = 10
)

// Report periods.
const (
	Period24h    = "24h"
	Period7d     = "7d"
	Period30d    = "30d"
	PeriodCustom = "custom"
)

// Labels and definitions carried by every report.
const (
	NearRealTimeLabel    = "Near real time, may change"
	CTRDefinition        = "contact clicks / views: contact_click events per valid listing_detail_view"
	ConversionDefinition = "enquiries / views: enquiry_submit events per valid listing_detail_view"
	DefinitionsPath      = "/analytics-definitions"
)

// Errors returned by the service.
var (
	ErrUnauthenticated = errors.New("analytics: not signed in")
	ErrForbidden       = errors.New("analytics: not allowed")
	ErrNotFound        = errors.New("analytics: not found")
)

// ValidationError lists the query fields that are wrong.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "analytics: invalid request" }

// Event is one stored raw event. It never leaves the API.
type Event struct {
	ID         uuid.UUID
	Type       string
	OccurredAt time.Time
	ReceivedAt time.Time
	// LocalDay is the market-local calendar day of OccurredAt (YYYY-MM-DD),
	// worked out at ingestion from the market's time zone.
	LocalDay   string
	ListingID  uuid.UUID
	MarketCode string
	Placement  string
	Rank       *int
	Source     string
	Device     string
	// Session is the browser's pseudonym ("" for server-side events).
	Session string
	// Network is the truncated client network ("" for server-side events).
	Network   string
	Bot       bool
	BotReason string
	Origin    string
}

// LateEvent is an event dropped because it arrived too late. Only its
// identity and where it would have counted are kept.
type LateEvent struct {
	EventID    uuid.UUID
	Type       string
	ListingID  uuid.UUID
	MarketCode string
	OccurredAt time.Time
	ReceivedAt time.Time
	// LocalDay is the market-local day of ReceivedAt: the day whose
	// late_events_dropped counts it.
	LocalDay string
}

// InsertResult is what Store.Insert did.
type InsertResult int

// Insert outcomes.
const (
	Inserted InsertResult = iota
	// Duplicate: an event with the same event_id is already stored.
	Duplicate
	// SPADuplicate: a view of the same listing in the same session is
	// already stored less than SPAWindow away.
	SPADuplicate
)

// Totals are the metric counts of a period, a day or a listing.
type Totals struct {
	Views                 int `json:"views"`
	UniqueViews           int `json:"unique_views"`
	Impressions           int `json:"impressions"`
	ContactClicks         int `json:"contact_clicks"`
	VideoPlays            int `json:"video_plays"`
	Saves                 int `json:"saves"`
	Enquiries             int `json:"enquiries"`
	ViewingRequests       int `json:"viewing_requests"`
	AppointmentsConfirmed int `json:"appointments_confirmed"`
	Relists               int `json:"relists"`
}

func (t *Totals) add(o Totals) {
	t.Views += o.Views
	t.UniqueViews += o.UniqueViews
	t.Impressions += o.Impressions
	t.ContactClicks += o.ContactClicks
	t.VideoPlays += o.VideoPlays
	t.Saves += o.Saves
	t.Enquiries += o.Enquiries
	t.ViewingRequests += o.ViewingRequests
	t.AppointmentsConfirmed += o.AppointmentsConfirmed
	t.Relists += o.Relists
}

// count adds one valid (non-bot) event of type t.
func (t *Totals) count(eventType string) {
	switch eventType {
	case TypeDetailView:
		t.Views++
	case TypeCardImpression:
		t.Impressions++
	case TypeContactClick:
		t.ContactClicks++
	case TypeVideoPlay:
		t.VideoPlays++
	case TypeListingSave:
		t.Saves++
	case TypeEnquirySubmit:
		t.Enquiries++
	case TypeViewingRequest:
		t.ViewingRequests++
	case TypeAppointmentConfirm:
		t.AppointmentsConfirmed++
	case TypeListingRelist:
		t.Relists++
	}
}

// DayRow is the aggregate of one listing on one market-local day: a stored
// rollup row, or the same figures worked out from raw events.
type DayRow struct {
	ListingID  uuid.UUID
	Day        string
	MarketCode string
	Totals
	FilteredBotEvents int
	LateEventsDropped int
	// Sources and Devices break the valid views down (Unknown for a view
	// sent without one).
	Sources     map[string]int
	Devices     map[string]int
	RefreshedAt time.Time
}

// AggregateFilter selects raw events to aggregate. Nil slices match
// everything; an empty non-nil slice matches nothing.
type AggregateFilter struct {
	MarketCodes []string
	ListingIDs  []uuid.UUID
	// FromDay keeps events whose LocalDay is on or after it ("" = any), and
	// late events whose LocalDay (of arrival) is.
	FromDay string
	// Since keeps events that occurred at or after it (zero = any), and late
	// events that arrived at or after it.
	Since time.Time
}

// RollupFilter selects stored rollup rows, days inclusive.
type RollupFilter struct {
	MarketCodes []string
	ListingIDs  []uuid.UUID
	FromDay     string
	ToDay       string
}

func matchesIDs(ids []uuid.UUID, id uuid.UUID) bool { return ids == nil || slices.Contains(ids, id) }

func matchesCodes(codes []string, code string) bool {
	return codes == nil || slices.Contains(codes, code)
}
