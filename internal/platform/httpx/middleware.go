package httpx

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/logging"
)

// HeaderRequestID is accepted from clients and always emitted on responses.
const HeaderRequestID = "X-Request-Id"

const (
	corsAllowMethods  = "GET, POST, PUT, PATCH, DELETE, OPTIONS"
	corsAllowHeaders  = "Content-Type, Idempotency-Key, Authorization, X-Request-Id, If-None-Match, X-Client"
	corsExposeHeaders = "X-Request-Id, Retry-After, ETag, Content-Disposition, X-Total-Count, X-Export-Truncated"
	corsMaxAge        = "600"
)

// RequestID accepts a well-formed X-Request-Id or generates one, echoes it on
// the response and stores it in the request context.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !validRequestID(id) {
			id = uuid.NewString()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(logging.WithRequestID(r.Context(), id)))
	})
}

// validRequestID restricts client-supplied IDs so they are safe to log and echo.
func validRequestID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// secretPathPrefixes are the routes whose next path segment is a credential
// (an invitation or signing token): it is never written to the access log.
var secretPathPrefixes = []string{"/v1/invitations/", "/v1/signing/"}

// LoggedPath is the request path as the access log writes it: the secret
// segment of a credential route replaced by "{token}".
func LoggedPath(path string) string {
	for _, prefix := range secretPathPrefixes {
		if rest, ok := strings.CutPrefix(path, prefix); ok && rest != "" {
			if _, tail, found := strings.Cut(rest, "/"); found {
				return prefix + "{token}/" + tail
			}
			return prefix + "{token}"
		}
	}
	return path
}

// AccessLog writes one structured line per request. Query strings are not
// logged because they may carry personal data, and the
// token of an invitation link is replaced (LoggedPath). It installs the user-ID slot
// (logging.WithUserIDSlot) so the line carries user_id when the request was
// authenticated further down the stack.
func AccessLog(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			r = r.WithContext(logging.WithUserIDSlot(r.Context()))
			next.ServeHTTP(rec, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}
			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}
			logger.LogAttrs(r.Context(), level, "http request",
				slog.String("method", r.Method),
				slog.String("path", LoggedPath(r.URL.Path)),
				slog.Int("status", status),
				slog.Int("bytes", rec.bytes),
				slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
				slog.String("remote_ip", ClientIP(r)),
				slog.String("user_agent", r.UserAgent()),
			)
		})
	}
}

// Recover turns panics into a logged 500 with the standard error envelope.
func Recover(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rv := recover()
				if rv == nil {
					return
				}
				if rv == http.ErrAbortHandler {
					panic(rv)
				}
				logger.ErrorContext(r.Context(), "panic recovered",
					slog.Any("panic", rv),
					slog.String("stack", string(debug.Stack())),
				)
				if rec, ok := w.(*statusRecorder); ok && rec.status != 0 {
					return // response already started; nothing safe to add
				}
				WriteError(w, r, http.StatusInternalServerError, CodeInternal, "Something went wrong on our side. Please try again.", nil)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets conservative headers suitable for a JSON-only API.
func SecurityHeaders(hsts bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
			h.Set("Cache-Control", "no-store")
			if hsts {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CORS implements an exact-match origin allowlist. Allowlisted origins may
// send credentials (the session cookie): the response then echoes that exact
// origin, never "*", together with Access-Control-Allow-Credentials.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		allowed[o] = struct{}{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Add("Vary", "Origin")

			origin := r.Header.Get("Origin")
			_, ok := allowed[origin]
			preflight := r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != ""

			if origin != "" && ok {
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Credentials", "true")
				h.Set("Access-Control-Expose-Headers", corsExposeHeaders)
			}
			if !preflight {
				next.ServeHTTP(w, r)
				return
			}

			h.Add("Vary", "Access-Control-Request-Method")
			h.Add("Vary", "Access-Control-Request-Headers")
			if !ok {
				WriteError(w, r, http.StatusForbidden, CodeOriginNotAllowed, "This origin is not allowed to call the API.", nil)
				return
			}
			h.Set("Access-Control-Allow-Methods", corsAllowMethods)
			h.Set("Access-Control-Allow-Headers", corsAllowHeaders)
			h.Set("Access-Control-Max-Age", corsMaxAge)
			w.WriteHeader(http.StatusNoContent)
		})
	}
}

// BodyLimit caps request bodies; decoding past the limit yields a 413.
func BodyLimit(maxBytes int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// BodyLimitFunc is BodyLimit with a per-request limit, for the few routes
// (file uploads) that need more than the default.
func BodyLimitFunc(limit func(*http.Request) int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, limit(r))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds the request context so slow downstream calls (database) are
// cancelled. Handlers map context.DeadlineExceeded to 504 via WriteInternalError.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ClientIP returns the peer IP of the connection. Forwarding headers
// (X-Forwarded-For etc.) are deliberately NOT trusted here: behind a load
// balancer this must be replaced with trusted-proxy-aware resolution,
// otherwise every client shares the balancer's IP.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// PrivateNoStore marks every answer of the routes it wraps, errors included
// (a 401 from the auth middleware too), Cache-Control: private, no-store.
func PrivateNoStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		next.ServeHTTP(w, r)
	})
}
