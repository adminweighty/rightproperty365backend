package app

import (
	"context"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/savedsearches"
)

// Saved searches (API 0.52.0): a saved search is a GET /v1/listings query
// string. searchQueries validates it with the search's own parser and
// reference codes; searchMatcher runs it against the database search for
// listings published in a window.

// savedSearchParams are the search parameters a saved search keeps: paging,
// sort and the map's box are left out.
var savedSearchParams = []string{
	"q", "deal", "category", "property_type", "country", "market", "region", "city", "suburb", "currency", "min_price", "max_price",
	"bedrooms_min", "bedrooms_max", "bathrooms_min", "bathrooms_max", "floor_area_min", "floor_area_max", "land_area_min", "land_area_max",
	"advertiser_type", "has_photos", "featured", "features", "guests_min", "check_in", "check_out", "listed_within_days", "reference",
}

type searchQueries struct{ svc *listings.Service }

func canonicalQuery(raw string) (url.Values, string, bool) {
	v, err := url.ParseQuery(strings.TrimPrefix(strings.TrimSpace(raw), "?"))
	if err != nil {
		return nil, "", false
	}
	kept := url.Values{}
	for k, vals := range v {
		if !slices.Contains(savedSearchParams, k) {
			continue
		}
		for _, val := range vals {
			if strings.TrimSpace(val) != "" {
				kept.Add(k, strings.TrimSpace(val))
			}
		}
	}
	return kept, kept.Encode(), true
}

// Canonical implements savedsearches.Checker.
func (c searchQueries) Canonical(ctx context.Context, raw string) (string, map[string]string, error) {
	v, canonical, ok := canonicalQuery(raw)
	if !ok {
		return "", map[string]string{"query": "is not a search query string"}, nil
	}
	q, fields := listings.ParseQuery(v)
	if len(fields) > 0 {
		return "", fields, nil
	}
	if _, fields, err := c.svc.Resolve(ctx, q); err != nil || len(fields) > 0 {
		return "", fields, err
	}
	return canonical, nil, nil
}

type searchMatcher struct{ svc *listings.Service }

// Match implements savedsearches.Matcher: the newest listings of the
// search published in (after, upto].
func (m searchMatcher) Match(ctx context.Context, raw string, after, upto time.Time) (savedsearches.Alert, error) {
	v, _, ok := canonicalQuery(raw)
	if !ok {
		return savedsearches.Alert{}, nil
	}
	v.Del("listed_within_days")
	q, fields := listings.ParseQuery(v)
	if len(fields) > 0 {
		return savedsearches.Alert{}, nil // no longer a valid search: nothing matches
	}
	q, fields, err := m.svc.Resolve(ctx, q)
	if err != nil || len(fields) > 0 {
		return savedsearches.Alert{}, err
	}
	days := int(upto.Sub(after)/(24*time.Hour)) + 2
	days = min(max(days, 1), 365)
	q.ListedWithinDays, q.Sort, q.Page, q.PageSize = &days, listings.SortNewest, 1, listings.MaxPageSize
	page, err := m.svc.Find(ctx, q)
	if err != nil {
		return savedsearches.Alert{}, err
	}
	var alert savedsearches.Alert
	for _, l := range page.Items {
		if !l.PublishedAt.After(after) || l.PublishedAt.After(upto) {
			continue
		}
		alert.Total++
		if len(alert.Lines) < savedsearches.MaxLines {
			place := l.City.Label
			if l.Suburb != nil {
				place = l.Suburb.Label + ", " + l.City.Label
			}
			price := l.Price.Currency + " " + l.Price.Amount
			if l.PricePeriod != nil && *l.PricePeriod != "" {
				price += " " + strings.ToLower(strings.ReplaceAll(*l.PricePeriod, "_", " "))
			}
			alert.Lines = append(alert.Lines, savedsearches.Line{ListingID: l.ID, Slug: l.Slug, Title: l.Title, Place: place, Price: price})
		}
	}
	// A full page of new listings: there may be more than the page held.
	alert.More = alert.Total == len(page.Items) && page.Total > len(page.Items)
	return alert, nil
}

// RunSavedSearchSweeps runs the due saved searches every
// savedsearches.SweepInterval (claimed with SKIP LOCKED: several workers are safe).
func RunSavedSearchSweeps(ctx context.Context, svc *savedsearches.Service, logger *slog.Logger) {
	ticker := time.NewTicker(savedsearches.SweepInterval)
	defer ticker.Stop()
	for {
		start := time.Now()
		n, err := svc.Sweep(ctx)
		sweepDone("saved_searches", start, err)
		if err != nil {
			logger.ErrorContext(ctx, "saved search sweep failed", slog.String("error", err.Error()))
		}
		if n > 0 {
			logger.InfoContext(ctx, "saved search alerts", slog.Int("alerts", n))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// SavedSearchService builds the worker's saved search service on the
// database search (the source of truth, not the index), with the location
// hierarchy behind the country and region filters.
func SavedSearchService(store savedsearches.Store, listingStore listings.Store, search listings.Search, ref *reference.Service, marketStore markets.Store, now func() time.Time) *savedsearches.Service {
	svc := listings.NewService(listingStore, search, ref)
	if now != nil {
		svc.SetClock(now)
	}
	svc.SetPlaces(places{ref: ref, markets: marketStore})
	return savedsearches.NewService(savedsearches.Deps{Store: store, Checker: searchQueries{svc: svc}, Matcher: searchMatcher{svc: svc}, Now: now})
}
