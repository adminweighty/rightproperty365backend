package identity

import "context"

// Mailer sends the account emails that carry a secret link.
//
// These are sent from the request path, not through the outbox: the token in
// the link is a credential and must never be persisted in an outbox payload.
// The implementation (internal/modules/notifications) renders the templates,
// builds the link from the configured public web URL, retries briefly and
// records the outcome without storing the token.
type Mailer interface {
	// SendVerification sends the "confirm your email address" email.
	SendVerification(ctx context.Context, to, name, token string) error
	// SendPasswordReset sends the "reset your password" email.
	SendPasswordReset(ctx context.Context, to, name, token string) error
	// SendAccountExists tells the owner of an already-verified account that
	// someone tried to register with their address (no token).
	SendAccountExists(ctx context.Context, to, name string) error
}
