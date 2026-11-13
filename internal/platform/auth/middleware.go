package auth

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"rightproperty365.com/backend/internal/platform/httpx"
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
	httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
}

func forbidden(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
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

// RequirePlatformRole requires one of the given platform roles (SUPER_ADMIN
// always qualifies): 401 when anonymous, 403 FORBIDDEN otherwise. For
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
