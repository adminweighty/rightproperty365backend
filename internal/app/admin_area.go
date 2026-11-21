package app

import (
	"context"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reports"
	"rightproperty365.com/backend/internal/modules/subscriptions"
)

// The administrators' area (API 0.25.0): accounts, subscriptions, reference
// data and reports, connected to the rest of the API.

// marketCurrency returns the currency of a country.
func marketCurrency(store markets.Store) func(ctx context.Context, code string) (string, bool, error) {
	return func(ctx context.Context, code string) (string, bool, error) {
		all, err := store.List(ctx)
		if err != nil {
			return "", false, err
		}
		for _, m := range all {
			if m.Code == code {
				return m.CurrencyCode, true, nil
			}
		}
		return "", false, nil
	}
}

// marketCodes lists the codes of every country.
func marketCodes(store markets.Store) func(ctx context.Context) ([]string, error) {
	return func(ctx context.Context) ([]string, error) {
		all, err := store.List(ctx)
		if err != nil {
			return nil, err
		}
		codes := make([]string, 0, len(all))
		for _, m := range all {
			codes = append(codes, m.Code)
		}
		return codes, nil
	}
}

// AccountSubscriptions lists a person's subscriptions, and their
// organisations', for the account detail.
func AccountSubscriptions(store subscriptions.Store, now func() time.Time) func(ctx context.Context, userID uuid.UUID, orgs []uuid.UUID) ([]accounts.SubscriptionInfo, error) {
	return func(ctx context.Context, userID uuid.UUID, orgs []uuid.UUID) ([]accounts.SubscriptionInfo, error) {
		list, _, err := store.Subscriptions(ctx, subscriptions.Filter{Subscribers: append([]uuid.UUID{userID}, orgs...), Limit: 50})
		if err != nil {
			return nil, err
		}
		at := time.Now()
		if now != nil {
			at = now()
		}
		out := make([]accounts.SubscriptionInfo, 0, len(list))
		for _, x := range list {
			if x.SubscriberKind == subscriptions.KindIndividual && x.SubscriberID != userID {
				continue
			}
			info := accounts.SubscriptionInfo{ID: x.ID, InvoiceNumber: x.InvoiceNumber, PlanName: x.PlanName, Period: x.Period, Status: x.EffectiveStatus(at),
				SubscriberKind: x.SubscriberKind, SubscriberName: x.SubscriberName, Amount: x.Amount, CurrencyCode: x.CurrencyCode,
				StartsAt: x.StartsAt, EndsAt: x.EndsAt, OrganisationID: x.OrganisationID}
			if x.OrganisationID != nil {
				info.OrganisationName = x.SubscriberName
			}
			out = append(out, info)
		}
		return out, nil
	}
}

// SuspensionReasons reads the reason behind an ACCOUNT_SUSPENDED email.
func SuspensionReasons(store accounts.Store) func(ctx context.Context, userID uuid.UUID) (string, error) {
	return func(ctx context.Context, userID uuid.UUID) (string, error) {
		a, err := store.Account(ctx, userID)
		if err != nil {
			return "", err
		}
		return a.SuspensionReason, nil
	}
}

// MemoryActiveListings counts a subscriber's published, unexpired listings
// in the memory listing store (the memory twin of the Postgres count).
func MemoryActiveListings(store *listings.MemoryStore) func(kind string, id uuid.UUID, now time.Time) int {
	return func(kind string, id uuid.UUID, now time.Time) int {
		n := 0
		for _, r := range store.ReportRows(now) {
			// A paused listing keeps its place (API 0.48.0).
			if r.Status != listings.StatusPublished && r.Status != listings.StatusPaused {
				continue
			}
			switch {
			case kind == subscriptions.KindOrganisation && r.OrganisationID != nil && *r.OrganisationID == id,
				kind == subscriptions.KindIndividual && ((r.OrganisationID == nil && r.SellerUserID != nil && *r.SellerUserID == id) ||
					(r.AgentUserID != nil && *r.AgentUserID == id)):
				n++
			}
		}
		return n
	}
}

// MemoryReportStores are the memory stores the reports read.
type MemoryReportStores struct {
	Identity      *identity.MemoryStore
	Onboarding    *onboarding.MemoryStore
	Listings      *listings.MemoryStore
	Requests      *propertyrequests.MemoryStore
	Enquiries     *businessenquiries.MemoryStore
	Leases        *leases.MemoryStore
	Agreements    *agreements.MemoryStore
	Subscriptions *subscriptions.MemoryStore
	Markets       markets.Store
	Now           func() time.Time
}

// MemoryReports builds the in-memory reports store.
func MemoryReports(m MemoryReportStores) *reports.MemoryStore {
	ctx := context.Background()
	codeOf := func(id uuid.UUID) string {
		if m.Markets == nil {
			return ""
		}
		all, err := m.Markets.List(ctx)
		if err != nil {
			return ""
		}
		for _, mk := range all {
			if mk.ID == id {
				return mk.Code
			}
		}
		return ""
	}
	src := reports.MemorySources{Now: m.Now}
	if m.Identity != nil {
		src.Users = func() []reports.UserRow {
			var out []reports.UserRow
			for _, a := range m.Identity.AdminAccounts() {
				row := reports.UserRow{ID: a.ID, Name: a.Name, Email: a.Email, Status: a.Status, EmailVerified: a.EmailVerifiedAt != nil,
					CreatedAt: a.CreatedAt, LastSignInAt: a.LastSignInAt}
				for _, r := range a.AccountRoles {
					row.AccountRoles = append(row.AccountRoles, r.Role)
				}
				for _, r := range a.PlatformRoles {
					row.PlatformRoles = append(row.PlatformRoles, r.Role)
				}
				for _, ms := range a.AllMemberships {
					if ms.Status != identity.MembershipRemoved {
						row.MarketCodes = append(row.MarketCodes, ms.MarketCode)
					}
				}
				out = append(out, row)
			}
			return out
		}
	}
	if m.Onboarding != nil {
		src.Onboarding = func() []reports.Row {
			var out []reports.Row
			for _, status := range onboarding.Statuses() {
				apps, _, _, err := m.Onboarding.List(ctx, onboarding.ListFilter{Status: status, Limit: 1 << 20})
				if err != nil {
					continue
				}
				for _, a := range apps {
					out = append(out, reports.Row{Group: a.Status, MarketCode: a.MarketCode, At: a.CreatedAt})
				}
			}
			return out
		}
	}
	if m.Listings != nil {
		src.Listings = m.Listings.ReportRows
	}
	if m.Requests != nil {
		src.Requests = func() []reports.Row {
			var out []reports.Row
			for _, r := range m.Requests.StoredRequests() {
				group := r.Stage
				if r.Status == propertyrequests.StatusRoutingException {
					group = propertyrequests.StatusRoutingException
				}
				out = append(out, reports.Row{Group: group, MarketCode: codeOf(r.MarketID), At: r.CreatedAt})
			}
			return out
		}
	}
	if m.Enquiries != nil {
		src.Enquiries = func() []reports.Row {
			var out []reports.Row
			for _, e := range m.Enquiries.ReportRows() {
				out = append(out, reports.Row{Group: e.Status, MarketCode: codeOf(e.MarketID), At: e.CreatedAt})
			}
			return out
		}
	}
	if m.Leases != nil {
		src.Leases = func() []reports.Row {
			list, _, err := m.Leases.Leases(ctx, leases.LeaseFilter{All: true, Limit: 1 << 20})
			if err != nil {
				return nil
			}
			out := make([]reports.Row, 0, len(list))
			for _, l := range list {
				out = append(out, reports.Row{Group: l.Status, MarketCode: l.MarketCode, At: l.CreatedAt})
			}
			return out
		}
	}
	if m.Agreements != nil {
		src.Agreements = func() []reports.Row {
			list, err := m.Agreements.Agreements(ctx, agreements.Filter{All: true})
			if err != nil {
				return nil
			}
			out := make([]reports.Row, 0, len(list))
			for _, a := range list {
				out = append(out, reports.Row{Group: a.Status, At: a.CreatedAt})
			}
			return out
		}
	}
	if m.Subscriptions != nil {
		src.Subscriptions = func() []subscriptions.Subscription {
			list, _, err := m.Subscriptions.Subscriptions(ctx, subscriptions.Filter{})
			if err != nil {
				return nil
			}
			return list
		}
	}
	return reports.NewMemoryStore(src)
}
