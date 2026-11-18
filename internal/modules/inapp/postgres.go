package inapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresStore is the Store on the tables of migration 0034.
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore builds the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

const deviceColumns = `id, user_id, session_id, token, platform, app_version, COALESCE(locale, ''), push_consent, created_at, last_seen_at`

func scanDevice(row pgx.Row) (Device, error) {
	var d Device
	err := row.Scan(&d.ID, &d.UserID, &d.SessionID, &d.Token, &d.Platform, &d.AppVersion, &d.Locale, &d.PushConsent, &d.CreatedAt, &d.LastSeenAt)
	return d, err
}

// retireDevicesSQL dead-letters the open push deliveries of the devices
// selected by the WHERE clause that follows it.
const retireDevicesSQL = `
	UPDATE notification_deliveries SET status = 'DEAD', last_error = 'device removed', next_attempt_at = NULL, updated_at = now()
	WHERE channel = 'PUSH' AND status IN ('PENDING', 'ATTEMPTED', 'FAILED') AND device_id IN (SELECT id FROM user_devices WHERE `

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// UpsertDevice implements Store.
func (s *PostgresStore) UpsertDevice(ctx context.Context, d Device) (Device, bool, error) {
	var out Device
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// A token moving to another user takes nothing of the previous
		// user's queue with it.
		if _, err := tx.Exec(ctx, retireDevicesSQL+`token = $1 AND user_id <> $2)`, d.Token, d.UserID); err != nil {
			return fmt.Errorf("retire reassigned device: %w", err)
		}
		row := tx.QueryRow(ctx, `
			INSERT INTO user_devices (id, user_id, session_id, token, platform, app_version, locale, push_consent, created_at, updated_at, last_seen_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $9)
			ON CONFLICT (token) DO UPDATE SET
				user_id = EXCLUDED.user_id, session_id = EXCLUDED.session_id, platform = EXCLUDED.platform,
				app_version = EXCLUDED.app_version, locale = EXCLUDED.locale, push_consent = EXCLUDED.push_consent,
				created_at = CASE WHEN user_devices.user_id = EXCLUDED.user_id THEN user_devices.created_at ELSE EXCLUDED.created_at END,
				updated_at = EXCLUDED.updated_at, last_seen_at = EXCLUDED.last_seen_at
			RETURNING `+deviceColumns+`, (xmax = 0)`,
			uuid.New(), d.UserID, d.SessionID, d.Token, d.Platform, d.AppVersion, nullable(d.Locale), d.PushConsent, d.LastSeenAt)
		return row.Scan(&out.ID, &out.UserID, &out.SessionID, &out.Token, &out.Platform, &out.AppVersion, &out.Locale, &out.PushConsent,
			&out.CreatedAt, &out.LastSeenAt, &created)
	})
	if err != nil {
		return Device{}, false, fmt.Errorf("upsert device: %w", err)
	}
	return out, created, nil
}

// Devices implements Store.
func (s *PostgresStore) Devices(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+deviceColumns+` FROM user_devices WHERE user_id = $1 ORDER BY created_at DESC, id`, userID)
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Device, error) { return scanDevice(row) })
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	if out == nil {
		out = []Device{}
	}
	return out, nil
}

// deleteDevices retires and deletes the devices matched by where.
func (s *PostgresStore) deleteDevices(ctx context.Context, where string, args ...any) (int, error) {
	var n int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, retireDevicesSQL+where+`)`, args...); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM user_devices WHERE `+where, args...)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("delete devices: %w", err)
	}
	return int(n), nil
}

// DeleteDevice implements Store.
func (s *PostgresStore) DeleteDevice(ctx context.Context, userID, deviceID uuid.UUID) error {
	n, err := s.deleteDevices(ctx, `id = $1 AND user_id = $2`, deviceID, userID)
	if err == nil && n == 0 {
		return ErrDeviceNotFound
	}
	return err
}

// DeleteDeviceByToken implements Store.
func (s *PostgresStore) DeleteDeviceByToken(ctx context.Context, userID *uuid.UUID, token string) error {
	var n int
	var err error
	if userID == nil {
		n, err = s.deleteDevices(ctx, `token = $1`, token)
	} else {
		n, err = s.deleteDevices(ctx, `token = $1 AND user_id = $2`, token, *userID)
	}
	if err == nil && n == 0 {
		return ErrDeviceNotFound
	}
	return err
}

// DeleteSessionDevices implements Store.
func (s *PostgresStore) DeleteSessionDevices(ctx context.Context, userID, sessionID uuid.UUID) (int, error) {
	return s.deleteDevices(ctx, `user_id = $1 AND session_id = $2`, userID, sessionID)
}

const notificationColumns = `id, user_id, source_event_id, type, title, body, link, COALESCE(reference, ''), created_at, read_at`

func scanNotification(row pgx.Row) (Notification, error) {
	var n Notification
	err := row.Scan(&n.ID, &n.UserID, &n.SourceEventID, &n.Type, &n.Title, &n.Body, &n.Link, &n.Reference, &n.CreatedAt, &n.ReadAt)
	return n, err
}

// CreateNotification implements Store.
func (s *PostgresStore) CreateNotification(ctx context.Context, d Draft, push bool, now time.Time) (Notification, bool, error) {
	var n Notification
	created := false
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		n, err = scanNotification(tx.QueryRow(ctx, `
			INSERT INTO notifications (id, user_id, source_event_id, type, title, body, link, reference, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			ON CONFLICT (source_event_id, user_id) DO NOTHING
			RETURNING `+notificationColumns,
			uuid.New(), d.UserID, d.SourceEventID, d.Type, d.Title, d.Body, d.Link, nullable(d.Reference), now))
		if errors.Is(err, pgx.ErrNoRows) {
			n, err = scanNotification(tx.QueryRow(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE source_event_id = $1 AND user_id = $2`,
				d.SourceEventID, d.UserID))
			return err
		}
		if err != nil {
			return err
		}
		created = true
		if !push {
			return nil
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO notification_deliveries (id, notification_id, user_id, channel, device_id, target, status, next_attempt_at, created_at, updated_at)
			SELECT gen_random_uuid(), $1, $2, 'PUSH', d.id, d.id::text, 'PENDING', $3, $3, $3
			FROM user_devices d WHERE d.user_id = $2 AND d.push_consent
			ON CONFLICT (notification_id, channel, target) DO NOTHING`, n.ID, d.UserID, now)
		return err
	})
	if err != nil {
		return Notification{}, false, fmt.Errorf("create notification: %w", err)
	}
	return n, created, nil
}

// NotificationBySource implements Store.
func (s *PostgresStore) NotificationBySource(ctx context.Context, sourceEventID, userID uuid.UUID) (Notification, bool, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `SELECT `+notificationColumns+` FROM notifications WHERE source_event_id = $1 AND user_id = $2`,
		sourceEventID, userID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, false, nil
	}
	if err != nil {
		return Notification{}, false, fmt.Errorf("notification by source: %w", err)
	}
	return n, true, nil
}

// List implements Store.
func (s *PostgresStore) List(ctx context.Context, userID uuid.UUID, unreadOnly bool, offset, limit int) ([]Notification, int, error) {
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND (NOT $2 OR read_at IS NULL)`,
		userID, unreadOnly).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count notifications: %w", err)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+notificationColumns+` FROM notifications
		WHERE user_id = $1 AND (NOT $2 OR read_at IS NULL) ORDER BY created_at DESC, id DESC OFFSET $3 LIMIT $4`,
		userID, unreadOnly, offset, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Notification, error) { return scanNotification(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("list notifications: %w", err)
	}
	if out == nil {
		out = []Notification{}
	}
	return out, total, nil
}

// UnreadCount implements Store.
func (s *PostgresStore) UnreadCount(ctx context.Context, userID uuid.UUID) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(*) FROM notifications WHERE user_id = $1 AND read_at IS NULL`, userID).Scan(&n)
	return n, err
}

// MarkRead implements Store.
func (s *PostgresStore) MarkRead(ctx context.Context, userID, id uuid.UUID, now time.Time) (Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, `UPDATE notifications SET read_at = COALESCE(read_at, $3)
		WHERE id = $1 AND user_id = $2 RETURNING `+notificationColumns, id, userID, now))
	if errors.Is(err, pgx.ErrNoRows) {
		return Notification{}, ErrNotFound
	}
	if err != nil {
		return Notification{}, fmt.Errorf("mark read: %w", err)
	}
	return n, nil
}

// MarkAllRead implements Store.
func (s *PostgresStore) MarkAllRead(ctx context.Context, userID uuid.UUID, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE notifications SET read_at = $2 WHERE user_id = $1 AND read_at IS NULL`, userID, now)
	if err != nil {
		return 0, fmt.Errorf("mark all read: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// RecordEmail implements Store.
func (s *PostgresStore) RecordEmail(ctx context.Context, notificationID uuid.UUID, status string, attempts int, lastError string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO notification_deliveries (id, notification_id, user_id, channel, target, status, attempts, last_error, last_attempt_at, created_at, updated_at)
		SELECT $1, n.id, n.user_id, 'EMAIL', 'email', $3, $4, $5, $6, $6, $6 FROM notifications n WHERE n.id = $2
		ON CONFLICT (notification_id, channel, target) DO UPDATE SET
			status = EXCLUDED.status, attempts = EXCLUDED.attempts, last_error = EXCLUDED.last_error,
			last_attempt_at = EXCLUDED.last_attempt_at, updated_at = EXCLUDED.updated_at`,
		uuid.New(), notificationID, status, attempts, nullable(lastError), now)
	if err != nil {
		return fmt.Errorf("record email delivery: %w", err)
	}
	return nil
}

// ClaimPush implements Store.
func (s *PostgresStore) ClaimPush(ctx context.Context, now time.Time, limit int, leaseFor time.Duration) ([]PushJob, error) {
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM notification_deliveries
			WHERE channel = 'PUSH' AND device_id IS NOT NULL AND status IN ('PENDING', 'FAILED', 'ATTEMPTED') AND next_attempt_at <= $1
			ORDER BY next_attempt_at, created_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notification_deliveries x
		SET status = 'ATTEMPTED', attempts = x.attempts + 1, next_attempt_at = $1 + ($3::int * interval '1 second'), updated_at = $1
		FROM due, notifications n, user_devices d
		WHERE x.id = due.id AND n.id = x.notification_id AND d.id = x.device_id
		RETURNING x.id, x.attempts, n.id, n.user_id, n.source_event_id, n.type, n.title, n.body, n.link, COALESCE(n.reference, ''),
		          n.created_at, n.read_at, d.id, d.token`, now, limit, int(leaseFor.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("claim push: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (PushJob, error) {
		var j PushJob
		n := &j.Notification
		err := row.Scan(&j.DeliveryID, &j.Attempts, &n.ID, &n.UserID, &n.SourceEventID, &n.Type, &n.Title, &n.Body, &n.Link, &n.Reference,
			&n.CreatedAt, &n.ReadAt, &j.DeviceID, &j.Token)
		return j, err
	})
}

// DeferPush implements Store.
func (s *PostgresStore) DeferPush(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE notification_deliveries SET status = 'PENDING', attempts = GREATEST(attempts - 1, 0),
		next_attempt_at = $2, updated_at = now() WHERE id = ANY($1) AND status = 'ATTEMPTED'`, ids, at)
	if err != nil {
		return fmt.Errorf("defer push: %w", err)
	}
	return nil
}

// RecordPush implements Store.
func (s *PostgresStore) RecordPush(ctx context.Context, o PushOutcome) error {
	_, err := s.pool.Exec(ctx, `UPDATE notification_deliveries SET status = $2, provider_message_id = $3, last_error = $4,
		next_attempt_at = $5, last_attempt_at = $6, batch_id = $7, summary = $8, updated_at = $6 WHERE id = ANY($1)`,
		o.DeliveryIDs, o.Status, nullable(o.MessageID), nullable(clip(o.LastError, 500)), o.NextAttemptAt, o.At, o.BatchID, o.Summary)
	if err != nil {
		return fmt.Errorf("record push: %w", err)
	}
	return nil
}

// PushBatchesSince implements Store.
func (s *PostgresStore) PushBatchesSince(ctx context.Context, deviceID uuid.UUID, since time.Time) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT count(DISTINCT batch_id) FROM notification_deliveries
		WHERE device_id = $1 AND batch_id IS NOT NULL AND last_attempt_at > $2`, deviceID, since).Scan(&n)
	return n, err
}

// Preferences implements Store.
func (s *PostgresStore) Preferences(ctx context.Context, userID uuid.UUID) (map[string]Preference, error) {
	rows, err := s.pool.Query(ctx, `SELECT type, email, push, changed_at, source FROM notification_preferences WHERE user_id = $1`, userID)
	if err != nil {
		return nil, fmt.Errorf("load preferences: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Preference, error) {
		var p Preference
		var at time.Time
		err := row.Scan(&p.Type, &p.Email, &p.Push, &at, &p.Source)
		p.ChangedAt = &at
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("load preferences: %w", err)
	}
	out := make(map[string]Preference, len(list))
	for _, p := range list {
		out[p.Type] = p
	}
	return out, nil
}

// SetPreferences implements Store.
func (s *PostgresStore) SetPreferences(ctx context.Context, userID uuid.UUID, changes []PreferenceChange, source string, now time.Time) error {
	if len(changes) == 0 {
		return nil
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		for _, c := range changes {
			cur := Preference{Email: true, Push: true}
			err := tx.QueryRow(ctx, `SELECT email, push FROM notification_preferences WHERE user_id = $1 AND type = $2 FOR UPDATE`,
				userID, c.Type).Scan(&cur.Email, &cur.Push)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			next := cur
			if c.Email != nil {
				next.Email = *c.Email
			}
			if c.Push != nil {
				next.Push = *c.Push
			}
			if next.Email == cur.Email && next.Push == cur.Push {
				continue // nothing changes (the default unchanged included)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO notification_preferences (user_id, type, email, push, changed_at, source) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (user_id, type) DO UPDATE SET email = EXCLUDED.email, push = EXCLUDED.push,
					changed_at = EXCLUDED.changed_at, source = EXCLUDED.source`,
				userID, c.Type, next.Email, next.Push, now, source); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO notification_preference_changes (id, user_id, type, email, push, changed_at, source)
				VALUES ($1, $2, $3, $4, $5, $6, $7)`, uuid.New(), userID, c.Type, next.Email, next.Push, now, source); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("set preferences: %w", err)
	}
	return nil
}

// Health implements Store.
func (s *PostgresStore) Health(ctx context.Context, now time.Time) (Health, error) {
	h := Health{Counts: emptyCounts()}
	rows, err := s.pool.Query(ctx, `SELECT channel, status, count(*) FROM notification_deliveries GROUP BY channel, status`)
	if err != nil {
		return Health{}, fmt.Errorf("delivery counts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var ch, st string
		var n int
		if err := rows.Scan(&ch, &st, &n); err != nil {
			return Health{}, err
		}
		if h.Counts[ch] != nil {
			h.Counts[ch][st] = n
		}
		if st == StatusDead {
			h.DeadLetterBacklog += n
		}
	}
	if err := rows.Err(); err != nil {
		return Health{}, err
	}
	err = s.pool.QueryRow(ctx, `
		SELECT (SELECT min(created_at) FROM notification_deliveries WHERE channel = 'PUSH' AND status IN ('PENDING', 'FAILED')),
		       (SELECT count(*) FROM notifications WHERE created_at > $1),
		       (SELECT count(*) FROM notifications WHERE read_at IS NULL)`, now.Add(-24*time.Hour)).
		Scan(&h.OldestUnattemptedAt, &h.NotificationsLast24h, &h.UnreadTotal)
	if err != nil {
		return Health{}, fmt.Errorf("notification health: %w", err)
	}
	return h, nil
}

// DeleteUserTx removes a user's devices, notifications (their deliveries
// cascade) and preferences inside the account deletion transaction: the
// users row is anonymised, not deleted, so no ON DELETE CASCADE fires.
func DeleteUserTx(ctx context.Context, tx pgx.Tx, userID uuid.UUID) error {
	for _, q := range []string{
		`DELETE FROM user_devices WHERE user_id = $1`,
		`DELETE FROM notifications WHERE user_id = $1`,
		`DELETE FROM notification_preferences WHERE user_id = $1`,
		`DELETE FROM notification_preference_changes WHERE user_id = $1`,
	} {
		if _, err := tx.Exec(ctx, q, userID); err != nil {
			return fmt.Errorf("delete notification data: %w", err)
		}
	}
	return nil
}
