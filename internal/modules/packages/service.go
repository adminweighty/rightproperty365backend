package packages

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/money"
)

// Deps are what the service needs.
type Deps struct {
	Store   Store
	Markets markets.Store
	Logger  *slog.Logger
	Now     func() time.Time
}

// Service implements the packages use cases.
type Service struct {
	store   Store
	markets markets.Store
	logger  *slog.Logger
	now     func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, markets: d.Markets, logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// AdminRoles manage packages and assignments: SUPER_ADMIN, PLATFORM_ADMIN,
// and MARKET_ADMIN for its own markets.
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformMarketAdmin}
}

// adminMarkets returns the markets the administrator manages: all for
// SUPER_ADMIN and PLATFORM_ADMIN, the MARKET_ADMIN markets otherwise.
func adminMarkets(p *auth.Principal) (codes []string, all bool) {
	if p == nil || p.SessionScope != auth.ScopeAdmin {
		return []string{}, false
	}
	if p.HasPlatformRole(auth.PlatformAdmin) {
		return nil, true
	}
	codes = []string{}
	for _, r := range p.PlatformRoles {
		if r.Role == auth.PlatformMarketAdmin && r.MarketCode != "" && !slices.Contains(codes, r.MarketCode) {
			codes = append(codes, r.MarketCode)
		}
	}
	slices.Sort(codes)
	return codes, false
}

func mayManage(p *auth.Principal, market string) bool {
	codes, all := adminMarkets(p)
	return all || slices.Contains(codes, market)
}

// marketsByCode returns every market by code.
func (s *Service) marketsByCode(ctx context.Context) (map[string]markets.Market, error) {
	all, err := s.markets.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]markets.Market, len(all))
	for _, m := range all {
		out[m.Code] = m
	}
	return out, nil
}

func marketView(code string, all map[string]markets.Market) MarketView {
	m, ok := all[code]
	if !ok {
		return MarketView{Code: code, Name: code, Currency: "", MinorUnit: 2}
	}
	return MarketView{Code: m.Code, Name: m.Name, Currency: m.CurrencyCode, MinorUnit: money.MinorUnit(m.CurrencyCode)}
}

// ---------------------------------------------------------------------------
// Public
// ---------------------------------------------------------------------------

// PublicPackages returns a market's active packages with their prices.
func (s *Service) PublicPackages(ctx context.Context, code string) (PublicList, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return PublicList{}, err
	}
	if _, ok := all[code]; !ok {
		return PublicList{}, ErrMarketNotFound
	}
	list, err := s.store.Packages(ctx, PackageFilter{Market: code})
	if err != nil {
		return PublicList{}, err
	}
	out := PublicList{Market: marketView(code, all), BillingTerms: BillingTermViews(), Items: []PublicPackageView{}}
	for _, p := range list {
		out.Items = append(out.Items, PublicPackageView{Code: p.Code, Name: p.Name, Description: p.Description, ListingLimit: p.ListingLimit,
			IsDefault: p.Default, Sort: p.Sort, Prices: priceViews(p.Prices)})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Administration of packages
// ---------------------------------------------------------------------------

func (s *Service) packageViews(ctx context.Context, list []Package) ([]PackageView, error) {
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(list))
	for i, p := range list {
		ids[i] = p.ID
	}
	uses, err := s.store.PackageUses(ctx, ids, s.clock())
	if err != nil {
		return nil, err
	}
	out := []PackageView{}
	for _, p := range list {
		u := uses[p.ID]
		out = append(out, PackageView{ID: p.ID, Code: p.Code, Name: p.Name, Market: marketView(p.MarketCode, all), ListingLimit: p.ListingLimit,
			Description: p.Description, Active: p.Active, IsDefault: p.Default, Sort: p.Sort, Prices: priceViews(p.Prices),
			CurrentAssignments: u.Current, CreatedAt: p.CreatedAt.UTC(), UpdatedAt: p.UpdatedAt.UTC(),
			Can: Can{Edit: true, Delete: !p.Default && u.Total == 0}})
	}
	return out, nil
}

func (s *Service) packageView(ctx context.Context, p Package) (PackageView, error) {
	list, err := s.packageViews(ctx, []Package{p})
	if err != nil {
		return PackageView{}, err
	}
	return list[0], nil
}

// AdminList returns the packages the administrator manages.
func (s *Service) AdminList(ctx context.Context, p *auth.Principal, market string, includeInactive bool) (AdminList, error) {
	codes, all := adminMarkets(p)
	market = strings.ToUpper(strings.TrimSpace(market))
	f := PackageFilter{Market: market, IncludeInactive: includeInactive}
	if !all {
		if market != "" && !slices.Contains(codes, market) {
			return AdminList{}, ErrMarketForbidden
		}
		f.Markets = codes
	}
	list, err := s.store.Packages(ctx, f)
	if err != nil {
		return AdminList{}, err
	}
	items, err := s.packageViews(ctx, list)
	if err != nil {
		return AdminList{}, err
	}
	out := AdminList{Items: items}
	if !all {
		out.Markets = codes
	}
	return out, nil
}

// managed loads a package the administrator manages (ErrPackageNotFound
// for another market's).
func (s *Service) managed(ctx context.Context, p *auth.Principal, id uuid.UUID) (Package, error) {
	pkg, err := s.store.Package(ctx, id)
	if err != nil {
		return Package{}, err
	}
	if !mayManage(p, pkg.MarketCode) {
		return Package{}, ErrPackageNotFound
	}
	return pkg, nil
}

// AdminGet returns one package.
func (s *Service) AdminGet(ctx context.Context, p *auth.Principal, id uuid.UUID) (PackageView, error) {
	pkg, err := s.managed(ctx, p, id)
	if err != nil {
		return PackageView{}, err
	}
	return s.packageView(ctx, pkg)
}

// PriceInput is one price as entered.
type PriceInput struct {
	BillingTerm string `json:"billing_term"`
	Amount      string `json:"amount"`
}

// CreateInput is POST /v1/admin/packages.
type CreateInput struct {
	Code         string       `json:"code"`
	Name         string       `json:"name"`
	MarketCode   string       `json:"market_code"`
	ListingLimit *int         `json:"listing_limit"`
	Description  string       `json:"description"`
	Active       *bool        `json:"active"`
	IsDefault    bool         `json:"is_default"`
	Sort         *int         `json:"sort"`
	Prices       []PriceInput `json:"prices"`
}

// UpdateInput is PATCH /v1/admin/packages/{id}. Code and MarketCode are
// refused when present.
type UpdateInput struct {
	Code         *string       `json:"code"`
	MarketCode   *string       `json:"market_code"`
	Name         *string       `json:"name"`
	Description  *string       `json:"description"`
	ListingLimit *int          `json:"listing_limit"`
	Active       *bool         `json:"active"`
	IsDefault    *bool         `json:"is_default"`
	Sort         *int          `json:"sort"`
	Prices       *[]PriceInput `json:"prices"`
}

var codeRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,31}$`)

type fieldErrors map[string]string

func (f fieldErrors) add(field, msg string) {
	if _, ok := f[field]; !ok {
		f[field] = msg
	}
}

func (f fieldErrors) err() error {
	if len(f) == 0 {
		return nil
	}
	return &ValidationError{Fields: f}
}

func checkText(errs fieldErrors, field, v string, min, max int) string {
	v = strings.TrimSpace(v)
	n := utf8.RuneCountInString(v)
	switch {
	case !utf8.ValidString(v):
		errs.add(field, "must be valid text")
	case n < min && min == 1:
		errs.add(field, "is required")
	case n > max:
		errs.add(field, fmt.Sprintf("must be at most %d characters", max))
	}
	return v
}

func checkPrices(errs fieldErrors, in []PriceInput, currency string) []Price {
	out := []Price{}
	if len(in) > len(Terms()) {
		errs.add("prices", fmt.Sprintf("must have at most %d prices", len(Terms())))
		return out
	}
	seen := map[string]bool{}
	for i, pr := range in {
		term := strings.ToUpper(strings.TrimSpace(pr.BillingTerm))
		if !slices.Contains(Terms(), term) {
			errs.add(fmt.Sprintf("prices[%d].billing_term", i), "must be MONTHLY, QUARTERLY or ANNUAL")
			continue
		}
		if seen[term] {
			errs.add(fmt.Sprintf("prices[%d].billing_term", i), "has a price already")
			continue
		}
		seen[term] = true
		r, problem := money.ParseAmount(pr.Amount, currency)
		if problem != "" {
			errs.add(fmt.Sprintf("prices[%d].amount", i), fmt.Sprintf("must be a positive amount with at most %d decimals", money.MinorUnit(currency)))
			continue
		}
		out = append(out, Price{Term: term, Amount: money.Format(r, currency), Currency: currency})
	}
	sortPrices(out)
	return out
}

func packageLabel(p Package) string { return p.Name + " (" + p.MarketCode + ")" }

func packageState(p Package) map[string]any {
	prices := map[string]string{}
	for _, pr := range p.Prices {
		prices[pr.Term] = pr.Amount + " " + pr.Currency
	}
	return map[string]any{"code": p.Code, "name": p.Name, "market_code": p.MarketCode, "listing_limit": p.ListingLimit,
		"description": p.Description, "active": p.Active, "is_default": p.Default, "sort": p.Sort, "prices": prices}
}

// Create creates a package.
func (s *Service) Create(ctx context.Context, p *auth.Principal, in CreateInput) (PackageView, error) {
	errs := fieldErrors{}
	market := strings.ToUpper(strings.TrimSpace(in.MarketCode))
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return PackageView{}, err
	}
	m, ok := all[market]
	if !ok {
		errs.add("market_code", "must be a country code")
	} else if !mayManage(p, market) {
		return PackageView{}, ErrMarketForbidden
	}
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if !codeRe.MatchString(code) {
		errs.add("code", "must be 2-32 capital letters, digits or underscores, starting with a letter")
	}
	pkg := Package{ID: uuid.New(), MarketCode: market, Code: code, Active: true, Default: in.IsDefault, Sort: DefaultSort}
	pkg.Name = checkText(errs, "name", in.Name, 1, MaxNameRunes)
	pkg.Description = checkText(errs, "description", in.Description, 0, MaxDescriptionRunes)
	if in.ListingLimit == nil {
		errs.add("listing_limit", "is required")
	} else if *in.ListingLimit < 0 || *in.ListingLimit > MaxListingLimit {
		errs.add("listing_limit", fmt.Sprintf("must be between 0 and %d", MaxListingLimit))
	} else {
		pkg.ListingLimit = *in.ListingLimit
	}
	if in.Active != nil {
		pkg.Active = *in.Active
	}
	if pkg.Default && !pkg.Active {
		errs.add("is_default", "an inactive package cannot be the default")
	}
	if in.Sort != nil {
		if *in.Sort < 0 || *in.Sort > MaxSort {
			errs.add("sort", fmt.Sprintf("must be between 0 and %d", MaxSort))
		} else {
			pkg.Sort = *in.Sort
		}
	}
	if ok {
		pkg.Prices = checkPrices(errs, in.Prices, m.CurrencyCode)
	}
	if err := errs.err(); err != nil {
		return PackageView{}, err
	}
	now := s.clock()
	actor := p.UserID
	pkg.CreatedBy, pkg.CreatedAt, pkg.UpdatedAt = &actor, now, now
	audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionPackageCreated, adminaudit.TargetListingPackage, pkg.ID.String(),
		packageLabel(pkg), "", nil, packageState(pkg), now)
	if err := s.store.CreatePackage(ctx, pkg, audit); err != nil {
		return PackageView{}, err
	}
	saved, err := s.store.Package(ctx, pkg.ID)
	if err != nil {
		return PackageView{}, err
	}
	return s.packageView(ctx, saved)
}

// Update changes a package.
func (s *Service) Update(ctx context.Context, p *auth.Principal, id uuid.UUID, in UpdateInput) (PackageView, error) {
	if _, err := s.managed(ctx, p, id); err != nil {
		return PackageView{}, err
	}
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return PackageView{}, err
	}
	now := s.clock()
	saved, err := s.store.ChangePackage(ctx, id, func(pkg *Package) (adminaudit.Event, error) {
		before := packageState(*pkg)
		errs := fieldErrors{}
		if in.Code != nil {
			errs.add("code", "cannot be changed")
		}
		if in.MarketCode != nil {
			errs.add("market_code", "cannot be changed")
		}
		if in.Name != nil {
			pkg.Name = checkText(errs, "name", *in.Name, 1, MaxNameRunes)
		}
		if in.Description != nil {
			pkg.Description = checkText(errs, "description", *in.Description, 0, MaxDescriptionRunes)
		}
		if in.ListingLimit != nil {
			if *in.ListingLimit < 0 || *in.ListingLimit > MaxListingLimit {
				errs.add("listing_limit", fmt.Sprintf("must be between 0 and %d", MaxListingLimit))
			} else {
				pkg.ListingLimit = *in.ListingLimit
			}
		}
		if in.Sort != nil {
			if *in.Sort < 0 || *in.Sort > MaxSort {
				errs.add("sort", fmt.Sprintf("must be between 0 and %d", MaxSort))
			} else {
				pkg.Sort = *in.Sort
			}
		}
		if in.Prices != nil {
			currency := all[pkg.MarketCode].CurrencyCode
			pkg.Prices = checkPrices(errs, *in.Prices, currency)
		}
		wasDefault := pkg.Default
		if in.IsDefault != nil {
			pkg.Default = *in.IsDefault
		}
		if in.Active != nil {
			pkg.Active = *in.Active
		}
		if err := errs.err(); err != nil {
			return adminaudit.Event{}, err
		}
		if !pkg.Active && pkg.Default {
			if wasDefault {
				return adminaudit.Event{}, ErrIsDefault
			}
			return adminaudit.Event{}, &ValidationError{Fields: map[string]string{"is_default": "an inactive package cannot be the default"}}
		}
		pkg.UpdatedAt = now
		return adminaudit.New(p.UserID, p.Name, adminaudit.ActionPackageUpdated, adminaudit.TargetListingPackage, pkg.ID.String(),
			packageLabel(*pkg), "", before, packageState(*pkg), now), nil
	})
	if err != nil {
		return PackageView{}, err
	}
	return s.packageView(ctx, saved)
}

// Delete deletes a package no assignment names and that is not the default.
func (s *Service) Delete(ctx context.Context, p *auth.Principal, id uuid.UUID) error {
	if _, err := s.managed(ctx, p, id); err != nil {
		return err
	}
	now := s.clock()
	return s.store.DeletePackage(ctx, id, func(pkg Package, n int) (adminaudit.Event, error) {
		if pkg.Default {
			return adminaudit.Event{}, ErrIsDefault
		}
		if n > 0 {
			return adminaudit.Event{}, &InUseError{Assignments: n}
		}
		return adminaudit.New(p.UserID, p.Name, adminaudit.ActionPackageDeleted, adminaudit.TargetListingPackage, pkg.ID.String(),
			packageLabel(pkg), "", packageState(pkg), nil, now), nil
	})
}

// ---------------------------------------------------------------------------
// Usage
// ---------------------------------------------------------------------------

// Usage returns the account's package and active listings in a market.
func (s *Service) Usage(ctx context.Context, acct Account, market string) (MarketUsage, error) {
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return MarketUsage{}, err
	}
	return s.usage(ctx, acct, market, all)
}

func (s *Service) usage(ctx context.Context, acct Account, market string, all map[string]markets.Market) (MarketUsage, error) {
	now := s.clock()
	r, err := s.store.Resolve(ctx, acct, market, now)
	if err != nil {
		return MarketUsage{}, err
	}
	live, err := s.store.LiveListings(ctx, acct, market, now)
	if err != nil {
		return MarketUsage{}, err
	}
	return usageOf(marketView(market, all), r, live), nil
}

// MarketName returns a market's name (its code when unknown).
func (s *Service) MarketName(ctx context.Context, code string) string {
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return code
	}
	return marketView(code, all).Name
}

// AccountName returns the account's name ("" when unknown).
func (s *Service) AccountName(ctx context.Context, acct Account) string {
	name, err := s.store.AccountName(ctx, acct)
	if err != nil {
		return ""
	}
	return name
}

// usages returns the usage in the account's markets (or in one market),
// keeping those keep accepts.
func (s *Service) usages(ctx context.Context, acct Account, market string, keep func(string) bool) ([]MarketUsage, error) {
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return nil, err
	}
	var codes []string
	if market != "" {
		market = strings.ToUpper(strings.TrimSpace(market))
		if _, ok := all[market]; !ok {
			return nil, &ValidationError{Fields: map[string]string{"market_code": "must be a country code"}}
		}
		codes = []string{market}
	} else if codes, err = s.store.AccountMarkets(ctx, acct, s.clock()); err != nil {
		return nil, err
	}
	out := []MarketUsage{}
	for _, c := range codes {
		if keep != nil && !keep(c) {
			continue
		}
		u, err := s.usage(ctx, acct, c, all)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

func (s *Service) accountRef(ctx context.Context, acct Account) (AccountRef, error) {
	name, err := s.store.AccountName(ctx, acct)
	if err != nil {
		return AccountRef{}, err
	}
	return AccountRef{Kind: acct.Kind, ID: acct.ID, Name: name}, nil
}

// AgentUsage is GET /v1/agent/organisations/{id}/package: id is an
// organisation the caller is a member of (whose kind manages listings), or
// the caller's own user id while they are an approved direct owner.
func (s *Service) AgentUsage(ctx context.Context, p *auth.Principal, id uuid.UUID, market string) (AccountView, error) {
	var acct Account
	switch {
	case p.MayReadListingPackage(id):
		acct = Account{Kind: KindOrganisation, ID: id}
	case id == p.UserID && p.IsDirectOwner():
		acct = Account{Kind: KindDirectOwner, ID: id}
	default:
		return AccountView{}, ErrAccountNotFound
	}
	return s.accountView(ctx, acct, market)
}

// MyUsage is GET /v1/me/package: the caller's own direct-owner account.
func (s *Service) MyUsage(ctx context.Context, p *auth.Principal, market string) (AccountView, error) {
	if !p.IsDirectOwner() {
		return AccountView{}, ErrForbidden
	}
	return s.accountView(ctx, Account{Kind: KindDirectOwner, ID: p.UserID}, market)
}

func (s *Service) accountView(ctx context.Context, acct Account, market string) (AccountView, error) {
	ref, err := s.accountRef(ctx, acct)
	if err != nil {
		return AccountView{}, err
	}
	list, err := s.usages(ctx, acct, market, nil)
	if err != nil {
		return AccountView{}, err
	}
	return AccountView{Account: ref, Markets: list}, nil
}

// ---------------------------------------------------------------------------
// Assignment (administrators)
// ---------------------------------------------------------------------------

func (s *Service) assignmentViews(ctx context.Context, ref AccountRef, list []Assignment) ([]AssignmentView, error) {
	today := Day(s.clock())
	cache := map[uuid.UUID]Package{}
	out := []AssignmentView{}
	for _, a := range list {
		pkg, ok := cache[a.PackageID]
		if !ok {
			var err error
			if pkg, err = s.store.Package(ctx, a.PackageID); err != nil {
				return nil, err
			}
			cache[a.PackageID] = pkg
		}
		v := AssignmentView{ID: a.ID, Account: ref, MarketCode: a.MarketCode, Package: RefOf(pkg), StartsOn: a.StartsOn.Format(DateLayout),
			EndsOn: dateString(a.EndsOn), Source: a.Source, Note: a.Note, Status: a.Status(today),
			AssignedBy: PersonRef{UserID: a.AssignedBy, Name: a.AssignedByName}, CreatedAt: a.CreatedAt.UTC(), CancelledAt: a.CancelledAt}
		out = append(out, v)
	}
	return out, nil
}

// AdminAccount is GET /v1/admin/organisations/{id}/package (and users):
// the usage and assignments in the markets the administrator manages.
func (s *Service) AdminAccount(ctx context.Context, p *auth.Principal, acct Account, market string) (AdminAccountView, error) {
	ref, err := s.accountRef(ctx, acct)
	if err != nil {
		return AdminAccountView{}, err
	}
	market = strings.ToUpper(strings.TrimSpace(market))
	if market != "" && !mayManage(p, market) {
		return AdminAccountView{}, ErrMarketForbidden
	}
	keep := func(code string) bool { return mayManage(p, code) }
	list, err := s.usages(ctx, acct, market, keep)
	if err != nil {
		return AdminAccountView{}, err
	}
	all, err := s.store.Assignments(ctx, acct)
	if err != nil {
		return AdminAccountView{}, err
	}
	shown := []Assignment{}
	for _, a := range all {
		if keep(a.MarketCode) && (market == "" || a.MarketCode == market) {
			shown = append(shown, a)
		}
	}
	views, err := s.assignmentViews(ctx, ref, shown)
	if err != nil {
		return AdminAccountView{}, err
	}
	return AdminAccountView{Account: ref, Markets: list, Assignments: views}, nil
}

// AssignInput is POST /v1/admin/organisations/{id}/package.
type AssignInput struct {
	PackageID *uuid.UUID `json:"package_id"`
	StartsOn  *string    `json:"starts_on"`
	EndsOn    *string    `json:"ends_on"`
	Note      string     `json:"note"`
	Source    *string    `json:"source"`
}

// AssignResultView is the answer of an assignment.
type AssignResultView struct {
	Assignment AssignmentView `json:"assignment"`
	Usage      MarketUsage    `json:"usage"`
}

func assignmentState(a Assignment, pkg Package) map[string]any {
	return map[string]any{"assignment_id": a.ID.String(), "package": pkg.Code, "package_name": pkg.Name, "market_code": a.MarketCode,
		"starts_on": a.StartsOn.Format(DateLayout), "ends_on": dateString(a.EndsOn), "source": a.Source, "note": a.Note}
}

func auditTarget(acct Account) string {
	if acct.Kind == KindOrganisation {
		return adminaudit.TargetOrganisation
	}
	return adminaudit.TargetUser
}

// Assign gives the account a package from a date.
func (s *Service) Assign(ctx context.Context, p *auth.Principal, acct Account, in AssignInput) (AssignResultView, error) {
	ref, err := s.accountRef(ctx, acct)
	if err != nil {
		return AssignResultView{}, err
	}
	errs := fieldErrors{}
	if in.PackageID == nil {
		errs.add("package_id", "is required")
	}
	if in.Source != nil && *in.Source != SourceAdmin {
		errs.add("source", "must be ADMIN (INVOICE is set by invoices)")
	}
	now := s.clock()
	today := Day(now)
	starts := today
	if in.StartsOn != nil {
		t, err := time.Parse(DateLayout, *in.StartsOn)
		switch {
		case err != nil:
			errs.add("starts_on", "must be a date (YYYY-MM-DD)")
		case t.Before(today):
			errs.add("starts_on", "may not be in the past")
		default:
			starts = t
		}
	}
	var ends *time.Time
	if in.EndsOn != nil {
		t, err := time.Parse(DateLayout, *in.EndsOn)
		switch {
		case err != nil:
			errs.add("ends_on", "must be a date (YYYY-MM-DD)")
		case t.Before(starts):
			errs.add("ends_on", "must be on or after starts_on")
		default:
			ends = &t
		}
	}
	note := checkText(errs, "note", in.Note, 0, MaxNoteRunes)
	if err := errs.err(); err != nil {
		return AssignResultView{}, err
	}
	pkg, err := s.store.Package(ctx, *in.PackageID)
	if errors.Is(err, ErrPackageNotFound) {
		return AssignResultView{}, &ValidationError{Fields: map[string]string{"package_id": "must be a package"}}
	}
	if err != nil {
		return AssignResultView{}, err
	}
	if !mayManage(p, pkg.MarketCode) {
		return AssignResultView{}, ErrMarketForbidden
	}
	if !pkg.Active {
		return AssignResultView{}, ErrInactive
	}
	actor := p.UserID
	a := Assignment{ID: uuid.New(), Account: acct, MarketCode: pkg.MarketCode, PackageID: pkg.ID, StartsOn: starts, EndsOn: ends,
		Source: SourceAdmin, Note: note, AssignedBy: &actor, AssignedByName: p.Name, CreatedAt: now}
	_, err = s.store.Assign(ctx, a, func(replaced []Assignment) adminaudit.Event {
		var before map[string]any
		if len(replaced) > 0 {
			ids := []string{}
			for _, r := range replaced {
				ids = append(ids, r.ID.String())
			}
			before = map[string]any{"replaced_assignments": ids}
		}
		return adminaudit.New(p.UserID, p.Name, adminaudit.ActionPackageAssigned, auditTarget(acct), acct.ID.String(), ref.Name, note,
			before, assignmentState(a, pkg), now)
	})
	if err != nil {
		return AssignResultView{}, err
	}
	views, err := s.assignmentViews(ctx, ref, []Assignment{a})
	if err != nil {
		return AssignResultView{}, err
	}
	u, err := s.Usage(ctx, acct, pkg.MarketCode)
	if err != nil {
		return AssignResultView{}, err
	}
	return AssignResultView{Assignment: views[0], Usage: u}, nil
}

// EndInput is POST /v1/admin/organisations/{id}/package/end.
type EndInput struct {
	MarketCode string `json:"market_code"`
	Note       string `json:"note"`
}

// End ends the account's current assignment in a market and cancels the
// scheduled ones: the market default applies again.
func (s *Service) End(ctx context.Context, p *auth.Principal, acct Account, in EndInput) (MarketUsage, error) {
	ref, err := s.accountRef(ctx, acct)
	if err != nil {
		return MarketUsage{}, err
	}
	errs := fieldErrors{}
	market := strings.ToUpper(strings.TrimSpace(in.MarketCode))
	all, err := s.marketsByCode(ctx)
	if err != nil {
		return MarketUsage{}, err
	}
	if _, ok := all[market]; !ok {
		errs.add("market_code", "must be a country code")
	}
	note := checkText(errs, "note", in.Note, 0, MaxNoteRunes)
	if err := errs.err(); err != nil {
		return MarketUsage{}, err
	}
	if !mayManage(p, market) {
		return MarketUsage{}, ErrMarketForbidden
	}
	now := s.clock()
	actor := p.UserID
	err = s.store.EndAssignments(ctx, acct, market, now, &actor, now, func(ended []Assignment) adminaudit.Event {
		ids := []string{}
		for _, a := range ended {
			ids = append(ids, a.ID.String())
		}
		return adminaudit.New(p.UserID, p.Name, adminaudit.ActionPackageAssignmentEnded, auditTarget(acct), acct.ID.String(), ref.Name, note,
			map[string]any{"market_code": market, "assignments": ids}, map[string]any{"market_code": market, "basis": "DEFAULT_OR_NONE"}, now)
	})
	if err != nil {
		return MarketUsage{}, err
	}
	return s.usage(ctx, acct, market, all)
}
