// Package mail renders and sends the platform's email.
//
// Layers, from the outside in:
//
//   - Renderer turns a template name and Data into a Message (subject, HTML
//     and plain-text parts) using html/template and text/template.
//   - Service delivers a Notification exactly once per logical notification:
//     it records every message in a DeliveryLog (ATTEMPTED, PROVIDER_ACCEPTED,
//     FAILED), skips one the provider already accepted, and passes the
//     notification key to the provider as its idempotency key.
//   - Sender is the transport: ResendSender for real delivery, LogSender for
//     development.
//
// Vocabulary matters: a provider that returns 200 has ACCEPTED the message.
// Nothing in this package knows, or may claim, that an email was delivered.
//
// Secrets: the provider API key never appears in an error, a log line or a
// stored record, and request headers are never logged.
package mail

import (
	"context"
	"errors"
	"fmt"
)

// Message is one rendered email.
type Message struct {
	// To is a single recipient address.
	To      string
	Subject string
	HTML    string
	Text    string
	// Kind is the template name, e.g. "verify-email". It is sent to the
	// provider as a tag and recorded in the delivery log.
	Kind string
	// DevLink is the email's action link. It exists only so the development
	// LogSender can print it; real transports ignore it and it is never stored.
	DevLink string
	// DevCode is the sign-in code of a sign-in-code email, for the same
	// development LogSender only; never stored, ignored by real transports.
	DevCode string
}

// Sender is an email transport.
type Sender interface {
	// Send hands the message to the provider and returns the provider's
	// message ID. idempotencyKey identifies the logical notification: sending
	// the same key again must not produce a second email.
	//
	// A failure is returned as *SendError; Transient says whether trying
	// again later can help.
	Send(ctx context.Context, idempotencyKey string, m Message) (providerID string, err error)
	// Name identifies the transport in logs and records ("resend", "log").
	Name() string
}

// SendError is a failed send. Its text is safe to log and store: it carries a
// status code, the provider's short error name and a sanitised explanation,
// never a header, the API key, the recipient or the message body.
type SendError struct {
	// Status is the HTTP status, or 0 for a network failure.
	Status int
	// Name is the provider's error name (e.g. "rate_limit_exceeded") or a
	// local one ("network_error", "timeout", "invalid_response").
	Name string
	// Transient reports whether a later retry may succeed.
	Transient bool
	// Detail is the provider's own explanation, sanitised: credentials and
	// email addresses are removed and the text is cut short. It exists so an
	// operator can tell "domain is not verified" from "API key is invalid".
	// It is for logs only and is never shown to a customer.
	Detail string
}

func (e *SendError) Error() string {
	kind := "terminal"
	if e.Transient {
		kind = "transient"
	}
	detail := ""
	if e.Detail != "" {
		detail = " (" + e.Detail + ")"
	}
	if e.Status == 0 {
		return fmt.Sprintf("email provider %s failure: %s%s", kind, e.Name, detail)
	}
	return fmt.Sprintf("email provider %s failure: HTTP %d %s%s", kind, e.Status, e.Name, detail)
}

// IsTransient reports whether err is a send failure worth retrying later.
// Errors that are not a *SendError (for example a database failure) are
// treated as transient: nothing was decided about the email itself.
func IsTransient(err error) bool {
	var se *SendError
	if errors.As(err, &se) {
		return se.Transient
	}
	return err != nil
}
