// Package outbox implements the transactional-outbox dispatcher run by the
// worker binary.
//
// Delivery is at-least-once: an event is leased, handled, then marked
// processed. A crash between "handled" and "marked" makes the event visible
// again once its lease expires, so every Handler MUST be idempotent — use
// Event.ID as the deduplication key for any side effect (e.g. a unique
// notification row keyed by event ID).
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Event is a leased outbox row.
type Event struct {
	ID            uuid.UUID
	Type          string
	AggregateType string
	AggregateID   uuid.UUID
	Payload       json.RawMessage
	// Attempts counts leases including the current one (1 on first delivery).
	Attempts  int
	CreatedAt time.Time
}

// Handler processes one event type. Returning an error schedules a retry.
type Handler interface {
	Handle(ctx context.Context, ev Event) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, ev Event) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, ev Event) error { return f(ctx, ev) }

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent wraps err so the dispatcher dead-letters the event immediately
// instead of retrying it. Use it when the outcome cannot change, for example
// an email the provider rejected as invalid.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err was wrapped with Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// Sink receives events as they are written by an in-memory store. It exists
// for development without a database, where there is no outbox table for the
// worker to poll; see cmd/api.
type Sink func(Event)

// Queue is the storage behind the dispatcher (Postgres in production).
type Queue interface {
	// Lease claims up to limit due events for leaseFor and increments their attempts.
	Lease(ctx context.Context, workerID string, limit int, leaseFor time.Duration) ([]Event, error)
	// MarkProcessed records successful handling.
	MarkProcessed(ctx context.Context, id uuid.UUID, workerID string) error
	// MarkRetry releases the lease and makes the event due again at retryAt.
	MarkRetry(ctx context.Context, id uuid.UUID, workerID, lastError string, retryAt time.Time) error
	// MarkDeadLetter parks the event permanently for manual inspection.
	MarkDeadLetter(ctx context.Context, id uuid.UUID, workerID, lastError string) error
}

// Config tunes the dispatcher. Zero values fall back to defaults.
type Config struct {
	WorkerID       string
	PollInterval   time.Duration
	BatchSize      int
	LeaseDuration  time.Duration // must exceed BatchSize * HandlerTimeout
	HandlerTimeout time.Duration
	MaxAttempts    int
	BaseBackoff    time.Duration
	MaxBackoff     time.Duration
}

func (c Config) withDefaults() Config {
	if c.WorkerID == "" {
		c.WorkerID = "worker-" + uuid.NewString()
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 2 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 10
	}
	if c.HandlerTimeout <= 0 {
		c.HandlerTimeout = 20 * time.Second
	}
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = time.Duration(c.BatchSize)*c.HandlerTimeout + time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	if c.BaseBackoff <= 0 {
		c.BaseBackoff = 30 * time.Second
	}
	if c.MaxBackoff <= 0 {
		c.MaxBackoff = time.Hour
	}
	return c
}

// Dispatcher polls the queue and routes events to handlers by type.
type Dispatcher struct {
	queue    Queue
	handlers map[string]Handler
	cfg      Config
	logger   *slog.Logger
	now      func() time.Time
}

// NewDispatcher builds a dispatcher.
func NewDispatcher(queue Queue, logger *slog.Logger, cfg Config) *Dispatcher {
	return &Dispatcher{queue: queue, handlers: make(map[string]Handler), cfg: cfg.withDefaults(), logger: logger, now: time.Now}
}

// Register sets the handler for an event type. Call before Run.
func (d *Dispatcher) Register(eventType string, h Handler) { d.handlers[eventType] = h }

// Run polls until ctx is cancelled. The in-flight event is allowed to finish.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.logger.Info("outbox dispatcher started",
		slog.String("worker_id", d.cfg.WorkerID),
		slog.Int("batch_size", d.cfg.BatchSize),
		slog.Duration("poll_interval", d.cfg.PollInterval),
	)
	ticker := time.NewTicker(d.cfg.PollInterval)
	defer ticker.Stop()

	for {
		n, err := d.ProcessBatch(ctx)
		if err != nil && ctx.Err() == nil {
			d.logger.Error("outbox poll failed", slog.String("error", err.Error()))
		}
		if n == d.cfg.BatchSize && ctx.Err() == nil {
			continue // backlog: drain without waiting
		}
		select {
		case <-ctx.Done():
			d.logger.Info("outbox dispatcher stopped")
			return nil
		case <-ticker.C:
		}
	}
}

// ProcessBatch leases and handles one batch, returning how many events were leased.
func (d *Dispatcher) ProcessBatch(ctx context.Context) (int, error) {
	events, err := d.queue.Lease(ctx, d.cfg.WorkerID, d.cfg.BatchSize, d.cfg.LeaseDuration)
	if err != nil {
		return 0, fmt.Errorf("lease outbox events: %w", err)
	}
	for _, ev := range events {
		d.process(ctx, ev)
	}
	return len(events), nil
}

func (d *Dispatcher) process(ctx context.Context, ev Event) {
	// Bookkeeping must survive shutdown so a handled event is not redelivered needlessly.
	bookkeeping := context.WithoutCancel(ctx)
	log := d.logger.With(
		slog.String("event_id", ev.ID.String()),
		slog.String("event_type", ev.Type),
		slog.Int("attempt", ev.Attempts),
	)

	err := d.handle(ctx, ev)
	if err == nil {
		if err := d.queue.MarkProcessed(bookkeeping, ev.ID, d.cfg.WorkerID); err != nil {
			// The event will be redelivered after the lease expires (at-least-once).
			log.Error("mark outbox event processed failed", slog.String("error", err.Error()))
		}
		return
	}

	if ev.Attempts >= d.cfg.MaxAttempts || IsPermanent(err) {
		log.Error("outbox event dead-lettered", slog.String("error", err.Error()), slog.Bool("permanent", IsPermanent(err)))
		if mErr := d.queue.MarkDeadLetter(bookkeeping, ev.ID, d.cfg.WorkerID, truncate(err.Error())); mErr != nil {
			log.Error("mark outbox event dead-lettered failed", slog.String("error", mErr.Error()))
		}
		return
	}

	retryAt := d.now().Add(Backoff(ev.Attempts, d.cfg.BaseBackoff, d.cfg.MaxBackoff))
	log.Warn("outbox event failed, will retry", slog.String("error", err.Error()), slog.Time("retry_at", retryAt))
	if mErr := d.queue.MarkRetry(bookkeeping, ev.ID, d.cfg.WorkerID, truncate(err.Error()), retryAt); mErr != nil {
		log.Error("mark outbox event for retry failed", slog.String("error", mErr.Error()))
	}
}

func (d *Dispatcher) handle(ctx context.Context, ev Event) (err error) {
	h, ok := d.handlers[ev.Type]
	if !ok {
		return fmt.Errorf("no handler registered for event type %q", ev.Type)
	}
	defer func() {
		if rv := recover(); rv != nil {
			err = fmt.Errorf("handler panic: %v", rv)
		}
	}()
	hctx, cancel := context.WithTimeout(ctx, d.cfg.HandlerTimeout)
	defer cancel()
	if err := h.Handle(hctx, ev); err != nil {
		return err
	}
	if hctx.Err() != nil {
		return errors.New("handler returned after its context ended")
	}
	return nil
}

// Backoff returns the delay before retrying after the given attempt number
// (1-based): base, 2*base, 4*base ... capped at max.
func Backoff(attempt int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= max {
			return max
		}
	}
	return min(d, max)
}

func truncate(s string) string {
	const limit = 1000
	if len(s) > limit {
		return s[:limit]
	}
	return s
}
