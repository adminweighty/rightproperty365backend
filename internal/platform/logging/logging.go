// Package logging configures structured JSON logging (log/slog) and carries the
// request ID through context so every log line of a request can be correlated.
package logging

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
)

type requestIDKey struct{}

// WithRequestID returns a context carrying the request ID.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID returns the request ID stored in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

type userIDKey struct{}

// WithUserIDSlot returns a context holding an empty, mutable user-ID slot.
// The access-log middleware installs it at the top of the stack so that the
// authentication middleware further down can fill it in (SetUserID) and every
// log line of the request, including the access-log line written after the
// handler returns, carries user_id.
func WithUserIDSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, userIDKey{}, new(atomic.Pointer[string]))
}

// SetUserID records the authenticated user's ID in the slot installed by
// WithUserIDSlot. It is a no-op when the context has no slot.
func SetUserID(ctx context.Context, id string) {
	if slot, ok := ctx.Value(userIDKey{}).(*atomic.Pointer[string]); ok {
		slot.Store(&id)
	}
}

// UserID returns the authenticated user's ID recorded for this request, or "".
func UserID(ctx context.Context) string {
	if slot, ok := ctx.Value(userIDKey{}).(*atomic.Pointer[string]); ok {
		if id := slot.Load(); id != nil {
			return *id
		}
	}
	return ""
}

// New returns a JSON logger that stamps every record with the service name,
// environment and, when the context carries them, the request ID and the
// authenticated user ID. The request
// ID doubles as the trace ID until distributed tracing is introduced.
func New(w io.Writer, level slog.Level, service, env string) *slog.Logger {
	base := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		// Log timestamps in UTC regardless of the host's time zone.
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
				a.Value = slog.TimeValue(a.Value.Time().UTC())
			}
			return a
		},
	})
	return slog.New(contextHandler{base}).With(
		slog.String("service", service),
		slog.String("env", env),
	)
}

type contextHandler struct{ slog.Handler }

func (h contextHandler) Handle(ctx context.Context, rec slog.Record) error {
	if id := RequestID(ctx); id != "" {
		rec.AddAttrs(slog.String("request_id", id))
	}
	if id := UserID(ctx); id != "" {
		rec.AddAttrs(slog.String("user_id", id))
	}
	return h.Handler.Handle(ctx, rec)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}
