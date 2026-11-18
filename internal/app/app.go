// Package app wires the modules into the HTTP router. It lives outside
// cmd/api so the complete stack (middleware order, CORS, CSRF, rate limits,
// authorisation) can be exercised by tests exactly as it is served.
package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/dashboard"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/reports"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/modules/trust"
	"rightproperty365.com/backend/internal/platform/adminaudit"
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

	// Per-IP limit on the sign-in code step (POST .../login/verify and
	// .../login/resend), one bucket for customers and one for administrators,
	// separate from the password step: bursts of 20, refilled at 20 per
	// minute. Each challenge also dies after 5 wrong codes.
	SignInCodeRequestsPerMinute = 20
	SignInCodeRequestsBurst     = 20

	// Per-IP limit on onboarding document uploads, its own bucket: bursts of
	// 20, refilled at 20 per minute. Each upload can be 10 MB.
	UploadRequestsPerMinute = 20
	UploadRequestsBurst     = 20
	// Uploads get longer than other requests: 10 MB over a slow connection.
	uploadTimeout = 2 * time.Minute

	// Per-IP limit on changing listings (create, PATCH, submit, archive,
	// relist, delete, photo text and order) and on review decisions, its own
	// bucket: bursts of 60, refilled at 60 per minute. Photo uploads share the
	// upload bucket.
	listingChangesPerMinute = 60
	listingChangesBurst     = 60

	// Per-IP limit on changing a team (invite, cancel an invitation, decide a
	// join request, change, suspend or remove a member), its own bucket:
	// bursts of 30, refilled at 30 per minute.
	teamChangesPerMinute = 30
	teamChangesBurst     = 30

	// Per-IP limit on the invitation links (GET /v1/invitations/{token} and
	// its accept), its own bucket: bursts of 20, refilled at 20 per minute.
	invitationRequestsPerMinute = 20
	invitationRequestsBurst     = 20

	// Per-IP limit on changing leases and lease templates (create, PATCH,
	// delete, send, cancel, approve, retire), its own bucket: bursts of 30,
	// refilled at 30 per minute. Each save renders a PDF.
	leaseChangesPerMinute = 30
	leaseChangesBurst     = 30

	// Per-IP limit on the administrators' changes (suspend, reactivate and
	// delete accounts, plans, subscription decisions, reference data) and on
	// a subscriber's requests, cancellations and renewals, its own bucket:
	// bursts of 30, refilled at 30 per minute.
	adminChangesPerMinute = 30
	adminChangesBurst     = 30

	// Per-IP limit on the public signing links (GET and POST
	// /v1/signing/{token}/...), its own bucket: bursts of 20, refilled at 20
	// per minute. Sending a code has a stricter bucket of its own (5 / 5 per
	// minute) on top of the per-recipient rules (one every 30 seconds, 10 in
	// all).
	signingRequestsPerMinute = 20
	signingRequestsBurst     = 20
	signingCodesPerMinute    = 5
	signingCodesBurst        = 5
	// Per-IP limit on the public verification (GET /v1/verify/{reference},
	// POST /v1/verify): bursts of 10, refilled at 10 per minute.
	verifyRequestsPerMinute = 10
	verifyRequestsBurst     = 10

	// Per-IP limit on POST /v1/analytics/events (public), its own bucket:
	// bursts of 60, refilled at 120 per minute. Each request carries at most
	// 50 events; the per-session and per-network bot rule is separate.
	analyticsEventsPerMinute = 120
	analyticsEventsBurst     = 60

	// Per-IP limit on changing contact books (create, edit, notes,
	// reminders, stage, owner, listings, merge, import, from-request), its
	// own bucket: bursts of 60, refilled at 60 per minute.
	contactChangesPerMinute = 60
	contactChangesBurst     = 60
)

// uploadPath matches POST /v1/me/onboarding/{role}/documents and logoPath
// PUT /v1/me/profiles/{kind}/{slug}/logo: the two routes with a larger body
// limit and a longer timeout.
var (
	uploadPath = regexp.MustCompile(`^/v1/me/onboarding/[^/]+/documents$`)
	logoPath   = regexp.MustCompile(`^/v1/me/profiles/[^/]+/[^/]+/logo$`)
	// photoPath is POST /v1/me/listings/{id}/photos: a listing photo upload.
	photoPath = regexp.MustCompile(`^/v1/me/listings/[^/]+/photos$`)
	// agreementDocumentPath is PUT /v1/me/agreements/{id}/document and
	// verifyPath POST /v1/verify: a PDF of up to 20 MB.
	agreementDocumentPath = regexp.MustCompile(`^/v1/me/agreements/[^/]+/document$`)
	// submitPath is POST /v1/signing/{token}/submit (adopted images).
	submitPath = regexp.MustCompile(`^/v1/signing/[^/]+/submit$`)
	// contactImportPath is POST /v1/agent/organisations/{id}/contacts/import (a CSV of up to 2 MB).
	contactImportPath = regexp.MustCompile(`^/v1/agent/organisations/[^/]+/contacts/import$`)
)

func isContactImport(r *http.Request) bool {
	return r.Method == http.MethodPost && contactImportPath.MatchString(r.URL.Path)
}

func isPDFUpload(r *http.Request) bool {
	return (r.Method == http.MethodPut && agreementDocumentPath.MatchString(r.URL.Path)) || (r.Method == http.MethodPost && r.URL.Path == "/v1/verify")
}

func isPhotoUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && photoPath.MatchString(r.URL.Path)
}

func isLogoUpload(r *http.Request) bool {
	return r.Method == http.MethodPut && logoPath.MatchString(r.URL.Path)
}

func isUpload(r *http.Request) bool {
	return (r.Method == http.MethodPost && uploadPath.MatchString(r.URL.Path)) || isLogoUpload(r) || isPhotoUpload(r) || isPDFUpload(r) ||
		isContactImport(r)
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
	// Teams holds invitations and the team audit trail, and changes
	// memberships together with everything a change releases.
	Teams teams.Store
	// TeamMailer sends invitation emails; nil means Mailer when it can.
	TeamMailer teams.Mailer
	// Leases holds the lease templates and the leases; their PDFs are kept
	// in Files.
	Leases leases.Store
	// Agreements holds the agreements (signing); AgreementMailer sends
	// their emails (nil means Mailer when it can).
	Agreements      agreements.Store
	AgreementMailer agreements.Mailer

	// The administrators' area (API 0.25.0): account administration,
	// subscriptions, reports and the administrators' audit trail.
	// AccountMailer sends the deletion notice (nil means Mailer when it can).
	// The Reference store must also implement reference.AdminStore.
	Accounts      accounts.Store
	AccountMailer accounts.Mailer
	Subscriptions subscriptions.Store
	Reports       reports.Store
	AdminAudit    adminaudit.Log

	// Saved holds the listings customers saved (API 0.27.0).
	Saved saved.Store

	// Analytics holds the raw analytics events and the daily rollups (API
	// 0.30.0). The raw retention is Config.AnalyticsRawRetentionDays.
	Analytics analytics.Store

	// CRM holds the contact books (API 0.31.0); CRMItems reads the property
	// requests, business enquiries and appointments contacts are linked to
	// (crm.PostgresItems, or MemoryCRMItems without a database).
	CRM      crm.Store
	CRMItems crm.Items

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
	// Listing management: advertisers create and edit their listings and
	// photos; platform reviewers approve them. Photos are kept in the same
	// private object storage as the documents and logos.
	manageStore, ok := d.Listings.(listings.ManageStore)
	if !ok {
		return nil, errors.New("app: the listing store does not implement listing management")
	}
	manageH := listings.NewManageHandler(listings.NewManager(listingsSvc, manageStore, d.Files, logger), logger)
	listingLimiter := httpx.NewRateLimiter(listingChangesPerMinute, listingChangesBurst)
	// Saved listings: a customer's hearts. Whether a listing may be saved
	// (public right now) is the listings service's own rule.
	if d.Saved == nil {
		return nil, errors.New("app: no saved-listings store")
	}
	savedH := saved.NewHandler(saved.NewService(d.Saved, listingsSvc, d.Now), logger)
	// Listing analytics: ingestion of the browser events and the owner-scoped
	// and administrators' reports. The rollups and the server-side events are
	// the worker's.
	if d.Analytics == nil {
		return nil, errors.New("app: no analytics store")
	}
	analyticsH := analytics.NewHandler(AnalyticsService(d.Analytics, d.Listings, d.Markets, cfg.AnalyticsRawRetentionDays, logger, d.Now), logger)
	analyticsLimiter := httpx.NewRateLimiter(analyticsEventsPerMinute, analyticsEventsBurst)
	// The agent CRM: an organisation's (or a direct owner's) contact book.
	if d.CRM == nil || d.CRMItems == nil {
		return nil, errNoCRM
	}
	crmH := crm.NewHandler(CRMService(d.CRM, d.CRMItems, d.Listings, d.Identity, d.Reference, d.Teams, logger, d.Now), logger)
	contactLimiter := httpx.NewRateLimiter(contactChangesPerMinute, contactChangesBurst)
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
	// The demo sign-in code: config.Load already refuses it outside
	// development and staging; checked again here so no other path can pass it.
	if cfg.AuthDemoCode.IsSet() && (cfg.Env == config.EnvDevelopment || cfg.Env == config.EnvStaging) {
		identityOpts = append(identityOpts, identity.WithDemoCode(cfg.AuthDemoCode.Reveal()))
	}
	requestOpts := []propertyrequests.Option{
		propertyrequests.WithVocabulary(vocab), propertyrequests.WithStayChecker(stayChecker{svc: availabilitySvc}),
		propertyrequests.WithAssignees(assignees{identity: d.Identity}),
	}
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

	// Teams: the OWNER and MANAGER of an organisation run its team. Join
	// requests are onboarding applications, decided through onboardingSvc.
	if d.Teams == nil {
		return nil, errors.New("app: no team store")
	}
	teamMailer := d.TeamMailer
	if teamMailer == nil {
		teamMailer, _ = d.Mailer.(teams.Mailer)
	}
	teamsSvc := teams.NewService(d.Teams, teamMailer, logger)
	if d.Now != nil {
		teamsSvc.SetClock(d.Now)
	}
	teamsH := teams.NewHandler(teamsSvc, onboardingSvc, logger)
	teamLimiter := httpx.NewRateLimiter(teamChangesPerMinute, teamChangesBurst)
	invitationLimiter := httpx.NewRateLimiter(invitationRequestsPerMinute, invitationRequestsBurst)

	// Leases: the template library (content administrators) and lease
	// drafting by whoever manages listings.
	if d.Leases == nil || d.Agreements == nil {
		return nil, errors.New("app: no lease or agreement store")
	}
	agreementMailer := d.AgreementMailer
	if agreementMailer == nil {
		agreementMailer, _ = d.Mailer.(agreements.Mailer)
	}
	agreementDeps := agreements.Deps{Store: d.Agreements, Files: d.Files, Mailer: agreementMailer, Logger: logger, Now: d.Now, PublicWebURL: cfg.PublicWebURL}
	// AUTH_DEMO_CODE is honoured exactly as for signing in.
	if cfg.AuthDemoCode.IsSet() && (cfg.Env == config.EnvDevelopment || cfg.Env == config.EnvStaging) {
		agreementDeps.DemoCode = cfg.AuthDemoCode.Reveal()
	}
	agreementsSvc := agreements.NewService(agreementDeps)
	agreementsH := agreements.NewHandler(agreementsSvc, logger)
	signingLimiter := httpx.NewRateLimiter(signingRequestsPerMinute, signingRequestsBurst)
	signingCodeLimiter := httpx.NewRateLimiter(signingCodesPerMinute, signingCodesBurst)
	verifyLimiter := httpx.NewRateLimiter(verifyRequestsPerMinute, verifyRequestsBurst)
	leasesSvc := leases.NewService(leases.Deps{
		Agreements: leaseAgreements{svc: agreementsSvc},
		Store:      d.Leases, Files: d.Files, Listings: leaseListings{svc: listingsSvc},
		CountryName: countryNames(d.Markets), Person: personOf(d.Identity), Logger: logger, Now: d.Now,
	})
	leasesH := leases.NewHandler(leasesSvc, logger)
	agreementsSvc.SetOnFinished(FollowLeases(leasesSvc, logger))
	leaseLimiter := httpx.NewRateLimiter(leaseChangesPerMinute, leaseChangesBurst)

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
		Leases: func(ctx context.Context, p *auth.Principal) (dashboard.LeaseCounts, error) {
			c, err := leasesSvc.DashboardCounts(ctx, p)
			return dashboard.LeaseCounts{Drafts: c.Drafts, AwaitingSignatures: c.AwaitingSignatures}, err
		},
		MayCreateLeases: leases.MayCreateLeases,
		Signing: func(ctx context.Context, p *auth.Principal) (int, int, error) {
			c, err := agreementsSvc.DashboardCounts(ctx, p)
			return c.WaitingForYou, c.WaitingForOthers, err
		},
	}, logger)
	uploadLimiter := httpx.NewRateLimiter(UploadRequestsPerMinute, UploadRequestsBurst)

	// The administrators' area: accounts, subscriptions, reference data,
	// reports and the audit trail.
	if d.Accounts == nil || d.Subscriptions == nil || d.Reports == nil {
		return nil, errors.New("app: no account, subscription or report store")
	}
	referenceAdminStore, ok := d.Reference.(reference.AdminStore)
	if !ok {
		return nil, errors.New("app: the reference store does not implement reference administration")
	}
	accountMailer := d.AccountMailer
	if accountMailer == nil {
		accountMailer, _ = d.Mailer.(accounts.Mailer)
	}
	subscriptionsSvc := subscriptions.NewService(subscriptions.Deps{Store: d.Subscriptions, Currency: marketCurrency(d.Markets), Logger: logger, Now: d.Now})
	subscriptionsH := subscriptions.NewHandler(subscriptionsSvc, logger)
	accountsH := accounts.NewHandler(accounts.NewService(accounts.Deps{Store: d.Accounts, Audit: d.AdminAudit, Mailer: accountMailer,
		Subscriptions: AccountSubscriptions(d.Subscriptions, d.Now), Logger: logger, Now: d.Now}), logger, secure)
	referenceAdminH := reference.NewAdminHandler(reference.NewAdmin(referenceSvc, referenceAdminStore, d.Now), logger)
	reportsH := reports.NewHandler(d.Reports, marketCodes(d.Markets), d.Now, logger)
	adminLimiter := httpx.NewRateLimiter(adminChangesPerMinute, adminChangesBurst)

	requestLimiter := httpx.NewRateLimiter(propertyRequestsPerMinute, propertyRequestsBurst)
	enquiryLimiter := httpx.NewRateLimiter(businessEnquiriesPerMinute, businessEnquiriesBurst)
	authLimiter := httpx.NewRateLimiter(AuthRequestsPerMinute, AuthRequestsBurst)
	adminAuthLimiter := httpx.NewRateLimiter(AdminAuthRequestsPerMinute, AdminAuthRequestsBurst)
	captchaLimiter := httpx.NewRateLimiter(CaptchaRequestsPerMinute, CaptchaRequestsBurst)
	codeLimiter := httpx.NewRateLimiter(SignInCodeRequestsPerMinute, SignInCodeRequestsBurst)
	adminCodeLimiter := httpx.NewRateLimiter(SignInCodeRequestsPerMinute, SignInCodeRequestsBurst)

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
			case isPhotoUpload(r):
				return listings.MaxPhotoBodyBytes
			case isPDFUpload(r):
				return agreements.MaxDocumentBodyBytes
			case r.Method == http.MethodPost && submitPath.MatchString(r.URL.Path):
				return agreements.MaxSubmitBodyBytes
			case r.Method == http.MethodPut && r.URL.Path == "/v1/me/signature":
				return agreements.MaxSignatureBodyBytes
			case r.Method == http.MethodPost && r.URL.Path == "/v1/analytics/events":
				return analytics.MaxBodyBytes
			case isContactImport(r):
				return crm.MaxImportBodyBytes
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
		// A listing photo: public (and cacheable for a year with its ?v=) while
		// the listing and the photo are public; otherwise only for the
		// listing's managers and reviewers (404 for everyone else).
		r.Get("/listing-photos/{photoId}/{size}", manageH.Photo)
		r.Get("/sitemap", directoryH.Sitemap)
		// How many advertisers are verified and how many listings are public.
		r.Get("/trust/summary", trustH.Summary)

		// Listing analytics (FR-033..036). Ingestion is public (signed-in
		// optional), in its own bucket, with a 32 KB body. The reports are
		// owner-scoped: 404 for a listing or an organisation that is not the
		// caller's. Raw events are never returned.
		r.With(analyticsLimiter.Middleware).Post("/analytics/events", analyticsH.Ingest)
		r.With(auth.RequireAuth).Get("/analytics/listings/{id}", analyticsH.ListingReport)
		r.With(auth.RequireAuth).Get("/analytics/organisations/{orgId}", analyticsH.OrganisationReport)

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
			// The second step of signing in: the emailed code.
			r.With(codeLimiter.Middleware).Post("/login/verify", identityH.LoginVerify)
			r.With(codeLimiter.Middleware).Post("/login/resend", identityH.LoginResend)
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
		// Deleting one's own account, confirmed with the password. It shares
		// the sign-in bucket (a password is checked).
		r.With(auth.RequireAuth, authLimiter.Middleware).Delete("/me", accountsH.DeleteSelf)

		// Subscriptions: the plans the caller may take, and their own
		// subscriptions and those of the organisations they own (404 for
		// anybody else's). There is no online payment: a request is invoiced.
		r.With(auth.RequireAuth).Get("/me/subscription-plans", subscriptionsH.AvailablePlans)
		r.Route("/me/subscriptions", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", subscriptionsH.Mine)
			r.With(adminLimiter.Middleware).Post("/", subscriptionsH.Request)
			r.Get("/{id}", subscriptionsH.MineOne)
			r.With(adminLimiter.Middleware).Post("/{id}/cancel", subscriptionsH.CancelPending)
			r.With(adminLimiter.Middleware).Post("/{id}/renew", subscriptionsH.Renew)
		})

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

		// Saved listings (FR-016): any customer-scope session (an admin
		// session gets 403). Saving and removing share the listing-changes
		// bucket (60 a minute per IP).
		r.Route("/me/saved-listings", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", savedH.List)
			r.Get("/ids", savedH.IDs)
			r.With(listingLimiter.Middleware).Put("/{listingId}", savedH.Save)
			r.With(listingLimiter.Middleware).Delete("/{listingId}", savedH.Remove)
		})

		// The lister's side: the listings the caller may manage, creating and
		// editing them with their photos and sending them for review, and the
		// availability calendar and stay rules of the short stays among them.
		// The handlers decide who may do what (auth/capabilities.go): 403 for a
		// caller who manages no listings (or may only read this one), 404 for
		// a listing that is not theirs.
		r.Route("/me/listings", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", availabilityH.MyListings)
			r.With(listingLimiter.Middleware).Post("/", manageH.Create)
			r.Get("/{id}", manageH.Get)
			r.With(listingLimiter.Middleware).Patch("/{id}", manageH.Patch)
			r.With(listingLimiter.Middleware).Delete("/{id}", manageH.Delete)
			r.With(listingLimiter.Middleware).Post("/{id}/submit", manageH.Submit)
			r.With(listingLimiter.Middleware).Post("/{id}/archive", manageH.Archive)
			r.With(listingLimiter.Middleware).Post("/{id}/relist", manageH.Relist)
			r.With(uploadLimiter.Middleware).Post("/{id}/photos", manageH.AddPhoto)
			r.With(listingLimiter.Middleware).Put("/{id}/photos/order", manageH.OrderPhotos)
			r.With(listingLimiter.Middleware).Patch("/{id}/photos/{photoId}", manageH.PatchPhoto)
			r.With(listingLimiter.Middleware).Delete("/{id}/photos/{photoId}", manageH.DeletePhoto)
			r.Get("/{id}/availability", availabilityH.Calendar)
			r.With(calendarLimiter.Middleware).Put("/{id}/availability", availabilityH.Change)
			r.With(calendarLimiter.Middleware).Patch("/{id}/stay-rules", availabilityH.StayRules)
		})

		// Leases: the approved templates on offer, and drafting. The service
		// decides who may (leases.MayCreateLeases: whoever manages listings;
		// 403 otherwise) and which leases each sees (404 for the others).
		r.With(auth.RequireAuth).Get("/me/lease-templates", leasesH.OfferedTemplates)
		r.Route("/me/leases", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", leasesH.List)
			r.With(leaseLimiter.Middleware).Post("/", leasesH.Create)
			r.Get("/{id}", leasesH.Get)
			r.With(leaseLimiter.Middleware).Patch("/{id}", leasesH.Patch)
			r.With(leaseLimiter.Middleware).Delete("/{id}", leasesH.Delete)
			r.Get("/{id}/pdf", leasesH.PDF)
			r.With(leaseLimiter.Middleware).Post("/{id}/send", leasesH.Send)
			r.With(leaseLimiter.Middleware).Post("/{id}/cancel", leasesH.Cancel)
			r.Get("/{id}/events", leasesH.Events)
		})
		// An organisation's own lease templates. The service decides who may:
		// its OWNER and MANAGER manage them, its agents read them (403 for
		// another member, 404 for somebody who is not a member).
		r.Route("/me/organisations/{orgId}/lease-templates", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", leasesH.OrgTemplates)
			r.With(leaseLimiter.Middleware).Post("/", leasesH.OrgCreateTemplate)
			r.Get("/{id}", leasesH.OrgTemplate)
			r.With(leaseLimiter.Middleware).Patch("/{id}", leasesH.OrgEditTemplate)
			r.With(leaseLimiter.Middleware).Post("/{id}/publish", leasesH.OrgPublishTemplate)
			r.With(leaseLimiter.Middleware).Post("/{id}/retire", leasesH.OrgRetireTemplate)
			r.Get("/{id}/events", leasesH.OrgTemplateEvents)
		})

		// Agreements (signing): a sender's documents, recipients and fields.
		// The service decides who may (agreements.MaySend; 403 otherwise)
		// and which each sees (404 for the others).
		r.Route("/me/agreements", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", agreementsH.List)
			r.With(leaseLimiter.Middleware).Post("/", agreementsH.Create)
			r.Get("/{id}", agreementsH.Get)
			r.With(leaseLimiter.Middleware).Patch("/{id}", agreementsH.Patch)
			r.With(leaseLimiter.Middleware).Delete("/{id}", agreementsH.Delete)
			r.With(uploadLimiter.Middleware).Put("/{id}/document", agreementsH.Document)
			r.With(leaseLimiter.Middleware).Put("/{id}/fields", agreementsH.Fields)
			r.Get("/{id}/pdf", agreementsH.PDF)
			r.With(leaseLimiter.Middleware).Post("/{id}/send", agreementsH.Send)
			r.With(leaseLimiter.Middleware).Post("/{id}/cancel", agreementsH.Cancel)
			r.With(leaseLimiter.Middleware).Post("/{id}/recipients/{recipientId}/replace", agreementsH.Replace)
			r.With(leaseLimiter.Middleware).Post("/{id}/recipients/{recipientId}/remind", agreementsH.Remind)
			r.Get("/{id}/events", agreementsH.Events)
		})
		// The documents waiting on the signed-in person's email address, and
		// their saved signature.
		r.With(auth.RequireAuth).Get("/me/signing-requests", agreementsH.SigningRequests)
		r.With(auth.RequireAuth, signingLimiter.Middleware).Post("/me/signing-requests/{recipientId}/link", agreementsH.OwnLink)
		r.With(auth.RequireAuth).Get("/me/signature", agreementsH.MySignature)
		r.With(auth.RequireAuth, leaseLimiter.Middleware).Put("/me/signature", agreementsH.SaveMySignature)
		// Signing by link: no session; the token is a credential (never in
		// the access log, see httpx.LoggedPath).
		r.Route("/signing/{token}", func(r chi.Router) {
			r.Use(signingLimiter.Middleware)
			r.Get("/", agreementsH.Signing)
			r.Get("/pdf", agreementsH.SigningPDF)
			r.With(signingCodeLimiter.Middleware).Post("/code", agreementsH.SendCode)
			r.Post("/submit", agreementsH.Submit)
			r.Post("/decline", agreementsH.Decline)
		})
		// A generated stamp as a PNG: no session (signers by link preview
		// their stamp too); the signing links' limit.
		r.With(signingLimiter.Middleware).Post("/stamp-preview", agreementsH.StampPreview)
		// Public verification of a completed document.
		r.With(verifyLimiter.Middleware).Get("/verify/{reference}", agreementsH.VerifyReference)
		r.With(verifyLimiter.Middleware).Post("/verify", agreementsH.VerifyFile)

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
			r.With(adminCodeLimiter.Middleware).Post("/auth/login/verify", identityH.AdminLoginVerify)
			r.With(adminCodeLimiter.Middleware).Post("/auth/login/resend", identityH.AdminLoginResend)
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

			// Reviewing listings: SUPER_ADMIN, PLATFORM_ADMIN, MODERATOR, and
			// MARKET_ADMIN for its own markets (the service limits it; another
			// country's listing is 404).
			r.Route("/listings", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(auth.ListingReviewRoles()...))
				r.Get("/", manageH.AdminList)
				r.Get("/{id}", manageH.AdminGet)
				r.With(listingLimiter.Middleware).Post("/{id}/approve", manageH.Approve)
				r.With(listingLimiter.Middleware).Post("/{id}/request-changes", manageH.RequestChanges)
				r.With(listingLimiter.Middleware).Post("/{id}/reject", manageH.Reject)
			})

			// The lease template library: SUPER_ADMIN and CONTENT_ADMIN.
			r.Route("/lease-templates", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(leases.TemplateAdminRoles()...))
				r.Get("/", leasesH.AdminTemplates)
				r.With(leaseLimiter.Middleware).Post("/", leasesH.AdminCreateTemplate)
				r.Get("/{id}", leasesH.AdminTemplate)
				r.With(leaseLimiter.Middleware).Patch("/{id}", leasesH.AdminEditTemplate)
				r.With(leaseLimiter.Middleware).Post("/{id}/approve", leasesH.AdminApproveTemplate)
				r.With(leaseLimiter.Middleware).Post("/{id}/retire", leasesH.AdminRetireTemplate)
			})
			// Every lease, read-only: SUPER_ADMIN and SUPPORT.
			r.With(auth.RequirePlatformRole(leases.LeaseAdminRoles()...)).Get("/leases", leasesH.AdminLeases)
			// Every agreement, read-only: SUPER_ADMIN and SUPPORT.
			r.With(auth.RequirePlatformRole(agreements.AdminRoles()...)).Get("/agreements", agreementsH.AdminList)

			// The counts an administrator starts from. Every platform role
			// may ask; the handler returns only the sections its role covers.
			r.With(auth.RequirePlatformRole(auth.AllPlatformRoles()...)).Get("/overview", dashboardH.Overview)

			// Accounts: SUPER_ADMIN, PLATFORM_ADMIN and SUPPORT read them;
			// SUPER_ADMIN and PLATFORM_ADMIN suspend, reactivate and delete
			// (an administrator account only by a SUPER_ADMIN, never one's own).
			r.Route("/users", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(accounts.ReadRoles()...))
				r.Get("/", accountsH.List)
				r.Get("/{id}", accountsH.Get)
				r.Group(func(r chi.Router) {
					r.Use(auth.RequirePlatformRole(accounts.ManageRoles()...), adminLimiter.Middleware)
					r.Post("/{id}/suspend", accountsH.Suspend)
					r.Post("/{id}/reactivate", accountsH.Reactivate)
					r.Post("/{id}/delete", accountsH.Delete)
				})
			})
			// The administrators' audit trail: SUPER_ADMIN and PLATFORM_ADMIN.
			r.With(auth.RequirePlatformRole(accounts.AuditRoles()...)).Get("/audit", accountsH.Audit)

			// Subscription plans: read by whoever reads subscriptions, changed
			// by SUPER_ADMIN and FINANCE_ADMIN.
			r.Route("/subscription-plans", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(subscriptions.ReadRoles()...))
				r.Get("/", subscriptionsH.AdminPlans)
				r.Get("/{id}", subscriptionsH.AdminPlan)
				r.Group(func(r chi.Router) {
					r.Use(auth.RequirePlatformRole(subscriptions.PlanAdminRoles()...), adminLimiter.Middleware)
					r.Post("/", subscriptionsH.CreatePlan)
					r.Patch("/{id}", subscriptionsH.UpdatePlan)
					r.Post("/{id}/retire", subscriptionsH.RetirePlan)
				})
			})
			// Subscriptions: read by SUPER_ADMIN, FINANCE_ADMIN, PLATFORM_ADMIN
			// and SUPPORT; payments, approvals, rejections and cancellations by
			// SUPER_ADMIN and FINANCE_ADMIN.
			r.Route("/subscriptions", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(subscriptions.ReadRoles()...))
				r.Get("/", subscriptionsH.AdminList)
				r.Get("/{id}", subscriptionsH.AdminGet)
				r.Group(func(r chi.Router) {
					r.Use(auth.RequirePlatformRole(subscriptions.DecisionRoles()...), adminLimiter.Middleware)
					r.Post("/{id}/approve", subscriptionsH.Approve)
					r.Post("/{id}/reject", subscriptionsH.Reject)
					r.Post("/{id}/cancel", subscriptionsH.Cancel)
				})
			})

			// Reference data: SUPER_ADMIN, PLATFORM_ADMIN, CONTENT_ADMIN, and
			// MARKET_ADMIN for the rows of its own market. Nothing is deleted.
			r.Route("/reference", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(reference.AdminRoles()...))
				r.Get("/", referenceAdminH.Sets)
				r.Get("/{set}", referenceAdminH.List)
				r.With(adminLimiter.Middleware).Post("/{set}", referenceAdminH.Create)
				r.With(adminLimiter.Middleware).Patch("/{set}/{code}", referenceAdminH.Update)
				r.With(adminLimiter.Middleware).Post("/{set}/{code}/deactivate", referenceAdminH.Deactivate)
				r.With(adminLimiter.Middleware).Post("/{set}/{code}/reactivate", referenceAdminH.Reactivate)
			})

			// Reports: the summary for SUPER_ADMIN, PLATFORM_ADMIN and
			// FINANCE_ADMIN; the CSV exports by what they contain.
			r.Route("/reports", func(r chi.Router) {
				r.With(auth.RequirePlatformRole(reports.SummaryRoles()...)).Get("/summary", reportsH.Summary)
				r.With(auth.RequirePlatformRole(reports.PeopleExportRoles()...)).Get("/users.csv", reportsH.Users)
				r.With(auth.RequirePlatformRole(reports.PeopleExportRoles()...)).Get("/listings.csv", reportsH.Listings)
				r.With(auth.RequirePlatformRole(reports.SubscriptionExportRoles()...)).Get("/subscriptions.csv", reportsH.Subscriptions)
			})

			// Listing analytics, site and market aggregates: SUPER_ADMIN,
			// PLATFORM_ADMIN, and MARKET_ADMIN for its own markets (403 for
			// another market).
			r.With(auth.RequirePlatformRole(analytics.AdminRoles()...)).Get("/analytics/summary", analyticsH.AdminSummary)

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
			// Giving a request to a member, or back to the organisation
			// inbox: the organisation's OWNER and MANAGER only.
			r.With(requestWorkLimiter.Middleware).Post("/{id}/assign", requestsH.Assign)
		})

		// An organisation's team, for its OWNER and MANAGER (what each may
		// do to whom: auth.GrantableRoles). The handlers answer 404 to anyone
		// who is not an active member and 403 to its AGENT and STAFF.
		r.Route("/agent/organisations/{id}", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/members", teamsH.Members)
			r.With(teamLimiter.Middleware).Patch("/members/{userId}", teamsH.PatchMember)
			r.With(teamLimiter.Middleware).Delete("/members/{userId}", teamsH.RemoveMember)
			r.Get("/invitations", teamsH.Invitations)
			r.With(teamLimiter.Middleware).Post("/invitations", teamsH.Invite)
			r.With(teamLimiter.Middleware).Delete("/invitations/{invitationId}", teamsH.CancelInvitation)
			r.Get("/join-requests", teamsH.JoinRequests)
			r.With(teamLimiter.Middleware).Post("/join-requests/{requestId}/approve", teamsH.ApproveJoinRequest)
			r.With(teamLimiter.Middleware).Post("/join-requests/{requestId}/decline", teamsH.DeclineJoinRequest)
			r.Get("/audit", teamsH.Audit)

			// The organisation's contact book (the agent CRM). {id} is the
			// organisation, or an approved direct owner's own user ID for their
			// personal book. The handlers answer 404 CONTACT_NOT_FOUND to
			// anyone else (auth.MayReadContacts / OwnsContactBook) and 403 to a
			// member whose role may not change, import or export it.
			r.Route("/contacts", func(r chi.Router) {
				r.Get("/", crmH.List)
				r.With(contactLimiter.Middleware).Post("/", crmH.Create)
				r.Get("/reminders", crmH.Reminders)
				r.Get("/tags", crmH.Tags)
				r.Get("/assignees", crmH.Assignees)
				r.Get("/export.csv", crmH.Export)
				r.With(contactLimiter.Middleware).Post("/merge", crmH.Merge)
				r.With(uploadLimiter.Middleware).Post("/import", crmH.Import)
				r.With(contactLimiter.Middleware).Post("/from-request/{requestId}", crmH.FromRequest)
				r.Get("/{contactId}", crmH.Get)
				r.With(contactLimiter.Middleware).Patch("/{contactId}", crmH.Patch)
				r.With(contactLimiter.Middleware).Delete("/{contactId}", crmH.Delete)
				r.Get("/{contactId}/notes", crmH.Notes)
				r.With(contactLimiter.Middleware).Post("/{contactId}/notes", crmH.AddNote)
				r.With(contactLimiter.Middleware).Post("/{contactId}/reminders", crmH.AddReminder)
				r.With(contactLimiter.Middleware).Patch("/{contactId}/reminders/{reminderId}", crmH.PatchReminder)
				r.With(contactLimiter.Middleware).Post("/{contactId}/stage", crmH.Stage)
				r.With(contactLimiter.Middleware).Post("/{contactId}/owner", crmH.Owner)
				r.With(contactLimiter.Middleware).Post("/{contactId}/listings", crmH.LinkListing)
				r.With(contactLimiter.Middleware).Delete("/{contactId}/listings/{listingId}", crmH.UnlinkListing)
				r.Get("/{contactId}/timeline", crmH.Timeline)
			})
		})

		// An invitation link: what it is for (anyone holding the link), and
		// accepting it (signed in, with the invited address).
		r.With(invitationLimiter.Middleware).Get("/invitations/{token}", teamsH.PublicInvitation)
		r.With(auth.RequireAuth, invitationLimiter.Middleware).Post("/invitations/{token}/accept", teamsH.Accept)
	})
	return r, nil
}
