// Package tax holds the tax rules per market (API 0.43.0, BR-001).
//
// A rule names a tax (VAT, GST, ...) with a rate in percent for one market
// and one use (SUBSCRIPTIONS, VENUE_BOOKINGS, LISTING_FEES), inclusive or
// exclusive, over a period [effective_from, effective_to). TaxFor picks the
// rule in force at a moment.
//
// NOTHING APPLIES THESE RULES YET. The only tax the API computes is the venue
// quote's TAX fee, which each venue owner sets in their own hourly settings
// (their tax, not a platform rate), and subscription invoices carry no tax.
// The development and staging seed (ZW VAT 15%, GB VAT 20%) holds SAMPLE
// values; production has no rule until finance adds one.
package tax

import (
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// What a rule applies to.
const (
	AppliesSubscriptions = "SUBSCRIPTIONS"
	AppliesVenueBookings = "VENUE_BOOKINGS"
	AppliesListingFees   = "LISTING_FEES"
)

// AppliesTo returns every use.
func AppliesTo() []string {
	return []string{AppliesSubscriptions, AppliesVenueBookings, AppliesListingFees}
}

// Statuses.
const (
	StatusActive  = "ACTIVE"
	StatusRetired = "RETIRED"
)

// Audit actions and target.
const (
	ActionRuleCreated = "TAX_RULE_CREATED"
	ActionRuleUpdated = "TAX_RULE_UPDATED"
	ActionRuleRetired = "TAX_RULE_RETIRED"
	TargetRule        = "TAX_RULE"
)

// Error codes.
const (
	CodeRuleNotFound = "TAX_RULE_NOT_FOUND"
	CodeRuleOverlap  = "TAX_RULE_OVERLAP"
	CodeRuleRetired  = "TAX_RULE_RETIRED"
)

// Errors.
var (
	ErrNotFound = errors.New("tax rule not found")
	ErrOverlap  = errors.New("another active rule covers part of the period")
	ErrRetired  = errors.New("the tax rule is retired")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// AdminRoles may manage tax rules.
func AdminRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformFinanceAdmin} }

// Rule is one tax rule.
type Rule struct {
	ID         uuid.UUID
	MarketCode string
	TaxName    string
	// Rate is a percent as a canonical decimal string ("15", "12.5").
	Rate                    string
	AppliesTo               string
	Inclusive               bool
	EffectiveFrom           time.Time
	EffectiveTo             *time.Time
	RegistrationNumberLabel *string
	Status                  string
	IsSample                bool
	Notes                   string
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Clone returns a deep copy.
func (r Rule) Clone() Rule {
	if r.EffectiveTo != nil {
		t := *r.EffectiveTo
		r.EffectiveTo = &t
	}
	if r.RegistrationNumberLabel != nil {
		s := *r.RegistrationNumberLabel
		r.RegistrationNumberLabel = &s
	}
	return r
}

// InForce reports whether the rule is ACTIVE and covers at.
func (r Rule) InForce(at time.Time) bool {
	return r.Status == StatusActive && !at.Before(r.EffectiveFrom) && (r.EffectiveTo == nil || at.Before(*r.EffectiveTo))
}

// overlaps reports whether two periods share a moment.
func (r Rule) overlaps(o Rule) bool {
	aEndsBeforeB := r.EffectiveTo != nil && !r.EffectiveTo.After(o.EffectiveFrom)
	bEndsBeforeA := o.EffectiveTo != nil && !o.EffectiveTo.After(r.EffectiveFrom)
	return !aEndsBeforeB && !bEndsBeforeA
}

// Conflicts reports whether r and o may not both be ACTIVE: same market and
// use, overlapping periods.
func (r Rule) Conflicts(o Rule) bool {
	return r.ID != o.ID && r.Status == StatusActive && o.Status == StatusActive && r.MarketCode == o.MarketCode &&
		r.AppliesTo == o.AppliesTo && r.overlaps(o)
}

// TaxFor returns the rule in force for market and appliesTo at the moment
// at, or false when there is none. It is a pure function over rules. Should
// two overlap (the service refuses that), the latest effective_from wins.
func TaxFor(rules []Rule, market, appliesTo string, at time.Time) (Rule, bool) {
	var best *Rule
	for i := range rules {
		r := &rules[i]
		if r.MarketCode != market || r.AppliesTo != appliesTo || !r.InForce(at) {
			continue
		}
		if best == nil || r.EffectiveFrom.After(best.EffectiveFrom) {
			best = r
		}
	}
	if best == nil {
		return Rule{}, false
	}
	return best.Clone(), true
}

var rateRe = regexp.MustCompile(`^\d{1,3}(\.\d{1,4})?$`)

// ParseRate checks a percent and returns its canonical form ("15.50" ->
// "15.5", "020" -> "20"), or false.
func ParseRate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if !rateRe.MatchString(s) {
		return "", false
	}
	whole, frac, _ := strings.Cut(s, ".")
	whole = strings.TrimLeft(whole, "0")
	if whole == "" {
		whole = "0"
	}
	frac = strings.TrimRight(frac, "0")
	if len(whole) > 3 || (len(whole) == 3 && (whole != "100" || frac != "")) {
		return "", false
	}
	if frac == "" {
		return whole, true
	}
	return whole + "." + frac, true
}

// Filter selects rules; empty fields match everything.
type Filter struct {
	Market, AppliesTo, Status string
}

// Matches reports whether r passes the filter.
func (f Filter) Matches(r Rule) bool {
	return (f.Market == "" || r.MarketCode == f.Market) && (f.AppliesTo == "" || r.AppliesTo == f.AppliesTo) &&
		(f.Status == "" || r.Status == f.Status)
}

// SortRules orders by market, use, then newest period first.
func SortRules(list []Rule) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.MarketCode != b.MarketCode {
			return a.MarketCode < b.MarketCode
		}
		if a.AppliesTo != b.AppliesTo {
			return slices.Index(AppliesTo(), a.AppliesTo) < slices.Index(AppliesTo(), b.AppliesTo)
		}
		if !a.EffectiveFrom.Equal(b.EffectiveFrom) {
			return a.EffectiveFrom.After(b.EffectiveFrom)
		}
		return a.ID.String() < b.ID.String()
	})
}

// SampleNote is the note on the seeded rules.
const SampleNote = "SAMPLE VALUE — confirm with a tax adviser. Not applied anywhere in API 0.43.0."

// Seed IDs (development and staging only).
var (
	SeedZWVATID = uuid.MustParse("0195c0de-0000-7000-8000-0000000a0e01")
	SeedGBVATID = uuid.MustParse("0195c0de-0000-7000-8000-0000000a0e02")
)

// SeedRules returns the DEVELOPMENT AND STAGING tax rules (the memory stores
// and devseed.SeedPostgresLegalTax; migration 0045 inserts none): ZW VAT 15%
// and GB VAT 20% for SUBSCRIPTIONS, exclusive, from 2026-01-01, ACTIVE,
// SAMPLE values.
func SeedRules(now time.Time) []Rule {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	label := "VAT registration number"
	mk := func(id uuid.UUID, market, rate string) Rule {
		l := label
		return Rule{ID: id, MarketCode: market, TaxName: "VAT", Rate: rate, AppliesTo: AppliesSubscriptions, EffectiveFrom: from,
			RegistrationNumberLabel: &l, Status: StatusActive, IsSample: true, Notes: SampleNote, CreatedAt: now.UTC(), UpdatedAt: now.UTC()}
	}
	return []Rule{mk(SeedZWVATID, "ZW", "15"), mk(SeedGBVATID, "GB", "20")}
}
