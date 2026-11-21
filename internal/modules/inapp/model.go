// Package inapp is the notification centre (API 0.35.0, FR-044, FR-045,
// FR-062, FR-064, FR-065, FR-003): the durable in-app notifications the
// worker creates from outbox events, the devices that receive push, the
// per-type preferences, and the push delivery records.
//
//   - One logical notification per (source event, recipient): a unique key,
//     so a redelivered outbox event never creates a second one.
//   - Text is safe text only: a reference and a place, never a customer's
//     name, phone, email, message or answers (the callers build it; the
//     notifications module has a test against the seed).
//   - Delivery records say PENDING, ATTEMPTED, PROVIDER_ACCEPTED, FAILED or
//     DEAD. Nothing ever says "delivered": the provider accepting a push is
//     not a person seeing it. read_at is the notification's own, set by the
//     recipient.
package inapp

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/httpx"
)

// Notification types. The catalogue below is the order and the labels the
// preference page shows.
const (
	TypeNewLead                 = "NEW_LEAD"
	TypeNewEnquiry              = "NEW_ENQUIRY"
	TypeRequestUpdate           = "REQUEST_UPDATE"
	TypeAppointmentRequested    = "APPOINTMENT_REQUESTED"
	TypeAppointmentConfirmed    = "APPOINTMENT_CONFIRMED"
	TypeAppointmentCancelled    = "APPOINTMENT_CANCELLED"
	TypeAppointmentRescheduled  = "APPOINTMENT_RESCHEDULED"
	TypeAppointmentReminder     = "APPOINTMENT_REMINDER"
	TypeSavedSearchAlert        = "SAVED_SEARCH_ALERT"
	TypeApplicationReceived     = "APPLICATION_RECEIVED"
	TypeApplicationUpdated      = "APPLICATION_UPDATED"
	TypeVenueBookingRequested   = "VENUE_BOOKING_REQUESTED"
	TypeVenueBookingConfirmed   = "VENUE_BOOKING_CONFIRMED"
	TypeVenueBookingDeclined    = "VENUE_BOOKING_DECLINED"
	TypeVenueBookingCancelled   = "VENUE_BOOKING_CANCELLED"
	TypeVenueBookingRescheduled = "VENUE_BOOKING_RESCHEDULED"
	TypeListingReviewed         = "LISTING_REVIEWED"
	TypeListingExpiry           = "LISTING_EXPIRY"
	TypeSubscriptionInvoice     = "SUBSCRIPTION_INVOICE"
	TypeSubscriptionActivated   = "SUBSCRIPTION_ACTIVATED"
	TypeSubscriptionExpired     = "SUBSCRIPTION_EXPIRED"
	TypeSubscriptionUpdated     = "SUBSCRIPTION_UPDATED"
	TypeTeamInvitation          = "TEAM_INVITATION"
	TypeMaintenance             = "MAINTENANCE"
	TypeInspection              = "INSPECTION"
	TypeReview                  = "REVIEW"
	TypeAccount                 = "ACCOUNT"
	TypeSecurity                = "SECURITY"

	// TypeSummary is the data type of a summary push (never a notification).
	TypeSummary = "SUMMARY"
)

// TypeInfo is one row of the catalogue.
type TypeInfo struct {
	Type  string
	Label string
	// Essential types are transactional notices nobody may switch off
	// (FR-003): account, security, subscription invoices. EssentialReason
	// says why, for the preference page.
	Essential       bool
	EssentialReason string
}

var catalogue = []TypeInfo{
	{TypeNewLead, "New property enquiries", false, ""},
	{TypeNewEnquiry, "New business enquiries", false, ""},
	{TypeRequestUpdate, "Replies and updates on your requests", false, ""},
	{TypeAppointmentRequested, "Viewing and consultation requests", false, ""},
	{TypeAppointmentConfirmed, "Appointment confirmations", false, ""},
	{TypeAppointmentCancelled, "Appointment cancellations", false, ""},
	{TypeAppointmentRescheduled, "Appointment time changes", false, ""},
	{TypeAppointmentReminder, "Appointment reminders", false, ""},
	{TypeSavedSearchAlert, "Saved search alerts", false, ""},
	{TypeApplicationReceived, "New applications", false, ""},
	{TypeApplicationUpdated, "Application updates", false, ""},
	{TypeVenueBookingRequested, "Venue booking requests", false, ""},
	{TypeVenueBookingConfirmed, "Venue booking confirmations", false, ""},
	{TypeVenueBookingDeclined, "Venue booking declines", false, ""},
	{TypeVenueBookingCancelled, "Venue booking cancellations", false, ""},
	{TypeVenueBookingRescheduled, "Venue booking time changes", false, ""},
	{TypeListingReviewed, "Listing review decisions", false, ""},
	{TypeListingExpiry, "Listing expiry reminders", false, ""},
	{TypeSubscriptionInvoice, "Subscription invoices", true, "Invoices are needed to pay for and keep your subscription."},
	{TypeSubscriptionActivated, "Subscription activated", false, ""},
	{TypeSubscriptionExpired, "Subscription ended", false, ""},
	{TypeSubscriptionUpdated, "Other subscription updates", false, ""},
	{TypeTeamInvitation, "Team invitations", false, ""},
	{TypeMaintenance, "Maintenance requests", false, ""},
	{TypeInspection, "Inspections", false, ""},
	{TypeReview, "Reviews (your reviews moderated, new reviews of your business)", false, ""},
	{TypeAccount, "Account suspension and reactivation", true, "We must tell you when access to your account changes."},
	{TypeSecurity, "Security (password changes, administrator sign-ins)", true, "Security notices protect your account and cannot be turned off."},
}

// Catalogue returns every type, in display order.
func Catalogue() []TypeInfo { return append([]TypeInfo(nil), catalogue...) }

// Lookup returns a type's catalogue row.
func Lookup(t string) (TypeInfo, bool) {
	for _, c := range catalogue {
		if c.Type == t {
			return c, true
		}
	}
	return TypeInfo{}, false
}

// IsEssential reports whether t can never be switched off.
func IsEssential(t string) bool {
	info, ok := Lookup(t)
	return ok && info.Essential
}

// Channels and delivery statuses.
const (
	ChannelPush  = "PUSH"
	ChannelEmail = "EMAIL"

	StatusPending          = "PENDING"
	StatusAttempted        = "ATTEMPTED"
	StatusProviderAccepted = "PROVIDER_ACCEPTED"
	StatusFailed           = "FAILED"
	StatusDead             = "DEAD"
)

// Statuses lists the delivery statuses in display order.
func Statuses() []string {
	return []string{StatusPending, StatusAttempted, StatusProviderAccepted, StatusFailed, StatusDead}
}

// Platforms of a device.
const (
	PlatformIOS     = "IOS"
	PlatformAndroid = "ANDROID"
	PlatformWeb     = "WEB"
)

// Preference change sources (from the X-Client header). The mapping is
// shared with marketing consent and lives in httpx.
const (
	SourceWeb = httpx.ClientSourceWeb
	SourceApp = httpx.ClientSourceApp
	SourceAPI = httpx.ClientSourceAPI
)

// SourceFromClient maps an X-Client header to a change source (see
// httpx.SourceFromClient).
func SourceFromClient(h string) string { return httpx.SourceFromClient(h) }

// Limits.
const (
	MaxTokenLength      = 4096
	MaxAppVersionLength = 40
	MaxLocaleLength     = 35
	MaxTitleRunes       = 120
	MaxBodyRunes        = 200
	PageSize            = 20
	// MaxPushAttempts is the number of tries before a push is DEAD.
	MaxPushAttempts = 6
	// BurstLimit pushes per device in BurstWindow; beyond it a summary push
	// is sent instead.
	BurstLimit  = 5
	BurstWindow = time.Minute
)

// Errors.
var (
	ErrNotFound       = errors.New("inapp: not found")
	ErrDeviceNotFound = errors.New("inapp: device not found")
)

// Draft is a notification to create.
type Draft struct {
	SourceEventID uuid.UUID
	UserID        uuid.UUID
	Type          string
	Title         string
	Body          string
	Link          string
	Reference     string
}

// Notification is a stored in-app notification.
type Notification struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	SourceEventID uuid.UUID
	Type          string
	Title         string
	Body          string
	Link          string
	Reference     string
	CreatedAt     time.Time
	ReadAt        *time.Time
}

// Device is a registered push token.
type Device struct {
	ID     uuid.UUID
	UserID uuid.UUID
	// SessionID is the session that registered (or last re-registered) the
	// token: its logout removes the device.
	SessionID   *uuid.UUID
	Token       string
	Platform    string
	AppVersion  string
	Locale      string
	PushConsent bool
	CreatedAt   time.Time
	LastSeenAt  time.Time
}

// Preference is a user's choice for one type (the default is email and push on).
type Preference struct {
	Type      string
	Email     bool
	Push      bool
	ChangedAt *time.Time
	Source    string
}

// PreferenceChange is one item of a PUT (nil leaves a channel as it is).
type PreferenceChange struct {
	Type  string
	Email *bool
	Push  *bool
}

// PushJob is a claimed push delivery with what sending it needs.
type PushJob struct {
	DeliveryID   uuid.UUID
	Attempts     int // including this one
	Notification Notification
	DeviceID     uuid.UUID
	Token        string
}

// PushOutcome is the result of a send for a set of deliveries (one push,
// possibly a summary covering several).
type PushOutcome struct {
	DeliveryIDs []uuid.UUID
	Status      string // PROVIDER_ACCEPTED, FAILED or DEAD
	MessageID   string
	LastError   string
	// NextAttemptAt is set for FAILED.
	NextAttemptAt *time.Time
	BatchID       uuid.UUID
	Summary       bool
	At            time.Time
}

// Health is what the admin health endpoint reports.
type Health struct {
	Counts               map[string]map[string]int // channel -> status -> count
	DeadLetterBacklog    int
	OldestUnattemptedAt  *time.Time
	NotificationsLast24h int
	UnreadTotal          int
}

// Store keeps the notification centre.
type Store interface {
	// UpsertDevice registers d.Token for d.UserID: a new device, or the
	// existing row of that token updated (and moved from another user).
	UpsertDevice(ctx context.Context, d Device) (Device, bool, error)
	Devices(ctx context.Context, userID uuid.UUID) ([]Device, error)
	// DeleteDevice removes the user's device (ErrDeviceNotFound otherwise).
	DeleteDevice(ctx context.Context, userID, deviceID uuid.UUID) error
	// DeleteDeviceByToken removes a token: the user's own (ErrDeviceNotFound
	// otherwise), or whoever's when userID is nil (the provider said the
	// token is invalid).
	DeleteDeviceByToken(ctx context.Context, userID *uuid.UUID, token string) error
	// DeleteSessionDevices removes the devices a session registered.
	DeleteSessionDevices(ctx context.Context, userID, sessionID uuid.UUID) (int, error)

	// CreateNotification stores a draft once per (source event, user): it
	// returns the existing one and false when it is already there. When
	// created and push is true, it queues one PENDING push delivery per
	// device of the user with push consent, in the same transaction.
	CreateNotification(ctx context.Context, d Draft, push bool, now time.Time) (Notification, bool, error)
	NotificationBySource(ctx context.Context, sourceEventID, userID uuid.UUID) (Notification, bool, error)
	List(ctx context.Context, userID uuid.UUID, unreadOnly bool, offset, limit int) ([]Notification, int, error)
	UnreadCount(ctx context.Context, userID uuid.UUID) (int, error)
	MarkRead(ctx context.Context, userID, id uuid.UUID, now time.Time) (Notification, error)
	MarkAllRead(ctx context.Context, userID uuid.UUID, now time.Time) (int, error)
	// RecordEmail upserts the EMAIL delivery record of a notification.
	RecordEmail(ctx context.Context, notificationID uuid.UUID, status string, attempts int, lastError string, now time.Time) error

	// ClaimPush leases up to limit due push deliveries (ATTEMPTED, attempts
	// + 1, leased until now + leaseFor).
	ClaimPush(ctx context.Context, now time.Time, limit int, leaseFor time.Duration) ([]PushJob, error)
	// DeferPush puts claimed deliveries back to PENDING, due at at, without
	// counting the attempt (burst control).
	DeferPush(ctx context.Context, ids []uuid.UUID, at time.Time) error
	// RecordPush stores the outcome of one send.
	RecordPush(ctx context.Context, o PushOutcome) error
	// PushBatchesSince counts the pushes sent to a device since.
	PushBatchesSince(ctx context.Context, deviceID uuid.UUID, since time.Time) (int, error)

	Preferences(ctx context.Context, userID uuid.UUID) (map[string]Preference, error)
	// SetPreferences applies the changes (already validated) and records
	// each actual change in the history.
	SetPreferences(ctx context.Context, userID uuid.UUID, changes []PreferenceChange, source string, now time.Time) error

	Health(ctx context.Context, now time.Time) (Health, error)
}
