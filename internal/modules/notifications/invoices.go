package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/invoices"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Renewal invoices (API 0.47.0): the payer (the subscriber, or the
// organisation's owners and managers) gets renewal-invoice when an invoice
// is issued, sent again or becomes overdue, and payment-received with the
// receipt for every payment. The emails read the invoice at send time.

// PathMyInvoices is followed by the invoice ID: the payer's invoice page.
const PathMyInvoices = "/account/invoices/"

// SetInvoiceSource supplies the invoice (and payment) behind an invoice
// event. Without it those events are dead-lettered.
func (h *Handlers) SetInvoiceSource(source func(ctx context.Context, invoiceID, paymentID uuid.UUID) (invoices.Notice, error)) {
	h.invoice = source
}

func money(currency, amount string) string { return currency + " " + amount }

// rateText is a percent without trailing decimal zeros: "15.0000" -> "15",
// "12.5000" -> "12.5", "10" -> "10".
func rateText(rate string) string {
	if !strings.Contains(rate, ".") {
		return rate
	}
	return strings.TrimRight(strings.TrimRight(rate, "0"), ".")
}

func (h *Handlers) invoiceNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if h.invoice == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no invoice source for event type %q", ev.Type))
	}
	var payload struct {
		PaymentID uuid.UUID `json:"payment_id"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	n, err := h.invoice(ctx, ev.AggregateID, payload.PaymentID)
	if err == invoices.ErrNotFound {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	inv := n.Invoice
	var people []Person
	if inv.SubscriberKind == "ORGANISATION" {
		org := inv.SubscriberID
		people, err = h.source.Recipients(ctx, nil, &org)
	} else {
		user := inv.SubscriberID
		people, err = h.source.Recipients(ctx, &user, nil)
	}
	if err != nil {
		return err
	}
	d := mail.Data{Path: PathMyInvoices + inv.ID.String(), InvoiceNumber: inv.Number, Payer: inv.PayerName, PlanName: inv.PlanName,
		ServicePeriod: mail.DateText(inv.ServiceFrom) + " to " + mail.DateText(inv.ServiceTo.AddDate(0, 0, -1)),
		DueOn:         inv.DueOn, When: occurredAt(ev), PayOnline: n.PayOnline && invoices.Compare(inv.AmountPaid, "0") == 0}
	if inv.Tax != nil {
		d.TaxLine = fmt.Sprintf("%s %s%% (%s)", inv.Tax.Name, rateText(inv.Tax.Rate), money(inv.CurrencyCode, inv.Tax.Amount))
		if inv.Tax.Inclusive {
			d.TaxLine += ", included"
		}
	}
	template := "renewal-invoice"
	switch ev.Type {
	case invoices.EventSent, invoices.EventOverdue:
		if inv.Status != invoices.StatusIssued && inv.Status != invoices.StatusPartPaid && inv.Status != invoices.StatusOverdue {
			return nil // paid, voided or disputed since: nothing to ask for
		}
		d.Amount, d.Overdue = money(inv.CurrencyCode, inv.Balance()), ev.Type == invoices.EventOverdue
	case invoices.EventPaymentRecorded:
		if n.Payment == nil {
			return outbox.Permanent(fmt.Errorf("notifications: %s without a payment", ev.Type))
		}
		template = "payment-received"
		d.Amount, d.ReceiptNumber, d.When = money(inv.CurrencyCode, n.Payment.Amount), n.Payment.ReceiptNumber, n.Payment.ReceivedOn
		if invoices.Compare(inv.Balance(), "0") > 0 {
			d.Balance = money(inv.CurrencyCode, inv.Balance())
		}
		if n.PaidUntil != nil {
			d.PaidUntil = *n.PaidUntil
		}
	}
	for _, p := range people {
		d := d
		d.Name = p.Name
		if err := add(template, recipientKey(p), p, d); err != nil {
			return err
		}
	}
	return nil
}
