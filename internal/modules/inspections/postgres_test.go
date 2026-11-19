package inspections

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
// Every run uses a fresh organisation, user, property, unit and tenancy.
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

type fixtures struct{ org, user, property, unit, tenancy uuid.UUID }

// seedRows inserts every row the inspections point at: the organisation,
// the user, the property, its unit and a DRAFT tenancy of that unit (with
// no parties: a tenancy needs none in the table).
func seedRows(t *testing.T, pool *pgxpool.Pool) fixtures {
	t.Helper()
	ctx := context.Background()
	fx := fixtures{org: uuid.New(), user: uuid.New(), property: uuid.New(), unit: uuid.New(), tenancy: uuid.New()}
	suffix := strings.ReplaceAll(fx.org.String(), "-", "")[:12]
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
			[]any{fx.org, "Inspections Test " + suffix}},
		{`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, 'Inspection Tester', 'ACTIVE', now())`,
			[]any{fx.user, "inspections." + suffix + "@example.test"}},
		{`INSERT INTO properties (id, organisation_id, name, address, market_code) VALUES ($1, $2, 'Avondale Court', '12 King George Road', 'ZW')`,
			[]any{fx.property, fx.org}},
		{`INSERT INTO property_units (id, property_id, label, position) VALUES ($1, $2, 'Flat 1', 1)`, []any{fx.unit, fx.property}},
		{`INSERT INTO tenancies (id, organisation_id, property_id, unit_id, rent_amount, rent_currency, rent_frequency, rent_due_day,
			start_date, status, book_name, created_at, updated_at)
			VALUES ($1, $2, $3, $4, 950, 'USD', 'MONTHLY', 1, '2026-12-01', 'DRAFT', 'Inspections Test', now(), now())`,
			[]any{fx.tenancy, fx.org, fx.property, fx.unit}},
	} {
		if _, err := pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return fx
}

func TestPostgresStoreContract(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	fx := seedRows(t, pool)
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	book := tenancies.OrganisationBook(fx.org)
	ref, err := s.NextReference(ctx, 2026)
	if err != nil || !strings.HasPrefix(ref, "RPI-2026-") {
		t.Fatalf("reference %q: %v", ref, err)
	}
	checklist := Snapshot(BuildTemplates(rows())[0])
	in := Inspection{ID: newID(), Reference: ref, Book: book, PropertyID: fx.property, UnitID: &fx.unit, TenancyID: &fx.tenancy,
		Type: TypeMoveIn, Status: StatusScheduled, ScheduledFor: now.Add(24 * time.Hour), InspectorID: &fx.user, InspectorName: "Inspection Tester",
		TemplateCode: "home", TemplateLabel: "Home", Checklist: checklist, CreatedBy: &fx.user, CreatedByName: "Inspection Tester",
		CreatedAt: now, UpdatedAt: now}
	notice := Notice{Type: OutboxScheduled, Payload: EventPayload{InspectionID: in.ID, Reference: ref, Type: in.Type, Status: in.Status,
		TenancyID: &fx.tenancy, OccurredAt: now}}
	if err := s.Apply(ctx, Change{Create: &in, Notices: []Notice{notice}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Inspection(ctx, in.ID)
	if err != nil || got.Reference != ref || got.UnitID == nil || *got.UnitID != fx.unit || !got.Book.Same(book) ||
		len(got.Checklist.Areas) != 2 || got.Checklist.Areas[0].Items[0].ID != checklist.Areas[0].Items[0].ID {
		t.Fatalf("inspection: %+v %v", got, err)
	}
	var outbox int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, in.ID,
		OutboxScheduled).Scan(&outbox); err != nil || outbox != 1 {
		t.Fatalf("outbox: %d %v", outbox, err)
	}
	// A stale version is a conflict; the right one replaces the row.
	next := got
	next.Status, next.Version = StatusInProgress, 1
	next.Checklist.Areas[0].Items[0].Status = FindingOK
	if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{in.ID: {Status: StatusScheduled, Version: 7}},
		Inspections: []Inspection{next}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{in.ID: {Status: StatusScheduled, Version: 0}},
		Inspections: []Inspection{next}}); err != nil {
		t.Fatal(err)
	}
	// Photos: the per-item limit holds.
	item := checklist.Areas[0].Items[0].ID
	for i := range MaxPhotosPerItem {
		p := Photo{ID: newID(), InspectionID: in.ID, ItemID: item, ObjectKey: "inspections/" + strings.ReplaceAll(uuid.NewString(), "-", ""),
			ContentType: "image/jpeg", Width: 10, Height: 10, Size: 100, SHA256: strings.Repeat("a", 64), UploadedBy: &fx.user,
			UploadedAt: now.Add(time.Duration(i) * time.Second)}
		if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{in.ID: {Status: StatusInProgress, AnyVersion: true}},
			Photos: []Photo{p}}); err != nil {
			t.Fatal(err)
		}
	}
	extra := Photo{ID: newID(), InspectionID: in.ID, ItemID: item, ObjectKey: "inspections/extra" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		ContentType: "image/jpeg", Width: 1, Height: 1, Size: 1, SHA256: strings.Repeat("b", 64), UploadedAt: now}
	if err := s.Apply(ctx, Change{Photos: []Photo{extra}}); !errors.Is(err, ErrPhotoLimit) {
		t.Fatalf("photo limit: %v", err)
	}
	photos, err := s.Photos(ctx, in.ID)
	if err != nil || len(photos) != MaxPhotosPerItem || photos[0].ItemID != item {
		t.Fatalf("photos: %d %v", len(photos), err)
	}
	// Lists and counts.
	list, total, counts, err := s.Inspections(ctx, book, Filter{Type: TypeMoveIn, From: now.Format(time.DateOnly)})
	if err != nil || total != 1 || len(list) != 1 || counts[StatusInProgress] != 1 || list[0].PhotoCount != MaxPhotosPerItem {
		t.Fatalf("list: %d %+v %v", total, counts, err)
	}
	if of, err := s.InspectionsOfTenancy(ctx, fx.tenancy); err != nil || len(of) != 1 {
		t.Fatalf("of tenancy: %d %v", len(of), err)
	}
	// Completing freezes the row in the database too.
	cur, _ := s.Inspection(ctx, in.ID)
	done := cur
	done.Status, done.CompletedAt, done.CompletedBy, done.Version = StatusCompleted, &now, &fx.user, cur.Version+1
	for i := range done.Checklist.Areas {
		for j := range done.Checklist.Areas[i].Items {
			done.Checklist.Areas[i].Items[j].Status = FindingOK
		}
	}
	if err := s.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{in.ID: {Status: StatusInProgress, Version: cur.Version}},
		Inspections: []Inspection{done}}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE inspections SET summary = 'changed later' WHERE id = $1`, in.ID); err == nil {
		t.Fatal("a completed inspection changed")
	}
	// Account deletion unlinks the person (the trigger allows it), and the
	// tenancy link may be cleared by its foreign key.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, fx.user, "Deleted account"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenancies WHERE id = $1`, fx.tenancy); err != nil {
		t.Fatal(err)
	}
	after, err := s.Inspection(ctx, in.ID)
	if err != nil || after.InspectorID != nil || after.CompletedBy != nil || after.CreatedBy != nil || after.InspectorName != "Inspection Tester" ||
		after.TenancyID != nil || after.Status != StatusCompleted {
		t.Fatalf("after forget: %+v %v", after, err)
	}
	if left, _ := s.Photos(ctx, in.ID); len(left) == 0 || left[0].UploadedBy != nil {
		t.Fatalf("photos after forget: %+v", left)
	}
}
