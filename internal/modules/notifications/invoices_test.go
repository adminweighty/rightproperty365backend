package notifications

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/invoices"
	"rightproperty365.com/backend/internal/platform/outbox"
)

func invoiceEvent(eventType string, id uuid.UUID, payload map[string]any) outbox.Event {
	raw, _ := json.Marshal(payload)
	return outbox.Event{ID: uuid.New(), Type: eventType, AggregateType: invoices.AggregateInvoice, AggregateID: id, Payload: raw,
		CreatedAt: time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)}
}

// The renewal invoice emails (API 0.47.0): to the payer, with the amount
// due, the tax line and the due date; nothing asked for once the invoice is
// paid; a receipt for each payment; a safe in-app title.
func TestRenewalInvoiceNotifications(t *testing.T) {
	f := newFixture(t)
	_, inbox := f.withInbox()
	payer := agentID(t, f)
	inv := invoices.Invoice{ID: uuid.New(), Number: "INV-ZW-2026-000007", Status: invoices.StatusIssued, CurrencyCode: "USD",
		SubscriberKind: "INDIVIDUAL", SubscriberID: payer, PayerName: "Tendai Ncube", PlanName: "Agent Pro",
		ServiceFrom: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), ServiceTo: time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC),
		DueOn: time.Date(2026, 12, 4, 0, 0, 0, 0, time.UTC), Subtotal: "30.00", Total: "34.50", AmountPaid: "0.00", AmountCredited: "0.00",
		Tax: &invoices.Tax{Name: "VAT", Rate: "15.0000", Amount: "4.50"}}
	payment := invoices.Payment{ID: uuid.New(), ReceiptNumber: "RCT-ZW-2026-000003", Amount: "10.00", Method: invoices.MethodBankTransfer,
		ReceivedOn: time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC)}
	f.handlers.SetInvoiceSource(func(_ context.Context, id, paymentID uuid.UUID) (invoices.Notice, error) {
		if id != inv.ID {
			return invoices.Notice{}, invoices.ErrNotFound
		}
		n := invoices.Notice{Invoice: inv, PayOnline: true}
		if paymentID == payment.ID {
			p := payment
			n.Payment = &p
		}
		return n, nil
	})

	sent := invoiceEvent(invoices.EventSent, inv.ID, map[string]any{"invoice_id": inv.ID, "kind": "ISSUED"})
	for range 2 {
		if err := f.handle(sent); err != nil {
			t.Fatal(err)
		}
	}
	mails := f.sender.byKind("renewal-invoice")
	if recipients(mails) != identity.SeedAgentEmail {
		t.Fatalf("sent to %v", recipients(mails))
	}
	for _, want := range []string{"Invoice: INV-ZW-2026-000007", "Tax: VAT 15% (USD 4.50)", "Amount due: USD 34.50", "Due: 4 December 2026",
		"Service period: 1 December 2026 to 28 February 2027", "pay online", "/account/invoices/" + inv.ID.String()} {
		if !strings.Contains(mails[0].Text, want) {
			t.Fatalf("missing %q:\n%s", want, mails[0].Text)
		}
	}
	notes := notificationsOf(t, inbox, payer)
	if len(notes) == 0 || notes[0].Type != inapp.TypeSubscriptionInvoice || notes[0].Title != "Invoice INV-ZW-2026-000007 for USD 34.50" {
		t.Fatalf("in-app: %+v", notes)
	}

	// A part payment: the receipt, and what is still due.
	inv.Status, inv.AmountPaid = invoices.StatusPartPaid, "10.00"
	if err := f.handle(invoiceEvent(invoices.EventPaymentRecorded, inv.ID, map[string]any{"invoice_id": inv.ID, "payment_id": payment.ID})); err != nil {
		t.Fatal(err)
	}
	receipts := f.sender.byKind("payment-received")
	if len(receipts) != 1 || !strings.Contains(receipts[0].Text, "Receipt: RCT-ZW-2026-000003") || !strings.Contains(receipts[0].Text, "Still due: USD 24.50") {
		t.Fatalf("receipt: %+v", receipts)
	}
	// Overdue after a part payment: the balance, and no online payment offered.
	overdue := invoiceEvent(invoices.EventOverdue, inv.ID, map[string]any{"invoice_id": inv.ID})
	if err := f.handle(overdue); err != nil {
		t.Fatal(err)
	}
	mails = f.sender.byKind("renewal-invoice")
	last := mails[len(mails)-1]
	if !strings.HasPrefix(last.Subject, "Overdue:") || !strings.Contains(last.Text, "Amount due: USD 24.50") || strings.Contains(last.Text, "pay online") {
		t.Fatalf("overdue: %s\n%s", last.Subject, last.Text)
	}
	// Paid since: a late resend asks for nothing.
	inv.Status = invoices.StatusPaid
	before := len(f.sender.byKind("renewal-invoice"))
	if err := f.handle(invoiceEvent(invoices.EventSent, inv.ID, map[string]any{"invoice_id": inv.ID, "kind": "RESENT"})); err != nil {
		t.Fatal(err)
	}
	if len(f.sender.byKind("renewal-invoice")) != before {
		t.Fatal("a paid invoice was sent again")
	}
}
