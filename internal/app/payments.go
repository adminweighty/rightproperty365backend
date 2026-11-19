package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/modules/payments"
	"rightproperty365.com/backend/internal/modules/subscriptions"
)

// Online payment (API 0.42.0) and the link between paid subscriptions and
// listing packages.

// invoiceGrant is the package grant of an activated subscription: the
// subscriber's listing account (an organisation, or the person as a direct
// owner) in the subscription's market, for its paid period.
func invoiceGrant(x subscriptions.Subscription, code string, at time.Time) (packages.InvoiceGrant, bool) {
	if x.StartsAt == nil || x.EndsAt == nil || code == "" {
		return packages.InvoiceGrant{}, false
	}
	acct := packages.Account{Kind: packages.KindDirectOwner, ID: x.SubscriberID}
	if x.SubscriberKind == subscriptions.KindOrganisation {
		acct.Kind = packages.KindOrganisation
	}
	from, to := packages.InvoiceDates(*x.StartsAt, *x.EndsAt)
	return packages.InvoiceGrant{Account: acct, MarketCode: x.MarketCode, PackageCode: code, StartsOn: from, EndsOn: to, SubscriptionID: x.ID,
		Label: invoiceLabel(x), At: at}, true
}

func invoiceLabel(x subscriptions.Subscription) string { return "Subscription " + x.InvoiceNumber }

// LinkSubscriptionPackages makes a subscription's activation (an
// administrator's approval or the payment gateway) grant its plan's listing
// package (an INVOICE assignment for the paid period), and its cancellation
// or full refund end it, in the same transaction (Postgres) or under the
// same lock (memory). Stores of other kinds are left as they are.
func LinkSubscriptionPackages(subs subscriptions.Store, pk packages.Store) {
	switch s := subs.(type) {
	case *subscriptions.PostgresStore:
		if _, ok := pk.(*packages.PostgresStore); !ok {
			return
		}
		s.SetEntitlements(func(ctx context.Context, tx pgx.Tx, x subscriptions.Subscription, e subscriptions.Entitlement, at time.Time) error {
			if e.Grant {
				g, ok := invoiceGrant(x, e.PackageCode, at)
				if !ok {
					return nil
				}
				_, err := packages.GrantInvoiceTx(ctx, tx, g)
				return err
			}
			_, err := packages.EndInvoiceTx(ctx, tx, x.ID, invoiceLabel(x), at)
			return err
		})
	case *subscriptions.MemoryStore:
		m, ok := pk.(*packages.MemoryStore)
		if !ok {
			return
		}
		s.SetEntitlements(func(x subscriptions.Subscription, e subscriptions.Entitlement, at time.Time) error {
			if e.Grant {
				if g, ok := invoiceGrant(x, e.PackageCode, at); ok {
					m.GrantInvoice(g)
				}
				return nil
			}
			m.EndInvoice(x.ID, invoiceLabel(x), at)
			return nil
		})
	}
}

// PaymentReconcileInterval is how often the payment reconciliation runs.
const PaymentReconcileInterval = 5 * time.Minute

// RunPaymentReconciliation runs payments.Service.Reconcile every
// PaymentReconcileInterval until ctx ends: open checkouts past their expiry
// become EXPIRED, older ones are checked with the provider, and follow-ups a
// crash could have left are completed. Idempotent; the API (in-memory stores,
// or the fake gateway, whose state is in the API process) and the worker may
// both run it.
func RunPaymentReconciliation(ctx context.Context, svc *payments.Service, logger *slog.Logger) {
	ticker := time.NewTicker(PaymentReconcileInterval)
	defer ticker.Stop()
	for {
		res, err := svc.Reconcile(ctx)
		if err != nil {
			logger.ErrorContext(ctx, "payment reconciliation failed", slog.String("error", err.Error()))
		} else if res.Expired+res.Activated+res.Cancelled > 0 {
			logger.InfoContext(ctx, "payment reconciliation", slog.Int("expired", res.Expired), slog.Int("checked", res.Checked),
				slog.Int("activated", res.Activated), slog.Int("cancelled", res.Cancelled))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// skipAuthentication is true for the payment provider's routes: webhooks and
// the fake hosted page never use a session, and a browser's session cookie
// on the fake page's form must not trip the CSRF origin rule.
func skipAuthentication(path string) bool { return strings.HasPrefix(path, "/v1/payments/") }
