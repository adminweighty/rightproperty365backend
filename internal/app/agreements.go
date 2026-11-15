package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/platform/auth"
)

// leaseAgreements is the signing workflow as the lease module sees it: a
// lease is sent as an agreement whose signers are its landlords and
// tenants (all of order 1: any order), with their initials on every page and
// their signature, name and date in the signature section, where the lease
// renderer left room for them.
type leaseAgreements struct{ svc *agreements.Service }

func partyKey(role string, position int) string { return fmt.Sprintf("%s-%d", role, position) }

func (l leaseAgreements) SendLease(ctx context.Context, p *auth.Principal, in leases.LeaseAgreement) (uuid.UUID, error) {
	prepared := agreements.Prepared{LeaseID: &in.LeaseID, Name: in.Name, Message: in.Message, ExpiresAt: in.ExpiresAt,
		ReminderFrequency: in.ReminderFrequency, OrganisationID: in.OrganisationID, OrganisationName: in.OrganisationName,
		Document: agreements.Document{ObjectKey: in.Document.ObjectKey, SHA256: in.Document.SHA256, Size: in.Document.Size, FileName: in.Reference + ".pdf"}}
	for _, page := range in.Layout.Pages {
		prepared.Pages = append(prepared.Pages, agreements.PageSize{Width: page.Width, Height: page.Height})
	}
	for _, party := range in.Parties {
		prepared.Recipients = append(prepared.Recipients, agreements.PreparedRecipient{Key: partyKey(party.Role, party.Position),
			Label: fmt.Sprintf("%s %d", leases.RoleLabel(party.Role, 1), party.Position), Name: party.Name, Email: party.Email,
			Role: agreements.RoleSigner, Order: 1})
	}
	for _, f := range in.Layout.Fields {
		prepared.Fields = append(prepared.Fields, agreements.PreparedField{RecipientKey: partyKey(f.Role, f.Position), Type: f.Type, Page: f.Page,
			X: f.X, Y: f.Y, W: f.W, H: f.H})
	}
	id, err := l.svc.CreateAndSend(ctx, p, prepared)
	var verr *agreements.ValidationError
	switch {
	case errors.As(err, &verr):
		return uuid.Nil, &leases.ValidationError{Fields: verr.Fields}
	case errors.Is(err, agreements.ErrForbidden):
		return uuid.Nil, leases.ErrForbidden
	}
	return id, err
}

func (l leaseAgreements) CancelLease(ctx context.Context, p *auth.Principal, agreementID uuid.UUID, reason string) error {
	_, err := l.svc.Cancel(ctx, p, agreementID, reason)
	return err
}

func (l leaseAgreements) Events(ctx context.Context, agreementID uuid.UUID) ([]leases.Event, error) {
	events, err := l.svc.RawEvents(ctx, agreementID)
	if err != nil {
		return nil, err
	}
	out := make([]leases.Event, 0, len(events))
	for _, e := range events {
		detail := map[string]any{"agreement_id": agreementID.String()}
		for k, v := range e.Detail {
			detail[k] = v
		}
		if e.IP != "" {
			detail["ip"] = e.IP
		}
		if e.RecipientID != nil {
			detail["recipient_id"] = e.RecipientID.String()
		}
		out = append(out, leases.Event{ID: e.ID, Action: "AGREEMENT_" + e.Action, ActorType: e.ActorType, ActorUserID: e.ActorUserID,
			ActorName: e.ActorName, Detail: detail, At: e.At})
	}
	return out, nil
}

// FollowLeases makes a lease follow its agreement once the agreement ends.
func FollowLeases(svc *leases.Service, logger *slog.Logger) func(ctx context.Context, a agreements.Agreement) {
	return func(ctx context.Context, a agreements.Agreement) {
		if a.LeaseID == nil {
			return
		}
		at, reason := a.UpdatedAt, a.CancelReason
		var final *leases.Document
		if a.Status == agreements.StatusCompleted && a.Final != nil {
			final = &leases.Document{ObjectKey: a.Final.ObjectKey, SHA256: a.Final.SHA256, Size: a.Final.Size, GeneratedAt: at}
		}
		if a.Status == agreements.StatusDeclined {
			for _, r := range a.Recipients {
				if r.Status == agreements.RecipientDeclined {
					reason = r.DeclineReason
				}
			}
		}
		if err := svc.Follow(ctx, *a.LeaseID, a.Status, final, at, reason); err != nil {
			logger.ErrorContext(ctx, "lease did not follow its agreement", slog.String("lease_id", a.LeaseID.String()),
				slog.String("agreement_id", a.ID.String()), slog.String("error", err.Error()))
		}
	}
}
