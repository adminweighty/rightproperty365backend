package businessenquiries

import (
	"errors"
	"log/slog"
	"net/http"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes specific to this module.
const (
	CodeRecipientNotFound    = "RECIPIENT_NOT_FOUND"
	CodeRecipientUnavailable = "RECIPIENT_UNAVAILABLE"
	CodeIdempotencyConflict  = "IDEMPOTENCY_CONFLICT"
)

// HeaderIdempotencyKey may carry the idempotency key instead of the body.
const HeaderIdempotencyKey = "Idempotency-Key"

// Handler serves POST /v1/business-enquiries and GET /v1/inbox/business-enquiries.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

// Create accepts an enquiry to a directory profile. It responds 201 only
// after the enquiry and its outbox event are committed, or 200 for an
// idempotent replay.
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
		switch {
		case errors.As(err, &verr):
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		case errors.Is(err, ErrRecipientNotFound):
			httpx.WriteError(w, r, http.StatusNotFound, CodeRecipientNotFound, "This profile does not exist.", nil)
		case errors.Is(err, ErrRecipientUnavailable):
			httpx.WriteError(w, r, http.StatusGone, CodeRecipientUnavailable, "This profile is not accepting enquiries.", nil)
		case errors.Is(err, ErrIdempotencyConflict):
			httpx.WriteError(w, r, http.StatusConflict, CodeIdempotencyConflict, "This idempotency key was already used for a different enquiry.", nil)
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
	h.logger.InfoContext(r.Context(), "business enquiry accepted",
		slog.String("business_enquiry_id", res.EnquiryID.String()),
		slog.String("reference", res.Reference),
		slog.Bool("replayed", res.Replayed),
	)
	httpx.WriteJSON(w, status, res)
}

// inboxResponse is the body of GET /v1/inbox/business-enquiries.
type inboxResponse struct {
	Items []InboxItem `json:"items"`
}

// Inbox lists the enquiries received by the caller's organisation(s). The
// route sits behind auth.RequireMembership; the scope below is derived solely
// from the caller's memberships, so a member of one organisation can never
// read another organisation's enquiries, and an agent or staff member only
// reads the enquiries addressed to their own agent profile.
func (h *Handler) Inbox(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
		return
	}
	items, err := h.svc.Inbox(r.Context(), InboxScope{
		UserID:                 p.UserID,
		ManagedOrganisationIDs: p.OrganisationIDs(auth.RoleOwner, auth.RoleManager),
		MemberOrganisationIDs:  p.OrganisationIDs(auth.RoleAgent, auth.RoleStaff),
	})
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inboxResponse{Items: items})
}
