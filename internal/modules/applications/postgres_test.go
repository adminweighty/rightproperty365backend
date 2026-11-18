package applications

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres store is only exercised against a throwaway database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	migs, err := migrate.Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.NewRunner(conn, migs).Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

var referenceRe = regexp.MustCompile(`^RPA-APP-2026-\d{6}$`)

// storeContract runs the same checks on both stores. org, applicant and
// listing are real rows for Postgres; outbox counts the application events.
func storeContract(t *testing.T, s Store, org, applicant, listing uuid.UUID, outbox func() int) (Application, Form) {
	ctx := context.Background()
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	book := OrganisationBook(org)
	v1 := Form{ID: newID(), Version: 1, Status: FormDraft, OrganisationID: &org, MarketCode: "ZW", Name: "Rental", Purpose: "P", PrivacyNotice: "N",
		RetentionDays: 30, Sections: rentalSectionsV1(), CreatedAt: now, UpdatedAt: now}
	v1.FamilyID = v1.ID
	if err := s.CreateForm(ctx, v1); err != nil {
		t.Fatal(err)
	}
	v1.Name = "Rental application"
	if err := s.UpdateDraft(ctx, v1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, v1.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, v1.ID, now); !errors.Is(err, ErrFormState) {
		t.Fatalf("publish twice: %v", err)
	}
	if err := s.PutIntake(ctx, Intake{ListingID: listing, FormID: v1.ID, Status: IntakeOpen, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	// An application, submitted with a reference, a file, events and a note.
	a := Application{ID: newID(), ListingID: listing, FormID: v1.ID, FormVersion: 1, ApplicantUserID: &applicant, ApplicantName: "Tariro Tenant",
		OrganisationID: &org, Status: StatusDraft, Answers: Answers{"occupants": float64(2)}, CreatedAt: now, UpdatedAt: now}
	a.Listing.Title = "Flat one"
	stored, err := s.Apply(ctx, Change{Application: &a, Insert: true, Events: []Event{{ID: newID(), ApplicationID: a.ID, Kind: EventCreated,
		ToStatus: StatusDraft, ActorType: ActorApplicant, ActorUserID: &applicant, ActorName: "Tariro", At: now}}})
	if err != nil || stored.Reference != "" {
		t.Fatalf("insert %+v %v", stored, err)
	}
	dup := a
	dup.ID = newID()
	if _, err := s.Apply(ctx, Change{Application: &dup, Insert: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second application for the same listing and version: %v", err)
	}
	if got, ok, err := s.Existing(ctx, applicant, listing, v1.ID); err != nil || !ok || got.ID != a.ID || got.Answers["occupants"] != float64(2) {
		t.Fatalf("existing %+v %v %v", got, ok, err)
	}
	file := File{ID: newID(), ApplicationID: a.ID, FieldKey: "payslip", FileName: "p.pdf", ContentType: "application/pdf", Size: 10, SHA256: "x",
		ObjectKey: "applications/" + uuid.NewString()[:8], UploadedAt: now}
	later := now.Add(time.Minute)
	a.Status, a.UpdatedAt, a.SubmittedAt, a.LastSubmittedAt = StatusSubmitted, later, &later, &later
	a.Answers = Answers{"occupants": float64(2), "move_in": "2026-12-01", "has_pets": "no", "payslip": []string{file.ID.String()}}
	stale := a
	if _, err := s.Apply(ctx, Change{Application: &stale, Expect: now.Add(time.Hour)}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale write: %v", err)
	}
	before := outbox()
	stored, err = s.Apply(ctx, Change{Application: &a, Expect: now, NextReference: true, AddFiles: []File{file},
		Events: []Event{{ID: newID(), ApplicationID: a.ID, Kind: EventSubmitted, FromStatus: StatusDraft, ToStatus: StatusSubmitted, ActorType: ActorApplicant, At: later}},
		Notes:  []Note{{ID: newID(), ApplicationID: a.ID, Text: "Internal", At: later}},
		Outbox: []Outbox{{ID: newID(), Type: EventTypeSubmitted, Payload: EventPayload{ApplicationID: a.ID, ListingID: listing, OccurredAt: later}}}})
	if err != nil || !referenceRe.MatchString(stored.Reference) {
		t.Fatalf("submit %+v %v", stored, err)
	}
	if outbox()-before != 1 {
		t.Fatal("no outbox event")
	}
	got, err := s.Application(ctx, a.ID)
	if err != nil || got.Reference != stored.Reference || got.Status != StatusSubmitted || got.Listing.Title != "Flat one" ||
		len(got.Answers["payslip"].([]string)) != 1 {
		t.Fatalf("read back %+v %v", got, err)
	}
	if ev, _ := s.Events(ctx, a.ID); len(ev) != 2 || ev[1].Kind != EventSubmitted {
		t.Fatalf("events %+v", ev)
	}
	if notes, _ := s.Notes(ctx, a.ID); len(notes) != 1 {
		t.Fatalf("notes %+v", notes)
	}
	// The version with a submission is counted and frozen.
	f, err := s.Form(ctx, v1.ID)
	if err != nil || f.Submissions != 1 || f.Status != FormPublished {
		t.Fatalf("form %+v %v", f, err)
	}
	// Version 2 takes the intake on publish; v1 is retired.
	v2 := v1
	v2.ID, v2.Status, v2.Name, v2.PublishedAt = newID(), FormDraft, "Rental application v2", nil
	if v2, err = s.CreateVersion(ctx, v2); err != nil || v2.Version != 2 {
		t.Fatalf("version 2 %+v %v", v2, err)
	}
	extra := v2
	extra.ID = newID()
	if _, err := s.CreateVersion(ctx, extra); !errors.Is(err, ErrDraftExists) {
		t.Fatalf("second draft: %v", err)
	}
	if _, err := s.Publish(ctx, v2.ID, later); err != nil {
		t.Fatal(err)
	}
	if in, ok, _ := s.Intake(ctx, listing); !ok || in.FormID != v2.ID {
		t.Fatalf("intake after publish %+v", in)
	}
	if old, _ := s.Form(ctx, v1.ID); old.Status != FormRetired || old.RetiredAt == nil {
		t.Fatalf("v1 after publish %+v", old)
	}
	if list, _ := s.Forms(ctx, book, "", "", &v1.FamilyID); len(list) != 2 || list[0].Version != 2 {
		t.Fatalf("family %+v", list)
	}
	// The receiving side's list: drafts never, ?q= on the reference.
	page, total, counts, err := s.Received(ctx, AppFilter{Book: book, Q: stored.Reference[len(stored.Reference)-6:], Limit: 10})
	if err != nil || total != 1 || len(page) != 1 || counts[StatusSubmitted] != 1 {
		t.Fatalf("received %d %v %v", total, counts, err)
	}
	if _, total, _, _ := s.Received(ctx, AppFilter{Book: book, ListingIDs: []uuid.UUID{}, Limit: 10}); total != 0 {
		t.Fatal("an empty listing filter matched")
	}
	if mine, total, _ := s.Mine(ctx, applicant, "", 10, 0); total != 1 || mine[0].ID != a.ID {
		t.Fatalf("mine %d", total)
	}
	// Retention: decided long ago, erased once, the file queued.
	decided := now.AddDate(0, 0, -40)
	a = got
	a.Status, a.DecidedAt, a.UpdatedAt = StatusRejected, &decided, later.Add(time.Minute)
	if _, err := s.Apply(ctx, Change{Application: &a, Expect: got.UpdatedAt}); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueForErasure(ctx, now, 10)
	if err != nil || len(due) != 1 || due[0].ID != a.ID {
		t.Fatalf("due %+v %v", due, err)
	}
	if ok, err := s.Erase(ctx, a.ID, ErasedByRetention, now); err != nil || !ok {
		t.Fatalf("erase %v %v", ok, err)
	}
	if ok, _ := s.Erase(ctx, a.ID, ErasedByRetention, now); ok {
		t.Fatal("erased twice")
	}
	tomb, _ := s.Application(ctx, a.ID)
	if tomb.ErasedAt == nil || len(tomb.Answers) != 0 || tomb.Reference != stored.Reference {
		t.Fatalf("tombstone %+v", tomb)
	}
	if files, _ := s.Files(ctx, a.ID); len(files) != 0 {
		t.Fatal("files kept")
	}
	keys, _ := s.PendingPurges(ctx, 100)
	found := false
	for _, k := range keys {
		found = found || k == file.ObjectKey
	}
	if !found {
		t.Fatalf("purge queue %v", keys)
	}
	if err := s.DonePurges(ctx, []string{file.ObjectKey}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAudit(ctx, []orgaudit.Event{orgaudit.New(org, applicant, "Viewer", orgaudit.ActionApplicationViewed, orgaudit.TargetApplication,
		a.ID, stored.Reference, nil, nil, now)}); err != nil {
		t.Fatal(err)
	}
	return tomb, v1
}

func TestMemoryStoreContract(t *testing.T) {
	s := NewMemoryStore()
	storeContract(t, s, uuid.New(), uuid.New(), uuid.New(), func() int { return len(s.OutboxEvents()) })
}

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	org, applicant, seller, listing := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	suffix := org.String()[:8]
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
		org, "Applications Test "+suffix)
	for _, u := range []uuid.UUID{applicant, seller} {
		exec(`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Applications Tester', 'ACTIVE', now())`,
			u, "apps."+u.String()[:8]+"@example.test")
	}
	exec(`INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, organisation_id)
		VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'Applications contract', 'DRAFT', 100, 'USD', $2)`, listing, org)
	s := NewPostgresStore(pool)
	tomb, v1 := storeContract(t, s, org, applicant, listing, func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_type = 'application'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	})

	// The database guard: a version with a submission never changes and is
	// never deleted; its status may still move.
	if _, err := pool.Exec(ctx, `UPDATE forms SET name = 'Changed' WHERE id = $1`, v1.ID); pgCode(err) != "23514" {
		t.Fatalf("content change of a submitted version: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE forms SET sections = '[]'::jsonb WHERE id = $1`, v1.ID); pgCode(err) != "23514" {
		t.Fatalf("sections change of a submitted version: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM forms WHERE id = $1`, v1.ID); err == nil {
		t.Fatal("a submitted version was deleted")
	}
	exec(`UPDATE forms SET updated_at = now() WHERE id = $1`, v1.ID)
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM organisation_audit_events WHERE organisation_id = $1 AND action = 'APPLICATION_VIEWED'`, org).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("%d audit rows, %v", audits, err)
	}

	// The CRM reads submitted applications.
	items := crm.NewPostgresItems(pool)
	it, err := items.Item(ctx, crm.KindApplication, tomb.ID)
	if err != nil || it.Reference != tomb.Reference || it.OrganisationID == nil || *it.OrganisationID != org || it.Title != "Applications contract" {
		t.Fatalf("crm item %+v %v", it, err)
	}
	if got, err := items.Candidates(ctx, crm.OrganisationBook(org), []string{"apps." + applicant.String()[:8] + "@example.test"}, nil); err != nil || len(got) != 1 {
		t.Fatalf("crm candidates %+v %v", got, err)
	}

	// Account deletion inside a transaction: drafts deleted, the others
	// withdrawn, erased and unlinked.
	other := uuid.New()
	exec(`INSERT INTO listings (id, market_id, title, status, price_amount, currency_code, organisation_id)
		VALUES ($1, (SELECT id FROM markets WHERE code = 'ZW'), 'Second', 'DRAFT', 100, 'USD', $2)`, other, org)
	now := time.Now().UTC().Truncate(time.Microsecond)
	draft := Application{ID: newID(), ListingID: other, FormID: v1.ID, FormVersion: 1, ApplicantUserID: &applicant, ApplicantName: "T", OrganisationID: &org,
		Status: StatusDraft, Answers: Answers{}, CreatedAt: now, UpdatedAt: now}
	if _, err := s.Apply(ctx, Change{Application: &draft, Insert: true, AddFiles: []File{{ID: newID(), ApplicationID: draft.ID, FieldKey: "payslip",
		FileName: "d.pdf", ContentType: "application/pdf", Size: 1, SHA256: "y", ObjectKey: "applications/" + uuid.NewString()[:8], UploadedAt: now}}}); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	deleted, anonymised, err := EraseApplicantTx(ctx, tx, applicant, now)
	if err != nil || deleted != 1 || anonymised != 1 {
		_ = tx.Rollback(ctx)
		t.Fatalf("erase applicant %d %d %v", deleted, anonymised, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Application(ctx, draft.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("draft kept")
	}
	if got, _ := s.Application(ctx, tomb.ID); got.ApplicantUserID != nil || got.ApplicantName != DeletedApplicantName {
		t.Fatalf("not anonymised %+v", got)
	}
}
