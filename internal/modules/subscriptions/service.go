package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// PlanAdminRoles manage the plans: SUPER_ADMIN and FINANCE_ADMIN.
func PlanAdminRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformFinanceAdmin} }

// ReadRoles may list and open subscriptions: SUPER_ADMIN, FINANCE_ADMIN,
// PLATFORM_ADMIN and SUPPORT (read-only for the last two).
func ReadRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformFinanceAdmin, auth.PlatformAdmin, auth.PlatformSupport}
}

// DecisionRoles record payments, approve, reject and cancel subscriptions:
// SUPER_ADMIN and FINANCE_ADMIN.
func DecisionRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformFinanceAdmin} }

// ProfessionalRoles are the account roles a plan may be offered to: every
// selectable role except the property seekers'.
func ProfessionalRoles() []string {
	var out []string
	for _, r := range identity.SelectableAccountRoles() {
		if !identity.IsSeekerRole(r) {
			out = append(out, r)
		}
	}
	return out
}

// Deps are the service's collaborators.
type Deps struct {
	Store Store
	// Currency returns the currency of a market (false for an unknown one).
	Currency func(ctx context.Context, marketCode string) (string, bool, error)
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service is the subscriptions use cases.
type Service struct{ d Deps }

// NewService builds the service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{d: d}
}

func (s *Service) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

// Store returns the store (wiring and tests).
func (s *Service) Store() Store { return s.d.Store }

// ---------------------------------------------------------------------------
// Plans
// ---------------------------------------------------------------------------

// PlanInput is the body of POST and PATCH /v1/admin/subscription-plans.
// On PATCH every field is optional; prices, features and benefits replace
// the whole list.
type PlanInput struct {
	Name           *string    `json:"name"`
	Description    *string    `json:"description"`
	SubscriberKind *string    `json:"subscriber_kind"`
	EligibleRoles  *[]string  `json:"eligible_roles"`
	Features       *[]Feature `json:"features"`
	Benefits       *[]string  `json:"benefits"`
	Prices         *[]Price   `json:"prices"`
	SortOrder      *int       `json:"sort_order"`
}

var currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)

func runes(s string) int { return utf8.RuneCountInString(s) }

// apply validates in and applies it to p (create: every required field must
// be present).
func applyPlan(p *Plan, in PlanInput, create bool) error {
	fields := map[string]string{}
	if in.Name != nil {
		p.Name = strings.TrimSpace(*in.Name)
	}
	if (create || in.Name != nil) && (p.Name == "" || runes(p.Name) > MaxNameRunes) {
		fields["name"] = "is required (at most 120 characters)"
	}
	if in.Description != nil {
		p.Description = strings.TrimSpace(*in.Description)
		if runes(p.Description) > MaxDescription {
			fields["description"] = "must be at most 2000 characters"
		}
	}
	if in.SubscriberKind != nil {
		p.SubscriberKind = strings.ToUpper(strings.TrimSpace(*in.SubscriberKind))
	}
	if (create || in.SubscriberKind != nil) && p.SubscriberKind != KindIndividual && p.SubscriberKind != KindOrganisation {
		fields["subscriber_kind"] = "must be INDIVIDUAL or ORGANISATION"
	}
	if in.EligibleRoles != nil {
		p.EligibleRoles = nil
		allowed := ProfessionalRoles()
		for _, r := range *in.EligibleRoles {
			r = strings.ToUpper(strings.TrimSpace(r))
			switch {
			case !slices.Contains(allowed, r):
				fields["eligible_roles"] = "must be professional account roles (" + strings.Join(allowed, ", ") + ")"
			case slices.Contains(p.EligibleRoles, r):
				fields["eligible_roles"] = "must not repeat a role"
			default:
				p.EligibleRoles = append(p.EligibleRoles, r)
			}
		}
	}
	if (create || in.EligibleRoles != nil) && len(p.EligibleRoles) == 0 && fields["eligible_roles"] == "" {
		fields["eligible_roles"] = "must list at least one role"
	}
	if in.Features != nil {
		p.Features = []Feature{}
		for _, f := range *in.Features {
			code := strings.ToUpper(strings.TrimSpace(f.Code))
			switch {
			case !slices.Contains(FeatureCodes(), code):
				fields["features"] = "codes must be one of " + strings.Join(FeatureCodes(), ", ")
			case f.Limit != nil && (*f.Limit < 0 || *f.Limit > MaxFeatureLimit):
				fields["features"] = "a limit must be between 0 and 1000000, or null for unlimited"
			default:
				for _, have := range p.Features {
					if have.Code == code {
						fields["features"] = "must not repeat a code"
					}
				}
				p.Features = append(p.Features, Feature{Code: code, Limit: f.Limit})
			}
		}
	}
	if in.Benefits != nil {
		p.Benefits = []string{}
		for _, b := range *in.Benefits {
			b = strings.TrimSpace(b)
			if b == "" || runes(b) > MaxBenefitRunes {
				fields["benefits"] = "each benefit must be 1 to 200 characters"
				continue
			}
			p.Benefits = append(p.Benefits, b)
		}
		if len(p.Benefits) > MaxBenefits {
			fields["benefits"] = "at most 20 benefits"
		}
	}
	if in.Prices != nil {
		p.Prices = []Price{}
		for _, pr := range *in.Prices {
			cur, period := strings.ToUpper(strings.TrimSpace(pr.CurrencyCode)), strings.ToUpper(strings.TrimSpace(pr.Period))
			amount, ok := ParseAmount(pr.Amount)
			switch {
			case !currencyRe.MatchString(cur):
				fields["prices"] = "currency_code must be an ISO 4217 code"
			case !slices.Contains(Periods(), period):
				fields["prices"] = "period must be QUARTERLY or YEARLY"
			case !ok:
				fields["prices"] = "amount must be a positive number with at most two decimals, as a string"
			default:
				if _, dup := p.PriceFor(cur, period); dup {
					fields["prices"] = "one price per currency and period"
				}
				p.Prices = append(p.Prices, Price{CurrencyCode: cur, Period: period, Amount: amount})
			}
		}
	}
	if (create || in.Prices != nil) && len(p.Prices) == 0 && fields["prices"] == "" {
		fields["prices"] = "must list at least one price"
	}
	if in.SortOrder != nil {
		p.SortOrder = *in.SortOrder
	}
	if len(fields) > 0 {
		return &ValidationError{Fields: fields}
	}
	return nil
}

func planState(p Plan) map[string]any {
	return map[string]any{"name": p.Name, "status": p.Status, "subscriber_kind": p.SubscriberKind, "eligible_roles": p.EligibleRoles,
		"features": p.Features, "prices": p.Prices, "sort_order": p.SortOrder}
}

// Plans returns every plan (retired ones too when asked).
func (s *Service) Plans(ctx context.Context, includeRetired bool) ([]Plan, error) {
	return s.d.Store.Plans(ctx, includeRetired)
}

// Plan returns one plan.
func (s *Service) Plan(ctx context.Context, id uuid.UUID) (Plan, error) {
	return s.d.Store.Plan(ctx, id)
}

// CreatePlan creates an ACTIVE plan.
func (s *Service) CreatePlan(ctx context.Context, p *auth.Principal, in PlanInput) (Plan, error) {
	now := s.now()
	plan := Plan{ID: uuid.New(), Status: PlanActive, Features: []Feature{}, Benefits: []string{}, CreatedAt: now, UpdatedAt: now}
	if p != nil {
		by := p.UserID
		plan.CreatedBy = &by
	}
	if err := applyPlan(&plan, in, true); err != nil {
		return Plan{}, err
	}
	audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionPlanCreated, adminaudit.TargetPlan, plan.ID.String(), plan.Name, "", nil, planState(plan), now)
	if err := s.d.Store.CreatePlan(ctx, plan, audit); err != nil {
		return Plan{}, err
	}
	return s.d.Store.Plan(ctx, plan.ID)
}

// UpdatePlan changes an ACTIVE plan. Existing subscriptions keep the name,
// period and amount they were invoiced with.
func (s *Service) UpdatePlan(ctx context.Context, p *auth.Principal, id uuid.UUID, in PlanInput) (Plan, error) {
	now := s.now()
	return s.d.Store.ChangePlan(ctx, id, func(plan *Plan) (adminaudit.Event, error) {
		if plan.Status == PlanRetired {
			return adminaudit.Event{}, ErrPlanRetired
		}
		before := planState(*plan)
		if err := applyPlan(plan, in, false); err != nil {
			return adminaudit.Event{}, err
		}
		plan.UpdatedAt = now
		return adminaudit.New(p.UserID, p.Name, adminaudit.ActionPlanUpdated, adminaudit.TargetPlan, plan.ID.String(), plan.Name, "", before, planState(*plan), now), nil
	})
}

// RetirePlan retires a plan: it is no longer offered or renewed; the
// subscriptions already taken run to their end.
func (s *Service) RetirePlan(ctx context.Context, p *auth.Principal, id uuid.UUID) (Plan, error) {
	now := s.now()
	return s.d.Store.ChangePlan(ctx, id, func(plan *Plan) (adminaudit.Event, error) {
		if plan.Status == PlanRetired {
			return adminaudit.Event{}, ErrPlanRetired
		}
		plan.Status, plan.RetiredAt, plan.UpdatedAt = PlanRetired, &now, now
		return adminaudit.New(p.UserID, p.Name, adminaudit.ActionPlanRetired, adminaudit.TargetPlan, plan.ID.String(), plan.Name, "",
			map[string]any{"status": PlanActive}, map[string]any{"status": PlanRetired}, now), nil
	})
}

// ---------------------------------------------------------------------------
// The subscriber's side
// ---------------------------------------------------------------------------

// Subscriber is someone who can hold a subscription.
type Subscriber struct {
	Kind       string    `json:"kind"`
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	MarketCode string    `json:"market_code,omitempty"`
}

// subscribers are the caller and the organisations they own.
func subscribers(p *auth.Principal) []Subscriber {
	out := []Subscriber{{Kind: KindIndividual, ID: p.UserID, Name: p.Name}}
	for _, m := range p.Memberships {
		if m.Role == auth.RoleOwner {
			out = append(out, Subscriber{Kind: KindOrganisation, ID: m.OrganisationID, Name: m.OrganisationName, MarketCode: m.MarketCode})
		}
	}
	return out
}

func subscriberIDs(list []Subscriber) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(list))
	for _, x := range list {
		ids = append(ids, x.ID)
	}
	return ids
}

// eligible reports whether the caller holds an approved (ACTIVE) role the
// plan is offered to, and owns an organisation when the plan is for one.
func eligible(p *auth.Principal, plan Plan) bool {
	holds := false
	for _, r := range p.AccountRoles {
		if r.Status == identity.AccountRoleActive && slices.Contains(plan.EligibleRoles, r.Role) {
			holds = true
		}
	}
	if !holds {
		return false
	}
	if plan.SubscriberKind == KindOrganisation {
		return len(subscribers(p)) > 1
	}
	return true
}

// AvailablePlans returns the ACTIVE plans the caller may take. With a market,
// only the prices in its currency are kept (and a plan without one is left out).
func (s *Service) AvailablePlans(ctx context.Context, p *auth.Principal, market string) ([]Plan, string, error) {
	currency := ""
	if market != "" {
		cur, ok, err := s.d.Currency(ctx, strings.ToUpper(market))
		if err != nil {
			return nil, "", err
		}
		if !ok {
			return nil, "", &ValidationError{Fields: map[string]string{"market": "is not a known country code"}}
		}
		currency = cur
	}
	all, err := s.d.Store.Plans(ctx, false)
	if err != nil {
		return nil, "", err
	}
	out := []Plan{}
	for _, plan := range all {
		if !eligible(p, plan) {
			continue
		}
		if currency != "" {
			var kept []Price
			for _, pr := range plan.Prices {
				if pr.CurrencyCode == currency {
					kept = append(kept, pr)
				}
			}
			if len(kept) == 0 {
				continue
			}
			plan.Prices = kept
		}
		out = append(out, plan)
	}
	return out, currency, nil
}

// SubscriberState is one subscriber's subscriptions as GET /v1/me/subscriptions shows them.
type SubscriberState struct {
	Subscriber Subscriber
	Current    *Subscription
	Upcoming   *Subscription
	Pending    *Subscription
	History    []Subscription
	CanRenew   bool
	// ActiveListings is the subscriber's published listings now;
	// MaxActiveListings the current plan's limit (nil: unlimited or no plan).
	ActiveListings    int
	MaxActiveListings *int
	HasLimit          bool
}

func (s *Service) state(ctx context.Context, sub Subscriber, list []Subscription, now time.Time) (SubscriberState, error) {
	st := SubscriberState{Subscriber: sub, History: []Subscription{}}
	for i := range list {
		x := list[i]
		if x.SubscriberID != sub.ID || x.SubscriberKind != sub.Kind {
			continue
		}
		st.History = append(st.History, x)
		switch {
		case x.Current(now) && st.Current == nil:
			st.Current = &list[i]
		case x.Upcoming(now) && st.Upcoming == nil:
			st.Upcoming = &list[i]
		case x.Status == StatusPendingPayment && st.Pending == nil:
			st.Pending = &list[i]
		}
	}
	if st.Current != nil {
		opens := st.Current.RenewalOpensAt()
		st.CanRenew = st.Pending == nil && st.Upcoming == nil && opens != nil && !now.Before(*opens)
		if plan, err := s.d.Store.Plan(ctx, st.Current.PlanID); err == nil {
			st.MaxActiveListings, st.HasLimit = plan.Limit(FeatureMaxActiveListings)
		} else if !errors.Is(err, ErrPlanNotFound) {
			return SubscriberState{}, err
		}
	}
	n, err := s.d.Store.ActiveListings(ctx, sub.Kind, sub.ID, now)
	if err != nil {
		return SubscriberState{}, err
	}
	st.ActiveListings = n
	return st, nil
}

// Mine returns the subscriptions of the caller and of the organisations
// they own.
func (s *Service) Mine(ctx context.Context, p *auth.Principal) ([]SubscriberState, error) {
	subs := subscribers(p)
	list, _, err := s.d.Store.Subscriptions(ctx, Filter{Subscribers: subscriberIDs(subs)})
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]SubscriberState, 0, len(subs))
	for _, sub := range subs {
		st, err := s.state(ctx, sub, list, now)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

// RequestInput is the body of POST /v1/me/subscriptions.
type RequestInput struct {
	PlanID         string `json:"plan_id"`
	Period         string `json:"period"`
	MarketCode     string `json:"market_code"`
	OrganisationID string `json:"organisation_id"`
}

// resolveSubscriber decides who the subscription is for and its market.
func resolveSubscriber(p *auth.Principal, plan Plan, in RequestInput, fields map[string]string) (Subscriber, string) {
	market := strings.ToUpper(strings.TrimSpace(in.MarketCode))
	if plan.SubscriberKind == KindOrganisation {
		id, err := uuid.Parse(strings.TrimSpace(in.OrganisationID))
		if err != nil {
			fields["organisation_id"] = "is required for this plan (an organisation you own)"
			return Subscriber{}, ""
		}
		for _, sub := range subscribers(p) {
			if sub.Kind == KindOrganisation && sub.ID == id {
				if market != "" && market != sub.MarketCode {
					fields["market_code"] = "must be the organisation's country (" + sub.MarketCode + ")"
				}
				return sub, sub.MarketCode
			}
		}
		return Subscriber{ID: uuid.Nil}, ""
	}
	if strings.TrimSpace(in.OrganisationID) != "" {
		fields["organisation_id"] = "must be empty: this plan is for a person"
	}
	if market == "" && len(p.Memberships) > 0 {
		market = p.Memberships[0].MarketCode
	}
	if market == "" {
		fields["market_code"] = "is required (the country whose currency you will pay in)"
	}
	return Subscriber{Kind: KindIndividual, ID: p.UserID, Name: p.Name}, market
}

// Request asks for a plan: a PENDING_PAYMENT subscription with its invoice.
func (s *Service) Request(ctx context.Context, p *auth.Principal, in RequestInput) (Subscription, error) {
	fields := map[string]string{}
	planID, err := uuid.Parse(strings.TrimSpace(in.PlanID))
	if err != nil {
		fields["plan_id"] = "is required"
	}
	period := strings.ToUpper(strings.TrimSpace(in.Period))
	if !slices.Contains(Periods(), period) {
		fields["period"] = "must be QUARTERLY or YEARLY"
	}
	if len(fields) > 0 {
		return Subscription{}, &ValidationError{Fields: fields}
	}
	plan, err := s.d.Store.Plan(ctx, planID)
	if err != nil {
		return Subscription{}, err
	}
	if plan.Status != PlanActive {
		return Subscription{}, ErrPlanNotAvailable
	}
	sub, market := resolveSubscriber(p, plan, in, fields)
	if len(fields) > 0 {
		return Subscription{}, &ValidationError{Fields: fields}
	}
	if sub.ID == uuid.Nil {
		return Subscription{}, ErrNotOwner
	}
	if !eligible(p, plan) {
		return Subscription{}, ErrNotEligible
	}
	currency, ok, err := s.d.Currency(ctx, market)
	if err != nil {
		return Subscription{}, err
	}
	if !ok {
		return Subscription{}, &ValidationError{Fields: map[string]string{"market_code": "is not a known country code"}}
	}
	price, ok := plan.PriceFor(currency, period)
	if !ok {
		return Subscription{}, ErrPriceMissing
	}
	now := s.now()
	number, err := s.d.Store.NextInvoiceNumber(ctx, now.Year())
	if err != nil {
		return Subscription{}, err
	}
	requester := p.UserID
	x := Subscription{ID: uuid.New(), InvoiceNumber: number, PlanID: plan.ID, PlanName: plan.Name, Period: period, Status: StatusPendingPayment,
		SubscriberKind: sub.Kind, SubscriberID: sub.ID, SubscriberName: sub.Name, RequestedBy: &requester, RequestedByName: p.Name,
		MarketCode: market, CurrencyCode: currency, Amount: price.Amount, CreatedAt: now, UpdatedAt: now}
	if sub.Kind == KindOrganisation {
		org := sub.ID
		x.OrganisationID = &org
	}
	ev := NewEvent(x.ID, ActionRequested, ActorSubscriber, p.UserID, p.Name, "", now)
	err = s.d.Store.CreateSubscription(ctx, x, ev, func(existing []Subscription) error {
		for _, e := range existing {
			switch {
			case e.Status == StatusPendingPayment:
				return ErrPendingExists
			case e.EffectiveStatus(now) == StatusActive:
				return ErrActiveExists
			}
		}
		return nil
	})
	if err != nil {
		return Subscription{}, err
	}
	return x, nil
}

// own returns a subscription of the caller (or of an organisation they
// own); ErrNotFound for anybody else's.
func (s *Service) own(ctx context.Context, p *auth.Principal, id uuid.UUID) (Subscription, error) {
	x, err := s.d.Store.Subscription(ctx, id)
	if err != nil {
		return Subscription{}, err
	}
	for _, sub := range subscribers(p) {
		if sub.Kind == x.SubscriberKind && sub.ID == x.SubscriberID {
			return x, nil
		}
	}
	return Subscription{}, ErrNotFound
}

// Mine returns one of the caller's subscriptions with its history.
func (s *Service) MineOne(ctx context.Context, p *auth.Principal, id uuid.UUID) (Subscription, []Event, error) {
	x, err := s.own(ctx, p, id)
	if err != nil {
		return Subscription{}, nil, err
	}
	events, err := s.d.Store.Events(ctx, id)
	return x, events, err
}

// CancelPending withdraws the caller's request while it waits for payment.
func (s *Service) CancelPending(ctx context.Context, p *auth.Principal, id uuid.UUID) (Subscription, error) {
	if _, err := s.own(ctx, p, id); err != nil {
		return Subscription{}, err
	}
	now := s.now()
	return s.d.Store.ChangeSubscription(ctx, id, func(x *Subscription) (Change, error) {
		if x.Status != StatusPendingPayment {
			return Change{}, ErrState
		}
		x.Status, x.CancelledAt, x.CancelReason, x.CancelledBy, x.UpdatedAt = StatusCancelled, &now, "Cancelled by the subscriber.", ActorSubscriber, now
		return Change{Events: []Event{NewEvent(x.ID, ActionCancelled, ActorSubscriber, p.UserID, p.Name, "", now)}}, nil
	})
}

// RenewInput is the body of POST /v1/me/subscriptions/{id}/renew.
type RenewInput struct {
	// Period defaults to the current subscription's.
	Period string `json:"period"`
}

// Renew asks for the next period of a current subscription: from 30 days
// before its end, a new PENDING_PAYMENT subscription that will start when
// the current one ends.
func (s *Service) Renew(ctx context.Context, p *auth.Principal, id uuid.UUID, in RenewInput) (Subscription, error) {
	cur, err := s.own(ctx, p, id)
	if err != nil {
		return Subscription{}, err
	}
	now := s.now()
	period := strings.ToUpper(strings.TrimSpace(in.Period))
	if period == "" {
		period = cur.Period
	}
	if !slices.Contains(Periods(), period) {
		return Subscription{}, &ValidationError{Fields: map[string]string{"period": "must be QUARTERLY or YEARLY"}}
	}
	if !cur.Current(now) {
		return Subscription{}, ErrState
	}
	if opens := cur.RenewalOpensAt(); opens == nil || now.Before(*opens) {
		return Subscription{}, ErrRenewalNotOpen
	}
	plan, err := s.d.Store.Plan(ctx, cur.PlanID)
	if err != nil {
		return Subscription{}, err
	}
	if plan.Status != PlanActive {
		return Subscription{}, ErrPlanNotAvailable
	}
	price, ok := plan.PriceFor(cur.CurrencyCode, period)
	if !ok {
		return Subscription{}, ErrPriceMissing
	}
	number, err := s.d.Store.NextInvoiceNumber(ctx, now.Year())
	if err != nil {
		return Subscription{}, err
	}
	requester, of := p.UserID, cur.ID
	x := Subscription{ID: uuid.New(), InvoiceNumber: number, PlanID: plan.ID, PlanName: plan.Name, Period: period, Status: StatusPendingPayment,
		SubscriberKind: cur.SubscriberKind, SubscriberID: cur.SubscriberID, SubscriberName: cur.SubscriberName, OrganisationID: cur.OrganisationID,
		RequestedBy: &requester, RequestedByName: p.Name, MarketCode: cur.MarketCode, CurrencyCode: cur.CurrencyCode, Amount: price.Amount,
		RenewalOf: &of, CreatedAt: now, UpdatedAt: now}
	ev := NewEvent(x.ID, ActionRenewalRequested, ActorSubscriber, p.UserID, p.Name, "Renewal of "+cur.InvoiceNumber, now)
	err = s.d.Store.CreateSubscription(ctx, x, ev, func(existing []Subscription) error {
		for _, e := range existing {
			switch {
			case e.Status == StatusPendingPayment:
				return ErrPendingExists
			case e.Upcoming(now):
				return ErrActiveExists
			}
		}
		return nil
	})
	if err != nil {
		return Subscription{}, err
	}
	return x, nil
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// List returns one page of subscriptions.
func (s *Service) List(ctx context.Context, f Filter) ([]Subscription, int, error) {
	fields := map[string]string{}
	if f.Status != "" && !slices.Contains(Statuses(), f.Status) {
		fields["status"] = "must be one of " + strings.Join(Statuses(), ", ")
	}
	if f.Period != "" && !slices.Contains(Periods(), f.Period) {
		fields["period"] = "must be QUARTERLY or YEARLY"
	}
	if f.SubscriberKind != "" && f.SubscriberKind != KindIndividual && f.SubscriberKind != KindOrganisation {
		fields["subscriber_kind"] = "must be INDIVIDUAL or ORGANISATION"
	}
	if f.Market != "" && len(f.Market) != 2 {
		fields["market"] = "must be a two-letter country code"
	}
	if len(fields) > 0 {
		return nil, 0, &ValidationError{Fields: fields}
	}
	return s.d.Store.Subscriptions(ctx, f)
}

// Detail is a subscription with its history and the subscriber's usage.
type Detail struct {
	Subscription      Subscription
	Events            []Event
	ActiveListings    int
	MaxActiveListings *int
	HasLimit          bool
}

// Get returns a subscription's detail.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Detail, error) {
	x, err := s.d.Store.Subscription(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	events, err := s.d.Store.Events(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Subscription: x, Events: events}
	if plan, err := s.d.Store.Plan(ctx, x.PlanID); err == nil {
		d.MaxActiveListings, d.HasLimit = plan.Limit(FeatureMaxActiveListings)
	}
	if d.ActiveListings, err = s.d.Store.ActiveListings(ctx, x.SubscriberKind, x.SubscriberID, s.now()); err != nil {
		return Detail{}, err
	}
	return d, nil
}

// ApproveInput is the payment an administrator records.
type ApproveInput struct {
	PaymentReference string `json:"payment_reference"`
	Amount           string `json:"amount"`
	ReceivedOn       string `json:"received_on"`
	Note             string `json:"note"`
}

// Approve records the payment of a PENDING_PAYMENT subscription and
// activates it: it starts now, or when the subscriber's paid period ends for
// a renewal, and ends 3 months or a year later.
func (s *Service) Approve(ctx context.Context, p *auth.Principal, id uuid.UUID, in ApproveInput) (Detail, error) {
	fields := map[string]string{}
	ref := strings.TrimSpace(in.PaymentReference)
	if ref == "" || len(ref) > MaxReferenceLen {
		fields["payment_reference"] = "is required (the bank reference or receipt number, at most 100 characters)"
	}
	amount, ok := ParseAmount(in.Amount)
	if !ok {
		fields["amount"] = "must be a positive number with at most two decimals, as a string"
	}
	now := s.now()
	received, err := time.Parse(PaymentDayLayout, strings.TrimSpace(in.ReceivedOn))
	switch {
	case err != nil:
		fields["received_on"] = "must be a date (YYYY-MM-DD)"
	case received.After(now):
		fields["received_on"] = "cannot be in the future"
	}
	note := strings.TrimSpace(in.Note)
	if runes(note) > MaxNoteRunes {
		fields["note"] = "must be at most 1000 characters"
	}
	if len(fields) > 0 {
		return Detail{}, &ValidationError{Fields: fields}
	}
	cur, err := s.d.Store.Subscription(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	// A paid period of the same subscriber that ends later pushes the start.
	others, _, err := s.d.Store.Subscriptions(ctx, Filter{SubscriberKind: cur.SubscriberKind, Subscribers: []uuid.UUID{cur.SubscriberID}, Status: StatusActive})
	if err != nil {
		return Detail{}, err
	}
	start := now
	for _, o := range others {
		if o.ID != cur.ID && o.EndsAt != nil && o.EndsAt.After(start) {
			start = *o.EndsAt
		}
	}
	by := p.UserID
	if _, err := s.d.Store.ChangeSubscription(ctx, id, func(x *Subscription) (Change, error) {
		if x.Status != StatusPendingPayment {
			return Change{}, ErrState
		}
		if CompareAmounts(amount, x.Amount) < 0 {
			return Change{}, ErrPaymentTooLow
		}
		end := End(x.Period, start)
		x.Status, x.StartsAt, x.EndsAt, x.ActivatedAt, x.UpdatedAt = StatusActive, &start, &end, &now, now
		x.Payment = &Payment{Reference: ref, Amount: amount, ReceivedOn: received.Format(PaymentDayLayout), Note: note, RecordedBy: &by, RecordedByName: p.Name}
		paid := fmt.Sprintf("%s %s received on %s, reference %s", x.CurrencyCode, amount, x.Payment.ReceivedOn, ref)
		audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionSubscriptionApproved, adminaudit.TargetSubscription, x.ID.String(), x.InvoiceNumber, note,
			map[string]any{"status": StatusPendingPayment}, map[string]any{"status": StatusActive, "payment_amount": amount, "currency_code": x.CurrencyCode,
				"payment_reference": ref, "received_on": x.Payment.ReceivedOn, "starts_at": start, "ends_at": end}, now)
		return Change{Events: []Event{
			NewEvent(x.ID, ActionPaymentRecorded, ActorAdmin, p.UserID, p.Name, paid, now),
			NewEvent(x.ID, ActionActivated, ActorAdmin, p.UserID, p.Name, "", now),
		}, Audit: &audit, Outbox: EventActivated}, nil
	}); err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, id)
}

// DecisionInput is the body of reject and of the administrator's cancel.
type DecisionInput struct {
	Reason     string `json:"reason"`
	RefundDue  bool   `json:"refund_due"`
	RefundNote string `json:"refund_note"`
}

func (in DecisionInput) validate() (reason, refundNote string, err error) {
	fields := map[string]string{}
	reason = strings.TrimSpace(in.Reason)
	if runes(reason) < 3 || runes(reason) > MaxReasonRunes {
		fields["reason"] = "is required (3 to 1000 characters)"
	}
	refundNote = strings.TrimSpace(in.RefundNote)
	if runes(refundNote) > MaxNoteRunes {
		fields["refund_note"] = "must be at most 1000 characters"
	}
	if len(fields) > 0 {
		return "", "", &ValidationError{Fields: fields}
	}
	return reason, refundNote, nil
}

// Reject refuses a PENDING_PAYMENT subscription. When a payment was received
// all the same, refund_due records that it must be returned (Refund Policy:
// "Service not activated").
func (s *Service) Reject(ctx context.Context, p *auth.Principal, id uuid.UUID, in DecisionInput) (Detail, error) {
	reason, refundNote, err := in.validate()
	if err != nil {
		return Detail{}, err
	}
	now := s.now()
	if _, err := s.d.Store.ChangeSubscription(ctx, id, func(x *Subscription) (Change, error) {
		if x.Status != StatusPendingPayment {
			return Change{}, ErrState
		}
		x.Status, x.RejectedAt, x.RejectionReason, x.RefundDue, x.RefundNote, x.UpdatedAt = StatusRejected, &now, reason, in.RefundDue, refundNote, now
		audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionSubscriptionRejected, adminaudit.TargetSubscription, x.ID.String(), x.InvoiceNumber, reason,
			map[string]any{"status": StatusPendingPayment}, map[string]any{"status": StatusRejected, "refund_due": in.RefundDue}, now)
		return Change{Events: []Event{NewEvent(x.ID, ActionRejected, ActorAdmin, p.UserID, p.Name, reason, now)}, Audit: &audit, Outbox: EventRejected}, nil
	}); err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, id)
}

// Cancel ends an ACTIVE (current or upcoming) or PENDING_PAYMENT
// subscription on an administrator's decision. refund_due records whether
// the unused part is to be refunded under the Refund Policy (for example a
// withdrawal that is not for the subscriber's breach of the terms); no money
// moves here.
func (s *Service) Cancel(ctx context.Context, p *auth.Principal, id uuid.UUID, in DecisionInput) (Detail, error) {
	reason, refundNote, err := in.validate()
	if err != nil {
		return Detail{}, err
	}
	now := s.now()
	if _, err := s.d.Store.ChangeSubscription(ctx, id, func(x *Subscription) (Change, error) {
		from := x.EffectiveStatus(now)
		if from != StatusActive && from != StatusPendingPayment {
			return Change{}, ErrState
		}
		x.Status, x.CancelledAt, x.CancelReason, x.CancelledBy, x.UpdatedAt = StatusCancelled, &now, reason, ActorAdmin, now
		x.RefundDue, x.RefundNote = in.RefundDue, refundNote
		audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionSubscriptionCanceled, adminaudit.TargetSubscription, x.ID.String(), x.InvoiceNumber, reason,
			map[string]any{"status": from}, map[string]any{"status": StatusCancelled, "refund_due": in.RefundDue, "refund_note": refundNote}, now)
		return Change{Events: []Event{NewEvent(x.ID, ActionCancelled, ActorAdmin, p.UserID, p.Name, reason, now)}, Audit: &audit, Outbox: EventCancelled}, nil
	}); err != nil {
		return Detail{}, err
	}
	return s.Get(ctx, id)
}

// Sweep records the expiry of every ACTIVE subscription whose end has passed.
func (s *Service) Sweep(ctx context.Context) (int, error) {
	expired, err := s.d.Store.ExpireDue(ctx, s.now())
	return len(expired), err
}
