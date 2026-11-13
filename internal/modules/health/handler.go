// Package health exposes liveness and readiness probes.
package health

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"rightproperty365.com/backend/internal/platform/httpx"
)

const (
	StoreMemory   = "memory"
	StorePostgres = "postgres"
)

// Handler serves /healthz and /readyz.
type Handler struct {
	store  string
	ping   func(context.Context) error // nil when running on the memory store
	logger *slog.Logger
}

// NewHandler builds the probe handler. ping is nil in memory mode.
func NewHandler(store string, ping func(context.Context) error, logger *slog.Logger) *Handler {
	return &Handler{store: store, ping: ping, logger: logger}
}

// Live reports that the process is up. It never touches dependencies.
func (h *Handler) Live(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready reports whether the process can serve traffic (database reachable).
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	if h.ping != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.ping(ctx); err != nil {
			h.logger.WarnContext(r.Context(), "readiness check failed", slog.String("error", err.Error()))
			httpx.WriteError(w, r, http.StatusServiceUnavailable, httpx.CodeNotReady, "The service is not ready.", nil)
			return
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ready", "store": h.store})
}
