package packages

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// MemoryDeps are what the memory store reads from the other memory stores.
type MemoryDeps struct {
	// LiveListings returns the expiries of the account's active listings
	// in the market (nil: none).
	LiveListings func(acct Account, market string, now time.Time) []time.Time
	// ListingMarkets returns the markets the account has listings in (nil: none).
	ListingMarkets func(acct Account) []string
	// AccountName returns the organisation's or the (not deleted) user's
	// name (nil: every account is unknown).
	AccountName func(acct Account) (string, bool)
	// HomeMarket returns an organisation's market (nil: none).
	HomeMarket func(organisationID uuid.UUID) string
}

// MemoryStore is the in-memory Store for development and tests.
type MemoryStore struct {
	mu          sync.Mutex
	packages    map[uuid.UUID]Package
	assignments []Assignment
	audit       *adminaudit.MemoryLog
	deps        MemoryDeps
}

// NewMemoryStore returns an empty store writing its audit records to audit
// (nil: a private log).
func NewMemoryStore(audit *adminaudit.MemoryLog) *MemoryStore {
	if audit == nil {
		audit = &adminaudit.MemoryLog{}
	}
	return &MemoryStore{packages: map[uuid.UUID]Package{}, audit: audit}
}

// SetDeps supplies the readers of the other memory stores.
func (s *MemoryStore) SetDeps(d MemoryDeps) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps = d
}

func clonePackage(p Package) Package {
	p.Prices = append([]Price{}, p.Prices...)
	return p
}

// PutPackage adds or replaces a package as it is (seed helper; the default
// flag of the market's other packages is cleared when p is the default).
func (s *MemoryStore) PutPackage(p Package) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.putLocked(clonePackage(p))
}

func (s *MemoryStore) putLocked(p Package) {
	if p.Default {
		for id, other := range s.packages {
			if other.MarketCode == p.MarketCode && other.Default && id != p.ID {
				other.Default, other.UpdatedAt = false, p.UpdatedAt
				s.packages[id] = other
			}
		}
	}
	sortPrices(p.Prices)
	s.packages[p.ID] = p
}

// PutAssignment adds or replaces an assignment as it is (seed helper).
func (s *MemoryStore) PutAssignment(a Assignment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.assignments {
		if s.assignments[i].ID == a.ID {
			s.assignments[i] = a
			return
		}
	}
	s.assignments = append(s.assignments, a)
}

func sortPackages(list []Package) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.MarketCode != b.MarketCode {
			return a.MarketCode < b.MarketCode
		}
		if a.Sort != b.Sort {
			return a.Sort < b.Sort
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID.String() < b.ID.String()
	})
}

// Packages implements Store.
func (s *MemoryStore) Packages(_ context.Context, f PackageFilter) ([]Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Package{}
	for _, p := range s.packages {
		if (f.Markets != nil && !slices.Contains(f.Markets, p.MarketCode)) || (f.Market != "" && p.MarketCode != f.Market) ||
			(!f.IncludeInactive && !p.Active) {
			continue
		}
		out = append(out, clonePackage(p))
	}
	sortPackages(out)
	return out, nil
}

// Package implements Store.
func (s *MemoryStore) Package(_ context.Context, id uuid.UUID) (Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.packages[id]
	if !ok {
		return Package{}, ErrPackageNotFound
	}
	return clonePackage(p), nil
}

// CreatePackage implements Store.
func (s *MemoryStore) CreatePackage(_ context.Context, p Package, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, other := range s.packages {
		if other.MarketCode == p.MarketCode && strings.EqualFold(other.Code, p.Code) {
			return ErrCodeTaken
		}
	}
	s.putLocked(clonePackage(p))
	s.audit.Append(audit)
	return nil
}

// ChangePackage implements Store.
func (s *MemoryStore) ChangePackage(_ context.Context, id uuid.UUID, change func(p *Package) (adminaudit.Event, error)) (Package, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.packages[id]
	if !ok {
		return Package{}, ErrPackageNotFound
	}
	p := clonePackage(cur)
	audit, err := change(&p)
	if err != nil {
		return Package{}, err
	}
	s.putLocked(clonePackage(p))
	s.audit.Append(audit)
	return clonePackage(s.packages[id]), nil
}

// DeletePackage implements Store.
func (s *MemoryStore) DeletePackage(_ context.Context, id uuid.UUID, check func(p Package, assignments int) (adminaudit.Event, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.packages[id]
	if !ok {
		return ErrPackageNotFound
	}
	n := 0
	for _, a := range s.assignments {
		if a.PackageID == id {
			n++
		}
	}
	audit, err := check(clonePackage(p), n)
	if err != nil {
		return err
	}
	delete(s.packages, id)
	s.audit.Append(audit)
	return nil
}

// PackageUses implements Store.
func (s *MemoryStore) PackageUses(_ context.Context, ids []uuid.UUID, today time.Time) (map[uuid.UUID]Uses, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	today = Day(today)
	out := map[uuid.UUID]Uses{}
	for _, a := range s.assignments {
		if !slices.Contains(ids, a.PackageID) {
			continue
		}
		u := out[a.PackageID]
		u.Total++
		if a.InForce(today) {
			u.Current++
		}
		out[a.PackageID] = u
	}
	return out, nil
}

// ResolveNow is Resolve without the context (the listing memory store's
// quota check, made while it holds its own lock; it never calls back).
func (s *MemoryStore) ResolveNow(acct Account, market string, today time.Time) Resolved {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resolveLocked(acct, market, Day(today))
}

func (s *MemoryStore) resolveLocked(acct Account, market string, today time.Time) Resolved {
	list := []Assignment{}
	for _, a := range s.assignments {
		if a.Account == acct && a.MarketCode == market && a.CancelledAt == nil && (a.EndsOn == nil || !a.EndsOn.Before(today)) {
			list = append(list, a)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].StartsOn.Before(list[j].StartsOn) })
	out := Resolved{Basis: BasisNone}
	for i := range list {
		a := list[i]
		switch {
		case a.InForce(today) && out.Assignment == nil:
			out.Assignment = &a
		case a.StartsOn.After(today) && out.Scheduled == nil:
			out.Scheduled = &a
		}
	}
	if out.Assignment != nil {
		p := clonePackage(s.packages[out.Assignment.PackageID])
		out.Basis, out.Package = BasisAssigned, &p
	} else {
		for _, p := range s.packages {
			if p.MarketCode == market && p.Default {
				c := clonePackage(p)
				out.Basis, out.Package = BasisDefault, &c
				break
			}
		}
	}
	if out.Scheduled != nil {
		p := clonePackage(s.packages[out.Scheduled.PackageID])
		out.ScheduledPackage = &p
	}
	return out
}

// Resolve implements Store.
func (s *MemoryStore) Resolve(_ context.Context, acct Account, market string, today time.Time) (Resolved, error) {
	return s.ResolveNow(acct, market, today), nil
}

// Assignments implements Store.
func (s *MemoryStore) Assignments(_ context.Context, acct Account) ([]Assignment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Assignment{}
	for _, a := range s.assignments {
		if a.Account == acct {
			out = append(out, a)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > MaxAssignmentsShown {
		out = out[:MaxAssignmentsShown]
	}
	return out, nil
}

// replaceFromLocked is replaceFrom of the Postgres store.
func (s *MemoryStore) replaceFromLocked(acct Account, market string, from time.Time, by *uuid.UUID, at time.Time) []Assignment {
	var before []Assignment
	for i := range s.assignments {
		a := &s.assignments[i]
		if a.Account != acct || a.MarketCode != market || a.CancelledAt != nil || (a.EndsOn != nil && a.EndsOn.Before(from)) {
			continue
		}
		before = append(before, *a)
		if !a.StartsOn.Before(from) {
			t := at
			a.CancelledAt, a.CancelledBy = &t, by
		} else {
			end := from.AddDate(0, 0, -1)
			a.EndsOn = &end
		}
	}
	sort.SliceStable(before, func(i, j int) bool { return before[i].StartsOn.Before(before[j].StartsOn) })
	return before
}

// Assign implements Store.
func (s *MemoryStore) Assign(_ context.Context, a Assignment, audit func(replaced []Assignment) adminaudit.Event) (AssignResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.packages[a.PackageID]
	if !ok {
		return AssignResult{}, ErrPackageNotFound
	}
	if !p.Active {
		return AssignResult{}, ErrInactive
	}
	replaced := s.replaceFromLocked(a.Account, a.MarketCode, a.StartsOn, a.AssignedBy, a.CreatedAt)
	s.assignments = append(s.assignments, a)
	s.audit.Append(audit(replaced))
	return AssignResult{Replaced: replaced}, nil
}

// EndAssignments implements Store.
func (s *MemoryStore) EndAssignments(_ context.Context, acct Account, market string, today time.Time, by *uuid.UUID, at time.Time,
	audit func(ended []Assignment) adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ended := s.replaceFromLocked(acct, market, Day(today), by, at)
	if len(ended) == 0 {
		return ErrAssignmentNotFound
	}
	s.audit.Append(audit(ended))
	return nil
}

func (s *MemoryStore) depsCopy() MemoryDeps {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deps
}

// LiveListings implements Store.
func (s *MemoryStore) LiveListings(_ context.Context, acct Account, market string, now time.Time) ([]time.Time, error) {
	d := s.depsCopy()
	if d.LiveListings == nil {
		return []time.Time{}, nil
	}
	out := d.LiveListings(acct, market, now)
	slices.SortFunc(out, func(a, b time.Time) int { return a.Compare(b) })
	return out, nil
}

// AccountMarkets implements Store.
func (s *MemoryStore) AccountMarkets(_ context.Context, acct Account, today time.Time) ([]string, error) {
	d := s.depsCopy()
	out := []string{}
	add := func(code string) {
		if code != "" && !slices.Contains(out, code) {
			out = append(out, code)
		}
	}
	if d.ListingMarkets != nil {
		for _, c := range d.ListingMarkets(acct) {
			add(c)
		}
	}
	if acct.Kind == KindOrganisation && d.HomeMarket != nil {
		add(d.HomeMarket(acct.ID))
	}
	today = Day(today)
	s.mu.Lock()
	for _, a := range s.assignments {
		if a.Account == acct && a.CancelledAt == nil && (a.EndsOn == nil || !a.EndsOn.Before(today)) {
			add(a.MarketCode)
		}
	}
	s.mu.Unlock()
	slices.Sort(out)
	return out, nil
}

// AccountName implements Store.
func (s *MemoryStore) AccountName(_ context.Context, acct Account) (string, error) {
	d := s.depsCopy()
	if d.AccountName == nil {
		return "", ErrAccountNotFound
	}
	name, ok := d.AccountName(acct)
	if !ok {
		return "", ErrAccountNotFound
	}
	return name, nil
}

// ForgetUser is ForgetUserTx of the Postgres store.
func (s *MemoryStore) ForgetUser(userID uuid.UUID, deletedName string, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	acct := Account{Kind: KindDirectOwner, ID: userID}
	markets := []string{}
	for _, a := range s.assignments {
		if a.Account == acct && !slices.Contains(markets, a.MarketCode) {
			markets = append(markets, a.MarketCode)
		}
	}
	for _, m := range markets {
		s.replaceFromLocked(acct, m, Day(at), nil, at)
	}
	for i := range s.assignments {
		a := &s.assignments[i]
		if a.AssignedBy != nil && *a.AssignedBy == userID {
			a.AssignedBy, a.AssignedByName = nil, deletedName
		}
		if a.CancelledBy != nil && *a.CancelledBy == userID {
			a.CancelledBy = nil
		}
	}
	for id, p := range s.packages {
		if p.CreatedBy != nil && *p.CreatedBy == userID {
			p.CreatedBy = nil
			s.packages[id] = p
		}
	}
}
