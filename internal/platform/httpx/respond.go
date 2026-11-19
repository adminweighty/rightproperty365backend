// Package httpx holds HTTP plumbing shared by every module: the JSON error
// envelope, request decoding and the middleware stack.
package httpx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"rightproperty365.com/backend/internal/platform/i18n"
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

// PublicError is an error that carries its own response: a status, an
// error code and a message that is safe to show (storage.PublicError: a
// file still being scanned for viruses, the scanner being down).
type PublicError interface {
	error
	HTTPStatus() int
	ErrorCode() string
	PublicMessage() string
}

// WriteInternalError logs err and returns an opaque 500 (or 504 when the
// request deadline was exceeded). Internal details never reach the client.
// A PublicError is answered with its own status, code and message.
func WriteInternalError(w http.ResponseWriter, r *http.Request, logger *slog.Logger, err error) {
	var pe PublicError
	if errors.As(err, &pe) {
		logger.WarnContext(r.Context(), "request refused", slog.String("code", pe.ErrorCode()), slog.Int("status", pe.HTTPStatus()))
		if pe.HTTPStatus() == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "60")
		}
		WriteError(w, r, pe.HTTPStatus(), pe.ErrorCode(), pe.PublicMessage(), nil)
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		logger.ErrorContext(r.Context(), "request timed out", slog.String("error", err.Error()))
		WriteError(w, r, http.StatusGatewayTimeout, CodeTimeout, i18n.T(r.Context(), "error.timeout", nil), nil)
		return
	}
	logger.ErrorContext(r.Context(), "internal error", slog.String("error", err.Error()))
	WriteError(w, r, http.StatusInternalServerError, CodeInternal, i18n.T(r.Context(), "error.internal", nil), nil)
}

// WriteValidation writes 400 VALIDATION_FAILED with the shared message.
func WriteValidation(w http.ResponseWriter, r *http.Request, fields map[string]string) {
	WriteError(w, r, http.StatusBadRequest, CodeValidationFailed, i18n.T(r.Context(), "error.validation_failed", nil), fields)
}

// NotFound is the router's 404 handler.
func NotFound(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusNotFound, CodeNotFound, i18n.T(r.Context(), "error.not_found", nil), nil)
}

// MethodNotAllowed is the router's 405 handler.
func MethodNotAllowed(w http.ResponseWriter, r *http.Request) {
	WriteError(w, r, http.StatusMethodNotAllowed, CodeMethodNotAllowed, i18n.T(r.Context(), "error.method_not_allowed", nil), nil)
}

// DecodeJSON strictly decodes a JSON request body into dst. On failure it
// writes the error response itself and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, i18n.T(r.Context(), "error.unsupported_media_type", nil), nil)
		return false
	}

	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			WriteError(w, r, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, i18n.T(r.Context(), "error.payload_too_large", nil), nil)
			return false
		}
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON, i18n.T(r.Context(), "error.invalid_json", nil), nil)
		return false
	}
	if dec.More() {
		WriteError(w, r, http.StatusBadRequest, CodeInvalidJSON, i18n.T(r.Context(), "error.single_json_object", nil), nil)
		return false
	}
	return true
}

// WriteCacheableJSON writes a PUBLIC response that may be cached: it sets
// Cache-Control (public, max-age, stale-while-revalidate) and a strong ETag
// derived from the body, and answers 304 Not Modified when the request's
// If-None-Match matches.
//
// Use it only for responses that are identical for every caller. It must
// never be used for anything that depends on the session: the response says
// "public" and carries no Vary on Cookie or Authorization.
func WriteCacheableJSON(w http.ResponseWriter, r *http.Request, v any, maxAge, staleWhileRevalidate time.Duration) {
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(v); err != nil {
		WriteError(w, r, http.StatusInternalServerError, CodeInternal, i18n.T(r.Context(), "error.internal", nil), nil)
		return
	}
	sum := sha256.Sum256(body.Bytes())
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`

	h := w.Header()
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d",
		int(maxAge.Seconds()), int(staleWhileRevalidate.Seconds())))
	h.Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body.Bytes())
}

// etagMatches implements the weak comparison of If-None-Match (RFC 9110).
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}
