package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Fixed IDs of the sample subscription plans and of the agent's pending
// subscription (so the seed is idempotent).
var (
	AgentStarterPlanID       = id("5001")
	AgencyProPlanID          = id("5002")
	LandlordPlanID           = id("5003")
	PendingAgentSubscription = id("5101")
)

func limit(n int) *int { return &n }

// SamplePlans are the development plans, priced in USD (Zimbabwe's currency).
func SamplePlans(now time.Time) []subscriptions.Plan {
	now = now.UTC().Truncate(time.Microsecond)
	return []subscriptions.Plan{
		{ID: AgentStarterPlanID, Name: "Agent Starter", Description: "For an estate agent advertising on their own account.",
			SubscriberKind: subscriptions.KindIndividual,
			EligibleRoles:  []string{identity.AccountRoleAgent, identity.AccountRoleSalesAgent, identity.AccountRoleLettingsAgent},
			Features: []subscriptions.Feature{{Code: subscriptions.FeatureMaxActiveListings, Limit: limit(10)},
				{Code: subscriptions.FeatureMaxFeaturedListings, Limit: limit(1)}},
			Benefits: []string{"Up to 10 active listings", "One featured listing at a time", "Viewing and enquiry inbox"},
			Prices: []subscriptions.Price{{CurrencyCode: "USD", Period: subscriptions.PeriodQuarterly, Amount: "30.00"},
				{CurrencyCode: "USD", Period: subscriptions.PeriodYearly, Amount: "100.00"}},
			Status: subscriptions.PlanActive, SortOrder: 10, CreatedAt: now, UpdatedAt: now},
		{ID: AgencyProPlanID, Name: "Agency Pro", Description: "For an agency and its whole team, taken by the agency's principal.",
			SubscriberKind: subscriptions.KindOrganisation, EligibleRoles: []string{identity.AccountRoleAgencyOwner},
			Features: []subscriptions.Feature{{Code: subscriptions.FeatureMaxActiveListings, Limit: nil},
				{Code: subscriptions.FeatureMaxFeaturedListings, Limit: limit(10)}, {Code: subscriptions.FeatureMaxTeamMembers, Limit: limit(25)}},
			Benefits: []string{"Unlimited active listings", "Up to 10 featured listings", "Up to 25 team members", "Lease templates and e-signing"},
			Prices: []subscriptions.Price{{CurrencyCode: "USD", Period: subscriptions.PeriodQuarterly, Amount: "150.00"},
				{CurrencyCode: "USD", Period: subscriptions.PeriodYearly, Amount: "500.00"}},
			Status: subscriptions.PlanActive, SortOrder: 20, CreatedAt: now, UpdatedAt: now},
		{ID: LandlordPlanID, Name: "Landlord", Description: "For a landlord or seller advertising their own property.",
			SubscriberKind: subscriptions.KindIndividual, EligibleRoles: []string{identity.AccountRoleLandlord, identity.AccountRolePrivateSeller},
			Features: []subscriptions.Feature{{Code: subscriptions.FeatureMaxActiveListings, Limit: limit(5)}},
			Benefits: []string{"Up to 5 active listings", "Lease drafting for your properties"},
			Prices: []subscriptions.Price{{CurrencyCode: "USD", Period: subscriptions.PeriodQuarterly, Amount: "20.00"},
				{CurrencyCode: "USD", Period: subscriptions.PeriodYearly, Amount: "65.00"}},
			Status: subscriptions.PlanActive, SortOrder: 30, CreatedAt: now, UpdatedAt: now},
	}
}

// pendingAgentSubscription is agent@example.test asking for Agent Starter,
// quarterly, in Zimbabwe: what the administrators' approval screen shows.
func pendingAgentSubscription(invoice string, now time.Time) (subscriptions.Subscription, subscriptions.Event) {
	now = now.UTC().Truncate(time.Microsecond)
	agent := identity.SeedAgentUserID
	x := subscriptions.Subscription{ID: PendingAgentSubscription, InvoiceNumber: invoice, PlanID: AgentStarterPlanID, PlanName: "Agent Starter",
		Period: subscriptions.PeriodQuarterly, Status: subscriptions.StatusPendingPayment, SubscriberKind: subscriptions.KindIndividual,
		SubscriberID: agent, SubscriberName: "Tendai Ncube", RequestedBy: &agent, RequestedByName: "Tendai Ncube",
		MarketCode: MarketCode, CurrencyCode: "USD", Amount: "30.00", CreatedAt: now, UpdatedAt: now}
	return x, subscriptions.NewEvent(x.ID, subscriptions.ActionRequested, subscriptions.ActorSubscriber, agent, "Tendai Ncube", "", now)
}

// SeedMemorySubscriptions puts the sample plans and the agent's pending
// subscription into the memory store (without emails or audit records).
func SeedMemorySubscriptions(ctx context.Context, store *subscriptions.MemoryStore, now time.Time) error {
	for _, p := range SamplePlans(now) {
		store.PutPlan(p)
	}
	if _, err := store.Subscription(ctx, PendingAgentSubscription); err == nil {
		return nil
	}
	invoice, err := store.NextInvoiceNumber(ctx, now.UTC().Year())
	if err != nil {
		return err
	}
	x, ev := pendingAgentSubscription(invoice, now)
	store.PutSubscription(x, ev)
	return nil
}

var errAlreadyWaiting = errors.New("the agent already has a subscription waiting")

// SeedPostgresSubscriptions inserts the sample plans and the agent's pending
// subscription when they are missing (idempotent: existing rows are never
// changed, and nothing is added when the agent already has a subscription
// waiting for payment). Needs migration 0024 and the seeded agent account.
func SeedPostgresSubscriptions(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	store := subscriptions.NewPostgresStore(pool)
	for _, p := range SamplePlans(now) {
		if _, err := store.Plan(ctx, p.ID); err == nil {
			continue
		} else if !errors.Is(err, subscriptions.ErrPlanNotFound) {
			return err
		}
		audit := adminaudit.New(uuid.Nil, "Development seed", adminaudit.ActionPlanCreated, adminaudit.TargetPlan, p.ID.String(), p.Name,
			"Sample plan (development seed).", nil, map[string]any{"name": p.Name}, p.CreatedAt)
		if err := store.CreatePlan(ctx, p, audit); err != nil {
			return fmt.Errorf("seed plan %s: %w", p.Name, err)
		}
	}
	if _, err := store.Subscription(ctx, PendingAgentSubscription); err == nil {
		return nil
	} else if !errors.Is(err, subscriptions.ErrNotFound) {
		return err
	}
	invoice, err := store.NextInvoiceNumber(ctx, now.UTC().Year())
	if err != nil {
		return err
	}
	x, ev := pendingAgentSubscription(invoice, now)
	err = store.CreateSubscription(ctx, x, ev, func(existing []subscriptions.Subscription) error {
		for _, e := range existing {
			if e.Status == subscriptions.StatusPendingPayment || e.Status == subscriptions.StatusActive {
				return errAlreadyWaiting
			}
		}
		return nil
	})
	if errors.Is(err, errAlreadyWaiting) || errors.Is(err, subscriptions.ErrPendingExists) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("seed pending subscription: %w", err)
	}
	return nil
}
