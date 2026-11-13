package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/httpx"
)

type fakeResolver map[string]*Principal

func (f fakeResolver) ResolveSession(_ context.Context, token string) (*Principal, error) {
	if token == "boom" {
		return nil, errors.New("store down")
	}
	p, ok := f[token]
	if !ok {
		return nil, ErrNoSession
	}
	cp := *p
	return &cp, nil
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env httpx.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return env.Error.Code
}

func TestAuthenticate(t *testing.T) {
	const origin = "http://localhost:3000"
	user := &Principal{UserID: uuid.New(), Name: "A", Email: "a@example.test", SessionScope: ScopeCustomer}
	admin := &Principal{UserID: uuid.New(), Name: "Admin", Email: "admin@example.test", SessionScope: ScopeAdmin,
		PlatformRoles: []PlatformRole{{Role: PlatformSuperAdmin}}}
	mw := NewMiddleware(fakeResolver{"good": user, "admin": admin}, []string{origin}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	tests := []struct {
		name       string
		method     string
		cookie     string
		bearer     string
		origin     string
		wantStatus int
		wantCode   string
		wantUser   bool
		wantVia    string
	}{
		{name: "anonymous", method: http.MethodGet, wantStatus: 200},
		{name: "cookie GET", method: http.MethodGet, cookie: "good", wantStatus: 200, wantUser: true, wantVia: ViaCookie},
		{name: "bearer GET", method: http.MethodGet, bearer: "good", wantStatus: 200, wantUser: true, wantVia: ViaBearer},
		{name: "unknown cookie is anonymous", method: http.MethodGet, cookie: "nope", wantStatus: 200},
		{name: "unknown bearer is anonymous", method: http.MethodPost, bearer: "nope", wantStatus: 200},
		{name: "invalid bearer does not fall back to cookie", method: http.MethodGet, bearer: "nope", cookie: "good", wantStatus: 200},
		{name: "cookie POST without Origin", method: http.MethodPost, cookie: "good", wantStatus: 403, wantCode: httpx.CodeCSRFOrigin},
		{name: "cookie POST with foreign Origin", method: http.MethodPost, cookie: "good", origin: "https://evil.example", wantStatus: 403, wantCode: httpx.CodeCSRFOrigin},
		{name: "cookie POST with allowed Origin", method: http.MethodPost, cookie: "good", origin: origin, wantStatus: 200, wantUser: true, wantVia: ViaCookie},
		{name: "cookie DELETE without Origin", method: http.MethodDelete, cookie: "good", wantStatus: 403, wantCode: httpx.CodeCSRFOrigin},
		{name: "bearer POST without Origin is exempt", method: http.MethodPost, bearer: "good", wantStatus: 200, wantUser: true, wantVia: ViaBearer},
		{name: "anonymous POST with unknown cookie is not a CSRF failure", method: http.MethodPost, cookie: "nope", wantStatus: 200},
		{name: "resolver failure", method: http.MethodGet, bearer: "boom", wantStatus: 500, wantCode: httpx.CodeInternal},
		{name: "admin session by cookie", method: http.MethodGet, cookie: "admin", wantStatus: 200, wantUser: true, wantVia: ViaCookie},
		{name: "admin session token as bearer does not authenticate", method: http.MethodGet, bearer: "admin", wantStatus: 200},
		{name: "admin session token as bearer POST does not authenticate", method: http.MethodPost, bearer: "admin", wantStatus: 200},
		{name: "admin cookie POST without Origin", method: http.MethodPost, cookie: "admin", wantStatus: 403, wantCode: httpx.CodeCSRFOrigin},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *Principal
			h := mw.Authenticate(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = FromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(tt.method, "/x", nil)
			if tt.cookie != "" {
				req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tt.cookie})
			}
			if tt.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tt.bearer)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantCode != "" && errorCode(t, rec) != tt.wantCode {
				t.Fatalf("code = %s, want %s", errorCode(t, rec), tt.wantCode)
			}
			if (got != nil) != tt.wantUser {
				t.Fatalf("principal present = %v, want %v", got != nil, tt.wantUser)
			}
			if got != nil && got.Via != tt.wantVia {
				t.Fatalf("via = %q, want %q", got.Via, tt.wantVia)
			}
		})
	}
}

func TestRequire(t *testing.T) {
	orgA, orgB := uuid.New(), uuid.New()
	consumer := &Principal{UserID: uuid.New(), SessionScope: ScopeCustomer}
	agent := &Principal{UserID: uuid.New(), SessionScope: ScopeCustomer, Memberships: []Membership{{OrganisationID: orgA, Role: RoleAgent}}}
	owner := &Principal{UserID: uuid.New(), SessionScope: ScopeCustomer, Memberships: []Membership{{OrganisationID: orgB, Role: RoleOwner}}}
	support := &Principal{UserID: uuid.New(), SessionScope: ScopeAdmin, PlatformRoles: []PlatformRole{{Role: PlatformSupport}}}
	super := &Principal{UserID: uuid.New(), SessionScope: ScopeAdmin, PlatformRoles: []PlatformRole{{Role: PlatformSuperAdmin}}}
	// Platform-role holders on a customer-scope (or scope-less) session.
	superCustomer := &Principal{UserID: uuid.New(), SessionScope: ScopeCustomer, PlatformRoles: []PlatformRole{{Role: PlatformSuperAdmin}}}
	supportNoScope := &Principal{UserID: uuid.New(), PlatformRoles: []PlatformRole{{Role: PlatformSupport}}}
	// An admin-scope session whose user holds no platform role.
	adminScopeNoRole := &Principal{UserID: uuid.New(), SessionScope: ScopeAdmin}

	tests := []struct {
		name       string
		mw         func(http.Handler) http.Handler
		principal  *Principal
		wantStatus int
		wantCode   string
	}{
		{"auth: anonymous", RequireAuth, nil, 401, httpx.CodeUnauthenticated},
		{"auth: consumer", RequireAuth, consumer, 200, ""},
		{"membership: anonymous", RequireMembership(), nil, 401, httpx.CodeUnauthenticated},
		{"membership: consumer", RequireMembership(), consumer, 403, httpx.CodeForbidden},
		{"membership: platform role is not a membership", RequireMembership(), super, 403, httpx.CodeForbidden},
		{"membership: any role", RequireMembership(), agent, 200, ""},
		{"membership: wrong role", RequireMembership(RoleOwner, RoleManager), agent, 403, httpx.CodeForbidden},
		{"membership: right role", RequireMembership(RoleOwner, RoleManager), owner, 200, ""},
		{"platform: anonymous", RequirePlatformRole(PlatformSupport), nil, 401, httpx.CodeUnauthenticated},
		{"platform: consumer", RequirePlatformRole(PlatformSupport), consumer, 403, httpx.CodeForbidden},
		{"platform: membership is not a platform role", RequirePlatformRole(PlatformSupport), owner, 403, httpx.CodeForbidden},
		{"platform: wrong role", RequirePlatformRole(PlatformFinanceAdmin), support, 403, httpx.CodeForbidden},
		{"platform: right role", RequirePlatformRole(PlatformFinanceAdmin, PlatformSupport), support, 200, ""},
		{"platform: super admin", RequirePlatformRole(PlatformFinanceAdmin), super, 200, ""},
		{"platform: any role list", RequirePlatformRole(AllPlatformRoles()...), support, 200, ""},
		{"platform: super admin on a customer session", RequirePlatformRole(AllPlatformRoles()...), superCustomer, 403, httpx.CodeForbidden},
		{"platform: role without a session scope", RequirePlatformRole(PlatformSupport), supportNoScope, 403, httpx.CodeForbidden},
		{"platform: admin scope without a role", RequirePlatformRole(AllPlatformRoles()...), adminScopeNoRole, 403, httpx.CodeForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := tt.mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if tt.principal != nil {
				req = req.WithContext(WithPrincipal(req.Context(), tt.principal))
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantCode != "" && errorCode(t, rec) != tt.wantCode {
				t.Fatalf("code = %s, want %s", errorCode(t, rec), tt.wantCode)
			}
		})
	}

	if got := agent.OrganisationIDs(RoleOwner); len(got) != 0 {
		t.Fatalf("agent must not be scoped as owner: %v", got)
	}
	if got := agent.OrganisationIDs(); len(got) != 1 || got[0] != orgA {
		t.Fatalf("OrganisationIDs() = %v, want [%s]", got, orgA)
	}
}
