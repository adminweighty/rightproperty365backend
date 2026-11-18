package crm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// PostgresStore is the production Store (migration 0030).
type PostgresStore struct{ pool *pgxpool.Pool }

// NewPostgresStore returns the Postgres store.
func NewPostgresStore(pool *pgxpool.Pool) *PostgresStore { return &PostgresStore{pool: pool} }

func (s *PostgresStore) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

const contactColumns = `c.id, c.organisation_id, c.book_user_id, c.name, c.type, c.stage, c.owner_user_id, COALESCE(c.source, ''),
	c.marketing, c.last_contact_at, c.created_by, c.created_at, c.updated_at, c.deleted_at, c.merged_into,
	COALESCE((SELECT array_agg(e.email ORDER BY e.position) FROM contact_emails e WHERE e.contact_id = c.id), '{}'),
	COALESCE((SELECT array_agg(p.phone ORDER BY p.position) FROM contact_phones p WHERE p.contact_id = c.id), '{}'),
	COALESCE((SELECT array_agg(t.tag ORDER BY t.position) FROM contact_tags t WHERE t.contact_id = c.id), '{}'),
	(SELECT count(*) FROM contact_reminders r WHERE r.contact_id = c.id AND NOT r.done),
	(SELECT min(r.due_at) FROM contact_reminders r WHERE r.contact_id = c.id AND NOT r.done),
	COALESCE((SELECT array_agg(l.item_id ORDER BY l.linked_at, l.item_id) FROM contact_links l WHERE l.contact_id = c.id AND l.kind = 'LISTING'), '{}')`

func scanContact(row pgx.Row, extra ...any) (Contact, error) {
	var c Contact
	var marketing []byte
	dest := []any{&c.ID, &c.Book.OrganisationID, &c.Book.UserID, &c.Name, &c.Type, &c.Stage, &c.OwnerUserID, &c.Source,
		&marketing, &c.LastContactAt, &c.CreatedBy, &c.CreatedAt, &c.UpdatedAt, &c.DeletedAt, &c.MergedInto,
		&c.Emails, &c.Phones, &c.Tags, &c.OpenReminders, &c.NextReminderAt, &c.ListingIDs}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return Contact{}, err
	}
	if len(marketing) > 0 {
		var m Marketing
		if err := json.Unmarshal(marketing, &m); err != nil {
			return Contact{}, fmt.Errorf("decode marketing permission: %w", err)
		}
		m.GrantedAt = m.GrantedAt.UTC()
		c.Marketing = &m
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	for _, t := range []**time.Time{&c.LastContactAt, &c.DeletedAt, &c.NextReminderAt} {
		if *t != nil {
			u := (**t).UTC()
			*t = &u
		}
	}
	return c, nil
}

// Get implements Store.
func (s *PostgresStore) Get(ctx context.Context, id uuid.UUID) (Contact, error) {
	c, err := scanContact(s.pool.QueryRow(ctx, `SELECT `+contactColumns+` FROM contacts c WHERE c.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	if err != nil {
		return Contact{}, fmt.Errorf("get contact: %w", err)
	}
	return c, nil
}

// bookColumn is the column a book is selected by, and its ID.
func bookColumn(b Book) (string, uuid.UUID) {
	if b.OrganisationID != nil {
		return "organisation_id", *b.OrganisationID
	}
	if b.UserID != nil {
		return "book_user_id", *b.UserID
	}
	return "organisation_id", uuid.Nil
}

// listWhere builds the WHERE clause of a filter; its arguments are added to args.
func listWhere(book Book, f ListFilter, args *[]any) []string {
	col, id := bookColumn(book)
	*args = append(*args, id)
	arg := func(v any) string {
		*args = append(*args, v)
		return fmt.Sprintf("$%d", len(*args))
	}
	where := []string{"c." + col + " = $1", "c.deleted_at IS NULL", "c.merged_into IS NULL"}
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		pq := arg(q)
		cond := `strpos(lower(c.name), ` + pq + `) > 0 OR EXISTS (SELECT 1 FROM contact_emails e WHERE e.contact_id = c.id AND strpos(e.email, ` + pq + `) > 0)`
		if d := strings.TrimPrefix(digits(q), "0"); len(d) >= 3 {
			cond += ` OR EXISTS (SELECT 1 FROM contact_phones p WHERE p.contact_id = c.id AND strpos(p.phone, ` + arg(d) + `) > 0)`
		}
		where = append(where, "("+cond+")")
	}
	if f.Type != "" {
		where = append(where, "c.type = "+arg(f.Type))
	}
	if f.Stage != "" {
		where = append(where, "c.stage = "+arg(f.Stage))
	}
	if f.Tag != "" {
		where = append(where, "EXISTS (SELECT 1 FROM contact_tags t WHERE t.contact_id = c.id AND t.tag = "+arg(normaliseTag(f.Tag))+")")
	}
	if f.Owner != nil {
		where = append(where, "c.owner_user_id = "+arg(*f.Owner))
	}
	if f.Unowned {
		where = append(where, "c.owner_user_id IS NULL")
	}
	if f.ReminderDueBefore != nil {
		where = append(where, "EXISTS (SELECT 1 FROM contact_reminders r WHERE r.contact_id = c.id AND NOT r.done AND r.due_at < "+arg(*f.ReminderDueBefore)+")")
	}
	return where
}

// List implements Store. The filters and the order are the memory store's.
func (s *PostgresStore) List(ctx context.Context, book Book, f ListFilter) ([]Contact, int, error) {
	var args []any
	where := listWhere(book, f, &args)
	whereArgs := len(args)
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}
	now := f.Now
	if now.IsZero() {
		now = time.Unix(0, 0)
	}
	due := arg(now)
	order := "c.last_contact_at DESC NULLS LAST, c.created_at DESC, c.id DESC"
	switch f.Sort {
	case SortName:
		order = "lower(c.name), c.id DESC"
	case SortCreated:
		order = "c.created_at DESC, c.id DESC"
	}
	limit := "ALL"
	if f.Limit > 0 {
		limit = arg(f.Limit)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+contactColumns+`, count(*) OVER (),
		(SELECT count(*) FROM contact_reminders r WHERE r.contact_id = c.id AND NOT r.done AND r.due_at <= `+due+`)
		FROM contacts c WHERE `+strings.Join(where, " AND ")+
		` ORDER BY `+order+` LIMIT `+limit+` OFFSET `+arg(max(f.Offset, 0)), args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list contacts: %w", err)
	}
	defer rows.Close()
	var out []Contact
	total := 0
	for rows.Next() {
		var dueCount int
		c, err := scanContact(rows, &total, &dueCount)
		c.DueReminders = dueCount
		if err != nil {
			return nil, 0, fmt.Errorf("list contacts: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("list contacts: %w", err)
	}
	if len(out) == 0 && f.Offset > 0 {
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM contacts c WHERE `+strings.Join(where, " AND "), args[:whereArgs]...).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count contacts: %w", err)
		}
	}
	return out, total, nil
}

// StageCounts implements Store.
func (s *PostgresStore) StageCounts(ctx context.Context, book Book, f ListFilter) (map[string]int, error) {
	f.Stage = ""
	var args []any
	where := listWhere(book, f, &args)
	rows, err := s.pool.Query(ctx, `SELECT c.stage, count(*) FROM contacts c WHERE `+strings.Join(where, " AND ")+` GROUP BY c.stage`, args...)
	if err != nil {
		return nil, fmt.Errorf("count contacts by stage: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for _, st := range Stages() {
		out[st] = 0
	}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// Tags implements Store.
func (s *PostgresStore) Tags(ctx context.Context, book Book) ([]TagCount, error) {
	col, id := bookColumn(book)
	rows, err := s.pool.Query(ctx, `SELECT t.tag, count(*) FROM contact_tags t JOIN contacts c ON c.id = t.contact_id
		WHERE c.`+col+` = $1 AND c.deleted_at IS NULL AND c.merged_into IS NULL GROUP BY t.tag`, id)
	if err != nil {
		return nil, fmt.Errorf("list contact tags: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (TagCount, error) {
		var t TagCount
		err := row.Scan(&t.Tag, &t.Count)
		return t, err
	})
	if err != nil {
		return nil, fmt.Errorf("list contact tags: %w", err)
	}
	SortTags(out)
	return out, nil
}

// Matching implements Store.
func (s *PostgresStore) Matching(ctx context.Context, book Book, emails, phones []string) ([]Contact, error) {
	if len(emails) == 0 && len(phones) == 0 {
		return nil, nil
	}
	col, id := bookColumn(book)
	rows, err := s.pool.Query(ctx, `SELECT `+contactColumns+` FROM contacts c
		WHERE c.`+col+` = $1 AND c.deleted_at IS NULL AND c.merged_into IS NULL
		  AND c.id IN (SELECT contact_id FROM contact_emails WHERE `+col+` = $1 AND email = ANY($2)
		               UNION SELECT contact_id FROM contact_phones WHERE `+col+` = $1 AND phone = ANY($3))
		ORDER BY c.created_at DESC, c.id DESC`, id, nonNil(emails), nonNil(phones))
	if err != nil {
		return nil, fmt.Errorf("match contacts: %w", err)
	}
	defer rows.Close()
	var out []Contact
	for rows.Next() {
		c, err := scanContact(rows)
		if err != nil {
			return nil, fmt.Errorf("match contacts: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func jsonOrNil(v any) (*string, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	s := string(raw)
	return &s, nil
}

// Apply implements Store: one transaction, in the order documented on Change.
func (s *PostgresStore) Apply(ctx context.Context, ch Change) error {
	return s.inTx(ctx, func(tx pgx.Tx) error { return ApplyTx(ctx, tx, ch) })
}

// ApplyTx writes a Change inside the caller's transaction (the store, and
// the development seed).
func ApplyTx(ctx context.Context, tx pgx.Tx, ch Change) error {
	if len(ch.Expect) > 0 {
		ids := make([]uuid.UUID, 0, len(ch.Expect))
		for id := range ch.Expect {
			ids = append(ids, id)
		}
		rows, err := tx.Query(ctx, `SELECT id, updated_at FROM contacts WHERE id = ANY($1) ORDER BY id FOR UPDATE`, ids)
		if err != nil {
			return fmt.Errorf("lock contacts: %w", err)
		}
		seen := 0
		for rows.Next() {
			var id uuid.UUID
			var at time.Time
			if err := rows.Scan(&id, &at); err != nil {
				rows.Close()
				return err
			}
			seen++
			if !at.Equal(ch.Expect[id]) {
				rows.Close()
				return ErrConflict
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if seen != len(ids) {
			return ErrNotFound
		}
	}
	b := &pgx.Batch{}
	for _, c := range ch.Contacts {
		var marketing any
		if c.Marketing != nil {
			marketing = c.Marketing
		}
		m, err := jsonOrNil(marketing)
		if err != nil {
			return err
		}
		var source *string
		if c.Source != "" {
			source = &c.Source
		}
		b.Queue(`
				INSERT INTO contacts (id, organisation_id, book_user_id, name, type, stage, owner_user_id, source, marketing,
					last_contact_at, created_by, created_at, updated_at, deleted_at, merged_into)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13, $14, $15)
				ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, type = EXCLUDED.type, stage = EXCLUDED.stage,
					owner_user_id = EXCLUDED.owner_user_id, source = EXCLUDED.source, marketing = EXCLUDED.marketing,
					last_contact_at = EXCLUDED.last_contact_at, updated_at = EXCLUDED.updated_at,
					deleted_at = EXCLUDED.deleted_at, merged_into = EXCLUDED.merged_into`,
			c.ID, c.Book.OrganisationID, c.Book.UserID, c.Name, c.Type, c.Stage, c.OwnerUserID, source, m,
			c.LastContactAt, c.CreatedBy, c.CreatedAt, c.UpdatedAt, c.DeletedAt, c.MergedInto)
		b.Queue(`DELETE FROM contact_emails WHERE contact_id = $1`, c.ID)
		b.Queue(`DELETE FROM contact_phones WHERE contact_id = $1`, c.ID)
		b.Queue(`DELETE FROM contact_tags WHERE contact_id = $1`, c.ID)
		for i, e := range c.Emails {
			b.Queue(`INSERT INTO contact_emails (contact_id, organisation_id, book_user_id, email, position) VALUES ($1, $2, $3, $4, $5)`,
				c.ID, c.Book.OrganisationID, c.Book.UserID, e, i)
		}
		for i, p := range c.Phones {
			b.Queue(`INSERT INTO contact_phones (contact_id, organisation_id, book_user_id, phone, position) VALUES ($1, $2, $3, $4, $5)`,
				c.ID, c.Book.OrganisationID, c.Book.UserID, p, i)
		}
		for i, t := range c.Tags {
			b.Queue(`INSERT INTO contact_tags (contact_id, tag, position) VALUES ($1, $2, $3)`, c.ID, t, i)
		}
	}
	if m := ch.Merge; m != nil {
		b.Queue(`UPDATE contact_notes SET merged_from = COALESCE(merged_from, contact_id), contact_id = $1 WHERE contact_id = ANY($2)`, m.Keep, m.From)
		b.Queue(`UPDATE contact_reminders SET contact_id = $1 WHERE contact_id = ANY($2)`, m.Keep, m.From)
		b.Queue(`UPDATE contact_events SET contact_id = $1 WHERE contact_id = ANY($2)`, m.Keep, m.From)
		b.Queue(`
				INSERT INTO contact_links (contact_id, kind, item_id, auto, linked_by, linked_at)
				SELECT $1, kind, item_id, bool_and(auto), (array_agg(linked_by ORDER BY linked_at))[1], min(linked_at)
				FROM contact_links WHERE contact_id = ANY($2) GROUP BY kind, item_id
				ON CONFLICT (contact_id, kind, item_id) DO NOTHING`, m.Keep, m.From)
		b.Queue(`DELETE FROM contact_links WHERE contact_id = ANY($1)`, m.From)
	}
	for _, n := range ch.Notes {
		b.Queue(`INSERT INTO contact_notes (id, contact_id, text, author_user_id, author_name, merged_from, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			n.ID, n.ContactID, n.Text, n.AuthorUserID, n.AuthorName, n.MergedFrom, n.CreatedAt)
	}
	for _, r := range ch.Reminders {
		b.Queue(`
				INSERT INTO contact_reminders (id, contact_id, due_at, text, done, done_at, done_by, created_by, created_by_name, created_at, notified_at)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
				ON CONFLICT (id) DO UPDATE SET due_at = EXCLUDED.due_at, text = EXCLUDED.text, done = EXCLUDED.done,
					done_at = EXCLUDED.done_at, done_by = EXCLUDED.done_by, notified_at = EXCLUDED.notified_at`,
			r.ID, r.ContactID, r.DueAt, r.Text, r.Done, r.DoneAt, r.DoneBy, r.CreatedBy, r.CreatedByName, r.CreatedAt, r.NotifiedAt)
	}
	for _, e := range ch.Events {
		data, err := jsonOrNil(mapOrNil(e.Data))
		if err != nil {
			return err
		}
		b.Queue(`INSERT INTO contact_events (id, contact_id, kind, actor_user_id, actor_name, data, created_at) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7)`,
			e.ID, e.ContactID, e.Kind, e.ActorUserID, e.ActorName, data, e.At)
	}
	for _, l := range ch.Links {
		b.Queue(`INSERT INTO contact_links (contact_id, kind, item_id, auto, linked_by, linked_at) VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (contact_id, kind, item_id) DO NOTHING`, l.ContactID, l.Kind, l.ItemID, l.Auto, l.LinkedBy, l.LinkedAt)
	}
	for _, l := range ch.Unlinks {
		b.Queue(`DELETE FROM contact_links WHERE contact_id = $1 AND kind = $2 AND item_id = $3`, l.ContactID, l.Kind, l.ItemID)
	}
	if b.Len() > 0 {
		br := tx.SendBatch(ctx, b)
		for i := 0; i < b.Len(); i++ {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return fmt.Errorf("apply contact change: %w", err)
			}
		}
		if err := br.Close(); err != nil {
			return fmt.Errorf("apply contact change: %w", err)
		}
	}
	for _, a := range ch.Audit {
		if err := orgaudit.Insert(ctx, tx, a); err != nil {
			return err
		}
	}
	return nil
}

func mapOrNil(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// Notes implements Store (oldest first).
func (s *PostgresStore) Notes(ctx context.Context, contactID uuid.UUID) ([]Note, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, contact_id, text, author_user_id, author_name, created_at, merged_from
		FROM contact_notes WHERE contact_id = $1 ORDER BY created_at, id`, contactID)
	if err != nil {
		return nil, fmt.Errorf("list notes: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Note, error) {
		var n Note
		err := row.Scan(&n.ID, &n.ContactID, &n.Text, &n.AuthorUserID, &n.AuthorName, &n.CreatedAt, &n.MergedFrom)
		n.CreatedAt = n.CreatedAt.UTC()
		return n, err
	})
}

const reminderColumns = `r.id, r.contact_id, r.due_at, r.text, r.done, r.done_at, r.done_by, r.created_by, r.created_by_name, r.created_at, r.notified_at`

func scanReminder(row pgx.Row, extra ...any) (Reminder, error) {
	var r Reminder
	err := row.Scan(append([]any{&r.ID, &r.ContactID, &r.DueAt, &r.Text, &r.Done, &r.DoneAt, &r.DoneBy, &r.CreatedBy, &r.CreatedByName,
		&r.CreatedAt, &r.NotifiedAt}, extra...)...)
	r.DueAt, r.CreatedAt = r.DueAt.UTC(), r.CreatedAt.UTC()
	for _, t := range []**time.Time{&r.DoneAt, &r.NotifiedAt} {
		if *t != nil {
			u := (**t).UTC()
			*t = &u
		}
	}
	return r, err
}

// Reminders implements Store (soonest first).
func (s *PostgresStore) Reminders(ctx context.Context, contactID uuid.UUID) ([]Reminder, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reminderColumns+` FROM contact_reminders r WHERE r.contact_id = $1 ORDER BY r.due_at, r.id`, contactID)
	if err != nil {
		return nil, fmt.Errorf("list reminders: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Reminder, error) { return scanReminder(row) })
}

// Events implements Store (oldest first).
func (s *PostgresStore) Events(ctx context.Context, contactID uuid.UUID) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, contact_id, kind, actor_user_id, actor_name, data, created_at
		FROM contact_events WHERE contact_id = $1 ORDER BY created_at, id`, contactID)
	if err != nil {
		return nil, fmt.Errorf("list contact events: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var data []byte
		if err := row.Scan(&e.ID, &e.ContactID, &e.Kind, &e.ActorUserID, &e.ActorName, &data, &e.At); err != nil {
			return Event{}, err
		}
		if len(data) > 0 {
			if err := json.Unmarshal(data, &e.Data); err != nil {
				return Event{}, err
			}
		}
		e.At = e.At.UTC()
		return e, nil
	})
}

// Links implements Store (oldest first).
func (s *PostgresStore) Links(ctx context.Context, contactID uuid.UUID) ([]Link, error) {
	rows, err := s.pool.Query(ctx, `SELECT contact_id, kind, item_id, auto, linked_by, linked_at
		FROM contact_links WHERE contact_id = $1 ORDER BY linked_at, kind, item_id`, contactID)
	if err != nil {
		return nil, fmt.Errorf("list contact links: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Link, error) {
		var l Link
		err := row.Scan(&l.ContactID, &l.Kind, &l.ItemID, &l.Auto, &l.LinkedBy, &l.LinkedAt)
		l.LinkedAt = l.LinkedAt.UTC()
		return l, err
	})
}

// DueReminders implements Store.
func (s *PostgresStore) DueReminders(ctx context.Context, book Book, before time.Time) ([]DueReminder, error) {
	col, id := bookColumn(book)
	rows, err := s.pool.Query(ctx, `SELECT `+reminderColumns+`, c.name, c.owner_user_id
		FROM contact_reminders r JOIN contacts c ON c.id = r.contact_id
		WHERE c.`+col+` = $1 AND c.deleted_at IS NULL AND c.merged_into IS NULL AND NOT r.done AND r.due_at < $2
		ORDER BY r.due_at, r.id`, id, before)
	if err != nil {
		return nil, fmt.Errorf("list due reminders: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (DueReminder, error) {
		var d DueReminder
		r, err := scanReminder(row, &d.ContactName, &d.OwnerUserID)
		d.Reminder = r
		return d, err
	})
}

// ClaimDueReminders implements Store: the reminders are marked and their
// outbox events written in one transaction (SKIP LOCKED: safe with several workers).
func (s *PostgresStore) ClaimDueReminders(ctx context.Context, now time.Time, limit int) ([]Reminder, error) {
	var out []Reminder
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH due AS (
				SELECT r.id FROM contact_reminders r JOIN contacts c ON c.id = r.contact_id
				WHERE NOT r.done AND r.notified_at IS NULL AND r.due_at <= $1 AND c.deleted_at IS NULL AND c.merged_into IS NULL
				ORDER BY r.due_at LIMIT $2 FOR UPDATE OF r SKIP LOCKED
			)
			UPDATE contact_reminders r SET notified_at = $1 FROM due, contacts c
			WHERE r.id = due.id AND c.id = r.contact_id
			RETURNING `+reminderColumns+`, c.organisation_id, c.book_user_id, c.owner_user_id`, now, limit)
		if err != nil {
			return fmt.Errorf("claim due reminders: %w", err)
		}
		type claimed struct {
			r Reminder
			c Contact
		}
		var got []claimed
		for rows.Next() {
			var c Contact
			r, err := scanReminder(rows, &c.Book.OrganisationID, &c.Book.UserID, &c.OwnerUserID)
			if err != nil {
				rows.Close()
				return err
			}
			c.ID = r.ContactID
			got = append(got, claimed{r, c})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, g := range got {
			payload, err := json.Marshal(reminderPayload(g.r, g.c))
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO outbox_events (id, event_type, aggregate_type, aggregate_id, payload, created_at, available_at)
				VALUES ($1, $2, $3, $4, $5::jsonb, $6, $6)`, uuid.New(), EventReminderDue, AggregateContact, g.c.ID, string(payload), now); err != nil {
				return fmt.Errorf("insert outbox event %s: %w", EventReminderDue, err)
			}
			out = append(out, g.r)
		}
		return nil
	})
	return out, err
}

// ---------------------------------------------------------------------------
// Items: property requests, business enquiries and appointments
// ---------------------------------------------------------------------------

// PostgresItems reads the items the CRM links, from their own tables.
type PostgresItems struct{ pool *pgxpool.Pool }

// NewPostgresItems returns the Postgres Items.
func NewPostgresItems(pool *pgxpool.Pool) *PostgresItems { return &PostgresItems{pool: pool} }

// Each query returns the same columns, in this order:
// id, reference, type, status, title, listing_id, organisation_id,
// assigned_user_id, direct_owner_user_id, market code, name, email, phone,
// verified account email, created_at.
const (
	requestItems = `
		SELECT r.id, r.reference, r.request_type, r.stage, COALESCE(l.title, ''), r.listing_id, r.recipient_organisation_id,
		       CASE WHEN r.recipient_organisation_id IS NOT NULL THEN r.recipient_user_id END,
		       CASE WHEN r.recipient_type = 'SELLER' AND r.recipient_organisation_id IS NULL THEN r.recipient_user_id END,
		       m.code, COALESCE(r.guest_name, u.full_name, ''), COALESCE(r.guest_email, u.email, ''), COALESCE(r.guest_phone, ''),
		       CASE WHEN u.email_verified_at IS NOT NULL THEN u.email ELSE '' END, r.created_at
		FROM property_requests r
		JOIN markets m ON m.id = r.market_id
		LEFT JOIN listings l ON l.id = r.listing_id
		LEFT JOIN users u ON u.id = r.customer_user_id`
	enquiryItems = `
		SELECT e.id, e.reference, COALESCE(e.service, ''), e.status, p.name, NULL::uuid, e.recipient_organisation_id,
		       e.recipient_user_id, NULL::uuid,
		       m.code, COALESCE(e.guest_name, u.full_name, ''), COALESCE(e.guest_email, u.email, ''), COALESCE(e.guest_phone, ''),
		       CASE WHEN u.email_verified_at IS NOT NULL THEN u.email ELSE '' END, e.created_at
		FROM business_enquiries e
		JOIN markets m ON m.id = e.market_id
		JOIN directory_profiles p ON p.id = e.profile_id
		LEFT JOIN users u ON u.id = e.customer_user_id`
	appointmentItems = `
		SELECT a.id, a.reference, a.type, a.status, COALESCE(NULLIF(a.listing_title, ''), a.with_name, ''), a.listing_id,
		       a.recipient_organisation_id,
		       CASE WHEN a.recipient_organisation_id IS NOT NULL THEN a.recipient_user_id END,
		       CASE WHEN a.recipient_type = 'SELLER' AND a.recipient_organisation_id IS NULL THEN a.recipient_user_id END,
		       m.code, a.customer_name, a.customer_email, COALESCE(a.customer_phone, ''),
		       CASE WHEN u.email_verified_at IS NOT NULL THEN u.email ELSE '' END, a.created_at
		FROM appointments a
		JOIN markets m ON m.id = a.market_id
		LEFT JOIN users u ON u.id = a.customer_user_id`
)

func itemQuery(kind string) (query, alias string, ok bool) {
	switch kind {
	case KindPropertyRequest:
		return requestItems, "r", true
	case KindBusinessEnquiry:
		return enquiryItems, "e", true
	case KindAppointment:
		return appointmentItems, "a", true
	}
	return "", "", false
}

func scanItem(kind string, row pgx.Row) (Item, error) {
	it := Item{Kind: kind}
	err := row.Scan(&it.ID, &it.Reference, &it.Type, &it.Status, &it.Title, &it.ListingID, &it.OrganisationID, &it.AssignedUserID,
		&it.DirectOwnerUserID, &it.MarketCode, &it.Name, &it.Email, &it.Phone, &it.AccountEmail, &it.CreatedAt)
	it.CreatedAt = it.CreatedAt.UTC()
	return it, err
}

// Item implements Items.
func (s *PostgresItems) Item(ctx context.Context, kind string, id uuid.UUID) (Item, error) {
	q, alias, ok := itemQuery(kind)
	if !ok {
		return Item{}, ErrItemNotFound
	}
	it, err := scanItem(kind, s.pool.QueryRow(ctx, q+` WHERE `+alias+`.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, ErrItemNotFound
	}
	if err != nil {
		return Item{}, fmt.Errorf("get %s: %w", kind, err)
	}
	return it, nil
}

// Candidates implements Items.
func (s *PostgresItems) Candidates(ctx context.Context, book Book, emails, phoneTails []string) ([]Item, error) {
	var out []Item
	for _, kind := range ItemKinds() {
		q, alias, _ := itemQuery(kind)
		emailCol, phoneCol, nameAlias := alias+".guest_email", alias+".guest_phone", alias
		if kind == KindAppointment {
			emailCol, phoneCol = "a.customer_email", "a.customer_phone"
		}
		var bookCond string
		var bookID uuid.UUID
		switch {
		case book.OrganisationID != nil:
			bookCond, bookID = nameAlias+".recipient_organisation_id = $1", *book.OrganisationID
		case book.UserID != nil && kind != KindBusinessEnquiry:
			bookCond, bookID = nameAlias+".recipient_organisation_id IS NULL AND "+nameAlias+".recipient_type = 'SELLER' AND "+nameAlias+".recipient_user_id = $1", *book.UserID
		default:
			continue
		}
		rows, err := s.pool.Query(ctx, q+` WHERE `+bookCond+` AND (
			lower(btrim(COALESCE(`+emailCol+`, u.email, ''))) = ANY($2)
			OR (u.email_verified_at IS NOT NULL AND lower(u.email) = ANY($2))
			OR right(regexp_replace(COALESCE(`+phoneCol+`, ''), '[^0-9]', '', 'g'), 7) = ANY($3))
			ORDER BY `+alias+`.created_at DESC LIMIT 500`, bookID, nonNil(emails), nonNil(phoneTails))
		if err != nil {
			return nil, fmt.Errorf("match %s: %w", kind, err)
		}
		got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Item, error) { return scanItem(kind, row) })
		if err != nil {
			return nil, fmt.Errorf("match %s: %w", kind, err)
		}
		out = append(out, got...)
	}
	return out, nil
}
