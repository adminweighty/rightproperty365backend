package agreements

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

func TestPostgresStore(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	s := NewPostgresStore(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ref, err := s.NextReference(ctx, 2026)
	if err != nil {
		t.Fatal(err)
	}
	r1, r2 := uuid.New(), uuid.New()
	a := Agreement{ID: uuid.New(), Reference: ref, Name: "PG test", Status: StatusDraft, Source: SourceUpload, ExpiresAt: now.Add(DefaultExpiry),
		ReminderFrequency: ReminderNone, CreatedAt: now, UpdatedAt: now, Pages: []PageSize{{Width: 612, Height: 792}},
		Original: &Document{ObjectKey: "agreements/pgtest", SHA256: Checksum([]byte("o")), Size: 1, FileName: "o.pdf"},
		Recipients: []Recipient{{ID: r1, Order: 1, Role: RoleSigner, Name: "A", Email: "a@example.test", Status: RecipientWaiting},
			{ID: r2, Order: 2, Role: RoleCC, Name: "B", Email: "b@example.test", Status: RecipientWaiting}},
		Fields: []Field{{ID: uuid.New(), RecipientID: r1, Type: FieldSignature, Page: 1, X: 1, Y: 2, W: 30, H: 10, Required: true}}}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM agreements WHERE id = $1`, a.ID) })
	if err := s.CreateAgreement(ctx, a, []Event{{ID: uuid.New(), AgreementID: a.ID, Action: EventCreated, ActorType: ActorUser, At: now}}); err != nil {
		t.Fatal(err)
	}
	hash := []byte("0123456789abcdef0123456789abcdef")
	got, err := s.ChangeAgreement(ctx, a.ID, func(cur *Agreement) ([]Event, error) {
		cur.Status, cur.SentAt = StatusOutForSignature, &now
		cur.Recipients[0].Status, cur.Recipients[0].Signature = RecipientSigned, &Adopted{Kind: AdoptTyped, Text: "A", Font: "BOLD"}
		cur.Fields[0].Value = "x"
		cur.Links = append(cur.Links, Link{ID: uuid.New(), RecipientID: r1, TokenHash: hash, ExpiresAt: cur.ExpiresAt, CreatedAt: now})
		return []Event{{ID: uuid.New(), AgreementID: cur.ID, Action: EventSent, ActorType: ActorUser, At: now}}, nil
	})
	if err != nil || got.Recipients[0].Signature == nil {
		t.Fatalf("change: %v", err)
	}
	again, err := s.Agreement(ctx, a.ID)
	if err != nil || len(again.Recipients) != 2 || again.Recipients[0].Signature.Font != "BOLD" || again.Fields[0].Value != "x" || len(again.Links) != 1 {
		t.Fatalf("reload: %+v %v", again, err)
	}
	if _, id, err := s.LinkByToken(ctx, hash); err != nil || id != a.ID {
		t.Fatalf("link: %v", err)
	}
	if list, err := s.RecipientAgreements(ctx, "a@example.test"); err != nil || len(list) == 0 {
		t.Fatalf("recipient agreements: %v", err)
	}
	if first, _ := s.ClaimJob(ctx, "pgtest:"+a.ID.String(), now); !first {
		t.Fatal("first claim")
	}
	if again, _ := s.ClaimJob(ctx, "pgtest:"+a.ID.String(), now); again {
		t.Fatal("a job is claimed once")
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM agreement_jobs WHERE key = $1`, "pgtest:"+a.ID.String())
	})
	if _, err := pool.Exec(ctx, `UPDATE agreement_events SET action = 'SIGNED' WHERE agreement_id = $1`, a.ID); err == nil {
		t.Fatal("agreement events are immutable")
	}
	if _, err := s.DeleteAgreement(ctx, a.ID); !errors.Is(err, ErrState) {
		t.Fatalf("only a draft can be deleted: %v", err)
	}
}
