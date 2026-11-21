# Traceability matrix

Developer return package, FSD v2.2 section 12 and Developer Handover Specification v2.2 section 17. State on
2026-11-21: backend `dev` at API **0.61.0**, web `dev`, mobile `main` (Flutter, up to API 0.57.0), all deployed to
**staging** only. Production has never been synced.

Every row was checked against the code and the OpenAPI document (`api/openapi.yaml`) of this repository, the web
repository's `src/app` pages and the mobile repository's `lib/features` screens. Where a row names an API version,
that is the version in which the capability landed. The matching section of the backend README describes it in
full.

## How to read it

**Status**

| Status | Meaning |
| --- | --- |
| Included | Built, on in staging, and what the requirement asks for is there. |
| Built, switched off | The code is there and tested, but it is off until an external account, secret or approval exists. `runbooks/enabling-switched-off-features.md` says how to switch it on. |
| Partial | Some of the requirement is built; the row says what is missing. |
| Deferred | Not built, or the required evidence (drills, load tests, store review) has not been produced. |
| Deviation | Built differently from the documents, on purpose and agreed: see "Deliberate deviations" below. |

**Verification**

| Term | What it covers |
| --- | --- |
| Tests | Go unit, contract and route tests (`go test ./...`). "Contract" includes the PostgreSQL contract tests, which run against a throwaway PostgreSQL database and are rerun on the same database. Web: `tsc`, `eslint`, `node --test` and `next build`. Mobile: `flutter test` and the CI builds. |
| Scratch | Run end to end in a browser against a real PostgreSQL database, with the real worker and the log mailer, with screenshots. This was done for API 0.45.0 to 0.61.0. |
| Staging | Deployed to staging, with a read-only smoke test after each deploy. Every row of the API is on staging. |
| Not UAT | No formal user acceptance run with the product owner has taken place for any row. Device testing on physical phones has not been done by the developers. |

**Locations.** "Web" is a page path in the web repository; "Mobile" is a screen file under `lib/features/` in the
mobile repository; backend modules are under `internal/modules/`.

## Summary

| Status | BR | FR | UAT | Total |
| --- | ---: | ---: | ---: | ---: |
| Included | 13 | 36 | 25 | 74 |
| Built, switched off | 1 | 6 | 5 | 12 |
| Partial | 5 | 17 | 4 | 26 |
| Deferred | 0 | 0 | 3 | 3 |
| Deviation | 3 | 6 | 5 | 14 |
| **Total** | **22** | **65** | **42** | **129** |

## Deliberate deviations (agreed with the product owner)

| Document says | Built instead | Why / effect |
| --- | --- | --- |
| Neon managed PostgreSQL (BR-021, FR-051, FR-057, UAT-33/35) | Self-hosted PostgreSQL 17 on the owner's server; in staging a single pod with a hostPath volume | Owner's decision. `DB_POOL_MODE=transaction` supports PgBouncer if added. No Neon branches or point-in-time restore: backups are `pg_dump` before each migration (see the runbooks). |
| MinIO object storage (BR-014, FR-009, FR-054, UAT-07/36) | Amazon S3, one private bucket (`STORAGE_DRIVER=s3`); staging MinIO is scaled to zero | Owner's decision. Files are never public. They are streamed through the API after an authorisation check, not through presigned GET URLs. The only presigned URLs are 10-minute PUTs for upload intents (0.60.0). |
| Sevalla application hosting with autoscaling (BR-021, FR-052, UAT-31/32) | Kubernetes on the owner's VM, deployed by Argo CD from the `adminweighty/gitops` repository; Traefik ingress behind host Nginx | Owner's decision. Fixed replicas (api 2, frontend 2, worker 2); **no HorizontalPodAutoscaler is defined**. Images are tagged `sha-<commit>`, not pinned by digest. |
| Lawyer-approved, jurisdiction-specific lease templates published by the platform (BR-011, FR-030, UAT-16) | Agents and landlords design their own templates (0.24.0, organisation templates 0.26.0); platform templates exist, but nobody reviews them for legal sign-off | Owner's decision: templates are user-owned. Every draft is watermarked DRAFT. |
| E-signature, witnessing and stamping not assumed (FR-032) | Electronic signing with emailed codes, an audit report, and a **typed digital stamp**: the user types the stamp text and the platform draws the stamp; a certificate seal is optional | Owner's decision. The product does not claim the signature is legally executed in any particular jurisdiction. |

## Business requirements (BRD v2.2)

| ID | Requirement | Status | Where it lives | API | Verified | Not verified / missing |
| --- | --- | --- | --- | --- | --- | --- |
| BR-001 | International platform foundation | Included | `markets` (admin 0.56.0), `legal`, `tax`, `platform/i18n`; web `/admin/markets`, `/admin/legal`, `/admin/tax-rules`; country notice on `/listings` | 0.43.0, 0.56.0 | Tests, contract, scratch, staging | Only ZW, GB and MU have sample data. ZA, BW and AE exist in the country list but were never provisioned or tested. |
| BR-002 | Property discovery and publishing | Included | `listings` (search, filters, map 0.29.0), `developments` 0.44.0; web `/listings`, `/listings/[slug]`, `/developments`; mobile `listings/presentation/search_screen.dart`, `listing_detail_screen.dart` | ≤0.29.0, 0.44.0 | Tests, staging | Elasticsearch runs as one node on staging only, never on a multi-node cluster. Index rebuilds are manual (`/app/reindex`). |
| BR-003 | 90-day publication and manual relisting | Included | `listings` expiry sweep, relist and lifecycle; web `/agent/listings`; mobile `listings/manage/my_listings_screens.dart` | 0.45.0, 0.48.0 | Tests, contract, scratch, staging | — |
| BR-004 | Agent CRM and private customer directory | Included | `crm`; web `/agent/contacts/*`; mobile `contacts/contacts_screens.dart`; branch scoping in `branches` | 0.31.0, 0.57.0 | Tests, contract, staging; branches scratch | — |
| BR-005 | Lead and enquiry pipelines | Included | `propertyrequests` (work, assign, funnel, replies); web `/agent/requests`, `/agent/requests/funnel`; mobile `inbox/inbox_screen.dart`, `lead_screen.dart` | 0.23.0, 0.38.0, 0.46.0 | Tests, contract, scratch (0.46+), staging | — |
| BR-006 | Listing and traffic analytics | Included | `analytics`; web `/agent/analytics`, `/admin/analytics`, `/analytics-definitions`; mobile `analytics/statistics_screen.dart` | 0.30.0 | Tests (including the 100-event fixture), contract, staging | — |
| BR-007 | Showroom or venue hourly rental | Partial | `venues`; web `/agent/listings/[id]/availability`, `/agent/venue-bookings`, `/account/venue-bookings`; mobile `venues/venue_screens.dart` | 0.33.0 | Tests (including concurrent confirm), contract, staging | Request mode only: deposits are disclosed ("payable to the host if confirmed") but never collected; no online payment or refunds. |
| BR-008 | Appointments, viewings and consultations | Included | `appointments`, `availability`; web `/agent/appointments`, `/agent/availability`, `/account/appointments`; mobile `appointments/*` | ≤0.21, 0.39.0, 0.51.0 | Tests, contract, scratch (0.51), staging | — |
| BR-009 | Customer property activity hub | Included | `propertyrequests`, `saved`, `applications`, `appointments`; web `/account/*`; mobile `activity/activity_screen.dart` | ≤0.27.0, 0.46.0 | Tests, staging | — |
| BR-010 | Applications and custom questionnaires | Included | `applications`; web `/agent/forms/*`, `/agent/applications/*`, `/apply/[slug]`, `/account/applications`; mobile `applications/apply_screen.dart` | 0.32.0 | Tests, contract, staging | — |
| BR-011 | Generate lease agreements | Deviation | `leases`, `agreements`; web `/agent/lease-templates/*`, `/agent/leases/*`, `/agent/agreements/*`, `/sign/[token]`; mobile `leases/*`, `signing/*` | 0.24.0, 0.26.0 | Tests, contract, staging | Templates are user-owned, with no counsel review (see deviations). |
| BR-012 | Agent payment ledger / books | Included | `ledger`; web `/agent/books/*`; mobile `ledger/books_screens.dart` | 0.34.0, 0.57.0 | Tests, contract, staging | No bank imports (FR-028). |
| BR-013 | YouTube property tours | Included | `listings` (video), web listing page player; mobile `listing_detail_screen.dart` | 0.28.0 | Tests, staging | — |
| BR-014 | MinIO object media store | Deviation | `platform/storage` (S3 adapter), `platform/malware`, `uploads` | 0.41.0, 0.60.0 | Tests, scratch (0.60), staging (S3) | Amazon S3 instead of MinIO. No restore drill. |
| BR-015 | Admin invoices and emailed checkout links | Built, switched off | `invoices` 0.47.0, `payments` 0.42.0; web `/admin/invoices/*`, `/admin/payments`, `/account/invoices/*`, `/account/subscriptions/[id]/payment` | 0.42.0, 0.47.0 | Tests (fake gateway), contract, scratch (0.47), staging | Invoices are live. Online checkout is off: only the `fake` gateway exists and production refuses it. Settlement through a real provider has never run. |
| BR-016 | Cross-platform mobile experience | Partial | Flutter app (Android, iOS) | up to 0.57.0 | `flutter test`; Android and iOS (`--no-codesign`) builds in CI | Not published to either store; no signed iOS build; no physical-device UAT; no published support matrix. Admin is web-only by design. |
| BR-017 | Consumer opt-out and account deletion | Partial | `identity` (`DELETE /v1/me`), `notifications` (preferences 0.35.0), marketing consent 0.37.1; web `/account/delete`, `/account/notification-preferences`; mobile `auth/presentation/delete_account_screen.dart` | 0.25.0, 0.35.0, 0.37.1 | Tests, staging | Deletion is immediate anonymisation: there is no requested → verifying → processing → completed state machine and no retention notice per record type. Analytics opt-out is Do Not Track / Global Privacy Control only (no in-site choice). |
| BR-018 | Moderation, fraud controls and trust | Included | `listings` review, reports and duplicate flags 0.54.0; `onboarding` verification; `reviews`; admin audit; web `/admin/listings/*`, `/admin/listing-reports`, `/admin/verifications/*`, `/admin/reviews`, `/admin/audit` | 0.22.0, 0.36.4, 0.54.0 | Tests, contract, scratch (0.54), staging | Suspending a verified business is not built (onboarding status "Suspended"). |
| BR-019 | Property management, services and short stays | Partial | Properties, tenancies, rent, maintenance, inspections (`tenancies`, `maintenance`, `inspections`), directory, reviews, short-stay availability | 0.36.0–0.36.4, ≤0.21 | Tests, contract, staging | Short stays are requests to stay, with no instant booking or payment. |
| BR-020 | Configuration, accessibility and SEO | Partial | `reference` admin, `legal`, `platform/i18n`; web `sitemap.ts`, `robots.ts`, SSR listing pages | 0.25.0, 0.43.0 | Tests, staging | One language (English) only. No country or category SEO landing pages beyond `/pricing/[country]` and `/developments/[market]/...`. No WCAG 2.2 AA audit report produced. |
| BR-021 | Required technology and hosting baseline | Deviation | Next.js web, Go API and worker, Flutter mobile; Kubernetes, Argo CD, self-hosted PostgreSQL, S3 | — | Staging runs the stack | Neon, Sevalla and MinIO replaced (see deviations). No scale or restore test evidence. |
| BR-022 | Customer request routing through the API | Included | `propertyrequests` (`POST /v1/property-requests`, outbox, routing, exceptions 0.49.0); web request form, `/agent/requests`, `/account/requests`; mobile `property_requests/*`, `inbox/*` | ≤0.21, 0.46.0, 0.49.0 | Tests, contract, scratch, staging | Push alerts are off (FR-062); in-app and email work. |

## Functional requirements (FSD v2.2)

| ID | Requirement | Status | Where it lives | API | Verified | Not verified / missing |
| --- | --- | --- | --- | --- | --- | --- |
| FR-001 | Market-aware identity | Partial | `identity` (register, verified email, emailed sign-in code, roles), `platform/auth` principal (user, organisation, market, role, branch) | ≤0.21, 0.40.0, 0.50.0 | Tests, contract, staging | MFA with an authenticator app exists for **administrators only** (0.40.0); users have the emailed sign-in code but no optional MFA. |
| FR-002 | Organisation membership | Included | `teams` (invite, roles, suspend, remove, audit), `branches` 0.57.0; web `/agent/team`; mobile `team/*` | 0.23.0, 0.57.0 | Tests, contract, scratch (0.57), staging | — |
| FR-003 | Consent and preferences | Partial | `identity` marketing consent (versioned, source-attributed) 0.37.1, notification preferences 0.35.0, legal acceptance 0.43.0; web `/account/notification-preferences`, `/account/legal-updates` | 0.35.0, 0.37.1, 0.43.0 | Tests, contract, staging | No in-site analytics choice: Do Not Track and Global Privacy Control are honoured instead. |
| FR-004 | Delete account | Partial | `DELETE /v1/me`; web `/account/delete`, `/account/deleted`; mobile `delete_account_screen.dart` | 0.25.0 | Tests, staging | Immediate, password-confirmed anonymisation with sessions revoked; no state machine, no "verifying" step, no per-record retention notice. |
| FR-005 | Listing drafts and moderation | Included | `listings` manage, review and photos; web `/agent/listings/*`, `/admin/listings/*` | 0.22.0 | Tests, contract, staging | — |
| FR-006 | 90-day expiry calculation | Included | `listings` expiry sweep (hourly), `GET /v1/sitemap` returns unexpired listings only | 0.45.0 | Tests, contract, scratch, staging | — |
| FR-007 | Agent-initiated relisting | Included | `POST /v1/me/listings/{id}/relist`, cycles kept | 0.45.0 | Tests, contract, scratch, staging | — |
| FR-008 | Packages and quotas | Included | `packages`; web `/admin/packages/*`, `/pricing/[country]` | 0.37.0 | Tests, including a PostgreSQL concurrency test that approvals never exceed the limit | — |
| FR-009 | Object lifecycle | Deviation | `platform/storage` (private S3, per-object scan record), files served by the API after an ownership check | 0.41.0 | Tests, staging | No short-lived presigned GET (the API streams files instead); no CDN for public photo variants; bucket versioning and retention are not set by the code. |
| FR-010 | Safe upload | Built, switched off | `uploads` (intents, presigned PUT, size, SHA-256, sniffed type, sweep), `platform/malware`, photo re-encoding (EXIF/GPS stripped) | 0.41.0, 0.60.0 | Tests (EXIF strip, EICAR rejection), contract, scratch, staging (validation only) | Malware scanning is off on staging (`MALWARE_SCANNER` unset; no ClamAV host). Direct browser PUT needs S3 bucket CORS, which is not set; browsers fall back to the through-API upload. |
| FR-011 | YouTube embed | Included | `listings` video (ID parsed, allowlisted hosts), web player with fallback | 0.28.0 | Tests, staging | — |
| FR-012 | Media deletion and recovery | Partial | Photo and file deletion, application retention job, deleted application files purged | 0.32.0, 0.41.0 | Tests | No CDN to purge; account deletion anonymises but does not list purged objects; no tested backup lifecycle for objects. |
| FR-013 | Enquiries and lead attribution | Included | `propertyrequests` (idempotency key, one request, one alert), campaign attribution | 0.58.0 | Tests, contract, scratch, staging | — |
| FR-014 | Agent customer directory | Included | `crm` (contacts, tags, merge, import, export, reminders); web `/agent/contacts/*` | 0.31.0 | Tests, contract, staging | — |
| FR-015 | Applications and statuses | Included | `applications`; web `/account/applications/*`, `/agent/applications/*` | 0.32.0 | Tests, contract, staging | — |
| FR-016 | Customer activity dashboard | Included | web `/account/requests`, `/account/applications`, `/account/appointments`, `/account/saved`; mobile `activity/*` | ≤0.27.0, 0.46.0 | Tests, scratch, staging | — |
| FR-017 | Form builder | Included | `applications` forms (versioned, published); web `/agent/forms/*` | 0.32.0 | Tests, contract, staging | — |
| FR-018 | Sensitive fields and retention | Included | Forms file-type checks, retention job, `APPLICATION_VIEWED` and `APPLICATION_FILE_VIEWED` audit | 0.32.0 | Tests, contract | — |
| FR-019 | Application workflow | Included | Questionnaire per listing, intake open or closed, drafts, more-info requests | 0.32.0 | Tests, contract, staging | — |
| FR-020 | Availability and slots | Included | `availability` (working hours, buffers, lead time, holidays, market time zone) | 0.51.0 | Tests (DST), contract, scratch, staging | — |
| FR-021 | Booking workflow | Included | `appointments` (REQUESTED, CONFIRMED, RESCHEDULE_REQUESTED, COMPLETED, NO_SHOW, CANCELLED), ICS 0.39.0, reminders | ≤0.21, 0.39.0, 0.51.0 | Tests, contract, scratch, staging | — |
| FR-022 | Consultation types | Included | Consultation services (free), shown in appointments and enquiries | 0.51.0 | Tests, scratch, staging | Paid consultations are not enabled, as the requirement says. |
| FR-023 | Hourly rate setup | Included | `venues` settings (rate, increment, min/max, buffer, deposit, fees with effective dates, tax) | 0.33.0 | Tests, contract, staging | — |
| FR-024 | Quote and reservation | Partial | `venues` quotes and requests; confirmation locks capacity | 0.33.0 | Tests (concurrent confirm, memory and PostgreSQL) | Request mode only: no payment holds or reservation mode. |
| FR-025 | Booking management | Partial | Host, agent and customer venue-booking lists and actions with audit | 0.33.0 | Tests, staging | No refunds or payment disclosures (payments are not active for venues). |
| FR-026 | Organisational accounts | Included | `ledger` books, categories, links, branch filter, exports | 0.34.0, 0.57.0 | Tests, contract, staging | — |
| FR-027 | Immutable transaction history | Included | Reversal and correcting entries, immutability trigger | 0.34.0 | Tests, contract | — |
| FR-028 | Trust and reconciliation | Partial | Entries are USER_RECORDED_UNVERIFIED; reconciliation status | 0.34.0 | Tests | No bank imports; VERIFIED_GATEWAY needs a real payment gateway (off). |
| FR-029 | Reports and multi-currency | Included | Ledger reports per currency, no silent conversion; web `/agent/books/reports` | 0.34.0 | Tests | — |
| FR-030 | Template library | Deviation | `leases` templates (platform and organisation), per market | 0.24.0, 0.26.0 | Tests, staging | User-owned templates; no legal sign-off workflow. |
| FR-031 | Draft generator | Included | `leases` drafts (DRAFT watermark, data snapshot, template version, reference) | 0.24.0 | Tests, contract, staging | — |
| FR-032 | Review and e-sign scope | Deviation | `agreements` signing with emailed codes, audit report, typed digital stamp | 0.24.0, 0.26.0 | Tests, contract, staging | Signing exists although the FSD did not assume it. No claim of legal execution is made. |
| FR-033 | Event ingestion | Included | `analytics` (all 11 listed event types plus `booking_confirm`; event_id dedupe; versioned schema) | 0.30.0 | Tests, contract | — |
| FR-034 | Metric definitions | Included | Shared metric contract; web `/analytics-definitions` | 0.30.0 | Tests | — |
| FR-035 | Near real-time and historical reports | Included | Rollups every 15 minutes, 24h/7d/30d/custom, funnel | 0.30.0, 0.38.0 | Tests (100-event fixture across time zones) | — |
| FR-036 | Privacy and accuracy | Partial | Pseudonymous sessions, IP truncation, bot filter, SPA dedupe, DNT/GPC | 0.30.0, 0.58.0 | Tests | No in-site consent choice (see FR-003). |
| FR-037 | Admin renewal invoice | Included | `invoices` (sequential numbers per market, credit, void, reissue, audit); web `/admin/invoices/*` | 0.47.0 | Tests (concurrent numbering), contract, scratch, staging | — |
| FR-038 | Hosted payment URL email | Built, switched off | `payments` checkout sessions and invoice email link | 0.42.0 | Tests with the fake gateway | No real provider integrated (`PAYMENT_GATEWAY` has only `fake`). |
| FR-039 | Payment confirmation | Built, switched off | Signed webhook, event dedupe, amount and currency check, finance reconciliation | 0.42.0 | Tests (replay and out-of-order) | As FR-038. |
| FR-040 | Subscription entitlement | Included | Renewal applied on confirmed settlement or approved reconciliation; listings are not republished | 0.42.0, 0.47.0 | Tests | Real-gateway settlement not run (FR-038). |
| FR-041 | Mobile commerce policy boundary | Included | Mobile shows invoices read-only; no in-app purchase or external payment steering | 0.47.0 | Mobile tests | Store-policy sign-off not obtained. |
| FR-042 | Mobile consumer features | Partial | Mobile search, map, photos, YouTube, saved, saved searches, enquiries, applications, viewings, deletion | up to 0.57.0 | `flutter test`, CI builds | Not published; not device-tested; no measured sync time. |
| FR-043 | Mobile professional features | Partial | Mobile listings, inbox, CRM, schedule, statistics, invoices, team, books, properties | up to 0.57.0 | `flutter test`, CI builds | As FR-042. Admin is a web handoff by design. |
| FR-044 | Notification service | Built, switched off | `notifications`, `inapp`, `platform/mail`, expiry reminders 14/7/1 days | 0.35.0, 0.45.0 | Tests, scratch, staging (email via Resend, in-app) | Push is off: no FCM credentials (`FCM_*` unset). |
| FR-045 | Device and offline behaviour | Built, switched off | Mobile secure storage, offline reading and clearing on sign-out; web serves `/.well-known/assetlinks.json` and `apple-app-site-association` | — | Mobile tests | App Links and Universal Links stay inactive until the release signing fingerprint and the Apple Team ID are supplied. Push token revocation applies once push is on. |
| FR-046 | Admin consoles | Partial | Seven platform roles (SUPER, PLATFORM, MARKET, FINANCE, CONTENT, MODERATOR, SUPPORT); web `/admin/*` | 0.25.0 onwards | Tests (role gates), staging | Finance and Content have no section of their own: their pages are spread over invoices, payments and tax (Finance) and legal, lease templates, reference and reviews (Content). |
| FR-047 | Legacy Phase 2 scope | Included | Tenancies, rent and arrears, maintenance, inspections, directory with verification, enquiries, reviews, sponsored placements | 0.36.0–0.36.4, 0.53.0 | Tests, contract, staging | — |
| FR-048 | Legacy Phase 3 scope | Partial | Short-stay availability and requests to stay; developments showcase (moderated, no return claims) | ≤0.21, 0.44.0 | Tests, staging | No transactional short-stay booking. |
| FR-049 | Next.js web application | Included | App Router, SSR/ISR public pages, revalidation hook, standalone image | — | `next build`, staging | No formal SEO test report. |
| FR-050 | Golang backend | Included | Modules per domain, pgx, `api/openapi.yaml` (550 operations) | 0.61.0 | Tests (route tests check the organisation, branch and market scope) | — |
| FR-051 | Neon PostgreSQL connectivity | Deviation | Self-hosted PostgreSQL; `DATABASE_URL`, `DATABASE_DIRECT_URL`, `DB_POOL_MODE`, `DB_MAX_CONNS` | — | Staging | No load test at maximum replicas. |
| FR-052 | Sevalla deployment and scaling | Deviation | Kubernetes and Argo CD (`adminweighty/gitops`) | — | Staging | No autoscaler; no forced-load scale test. |
| FR-053 | Scheduled jobs and worker safety | Included | Outbox `FOR UPDATE SKIP LOCKED` leases, advisory locks, idempotent keys; two workers on staging | — | Tests, staging (two workers) | No chaos test of a worker killed mid-sweep. |
| FR-054 | MinIO production boundary | Deviation | Amazon S3 private bucket | — | Staging | No object restore drill; provider SLA not recorded here. |
| FR-055 | CI/CD and environments | Partial | GitHub Actions `ci.yml` (gofmt, vet, `go test -race`, build) and `publish-and-update-gitops.yml`; PreSync migration job | — | CI | No image vulnerability scan; images not pinned by digest. The GitOps update step fails for lack of `GITOPS_TOKEN`, so gitops is bumped by hand. No release log with approver. |
| FR-056 | Observability and capacity | Partial | JSON logs with request IDs; Prometheus metrics on `METRICS_ADDR` | 0.61.0 | Tests, staging (both pods scraped by port-forward) | No central log store; alert rules are files, not applied; no load test or scale alert evidence. |
| FR-057 | Database recovery and performance | Partial | `pg_dump -Fc` before each migration, kept on the server | — | Backups taken | No restore drill; no point-in-time recovery; RPO and RTO not agreed. |
| FR-058 | Mobile apps and API parity | Partial | Flutter on the shared OpenAPI | up to 0.57.0 | `flutter test` | No two-platform UAT; mobile lacks 0.58–0.61 (none customer-facing except upload intents, which the multipart upload covers). |
| FR-059 | Single first-party entry point | Included | `POST /v1/property-requests` (request types, guest contact, captcha, rate limit) | ≤0.21 | Tests, contract, staging | — |
| FR-060 | Transactional persistence and outbox | Included | One transaction for request, timeline, activity and outbox; idempotency | ≤0.21 | Tests, contract | — |
| FR-061 | Ownership resolution and routing | Included | Routing to agent, agency or seller; exception queue; reassignment | ≤0.21, 0.49.0 | Tests, contract, scratch, staging | — |
| FR-062 | Event-driven notifications with deep links | Built, switched off | Worker creates in-app notifications, push queue and email | 0.35.0 | Tests, staging (in-app and email) | Push off (FCM); deep links inactive (FR-045). |
| FR-063 | Agent and seller inbox and response | Included | Inbox, assign, stage, replies, notes; web `/agent/requests/*`; mobile `inbox/*` | 0.23.0, 0.46.0 | Tests, contract, scratch, staging | — |
| FR-064 | Customer history and status transparency | Included | `/account/requests/*`; customer status vocabulary; kept after expiry | 0.46.0 | Tests, scratch, staging | — |
| FR-065 | Delivery resilience, monitoring and support | Partial | Outbox retries and dead letters, routing exception report, email bounce and complaint handling, metrics | 0.49.0, 0.59.0, 0.61.0 | Tests, scratch, staging | Resend webhook off (no `RESEND_WEBHOOK_SECRET` on staging); 30-second notification SLO not measured; push outage not simulated with a real provider. |

## UAT scenarios (FSD and Handover v2.2)

No scenario has been run as a formal UAT with the product owner. Status here says whether the system can pass the
scenario; "Verified" names the automated or scratch evidence.

| ID | Scenario | Status | Verified by | Not verified / missing |
| --- | --- | --- | --- | --- |
| UAT-01 | Register and approve an agent; restrict a wrong-market branch | Included | Onboarding approval tests, `TestBranches` (403 BRANCH_FORBIDDEN), MARKET_ADMIN scope tests | Not UAT |
| UAT-02 | Publish 20/40/60 quota boundary atomically under concurrent requests | Included | `TestConcurrentApprovalsNeverExceedTheLimit` (PostgreSQL) | Not UAT |
| UAT-03 | Auto-expire at 90 days; gone from search, sitemap, browse | Included | Expiry sweep tests, scratch (0.45) | Not UAT |
| UAT-04 | Renewed via invoice, listing stays expired until relist | Included | Invoice and renewal tests | Real gateway not used |
| UAT-05 | Relist creates a new cycle; history kept | Included | Relist tests, scratch | — |
| UAT-06 | YouTube valid embed; removed/private fallback; no HTML | Included | Video tests | Not device-tested on mobile |
| UAT-07 | Private leases and documents inaccessible without permission | Deviation | Ownership tests on every file route | S3, not MinIO |
| UAT-08 | Blocked malware upload; presigned URL TTL | Built, switched off | EICAR tests, presign TTL test (0.60) | Scanner off on staging |
| UAT-09 | CRM contact create, merge, export; wrong organisation denied | Included | CRM route tests | — |
| UAT-10 | Enquiry and application both in the activity dashboard | Included | Activity tests | — |
| UAT-11 | Form version change keeps submitted answers | Included | Form version tests | — |
| UAT-12 | Appointment conflicts, time zone, reschedule, reminders | Included | Availability (DST) and appointment tests, scratch (0.51) | — |
| UAT-13 | Showroom hourly quote with fee; simultaneous booking | Included | `TestVenueQuoteRequestAndConfirmConflict`, concurrent confirm tests | No payment of the quote |
| UAT-14 | Manual ledger entry unverified; reversal keeps audit | Included | Ledger tests | — |
| UAT-15 | Currency separation across ledger, invoices, reports | Included | Ledger and invoice tests | — |
| UAT-16 | Lease PDF with approved market template and draft status | Deviation | Lease draft tests | Templates not lawyer-approved (user-owned) |
| UAT-17 | Private form response access and retention | Included | Application access and retention tests | — |
| UAT-18 | 100-view fixture matches unique session and valid views | Included | FR-035 fixture test | — |
| UAT-19 | Duplicate SPA page view and bot traffic excluded | Included | Analytics dedupe and bot tests | — |
| UAT-20 | Finance Admin issues renewal invoice with emailed secure link | Built, switched off | Invoice tests, payment tests with the fake gateway, scratch (0.47) | No real provider |
| UAT-21 | Duplicate or out-of-order payment webhooks cannot double credit | Built, switched off | Webhook replay tests (fake driver) | No real provider |
| UAT-22 | Refund or chargeback marks invoice for review | Built, switched off | Payment review tests (fake driver) | No real provider |
| UAT-23 | iOS customer deletes in-app; completed per policy | Partial | Mobile deletion screen; API tests | No signed iOS build; no state machine (FR-004) |
| UAT-24 | Android deletion in-app and through public web | Partial | Mobile screen; web `/account/delete` | Not device-tested; no state machine |
| UAT-25 | Mobile roles cannot reach another organisation's data | Included | Server-side scope tests (the app uses the same API) | Not device-tested |
| UAT-26 | Offline or logout clears cached private records | Included | Mobile offline implementation and tests | Not device-tested |
| UAT-27 | Multi-market tests across ZW, ZA, BW, UAE | Partial | Market, currency and time-zone tests with ZW, GB, MU | ZA, BW, AE never provisioned or tested |
| UAT-28 | Security, accessibility, backup/restore and load evidence | Deferred | — | No penetration test, WCAG audit, restore drill or load test |
| UAT-29 | Regional legal terms, currencies, address and local time | Included | Legal-per-market, currency and time-zone tests | Only ZW, GB, MU data |
| UAT-30 | Analytics and ledger exports scoped by role | Included | Export permission tests | — |
| UAT-31 | Images deployed from pinned digests with staging smoke tests | Deviation | Staging smoke tests after every deploy | Kubernetes, not Sevalla; tags, not digests |
| UAT-32 | Scale web/API under load; no duplicate jobs or bookings | Deferred | Worker idempotency tests | No autoscaler, no load test |
| UAT-33 | Pooled runtime, direct migrations, max-replica connections | Deviation | PreSync migration job | Self-hosted PostgreSQL; no connection load test |
| UAT-34 | Worker loss during 90-day expiry recovers without double processing | Included | Lease and idempotency tests, two workers on staging | No chaos test |
| UAT-35 | Point-in-time recovery and object restore drills | Deferred | — | No drill; only `pg_dump` backups |
| UAT-36 | Restarted instances keep no uploads; objects persist | Deviation | Pods are stateless; files in S3 | S3, not MinIO |
| UAT-37 | Request committed with timeline and outbox; request_id returned | Included | Request tests, scratch, staging | — |
| UAT-38 | Agent or seller routing; wrong organisation cannot read | Included | Routing and inbox scope tests | — |
| UAT-39 | In-app and push alert with deep link; reply reaches customer | Built, switched off | In-app and reply tests, scratch (0.46) | Push and deep links off |
| UAT-40 | Push outage and worker restart lose nothing; no duplicate lead | Partial | Outbox retry and idempotency tests | Not simulated against a real push provider |
| UAT-41 | Inactive recipient goes to monitored exception handling | Included | Routing exception tests, `rp365_routing_exceptions_open` metric | — |
| UAT-42 | Request history survives 90-day expiry | Included | Activity after expiry tests | — |

## Open decisions (FSD section 14, Handover section 19)

| ID | Decision | State in the build |
| --- | --- | --- |
| D-01 | Scope cut-line | This matrix is the proposal. |
| D-02 | 90-day counting | Implemented from the publish instant (UTC), reminders at 14, 7 and 1 days. |
| D-03 | Re-approval on relist | Relist goes through the normal review rules (0.45.0, 0.48.0). |
| D-04 | Hourly reservation mode | Request mode only. |
| D-05 | Ledger complexity | Cashbook with reversals; no double entry. |
| D-06 | Lease jurisdiction | User-owned templates (deviation). |
| D-07 | Data handling | Application retention, data export (0.55.0), anonymising deletion; retention exceptions not written up. |
| D-08, D-15 | MinIO | Replaced by S3 (deviation). |
| D-09 | Payment provider | Open: only the fake gateway exists. |
| D-10 | Analytics | First-party only; DNT/GPC honoured; definitions published. |
| D-11 | App stores | Open: store drafts prepared, nothing submitted. |
| D-12 | Availability integrations | None; ICS files only. |
| D-13 | Neon | Replaced by self-hosted PostgreSQL (deviation). |
| D-14 | Sevalla | Replaced by Kubernetes and Argo CD (deviation). |
| D-16 | Mobile framework | Flutter. |
| D-17 | Routing precedence | Assigned agent, then agency inbox, then direct seller, else exception queue. |
| D-18 | Notification channels | Email and in-app live; push built, off. |
| D-19 | SLA, guest request policy | Open. Guests may submit with captcha and rate limits. |

## Section 17 items not in this package

- Architecture or deployment diagram, cost and capacity evidence, and the container image digest inventory are not
  produced. Images are tagged `sha-<commit>`; digests can be read from GHCR.
- Lease jurisdiction approval list and app-store review matrix: not applicable (user-owned templates) or not started.
- UX files, IP assignment or licence, support SLA and warranty, and training: commercial items for the parties.
