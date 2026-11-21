package emaildelivery

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes.
const (
	CodeNotConfigured = "WEBHOOK_NOT_CONFIGURED"
	CodeBadSignature  = "WEBHOOK_SIGNATURE_INVALID"
	CodeNotFound      = "SUPPRESSION_NOT_FOUND"
)

// Handler serves the webhook and the administrators' endpoints.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You cannot see email deliveries.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This address is not suppressed.", nil)
	case errors.Is(err, ErrNotConfigured):
		httpx.WriteError(w, r, http.StatusServiceUnavailable, CodeNotConfigured, "Email delivery webhooks are not configured.", nil)
	case errors.Is(err, ErrSignature):
		httpx.WriteError(w, r, http.StatusUnauthorized, CodeBadSignature, "The webhook signature does not verify.", nil)
	case errors.Is(err, ErrBadPayload):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeInvalidJSON, "The webhook body is not a delivery event.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// Webhook serves POST /v1/webhooks/email/resend: public, authenticated by
// the svix-* signature headers only. 204 for a verified event (new, repeated
// or not about delivery).
func (h *Handler) Webhook(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Configured() {
		h.fail(w, r, ErrNotConfigured)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "The request body is too large.", nil)
		return
	}
	if _, err := h.svc.Webhook(r.Context(), r.Header.Get("svix-id"), r.Header.Get("svix-timestamp"), r.Header.Get("svix-signature"), body); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type countsView struct {
	Accepted      int     `json:"accepted"`
	Delivered     int     `json:"delivered"`
	Delayed       int     `json:"delayed"`
	Bounced       int     `json:"bounced"`
	Complained    int     `json:"complained"`
	Suppressed    int     `json:"suppressed"`
	Failed        int     `json:"failed"`
	BounceRate    *string `json:"bounce_rate"`
	ComplaintRate *string `json:"complaint_rate"`
}

func countsOf(c Counts) countsView {
	return countsView{Accepted: c.Accepted, Delivered: c.Delivered, Delayed: c.Delayed, Bounced: c.Bounced, Complained: c.Complained,
		Suppressed: c.Suppressed, Failed: c.Failed, BounceRate: Rate(c.Bounced, c.Accepted), ComplaintRate: Rate(c.Complained, c.Accepted)}
}

type alertView struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	Events      int       `json:"events"`
	Accepted    int       `json:"accepted"`
	CreatedAt   time.Time `json:"created_at"`
}

type problemView struct {
	Kind          string    `json:"kind"`
	Status        string    `json:"status"`
	DeliveryState *string   `json:"delivery_state"`
	BounceType    *string   `json:"bounce_type"`
	Reason        *string   `json:"reason"`
	Recipient     string    `json:"recipient"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// short is the first 12 characters of an address hash: enough to compare,
// not to look up.
func short(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

// Overview serves GET /v1/admin/email-deliveries.
func (h *Handler) Overview(w http.ResponseWriter, r *http.Request) {
	o, err := h.svc.Overview(r.Context(), auth.FromContext(r.Context()))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	alerts := make([]alertView, 0, len(o.Alerts))
	for _, a := range o.Alerts {
		alerts = append(alerts, alertView{ID: a.ID.String(), Kind: a.Kind, WindowStart: a.WindowStart, WindowEnd: a.WindowEnd, Events: a.Events, Accepted: a.Accepted, CreatedAt: a.CreatedAt})
	}
	problems := make([]problemView, 0, len(o.Problems))
	for _, p := range o.Problems {
		problems = append(problems, problemView{Kind: p.Kind, Status: p.Status, DeliveryState: nullable(p.DeliveryState), BounceType: nullable(p.BounceType),
			Reason: nullable(p.LastError), Recipient: short(p.RecipientSHA256), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"webhook_configured": o.Configured,
		"last_24h":           countsOf(o.Last24h),
		"last_7d":            countsOf(o.Last7d),
		"alerts":             alerts,
		"problems":           problems,
	})
}

type suppressionView struct {
	Hash      string    `json:"hash"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Suppressions serves GET /v1/admin/email-deliveries/suppressions?email=.
func (h *Handler) Suppressions(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Suppressions(r.Context(), auth.FromContext(r.Context()), r.URL.Query().Get("email"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]suppressionView, 0, len(list))
	for _, s := range list {
		items = append(items, suppressionView{Hash: s.RecipientSHA256, Reason: s.Reason, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Clear serves DELETE /v1/admin/email-deliveries/suppressions/{hash} {reason}.
func (h *Handler) Clear(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.Clear(r.Context(), auth.FromContext(r.Context()), chi.URLParam(r, "hash"), in.Reason); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
