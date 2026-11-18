package outbox

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

type fakeQueue struct {
	events     []Event
	processed  []uuid.UUID
	retried    map[uuid.UUID]time.Time
	deadLetter []uuid.UUID
}

func (q *fakeQueue) Lease(context.Context, string, int, time.Duration) ([]Event, error) {
	out := q.events
	q.events = nil
	return out, nil
}

func (q *fakeQueue) MarkProcessed(_ context.Context, id uuid.UUID, _ string) error {
	q.processed = append(q.processed, id)
	return nil
}

func (q *fakeQueue) MarkRetry(_ context.Context, id uuid.UUID, _, _ string, retryAt time.Time) error {
	if q.retried == nil {
		q.retried = map[uuid.UUID]time.Time{}
	}
	q.retried[id] = retryAt
	return nil
}

func (q *fakeQueue) MarkDeadLetter(_ context.Context, id uuid.UUID, _, _ string) error {
	q.deadLetter = append(q.deadLetter, id)
	return nil
}

func TestDispatcherOutcomes(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ok, failing, exhausted, unknown, panics := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	q := &fakeQueue{events: []Event{
		{ID: ok, Type: "GOOD", Attempts: 1},
		{ID: failing, Type: "BAD", Attempts: 2},
		{ID: exhausted, Type: "BAD", Attempts: 3},
		{ID: unknown, Type: "UNREGISTERED", Attempts: 1},
		{ID: panics, Type: "PANIC", Attempts: 1},
	}}
	d := NewDispatcher(q, slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{
		MaxAttempts: 3, BaseBackoff: time.Minute, MaxBackoff: time.Hour,
	})
	d.now = func() time.Time { return now }
	d.Register("GOOD", HandlerFunc(func(context.Context, Event) error { return nil }))
	d.Register("BAD", HandlerFunc(func(context.Context, Event) error { return errors.New("smtp down") }))
	d.Register("PANIC", HandlerFunc(func(context.Context, Event) error { panic("boom") }))

	n, err := d.ProcessBatch(context.Background())
	if err != nil || n != 5 {
		t.Fatalf("ProcessBatch = %d, %v", n, err)
	}

	if len(q.processed) != 1 || q.processed[0] != ok {
		t.Fatalf("processed = %v, want only the successful event", q.processed)
	}
	if got, want := q.retried[failing], now.Add(2*time.Minute); !got.Equal(want) {
		t.Fatalf("retry at %v, want %v (second attempt backs off 2x base)", got, want)
	}
	if _, retried := q.retried[unknown]; !retried {
		t.Fatal("event without a handler should be retried, not dropped")
	}
	if _, retried := q.retried[panics]; !retried {
		t.Fatal("a panicking handler should be treated as a failure")
	}
	if len(q.deadLetter) != 1 || q.deadLetter[0] != exhausted {
		t.Fatalf("deadLetter = %v, want only the exhausted event", q.deadLetter)
	}
}

func TestBackoff(t *testing.T) {
	base, max := 30*time.Second, time.Hour
	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{8, time.Hour},
		{100, time.Hour},
	}
	for _, tt := range tests {
		if got := Backoff(tt.attempt, base, max); got != tt.want {
			t.Errorf("Backoff(%d) = %v, want %v", tt.attempt, got, tt.want)
		}
	}
}

// A permanent failure is dead-lettered on its first attempt, not retried.
func TestPermanentFailureIsDeadLetteredImmediately(t *testing.T) {
	id := uuid.New()
	q := &fakeQueue{events: []Event{{ID: id, Type: "X", Attempts: 1}}}
	d := NewDispatcher(q, slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{})
	d.Register("X", HandlerFunc(func(context.Context, Event) error {
		return Permanent(errors.New("rejected by the provider"))
	}))
	if _, err := d.ProcessBatch(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(q.deadLetter) != 1 || q.deadLetter[0] != id || len(q.retried) != 0 || len(q.processed) != 0 {
		t.Fatalf("deadLetter=%v retried=%v processed=%v", q.deadLetter, q.retried, q.processed)
	}
	if IsPermanent(errors.New("plain")) || !IsPermanent(Permanent(errors.New("x"))) || Permanent(nil) != nil {
		t.Fatal("Permanent/IsPermanent misbehave")
	}
}

func TestAlsoChainsHandlers(t *testing.T) {
	var calls []string
	h := func(name string, err error) Handler {
		return HandlerFunc(func(context.Context, Event) error { calls = append(calls, name); return err })
	}
	d := NewDispatcher(&fakeQueue{}, slog.New(slog.NewJSONHandler(io.Discard, nil)), Config{})
	d.Also("NEW", h("only", nil))
	d.Register("X", h("first", Permanent(errors.New("gone"))))
	d.Also("X", h("second", errors.New("db down")))
	if err := d.handle(context.Background(), Event{Type: "NEW"}); err != nil || len(calls) != 1 {
		t.Fatalf("alone: %v %v", err, calls)
	}
	calls = nil
	// Both run; a transient failure wins over a permanent one (retry).
	err := d.handle(context.Background(), Event{Type: "X"})
	if err == nil || IsPermanent(err) || len(calls) != 2 || calls[0] != "first" || calls[1] != "second" {
		t.Fatalf("chain: %v %v", err, calls)
	}
	if err := Chain(h("a", Permanent(errors.New("x"))), h("b", nil)).Handle(context.Background(), Event{}); !IsPermanent(err) {
		t.Fatalf("only permanent failures: %v", err)
	}
}
