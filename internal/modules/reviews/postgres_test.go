package reviews

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

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// RP365_TEST_DATABASE_URL names a THROWAWAY Postgres database (see the
// teams module's Postgres test). Skipped when unset; never read from .env.
// Every run uses fresh organisations, users and a profile.
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

type fixtures struct{ provider, reviewerOrg, profile, reviewer, reporter, moderator, enquiry uuid.UUID }

// seedRows inserts every row the reviews point at: the provider and the
// reviewing organisations, the users (reviewer, reporter, moderator), the
// provider's public profile and a business enquiry to it.
func seedRows(t *testing.T, pool *pgxpool.Pool) fixtures {
	t.Helper()
	ctx := context.Background()
	fx := fixtures{provider: uuid.New(), reviewerOrg: uuid.New(), profile: uuid.New(), reviewer: uuid.New(), reporter: uuid.New(),
		moderator: uuid.New(), enquiry: uuid.New()}
	suffix := strings.ReplaceAll(fx.provider.String(), "-", "")[:12]
	user := func(id uuid.UUID, name string) struct {
		sql  string
		args []any
	} {
		return struct {
			sql  string
			args []any
		}{`INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, $3, 'ACTIVE', now())`,
			[]any{id, strings.ToLower(strings.ReplaceAll(name, " ", ".")) + "." + suffix + "@example.test", name}}
	}
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'SERVICE_PROVIDER', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
			[]any{fx.provider, "Reviews Plumbers " + suffix}},
		{`INSERT INTO organisations (id, market_id, name, kind, status) SELECT $1, id, $2, 'AGENCY', 'ACTIVE' FROM markets WHERE code = 'ZW'`,
			[]any{fx.reviewerOrg, "Reviews Estates " + suffix}},
		user(fx.reviewer, "Review Writer"),
		user(fx.reporter, "Review Reporter"),
		user(fx.moderator, "Review Moderator"),
		{`INSERT INTO directory_profiles (id, market_id, kind, slug, name, organisation_id, status, verified)
			SELECT $1, id, 'service-providers', $2, 'Reviews Plumbers', $3, 'ACTIVE', true FROM markets WHERE code = 'ZW'`,
			[]any{fx.profile, "reviews-plumbers-" + suffix, fx.provider}},
		{`INSERT INTO business_enquiries (id, reference, market_id, profile_id, status, message, preferred_contact_method, idempotency_key,
				request_hash, customer_user_id, guest_name, guest_email, consent_privacy_version, consent_accepted_at, recipient_organisation_id,
				created_at, updated_at)
			SELECT $1, $2, m.id, $3, 'RECEIVED', 'Test', 'EMAIL', $4, 'x', $5, 'Review Writer', 'w@example.test', '2026-01', now(), $6,
				now() - interval '20 days', now() - interval '20 days' FROM markets m WHERE m.code = 'ZW'`,
			[]any{fx.enquiry, "BE-RV" + strings.ToUpper(suffix[:6]), fx.profile, "reviews-test-" + suffix, fx.reviewer, fx.provider}},
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

	r := Review{ID: newID(), ProfileID: fx.profile, ProviderOrganisationID: fx.provider, ProviderKind: ReviewableKind,
		ProviderSlug: "reviews-plumbers", ProviderName: "Reviews Plumbers", ReviewerKind: ReviewerCustomer, ReviewerUserID: &fx.reviewer,
		ReviewerName: "Review W.", BasisKind: BasisEnquiry, BasisID: fx.enquiry, BasisReference: "BE-TEST", Rating: 4,
		Text: "Answered the same day and fixed the leak.", Status: StatusPending, Version: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.Apply(ctx, Change{Create: &r}); err != nil {
		t.Fatal(err)
	}
	dup := r
	dup.ID = newID()
	if err := s.Apply(ctx, Change{Create: &dup}); !errors.Is(err, ErrBasisReviewed) {
		t.Fatalf("one review per basis: %v", err)
	}
	org := r
	org.ID, org.BasisKind, org.BasisID, org.ReviewerKind = newID(), BasisMaintenance, uuid.New(), ReviewerOrganisation
	if err := s.Apply(ctx, Change{Create: &org}); err == nil {
		t.Fatal("an organisation's review without its organisation was stored")
	}
	org.ReviewerOrganisationID, org.ReviewerName, org.AuthorName = &fx.reviewerOrg, "Reviews Estates", "Review Writer"
	if err := s.Apply(ctx, Change{Create: &org}); err != nil {
		t.Fatal(err)
	}
	bad := r
	bad.ID, bad.BasisID, bad.Rating = newID(), uuid.New(), 6
	if err := s.Apply(ctx, Change{Create: &bad}); err == nil {
		t.Fatal("a rating of 6 was stored")
	}
	bad.Rating, bad.Text = 3, "too short"
	if err := s.Apply(ctx, Change{Create: &bad}); err == nil {
		t.Fatal("a 9-character text was stored")
	}

	// A report needs a published review.
	report := Report{ID: newID(), ReviewID: r.ID, ReporterUserID: &fx.reporter, Reason: "Spam", Status: ReportOpen, CreatedAt: now}
	if err := s.Apply(ctx, Change{Reports: []Report{report}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report of a pending review: %v", err)
	}
	published := r
	published.Status, published.PublishedAt, published.ModeratedAt, published.ModeratedBy, published.ModeratedByName =
		StatusPublished, &now, &now, &fx.moderator, "Review Moderator"
	audit := adminaudit.New(fx.moderator, "Review Moderator", adminaudit.ActionReviewPublished, adminaudit.TargetProviderReview, r.ID.String(),
		"Review of Reviews Plumbers", "", map[string]any{"status": StatusPending}, map[string]any{"status": StatusPublished}, now)
	notice := EventPayload{ReviewID: r.ID, Action: ActionPublish, Status: StatusPublished, FirstPublish: true, ActorUserID: &fx.moderator,
		OccurredAt: now}
	if err := s.Apply(ctx, Change{Update: &published, ExpectVersion: 9}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: %v", err)
	}
	if err := s.Apply(ctx, Change{Update: &published, ExpectVersion: 1, Audit: []adminaudit.Event{audit}, Notices: []EventPayload{notice}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Apply(ctx, Change{Reports: []Report{report}}); err != nil {
		t.Fatal(err)
	}
	again := report
	again.ID = newID()
	if err := s.Apply(ctx, Change{Reports: []Report{again}}); !errors.Is(err, ErrAlreadyReported) {
		t.Fatalf("second open report: %v", err)
	}
	got, err := s.Review(ctx, r.ID)
	if err != nil || got.Version != 2 || got.Status != StatusPublished || got.OpenReports != 1 || got.PublishedAt == nil {
		t.Fatalf("review: %+v %v", got, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = $2`, r.ID, OutboxModerated).Scan(&n); err != nil || n != 1 {
		t.Fatalf("outbox: %d %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE target_type = 'PROVIDER_REVIEW' AND target_id = $1`,
		r.ID.String()).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit: %d %v", n, err)
	}

	// Lists, the queue and the ratings: published reviews only count.
	ratings, err := s.Ratings(ctx, []uuid.UUID{fx.profile})
	if err != nil || ratings[fx.profile] != (Rating{Count: 1, Sum: 4}) {
		t.Fatalf("ratings: %+v %v", ratings, err)
	}
	queue, total, err := s.Reviews(ctx, Filter{Queue: true, ProfileID: &fx.profile, Order: "queue", Limit: 10})
	if err != nil || total != 2 || len(queue) != 2 {
		t.Fatalf("queue: %d %v", total, err)
	}
	mine, total, err := s.Reviews(ctx, Filter{ReviewerKind: ReviewerCustomer, ReviewerUserID: &fx.reviewer, Limit: 10})
	if err != nil || total != 1 || mine[0].ID != r.ID {
		t.Fatalf("mine: %d %v", total, err)
	}
	byBasis, err := s.ByBasis(ctx, BasisEnquiry, []uuid.UUID{fx.enquiry})
	if err != nil || byBasis[fx.enquiry].ID != r.ID {
		t.Fatalf("by basis: %+v %v", byBasis, err)
	}

	// Moderation closes the reports; the reply round-trips.
	replied := got
	replied.Reply = &Reply{Text: "Thank you", At: now, UpdatedAt: now, ByUserID: &fx.moderator, ByName: "Owner"}
	if err := s.Apply(ctx, Change{Update: &replied, ExpectVersion: 2,
		Resolve: &ReportResolution{ReviewID: r.ID, At: now, By: fx.moderator, Resolution: ActionPublish}}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Review(ctx, r.ID)
	reports, _ := s.Reports(ctx, r.ID)
	if got.Reply == nil || got.Reply.Text != "Thank you" || got.OpenReports != 0 || len(reports) != 1 || reports[0].Status != ReportResolved ||
		reports[0].Resolution != ActionPublish {
		t.Fatalf("after moderation: %+v %+v", got, reports)
	}

	// Account deletion: kept, unlinked, "Former customer"; an
	// organisation's review keeps its name and loses its author's.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, fx.reviewer, "Deleted account"); err != nil {
		t.Fatal(err)
	}
	if err := ForgetUserTx(ctx, tx, fx.reporter, "Deleted account"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Review(ctx, r.ID)
	orgReview, _ := s.Review(ctx, org.ID)
	reports, _ = s.Reports(ctx, r.ID)
	if got.ReviewerUserID != nil || got.ReviewerName != FormerCustomer || orgReview.ReviewerUserID != nil ||
		orgReview.ReviewerName != "Reviews Estates" || orgReview.AuthorName != "Deleted account" || reports[0].ReporterUserID != nil {
		t.Fatalf("after deletion: %+v / %+v / %+v", got, orgReview, reports)
	}

	// Deleting a review removes its reports.
	if err := s.Apply(ctx, Change{Delete: &r.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Review(ctx, r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted review: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_review_reports WHERE review_id = $1`, r.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("reports left: %d %v", n, err)
	}
}
