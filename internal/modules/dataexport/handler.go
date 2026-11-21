package dataexport

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes.
const (
	CodeRecent          = "EXPORT_RECENT"
	CodeNotReady        = "EXPORT_NOT_READY"
	CodePlatformAccount = "PLATFORM_ACCOUNT"
)

// Handler serves /v1/me/data-export.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

type view struct {
	ID          uuid.UUID  `json:"id"`
	Status      string     `json:"status"`
	RequestedAt time.Time  `json:"requested_at"`
	ReadyAt     *time.Time `json:"ready_at"`
	ExpiresAt   *time.Time `json:"expires_at"`
	SizeBytes   int64      `json:"size_bytes"`
	Downloads   int        `json:"downloads"`
}

func viewOf(e Export) view {
	v := view{ID: e.ID, Status: e.Status, RequestedAt: e.RequestedAt, ExpiresAt: e.ExpiresAt, SizeBytes: e.SizeBytes, Downloads: e.Downloads}
	if e.Status == StatusReady || e.Status == StatusExpired {
		v.ReadyAt = e.ReadyAt
	}
	// While building, the internal lease is not a time the person needs.
	if e.Status == StatusBuilding {
		v.Status = StatusRequested
	}
	return v
}

func (h *Handler) user(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	switch {
	case p == nil:
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
		return uuid.Nil, false
	case p.SessionScope == auth.ScopeAdmin:
		httpx.WriteError(w, r, http.StatusForbidden, CodePlatformAccount, "Administrator accounts cannot export their data here.", nil)
		return uuid.Nil, false
	}
	return p.UserID, true
}

// Request serves POST /v1/me/data-export.
func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	user, ok := h.user(w, r)
	if !ok {
		return
	}
	e, err := h.svc.Request(r.Context(), user)
	if errors.Is(err, ErrRecent) {
		// The existing export is GET /v1/me/data-export.
		httpx.WriteError(w, r, http.StatusConflict, CodeRecent, "You asked for an export in the last 24 hours. It is shown on the page.", nil)
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"export": viewOf(e)})
}

// Latest serves GET /v1/me/data-export.
func (h *Handler) Latest(w http.ResponseWriter, r *http.Request) {
	user, ok := h.user(w, r)
	if !ok {
		return
	}
	e, err := h.svc.Latest(r.Context(), user)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if e == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"export": nil})
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"export": viewOf(*e)})
}

// Download serves GET /v1/me/data-export/download: the archive, to its owner only.
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	user, ok := h.user(w, r)
	if !ok {
		return
	}
	e, rc, err := h.svc.Open(r.Context(), user)
	if errors.Is(err, ErrNotReady) {
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotReady, "There is no export ready to download.", nil)
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="rightproperty365-data-`+e.RequestedAt.Format("2006-01-02")+`.zip"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if e.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(e.SizeBytes, 10))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, rc); err != nil {
		h.logger.WarnContext(r.Context(), "data export download interrupted", slog.String("export_id", e.ID.String()))
	}
}
