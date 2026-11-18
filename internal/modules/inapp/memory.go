package inapp

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DeliveryRecord is a delivery row as the memory store keeps it.
type DeliveryRecord struct {
	ID             uuid.UUID
	NotificationID uuid.UUID
	UserID         uuid.UUID
	Channel        string
	DeviceID       *uuid.UUID
	Target         string
	Status         string
	Attempts       int
	LastError      string
	MessageID      string
	NextAttemptAt  *time.Time
	LastAttemptAt  *time.Time
	BatchID        *uuid.UUID
	Summary        bool
	CreatedAt      time.Time
}

type memPrefKey struct {
	user uuid.UUID
	typ  string
}

// MemoryStore is the in-memory Store (development and tests).
type MemoryStore struct {
	mu            sync.Mutex
	devices       map[uuid.UUID]*Device
	notifications []*Notification
	deliveries    []*DeliveryRecord
	prefs         map[memPrefKey]Preference
	// History is every preference change, oldest first (tests read it).
	history []PreferenceHistory
}

// PreferenceHistory is one recorded preference change.
type PreferenceHistory struct {
	UserID    uuid.UUID
	Type      string
	Email     bool
	Push      bool
	ChangedAt time.Time
	Source    string
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{devices: map[uuid.UUID]*Device{}, prefs: map[memPrefKey]Preference{}}
}

// History returns the preference changes (tests).
func (s *MemoryStore) History() []PreferenceHistory {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]PreferenceHistory(nil), s.history...)
}

// DeleteUser removes everything of a user (account deletion; the accounts
// module's MemoryDeps hook).
func (s *MemoryStore) DeleteUser(userID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, d := range s.devices {
		if d.UserID == userID {
			delete(s.devices, id)
		}
	}
	s.notifications = slices.DeleteFunc(s.notifications, func(n *Notification) bool { return n.UserID == userID })
	s.deliveries = slices.DeleteFunc(s.deliveries, func(d *DeliveryRecord) bool { return d.UserID == userID })
	for k := range s.prefs {
		if k.user == userID {
			delete(s.prefs, k)
		}
	}
	s.history = slices.DeleteFunc(s.history, func(h PreferenceHistory) bool { return h.UserID == userID })
}

// Deliveries returns copies of the delivery records of a notification (tests).
func (s *MemoryStore) Deliveries(notificationID uuid.UUID) []DeliveryRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DeliveryRecord
	for _, d := range s.deliveries {
		if d.NotificationID == notificationID {
			out = append(out, *d)
		}
	}
	return out
}

// UpsertDevice implements Store.
func (s *MemoryStore) UpsertDevice(_ context.Context, d Device) (Device, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.devices {
		if existing.Token != d.Token {
			continue
		}
		if existing.UserID != d.UserID {
			s.retireDeviceLocked(existing.ID, false)
			existing.CreatedAt = d.LastSeenAt
		}
		existing.UserID, existing.SessionID, existing.Platform = d.UserID, d.SessionID, d.Platform
		existing.AppVersion, existing.Locale, existing.PushConsent, existing.LastSeenAt = d.AppVersion, d.Locale, d.PushConsent, d.LastSeenAt
		return *existing, false, nil
	}
	d.ID = uuid.New()
	d.CreatedAt = d.LastSeenAt
	c := d
	s.devices[d.ID] = &c
	return d, true, nil
}

// retireDeviceLocked dead-letters the open push deliveries of a device that
// is removed or changes hands, and (when it is removed) unlinks it from its
// records, as ON DELETE SET NULL does.
func (s *MemoryStore) retireDeviceLocked(deviceID uuid.UUID, unlink bool) {
	for _, d := range s.deliveries {
		if d.DeviceID == nil || *d.DeviceID != deviceID {
			continue
		}
		if d.Status == StatusPending || d.Status == StatusFailed || d.Status == StatusAttempted {
			d.Status, d.LastError, d.NextAttemptAt = StatusDead, "device removed", nil
		}
		if unlink {
			d.DeviceID = nil // as ON DELETE SET NULL
		}
	}
}

// Devices implements Store.
func (s *MemoryStore) Devices(_ context.Context, userID uuid.UUID) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Device{}
	for _, d := range s.devices {
		if d.UserID == userID {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DeleteDevice implements Store.
func (s *MemoryStore) DeleteDevice(_ context.Context, userID, deviceID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.devices[deviceID]
	if !ok || d.UserID != userID {
		return ErrDeviceNotFound
	}
	s.retireDeviceLocked(deviceID, true)
	delete(s.devices, deviceID)
	return nil
}

// DeleteDeviceByToken implements Store.
func (s *MemoryStore) DeleteDeviceByToken(_ context.Context, userID *uuid.UUID, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, d := range s.devices {
		if d.Token == token && (userID == nil || d.UserID == *userID) {
			s.retireDeviceLocked(id, true)
			delete(s.devices, id)
			return nil
		}
	}
	return ErrDeviceNotFound
}

// DeleteSessionDevices implements Store.
func (s *MemoryStore) DeleteSessionDevices(_ context.Context, userID, sessionID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, d := range s.devices {
		if d.UserID == userID && d.SessionID != nil && *d.SessionID == sessionID {
			s.retireDeviceLocked(id, true)
			delete(s.devices, id)
			n++
		}
	}
	return n, nil
}

// CreateNotification implements Store.
func (s *MemoryStore) CreateNotification(_ context.Context, d Draft, push bool, now time.Time) (Notification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notifications {
		if n.SourceEventID == d.SourceEventID && n.UserID == d.UserID {
			return *n, false, nil
		}
	}
	n := &Notification{ID: uuid.New(), UserID: d.UserID, SourceEventID: d.SourceEventID, Type: d.Type, Title: d.Title,
		Body: d.Body, Link: d.Link, Reference: d.Reference, CreatedAt: now}
	s.notifications = append(s.notifications, n)
	if push {
		for _, dev := range s.devices {
			if dev.UserID != d.UserID || !dev.PushConsent {
				continue
			}
			id, due := dev.ID, now
			s.deliveries = append(s.deliveries, &DeliveryRecord{ID: uuid.New(), NotificationID: n.ID, UserID: d.UserID, Channel: ChannelPush,
				DeviceID: &id, Target: id.String(), Status: StatusPending, NextAttemptAt: &due, CreatedAt: now})
		}
	}
	return *n, true, nil
}

// NotificationBySource implements Store.
func (s *MemoryStore) NotificationBySource(_ context.Context, sourceEventID, userID uuid.UUID) (Notification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notifications {
		if n.SourceEventID == sourceEventID && n.UserID == userID {
			return *n, true, nil
		}
	}
	return Notification{}, false, nil
}

// List implements Store.
func (s *MemoryStore) List(_ context.Context, userID uuid.UUID, unreadOnly bool, offset, limit int) ([]Notification, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var all []Notification
	for _, n := range s.notifications {
		if n.UserID == userID && (!unreadOnly || n.ReadAt == nil) {
			all = append(all, *n)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].CreatedAt.After(all[j].CreatedAt) })
	total := len(all)
	if offset >= total {
		return []Notification{}, total, nil
	}
	return all[offset:min(total, offset+limit)], total, nil
}

// UnreadCount implements Store.
func (s *MemoryStore) UnreadCount(_ context.Context, userID uuid.UUID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, x := range s.notifications {
		if x.UserID == userID && x.ReadAt == nil {
			n++
		}
	}
	return n, nil
}

// MarkRead implements Store.
func (s *MemoryStore) MarkRead(_ context.Context, userID, id uuid.UUID, now time.Time) (Notification, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.notifications {
		if n.ID == id && n.UserID == userID {
			if n.ReadAt == nil {
				t := now
				n.ReadAt = &t
			}
			return *n, nil
		}
	}
	return Notification{}, ErrNotFound
}

// MarkAllRead implements Store.
func (s *MemoryStore) MarkAllRead(_ context.Context, userID uuid.UUID, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := 0
	for _, n := range s.notifications {
		if n.UserID == userID && n.ReadAt == nil {
			t := now
			n.ReadAt = &t
			c++
		}
	}
	return c, nil
}

// RecordEmail implements Store.
func (s *MemoryStore) RecordEmail(_ context.Context, notificationID uuid.UUID, status string, attempts int, lastError string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var userID uuid.UUID
	for _, n := range s.notifications {
		if n.ID == notificationID {
			userID = n.UserID
		}
	}
	for _, d := range s.deliveries {
		if d.NotificationID == notificationID && d.Channel == ChannelEmail {
			d.Status, d.Attempts, d.LastError, d.LastAttemptAt = status, attempts, lastError, &now
			return nil
		}
	}
	s.deliveries = append(s.deliveries, &DeliveryRecord{ID: uuid.New(), NotificationID: notificationID, UserID: userID, Channel: ChannelEmail,
		Target: "email", Status: status, Attempts: attempts, LastError: lastError, LastAttemptAt: &now, CreatedAt: now})
	return nil
}

// ClaimPush implements Store.
func (s *MemoryStore) ClaimPush(_ context.Context, now time.Time, limit int, leaseFor time.Duration) ([]PushJob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []*DeliveryRecord
	for _, d := range s.deliveries {
		if d.Channel != ChannelPush || d.DeviceID == nil || d.NextAttemptAt == nil || d.NextAttemptAt.After(now) {
			continue
		}
		if d.Status == StatusPending || d.Status == StatusFailed || d.Status == StatusAttempted {
			due = append(due, d)
		}
	}
	sort.SliceStable(due, func(i, j int) bool { return due[i].CreatedAt.Before(due[j].CreatedAt) })
	var out []PushJob
	for _, d := range due {
		if len(out) == limit {
			break
		}
		dev, ok := s.devices[*d.DeviceID]
		if !ok {
			continue
		}
		var n Notification
		for _, x := range s.notifications {
			if x.ID == d.NotificationID {
				n = *x
			}
		}
		lease := now.Add(leaseFor)
		d.Status, d.Attempts, d.NextAttemptAt = StatusAttempted, d.Attempts+1, &lease
		out = append(out, PushJob{DeliveryID: d.ID, Attempts: d.Attempts, Notification: n, DeviceID: dev.ID, Token: dev.Token})
	}
	return out, nil
}

// DeferPush implements Store.
func (s *MemoryStore) DeferPush(_ context.Context, ids []uuid.UUID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deliveries {
		if slices.Contains(ids, d.ID) && d.Status == StatusAttempted {
			t := at
			d.Status, d.Attempts, d.NextAttemptAt = StatusPending, max(0, d.Attempts-1), &t
		}
	}
	return nil
}

// RecordPush implements Store.
func (s *MemoryStore) RecordPush(_ context.Context, o PushOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.deliveries {
		if !slices.Contains(o.DeliveryIDs, d.ID) {
			continue
		}
		at, batch := o.At, o.BatchID
		d.Status, d.MessageID, d.LastError, d.NextAttemptAt = o.Status, o.MessageID, o.LastError, o.NextAttemptAt
		d.LastAttemptAt, d.BatchID, d.Summary = &at, &batch, o.Summary
	}
	return nil
}

// PushBatchesSince implements Store.
func (s *MemoryStore) PushBatchesSince(_ context.Context, deviceID uuid.UUID, since time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	batches := map[uuid.UUID]bool{}
	for _, d := range s.deliveries {
		if d.DeviceID != nil && *d.DeviceID == deviceID && d.BatchID != nil && d.LastAttemptAt != nil && d.LastAttemptAt.After(since) {
			batches[*d.BatchID] = true
		}
	}
	return len(batches), nil
}

// Preferences implements Store.
func (s *MemoryStore) Preferences(_ context.Context, userID uuid.UUID) (map[string]Preference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]Preference{}
	for k, p := range s.prefs {
		if k.user == userID {
			out[k.typ] = p
		}
	}
	return out, nil
}

// SetPreferences implements Store.
func (s *MemoryStore) SetPreferences(_ context.Context, userID uuid.UUID, changes []PreferenceChange, source string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range changes {
		k := memPrefKey{userID, c.Type}
		p, ok := s.prefs[k]
		if !ok {
			p = Preference{Type: c.Type, Email: true, Push: true}
		}
		next := p
		if c.Email != nil {
			next.Email = *c.Email
		}
		if c.Push != nil {
			next.Push = *c.Push
		}
		if ok && next.Email == p.Email && next.Push == p.Push {
			continue
		}
		if !ok && next.Email && next.Push {
			continue // the default, unchanged
		}
		t := now
		next.ChangedAt, next.Source = &t, source
		s.prefs[k] = next
		s.history = append(s.history, PreferenceHistory{UserID: userID, Type: c.Type, Email: next.Email, Push: next.Push, ChangedAt: now, Source: source})
	}
	return nil
}

// Health implements Store.
func (s *MemoryStore) Health(_ context.Context, now time.Time) (Health, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h := Health{Counts: emptyCounts()}
	for _, d := range s.deliveries {
		h.Counts[d.Channel][d.Status]++
		if d.Status == StatusDead {
			h.DeadLetterBacklog++
		}
		if d.Channel == ChannelPush && (d.Status == StatusPending || d.Status == StatusFailed) {
			if h.OldestUnattemptedAt == nil || d.CreatedAt.Before(*h.OldestUnattemptedAt) {
				t := d.CreatedAt
				h.OldestUnattemptedAt = &t
			}
		}
	}
	for _, n := range s.notifications {
		if n.CreatedAt.After(now.Add(-24 * time.Hour)) {
			h.NotificationsLast24h++
		}
		if n.ReadAt == nil {
			h.UnreadTotal++
		}
	}
	return h, nil
}

func emptyCounts() map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, ch := range []string{ChannelPush, ChannelEmail} {
		out[ch] = map[string]int{}
		for _, st := range Statuses() {
			out[ch][st] = 0
		}
	}
	return out
}
