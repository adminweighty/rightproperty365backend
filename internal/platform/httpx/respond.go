// Package httpx holds HTTP plumbing shared by every module: the JSON error
// envelope, request decoding and the middleware stack.
package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"

	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes used in the error envelope. Clients should branch on these, not
// on the human-readable message.
const (
	CodeValidationFailed     = "VALIDATION_FAILED"
	CodeInvalidJSON          = "INVALID_JSON"
	CodeUnsupportedMediaType = "UNSUPPORTED_MEDIA_TYPE"
	CodePayloadTooLarge      = "PAYLOAD_TOO_LARGE"
	CodeNotFound             = "NOT_FOUND"
	CodeMethodNotAllowed     = "METHOD_NOT_ALLOWED"
	CodeRateLimited          = "RATE_LIMITED"
	CodeOriginNotAllowed     = "ORIGIN_NOT_ALLOWED"
	CodeUnauthenticated      = "UNAUTHENTICATED"
	CodeForbidden            = "FORBIDDEN"
	CodeCSRFOrigin           = "CSRF_ORIGIN"
	CodeNotReady             = "NOT_READY"
	CodeTimeout              = "TIMEOUT"
	CodeInternal             = "INTERNAL"
)

// ErrorBody is the "error" member of the envelope.
type ErrorBody struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
}

// ErrorEnvelope is the single error shape returned by every endpoint.
type ErrorEnvelope struct {
	Error   ErrorBody `json:"error"`
	TraceID string    `json:"trace_id"`
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes the standard error envelope. fields may be nil.
func WriteError(w http.ResponseWriter, r *http.Request, status int, code, message string, fields map[string]string) {
	WriteJSON(w, status, ErrorEnvelope{
		Error:   ErrorBody{Code: code, Message: message, Fields: fields},
		TraceID: logging.RequestID(r.Context()),
	})
}

// WriteInternalError logs err and returns an opaque 500 (or 504 when the
// request deadline was exceeded). Internal details never reach the client.
func WriteInternalError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		logger.ErrorContext(r.Context(), "request timed out", slog.String("error", err.Error()))
		WriteError(w, r, http.StatusGatewayTimeout, CodeTimeout, "The request took too long. Please try again.", nil)
		return
	}
	logger.ErrorContext(r.Context(), "internal error", slog.String("error", err.Error()))
	WriteError(w, r, http.StatusInternalServerError, CodeInternal, "Something went wrong on our side. Please try again.", nil)
}

// NotFound is the router's 404 handler.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, CodeNotFound, "The requested resource was not found.", nil)
}

// MethodNotAllowed is the router's 405 handler.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "This method is not allowed for the requested resource.", nil)
}

// DecodeJSON strictly decodes a JSON request body into dst. On failure it
// writes the error response itself and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, "Content-Type must be application/json.", nil)
		return false
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "The request body is too large.", nil)
			return false
		}
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON, "The request body is not valid JSON for this endpoint.", nil)
		return false
	}
	if dec.More() {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON, "The request body must contain a single JSON object.", nil)
		return false
	}
	return true
}
