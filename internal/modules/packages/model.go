// Package packages holds the listing packages and quotas (API 0.37.0,
// FR-008, BR/FR-007).
//
// A package belongs to one market (Starter, Business, Professional ...) and
// allows a number of active published listings (ListingLimit), with a price
// per billing term in the market's currency. A listing account, an
// organisation or a direct owner (a landlord or private seller without an
// organisation), holds at most one package per market on any date
// (Assignment, from-to dated, source ADMIN or INVOICE). Without an
// assignment the market's default package applies; a market without a
// default is not limited.
//
// The quota is enforced where a listing becomes publicly live, which today
// is only a reviewer's approval of a NEW review (a first publication or a
// relist): EnforceTx (Postgres) takes a transaction-scoped advisory lock on
// the account and market, resolves the package and counts the active
// listings in the same transaction as the status change, so simultaneous
// approvals can never exceed the limit. An active listing is PUBLISHED with
// its publication period not over; PAUSED listings do not count.
//
// Grace rule: when the limit drops below the number of active listings (a
// downgrade, or an assignment ending), the listings already live stay live
// until they expire or are archived; only new go-lives are refused.
package packages

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// Account kinds: an organisation, or a direct owner (a user).
const (
	KindOrganisation = "ORGANISATION"
	KindDirectOwner  = "DIRECT_OWNER"
)

// Account is a listing account: who a package is held by and counted
// against.
type Account struct {
	Kind string
	ID   uuid.UUID
}

// AccountOf returns the account a listing belongs to: its organisation, else
// its direct owner. ok is false for a listing with neither.
func AccountOf(organisationID, sellerUserID *uuid.UUID) (Account, bool) {
	switch {
	case organisationID != nil:
		return Account{Kind: KindOrganisation, ID: *organisationID}, true
	case sellerUserID != nil:
		return Account{Kind: KindDirectOwner, ID: *sellerUserID}, true
	}
	return Account{}, false
}

// key is the account and market as one string (the advisory lock's key).
func (a Account) key(market string) string { return a.Kind + ":" + a.ID.String() + ":" + market }

// Billing terms.
const (
	TermMonthly   = "MONTHLY"
	TermQuarterly = "QUARTERLY"
	TermAnnual    = "ANNUAL"
)

// Terms returns the billing terms in order.
func Terms() []string { return []string{TermMonthly, TermQuarterly, TermAnnual} }

var termLabels = map[string]string{TermMonthly: "Monthly", TermQuarterly: "Quarterly", TermAnnual: "Annual"}
var termMonths = map[string]int{TermMonthly: 1, TermQuarterly: 3, TermAnnual: 12}

// TermLabel is how a billing term is shown.
func TermLabel(term string) string { return termLabels[term] }

// Assignment sources.
const (
	SourceAdmin   = "ADMIN"
	SourceInvoice = "INVOICE"
)

// What decided an account's package in a market.
const (
	BasisAssigned = "ASSIGNED"
	BasisDefault  = "DEFAULT"
	BasisNone     = "NONE"
)

// Assignment statuses (computed).
const (
	AssignmentScheduled = "SCHEDULED"
	AssignmentCurrent   = "CURRENT"
	AssignmentEnded     = "ENDED"
	AssignmentCancelled = "CANCELLED"
)

// Limits.
const (
	MaxNameRunes        = 80
	MaxDescriptionRunes = 1000
	MaxNoteRunes        = 500
	MaxListingLimit     = 100000
	MaxSort             = 10000
	DefaultSort         = 100
	MaxAssignmentsShown = 200
	// DateLayout is the layout of starts_on and ends_on.
	DateLayout = "2006-01-02"
)

// Error codes of this module's endpoints (LISTING_QUOTA_REACHED is answered
// by listing management).
const (
	CodeMarketNotFound       = "MARKET_NOT_FOUND"
	CodePackageNotFound      = "PACKAGE_NOT_FOUND"
	CodeAssignmentNotFound   = "PACKAGE_ASSIGNMENT_NOT_FOUND"
	CodeAccountNotFound      = "PACKAGE_ACCOUNT_NOT_FOUND"
	CodeOrganisationNotFound = "ORGANISATION_NOT_FOUND"
	CodeUserNotFound         = "USER_NOT_FOUND"
	CodeCodeTaken            = "PACKAGE_CODE_TAKEN"
	CodeInUse                = "PACKAGE_IN_USE"
	CodeIsDefault            = "PACKAGE_IS_DEFAULT"
	CodeInactive             = "PACKAGE_INACTIVE"
	CodeMarketForbidden      = "PACKAGE_MARKET_FORBIDDEN"
	CodeQuotaReached         = "LISTING_QUOTA_REACHED"
)

// Errors, mapped to HTTP by the handler.
var (
	ErrMarketNotFound     = errors.New("market not found")
	ErrPackageNotFound    = errors.New("package not found")
	ErrAssignmentNotFound = errors.New("no current or scheduled assignment")
	ErrAccountNotFound    = errors.New("listing account not found")
	ErrCodeTaken          = errors.New("the package code is taken in this market")
	ErrIsDefault          = errors.New("the package is the market's default")
	ErrInactive           = errors.New("the package is not active")
	ErrMarketForbidden    = errors.New("the market is outside the administrator's markets")
	ErrForbidden          = errors.New("not allowed")
)

// InUseError is a package that assignments still name (409 PACKAGE_IN_USE).
type InUseError struct{ Assignments int }

func (e *InUseError) Error() string {
	return fmt.Sprintf("the package is named by %d assignments", e.Assignments)
}

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// Price is a package's price for one billing term. Amount is a decimal with
// exactly the currency's minor-unit decimals.
type Price struct {
	Term     string
	Amount   string
	Currency string
	// Sample marks a development seed price (never an approved one).
	Sample bool
}

// Package is one listing package of a market.
type Package struct {
	ID           uuid.UUID
	MarketCode   string
	Code         string
	Name         string
	Description  string
	ListingLimit int
	Active       bool
	Default      bool
	Sort         int
	// Prices are ordered by term (Terms).
	Prices    []Price
	CreatedBy *uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Assignment is the package an account holds in a market from StartsOn to
// EndsOn (UTC dates at midnight, inclusive; EndsOn nil is open-ended).
type Assignment struct {
	ID             uuid.UUID
	Account        Account
	MarketCode     string
	PackageID      uuid.UUID
	StartsOn       time.Time
	EndsOn         *time.Time
	Source         string
	Note           string
	AssignedBy     *uuid.UUID
	AssignedByName string
	CreatedAt      time.Time
	CancelledAt    *time.Time
	CancelledBy    *uuid.UUID
}

// InForce reports whether the assignment applies on the UTC date today.
func (a Assignment) InForce(today time.Time) bool {
	return a.CancelledAt == nil && !a.StartsOn.After(today) && (a.EndsOn == nil || !a.EndsOn.Before(today))
}

// Status is the assignment's status on the UTC date today.
func (a Assignment) Status(today time.Time) string {
	switch {
	case a.CancelledAt != nil:
		return AssignmentCancelled
	case a.StartsOn.After(today):
		return AssignmentScheduled
	case a.EndsOn != nil && a.EndsOn.Before(today):
		return AssignmentEnded
	}
	return AssignmentCurrent
}

// Resolved is the package that applies to an account in a market on a date.
type Resolved struct {
	Basis string
	// Package is nil for BasisNone (no limit).
	Package *Package
	// Assignment is the current assignment (BasisAssigned only).
	Assignment *Assignment
	// Scheduled is the next assignment that starts later, if any.
	Scheduled *Assignment
	// ScheduledPackage is the package of Scheduled.
	ScheduledPackage *Package
}

// QuotaError is a go-live refused because the account's active listings
// already reach its package's limit (409 LISTING_QUOTA_REACHED).
type QuotaError struct {
	Limit      int
	Used       int
	Package    Package
	MarketCode string
	Account    Account
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("listing quota reached: %d of %d on %s in %s", e.Used, e.Limit, e.Package.Code, e.MarketCode)
}

// Day returns the UTC date of t, at midnight.
func Day(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// PackageFilter selects packages.
type PackageFilter struct {
	// Markets limits the list to these markets (nil: every market).
	Markets []string
	// Market narrows to one market ("" for all).
	Market          string
	IncludeInactive bool
}

// Uses counts the assignments naming a package.
type Uses struct {
	// Total counts every assignment (cancelled and past included).
	Total int
	// Current counts the assignments in force today.
	Current int
}

// AssignResult is what an assignment replaced.
type AssignResult struct {
	// Replaced are the assignments cancelled or ended by it, as they were.
	Replaced []Assignment
}

// Store persists packages and assignments.
type Store interface {
	// Packages returns packages ordered by market, sort and name.
	Packages(ctx context.Context, f PackageFilter) ([]Package, error)
	// Package returns one, or ErrPackageNotFound.
	Package(ctx context.Context, id uuid.UUID) (Package, error)
	// CreatePackage stores p (moving the market's default to it when
	// p.Default) with its audit record. ErrCodeTaken for a duplicate code.
	CreatePackage(ctx context.Context, p Package, audit adminaudit.Event) error
	// ChangePackage locks the package, calls change with a copy and saves it
	// (prices replaced; the market's default moved to it when it becomes
	// the default) with the audit record change returns.
	ChangePackage(ctx context.Context, id uuid.UUID, change func(p *Package) (adminaudit.Event, error)) (Package, error)
	// DeletePackage locks the package, calls check with it and the number of
	// assignments naming it, and deletes it with the audit record.
	DeletePackage(ctx context.Context, id uuid.UUID, check func(p Package, assignments int) (adminaudit.Event, error)) error
	// PackageUses counts the assignments naming each package.
	PackageUses(ctx context.Context, ids []uuid.UUID, today time.Time) (map[uuid.UUID]Uses, error)

	// Resolve returns the package that applies to the account in the market
	// on the UTC date today, and the next scheduled assignment.
	Resolve(ctx context.Context, acct Account, market string, today time.Time) (Resolved, error)
	// Assignments returns the account's assignments, newest first.
	Assignments(ctx context.Context, acct Account) ([]Assignment, error)
	// Assign stores a, under the account and market's lock, after cancelling
	// the assignments starting on or after a.StartsOn and ending the one in
	// force then the day before (cancelled if it starts that day), and
	// writes the audit record audit builds from what it replaced.
	Assign(ctx context.Context, a Assignment, audit func(replaced []Assignment) adminaudit.Event) (AssignResult, error)
	// EndAssignments ends the account's current assignment in the market
	// (yesterday, or cancelled when it started today) and cancels the
	// scheduled ones, with the audit record. ErrAssignmentNotFound when
	// there is nothing to end.
	EndAssignments(ctx context.Context, acct Account, market string, today time.Time, by *uuid.UUID, at time.Time,
		audit func(ended []Assignment) adminaudit.Event) error

	// LiveListings returns the expiries of the account's active listings in
	// the market (PUBLISHED, expires_at after now), earliest first.
	LiveListings(ctx context.Context, acct Account, market string, now time.Time) ([]time.Time, error)
	// AccountMarkets returns the markets the account has listings in (any
	// status), current or scheduled assignments in, and (an organisation)
	// its home market.
	AccountMarkets(ctx context.Context, acct Account, today time.Time) ([]string, error)
	// AccountName returns the organisation's or the (not deleted) user's
	// name, or ErrAccountNotFound.
	AccountName(ctx context.Context, acct Account) (string, error)
}
