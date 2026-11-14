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

var (
	slugRe     = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	currencyRe = regexp.MustCompile(`^[A-Z]{3}$`)
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

	for name, dst := range map[string]**int{"bedrooms_min": &q.BedroomsMin, "bathrooms_min": &q.BathroomsMin} {
		if raw := v.Get(name); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || n > 50 {
				fields[name] = "must be a whole number from 0 to 50"
				continue
			}
			*dst = &n
		}
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
	case SortNewest, SortPriceAsc, SortPriceDesc, SortRelevance:
	default:
		fields["sort"] = "must be one of newest, price_asc, price_desc, relevance"
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
		codeFields, err := h.svc.CheckCodes(r.Context(), q)
		if err != nil {
			httpx.WriteInternalError(w, r, h.logger, err)
			return
		}
		fields = codeFields
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
