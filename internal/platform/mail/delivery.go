package mail

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Delivery statuses of one email message.
const (
	// StatusAttempted: a send is in progress or was tried and may be retried.
	StatusAttempted = "ATTEMPTED"
	// StatusProviderAccepted: the provider accepted the message and returned
	// an ID. This is NOT "delivered": what happens after acceptance is unknown.
	StatusProviderAccepted = "PROVIDER_ACCEPTED"
	// StatusFailed: the last attempt failed. Terminal says whether it will be retried.
	StatusFailed = "FAILED"
)

// MaxKeyLength is the longest notification key (the provider's idempotency-key limit).
const MaxKeyLength = 256

// Notification is one logical email to one recipient.
type Notification struct {
	// Key identifies the logical notification, e.g.
	// "property-request-received/<event id>/customer". It is the delivery-log
	// key and the provider idempotency key: delivering the same Key twice
	// sends at most one email. It must not contain a secret.
	Key string
	// RecipientUserID is set when the recipient is a known account.
	RecipientUserID *uuid.UUID
	Message         Message
}

// Record is the delivery-log row of a notification. It holds no address (only
// a SHA-256 of it), no subject, no body and no link.
type Record struct {
	Key               string
	Kind              string
	RecipientUserID   *uuid.UUID
	RecipientSHA256   string
	Status            string
	Terminal          bool // meaningful when Status is FAILED
	Attempts          int
	Provider          string
	ProviderMessageID string
	LastError         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// DeliveryLog records what happened to each notification.
type DeliveryLog interface {
	// Begin registers an attempt: it creates the record as ATTEMPTED, or, for
	// an existing record that is neither PROVIDER_ACCEPTED nor terminally
	// FAILED, increments Attempts and sets it back to ATTEMPTED. It returns
	// the record as it was BEFORE this call (nil when it did not exist).
	Begin(ctx context.Context, rec Record, now time.Time) (previous *Record, err error)
	// Accepted marks the notification PROVIDER_ACCEPTED with the provider's ID.
	Accepted(ctx context.Context, key, provider, providerMessageID string, now time.Time) error
	// Failed marks the notification FAILED with a short, safe reason.
	Failed(ctx context.Context, key, reason string, terminal bool, now time.Time) error
}

// ErrTerminal wraps a delivery failure that retrying cannot fix (the
// provider rejected the message). Outbox handlers turn it into a dead letter.
var ErrTerminal = errors.New("email cannot be sent")

// Service delivers notifications once each.
type Service struct {
	sender Sender
	log    DeliveryLog
	logger *slog.Logger
	now    func() time.Time
}

// NewService builds a Service.
func NewService(sender Sender, log DeliveryLog, logger *slog.Logger) *Service {
	return &Service{sender: sender, log: log, logger: logger, now: time.Now}
}

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// SenderName names the transport in use ("resend" or "log").
func (s *Service) SenderName() string { return s.sender.Name() }

// RecipientHash is the hex SHA-256 of a lower-cased address. Delivery records
// and log lines carry this instead of the address.
func RecipientHash(address string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(address))))
	return hex.EncodeToString(sum[:])
}

// reservedSuffixes are the top-level domains reserved for testing and
// documentation (RFC 2606 and RFC 6761).
var reservedSuffixes = []string{".test", ".example", ".invalid", ".localhost"}

// IsReservedTestAddress reports whether an address is on a domain that can
// never receive real mail: example.com, example.org, example.net, any
// *.example.* domain, or one ending in .test, .example, .invalid or
// .localhost. An address without a domain counts as reserved too.
func IsReservedTestAddress(address string) bool {
	address = strings.ToLower(strings.TrimSpace(address))
	if i := strings.LastIndex(address, ">"); i >= 0 {
		address = address[:i]
	}
	at := strings.LastIndex(address, "@")
	if at < 0 || at == len(address)-1 {
		return true
	}
	domain := strings.TrimSuffix(address[at+1:], ".")
	for _, suffix := range reservedSuffixes {
		// "." + domain so that the bare names ("localhost", "test") count too.
		if strings.HasSuffix("."+domain, suffix) {
			return true
		}
	}
	labels := strings.Split(domain, ".")
	for i, label := range labels {
		// example.com / example.org / example.net, and any *.example.<tld>
		if label == "example" && i < len(labels)-1 {
			return true
		}
	}
	return false
}

// Deliver sends n unless it was already accepted by the provider.
//
// With the real transport (Resend), a recipient on a reserved test domain
// (IsReservedTestAddress) is skipped with an info log line that does not name
// the address. The development LogSender sends nothing anyway and still
// prints such messages for developers.
//
//   - Already PROVIDER_ACCEPTED: nothing is sent; nil.
//   - Already terminally FAILED: nothing is sent; an error wrapping ErrTerminal.
//   - Otherwise one send (the transport may retry briefly). Success is
//     recorded as PROVIDER_ACCEPTED. A failure is recorded as FAILED and
//     returned: wrapping ErrTerminal when retrying cannot help, or a plain
//     error when the caller should try again later.
//
// The notification key is also the provider idempotency key, so even a crash
// between the provider accepting the message and the record being updated
// cannot lead to a second email on the retry.
func (s *Service) Deliver(ctx context.Context, n Notification) error {
	if n.Key == "" || len(n.Key) > MaxKeyLength {
		return fmt.Errorf("%w: invalid notification key", ErrTerminal)
	}
	hash := RecipientHash(n.Message.To)
	if s.sender.Name() == resendName && IsReservedTestAddress(n.Message.To) {
		// Sample and test accounts (@example.test and the like) are never
		// emailed through a real provider: the address cannot receive mail
		// and bounces hurt the sending domain. Nothing is recorded or sent.
		s.logger.InfoContext(ctx, "email skipped: the recipient is on a reserved test domain",
			slog.String("email_kind", n.Message.Kind), slog.String("notification_key", n.Key))
		return nil
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	previous, err := s.log.Begin(ctx, Record{
		Key: n.Key, Kind: n.Message.Kind, RecipientUserID: n.RecipientUserID, RecipientSHA256: hash, Provider: s.sender.Name(),
	}, now)
	if err != nil {
		return fmt.Errorf("record email attempt: %w", err)
	}
	attrs := []any{slog.String("email_kind", n.Message.Kind), slog.String("notification_key", n.Key), slog.String("recipient_sha256", hash)}
	if previous != nil {
		switch {
		case previous.Status == StatusProviderAccepted:
			s.logger.InfoContext(ctx, "email already accepted by the provider; not sending again", attrs...)
			return nil
		case previous.Status == StatusFailed && previous.Terminal:
			return fmt.Errorf("%w: %s", ErrTerminal, previous.LastError)
		}
	}

	providerID, err := s.sender.Send(ctx, n.Key, n.Message)
	now = s.now().UTC().Truncate(time.Microsecond)
	if err != nil {
		terminal := !IsTransient(err)
		reason := "send_failed"
		var se *SendError
		if errors.As(err, &se) {
			reason = se.Error()
		}
		if logErr := s.log.Failed(context.WithoutCancel(ctx), n.Key, reason, terminal, now); logErr != nil {
			s.logger.ErrorContext(ctx, "could not record the failed email", append(attrs, slog.String("error", logErr.Error()))...)
		}
		s.logger.WarnContext(ctx, "email was not accepted by the provider", append(attrs, slog.String("reason", reason), slog.Bool("terminal", terminal))...)
		if terminal {
			return fmt.Errorf("%w: %s", ErrTerminal, reason)
		}
		return errors.New(reason)
	}

	if err := s.log.Accepted(context.WithoutCancel(ctx), n.Key, s.sender.Name(), providerID, now); err != nil {
		// The provider has it. A retry reuses the idempotency key, so it is safe.
		return fmt.Errorf("record provider acceptance: %w", err)
	}
	if s.sender.Name() == "log" {
		// Nothing was sent: the development transport already wrote its own line.
		return nil
	}
	s.logger.InfoContext(ctx, "email accepted by the provider", append(attrs, slog.String("provider", s.sender.Name()), slog.String("provider_message_id", providerID))...)
	return nil
}

// ---------------------------------------------------------------------------
// Memory log
// ---------------------------------------------------------------------------

// MemoryDeliveryLog is an in-memory DeliveryLog for development and tests.
type MemoryDeliveryLog struct {
	mu      sync.Mutex
	records map[string]*Record
	order   []string
}

// NewMemoryDeliveryLog returns an empty log.
func NewMemoryDeliveryLog() *MemoryDeliveryLog {
	return &MemoryDeliveryLog{records: make(map[string]*Record)}
}

// Begin implements DeliveryLog.
func (l *MemoryDeliveryLog) Begin(_ context.Context, rec Record, now time.Time) (*Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	existing, ok := l.records[rec.Key]
	if !ok {
		rec.Status, rec.Attempts, rec.CreatedAt, rec.UpdatedAt = StatusAttempted, 1, now, now
		l.records[rec.Key] = &rec
		l.order = append(l.order, rec.Key)
		return nil, nil
	}
	previous := *existing
	if existing.Status == StatusProviderAccepted || (existing.Status == StatusFailed && existing.Terminal) {
		return &previous, nil
	}
	existing.Status, existing.Attempts, existing.UpdatedAt = StatusAttempted, existing.Attempts+1, now
	return &previous, nil
}

// Accepted implements DeliveryLog.
func (l *MemoryDeliveryLog) Accepted(_ context.Context, key, provider, providerMessageID string, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[key]
	if !ok {
		return fmt.Errorf("no email record %q", key)
	}
	rec.Status, rec.Terminal, rec.Provider, rec.ProviderMessageID, rec.LastError, rec.UpdatedAt = StatusProviderAccepted, false, provider, providerMessageID, "", now
	return nil
}

// Failed implements DeliveryLog.
func (l *MemoryDeliveryLog) Failed(_ context.Context, key, reason string, terminal bool, now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[key]
	if !ok {
		return fmt.Errorf("no email record %q", key)
	}
	rec.Status, rec.Terminal, rec.LastError, rec.UpdatedAt = StatusFailed, terminal, reason, now
	return nil
}

// Records returns a copy of every record in creation order (test helper).
func (l *MemoryDeliveryLog) Records() []Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]Record, 0, len(l.order))
	for _, k := range l.order {
		out = append(out, *l.records[k])
	}
	return out
}

// ---------------------------------------------------------------------------
// Postgres log
// ---------------------------------------------------------------------------

// PostgresDeliveryLog stores delivery records in email_messages
// (migrations/0006_email_messages.up.sql).
//
// NOTE: compiled but not yet exercised against a real database.
type PostgresDeliveryLog struct{ pool *pgxpool.Pool }

// NewPostgresDeliveryLog returns a Postgres-backed log.
func NewPostgresDeliveryLog(pool *pgxpool.Pool) *PostgresDeliveryLog {
	return &PostgresDeliveryLog{pool: pool}
}

// Begin implements DeliveryLog. The row lock taken by SELECT ... FOR UPDATE
// serialises two workers that pick up the same notification.
func (l *PostgresDeliveryLog) Begin(ctx context.Context, rec Record, now time.Time) (*Record, error) {
	tx, err := l.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO email_messages (id, notification_key, kind, recipient_user_id, recipient_sha256, status, attempts, provider, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, 'ATTEMPTED', 1, $6, $7, $7)
		ON CONFLICT (notification_key) DO NOTHING`,
		uuid.New(), rec.Key, rec.Kind, rec.RecipientUserID, rec.RecipientSHA256, rec.Provider, now)
	if err != nil {
		return nil, fmt.Errorf("insert email record: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil, tx.Commit(ctx)
	}

	var prev Record
	var providerID, lastError *string
	err = tx.QueryRow(ctx, `
		SELECT notification_key, kind, recipient_user_id, recipient_sha256, status, terminal, attempts,
		       provider, provider_message_id, last_error, created_at, updated_at
		FROM email_messages WHERE notification_key = $1 FOR UPDATE`, rec.Key).Scan(
		&prev.Key, &prev.Kind, &prev.RecipientUserID, &prev.RecipientSHA256, &prev.Status, &prev.Terminal, &prev.Attempts,
		&prev.Provider, &providerID, &lastError, &prev.CreatedAt, &prev.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("load email record: %w", err)
	}
	if providerID != nil {
		prev.ProviderMessageID = *providerID
	}
	if lastError != nil {
		prev.LastError = *lastError
	}
	if prev.Status == StatusProviderAccepted || (prev.Status == StatusFailed && prev.Terminal) {
		return &prev, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE email_messages SET status = 'ATTEMPTED', terminal = false, attempts = attempts + 1, updated_at = $2
		WHERE notification_key = $1`, rec.Key, now); err != nil {
		return nil, fmt.Errorf("update email record: %w", err)
	}
	return &prev, tx.Commit(ctx)
}

// Accepted implements DeliveryLog.
func (l *PostgresDeliveryLog) Accepted(ctx context.Context, key, provider, providerMessageID string, now time.Time) error {
	_, err := l.pool.Exec(ctx, `
		UPDATE email_messages
		SET status = 'PROVIDER_ACCEPTED', terminal = false, provider = $2, provider_message_id = $3,
		    last_error = NULL, accepted_at = $4, updated_at = $4
		WHERE notification_key = $1`, key, provider, providerMessageID, now)
	return err
}

// Failed implements DeliveryLog.
func (l *PostgresDeliveryLog) Failed(ctx context.Context, key, reason string, terminal bool, now time.Time) error {
	_, err := l.pool.Exec(ctx, `
		UPDATE email_messages
		SET status = 'FAILED', terminal = $3, last_error = $2, updated_at = $4
		WHERE notification_key = $1 AND status <> 'PROVIDER_ACCEPTED'`, key, reason, terminal, now)
	return err
}
