// Package reports is the administrators' numbers over a date range and per
// market (GET /v1/admin/reports/summary) and the CSV exports of users,
// listings and subscriptions (GET /v1/admin/reports/*.csv).
//
// Every count is of things CREATED in the range ([from, to), UTC), grouped
// by their status (or role) now, per market. Revenue is the sum of the
// payments administrators recorded (subscriptions.payment_amount), by the
// day the payment was received, per currency and market: there is no
// online payment, so this is the only money the platform knows about.
//
// Markets: a listing, request, enquiry, lease, onboarding application and
// subscription has its own market; an account has none, so it is counted in
// the market of its first membership (null when it has none); an agreement
// has none and is counted under null. Administrator accounts are not
// counted as new users.
package reports

import (
	"context"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/platform/auth"
)

// SummaryRoles may read the summary: SUPER_ADMIN, PLATFORM_ADMIN, FINANCE_ADMIN.
func SummaryRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformFinanceAdmin}
}

// PeopleExportRoles may export users and listings (they carry names and
// email addresses): SUPER_ADMIN and PLATFORM_ADMIN.
func PeopleExportRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin} }

// SubscriptionExportRoles may export subscriptions: SUPER_ADMIN,
// FINANCE_ADMIN and PLATFORM_ADMIN.
func SubscriptionExportRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformFinanceAdmin, auth.PlatformAdmin}
}

// Limits.
const (
	DefaultRange       = 30 * 24 * time.Hour
	MaxRange           = 3 * 366 * 24 * time.Hour
	DefaultExportLimit = 10000
	MaxExportLimit     = 50000
	// NoRole groups the new accounts that declared no role.
	NoRole = "NONE"
)

// Range is what a report covers: [From, To) and the markets (nil: all).
type Range struct {
	From, To time.Time
	Markets  []string
}

// Contains reports whether t is in the range.
func (r Range) Contains(t time.Time) bool { return !t.Before(r.From) && t.Before(r.To) }

// HasMarket reports whether a market code passes the market filter. A row
// without a market passes only when every market is reported.
func (r Range) HasMarket(code string) bool {
	if r.Markets == nil {
		return true
	}
	for _, m := range r.Markets {
		if m == code {
			return true
		}
	}
	return false
}

// Count is one group of one market.
type Count struct {
	Group      string
	MarketCode string // "" when the row has no market
	Count      int
}

// Amount is the payments recorded in one currency and market.
type Amount struct {
	CurrencyCode string
	MarketCode   string
	Total        string
	Payments     int
}

// UserRow is one account in the users export.
type UserRow struct {
	ID            uuid.UUID
	Name          string
	Email         string
	Status        string
	EmailVerified bool
	AccountRoles  []string
	PlatformRoles []string
	MarketCodes   []string
	CreatedAt     time.Time
	LastSignInAt  *time.Time
}

// Store computes the reports.
type Store interface {
	NewUsers(ctx context.Context, r Range) ([]Count, error)
	Onboarding(ctx context.Context, r Range) ([]Count, error)
	Listings(ctx context.Context, r Range) ([]Count, error)
	PropertyRequests(ctx context.Context, r Range) ([]Count, error)
	BusinessEnquiries(ctx context.Context, r Range) ([]Count, error)
	Leases(ctx context.Context, r Range) ([]Count, error)
	Agreements(ctx context.Context, r Range) ([]Count, error)
	Subscriptions(ctx context.Context, r Range) ([]Count, error)
	Revenue(ctx context.Context, r Range) ([]Amount, error)

	// The exports call start once with the number of matching rows, then
	// each for at most limit of them, newest first.
	ExportUsers(ctx context.Context, r Range, limit int, start func(total int), each func(UserRow) error) error
	ExportListings(ctx context.Context, r Range, limit int, start func(total int), each func(listings.ReportRow) error) error
	ExportSubscriptions(ctx context.Context, r Range, limit int, start func(total int), each func(subscriptions.Subscription) error) error
}
