package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/i18n"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Middleware authenticates requests.
type Middleware struct {
	resolver SessionResolver
	origins  map[string]struct{}
	logger   *slog.Logger
}

// NewMiddleware builds the authentication middleware. allowedOrigins is the
// CORS allowlist; it doubles as the CSRF allowlist for cookie sessions.
func NewMiddleware(resolver SessionResolver, allowedOrigins []string, logger *slog.Logger) *Middleware {
	origins := make(map[string]struct{}, len(allowedOrigins))
	for _, o := range allowedOrigins {
		origins[o] = struct{}{}
	}
	return &Middleware{resolver: resolver, origins: origins, logger: logger}
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" header.
func BearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// Authenticate resolves the session token (Authorization bearer first, else
// the rp_session cookie) into a Principal stored in the request context. A
// missing, invalid, expired or revoked token simply leaves the request
// anonymous; routes decide with RequireAuth whether that is acceptable.
//
// CSRF: a request authenticated by the COOKIE with a method other than
// GET/HEAD/OPTIONS must carry an allowlisted Origin header, otherwise it is
// rejected with 403 CSRF_ORIGIN. Bearer requests are exempt because a browser
// never attaches that header on its own.
func (m *Middleware) Authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, via := "", ""
		if t, ok := BearerToken(r); ok {
			// An Authorization header always wins; the cookie is then ignored.
			token, via = t, ViaBearer
		} else if c, err := r.Cookie(SessionCookieName); err == nil && c.Value != "" {
			token, via = c.Value, ViaCookie
		}
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}

		p, err := m.resolver.ResolveSession(r.Context(), token)
		if errors.Is(err, ErrNoSession) {
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			httpx.WriteInternalError(w, r, m.logger, err)
			return
		}

		// Admin sessions exist only as cookies. The same token presented as a
		// bearer credential (which would skip the CSRF origin rule) is not a
		// valid login.
		if via == ViaBearer && p.SessionScope == ScopeAdmin {
			next.ServeHTTP(w, r)
			return
		}

		if via == ViaCookie && !safeMethod(r.Method) {
			if _, ok := m.origins[r.Header.Get("Origin")]; !ok {
				httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeCSRFOrigin, "This request must come from an allowed origin.", nil)
				return
			}
		}

		p.Via = via
		logging.SetUserID(r.Context(), p.UserID.String())
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

func safeMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func unauthenticated(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, i18n.T(r.Context(), "error.unauthenticated", nil), nil)
}

func forbidden(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, i18n.T(r.Context(), "error.forbidden", nil), nil)
}

// RequireAuth rejects anonymous requests with 401 UNAUTHENTICATED.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if FromContext(r.Context()) == nil {
			unauthenticated(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireMembership requires an organisation membership with one of roles
// (any membership when roles is empty): 401 when anonymous, 403 FORBIDDEN
// otherwise. It only answers "may this caller use the route at all"; the
// handler must still scope its data to Principal.OrganisationIDs.
func RequireMembership(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := FromContext(r.Context())
			switch {
			case p == nil:
				unauthenticated(w, r)
			case !p.HasMembership(roles...):
				forbidden(w, r)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// RequirePlatformRole requires an ADMIN-scope session whose user holds one of
// the given platform roles (SUPER_ADMIN always qualifies): 401 when anonymous,
// 403 FORBIDDEN otherwise — including for a platform-role holder signed in
// with a customer-scope session. For
// MARKET_ADMIN the handler must still compare PlatformRole.MarketCode with
// the market being acted on.
func RequirePlatformRole(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := FromContext(r.Context())
			switch {
			case p == nil:
				unauthenticated(w, r)
			case !p.HasPlatformRole(roles...):
				forbidden(w, r)
			default:
				next.ServeHTTP(w, r)
			}
		})
	}
}

// CodeMFAEnrolmentRequired is the error of an enrolment-only administrator
// session on any route other than signing in and setting up MFA.
const CodeMFAEnrolmentRequired = "MFA_ENROLMENT_REQUIRED"

// MFAEnrolmentPathPrefix is what an enrolment-only session may still use
// under /v1/admin: the sign-in steps and the MFA routes. Signing out is
// POST /v1/auth/logout, outside /v1/admin.
const MFAEnrolmentPathPrefix = "/v1/admin/auth/"

// RefuseMFAEnrolmentOnly answers 403 MFA_ENROLMENT_REQUIRED to an
// enrolment-only session (Principal.MFAEnrolmentOnly) on every path that does
// not start with MFAEnrolmentPathPrefix. It is mounted on /v1/admin.
func RefuseMFAEnrolmentOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p := FromContext(r.Context()); p != nil && p.MFAEnrolmentOnly && !strings.HasPrefix(r.URL.Path, MFAEnrolmentPathPrefix) {
			httpx.WriteError(w, r, http.StatusForbidden, CodeMFAEnrolmentRequired,
				"Set up two-step sign-in with an authenticator app to continue.", nil)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireAdminSession requires an admin-scope session of a platform-role
// holder, INCLUDING an enrolment-only one: it guards the MFA routes, which
// such a session must reach. 401 when anonymous, 403 FORBIDDEN otherwise.
func RequireAdminSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := FromContext(r.Context())
		switch {
		case p == nil:
			unauthenticated(w, r)
		case p.SessionScope != ScopeAdmin || len(p.PlatformRoles) == 0:
			forbidden(w, r)
		default:
			next.ServeHTTP(w, r)
		}
	})
}
