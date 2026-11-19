// Package subscriptions is the paid plans of professionals and
// organisations.
//
// There is no stored payment method and nothing is ever charged
// automatically. A request is an invoice: the subscription waits as
// PENDING_PAYMENT with an invoice number and the amount. It is paid either
// outside the platform (for example by bank transfer, quoting the invoice
// number) and recorded by a finance administrator (Approve), or, when a
// payment gateway is configured (API 0.42.0, modules/payments), online on
// the provider's hosted page, confirmed by its signed webhook
// (ActivatePaid). Both activate through the same path (activate):
//
//	PENDING_PAYMENT -> ACTIVE (payment recorded; starts_at, ends_at)
//	                -> REJECTED (administrator, with a reason)
//	                -> CANCELLED (subscriber, while pending; or administrator)
//	ACTIVE          -> EXPIRED (ends_at passed; the worker's sweep, and every
//	                   read treats a past ends_at as EXPIRED at once)
//	                -> CANCELLED (administrator, with a reason and whether a
//	                   pro-rata refund is due; or a full refund through the
//	                   gateway, RecordRefund)
//
// A period is QUARTERLY (3 months) or YEARLY (1 year). Renewal opens 30 days
// before the end: it is a new PENDING_PAYMENT subscription (renewal_of) that
// starts when the current one ends, invoiced and paid like the first.
//
// A plan's limits (Feature) are shown with the subscriber's usage. A plan
// with a package_code grants the listing package of that code in the
// subscription's market for the paid period (an INVOICE assignment, written
// in the activation's transaction through the store's entitlement hook);
// that package's listing limit is what listing management enforces.
package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Subscription statuses.
const (
	StatusPendingPayment = "PENDING_PAYMENT"
	StatusActive         = "ACTIVE"
	StatusExpired        = "EXPIRED"
	StatusRejected       = "REJECTED"
	StatusCancelled      = "CANCELLED"
)

// Statuses returns every subscription status.
func Statuses() []string {
	return []string{StatusPendingPayment, StatusActive, StatusExpired, StatusRejected, StatusCancelled}
}

// Billing periods.
const (
	PeriodQuarterly = "QUARTERLY"
	PeriodYearly    = "YEARLY"
)

// Periods returns the billing periods.
func Periods() []string { return []string{PeriodQuarterly, PeriodYearly} }

// End returns when a period that starts at start ends: 3 or 12 calendar
// months later, kept inside the target month (30 November + 3 months is the
// end of February, not 2 March; 29 February + 1 year is 28 February).
func End(period string, start time.Time) time.Time {
	months := 3
	if period == PeriodYearly {
		months = 12
	}
	end := start.AddDate(0, months, 0)
	target := (int(start.Month())-1+months)%12 + 1
	for int(end.Month()) != target {
		end = end.AddDate(0, 0, -1)
	}
	return end
}

// Subscriber kinds (and the plans they may take).
const (
	KindIndividual   = "INDIVIDUAL"
	KindOrganisation = "ORGANISATION"
)

// Plan statuses.
const (
	PlanActive  = "ACTIVE"
	PlanRetired = "RETIRED"
)

// Who did something.
const (
	ActorSubscriber = "SUBSCRIBER"
	ActorAdmin      = "ADMIN"
	ActorSystem     = "SYSTEM"
	// ActorGateway is the payment gateway (a verified webhook).
	ActorGateway = "GATEWAY"
)

// History actions (subscription_events.action).
const (
	ActionRequested        = "REQUESTED"
	ActionRenewalRequested = "RENEWAL_REQUESTED"
	ActionCancelled        = "CANCELLED"
	ActionPaymentRecorded  = "PAYMENT_RECORDED"
	ActionActivated        = "ACTIVATED"
	ActionRejected         = "REJECTED"
	ActionExpired          = "EXPIRED"
	// ActionRefunded is money returned through the payment gateway.
	ActionRefunded = "REFUNDED"
)

// Outbox events (aggregate "subscription"; payload subscription_id and
// occurred_at). The worker emails the person who asked for the subscription.
const (
	EventRequested = "SUBSCRIPTION_REQUESTED"
	EventActivated = "SUBSCRIPTION_ACTIVATED"
	EventRejected  = "SUBSCRIPTION_REJECTED"
	EventCancelled = "SUBSCRIPTION_CANCELLED"
	EventExpired   = "SUBSCRIPTION_EXPIRED"

	AggregateSubscription = "subscription"
)

// EventTypes lists the outbox events of this module.
func EventTypes() []string {
	return []string{EventRequested, EventActivated, EventRejected, EventCancelled, EventExpired}
}

// Plan feature codes. A limit of nil means unlimited.
const (
	FeatureMaxActiveListings   = "MAX_ACTIVE_LISTINGS"
	FeatureMaxFeaturedListings = "MAX_FEATURED_LISTINGS"
	FeatureMaxTeamMembers      = "MAX_TEAM_MEMBERS"
)

var featureLabels = map[string]string{
	FeatureMaxActiveListings:   "Active listings",
	FeatureMaxFeaturedListings: "Featured listings",
	FeatureMaxTeamMembers:      "Team members",
}

// FeatureCodes returns the feature codes a plan may list.
func FeatureCodes() []string {
	return []string{FeatureMaxActiveListings, FeatureMaxFeaturedListings, FeatureMaxTeamMembers}
}

// FeatureLabel is how a feature is shown.
func FeatureLabel(code string) string {
	if l, ok := featureLabels[code]; ok {
		return l
	}
	return code
}

// Limits.
const (
	// RenewalWindow is how long before the end a renewal may be asked for.
	RenewalWindow    = 30 * 24 * time.Hour
	MaxNameRunes     = 120
	MaxDescription   = 2000
	MaxBenefits      = 20
	MaxBenefitRunes  = 200
	MaxReasonRunes   = 1000
	MaxNoteRunes     = 1000
	MaxReferenceLen  = 100
	MaxFeatureLimit  = 1000000
	DefaultPageSize  = 20
	MaxPageSize      = 100
	InvoicePrefix    = "RPS"
	PaymentDayLayout = "2006-01-02"
)

// PaymentInstructions is what every invoice says about paying it.
const PaymentInstructions = "Pay by bank transfer, or by another method stated on your invoice, into the account named on the invoice we sent you, " +
	"quoting the invoice number. Your subscription starts once we have recorded the payment. Nothing is charged automatically."

// Error codes of this module's endpoints.
const (
	CodePlanNotFound         = "PLAN_NOT_FOUND"
	CodePlanNotAvailable     = "PLAN_NOT_AVAILABLE"
	CodePlanRetired          = "PLAN_RETIRED"
	CodePlanNotEligible      = "PLAN_NOT_ELIGIBLE"
	CodePriceNotAvailable    = "PRICE_NOT_AVAILABLE"
	CodeSubscriptionNotFound = "SUBSCRIPTION_NOT_FOUND"
	CodeSubscriptionState    = "SUBSCRIPTION_STATE_CONFLICT"
	CodePendingExists        = "SUBSCRIPTION_PENDING_EXISTS"
	CodeActiveExists         = "SUBSCRIPTION_ACTIVE_EXISTS"
	CodeRenewalNotOpen       = "RENEWAL_NOT_OPEN"
	CodePaymentTooLow        = "PAYMENT_AMOUNT_TOO_LOW"
	CodeNotOrganisationOwner = "NOT_ORGANISATION_OWNER"
	CodePaymentMismatch      = "PAYMENT_MISMATCH"
)

// Errors, mapped to HTTP by the handler.
var (
	ErrPlanNotFound     = errors.New("plan not found")
	ErrPlanNotAvailable = errors.New("the plan is retired")
	ErrPlanRetired      = errors.New("a retired plan cannot be changed")
	ErrNotEligible      = errors.New("the caller may not take this plan")
	ErrPriceMissing     = errors.New("the plan has no price for this currency and period")
	ErrNotFound         = errors.New("subscription not found")
	ErrState            = errors.New("the subscription's status does not allow this")
	ErrPendingExists    = errors.New("a subscription already waits for payment")
	ErrActiveExists     = errors.New("the subscriber already has an active subscription")
	ErrRenewalNotOpen   = errors.New("renewal is not open")
	ErrPaymentTooLow    = errors.New("the payment is less than the invoice")
	ErrNotOwner         = errors.New("only the organisation's owner may do this")
	// ErrPaymentMismatch is a gateway payment whose amount or currency is
	// not the invoice's.
	ErrPaymentMismatch = errors.New("the payment does not match the invoice")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Feature is one limit of a plan; Limit nil is unlimited.
type Feature struct {
	Code  string `json:"code"`
	Limit *int   `json:"limit"`
}

// Price is a plan's price in one currency for one period. Amount has two
// decimals ("25.00").
type Price struct {
	CurrencyCode string `json:"currency_code"`
	Period       string `json:"period"`
	Amount       string `json:"amount"`
}

// Plan is a subscription plan.
type Plan struct {
	ID             uuid.UUID
	Name           string
	Description    string
	SubscriberKind string
	EligibleRoles  []string
	Features       []Feature
	Benefits       []string
	Prices         []Price
	Status         string
	SortOrder      int
	// PackageCode is the listing package a paid subscription grants (""
	// for none).
	PackageCode string
	CreatedBy   *uuid.UUID
	CreatedAt   time.Time
	UpdatedAt   time.Time
	RetiredAt   *time.Time
}

// PriceFor returns the plan's price in a currency for a period.
func (p Plan) PriceFor(currency, period string) (Price, bool) {
	for _, pr := range p.Prices {
		if pr.CurrencyCode == currency && pr.Period == period {
			return pr, true
		}
	}
	return Price{}, false
}

// Limit returns the limit of a feature: (nil, true) unlimited, (_, false)
// when the plan does not list it.
func (p Plan) Limit(code string) (*int, bool) {
	for _, f := range p.Features {
		if f.Code == code {
			return f.Limit, true
		}
	}
	return nil, false
}

// Who recorded a payment.
const (
	RecordedByAdmin   = "ADMIN"
	RecordedByGateway = "GATEWAY"
)

// Payment is what the administrator, or the payment gateway, recorded.
type Payment struct {
	Reference      string
	Amount         string
	ReceivedOn     string // YYYY-MM-DD
	Note           string
	RecordedBy     *uuid.UUID
	RecordedByName string
	// RecordedByKind is RecordedByAdmin or RecordedByGateway; Provider is
	// the gateway's name ("" for an administrator).
	RecordedByKind string
	Provider       string
}

// Entitlement is what a change does to the subscriber's listing package
// (applied by the store, in the change's transaction, through the hook set
// with SetEntitlements).
type Entitlement struct {
	// Grant gives the package PackageCode for the subscription's period;
	// otherwise the package the subscription granted ends.
	Grant       bool
	PackageCode string
}

// EntitlementFunc applies an entitlement in memory.
type EntitlementFunc func(x Subscription, e Entitlement, at time.Time) error

// Subscription is one subscription (or a request for one).
type Subscription struct {
	ID              uuid.UUID
	InvoiceNumber   string
	PlanID          uuid.UUID
	PlanName        string
	Period          string
	Status          string
	SubscriberKind  string
	SubscriberID    uuid.UUID
	SubscriberName  string
	OrganisationID  *uuid.UUID
	RequestedBy     *uuid.UUID
	RequestedByName string
	MarketCode      string
	CurrencyCode    string
	Amount          string
	RenewalOf       *uuid.UUID
	StartsAt        *time.Time
	EndsAt          *time.Time
	ActivatedAt     *time.Time
	Payment         *Payment
	RejectedAt      *time.Time
	RejectionReason string
	CancelledAt     *time.Time
	CancelReason    string
	CancelledBy     string
	RefundDue       bool
	RefundNote      string
	ExpiredAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// EffectiveStatus is the status as of now: an ACTIVE subscription whose end
// has passed is EXPIRED even before the sweep has recorded it.
func (s Subscription) EffectiveStatus(now time.Time) string {
	if s.Status == StatusActive && s.EndsAt != nil && !now.Before(*s.EndsAt) {
		return StatusExpired
	}
	return s.Status
}

// Current reports whether the subscription is in force at now.
func (s Subscription) Current(now time.Time) bool {
	return s.EffectiveStatus(now) == StatusActive && s.StartsAt != nil && !now.Before(*s.StartsAt)
}

// Upcoming reports whether the subscription is paid and starts later (a
// renewal waiting for the current period to end).
func (s Subscription) Upcoming(now time.Time) bool {
	return s.EffectiveStatus(now) == StatusActive && s.StartsAt != nil && now.Before(*s.StartsAt)
}

// RenewalOpensAt is when a renewal may be asked for (nil when not ACTIVE).
func (s Subscription) RenewalOpensAt() *time.Time {
	if s.Status != StatusActive || s.EndsAt == nil {
		return nil
	}
	t := s.EndsAt.Add(-RenewalWindow)
	return &t
}

// Event is one entry of a subscription's history.
type Event struct {
	ID             uuid.UUID
	SubscriptionID uuid.UUID
	Action         string
	ActorType      string
	ActorUserID    *uuid.UUID
	ActorName      string
	Note           string
	At             time.Time
}

// NewEvent returns a history entry with a fresh ID.
func NewEvent(subscriptionID uuid.UUID, action, actorType string, actorID uuid.UUID, actorName, note string, at time.Time) Event {
	var actor *uuid.UUID
	if actorID != uuid.Nil {
		a := actorID
		actor = &a
	}
	return Event{ID: uuid.New(), SubscriptionID: subscriptionID, Action: action, ActorType: actorType, ActorUserID: actor,
		ActorName: actorName, Note: note, At: at}
}

// Change is what a change to a subscription writes with it.
type Change struct {
	Events []Event
	Audit  *adminaudit.Event
	// Outbox is the event type to write ("" for none).
	Outbox string
	// Entitlement is applied with the change when not nil.
	Entitlement *Entitlement
}

// Filter selects subscriptions. Empty fields match everything.
type Filter struct {
	Status         string
	PlanID         *uuid.UUID
	Market         string
	Period         string
	SubscriberKind string
	// Subscribers limits the list to these subscriber IDs (any kind).
	Subscribers []uuid.UUID
	// CreatedFrom and CreatedTo bound created_at ([from, to); zero: open);
	// Markets keeps these market codes (nil: every market).
	CreatedFrom, CreatedTo time.Time
	Markets                []string
	// Q matches the invoice number or the subscriber's name.
	Q             string
	Limit, Offset int
}

// Store persists plans and subscriptions.
type Store interface {
	// NextInvoiceNumber returns a fresh RPS-<year>-<6 digits> number.
	NextInvoiceNumber(ctx context.Context, year int) (string, error)

	CreatePlan(ctx context.Context, p Plan, audit adminaudit.Event) error
	// Plan returns a plan, or ErrPlanNotFound.
	Plan(ctx context.Context, id uuid.UUID) (Plan, error)
	// Plans returns the plans ordered by sort order then name.
	Plans(ctx context.Context, includeRetired bool) ([]Plan, error)
	// ChangePlan locks the plan, calls change with a copy and saves it
	// (prices replaced) with the audit record.
	ChangePlan(ctx context.Context, id uuid.UUID, change func(p *Plan) (adminaudit.Event, error)) (Plan, error)

	// CreateSubscription locks the subscriber's subscriptions, calls check
	// with them, and stores s with its history entry and the outbox event.
	// ErrPendingExists when another one waits for payment.
	CreateSubscription(ctx context.Context, s Subscription, ev Event, check func(existing []Subscription) error) error
	// Subscription returns one, or ErrNotFound.
	Subscription(ctx context.Context, id uuid.UUID) (Subscription, error)
	// Subscriptions returns one page, newest first, and the total.
	Subscriptions(ctx context.Context, f Filter) ([]Subscription, int, error)
	// ChangeSubscription locks the subscription, calls change with a copy
	// and saves it with what change returns.
	ChangeSubscription(ctx context.Context, id uuid.UUID, change func(s *Subscription) (Change, error)) (Subscription, error)
	// Events returns a subscription's history, oldest first.
	Events(ctx context.Context, id uuid.UUID) ([]Event, error)
	// ExpireDue marks every ACTIVE subscription whose end is at or before
	// now EXPIRED, with its history entry and outbox event, and returns them.
	ExpireDue(ctx context.Context, now time.Time) ([]Subscription, error)
	// ActiveListings counts the subscriber's published, unexpired listings:
	// an organisation's, or a person's own (direct owner or assigned agent).
	ActiveListings(ctx context.Context, kind string, subscriberID uuid.UUID, now time.Time) (int, error)
}

// ---------------------------------------------------------------------------
// Money: decimal strings with at most two decimals, compared exactly.
// ---------------------------------------------------------------------------

var amountRe = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,2})?$`)

// ParseAmount checks a positive amount with at most two decimals and returns
// it with exactly two ("25" -> "25.00").
func ParseAmount(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if !amountRe.MatchString(raw) {
		return "", false
	}
	r, ok := new(big.Rat).SetString(raw)
	if !ok || r.Sign() <= 0 {
		return "", false
	}
	return r.FloatString(2), true
}

// CompareAmounts compares two decimal amounts (-1, 0, 1).
func CompareAmounts(a, b string) int {
	ra, okA := new(big.Rat).SetString(a)
	rb, okB := new(big.Rat).SetString(b)
	if !okA || !okB {
		return strings.Compare(a, b)
	}
	return ra.Cmp(rb)
}

// SumAmounts adds decimal amounts and returns the total with two decimals.
func SumAmounts(amounts ...string) string {
	total := new(big.Rat)
	for _, a := range amounts {
		if r, ok := new(big.Rat).SetString(a); ok {
			total.Add(total, r)
		}
	}
	return total.FloatString(2)
}

func invoiceNumber(year, n int) string { return fmt.Sprintf("%s-%04d-%06d", InvoicePrefix, year, n) }
