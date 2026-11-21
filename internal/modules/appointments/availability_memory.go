package appointments

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
)

// Settings implements AvailabilityStore.
func (s *MemoryStore) Settings(_ context.Context, userID uuid.UUID) (Settings, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	got, ok := s.settings[userID]
	return got, ok, nil
}

// SaveSettings implements AvailabilityStore.
func (s *MemoryStore) SaveSettings(_ context.Context, st Settings) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st.WorkingHours, st.Exceptions = slices.Clone(st.WorkingHours), slices.Clone(st.Exceptions)
	s.settings[st.UserID] = st
	return nil
}

// Services implements AvailabilityStore.
func (s *MemoryStore) Services(_ context.Context, userID uuid.UUID) ([]ConsultationService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ConsultationService{}
	for _, c := range s.services {
		if c.UserID == userID {
			out = append(out, c)
		}
	}
	sortServices(out)
	return out, nil
}

// sortServices orders active services first, then newest first.
func sortServices(out []ConsultationService) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Active != out[j].Active {
			return out[i].Active
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
}

// Service implements AvailabilityStore.
func (s *MemoryStore) Service(_ context.Context, id uuid.UUID) (ConsultationService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.services[id]
	if !ok {
		return ConsultationService{}, ErrServiceNotFound
	}
	return c, nil
}

// SaveService implements AvailabilityStore.
func (s *MemoryStore) SaveService(_ context.Context, c ConsultationService, insert bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if insert && c.Active {
		active := 0
		for _, x := range s.services {
			if x.UserID == c.UserID && x.Active {
				active++
			}
		}
		if active >= MaxActiveServices {
			return ErrTooManyServices
		}
	}
	if !insert {
		if _, ok := s.services[c.ID]; !ok {
			return ErrServiceNotFound
		}
	}
	s.services[c.ID] = c
	return nil
}

// BusyFor implements AvailabilityStore.
func (s *MemoryStore) BusyFor(_ context.Context, userID uuid.UUID, from, to time.Time, except uuid.UUID) ([]Busy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Busy{}
	for id, a := range s.byID {
		if id == except || !a.Open() || a.AssignedUserID == nil || *a.AssignedUserID != userID {
			continue
		}
		out = appendBusy(out, a, from, to)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// appendBusy adds the appointment's time, and a pending proposed time, when
// they overlap [from, to).
func appendBusy(out []Busy, a Appointment, from, to time.Time) []Busy {
	length := time.Duration(a.DurationMinutes) * time.Minute
	add := func(start time.Time) {
		if b := (Busy{Start: start, End: start.Add(length)}); b.Start.Before(to) && from.Before(b.End) {
			out = append(out, b)
		}
	}
	add(a.StartUTC)
	if a.Status == StatusRescheduleRequested && a.ProposedStartUTC != nil {
		add(*a.ProposedStartUTC)
	}
	return out
}

// SendReminders implements ReminderStore.
func (s *MemoryStore) SendReminders(_ context.Context, now time.Time, ahead time.Duration, limit int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, id := range s.order {
		if n >= limit {
			break
		}
		a := s.byID[id]
		if a.Status != StatusConfirmed || !a.StartUTC.After(now) || a.StartUTC.After(now.Add(ahead)) ||
			(a.RemindedForStart != nil && a.RemindedForStart.Equal(a.StartUTC)) {
			continue
		}
		start := a.StartUTC
		a.RemindedForStart = &start
		s.byID[id] = a
		if err := s.emit(EventReminder, a, reminderEntry(a, now)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
