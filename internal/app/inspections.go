package app

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/storage"
)

// InspectionsDeps is what the inspections service is built from.
type InspectionsDeps struct {
	Store     inspections.Store
	Tenancies *tenancies.Service
	Reference *reference.Service
	Teams     teams.Store
	Files     storage.Storage
	Logger    *slog.Logger
	Now       func() time.Time
}

// InspectionsService builds inspections (API 0.36.3): the books and
// tenancies of the tenancies service, checklist templates from reference
// data, inspectors among the organisation's active members, photos in the
// private file storage.
func InspectionsService(d InspectionsDeps) *inspections.Service {
	deps := inspections.Deps{Store: d.Store, Tenancies: d.Tenancies, Files: d.Files, Logger: d.Logger, Now: d.Now,
		Templates: func(ctx context.Context) ([]inspections.TemplateRow, error) {
			items, err := d.Reference.Set(ctx, reference.SetInspectionChecklistTemplates, "")
			if err != nil {
				return nil, err
			}
			out := make([]inspections.TemplateRow, 0, len(items))
			for _, it := range items {
				r := inspections.TemplateRow{Code: it.Code, Label: it.Label}
				if it.ParentCode != nil {
					r.ParentCode = *it.ParentCode
				}
				out = append(out, r)
			}
			return out, nil
		}}
	if d.Teams != nil {
		deps.Members = func(ctx context.Context, organisationID uuid.UUID) ([]inspections.Person, error) {
			list, err := d.Teams.Members(ctx, organisationID)
			if err != nil {
				return nil, err
			}
			out := make([]inspections.Person, 0, len(list))
			for _, m := range list {
				if m.EffectiveStatus() == teams.MemberActive {
					out = append(out, inspections.Person{UserID: m.UserID, Name: m.Name})
				}
			}
			return out, nil
		}
	}
	return inspections.NewService(deps)
}

// InspectionTenants finds the accounts to tell about a tenancy's
// inspections: the matching of GET /v1/me/tenancies (MaintenanceTenants).
func InspectionTenants(contacts crm.Store, identityStore identity.Store) inspections.TenantAccounts {
	find := MaintenanceTenants(contacts, identityStore)
	return func(ctx context.Context, t tenancies.Tenancy) ([]inspections.Recipient, error) {
		list, err := find(ctx, t)
		if err != nil {
			return nil, err
		}
		out := make([]inspections.Recipient, 0, len(list))
		for _, r := range list {
			out = append(out, inspections.Recipient{UserID: r.UserID, Name: r.Name, Email: r.Email})
		}
		return out, nil
	}
}

// InspectionNotices is the notifications module's source of inspection
// events: the inspection, its market's time zone and the tenants to tell.
func InspectionNotices(store inspections.Store, tenancyStore tenancies.Store, contacts crm.Store, identityStore identity.Store,
	marketStore markets.Store) func(ctx context.Context, id uuid.UUID) (inspections.NotificationInfo, error) {
	zone := func(ctx context.Context, market string) (string, error) {
		if marketStore == nil {
			return "", nil
		}
		all, err := marketStore.List(ctx)
		if err != nil {
			return "", err
		}
		for _, m := range all {
			if strings.EqualFold(m.Code, market) {
				return m.Timezone, nil
			}
		}
		return "", nil
	}
	return inspections.Notices(store, tenancyStore, InspectionTenants(contacts, identityStore), zone)
}
