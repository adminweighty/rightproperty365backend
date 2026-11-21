package app

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/dataexport"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// accountOnly is a memory collector: the account section only.
type accountOnly struct{ e *env }

func (c accountOnly) Collect(ctx context.Context, userID uuid.UUID) ([]dataexport.Section, error) {
	u, err := c.e.identity.UserByID(ctx, userID)
	if err != nil {
		return nil, err
	}
	return []dataexport.Section{{Name: "account", Rows: []map[string]any{{"id": u.ID, "email": u.Email, "full_name": u.Name}}}}, nil
}

// API 0.55.0: ask, build (the worker), download; one a day; admins refused.
func TestDataExport(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	me := c.as(identity.SeedConsumerEmail)

	wantError(t, e.post("/v1/me/data-export", nil), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.post("/v1/me/data-export", nil, e.asAdmin(identity.SeedAdminEmail)), http.StatusForbidden, dataexport.CodePlatformAccount)
	rec := e.get("/v1/me/data-export", me)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[obj](t, rec); got["export"] != nil {
		t.Fatalf("nothing yet: %v", got)
	}
	wantStatus(t, e.post("/v1/me/data-export", nil, me), http.StatusAccepted)
	wantError(t, e.post("/v1/me/data-export", nil, me), http.StatusConflict, dataexport.CodeRecent)
	wantError(t, e.get("/v1/me/data-export/download", me), http.StatusNotFound, dataexport.CodeNotReady)

	// The worker's run.
	worker := dataexport.NewService(e.dataExports, e.files, accountOnly{e: e}, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil)
	if n, err := worker.Sweep(context.Background()); err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	rec = e.get("/v1/me/data-export", me)
	if got := decode[obj](t, rec)["export"].(map[string]any); got["status"] != "READY" || got["expires_at"] == nil {
		t.Fatalf("ready: %v", got)
	}
	rec = e.get("/v1/me/data-export/download", me)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Content-Type") != "application/zip" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", rec.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil || len(zr.File) != 2 {
		t.Fatalf("archive: %v", err)
	}
	f, _ := zr.File[1].Open()
	body, _ := io.ReadAll(f)
	if !bytes.Contains(body, []byte(identity.SeedConsumerEmail)) {
		t.Fatalf("account.json: %s", body)
	}
	// Another person cannot download it.
	wantError(t, e.get("/v1/me/data-export/download", c.as(identity.SeedAgentEmail)), http.StatusNotFound, dataexport.CodeNotReady)
	if evs := e.dataExports.Outbox(); len(evs) != 1 || evs[0].Type != dataexport.EventReady {
		t.Fatalf("events: %+v", evs)
	}
}
