// Package app wires the modules into the HTTP router. It lives outside
// cmd/api so the complete stack (middleware order, CORS, CSRF, rate limits,
// authorisation) can be exercised by tests exactly as it is served.
package app

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/storage"
)

const (
	maxBodyBytes   = 64 << 10 // 64 KB
	requestTimeout = 15 * time.Second

	// Per-IP limit on POST /v1/property-requests: bursts of 10, refilled at
	// 10 per minute. In-memory and per-process — see httpx.RateLimiter; it must
	// become a shared limiter before the API is autoscaled.
	propertyRequestsPerMinute = 10
	propertyRequestsBurst     = 10

	// Per-IP limit on POST /v1/business-enquiries, its own bucket: bursts of
	// 10, refilled at 10 per minute.
	businessEnquiriesPerMinute = 10
	businessEnquiriesBurst     = 10

	// Per-IP limit shared by every POST /v1/auth/* route (its own bucket,
	// separate from property requests): bursts of 20, refilled at 20 per minute.
	// This is the only brute-force protection today; there is no per-account
	// lockout yet.
	AuthRequestsPerMinute = 20
	AuthRequestsBurst     = 20

	// Per-IP limit on POST /v1/admin/auth/login, its own and much stricter
	// bucket: bursts of 5, refilled at 5 per minute.
	AdminAuthRequestsPerMinute = 5
	AdminAuthRequestsBurst     = 5

	// Per-IP limit on onboarding document uploads, its own bucket: bursts of
	// 20, refilled at 20 per minute. Each upload can be 10 MB.
	UploadRequestsPerMinute = 20
	UploadRequestsBurst     = 20
	// Uploads get longer than other requests: 10 MB over a slow connection.
	uploadTimeout = 2 * time.Minute
)

// uploadPath matches POST /v1/me/onboarding/{role}/documents, the one route
// with a larger body limit and a longer timeout.
var uploadPath = regexp.MustCompile(`^/v1/me/onboarding/[^/]+/documents$`)

func isUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && uploadPath.MatchString(r.URL.Path)
}

// Deps is everything the router needs.
type Deps struct {
	Config    config.Config
	Logger    *slog.Logger
	Health    *health.Handler
	Markets   markets.Store
	Requests  propertyrequests.Store
	Enquiries businessenquiries.Store
	Identity  identity.Store
	Reference reference.Store
	// Listings is the source of truth for listings; ListingSearch is the
	// search engine in front of it (the same adapter today).
	Listings      listings.Store
	ListingSearch listings.Search
	Directory     directory.Store
	Mailer        identity.Mailer
	// Onboarding holds the professional onboarding applications; Files is the
	// private object storage their documents are kept in.
	Onboarding onboarding.Store
	Files      storage.Storage

	// Optional overrides used by tests.
	Now    func() time.Time
	Hasher *identity.Hasher
}

// NewRouter builds the complete API handler.
func NewRouter(d Deps) (http.Handler, error) {
	cfg, logger := d.Config, d.Logger

	// Reference data: served to clients and used to validate what they send,
	// so drop-downs and validation can never disagree.
	referenceSvc := reference.NewService(d.Reference, func(ctx context.Context) ([]string, error) {
		all, err := d.Markets.List(ctx)
		if err != nil {
			return nil, err
		}
		codes := make([]string, 0, len(all))
		for _, m := range all {
			codes = append(codes, m.Code)
		}
		return codes, nil
	})
	vocab := vocabulary{ref: referenceSvc}
	referenceH := reference.NewHandler(referenceSvc, logger)

	// Public catalogue: listings and the directories. Read-only and cacheable.
	listingsSvc := listings.NewService(d.Listings, d.ListingSearch, referenceSvc)
	if d.Now != nil {
		listingsSvc.SetClock(d.Now)
	}
	listingsH := listings.NewHandler(listingsSvc, logger)
	directoryH := directory.NewHandler(directory.NewService(d.Directory, vocab, listingsSvc), logger)

	identityOpts := []identity.Option{
		identity.WithAdminSessionTTLs(cfg.AdminSessionTTL, cfg.AdminSessionIdleTTL),
		identity.WithRoleCatalogue(vocab),
	}
	requestOpts := []propertyrequests.Option{propertyrequests.WithVocabulary(vocab)}
	enquiryOpts := []businessenquiries.Option{businessenquiries.WithVocabulary(vocab)}
	if d.Now != nil {
		identityOpts = append(identityOpts, identity.WithClock(d.Now))
		requestOpts = append(requestOpts, propertyrequests.WithClock(d.Now))
		enquiryOpts = append(enquiryOpts, businessenquiries.WithClock(d.Now))
	}
	if d.Hasher != nil {
		identityOpts = append(identityOpts, identity.WithHasher(*d.Hasher))
	}
	identitySvc, err := identity.NewService(d.Identity, d.Mailer, logger, identityOpts...)
	if err != nil {
		return nil, err
	}

	secure := cfg.Env != config.EnvDevelopment
	identityH := identity.NewHandler(identitySvc, logger, secure)
	marketsH := markets.NewHandler(d.Markets, logger)
	requestsH := propertyrequests.NewHandler(propertyrequests.NewService(d.Requests, requestOpts...), logger)
	requestsH.SetListingNames(listingNames{svc: listingsSvc})
	enquiriesH := businessenquiries.NewHandler(businessenquiries.NewService(d.Enquiries, enquiryOpts...), logger)
	authn := auth.NewMiddleware(identitySvc, cfg.CORSAllowedOrigins, logger)

	onboardingSvc := onboarding.NewService(d.Onboarding, d.Files, onboardingVocabulary{ref: referenceSvc},
		agencyDirectory{directory: d.Directory, ref: referenceSvc, markets: d.Markets}, logger)
	if d.Now != nil {
		onboardingSvc.SetClock(d.Now)
	}
	onboardingH := onboarding.NewHandler(onboardingSvc, logger)
	uploadLimiter := httpx.NewRateLimiter(UploadRequestsPerMinute, UploadRequestsBurst)

	requestLimiter := httpx.NewRateLimiter(propertyRequestsPerMinute, propertyRequestsBurst)
	enquiryLimiter := httpx.NewRateLimiter(businessEnquiriesPerMinute, businessEnquiriesBurst)
	authLimiter := httpx.NewRateLimiter(AuthRequestsPerMinute, AuthRequestsBurst)
	adminAuthLimiter := httpx.NewRateLimiter(AdminAuthRequestsPerMinute, AdminAuthRequestsBurst)

	r := chi.NewRouter()
	r.Use(
		httpx.RequestID,
		httpx.AccessLog(logger),
		httpx.Recover(logger),
		httpx.SecurityHeaders(secure),
		httpx.CORS(cfg.CORSAllowedOrigins),
		httpx.BodyLimitFunc(func(r *http.Request) int64 {
			if isUpload(r) {
				return onboarding.MaxUploadBodyBytes
			}
			return maxBodyBytes
		}),
		func(next http.Handler) http.Handler {
			normal, upload := httpx.Timeout(requestTimeout)(next), httpx.Timeout(uploadTimeout)(next)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if isUpload(r) {
					upload.ServeHTTP(w, r)
					return
				}
				normal.ServeHTTP(w, r)
			})
		},
		// After CORS so preflights never touch the session store; resolves the
		// cookie/bearer session and enforces the CSRF origin rule for cookies.
		authn.Authenticate,
	)
	r.NotFound(httpx.NotFound)
	r.MethodNotAllowed(httpx.MethodNotAllowed)

	r.Get("/healthz", d.Health.Live)
	r.Get("/readyz", d.Health.Ready)
	r.Route("/v1", func(r chi.Router) {
		r.Get("/markets", marketsH.List)

		// Reference data behind every drop-down, filter and list. Public and cacheable.
		r.Get("/reference", referenceH.All)
		r.Get("/reference/{set}", referenceH.One)

		// Open to guests and signed-in customers alike.
		r.With(requestLimiter.Middleware).Post("/property-requests", requestsH.Create)

		// Public catalogue. These never read the session and are cacheable.
		r.Get("/listings", listingsH.List)
		r.Get("/listings/suggest", listingsH.Suggest)
		r.Get("/listings/{slug}", listingsH.Get)
		r.Get("/directory", directoryH.Kinds)
		r.Get("/directory/{kind}", directoryH.List)
		r.Get("/directory/{kind}/{slug}", directoryH.Get)
		r.Get("/sitemap", directoryH.Sitemap)

		// The only way to contact a directory profile. Open to guests and
		// signed-in customers alike.
		r.With(enquiryLimiter.Middleware).Post("/business-enquiries", enquiriesH.Create)

		// Enquiries received by the caller's organisation. Any membership may
		// enter; the handler scopes the data to the caller's organisations
		// and, for agents and staff, to their own agent profile.
		r.With(auth.RequireMembership()).Get("/inbox/business-enquiries", enquiriesH.Inbox)

		r.Route("/auth", func(r chi.Router) {
			r.Use(authLimiter.Middleware)
			r.Post("/register", identityH.Register)
			r.Post("/verify-email", identityH.VerifyEmail)
			r.Post("/resend-verification", identityH.ResendVerification)
			r.Post("/login", identityH.Login)
			r.Post("/logout", identityH.Logout)
			r.Post("/password-reset/request", identityH.RequestPasswordReset)
			r.Post("/password-reset/confirm", identityH.ConfirmPasswordReset)
		})

		r.With(auth.RequireAuth).Get("/me", identityH.Me)

		// Onboarding for professional roles. What each role has to provide is
		// public; an application is only ever its owner's.
		r.Get("/onboarding/requirements", onboardingH.Requirements)
		r.Route("/me/onboarding", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", onboardingH.Mine)
			r.Put("/{role}", onboardingH.Save)
			r.Post("/{role}/submit", onboardingH.Submit)
			r.With(uploadLimiter.Middleware).Post("/{role}/documents", onboardingH.Upload)
			r.Delete("/{role}/documents/{id}", onboardingH.DeleteDocument)
			r.Get("/{role}/documents/{id}/file", onboardingH.File)
		})

		// Platform administration. Signing in has its own endpoint and rate
		// limit; signing out is POST /v1/auth/logout like any other session.
		// Everything else under /admin requires an ADMIN-scope session whose
		// user holds a platform role.
		r.Route("/admin", func(r chi.Router) {
			r.With(adminAuthLimiter.Middleware).Post("/auth/login", identityH.AdminLogin)
			r.With(auth.RequireAuth, auth.RequirePlatformRole(auth.AllPlatformRoles()...)).Get("/me", identityH.Me)

			// Reviewing onboarding applications: an ADMIN-scope session with
			// SUPER_ADMIN, PLATFORM_ADMIN, MODERATOR or MARKET_ADMIN (the last
			// limited by the service to its own markets).
			r.Route("/onboarding", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(onboarding.AdminRoles()...))
				r.Get("/", onboardingH.AdminList)
				r.Get("/{id}", onboardingH.AdminGet)
				r.Get("/{id}/documents/{docId}/file", onboardingH.AdminFile)
				r.Post("/{id}/approve", onboardingH.AdminApprove)
				r.Post("/{id}/reject", onboardingH.AdminReject)
				r.Post("/{id}/request-changes", onboardingH.AdminRequestChanges)
			})
		})

		// Request inbox for organisation members and direct sellers. The
		// handler decides who may enter (membership, direct seller, or a
		// declared PRIVATE_SELLER) and scopes the data to the caller's own
		// organisations, role and listings.
		r.With(auth.RequireAuth).Get("/agent/property-requests", requestsH.Inbox)
	})
	return r, nil
}
