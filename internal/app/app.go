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
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/accounts"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/modules/analytics"
	"rightproperty365.com/backend/internal/modules/applications"
	"rightproperty365.com/backend/internal/modules/appointments"
	"rightproperty365.com/backend/internal/modules/availability"
	"rightproperty365.com/backend/internal/modules/branches"
	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/crm"
	"rightproperty365.com/backend/internal/modules/dashboard"
	"rightproperty365.com/backend/internal/modules/dataexport"
	"rightproperty365.com/backend/internal/modules/developments"
	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/emaildelivery"
	"rightproperty365.com/backend/internal/modules/health"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/inspections"
	"rightproperty365.com/backend/internal/modules/invoices"
	"rightproperty365.com/backend/internal/modules/leases"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/legal"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/modules/payments"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/reports"
	"rightproperty365.com/backend/internal/modules/reviews"
	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/modules/savedsearches"
	"rightproperty365.com/backend/internal/modules/subscriptions"
	"rightproperty365.com/backend/internal/modules/tax"
	"rightproperty365.com/backend/internal/modules/teams"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/modules/trust"
	"rightproperty365.com/backend/internal/modules/uploads"
	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/i18n"
	"rightproperty365.com/backend/internal/platform/ics"
	"rightproperty365.com/backend/internal/platform/malware"
	"rightproperty365.com/backend/internal/platform/paygateway"
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

	// Per-IP limit on changing forms, listing intake and applications
	// (autosave, submit, withdraw, status, request-info, notes), its own
	// bucket: bursts of 60, refilled at 60 per minute. File uploads share the
	// upload bucket.
	applicationChangesPerMinute = 60
	applicationChangesBurst     = 60

	// Per-IP limit on hourly rental (API 0.33.0): public quotes in their own
	// bucket (bursts of 30, refilled at 30 per minute), and booking requests,
	// customer and host actions and hourly settings in another (bursts of
	// 60, refilled at 60 per minute).
	venueQuotesPerMinute  = 30
	venueQuotesBurst      = 30
	venueChangesPerMinute = 60
	venueChangesBurst     = 60
	// Ledger writes (entries, reversals, notes, attachments) per member.
	ledgerChangesPerMinute = 120
	ledgerChangesBurst     = 120
	// Property, unit and tenancy writes per member.
	tenancyChangesPerMinute = 120
	tenancyChangesBurst     = 120
	// Review writes (reviews, replies, reports, marking enquiries replied,
	// moderation) per caller.
	reviewChangesPerMinute = 60
	reviewChangesBurst     = 30
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
	// applicationFilePath is POST /v1/me/applications/{id}/files (10 MB, as onboarding documents).
	applicationFilePath = regexp.MustCompile(`^/v1/me/applications/[^/]+/files$`)
	// ledgerAttachmentPath is POST /v1/agent/organisations/{id}/ledger/entries/{entryId}/attachments (10 MB).
	ledgerAttachmentPath = regexp.MustCompile(`^/v1/agent/organisations/[^/]+/ledger/entries/[^/]+/attachments$`)
	// maintenancePhotoPath is POST .../maintenance/{requestId}/photos, a
	// manager's or a tenant's (10 MB, as onboarding documents).
	maintenancePhotoPath = regexp.MustCompile(`^/v1/(agent/organisations|me/tenancies)/[^/]+/maintenance/[^/]+/photos$`)
	// inspectionPhotoPath is POST .../inspections/{inspectionId}/items/{itemId}/photos (10 MB).
	inspectionPhotoPath = regexp.MustCompile(`^/v1/agent/organisations/[^/]+/inspections/[^/]+/items/[^/]+/photos$`)
	// developmentPhotoPath is POST .../developments/{developmentId}/photos
	// (a development photo: the listing photo limit, API 0.44.0).
	developmentPhotoPath = regexp.MustCompile(`^/v1/agent/organisations/[^/]+/developments/[^/]+/photos$`)
)

func isContactImport(r *http.Request) bool {
	return r.Method == http.MethodPost && contactImportPath.MatchString(r.URL.Path)
}

func isPDFUpload(r *http.Request) bool {
	return (r.Method == http.MethodPut && agreementDocumentPath.MatchString(r.URL.Path)) || (r.Method == http.MethodPost && r.URL.Path == "/v1/verify")
}

func isPhotoUpload(r *http.Request) bool {
	return r.Method == http.MethodPost && (photoPath.MatchString(r.URL.Path) || developmentPhotoPath.MatchString(r.URL.Path))
}

func isLogoUpload(r *http.Request) bool {
	return r.Method == http.MethodPut && logoPath.MatchString(r.URL.Path)
}

func isUpload(r *http.Request) bool {
	return (r.Method == http.MethodPost && uploadPath.MatchString(r.URL.Path)) || isLogoUpload(r) || isPhotoUpload(r) || isPDFUpload(r) ||
		isContactImport(r) || (r.Method == http.MethodPost && applicationFilePath.MatchString(r.URL.Path)) ||
		(r.Method == http.MethodPost && ledgerAttachmentPath.MatchString(r.URL.Path)) ||
		(r.Method == http.MethodPost && maintenancePhotoPath.MatchString(r.URL.Path)) ||
		(r.Method == http.MethodPost && inspectionPhotoPath.MatchString(r.URL.Path))
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
	// SavedSearches holds saved searches and their alert state (API 0.52.0).
	// Nil: the routes answer 404 (a test double without them).
	SavedSearches savedsearches.Store
	// DataExports holds personal data export requests (API 0.55.0). Nil: no
	// export routes (a test double without them).
	DataExports dataexport.Store
	// EmailDelivery is Resend's delivery webhook, the suppressions and the
	// administrators' email figures (API 0.59.0). Nil: no routes.
	EmailDelivery *emaildelivery.Service
	// Uploads holds upload intents (API 0.60.0). Nil: no intent routes, and
	// photos are uploaded through the API only.
	Uploads uploads.Store

	// Analytics holds the raw analytics events and the daily rollups (API
	// 0.30.0). The raw retention is Config.AnalyticsRawRetentionDays.
	Analytics analytics.Store

	// CRM holds the contact books (API 0.31.0); CRMItems reads the property
	// requests, business enquiries and appointments contacts are linked to
	// (crm.PostgresItems, or MemoryCRMItems without a database).
	CRM      crm.Store
	CRMItems crm.Items

	// Applications holds the questionnaires, listing intakes and
	// applications (API 0.32.0); their files are kept in Files.
	Applications applications.Store

	// Venues holds the hourly settings, quotes and hourly bookings of the
	// showrooms and venues (API 0.33.0).
	Venues venues.Store

	// Ledger holds the agent books (API 0.34.0); their attachments are kept
	// in Files.
	Ledger ledger.Store
	// Branches holds organisation branches and their pins and ties (API
	// 0.57.0). Nil: no branch routes.
	Branches branches.Store

	// Tenancies holds the properties, units and tenancies (API 0.36.0).
	Tenancies tenancies.Store

	// Maintenance holds the maintenance requests (API 0.36.2); their photos
	// are kept in Files.
	Maintenance maintenance.Store

	// Inspections holds the property inspections (API 0.36.3); their photos
	// are kept in Files.
	Inspections inspections.Store

	// Reviews holds the reviews of service providers (API 0.36.4).
	Reviews reviews.Store

	// Packages holds the listing packages and their assignments (API
	// 0.37.0). The listing store must check the quota on a go-live: the
	// Postgres listing store always does (packages.EnforceTx); the memory one
	// once MemoryPackages has been called on it.
	Packages packages.Store

	// Payments holds the online payments of subscription invoices (API
	// 0.42.0); nil means an in-memory store. PaymentGateway is the provider;
	// nil means the one PAYMENT_GATEWAY configures (none: online payment is
	// not offered).
	Payments       payments.Store
	PaymentGateway paygateway.Gateway

	// Legal holds the per-market legal document versions, LegalAcceptances
	// the acceptances (user_consents) and Tax the tax rules (API 0.43.0).
	// nil Legal / Tax: seeded memory stores; nil LegalAcceptances: the
	// memory identity store's consents, or Legal when it implements
	// legal.AcceptanceStore (the Postgres store does).
	Legal            legal.Store
	LegalAcceptances legal.AcceptanceStore
	Tax              tax.Store

	// Developments holds the development showcase (API 0.44.0); nil means
	// an empty memory store.
	Developments developments.Store

	// Invoices holds the finance administrators' renewal invoices (API
	// 0.47.0); nil means a memory store.
	Invoices invoices.Store

	// Inbox is the notification centre (API 0.35.0): devices, in-app
	// notifications, preferences and the delivery health. Logging out
	// removes the session's devices through it.
	Inbox *inapp.Service

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
	// Listing packages and quotas (FR-008): the approval of a NEW review is
	// refused when the advertiser's package is full; the views show the usage.
	if d.Packages == nil {
		return nil, errors.New("app: no listing packages store")
	}
	packagesSvc := packages.NewService(packages.Deps{Store: d.Packages, Markets: everyMarket{store: d.Markets}, Logger: logger, Now: d.Now})
	packagesH := packages.NewHandler(packagesSvc, logger)
	manager := listings.NewManager(listingsSvc, manageStore, d.Files, logger)
	manager.SetQuota(packagesSvc)
	manageH := listings.NewManageHandler(manager, logger)
	var uploadsSvc *uploads.Service
	if d.Uploads != nil {
		uploadsSvc = uploads.NewService(uploads.Deps{Store: d.Uploads, Storage: d.Files, Logger: logger, Now: d.Now})
		manageH.SetUploads(uploadsSvc)
	}
	// Listing reports (API 0.54.0): anyone reports, the listing reviewers
	// decide; both listing stores keep them.
	var listingReportsH *listings.ReportHandler
	if store, ok := manageStore.(listings.ReportStore); ok {
		listingReportsH = listings.NewReportHandler(listings.NewReports(store, listingsSvc, manager, logger), logger)
	}
	reportLimiter := httpx.NewRateLimiter(5, 5)
	listingLimiter := httpx.NewRateLimiter(listingChangesPerMinute, listingChangesBurst)
	// Saved listings: a customer's hearts. Whether a listing may be saved
	// (public right now) is the listings service's own rule.
	if d.Saved == nil {
		return nil, errors.New("app: no saved-listings store")
	}
	savedH := saved.NewHandler(saved.NewService(d.Saved, listingsSvc, d.Now), logger)
	// Personal data export (API 0.55.0): the API takes requests and serves
	// the archive; the worker builds it.
	var dataExportH *dataexport.Handler
	if d.DataExports != nil {
		dataExportH = dataexport.NewHandler(dataexport.NewService(d.DataExports, d.Files, nil, logger, d.Now), logger)
	}
	// Saved searches (API 0.52.0): validated with the search's own parser.
	var savedSearchesH *savedsearches.Handler
	if d.SavedSearches != nil {
		savedSearchesH = savedsearches.NewHandler(savedsearches.NewService(savedsearches.Deps{
			Store: d.SavedSearches, Checker: searchQueries{svc: listingsSvc}, Now: d.Now}), logger)
	}
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
	// Questionnaires and applications (BR-010, FR-015, FR-017..019). A
	// listing says whether it takes applications (`applications`).
	if d.Applications == nil {
		return nil, errors.New("app: no applications store")
	}
	applicationsSvc := ApplicationsService(d.Applications, d.Files, listingsSvc, d.Listings, d.Identity, logger, d.Now)
	listingsSvc.SetIntake(applicationIntake{svc: applicationsSvc})
	applicationsH := applications.NewHandler(applicationsSvc, logger)
	applicationLimiter := httpx.NewRateLimiter(applicationChangesPerMinute, applicationChangesBurst)
	// Hourly rental of showrooms and venues (BR-007, FR-022..025): enquiry
	// mode only, a table and service type of its own (VENUE_HOURLY).
	if d.Venues == nil {
		return nil, errors.New("app: no venue store")
	}
	venuesSvc := VenuesService(d.Venues, listingsSvc, d.Listings, d.Markets, d.Reference, logger, d.Now)
	listingsSvc.SetHourlyRates(venueRates{svc: venuesSvc})
	venuesH := venues.NewHandler(venuesSvc, logger)
	// Calendar downloads (API 0.39.0): the web app's pages and the platform
	// sender as ORGANIZER, never a party's address.
	venuesH.SetCalendar(venues.CalendarConfig{PublicWebURL: cfg.PublicWebURL, Organizer: ics.Organizer(cfg.EmailFrom)})
	venueQuoteLimiter := httpx.NewRateLimiter(venueQuotesPerMinute, venueQuotesBurst)
	venueLimiter := httpx.NewRateLimiter(venueChangesPerMinute, venueChangesBurst)
	orgVenues, ownerVenues := venuesH.Organisation("id"), venuesH.DirectOwner()
	// The agent ledger / books (BR-012, FR-026..029): a cashbook per
	// organisation (or direct owner), per currency, never consolidated.
	if d.Ledger == nil {
		return nil, errors.New("app: no ledger store")
	}
	// Properties, units and tenancies (BR-019, FR-047): the same books as the
	// CRM and the ledger; tenants read their own through /v1/me/tenancies.
	// Rent (API 0.36.1): a receipt posts its RENT_RECEIVED entry through the
	// ledger in its own transaction, a ledger reversal of that entry undoes
	// the receipt (ConnectLedger), and the ledger's receivables report
	// includes the rent arrears.
	if d.Tenancies == nil {
		return nil, errors.New("app: no tenancy store")
	}
	if err := tenancies.ConnectLedger(d.Tenancies, d.Ledger); err != nil {
		return nil, err
	}
	tenancySvc := TenanciesService(d.Tenancies, d.CRM, d.Listings, d.Leases, d.Markets, d.Identity, logger, d.Now)
	ledgerSvc := LedgerService(d.Ledger, d.Files, d.CRM, d.Listings, d.Leases, d.Markets, logger, d.Now)
	ledgerSvc.SetRentArrears(tenancySvc.LedgerArrears)
	tenancySvc.SetLedger(ledgerSvc)
	ledgerH := ledger.NewHandler(ledgerSvc, logger)
	ledgerLimiter := httpx.NewRateLimiter(ledgerChangesPerMinute, ledgerChangesBurst)
	tenanciesH := tenancies.NewHandler(tenancySvc, logger)
	tenancyLimiter := httpx.NewRateLimiter(tenancyChangesPerMinute, tenancyChangesBurst)
	directorySvc := directory.NewService(d.Directory, vocab, listingsSvc)
	directorySvc.SetPlaces(geography)
	// Logos are kept in the object storage and served by the API itself.
	directorySvc.SetFiles(d.Files, logger)
	if d.Now != nil {
		directorySvc.SetClock(d.Now)
	}
	// Directory profiles show the rating of their published reviews (API 0.36.4).
	if d.Reviews == nil {
		return nil, errors.New("app: no reviews store")
	}
	directorySvc.SetRatings(DirectoryRatings(d.Reviews))
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
		propertyrequests.WithRouteTargets(routeTargets{identity: d.Identity, markets: d.Markets}),
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
	// Administrator MFA (API 0.40.0): the sealing key and ADMIN_MFA_REQUIRED.
	mfaBox, err := AdminMFABox(cfg, logger)
	if err != nil {
		return nil, err
	}
	identityOpts = append(identityOpts, identity.WithAdminMFA(mfaBox, cfg.AdminMFARequired))
	identitySvc, err := identity.NewService(d.Identity, d.Mailer, logger, identityOpts...)
	if err != nil {
		return nil, err
	}

	secure := cfg.Env != config.EnvDevelopment
	identityH := identity.NewHandler(identitySvc, logger, secure)
	// Account security (API 0.50.0): the email-change emails.
	if sm, ok := d.Mailer.(identity.SecurityMailer); ok {
		identitySvc.SetSecurityMailer(sm)
	}
	if d.Inbox == nil {
		return nil, errors.New("app: no notification centre")
	}
	// Signing out removes the push devices the session registered (FR-045).
	identityH.SetLogoutHook(d.Inbox.OnLogout)
	inboxH := inapp.NewHandler(d.Inbox, logger)
	// Malware scanning of uploads (API 0.41.0): the administrators' view of
	// the quarantined and infected files. Files is a *malware.Guard when
	// scanning is wired (cmd/api); otherwise the summary is empty.
	fileScans, _ := d.Files.(*malware.Guard)
	fileScansH := malware.NewHandler(fileScans, logger)
	// Countries with their listing counts: every country can be used, and
	// ?has_listings=true is what the public's country lists show.
	marketsH := markets.NewHandler(d.Markets, listingsSvc.CountByCountry, logger)
	// Appointments: routed with the request and enquiry rules, named from the
	// public catalogue, shown in the market's time zone.
	appointmentsSvc := appointments.NewService(appointments.Deps{
		Store: d.Appointments, Listings: requestStore, Profiles: d.Enquiries,
		Catalogue: listingCards{store: d.Listings}, Zones: marketZones{markets: d.Markets},
		People: people{identity: d.Identity}, Vocabulary: vocab, Now: d.Now,
		// Availability, services and slots (API 0.51.0): both stores have them.
		Availability: availabilityOf(d.Appointments),
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
	// The development showcase (API 0.44.0, BR-019 / FR-048): projects of
	// the developers' profiles, enquiries to the developer through the
	// business enquiries (quoting the project), on the developer's profile
	// page and in the sitemap.
	developmentsSvc := DevelopmentsService(DevelopmentsDeps{Store: d.Developments, Files: d.Files, Listings: listingsSvc,
		Directory: d.Directory, Labels: referenceSvc, Places: geography, Inbox: enquiriesSvc, Audit: d.AdminAudit,
		Enquiries: d.Enquiries, Logger: logger, Now: d.Now})
	enquiriesSvc.SetDevelopments(developmentsSvc)
	directorySvc.SetDevelopments(developmentProjects(developmentsSvc))
	developmentsH := developments.NewHandler(developmentsSvc, logger)
	// Maintenance requests (API 0.36.2): the tenancies' books, a cost posts
	// its EXPENSE entry through the ledger in its own transaction, and an
	// assignment sends the provider a business enquiry.
	if d.Maintenance == nil {
		return nil, errors.New("app: no maintenance store")
	}
	if err := maintenance.ConnectLedger(d.Maintenance, d.Ledger); err != nil {
		return nil, err
	}
	maintenanceH := maintenance.NewHandler(MaintenanceService(MaintenanceDeps{Store: d.Maintenance, Tenancies: tenancySvc, Ledger: ledgerSvc,
		LedgerStore: d.Ledger, Reference: referenceSvc, Markets: d.Markets, Directory: d.Directory, Enquiries: enquiriesSvc, Files: d.Files,
		Logger: logger, Now: d.Now}), logger)
	// Inspections (API 0.36.3): the same books and roles; checklists from
	// reference data, inspectors among the members, photos in Files.
	if d.Inspections == nil {
		return nil, errors.New("app: no inspections store")
	}
	inspectionsH := inspections.NewHandler(InspectionsService(InspectionsDeps{Store: d.Inspections, Tenancies: tenancySvc,
		Reference: referenceSvc, Teams: d.Teams, Files: d.Files, Logger: logger, Now: d.Now}), logger)
	// Reviews of service providers (API 0.36.4): the tenancies' books,
	// public directory profiles, maintenance requests and enquiries.
	reviewsH := reviews.NewHandler(ReviewsService(ReviewsDeps{Store: d.Reviews, Tenancies: tenancySvc, Directory: d.Directory,
		Maintenance: d.Maintenance, Enquiries: d.Enquiries, Logger: logger, Now: d.Now}), logger)
	appointmentsH := appointments.NewHandler(appointmentsSvc, inboxCounts{requests: requestsSvc, enquiries: enquiriesSvc}, logger)
	appointmentsH.SetCalendar(appointments.CalendarConfig{PublicWebURL: cfg.PublicWebURL, Organizer: ics.Organizer(cfg.EmailFrom)})
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
	// Branches (API 0.57.0): OWNER and MANAGER manage them; any member reads.
	var branchesH *branches.Handler
	if d.Branches != nil {
		branchesH = branches.NewHandler(branches.NewService(d.Branches, d.Now), logger)
	}
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
	// Featured and sponsored placements (API 0.53.0): both listing stores
	// keep them; a test double without them has no placement routes.
	var placementsH *listings.PlacementHandler
	if store, ok := d.Listings.(listings.PlacementStore); ok {
		placementsH = listings.NewPlacementHandler(listings.NewPlacements(store, d.Listings, placementQuota(subscriptionsSvc), d.Now), logger)
	}
	// A paid subscription grants its plan's listing package (API 0.42.0).
	LinkSubscriptionPackages(d.Subscriptions, d.Packages)
	// Online payment (API 0.42.0): off unless PAYMENT_GATEWAY names a driver.
	gateway := d.PaymentGateway
	if gateway == nil {
		var err error
		if gateway, err = paygateway.Open(cfg, logger, d.Now); err != nil {
			return nil, err
		}
	}
	paymentStore := d.Payments
	if paymentStore == nil {
		memAudit, _ := d.AdminAudit.(*adminaudit.MemoryLog)
		paymentStore = payments.NewMemoryStore(memAudit)
	}
	if gateway != nil {
		subscriptionsSvc.SetOnlinePayment(gateway.Name())
	}
	// Renewal invoices (API 0.47.0): an invoice partly paid offline is paid
	// off offline (the guard refuses its online checkout).
	var invoicesSvc *invoices.Service
	paymentsSvc := payments.NewService(payments.Deps{Store: paymentStore, Subscriptions: subscriptionsSvc, Gateway: gateway,
		PublicWebURL: cfg.PublicWebURL, ReconcileAfter: cfg.PaymentReconcileAfter, Logger: logger, Now: d.Now,
		CheckoutGuard: func(ctx context.Context, id uuid.UUID) error { return invoicesSvc.CheckoutGuard(ctx, id) }})
	paymentsH := payments.NewHandler(paymentsSvc, logger)
	subscriptionsH := subscriptions.NewHandler(subscriptionsSvc, logger)
	accountsH := accounts.NewHandler(accounts.NewService(accounts.Deps{Store: d.Accounts, Audit: d.AdminAudit, Mailer: accountMailer,
		Subscriptions: AccountSubscriptions(d.Subscriptions, d.Now), Logger: logger, Now: d.Now}), logger, secure)
	// Legal documents, tax rules and translations (API 0.43.0).
	legalStore, legalAcceptances, taxStore := legalStores(d)
	legalSvc := legal.NewService(legal.Deps{Store: legalStore, Acceptances: legalAcceptances,
		KnownMarket: knownMarket(everyMarket{store: d.Markets}), Now: d.Now})
	legalH := legal.NewHandler(legalSvc, logger)
	// Market administration (API 0.56.0): both market stores have it.
	var marketsAdminH *markets.AdminHandler
	if store, ok := d.Markets.(markets.AdminStore); ok {
		marketsAdminH = markets.NewAdminHandler(markets.NewAdmin(store, marketChecker{packages: d.Packages, legal: legalSvc}, d.Now), logger)
	}
	identityH.SetPendingLegalHook(func(r *http.Request, userID uuid.UUID) (any, error) { return legalH.PendingViews(r, userID) })
	taxSvc := tax.NewService(taxStore, knownMarket(everyMarket{store: d.Markets}), d.Now)
	taxH := tax.NewHandler(taxSvc, logger)
	invoicesSvc = InvoicesService(d.Invoices, subscriptionsSvc, taxSvc, d.Markets, gateway, logger, d.Now)
	invoicesH := invoices.NewHandler(invoicesSvc, logger, cfg.PublicWebURL)
	referenceAdminH := reference.NewAdminHandler(reference.NewAdmin(referenceSvc, referenceAdminStore, d.Now), logger)
	reportsH := reports.NewHandler(d.Reports, marketCodes(d.Markets), d.Now, logger)
	adminLimiter := httpx.NewRateLimiter(adminChangesPerMinute, adminChangesBurst)

	requestLimiter := httpx.NewRateLimiter(propertyRequestsPerMinute, propertyRequestsBurst)
	enquiryLimiter := httpx.NewRateLimiter(businessEnquiriesPerMinute, businessEnquiriesBurst)
	reviewLimiter := httpx.NewRateLimiter(reviewChangesPerMinute, reviewChangesBurst)
	authLimiter := httpx.NewRateLimiter(AuthRequestsPerMinute, AuthRequestsBurst)
	adminAuthLimiter := httpx.NewRateLimiter(AdminAuthRequestsPerMinute, AdminAuthRequestsBurst)
	captchaLimiter := httpx.NewRateLimiter(CaptchaRequestsPerMinute, CaptchaRequestsBurst)
	codeLimiter := httpx.NewRateLimiter(SignInCodeRequestsPerMinute, SignInCodeRequestsBurst)
	adminCodeLimiter := httpx.NewRateLimiter(SignInCodeRequestsPerMinute, SignInCodeRequestsBurst)

	r := chi.NewRouter()
	r.Use(
		httpx.RequestID,
		// The language of messages (API 0.43.0): ?lang, else Accept-Language, else en.
		i18n.Middleware,
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
		func(next http.Handler) http.Handler {
			authenticated := authn.Authenticate(next)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if skipAuthentication(r.URL.Path) {
					next.ServeHTTP(w, r)
					return
				}
				authenticated.ServeHTTP(w, r)
			})
		},
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
		// A country's active listing packages with their prices, for the
		// pricing page (public, cacheable; API 0.37.0).
		r.Get("/markets/{code}/packages", packagesH.Public)

		// Languages, and the per-market legal documents with their history
		// (API 0.43.0). Public and cacheable; ?preview=true is for
		// administrators (private).
		r.Get("/i18n/languages", languagesHandler)
		r.Get("/legal", legalH.List)
		r.Get("/legal/{kind}", legalH.Get)
		r.Get("/legal/{kind}/versions", legalH.Versions)

		// Reference data behind every drop-down, filter and list. Public and cacheable.
		r.Get("/reference", referenceH.All)
		r.Get("/reference/{set}", referenceH.One)

		// Online payment (API 0.42.0): the provider's signed webhooks, and the
		// fake provider's hosted page (PAYMENT_GATEWAY=fake only). No session.
		r.Post("/payments/webhooks/{provider}", paymentsH.Webhook)
		// Email delivery (API 0.59.0): Resend's signed webhooks. No session;
		// 503 while RESEND_WEBHOOK_SECRET is unset.
		if d.EmailDelivery != nil {
			r.Post("/webhooks/email/resend", emaildelivery.NewHandler(d.EmailDelivery, logger).Webhook)
		}
		if _, fake := gateway.(*paygateway.Fake); fake {
			r.Get("/payments/fake/checkout/{ref}", paymentsH.FakeCheckout)
			r.Post("/payments/fake/checkout/{ref}/{action}", paymentsH.FakeComplete)
		}

		// Open to guests and signed-in customers alike.
		r.With(requestLimiter.Middleware).Post("/property-requests", requestsH.Create)

		// Public catalogue. These never read the session and are cacheable.
		r.Get("/listings", listingsH.List)
		r.Get("/listings/suggest", listingsH.Suggest)
		r.Get("/listings/{slug}", listingsH.Get)
		if listingReportsH != nil {
			// Anyone may report a public listing (API 0.54.0): 5 a minute per IP.
			r.With(reportLimiter.Middleware).Post("/listings/{slug}/reports", listingReportsH.Report)
		}
		// A short stay's calendar as the public sees it: available or not
		// per night, never why.
		r.Get("/listings/{slug}/availability", availabilityH.Public)
		// The questionnaire of a listing, for the preview (public; 404 when
		// the listing takes no applications).
		r.Get("/listings/{slug}/application-form", applicationsH.PublicForm)
		// Hourly rental (public): the availability in the venue's local
		// time, and a stored, itemised quote (enquiry mode: no payment).
		r.Get("/venues/{listingId}/availability", venuesH.Availability)
		r.With(venueQuoteLimiter.Middleware).Post("/venues/{listingId}/quotes", venuesH.Quote)
		r.Get("/directory", directoryH.Kinds)
		r.Get("/directory/{kind}", directoryH.List)
		r.Get("/directory/{kind}/{slug}", directoryH.Get)
		// A public profile's logo: an image, no session, no cookie.
		r.Get("/directory/{kind}/{slug}/logo", directoryH.Logo)
		// A profile's published reviews with the provider's replies (public,
		// cacheable; API 0.36.4).
		r.Get("/directory/{kind}/{slug}/reviews", reviewsH.Public)
		// A listing photo: public (and cacheable for a year with its ?v=) while
		// the listing and the photo are public; otherwise only for the
		// listing's managers and reviewers (404 for everyone else).
		r.Get("/listing-photos/{photoId}/{size}", manageH.Photo)
		r.Get("/sitemap", directoryH.Sitemap)
		// The development showcase (API 0.44.0): public, cacheable, no
		// contact details; a project's photos as the listing photos.
		r.Get("/developments", developmentsH.List)
		r.Get("/developments/{market}/{slug}", developmentsH.Get)
		r.Get("/development-photos/{photoId}/{size}", developmentsH.Photo)
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
		// Marking an enquiry as replied to (API 0.36.4): whoever reads it in
		// their inbox; another organisation's is 404.
		r.With(auth.RequireMembership(), reviewLimiter.Middleware).Post("/inbox/business-enquiries/{enquiryId}/replied", enquiriesH.MarkReplied)

		// Reviews of service providers (API 0.36.4). A customer's own
		// (basis ENQUIRY), and reporting a published review (anybody signed
		// in, one open report per person and review).
		r.Route("/me/reviews", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			reviewsH.MyRoutes(r, reviewLimiter.Middleware)
		})
		r.With(auth.RequireAuth, reviewLimiter.Middleware).Post("/reviews/{id}/report", reviewsH.Report)

		// The question of the registration form, in its own bucket so that
		// asking for one does not use up the sign-in allowance.
		r.Route("/auth", func(r chi.Router) {
			// The device label of the sessions signing in starts (API 0.50.0).
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					next.ServeHTTP(w, r.WithContext(identity.WithClient(r.Context(), r.UserAgent())))
				})
			})
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
		// Account security (API 0.50.0): change the password, the signed-in
		// devices, change the email address (confirmed from the new address).
		r.With(auth.RequireAuth, authLimiter.Middleware).Post("/me/password", identityH.ChangePassword)
		r.With(auth.RequireAuth).Get("/me/sessions", identityH.Sessions)
		r.With(auth.RequireAuth).Delete("/me/sessions/{id}", identityH.RevokeSession)
		r.With(auth.RequireAuth).Post("/me/sessions/revoke-others", identityH.RevokeOtherSessions)
		r.With(auth.RequireAuth, authLimiter.Middleware).Post("/me/email", identityH.RequestEmailChange)
		r.With(authLimiter.Middleware).Post("/auth/email-change/confirm", identityH.ConfirmEmailChange)
		// The notification centre: devices, in-app notifications and
		// preferences (API 0.35.0).
		settingsLimiter := newSettingsLimiter()
		notificationRoutes(r, inboxH, settingsLimiter)
		// Marketing preferences (API 0.37.1): the current choice with its
		// newest records; a change is recorded with the X-Client source.
		// Writes share the settings bucket with notification preferences.
		// One marketing consent wording by version (public, cacheable; rows
		// never change).
		r.Get("/marketing-consent-texts/{version}", identityH.MarketingConsentText)
		r.Route("/me/marketing-preferences", func(r chi.Router) {
			r.Use(httpx.PrivateNoStore, auth.RequireAuth)
			r.Get("/", identityH.MarketingPreferences)
			r.With(settingsLimiter.Middleware).Put("/", identityH.SetMarketingPreferences)
		})
		// Legal acceptances (API 0.43.0): the person's own, and accepting a
		// published version (re-acceptance). Writes share the settings bucket.
		r.Route("/me/legal-acceptances", func(r chi.Router) {
			r.Use(httpx.PrivateNoStore, auth.RequireAuth)
			r.Get("/", legalH.MyAcceptances)
			r.With(settingsLimiter.Middleware).Post("/", legalH.Accept)
		})
		// Deleting one's own account, confirmed with the password. It shares
		// the sign-in bucket (a password is checked).
		r.With(auth.RequireAuth, authLimiter.Middleware).Delete("/me", accountsH.DeleteSelf)

		// Subscriptions: the plans the caller may take, and their own
		// subscriptions and those of the organisations they own (404 for
		// anybody else's). There is no online payment: a request is invoiced.
		r.With(auth.RequireAuth).Get("/me/subscription-plans", subscriptionsH.AvailablePlans)
		// Renewal invoices (API 0.47.0): the issued invoices of the caller
		// and of the organisations they own or manage (404 for anybody else's).
		r.Route("/me/invoices", func(r chi.Router) {
			r.Use(auth.RequireAuth, httpx.PrivateNoStore)
			r.Get("/", invoicesH.Mine)
			r.Get("/{id}", invoicesH.MineOne)
		})
		r.Route("/me/subscriptions", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", subscriptionsH.Mine)
			r.With(adminLimiter.Middleware).Post("/", subscriptionsH.Request)
			r.Get("/{id}", subscriptionsH.MineOne)
			r.With(adminLimiter.Middleware).Post("/{id}/cancel", subscriptionsH.CancelPending)
			r.With(adminLimiter.Middleware).Post("/{id}/renew", subscriptionsH.Renew)
			// Online payment (API 0.42.0): 404 ONLINE_PAYMENT_NOT_OFFERED when off.
			r.With(adminLimiter.Middleware).Post("/{id}/checkout", paymentsH.Checkout)
			r.Get("/{id}/payments", paymentsH.Mine)
		})

		// One role-aware summary for the signed-in account: which dashboard
		// applies, counters, next actions and recent items, each scoped as
		// the endpoint it summarises.
		r.With(auth.RequireAuth).Get("/me/dashboard", dashboardH.Me)

		// The listing package and quota usage of the caller's own
		// direct-owner account (API 0.37.0; 403 for anybody else).
		r.With(auth.RequireAuth).Get("/me/package", packagesH.MyUsage)

		// The requests this account sent while signed in, as their customer
		// sees them: never the lead stage, an internal note or who handles it.
		r.Route("/me/property-requests", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", requestsH.Mine)
			r.Get("/{id}", requestsH.MineOne)
			// The conversation with the receiving side (API 0.46.0): the
			// customer answers once the agent or seller has written.
			r.Get("/{id}/messages", requestsH.CustomerMessages)
			r.With(requestWorkLimiter.Middleware).Post("/{id}/messages", requestsH.PostCustomerMessage)
		})
		// The lead funnel and response-time KPIs of the requests the caller
		// received as a direct seller (API 0.38.0, BR-005). Who may call it is
		// who may open the request inbox.
		r.With(auth.RequireAuth).Get("/me/reports/lead-funnel", requestsH.MyLeadFunnel)

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
		// Personal data export (API 0.55.0): one a day; the archive streams
		// to its owner only.
		if dataExportH != nil {
			r.With(auth.RequireAuth).Get("/me/data-export", dataExportH.Latest)
			r.With(auth.RequireAuth, authLimiter.Middleware).Post("/me/data-export", dataExportH.Request)
			r.With(auth.RequireAuth, httpx.PrivateNoStore).Get("/me/data-export/download", dataExportH.Download)
		}
		// Saved searches and alerts (API 0.52.0, FR-042).
		if savedSearchesH != nil {
			r.Route("/me/saved-searches", func(r chi.Router) {
				r.Use(auth.RequireAuth)
				r.Get("/", savedSearchesH.List)
				r.With(listingLimiter.Middleware).Post("/", savedSearchesH.Create)
				r.With(listingLimiter.Middleware).Patch("/{id}", savedSearchesH.Update)
				r.With(listingLimiter.Middleware).Delete("/{id}", savedSearchesH.Delete)
			})
			r.With(authLimiter.Middleware).Post("/saved-searches/unsubscribe", savedSearchesH.Unsubscribe)
		}

		// The lister's side: the listings the caller may manage, creating and
		// editing them with their photos and sending them for review, and the
		// availability calendar and stay rules of the short stays among them.
		// The handlers decide who may do what (auth/capabilities.go): 403 for a
		// caller who manages no listings (or may only read this one), 404 for
		// a listing that is not theirs.
		// Upload intents (API 0.60.0): authorise one upload, PUT it straight to
		// private storage (or through here with local storage), complete it.
		if uploadsSvc != nil {
			uploadsH := uploads.NewHandler(uploadsSvc, logger)
			r.Route("/media/upload-intents", func(r chi.Router) {
				r.Use(auth.RequireAuth, httpx.PrivateNoStore)
				r.With(uploadLimiter.Middleware).Post("/", uploadsH.Create)
				r.Get("/{intentId}", uploadsH.Get)
				r.With(uploadLimiter.Middleware).Put("/{intentId}/content", uploadsH.Content)
				r.With(uploadLimiter.Middleware).Post("/{intentId}/complete", uploadsH.Complete)
			})
		}
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
			// The lifecycle (API 0.48.0): pause and resume, mark sold or let, close.
			r.With(listingLimiter.Middleware).Post("/{id}/pause", manageH.Pause)
			r.With(listingLimiter.Middleware).Post("/{id}/resume", manageH.Resume)
			r.With(listingLimiter.Middleware).Post("/{id}/mark-sold", manageH.MarkSold)
			r.With(listingLimiter.Middleware).Post("/{id}/mark-let", manageH.MarkLet)
			r.With(listingLimiter.Middleware).Post("/{id}/close", manageH.Close)
			r.With(uploadLimiter.Middleware).Post("/{id}/photos", manageH.AddPhoto)
			r.With(listingLimiter.Middleware).Put("/{id}/photos/order", manageH.OrderPhotos)
			r.With(listingLimiter.Middleware).Patch("/{id}/photos/{photoId}", manageH.PatchPhoto)
			r.With(listingLimiter.Middleware).Delete("/{id}/photos/{photoId}", manageH.DeletePhoto)
			r.Get("/{id}/availability", availabilityH.Calendar)
			r.With(calendarLimiter.Middleware).Put("/{id}/availability", availabilityH.Change)
			r.With(calendarLimiter.Middleware).Patch("/{id}/stay-rules", availabilityH.StayRules)
			// The listing's questionnaire and intake (whoever may edit the listing).
			r.Get("/{id}/applications-intake", applicationsH.GetIntake)
			r.With(applicationLimiter.Middleware).Put("/{id}/applications-intake", applicationsH.PutIntake)
			// The listing's hourly settings (whoever may edit the listing;
			// 400 for a property type that is not hourly_bookable).
			r.Get("/{id}/hourly", venuesH.GetSettings)
			r.With(venueLimiter.Middleware).Put("/{id}/hourly", venuesH.PutSettings)
		})

		// A tenant's own tenancies (FR-047): matched by their verified email
		// to a tenant contact of the tenancy's book; anybody else's is 404.
		r.Route("/me/tenancies", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", tenanciesH.MyTenancies)
			r.Get("/{id}", tenanciesH.MyTenancy)
			r.Get("/{id}/dues", tenanciesH.MyDues)
			r.Get("/{id}/receipts", tenanciesH.MyReceipts)
			r.Get("/{id}/statement", tenanciesH.MyStatement)
			// Maintenance (API 0.36.2): the requests of the tenant's own
			// tenancy; another tenancy's or another request is 404.
			r.Route("/{id}/maintenance", func(r chi.Router) { maintenanceH.MyRoutes(r, tenancyLimiter.Middleware) })
			// Inspections (API 0.36.3): the completed inspections of the
			// tenant's own tenancy, read-only; anything else is 404.
			r.Route("/{id}/inspections", inspectionsH.MyRoutes)
		})

		// A customer's hourly bookings (FR-025): another customer's is 404.
		r.Route("/me/venue-bookings", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", venuesH.ListMine)
			r.With(venueLimiter.Middleware).Post("/", venuesH.Request)
			r.Get("/{bookingId}", venuesH.GetMine)
			r.Get("/{bookingId}/calendar.ics", venuesH.CalendarMine)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/cancel", venuesH.CancelMine)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/reschedule", venuesH.RescheduleMine)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/accept", venuesH.AcceptMine)
		})
		// The hourly bookings of a direct owner's (landlord's or seller's)
		// own listings; 403 for anybody who is not an approved direct owner.
		r.Route("/me/hosted-venue-bookings", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", ownerVenues.List)
			r.Get("/{bookingId}", ownerVenues.Get)
			r.Get("/{bookingId}/calendar.ics", ownerVenues.Calendar)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/confirm", ownerVenues.Confirm)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/decline", ownerVenues.Decline)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/cancel", ownerVenues.Cancel)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/propose", ownerVenues.Propose)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/complete", ownerVenues.Complete)
			r.With(venueLimiter.Middleware).Post("/{bookingId}/no-show", ownerVenues.NoShow)
		})

		// A customer's own applications (FR-015): any customer-scope session;
		// another applicant's application is 404.
		r.Route("/me/applications", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", applicationsH.Mine)
			r.With(applicationLimiter.Middleware).Post("/", applicationsH.Start)
			r.Get("/{applicationId}", applicationsH.MineOne)
			r.With(applicationLimiter.Middleware).Patch("/{applicationId}", applicationsH.Autosave)
			r.With(uploadLimiter.Middleware).Post("/{applicationId}/files", applicationsH.Upload)
			r.Get("/{applicationId}/files/{fileId}", applicationsH.MyFile)
			r.With(applicationLimiter.Middleware).Delete("/{applicationId}/files/{fileId}", applicationsH.DeleteMyFile)
			r.With(applicationLimiter.Middleware).Post("/{applicationId}/submit", applicationsH.Submit)
			r.With(applicationLimiter.Middleware).Post("/{applicationId}/withdraw", applicationsH.Withdraw)
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
		// Availability (API 0.51.0): the slots a customer may choose (public,
		// never naming the professional), and the professional's own hours
		// and consultation services.
		r.With(venueQuoteLimiter.Middleware).Get("/appointments/slots", appointmentsH.Slots)
		r.With(auth.RequireAuth).Get("/me/appointment-settings", appointmentsH.MySettings)
		r.With(auth.RequireAuth, calendarLimiter.Middleware).Put("/me/appointment-settings", appointmentsH.SaveSettings)
		r.Route("/me/consultation-services", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", appointmentsH.MyServices)
			r.With(calendarLimiter.Middleware).Post("/", appointmentsH.CreateService)
			r.With(calendarLimiter.Middleware).Patch("/{id}", appointmentsH.UpdateService)
			r.With(calendarLimiter.Middleware).Delete("/{id}", appointmentsH.ArchiveService)
		})
		r.Route("/me/appointments", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			r.Get("/", appointmentsH.ListMine)
			r.Get("/{id}", appointmentsH.GetMine)
			r.Get("/{id}/calendar.ics", appointmentsH.CalendarMine)
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
			r.Get("/{id}/calendar.ics", appointmentsH.CalendarReceived)
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
			// An enrolment-only session (ADMIN_MFA_REQUIRED and no
			// authenticator yet) reaches /v1/admin/auth/* only: everything
			// else answers 403 MFA_ENROLMENT_REQUIRED (API 0.40.0).
			r.Use(auth.RefuseMFAEnrolmentOnly)
			r.With(adminAuthLimiter.Middleware).Post("/auth/login", identityH.AdminLogin)
			r.With(adminCodeLimiter.Middleware).Post("/auth/login/verify", identityH.AdminLoginVerify)
			r.With(adminCodeLimiter.Middleware).Post("/auth/login/resend", identityH.AdminLoginResend)
			// The second factor of an administrator with an authenticator,
			// in the same per-IP bucket as the emailed code.
			r.With(adminCodeLimiter.Middleware).Post("/auth/login/mfa", identityH.AdminLoginMFA)
			// The signed-in administrator's own MFA (an enrolment-only
			// session included). Changes share the administrators' bucket.
			r.Route("/auth/mfa", func(r chi.Router) {
				r.Use(httpx.PrivateNoStore, auth.RequireAdminSession)
				r.Get("/", identityH.MFAStatus)
				r.Group(func(r chi.Router) {
					r.Use(adminLimiter.Middleware)
					r.Post("/totp/setup", identityH.SetupTOTP)
					r.Post("/totp/confirm", identityH.ConfirmTOTP)
					r.Delete("/totp", identityH.RemoveTOTP)
					r.Post("/recovery-codes/regenerate", identityH.RegenerateRecoveryCodes)
				})
			})
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
			// Reviewing development projects (API 0.44.0): the listing
			// reviewers, a MARKET_ADMIN in its own markets; every decision
			// is in the admin audit.
			r.Route("/developments", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(auth.ListingReviewRoles()...))
				developmentsH.AdminRoutes(r, listingLimiter.Middleware)
			})
			// Market administration (API 0.56.0): SUPER_ADMIN and
			// PLATFORM_ADMIN; a MARKET_ADMIN edits its own markets (not their
			// status). Audited.
			// Email deliveries (API 0.59.0): SUPER_ADMIN, PLATFORM_ADMIN and
			// SUPPORT see the figures and suppressions and clear one (audited).
			if d.EmailDelivery != nil {
				emailH := emaildelivery.NewHandler(d.EmailDelivery, logger)
				r.Route("/email-deliveries", func(r chi.Router) {
					r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(emaildelivery.AdminRoles()...))
					r.Get("/", emailH.Overview)
					r.Get("/suppressions", emailH.Suppressions)
					r.With(listingLimiter.Middleware).Delete("/suppressions/{hash}", emailH.Clear)
				})
			}
			if marketsAdminH != nil {
				r.Route("/markets", func(r chi.Router) {
					r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(markets.AdminRoles()...))
					r.Get("/", marketsAdminH.List)
					r.Get("/{code}", marketsAdminH.Get)
					r.With(listingLimiter.Middleware).Post("/", marketsAdminH.Create)
					r.With(listingLimiter.Middleware).Patch("/{code}", marketsAdminH.Update)
				})
			}
			// Placements (API 0.53.0): SUPER_ADMIN, PLATFORM_ADMIN and
			// FINANCE_ADMIN everywhere, MARKET_ADMIN in its own markets;
			// every grant and end is audited.
			if placementsH != nil {
				r.Route("/placements", func(r chi.Router) {
					r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(listings.PlacementRoles()...))
					r.Get("/", placementsH.List)
					r.With(listingLimiter.Middleware).Post("/", placementsH.Grant)
					r.With(listingLimiter.Middleware).Post("/{id}/end", placementsH.End)
				})
			}
			// The listing report queue (API 0.54.0): the listing reviewers,
			// a MARKET_ADMIN in its own markets; every decision is audited.
			if listingReportsH != nil {
				r.Route("/listing-reports", func(r chi.Router) {
					r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(auth.ListingReviewRoles()...))
					r.Get("/", listingReportsH.List)
					r.With(listingLimiter.Middleware).Post("/{id}/dismiss", listingReportsH.Decide(listings.ReportDecisionDismiss))
					r.With(listingLimiter.Middleware).Post("/{id}/unpublish", listingReportsH.Decide(listings.ReportDecisionUnpublish))
					r.With(listingLimiter.Middleware).Post("/{id}/warn", listingReportsH.Decide(listings.ReportDecisionWarn))
				})
			}
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
				r.Group(func(r chi.Router) {
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
				// An administrator's MFA: read and reset by a SUPER_ADMIN only.
				r.Group(func(r chi.Router) {
					r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(auth.PlatformSuperAdmin))
					r.Get("/{id}/mfa", identityH.UserMFAStatus)
					r.With(adminLimiter.Middleware).Post("/{id}/mfa/reset", identityH.ResetUserMFA)
				})
				// A direct owner's listing package (API 0.37.0): the package
				// administrators (MARKET_ADMIN for its own markets).
				r.Group(func(r chi.Router) {
					r.Use(auth.RequirePlatformRole(packages.AdminRoles()...))
					packagesH.AccountRoutes(r, packages.KindDirectOwner, "/{id}", adminLimiter.Middleware)
				})
			})
			// Notification delivery health (FR-065): counts by status, the
			// dead-letter backlog and the oldest unattempted push.
			r.With(auth.RequirePlatformRole(inapp.AdminRoles()...)).Get("/notifications/health", inboxH.Health)
			// Malware scanning (API 0.41.0, BR-014): counts by status and the
			// latest PENDING and INFECTED files. SUPER_ADMIN and PLATFORM_ADMIN.
			r.With(httpx.PrivateNoStore, auth.RequirePlatformRole(FileScanAdminRoles()...)).Get("/file-scans", fileScansH.Summary)
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
				r.Get("/{id}/payments", paymentsH.AdminOf)
				r.Group(func(r chi.Router) {
					r.Use(auth.RequirePlatformRole(subscriptions.DecisionRoles()...), adminLimiter.Middleware)
					r.Post("/{id}/approve", subscriptionsH.Approve)
					r.Post("/{id}/refund", paymentsH.Refund)
					r.Post("/{id}/reject", subscriptionsH.Reject)
					r.Post("/{id}/cancel", subscriptionsH.Cancel)
				})
			})

			// Renewal invoices (API 0.47.0): read like subscriptions; drafted,
			// issued, paid, credited, voided and disputed by SUPER_ADMIN and
			// FINANCE_ADMIN.
			r.Route("/invoices", func(r chi.Router) {
				r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(invoices.AdminRoles()...))
				invoicesH.AdminRoutes(r, func(next http.Handler) http.Handler {
					return auth.RequirePlatformRole(invoices.DecisionRoles()...)(adminLimiter.Middleware(next))
				})
			})

			// Online payments (API 0.42.0): read like subscriptions.
			r.With(auth.RequirePlatformRole(subscriptions.ReadRoles()...)).Get("/payments", paymentsH.List)

			// Reference data: SUPER_ADMIN, PLATFORM_ADMIN, CONTENT_ADMIN, and
			// MARKET_ADMIN for the rows of its own market. Nothing is deleted.
			// Legal documents (API 0.43.0): SUPER_ADMIN, CONTENT_ADMIN, and
			// MARKET_ADMIN for its own market (checked per version).
			r.Route("/legal", func(r chi.Router) {
				r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(legal.AdminRoles()...))
				legalH.AdminRoutes(r, adminLimiter.Middleware)
			})
			// Tax rules (API 0.43.0): SUPER_ADMIN, FINANCE_ADMIN. Applied nowhere yet.
			r.Route("/tax-rules", func(r chi.Router) {
				r.Use(httpx.PrivateNoStore, auth.RequirePlatformRole(tax.AdminRoles()...))
				taxH.Routes(r, adminLimiter.Middleware)
			})
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
				r.With(auth.RequirePlatformRole(payments.ExportRoles()...)).Get("/payments.csv", paymentsH.Export)
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
				// Give it to an organisation of its market, or one of its members (API 0.49.0).
				r.Get("/{id}/candidates", requestsH.Candidates)
				r.With(requestWorkLimiter.Middleware).Post("/{id}/assign", requestsH.RerouteException)
			})

			// Moderating reviews of service providers (API 0.36.4):
			// SUPER_ADMIN, CONTENT_ADMIN and MODERATOR. Every action is in
			// the admin audit.
			r.Route("/reviews", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(reviews.AdminRoles()...))
				reviewsH.AdminRoutes(r, reviewLimiter.Middleware)
			})

			// Listing packages and their assignment to organisations and
			// direct owners (API 0.37.0): SUPER_ADMIN, PLATFORM_ADMIN, and
			// MARKET_ADMIN for its own markets (the service limits it). Every
			// change is in the admin audit.
			r.Route("/packages", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(packages.AdminRoles()...))
				packagesH.AdminRoutes(r, adminLimiter.Middleware)
			})
			r.Route("/organisations/{id}", func(r chi.Router) {
				r.Use(auth.RequirePlatformRole(packages.AdminRoles()...))
				packagesH.AccountRoutes(r, packages.KindOrganisation, "", adminLimiter.Middleware)
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
			// Replying to the customer (API 0.46.0): whoever may work the
			// request; a first reply moves NEW/ASSIGNED to CONTACTED.
			r.Get("/{id}/messages", requestsH.ProfessionalMessages)
			r.With(requestWorkLimiter.Middleware).Post("/{id}/messages", requestsH.PostProfessionalMessage)
		})

		// An organisation's team, for its OWNER and MANAGER (what each may
		// do to whom: auth.GrantableRoles). The handlers answer 404 to anyone
		// who is not an active member and 403 to its AGENT and STAFF.
		r.Route("/agent/organisations/{id}", func(r chi.Router) {
			r.Use(auth.RequireAuth)
			// The listing package and quota usage (API 0.37.0): any member of
			// a listings-managing organisation, or the direct owner's own id.
			r.Get("/package", packagesH.AgentUsage)
			// The lead funnel and response-time KPIs of the request inbox (API
			// 0.38.0, BR-005): OWNER and MANAGER the whole organisation, AGENT
			// and STAFF what their inbox shows, 404 for anybody else.
			r.Get("/reports/lead-funnel", requestsH.LeadFunnel)
			// Development projects (API 0.44.0): OWNER, MANAGER and AGENT
			// change them, STAFF read them, 404 DEVELOPMENT_NOT_FOUND for
			// anybody else on every route.
			r.Route("/developments", func(r chi.Router) {
				developmentsH.AgentRoutes(r, listingLimiter.Middleware, uploadLimiter.Middleware)
			})
			r.Get("/members", teamsH.Members)
			r.With(teamLimiter.Middleware).Patch("/members/{userId}", teamsH.PatchMember)
			r.With(teamLimiter.Middleware).Delete("/members/{userId}", teamsH.RemoveMember)
			if branchesH != nil {
				r.Get("/branches", branchesH.List)
				r.With(teamLimiter.Middleware).Post("/branches", branchesH.Create)
				r.With(teamLimiter.Middleware).Patch("/branches/{branchId}", branchesH.Update)
				r.With(teamLimiter.Middleware).Put("/members/{userId}/branch", branchesH.PinMember)
				r.With(teamLimiter.Middleware).Put("/listings/{listingId}/branch", branchesH.TieListing)
			}
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

			// Screening questionnaires (FR-017, FR-018). {id} is the
			// organisation, or an approved direct owner's own user ID. OWNER and
			// MANAGER manage, AGENT reads (403 for STAFF), 404 FORM_NOT_FOUND
			// for anybody else (auth.MayReadForms / MayManageForms).
			r.Route("/forms", func(r chi.Router) {
				r.Get("/", applicationsH.ListForms)
				r.With(applicationLimiter.Middleware).Post("/", applicationsH.CreateForm)
				r.Get("/{formId}", applicationsH.GetForm)
				r.With(applicationLimiter.Middleware).Patch("/{formId}", applicationsH.EditForm)
				r.With(applicationLimiter.Middleware).Post("/{formId}/publish", applicationsH.PublishForm)
				r.With(applicationLimiter.Middleware).Post("/{formId}/retire", applicationsH.RetireForm)
			})
			// Applications received (FR-015): OWNER and MANAGER all, the
			// listing's assigned AGENT their own; 404 for anybody else. Every
			// read of the answers and every file download is audited.
			r.Route("/applications", func(r chi.Router) {
				r.Get("/", applicationsH.Received)
				r.Get("/{applicationId}", applicationsH.ReceivedOne)
				r.Get("/{applicationId}/files/{fileId}", applicationsH.ReceivedFile)
				r.With(applicationLimiter.Middleware).Post("/{applicationId}/status", applicationsH.SetStatus)
				r.With(applicationLimiter.Middleware).Post("/{applicationId}/request-info", applicationsH.RequestInfo)
				r.With(applicationLimiter.Middleware).Post("/{applicationId}/notes", applicationsH.AddNote)
			})
			// Hourly bookings (FR-025): OWNER and MANAGER all, the listing's
			// assigned AGENT their own; 404 for anybody else (another
			// organisation, STAFF). Every host decision is audited.
			// The organisation's books (the agent ledger). {id} is the
			// organisation, or an approved direct owner's own user ID for their
			// personal book. 404 BOOK_NOT_FOUND to anyone else on every route
			// (auth.MayReadLedger / OwnsLedgerBook); 403 to a member whose role
			// may not do it. No PATCH and no DELETE on entries.
			r.Route("/ledger", func(r chi.Router) { ledgerH.Routes(r, ledgerLimiter.Middleware) })
			// Properties, units and tenancies (FR-047). {id} is the organisation,
			// or an approved direct owner's own user ID. 404 BOOK_NOT_FOUND to
			// anyone else on every route (auth.MayReadProperties /
			// OwnsPropertyBook); 403 to STAFF for every change.
			r.Route("/properties", func(r chi.Router) { tenanciesH.PropertyRoutes(r, tenancyLimiter.Middleware) })
			r.Route("/tenancies", func(r chi.Router) { tenanciesH.TenancyRoutes(r, tenancyLimiter.Middleware) })
			// Rent (API 0.36.1): waive a due; the book's arrears.
			r.Route("/dues", func(r chi.Router) { tenanciesH.DueRoutes(r, tenancyLimiter.Middleware) })
			r.Get("/arrears", tenanciesH.Arrears)
			// Maintenance requests (API 0.36.2): the same books and roles.
			r.Route("/maintenance", func(r chi.Router) { maintenanceH.Routes(r, tenancyLimiter.Middleware) })
			// Inspections (API 0.36.3): the same books and roles.
			r.Route("/inspections", func(r chi.Router) { inspectionsH.Routes(r, tenancyLimiter.Middleware) })
			// Reviews (API 0.36.4): the book's own reviews of providers
			// (basis MAINTENANCE; 404 BOOK_NOT_FOUND to anyone else), and
			// /received + /{reviewId}/reply for the members of a provider
			// organisation (404 BOOK_NOT_FOUND to non-members).
			r.Route("/reviews", func(r chi.Router) { reviewsH.BookRoutes(r, reviewLimiter.Middleware) })
			r.Route("/venue-bookings", func(r chi.Router) {
				r.Get("/", orgVenues.List)
				r.Get("/{bookingId}", orgVenues.Get)
				r.Get("/{bookingId}/calendar.ics", orgVenues.Calendar)
				r.With(venueLimiter.Middleware).Post("/{bookingId}/confirm", orgVenues.Confirm)
				r.With(venueLimiter.Middleware).Post("/{bookingId}/decline", orgVenues.Decline)
				r.With(venueLimiter.Middleware).Post("/{bookingId}/cancel", orgVenues.Cancel)
				r.With(venueLimiter.Middleware).Post("/{bookingId}/propose", orgVenues.Propose)
				r.With(venueLimiter.Middleware).Post("/{bookingId}/complete", orgVenues.Complete)
				r.With(venueLimiter.Middleware).Post("/{bookingId}/no-show", orgVenues.NoShow)
			})
		})

		// An invitation link: what it is for (anyone holding the link), and
		// accepting it (signed in, with the invited address).
		r.With(invitationLimiter.Middleware).Get("/invitations/{token}", teamsH.PublicInvitation)
		r.With(auth.RequireAuth, invitationLimiter.Middleware).Post("/invitations/{token}/accept", teamsH.Accept)
	})
	return r, nil
}

// availabilityOf returns the appointment store's availability side, or nil
// when it has none (a test double).
func availabilityOf(store appointments.Store) appointments.AvailabilityStore {
	if a, ok := store.(appointments.AvailabilityStore); ok {
		return a
	}
	return nil
}
