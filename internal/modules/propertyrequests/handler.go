package propertyrequests

import (
	"errors"
	"log/slog"
	"net/http"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes specific to this module.
const (
	CodeListingNotFound     = "LISTING_NOT_FOUND"
	CodeListingUnavailable  = "LISTING_UNAVAILABLE"
	CodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
	// CodeDatesUnavailable: a request to stay names at least one night that is taken.
	CodeDatesUnavailable = "DATES_UNAVAILABLE"
)

// HeaderIdempotencyKey may carry the idempotency key instead of the body.
const HeaderIdempotencyKey = "Idempotency-Key"

// Handler serves POST /v1/property-requests and GET /v1/agent/property-requests.
type Handler struct {
	svc    *Service
	logger *slog.Logger
	// names resolves listing IDs to their public summary for the inbox. Nil
	// leaves `listing` null.
	names ListingNames
	// markets resolves country codes to market IDs for a market
	// administrator's routing exceptions. Nil means such an administrator
	// sees none.
	markets MarketIDs
}

// SetListingNames makes the inbox show which listing each request is about.
func (h *Handler) SetListingNames(names ListingNames) { h.names = names }

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// Create accepts a property request. It responds 201 only after the request
// and its outbox event are committed, or 200 for an idempotent replay.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}

	// The customer's identity comes from the session only, never from the body.
	if p := auth.FromContext(r.Context()); p != nil {
		in.Customer = &Customer{UserID: p.UserID, Name: p.Name, Email: p.Email}
	}

	res, err := h.svc.Create(r.Context(), in, r.Header.Get(HeaderIdempotencyKey))
	if err != nil {
		var verr *ValidationError
		var taken *DatesUnavailableError
		switch {
		case errors.As(err, &verr):
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		case errors.Is(err, ErrListingNotFound):
			httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
		case errors.Is(err, ErrListingUnavailable):
			httpx.WriteError(w, r, http.StatusGone, CodeListingUnavailable, "This listing is no longer accepting requests.", nil)
		case errors.As(err, &taken):
			httpx.WriteError(w, r, http.StatusConflict, CodeDatesUnavailable, "Some of these nights are no longer available.",
				map[string]string{"check_in": "is not available for every night: the first night that is taken is " + taken.Date})
		case errors.Is(err, ErrIdempotencyConflict):
			httpx.WriteError(w, r, http.StatusConflict, CodeIdempotencyConflict, "This idempotency key was already used for a different request.", nil)
		default:
			httpx.WriteInternalError(w, r, h.logger, err)
		}
		return
	}

	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	// Log identifiers only; message and contact details are personal data.
	h.logger.InfoContext(r.Context(), "property request accepted",
		slog.String("property_request_id", res.RequestID.String()),
		slog.String("reference", res.Reference),
		slog.Bool("replayed", res.Replayed),
	)
	httpx.WriteJSON(w, status, res)
}
