package listings

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// The listing expiry job and its reminders (API 0.45.0, FR-006, FR-044,
// FR-053, FSD section 4).
//
// The read rule stays: a PUBLISHED listing whose expires_at has passed reads
// EXPIRED everywhere (EffectiveStatus, the public filter, counts, quotas), so
// a delayed worker never keeps an expired listing public. The sweep, run by
// the worker every ExpirySweepInterval, makes it durable and tells the
// advertiser:
//
//  1. It claims PUBLISHED listings with expires_at <= now + 14 days that have
//     a notice due, in batches of ExpiryBatchSize, each batch in one
//     transaction (FOR UPDATE SKIP LOCKED: concurrent workers never take the
//     same row), with their current cycle (publication_cycles.relist_no).
//  2. expires_at <= now: the listing is stored EXPIRED, with a timeline entry
//     (actor SYSTEM) and the EXPIRED notice; LISTING_EXPIRED is written (the
//     search index event, which the notifications now also handle). A
//     pending revision is left as it is.
//  3. Otherwise the smallest threshold N in {14, 7, 1} days with
//     expires_at - now <= N is due: its notice is recorded, the larger ones
//     not yet recorded are recorded as skipped, and LISTING_EXPIRING
//     {days: N} is written when the notice for N was recorded now. A worker
//     that was down sends only the most recent reminder.
//
// listing_expiry_notices' key (listing_id, relist_no, kind) makes every
// reminder and every expiry happen once per publication cycle, across reruns
// and replicas. A relist starts a new cycle, with reminders of its own.

// Expiry states (Expiry.State).
const (
	ExpiryActive       = "ACTIVE"
	ExpiryExpiringSoon = "EXPIRING_SOON"
	ExpiryExpired      = "EXPIRED"
)

// ExpiryStates lists the expiry states (the list filter's values).
func ExpiryStates() []string { return []string{ExpiryActive, ExpiryExpiringSoon, ExpiryExpired} }

// ExpiringSoonWindow is how long before the end of its period a listing is
// EXPIRING_SOON (and gets its first reminder).
const ExpiringSoonWindow = 14 * 24 * time.Hour

// Expiry is the publication period of a listing as its managers see it
// (ManagedView, the list rows, the admin view).
type Expiry struct {
	ExpiresAt time.Time `json:"expires_at"`
	// DaysLeft is ceil((expires_at - now) / 24h) while the listing is live,
	// 0 otherwise.
	DaysLeft int `json:"days_left"`
	// State is ACTIVE (live, more than 14 days left), EXPIRING_SOON (live,
	// 14 days or fewer) or EXPIRED (not live: the period is over, or the
	// listing is archived or waits for the review of a relist).
	State string `json:"state"`
	// RelistNo is the publication cycle, 1 for the first publication.
	RelistNo int `json:"relist_no"`
}

// CycleNo is the listing's current publication cycle, 1-based (1 for the
// first publication); 0 for a listing that was never published.
func (r Record) CycleNo() int {
	switch {
	case r.Cycles > 0:
		return r.Cycles
	case !r.PublishedAt.IsZero():
		return 1
	}
	return 0
}

// ExpiryOf returns the publication period of a listing at now, nil when it
// was never published.
func ExpiryOf(r Record, now time.Time) *Expiry {
	if r.PublishedAt.IsZero() || r.ExpiresAt.IsZero() {
		return nil
	}
	e := &Expiry{ExpiresAt: r.ExpiresAt.UTC(), State: ExpiryExpired, RelistNo: r.CycleNo()}
	if r.EffectiveStatus(now) == StatusPublished {
		left := r.ExpiresAt.Sub(now)
		e.DaysLeft = int(left / (24 * time.Hour))
		if left%(24*time.Hour) != 0 {
			e.DaysLeft++
		}
		e.State = ExpiryActive
		if left <= ExpiringSoonWindow {
			e.State = ExpiryExpiringSoon
		}
	}
	return e
}

// Kinds of an expiry notice (listing_expiry_notices.kind).
const (
	NoticeDays14  = "DAYS_14"
	NoticeDays7   = "DAYS_7"
	NoticeDays1   = "DAYS_1"
	NoticeExpired = "EXPIRED"
)

// reminderThresholds are the reminders, smallest first.
var reminderThresholds = []struct {
	kind string
	days int
}{{NoticeDays1, 1}, {NoticeDays7, 7}, {NoticeDays14, 14}}

// EventListingExpiring is written when a reminder is due. Its payload is
// ExpiryEventPayload with Days set. (EventListingExpired, the search index
// event, carries the same payload without Days.)
const EventListingExpiring = "LISTING_EXPIRING"

// ExpiryEventTypes are the events the advertiser's expiry emails follow.
func ExpiryEventTypes() []string { return []string{EventListingExpiring, EventListingExpired} }

// ExpiryEventPayload is the payload of LISTING_EXPIRING and LISTING_EXPIRED.
// RelistNo is the 1-based cycle, as in Expiry.
type ExpiryEventPayload struct {
	ListingID  uuid.UUID `json:"listing_id"`
	Days       int       `json:"days,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
	RelistNo   int       `json:"relist_no"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ExpiryBatchSize is how many listings one transaction of the sweep claims.
const ExpiryBatchSize = 200

// ExpiryResult counts what a sweep did.
type ExpiryResult struct {
	// Claimed listings had a notice due; Expired were stored EXPIRED;
	// Reminded got a LISTING_EXPIRING event; Skipped reminders were recorded
	// without one (a later reminder was already due).
	Claimed, Expired, Reminded, Skipped int
}

func (r *ExpiryResult) add(o ExpiryResult) {
	r.Claimed += o.Claimed
	r.Expired += o.Expired
	r.Reminded += o.Reminded
	r.Skipped += o.Skipped
}

// ExpiryStore is the storage side of the sweep (memory and Postgres).
type ExpiryStore interface {
	// ExpiryBatch deals with up to limit listings that have a notice due at
	// now, in one transaction (see the top of this file).
	ExpiryBatch(ctx context.Context, now time.Time, limit int) (ExpiryResult, error)
}

// maxExpiryBatches bounds one sweep (200 000 listings); the next run goes on.
const maxExpiryBatches = 1000

// SweepExpiry runs batches until one claims fewer than ExpiryBatchSize
// listings. Idempotent: a second run at the same instant does nothing.
func SweepExpiry(ctx context.Context, store ExpiryStore, now time.Time) (ExpiryResult, error) {
	now = now.UTC().Truncate(time.Microsecond)
	var total ExpiryResult
	for range maxExpiryBatches {
		r, err := store.ExpiryBatch(ctx, now, ExpiryBatchSize)
		if err != nil {
			return total, err
		}
		total.add(r)
		if r.Claimed < ExpiryBatchSize {
			break
		}
	}
	return total, nil
}

// expiryCandidate is a claimed listing: relistNo is its current
// publication_cycles.relist_no (0-based; 0 without a cycle row).
type expiryCandidate struct {
	id        uuid.UUID
	expiresAt time.Time
	relistNo  int
}

// expiryNotice is one row of listing_expiry_notices to record.
type expiryNotice struct {
	kind    string
	skipped bool
}

// expiryStep is what the sweep does to one listing. notices[0] is the due
// notice: event is written (and, for an expiry, the listing changes) only
// when it is recorded now.
type expiryStep struct {
	expire  bool
	days    int
	notices []expiryNotice
}

// planExpiry decides the step of a listing ending at expiresAt (false when
// nothing is due yet).
func planExpiry(expiresAt, now time.Time) (expiryStep, bool) {
	left := expiresAt.Sub(now)
	if left <= 0 {
		return expiryStep{expire: true, notices: []expiryNotice{{kind: NoticeExpired}}}, true
	}
	for i, t := range reminderThresholds {
		if left <= time.Duration(t.days)*24*time.Hour {
			step := expiryStep{days: t.days, notices: []expiryNotice{{kind: t.kind}}}
			for _, larger := range reminderThresholds[i+1:] {
				step.notices = append(step.notices, expiryNotice{kind: larger.kind, skipped: true})
			}
			return step, true
		}
	}
	return expiryStep{}, false
}

// dueKind is the notice kind due for a listing ending at expiresAt ("" for none).
func dueKind(expiresAt, now time.Time) string {
	step, ok := planExpiry(expiresAt, now)
	if !ok {
		return ""
	}
	return step.notices[0].kind
}

// event is the outbox event of a step whose due notice was recorded now.
func (st expiryStep) event(c expiryCandidate, now time.Time) ChangeEvent {
	payload := ExpiryEventPayload{ListingID: c.id, Days: st.days, ExpiresAt: c.expiresAt.UTC(), RelistNo: c.relistNo + 1, OccurredAt: now}
	raw, _ := json.Marshal(payload)
	eventType := EventListingExpiring
	if st.expire {
		eventType = EventListingExpired
	}
	return ChangeEvent{Type: eventType, Payload: raw}
}

// expiryEntry is the timeline entry of an expiry.
func expiryEntry(id uuid.UUID, now time.Time) *TimelineEntry {
	return &TimelineEntry{ID: uuid.New(), ListingID: id, ActorKind: ActorSystem, Action: ActionExpired,
		FromStatus: StatusPublished, ToStatus: StatusExpired, CreatedAt: now}
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

// expiryNoticeKey is the key of a notice (listing_expiry_notices).
type expiryNoticeKey struct {
	listing  uuid.UUID
	relistNo int
	kind     string
}

// ExpiryBatch implements ExpiryStore.
func (s *MemoryStore) ExpiryBatch(_ context.Context, now time.Time, limit int) (ExpiryResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []int
	for i, r := range s.records {
		if r.Status != StatusPublished || r.ExpiresAt.IsZero() || r.ExpiresAt.After(now.Add(ExpiringSoonWindow)) {
			continue
		}
		kind := dueKind(r.ExpiresAt, now)
		if kind == NoticeExpired || !s.notices[expiryNoticeKey{r.ID, max(s.cycles[r.ID]-1, 0), kind}] {
			due = append(due, i)
		}
	}
	slices.SortStableFunc(due, func(a, b int) int { return s.records[a].ExpiresAt.Compare(s.records[b].ExpiresAt) })
	if len(due) > limit {
		due = due[:limit]
	}
	out := ExpiryResult{Claimed: len(due)}
	for _, i := range due {
		r := s.records[i]
		c := expiryCandidate{id: r.ID, expiresAt: r.ExpiresAt, relistNo: max(s.cycles[r.ID]-1, 0)}
		step, _ := planExpiry(r.ExpiresAt, now)
		recorded := make([]bool, len(step.notices))
		for n, notice := range step.notices {
			k := expiryNoticeKey{c.id, c.relistNo, notice.kind}
			if !s.notices[k] {
				s.notices[k], recorded[n] = true, true
				if notice.skipped {
					out.Skipped++
				}
			}
		}
		ch := Change{}
		if step.expire {
			s.records[i].Status, s.records[i].UpdatedAt = StatusExpired, now
			ch.Entry = expiryEntry(r.ID, now)
			out.Expired++
		}
		if recorded[0] {
			ch.Events = append(ch.Events, step.event(c, now))
			if !step.expire {
				out.Reminded++
			}
		}
		s.record(r.ID, ch, now)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// currentRelistNo is the listing's current publication_cycles.relist_no.
const currentRelistNo = `COALESCE((SELECT max(pc.relist_no) FROM publication_cycles pc WHERE pc.listing_id = l.id), 0)`

// claimExpiry selects the listings with a notice due at $1 (the same
// thresholds as planExpiry, in exact hours), locking them.
const claimExpiry = `
	SELECT l.id, l.expires_at, ` + currentRelistNo + `
	FROM listings l
	WHERE l.status = 'PUBLISHED' AND l.expires_at IS NOT NULL AND l.expires_at <= $1::timestamptz + interval '336 hours'
	  AND (l.expires_at <= $1::timestamptz OR NOT EXISTS (
	        SELECT 1 FROM listing_expiry_notices n
	        WHERE n.listing_id = l.id AND n.relist_no = ` + currentRelistNo + `
	          AND n.kind = CASE WHEN l.expires_at <= $1::timestamptz + interval '24 hours' THEN 'DAYS_1'
	                            WHEN l.expires_at <= $1::timestamptz + interval '168 hours' THEN 'DAYS_7'
	                            ELSE 'DAYS_14' END))
	ORDER BY l.expires_at, l.id
	LIMIT $2
	FOR UPDATE OF l SKIP LOCKED`

// ExpiryBatch implements ExpiryStore.
func (s *PostgresStore) ExpiryBatch(ctx context.Context, now time.Time, limit int) (ExpiryResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ExpiryResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, claimExpiry, now, limit)
	if err != nil {
		return ExpiryResult{}, fmt.Errorf("claim expiring listings: %w", err)
	}
	claimed, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (expiryCandidate, error) {
		var c expiryCandidate
		err := row.Scan(&c.id, &c.expiresAt, &c.relistNo)
		return c, err
	})
	if err != nil {
		return ExpiryResult{}, fmt.Errorf("claim expiring listings: %w", err)
	}
	out := ExpiryResult{Claimed: len(claimed)}
	for _, c := range claimed {
		step, ok := planExpiry(c.expiresAt, now)
		if !ok {
			continue
		}
		recordedDue := false
		for n, notice := range step.notices {
			tag, err := tx.Exec(ctx, `
				INSERT INTO listing_expiry_notices (listing_id, relist_no, kind, skipped, created_at)
				VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`, c.id, c.relistNo, notice.kind, notice.skipped, now)
			if err != nil {
				return ExpiryResult{}, fmt.Errorf("listing expiry notice: %w", err)
			}
			if tag.RowsAffected() == 1 {
				if n == 0 {
					recordedDue = true
				}
				if notice.skipped {
					out.Skipped++
				}
			}
		}
		ch := Change{}
		if step.expire {
			tag, err := tx.Exec(ctx, `UPDATE listings SET status = 'EXPIRED', updated_at = $2 WHERE id = $1 AND status = 'PUBLISHED'`, c.id, now)
			if err != nil {
				return ExpiryResult{}, fmt.Errorf("expire listing: %w", err)
			}
			if tag.RowsAffected() == 1 {
				ch.Entry = expiryEntry(c.id, now)
				out.Expired++
			}
		}
		if recordedDue {
			ch.Events = append(ch.Events, step.event(c, now))
			if !step.expire {
				out.Reminded++
			}
		}
		if err := writeChange(ctx, tx, c.id, ch, now); err != nil {
			return ExpiryResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ExpiryResult{}, err
	}
	return out, nil
}
