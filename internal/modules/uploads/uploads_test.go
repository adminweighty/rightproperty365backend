package uploads

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/malware"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/internal/platform/storage"
	"rightproperty365.com/backend/migrations"
)

func jpegBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type rig struct {
	svc     *Service
	files   storage.Storage
	scanner *malware.Fake
	now     time.Time
	user    *auth.Principal
	other   *auth.Principal
}

func newRig(t *testing.T, store Store) *rig {
	t.Helper()
	local, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{scanner: malware.NewFake(), now: time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC),
		user: &auth.Principal{UserID: uuid.New()}, other: &auth.Principal{UserID: uuid.New()}}
	r.files = malware.NewGuard(local, r.scanner, quiet())
	r.svc = NewService(Deps{Store: store, Storage: r.files, Logger: quiet(), Now: func() time.Time { return r.now }})
	return r
}

// upload creates an intent for content (declared as ct) and PUTs it through the API.
func (r *rig) upload(t *testing.T, content []byte, ct string) Intent {
	t.Helper()
	i, up, err := r.svc.Create(context.Background(), r.user, Input{Purpose: PurposeListingPhoto, ContentType: ct, SizeBytes: int64(len(content)), SHA256: sum(content)})
	if err != nil {
		t.Fatal(err)
	}
	if up.Direct || up.URL != "/v1/media/upload-intents/"+i.ID.String()+"/content" || up.Method != "PUT" || up.Headers["Content-Type"] != ct {
		t.Fatalf("upload %+v", up)
	}
	if err := r.svc.PutContent(context.Background(), r.user, i.ID, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	return i
}

func rejected(t *testing.T, err error, reason string) {
	t.Helper()
	var rej *RejectedError
	if !errors.As(err, &rej) || rej.Reason != reason {
		t.Fatalf("want rejection %s, got %v", reason, err)
	}
}

func TestServiceRules(t *testing.T) {
	ctx := context.Background()
	r := newRig(t, NewMemoryStore())
	photo := jpegBytes(t)

	var verr *ValidationError
	_, _, err := r.svc.Create(ctx, r.user, Input{Purpose: "AVATAR", SizeBytes: 1, SHA256: "x"})
	if !errors.As(err, &verr) || verr.Fields["purpose"] == "" || verr.Fields["sha256"] == "" {
		t.Fatalf("validation %v", err)
	}
	_, _, err = r.svc.Create(ctx, r.user, Input{Purpose: PurposeListingPhoto, ContentType: "application/pdf", SizeBytes: 16 << 20, SHA256: sum(photo)})
	if !errors.As(err, &verr) || verr.Fields["content_type"] == "" || verr.Fields["size_bytes"] == "" {
		t.Fatalf("type and size %v", err)
	}
	if _, _, err := r.svc.Create(ctx, nil, Input{}); !errors.Is(err, ErrNoUpload) {
		t.Fatal("anonymous")
	}

	// The happy path: READY, read once, used once, object gone.
	i := r.upload(t, photo, "image/jpeg")
	if _, err := r.svc.Complete(ctx, r.other, i.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("someone else's intent")
	}
	done, err := r.svc.Complete(ctx, r.user, i.ID)
	if err != nil || done.Status != StatusReady {
		t.Fatalf("complete %+v %v", done, err)
	}
	if again, err := r.svc.Complete(ctx, r.user, i.ID); err != nil || again.Status != StatusReady {
		t.Fatal("completing twice")
	}
	got, err := r.svc.Read(ctx, r.user, i.ID, PurposeListingPhoto)
	if err != nil || !bytes.Equal(got, photo) {
		t.Fatalf("read %v", err)
	}
	if err := r.svc.Consume(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Consume(ctx, i.ID); !errors.Is(err, ErrState) {
		t.Fatal("consumed twice")
	}
	if _, err := r.files.Open(ctx, i.ObjectKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("the used object is still stored")
	}

	// Wrong size through the API.
	j, _, _ := r.svc.Create(ctx, r.user, Input{Purpose: PurposeListingPhoto, ContentType: "image/jpeg", SizeBytes: int64(len(photo)), SHA256: sum(photo)})
	if err := r.svc.PutContent(ctx, r.user, j.ID, bytes.NewReader(photo[:10])); !errors.As(err, &verr) {
		t.Fatalf("short body %v", err)
	}
	// Not uploaded yet: completing says so and changes nothing; after the window, rejected.
	_, err = r.svc.Complete(ctx, r.user, j.ID)
	rejected(t, err, ReasonMissing)
	if cur, _ := r.svc.Get(ctx, r.user, j.ID); cur.Status != StatusPendingUpload {
		t.Fatal("an early complete rejected the intent")
	}
	r.now = r.now.Add(UploadWindow + time.Second)
	if err := r.svc.PutContent(ctx, r.user, j.ID, bytes.NewReader(photo)); !errors.Is(err, ErrExpired) {
		t.Fatal("late PUT")
	}
	_, err = r.svc.Complete(ctx, r.user, j.ID)
	rejected(t, err, ReasonMissing)
	r.now = r.now.Add(-UploadWindow - time.Second)

	// A PNG declared as a JPEG (checked from the content), a bad checksum,
	// and an infected file: rejected, and the object deleted.
	p := pngBytes(t)
	k := r.upload(t, p, "image/jpeg")
	_, err = r.svc.Complete(ctx, r.user, k.ID)
	rejected(t, err, ReasonType)
	if _, err := r.files.Open(ctx, k.ObjectKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("a rejected object is still stored")
	}
	l, _, _ := r.svc.Create(ctx, r.user, Input{Purpose: PurposeListingPhoto, ContentType: "image/jpeg", SizeBytes: int64(len(photo)), SHA256: strings.Repeat("0", 64)})
	_ = r.svc.PutContent(ctx, r.user, l.ID, bytes.NewReader(photo))
	_, err = r.svc.Complete(ctx, r.user, l.ID)
	rejected(t, err, ReasonChecksum)
	infected := append(append([]byte{}, photo...), malware.EICAR()...)
	m := r.upload(t, infected, "image/jpeg")
	_, err = r.svc.Complete(ctx, r.user, m.ID)
	rejected(t, err, ReasonInfected)
	if _, err := r.files.Open(ctx, m.ObjectKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("an infected object is still stored")
	}
	if _, err := r.svc.Read(ctx, r.user, m.ID, PurposeListingPhoto); !errors.Is(err, ErrState) {
		t.Fatal("an infected upload can be read")
	}

	// The scanner down: nothing changes, try again later.
	n := r.upload(t, photo, "image/jpeg")
	r.scanner.SetDown(true)
	if _, err := r.svc.Complete(ctx, r.user, n.ID); err == nil {
		t.Fatal("completed with the scanner down")
	}
	if cur, _ := r.svc.Get(ctx, r.user, n.ID); cur.Status != StatusPendingUpload {
		t.Fatalf("status %s", cur.Status)
	}
	r.scanner.SetDown(false)
	if _, err := r.svc.Complete(ctx, r.user, n.ID); err != nil {
		t.Fatal(err)
	}

	// Orphans: a day later the unused ones are deleted, objects too.
	r.now = r.now.Add(Lifetime + time.Minute)
	expired, err := r.svc.Sweep(ctx)
	if err != nil || expired != 5 { // j, k, l, m, n (i was used)
		t.Fatalf("sweep %d %v", expired, err)
	}
	if _, err := r.files.Open(ctx, n.ObjectKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("an orphan object is still stored")
	}
	if again, _ := r.svc.Sweep(ctx); again != 0 {
		t.Fatal("swept twice")
	}
}

func TestPresignedPut(t *testing.T) {
	key := func() string {
		b := make([]byte, 12)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
	s3, err := storage.NewS3(storage.S3Config{Endpoint: "https://s3.eu-north-1.amazonaws.com", Bucket: "test-bucket", AccessKey: key(), SecretKey: key(), Region: "eu-north-1"})
	if err != nil {
		t.Fatal(err)
	}
	guarded := malware.NewGuard(s3, malware.NewFake(), quiet())
	raw, ok, err := storage.PresignPut(context.Background(), guarded, "intents/abcdef", UploadWindow)
	if err != nil || !ok {
		t.Fatalf("presign %v %v", ok, err)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Path, "/intents/abcdef") || u.Query().Get("X-Amz-Expires") != "600" || u.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("url %s", raw)
	}
	local, _ := storage.NewLocal(t.TempDir())
	if _, ok, _ := storage.PresignPut(context.Background(), local, "intents/abcdef", time.Minute); ok {
		t.Fatal("local storage presigned")
	}
}

func openTestDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RP365_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set: the Postgres store is only exercised against a throwaway database")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
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
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The same rules on Postgres (fresh users per run, so reruns do not collide).
func TestPostgresServiceRules(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	store := NewPostgresStore(pool)
	r := newRig(t, store)
	// The sweep must only see this run's intents.
	r.now = time.Now().UTC().Truncate(time.Second)
	for _, p := range []*auth.Principal{r.user, r.other} {
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at, password_hash) VALUES ($1, $2, 'Upload Tester', 'ACTIVE', now(), 'x')`,
			p.UserID, "uploads."+p.UserID.String()[:8]+"@example.test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE upload_intents SET status = 'EXPIRED' WHERE status IN ('PENDING_UPLOAD', 'READY', 'REJECTED')`); err != nil {
		t.Fatal(err)
	}
	photo := jpegBytes(t)
	i := r.upload(t, photo, "image/jpeg")
	if done, err := r.svc.Complete(ctx, r.user, i.ID); err != nil || done.Status != StatusReady || done.CompletedAt == nil {
		t.Fatalf("complete %+v %v", done, err)
	}
	if _, err := r.svc.Get(ctx, r.other, i.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("someone else's")
	}
	if _, err := r.svc.Read(ctx, r.user, i.ID, PurposeListingPhoto); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Consume(ctx, i.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, i.ID)
	if got.Status != StatusUsed || got.UsedAt == nil {
		t.Fatalf("used %+v", got)
	}
	k := r.upload(t, pngBytes(t), "image/jpeg")
	_, err := r.svc.Complete(ctx, r.user, k.ID)
	rejected(t, err, ReasonType)
	if got, _ := store.Get(ctx, k.ID); got.Status != StatusRejected || got.Reason != ReasonType {
		t.Fatalf("rejected %+v", got)
	}
	if n, err := store.OpenCount(ctx, r.user.UserID, r.now); err != nil || n != 0 {
		t.Fatalf("open %d %v", n, err)
	}
	r.now = r.now.Add(Lifetime + time.Minute)
	if n, err := r.svc.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("sweep %d %v", n, err)
	}
}
