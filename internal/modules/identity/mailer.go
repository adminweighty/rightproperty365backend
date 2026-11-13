package identity

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
)

// Mailer delivers account emails.
//
// Delivery is currently synchronous (inside the HTTP request) and NOT durable:
// if a send fails the email is lost and the customer has to ask again. It must
// move behind the transactional outbox once an email provider is chosen.
type Mailer interface {
	// SendVerification sends the email-verification link.
	SendVerification(ctx context.Context, to, name, link string) error
	// SendPasswordReset sends the password-reset link.
	SendPasswordReset(ctx context.Context, to, name, link string) error
	// SendAccountExists tells the owner of an already-verified account that
	// someone tried to register with their address (no link, no token).
	SendAccountExists(ctx context.Context, to, name string) error
}

// Links builds the frontend URLs placed in emails.
type Links struct{ base string }

// NewLinks returns a link builder rooted at the web app's base URL.
func NewLinks(webBaseURL string) Links { return Links{base: strings.TrimRight(webBaseURL, "/")} }

// VerifyEmail returns the verification link for token.
func (l Links) VerifyEmail(token string) string {
	return l.base + "/verify-email?token=" + url.QueryEscape(token)
}

// ResetPassword returns the password-reset link for token.
func (l Links) ResetPassword(token string) string {
	return l.base + "/reset-password?token=" + url.QueryEscape(token)
}

// DevMailer is the DEVELOPMENT "delivery": it writes one log line per email
// containing the link, so a developer can click through without a mail
// provider. It must never be used outside development because the link is a
// credential.
type DevMailer struct{ logger *slog.Logger }

// NewDevMailer returns the development mailer.
func NewDevMailer(logger *slog.Logger) *DevMailer { return &DevMailer{logger: logger} }

// SendVerification implements Mailer.
func (m *DevMailer) SendVerification(ctx context.Context, to, _, link string) error {
	m.logger.InfoContext(ctx, "DEV MAIL (not sent): verify-email link", slog.String("to", to), slog.String("link", link))
	return nil
}

// SendPasswordReset implements Mailer.
func (m *DevMailer) SendPasswordReset(ctx context.Context, to, _, link string) error {
	m.logger.InfoContext(ctx, "DEV MAIL (not sent): reset-password link", slog.String("to", to), slog.String("link", link))
	return nil
}

// SendAccountExists implements Mailer.
func (m *DevMailer) SendAccountExists(ctx context.Context, to, _ string) error {
	m.logger.InfoContext(ctx, "DEV MAIL (not sent): you already have an account", slog.String("to", to))
	return nil
}

// UnconfiguredMailer is used in staging/production until a real provider is
// wired in. It sends nothing and logs a warning WITHOUT the recipient, link or
// token, so credentials never reach the logs.
type UnconfiguredMailer struct{ logger *slog.Logger }

// NewUnconfiguredMailer returns the placeholder mailer.
func NewUnconfiguredMailer(logger *slog.Logger) *UnconfiguredMailer {
	return &UnconfiguredMailer{logger: logger}
}

func (m *UnconfiguredMailer) warn(ctx context.Context, kind string) error {
	m.logger.WarnContext(ctx, "email NOT sent: no email provider is configured", slog.String("email_kind", kind))
	return nil
}

// SendVerification implements Mailer.
func (m *UnconfiguredMailer) SendVerification(ctx context.Context, _, _, _ string) error {
	return m.warn(ctx, "verify_email")
}

// SendPasswordReset implements Mailer.
func (m *UnconfiguredMailer) SendPasswordReset(ctx context.Context, _, _, _ string) error {
	return m.warn(ctx, "password_reset")
}

// SendAccountExists implements Mailer.
func (m *UnconfiguredMailer) SendAccountExists(ctx context.Context, _, _ string) error {
	return m.warn(ctx, "account_exists")
}
