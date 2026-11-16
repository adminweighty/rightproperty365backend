package notifications

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Account administration and subscriptions (API 0.25.0).

// PathSubscriptions is the subscriber's page.
const PathSubscriptions = "/account/subscriptions"

// SendAccountDeleted implements accounts.Mailer: the last email to the
// address an account had. It is sent from the request path because the
// address is erased with the account.
func (m *AccountMailer) SendAccountDeleted(ctx context.Context, to, name string) error {
	return m.send(ctx, "account-deletion-completed", to, mail.Data{Name: name, Path: PathHome})
}

// SetSuspensionReasons supplies the administrator's reason behind an
// ACCOUNT_SUSPENDED event (read at send time, like every personal detail).
func (h *Handlers) SetSuspensionReasons(reason func(ctx context.Context, userID uuid.UUID) (string, error)) {
	h.suspensionReason = reason
}

// SetSubscriptionSource supplies the subscription behind a subscription
// event. Without it those events are dead-lettered.
func (h *Handlers) SetSubscriptionSource(source func(ctx context.Context, id uuid.UUID) (subscriptions.Subscription, error)) {
	h.subscription = source
}

// periodText is how a billing period is written in an email.
func periodText(period string) string {
	if period == subscriptions.PeriodYearly {
		return "1 year"
	}
	return "3 months"
}

var subscriptionTemplates = map[string]struct{ template, status string }{
	subscriptions.EventRequested: {"subscription-invoice", "Waiting for payment"},
	subscriptions.EventActivated: {"subscription-activated", "Active"},
	subscriptions.EventRejected:  {"subscription-rejected", "Not approved"},
	subscriptions.EventCancelled: {"subscription-cancelled", "Cancelled"},
	subscriptions.EventExpired:   {"subscription-expired", "Ended"},
}

// accountNotes builds the suspension and reactivation emails (to the account).
func (h *Handlers) accountNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	user, err := h.source.User(ctx, ev.AggregateID)
	if err != nil {
		return err
	}
	d := mail.Data{Name: user.Name, When: occurredAt(ev), Path: PathSignIn}
	template := "account-reactivated"
	if ev.Type == accounts.EventAccountSuspended {
		if h.suspensionReason == nil {
			return outbox.Permanent(fmt.Errorf("notifications: no suspension reasons for event type %q", ev.Type))
		}
		reason, err := h.suspensionReason(ctx, ev.AggregateID)
		if err != nil {
			return err
		}
		template, d.Path, d.Reason = "account-suspended", PathHome, mail.SafePreview(reason, accounts.MaxReasonRunes)
	}
	return add(template, "account", user.Person, d)
}

// subscriptionNotes builds a subscription email, to the person who asked
// for the subscription only.
func (h *Handlers) subscriptionNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.subscription == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no subscription source for event type %q", ev.Type))
	}
	x, err := h.subscription(ctx, ev.AggregateID)
	if errors.Is(err, subscriptions.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if x.RequestedBy == nil {
		return nil
	}
	user, err := h.source.User(ctx, *x.RequestedBy)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	t := subscriptionTemplates[ev.Type]
	d := mail.Data{Name: user.Name, Path: PathSubscriptions, InvoiceNumber: x.InvoiceNumber, PlanName: x.PlanName, Period: periodText(x.Period),
		Amount: x.CurrencyCode + " " + x.Amount, StatusText: t.status, When: occurredAt(ev), Edit: x.RenewalOf != nil}
	switch ev.Type {
	case subscriptions.EventActivated:
		if x.Payment != nil {
			d.Amount = x.CurrencyCode + " " + x.Payment.Amount
		}
		if x.StartsAt != nil {
			d.When = *x.StartsAt
		}
		if x.EndsAt != nil {
			d.ExpiresOn = *x.EndsAt
		}
	case subscriptions.EventRejected:
		d.Reason, d.RefundDue = mail.SafePreview(x.RejectionReason, subscriptions.MaxReasonRunes), x.RefundDue
	case subscriptions.EventCancelled:
		d.Reason, d.RefundDue = mail.SafePreview(x.CancelReason, subscriptions.MaxReasonRunes), x.RefundDue
	case subscriptions.EventExpired:
		if x.EndsAt != nil {
			d.ExpiresOn = *x.EndsAt
		}
	}
	return add(t.template, "requester", user.Person, d)
}
