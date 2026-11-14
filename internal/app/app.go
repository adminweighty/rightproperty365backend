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

	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/dashboard"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/trust"
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

	// Per-IP limit on POST /v1/appointments, its own bucket: bursts of 10,
	// refilled at 10 per minute.
	appointmentsPerMinute = 10
	appointmentsBurst     = 10

	// Per-IP limit on changes to an availability calendar and to stay rules
	// (PUT / PATCH under /v1/me/listings), its own bucket: bursts of 30,
	// refilled at 30 per minute.
	calendarChangesPerMinute = 30
	calendarChangesBurst     = 30

	// Per-IP limit on working a property request (PATCH the stage, POST a
	// note) and on resolving a routing exception, its own bucket: bursts of
	// 60, refilled at 60 per minute.
	requestWorkPerMinute = 60
	requestWorkBurst     = 60

	// Per-IP limit shared by every POST /v1/auth/* route (its own bucket,
	// separate from property requests): bursts of 20, refilled at 20 per minute.
	// This is the only brute-force protection today; there is no per-account
	// lockout yet.
	AuthRequestsPerMinute = 20
	AuthRequestsBurst     = 20

	// Per-IP limit on GET /v1/auth/captcha, its own bucket: bursts of 30,
	// refilled at 30 per minute (one question per form load and one after
	// each failed submit).
	CaptchaRequestsPerMinute = 30
	CaptchaRequestsBurst     = 30

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

// uploadPath matches POST /v1/me/onboarding/{role}/documents and logoPath
// PUT /v1/me/profiles/{kind}/{slug}/logo: the two routes with a larger body
// limit and a longer timeout.
var (
	uploadPath = regexp.MustCompile(`^/v1/me/onboarding/[^/]+/documents$`)
	logoPath   = regexp.MustCompile(`^/v1/me/profiles/[^/]+/[^/]+/logo$`)
)

func isLogoUpload(r *http.Request) bool {
	return r.Method == http.MethodPut && logoPath.MatchString(r.URL.Path)
}

func isUpload(r *http.Request) bool {
	return (r.Method == http.MethodPost && uploadPath.MatchString(r.URL.Path)) || isLogoUpload(r)
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
	// Appointments holds viewings and consultations. The store behind
	// Requests must have been given its appointment writer (see cmd/api), so
	// that a request and its appointment are written together.
	Appointments appointments.Store
	// Availability holds the calendars of the short stays.
	Availability availability.Store

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
	// The location hierarchy (country > region > city > suburb) behind the
	// country and region filters, facets and suggestions.
	geography := places{ref: referenceSvc, markets: d.Markets}
	listingsSvc.SetPlaces(geography)
	// The availability calendar of the short stays: behind a listing's
	// `availability`, the check_in / check_out search and requests to stay.
	availabilitySvc := availability.NewService(d.Availability, listingsSvc, countryZones{markets: d.Markets})
	if d.Now != nil {
		availabilitySvc.SetClock(d.Now)
	}
	listingsSvc.SetCalendar(availabilitySvc)
	availabilityH := availability.NewHandler(availabilitySvc, logger)
	calendarLimiter := httpx.NewRateLimiter(calendarChangesPerMinute, calendarChangesBurst)
	listingsH := listings.NewHandler(listingsSvc, logger)
	directorySvc := directory.NewService(d.Directory, vocab, listingsSvc)
	directorySvc.SetPlaces(geography)
	// Logos are kept in the object storage and served by the API itself.
	directorySvc.SetFiles(d.Files, logger)
	if d.Now != nil {
		directorySvc.SetClock(d.Now)
	}
	directoryH := directory.NewHandler(directorySvc, logger)
	// The numbers behind "every listing is from a verified advertiser".
	trustH := trust.NewHandler(trust.Sources{
		Listings: listingsSvc.PublicCounts, Organisations: directorySvc.VerifiedOrganisations,
		DirectOwners: d.Identity.ApprovedDirectOwners, Now: d.Now,
	}, logger)
	// A listing whose advertiser is not verified takes no request, no
	// appointment and no request to stay.
	requestStore := verifiedListings{Store: d.Requests, catalogue: listingsSvc}

	identityOpts := []identity.Option{
		identity.WithAdminSessionTTLs(cfg.AdminSessionTTL, cfg.AdminSessionIdleTTL),
		identity.WithRoleCatalogue(vocab),
	}
	requestOpts := []propertyrequests.Option{propertyrequests.WithVocabulary(vocab), propertyrequests.WithStayChecker(stayChecker{svc: availabilitySvc})}
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
	// Countries with their listing counts: every country can be used, and
	// ?has_listings=true is what the public's country lists show.
	marketsH := markets.NewHandler(d.Markets, listingsSvc.CountByCountry, logger)
	// Appointments: routed with the request and enquiry rules, named from the
	// public catalogue, shown in the market's time zone.
	appointmentsSvc := appointments.NewService(appointments.Deps{
		Store: d.Appointments, Listings: requestStore, Profiles: d.Enquiries,
		Catalogue: listingCards{store: d.Listings}, Zones: marketZones{markets: d.Markets},
		People: people{identity: d.Identity}, Vocabulary: vocab, Now: d.Now,
	})
	// A viewing or consultation request with a preferred slot creates its
	// appointment in the same transaction.
	requestOpts = append(requestOpts, propertyrequests.WithAppointmentPlanner(appointmentsSvc))
	requestsSvc := propertyrequests.NewService(requestStore, requestOpts...)
	enquiriesSvc := businessenquiries.NewService(d.Enquiries, enquiryOpts...)
	requestsH := propertyrequests.NewHandler(requestsSvc, logger)
	requestsH.SetListingNames(listingNames{svc: listingsSvc})
	requestsH.SetMarkets(marketIDs(d.Markets))
	requestWorkLimiter := httpx.NewRateLimiter(requestWorkPerMinute, requestWorkBurst)
	enquiriesH := businessenquiries.NewHandler(enquiriesSvc, logger)
	appointmentsH := appointments.NewHandler(appointmentsSvc, inboxCounts{requests: requestsSvc, enquiries: enquiriesSvc}, logger)
	appointmentLimiter := httpx.NewRateLimiter(appointmentsPerMinute, appointmentsBurst)
	authn := auth.NewMiddleware(identitySvc, cfg.CORSAllowedOrigins, logger)

	onboardingSvc := onboarding.NewService(d.Onboarding, d.Files, onboardingVocabulary{ref: referenceSvc, markets: d.Markets},
		agencyDirectory{directory: d.Directory, ref: referenceSvc, markets: d.Markets}, logger)
	if d.Now != nil {
		onboardingSvc.SetClock(d.Now)
	}
	onboardingH := onboarding.NewHandler(onboardingSvc, logger)

	// Dashboards: one role-aware summary per account, an organisation's
	// members, and the administrators' overview. Every number is read through
	// the module that owns it, with that module's own scoping.
	dashboardH := dashboard.NewHandler(dashboard.Sources{
		Requests: requestsSvc, RequestViews: requestsH, Appointments: appointmentsH,
		Onboarding: onboardingSvc, Listings: availabilitySvc, People: d.Identity,
		RoleLabels: onboardingVocabulary{ref: referenceSvc, markets: d.Markets}.RoleLabels,
		ListingCounts: func(ctx context.Context, marketCodes []string) (map[string]int, error) {
			now := time.Now()
			if d.Now != nil {
				now = d.Now()
			}
			return d.Listings.CountByStatus(ctx, marketCodes, now.UTC())
		},
		MarketIDs: marketIDs(d.Markets), Now: d.Now,
	}, logger)
	uploadLimiter := httpx.NewRateLimiter(UploadRequestsPerMinute, UploadRequestsBurst)

	requestLimiter := httpx.NewRateLimiter(propertyRequestsPerMinute, propertyRequestsBurst)
	enquiryLimiter := httpx.NewRateLimiter(businessEnquiriesPerMinute, businessEnquiriesBurst)
	authLimiter := httpx.NewRateLimiter(AuthRequestsPerMinute, AuthRequestsBurst)
	adminAuthLimiter := httpx.NewRateLimiter(AdminAuthRequestsPerMinute, AdminAuthRequestsBurst)
	captchaLimiter := httpx.NewRateLimiter(CaptchaRequestsPerMinute, CaptchaRequestsBurst)

	r := chi.NewRouter()
	r.Use(
		httpx.RequestID,
		httpx.AccessLog(logger),
		httpx.Recover(logger),
		httpx.SecurityHeaders(secure),
		httpx.CORS(cfg.CORSAllowedOrigins),
		httpx.BodyLimitFunc(func(r *http.Request) int64 {
			switch {
			case isLogoUpload(r):
				return directory.MaxLogoBodyBytes
			case isUpload(r):
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
		// Every country (the platform is global). /markets is the older name
		// of the same data, kept for existing clients.
		r.Get("/countries", marketsH.List)
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
		// A short stay's calendar as the public sees it: available or not
		// per night, never why.
		r.Get("/listings/{slug}/availability", availabilityH.Public)
		r.Get("/directory", directoryH.Kinds)
		r.Get("/directory/{kind}", directoryH.List)
		r.Get("/directory/{kind}/{slug}", directoryH.Get)
		// A public profile's logo: an image, no session, no cookie.
		r.Get("/directory/{kind}/{slug}/logo", directoryH.Logo)
		r.Get("/sitemap", directoryH.Sitemap)
		// How many advertisers are verified and how many listings are public.
		r.Get("/trust/summary", trustH.Summary)

		// The only way to contact a directory profile. Open to guests and
		// signed-in customers alike.
		r.With(enquiryLimiter.Middleware).Post("/business-enquiries", enquiriesH.Create)

		// Enquiries received by the caller's organisation. Any membership may
		// enter; the handler scopes the data to the caller's organisations
		// and, for agents and staff, to their own agent profile.
		r.With(auth.RequireMembership()).Get("/inbox/business-enquiries", enquiriesH.Inbox)

		// The question of the registration form, in its own bucket so that
		// asking for one does not use up the sign-in allowance.
		r.Route("/auth", func(r chi.Router) {
			r.With(captchaLimiter.Middleware).Get("/captcha", identityH.Captcha)
			r.Group(func(r chi.Router) {
				r.Use(authLimiter.Middleware)
				r.Post("/register", identityH.Register)
				r.Post("/verify-email", identityH.VerifyEmail)
				r.Post("/resend-verification", identityH.ResendVerification)
				r.Post("/login", identityH.Login)
				r.Post("/logout", identityH.Logout)
				r.Post("/password-reset/request", identityH.RequestPasswordReset)
				r.Post("/password-reset/confirm", identityH.ConfirmPasswordReset)
			})
		})

		r.With(auth.RequireAuth).Get("/me", identityH.Me)

		// One role-aware summary for the signed-in account: which dashboard
		// applies, counters, next actions and recent items, each scoped as
		// the endpoint it summarises.
		r.With(auth.RequireAuth).Get("/me/dashboard", dashboardH.Me)

		// The requests this account sent while signed in, as their customer
		// sees them: never the lead stage, an internal note or who handles it.
		r.Route("/me/property-requests", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", requestsH.Mine)
			r.Get("/{id}", requestsH.MineOne)
		})

		// The lister's side: the listings the caller may manage (read-only;
		// creating and editing listings is not built) and the availability
		// calendar and stay rules of the short stays among them. The handler
		// decides who may enter (auth/capabilities.go): 403 for a caller who
		// manages no listings, 404 for a listing that is not theirs.
		r.Route("/me/listings", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", availabilityH.MyListings)
			r.Get("/{id}/availability", availabilityH.Calendar)
			r.With(calendarLimiter.Middleware).Put("/{id}/availability", availabilityH.Change)
			r.With(calendarLimiter.Middleware).Patch("/{id}/stay-rules", availabilityH.StayRules)
		})

		// The directory profiles the caller may manage, and their logos. The
		// service decides who may change which (auth/capabilities.go): 404 for
		// another organisation's profile, 403 for a member whose role may not.
		r.Route("/me/profiles", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", directoryH.Mine)
			r.With(uploadLimiter.Middleware).Put("/{kind}/{slug}/logo", directoryH.PutLogo)
			r.With(uploadLimiter.Middleware).Delete("/{kind}/{slug}/logo", directoryH.DeleteLogo)
		})

		// Appointments. A signed-in customer asks for one and manages their
		// own; they never see another customer's (404).
		r.With(auth.RequireAuth, appointmentLimiter.Middleware).Post("/appointments", appointmentsH.Create)
		r.Route("/me/appointments", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", appointmentsH.ListMine)
			r.Get("/{id}", appointmentsH.GetMine)
			r.Post("/{id}/cancel", appointmentsH.CancelMine)
			r.Post("/{id}/reschedule", appointmentsH.RescheduleMine)
			r.Post("/{id}/accept", appointmentsH.AcceptMine)
		})
		// The professional side. The handler decides who may enter (a member
		// of an organisation that takes appointments, or an approved direct
		// owner: auth/capabilities.go) and scopes every row to the caller.
		r.Route("/inbox/appointments", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", appointmentsH.Inbox)
			r.Get("/{id}", appointmentsH.Received)
			r.Post("/{id}/confirm", appointmentsH.Confirm)
			r.Post("/{id}/reschedule", appointmentsH.Reschedule)
			r.Post("/{id}/cancel", appointmentsH.Cancel)
			r.Post("/{id}/complete", appointmentsH.Complete)
			r.Post("/{id}/no-show", appointmentsH.NoShow)
			r.Post("/{id}/assign", appointmentsH.Assign)
		})
		// Badge counts for the apps, each scoped as its own inbox.
		r.With(auth.RequireAuth).Get("/inbox/summary", appointmentsH.Summary)

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

			// The counts an administrator starts from. Every platform role
			// may ask; the handler returns only the sections its role covers.
			r.With(auth.RequirePlatformRole(auth.AllPlatformRoles()...)).Get("/overview", dashboardH.Overview)

			// Requests nobody could receive: SUPER_ADMIN, PLATFORM_ADMIN,
			// SUPPORT, and MARKET_ADMIN for its own markets (the handler
			// limits it; another market's request is 404).
			r.Route("/routing-exceptions", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(propertyrequests.ExceptionAdminRoles()...))
				r.Get("/", requestsH.Exceptions)
				r.With(requestWorkLimiter.Middleware).Post("/{id}/resolve", requestsH.ResolveException)
			})
		})

		// Request inbox for organisation members and direct sellers. The
		// handler decides who may enter (membership, direct seller, or a
		// declared PRIVATE_SELLER) and scopes the data to the caller's own
		// organisations, role and listings.
		r.Route("/agent/property-requests", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", requestsH.Inbox)
			r.Get("/{id}", requestsH.Get)
			// Working a request: move its lead stage, add an internal note.
			r.With(requestWorkLimiter.Middleware).Patch("/{id}", requestsH.Patch)
			r.With(requestWorkLimiter.Middleware).Post("/{id}/notes", requestsH.AddNote)
		})

		// The members of an organisation, read-only, for its OWNER and
		// MANAGER. The handler answers 404 to anyone who is not a member.
		r.With(auth.RequireAuth).Get("/agent/organisations/{id}/members", dashboardH.Members)
	})
	return r, nil
}
