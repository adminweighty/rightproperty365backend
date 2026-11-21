package listings

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes specific to this module.
const (
	CodeListingNotFound    = "LISTING_NOT_FOUND"
	CodeListingUnavailable = "LISTING_UNAVAILABLE"
)

// Listings change more often than reference data: cache briefly.
const (
	CacheMaxAge = 30 * time.Second
	CacheStale  = 2 * time.Minute
)

const maxQueryLength = 100

// maxArea bounds the area filters (square metres).
const maxArea = 100000000

var (
	slugRe      = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	currencyRe  = regexp.MustCompile(`^[A-Z]{3}$`)
	countryRe   = regexp.MustCompile(`^[A-Z]{2}$`)
	referenceRe = regexp.MustCompile(`^[A-Za-z0-9-]{1,40}$`)
)

// Handler serves GET /v1/listings, /v1/listings/suggest and /v1/listings/{slug}.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func badRequest(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", fields)
}

// ParsePaging reads page and page_size (shared with the directory).
func ParsePaging(v url.Values, fields map[string]string) (page, size int) {
	page, size = 1, DefaultPageSize
	if raw := v.Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := v.Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	return page, size
}

// parseQuery turns the query string into a Query, collecting per-field errors.
func parseQuery(v url.Values) (Query, map[string]string) {
	fields := map[string]string{}
	q := Query{
		Q: strings.TrimSpace(v.Get("q")), Deal: v.Get("deal"), Category: v.Get("category"),
		PropertyType: v.Get("property_type"), City: v.Get("city"), Suburb: v.Get("suburb"),
		Currency: strings.ToUpper(strings.TrimSpace(v.Get("currency"))), Sort: v.Get("sort"),
	}
	if len([]rune(q.Q)) > maxQueryLength {
		fields["q"] = "must be at most 100 characters"
	}
	q.Page, q.PageSize = ParsePaging(v, fields)
	for name, dst := range map[string]**int{
		"bedrooms_min": &q.BedroomsMin, "bathrooms_min": &q.BathroomsMin, "bedrooms_max": &q.BedroomsMax, "bathrooms_max": &q.BathroomsMax,
	} {
		if raw := v.Get(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n > 50 {
				fields[name] = "must be a whole number from 0 to 50"
				continue
			}
			*dst = &n
		}
	}
	// Areas, in square metres.
	for name, dst := range map[string]**int{
		"floor_area_min": &q.FloorAreaMin, "floor_area_max": &q.FloorAreaMax, "land_area_min": &q.LandAreaMin, "land_area_max": &q.LandAreaMax,
	} {
		if raw := v.Get(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n > maxArea {
				fields[name] = "must be a whole number of square metres from 0 to " + strconv.Itoa(maxArea)
				continue
			}
			*dst = &n
		}
	}
	for _, pair := range []struct {
		name     string
		min, max *int
	}{
		{"bedrooms_max", q.BedroomsMin, q.BedroomsMax}, {"bathrooms_max", q.BathroomsMin, q.BathroomsMax},
		{"floor_area_max", q.FloorAreaMin, q.FloorAreaMax}, {"land_area_max", q.LandAreaMin, q.LandAreaMax},
	} {
		if _, bad := fields[pair.name]; !bad && pair.min != nil && pair.max != nil && *pair.min > *pair.max {
			fields[pair.name] = "must not be lower than " + strings.TrimSuffix(pair.name, "max") + "min"
		}
	}
	if raw := v.Get("guests_min"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100 {
			fields["guests_min"] = "must be a whole number from 1 to 100"
		} else {
			q.GuestsMin = &n
		}
	}
	// A stay: both dates or neither. Their format and range are checked by
	// the Service, which also resolves which listings are free.
	q.CheckIn, q.CheckOut = strings.TrimSpace(v.Get("check_in")), strings.TrimSpace(v.Get("check_out"))
	switch {
	case q.CheckIn != "" && q.CheckOut == "":
		fields["check_out"] = "is required with check_in"
	case q.CheckOut != "" && q.CheckIn == "":
		fields["check_in"] = "is required with check_out"
	}
	if raw := v.Get("listed_within_days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 365 {
			fields["listed_within_days"] = "must be a whole number from 1 to 365"
		} else {
			q.ListedWithinDays = &n
		}
	}
	// Country: a market code, in any case. `market` is an alias.
	country, market := strings.ToUpper(strings.TrimSpace(v.Get("country"))), strings.ToUpper(strings.TrimSpace(v.Get("market")))
	switch {
	case country != "" && market != "" && country != market:
		fields["market"] = "is an alias of country; send one of them, or the same value in both"
	case country == "":
		country = market
	}
	if country != "" && !countryRe.MatchString(country) {
		fields["country"] = "must be a two-letter country code such as ZW"
	}
	q.Country, q.Region = country, strings.TrimSpace(v.Get("region"))
	switch raw := v.Get("advertiser_type"); raw {
	case "", AdvertiserFilterAgency, AdvertiserFilterPrivate:
		q.AdvertiserType = raw
	default:
		fields["advertiser_type"] = "must be agency or private"
	}
	if raw := strings.TrimSpace(v.Get("reference")); raw != "" {
		if !referenceRe.MatchString(raw) {
			fields["reference"] = "must be a listing reference such as RP-ZW-10001"
		}
		q.Reference = raw
	}
	switch raw := v.Get("has_photos"); raw {
	case "", "false":
	case "true":
		q.HasPhotos = true
	default:
		fields["has_photos"] = "must be true or false"
	}
	// The map: a box (west,south,east,north) and / or "only with coordinates".
	if raw := strings.TrimSpace(v.Get("bbox")); raw != "" {
		box, problem := ParseBBox(raw)
		if problem != "" {
			fields["bbox"] = problem
		}
		q.BBox = box
	}
	switch raw := v.Get("has_location"); raw {
	case "", "false":
	case "true":
		q.HasLocation = true
	default:
		fields["has_location"] = "must be true or false"
	}
	if raw := v.Get("min_price"); raw != "" {
		amount, err := ParseAmount(raw)
		if err != nil {
			fields["min_price"] = "must be an amount such as 250000 or 950.50"
		}
		q.MinPrice = amount
	}
	if raw := v.Get("max_price"); raw != "" {
		amount, err := ParseAmount(raw)
		if err != nil {
			fields["max_price"] = "must be an amount such as 250000 or 950.50"
		}
		q.MaxPrice = amount
	}
	if q.MinPrice != nil && q.MaxPrice != nil && q.MinPrice.Cmp(q.MaxPrice) > 0 {
		fields["max_price"] = "must not be lower than min_price"
	}
	if q.Currency != "" && !currencyRe.MatchString(q.Currency) {
		fields["currency"] = "must be a three-letter currency code such as USD"
	}
	if raw := v.Get("featured"); raw != "" {
		switch raw {
		case "true", "false":
			b := raw == "true"
			q.Featured = &b
		default:
			fields["featured"] = "must be true or false"
		}
	}
	// features may be repeated (features=a&features=b) or comma-separated.
	for _, raw := range v["features"] {
		for _, f := range strings.Split(raw, ",") {
			if f = strings.TrimSpace(f); f != "" && !containsString(q.Features, f) {
				q.Features = append(q.Features, f)
			}
		}
	}
	if len(q.Features) > 20 {
		fields["features"] = "at most 20 features can be combined"
	}
	switch q.Sort {
	case "":
		q.Sort = SortNewest
		if q.Q != "" {
			q.Sort = SortRelevance
		}
	case SortNewest, SortOldest, SortPriceAsc, SortPriceDesc, SortAreaDesc, SortRelevance:
	default:
		fields["sort"] = "must be one of " + strings.Join(Sorts(), ", ")
	}
	return q, fields
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// List handles GET /v1/listings.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	q, fields := parseQuery(r.URL.Query())
	if len(fields) == 0 {
		resolved, codeFields, err := h.svc.Resolve(r.Context(), q)
		if err != nil {
			httpx.WriteInternalError(w, r, h.logger, err)
			return
		}
		q, fields = resolved, codeFields
	}
	if len(fields) > 0 {
		badRequest(w, r, fields)
		return
	}
	page, err := h.svc.Find(r.Context(), q)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, page, CacheMaxAge, CacheStale)
}

type listingResponse struct {
	Listing Listing `json:"listing"`
}

// Get handles GET /v1/listings/{slug}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if !slugRe.MatchString(slug) || len(slug) > 200 {
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
		return
	}
	listing, err := h.svc.BySlug(r.Context(), slug)
	switch {
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
	case errors.Is(err, ErrUnavailable):
		httpx.WriteError(w, r, http.StatusGone, CodeListingUnavailable, "This listing is no longer available.", nil)
	case err != nil:
		httpx.WriteInternalError(w, r, h.logger, err)
	default:
		httpx.WriteCacheableJSON(w, r, listingResponse{Listing: listing}, CacheMaxAge, CacheStale)
	}
}

type suggestResponse struct {
	Suggestions []Suggestion `json:"suggestions"`
}

// Suggest handles GET /v1/listings/suggest.
func (h *Handler) Suggest(w http.ResponseWriter, r *http.Request) {
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	if len([]rune(term)) > maxQueryLength {
		badRequest(w, r, map[string]string{"q": "must be at most 100 characters"})
		return
	}
	suggestions, err := h.svc.Suggest(r.Context(), term)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, suggestResponse{Suggestions: suggestions}, CacheMaxAge, CacheStale)
}

// ParseQuery reads a search from query parameters, as GET /v1/listings does
// (saved searches validate and run their query string with it, API 0.52.0).
func ParseQuery(v url.Values) (Query, map[string]string) { return parseQuery(v) }
