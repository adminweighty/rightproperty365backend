package maintenance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// AssignInput is the body of POST .../maintenance/{requestId}/assign.
type AssignInput struct {
	DirectoryProfileID *uuid.UUID `json:"directory_profile_id"`
	Message            *string    `json:"message"`
}

// SearchProviders serves GET .../maintenance/providers.
func (s *Service) SearchProviders(ctx context.Context, p *auth.Principal, bookID uuid.UUID, q, trade string) ([]ProviderOption, error) {
	if _, err := s.access(p, bookID); err != nil {
		return nil, err
	}
	if s.providers == nil {
		return []ProviderOption{}, nil
	}
	list, err := s.providers.Search(ctx, strings.TrimSpace(q), strings.TrimSpace(trade), MaxProviders)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []ProviderOption{}
	}
	return list, nil
}

// EnquiryKey is the idempotency key of the enquiry of a request's n-th
// assignment to a profile: a retry of the same assignment reuses it.
func EnquiryKey(requestID, profileID uuid.UUID, n int) string {
	return fmt.Sprintf("rpm-%s-%s-%d", requestID, profileID, n)
}

// EnquiryMessage is the message of the enquiry sent to a provider: the
// reference, the category, the priority, the property and unit, and the
// manager's own message. Never the title, the description or anything of
// the tenant's.
func EnquiryMessage(r Request, propertyName, unitLabel, marketCode, category, priority, message string) string {
	place := propertyName
	if unitLabel != "" {
		place += ", " + unitLabel
	}
	if marketCode != "" {
		place += " (" + marketCode + ")"
	}
	lines := []string{
		"Maintenance request " + r.Reference,
		"Category: " + category + ". Priority: " + priority + ".",
		"Property: " + place + ".",
	}
	if message = strings.TrimSpace(message); message != "" {
		lines = append(lines, "", message)
	}
	return strings.Join(lines, "\n")
}

// Assign serves POST .../maintenance/{requestId}/assign: one business
// enquiry to the provider (idempotent), then the request is ASSIGNED.
func (s *Service) Assign(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in AssignInput) (AssignResult, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return AssignResult{}, err
	}
	r, err := s.request(ctx, a, id)
	if err != nil {
		return AssignResult{}, err
	}
	if !Assignable(r.Status) {
		return AssignResult{}, &TransitionError{Status: r.Status, Allowed: Transitions(r.Status)}
	}
	f := map[string]string{}
	message := ""
	if in.Message != nil {
		message = text(f, "message", *in.Message, 0, MaxMessage)
	}
	var prov ProviderOption
	switch {
	case in.DirectoryProfileID == nil:
		f["directory_profile_id"] = "is required"
	case s.providers == nil:
		f["directory_profile_id"] = "is not a service provider of the directory"
	default:
		got, ok, err := s.providers.Provider(ctx, *in.DirectoryProfileID)
		switch {
		case err != nil:
			return AssignResult{}, err
		case !ok || got.Kind != ProviderKind:
			f["directory_profile_id"] = "is not a service provider of the directory"
		default:
			prov = got
		}
	}
	if err := invalid(f); err != nil {
		return AssignResult{}, err
	}
	// Already assigned to this provider: nothing new is sent.
	if r.Status == StatusAssigned && r.Provider != nil && r.Provider.ProfileID != nil && *r.Provider.ProfileID == prov.ID && r.EnquiryID != nil {
		view, err := s.oneView(ctx, a, id)
		if err != nil {
			return AssignResult{}, err
		}
		return AssignResult{Request: view, Enquiry: AssignedEnquiry{ID: *r.EnquiryID, Reference: r.EnquiryReference, Replayed: true}}, nil
	}
	if s.enquiries == nil {
		return AssignResult{}, errors.New("maintenance: no business enquiry service")
	}
	props, err := s.tenancies.Properties(ctx, []uuid.UUID{r.PropertyID})
	if err != nil {
		return AssignResult{}, err
	}
	prop := props[r.PropertyID]
	unit := ""
	if r.UnitID != nil {
		u, _ := prop.Unit(*r.UnitID)
		unit = u.Label
	}
	v, err := s.vocab(ctx)
	if err != nil {
		return AssignResult{}, err
	}
	category, priority := label(v.categories, r.Category, r.CategoryLabel).Label, label(v.priorities, r.Priority, r.PriorityLabel).Label
	service := category
	if len([]rune(service)) > 120 {
		service = string([]rune(service)[:120])
	}
	res, err := s.enquiries.Create(ctx, businessenquiries.Input{
		Recipient: &businessenquiries.RecipientRef{Kind: prov.Kind, Slug: prov.Slug}, Service: &service,
		Message:                EnquiryMessage(r, prop.Name, unit, prop.MarketCode, category, priority, message),
		PreferredContactMethod: businessenquiries.ContactEmail, IdempotencyKey: EnquiryKey(r.ID, prov.ID, r.Assignments+1),
		Consent:  &businessenquiries.Consent{PrivacyVersion: s.privacyVersion, Accepted: true},
		Customer: &businessenquiries.Customer{UserID: p.UserID, Name: p.Name, Email: p.Email},
	}, "")
	var ve *businessenquiries.ValidationError
	switch {
	case errors.As(err, &ve):
		out := map[string]string{}
		for k, msg := range ve.Fields {
			switch {
			case k == "message":
				out["message"] = msg
			case strings.HasPrefix(k, "recipient"):
				out["directory_profile_id"] = "is not a service provider of the directory"
			default:
				out["enquiry."+k] = msg
			}
		}
		return AssignResult{}, &ValidationError{Fields: out}
	case errors.Is(err, businessenquiries.ErrRecipientNotFound), errors.Is(err, businessenquiries.ErrRecipientUnavailable):
		return AssignResult{}, &ValidationError{Fields: map[string]string{"directory_profile_id": "is not accepting enquiries"}}
	case errors.Is(err, businessenquiries.ErrIdempotencyConflict):
		// The same assignment was sent before with another message.
		return AssignResult{}, ErrConflict
	case err != nil:
		return AssignResult{}, err
	}

	now := s.clock()
	x := managerActor(p)
	from := r.Status
	next := r
	profileID, enquiryID, by := prov.ID, res.EnquiryID, p.UserID
	next.Provider = &Provider{ProfileID: &profileID, Kind: prov.Kind, Slug: prov.Slug, Name: prov.Name}
	next.EnquiryID, next.EnquiryReference = &enquiryID, res.Reference
	next.Assignments = r.Assignments + 1
	next.AssignedAt, next.AssignedBy, next.AssignedByName = &now, &by, p.Name
	next.UpdatedAt = now
	ch := Change{Expect: map[uuid.UUID]Expect{r.ID: {Status: r.Status, Assignments: r.Assignments}}}
	if from != StatusAssigned {
		move(&next, StatusAssigned, now)
		ev := x.event(r.ID, EventStatusChanged, now)
		ev.FromStatus, ev.ToStatus = from, StatusAssigned
		ch.Events = append(ch.Events, ev)
		ch.Notices = append(ch.Notices, x.notice(OutboxStatusChanged, next, from, now))
	}
	assigned := x.event(r.ID, EventAssigned, now)
	assigned.Details = map[string]any{
		"provider":          map[string]any{"id": prov.ID.String(), "kind": prov.Kind, "slug": prov.Slug, "name": prov.Name},
		"enquiry_reference": res.Reference,
	}
	ch.Events = append(ch.Events, assigned)
	ch.Requests = []Request{next}
	ch.Audit = s.audit(ctx, a, orgaudit.ActionMaintenanceAssigned, r, map[string]any{"directory_profile_id": prov.ID.String(),
		"provider": prov.Name, "enquiry_reference": res.Reference, "enquiry_id": res.EnquiryID.String()})
	if err := s.store.Apply(ctx, ch); err != nil {
		return AssignResult{}, err
	}
	view, err := s.oneView(ctx, a, id)
	if err != nil {
		return AssignResult{}, err
	}
	return AssignResult{Request: view, Enquiry: AssignedEnquiry{ID: res.EnquiryID, Reference: res.Reference, Replayed: res.Replayed}}, nil
}
