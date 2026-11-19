package packages

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Handler serves /v1/markets/{code}/packages, /v1/admin/packages,
// /v1/admin/organisations/{id}/package, /v1/admin/users/{id}/package,
// /v1/agent/organisations/{id}/package and /v1/me/package.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

type detailBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
	TraceID string `json:"trace_id"`
}

// WriteDetails writes the error envelope with details.
func WriteDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	var b detailBody
	b.Error.Code, b.Error.Message, b.Error.Details, b.TraceID = code, message, details, logging.RequestID(r.Context())
	httpx.WriteJSON(w, status, b)
}

// QuotaDetails are the details of 409 LISTING_QUOTA_REACHED.
func QuotaDetails(e *QuotaError, accountName string) map[string]any {
	return map[string]any{"limit": e.Limit, "used": e.Used, "package": RefOf(e.Package), "market_code": e.MarketCode,
		"account": AccountRef{Kind: e.Account.Kind, ID: e.Account.ID, Name: accountName}}
}

// fail answers an error. notFound is the code of ErrAccountNotFound on
// this route.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error, notFound string) {
	var verr *ValidationError
	var inUse *InUseError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &inUse):
		WriteDetails(w, r, http.StatusConflict, CodeInUse, "Assignments name this package, so it cannot be deleted. Deactivate it instead.",
			map[string]any{"assignments": inUse.Assignments})
	case errors.Is(err, ErrMarketNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeMarketNotFound, "This country does not exist.", nil)
	case errors.Is(err, ErrPackageNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodePackageNotFound, "This package does not exist.", nil)
	case errors.Is(err, ErrAssignmentNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeAssignmentNotFound, "This account has no current or scheduled package in that country.", nil)
	case errors.Is(err, ErrAccountNotFound):
		msg := map[string]string{CodeOrganisationNotFound: "This organisation does not exist.", CodeUserNotFound: "This user does not exist.",
			CodeAccountNotFound: "This listing account does not exist."}[notFound]
		httpx.WriteError(w, r, http.StatusNotFound, notFound, msg, nil)
	case errors.Is(err, ErrCodeTaken):
		httpx.WriteError(w, r, http.StatusConflict, CodeCodeTaken, "A package with this code already exists in this country.", nil)
	case errors.Is(err, ErrIsDefault):
		httpx.WriteError(w, r, http.StatusConflict, CodeIsDefault, "This package is the country's default. Make another package the default first.", nil)
	case errors.Is(err, ErrInactive):
		httpx.WriteError(w, r, http.StatusConflict, CodeInactive, "This package is not active. Activate it before assigning it.", nil)
	case errors.Is(err, ErrMarketForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, CodeMarketForbidden, "You manage the packages of other countries only.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Only an approved landlord or seller has their own listing package.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

func pathID(w http.ResponseWriter, r *http.Request, notFound, message string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, notFound, message, nil)
		return uuid.Nil, false
	}
	return id, true
}

// Public is GET /v1/markets/{code}/packages (public, cacheable).
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.PublicPackages(r.Context(), chi.URLParam(r, "code"))
	if err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	httpx.WriteCacheableJSON(w, r, out, 5*time.Minute, 10*time.Minute)
}

// AdminRoutes mounts /v1/admin/packages (the caller wraps the role check).
func (h *Handler) AdminRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.adminList)
	r.With(limit).Post("/", h.adminCreate)
	r.Get("/{id}", h.adminGet)
	r.With(limit).Patch("/{id}", h.adminUpdate)
	r.With(limit).Delete("/{id}", h.adminDelete)
}

func (h *Handler) adminList(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	private(w)
	q := r.URL.Query()
	include := q.Get("include_inactive") != "false"
	out, err := h.svc.AdminList(r.Context(), p, q.Get("market_code"), include)
	if err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) adminCreate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	private(w)
	var in CreateInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Create(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"package": v})
}

func (h *Handler) adminGet(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	private(w)
	id, ok := pathID(w, r, CodePackageNotFound, "This package does not exist.")
	if !ok {
		return
	}
	v, err := h.svc.AdminGet(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"package": v})
}

func (h *Handler) adminUpdate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	private(w)
	id, ok := pathID(w, r, CodePackageNotFound, "This package does not exist.")
	if !ok {
		return
	}
	var in UpdateInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.Update(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"package": v})
}

func (h *Handler) adminDelete(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	private(w)
	id, ok := pathID(w, r, CodePackageNotFound, "This package does not exist.")
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), p, id); err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AccountRoutes mounts prefix+/package and prefix+/package/end for
// /v1/admin/organisations/{id} (kind ORGANISATION) or /v1/admin/users/{id}
// (kind DIRECT_OWNER). The caller wraps the role check.
func (h *Handler) AccountRoutes(r chi.Router, kind, prefix string, limit func(http.Handler) http.Handler) {
	notFound, message := CodeOrganisationNotFound, "This organisation does not exist."
	if kind == KindDirectOwner {
		notFound, message = CodeUserNotFound, "This user does not exist."
	}
	account := func(w http.ResponseWriter, r *http.Request) (*auth.Principal, Account, bool) {
		p := principal(w, r)
		if p == nil {
			return nil, Account{}, false
		}
		private(w)
		id, ok := pathID(w, r, notFound, message)
		if !ok {
			return nil, Account{}, false
		}
		return p, Account{Kind: kind, ID: id}, true
	}
	r.Get(prefix+"/package", func(w http.ResponseWriter, r *http.Request) {
		p, acct, ok := account(w, r)
		if !ok {
			return
		}
		out, err := h.svc.AdminAccount(r.Context(), p, acct, r.URL.Query().Get("market_code"))
		if err != nil {
			h.fail(w, r, err, notFound)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, out)
	})
	r.With(limit).Post(prefix+"/package", func(w http.ResponseWriter, r *http.Request) {
		p, acct, ok := account(w, r)
		if !ok {
			return
		}
		var in AssignInput
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		out, err := h.svc.Assign(r.Context(), p, acct, in)
		if err != nil {
			h.fail(w, r, err, notFound)
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, out)
	})
	r.With(limit).Post(prefix+"/package/end", func(w http.ResponseWriter, r *http.Request) {
		p, acct, ok := account(w, r)
		if !ok {
			return
		}
		var in EndInput
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		u, err := h.svc.End(r.Context(), p, acct, in)
		if err != nil {
			h.fail(w, r, err, notFound)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"usage": u})
	})
}

// AgentUsage is GET /v1/agent/organisations/{id}/package.
func (h *Handler) AgentUsage(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	private(w)
	id, ok := pathID(w, r, CodeAccountNotFound, "This listing account does not exist.")
	if !ok {
		return
	}
	out, err := h.svc.AgentUsage(r.Context(), p, id, strings.TrimSpace(r.URL.Query().Get("market_code")))
	if err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// MyUsage is GET /v1/me/package.
func (h *Handler) MyUsage(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	private(w)
	out, err := h.svc.MyUsage(r.Context(), p, strings.TrimSpace(r.URL.Query().Get("market_code")))
	if err != nil {
		h.fail(w, r, err, CodeAccountNotFound)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
