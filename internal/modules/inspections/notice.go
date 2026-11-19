package inspections

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
)

// Recipient is a tenant who is told about an inspection: an account whose
// verified email is a tenant contact's of the inspection's tenancy.
type Recipient struct {
	UserID uuid.UUID
	Name   string
	Email  string
}

// NotificationInfo is what the notifications about an inspection need. It
// holds safe text only (the reference, the type and the time), and who to
// tell: the tenants only.
type NotificationInfo struct {
	InspectionID uuid.UUID
	Reference    string
	Type         string
	Status       string
	ScheduledFor time.Time
	// Zone is the IANA time zone of the property's market (the time in the
	// email is written in it; "" means UTC).
	Zone string
	// TenancyID is the inspection's tenancy (nil when none): the tenants' link.
	TenancyID *uuid.UUID
	// Tenants are the tenancy's tenants with a verified account (none when
	// the inspection has no tenancy, or the tenancy is a DRAFT).
	Tenants []Recipient
}

// TenantAccounts finds the verified, active accounts of a tenancy's tenant
// contacts (the app wires the CRM and identity stores).
type TenantAccounts func(ctx context.Context, t tenancies.Tenancy) ([]Recipient, error)

// Notices returns the source of the notifications module: the data behind
// an inspection's outbox events, read when the event is handled. zone
// returns a market's time zone.
func Notices(store Store, tenancyStore tenancies.Store, tenants TenantAccounts,
	zone func(ctx context.Context, market string) (string, error)) func(ctx context.Context, id uuid.UUID) (NotificationInfo, error) {
	return func(ctx context.Context, id uuid.UUID) (NotificationInfo, error) {
		in, err := store.Inspection(ctx, id)
		if err != nil {
			return NotificationInfo{}, err
		}
		out := NotificationInfo{InspectionID: in.ID, Reference: in.Reference, Type: in.Type, Status: in.Status, ScheduledFor: in.ScheduledFor,
			TenancyID: in.TenancyID}
		if tenancyStore != nil {
			props, err := tenancyStore.PropertiesByIDs(ctx, []uuid.UUID{in.PropertyID})
			if err != nil {
				return NotificationInfo{}, err
			}
			if len(props) == 1 && zone != nil {
				if out.Zone, err = zone(ctx, props[0].MarketCode); err != nil {
					return NotificationInfo{}, err
				}
			}
		}
		if in.TenancyID == nil || tenants == nil || tenancyStore == nil {
			return out, nil
		}
		t, err := tenancyStore.Tenancy(ctx, *in.TenancyID)
		switch {
		case errors.Is(err, tenancies.ErrTenancyNotFound):
			return out, nil
		case err != nil:
			return NotificationInfo{}, err
		case t.Status == tenancies.StatusDraft || !t.Book.Same(in.Book):
			return out, nil // a draft is not shown to its tenants yet
		}
		if out.Tenants, err = tenants(ctx, t); err != nil {
			return NotificationInfo{}, err
		}
		return out, nil
	}
}
