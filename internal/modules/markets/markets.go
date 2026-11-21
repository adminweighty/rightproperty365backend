// Package markets exposes the countries of the marketplace.
//
// The platform is global: every ISO 3166-1 country is a row here and any of
// them may be used by a lister. There is no short list of countries that are
// "switched on". What the public is shown in a country list is a different
// question — the countries that have published listings right now — and is
// answered by GET /v1/countries?has_listings=true.
//
// "Market" is the older name of the same thing and is kept in the table name,
// the Go types and GET /v1/markets.
package markets

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // every country's zone must load on any machine

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"

	"rightproperty365.com/backend/internal/platform/httpx"
)

// StatusActive is the only status the API reports. The column and the JSON
// field are kept so existing clients do not break, but a status no longer
// gates anything: there is no PLANNED country any more. A country that has to
// be switched off is disabled (see Market.Enabled) and then simply is not
// listed or accepted.
const StatusActive = "ACTIVE"

// ZimbabweID is the fixed ID of Zimbabwe, the first country (migration 0001).
var ZimbabweID = uuid.MustParse("0195c0de-0000-7000-8000-00000000a001")

// UnitedKingdomID is the fixed ID of the United Kingdom (migration 0013).
var UnitedKingdomID = uuid.MustParse("0195c0de-0000-7000-8000-00000000a005")

// Market is a country.
type Market struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"` // ISO 3166-1 alpha-2
	Name         string    `json:"name"`
	CurrencyCode string    `json:"currency_code"` // ISO 4217
	Timezone     string    `json:"timezone"`      // IANA zone of the capital
	// Status is always "ACTIVE" (kept for older clients; see StatusActive).
	Status string `json:"status"`
	// ListingCount is the number of published, unexpired listings in the
	// country right now; HasListings is ListingCount > 0. Both are filled by
	// the HTTP handler, not by the stores.
	ListingCount int  `json:"listing_count"`
	HasListings  bool `json:"has_listings"`

	// Enabled is false for a country that was switched off. Stores never
	// return such a row, so it is not part of the JSON.
	Enabled bool `json:"-"`
	// SortOrder puts a few countries before the alphabetical rest.
	SortOrder int `json:"-"`

	// Market administration (API 0.56.0). Currencies is the main currency
	// then the extra permitted ones; Notice a short text shown on the
	// country's pages (nil for none).
	Currencies []string `json:"currencies"`
	Notice     *string  `json:"notice"`
	// The administrators' fields: the real status (PLANNED, ACTIVE,
	// SUSPENDED), the extra currencies and the default listing package.
	AdminStatus        string    `json:"-"`
	ExtraCurrencies    []string  `json:"-"`
	DefaultPackageCode string    `json:"-"`
	UpdatedAt          time.Time `json:"-"`
}

// withCurrencies fills Currencies from the main and extra currencies.
func withCurrencies(m Market) Market {
	m.Currencies = append([]string{m.CurrencyCode}, m.ExtraCurrencies...)
	return m
}

// Store lists the enabled countries, ordered by sort order, then name.
type Store interface {
	List(ctx context.Context) ([]Market, error)
}

// Order sorts countries the way every store returns them.
func Order(list []Market) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].SortOrder != list[j].SortOrder {
			return list[i].SortOrder < list[j].SortOrder
		}
		if a, b := Fold(list[i].Name), Fold(list[j].Name); a != b {
			return a < b
		}
		return list[i].Code < list[j].Code
	})
}

// MemoryStore serves the seed data (development and tests).
type MemoryStore struct {
	mu      sync.RWMutex
	markets []Market
	// audit receives the market administration events (API 0.56.0).
	audit *adminaudit.MemoryLog
}

// NewMemoryStore returns a store holding Seed().
func NewMemoryStore() *MemoryStore { return &MemoryStore{markets: Seed()} }

// List implements Store.
func (s *MemoryStore) List(context.Context) ([]Market, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Market, 0, len(s.markets))
	for _, m := range s.markets {
		if m.Enabled {
			out = append(out, withCurrencies(m))
		}
	}
	return out, nil
}

// SetEnabled switches a country on or off (what an operator does in the
// database). It reports whether the country exists.
func (s *MemoryStore) SetEnabled(code string, enabled bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.markets {
		if s.markets[i].Code == code {
			s.markets[i].Enabled = enabled
			return true
		}
	}
	return false
}

// PostgresStore reads the countries from the markets table.
//
// NOTE: the enabled column and the full country list come from migration
// 0014, which has not been run against a database from this repository.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// List implements Store.
func (s *PostgresStore) List(ctx context.Context) ([]Market, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, code, name, currency_code, timezone, sort_order, extra_currencies, notice
		FROM markets
		WHERE enabled
		ORDER BY sort_order, name, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Market{}
	for rows.Next() {
		m := Market{Status: StatusActive, Enabled: true}
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.CurrencyCode, &m.Timezone, &m.SortOrder, &m.ExtraCurrencies, &m.Notice); err != nil {
			return nil, err
		}
		out = append(out, withCurrencies(m))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The database's collation decides ORDER BY name; sort again so accented
	// names ("Åland Islands") sit where the memory store puts them.
	Order(out)
	return out, nil
}

// folds maps the accented letters that occur in country names (and the
// common Latin ones) to their base letter.
var folds = strings.NewReplacer(
	"à", "a", "á", "a", "â", "a", "ã", "a", "ä", "a", "å", "a", "ā", "a",
	"ç", "c", "č", "c", "ć", "c",
	"è", "e", "é", "e", "ê", "e", "ë", "e", "ē", "e", "ę", "e",
	"ì", "i", "í", "i", "î", "i", "ï", "i", "ī", "i", "ı", "i",
	"ñ", "n", "ń", "n",
	"ò", "o", "ó", "o", "ô", "o", "õ", "o", "ö", "o", "ø", "o", "ō", "o",
	"ù", "u", "ú", "u", "û", "u", "ü", "u", "ū", "u",
	"ý", "y", "ÿ", "y", "š", "s", "ś", "s", "ş", "s", "ž", "z", "ź", "z", "ż", "z", "ł", "l", "ğ", "g",
	"ß", "ss", "æ", "ae", "œ", "oe",
	"’", "'",
)

// Fold lower-cases a name and removes its diacritics, so "reunion" finds
// "Réunion" and "TURKIYE" finds "Türkiye".
func Fold(s string) string { return folds.Replace(strings.ToLower(strings.TrimSpace(s))) }

// Countries change rarely; the counts follow the listings. Cache briefly,
// like the reference data, with an ETag so revalidation is a cheap 304.
const (
	cacheMaxAge = time.Minute
	cacheStale  = 5 * time.Minute
)

const maxQueryLength = 100

// Counter returns the number of published, unexpired listings per country
// code. It is answered by the listings store, the source of truth.
type Counter func(ctx context.Context) (map[string]int, error)

// Handler serves GET /v1/countries and GET /v1/markets (the same data).
type Handler struct {
	store  Store
	counts Counter
	logger *slog.Logger
}

// NewHandler builds the handler. counts may be nil (every count is then 0).
func NewHandler(store Store, counts Counter, logger *slog.Logger) *Handler {
	return &Handler{store: store, counts: counts, logger: logger}
}

// Countries returns the enabled countries with their listing counts,
// narrowed by q (a name or a code, without case or diacritics) and, when
// onlyWithListings is set, to the countries that have listings, most first.
func (h *Handler) Countries(ctx context.Context, q string, onlyWithListings bool) ([]Market, error) {
	list, err := h.store.List(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	if h.counts != nil {
		if counts, err = h.counts(ctx); err != nil {
			return nil, err
		}
	}
	needle := Fold(q)
	out := make([]Market, 0, len(list))
	for _, m := range list {
		m.Status = StatusActive
		m.ListingCount = counts[m.Code]
		m.HasListings = m.ListingCount > 0
		if onlyWithListings && !m.HasListings {
			continue
		}
		if needle != "" && !strings.Contains(Fold(m.Name), needle) && Fold(m.Code) != needle {
			continue
		}
		out = append(out, m)
	}
	if onlyWithListings {
		sort.SliceStable(out, func(i, j int) bool {
			if out[i].ListingCount != out[j].ListingCount {
				return out[i].ListingCount > out[j].ListingCount
			}
			return Fold(out[i].Name) < Fold(out[j].Name)
		})
	}
	return out, nil
}

// List handles GET /v1/countries and GET /v1/markets.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	fields := map[string]string{}
	q := strings.TrimSpace(v.Get("q"))
	if len([]rune(q)) > maxQueryLength {
		fields["q"] = "must be at most 100 characters"
	}
	only := v.Get("has_listings")
	if only != "" && only != "true" && only != "false" {
		fields["has_listings"] = "must be true or false"
	}
	if len(fields) > 0 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", fields)
		return
	}
	list, err := h.Countries(r.Context(), q, only == "true")
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, map[string]any{"data": list}, cacheMaxAge, cacheStale)
}
