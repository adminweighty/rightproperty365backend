package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/metrics"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

// The queue gauges' SQL runs on a migrated Postgres (skipped without one).
func TestQueueGaugesOnPostgres(t *testing.T) {
	url := os.Getenv("RP365_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("RP365_TEST_DATABASE_URL is not set")
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
	defer pool.Close()
	// Every gauge that fails is logged; capture the log to catch bad SQL.
	var logs bytes.Buffer
	p := metrics.NewProcess(slog.New(slog.NewTextHandler(io.MultiWriter(&logs), nil)))
	RegisterQueueGauges(p, pool)
	var out bytes.Buffer
	if err := p.Write(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "could not be read") {
		t.Fatalf("a gauge failed: %s", logs.String())
	}
	for _, name := range []string{"rp365_outbox_pending", "rp365_outbox_oldest_pending_age_seconds", "rp365_outbox_dead_letters", "rp365_push_deliveries",
		"rp365_routing_exceptions_open", "rp365_email_delivery_events_last_hour", "rp365_email_accepted_last_hour", "rp365_upload_intents_open"} {
		if !strings.Contains(out.String(), "# TYPE "+name+" gauge") {
			t.Errorf("missing %s", name)
		}
	}
}

// Requests are counted by route pattern; the public API does not serve the
// metrics (they have their own listener).
func TestRequestMetrics(t *testing.T) {
	e := newEnv(t)
	wantStatus(t, e.get("/v1/markets"), 200)
	wantStatus(t, e.get("/v1/no-such-route"), 404)
	wantStatus(t, e.get("/metrics"), 404)
	var out bytes.Buffer
	if err := e.metrics.Write(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{`rp365_http_requests_total{method="GET",route="/v1/markets",status="2xx"} 1`, `route="unmatched",status="4xx"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(text, "no-such-route") {
		t.Fatal("a raw path became a label")
	}
}
