package packages

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/money"
)

// MarketView is a market as the package views show it.
type MarketView struct {
	Code      string `json:"code"`
	Name      string `json:"name"`
	Currency  string `json:"currency"`
	MinorUnit int    `json:"minor_unit"`
}

// BillingTermView is one billing term.
type BillingTermView struct {
	Code   string `json:"code"`
	Label  string `json:"label"`
	Months int    `json:"months"`
}

// BillingTermViews returns every billing term, in order.
func BillingTermViews() []BillingTermView {
	out := []BillingTermView{}
	for _, t := range Terms() {
		out = append(out, BillingTermView{Code: t, Label: termLabels[t], Months: termMonths[t]})
	}
	return out
}

// PriceView is one price.
type PriceView struct {
	BillingTerm      string `json:"billing_term"`
	BillingTermLabel string `json:"billing_term_label"`
	Amount           string `json:"amount"`
	Currency         string `json:"currency"`
	MinorUnit        int    `json:"minor_unit"`
	Sample           bool   `json:"sample"`
}

func priceViews(prices []Price) []PriceView {
	out := []PriceView{}
	for _, p := range prices {
		out = append(out, PriceView{BillingTerm: p.Term, BillingTermLabel: termLabels[p.Term], Amount: money.Normalise(p.Amount, p.Currency),
			Currency: p.Currency, MinorUnit: money.MinorUnit(p.Currency), Sample: p.Sample})
	}
	return out
}

// PackageRef names a package.
type PackageRef struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"`
	Name         string    `json:"name"`
	ListingLimit int       `json:"listing_limit"`
	Active       bool      `json:"active"`
}

// RefOf returns the reference of a package.
func RefOf(p Package) PackageRef {
	return PackageRef{ID: p.ID, Code: p.Code, Name: p.Name, ListingLimit: p.ListingLimit, Active: p.Active}
}

func refPtr(p *Package) *PackageRef {
	if p == nil {
		return nil
	}
	r := RefOf(*p)
	return &r
}

// Can says what the administrator may do with a package.
type Can struct {
	Edit   bool `json:"edit"`
	Delete bool `json:"delete"`
}

// PackageView is a package as administrators see it.
type PackageView struct {
	ID                 uuid.UUID   `json:"id"`
	Code               string      `json:"code"`
	Name               string      `json:"name"`
	Market             MarketView  `json:"market"`
	ListingLimit       int         `json:"listing_limit"`
	Description        string      `json:"description"`
	Active             bool        `json:"active"`
	IsDefault          bool        `json:"is_default"`
	Sort               int         `json:"sort"`
	Prices             []PriceView `json:"prices"`
	CurrentAssignments int         `json:"current_assignments"`
	CreatedAt          time.Time   `json:"created_at"`
	UpdatedAt          time.Time   `json:"updated_at"`
	Can                Can         `json:"can"`
}

// PublicPackageView is an active package on the pricing page.
type PublicPackageView struct {
	Code         string      `json:"code"`
	Name         string      `json:"name"`
	Description  string      `json:"description"`
	ListingLimit int         `json:"listing_limit"`
	IsDefault    bool        `json:"is_default"`
	Sort         int         `json:"sort"`
	Prices       []PriceView `json:"prices"`
}

// PublicList is GET /v1/markets/{code}/packages.
type PublicList struct {
	Market       MarketView          `json:"market"`
	BillingTerms []BillingTermView   `json:"billing_terms"`
	Items        []PublicPackageView `json:"items"`
}

// AdminList is GET /v1/admin/packages.
type AdminList struct {
	Items []PackageView `json:"items"`
	// Markets are the markets the caller may manage (nil: every market).
	Markets []string `json:"markets"`
}

// AccountRef names a listing account.
type AccountRef struct {
	Kind string    `json:"kind"`
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// PersonRef names a person.
type PersonRef struct {
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
}

// AssignmentView is an assignment.
type AssignmentView struct {
	ID          uuid.UUID  `json:"id"`
	Account     AccountRef `json:"account"`
	MarketCode  string     `json:"market_code"`
	Package     PackageRef `json:"package"`
	StartsOn    string     `json:"starts_on"`
	EndsOn      *string    `json:"ends_on"`
	Source      string     `json:"source"`
	Note        string     `json:"note"`
	Status      string     `json:"status"`
	AssignedBy  PersonRef  `json:"assigned_by"`
	CreatedAt   time.Time  `json:"created_at"`
	CancelledAt *time.Time `json:"cancelled_at"`
	// SubscriptionID is the paid subscription of an INVOICE assignment.
	SubscriptionID *uuid.UUID `json:"subscription_id"`
}

func dateString(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.Format(DateLayout)
	return &s
}

// AssignmentBrief is the current assignment in a usage view.
type AssignmentBrief struct {
	ID       uuid.UUID `json:"id"`
	StartsOn string    `json:"starts_on"`
	EndsOn   *string   `json:"ends_on"`
	Source   string    `json:"source"`
}

// ScheduledBrief is the next assignment in a usage view.
type ScheduledBrief struct {
	ID       uuid.UUID  `json:"id"`
	Package  PackageRef `json:"package"`
	StartsOn string     `json:"starts_on"`
	EndsOn   *string    `json:"ends_on"`
	Source   string     `json:"source"`
}

// MarketUsage is an account's package and active listings in one market.
type MarketUsage struct {
	Market     MarketView       `json:"market"`
	Package    *PackageRef      `json:"package"`
	Basis      string           `json:"basis"`
	Assignment *AssignmentBrief `json:"assignment"`
	Scheduled  *ScheduledBrief  `json:"scheduled"`
	Limit      *int             `json:"limit"`
	Used       int              `json:"used"`
	Remaining  *int             `json:"remaining"`
	AtLimit    bool             `json:"at_limit"`
	OverLimit  bool             `json:"over_limit"`
	OverBy     int              `json:"over_by"`
	GraceUntil *time.Time       `json:"grace_until"`
	Message    string           `json:"message"`
}

// usageOf builds the usage view from the resolved package and the expiries
// of the active listings (earliest first).
func usageOf(market MarketView, r Resolved, live []time.Time) MarketUsage {
	u := MarketUsage{Market: market, Package: refPtr(r.Package), Basis: r.Basis, Used: len(live)}
	if a := r.Assignment; a != nil {
		u.Assignment = &AssignmentBrief{ID: a.ID, StartsOn: a.StartsOn.Format(DateLayout), EndsOn: dateString(a.EndsOn), Source: a.Source}
	}
	if a := r.Scheduled; a != nil && r.ScheduledPackage != nil {
		u.Scheduled = &ScheduledBrief{ID: a.ID, Package: RefOf(*r.ScheduledPackage), StartsOn: a.StartsOn.Format(DateLayout),
			EndsOn: dateString(a.EndsOn), Source: a.Source}
	}
	if r.Package == nil {
		u.Basis = BasisNone
		u.Message = "No listing limit applies in this market."
		return u
	}
	limit := r.Package.ListingLimit
	remaining := max(limit-u.Used, 0)
	u.Limit, u.Remaining = &limit, &remaining
	u.AtLimit, u.OverLimit, u.OverBy = u.Used >= limit, u.Used > limit, max(u.Used-limit, 0)
	name := r.Package.Name
	switch {
	case u.OverLimit:
		until := live[u.OverBy-1]
		u.GraceUntil = &until
		u.Message = fmt.Sprintf("%d active listings on the %s package, which allows %d. The listings already live stay live until they expire or are archived; "+
			"new ones can be approved only when fewer than %d are live. Back within the limit by %s if nothing changes.",
			u.Used, name, limit, limit, until.Format(DateLayout))
	case u.AtLimit:
		u.Message = fmt.Sprintf("%d of %d active listings used on the %s package. New listings can be approved when one is archived or expires.",
			u.Used, limit, name)
	default:
		u.Message = fmt.Sprintf("%d of %d active listings used on the %s package.", u.Used, limit, name)
	}
	return u
}

// Warning is the quota_warning of a listing view: set when the account is
// at its limit and the listing is not live. reviewer chooses the wording of
// the administrators' review screen.
func Warning(u *MarketUsage, live, reviewer bool) *string {
	if u == nil || !u.AtLimit || live || u.Package == nil || u.Limit == nil {
		return nil
	}
	var s string
	if reviewer {
		s = fmt.Sprintf("The advertiser uses %d of %d active listings on the %s package in %s. "+
			"Approving this listing will be refused until a slot is free or their package changes.", u.Used, *u.Limit, u.Package.Name, u.Market.Name)
	} else {
		s = fmt.Sprintf("Your %s package allows %d active listings in %s and %d are live. You can submit this listing, "+
			"but it can only be approved when a slot is free: archive a live listing, or ask us about a larger package.",
			u.Package.Name, *u.Limit, u.Market.Name, u.Used)
	}
	return &s
}

// QuotaMessage is the message of 409 LISTING_QUOTA_REACHED.
func QuotaMessage(e *QuotaError, marketName string) string {
	return fmt.Sprintf("The advertiser's %s package allows %d active listings in %s and %d are live. "+
		"This listing can be approved when one of them is archived or expires, or when the advertiser's package changes.",
		e.Package.Name, e.Limit, marketName, e.Used)
}

// AccountView is GET /v1/agent/organisations/{id}/package and /v1/me/package.
type AccountView struct {
	Account AccountRef    `json:"account"`
	Markets []MarketUsage `json:"markets"`
}

// AdminAccountView is GET /v1/admin/organisations/{id}/package.
type AdminAccountView struct {
	Account     AccountRef       `json:"account"`
	Markets     []MarketUsage    `json:"markets"`
	Assignments []AssignmentView `json:"assignments"`
}
