package analytics

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
)

// PeriodQuery is a report's period as asked for.
type PeriodQuery struct {
	Name string
	// From and To are the custom period's days (YYYY-MM-DD), inclusive.
	From, To string
}

// ParsePeriod validates period, from and to. The default is 30d.
func ParsePeriod(period, from, to string) (PeriodQuery, error) {
	fields := map[string]string{}
	q := PeriodQuery{Name: period}
	if q.Name == "" {
		q.Name = Period30d
	}
	switch q.Name {
	case Period24h, Period7d, Period30d:
		if from != "" || to != "" {
			fields["period"] = "from and to are only used with period=custom"
		}
	case PeriodCustom:
		f, errF := time.Parse(dayLayout, from)
		t, errT := time.Parse(dayLayout, to)
		switch {
		case errF != nil:
			fields["from"] = "must be a date (YYYY-MM-DD)"
		case errT != nil:
			fields["to"] = "must be a date (YYYY-MM-DD)"
		case t.Before(f):
			fields["to"] = "must not be before from"
		case int(t.Sub(f).Hours()/24)+1 > MaxCustomDays:
			fields["to"] = fmt.Sprintf("a custom period is at most %d days", MaxCustomDays)
		}
		q.From, q.To = from, to
	default:
		fields["period"] = "must be 24h, 7d, 30d or custom"
	}
	if len(fields) > 0 {
		return PeriodQuery{}, &ValidationError{Fields: fields}
	}
	return q, nil
}

// window is a resolved period in one time zone.
type window struct {
	name     string
	from, to string
	since    time.Time // 24h only
	zone     string
}

func (s *Service) resolve(q PeriodQuery, loc *time.Location) window {
	now := s.now().UTC()
	w := window{name: q.Name, zone: loc.String()}
	today := LocalDay(now, loc)
	switch q.Name {
	case Period24h:
		w.since = now.Add(-24 * time.Hour)
		w.from, w.to = LocalDay(w.since, loc), today
	case Period7d:
		w.from, w.to = addDays(today, -6), today
	case PeriodCustom:
		w.from, w.to = q.From, q.To
	default:
		w.from, w.to = addDays(today, -29), today
	}
	return w
}

// collect reads the rows of a window: raw events for 24h (near real time),
// the rollups otherwise. listingIDs nil means every listing (admin).
func (s *Service) collect(ctx context.Context, w window, listingIDs []uuid.UUID, markets []string) ([]DayRow, *time.Time, error) {
	if listingIDs != nil && len(listingIDs) == 0 || markets != nil && len(markets) == 0 {
		refreshed, err := s.refreshedFor(ctx, w)
		return nil, refreshed, err
	}
	if w.name == Period24h {
		rows, err := s.store.Aggregate(ctx, AggregateFilter{ListingIDs: listingIDs, MarketCodes: markets, Since: w.since})
		if err != nil {
			return nil, nil, fmt.Errorf("aggregate analytics events: %w", err)
		}
		now := s.now().UTC()
		return rows, &now, nil
	}
	rows, err := s.store.Rollups(ctx, RollupFilter{ListingIDs: listingIDs, MarketCodes: markets, FromDay: w.from, ToDay: w.to})
	if err != nil {
		return nil, nil, fmt.Errorf("read analytics rollups: %w", err)
	}
	refreshed, err := s.store.LastRefresh(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("read analytics refresh time: %w", err)
	}
	return rows, refreshed, nil
}

func (s *Service) refreshedFor(ctx context.Context, w window) (*time.Time, error) {
	if w.name == Period24h {
		now := s.now().UTC()
		return &now, nil
	}
	return s.store.LastRefresh(ctx)
}

// Report is the part every analytics report shares (one metric contract for
// the advertiser and the administrators).
type Report struct {
	Period   string `json:"period"`
	From     string `json:"from"`
	To       string `json:"to"`
	Timezone string `json:"timezone"`
	// NearRealTime is true for 24h, read from raw events: Label then says
	// the figures may change.
	NearRealTime         bool         `json:"near_real_time"`
	Label                *string      `json:"label"`
	Totals               Totals       `json:"totals"`
	Daily                []DailyPoint `json:"daily"`
	Conversion           *float64     `json:"conversion"`
	ConversionDefinition string       `json:"conversion_definition"`
	CTR                  *float64     `json:"ctr"`
	CTRDefinition        string       `json:"ctr_definition"`
	Sources              []KeyCount   `json:"sources"`
	Devices              []KeyCount   `json:"devices"`
	FilteredBotEvents    int          `json:"filtered_bot_events"`
	LateEventsDropped    int          `json:"late_events_dropped"`
	RefreshedAt          *time.Time   `json:"refreshed_at"`
	DefinitionsPath      string       `json:"definitions_path"`
	rowsByListing        map[uuid.UUID]*listingAgg
}

// DailyPoint is one market-local day of the daily series.
type DailyPoint struct {
	Date          string `json:"date"`
	Views         int    `json:"views"`
	UniqueViews   int    `json:"unique_views"`
	Impressions   int    `json:"impressions"`
	ContactClicks int    `json:"contact_clicks"`
	Saves         int    `json:"saves"`
	Enquiries     int    `json:"enquiries"`
}

// KeyCount is one entry of a breakdown (sources, devices), largest first.
type KeyCount struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// keyCounts lists every known key (and unknown), largest count first, then by key.
func keyCounts(m map[string]int) []KeyCount {
	out := make([]KeyCount, 0, len(m))
	for k, v := range m {
		out = append(out, KeyCount{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

type listingAgg struct {
	totals   Totals
	filtered int
	late     int
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	v := math.Round(float64(n)/float64(d)*10000) / 10000
	return &v
}

func breakdown(keys []string) map[string]int {
	m := map[string]int{Unknown: 0}
	for _, k := range keys {
		m[k] = 0
	}
	return m
}

// build sums rows into a report over w. The series has every day of the
// window (and any other day a row has, for 24h across time zones).
func build(w window, rows []DayRow, refreshed *time.Time) Report {
	r := Report{Period: w.name, From: w.from, To: w.to, Timezone: w.zone, NearRealTime: w.name == Period24h,
		ConversionDefinition: ConversionDefinition, CTRDefinition: CTRDefinition, DefinitionsPath: DefinitionsPath,
		RefreshedAt: refreshed, Daily: []DailyPoint{},
		rowsByListing: map[uuid.UUID]*listingAgg{}}
	if r.NearRealTime {
		label := NearRealTimeLabel
		r.Label = &label
	}
	sources, devices := breakdown(Sources()), breakdown(Devices())
	days := map[string]*Totals{}
	if w.from != "" && w.to != "" && w.from <= w.to {
		for d := w.from; d <= w.to; d = addDays(d, 1) {
			days[d] = &Totals{}
		}
	}
	for _, row := range rows {
		r.Totals.add(row.Totals)
		r.FilteredBotEvents += row.FilteredBotEvents
		r.LateEventsDropped += row.LateEventsDropped
		for k, v := range row.Sources {
			sources[k] += v
		}
		for k, v := range row.Devices {
			devices[k] += v
		}
		if days[row.Day] == nil {
			days[row.Day] = &Totals{}
		}
		days[row.Day].add(row.Totals)
		a := r.rowsByListing[row.ListingID]
		if a == nil {
			a = &listingAgg{}
			r.rowsByListing[row.ListingID] = a
		}
		a.totals.add(row.Totals)
		a.filtered += row.FilteredBotEvents
		a.late += row.LateEventsDropped
	}
	keys := make([]string, 0, len(days))
	for d := range days {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	for _, d := range keys {
		t := days[d]
		r.Daily = append(r.Daily, DailyPoint{Date: d, Views: t.Views, UniqueViews: t.UniqueViews, Impressions: t.Impressions,
			ContactClicks: t.ContactClicks, Saves: t.Saves, Enquiries: t.Enquiries})
	}
	r.Sources, r.Devices = keyCounts(sources), keyCounts(devices)
	r.Conversion = ratio(r.Totals.Enquiries, r.Totals.Views)
	r.CTR = ratio(r.Totals.ContactClicks, r.Totals.Views)
	return r
}

// ListingRef names a listing in a report.
type ListingRef struct {
	ID         uuid.UUID `json:"id"`
	Title      string    `json:"title"`
	Reference  string    `json:"reference"`
	MarketCode string    `json:"market_code"`
}

func refOf(l ListingInfo) ListingRef {
	return ListingRef{ID: l.ID, Title: l.Title, Reference: l.Reference, MarketCode: l.MarketCode}
}

// TopListing is one entry of a top-listings list. Slug is set only while
// the listing is public (it links to the public page).
type TopListing struct {
	ListingID     uuid.UUID `json:"listing_id"`
	Title         string    `json:"title"`
	Reference     string    `json:"reference"`
	Slug          *string   `json:"slug"`
	MarketCode    string    `json:"market_code"`
	Views         int       `json:"views"`
	UniqueViews   int       `json:"unique_views"`
	ContactClicks int       `json:"contact_clicks"`
	Enquiries     int       `json:"enquiries"`
}

func topOf(l ListingInfo, t Totals) TopListing {
	x := TopListing{ListingID: l.ID, Title: l.Title, Reference: l.Reference, MarketCode: l.MarketCode,
		Views: t.Views, UniqueViews: t.UniqueViews, ContactClicks: t.ContactClicks, Enquiries: t.Enquiries}
	if l.Public && l.Slug != "" {
		slug := l.Slug
		x.Slug = &slug
	}
	return x
}

func (r Report) top(info map[uuid.UUID]ListingInfo) []TopListing {
	out := []TopListing{}
	for id, a := range r.rowsByListing {
		l, ok := info[id]
		if !ok {
			l = ListingInfo{ID: id}
		}
		out = append(out, topOf(l, a.totals))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Views != b.Views {
			return a.Views > b.Views
		}
		if a.UniqueViews != b.UniqueViews {
			return a.UniqueViews > b.UniqueViews
		}
		if a.Enquiries != b.Enquiries {
			return a.Enquiries > b.Enquiries
		}
		return a.ListingID.String() < b.ListingID.String()
	})
	if len(out) > TopListings {
		out = out[:TopListings]
	}
	return out
}

// ListingReport is GET /v1/analytics/listings/{id}.
type ListingReport struct {
	Listing ListingRef `json:"listing"`
	Report
}

// mayReadListing is who may read a listing's figures: its lister (the
// approved direct owner), an OWNER or MANAGER of its organisation, or its
// assigned agent — auth.MayEditListing, the manage API's rule for the
// listing's own data. Everyone else is told it does not exist.
func mayReadListing(p *auth.Principal, l ListingInfo) bool {
	return p.MayEditListing(l.OrganisationID, l.AgentUserID, l.SellerUserID)
}

// ListingReport returns one listing's figures.
func (s *Service) ListingReport(ctx context.Context, p *auth.Principal, id uuid.UUID, q PeriodQuery) (ListingReport, error) {
	if p == nil {
		return ListingReport{}, ErrUnauthenticated
	}
	known, err := s.listings.Listings(ctx, []uuid.UUID{id})
	if err != nil {
		return ListingReport{}, fmt.Errorf("load listing: %w", err)
	}
	info, ok := known[id]
	if !ok || !mayReadListing(p, info) {
		return ListingReport{}, ErrNotFound
	}
	zones, err := s.zoneMap(ctx)
	if err != nil {
		return ListingReport{}, err
	}
	w := s.resolve(q, s.location(zones, info.MarketCode))
	rows, refreshed, err := s.collect(ctx, w, []uuid.UUID{id}, nil)
	if err != nil {
		return ListingReport{}, err
	}
	return ListingReport{Listing: refOf(info), Report: build(w, rows, refreshed)}, nil
}

// OrganisationRef names the organisation of a report.
type OrganisationRef struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

// OrganisationReport is GET /v1/analytics/organisations/{orgId}.
type OrganisationReport struct {
	Organisation OrganisationRef `json:"organisation"`
	// Scope is "organisation" (OWNER, MANAGER: every listing) or "assigned"
	// (AGENT: only the listings they are the assigned agent of).
	Scope         string `json:"scope"`
	ListingsCount int    `json:"listings_count"`
	Report
	TopListings []TopListing `json:"top_listings"`
}

// Organisation report scopes.
const (
	ScopeOrganisation = "organisation"
	ScopeAssigned     = "assigned"
)

// OrganisationReport sums the figures of the organisation's listings the
// caller may read. Not a member: ErrNotFound. A member whose role manages
// no listings (STAFF), or of an organisation that has none: ErrForbidden.
func (s *Service) OrganisationReport(ctx context.Context, p *auth.Principal, orgID uuid.UUID, q PeriodQuery) (OrganisationReport, error) {
	if p == nil {
		return OrganisationReport{}, ErrUnauthenticated
	}
	var member *auth.Membership
	for i := range p.Memberships {
		if p.Memberships[i].OrganisationID == orgID {
			member = &p.Memberships[i]
			break
		}
	}
	if member == nil {
		return OrganisationReport{}, ErrNotFound
	}
	if !p.MayManageListings(orgID) {
		return OrganisationReport{}, ErrForbidden
	}
	all, err := s.listings.OrganisationListings(ctx, orgID)
	if err != nil {
		return OrganisationReport{}, fmt.Errorf("load organisation listings: %w", err)
	}
	scope := ScopeOrganisation
	if member.Role == auth.RoleAgent {
		scope = ScopeAssigned
	}
	info := map[uuid.UUID]ListingInfo{}
	ids := []uuid.UUID{}
	for _, l := range all {
		if mayReadListing(p, l) {
			info[l.ID] = l
			ids = append(ids, l.ID)
		}
	}
	zones, err := s.zoneMap(ctx)
	if err != nil {
		return OrganisationReport{}, err
	}
	w := s.resolve(q, s.location(zones, member.MarketCode))
	rows, refreshed, err := s.collect(ctx, w, ids, nil)
	if err != nil {
		return OrganisationReport{}, err
	}
	rep := build(w, rows, refreshed)
	return OrganisationReport{Organisation: OrganisationRef{ID: orgID, Name: member.OrganisationName}, Scope: scope,
		ListingsCount: len(ids), Report: rep, TopListings: rep.top(info)}, nil
}

// AdminRoles may read the site and market summary: SUPER_ADMIN,
// PLATFORM_ADMIN, and MARKET_ADMIN for its own markets.
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformMarketAdmin}
}

// adminMarkets returns the markets the caller may see: all for SUPER_ADMIN
// and PLATFORM_ADMIN, its own for a MARKET_ADMIN; ok false for anyone else.
func adminMarkets(p *auth.Principal) (codes []string, all, ok bool) {
	if p == nil || p.SessionScope != auth.ScopeAdmin {
		return nil, false, false
	}
	for _, r := range p.PlatformRoles {
		switch r.Role {
		case auth.PlatformSuperAdmin, auth.PlatformAdmin:
			return nil, true, true
		case auth.PlatformMarketAdmin:
			if r.MarketCode != "" && !slices.Contains(codes, r.MarketCode) {
				codes = append(codes, r.MarketCode)
			}
		}
	}
	return codes, false, len(codes) > 0
}

// MarketTotals is one market of the admin summary.
type MarketTotals struct {
	MarketCode        string `json:"market_code"`
	Totals            Totals `json:"totals"`
	FilteredBotEvents int    `json:"filtered_bot_events"`
	LateEventsDropped int    `json:"late_events_dropped"`
}

// AdminSummary is GET /v1/admin/analytics/summary.
type AdminSummary struct {
	// Market is the market asked for (null: every market the caller may see).
	Market *string `json:"market"`
	Report
	Markets     []MarketTotals `json:"markets"`
	TopListings []TopListing   `json:"top_listings"`
}

// AdminSummary returns the site's (or one market's) aggregates. market ""
// means every market the caller may see. Raw events are never returned.
func (s *Service) AdminSummary(ctx context.Context, p *auth.Principal, market string, q PeriodQuery) (AdminSummary, error) {
	own, all, ok := adminMarkets(p)
	if !ok {
		return AdminSummary{}, ErrForbidden
	}
	zones, err := s.zoneMap(ctx)
	if err != nil {
		return AdminSummary{}, err
	}
	var markets []string
	loc := time.UTC
	switch {
	case market != "":
		if _, known := zones[market]; !known {
			return AdminSummary{}, &ValidationError{Fields: map[string]string{"market": "must be a known country code"}}
		}
		if !all && !slices.Contains(own, market) {
			return AdminSummary{}, ErrForbidden
		}
		markets, loc = []string{market}, s.location(zones, market)
	case !all:
		markets = own
		if len(own) == 1 {
			loc = s.location(zones, own[0])
		}
	}
	w := s.resolve(q, loc)
	rows, refreshed, err := s.collect(ctx, w, nil, markets)
	if err != nil {
		return AdminSummary{}, err
	}
	rep := build(w, rows, refreshed)
	byMarket := map[string]*MarketTotals{}
	for _, row := range rows {
		m := byMarket[row.MarketCode]
		if m == nil {
			m = &MarketTotals{MarketCode: row.MarketCode}
			byMarket[row.MarketCode] = m
		}
		m.Totals.add(row.Totals)
		m.FilteredBotEvents += row.FilteredBotEvents
		m.LateEventsDropped += row.LateEventsDropped
	}
	perMarket := []MarketTotals{}
	for _, m := range byMarket {
		perMarket = append(perMarket, *m)
	}
	sort.Slice(perMarket, func(i, j int) bool {
		if perMarket[i].Totals.Views != perMarket[j].Totals.Views {
			return perMarket[i].Totals.Views > perMarket[j].Totals.Views
		}
		return perMarket[i].MarketCode < perMarket[j].MarketCode
	})
	top := rep.top(nil)
	topIDs := make([]uuid.UUID, 0, len(top))
	for _, t := range top {
		topIDs = append(topIDs, t.ListingID)
	}
	info := map[uuid.UUID]ListingInfo{}
	if len(topIDs) > 0 {
		if info, err = s.listings.Listings(ctx, topIDs); err != nil {
			return AdminSummary{}, fmt.Errorf("load listings: %w", err)
		}
	}
	for i := range top {
		if l, ok := info[top[i].ListingID]; ok {
			top[i] = topOf(l, Totals{Views: top[i].Views, UniqueViews: top[i].UniqueViews, ContactClicks: top[i].ContactClicks, Enquiries: top[i].Enquiries})
		}
	}
	var asked *string
	if market != "" {
		asked = &market
	}
	return AdminSummary{Market: asked, Report: rep, Markets: perMarket, TopListings: top}, nil
}
