// Package markets exposes the countries the marketplace operates in.
package markets

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/httpx"
)

const (
	StatusActive  = "ACTIVE"
	StatusPlanned = "PLANNED"
)

// ZimbabweID is the fixed ID of the launch market. The same IDs are seeded by
// migrations/0001_init.up.sql.
var ZimbabweID = uuid.MustParse("0195c0de-0000-7000-8000-00000000a001")

// UnitedKingdomID is the fixed ID of the United Kingdom market (migration 0013).
var UnitedKingdomID = uuid.MustParse("0195c0de-0000-7000-8000-00000000a005")

// Market is a country the platform serves or plans to serve.
type Market struct {
	ID           uuid.UUID `json:"id"`
	Code         string    `json:"code"` // ISO 3166-1 alpha-2
	Name         string    `json:"name"`
	CurrencyCode string    `json:"currency_code"` // ISO 4217
	Timezone     string    `json:"timezone"`      // IANA zone
	Status       string    `json:"status"`
}

// Store lists markets.
type Store interface {
	List(ctx context.Context) ([]Market, error)
}

// Seed returns the markets, ordered as the API returns them: active markets
// first, in their display order (Zimbabwe, the launch market, stays first),
// then the planned ones by code. Keep in sync with migrations 0001 and 0013.
func Seed() []Market {
	return []Market{
		{ID: ZimbabweID, Code: "ZW", Name: "Zimbabwe", CurrencyCode: "USD", Timezone: "Africa/Harare", Status: StatusActive},
		{ID: UnitedKingdomID, Code: "GB", Name: "United Kingdom", CurrencyCode: "GBP", Timezone: "Europe/London", Status: StatusActive},
		{ID: uuid.MustParse("0195c0de-0000-7000-8000-00000000a004"), Code: "AE", Name: "United Arab Emirates", CurrencyCode: "AED", Timezone: "Asia/Dubai", Status: StatusPlanned},
		{ID: uuid.MustParse("0195c0de-0000-7000-8000-00000000a002"), Code: "BW", Name: "Botswana", CurrencyCode: "BWP", Timezone: "Africa/Gaborone", Status: StatusPlanned},
		{ID: uuid.MustParse("0195c0de-0000-7000-8000-00000000a003"), Code: "ZA", Name: "South Africa", CurrencyCode: "ZAR", Timezone: "Africa/Johannesburg", Status: StatusPlanned},
	}
}

// MemoryStore serves the seed data (development only).
type MemoryStore struct{ markets []Market }

// NewMemoryStore returns a store holding Seed().
func NewMemoryStore() *MemoryStore { return &MemoryStore{markets: Seed()} }

// List implements Store.
func (s *MemoryStore) List(context.Context) ([]Market, error) {
	return append([]Market(nil), s.markets...), nil
}

// PostgresStore reads markets from the database.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns a Postgres-backed store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

// List implements Store.
func (s *PostgresStore) List(ctx context.Context) ([]Market, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, code, name, currency_code, timezone, status
		FROM markets
		ORDER BY (status = 'ACTIVE') DESC, sort_order, code`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Market{}
	for rows.Next() {
		var m Market
		if err := rows.Scan(&m.ID, &m.Code, &m.Name, &m.CurrencyCode, &m.Timezone, &m.Status); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Handler serves GET /v1/markets.
type Handler struct {
	store  Store
	logger *slog.Logger
}

// NewHandler builds the markets handler.
func NewHandler(store Store, logger *slog.Logger) *Handler {
	return &Handler{store: store, logger: logger}
}

// List writes all markets.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	list, err := h.store.List(r.Context())
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"data": list})
}
