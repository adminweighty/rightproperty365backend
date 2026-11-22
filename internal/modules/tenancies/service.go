package tenancies

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/marketday"
	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// ContactInfo is a CRM contact as tenancies see it.
type ContactInfo struct {
	ID   uuid.UUID
	Book Book
	Name string
}

// Links looks up what a property or tenancy may be linked to. ok is false
// when the item does not exist, is not live (contacts) or does not belong
// to the book.
type Links interface {
	Contact(ctx context.Context, book Book, id uuid.UUID) (ContactInfo, bool, error)
	Listing(ctx context.Context, book Book, id uuid.UUID) (ListingRef, bool, error)
	Lease(ctx context.Context, book Book, id uuid.UUID) (LeaseRef, bool, error)
	// ContactsWithEmail returns the live contacts of every book with the
	// email (normalised: trimmed and lower-cased).
	ContactsWithEmail(ctx context.Context, email string) ([]ContactInfo, error)
}

// Deps is what the service needs.
type Deps struct {
	Store Store
	Links Links
	// Currencies returns the ISO 4217 codes a tenancy may use (reference data).
	Currencies func(ctx context.Context) ([]string, error)
	// Markets returns the country codes a property may be in (reference data).
	Markets func(ctx context.Context) ([]string, error)
	// Account returns a user's email and whether it is verified.
	Account func(ctx context.Context, userID uuid.UUID) (email string, verified bool, err error)
	// Ledger posts the RENT_RECEIVED entry of a rent receipt (part b); it may
	// also be set later with SetLedger.
	Ledger *ledger.Service
	// Zone returns the IANA time zone of a market code: "today" for a
	// tenancy is the date in its property's market (nil: UTC).
	Zone   marketday.Zone
	Logger *slog.Logger
	Now    func() time.Time
}

// Service is properties, units and tenancies.
type Service struct {
	store      Store
	links      Links
	currencies func(ctx context.Context) ([]string, error)
	markets    func(ctx context.Context) ([]string, error)
	account    func(ctx context.Context, userID uuid.UUID) (string, bool, error)
	ledger     *ledger.Service
	zone       marketday.Zone
	logger     *slog.Logger
	now        func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, links: d.Links, currencies: d.Currencies, markets: d.Markets, account: d.Account, ledger: d.Ledger,
		zone: d.Zone, logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// SetLedger connects the ledger service that posts rent receipts' entries
// (the ledger's receivables report reads this service's arrears, so the two
// are built one after the other).
func (s *Service) SetLedger(l *ledger.Service) { s.ledger = l }

// todayIn is the calendar date in a market's time zone (UTC when unknown).
// MarketZone is the IANA zone of a market ("UTC" when unknown); inspections
// show their times in the property's zone (API 0.62.0).
func (s *Service) MarketZone(ctx context.Context, marketCode string) string {
	return marketday.ZoneName(ctx, s.zone, marketCode)
}

func (s *Service) todayIn(ctx context.Context, marketCode string) string {
	return marketday.Today(ctx, s.zone, marketCode, s.clock())
}

// tenancyToday is "today" for a tenancy: the date in its property's market.
func (s *Service) tenancyToday(ctx context.Context, t Tenancy) string {
	return s.PropertyToday(ctx, t.PropertyID)
}

// PropertyToday is the calendar date in the property's market (UTC when the
// property or its market's zone is unknown). Maintenance uses it for the date
// checks of a request's costs.
func (s *Service) PropertyToday(ctx context.Context, propertyID uuid.UUID) string {
	code := ""
	if p, err := s.store.Property(ctx, propertyID); err == nil {
		code = p.MarketCode
	}
	return s.todayIn(ctx, code)
}

// bookToday is "today" for a whole book: the organisation's market for an
// organisation's book, else the market of the book's first property.
func (s *Service) bookToday(ctx context.Context, a access) string {
	if id := a.book.OrganisationID; id != nil {
		for _, m := range a.p.Memberships {
			if m.OrganisationID == *id && m.MarketCode != "" {
				return s.todayIn(ctx, m.MarketCode)
			}
		}
	}
	if list, _, err := s.store.Properties(ctx, a.book, PropertyFilter{Limit: 1}); err == nil && len(list) > 0 {
		return s.todayIn(ctx, list[0].MarketCode)
	}
	return s.todayIn(ctx, "")
}

// access is what the caller may do in one book.
type access struct {
	p     *auth.Principal
	book  Book
	write bool
	// name is the book's name (the organisation's, or the direct owner's).
	name string
}

func (s *Service) access(p *auth.Principal, bookID uuid.UUID) (access, error) {
	if p == nil {
		return access{}, ErrUnauthenticated
	}
	if p.MayReadProperties(bookID) {
		name := ""
		for _, m := range p.Memberships {
			if m.OrganisationID == bookID {
				name = m.OrganisationName
			}
		}
		return access{p: p, book: OrganisationBook(bookID), write: p.MayWriteProperties(bookID), name: name}, nil
	}
	if p.OwnsPropertyBook(bookID) {
		return access{p: p, book: PersonalBook(bookID), write: true, name: p.Name}, nil
	}
	return access{}, ErrBookNotFound
}

func (s *Service) writer(p *auth.Principal, bookID uuid.UUID) (access, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return access{}, err
	}
	if !a.write {
		return access{}, ErrForbidden
	}
	return a, nil
}

// Check answers ErrBookNotFound (or ErrUnauthenticated) when the caller has
// no access to the book: handlers ask first, so another organisation never
// learns more than 404.
func (s *Service) Check(p *auth.Principal, bookID uuid.UUID) error {
	_, err := s.access(p, bookID)
	return err
}

func (a access) actor() *uuid.UUID {
	id := a.p.UserID
	return &id
}

// ---------------------------------------------------------------------------
// Errors with details
// ---------------------------------------------------------------------------

// ValidationError is 400 VALIDATION_FAILED with per-field messages.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

func invalid(f map[string]string) error {
	if len(f) == 0 {
		return nil
	}
	return &ValidationError{Fields: f}
}

// OccupiedError is 409 UNIT_OCCUPIED.
type OccupiedError struct{ TenancyID *uuid.UUID }

func (e *OccupiedError) Error() string { return ErrUnitOccupied.Error() }

// Unwrap lets errors.Is find ErrUnitOccupied.
func (e *OccupiedError) Unwrap() error { return ErrUnitOccupied }

// StateError is 409 TENANCY_INVALID_STATE.
type StateError struct{ Status string }

func (e *StateError) Error() string { return ErrInvalidState.Error() }

// Unwrap lets errors.Is find ErrInvalidState.
func (e *StateError) Unwrap() error { return ErrInvalidState }

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

func (s *Service) event(ctx context.Context, a access, action string, targetID uuid.UUID, label string, before, after map[string]any) []orgaudit.Event {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", slog.Bool("audit", true), slog.String("event", "tenancy."+strings.ToLower(action)),
		slog.String("trace_id", logging.RequestID(ctx)), slog.String("book_id", a.book.ID().String()),
		slog.String("actor_user_id", a.p.UserID.String()), slog.String("target_id", targetID.String()))
	if a.book.OrganisationID == nil {
		return nil
	}
	return []orgaudit.Event{orgaudit.New(*a.book.OrganisationID, a.p.UserID, a.p.Name, action, orgaudit.TargetTenancy, targetID, label, before, after, s.clock())}
}

// Label is the audit label of a tenancy: the property and the unit.
func Label(p Property, t Tenancy) string {
	label := p.Name
	if t.UnitID != nil {
		if u, ok := p.Unit(*t.UnitID); ok {
			label += " " + u.Label
		}
	}
	return label
}

func tenancyAudit(t Tenancy) map[string]any {
	m := map[string]any{"status": t.Status, "rent_amount": t.RentAmount, "rent_currency": t.RentCurrency,
		"rent_frequency": t.RentFrequency, "rent_due_day": t.RentDueDay, "start_date": t.StartDate, "tenants": len(t.PartyIDs(RoleTenant))}
	if t.EndDate != "" {
		m["end_date"] = t.EndDate
	}
	if t.DepositAmount != "" {
		m["deposit_amount"], m["deposit_currency"] = t.DepositAmount, t.DepositCurrency
	}
	if t.EndedOn != "" {
		m["ended_on"] = t.EndedOn
	}
	return m
}

// ---------------------------------------------------------------------------
// Reference data
// ---------------------------------------------------------------------------

func codes(ctx context.Context, fn func(ctx context.Context) ([]string, error), fallback string, valid func(string) bool) ([]string, error) {
	if fn == nil {
		return []string{fallback}, nil
	}
	list, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, c := range list {
		c = strings.ToUpper(strings.TrimSpace(c))
		if valid(c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *Service) allowedCurrencies(ctx context.Context) ([]string, error) {
	return codes(ctx, s.currencies, "USD", money.ValidCurrencyCode)
}

func validMarket(c string) bool {
	return len(c) == 2 && c[0] >= 'A' && c[0] <= 'Z' && c[1] >= 'A' && c[1] <= 'Z'
}

func (s *Service) allowedMarkets(ctx context.Context) ([]string, error) {
	return codes(ctx, s.markets, "ZW", validMarket)
}

// Options serves GET .../tenancies/options.
func (s *Service) Options(ctx context.Context, p *auth.Principal, bookID uuid.UUID) (Options, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Options{}, err
	}
	out := Options{Frequencies: []CodeLabel{}, Statuses: []CodeLabel{}, Currencies: []CurrencyOption{}, Permissions: Permissions{Write: a.write}}
	for _, f := range Frequencies() {
		out.Frequencies = append(out.Frequencies, CodeLabel{Code: f, Label: FrequencyLabel(f)})
	}
	for _, st := range Statuses() {
		out.Statuses = append(out.Statuses, CodeLabel{Code: st, Label: StatusLabel(st)})
	}
	curs, err := s.allowedCurrencies(ctx)
	if err != nil {
		return Options{}, err
	}
	for _, c := range curs {
		out.Currencies = append(out.Currencies, CurrencyOption{Code: c, MinorUnit: money.MinorUnit(c)})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Inputs
// ---------------------------------------------------------------------------

// Optional is a body field that may be absent, null or a value.
type Optional[T any] struct {
	Set   bool
	Null  bool
	Value T
}

// UnmarshalJSON implements json.Unmarshaler (unknown fields of an object
// value are refused, as in the body itself).
func (o *Optional[T]) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(bytes.TrimSpace(b)) == "null" {
		o.Null = true
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(&o.Value)
}

// Has reports whether the field carries a value (set and not null).
func (o Optional[T]) Has() bool { return o.Set && !o.Null }

// UnitInput is one unit of POST .../properties, or the body of the unit routes.
type UnitInput struct {
	Label string `json:"label"`
}

// PropertyInput is the body of POST .../properties and PATCH
// .../properties/{propertyId} (units only on POST).
type PropertyInput struct {
	Name       Optional[string]      `json:"name"`
	Address    Optional[string]      `json:"address"`
	MarketCode Optional[string]      `json:"market_code"`
	ListingID  Optional[uuid.UUID]   `json:"listing_id"`
	Notes      Optional[string]      `json:"notes"`
	Units      Optional[[]UnitInput] `json:"units"`
}

// RentInput is the rent rule of a tenancy body.
type RentInput struct {
	Amount    *string `json:"amount"`
	Currency  string  `json:"currency"`
	Frequency string  `json:"frequency"`
	DueDay    *int    `json:"due_day"`
}

// DepositInput is the deposit of a tenancy body.
type DepositInput struct {
	Amount   *string `json:"amount"`
	Currency string  `json:"currency"`
}

// TenancyInput is the body of POST .../tenancies and PATCH .../tenancies/{tenancyId}.
type TenancyInput struct {
	PropertyID Optional[uuid.UUID]    `json:"property_id"`
	UnitID     Optional[uuid.UUID]    `json:"unit_id"`
	Tenants    Optional[[]uuid.UUID]  `json:"tenants"`
	Landlords  Optional[[]uuid.UUID]  `json:"landlords"`
	LeaseID    Optional[uuid.UUID]    `json:"lease_id"`
	Rent       Optional[RentInput]    `json:"rent"`
	Deposit    Optional[DepositInput] `json:"deposit"`
	StartDate  Optional[string]       `json:"start_date"`
	EndDate    Optional[string]       `json:"end_date"`
	Status     Optional[string]       `json:"status"`
	Notes      Optional[string]       `json:"notes"`
}

// EndInput is the body of POST .../tenancies/{tenancyId}/end.
type EndInput struct {
	EndDate string `json:"end_date"`
	Reason  string `json:"reason"`
}

func validDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Year() >= 1900 && t.Year() <= 2999 && t.Format(time.DateOnly) == s
}

func text(f map[string]string, field, v string, minLen, maxLen int) string {
	v = strings.TrimSpace(v)
	n := utf8.RuneCountInString(v)
	switch {
	case n < minLen && minLen > 0:
		f[field] = "is required"
	case n > maxLen:
		f[field] = fmt.Sprintf("must be at most %d characters", maxLen)
	}
	return v
}

// amount validates a decimal string in a currency.
func amount(f map[string]string, field string, raw *string, cur string) string {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		f[field] = "is required: a positive decimal string such as \"950.00\""
		return ""
	}
	r, problem := money.ParseAmount(*raw, cur)
	switch problem {
	case "":
		return money.Format(r, cur)
	case money.ProblemDecimals:
		if d := money.MinorUnit(cur); d == 0 {
			f[field] = "must be a whole number for " + cur + " (it has no minor unit)"
		} else {
			f[field] = fmt.Sprintf("must have at most %d decimals for %s", d, cur)
		}
	case money.ProblemZero:
		f[field] = "must be more than zero"
	default:
		f[field] = "must be a positive decimal string such as \"950.00\" (no sign, exponent or thousands separator)"
	}
	return ""
}

func currency(f map[string]string, field, raw string, allowed []string) string {
	c := strings.ToUpper(strings.TrimSpace(raw))
	switch {
	case c == "":
		f[field] = "is required"
	case !money.ValidCurrencyCode(c) || !slices.Contains(allowed, c):
		f[field] = "must be an ISO 4217 currency code from reference data, such as USD"
	default:
		return c
	}
	return ""
}

// ---------------------------------------------------------------------------
// Properties
// ---------------------------------------------------------------------------

func (s *Service) property(ctx context.Context, a access, id uuid.UUID) (Property, error) {
	p, err := s.store.Property(ctx, id)
	if err != nil {
		return Property{}, err
	}
	if !p.Book.Same(a.book) || p.DeletedAt != nil {
		return Property{}, ErrPropertyNotFound
	}
	return p, nil
}

func (s *Service) propertyViews(ctx context.Context, a access, list []Property) ([]PropertyView, error) {
	ids := make([]uuid.UUID, 0, len(list))
	for _, p := range list {
		ids = append(ids, p.ID)
	}
	uses, err := s.store.Uses(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]PropertyView, 0, len(list))
	for _, p := range list {
		v, err := s.propertyView(ctx, a, p, uses)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) propertyView(ctx context.Context, a access, p Property, uses []Use) (PropertyView, error) {
	v := PropertyView{ID: p.ID, Name: p.Name, Address: p.Address, MarketCode: p.MarketCode, Notes: p.Notes, Units: []UnitView{},
		CreatedBy: PersonRef{UserID: p.CreatedBy, Name: p.CreatedByName}, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	active := map[uuid.UUID]uuid.UUID{}
	for _, u := range uses {
		if u.PropertyID != p.ID {
			continue
		}
		v.OpenTenancies++
		if u.Status != StatusActive {
			continue
		}
		id := u.TenancyID
		if u.UnitID == nil {
			v.ActiveTenancyID = &id
		} else {
			active[*u.UnitID] = id
		}
	}
	for _, u := range p.LiveUnits() {
		uv := UnitView{ID: u.ID, Label: u.Label, Position: u.Position}
		if id, ok := active[u.ID]; ok {
			uv.ActiveTenancyID = &id
			v.OccupiedUnits++
		}
		v.Units = append(v.Units, uv)
	}
	v.UnitCount = len(v.Units)
	v.LettableUnits = max(v.UnitCount, 1)
	if v.ActiveTenancyID != nil && v.UnitCount == 0 {
		v.OccupiedUnits = 1
	}
	v.Can = PropertyCan{Edit: a.write, Delete: a.write && v.OpenTenancies == 0}
	if p.ListingID != nil {
		ref := &ListingRef{ID: *p.ListingID}
		if s.links != nil {
			got, ok, err := s.links.Listing(ctx, a.book, *p.ListingID)
			if err != nil {
				return PropertyView{}, err
			}
			if ok {
				ref = &got
			}
		}
		v.Listing = ref
	}
	return v, nil
}

func (s *Service) onePropertyView(ctx context.Context, a access, id uuid.UUID) (PropertyView, error) {
	p, err := s.property(ctx, a, id)
	if err != nil {
		return PropertyView{}, err
	}
	list, err := s.propertyViews(ctx, a, []Property{p})
	if err != nil {
		return PropertyView{}, err
	}
	return list[0], nil
}

// ListProperties serves GET .../properties.
func (s *Service) ListProperties(ctx context.Context, p *auth.Principal, bookID uuid.UUID, f PropertyFilter, page, size int) (PropertyPage, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return PropertyPage{}, err
	}
	f.Limit, f.Offset = size, (page-1)*size
	list, total, err := s.store.Properties(ctx, a.book, f)
	if err != nil {
		return PropertyPage{}, err
	}
	items, err := s.propertyViews(ctx, a, list)
	if err != nil {
		return PropertyPage{}, err
	}
	return PropertyPage{Items: items, Total: total, Page: page, PageSize: size}, nil
}

// GetProperty serves GET .../properties/{propertyId}.
func (s *Service) GetProperty(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (PropertyView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return PropertyView{}, err
	}
	return s.onePropertyView(ctx, a, id)
}

// applyProperty validates the property fields of a body onto p.
func (s *Service) applyProperty(ctx context.Context, a access, p *Property, in PropertyInput, create bool, f map[string]string) error {
	if create || in.Name.Set {
		p.Name = text(f, "name", in.Name.Value, 1, MaxName)
	}
	if create || in.Address.Set {
		p.Address = text(f, "address", in.Address.Value, 1, MaxAddress)
	}
	if create || in.MarketCode.Set {
		code := strings.ToUpper(strings.TrimSpace(in.MarketCode.Value))
		markets, err := s.allowedMarkets(ctx)
		if err != nil {
			return err
		}
		switch {
		case code == "":
			f["market_code"] = "is required"
		case !slices.Contains(markets, code):
			f["market_code"] = "must be the ISO 3166-1 code of a country in reference data, such as ZW"
		}
		p.MarketCode = code
	}
	if in.Notes.Set {
		p.Notes = text(f, "notes", in.Notes.Value, 0, MaxNotes)
	}
	if in.ListingID.Set {
		p.ListingID = nil
		if in.ListingID.Has() {
			id := in.ListingID.Value
			ok := false
			if s.links != nil {
				var err error
				if _, ok, err = s.links.Listing(ctx, a.book, id); err != nil {
					return err
				}
			}
			if !ok {
				f["listing_id"] = "is not a listing of these books"
			}
			p.ListingID = &id
		}
	}
	return nil
}

func unitLabels(f map[string]string, field string, labels []string, existing []Unit) []string {
	seen := map[string]bool{}
	for _, u := range existing {
		seen[strings.ToLower(u.Label)] = true
	}
	out := make([]string, 0, len(labels))
	for i, l := range labels {
		key := field
		if len(labels) > 1 || field == "units" {
			key = fmt.Sprintf("%s[%d].label", field, i)
		}
		l = text(f, key, l, 1, MaxUnitLabel)
		if l != "" && seen[strings.ToLower(l)] {
			f[key] = "is already a unit of this property"
		}
		seen[strings.ToLower(l)] = true
		out = append(out, l)
	}
	return out
}

// CreateProperty serves POST .../properties.
func (s *Service) CreateProperty(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in PropertyInput) (PropertyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return PropertyView{}, err
	}
	now := s.clock()
	prop := Property{ID: newID(), Book: a.book, CreatedBy: a.actor(), CreatedByName: a.p.Name, CreatedAt: now, UpdatedAt: now}
	f := map[string]string{}
	if err := s.applyProperty(ctx, a, &prop, in, true, f); err != nil {
		return PropertyView{}, err
	}
	var units []Unit
	if in.Units.Has() {
		if len(in.Units.Value) > MaxUnits {
			f["units"] = fmt.Sprintf("may hold at most %d units", MaxUnits)
		} else {
			labels := make([]string, 0, len(in.Units.Value))
			for _, u := range in.Units.Value {
				labels = append(labels, u.Label)
			}
			for i, l := range unitLabels(f, "units", labels, nil) {
				units = append(units, Unit{ID: newID(), PropertyID: prop.ID, Label: l, Position: i + 1, CreatedAt: now, UpdatedAt: now})
			}
		}
	}
	if err := invalid(f); err != nil {
		return PropertyView{}, err
	}
	if err := s.store.Apply(ctx, Change{Properties: []Property{prop}, Units: units}); err != nil {
		return PropertyView{}, err
	}
	return s.onePropertyView(ctx, a, prop.ID)
}

// UpdateProperty serves PATCH .../properties/{propertyId}.
func (s *Service) UpdateProperty(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in PropertyInput) (PropertyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return PropertyView{}, err
	}
	prop, err := s.property(ctx, a, id)
	if err != nil {
		return PropertyView{}, err
	}
	f := map[string]string{}
	if in.Units.Set {
		f["units"] = "cannot be changed here: use POST, PATCH and DELETE .../units"
	}
	if err := s.applyProperty(ctx, a, &prop, in, false, f); err != nil {
		return PropertyView{}, err
	}
	if err := invalid(f); err != nil {
		return PropertyView{}, err
	}
	prop.UpdatedAt = s.clock()
	if err := s.store.Apply(ctx, Change{Properties: []Property{prop}}); err != nil {
		return PropertyView{}, err
	}
	return s.onePropertyView(ctx, a, id)
}

// DeleteProperty serves DELETE .../properties/{propertyId}: archived.
func (s *Service) DeleteProperty(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) error {
	a, err := s.writer(p, bookID)
	if err != nil {
		return err
	}
	prop, err := s.property(ctx, a, id)
	if err != nil {
		return err
	}
	uses, err := s.store.Uses(ctx, []uuid.UUID{id})
	if err != nil {
		return err
	}
	if len(uses) > 0 {
		return ErrPropertyInUse
	}
	now := s.clock()
	prop.DeletedAt, prop.UpdatedAt = &now, now
	return s.store.Apply(ctx, Change{PropertyFree: []uuid.UUID{id}, Properties: []Property{prop}})
}

// AddUnit serves POST .../properties/{propertyId}/units.
func (s *Service) AddUnit(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in UnitInput) (PropertyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return PropertyView{}, err
	}
	prop, err := s.property(ctx, a, id)
	if err != nil {
		return PropertyView{}, err
	}
	live := prop.LiveUnits()
	f := map[string]string{}
	label := unitLabels(f, "label", []string{in.Label}, live)[0]
	if len(live) >= MaxUnits {
		f["label"] = fmt.Sprintf("a property holds at most %d units", MaxUnits)
	}
	if err := invalid(f); err != nil {
		return PropertyView{}, err
	}
	ch := Change{}
	if len(live) == 0 {
		// A property without units is one unit: it gains units only while no
		// open tenancy holds the whole of it.
		uses, err := s.store.Uses(ctx, []uuid.UUID{id})
		if err != nil {
			return PropertyView{}, err
		}
		if len(uses) > 0 {
			return PropertyView{}, ErrPropertyInUse
		}
		ch.PropertyFree = []uuid.UUID{id}
	}
	pos := 0
	for _, u := range prop.Units {
		pos = max(pos, u.Position)
	}
	now := s.clock()
	ch.Units = []Unit{{ID: newID(), PropertyID: id, Label: label, Position: pos + 1, CreatedAt: now, UpdatedAt: now}}
	prop.UpdatedAt = now
	ch.Properties = []Property{prop}
	if err := s.store.Apply(ctx, ch); err != nil {
		return PropertyView{}, err
	}
	return s.onePropertyView(ctx, a, id)
}

func liveUnit(prop Property, unitID uuid.UUID) (Unit, error) {
	u, ok := prop.Unit(unitID)
	if !ok || u.DeletedAt != nil {
		return Unit{}, ErrUnitNotFound
	}
	return u, nil
}

// RenameUnit serves PATCH .../properties/{propertyId}/units/{unitId}.
func (s *Service) RenameUnit(ctx context.Context, p *auth.Principal, bookID, id, unitID uuid.UUID, in UnitInput) (PropertyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return PropertyView{}, err
	}
	prop, err := s.property(ctx, a, id)
	if err != nil {
		return PropertyView{}, err
	}
	u, err := liveUnit(prop, unitID)
	if err != nil {
		return PropertyView{}, err
	}
	others := slices.DeleteFunc(prop.LiveUnits(), func(x Unit) bool { return x.ID == unitID })
	f := map[string]string{}
	u.Label = unitLabels(f, "label", []string{in.Label}, others)[0]
	if err := invalid(f); err != nil {
		return PropertyView{}, err
	}
	now := s.clock()
	u.UpdatedAt, prop.UpdatedAt = now, now
	if err := s.store.Apply(ctx, Change{Properties: []Property{prop}, Units: []Unit{u}}); err != nil {
		return PropertyView{}, err
	}
	return s.onePropertyView(ctx, a, id)
}

// DeleteUnit serves DELETE .../properties/{propertyId}/units/{unitId}: archived.
func (s *Service) DeleteUnit(ctx context.Context, p *auth.Principal, bookID, id, unitID uuid.UUID) (PropertyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return PropertyView{}, err
	}
	prop, err := s.property(ctx, a, id)
	if err != nil {
		return PropertyView{}, err
	}
	u, err := liveUnit(prop, unitID)
	if err != nil {
		return PropertyView{}, err
	}
	uses, err := s.store.Uses(ctx, []uuid.UUID{id})
	if err != nil {
		return PropertyView{}, err
	}
	for _, x := range uses {
		if x.UnitID != nil && *x.UnitID == unitID {
			return PropertyView{}, ErrUnitInUse
		}
	}
	now := s.clock()
	u.DeletedAt, u.UpdatedAt, prop.UpdatedAt = &now, now, now
	if err := s.store.Apply(ctx, Change{UnitFree: []uuid.UUID{unitID}, Properties: []Property{prop}, Units: []Unit{u}}); err != nil {
		return PropertyView{}, err
	}
	return s.onePropertyView(ctx, a, id)
}

// ---------------------------------------------------------------------------
// Tenancies
// ---------------------------------------------------------------------------

func (s *Service) tenancy(ctx context.Context, a access, id uuid.UUID) (Tenancy, error) {
	t, err := s.store.Tenancy(ctx, id)
	if err != nil {
		return Tenancy{}, err
	}
	if !t.Book.Same(a.book) {
		return Tenancy{}, ErrTenancyNotFound
	}
	return t, nil
}

type viewer struct {
	s          *Service
	book       Book
	properties map[uuid.UUID]Property
	contacts   map[uuid.UUID]*ContactInfo
	leases     map[uuid.UUID]*LeaseRef
}

func (s *Service) viewer(ctx context.Context, book Book, list []Tenancy) (*viewer, error) {
	v := &viewer{s: s, book: book, properties: map[uuid.UUID]Property{}, contacts: map[uuid.UUID]*ContactInfo{}, leases: map[uuid.UUID]*LeaseRef{}}
	var ids []uuid.UUID
	for _, t := range list {
		if !slices.Contains(ids, t.PropertyID) {
			ids = append(ids, t.PropertyID)
		}
	}
	if len(ids) > 0 {
		props, err := s.store.PropertiesByIDs(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, p := range props {
			v.properties[p.ID] = p
		}
	}
	return v, nil
}

func (v *viewer) contact(ctx context.Context, book Book, id uuid.UUID) (*ContactInfo, error) {
	if c, ok := v.contacts[id]; ok {
		return c, nil
	}
	var ref *ContactInfo
	if v.s.links != nil {
		got, ok, err := v.s.links.Contact(ctx, book, id)
		if err != nil {
			return nil, err
		}
		if ok {
			ref = &got
		}
	}
	v.contacts[id] = ref
	return ref, nil
}

func (v *viewer) lease(ctx context.Context, book Book, id *uuid.UUID) (*LeaseRef, error) {
	if id == nil {
		return nil, nil
	}
	if r, ok := v.leases[*id]; ok {
		return r, nil
	}
	ref := &LeaseRef{ID: *id}
	if v.s.links != nil {
		got, ok, err := v.s.links.Lease(ctx, book, *id)
		if err != nil {
			return nil, err
		}
		if ok {
			ref = &got
		}
	}
	v.leases[*id] = ref
	return ref, nil
}

func (v *viewer) parties(ctx context.Context, t Tenancy, role string) ([]ContactRef, error) {
	out := []ContactRef{}
	for _, p := range t.Parties {
		if p.Role != role {
			continue
		}
		c, err := v.contact(ctx, t.Book, p.ContactID)
		if err != nil {
			return nil, err
		}
		ref := ContactRef{ID: p.ContactID, Name: p.Name}
		if c != nil {
			ref.Name, ref.Live = c.Name, true
		}
		out = append(out, ref)
	}
	return out, nil
}

func rentView(t Tenancy) RentView {
	return RentView{Amount: t.RentAmount, Currency: t.RentCurrency, MinorUnit: money.MinorUnit(t.RentCurrency), Frequency: t.RentFrequency,
		FrequencyLabel: FrequencyLabel(t.RentFrequency), DueDay: t.RentDueDay}
}

func depositView(t Tenancy) *MoneyView {
	if t.DepositAmount == "" {
		return nil
	}
	return &MoneyView{Amount: t.DepositAmount, Currency: t.DepositCurrency, MinorUnit: money.MinorUnit(t.DepositCurrency)}
}

func person(id *uuid.UUID, name string, at *time.Time) *PersonRef {
	if at == nil {
		return nil
	}
	return &PersonRef{UserID: id, Name: name}
}

func (v *viewer) view(ctx context.Context, a access, t Tenancy) (TenancyView, error) {
	prop := v.properties[t.PropertyID]
	out := TenancyView{ID: t.ID, Status: t.Status, StatusLabel: StatusLabel(t.Status),
		Property: TenancyPropertyRef{ID: t.PropertyID, Name: prop.Name, Address: prop.Address, MarketCode: prop.MarketCode, Archived: prop.DeletedAt != nil},
		Rent:     rentView(t), Deposit: depositView(t), StartDate: t.StartDate, EndDate: strPtr(t.EndDate), EndedOn: strPtr(t.EndedOn),
		EndReason: strPtr(t.EndReason), Notes: t.Notes, ActivatedAt: t.ActivatedAt, ActivatedBy: person(t.ActivatedBy, t.ActivatedByName, t.ActivatedAt),
		EndedAt: t.EndedAt, EndedBy: person(t.EndedBy, t.EndedByName, t.EndedAt), CreatedBy: PersonRef{UserID: t.CreatedBy, Name: t.CreatedByName},
		CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
		Can: TenancyCan{Edit: a.write, Activate: a.write && t.Status == StatusDraft, End: a.write && t.Status == StatusActive,
			Delete: a.write && t.Status == StatusDraft}}
	if t.UnitID != nil {
		u, _ := prop.Unit(*t.UnitID)
		out.Unit = &TenancyUnitRef{ID: *t.UnitID, Label: u.Label, Archived: u.DeletedAt != nil}
	}
	var err error
	if out.Tenants, err = v.parties(ctx, t, RoleTenant); err != nil {
		return TenancyView{}, err
	}
	if out.Landlords, err = v.parties(ctx, t, RoleLandlord); err != nil {
		return TenancyView{}, err
	}
	if out.Lease, err = v.lease(ctx, t.Book, t.LeaseID); err != nil {
		return TenancyView{}, err
	}
	return out, nil
}

func (s *Service) oneTenancyView(ctx context.Context, a access, id uuid.UUID) (TenancyView, error) {
	t, err := s.tenancy(ctx, a, id)
	if err != nil {
		return TenancyView{}, err
	}
	v, err := s.viewer(ctx, a.book, []Tenancy{t})
	if err != nil {
		return TenancyView{}, err
	}
	return v.view(ctx, a, t)
}

// ListTenancies serves GET .../tenancies.
func (s *Service) ListTenancies(ctx context.Context, p *auth.Principal, bookID uuid.UUID, f TenancyFilter, page, size int) (TenancyPage, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return TenancyPage{}, err
	}
	f.Limit, f.Offset = size, (page-1)*size
	list, total, counts, err := s.store.Tenancies(ctx, a.book, f)
	if err != nil {
		return TenancyPage{}, err
	}
	v, err := s.viewer(ctx, a.book, list)
	if err != nil {
		return TenancyPage{}, err
	}
	out := TenancyPage{Items: make([]TenancyView, 0, len(list)), Total: total, Page: page, PageSize: size, StatusCounts: map[string]int{}}
	for _, st := range Statuses() {
		out.StatusCounts[st] = counts[st]
	}
	for _, t := range list {
		tv, err := v.view(ctx, a, t)
		if err != nil {
			return TenancyPage{}, err
		}
		out.Items = append(out.Items, tv)
	}
	return out, nil
}

// GetTenancy serves GET .../tenancies/{tenancyId}.
func (s *Service) GetTenancy(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (TenancyView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return TenancyView{}, err
	}
	return s.oneTenancyView(ctx, a, id)
}

// Which fields may change in which status.
var (
	activeFields = []string{"tenants", "landlords", "lease_id", "deposit", "end_date", "notes"}
	closedFields = []string{"notes"}
)

func (in TenancyInput) setFields() []string {
	var out []string
	add := func(name string, set bool) {
		if set {
			out = append(out, name)
		}
	}
	add("property_id", in.PropertyID.Set)
	add("unit_id", in.UnitID.Set)
	add("tenants", in.Tenants.Set)
	add("landlords", in.Landlords.Set)
	add("lease_id", in.LeaseID.Set)
	add("rent", in.Rent.Set)
	add("deposit", in.Deposit.Set)
	add("start_date", in.StartDate.Set)
	add("end_date", in.EndDate.Set)
	add("notes", in.Notes.Set)
	return out
}

// parties validates the tenant and landlord lists (live contacts of the book).
func (s *Service) parties(ctx context.Context, a access, f map[string]string, field string, ids []uuid.UUID, minN, maxN int) ([]Party, error) {
	role := RoleTenant
	if field == "landlords" {
		role = RoleLandlord
	}
	switch {
	case len(ids) < minN:
		f[field] = "needs at least one contact"
		return nil, nil
	case len(ids) > maxN:
		f[field] = fmt.Sprintf("may hold at most %d contacts", maxN)
		return nil, nil
	}
	out := make([]Party, 0, len(ids))
	seen := map[uuid.UUID]bool{}
	for i, id := range ids {
		key := fmt.Sprintf("%s[%d]", field, i)
		if seen[id] {
			f[key] = "is listed twice"
			continue
		}
		seen[id] = true
		var c ContactInfo
		ok := false
		if s.links != nil {
			var err error
			if c, ok, err = s.links.Contact(ctx, a.book, id); err != nil {
				return nil, err
			}
		}
		if !ok {
			f[key] = "is not a contact of these books"
			continue
		}
		out = append(out, Party{ContactID: id, Role: role, Name: c.Name})
	}
	return out, nil
}

// assemble validates a body onto a tenancy (cur nil: a new one).
func (s *Service) assemble(ctx context.Context, a access, cur *Tenancy, in TenancyInput) (Tenancy, Property, error) {
	f := map[string]string{}
	create := cur == nil
	var t Tenancy
	if create {
		t = Tenancy{Status: StatusDraft}
		if in.Status.Has() && in.Status.Value != StatusDraft {
			f["status"] = "a tenancy starts as DRAFT: activate it with POST .../activate"
		}
	} else {
		t = *cur
		t.Parties = slices.Clone(cur.Parties)
		if in.Status.Set {
			f["status"] = "changes only through POST .../activate and POST .../end"
		}
		var allowed []string
		switch t.Status {
		case StatusDraft:
		case StatusActive:
			allowed = activeFields
		default:
			allowed = closedFields
		}
		if allowed != nil {
			for _, name := range in.setFields() {
				if slices.Contains(allowed, name) {
					continue
				}
				if t.Status == StatusActive {
					f[name] = "is fixed once the tenancy is active"
				} else {
					f[name] = "cannot change once the tenancy has ended"
				}
			}
		}
	}
	if len(f) > 0 {
		return Tenancy{}, Property{}, invalid(f)
	}

	// Property and unit.
	var prop Property
	propertyChanged := create || in.PropertyID.Set
	if propertyChanged {
		if !in.PropertyID.Has() {
			f["property_id"] = "is required"
		} else {
			got, err := s.property(ctx, a, in.PropertyID.Value)
			switch {
			case err == nil:
				prop = got
				t.PropertyID = got.ID
			case errors.Is(err, ErrPropertyNotFound):
				f["property_id"] = "is not a property of these books"
			default:
				return Tenancy{}, Property{}, err
			}
		}
	} else {
		got, err := s.store.Property(ctx, t.PropertyID)
		if err != nil {
			return Tenancy{}, Property{}, err
		}
		prop = got
	}
	if in.UnitID.Set {
		t.UnitID = nil
		if in.UnitID.Has() {
			id := in.UnitID.Value
			t.UnitID = &id
		}
	}
	if (propertyChanged || in.UnitID.Set) && prop.ID != uuid.Nil {
		live := prop.LiveUnits()
		switch {
		case len(live) == 0 && t.UnitID != nil:
			f["unit_id"] = "must be null: this property has no units (it is let as a whole)"
		case len(live) > 0 && t.UnitID == nil:
			f["unit_id"] = "is required: this property has units"
		case t.UnitID != nil && !slices.ContainsFunc(live, func(u Unit) bool { return u.ID == *t.UnitID }):
			f["unit_id"] = "is not a unit of this property"
		}
	}

	// Parties.
	tenants, landlords := t.PartyIDs(RoleTenant), t.PartyIDs(RoleLandlord)
	var newTenants, newLandlords []Party
	var err error
	if create || in.Tenants.Set {
		if newTenants, err = s.parties(ctx, a, f, "tenants", in.Tenants.Value, 1, MaxTenants); err != nil {
			return Tenancy{}, Property{}, err
		}
		tenants = in.Tenants.Value
	}
	if in.Landlords.Set {
		if newLandlords, err = s.parties(ctx, a, f, "landlords", in.Landlords.Value, 0, MaxLandlords); err != nil {
			return Tenancy{}, Property{}, err
		}
		landlords = in.Landlords.Value
	}
	for _, id := range landlords {
		if slices.Contains(tenants, id) {
			f["landlords"] = "a contact cannot be both a tenant and a landlord"
		}
	}
	if create || in.Tenants.Set || in.Landlords.Set {
		var parties []Party
		if create || in.Tenants.Set {
			parties = append(parties, newTenants...)
		} else {
			parties = append(parties, keep(t.Parties, RoleTenant)...)
		}
		if in.Landlords.Set {
			parties = append(parties, newLandlords...)
		} else {
			parties = append(parties, keep(t.Parties, RoleLandlord)...)
		}
		t.Parties = parties
	}

	// Lease.
	if in.LeaseID.Set {
		t.LeaseID = nil
		if in.LeaseID.Has() {
			id := in.LeaseID.Value
			ok := false
			if s.links != nil {
				if _, ok, err = s.links.Lease(ctx, a.book, id); err != nil {
					return Tenancy{}, Property{}, err
				}
			}
			if !ok {
				f["lease_id"] = "is not a lease of these books"
			}
			t.LeaseID = &id
		}
	}

	// Rent and deposit.
	curs, err := s.allowedCurrencies(ctx)
	if err != nil {
		return Tenancy{}, Property{}, err
	}
	if create || in.Rent.Set {
		if !in.Rent.Has() {
			f["rent"] = "is required: {amount, currency, frequency, due_day}"
		} else {
			r := in.Rent.Value
			t.RentCurrency = currency(f, "rent.currency", r.Currency, curs)
			if t.RentCurrency != "" {
				t.RentAmount = amount(f, "rent.amount", r.Amount, t.RentCurrency)
			}
			t.RentFrequency = r.Frequency
			if !slices.Contains(Frequencies(), r.Frequency) {
				f["rent.frequency"] = "must be " + strings.Join(Frequencies(), ", ")
			}
			switch {
			case r.DueDay == nil:
				f["rent.due_day"] = "is required"
			case *r.DueDay < 1 || *r.DueDay > MaxDueDay(r.Frequency):
				if r.Frequency == FrequencyWeekly {
					f["rent.due_day"] = "must be a weekday from 1 (Monday) to 7 (Sunday) for WEEKLY rent"
				} else {
					f["rent.due_day"] = "must be a day of the month from 1 to 31 (a shorter month uses its last day)"
				}
			default:
				t.RentDueDay = *r.DueDay
			}
		}
	}
	if in.Deposit.Set {
		t.DepositAmount, t.DepositCurrency = "", ""
		if in.Deposit.Has() {
			d := in.Deposit.Value
			c := currency(f, "deposit.currency", d.Currency, curs)
			if c != "" && t.RentCurrency != "" && c != t.RentCurrency {
				f["deposit.currency"] = "must be the rent currency (" + t.RentCurrency + ")"
			}
			if c != "" {
				t.DepositAmount, t.DepositCurrency = amount(f, "deposit.amount", d.Amount, c), c
			}
		}
	} else if t.DepositCurrency != "" && t.RentCurrency != "" && t.DepositCurrency != t.RentCurrency {
		f["deposit"] = "is in " + t.DepositCurrency + ": the deposit must be in the rent currency; send it again or null"
	}

	// Dates.
	if create || in.StartDate.Set {
		t.StartDate = strings.TrimSpace(in.StartDate.Value)
		if !validDate(t.StartDate) {
			f["start_date"] = "must be a date (YYYY-MM-DD)"
		}
	}
	if in.EndDate.Set {
		t.EndDate = ""
		if in.EndDate.Has() {
			t.EndDate = strings.TrimSpace(in.EndDate.Value)
			if !validDate(t.EndDate) {
				f["end_date"] = "must be a date (YYYY-MM-DD) or null"
			}
		}
	}
	if _, bad := f["start_date"]; !bad && t.EndDate != "" && f["end_date"] == "" && t.EndDate < t.StartDate {
		f["end_date"] = "must be on or after start_date"
	}
	if in.Notes.Set {
		t.Notes = text(f, "notes", in.Notes.Value, 0, MaxNotes)
	}
	if err := invalid(f); err != nil {
		return Tenancy{}, Property{}, err
	}
	return t, prop, nil
}

func keep(parties []Party, role string) []Party {
	var out []Party
	for _, p := range parties {
		if p.Role == role {
			out = append(out, p)
		}
	}
	return out
}

// CreateTenancy serves POST .../tenancies.
func (s *Service) CreateTenancy(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in TenancyInput) (TenancyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return TenancyView{}, err
	}
	t, prop, err := s.assemble(ctx, a, nil, in)
	if err != nil {
		return TenancyView{}, err
	}
	now := s.clock()
	t.ID, t.Book, t.BookName, t.CreatedBy, t.CreatedByName, t.CreatedAt, t.UpdatedAt = newID(), a.book, a.name, a.actor(), a.p.Name, now, now
	ch := Change{Tenancies: []Tenancy{t}, Audit: s.event(ctx, a, orgaudit.ActionTenancyCreated, t.ID, Label(prop, t), nil, tenancyAudit(t))}
	if err := s.store.Apply(ctx, ch); err != nil {
		return TenancyView{}, err
	}
	return s.oneTenancyView(ctx, a, t.ID)
}

// UpdateTenancy serves PATCH .../tenancies/{tenancyId}.
func (s *Service) UpdateTenancy(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in TenancyInput) (TenancyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return TenancyView{}, err
	}
	cur, err := s.tenancy(ctx, a, id)
	if err != nil {
		return TenancyView{}, err
	}
	t, _, err := s.assemble(ctx, a, &cur, in)
	if err != nil {
		return TenancyView{}, err
	}
	t.UpdatedAt = s.clock()
	ch := Change{ExpectStatus: map[uuid.UUID]string{id: cur.Status}, Tenancies: []Tenancy{t}}
	if t.Status == StatusActive && t.EndDate != cur.EndDate {
		if err := s.resync(ctx, &ch, t); err != nil {
			return TenancyView{}, err
		}
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return TenancyView{}, err
	}
	return s.oneTenancyView(ctx, a, id)
}

// DeleteTenancy serves DELETE .../tenancies/{tenancyId} (DRAFT only).
func (s *Service) DeleteTenancy(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) error {
	a, err := s.writer(p, bookID)
	if err != nil {
		return err
	}
	t, err := s.tenancy(ctx, a, id)
	if err != nil {
		return err
	}
	if t.Status != StatusDraft {
		return &StateError{Status: t.Status}
	}
	prop, err := s.store.Property(ctx, t.PropertyID)
	if err != nil {
		return err
	}
	return s.store.Apply(ctx, Change{ExpectStatus: map[uuid.UUID]string{id: StatusDraft}, DeleteTenancies: []uuid.UUID{id},
		Audit: s.event(ctx, a, orgaudit.ActionTenancyDeleted, id, Label(prop, t), tenancyAudit(t), nil)})
}

// Activate serves POST .../tenancies/{tenancyId}/activate.
func (s *Service) Activate(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (TenancyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return TenancyView{}, err
	}
	t, err := s.tenancy(ctx, a, id)
	if err != nil {
		return TenancyView{}, err
	}
	if t.Status != StatusDraft {
		return TenancyView{}, &StateError{Status: t.Status}
	}
	prop, err := s.store.Property(ctx, t.PropertyID)
	if err != nil {
		return TenancyView{}, err
	}
	uses, err := s.store.Uses(ctx, []uuid.UUID{t.PropertyID})
	if err != nil {
		return TenancyView{}, err
	}
	for _, u := range uses {
		if u.Status == StatusActive && u.TenancyID != t.ID && ptrEq(u.UnitID, t.UnitID) {
			other := u.TenancyID
			return TenancyView{}, &OccupiedError{TenancyID: &other}
		}
	}
	before := tenancyAudit(t)
	now := s.clock()
	t.Status, t.ActivatedAt, t.ActivatedBy, t.ActivatedByName, t.UpdatedAt = StatusActive, &now, a.actor(), a.p.Name, now
	// The rent schedule is generated in the activation's transaction.
	dues, _ := planSchedule(t, nil, s.todayIn(ctx, prop.MarketCode), now)
	ch := Change{ExpectStatus: map[uuid.UUID]string{id: StatusDraft}, Tenancies: []Tenancy{t}, Dues: dues,
		Audit: s.event(ctx, a, orgaudit.ActionTenancyActivated, id, Label(prop, t), before, tenancyAudit(t))}
	if err := s.store.Apply(ctx, ch); err != nil {
		return TenancyView{}, err
	}
	return s.oneTenancyView(ctx, a, id)
}

// End serves POST .../tenancies/{tenancyId}/end.
func (s *Service) End(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in EndInput) (TenancyView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return TenancyView{}, err
	}
	t, err := s.tenancy(ctx, a, id)
	if err != nil {
		return TenancyView{}, err
	}
	if t.Status != StatusActive {
		return TenancyView{}, &StateError{Status: t.Status}
	}
	f := map[string]string{}
	ended := strings.TrimSpace(in.EndDate)
	switch {
	case !validDate(ended):
		f["end_date"] = "must be a date (YYYY-MM-DD)"
	case ended < t.StartDate:
		f["end_date"] = "must be on or after the start date (" + t.StartDate + ")"
	}
	reason := text(f, "reason", in.Reason, 1, MaxReason)
	if err := invalid(f); err != nil {
		return TenancyView{}, err
	}
	prop, err := s.store.Property(ctx, t.PropertyID)
	if err != nil {
		return TenancyView{}, err
	}
	before := tenancyAudit(t)
	now := s.clock()
	t.Status = StatusEnded
	if t.EndDate != "" && ended < t.EndDate {
		t.Status = StatusTerminated
	}
	t.EndedOn, t.EndReason, t.EndedAt, t.EndedBy, t.EndedByName, t.UpdatedAt = ended, reason, &now, a.actor(), a.p.Name, now
	after := tenancyAudit(t)
	after["reason"] = reason
	ch := Change{ExpectStatus: map[uuid.UUID]string{id: StatusActive}, Tenancies: []Tenancy{t},
		Audit: s.event(ctx, a, orgaudit.ActionTenancyEnded, id, Label(prop, t), before, after)}
	if err := s.resync(ctx, &ch, t); err != nil {
		return TenancyView{}, err
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return TenancyView{}, err
	}
	return s.oneTenancyView(ctx, a, id)
}

// ---------------------------------------------------------------------------
// The tenant's own tenancies
// ---------------------------------------------------------------------------

// mine returns the caller's tenancies: not DRAFT, with a tenant contact of
// the tenancy's own book whose email is the caller's verified email.
func (s *Service) mine(ctx context.Context, p *auth.Principal) ([]Tenancy, error) {
	if p == nil {
		return nil, ErrUnauthenticated
	}
	if s.account == nil || s.links == nil {
		return nil, nil
	}
	email, verified, err := s.account(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !verified || email == "" {
		return nil, nil
	}
	contacts, err := s.links.ContactsWithEmail(ctx, email)
	if err != nil || len(contacts) == 0 {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(contacts))
	for _, c := range contacts {
		ids = append(ids, c.ID)
	}
	list, err := s.store.TenanciesOfTenants(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []Tenancy
	for _, t := range list {
		if t.Status == StatusDraft {
			continue
		}
		if slices.ContainsFunc(contacts, func(c ContactInfo) bool { return c.Book.Same(t.Book) && t.HasTenant(c.ID) }) {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartDate != out[j].StartDate {
			return out[i].StartDate > out[j].StartDate
		}
		return out[i].ID.String() > out[j].ID.String()
	})
	return out, nil
}

func (v *viewer) mine(ctx context.Context, t Tenancy) (MyTenancyView, error) {
	prop := v.properties[t.PropertyID]
	out := MyTenancyView{ID: t.ID, Status: t.Status, StatusLabel: StatusLabel(t.Status),
		ManagedBy: ManagerRef{Kind: ManagerOrganisation, Name: t.BookName},
		Property:  MyPropertyRef{Name: prop.Name, Address: prop.Address, MarketCode: prop.MarketCode},
		Tenants:   []NameRef{}, Landlords: []NameRef{}, Rent: rentView(t), Deposit: depositView(t), StartDate: t.StartDate,
		EndDate: strPtr(t.EndDate), EndedOn: strPtr(t.EndedOn)}
	if t.Book.OrganisationID == nil {
		out.ManagedBy.Kind = ManagerDirectOwner
	}
	if t.UnitID != nil {
		u, _ := prop.Unit(*t.UnitID)
		out.Unit = &LabelRef{Label: u.Label}
	}
	for _, role := range []string{RoleTenant, RoleLandlord} {
		refs, err := v.parties(ctx, t, role)
		if err != nil {
			return MyTenancyView{}, err
		}
		for _, r := range refs {
			if role == RoleTenant {
				out.Tenants = append(out.Tenants, NameRef{Name: r.Name})
			} else {
				out.Landlords = append(out.Landlords, NameRef{Name: r.Name})
			}
		}
	}
	lease, err := v.lease(ctx, t.Book, t.LeaseID)
	if err != nil {
		return MyTenancyView{}, err
	}
	out.Lease = lease
	return out, nil
}

// MyTenancies serves GET /v1/me/tenancies.
func (s *Service) MyTenancies(ctx context.Context, p *auth.Principal) ([]MyTenancyView, error) {
	list, err := s.mine(ctx, p)
	if err != nil {
		return nil, err
	}
	v, err := s.viewer(ctx, Book{}, list)
	if err != nil {
		return nil, err
	}
	out := make([]MyTenancyView, 0, len(list))
	for _, t := range list {
		mv, err := v.mine(ctx, t)
		if err != nil {
			return nil, err
		}
		out = append(out, mv)
	}
	return out, nil
}

// MyTenancy serves GET /v1/me/tenancies/{id}: anybody else's is ErrTenancyNotFound.
func (s *Service) MyTenancy(ctx context.Context, p *auth.Principal, id uuid.UUID) (MyTenancyView, error) {
	list, err := s.mine(ctx, p)
	if err != nil {
		return MyTenancyView{}, err
	}
	for _, t := range list {
		if t.ID == id {
			v, err := s.viewer(ctx, Book{}, []Tenancy{t})
			if err != nil {
				return MyTenancyView{}, err
			}
			return v.mine(ctx, t)
		}
	}
	return MyTenancyView{}, ErrTenancyNotFound
}
