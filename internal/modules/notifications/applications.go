package notifications

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/platform/mail"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// Where the application emails send the applicant and the receiving side.
const (
	PathMyApplications    = "/account/applications"
	PathApplicationsInbox = "/agent/applications"
)

// SetApplicationSource supplies the data behind application events (the
// applications service's Notice). Without it those events are dead-lettered.
func (h *Handlers) SetApplicationSource(source func(ctx context.Context, id uuid.UUID) (applications.Notice, error)) {
	h.application = source
}

// applicationNotes builds the emails of one application event:
//
//   - APPLICATION_SUBMITTED: to the applicant (with the reference) and to
//     the receiving side, the same audience as a property request (the
//     listing's assigned agent, else the organisation's owners and
//     managers, or the direct owner);
//   - APPLICATION_INFO_REQUESTED: to the applicant, with the request;
//   - APPLICATION_STATUS_CHANGED: to the applicant when shortlisted,
//     accepted or rejected (UNDER_REVIEW sends nothing);
//   - APPLICATION_WITHDRAWN: nothing (kept for later push notifications).
//
// No email carries an answer, a document or the applicant's contact details.
func (h *Handlers) applicationNotes(ctx context.Context, ev outbox.Event, add func(template, recipient string, to Person, d mail.Data) error) error {
	if ev.Type == applications.EventTypeWithdrawn {
		return nil
	}
	var payload applications.EventPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		return outbox.Permanent(fmt.Errorf("notifications: unreadable %s payload: %w", ev.Type, err))
	}
	if ev.Type == applications.EventTypeStatusChanged &&
		payload.To != applications.StatusShortlisted && payload.To != applications.StatusAccepted && payload.To != applications.StatusRejected {
		return nil
	}
	if h.application == nil {
		return outbox.Permanent(fmt.Errorf("notifications: no application source for event type %q", ev.Type))
	}
	n, err := h.application(ctx, ev.AggregateID)
	if errors.Is(err, applications.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	d := mail.Data{Reference: n.Reference, ListingTitle: n.ListingTitle, ListingRef: n.ListingReference, When: occurredAt(ev),
		Place: h.placeOf(ctx, &n.ListingID)}
	applicant := Person{UserID: n.ApplicantUserID, Name: n.ApplicantName, Email: n.ApplicantEmail}
	toApplicant := func(template, status string) error {
		if n.ApplicantUserID == nil {
			return nil
		}
		d := d
		d.Name, d.Path, d.StatusText = n.ApplicantName, PathMyApplications, applications.StatusLabel(status)
		return add(template, "applicant", applicant, d)
	}
	switch ev.Type {
	case applications.EventTypeSubmitted:
		d.Edit = payload.Resubmission
		if err := toApplicant("application-submitted", applications.StatusSubmitted); err != nil {
			return err
		}
		var people []Person
		switch {
		case n.OrganisationID != nil:
			people, err = h.source.Recipients(ctx, n.AgentUserID, n.OrganisationID)
		case n.OwnerUserID != nil:
			people, err = h.source.Recipients(ctx, n.OwnerUserID, nil)
		}
		if err != nil {
			return err
		}
		for _, r := range people {
			d := d
			d.Name, d.Path = r.Name, PathApplicationsInbox
			if err := add("application-new", recipientKey(r), r, d); err != nil {
				return err
			}
		}
	case applications.EventTypeInfoRequested:
		d.Message = mail.SafePreview(n.InfoMessage, 500)
		return toApplicant("application-more-info", applications.StatusMoreInfo)
	case applications.EventTypeStatusChanged:
		return toApplicant("application-status-changed", payload.To)
	}
	return nil
}
