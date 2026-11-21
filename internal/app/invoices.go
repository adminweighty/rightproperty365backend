package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/invoices"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/outbox"
	"rightproperty365.com/backend/internal/platform/paygateway"
)

// MarketZone returns the IANA time zone of a market ("" when unknown).
func MarketZone(store markets.Store) func(ctx context.Context, code string) string {
	return func(ctx context.Context, code string) string {
		if store == nil {
			return ""
		}
		all, err := store.List(ctx)
		if err != nil {
			return ""
		}
		for _, m := range all {
			if m.Code == code {
				return m.Timezone
			}
		}
		return ""
	}
}

// InvoicesService builds the renewal invoices (API 0.47.0) on store (a
// memory store when nil), with the market's tax rule for subscriptions and
// the market's time zone for the due dates.
func InvoicesService(store invoices.Store, subs *subscriptions.Service, taxRules invoices.TaxRules, ms markets.Store,
	gateway paygateway.Gateway, logger *slog.Logger, now func() time.Time) *invoices.Service {
	if store == nil {
		store = invoices.NewMemoryStore()
	}
	online := ""
	if gateway != nil {
		online = gateway.Name()
	}
	return invoices.NewService(invoices.Deps{Store: store, Subscriptions: subs, Tax: taxRules, Zone: MarketZone(ms), OnlinePayment: online,
		Logger: logger, Now: now})
}

// RunInvoiceSweeps marks overdue invoices (with their reminder email) at
// start and then every invoices.SweepInterval until ctx ends.
func RunInvoiceSweeps(ctx context.Context, svc *invoices.Service, logger *slog.Logger) {
	ticker := time.NewTicker(invoices.SweepInterval)
	defer ticker.Stop()
	for {
		start := time.Now()
		n, err := svc.Sweep(ctx)
		sweepDone("invoices", start, err)
		if err != nil {
			logger.ErrorContext(ctx, "invoice sweep failed", slog.String("error", err.Error()))
		} else if n > 0 {
			logger.InfoContext(ctx, "invoice sweep", slog.Int("overdue", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RegisterInvoiceSettlement chains the invoice handlers after the others of
// the subscription events: an activated renewal settles its open invoice
// (paid online, or approved on the subscription's page), a renewal
// cancelled by a full refund puts its paid invoice under review.
func RegisterInvoiceSettlement(d *outbox.Dispatcher, svc *invoices.Service) {
	subscriptionID := func(ev outbox.Event) uuid.UUID {
		var p struct {
			SubscriptionID uuid.UUID `json:"subscription_id"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err == nil && p.SubscriptionID != uuid.Nil {
			return p.SubscriptionID
		}
		return ev.AggregateID
	}
	d.Also(subscriptions.EventActivated, outbox.HandlerFunc(func(ctx context.Context, ev outbox.Event) error {
		return svc.Settle(ctx, subscriptionID(ev))
	}))
	d.Also(subscriptions.EventCancelled, outbox.HandlerFunc(func(ctx context.Context, ev outbox.Event) error {
		return svc.RefundCancelled(ctx, subscriptionID(ev))
	}))
}
