package inapp

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/push"
)

// Sender sends the queued push deliveries (the worker; the API process
// without a database). It runs after the notification's commit, so a push
// outage never loses an inbox item: deliveries wait and are retried with
// exponential backoff (MaxPushAttempts tries, then DEAD) once the provider
// recovers.
type Sender struct {
	store  Store
	pusher push.Pusher
	logger *slog.Logger
	now    func() time.Time

	// Interval between polls; BatchSize deliveries per poll; Lease is how
	// long a claimed delivery is ours; BaseBackoff and MaxBackoff shape the
	// retries.
	Interval    time.Duration
	BatchSize   int
	Lease       time.Duration
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// NewSender builds the push sender.
func NewSender(store Store, pusher push.Pusher, logger *slog.Logger, now func() time.Time) *Sender {
	if now == nil {
		now = time.Now
	}
	return &Sender{store: store, pusher: pusher, logger: logger, now: now,
		Interval: 2 * time.Second, BatchSize: 100, Lease: 2 * time.Minute, BaseBackoff: 30 * time.Second, MaxBackoff: time.Hour}
}

// Run polls until ctx ends.
func (s *Sender) Run(ctx context.Context) {
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		if _, err := s.ProcessOnce(ctx); err != nil && ctx.Err() == nil {
			s.logger.Error("push sender poll failed", slog.String("error", err.Error()))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// backoff is base, 2*base, 4*base ... capped.
func (s *Sender) backoff(attempt int) time.Duration {
	d := s.BaseBackoff
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= s.MaxBackoff {
			return s.MaxBackoff
		}
	}
	return min(d, s.MaxBackoff)
}

// ProcessOnce claims the due deliveries and sends them, per device: one
// push each, or one summary push when more than BurstLimit would reach the
// device within BurstWindow; a device that already had BurstLimit pushes in
// the window waits until it passes. It returns how many deliveries it
// claimed.
func (s *Sender) ProcessOnce(ctx context.Context) (int, error) {
	now := s.now().UTC()
	jobs, err := s.store.ClaimPush(ctx, now, s.BatchSize, s.Lease)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	var order []uuid.UUID
	byDevice := map[uuid.UUID][]PushJob{}
	for _, j := range jobs {
		if _, ok := byDevice[j.DeviceID]; !ok {
			order = append(order, j.DeviceID)
		}
		byDevice[j.DeviceID] = append(byDevice[j.DeviceID], j)
	}
	bookkeeping := context.WithoutCancel(ctx)
	for _, dev := range order {
		group := byDevice[dev]
		recent, err := s.store.PushBatchesSince(ctx, dev, now.Add(-BurstWindow))
		if err != nil {
			return len(jobs), err
		}
		switch {
		case recent >= BurstLimit:
			if err := s.store.DeferPush(bookkeeping, ids(group), now.Add(BurstWindow)); err != nil {
				return len(jobs), err
			}
		case len(group) > 1 && recent+len(group) > BurstLimit:
			s.send(ctx, group, true)
		default:
			for _, j := range group {
				s.send(ctx, []PushJob{j}, false)
			}
		}
	}
	return len(jobs), nil
}

func ids(jobs []PushJob) []uuid.UUID {
	out := make([]uuid.UUID, len(jobs))
	for i, j := range jobs {
		out[i] = j.DeliveryID
	}
	return out
}

// send pushes one message for the jobs of one device (a summary when there
// are several) and records the outcome on every one of them.
func (s *Sender) send(ctx context.Context, jobs []PushJob, summary bool) {
	first := jobs[0]
	msg := push.Message{Token: first.Token}
	if summary {
		msg.Title, msg.Body = "RightProperty365", "You have "+strconv.Itoa(len(jobs))+" new notifications"
		msg.Data = map[string]string{"type": TypeSummary, "link": "/account/notifications", "notification_id": ""}
		msg.CollapseKey = "summary"
	} else {
		n := first.Notification
		msg.Title, msg.Body = n.Title, n.Body
		msg.Data = map[string]string{"type": n.Type, "link": n.Link, "notification_id": n.ID.String()}
	}
	if unread, err := s.store.UnreadCount(ctx, first.Notification.UserID); err == nil {
		msg.Badge = &unread
	}
	res, err := s.pusher.Send(ctx, msg)
	now := s.now().UTC()
	o := PushOutcome{DeliveryIDs: ids(jobs), BatchID: uuid.New(), Summary: summary, At: now}
	attempts := 0
	for _, j := range jobs {
		attempts = max(attempts, j.Attempts)
	}
	log := s.logger.With(slog.String("device_id", first.DeviceID.String()), slog.Int("deliveries", len(jobs)), slog.Int("attempt", attempts))
	switch kind := push.KindOf(err); {
	case err == nil:
		o.Status, o.MessageID = StatusProviderAccepted, res.MessageID
	case kind == push.InvalidToken:
		o.Status, o.LastError = StatusDead, err.Error()
		log.Info("push token invalid: device removed", slog.String("error", err.Error()))
		if dErr := s.store.DeleteDeviceByToken(context.WithoutCancel(ctx), nil, first.Token); dErr != nil && !errors.Is(dErr, ErrDeviceNotFound) {
			log.Error("could not remove the invalid device", slog.String("error", dErr.Error()))
		}
	case kind == push.Permanent || attempts >= MaxPushAttempts:
		o.Status, o.LastError = StatusDead, err.Error()
		log.Warn("push dead-lettered", slog.String("error", err.Error()))
	default:
		wait := s.backoff(attempts)
		var pe *push.Error
		if errors.As(err, &pe) && pe.RetryAfter > wait {
			wait = pe.RetryAfter
		}
		next := now.Add(wait)
		o.Status, o.LastError, o.NextAttemptAt = StatusFailed, err.Error(), &next
		log.Warn("push failed, will retry", slog.String("error", err.Error()), slog.Time("retry_at", next))
	}
	if err := s.store.RecordPush(context.WithoutCancel(ctx), o); err != nil {
		log.Error("could not record the push outcome", slog.String("error", err.Error()))
	}
}
