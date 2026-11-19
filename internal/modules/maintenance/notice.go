package maintenance

import (
	"context"
	"errors"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/tenancies"
)

// Recipient is a tenant who is told about a request: an account whose
// verified email is a tenant contact's of the request's tenancy.
type Recipient struct {
	UserID uuid.UUID
	Name   string
	Email  string
}

// NotificationInfo is what the notifications about a request need. It
// holds safe text only (the reference and labels), and who to tell.
type NotificationInfo struct {
	RequestID     uuid.UUID
	Reference     string
	Status        string
	CategoryLabel string
	// TenancyID is the request's tenancy (nil when none): the tenants' link.
	TenancyID *uuid.UUID
	// OrganisationID is the book's organisation (its owners and managers are
	// told); OwnerUserID the direct owner of a personal book.
	OrganisationID *uuid.UUID
	OwnerUserID    *uuid.UUID
	// Tenants are the tenancy's tenants with a verified account (none when
	// the request has no tenancy).
	Tenants []Recipient
}

// TenantAccounts finds the verified, active accounts of a tenancy's tenant
// contacts (the app wires the CRM and identity stores).
type TenantAccounts func(ctx context.Context, t tenancies.Tenancy) ([]Recipient, error)

// Notices returns the source of the notifications module: the data behind
// a request's outbox events, read when the event is handled.
func Notices(store Store, tenancy func(ctx context.Context, id uuid.UUID) (tenancies.Tenancy, error),
	tenants TenantAccounts) func(ctx context.Context, id uuid.UUID) (NotificationInfo, error) {
	return func(ctx context.Context, id uuid.UUID) (NotificationInfo, error) {
		r, err := store.Request(ctx, id)
		if err != nil {
			return NotificationInfo{}, err
		}
		out := NotificationInfo{RequestID: r.ID, Reference: r.Reference, Status: r.Status, CategoryLabel: r.CategoryLabel,
			TenancyID: r.TenancyID, OrganisationID: r.Book.OrganisationID, OwnerUserID: r.Book.UserID}
		if r.TenancyID != nil && tenants != nil && tenancy != nil {
			t, err := tenancy(ctx, *r.TenancyID)
			switch {
			case err == nil:
				if out.Tenants, err = tenants(ctx, t); err != nil {
					return NotificationInfo{}, err
				}
			case !errors.Is(err, tenancies.ErrTenancyNotFound):
				return NotificationInfo{}, err
			}
		}
		return out, nil
	}
}
