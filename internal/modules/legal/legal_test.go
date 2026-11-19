package legal

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/migrate"
	"rightproperty365.com/backend/migrations"
)

var t0 = time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)

func ver(kind, market, lang, label, status string, from time.Time) Version {
	return Version{ID: uuid.New(), Kind: kind, MarketCode: market, Lang: lang, Label: label, Status: status, EffectiveFrom: from,
		Body: "x", CreatedByName: "t", CreatedAt: from, UpdatedAt: from}
}

func TestResolveFallsBackToGlobalAndEnglish(t *testing.T) {
	day := 24 * time.Hour
	list := []Version{
		ver(KindTerms, MarketGlobal, "en", "g1", StatusPublished, t0.Add(-10*day)),
		ver(KindTerms, MarketGlobal, "en", "g2", StatusPublished, t0.Add(-day)),
		ver(KindTerms, MarketGlobal, "en", "g3-future", StatusPublished, t0.Add(day)),
		ver(KindTerms, MarketGlobal, "en", "g4-draft", StatusDraft, t0.Add(-day/2)),
		ver(KindTerms, "GB", "en", "gb1", StatusPublished, t0.Add(-5*day)),
		ver(KindTerms, "ZW", "en", "zw-draft", StatusDraft, t0.Add(-day)),
		ver(KindTerms, "KE", "en", "ke-retired", StatusRetired, t0.Add(-day)),
		ver(KindPrivacy, "ZW", "en", "p1", StatusPublished, t0.Add(-day)),
	}
	cases := []struct {
		market, lang, want string
		fbMarket, fbLang   bool
	}{
		{"GB", "en", "gb1", false, false},       // the market's own, even if GLOBAL has a newer one
		{"ZW", "en", "g2", true, false},         // a DRAFT is never current: GLOBAL
		{"KE", "en", "g2", true, false},         // nor is a RETIRED one
		{"", "en", "g2", false, false},          // no market: GLOBAL; the future and the draft do not count
		{MarketGlobal, "fr", "g2", false, true}, // a missing language: English
		{"GB", "fr", "gb1", false, true},
	}
	for _, c := range cases {
		v, res, ok := Resolve(list, KindTerms, c.market, c.lang, t0)
		if !ok || v.Label != c.want || res.FallbackMarket != c.fbMarket || res.FallbackLang != c.fbLang {
			t.Fatalf("Resolve(%s, %s) = %s %+v %v; want %s", c.market, c.lang, v.Label, res, ok, c.want)
		}
	}
	// The future version becomes current when its time comes.
	if v, _, _ := Resolve(list, KindTerms, MarketGlobal, "en", t0.Add(2*day)); v.Label != "g3-future" {
		t.Fatalf("scheduled: %s", v.Label)
	}
	if _, _, ok := Resolve(list, KindCookies, "ZW", "en", t0); ok {
		t.Fatal("a kind with no version resolves to nothing")
	}
}

func TestFrozenChange(t *testing.T) {
	pub := ver(KindTerms, MarketGlobal, "en", "v1", StatusPublished, t0)
	at := t0
	pub.PublishedAt, pub.PublishedByName = &at, "x"
	admin := uuid.New()
	pub.CreatedBy = &admin

	for name, mutate := range map[string]func(v *Version){
		"body":            func(v *Version) { v.Body = "changed" },
		"label":           func(v *Version) { v.Label = "v2" },
		"effective":       func(v *Version) { v.EffectiveFrom = v.EffectiveFrom.Add(time.Hour) },
		"reacceptance":    func(v *Version) { v.RequiresReacceptance = true },
		"draft notice":    func(v *Version) { v.DraftNotice = !v.DraftNotice },
		"back to draft":   func(v *Version) { v.Status = StatusDraft },
		"relink creator":  func(v *Version) { other := uuid.New(); v.CreatedBy = &other },
		"published again": func(v *Version) { later := t0.Add(time.Hour); v.PublishedAt = &later },
	} {
		next := pub.Clone()
		mutate(&next)
		if !FrozenChange(pub, next) {
			t.Fatalf("%s: a published version must not change", name)
		}
	}
	// Allowed: retiring, and unlinking a deleted account.
	next := pub.Clone()
	next.Status, next.RetiredAt = StatusRetired, &at
	if FrozenChange(pub, next) {
		t.Fatal("retiring is allowed")
	}
	next = pub.Clone()
	next.CreatedBy, next.CreatedByName = nil, "Deleted user"
	if FrozenChange(pub, next) {
		t.Fatal("unlinking is allowed")
	}
	// A retired version cannot even be "retired again" with a new time.
	retired := pub.Clone()
	retired.Status, retired.RetiredAt = StatusRetired, &at
	again := retired.Clone()
	later := t0.Add(time.Hour)
	again.RetiredAt = &later
	if !FrozenChange(retired, again) {
		t.Fatal("a retired version is frozen")
	}
	draft := ver(KindTerms, MarketGlobal, "en", "d", StatusDraft, t0)
	changed := draft.Clone()
	changed.Body = "anything"
	if FrozenChange(draft, changed) {
		t.Fatal("a draft may change")
	}
}

// Migration 0045 inserts no document: production has nothing published.
// The development seed is clearly a draft everywhere.
func TestMigrationSeedsNothingAndSeedIsDraft(t *testing.T) {
	raw, err := migrations.FS.ReadFile("0045_markets_legal_tax.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "INSERT INTO legal_document_versions") || strings.Contains(string(raw), "INSERT INTO tax_rules") {
		t.Fatal("migration 0045 must not insert legal documents or tax rules")
	}
	seeds := SeedVersions(t0)
	published := 0
	for _, v := range seeds {
		if !strings.HasPrefix(v.Body, DraftBanner) || !v.DraftNotice || !strings.Contains(v.Body, "not legal advice") {
			t.Fatalf("%s %s is not clearly a draft: %q", v.Kind, v.MarketCode, v.Body)
		}
		if v.Status == StatusPublished {
			published++
			if v.MarketCode != MarketGlobal {
				t.Fatal("only GLOBAL is published")
			}
		}
	}
	if published != 4 || len(seeds) != 12 {
		t.Fatalf("seeds: %d published of %d", published, len(seeds))
	}
}

// With nothing published (production after the migration), the public
// routes answer 404 LEGAL_DOCUMENT_NOT_FOUND or an empty list, and nothing
// is pending.
func TestNothingPublished(t *testing.T) {
	s := NewService(Deps{Store: NewMemoryStore(nil, nil), Acceptances: &memAcceptances{}, Now: func() time.Time { return t0 }})
	h := NewHandler(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r := chi.NewRouter()
	r.Get("/v1/legal", h.List)
	r.Get("/v1/legal/{kind}", h.Get)
	r.Get("/v1/legal/{kind}/versions", h.Versions)
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	for _, path := range []string{"/v1/legal/terms", "/v1/legal/privacy?market=ZW", "/v1/legal/terms?version=" + SeedVersion} {
		if rec := get(path); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), CodeDocumentNotFound) {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := get("/v1/legal?market=GB"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"documents":[]`) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if rec := get("/v1/legal/terms/versions"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"versions":[]`) {
		t.Fatalf("history: %d %s", rec.Code, rec.Body)
	}
	if p, err := s.Pending(context.Background(), uuid.New(), MarketGlobal); err != nil || p == nil || len(p) != 0 {
		t.Fatalf("pending: %v %v", p, err)
	}
}

func TestMayManage(t *testing.T) {
	admin := func(roles ...auth.PlatformRole) *auth.Principal {
		return &auth.Principal{UserID: uuid.New(), SessionScope: auth.ScopeAdmin, PlatformRoles: roles}
	}
	content := admin(auth.PlatformRole{Role: auth.PlatformContentAdmin})
	market := admin(auth.PlatformRole{Role: auth.PlatformMarketAdmin, MarketCode: "ZW"})
	finance := admin(auth.PlatformRole{Role: auth.PlatformFinanceAdmin})
	switch {
	case !MayManage(content, MarketGlobal) || !MayManage(content, "GB"):
		t.Fatal("content admin manages every market")
	case !MayManage(market, "ZW") || MayManage(market, "GB") || MayManage(market, MarketGlobal):
		t.Fatal("market admin manages its own market only, never GLOBAL")
	case MayManage(finance, "ZW") || MayManage(nil, "ZW"):
		t.Fatal("others manage nothing")
	}
	customer := &auth.Principal{UserID: uuid.New(), SessionScope: auth.ScopeCustomer, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformContentAdmin}}}
	if MayManage(customer, "ZW") {
		t.Fatal("a platform role on a customer session grants nothing")
	}
}

// ---------------------------------------------------------------------------
// Store contract: the memory store and (behind RP365_TEST_DATABASE_URL) Postgres.
// ---------------------------------------------------------------------------

type memAcceptances struct{ list []Acceptance }

func (m *memAcceptances) Acceptances(_ context.Context, userID uuid.UUID) ([]Acceptance, error) {
	out := []Acceptance{}
	for i := len(m.list) - 1; i >= 0; i-- {
		if m.list[i].UserID == userID {
			out = append(out, m.list[i])
		}
	}
	return out, nil
}

func (m *memAcceptances) Accept(_ context.Context, a Acceptance) error {
	m.list = append(m.list, a)
	return nil
}

type rig struct {
	store   Store
	accepts AcceptanceStore
	admin   *auth.Principal
	user    uuid.UUID
	now     time.Time
	forget  func(userID uuid.UUID)
	audits  func() int
}

func (r *rig) svc() *Service {
	return NewService(Deps{Store: r.store, Acceptances: r.accepts, Now: func() time.Time { return r.now },
		KnownMarket: func(_ context.Context, code string) (bool, error) { return code == "ZW" || code == "GB", nil }})
}

func ptr[T any](v T) *T { return &v }

// storeContract exercises the rules every store must keep. Labels are
// unique per run so a shared Postgres database can be reused.
func storeContract(t *testing.T, r *rig) {
	t.Helper()
	ctx := context.Background()
	s := r.svc()
	label := "t-" + uuid.NewString()[:8]
	from := r.now.Add(-time.Hour)

	// Create a draft; drafts are not public.
	v, err := s.Create(ctx, r.admin, VersionInput{Kind: ptr("cookies"), MarketCode: ptr("zw"), Version: ptr(label),
		EffectiveFrom: &from, Body: ptr("# Cookies\n\nDRAFT"), RequiresReacceptance: ptr(true)})
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != StatusDraft || v.MarketCode != "ZW" || !v.DraftNotice || v.Kind != KindCookies {
		t.Fatalf("created: %+v", v)
	}
	if _, err := s.Create(ctx, r.admin, VersionInput{Kind: ptr("COOKIES"), MarketCode: ptr("ZW"), Version: ptr(label),
		EffectiveFrom: &from, Body: ptr("dup")}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate label: %v", err)
	}
	if res, err := s.Current(ctx, KindCookies, "ZW", "en"); err == nil && res.Version.ID == v.ID {
		t.Fatal("a draft must not be current")
	}
	if _, err := s.Specific(ctx, KindCookies, "ZW", "en", label); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a draft is not public by label: %v", err)
	}
	if _, _, err := s.Accept(ctx, r.user, AcceptInput{Kind: KindCookies, Version: label, MarketCode: ptr("ZW")}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a draft cannot be accepted: %v", err)
	}

	// Edit, publish; the published version is current and frozen.
	if v, err = s.Update(ctx, r.admin, v.ID, VersionInput{Body: ptr("# Cookies\n\nReviewed"), Summary: ptr("First")}); err != nil {
		t.Fatal(err)
	}
	if v, err = s.Publish(ctx, r.admin, v.ID); err != nil || v.Status != StatusPublished || v.PublishedAt == nil {
		t.Fatalf("publish: %+v %v", v, err)
	}
	res, err := s.Current(ctx, KindCookies, "ZW", "en")
	if err != nil || res.Version.ID != v.ID || res.FallbackMarket {
		t.Fatalf("current: %+v %v", res, err)
	}
	if _, err := s.Update(ctx, r.admin, v.ID, VersionInput{Body: ptr("sneaky")}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("published is immutable: %v", err)
	}
	if _, err := s.Publish(ctx, r.admin, v.ID); !errors.Is(err, ErrImmutable) {
		t.Fatalf("published twice: %v", err)
	}
	if err := s.Delete(ctx, r.admin, v.ID); !errors.Is(err, ErrImmutable) {
		t.Fatalf("published deleted: %v", err)
	}
	// The store itself refuses a frozen change even if the service is bypassed.
	if _, err := r.store.Change(ctx, v.ID, func(x *Version) (adminaudit.Event, error) {
		x.Body = "bypass\n"
		return adminaudit.New(uuid.Nil, "t", ActionVersionUpdated, TargetVersion, x.ID.String(), "x", "", nil, nil, r.now), nil
	}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("store accepted a frozen change: %v", err)
	}
	got, err := s.Get(ctx, v.ID)
	if err != nil || !strings.Contains(got.Body, "Reviewed") {
		t.Fatalf("body after refused change: %q %v", got.Body, err)
	}

	// Re-acceptance: pending until accepted (idempotent).
	pending, err := s.Pending(ctx, r.user, "ZW")
	if err != nil || !hasVersion(pending, v.ID) {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	a, created, err := s.Accept(ctx, r.user, AcceptInput{Kind: "cookies", Version: label, MarketCode: ptr("ZW")})
	if err != nil || !created || a.Kind != KindCookies || a.MarketCode == nil || *a.MarketCode != "ZW" || *a.Context != ContextReacceptance {
		t.Fatalf("accept: %+v %v %v", a, created, err)
	}
	if _, created, err = s.Accept(ctx, r.user, AcceptInput{Kind: KindCookies, Version: label, MarketCode: ptr("ZW")}); err != nil || created {
		t.Fatalf("accept again: %v %v", created, err)
	}
	if pending, _ = s.Pending(ctx, r.user, "ZW"); hasVersion(pending, v.ID) {
		t.Fatal("still pending after acceptance")
	}

	// Retire: no longer current, still in the history, cannot be accepted.
	if v, err = s.Retire(ctx, r.admin, v.ID); err != nil || v.Status != StatusRetired || v.RetiredAt == nil {
		t.Fatalf("retire: %+v %v", v, err)
	}
	if res, err := s.Current(ctx, KindCookies, "ZW", "en"); err == nil && res.Version.ID == v.ID {
		t.Fatal("a retired version is not current")
	}
	hist, _, err := s.History(ctx, KindCookies, "ZW", "en")
	if err != nil || !hasVersion(hist, v.ID) {
		t.Fatalf("history: %v", err)
	}
	if _, _, err := s.Accept(ctx, r.user, AcceptInput{Kind: KindCookies, Version: label, MarketCode: ptr("ZW")}); !errors.Is(err, ErrNotAcceptable) {
		// Already accepted before it retired, but a retired version is never acceptable.
		t.Fatalf("retired accepted: %v", err)
	}
	if _, err := s.Retire(ctx, r.admin, v.ID); !errors.Is(err, ErrImmutable) {
		t.Fatalf("retired twice: %v", err)
	}

	// A draft can be deleted.
	d, err := s.Create(ctx, r.admin, VersionInput{Kind: ptr(KindAgentTerms), MarketCode: ptr("GB"), Version: ptr(label), EffectiveFrom: &from, Body: ptr("x")})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, r.admin, d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}

	// Account deletion unlinks the administrator; the version is kept.
	r.forget(r.admin.UserID)
	got, err = s.Get(ctx, v.ID)
	if err != nil || got.CreatedBy != nil || got.PublishedBy != nil || got.CreatedByName != "Deleted user" || got.PublishedByName != "Deleted user" {
		t.Fatalf("forgotten: %+v %v", got, err)
	}
	if r.audits() < 6 {
		t.Fatalf("audit records: %d", r.audits())
	}
}

func hasVersion(list []Version, id uuid.UUID) bool {
	for _, v := range list {
		if v.ID == id {
			return true
		}
	}
	return false
}

func contentAdmin(id uuid.UUID) *auth.Principal {
	return &auth.Principal{UserID: id, Name: "Connor Content", SessionScope: auth.ScopeAdmin,
		PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformContentAdmin}}}
}

func TestStoreContractMemory(t *testing.T) {
	log := &adminaudit.MemoryLog{}
	mem := NewMemoryStore(SeedVersions(t0), log)
	storeContract(t, &rig{store: mem, accepts: &memAcceptances{}, admin: contentAdmin(uuid.New()), user: uuid.New(), now: t0,
		forget: func(id uuid.UUID) { mem.ForgetUser(id, "Deleted user") },
		audits: func() int { return len(log.All()) }})
}

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

func TestStoreContractPostgres(t *testing.T) {
	pool := openTestDatabase(t)
	ctx := context.Background()
	// Every row a foreign key names: the administrator (created_by,
	// published_by, the audit actor) and the person accepting (user_consents).
	insertUser := func(name string) uuid.UUID {
		id := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO users (id, email, full_name, status, email_verified_at) VALUES ($1, $2, $3, 'ACTIVE', now())`,
			id, "legal."+id.String()[:8]+"@example.test", name); err != nil {
			t.Fatal(err)
		}
		return id
	}
	admin, user := insertUser("Legal Admin"), insertUser("Legal Reader")
	pg := NewPostgresStore(pool)
	countAudits := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM admin_audit_events WHERE actor_user_id = $1 OR actor_name = 'Connor Content'`, admin).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	storeContract(t, &rig{store: pg, accepts: pg, admin: contentAdmin(admin), user: user, now: time.Now().UTC(),
		forget: func(id uuid.UUID) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := ForgetUserTx(ctx, tx, id, "Deleted user"); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
		},
		audits: countAudits})

	// The trigger refuses a direct UPDATE or DELETE of a published row.
	s := NewService(Deps{Store: pg, Acceptances: pg})
	from := time.Now().Add(-time.Minute)
	v, err := s.Create(ctx, contentAdmin(admin), VersionInput{Kind: ptr(KindEULA), MarketCode: ptr(MarketGlobal),
		Version: ptr("trigger-" + uuid.NewString()[:8]), EffectiveFrom: &from, Body: ptr("x")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(ctx, contentAdmin(admin), v.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE legal_document_versions SET body_md = 'x2' WHERE id = $1`, v.ID); err == nil {
		t.Fatal("the database accepted a change to a published version")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM legal_document_versions WHERE id = $1`, v.ID); err == nil {
		t.Fatal("the database deleted a published version")
	}
	if _, err := s.Retire(ctx, contentAdmin(admin), v.ID); err != nil {
		t.Fatal(err)
	}
	// The old privacy-notice consent still works; a TERMS one is now allowed.
	if _, err := pool.Exec(ctx, `INSERT INTO user_consents (id, user_id, consent_type, version, accepted_at) VALUES ($1, $2, 'TERMS', 'x', now())`,
		uuid.New(), user); err != nil {
		t.Fatal(err)
	}
	list, err := pg.Acceptances(ctx, user)
	if err != nil || len(list) < 2 {
		t.Fatalf("acceptances: %+v %v", list, err)
	}
}
