package notifications

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// The conversation on a property request (API 0.46.0, FR-063, FR-064):
//
//   - PROPERTY_REQUEST_REPLIED: the receiving side wrote; the customer gets
//     request-update with a preview (an account holder also in-app). A guest
//     gets the email only, saying the conversation continues from an account.
//   - PROPERTY_REQUEST_CUSTOMER_REPLIED: the customer answered; the request's
//     recipients (the assigned member or direct seller, otherwise the
//     organisation's owners and managers) get request-customer-replied.
//   - PROPERTY_REQUEST_STAGE_CHANGED: the customer gets request-update only
//     when THEIR status changes, and not for the move a reply makes (the
//     reply email already says it). Assignments send the customer nothing.

// PathMyRequests is followed by the request ID: the customer's request page.
const PathMyRequests = "/account/requests/"

// SetRequestMessageSource supplies a property request's conversation, for
// the preview in the reply emails. Without it the emails carry no preview.
func (h *Handlers) SetRequestMessageSource(source func(ctx context.Context, requestID uuid.UUID) ([]propertyrequests.Message, error)) {
	h.requestMessages = source
}

// messagePreview is the safe preview of one message of the conversation.
func (h *Handlers) messagePreview(ctx context.Context, requestID, messageID uuid.UUID) (string, error) {
	if h.requestMessages == nil || messageID == uuid.Nil {
		return "", nil
	}
	msgs, err := h.requestMessages(ctx, requestID)
	if err != nil {
		return "", err
	}
	for _, m := range msgs {
		if m.ID == messageID {
			return mail.SafePreview(m.Body, mailPreviewLength), nil
		}
	}
	return "", nil
}

// mailPreviewLength is the size of a reply's preview in the emails.
const mailPreviewLength = 300

func (h *Handlers) requestConversationNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	var payload struct {
		MessageID uuid.UUID `json:"message_id"`
		From      string    `json:"from"`
		To        string    `json:"to"`
		Via       string    `json:"via"`
	}
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	if ev.Type == propertyrequests.EventStageChanged {
		before, after := propertyrequests.CustomerStatus(payload.From), propertyrequests.CustomerStatus(payload.To)
		if payload.Via == propertyrequests.StageViaReply || before == after {
			return nil // nothing the customer would see changed, or the reply email says it
		}
	}
	info, err := h.source.PropertyRequest(ctx, ev.AggregateID)
	if err != nil {
		return err
	}
	var listingTitle, listingRef string
	if h.listing != nil {
		listingTitle, listingRef, _ = h.listing(ctx, info.ListingID)
	}
	base := mail.Data{Reference: info.Reference, RequestType: mail.RequestTypeName(info.RequestType), ListingTitle: listingTitle,
		ListingRef: listingRef, When: occurredAt(ev), Place: h.placeOf(ctx, &info.ListingID)}

	switch ev.Type {
	case propertyrequests.EventCustomerReplied:
		preview, err := h.messagePreview(ctx, ev.AggregateID, payload.MessageID)
		if err != nil {
			return err
		}
		for _, r := range info.Recipients {
			d := base
			d.Name, d.Path, d.Preview = r.Name, pathAgentRequests+ev.AggregateID.String(), preview
			if err := add("request-customer-replied", recipientKey(r), r, d); err != nil {
				return err
			}
		}
		return nil

	case propertyrequests.EventReplied:
		preview, err := h.messagePreview(ctx, ev.AggregateID, payload.MessageID)
		if err != nil {
			return err
		}
		d := base
		d.Preview, d.StatusText = preview, "You have a new reply"
		return h.addRequestUpdate(info, ev, d, add)
	}
	d := base
	d.StatusText = propertyrequests.CustomerStatusLabel(propertyrequests.CustomerStatus(payload.To))
	return h.addRequestUpdate(info, ev, d, add)
}

// addRequestUpdate sends request-update to the request's customer: to their
// request page when they have an account, otherwise (a guest) to the home
// page with the guest wording.
func (h *Handlers) addRequestUpdate(info PropertyRequestInfo, ev outbox.Event, d mail.Data, add func(template, recipient string, to Person, d mail.Data) error) error {
	d.Name = info.Customer.Name
	if info.Customer.UserID != nil {
		d.Path = PathMyRequests + ev.AggregateID.String()
	} else {
		d.Path, d.Guest = PathHome, true
	}
	return add("request-update", "customer", info.Customer, d)
}
