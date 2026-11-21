package uploads

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Error codes.
const (
	CodeNotFound = "UPLOAD_INTENT_NOT_FOUND"
	CodeState    = "UPLOAD_INTENT_STATE"
	CodeExpired  = "UPLOAD_INTENT_EXPIRED"
	CodeTooMany  = "UPLOAD_INTENTS_LIMIT"
	CodeRejected = "UPLOAD_REJECTED"
)

// Handler serves /v1/media/upload-intents.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

var reasonText = map[string]string{
	ReasonMissing:      "Nothing was uploaded before the upload window closed.",
	ReasonSizeMismatch: "The file is not the size that was declared.",
	ReasonTooLarge:     "The file is larger than allowed.",
	ReasonChecksum:     "The file's SHA-256 does not match the one declared.",
	ReasonType:         "The file is not one of the allowed types (checked from its content).",
	ReasonInfected:     "The file failed our virus check and was deleted.",
	ReasonTooLargeScan: "The file is too large to be checked for viruses.",
}

// Fail writes an upload error (also used by the consumers).
func Fail(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	var verr *ValidationError
	var rej *RejectedError
	var pub *storage.PublicError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &rej):
		status := http.StatusUnprocessableEntity
		if rej.Reason == ReasonMissing {
			status = http.StatusConflict
		}
		httpx.WriteError(w, r, status, CodeRejected, reasonText[rej.Reason], map[string]string{"reason": rej.Reason})
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This upload does not exist.", nil)
	case errors.Is(err, ErrState):
		httpx.WriteError(w, r, http.StatusConflict, CodeState, "This upload cannot be used for this now.", nil)
	case errors.Is(err, ErrExpired):
		httpx.WriteError(w, r, http.StatusGone, CodeExpired, "The upload window has closed. Start a new upload.", nil)
	case errors.Is(err, ErrTooMany):
		httpx.WriteError(w, r, http.StatusTooManyRequests, CodeTooMany, "You have too many uploads waiting. Finish or wait for them first.", nil)
	case errors.Is(err, ErrNoUpload):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "Sign in to upload.", nil)
	case errors.As(err, &pub):
		httpx.WriteError(w, r, pub.Status, pub.Code, pub.Message, nil)
	default:
		httpx.WriteInternalError(w, r, logger, err)
	}
}

type intentView struct {
	ID          uuid.UUID  `json:"id"`
	Purpose     string     `json:"purpose"`
	ContentType string     `json:"content_type"`
	SizeBytes   int64      `json:"size_bytes"`
	Status      string     `json:"status"`
	Reason      *string    `json:"reason"`
	CreatedAt   time.Time  `json:"created_at"`
	UploadBy    time.Time  `json:"upload_by"`
	CompletedAt *time.Time `json:"completed_at"`
	ExpiresAt   time.Time  `json:"expires_at"`
}

func viewOf(i Intent) intentView {
	v := intentView{ID: i.ID, Purpose: i.Purpose, ContentType: i.ContentType, SizeBytes: i.SizeBytes, Status: i.Status,
		CreatedAt: i.CreatedAt, UploadBy: i.UploadBy, CompletedAt: i.CompletedAt, ExpiresAt: i.ExpiresAt}
	if i.Reason != "" {
		r := i.Reason
		v.Reason = &r
	}
	return v
}

func intentID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "intentId"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This upload does not exist.", nil)
		return uuid.Nil, false
	}
	return id, true
}

// Create serves POST /v1/media/upload-intents.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	i, up, err := h.svc.Create(r.Context(), auth.FromContext(r.Context()), in)
	if err != nil {
		Fail(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"intent": viewOf(i), "upload": map[string]any{
		"method": up.Method, "url": up.URL, "headers": up.Headers, "direct": up.Direct, "expires_at": up.ExpiresAt,
	}})
}

// Get serves GET /v1/media/upload-intents/{intentId}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := intentID(w, r)
	if !ok {
		return
	}
	i, err := h.svc.Get(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		Fail(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"intent": viewOf(i)})
}

// Content serves PUT /v1/media/upload-intents/{intentId}/content: the file
// through the API, when storage cannot take it directly.
func (h *Handler) Content(w http.ResponseWriter, r *http.Request) {
	id, ok := intentID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	if err := h.svc.PutContent(r.Context(), auth.FromContext(r.Context()), id, r.Body); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, httpx.CodePayloadTooLarge, "The file is too large.", nil)
			return
		}
		Fail(w, r, h.logger, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Complete serves POST /v1/media/upload-intents/{intentId}/complete.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	id, ok := intentID(w, r)
	if !ok {
		return
	}
	i, err := h.svc.Complete(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		Fail(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"intent": viewOf(i)})
}
