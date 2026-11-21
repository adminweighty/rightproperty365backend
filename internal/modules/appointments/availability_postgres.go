package appointments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Settings implements AvailabilityStore.
func (s *PostgresStore) Settings(ctx context.Context, userID uuid.UUID) (Settings, bool, error) {
	var st Settings
	var hours, exceptions []byte
	var updated time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT user_id, enabled, timezone, working_hours, exceptions, slot_minutes, buffer_minutes,
		       lead_time_hours, horizon_days, max_per_day, updated_at
		FROM appointment_settings WHERE user_id = $1`, userID).Scan(
		&st.UserID, &st.Enabled, &st.Timezone, &hours, &exceptions, &st.SlotMinutes, &st.BufferMinutes,
		&st.LeadTimeHours, &st.HorizonDays, &st.MaxPerDay, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return Settings{}, false, nil
	}
	if err != nil {
		return Settings{}, false, err
	}
	if err := json.Unmarshal(hours, &st.WorkingHours); err != nil {
		return Settings{}, false, fmt.Errorf("decode working hours: %w", err)
	}
	if err := json.Unmarshal(exceptions, &st.Exceptions); err != nil {
		return Settings{}, false, fmt.Errorf("decode exceptions: %w", err)
	}
	updated = updated.UTC()
	st.UpdatedAt = &updated
	return st, true, nil
}

// SaveSettings implements AvailabilityStore.
func (s *PostgresStore) SaveSettings(ctx context.Context, st Settings) error {
	hours, err := json.Marshal(st.WorkingHours)
	if err != nil {
		return err
	}
	exceptions, err := json.Marshal(st.Exceptions)
	if err != nil {
		return err
	}
	updated := time.Now().UTC()
	if st.UpdatedAt != nil {
		updated = *st.UpdatedAt
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO appointment_settings (user_id, enabled, timezone, working_hours, exceptions, slot_minutes, buffer_minutes,
		                                  lead_time_hours, horizon_days, max_per_day, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (user_id) DO UPDATE SET enabled = EXCLUDED.enabled, timezone = EXCLUDED.timezone,
			working_hours = EXCLUDED.working_hours, exceptions = EXCLUDED.exceptions, slot_minutes = EXCLUDED.slot_minutes,
			buffer_minutes = EXCLUDED.buffer_minutes, lead_time_hours = EXCLUDED.lead_time_hours,
			horizon_days = EXCLUDED.horizon_days, max_per_day = EXCLUDED.max_per_day, updated_at = EXCLUDED.updated_at`,
		st.UserID, st.Enabled, st.Timezone, string(hours), string(exceptions), st.SlotMinutes, st.BufferMinutes,
		st.LeadTimeHours, st.HorizonDays, st.MaxPerDay, updated)
	return err
}

const serviceColumns = `id, user_id, name, duration_minutes, channel, COALESCE(location, ''), COALESCE(notes, ''), active, created_at, updated_at`

func scanService(row pgx.Row) (ConsultationService, error) {
	var c ConsultationService
	err := row.Scan(&c.ID, &c.UserID, &c.Name, &c.DurationMinutes, &c.Channel, &c.Location, &c.Notes, &c.Active, &c.CreatedAt, &c.UpdatedAt)
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, err
}

// Services implements AvailabilityStore.
func (s *PostgresStore) Services(ctx context.Context, userID uuid.UUID) ([]ConsultationService, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+serviceColumns+` FROM consultation_services WHERE user_id = $1
		ORDER BY active DESC, created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ConsultationService, error) { return scanService(row) })
	if out == nil {
		out = []ConsultationService{}
	}
	return out, err
}

// Service implements AvailabilityStore.
func (s *PostgresStore) Service(ctx context.Context, id uuid.UUID) (ConsultationService, error) {
	c, err := scanService(s.pool.QueryRow(ctx, `SELECT `+serviceColumns+` FROM consultation_services WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsultationService{}, ErrServiceNotFound
	}
	return c, err
}

// SaveService implements AvailabilityStore. An insert counts the active
// services under a per-user advisory lock.
func (s *PostgresStore) SaveService(ctx context.Context, c ConsultationService, insert bool) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if insert {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text, 0))`, "consultation_services:"+c.UserID.String()); err != nil {
			return err
		}
		if c.Active {
			var active int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM consultation_services WHERE user_id = $1 AND active`, c.UserID).Scan(&active); err != nil {
				return err
			}
			if active >= MaxActiveServices {
				return ErrTooManyServices
			}
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO consultation_services (id, user_id, name, duration_minutes, channel, location, notes, active, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			c.ID, c.UserID, c.Name, c.DurationMinutes, c.Channel, null(c.Location), null(c.Notes), c.Active, c.CreatedAt, c.UpdatedAt); err != nil {
			return err
		}
	} else {
		tag, err := tx.Exec(ctx, `
			UPDATE consultation_services SET name = $2, duration_minutes = $3, channel = $4, location = $5, notes = $6,
				active = $7, updated_at = $8
			WHERE id = $1`,
			c.ID, c.Name, c.DurationMinutes, c.Channel, null(c.Location), null(c.Notes), c.Active, c.UpdatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrServiceNotFound
		}
	}
	return tx.Commit(ctx)
}

// BusyFor implements AvailabilityStore.
func (s *PostgresStore) BusyFor(ctx context.Context, userID uuid.UUID, from, to time.Time, except uuid.UUID) ([]Busy, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+columns+` FROM appointments a
		WHERE a.recipient_user_id = $1 AND a.id <> $4
		  AND a.status IN ('REQUESTED', 'CONFIRMED', 'RESCHEDULE_REQUESTED')
		  AND ((a.start_at < $3 AND a.end_at > $2)
		    OR (a.proposed_start_at IS NOT NULL AND a.proposed_start_at < $3
		        AND a.proposed_start_at + make_interval(mins => a.duration_minutes) > $2))
		ORDER BY a.start_at`, userID, from, to, except)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Appointment, error) { return scan(row) })
	if err != nil {
		return nil, err
	}
	out := []Busy{}
	for _, a := range list {
		out = appendBusy(out, a, from, to)
	}
	return out, nil
}

// SendReminders implements ReminderStore: the rows are claimed FOR UPDATE
// SKIP LOCKED, so two workers never remind the same appointment twice.
func (s *PostgresStore) SendReminders(ctx context.Context, now time.Time, ahead time.Duration, limit int) (int, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT `+columns+` FROM appointments a
		WHERE a.status = 'CONFIRMED' AND a.start_at > $1 AND a.start_at <= $2
		  AND a.reminded_for_start IS DISTINCT FROM a.start_at
		ORDER BY a.start_at LIMIT $3 FOR UPDATE SKIP LOCKED`, now, now.Add(ahead), limit)
	if err != nil {
		return 0, err
	}
	due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Appointment, error) { return scan(row) })
	if err != nil {
		return 0, err
	}
	for _, a := range due {
		if _, err := tx.Exec(ctx, `UPDATE appointments SET reminded_for_start = start_at WHERE id = $1`, a.ID); err != nil {
			return 0, err
		}
		start := a.StartUTC
		a.RemindedForStart = &start
		if err := insertEvent(ctx, tx, EventReminder, a, reminderEntry(a, now)); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(due), nil
}
