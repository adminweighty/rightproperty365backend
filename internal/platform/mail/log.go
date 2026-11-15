package mail

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
)

// LogSender is the DEVELOPMENT transport: it sends nothing and writes one log
// line per email, including the email's action link so a developer can click
// through without a mail provider. That link can be a credential (a
// verification or reset link), which is why this transport is refused outside
// development.
type LogSender struct{ logger *slog.Logger }

// NewLogSender returns the development transport.
func NewLogSender(logger *slog.Logger) *LogSender { return &LogSender{logger: logger} }

// Name implements Sender.
func (s *LogSender) Name() string { return "log" }

// logLabels keeps the two historical log messages that the README and the
// frontend developers rely on to find the links.
var logLabels = map[string]string{
	"verify-email":   "verify-email link",
	"reset-password": "reset-password link",
}

// Send implements Sender. It never fails.
func (s *LogSender) Send(ctx context.Context, _ string, m Message) (string, error) {
	label, ok := logLabels[m.Kind]
	if !ok {
		label = m.Kind
	}
	attrs := []any{slog.String("to", m.To), slog.String("subject", m.Subject)}
	if m.DevLink != "" {
		attrs = append(attrs, slog.String("link", m.DevLink))
	}
	if m.DevCode != "" {
		attrs = append(attrs, slog.String("code", m.DevCode))
	}
	s.logger.InfoContext(ctx, "DEV MAIL (not sent): "+label, attrs...)
	return "log-" + uuid.NewString(), nil
}
