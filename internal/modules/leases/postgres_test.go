package leases

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// testDatabaseEnv names a THROWAWAY Postgres database (see the teams
// module's Postgres test). Skipped when unset; never read from .env.
const testDatabaseEnv = "RP365_TEST_DATABASE_URL"

func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(testDatabaseEnv)
	if url == "" {
		t.Skipf("%s is not set: the Postgres store is only exercised against a throwaway database", testDatabaseEnv)
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

// The Postgres store, end to end: a template's versions and approval, a
// lease's creation, change, listing, audit trail and deletion.
func TestPostgresStore(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	tid := uuid.New()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM leases WHERE template_id IN (SELECT id FROM lease_templates WHERE family_id = $1)`, tid)
		_, _ = pool.Exec(context.Background(), `DELETE FROM lease_templates WHERE family_id = $1`, tid)
	})
	tmpl := Template{ID: tid, FamilyID: tid, Version: 1, MarketCode: "ZW", Name: "PG test", LeaseKind: KindResidential,
		Sections: []Section{{Heading: "Rent", Text: "{{rent_amount}}"}}, Status: TemplateDraft, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateTemplate(ctx, tmpl, TemplateEvent{ID: uuid.New(), TemplateID: tid, Action: TemplateEventCreated, At: now}); err != nil {
		t.Fatal(err)
	}
	approved, err := s.ApproveTemplate(ctx, tid, LegalReview{ReviewerName: "R", Organisation: "O", ReviewedOn: "2026-11-01"}, nil, "", now,
		func(t Template, _ []Template) []TemplateEvent {
			return []TemplateEvent{{ID: uuid.New(), TemplateID: t.ID, Action: TemplateEventApproved, At: now}}
		})
	if err != nil || approved.Status != TemplateApproved || approved.LegalReview == nil {
		t.Fatalf("approve: %+v %v", approved, err)
	}
	v2 := approved
	v2.ID, v2.Status, v2.LegalReview, v2.ApprovedAt = uuid.New(), TemplateDraft, nil, nil
	if v2, err = s.CreateTemplateVersion(ctx, v2, TemplateEvent{ID: uuid.New(), Action: TemplateEventNewVersion, At: now}); err != nil || v2.Version != 2 {
		t.Fatalf("new version: %+v %v", v2, err)
	}
	v3 := v2
	v3.ID = uuid.New()
	if _, err := s.CreateTemplateVersion(ctx, v3, TemplateEvent{ID: uuid.New(), Action: TemplateEventNewVersion, At: now}); !errors.Is(err, ErrTemplateDraftExists) {
		t.Fatalf("a second draft: %v", err)
	}

	ref, err := s.NextReference(ctx, 2026)
	if err != nil {
		t.Fatal(err)
	}
	l := Lease{ID: uuid.New(), Reference: ref, Status: StatusDraft, TemplateID: tid, TemplateVersion: 1, TemplateName: "PG test", MarketCode: "ZW",
		LeaseKind: KindResidential, Input: Input{TemplateID: tid, PropertyAddress: "1 Road", RentAmount: "1.00"}, CreatedAt: now, UpdatedAt: now,
		Documents: map[string]Document{DocumentDraft: {Kind: DocumentDraft, TemplateID: tid, TemplateVersion: 1, Snapshot: []byte(`{}`),
			ObjectKey: "leases/pgtest", SHA256: Checksum([]byte("x")), Size: 1, GeneratedAt: now}}}
	if err := s.CreateLease(ctx, l, []Event{{ID: uuid.New(), LeaseID: l.ID, Action: EventCreated, ActorType: ActorUser, At: now}}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateLease(ctx, Lease{ID: uuid.New(), Reference: ref, Status: StatusDraft, TemplateID: tid, TemplateVersion: 1, MarketCode: "ZW",
		LeaseKind: KindResidential, CreatedAt: now, UpdatedAt: now}, nil); !errors.Is(err, ErrReferenceTaken) {
		t.Fatalf("a reference is unique: %v", err)
	}
	changed, err := s.ChangeLease(ctx, l.ID, func(cur *Lease) ([]Event, error) {
		cur.Input.RentAmount = "2.00"
		d := cur.Documents[DocumentDraft]
		d.ObjectKey = "leases/pgtest2"
		cur.Documents[DocumentDraft] = d
		return []Event{{ID: uuid.New(), LeaseID: cur.ID, Action: EventEdited, ActorType: ActorUser, At: now.Add(time.Second)}}, nil
	})
	if err != nil || changed.Input.RentAmount != "2.00" {
		t.Fatalf("change: %v", err)
	}
	got, err := s.Lease(ctx, l.ID)
	if err != nil || got.Documents[DocumentDraft].ObjectKey != "leases/pgtest2" {
		t.Fatalf("reload: %+v %v", got.Documents, err)
	}
	list, total, err := s.Leases(ctx, LeaseFilter{All: true, Status: StatusDraft, Limit: 500})
	if err != nil || total < 1 || len(list) == 0 {
		t.Fatalf("list: %d %v", total, err)
	}
	events, err := s.Events(ctx, l.ID)
	if err != nil || len(events) != 2 {
		t.Fatalf("events: %d %v", len(events), err)
	}
	if _, err := pool.Exec(ctx, `UPDATE lease_events SET action = 'SENT' WHERE lease_id = $1`, l.ID); err == nil {
		t.Fatal("lease events are immutable")
	}
	if _, err := s.DeleteLease(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lease(ctx, l.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("deleted: %v", err)
	}
}
