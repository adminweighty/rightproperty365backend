# RightProperty365 Backend

Go backend for RightProperty365, a global property marketplace: any country can be listed in,
and the public is shown the countries that have listings (Zimbabwe and the United Kingdom first).

It is a **modular monolith** exposing a versioned REST API (`/v1`), with separate binaries for the
API, the background worker and database migrations. The API contract lives in
[`api/openapi.yaml`](api/openapi.yaml).

## Quick start (no database, no Docker)

Requires Go 1.26+. Without `DATABASE_URL` the API runs in development mode on **in-memory stores**
(seeded sample data, lost on restart) and logs a warning saying so.

```powershell
Copy-Item .env.example .env   # optional, but needed for the development seed users (see below)
go run ./cmd/api              # listens on http://localhost:8080
```

```powershell
curl.exe http://localhost:8080/healthz
curl.exe http://localhost:8080/readyz          # {"status":"ready","store":"memory"}
curl.exe "http://localhost:8080/v1/countries?has_listings=true"
```

Submit a property request (PowerShell):

```powershell
$body = @{
  listing_id = "0195c0de-0000-7000-8000-000000000001"
  request_type = "ENQUIRY"
  message = "Is this property still available?"
  preferred_contact_method = "EMAIL"
  idempotency_key = [guid]::NewGuid().ToString()
  guest_contact = @{ name = "Tariro Moyo"; email = "tariro@example.com" }
  consent = @{ privacy_version = "2026-01"; accepted = $true }
} | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/property-requests -ContentType "application/json" -Body $body
```

### Development sample data

In development without a database the API seeds its in-memory stores with the data below (defined
once in `internal/devseed`; the web app's sample data uses the same names): 28 organisations, 47
accounts, 24 listings and 34 directory profiles (the web's 29, three solicitors, and the United
Kingdom's agency and host). Listing IDs are
`0195c0de-0000-7000-8000-000000000001` … `…000000000018`: the web's 18 **PUBLISHED** listings in
market ZW, each with an organisation **and** a named assigned agent, or a direct seller. Listing
`…000000000099` is **EXPIRED** (requests get `410`). Two more are not public: the sample `DRAFT` and `PENDING_REVIEW` listings of "Listing management" (`…00000000e101`, `…00000000e102`).

Six more PUBLISHED listings exist only on the backend (`internal/devseed/backend_samples.go`;
the web's sample files do not have them):

| ID | Reference | Slug | What | Owner |
| --- | --- | --- | --- | --- |
| `…019` | `RP-GB-10001` | `three-bedroom-terraced-house-kensington` | London (Kensington), for sale, GBP 1,250,000 | Sample Homes London |
| `…020` | `RP-GB-10002` | `two-bedroom-apartment-northern-quarter` | Manchester, to rent, GBP 1,650 per month | Sample Homes London |
| `…021` | `RP-GB-10003` | `serviced-apartment-shoreditch` | London (Shoreditch), **short stay**, GBP 185 per night, 2 guests, 2 nights minimum | Sample Homes London |
| `…022` | `RP-ZW-10019` | `safari-lodge-victoria-falls` | Victoria Falls, **short stay**, USD 320 per night, 8 guests, 2 nights minimum | Sample Falls Stays (host) |
| `…023` | `RP-ZW-10020` | `garden-guest-house-avondale` | Harare (Avondale), **short stay**, USD 75 per night, 4 guests, 1 night minimum | Sample Showrooms (host) |
| `…024` | `RP-MU-10001` | `beach-villa-grand-baie` | Grand Baie, Mauritius (**free-text** city and region: the country has no reference places), **short stay**, MUR 8,500 per night, 6 guests, 3 nights minimum | Sample Falls Stays (host) |

All accounts are email-verified and share one password: **`DEV_SEED_PASSWORD` in
[`.env.example`](.env.example)**. They are created only when `APP_ENV=development` and that
variable is set; otherwise the API logs a warning and creates no users (the listings are still
seeded).

| Email | Name | Organisation | Kind | Role | Listings / profiles |
| --- | --- | --- | --- | --- | --- |
| `consumer@example.test` | Casey Consumer | — | — | — | — |
| `admin@example.test` | Sam Admin | — | — | platform `SUPER_ADMIN` (admin login only) | — |
| `estates.owner@example.test` | Rudo Moyo | Sample Estates | `AGENCY` | `OWNER` | `agents/rudo-moyo`; `agencies/sample-estates` |
| `agent@example.test` | Tendai Ncube | Sample Estates | `AGENCY` | `AGENT` | listings 1, 2, 5; `agents/tendai-ncube` |
| `estates.agent2@example.test` | Chipo Dube | Sample Estates | `AGENCY` | `AGENT` | listings 7, 10, 17; `agents/chipo-dube` |
| `commercial.agent@example.test` | Farai Chikwanha | Sample Commercial | `AGENCY` | `OWNER` | listings 3, 13; `agents/farai-chikwanha`; `agencies/sample-commercial` |
| `land.agent@example.test` | Nomsa Sibanda | Sample Land Agents | `AGENCY` | `OWNER` | listings 4, 14, 15; `agents/nomsa-sibanda`; `agencies/sample-land-agents` |
| `realty.agent@example.test` | Tatenda Mutasa | Sample Realty | `AGENCY` | `OWNER` | listings 8, 9, 16, 18; `agents/tatenda-mutasa`; `agencies/sample-realty` |
| `bulawayo.agent@example.test` | Sipho Nkomo | Sample Homes Bulawayo | `AGENCY` | `OWNER` | listings 11, 12; `agents/sipho-nkomo`; `agencies/sample-homes-bulawayo` |
| `seller@example.test` | Grace Mhlanga | — | — | direct seller | listings 6 |
| `agent-b@example.test` | Blake Agent | Second Street Realty | `AGENCY` | `AGENT` | — |
| `tenant@example.test` | Tariro Tenant | — | — | account role `TENANT` | three sample consultations as the customer |
| `sales.agent@example.test` | Simba Sales | Sample Estates | `AGENCY` | `AGENT` (account role `SALES_AGENT`) | — |
| `lettings.agent@example.test` | Lindiwe Lettings | Sample Estates | `AGENCY` | `AGENT` (account role `LETTINGS_AGENT`) | — |
| `agency.admin@example.test` | Ruvimbo Chari | Sample Estates | `AGENCY` | `MANAGER` (account role `AGENCY_ADMIN`) | sees the agency's requests and appointments |
| `agency.staff@example.test` | Kuda Staff | Sample Estates | `AGENCY` | `STAFF` (account role `AGENCY_STAFF`) | — |
| `landlord@example.test` | Lloyd Landlord | — | — | account role `LANDLORD` (approved direct owner) | — |
| `admin.platform@example.test`, `admin.market@example.test` (market ZW), `admin.moderator@example.test`, `admin.support@example.test`, `admin.finance@example.test`, `admin.content@example.test` | | — | — | platform `PLATFORM_ADMIN`, `MARKET_ADMIN`, `MODERATOR`, `SUPPORT`, `FINANCE_ADMIN`, `CONTENT_ADMIN` (admin login only) | — |
| `admin.mfa@example.test` | Avery MFA | — | — | platform `PLATFORM_ADMIN`, **enrolled in MFA** with a fixed development-only authenticator secret (see docs/sample-accounts.md; API 0.40.0) | — |
| `sample-developments@example.test` | Sample Developments Owner | Sample Developments | `DEVELOPER` | `OWNER` | `developers/sample-developments` |
| `sample-urban-builders@example.test` | Sample Urban Builders Owner | Sample Urban Builders | `DEVELOPER` | `OWNER` | `developers/sample-urban-builders` |
| `sample-heights@example.test` | Sample Heights Owner | Sample Heights | `DEVELOPER` | `OWNER` | `developers/sample-heights` |
| `sample-property-management@example.test` | Sample Property Management Owner | Sample Property Management | `LANDLORD` | `OWNER` | `property-managers/sample-property-management` |
| `sample-rentals-bulawayo@example.test` | Sample Rentals Bulawayo Owner | Sample Rentals Bulawayo | `LANDLORD` | `OWNER` | `property-managers/sample-rentals-bulawayo` |
| `sample-falls-stays@example.test` | Sample Falls Stays Owner | Sample Falls Stays | `HOST` | `OWNER` | listings 22 and 24 (short stays; 24 is in Mauritius); `hosts/sample-falls-stays` |
| `sample-showrooms@example.test` | Sample Showrooms Owner | Sample Showrooms | `HOST` | `OWNER` | listing 23 (short stay); `hosts/sample-showrooms` |
| `sample-homes-london@example.test` | Sample Homes London Owner | Sample Homes London (market **GB**) | `AGENCY` | `OWNER` | listings 19, 20, 21; `agencies/sample-homes-london` |
| `sample-stays-edinburgh@example.test` | Sample Stays Edinburgh Owner | Sample Stays Edinburgh (market **GB**) | `HOST` | `OWNER` | `hosts/sample-stays-edinburgh` |
| `sample-plumbers@example.test` | Sample Plumbers Owner | Sample Plumbers | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-plumbers` |
| `sample-rapid-plumbing@example.test` | Sample Rapid Plumbing Owner | Sample Rapid Plumbing | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-rapid-plumbing` |
| `sample-electricians@example.test` | Sample Electricians Owner | Sample Electricians | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-electricians` |
| `sample-sparks@example.test` | Sample Sparks Owner | Sample Sparks | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-sparks` |
| `sample-solar-installers@example.test` | Sample Solar Installers Owner | Sample Solar Installers | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-solar-installers` |
| `sample-sun-power@example.test` | Sample Sun Power Owner | Sample Sun Power | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-sun-power` |
| `sample-borehole-drillers@example.test` | Sample Borehole Drillers Owner | Sample Borehole Drillers | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-borehole-drillers` |
| `sample-conveyancers@example.test` | Sample Conveyancers Owner | Sample Conveyancers | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-conveyancers` |
| `sample-valuers@example.test` | Sample Valuers Owner | Sample Valuers | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-valuers` |
| `sample-movers@example.test` | Sample Movers Owner | Sample Movers | `SERVICE_PROVIDER` | `OWNER` | `service-providers/sample-movers` |

"Profiles" are the directory profiles (`kind/slug`) whose enquiries that person receives: an agent's
own profile, the agency's profile for the agency's owner, or the business's profile. Rudo Moyo has
no listings assigned and, as `OWNER`, sees all of Sample Estates' requests and enquiries.
`agent-b` belongs to an agency with no listings and no profile (kept for checking that
organisations cannot see each other's data).

Account roles of the sample accounts are all `ACTIVE` and follow the registration rule (a
property seeker account is only that): `AGENT` for the agents, `AGENCY_OWNER` for the agency
owners, `PRIVATE_SELLER` for Grace, the role matching their organisation's kind for the business
owners (`DEVELOPER`, `PROPERTY_MANAGER`, `HOST`, `SERVICE_PROVIDER`, `SOLICITOR`), `CONSUMER` for
the consumer, and none for the administrator (like an administrator made by `cmd/adminctl`). The
two onboarding demo accounts hold only the role they applied for. `cmd/seed` removes the
`CONSUMER` role that earlier seeds gave the professional sample accounts.

A request for a listing is routed to its assigned agent and appears in that agent's inbox; the
agency's `OWNER` sees all of the agency's requests; another `AGENT` of the same agency does not see
it, and no other agency does. A request for listing 6 goes to Grace, the direct seller.

The 29 directory profiles (same `kind` + `slug` and display name as the web app's
`src/lib/data/directory.ts`) are all `ACTIVE`: the 5 agencies and 7 agents map to the organisations
and people above by name, and each of the 17 developers, property managers, hosts and service
providers is its own organisation with one `OWNER`, `<slug>@example.test`.

#### The same data in Postgres: `cmd/seed`

```powershell
go run ./cmd/migrate up
go run ./cmd/seed          # or: make seed
```

`cmd/seed` writes the same organisations, accounts, roles, listings and directory profiles into the database at
`DATABASE_URL`. It runs in one transaction and is idempotent (re-running it resets the sample rows,
including the shared password, and touches nothing else). Where it may run:

- **Development:** `APP_ENV=development` set explicitly and `DEV_SEED_PASSWORD` set.
- **Staging:** only with `SEED_ALLOW_STAGING=yes-create-sample-accounts` **and** a
  `DEV_SEED_PASSWORD` of at least 24 characters. Staging is on the internet and one sample account
  (`admin@example.test`) is a Super Administrator, so generate a private password, keep it in a
  secret, and never use the published value from `.env.example`. On the staging cluster:
  `kubectl -n rightproperty-staging exec deploy/worker -- env SEED_ALLOW_STAGING=... DEV_SEED_PASSWORD=... /app/seed`
  is not possible (the image has no shell or `env`); run it as a one-off Pod that takes the API's
  environment plus those two variables, the password from a Secret.
- **Production:** never.

After seeding a database that Elasticsearch searches, rebuild the index (`/app/reindex`): the seed
writes listings directly and the search index only learns about them from a reindex.

The seed also writes the sample logos (one small PNG per sample business, see "Logos") to the
configured file storage and marks the sample listings as reviewed. It needs migration 0016.

Sign in and read the organisation inbox (PowerShell; `-SessionVariable` keeps the cookie, and the
`Origin` header is required on cookie-authenticated POSTs):

```powershell
$pw = (Get-Content .env.example | Select-String '^DEV_SEED_PASSWORD=').Line.Split('=', 2)[1]
$login = @{ email = "agent@example.test"; password = $pw } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/auth/login -ContentType "application/json" -Body $login -SessionVariable s
Invoke-RestMethod -Uri http://localhost:8080/v1/me -WebSession $s
Invoke-RestMethod -Uri http://localhost:8080/v1/agent/property-requests -WebSession $s
```

**Solicitors** (verified sample profiles in the `solicitors` directory; each firm is an
organisation of kind `LAW_FIRM` whose owner signs in with `DEV_SEED_PASSWORD`):

| Email | Firm | City | Profile |
| --- | --- | --- | --- |
| `sample-law-chambers@example.test` | Sample Law Chambers | Harare | `solicitors/sample-law-chambers` |
| `sample-conveyancing-attorneys@example.test` | Sample Conveyancing Attorneys | Bulawayo | `solicitors/sample-conveyancing-attorneys` |
| `sample-property-lawyers@example.test` | Sample Property Lawyers | Mutare | `solicitors/sample-property-lawyers` |

The two "property-managers" sample businesses are organisations of kind `PROPERTY_MANAGER`, and
their owners hold the `PROPERTY_MANAGER` account role (it used to be LANDLORD).

**Onboarding demo accounts** (same `DEV_SEED_PASSWORD`; created in memory by the API and in
Postgres by `cmd/seed`). Their professional role is **not** active: it waits for an administrator.
The other seeded professionals are already approved and are deliberately left **without**
onboarding applications (their roles were granted by the seed, not by a review).

| Email | Name | Role and application |
| --- | --- | --- |
| `pending.provider@example.test` | Pamela Provider | SERVICE_PROVIDER, application **SUBMITTED** with details ("Sample Roofing Repairs") and one tiny generated sample document per required type (a PNG and two PDFs). Sign in as `admin@example.test` at the administrator login to review and decide it |
| `pending.agent@example.test` | Peter Pending | AGENT, application still a **DRAFT** (nothing filled in) |

**Team samples** (Sample Estates, API 0.23.0): a pending invitation to `invited.agent@example.test`
(nobody has its link; invite an address yourself to get one in the log) and a pending join request
from `joining.agent@example.test` (Jamie Joiner, same `DEV_SEED_PASSWORD`), decided by Rudo Moyo or
Ruvimbo Chari on the team page, or by an administrator. See docs/sample-accounts.md.

**Ledger samples** (Sample Estates, API 0.34.0): opening balances in USD (cash, bank) and ZAR,
a month of rent, commissions, expenses and a deposit, one corrected entry, one reconciled entry,
one cash-to-bank transfer and two receivables. See "Agent ledger / books" below.

Registering a new account in development does not send email: the API **logs** the link, e.g.
`"msg":"DEV MAIL (not sent): verify-email link", "link":"http://localhost:3000/verify-email?token=..."`.

### Everyday commands

| Task | Command |
| --- | --- |
| Run API | `go run ./cmd/api` |
| Run worker | `go run ./cmd/worker` |
| Format check | `gofmt -l .` (must print nothing) |
| Vet | `go vet ./...` |
| Test | `go test ./...` |
| Build all | `go build ./...` |
| Migrations | `go run ./cmd/migrate up` / `down` / `status` |
| Sample data into Postgres (development only) | `go run ./cmd/seed` |

A `Makefile` wraps the same commands for environments that have `make`.

## Configuration

All configuration comes from environment variables. For local development you may copy
`.env.example` to `.env`; the binaries load it if present (real environment variables always win).
`.env` is gitignored. **No secrets belong in this repository.**

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_ENV` | `development` | `development`, `staging` or `production` |
| `HTTP_ADDR` | `:8080` | API listen address |
| `DATABASE_URL` | _(empty)_ | PostgreSQL connection string for the self-hosted server; **the one required database setting**. Empty is allowed only in development (in-memory stores); startup **fails** in staging/production. Use `sslmode=require` or `verify-full` when the database is on another host |
| `DB_POOL_MODE` | `session` | `session`: `DATABASE_URL` connects straight to PostgreSQL. `transaction`: it goes through PgBouncer in transaction-pooling mode |
| `DATABASE_DIRECT_URL` | _(empty)_ | Optional override for `cmd/migrate`, `adminctl` and `seed`; only needed with `DB_POOL_MODE=transaction`. Defaults to `DATABASE_URL` |
| `DB_MAX_CONNS` | `8` | Per-process pool cap (1–100). Each API replica and the worker have their own pool; see "Database" for sizing against `max_connections` |
| `CORS_ALLOWED_ORIGINS` | `http://localhost:3000` | Comma-separated exact origins; wildcards are rejected |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `PUBLIC_WEB_URL` | `http://localhost:3000` | Public origin of the web app; every link and image in an email is built from it (`WEB_BASE_URL` is a deprecated alias) |
| `RESEND_API_KEY`, `MAIL_DRIVER`, `EMAIL_FROM`, `EMAIL_REPLY_TO` | | Email settings: see "Email". **A key in `.env` makes local runs send real email unless `MAIL_DRIVER=log`** |
| `DEV_SEED_PASSWORD` | _(empty)_ | Development only: shared password of the seed users. Ignored outside `APP_ENV=development`; never logged |
| `AUTH_DEMO_CODE` | _(empty)_ | A demo sign-in code (4–6 digits) accepted for every sign-in besides the emailed one. **Development and staging only: set with `APP_ENV=production` the API and the worker refuse to start.** Never logged; the API logs a warning while it is active. See "Sign-in code" |
| `ADMIN_SESSION_TTL` | `12h` | Absolute lifetime of an administrator session (Go duration, 1m–24h) |
| `ADMIN_SESSION_IDLE_TTL` | `30m` | Administrator inactivity timeout (Go duration; must not exceed `ADMIN_SESSION_TTL`) |
| `ADMIN_MFA_REQUIRED` | `true` in production, `false` in development and staging | `true`: administrators without an authenticator app sign in to an **enrolment-only** session and cannot remove theirs. See "Administrator MFA" |
| `MFA_ENCRYPTION_KEY` | _(empty)_ | 32 random bytes, base64 (`openssl rand -base64 32`); a **secret** sealing the administrators' TOTP secrets (AES-256-GCM). **Required outside development when `ADMIN_MFA_REQUIRED` is true (startup fails).** Unset in staging/production with MFA not required: MFA answers `503 MFA_UNAVAILABLE`. Unset in development: a fixed, public development key (loud warning). Never logged |
| `ANALYTICS_RAW_RETENTION_DAYS` | `90` | Days raw analytics events are kept before the worker deletes them (7..3650); rollups are kept. See "Listing analytics" |
| `STORAGE_DRIVER`, `STORAGE_LOCAL_DIR`, `S3_*` | | Private file storage for onboarding documents: see "File storage". **Staging and production must use `STORAGE_DRIVER=s3`** |
| `MALWARE_SCANNER`, `CLAMD_ADDRESS`, `MALWARE_SCAN_TIMEOUT`, `MALWARE_SCAN_MAX_BYTES` | `none` | Malware scanning of uploads: see "Uploads and malware scanning". **Off unless `MALWARE_SCANNER=clamav`; the ClamAV host is yours to provide** |
| `PAYMENT_GATEWAY`, `PAYMENT_FAKE_WEBHOOK_SECRET`, `PAYMENT_RECONCILE_AFTER` | `none` | Online payment of subscription invoices: see "Online payment of subscription invoices". **Off unless set; only the `fake` driver exists, refused in production** |
| `FCM_PROJECT_ID`, `FCM_SERVICE_ACCOUNT_JSON`, `FCM_SERVICE_ACCOUNT_FILE` | _(empty)_ | Push through FCM: see "Notifications and push". Without them push is **disabled** in staging/production (one log line, never a startup failure) and logged in development. The key is a secret, never logged |
| `ADMINCTL_PASSWORD` | _(empty)_ | `cmd/adminctl` only: the administrator password, as an alternative to stdin |

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz` | Liveness (always 200) |
| GET | `/readyz` | Readiness (pings the database when configured; reports the store in use) |
| GET | `/v1/countries`, `/v1/markets` | Every country (249); `?has_listings=true` for the ones the public is shown; `?q=` for drop-downs. `/v1/markets` is the older name of the same data |
| GET | `/v1/reference`, `/v1/reference/{set}` | Reference data behind every drop-down, filter and list (public, cacheable) |
| GET | `/v1/listings`, `/v1/listings/suggest`, `/v1/listings/{slug}` | Public listing search, suggestions and detail |
| GET | `/v1/directory`, `/v1/directory/{kind}`, `/v1/directory/{kind}/{slug}`, `/v1/sitemap` | Public directories, profiles and sitemap data |
| GET | `/v1/directory/{kind}/{slug}/logo` | A public profile's logo: the image itself, cacheable, no session |
| GET | `/v1/developments`, `/v1/developments/{market}/{slug}` | Public development projects (API 0.44.0): list (`market`, `city`, `status`, `q`, paging) and detail with phases, unit types and available listings; no contact details |
| GET | `/v1/development-photos/{photoId}/{size}` | One size of a development photo (public while the project is) |
| GET, POST, PATCH, PUT, DELETE | `/v1/agent/organisations/{id}/developments/...` | A developer's projects: CRUD, submit, withdraw, phases, unit types, photos, listing links, enquiries (OWNER / MANAGER / AGENT; STAFF read; 404 for anybody else) |
| GET, POST | `/v1/admin/developments`, `/{id}`, `/{id}/approve`, `/{id}/reject` | The development review queue (the listing reviewers; audited) |
| GET | `/v1/me/profiles` | The directory profiles the caller may manage |
| PUT, DELETE | `/v1/me/profiles/{kind}/{slug}/logo` | Set (multipart) or remove a profile's logo |
| GET, PUT | `/v1/me/listings/{id}/hourly` | A listing's hourly settings (API 0.33.0) |
| GET | `/v1/venues/{listingId}/availability` | Public availability in the venue's local time |
| POST | `/v1/venues/{listingId}/quotes` | Public itemised quote (enquiry mode, no payment) |
| GET, POST | `/v1/me/venue-bookings`, `/v1/me/venue-bookings/{id}` (+ `/cancel`, `/reschedule`, `/accept`) | A customer's hourly bookings |
| GET, POST | `/v1/agent/organisations/{id}/venue-bookings[/{bookingId}]` (+ `/confirm`, `/decline`, `/cancel`, `/propose`, `/complete`, `/no-show`), `/v1/me/hosted-venue-bookings` | Hourly bookings a host received |
| GET, POST, DELETE | `/v1/me/devices`, `/v1/me/devices/{deviceId}` | Push devices (API 0.35.0) |
| GET, POST | `/v1/me/notifications` (+ `/unread-count`, `/{id}/read`, `/read-all`) | In-app notifications and the unread badge |
| GET, PUT | `/v1/me/notification-preferences` | Email and push per type (essential types locked on) |
| GET | `/v1/marketing-consent-texts/{version}` | One marketing consent wording by version (public, cacheable; API 0.37.1) |
| GET, PUT | `/v1/me/marketing-preferences` | The caller's marketing opt-in with its newest consent records; a change is recorded with its `X-Client` source (API 0.37.1) |
| GET | `/v1/agent/organisations/{id}/reports/lead-funnel`, `/v1/me/reports/lead-funnel` | The request inbox's lead funnel, response-time KPIs and breakdowns, JSON or `?format=csv` (API 0.38.0) |
| GET | `/v1/me/appointments/{id}/calendar.ics`, `/v1/inbox/appointments/{id}/calendar.ics`, `/v1/me/venue-bookings/{bookingId}/calendar.ics`, `/v1/me/hosted-venue-bookings/{bookingId}/calendar.ics`, `/v1/agent/organisations/{id}/venue-bookings/{bookingId}/calendar.ics` | "Add to calendar" iCalendar downloads for the parties who can see the appointment or booking, 404 otherwise (API 0.39.0) |
| POST | `/v1/admin/auth/login/mfa` | Third step of the administrator sign-in for an administrator with an authenticator: a TOTP or recovery code (API 0.40.0) |
| GET, POST, DELETE | `/v1/admin/auth/mfa`, `/v1/admin/auth/mfa/totp/setup`, `/v1/admin/auth/mfa/totp/confirm`, `/v1/admin/auth/mfa/recovery-codes/regenerate`, `/v1/admin/auth/mfa/totp` | The signed-in administrator's own MFA: status, set up, confirm, new recovery codes, remove (API 0.40.0) |
| GET, POST | `/v1/admin/users/{id}/mfa`, `/v1/admin/users/{id}/mfa/reset` | A SUPER_ADMIN reads or resets another administrator's MFA (API 0.40.0) |
| GET | `/v1/admin/notifications/health` | Delivery counts, dead-letter backlog, oldest unattempted push |
| GET | `/v1/admin/file-scans` | Malware scanning: counts by status and the latest PENDING and INFECTED files (SUPER_ADMIN, PLATFORM_ADMIN; API 0.41.0) |
| GET, POST, PATCH | `/v1/agent/organisations/{id}/ledger/*` (`/options`, `/entries[/{entryId}]` + `/reverse`, `/correct`, `/reconcile`, `/notes`, `/attachments`; `/reports/*`, `/categories`) | The agent ledger / books, per currency (API 0.34.0) |
| GET, POST, PATCH, DELETE | `/v1/agent/organisations/{id}/properties[/{propertyId}]` (+ `/units[/{unitId}]`) | Properties and their units (API 0.36.0) |
| GET, POST, PATCH, DELETE | `/v1/agent/organisations/{id}/tenancies[/{tenancyId}]` (+ `/options`, `/activate`, `/end`) | Tenancies (API 0.36.0) |
| GET, POST | `/v1/agent/organisations/{id}/tenancies/{tenancyId}/dues`, `/receipts[/{receiptId}]`, `/statement`; `/dues/{dueId}/waive`; `/arrears` | Rent schedule, receipts, statement and arrears (API 0.36.1) |
| GET | `/v1/me/tenancies/{id}/dues`, `/receipts`, `/statement` | The tenant's own rent (API 0.36.1) |
| GET, POST, PATCH, DELETE | `/v1/agent/organisations/{id}/maintenance[/{requestId}]` (+ `/options`, `/providers`, `/status`, `/comments`, `/assign`, `/costs`, `/photos[/{photoId}]`) | Maintenance requests (API 0.36.2) |
| GET, POST | `/v1/me/tenancies/{id}/maintenance[/{requestId}]` (+ `/options`, `/comments`, `/photos[/{photoId}]`) | The tenant's own maintenance requests (API 0.36.2) |
| GET, POST, PATCH, PUT, DELETE | `/v1/agent/organisations/{id}/inspections[/{inspectionId}]` (+ `/options`, `/checklist`, `/findings`, `/start`, `/complete`, `/cancel`, `/report.pdf`, `/items/{itemId}/photos`, `/photos/{photoId}`) | Inspections (API 0.36.3) |
| GET | `/v1/me/tenancies/{id}/inspections[/{inspectionId}]` (+ `/report.pdf`, `/photos/{photoId}`) | The tenant's own completed inspections, read-only (API 0.36.3) |
| GET, POST, PATCH, DELETE | `/v1/agent/organisations/{id}/reviews[/{reviewId}]` (+ `/eligibility`, `/received`, `/{reviewId}/reply`) | Reviews a book wrote of service providers; the provider's received reviews and reply (API 0.36.4) |
| GET, POST, PATCH, DELETE | `/v1/me/reviews[/{reviewId}]` (+ `/eligibility`) | A customer's own reviews of service providers (API 0.36.4) |
| POST | `/v1/reviews/{id}/report` | Report a published review (signed in; API 0.36.4) |
| GET | `/v1/directory/{kind}/{slug}/reviews` | A profile's published reviews with replies (public, cacheable; API 0.36.4) |
| GET, POST | `/v1/admin/reviews[/{reviewId}]` (+ `/publish`, `/hide`, `/reject`, `/remove-reply`) | Review moderation (SUPER_ADMIN, CONTENT_ADMIN, MODERATOR; API 0.36.4) |
| GET | `/v1/markets/{code}/packages` | A country's active listing packages with prices, for the pricing page (public, cacheable; API 0.37.0) |
| GET, POST, PATCH, DELETE | `/v1/admin/packages[/{packageId}]` | Listing packages per market (SUPER_ADMIN, PLATFORM_ADMIN, MARKET_ADMIN for its markets; audited; API 0.37.0) |
| GET, POST | `/v1/admin/organisations/{id}/package` (+ `/end`), `/v1/admin/users/{id}/package` (+ `/end`) | Assign or end an account's package; usage and history (API 0.37.0) |
| GET | `/v1/agent/organisations/{id}/package`, `/v1/me/package` | The account's package and quota usage per market (API 0.37.0) |
| GET | `/v1/me/tenancies`, `/v1/me/tenancies/{id}` | A tenant's own tenancies, read-only (matched by verified email) |
| GET | `/v1/trust/summary` | How many advertisers are verified and how many listings are public (public, cacheable) |
| POST | `/v1/property-requests` | The single entry point for every customer enquiry (guest or signed-in) |
| POST | `/v1/business-enquiries` | The only way to contact a directory profile (guest or signed-in) |
| GET | `/v1/inbox/business-enquiries` | Business enquiries received by the caller's organisation(s) |
| POST | `/v1/inbox/business-enquiries/{enquiryId}/replied` | Mark a received enquiry as replied to (idempotent; API 0.36.4) |
| GET | `/v1/auth/captcha` | The question a registration must answer (public, never cached) |
| POST | `/v1/auth/register` | Create an account (needs an answered captcha); always `202 VERIFICATION_SENT` for valid input |
| POST | `/v1/auth/verify-email` | Consume the emailed token, verify the email and sign in |
| POST | `/v1/auth/resend-verification` | Always `202 VERIFICATION_SENT` |
| POST | `/v1/auth/login` | Sign in (`token_delivery`: `cookie` default, or `bearer`) |
| POST | `/v1/auth/logout` | Revoke the current session; `204` even when not signed in |
| POST | `/v1/auth/password-reset/request` | Always `202 RESET_SENT` |
| POST | `/v1/auth/password-reset/confirm` | Set a new password; revokes all of the user's sessions |
| GET | `/v1/me` | The signed-in user (roles, memberships) and the session (`scope`, `expires_at`) |
| GET | `/v1/onboarding/requirements` | What each professional role must provide: details and documents (public, cacheable) |
| GET | `/v1/me/onboarding` | The caller's onboarding applications |
| PUT | `/v1/me/onboarding/{role}` | Save details (partial); after a rejection, start a new application |
| POST, DELETE, GET | `/v1/me/onboarding/{role}/documents`, `.../documents/{id}`, `.../documents/{id}/file` | Attach (multipart), remove and download the caller's own documents |
| POST | `/v1/me/onboarding/{role}/submit` | Submit for review |
| GET | `/v1/admin/onboarding`, `/v1/admin/onboarding/{id}`, `.../documents/{docId}/file` | Review queue, one application in full, a document (admin-scope session; SUPER_ADMIN, PLATFORM_ADMIN, MODERATOR, MARKET_ADMIN) |
| POST | `/v1/admin/onboarding/{id}/approve`, `/reject`, `/request-changes` | Decide an application |
| POST | `/v1/admin/auth/login` | Administrator sign-in (platform-role accounts only; cookie only) |
| GET | `/v1/admin/me` | Same as `/v1/me`, but requires an admin-scope session and a platform role |
| GET | `/v1/agent/property-requests` | Requests received by the caller: their organisation's (scoped by role) and/or their own as a direct seller; paged, filtered, with counts per lead stage |
| GET, PATCH | `/v1/agent/property-requests/{id}` | One received request with its timeline and internal notes; move its lead stage |
| POST | `/v1/agent/property-requests/{id}/notes` | Add an internal note (never shown to the customer) |
| GET | `/v1/agent/organisations/{id}/members` | The members of an organisation, read-only (its OWNER and MANAGER) |
| GET | `/v1/me/dashboard` | One role-aware summary for the signed-in account: kind, counters, next actions, recent items |
| GET | `/v1/me/property-requests`, `/{id}` | The requests this account sent, in the customer's own terms |
| GET | `/v1/me/saved-listings`, `/ids` | The listings this account saved, newest first, with a snapshot for those no longer public; the IDs of those still public (see "Saved properties", 0.27.0) |
| PUT, DELETE | `/v1/me/saved-listings/{listingId}` | Save a public listing (idempotent, `created` flag) / remove a save (always `204`) |
| POST | `/v1/analytics/events` | Browser analytics events (public; see "Listing analytics", 0.30.0) |
| GET | `/v1/analytics/listings/{id}`, `/v1/analytics/organisations/{orgId}` | A listing's / an organisation's figures, owner-scoped (404 otherwise) |
| GET, POST | `/v1/agent/organisations/{id}/contacts` | The organisation's contact book: list (filters, `stage_counts`) / create (duplicate check); see "Agent CRM", 0.31.0 |
| GET, PATCH, DELETE | `/v1/agent/organisations/{id}/contacts/{contactId}` | One contact / edit / soft delete; `.../notes`, `.../reminders[/{rid}]`, `.../stage`, `.../owner`, `.../listings[/{listingId}]`, `.../timeline` |
| GET, POST | `/v1/agent/organisations/{id}/contacts/reminders`, `/tags`, `/assignees`, `/export.csv`, `/merge`, `/import`, `/from-request/{requestId}` | Due reminders, tags, who may own contacts, CSV export and import (OWNER and MANAGER), merge, one-click contact |
| GET, POST, PATCH | `/v1/agent/organisations/{id}/forms`, `/{formId}`, `/{formId}/publish`, `/{formId}/retire` | Screening questionnaires, versioned (see "Questionnaires and applications", 0.32.0) |
| GET, PUT | `/v1/me/listings/{id}/applications-intake` | A listing's questionnaire and intake (OPEN, PAUSED, CLOSED) |
| GET | `/v1/listings/{slug}/application-form` | The questionnaire of a public listing (preview) |
| GET, POST, PATCH | `/v1/me/applications`, `/{applicationId}`, `/files[/{fileId}]`, `/submit`, `/withdraw` | The customer's own applications: start, autosave, files, submit, withdraw |
| GET, POST | `/v1/agent/organisations/{id}/applications`, `/{applicationId}`, `/files/{fileId}`, `/status`, `/request-info`, `/notes` | Applications received (reads audited) |
| GET | `/v1/admin/overview` | Counts an administrator starts from, sections by platform role |
| GET, POST | `/v1/admin/routing-exceptions`, `/{id}/resolve` | Requests nobody could receive, and marking one as dealt with |
| POST | `/v1/appointments` | A signed-in customer asks for a viewing or a consultation |
| GET, POST | `/v1/me/appointments`, `/{id}`, `/{id}/cancel`, `/{id}/reschedule`, `/{id}/accept` | The customer's own appointments |
| GET, POST | `/v1/inbox/appointments`, `/{id}`, `/{id}/confirm`, `/reschedule`, `/cancel`, `/complete`, `/no-show`, `/assign` | Appointments received by a professional (list, calendar and actions) |
| GET | `/v1/inbox/summary` | Badge counts: new requests, new enquiries, appointments |
| GET | `/v1/listings/{slug}/availability` | A short stay's calendar as the public sees it: available or not per night |
| GET | `/v1/me/listings` | The listings the caller may manage (summaries; `can_edit` per item since 0.26.1) |
| POST, GET, PATCH, DELETE | `/v1/me/listings`, `/v1/me/listings/{id}` | Create a draft, read, edit, delete a listing (see "Listing management") |
| POST | `/v1/me/listings/{id}/submit`, `/archive`, `/relist` | Send for review, withdraw, bring back |
| POST, PUT, PATCH, DELETE | `/v1/me/listings/{id}/photos`, `/photos/order`, `/photos/{photoId}` | Upload (at most 20), order, describe and delete photos |
| GET | `/v1/listing-photos/{photoId}/{size}` | A listing photo: public while the listing is, else managers and reviewers only |
| GET, POST | `/v1/admin/listings`, `/{id}`, `/{id}/approve`, `/request-changes`, `/reject` | Listing review queue and decisions |
| GET, PUT | `/v1/me/listings/{id}/availability` | The lister's calendar of a short stay: read it, mark nights available, blocked or booked |
| PATCH | `/v1/me/listings/{id}/stay-rules` | A short stay's `min_nights` and `max_guests` |
| GET, POST, PATCH | `/v1/admin/lease-templates`, `/{id}`, `/{id}/approve`, `/{id}/retire` | The sample lease templates anyone may use or copy (SUPER_ADMIN, CONTENT_ADMIN) |
| GET | `/v1/admin/leases` | Every lease, read-only (SUPER_ADMIN, SUPPORT) |
| GET, POST, PATCH | `/v1/me/organisations/{orgId}/lease-templates`, `/{id}`, `/{id}/publish`, `/{id}/retire`, `/{id}/events` | An organisation's own lease templates (its OWNER and MANAGER manage them, its agents read them; see "Leases") |
| GET | `/v1/me/lease-templates` | The templates a lease can be made from: the samples and the published templates of the caller's organisations |
| GET, POST, PATCH, DELETE | `/v1/me/leases`, `/{id}`, `/{id}/pdf`, `/{id}/send`, `/{id}/cancel`, `/{id}/events` | Draft leases and send them for signing (see "Leases") |
| GET, POST, PATCH, PUT, DELETE | `/v1/me/agreements`, `/{id}`, `/{id}/document`, `/{id}/fields`, `/{id}/pdf`, `/{id}/send`, `/{id}/cancel`, `/{id}/recipients/{rid}/replace`, `/remind`, `/{id}/events` | Documents sent for signature (see "Agreements") |
| GET, POST | `/v1/me/signing-requests`, `/{recipientId}/link`; GET, PUT `/v1/me/signature` | Documents waiting for the signed-in person; their saved signature, initials and stamp |
| GET, POST | `/v1/signing/{token}`, `/pdf`, `/code`, `/submit`, `/decline` | Signing by link (no session) |
| POST | `/v1/stamp-preview` | A generated stamp as a transparent PNG (public, no session; see "Agreements") |
| GET, POST | `/v1/verify/{reference}`, `/v1/verify` | Check a completed document (public) |
| GET | `/v1/admin/agreements` | Every agreement, read-only (SUPER_ADMIN, SUPPORT) |
| DELETE | `/v1/me` | Delete one's own account, confirmed with the password (not for administrator accounts; see "Administrators' area") |
| GET | `/v1/me/subscription-plans` | The subscription plans the caller may take |
| GET, POST | `/v1/me/subscriptions`, `/{id}`, `/{id}/cancel`, `/{id}/renew` | The caller's subscriptions and those of the organisations they own: ask for a plan, withdraw a pending request, ask for the next period |
| GET, POST | `/v1/admin/users`, `/{id}`, `/{id}/suspend`, `/{id}/reactivate`, `/{id}/delete` | Accounts: read (SUPER_ADMIN, PLATFORM_ADMIN, SUPPORT); suspend, reactivate, delete (SUPER_ADMIN, PLATFORM_ADMIN) |
| GET | `/v1/admin/audit` | The administrators' audit trail (SUPER_ADMIN, PLATFORM_ADMIN) |
| GET, POST, PATCH | `/v1/admin/subscription-plans`, `/{id}`, `/{id}/retire` | Subscription plans: read (SUPER_ADMIN, FINANCE_ADMIN, PLATFORM_ADMIN, SUPPORT); create, change, retire (SUPER_ADMIN, FINANCE_ADMIN) |
| GET, POST | `/v1/admin/subscriptions`, `/{id}`, `/{id}/approve`, `/{id}/reject`, `/{id}/cancel` | Subscriptions: read (SUPER_ADMIN, FINANCE_ADMIN, PLATFORM_ADMIN, SUPPORT); record a payment, reject, cancel (SUPER_ADMIN, FINANCE_ADMIN) |
| GET, POST, PATCH | `/v1/admin/reference`, `/{set}`, `/{set}/{code}`, `/{set}/{code}/deactivate`, `/reactivate` | Change reference data (SUPER_ADMIN, PLATFORM_ADMIN, CONTENT_ADMIN; MARKET_ADMIN for its own market's rows) |
| GET | `/v1/admin/analytics/summary` | Site and market analytics (SUPER_ADMIN, PLATFORM_ADMIN, MARKET_ADMIN for its markets) |
| GET | `/v1/admin/reports/summary`, `/users.csv`, `/listings.csv`, `/subscriptions.csv` | Report summary (SUPER_ADMIN, PLATFORM_ADMIN, FINANCE_ADMIN); user and listing exports (SUPER_ADMIN, PLATFORM_ADMIN); subscription export (SUPER_ADMIN, PLATFORM_ADMIN, FINANCE_ADMIN) |

Every error uses one envelope, and `trace_id` equals the `X-Request-Id` response header:

```json
{"error":{"code":"VALIDATION_FAILED","message":"...","fields":{"message":"..."}},"trace_id":"..."}
```

### The property-request rule

Every customer enquiry goes through `POST /v1/property-requests`. In **one database transaction**
the API writes the `property_requests` row, a `request_timeline` entry, a `customer_activity` entry
and an `outbox_events` row, and answers `201` only after the commit. Notifications are sent later
by the worker from the outbox.

- **Idempotency** – a key is required (body `idempotency_key`, or `Idempotency-Key` header; the body
  wins). Same key + same content → `200` with the original receipt. Same key + different content →
  `409`. Content is compared by SHA-256 of the canonicalised payload. A unique constraint on the key
  makes concurrent retries safe.
- **Routing** – recipient is the listing's assigned agent, else the organisation inbox, else the
  direct seller. If none is eligible the request is still saved and the customer still gets
  `201 RECEIVED`; internally it is stored as `ROUTING_EXCEPTION` for operations to resolve.
- **Wording** – the API only ever says `RECEIVED`. It never claims a request was delivered.
- **Guests and signed-in customers** – authentication is optional. A guest must send
  `guest_contact` (name, email). A signed-in customer may omit it: name and email come from the
  account (anything sent for them is ignored), the user ID is stored as `customer_user_id`, and
  only `guest_contact.phone` is read. The phone stays mandatory for `PHONE`/`WHATSAPP` (field
  `guest_contact.phone`) and `consent.accepted` must be true in both cases.

### Business enquiries: profiles are contacted only by enquiry

Directory profiles — agencies, agents, developers, property managers, hosts and service providers
— **never expose contact details**. The backend stores no phone number, email address or street
address for a profile (the `directory_profiles` table has no such columns) and has **no public
profile endpoints**. The only way to reach a profile is `POST /v1/business-enquiries`:

```json
{
  "recipient": { "kind": "service-providers", "slug": "sample-plumbers" },
  "service": "Burst pipes and leaks",
  "message": "The geyser is leaking. Can someone come this week?",
  "preferred_contact_method": "EMAIL",
  "idempotency_key": "6f1c2f0a-8f0e-4f43-9a55-2f6f3f1f9c11",
  "guest_contact": { "name": "Tariro Moyo", "email": "tariro@example.com" },
  "consent": { "privacy_version": "2026-01", "accepted": true }
}
```

It follows the property-request rule exactly: one transaction writes the `business_enquiries` row,
a `business_enquiry_timeline` entry, a `customer_activity` entry and an outbox event
`BUSINESS_ENQUIRY_CREATED` (identifiers only), and the answer is
`201 {"enquiry_id","reference":"BE-XXXXXXXX","status":"RECEIVED","created_at"}`. It never claims
delivery.

- **Idempotency, guests vs signed-in customers, phone and consent rules** – identical to property
  requests (`200` replay, `409 IDEMPOTENCY_CONFLICT`; `guest_contact.name`/`email` required for
  guests; only `guest_contact.phone` read for signed-in customers, whose user ID is recorded). The
  idempotency key space is separate from property requests.
- **Errors** – `400 VALIDATION_FAILED` (fields `recipient.kind`, `recipient.slug`, `service`,
  `message`, `preferred_contact_method`, `guest_contact.name|email|phone`, `consent.accepted`),
  `404 RECIPIENT_NOT_FOUND` for an unknown kind + slug, `410 RECIPIENT_UNAVAILABLE` for a profile
  that is suspended or not yet verified, `429` from its own per-IP bucket (burst 10, 10/min).
- **Routing** – to the inbox of the organisation that owns the profile. For kind `agents`: to the
  named agent, and their agency's `OWNER`/`MANAGER` see it too. If the agent can no longer receive
  it, it falls back to the agency. If nobody can, the enquiry is still saved and the customer still
  gets `201 RECEIVED`; internally it is a `ROUTING_EXCEPTION`.
- **Inbox** – `GET /v1/inbox/business-enquiries` (newest first, capped at 100, **no pagination
  yet**). `401` when anonymous, `403 FORBIDDEN` without an organisation membership. `OWNER` and
  `MANAGER` see every enquiry routed to their organisation, including those addressed to its
  agents' profiles; `AGENT` and `STAFF` see only the enquiries addressed to their own agent
  profile. Another organisation gets nothing.
- **Where the customer's contact details go** – they are stored on the enquiry and returned
  **only** by that inbox, to the authorised recipient. They are never returned by any other
  endpoint, never put in the outbox payload and never logged.
- **Notifications.** The customer gets an "enquiry received" email and the recipient a "new
  enquiry" email with a reference, a preview with contact details removed and a button to the
  inbox (see "Email"). The customer's contact details are never put in an email.

## Dashboards and working a request

Added in API 0.20.0 (migration `0018`). The specification describes no dashboard widgets, so the
dashboards are derived from what each role can already do; every number is read through the
module that owns it, with that module's own scoping (the `dashboard` module owns no data).

- **`GET /v1/me/dashboard`** – one body per signed-in account: `kind` (`SEEKER`,
  `PROFESSIONAL_PENDING`, `PROFESSIONAL`, `DIRECT_OWNER`, `ADMIN`), roles, organisations with
  the caller's role and what it allows there, `capabilities`, onboarding status per role,
  `counters` (stable keys such as `requests.new`, `requests.stage.FOLLOW_UP_REQUIRED`,
  `appointments.requested`, `listings.status.PUBLISHED`, `short_stays.calendar_stale`,
  `my_requests.status.RECEIVED`), `needs_attention`, `next_actions` (stable codes, text and a
  `target` key such as `requests` or `onboarding` that the client maps to a page) and `recent`
  (5 newest received requests, 5 newest sent, next 5 appointments). The badge numbers are the
  ones of `GET /v1/inbox/summary` (same code). The OpenAPI lists every key, code and target.
- **Lead stages** – `NEW_ENQUIRY`, `ASSIGNED`, `CONTACTED`, `VIEWING_REQUESTED`,
  `VIEWING_SCHEDULED`, `FOLLOW_UP_REQUIRED`, `CONVERTED`, `CLOSED` (the updated functional
  specification's eight, as the owner decided over the FSD's older list). Allowed moves are a
  fixed table in `propertyrequests/work.go`: nothing returns to `NEW_ENQUIRY`, `CONVERTED` needs
  prior contact or a viewing and can only be closed, `CLOSED` reopens only as
  `FOLLOW_UP_REQUIRED`. Anything else is `409 INVALID_STAGE_TRANSITION`. Who may move a
  request: whoever sees it in their inbox (same rule as the list). The move, its timeline entry
  (who, when, from, to) and the outbox event are one transaction, with the row locked.
- **Internal notes** – `POST /v1/agent/property-requests/{id}/notes`; visible to everyone who
  sees the request in their inbox, never on a customer endpoint (a test checks the customer
  bodies for the text). The timeline records that a note was added, not what it says.
- **The customer's own requests** – `GET /v1/me/property-requests[/{id}]`, linked by
  `customer_user_id` (requests sent while signed in). A guest request is never linked, even with
  the same email. The customer sees a status derived from the stage (`RECEIVED`, `IN_PROGRESS`,
  `RESPONDED`, `VIEWING_SCHEDULED`, `CLOSED`) and a timeline of those statuses only; a request
  nobody could receive stays `RECEIVED` to them.
- **Inbox filters** – `stage`, `type`, `listing_id`, `from`, `to`, `q` (reference, name, email,
  message), `page`, `page_size`; `stage_counts` per stage. Filters never widen the scope.
- **Team** – see "Agency teams" below (API 0.23.0).
- **Administrators** – `GET /v1/admin/overview` (accounts and recent sign-ups for SUPER_ADMIN,
  PLATFORM_ADMIN, SUPPORT; verifications for whoever reviews them; listings by status for
  SUPER_ADMIN, PLATFORM_ADMIN, MODERATOR; requests by stage and routing exceptions for
  SUPER_ADMIN, PLATFORM_ADMIN, SUPPORT; MARKET_ADMIN gets its own countries' verifications,
  listings and requests; FINANCE_ADMIN and CONTENT_ADMIN get no section).
  `GET /v1/admin/routing-exceptions` and `POST .../{id}/resolve` list and close the requests
  nobody could receive (routing them to someone is not built).
- Working a request and resolving an exception share one per-IP limit: bursts of 60, refilled at
  60 per minute.

**Not built** (dashboards and requests): replies to the customer and any email about a stage
change; follow-up dates and reminders; editing or deleting a note; listing analytics; seeker
favourites, saved searches and alerts. (Conversion funnels and response-time KPIs: API 0.38.0,
below.)

## Listing expiry and reminders (API 0.45.0)

FR-006, FR-044, FR-053. An approval publishes a listing for 90 days (`expires_at`), one publication
cycle (`publication_cycles`, `relist_no` 0 for the first). The **read rule is unchanged**: a
`PUBLISHED` listing whose `expires_at` has passed reads `EXPIRED` everywhere at once (public
filter, counts, quotas, `EffectiveStatus`), so a late worker never keeps an expired listing public.

- **The sweep** (`listings.SweepExpiry`, `internal/modules/listings/expiry.go`) runs in
  `cmd/worker` at start and then **every hour** (`app.ListingExpirySweepInterval`; hourly rather
  than daily so the expiry and the 1-day reminder are never up to a day late). In batches of 200,
  each in one transaction, it claims (`FOR UPDATE SKIP LOCKED`) the `PUBLISHED` listings with
  `expires_at <= now + 14 days` that have a notice due:
  - past the end: status **stored** `EXPIRED`, a timeline entry (actor `SYSTEM`, action
    `EXPIRED`), the notice `EXPIRED`, and the outbox event `LISTING_EXPIRED` (the search-index
    event, now also emailed). A pending revision is left as it is (a relist merges it).
  - otherwise the smallest threshold N of 14, 7 and 1 days with `expires_at - now <= N days`: the
    notice for N, the larger ones not yet recorded as `skipped`, and `LISTING_EXPIRING`
    `{days: N, expires_at, relist_no}` when the notice for N was recorded now. A worker that was
    down sends only the most recent reminder, never a stale "14 days".
  - `listing_expiry_notices (listing_id, relist_no, kind)` is the primary key: every reminder and
    every expiry happens **once per publication cycle**, across reruns and several workers. A
    relist's approval starts the next cycle, with reminders of its own.
- **Emails and in-app** (to the advertiser, as the review emails: the listing's agent, else the
  organisation's owners and managers, or the direct owner): `listing-expiring` (14 / 7 / 1 days),
  `listing-expired`, and `listing-relisted` instead of `listing-approved` when the approved NEW
  review is a relist (`LISTING_APPROVED` payload `relist: true`). In-app type `LISTING_EXPIRY`
  ("Listing expiry reminders", not essential: its email and push follow the preference centre;
  the relisted notice is `LISTING_REVIEWED`). Links go to `/agent/listings/{id}`.
- **API**: the managed listing (`GET /v1/me/listings/{id}` and the other management responses),
  the admin listing view, the review queue rows and the rows of `GET /v1/me/listings` gain
  `expiry` `{expires_at, days_left, state: ACTIVE|EXPIRING_SOON|EXPIRED, relist_no}` (`null` when
  never published; `relist_no` is 1 for the first publication). `GET /v1/me/listings` returns the
  effective status, its `status` filter matches it, and it takes `expiry=EXPIRING_SOON|EXPIRED`.
  The lister's dashboard counts use the effective status.
- **Development without a database**: the API process runs the same sweep on the memory store and
  sends the emails in-process.
- **Seed**: the sample listings that are not published write `LISTING_UNPUBLISHED` for the index
  (no longer `LISTING_EXPIRED`, which would now email their advertisers), and a re-run clears the
  sample listings' expiry notices. The sample advertisers are `@example.test` accounts: the Resend
  mailer never sends to reserved test domains (`mail.IsReservedTestAddress`).

## Development and investment showcase (API 0.44.0)

BR-019 / FR-048. Migration `0046_developments`; `modules/developments` (model, memory and Postgres
stores, service, handler), `app/developments.go` (wiring: the developer profiles, the directory's
`projects` and the sitemap). The exact JSON is the 0.44 developments contract. OpenAPI tag
`developments`.

### What existed before 0.44.0 (reused, not duplicated)

| Area | What was there | What 0.44.0 does with it |
| --- | --- | --- |
| Developers | Directory kind `developers` (sample `developers/sample-developments`, owner `sample-developments@example.test`, organisation kind DEVELOPER, account role DEVELOPER); DEVELOPER is a listing-managing kind (`auth.KindManagesListings`) | A project belongs to the organisation's `developers` profile (409 `NO_DEVELOPER_PROFILE` without one). Access is `auth.MayManageListings` (OWNER / MANAGER / AGENT change) and `auth.MayReadListingPackage` (STAFF read) |
| New developments / off-plan | Nothing: listings have no development, off-plan or project field (only the feature `gated-development`) | Projects are their own table; ordinary listings are linked to a project (and a unit type) through `development_listings` |
| Contact | `businessenquiries`: profiles have no contact columns; `POST /v1/business-enquiries` needs a recipient profile; 10/min/IP limit, idempotency, consent | The enquiry gains an optional `development` (market, slug, unit type); with it the recipient is the project's developer. The enquiry stores the project and unit type IDs and name snapshots; inbox items show them. Same route, limiter and validation |
| Photos | Listing photo pipeline (`listings.ProcessPhoto`: three JPEG sizes, metadata stripped) and synchronous malware screening (`storage.Screen`, 0.41.0) | Reused as is (`developmentphotos/` keys, `/v1/development-photos/{id}/{size}`); the key prefix is in `UnscannedKeyPrefixes` (screened before storing) |
| Coordinates, YouTube | 0028 (EXACT / APPROXIMATE, rounded publicly), 0027 (`ParseYouTubeURL`) | Reused through `listings.CheckCoordinates`, `PublicLocationOf`, `VideoOf` (new exported wrappers) |
| Moderation | Listing review queue `/v1/admin/listings` (`auth.ListingReviewRoles`, market scoping, self-review refused) | A separate queue `/v1/admin/developments` with the same roles, scoping and self-review rule; decisions are in `admin_audit_events` (target DEVELOPMENT) |
| Sitemap / SEO | `GET /v1/sitemap` (listings and profiles) | Gains `developments` [{market, slug, updated_at}]; no separate endpoint |
| Reference data | No amenities set (`listing_features` is about a property) | New admin-editable set `development_amenities` (14 rows; creatable through `/v1/admin/reference`) |

### Rules

- **No contact details anywhere public.** No phone, email or street address is stored or accepted;
  free text with an email address, a phone number (written with `+` or a leading 0, nine digits or
  more) or a WhatsApp link is refused (400). The developer card is `{kind, slug, name, verified, logo}`.
  A test walks every public response for `@`, `email`, `phone`, `whatsapp`, `+263`, `address`.
- **Only PUBLISHED projects of a public profile (ACTIVE, verified) of an ACTIVE organisation are
  public** (list, detail, directory `projects`, sitemap, photos, enquiries). Linked listings show only
  while the listing itself is public.
- **Investment highlights are the developer's statements**: always answered with
  `stated_by: DEVELOPER` and a disclaimer; never advice.
- Moderation: DRAFT → submit → PENDING_REVIEW → approve → PUBLISHED, or reject (reason required) →
  REJECTED (editable, can be submitted again). Withdraw takes PENDING_REVIEW / PUBLISHED back to DRAFT.
  While PENDING_REVIEW / PUBLISHED the project's own fields and photos are locked (409
  `DEVELOPMENT_LOCKED`); phases, unit types and listing links still change at once.
- The slug is unique per market (`developments_market_slug_unique`), defaults to the name (`-2`, `-3`
  when taken) and is fixed once published.
- Every route under `/v1/agent/organisations/{id}/developments` answers 404 `DEVELOPMENT_NOT_FOUND`
  to anybody who is not a member of the organisation, before any validation. Project enquiries
  (with the customers' contact details) are for OWNER and MANAGER only, as in the inbox.
- Account deletion keeps the organisation's projects and unlinks the person
  (`developments.ForgetUserTx`; the memory store's `ForgetUser`). Organisations are never deleted by
  the API; the schema cascades projects with their organisation or profile.
- **Not in Elasticsearch**: the public list is a Postgres query, so there is no mapping and no reindex.

### Sample data

`sample-developments@example.test` (DEVELOPER, OWNER): `ZW/borrowdale-heights` "Borrowdale Heights"
(PUBLISHED, SELLING, Harare / Borrowdale, 2 phases, 3 unit types of which one is price on request,
3 bundled photos, a YouTube tour, one linked published listing
`three-bedroom-cluster-home-at-borrowdale-heights-zw-200401`), and `ZW/ruwa-gardens-estate` (DRAFT).
In memory they are loaded by `cmd/api`; in Postgres by `cmd/seed` (`devseed.SeedPostgresDevelopments`,
re-runnable: existing projects are saved over by their fixed IDs). The linked listing is seeded with
the projects, so the sample listing counts of the other tests do not change.

### Backend deviations

No street address (the listing privacy rule); no "request changes" (approve / reject only); no
listing-style revision of a published project (withdraw, edit, resubmit); no separate
`/v1/developments/sitemap` (the existing sitemap is extended); project enquiries for OWNER / MANAGER
only; contact details refused in free text.

## Per-market legal documents, tax rules and translations (API 0.43.0)

BR-001. Migration `0045_markets_legal_tax`; `modules/legal` (document versions, resolution,
acceptances), `modules/tax` (tax rules, `TaxFor`), `platform/i18n` (message catalogue and
language negotiation), `app/legal.go` (wiring). The exact JSON is the 0.43 markets / legal / tax /
i18n contract. OpenAPI tags `legal` and `tax`.

**This is structure only. None of the legal text in the API is final, and none of it is legal
advice.** The content must come from the business's lawyers. **Migration 0045 inserts no document and no tax rule: production starts
with nothing published and nothing live.** The development and staging seed's bodies open with
`DRAFT — awaiting legal review` and carry `draft_notice: true`.

### What existed before 0.43.0 (reused, not duplicated)

| Area | What was there | What 0.43.0 does with it |
| --- | --- | --- |
| Privacy notice versions | `user_consents` (0002) with `consent_type` PRIVACY_NOTICE and a free `version`; registration requires `consent.privacy_version` (≤ 32 printable ASCII), never checked against anything; kept on account deletion | The acceptance store for every legal kind: the type check is widened, `market_code` and `context` added. PRIVACY stays PRIVACY_NOTICE. Registration gains optional `terms_version` and `market_code` |
| Marketing consent texts | `marketing_consent_texts` (0041), versioned and immutable by trigger, DRAFT / APPROVED, current version a Go constant | Same pattern (immutability trigger, new version per change) for legal versions; marketing texts are untouched |
| Markets | `markets` (every ISO country, currency, IANA time zone; `modules/markets`); MARKET_ADMIN carries a market code | Legal and tax validate market codes against it; MARKET_ADMIN scoping as in the reference data |
| Tax | Only the venue quote's `TAX` fee (0032), a percent each venue **owner** sets (the sample showroom: VAT 15%). Subscription invoices carry no tax | Not changed and not wired: it is the owner's tax, not a platform rate. A test proves the quote is identical whatever the tax rules say |
| Legal pages | The web serves `/legal/privacy-policy`, `/legal/terms-and-conditions`, `/legal/refund-policy`, `/legal/eula` from React components (`src/content/legal/*.tsx`, facts in `src/lib/legal.ts`, version `2026-11-14`, marked "not legal advice" in `docs/legal.md`). No cookies or agent-terms page | Kinds TERMS, PRIVACY, REFUND, EULA seeded; COOKIES and AGENT_TERMS valid but unseeded. Bodies are placeholders pointing at the web pages (copying the TSX would create a second diverging copy of unreviewed text) |
| i18n | None: every message a literal English string; the web has no i18n either | `platform/i18n`, English only |

### Legal documents

A version belongs to a kind, a market (`GLOBAL` or a country code) and a language (`en`): status
DRAFT → PUBLISHED → RETIRED. **A published version never changes**: the service refuses it
(`409 LEGAL_VERSION_IMMUTABLE`), the memory store refuses it (`legal.FrozenChange`) and the database
refuses it (trigger `legal_document_versions_frozen`). A change is a new version.

**Resolution** (`legal.Resolve`, pure): the PUBLISHED version with the latest `effective_from` that is
not in the future, for the market, else for GLOBAL; the language, else English. Public routes:
`GET /v1/legal`, `GET /v1/legal/{kind}?market=&lang=&version=`, `GET /v1/legal/{kind}/versions`
(cacheable, `Vary: Accept-Language`). **DRAFTs are never public**; `?preview=true` returns the newest
DRAFT to an administrator of that market (otherwise `403 LEGAL_PREVIEW_FORBIDDEN`, never silently
ignored).

**Administration** `/v1/admin/legal`: SUPER_ADMIN and CONTENT_ADMIN everywhere, MARKET_ADMIN for its
own market (never GLOBAL). Create, edit and delete DRAFTs; publish; retire. Every write is an
`admin_audit_events` record (target `LEGAL_DOCUMENT_VERSION`).

**Acceptances.** Registration records PRIVACY (as before) and, when `consent.terms_version` is sent,
TERMS, with the market. When the current version of a kind has `requires_reacceptance`, `GET /v1/me`
lists it in `pending_legal_acceptances` (`?market=`, default GLOBAL) until the person
`POST /v1/me/legal-acceptances`. **Nothing is blocked**: the web decides how to prompt.

**Account deletion:** acceptances are **kept** (as the privacy-notice records always were; the users
row is anonymised, so they name nobody). Versions keep existing; the deleted person is unlinked as
creator / publisher and named "Deleted user" (`legal.ForgetUserTx`, allowed by the trigger).

**Seed decision.** The migration inserts **nothing** (production: no document, `GET /v1/legal` is an
empty list, `GET /v1/legal/{kind}` is `404 LEGAL_DOCUMENT_NOT_FOUND`, nothing is ever pending, until a
content administrator publishes the lawyers' text). **Development and staging only** (the in-memory
stores, and `cmd/seed` → `devseed.SeedPostgresLegalTax`, guarded by `devseed.CheckEnvironment` as
every sample; re-runnable): GLOBAL TERMS, PRIVACY, REFUND and EULA **PUBLISHED** with the web's
version `2026-11-14` and a short placeholder body that says DRAFT and links to the web page
(`draft_notice: true`), and DRAFT copies for ZW and GB. **The web keeps serving its own pages**;
nothing on the site reads these routes yet.

### Tax rules

`tax_rules` per market and use (SUBSCRIPTIONS, VENUE_BOOKINGS, LISTING_FEES), rate as a decimal string
percent, inclusive or exclusive, `[effective_from, effective_to)`, registration-number label, ACTIVE /
RETIRED, `is_sample`. `tax.TaxFor(rules, market, appliesTo, at)` is the pure lookup; overlapping ACTIVE
rules are refused. Administration `/v1/admin/tax-rules` (SUPER_ADMIN, FINANCE_ADMIN; audited,
target `TAX_RULE`), with `/lookup`. **No rule exists in production** (the migration inserts none). The
development and staging seed adds ZW VAT 15% and GB VAT 20% (ACTIVE, `is_sample: true`), **SAMPLE values
to confirm with a tax adviser** (15% matches the sample showroom's VAT). **Nothing applies them**: no tax is
charged anywhere new. When the platform starts charging tax on its own fees, call `TaxFor` where the
invoice is issued and keep `is_sample` rules out (confirm them first with `PATCH {"is_sample": false}`).

### Translations

`i18n.T(ctx, key, args)`; `i18n.Middleware` (in the router) picks `?lang=` if supported, else the best
`Accept-Language` match (q-values, `en-GB` → `en`), else `en`; an unknown language is never an
error. Missing keys fall back to English, then to the key. Converted so far: the error envelope's
shared messages (not found, method not allowed, invalid JSON / media type / size, internal, timeout,
sign in, forbidden; `httpx.WriteValidation`) and the legal and tax routes. Everything else is still
literal English: convert place by place. `GET /v1/i18n/languages` lists the languages.

**Adding a language:** add it to `i18n.Languages()`; add `internal/platform/i18n/catalogue_<code>.go`
with a map like `catalogueEN` and register it in `catalogues` (missing keys fall back to English, so a
partial translation is safe); run the tests (`TestCataloguesUseKnownKeys` refuses unknown keys and
mismatched `{placeholders}`); translate legal documents as new versions with that `lang` (the admin
API accepts any listed language).

### Payment gateways per market: next step (not built)

0.42.0 has one gateway for the whole platform (`PAYMENT_GATEWAY`, `paygateway.Open`). Per-market
gateways would be:

1. A mapping `payment_gateway_markets (market_code PK REFERENCES markets(code), provider text, enabled
   boolean, updated_at)`, or for a handful of markets config `PAYMENT_GATEWAY_<MARKET>=<driver>`
   (e.g. `PAYMENT_GATEWAY_ZW=paynow`, `PAYMENT_GATEWAY_GB=stripe`), **defaulting to the global
   `PAYMENT_GATEWAY`** for every market without an entry.
2. `paygateway.Open` returns a registry `map[provider]Gateway`; `payments.Service` resolves the gateway
   from the subscription's `market_code` (already stored on every payment) at checkout; webhooks are
   already routed by `/v1/payments/webhooks/{provider}` and `payments.provider`, so no change there.
3. `online_payment.enabled/provider` in `GET /v1/me/subscription-plans` becomes per market.

It was not built in 0.43.0: only the `fake` driver exists, so a mapping would have nothing to choose
between, and a wrong default could offer the fake page in a market. Build it with the first real
provider.

**Not verified / follow-ups:** migration `0045` and the Postgres stores have only run where
`RP365_TEST_DATABASE_URL` is set (`TestStoreContractPostgres` in `modules/legal` and `modules/tax`).
The legal text itself, the tax rates and the choice of re-acceptance wording are the business's and
its lawyers' to supply.

## Online payment of subscription invoices (API 0.42.0)

BR-015, FR-037-040. Migration `0044_online_payment`; `platform/paygateway` (the provider-neutral
adapter and the `fake` driver), `modules/payments` (checkouts, webhooks, refunds, reconciliation,
the payments export), `app/payments.go` (wiring, the packages link, the reconciliation loop). OpenAPI
tag `payments`. The exact JSON is the 0.42 online payment contract.

**Online payment is OFF unless configured, and no real provider is built.** With `PAYMENT_GATEWAY`
unset (`none`) nothing is offered (`online_payment.enabled: false`, `404 ONLINE_PAYMENT_NOT_OFFERED`)
and subscriptions work exactly as before: invoice, bank transfer, the administrator's approve. The
only other driver is **`fake`**, for development and staging: its hosted page is served by the API
itself and charges nothing. **`PAYMENT_GATEWAY=fake` with `APP_ENV=production` refuses to start.**

| Variable | Default | Notes |
| --- | --- | --- |
| `PAYMENT_GATEWAY` | `none` | `none` or `fake`. Anything else fails startup (a real provider adds its name) |
| `PAYMENT_FAKE_WEBHOOK_SECRET` | _(random per process)_ | `fake` only; at least 16 characters. Set it when several API instances run (the page of one may be answered by another). A secret: never logged |
| `PAYMENT_RECONCILE_AFTER` | `15m` | An OPEN checkout older than this is checked with the provider by the reconciliation job (1m..24h) |

**Flow.** `POST /v1/me/subscriptions/{id}/checkout` (the subscriber, or the organisation's owner;
`PENDING_PAYMENT` only) creates a `payments` row (OPEN) and a checkout at the provider, and returns
`{hosted_url}`; it is idempotent per invoice (an open checkout more than a minute from expiry is
handed out again; a concurrent creation is `409 CHECKOUT_IN_PROGRESS`). The provider calls
`POST /v1/payments/webhooks/{provider}`: the driver verifies the signature first (`401
INVALID_WEBHOOK_SIGNATURE`), the event is recorded in `payment_events` (unique per provider and
event id, with a 2-minute processing lease, so a redelivery is a no-op and a crash mid-way is
retried by the next delivery), and a `PAYMENT_SUCCEEDED` whose amount and currency equal the
invoice activates the subscription through **the same path as the administrator's approve**
(`subscriptions.Service.ActivatePaid` and `Approve` share `activate`: history, `SUBSCRIPTION_APPROVED`
audit with `recorded_by: GATEWAY`, the activation email, the package grant), with
`payment_reference` the provider's payment reference and `recorded_by` GATEWAY. A mismatch, or a
payment for a subscription no longer waiting, goes to `REVIEW` (`PAYMENT_FLAGGED`) and activates
nothing. Only `PENDING_PAYMENT` activates, under the subscription's row lock, so concurrent
deliveries activate once. The browser returns to
`{PUBLIC_WEB_URL}/account/subscriptions/{id}/payment?payment_id=&result=`; that page only reads
`GET /v1/me/subscriptions/{id}/payments`. The return never activates anything.

**The fake driver.** `GET /v1/payments/fake/checkout/{ref}` shows the invoice with **Pay**, **Pay
(no webhook)** and **Decline**; the buttons sign a webhook (`Fake-Signature: t=<unix>,v1=<hex
HMAC-SHA256(secret, "t.body")>`, 5 minutes' tolerance) and deliver it to the webhook handler in the
same process, then 303 to the return page. "Pay (no webhook)" exercises reconciliation. The fake's
checkouts live in the API process (a restart forgets them), so with `fake` the API also runs the
reconciliation loop. These routes, and the webhooks, skip the session middleware (no CSRF origin
rule: they never use a session).

**Refunds.** `POST /v1/admin/subscriptions/{id}/refund {amount, reason}` (SUPER_ADMIN, FINANCE_ADMIN;
optional `Idempotency-Key`, else derived from the subscription, amount and reason): full or partial,
never more than paid minus refunded and pending (`422 REFUND_EXCEEDS_PAYMENT`); `409
NO_GATEWAY_PAYMENT` for a subscription paid by transfer (refund those outside, as before, with
`cancel` and `refund_due`). Audited `PAYMENT_REFUND_REQUESTED` and `PAYMENT_REFUNDED`. A provider
refusal records the refund `FAILED` (`502`; the key is freed so it can be tried again). A
`REFUNDED` webhook completes a pending refund or records one made in the provider's dashboard.
**A full refund cancels the subscription at once**: access ends at the refund, `starts_at` and
`ends_at` keep the paid period for the record, nothing is pro-rated or carried over, `refund_due`
is false (the money is back), `cancelled_by` is ADMIN (or SYSTEM for a dashboard refund), the
subscriber is emailed as for any cancellation, and the package it granted ends. **A partial refund**
adds a `REFUNDED` history event and nothing else: the subscription and its package run to
`ends_at`.

**Records (a deliberate choice).** The platform's finance record of online payments is the
`payments`, `payment_refunds` and `payment_events` tables plus the admin audit; **the agent ledger
is not written** (it is the agents' own books, not platform finance). FINANCE_ADMIN reads them in
`GET /v1/admin/payments?status=REVIEW` (the work queue), `GET /v1/admin/subscriptions/{id}/payments`
and exports `GET /v1/admin/reports/payments.csv` (SUPER_ADMIN, FINANCE_ADMIN). Account deletion
keeps the payments, unlinks the person as who opened a checkout or asked for a refund (name
replaced), and expires their open checkouts (`payments.ForgetUserTx`).

**Plans and listing packages.** A plan's optional `package_code` (admin-editable on
`POST/PATCH /v1/admin/subscription-plans`; the seed sets Agency Pro to `PROFESSIONAL`) names a
listing package. When a subscription activates (gateway **or** admin approve) and its market has an
active package of that code, the subscriber's listing account (the person as a direct owner for an
INDIVIDUAL plan, the organisation for an ORGANISATION plan) gets an `INVOICE` assignment
(`subscription_id` set) for the paid period, in the activation's transaction
(`app.LinkSubscriptionPackages`, `packages.GrantInvoiceTx`); it replaces what overlaps from its
start, like an administrator's assignment. An admin cancellation or a full refund ends it
(cancelled when it started today, else ended yesterday; `EndInvoiceTx`); expiry ends it by date.
This resolves the display-versus-enforced mismatch of 0.37.0: a plan with a `package_code` shows
`limit_enforced: true` and the package's `listing_limit` is what listing approval enforces; the
plan's `MAX_ACTIVE_LISTINGS` stays a display feature, so administrators should keep the two equal.
A plan without a code grants nothing (display only, as before).

**Reconciliation** (`app.RunPaymentReconciliation`, every 5 minutes in the worker, and in the API on
in-memory stores or with `fake`; idempotent): OPEN checkouts past `expires_at` become EXPIRED;
those older than `PAYMENT_RECONCILE_AFTER` are checked with `FetchPayment`, a success or failure
being applied as a synthetic event through the webhook path; a SUCCEEDED payment whose subscription
still waits is activated, and a full refund whose subscription is still active cancels it.

**Adding a real provider** (Paynow, Stripe, Flutterwave, DPO ...): one file in `platform/paygateway`
implementing `Gateway`:
`CreateCheckout(ctx, {PaymentID, InvoiceNumber, Description, Amount, Currency, SuccessURL, CancelURL,
IdempotencyKey, BaseURL}) -> {ProviderRef, HostedURL, ExpiresAt}` (pass the idempotency key on;
convert the decimal string to minor units with string arithmetic, never floats);
`VerifyWebhook(ctx, header, body) -> Event{ID, Type PAYMENT_SUCCEEDED|PAYMENT_FAILED|REFUNDED|
CHECKOUT_EXPIRED, ProviderRef, PaymentRef, RefundRef, Amount, Currency, OccurredAt}` (verify the
signature before parsing, constant time, with a timestamp tolerance; `ErrInvalidSignature`; a
provider that only posts a status notification, like Paynow, must confirm with its status API
before reporting a success); `Refund(ctx, {PaymentRef, Amount, Currency, IdempotencyKey, Reason}) ->
{RefundRef, Status PENDING|SUCCEEDED}`; `FetchPayment(ctx, providerRef)`. Then its name in
`config.PaymentGateways()` and `PAYMENT_GATEWAY`, its credentials as `config.Secret` fields read
from the environment (from the deployment's secret store, never this repository), a case in
`paygateway.Open`, and its webhook URL `https://<api>/v1/payments/webhooks/<name>` registered in the
provider's dashboard. Test the signature (valid, invalid, stale), the amount conversion and checkout
idempotency.

**Not verified / follow-ups:** migration `0044` and the Postgres stores have only run where
`RP365_TEST_DATABASE_URL` is set (`TestFlowPostgres` in `modules/payments`, including concurrent
duplicate webhooks, and `TestInvoiceGrantPostgres` in `modules/packages`). No receipts PDF, no saved
cards, nothing recurring; renewal is still a new invoice.

## Uploads and malware scanning (API 0.41.0)

BR-014, FR-009/010. Migration `0043_malware_scanning`; `platform/malware` (the adapter, the
storage guard, the worker job, the admin view) and `platform/storage/scan.go` (what the modules
call). The exact JSON is the 0.41 malware scanning contract.

**Scanning is OFF unless configured.** With `MALWARE_SCANNER` unset (`none`) nothing is scanned
and every upload behaves exactly as before 0.41.0. **A ClamAV host is yours to provide**: run
`clamd` (for example the official `clamav/clamav` image, with `freshclam` keeping its signatures
current) somewhere the API and the worker can reach over TCP, then set `MALWARE_SCANNER=clamav`
and `CLAMD_ADDRESS=host:3310` on **both** the API and the worker. Nothing in this repository runs
or updates ClamAV, and a clamd port must never be reachable from the internet.

| Variable | Default | Notes |
| --- | --- | --- |
| `MALWARE_SCANNER` | `none` | `none` or `clamav`. Startup fails on anything else |
| `CLAMD_ADDRESS` | _(empty)_ | `host:port` of clamd (TCP, usually 3310). Required with `clamav` |
| `MALWARE_SCAN_TIMEOUT` | `60s` | One scan: connect, stream, verdict (1s..10m) |
| `MALWARE_SCAN_MAX_BYTES` | `26214400` | **Must equal clamd's `StreamMaxLength`** (25 MB is clamd's default). At least 20 MB, the largest upload the API accepts |

At startup the API and the worker log one line naming the scanner and ping clamd; an unreachable
clamd is a warning, never a startup failure (uploads then fail closed, below).

### Every upload path

Storage is Amazon S3 (one private bucket; the `local` disk in development). **Every upload goes
through the API, as `multipart/form-data` (base64 JSON for signature images); there are no
presigned uploads**, so the bytes are always in the API's hands before anything is stored.

| Path | What | Stored as | Scanned |
| --- | --- | --- | --- |
| `POST /v1/me/listings/{id}/photos` | Listing photo | Re-encoded JPEG (three sizes) | **Sync**, original bytes, before storing |
| `POST .../maintenance/{requestId}/photos` (agent and tenant) | Maintenance photo | Re-encoded JPEG | **Sync**, original bytes |
| `POST .../inspections/{id}/items/{itemId}/photos` | Inspection photo | Re-encoded JPEG | **Sync**, original bytes |
| `PUT /v1/me/profiles/{kind}/{slug}/logo` | Profile logo | A scaled copy, or **the original byte for byte** when small enough | **Sync**, original bytes (again when an onboarding LOGO is promoted on approval) |
| `PUT /v1/me/signature` | Saved signature or initials (DRAWN, UPLOADED) | Re-encoded PNG | **Sync**, original bytes |
| `POST /v1/signing/{token}/submit` | Adopted signature or initials (DRAWN, UPLOADED) | Re-encoded PNG | **Sync**, original bytes |
| `POST /v1/me/onboarding/{role}/documents` | Onboarding document (PDF, JPEG, PNG) | As uploaded | **Async**: quarantined, scanned by the worker |
| `POST /v1/me/applications/{id}/files` | Application file | As uploaded | **Async** |
| `POST .../ledger/entries/{id}/attachments` | Ledger attachment (PDF, JPEG, PNG, WebP) | As uploaded | **Async** |
| `PUT /v1/me/agreements/{id}/document` | Uploaded agreement PDF (20 MB) | As uploaded | **Async**; cannot be opened or **sent for signature** until CLEAN |
| `POST /v1/agent/organisations/{id}/contacts/import` | CRM contacts CSV | **Not stored**: parsed into text fields | Not scanned |
| `POST /v1/verify` | A PDF to check against a completed agreement | **Not stored**: hashed and compared | Not scanned |
| _(generated)_ | Lease PDFs, signed agreement PDFs, stamps | Rendered by the platform | Not scanned (not user bytes; an uploaded original is CLEAN before it can be sent) |

**Images: synchronous.** The image is validated as before, then its **original bytes** are
scanned, then the re-encoded copy is stored. (Re-encoding neutralises most image payloads, but a
small logo is kept byte for byte and bytes after an image's end marker survive decoding, so the
original is what is scanned.) Infected: `422 FILE_INFECTED`, nothing stored, an audit log line
(`event=file_scan.infected`, the signature, the actor). **Scanner unavailable: fail closed,
`503 SCANNER_UNAVAILABLE` with `Retry-After: 60`, nothing stored**: an unscanned image is never
accepted, and an image upload is small and simply repeated. Larger than the scanner accepts:
`413 FILE_TOO_LARGE_TO_SCAN`. Images get no scan record (only clean ones are ever stored).

**Documents: asynchronous and quarantined.** The file is stored under its usual random key and a
scan record (`file_scans`) says `PENDING`. **Every read refuses it** while PENDING (`409
FILE_SCAN_PENDING`) and once INFECTED (`409 FILE_INFECTED`): the guard sits in front of the
storage itself, so the owner's and the reviewers' downloads, an agent's application files, the
signing pages, the promotion of an onboarding logo and an agreement's send all see it. The upload
answers as before with `scan_status: "PENDING"`; views carry `scan_status` (`PENDING`, `CLEAN`,
`INFECTED`, `LEGACY_UNSCANNED`; absent when scanning is off). The **worker** (`cmd/worker`; the API
process itself without a database) takes due records every 5 seconds (batches of 10, a 5-minute
lease, `FOR UPDATE SKIP LOCKED`):

- **CLEAN**: served from then on.
- **INFECTED**: recorded with the signature name, then the **object is deleted** from storage (not
  moved: nothing infected is kept; the record stays for the administrators, even when the document
  is deleted later). An audit log line, and the uploader gets an in-app **SECURITY** notice
  (essential, cannot be switched off) in safe text: no file name, no signature name.
- **Scanner (or storage) unavailable: fail closed.** The file stays PENDING and is retried with
  backoff (30 s, 1, 2, 4 minutes and so on, capped at 1 hour) and is **never given up**;
  `last_error` says why. With `MALWARE_SCANNER=none` the worker does not scan, and **switching
  scanning off does not release files already quarantined**: they stay PENDING until it is back on.
- A file deleted while PENDING drops its record.

**Legacy files.** The migration adds a `LEGACY_UNSCANNED` record for every onboarding document,
application file, ledger attachment and uploaded agreement PDF that existed before it. They are
**served as before (treated as clean) and are not scanned on read**; the admin view counts them.
Scanning them is a separate, deliberate job (not built).

**Metrics and logs.** Every scan is logged (`file scanned`, `file scan failed`) with the mode
(`sync`, `async`), the kind of file, the result, the size and the duration, **never the content
or the user's file name**. Counts per mode and result are kept per process (`process_counts` on the
admin view: the API's synchronous scans; the worker's are in its logs); the database counts by
status.

**Admin.** `GET /v1/admin/file-scans` (SUPER_ADMIN, PLATFORM_ADMIN): the scanner, counts by
status, the oldest PENDING time, the latest 50 PENDING and INFECTED records (kind, status,
signature, size, attempts, last error, times; never a file name, an owner or a link) and
`process_counts`.

**Account deletion** unlinks the person from their scan records (`malware.ForgetUploaderTx`; the
users row is anonymised, so the foreign key's `ON DELETE SET NULL` never fires).

**Not verified:** the clamd protocol is tested against a fake TCP server (clean, FOUND, ERROR, the
size limit, no reply, unreachable) and the flows with a fake scanner that finds the EICAR test
string; it has **not been run against a real clamd**. `malware/postgres.go` and the migration's
backfill have only run where `RP365_TEST_DATABASE_URL` is set.

## Administrator MFA (API 0.40.0)

Migration `0042_admin_mfa`, in the identity module (`identity/mfa*.go`; FR-001), with `platform/totp` (RFC 6238), `platform/secretbox` (AES-256-GCM), `app/mfa.go` (the key) and `devseed/admin_mfa.go`. OpenAPI tag `admin`. The exact JSON is the 0.40 admin MFA contract.

- **Method**: an authenticator app (TOTP: HMAC-SHA1, 6 digits, 30-second step, ±1 step accepted) plus ten one-time **recovery codes** (`xxxxx-xxxxx`, case, spaces and dashes ignored). **WebAuthn / hardware keys are not implemented** (no lightweight dependency); the status always says `webauthn_available: false`. No QR image is returned: the web draws it from `otpauth_uri` (`otpauth://totp/RightProperty365:<email>?secret=...&issuer=RightProperty365&algorithm=SHA1&digits=6&period=30`).
- **Sign-in** is a third step on the same challenge, after the password and the emailed code: for an administrator with a confirmed authenticator, `POST /v1/admin/auth/login/verify` answers `200 {"status":"MFA_REQUIRED","challenge_id","methods":["totp","recovery_code"],"expires_at","code_length":6}` and **no cookie**; `POST /v1/admin/auth/login/mfa {challenge_id, code | recovery_code}` starts the session. `AUTH_DEMO_CODE` answers only the emailed-code step, never the authenticator. Once the emailed code was accepted, `verify` and `resend` refuse the challenge (`CODE_EXPIRED`). Customers are unchanged.
- **Required MFA** (`ADMIN_MFA_REQUIRED`, default true in production only): an administrator without an authenticator signs in to an **enrolment-only** session (`session.mfa_enrolment_required: true`). It reaches `/v1/admin/auth/*` only (the sign-in steps, `GET /v1/admin/auth/mfa`, setup and confirm); every other `/v1/admin` route answers `403 MFA_ENROLMENT_REQUIRED`, `GET /v1/admin/me` included, and platform-role checks elsewhere fail. Sign out with `POST /v1/auth/logout`. The restriction is judged on every request, so confirming lifts it on the same session, and switching the setting on restricts existing sessions of administrators without one.
- **Enrolment**: `POST .../mfa/totp/setup` stores a **pending** secret (replaced by a new setup, confirmable for 15 minutes; `409 MFA_ALREADY_ENROLLED` once confirmed) and answers the secret and URI with `Cache-Control: no-store`; `POST .../totp/confirm {code}` activates it and returns the ten recovery codes **once**. `GET /v1/admin/auth/mfa` is the status. `POST .../recovery-codes/regenerate {code}` and `DELETE .../totp {code}` need a current code; removal is refused while MFA is required (`403 MFA_REMOVAL_NOT_ALLOWED`).
- **Storage**: the secret is sealed with AES-256-GCM under `MFA_ENCRYPTION_KEY`, bound to the user ID (a sealed value copied to another account does not open); recovery codes are Argon2id hashes (the password hasher). Changing the key makes every authenticator unusable (`503 MFA_UNAVAILABLE` at sign-in): reset them.
- **Replay protection**: the newest accepted step is stored per administrator; a code of that step or an earlier one is refused ("already used, wait for the next code"), under concurrency too (conditional UPDATE).
- **Lockout**: each challenge allows 5 wrong second-factor answers (counted apart from the 5 wrong emailed codes), then it is dead (`CODE_EXPIRED`, sign in again); the 20-a-minute per-IP bucket of the code step covers `/login/mfa` too. In-session code checks (regenerate, remove) share the administrators' 30-a-minute bucket.
- **Reset**: `POST /v1/admin/users/{id}/mfa/reset {reason}` (SUPER_ADMIN only, never their own account: they use `DELETE`) deletes the authenticator and codes, **revokes every session of the target**, writes `ADMIN_MFA_RESET` with the reason and emails the target. `GET /v1/admin/users/{id}/mfa` shows the status.
- **Audit**: durable `admin_audit_events` (target USER): `ADMIN_MFA_ENROLLED`, `ADMIN_MFA_RECOVERY_CODE_USED`, `ADMIN_MFA_RECOVERY_CODES_REGENERATED`, `ADMIN_MFA_REMOVED`, `ADMIN_MFA_RESET` and `ADMIN_MFA_CHALLENGE_LOCKED` (failed codes aggregated: one record when a challenge reaches 5). Each failed code, the setup and the outcome of every step are log audit lines (`admin_mfa`, `admin_mfa_sign_in`, `admin_sign_in` with `mfa_method`). **No secret, code, recovery code or email address is ever logged** (a test captures the logs).
- **Emails** (outbox, worker): `admin-mfa-recovery-code-used` when a recovery code signs someone in, `admin-mfa-reset` after a reset; in-app security notices too.
- **Account deletion** deletes the authenticator and recovery codes explicitly (`identity.DeleteAdminMFATx`; the users row is anonymised, so the cascade never fires).
- **Sample data**: `admin.mfa@example.test` (PLATFORM_ADMIN) is enrolled with a **fixed, public, development-only** secret and ten recovery codes (docs/sample-accounts.md); the other sample administrators are not. `cmd/seed` seals it with `MFA_ENCRYPTION_KEY` (or the development key in development) and skips it without a key elsewhere; the seed never runs in production.

**Not verified:** `mfa_postgres.go` has only run where `RP365_TEST_DATABASE_URL` is set; not tried with real authenticator apps (the RFC 6238 vectors pass).

## Calendar invitations (API 0.39.0)

FR-021. No migration: nothing new is stored. The exact rules are the 0.39 calendar invitations contract.

- **Generator**: `internal/platform/ics` writes one VCALENDAR with one VEVENT (RFC 5545): CRLF line endings, lines folded at 75 octets without splitting a UTF-8 character, TEXT values escaped (`\\`, `\;`, `\,`, newline as `\n`). Every time is UTC with `Z` (no `VTIMEZONE`); the market's zone is in `X-WR-TIMEZONE` and the description's `Time:` line. Golden files: `internal/platform/ics/testdata/{request,update,cancel}.ics` (kept byte-exact by `*.ics -text` in `.gitattributes`; rewrite with `go test ./internal/platform/ics -update`).
- **One entry per appointment / booking**: UID `appointment-<id>@rightproperty365.com` / `venue-booking-<id>@rightproperty365.com`. **SEQUENCE is derived**, not stored: the number of entries in the append-only history (`appointment_timeline`, `venue_booking_events`) at or before the moment described, minus one, so it never goes down. `appointments/calendar.go`, `venues/calendar.go`.
- **Emails** (`notifications.Handlers.SetCalendar`, wired in the worker and the database-less API): `METHOD:REQUEST` on `appointment-confirmed` (customer), `venue-booking-confirmed` (customer) and `venue-booking-accepted` (host); `METHOD:CANCEL` on `appointment-cancelled`, `appointment-cancelled-by-customer`, `venue-booking-cancelled` and `venue-booking-cancelled-by-customer`, only when the appointment or booking had been confirmed. Proposals of a new time carry nothing (the confirmed time stands until accepted). The attachment rides on the existing email only, so the notification preferences apply unchanged.
- **Privacy**: ORGANIZER is the platform sender (`EMAIL_FROM`), ATTENDEE only the email's own recipient (`RSVP=FALSE`); no file holds the other party's address or phone, or the customer's name towards the professional. LOCATION is the appointment's location note as the confirmation email shows it (contact details removed); venue entries have none.
- **Outbox**: payloads stay identifiers-only. The worker rebuilds the file at send time from the event (DTSTAMP is the event's `occurred_at`), so a retry produces the same bytes and no blob is ever stored.
- **Mail platform**: `mail.Message.Attachments` (`Filename`, `ContentType`, `Content`); Resend gets `attachments: [{filename, content (base64), content_type}]`; the log driver logs name, type and size only.
- **Downloads** (`METHOD:PUBLISH`, `text/calendar; charset=utf-8`, `Content-Disposition: attachment; filename="..."`, `Cache-Control: private, no-store`): `GET /v1/me/appointments/{id}/calendar.ics`, `GET /v1/inbox/appointments/{id}/calendar.ics` (404, not 403, without a professional scope), `GET /v1/me/venue-bookings/{bookingId}/calendar.ics`, `GET /v1/me/hosted-venue-bookings/{bookingId}/calendar.ics` and `GET /v1/agent/organisations/{id}/venue-bookings/{bookingId}/calendar.ics`; 404 for anybody else. `Content-Disposition` is in the CORS expose list.

**Not verified / follow-ups:** not checked in real calendar apps (Gmail, Outlook, Apple Calendar); no Resend send with an attachment was made. The existing venue booking emails still link customers to `/account/hourly-bookings`, while the web page is `/account/venue-bookings` (the calendar entries use the latter).

## Lead funnel and response-time KPIs (API 0.38.0)

No migration (the existing indexes `property_requests_recipient_org_idx`, `property_requests_recipient_user_idx` and `request_timeline_request_idx` serve it). In `internal/modules/propertyrequests`: `funnel.go` (the rules and the counting), `funnel_handler.go`, `memory_funnel.go`, `postgres_funnel.go`; sample data in `devseed/lead_funnel.go` (BR-005). Routes: `GET /v1/agent/organisations/{id}/reports/lead-funnel` and the direct seller's `GET /v1/me/reports/lead-funnel` (OpenAPI tags `reports`, `property-requests`; every answer `Cache-Control: private, no-store`). The exact JSON is the 0.38 lead funnel contract.

- **Who sees what**: the inbox's own `InboxScope`, limited to the organisation. OWNER and MANAGER: every request it received (`scope: ORGANISATION`). AGENT and STAFF: their own requests **and the unassigned organisation inbox**, exactly what their inbox lists (`MEMBER`); `agent_user_id` only narrows. Not a member of `{id}` (or not a UUID): `404 ORGANISATION_NOT_FOUND`. `/v1/me/...`: the requests routed to the caller as a direct seller (`DIRECT_SELLER`), for whoever may open the inbox (`403` otherwise). Routing exceptions are never counted.
- **Cohort**: requests **created** from `from` 00:00 UTC to the end of `to` (UTC dates, inclusive; default the last 30 days to today, at most 366 days; `400 VALIDATION_FAILED` otherwise). Filters: `agent_user_id` (the current assignee), `listing_id`, `request_type`, `market_code` (an unknown code is `400`). The cohort is described as it is now, moves after `to` included.
- **Reached** (the rule): ranks NEW_ENQUIRY 0, ASSIGNED 1, CONTACTED 2, VIEWING_REQUESTED 3, VIEWING_SCHEDULED 4, CONVERTED 5; FOLLOW_UP_REQUIRED and CLOSED have none. A request reached a step when the highest rank of its current stage and of every `STAGE_CHANGED` to-stage is at least the step's: **skipped stages count** (CONTACTED straight to VIEWING_SCHEDULED reached VIEWING_REQUESTED, anything converted reached every step) and **moving back undoes nothing**. ASSIGNED is also reached with an `ASSIGNED` timeline row or when the request is routed to a person now (recipient AGENT or SELLER). Conversion from the previous step and from received are 4-place decimal strings (half up), `null` at a zero denominator; `current` is the number at that stage now.
- **Side states**: closed without converting, converted then closed, follow-up required (now and ever), open (not CONVERTED or CLOSED) and closed.
- **A response** is the first `STAGE_CHANGED` by the receiving side (actor `AGENT`) to CONTACTED or any later ranked stage. Assigning, FOLLOW_UP_REQUIRED, CLOSED, internal notes (the customer never sees them) and appointments (the one created with a request is the customer's; later appointment changes are in another module) are not responses. KPIs: median and p90 (nearest rank) in minutes over the responded requests, the counts and shares (of **all** received) answered within 60 and 1440 minutes, the never answered; time to conversion as the median days to the first move to CONVERTED.
- **Breakdowns** with received, contacted, converted, conversion rate and median first response: by **current** assignee (`user_id: null` is the organisation inbox), by request type (all five), by listing (top 20 by received). `?format=csv` returns them stacked (`table,key,label,...`), formula-like cells prefixed with `'` as in the CRM export.
- **One computation**: both stores only return the cohort's facts (`FunnelFacts`: the request, its current routing, its `STAGE_CHANGED` moves and whether it was ever assigned; Postgres reads them with the inbox's WHERE clause and one LEFT JOIN on the timeline) and `ComputeFunnel` counts. `TestPostgresFunnelMatchesMemory` runs the same fixture through both and requires equal reports.
- **Sample data** (`devseed/lead_funnel.go`, memory and Postgres): 24 received requests with stage histories over the four weeks before the seed runs, 21 for Sample Estates (Tendai Ncube's listings 1, 2, 5; Chipo Dube's 7, 10, 17; the rest through the organisation inbox, assigned by Rudo Moyo) and 3 for Grace Mhlanga's listing 6. Sample Estates: 4 converted (one later closed), 6 closed without converting, 2 at follow-up, 7 never answered, first responses from 15 minutes to two days. Re-runnable: the fixed-ID samples (`LeadFunnelRequestID`) are deleted and written again with fresh dates (one an appointment points at is kept). The existing tests' counts are unchanged: the app tests do not load this seed.

**Not verified:** `postgres_funnel.go` (the cohort query and the seed) has only run where `RP365_TEST_DATABASE_URL` is set. The report counts the cohort live on every call (no rollup); fine for an organisation's year of requests, not tuned beyond that.

## Marketing preferences (API 0.37.1)

Migration `0041_marketing_consent`, in the identity module (`identity/marketing*.go`; BR-017, FR-003), with `devseed/marketing.go`. Routes: `GET` and `PUT /v1/me/marketing-preferences` (signed in, `401` otherwise; every answer `Cache-Control: private, no-store`; OpenAPI tag `marketing`). The exact JSON is the 0.37 marketing preferences contract.

- **State**: `users.marketing_opt_in` is the current value (false for an account that never chose). `marketing_consent_events` is the append-only record of every change: `opt_in`, `changed_at` (UTC), `source` (`web | app | api`, from `X-Client` through `httpx.SourceFromClient`, shared with notification preferences), `context` (`REGISTRATION | PREFERENCES | MIGRATION`; `ADMIN` and `UNSUBSCRIBE` are reserved) and `consent_text_version` (opt-in only). No IP address or user agent. The table refuses UPDATE (trigger `marketing_consent_events_no_update`); rows are deleted only by account deletion.
- **GET** answers the value, the newest record's `changed_at`, `source`, `context` and `consent_text_version` (all `null` without a record), `current_consent_text_version` and `history` (newest first, at most 20).
- **PUT** `{marketing_opt_in, consent_text_version?}`: a missing, null or non-boolean value is `400 VALIDATION_FAILED` (`marketing_opt_in`). On opt-in the version defaults to the current one; any other is `400` on `consent_text_version`. On opt-out it is ignored. **Only a real change is recorded**: the store locks the users row, and the value the account already has writes nothing (the answer keeps its `changed_at`). A change updates the user and appends the record in one transaction. Writes share the `/v1/me` settings bucket with notification preferences (60 a minute per caller IP, `429 RATE_LIMITED`).
- **Consent wording** lives in `marketing_consent_texts` (`version`, `text`, `status` DRAFT / APPROVED, `created_at`), and GET and PUT return the current one as `consent_text` so no client hard-codes it. `GET /v1/marketing-consent-texts/{version}` (public, cacheable) serves any version. The current version is `identity.CurrentMarketingConsentVersion` (`marketing-2026-01`); an opt-in naming any other is refused. Rows are immutable (the trigger `marketing_consent_texts_immutable` refuses UPDATE and DELETE): a change of wording is always a new version, inserted by a migration, with the constant then pointed at it. The migration inserts `marketing-2026-01` (production too). The memory store holds the same row (`builtInMarketingConsentTexts`, which must match the migration).
- **DRAFT notice:** `marketing-2026-01` is **DRAFT**. It awaits the owner's legal wording. Approving it means inserting a **new version** with status `APPROVED` and the approved text, never editing the draft.
- **Registration**: `POST /v1/auth/register` with `marketing_opt_in: true` writes a `REGISTRATION` record (source from `X-Client`, the current version); `false` writes none. Re-registering a still-unverified account records a `REGISTRATION` record only when it changes the choice (opt-out records carry no version).
- **Migration backfill**: every account already opted in gets one `MIGRATION` record dated at its `created_at`, source `api`, no version.
- **Honouring it: `identity.Service.MarketingAllowed(ctx, userID)`** is true only when the account is ACTIVE, its email verified and `marketing_opt_in` true (false for an unknown account or a read failure). **Every future marketing send (newsletter, promotions) MUST call it for the recipient at send time, not at queue time.** No send is marketing today: service and account mail is unaffected and follows notification preferences. A notification category `MARKETING`, once one exists, is never forced on.
- **Account deletion** (`identity.DeleteMarketingConsentTx` / `MemoryStore.DeleteMarketingConsents`): the records are deleted explicitly (users rows are anonymised, so the cascade never fires) and `marketing_opt_in` becomes false, as before.
- **Sample data** (`devseed/marketing.go`): `consumer@example.test` opted in at registration (web, `marketing-2026-01`); `tenant@example.test` opted in at registration (web), then out from the app (two records). Every other sample account never chose. Re-runnable: the sample accounts' records are deleted and the fixed-id samples inserted again, and each sample account's value is reset.

**Not verified / follow-ups:** migration `0041` and `identity/marketing_postgres.go` have only run where `RP365_TEST_DATABASE_URL` is set (`TestPostgresMarketingStoreContract` in the identity module). There is no per-user audit trail of account settings; the consent records are the trail. The web preferences screen (the rest of item D) is still to come.

## Listing packages and quotas (API 0.37.0)

Migration `0040_listing_packages`, module `internal/modules/packages` (FR-008, BR/FR-007), with `listings/quota.go` (the go-live check and the view hint), `app/packages.go` (memory wiring) and `devseed/packages.go`. Routes: `/v1/markets/{code}/packages` (public), `/v1/admin/packages[/{packageId}]`, `/v1/admin/organisations/{id}/package` (+ `/end`), `/v1/admin/users/{id}/package` (+ `/end`), `/v1/agent/organisations/{id}/package` and `/v1/me/package` (OpenAPI tag `packages`). The exact JSON is the 0.37 contract's "Response shapes".

- **Packages** belong to one market: a code unique in it, a name, a description, `listing_limit` (active published listings allowed), active, sort, and a price per billing term (MONTHLY, QUARTERLY, ANNUAL) in the market's currency as an exact decimal string. SUPER_ADMIN and PLATFORM_ADMIN manage every market, MARKET_ADMIN its own (`403 PACKAGE_MARKET_FORBIDDEN`; another market's package is `404 PACKAGE_NOT_FOUND`). Every change is in the admin audit (`PACKAGE_CREATED`, `PACKAGE_UPDATED`, `PACKAGE_DELETED`, target `LISTING_PACKAGE`). A package named by any assignment, or the default, cannot be deleted (deactivate it).
- **Which package applies** (`packages.Resolve`): the account's current assignment in the market, else the market's **default** package (`is_default`, one per market, always active), else none: **a market without a default is not limited**. The listing account is the listing's organisation, else its direct owner (`packages.AccountOf`).
- **Assignment** (`POST .../package {package_id, starts_on?, ends_on?, note}`; audited `PACKAGE_ASSIGNED` / `PACKAGE_ASSIGNMENT_ENDED`, target `ORGANISATION` or `USER`): from-to UTC dates (inclusive), source ADMIN (`INVOICE` is reserved for invoices, item G). It replaces whatever overlaps from `starts_on` on: later assignments are cancelled, the one in force then ends the day before (or is cancelled if it starts that day). `.../package/end` ends the current one and cancels the scheduled ones.
- **Counted**: PUBLISHED listings whose publication period is not over, of the account, in the market. PAUSED (no action reaches it today), pending, archived and expired listings do not count.
- **Enforcement**: the only go-live is a reviewer's approval of a NEW review (a first publication, or a relist of a listing published before; relist itself only moves the listing to PENDING_REVIEW). `listings.Manager.Decide` sets `Change.Quota`; `PostgresStore.SaveListing` calls `packages.EnforceTx` in the same transaction as the status change, which takes `pg_advisory_xact_lock` on (account, market), resolves the package and counts the other active listings: `409 LISTING_QUOTA_REACHED` with `details {limit, used, package, market_code, account}` when they reach the limit. Assignment changes take the same lock. The memory store does the same under its mutex. Approving an EDIT is never refused; submit and relist are never blocked.
- **Grace rule**: a lower limit (a downgrade, a package change, an assignment ending) unpublishes nothing; listings already live stay live until they expire or are archived; only new go-lives are refused while `used >= limit`. The usage view shows `over_limit`, `over_by` and `grace_until` (when the account is back within its limit if nothing changes: the `over_by`-th earliest expiry).
- **Usage**: `GET /v1/agent/organisations/{id}/package` (any member of a listings-managing organisation, STAFF included, or the direct owner's own user id; anybody else `404 PACKAGE_ACCOUNT_NOT_FOUND`) and `GET /v1/me/package` (approved direct owners; others `403`). Every listing management view (`/v1/me/listings/{id}`, submit, relist ...) and the admin review screen (`/v1/admin/listings/{id}`) carry `package_usage` and `quota_warning` (set at the limit for a listing that is not live).
- **Account deletion** (`packages.ForgetUserTx` / `MemoryStore.ForgetUser`): the person's own current and scheduled assignments end or are cancelled; they are unlinked as assigner, canceller and package creator.
- **Sample data** (`devseed/packages.go`, development seed only): Starter (20, the default), Business (40) and Professional (60) in ZW (USD) and GB (GBP), every price marked `sample`; Sample Estates holds Business in ZW. Other markets have no packages (not limited) until an administrator creates them; the migration seeds nothing, so production starts unlimited until packages and a default are configured. Re-runnable: packages are upserted by market and code, an administrator's (non-sample) price is kept, and the sample assignment is added only when Sample Estates holds nothing else in ZW.

**Not verified / follow-ups:** migration `0040` and `packages/postgres.go` have only run where `RP365_TEST_DATABASE_URL` is set (`TestPostgresStoreContract` and `TestConcurrentApprovalsNeverExceedTheLimit` in the module). Since 0.42.0 a plan's `package_code` makes a paid subscription create an `INVOICE` assignment (see "Online payment of subscription invoices"); the plan's `MAX_ACTIVE_LISTINGS` stays display-only. The review queue rows carry no usage (only the listing detail does), and the dashboards gain no usage hint.

## Reviews of service providers (API 0.36.4)

Migration `0039_provider_reviews`, module `internal/modules/reviews` (BR-019, FR-047; Phase 2 part e), with `app/reviews.go` (wiring: books, directory profiles, maintenance requests and enquiries), `notifications/reviews.go` and `devseed/reviews.go`. Routes: `/v1/me/reviews` (+ `/eligibility`, `/{reviewId}`), `/v1/agent/organisations/{id}/reviews` (+ `/eligibility`, `/received`, `/{reviewId}`, `/{reviewId}/reply`), `/v1/reviews/{id}/report`, `/v1/directory/{kind}/{slug}/reviews`, `/v1/admin/reviews` (+ `/{reviewId}`, `/publish`, `/hide`, `/reject`, `/remove-reply`) and `POST /v1/inbox/business-enquiries/{enquiryId}/replied` (OpenAPI tag `reviews`). The exact JSON is the 0.36 contract's "Response shapes (e)".

- **Who may review** (`reviews.maintenanceReason` / `enquiryReason`, table-tested): only public `service-providers` profiles, never one's own organisation's. A **book** (the tenancies' books: STAFF read, OWNER / MANAGER / AGENT write, anybody else `404 BOOK_NOT_FOUND`) reviews on a **MAINTENANCE** basis: a RESOLVED or CLOSED request of its book whose current assignment is the provider. A **customer** (`/v1/me/reviews`) reviews on an **ENQUIRY** basis: an enquiry they sent to the provider, at least 14 days old, marked replied by the provider (enquiries sent by a maintenance assignment are the book's job, not a customer basis). Each refusal is `409 REVIEW_NOT_ELIGIBLE` with `details.reason` (`BASIS_NOT_FOUND` for another book's or person's basis, `WRONG_PROVIDER`, `NOT_COMPLETED`, `TOO_RECENT` with `eligible_from`, `NOT_REPLIED`). One review per basis (`provider_reviews_one_per_basis`; `409 REVIEW_EXISTS`). `GET .../eligibility?kind=&slug=` (or `directory_profile_id`) lists the usable bases, the not-yet ones and the reviewed ones, so the web shows the form only where it is allowed.
- **Replied**: business enquiries had no reply record; `POST /v1/inbox/business-enquiries/{id}/replied` (whoever reads it in their inbox) sets `replied_at` once (the inbox shows it). The reply itself still happens by email, phone or WhatsApp.
- **Lifecycle** (`reviews.Allowed`, table-tested): PENDING (rating 1-5, text 20-2000 characters after trimming; the reviewer edits it until moderated, `409 REVIEW_LOCKED` after, and may delete it at any time) → PUBLISHED or REJECTED; PUBLISHED ↔ HIDDEN; publishing a published review with open reports keeps it and closes the reports. Every action needs a reason (optional for publish), closes the open reports, is audited in the **admin audit** (`REVIEW_PUBLISHED`, `REVIEW_HIDDEN`, `REVIEW_REJECTED`, `REVIEW_REPLY_REMOVED`, target `PROVIDER_REVIEW`) and is guarded by the row's `version` (`409 REVIEW_CHANGED`).
- **Provider**: any member of the provider organisation reads `/reviews/received` (published only); OWNER and MANAGER reply once (published directly, reportable), editable for 7 days (`409 REVIEW_REPLY_LOCKED` with `editable_until`); a moderator may remove a reply, after which the provider cannot reply again.
- **Reports**: anybody signed in, published reviews only, one OPEN report per person and review (`409 REVIEW_ALREADY_REPORTED`); the review stays public until a moderator acts.
- **Public**: every directory profile gains `rating: {average, count}` (published reviews only; `average` one decimal, rounded half up, null when none); `GET /v1/directory/{kind}/{slug}/reviews` (cacheable, 60 s) lists the published reviews with replies. The reviewer is shown as first name and last initial (`reviews.PublicName`), the organisation's name, or "Former customer"; the platform always stores the reviewer's user ID (and organisation).
- **Notifications** (`PROVIDER_REVIEW_MODERATED`, ids and statuses only): the reviewer is told the outcome (`review-moderated`), the provider's OWNERs and MANAGERs a first publication (`review-published`); in-app type `REVIEW`. Safe text only: the provider's name and the outcome.
- **Account deletion** (`reviews.ForgetUserTx` / `MemoryStore.ForgetUser`): reviews stay; a customer's or direct owner's shows "Former customer" and loses its user link; an organisation's keeps the organisation's name and loses its author; reporters, repliers, moderators and enquiry repliers are unlinked.
- **Sample data** (`devseed/reviews.go`): RPM-2026-900005 (Avondale Court, "Blocked courtyard drain", assigned to Sample Plumbers, CLOSED) reviewed by Sample Estates (5 stars, PUBLISHED, with the provider's reply); Casey Consumer's enquiry review (4 stars, PUBLISHED, reported by the sample tenant); Grace Mhlanga's (3 stars, PENDING); the tenant has a replied enquiry 20 days old to review. Sample Plumbers shows 4.5 from 2 reviews. Re-runnable: the job and the reviews are replaced, the enquiries are inserted once.

**Not verified / follow-ups:** migration `0039` and `reviews/postgres.go` have only run where `RP365_TEST_DATABASE_URL` is set (`TestPostgresStoreContract` in the module). Removing a reply does not notify the provider.

## Inspections (API 0.36.3)

Migration `0038_inspections`, module `internal/modules/inspections` (BR-019, FR-047; Phase 2 part d), with `devseed/inspection_templates.go` (the default checklists) and `app/inspections.go` (wiring). Routes: `/v1/agent/organisations/{id}/inspections` (+ `/options`, `/{inspectionId}`, `/checklist`, `/findings`, `/start`, `/complete`, `/cancel`, `/report.pdf`, `/items/{itemId}/photos`, `/photos/{photoId}`) and the tenant's `/v1/me/tenancies/{id}/inspections` (+ `/{inspectionId}`, `/report.pdf`, `/photos/{photoId}`) (OpenAPI tag `inspections`). The exact JSON is the 0.36 contract's "Response shapes (d)".

- **Books and rights** are the tenancies': every agent route answers `404 BOOK_NOT_FOUND` to anybody outside the book, before any validation (tested on every endpoint); STAFF read; OWNER, MANAGER, AGENT write. A tenant sees only the COMPLETED inspections of their own tenancy (another tenancy `404 TENANCY_NOT_FOUND`, any other inspection `404 INSPECTION_NOT_FOUND`) and has no write route.
- **Checklist templates**: the admin-editable reference set `inspection_checklist_templates`, a tree in one set by `parent_code` (template, area, item; `reference/admin.go` lets this set point into itself and its templates have no parent). Seeded `residential` "Standard residential" (8 areas, 32 items) and `commercial` "Short commercial" (4 areas, 13 items). On create the template is **copied** into the inspection's `checklist` (JSONB, its own ids), so template edits never change existing inspections (tested); `PUT .../checklist` edits the copy while open.
- **Findings**: `PUT .../findings` is partial (absent keeps, null clears). Photos per item (`maintenance.ProcessPhoto`: sniffed JPEG / PNG / WebP, 10 MB, re-encoded without metadata) in the private storage (`inspections/<random>`), 6 per item, 200 per inspection, served only by the API.
- **Status** (`inspections.Transitions`, table-tested): SCHEDULED → IN_PROGRESS (start, or the first findings / photo) → COMPLETED (every item needs a status: `409 INSPECTION_INCOMPLETE`), or CANCELLED (a reason). COMPLETED and CANCELLED are **frozen**: every write is `409 INSPECTION_CLOSED` (tested on every write route), and the trigger `inspections_freeze` refuses any change but unlinking people. Writes are guarded by the row's `version` (`409 INSPECTION_CHANGED`). Audited `INSPECTION_COMPLETED` / `INSPECTION_CANCELLED`.
- **Report**: `GET .../report.pdf` (gofpdf and the Go fonts, as the leases): header, property and unit, tenants, type, dates, inspector, a table per area, embedded photo thumbnails (at most 60), summary, sign-off names, footer with the reference. A MOVE_OUT is compared with the tenancy's latest completed MOVE_IN (per item by area and item labels), in the JSON and the PDF.
- **Notifications**: scheduling (and rescheduling) and completing an inspection with a tenancy write an outbox event (`INSPECTION_SCHEDULED`, `INSPECTION_COMPLETED`; ids, reference, type and status only). `notifications/inspections.go` tells the tenancy's tenants with a verified account (not while it is a DRAFT), in-app (type `INSPECTION`) and by email (`inspection-scheduled` with the time in the market's zone, `inspection-completed`), link `/account/tenancies/{tenancyId}`. Managers are not notified.
- **Account deletion**: `inspections.ForgetUserTx` unlinks the person as inspector, creator, completer, canceller and photo uploader (the names stay).
- **Sample data** (`devseed/inspections.go`, IDs `…b77x`): RPI-2026-900001 (Flat 1 MOVE_IN, COMPLETED on the tenancy's start date, two ATTENTION and one DAMAGED finding with notes, the second bedroom NA) and RPI-2026-900002 (Flat 1 ROUTINE, SCHEDULED 7 days ahead). A Postgres re-seed replaces both.

**Not verified / follow-ups:** migration `0038` and `inspections/postgres.go` have only run where `RP365_TEST_DATABASE_URL` is set (`TestPostgresStoreContract` in the module). Cancelling an inspection does not notify the tenants; the seed has no photos.

## Maintenance requests (API 0.36.2)

Migration `0037_maintenance`, module `internal/modules/maintenance` (BR-019, FR-047; Phase 2 part c), with the ledger's `maintenance.go`, `tenancies/books.go` (the books, properties and tenant matching it reuses) and `app/maintenance.go` (wiring). Routes: `/v1/agent/organisations/{id}/maintenance` (+ `/options`, `/providers`, `/{requestId}`, `/status`, `/comments`, `/assign`, `/costs`, `/photos[/{photoId}]`) and the tenant's `/v1/me/tenancies/{id}/maintenance` (+ `/options`, `/{requestId}`, `/comments`, `/photos[/{photoId}]`) (OpenAPI tag `maintenance`). The exact JSON is the 0.36 contract's "Response shapes (c)".

- **Books and rights** are the tenancies': every agent route answers `404 BOOK_NOT_FOUND` to anybody outside the book, before any validation (tested on every endpoint); STAFF read; OWNER, MANAGER, AGENT write. A tenant reaches only the requests linked to their own tenancy (another tenancy `404 TENANCY_NOT_FOUND`, another request `404 MAINTENANCE_NOT_FOUND`) and has no status, assign, cost, edit or delete route.
- **Reference data**: `maintenance_categories` (8 rows) and `maintenance_priorities` (4), admin-editable and extensible. A request keeps the code and the label it had.
- **Status flow** (`maintenance.Transitions`, table-tested): NEW → TRIAGED → (ASSIGNED) → IN_PROGRESS → RESOLVED → CLOSED, CANCELLED from any open status (with a reason, audited `MAINTENANCE_CANCELLED`), RESOLVED → IN_PROGRESS reopens. Writes are guarded by the status and assignment count read (`409 MAINTENANCE_CHANGED`). Every change goes on the timeline (`maintenance_events`).
- **Assign** (`POST .../assign {directory_profile_id, message?}`): a public, verified `service-providers` profile; sends **one** business enquiry through `businessenquiries.Service.Create`, from the manager's account, quoting the reference, category, priority and property plus the manager's message, never the title, description or anything of the tenant. The idempotency key is the request, the profile and the assignment count, so a retry (even after a failure between the enquiry and the link) replays the same enquiry and links it. Audited `MAINTENANCE_ASSIGNED`.
- **Costs** (`POST .../costs`): **one transaction** writes the cost and **one EXPENSE ledger entry** (`ledger.Service.PrepareMaintenanceCost`: the ledger's own rules, category `maintenance` added to the book when missing, `ledger_entries.property_id` set, the property's listing). Audited `LEDGER_ENTRY_CREATED` and `MAINTENANCE_COST_RECORDED`. Undo = a ledger reversal (the cost then shows `reversed` and leaves the totals).
- **Photos**: JPEG / PNG / WebP decided by the bytes, 10 MB, 8000 px a side, 40 MP, 10 per request; re-encoded as JPEG (2048 px, upright, no EXIF/GPS) into the private object storage (`maintenance/<random>`); served only by the API (`private, no-store`).
- **Notifications**: creating a request and changing its status write an outbox event (`MAINTENANCE_REQUEST_CREATED`, `MAINTENANCE_STATUS_CHANGED`; ids, reference and statuses only). `notifications/maintenance.go` emails and notifies in-app (type `MAINTENANCE`) the book's owners and managers (link `/agent/maintenance/{id}`) and the tenancy's tenants with a verified account (link `/account/tenancies/{tenancyId}`), never the person who acted. Templates `maintenance-request-new` and `maintenance-status-changed`: reference, category and status only.
- **Account deletion**: `maintenance.ForgetUserTx` unlinks the person everywhere and replaces a tenant's name where they acted as tenant (managers' names stay).
- **Sample data** (`devseed/maintenance.go`, IDs `…b75x`): RPM-2026-900001 (Flat 1, raised by tenant@example.test, ASSIGNED to Sample Plumbers with its enquiry `BE-RPM9TAPX` and a USD 85.00 cost / EXPENSE entry `…b762`), 900002 (cottage, IN_PROGRESS), 900003 (Flat 1, tenant, NEW), 900004 (Avondale Court, CLOSED). A Postgres re-seed replaces the requests; the ledger entry and the enquiry are written once.

**Not verified / follow-ups:** migration `0037` and `maintenance/postgres.go` have only run where `RP365_TEST_DATABASE_URL` is set (`TestPostgresStoreContract` in the module). There is no un-assign; an earlier provider stays on the timeline only.

## Rent schedule, receipts and arrears (API 0.36.1)

Migration `0036_rent_schedule`, module `internal/modules/tenancies` (files `schedule.go`, `rent*.go`, `postgres_rent.go`, `memory_rent.go`; BR-019, FR-047; Phase 2 part b), with the ledger's `rent.go`. Routes under `/v1/agent/organisations/{id}`: `/tenancies/{tenancyId}/dues`, `/tenancies/{tenancyId}/receipts[/{receiptId}]`, `/tenancies/{tenancyId}/statement`, `/dues/{dueId}/waive`, `/arrears`; the tenant's `/v1/me/tenancies/{id}/dues|receipts|statement` (OpenAPI tag `rent`). The exact JSON is the 0.36 contract's "Response shapes (b)".

- **The schedule rule** (`schedule.go`, table-tested). Activating a tenancy generates its dues in the same transaction: from `start_date`, the first due day on or after it, every week (ISO weekday) or every 1 / 3 / 12 months on `due_day`; **a month without that day uses its last day** (31 → 28 or 29 February, 30 April), computed from the first due so it never drifts. Through `ended_on`, else the agreed `end_date`, else **rolling: the end of the month 12 months ahead** (an end more than 10 years away counts as rolling). No pro-rating: each due is the full rent. One due per tenancy and date (`rent_dues_tenancy_date_key`), so re-running is idempotent. Ending a tenancy or changing an ACTIVE one's `end_date` re-runs the rule: dues after the end with no allocation and no waiver are removed.
- **The worker** (`app.RunRentScheduleSweeps`, daily and at start) re-runs the rule of every ACTIVE tenancy: a rolling schedule gains the next month's due when the month turns, and tenancies activated under 0.36.0 get their schedules.
- **Dues** are DUE, PART_PAID, PAID or WAIVED. What is paid is **never stored on a due**: it is the sum of the allocations of receipts that are not reversed, so a reversal can never leave a stale figure. Figures "as of" a date (arrears, statements) count receipts received by then and not reversed by then.
- **Receipts** (`POST .../receipts`): the rent currency only; allocated oldest due first or by explicit lines; never above a due's outstanding or the schedule's (checked again under row locks: `409 RENT_DUE_CHANGED`). **One transaction** writes the receipt, its allocations and **one RENT_RECEIVED ledger entry** (USER_RECORDED_UNVERIFIED, `tenancy_id`, tenant contact, lease, listing): `ledger.Service.PrepareRentReceipt` validates the entry with the ledger's own rules and returns its `ledger.Change`, which the tenancies store applies inside its transaction (`ledger.ApplyTx`; in memory `ledger.MemoryStore.ApplyWith`). Audited (`LEDGER_ENTRY_CREATED`, `RENT_RECEIPT_RECORDED`).
- **Undo = ledger reversal.** Reversing the entry (`POST .../ledger/entries/{id}/reverse`, the ledger's rules and rights) runs the ledger stores' **reversal hook** (`tenancies.ConnectLedger`, wired in `app.NewRouter`): in the reversal's transaction the receipt gets `reversed_at`, `reversal_entry_id` and `reversed_on`, and its allocations stop counting. Correcting a receipt's entry is `409 LEDGER_ENTRY_IS_RENT_RECEIPT`; a manual entry still cannot carry `tenancy_id`.
- **Waive** (`POST .../dues/{dueId}/waive {reason}`): forgives what is outstanding; PAID or WAIVED is `409 RENT_DUE_NOT_WAIVABLE`. Audited (`RENT_DUE_WAIVED`).
- **Statement** (`?from&to&format=json|csv|pdf`): CHARGE / PAYMENT / PAYMENT_REVERSED / WAIVER lines with a running balance, per currency; CSV cells guarded against formula injection; the PDF uses the leases' gofpdf stack and Go fonts.
- **Arrears** (`GET .../arrears?as_of&currency&property_id`): per currency, by tenancy, buckets 0-30 (1-30 days), 31-60, 61-90, 90+. The **ledger's receivables report includes them** (`ledger.Service.SetRentArrears`): `outstanding`, `count` and buckets add them, plus `rent_outstanding` and `rent_items` (side RECEIVABLE, no branch filter).
- **Tenant view**: their own dues, receipts and statement; no ledger IDs, contact IDs, audit names, waiver reasons or `can`; anybody else's `404 TENANCY_NOT_FOUND`.
- **Rights and 404s** as part a: every agent route answers `404 BOOK_NOT_FOUND` to anybody outside the book, before any validation (tested on every endpoint); STAFF read; OWNER, MANAGER, AGENT record and waive.
- **Account deletion.** `ForgetUserTx` also unlinks the person as who recorded a receipt (`recorded_by`) or waived a due (`waived_by`); the names stay, as the ledger's author names do.
- **Sample data.** Flat 1's first two dues are PAID; Borrowdale Cottage's due day is now the day of the month 45 days ago (`devseed.CottageDueDay`), so its four past dues are PAID, PAID, DUE (exactly 45 days overdue) and PART_PAID (300.00 of 700.00). Five RENT_RECEIVED ledger entries (IDs `…b74x`, receipts `…b73x`) are posted once; a Postgres re-seed replaces the seeded tenancies' dues, receipts and allocations and keeps the entries.

**Not verified / follow-ups:** migration `0036` and `postgres_rent.go` have only run where `RP365_TEST_DATABASE_URL` is set (`TestPostgresRentStoreContract`, `TestPostgresRentGuards`). The worker sweeps every ACTIVE tenancy once a day (fine at today's scale; batch it if books grow large).

## Properties, units and tenancies (API 0.36.0)

Migration `0035_properties_tenancies`, module `internal/modules/tenancies` (BR-019, FR-047; Phase 2 part a). Routes: `/v1/agent/organisations/{id}/properties` (+ `/{propertyId}`, `/{propertyId}/units[/{unitId}]`), `/v1/agent/organisations/{id}/tenancies` (+ `/options`, `/{tenancyId}`, `/activate`, `/end`) and `/v1/me/tenancies[/{id}]` (OpenAPI tags `properties`, `tenancies`). The exact JSON is the 0.36 contract's "Response shapes (a)".

- **Books.** The same as the CRM and the ledger: an organisation (a kind that manages listings) or an approved direct owner's personal book, addressed with their own user ID. Rights in `auth/capabilities.go`: `MayReadProperties` (any member, STAFF included), `MayWriteProperties` (OWNER, MANAGER, AGENT), `OwnsPropertyBook`. Anybody else, platform administrators and tenants included, gets `404 BOOK_NOT_FOUND` on every route before any validation (tested on every endpoint).
- **Properties and units.** `{name, address, market_code, listing_id?, notes, units: [{label}]}`. A property with no units is one lettable unit. Unit labels are unique per property (case-insensitive). Properties and units are **archived, never deleted** (`deleted_at`), so tenancies, and later maintenance and inspections, keep pointing at them; neither is archived while a DRAFT or ACTIVE tenancy uses it (`409 PROPERTY_IN_USE` / `409 UNIT_IN_USE`). A property without units gains its first unit only while no open tenancy holds the whole of it.
- **Tenancies.** A property; a unit (required when the property has units, null otherwise); tenants (1-10) and landlords (0-10) that are live CRM contacts of the same book; an optional lease of the book; rent `{amount, currency, frequency, due_day}` (WEEKLY, MONTHLY, QUARTERLY, ANNUALLY; `due_day` an ISO weekday for WEEKLY, else a day of the month); an optional deposit in the rent's currency; `start_date`, an optional agreed `end_date`, and notes.
  - Money is exact (`internal/platform/money`, stored `numeric(20,4)`, ISO 4217 codes of reference data, answered with the minor-unit decimals; a JSON number is `400 INVALID_JSON`).
  - Created DRAFT (editable, deletable). `activate` makes it ACTIVE: one per unit or whole property, a partial unique index, `409 UNIT_OCCUPIED`. `end {end_date, reason}` makes it ENDED, or TERMINATED when it ends before the agreed end date; `ended_on` is the actual end.
  - An ACTIVE tenancy may change its parties, lease, deposit, end date and notes; an ended one only its notes.
  - Create, activate, end and delete are `organisation_audit_events` rows (`TENANCY_*`, target `TENANCY`) in the same transaction; a direct owner's book logs them (`audit=true`).
- **Tenant view.** `GET /v1/me/tenancies[/{id}]`: the tenancies (never a DRAFT) where one of the **tenant** contacts is a live contact of the tenancy's own book with the caller's **verified** account email (`crm.Store.WithEmail`). Names, property, unit, rent, deposit, dates and the lease reference only: no notes, contact IDs, emails, phones or audit names. Anybody else's is `404 TENANCY_NOT_FOUND`.
- **Ready for parts b to e.** `ledger_entries.tenancy_id` references `tenancies` (part b lifted the CHECK that kept it NULL: rent receipts set it); units have stable IDs for maintenance and inspections.
- **Account deletion.** The users row is anonymised, so nothing cascades. `tenancies.ForgetUserTx` (and `MemoryStore.ForgetUser`) deletes the person's personal-book DRAFT tenancies, replaces the manager name their tenants see, and unlinks them as creator, activator and ender. The personal book's other records are kept (rent records), as the ledger's are.
- **Sample data** (IDs `…b7xx`): Sample Estates has Avondale Court (Flat 1-3, linked to listing 2) and Borrowdale Cottage (no units).
  - ACTIVE Flat 1: Kudzai Sithole, whose CRM contact now also has `tenant@example.test`, so the sample tenant sees it; landlord Tapiwa Chirwa; USD 950.00 monthly; the sample draft lease.
  - ACTIVE Borrowdale Cottage: Tariro Moyo, USD 700.00, periodic. ENDED Flat 2: Tariro Moyo's earlier tenancy.
  - Second Street Realty has Second Street House (the isolation tests). In Postgres the samples are upserted by ID on every seed.

**Not verified / follow-ups:** migration `0035` and `tenancies/postgres.go` have only run where `RP365_TEST_DATABASE_URL` is set. Rent schedules, receipts and arrears are part b (API 0.36.1, above); maintenance, inspections and reviews are parts c to e.

## Notifications and push (API 0.35.0)

FR-044, FR-045, FR-062, FR-064, FR-065 and FR-003. The code is in `internal/modules/inapp`: devices, in-app notifications, preferences, delivery records and the push sender. The FCM adapter is in `internal/platform/push`. The template-to-notification mapping is in `internal/modules/notifications/inapp.go`.

- **In-app notifications.** The worker creates them from the outbox events the email path already handles, for the same recipients. Only known accounts get one: a guest has no inbox.
  - There is **one per (event, recipient)**. The unique key `notifications(source_event_id, user_id)` enforces it, so a redelivered event creates nothing twice.
  - The types:
    - NEW_LEAD: a property request, or one assigned to you
    - NEW_ENQUIRY
    - APPOINTMENT_REQUESTED, APPOINTMENT_CONFIRMED, APPOINTMENT_CANCELLED, APPOINTMENT_RESCHEDULED
    - APPLICATION_RECEIVED, APPLICATION_UPDATED
    - VENUE_BOOKING_REQUESTED, VENUE_BOOKING_CONFIRMED, VENUE_BOOKING_DECLINED, VENUE_BOOKING_CANCELLED, VENUE_BOOKING_RESCHEDULED
    - LISTING_REVIEWED (including "Your listing is live again" for an approved relist)
    - LISTING_EXPIRY: the 14 / 7 / 1-day reminders and the expiry (API 0.45.0)
    - SUBSCRIPTION_INVOICE, SUBSCRIPTION_ACTIVATED, SUBSCRIPTION_EXPIRED, SUBSCRIPTION_UPDATED (rejected or cancelled)
    - ACCOUNT: suspended or reactivated
    - SECURITY: password changed, administrator sign-in
    - TEAM_INVITATION: created on the request path, when the invited address belongs to an account
  - Receipts of your own action, the welcome email and the onboarding emails have no in-app item.
- **Safe text only.** A notification has a fixed title, plus the reference and the listing's place, for example `PR-ZW-10001 · Borrowdale`.
  - It never contains a name, phone, email, message, reason or answer.
  - `TestInAppTextHasNoPersonalData` renders every template against the seed to check this.
  - Links are pages the web has. Each page checks sign-in and ownership itself.
- **Push** goes through FCM HTTP v1, which also carries APNs.
  - A notification queues one `PENDING` push per device of the recipient that has push consent, provided push is on for the type.
  - The push sender (in the worker; in the API process when there is no database) sends after the commit:
    - The data is `{type, link, notification_id}`, and the APNs badge is the unread count.
    - More than 5 pushes to one device within a minute become one summary push. A device that has already had 5 in the window waits until the window has passed.
    - Statuses are `PENDING`, `ATTEMPTED`, `PROVIDER_ACCEPTED`, `FAILED` (retried at `next_attempt_at`; 30 s doubling, 6 tries) and `DEAD`.
    - **No status means "delivered".** The provider accepting a push is not a person seeing it.
    - During an FCM outage the inbox still fills, and the queued pushes go out when FCM recovers.
  - Email deliveries of a notification are recorded too, as `EMAIL` rows.
- **Devices** (`/v1/me/devices`) are removed:
  - on logout of the session that registered them, and for the optional `device_token` of `POST /v1/auth/logout`;
  - on account deletion (deleted explicitly, because the users row is anonymised);
  - when FCM answers `UNREGISTERED`, `SENDER_ID_MISMATCH` or `INVALID_ARGUMENT` about the token.
- **Preferences** (`/v1/me/notification-preferences`) set email and push per type. In-app is always on.
  - ACCOUNT, SECURITY and SUBSCRIPTION_INVOICE are essential and cannot be turned off (400 on that item).
  - Every change is stored with `changed_at` and its source (`X-Client` header: `web`, `app`, otherwise `api`), and is kept in `notification_preference_changes`.
  - **The notification emails of non-essential types now check the preference before sending.**
- **Health.** `GET /v1/admin/notifications/health` (SUPER_ADMIN, PLATFORM_ADMIN, SUPPORT) returns:
  - counts by channel and status;
  - the dead-letter backlog;
  - the oldest unattempted push.

### Configuration (push)

| Variable | Notes |
| --- | --- |
| `FCM_PROJECT_ID` | The Firebase project id (this project: `rightproperty365`). |
| `FCM_SERVICE_ACCOUNT_JSON` | The service-account key JSON. **A secret**: it is never logged or echoed in an error. |
| `FCM_SERVICE_ACCOUNT_FILE` | Alternatively, the path of the key file, for example a mounted Kubernetes Secret. The JSON variable wins when both are set. |

The OAuth token is a signed RS256 JWT exchanged at Google's token endpoint (plain `crypto/rsa`, no SDK). It is cached until a minute before it expires.

At startup the API and the worker each log **one line** naming the pusher:
- `push provider selected: fcm` when the credentials are set;
- the log pusher in development, which sends nothing and logs the safe text only;
- `push disabled: …` in staging and production without usable credentials. **This is never a startup failure:** in-app notifications and email continue, and no push is queued.

### What you must supply

Already done:
- the Firebase project `rightproperty365`;
- the Android and iOS apps, both `com.rightproperty365.rightproperty365`;
- the APNs auth key, uploaded to Firebase.

Still to supply:
1. **A service-account JSON key** with the **Firebase Cloud Messaging API Admin** role (Google Cloud console → IAM → Service accounts → create a key). Never commit it.
2. **A Kubernetes Secret** holding the key, mounted or exposed to **both the api and the worker** Deployments. Either set `FCM_SERVICE_ACCOUNT_JSON` from the secret, or mount it as a file and set `FCM_SERVICE_ACCOUNT_FILE`. Set `FCM_PROJECT_ID=rightproperty365` on both. For example:
   `kubectl create secret generic rp365-fcm --from-file=service-account.json=./key.json`
3. Run migration `0034_notifications_push`.

The website registers no device; there is no web push in this slice.

## Agent ledger / books (API 0.34.0)

Migration `0033`, module `internal/modules/ledger` (BR-012, FR-026 to FR-029, FSD section 6;
decision D-05: **a cashbook, not double-entry**). Routes under
`/v1/agent/organisations/{id}/ledger` (OpenAPI tag `ledger`): `/options`, `/entries` (list,
post), `/entries/{entryId}` (+ `/reverse`, `/correct`, `/reconcile`, `/notes`, `/attachments`,
`/attachments/{attachmentId}`), `/reports/cashbook`, `/reports/categories`,
`/reports/statement`, `/reports/receivables`, `/reports/export.csv`, `/categories`
(+ `PATCH /categories/{code}`). The exact JSON is the 0.34 contract's "Response shapes".

- **Whose books.** An organisation's (any kind that takes enquiries or consultations, as the
  CRM), or an approved direct owner's personal book addressed with their own user ID. Private
  to them, and not the platform's subscription revenue. Branches are a free-text `branch` label
  on each entry (case-insensitive in filters, cashbook `by=branch`). Rights in
  `auth/capabilities.go`: OWNER and MANAGER read, write, reverse/correct any entry, reconcile,
  export and manage categories (`MayManageLedger`); an AGENT reads, records, and reverses,
  corrects, notes and attaches to **their own** entries while unreconciled
  (`MayCorrectLedgerEntry`); STAFF read. Anybody else, platform administrators included, gets
  `404 BOOK_NOT_FOUND` on every route, before any validation (tested on every endpoint).
- **Money** (`internal/platform/money`, shared with the venue quotes): exact decimals with
  `math/big` (shopspring/decimal is not a dependency), never a float. `amount` is a positive
  decimal **string** with at most the currency's ISO 4217 minor-unit decimals (USD 2, JPY 0,
  KWD 3); a JSON number is `400 INVALID_JSON`. Stored as `numeric(20,4)`, answered with exactly
  the minor-unit decimals (half-even, which only ever pads). Currencies are the ISO 4217 codes of
  the countries in reference data. **Every total is per currency**: each accumulator is keyed by
  currency (and panics if fed another one), every total object carries `currency`, and no
  consolidated conversion is built. A test walks every report's JSON and fails if a total lacks a
  currency or a figure equals a USD figure plus a ZAR one (`ledger/ledgertest`).
- **Posted entries are immutable** (FR-027). No PATCH, no DELETE. `reverse` posts a `REVERSAL`
  (same amount, other direction, `reversal_of`; dated the original's date unless later) and marks
  the original `reversed_by`; `correct` reverses and posts the corrected entry (`correction_of`;
  the original gets `corrected_by`) in **one transaction**. A transfer leg is reversed together
  with its pair. The trigger `ledger_entries_guard` refuses DELETE / TRUNCATE and any UPDATE of
  the book, type, direction, amount, currency, method, category, effective date, recorded_at,
  author, texts, `reversal_of`, `correction_of`, `settles_entry_id`; `reversed_by`,
  `corrected_by` and `transfer_pair` are set once. Only the reconciliation fields and the
  contact / listing / lease links (ON DELETE SET NULL) may change. Books and authors reference
  organisations and users ON DELETE RESTRICT.
- **Types.** INCOME, EXPENSE, RENT_RECEIVED, COMMISSION, DEPOSIT_HELD, DEPOSIT_RETURNED (fixed
  directions and default categories), OPENING_BALANCE (IN or OUT; **once per currency, method and
  branch** while not reversed: a partial unique index, `409 LEDGER_OPENING_BALANCE_EXISTS`; it is
  corrected by reversing it), TRANSFER (an OUT / IN pair between methods or branches, linked both
  ways through `transfer_pair`), and the memo types RECEIVABLE / PAYABLE (no method; never in a
  cashbook balance) that cash entries settle (`settles`, same currency, at most the outstanding,
  checked again under a row lock). Methods CASH, BANK, MOBILE_MONEY, OTHER. Links to a contact
  (CRM), listing or lease must be the same book's (`400`); `tenancy_id` must be null (since 0.36.1
  only a rent receipt sets it).
- **No false verification** (FR-028, BR-012). Manual entries are `USER_RECORDED_UNVERIFIED`
  ("Recorded by you, not verified"). `RECONCILED` ("Marked reconciled by your team") is the
  organisation's own mark against its own statement, set and cleared by OWNER / MANAGER.
  `GATEWAY_CONFIRMED` is reserved: there is no gateway, a body with `status` or
  `gateway_reference` is refused, and a CHECK refuses it without a gateway reference.
- **Reports** (FR-029), computed in Go from the book's entries: the cashbook (opening, the
  period's entries with a running balance, closing; `by=method|branch`), totals per category, a
  contact's statement (with the receivables / payables outstanding), receivables or payables
  ageing (0-30, 31-60, 61-90, 90+ days after the due date) and CSV exports
  (`report=cashbook|entries|categories|statement`; OWNER and MANAGER; every row has a currency;
  formula-like cells are prefixed with `'`). A REVERSAL counts against its original's column, so
  opening + in - out = closing always holds.
- **Attachments**: pdf, jpg, png, webp, sniffed from the content as onboarding documents and
  application files are, 10 MB, 20 per entry, in the private object storage (`ledger/...` keys);
  downloads only through the API (`Content-Disposition: attachment`, sandbox CSP).
- **Audit.** Every post, reversal, correction, (un)reconciliation, attachment, attachment
  download and export is an `organisation_audit_events` row (`LEDGER_*`, target `LEDGER_ENTRY` or
  `LEDGER_BOOK`) in the same transaction. A direct owner's book has no organisation trail: the
  same events are written to the log (`audit=true`), as for applications.
- **Categories** per book, created from the defaults on first use (rent, commission, deposit,
  maintenance, utilities, advertising, salaries, fees, other-income, other-expense), each IN, OUT
  or ANY; OWNER / MANAGER add, rename and deactivate them; never deleted; `kind` fixed once used.
- **Sample data** (Sample Estates; IDs `…a9xx`): opening balances USD cash 500.00, USD bank
  12,000.00 and ZAR bank 25,000.00 (Johannesburg branch); rent received, a deposit held,
  commissions (USD and ZAR) and expenses over the last month; a rent payment entered as 70.00 and
  corrected to 700.00 (a reversal plus a new entry); one reconciled rent payment; a 600.00
  cash-to-bank transfer; an overdue management-fee receivable and a rent receivable partly
  settled. Second Street Realty has one opening balance (the isolation tests). In Postgres the
  samples are written once (entries can never be deleted); a re-seed only restores their contact
  and listing links.

**Not verified / follow-ups:** migration `0033` and `ledger/postgres.go` have only run where
`RP365_TEST_DATABASE_URL` is set. Tenancy-based arrears (feature 10), bank imports and matching,
gateway confirmation, consolidated conversion and double-entry are not built. Reports load the
whole book's entries up to the period's end into memory (fine at agency scale; a SQL aggregation
is the next step for large books).

## Showroom and venue hourly rental (API 0.33.0)

Migration `0032`, module `internal/modules/venues` (BR-007, FR-022 to FR-025). Routes:
`/v1/me/listings/{id}/hourly`, `/v1/venues/{listingId}/availability` and `.../quotes` (public),
`/v1/me/venue-bookings`, `/v1/agent/organisations/{id}/venue-bookings` and
`/v1/me/hosted-venue-bookings` (OpenAPI tag `venues`). The response shapes are in the 0.33
contract's "Response shapes".

- **Eligibility.** The `property_types` reference rows carry `hourly_bookable` (admin-editable
  through `/v1/admin/reference/property_types`). Seeded true: showroom, event-space (venue /
  event space), hall, conference-room, studio and office. Only such listings can have hourly
  settings: `PUT` is `400 LISTING_NOT_ELIGIBLE`, `GET` answers `eligible: false`.
- **Settings** (FR-023), managed by whoever may edit the listing (`auth.MayEditListing`) and not
  reviewed: rate per hour in the listing country's currency, 15 / 30 / 60-minute increments,
  minimum and maximum duration (multiples of the increment), cleaning buffer (0 to 240), capacity,
  deposit, opening hours per weekday in venue-local wall time (several ranges, on the
  increment, up to 24:00), date exceptions (closed, or other hours), fees PERCENT / FIXED /
  PER_HOUR with inclusive effective dates (the tax is code `TAX`), cancellation policy text and
  free-cancellation hours, lead time and horizon. Every public listing and search card carries
  `hourly_booking` (`{enabled, rate_per_hour, currency, increment_minutes}` or null).
- **Time.** The zone is the listing country's IANA zone (`markets`). Opening hours are wall-clock
  times turned into instants per date with `time.LoadLocation`, so their UTC times move with
  daylight saving and the night the clocks change is an hour shorter or longer (tested with
  Europe/London). A slot starts on the increment of the venue's clock; durations are real
  elapsed time. Availability returns UTC instants and offset-carrying local times.
- **Quotes** (`venues/pricing.go`): exact decimals (`math/big`, never a float), one currency
  per quote (a fee in another one is refused), each line rounded half-up to the currency's
  minor unit (JPY 0, KWD 3, most 2): RATE, then FIXED and PER_HOUR fees (the subtotal), then
  PERCENT fees of the subtotal; the deposit apart. A fee applies when its dates cover the
  venue-local start date. Quotes are stored for 30 minutes. A slot outside the rules is
  `400` (misaligned, too short or long) or `409 SLOT_UNAVAILABLE` with `details.reason`
  (CLOSED, TAKEN, BUFFER, LEAD_TIME, OUTSIDE_HORIZON).
- **Enquiry mode only.** No payment is taken or implied anywhere: every quote and booking says
  `mode: ENQUIRY`, the notice "This is an estimate. No payment is taken on RightProperty365; the
  host confirms and arranges payment with you." and that the deposit is "payable to the host if
  confirmed". No unpaid holds exist, so there is nothing to expire.
- **Requests and confirmation** (FR-024). A request (`POST /v1/me/venue-bookings`, idempotent
  per quote) re-checks the slot but holds nothing; overlapping requests may wait side by side.
  **Confirming locks the slot**: in Postgres one transaction takes the listing's
  `venue_settings` row `FOR UPDATE` (the per-listing lock row), then the booking row, then reads
  the other rows that hold capacity (CONFIRMED, and RESCHEDULE_REQUESTED still holding its
  confirmed slot) through the `btree_gist` index on `(listing_id, occupied tstzrange)` and
  checks the new block (slot plus its cleaning buffer) against the capacity; the memory store
  does the same under its mutex. Of two concurrent confirmations for the same capacity one
  gets `409 SLOT_TAKEN` (tested with two goroutines on both stores).
- **States** (FR-025): REQUESTED > CONFIRMED / DECLINED / CANCELLED / RESCHEDULE_REQUESTED (host
  proposal); CONFIRMED > RESCHEDULE_REQUESTED / CANCELLED / COMPLETED / NO_SHOW (after the end);
  RESCHEDULE_REQUESTED > CONFIRMED (re-locks) / CANCELLED. A customer cancels until the start,
  free up to `free_cancellation_hours` before it (`cancellation.free`); the host cancels or
  declines with a reason. `venues.Allowed` is the one table; anything else is `409
  INVALID_BOOKING_TRANSITION`. Every transition is a `venue_booking_events` row (actor, time,
  reason); host decisions on an organisation's booking go to `organisation_audit_events`
  (`VENUE_BOOKING_*`).
- **Who sees what.** The customer sees their own (`404` for another's). The host is the
  organisation's OWNER and MANAGER (every booking) and the listing's assigned AGENT (that
  listing's) (`auth.MaySeeAllVenueBookings` / `MayHandleVenueBooking`); STAFF and other
  organisations get `404`; a direct owner uses `/v1/me/hosted-venue-bookings`.
- **Notifications** (outbox `VENUE_BOOKING_*`, templates `venue-booking-*`, all triggered): the
  party that did not act, plus a receipt to the customer for a request. `VENUE_BOOKING_CONFIRMED`
  (first confirmation) is the analytics server event `booking_confirm`.
- **Kept apart from appointments** (FR-022): own tables and service type `VENUE_HOURLY`; the
  appointment lists, counts and reports never include them (tested).
- **Sample data.** The sample-showrooms host lists a showroom in Harare (`RP-ZW-10021`): USD 50
  an hour, 30-minute steps, 2 to 8 hours, 30-minute buffer, Monday to Saturday 08:00 to 18:00,
  closed on Sunday and on Christmas Day, VAT 15% from 2026-01-01, USD 100 deposit.
  tenant@example.test has a CONFIRMED (a week ahead, 10:00 to 14:00) and a REQUESTED booking;
  consumer@example.test has a REQUESTED booking overlapping the confirmed one (confirming it is
  `409 SLOT_TAKEN`).

**Not verified / follow-ups:** migration `0032` and `venues/postgres.go` have only run where
`RP365_TEST_DATABASE_URL` is set. Reservation mode and payments are not built. Account deletion
removes a customer's bookings through the database cascade only (the memory store keeps them).
`booking_confirm` is recorded as a raw event but has no rollup column yet. Calendar invitations: API 0.39.0, below.

## Questionnaires and applications (API 0.32.0)

Migration `0031`, module `internal/modules/applications` (BR-010, FR-015, FR-017 to FR-019).
Routes: `/v1/agent/organisations/{id}/forms` and `.../applications` (OpenAPI tags `forms` and
`applications`), `/v1/me/listings/{id}/applications-intake`, `/v1/listings/{slug}/application-form`
and `/v1/me/applications`. The response shapes are in the 0.32 contract's "Response shapes".

- **Forms.** A form belongs to an organisation whose kind manages listings, or (with `{id}` =
  their own user ID) to an approved direct owner. OWNER and MANAGER manage, AGENT reads, STAFF
  `403`, anybody else `404 FORM_NOT_FOUND` (`auth.MayReadForms` / `MayManageForms`). Fields:
  short and long text, number, date, single and multiple choice, file; `required`, min / max
  (value, selections), lengths, dates, `accept` (pdf, jpg, png, webp, heic, docx) and
  `max_files` (at most 5), and `visible_if` on an EARLIER choice field. Each form has a
  purpose, a privacy notice, `retention_days` (30 to 3650), a country and an optional expiry.
  Keys that look like government IDs or other sensitive data (passport, national ID, ID
  number, social security, bank account, BVN, tax number, driving licence, health, religion,
  card numbers) give `warnings`, never a block (`applications/forms.go`).
- **Versions** follow lease templates: a DRAFT is edited in place, editing a PUBLISHED version
  creates the next DRAFT (`201`, `409 FORM_DRAFT_EXISTS` when there is one), publishing retires
  the previous version and moves the listings attached to it. **A version with a submitted
  application never changes**: the service only ever edits drafts, the memory store refuses it,
  and the trigger `forms_frozen_after_submission` refuses any content change or delete in the
  database. An application keeps `form_id` and shows exactly its version's questions.
- **Intake** (FR-019): one questionnaire per listing; OPEN, PAUSED or CLOSED with an optional
  `closes_at`; only a PUBLISHED, unexpired form of the listing's organisation and country;
  sale, rent and commercial listings (a short stay is `400 LISTING_NOT_ELIGIBLE`). Whoever may
  edit the listing may change it. Every public listing has `applications`
  (`{open, closes_at, form_name}` or null).
- **Applying.** Signed-in customer sessions only. `POST /v1/me/applications` is idempotent per
  customer, listing and form version; not OPEN is `409 APPLICATIONS_CLOSED` with
  `error.details.reason` (PAUSED, CLOSED, CLOSES_AT_PASSED, FORM_EXPIRED, FORM_RETIRED, NONE)
  and a sentence in `message`. Autosave merges and checks each answer; submit checks every
  required visible answer (a hidden field is not required and its answer is dropped), removes
  files no longer answered, and gives the reference `RPA-APP-<year>-<6 digits>`
  (`application_reference_seq`). Files are checked like onboarding documents: sniffed from the
  content (HEIC by its `ftyp` brand, DOCX as a zip with `word/document.xml`, macro documents
  refused), 10 MB, private storage keys `applications/<random>`, served only by the API as
  attachments. A disallowed type is `400 UNSUPPORTED_FILE_TYPE`.
- **States.** DRAFT > SUBMITTED > UNDER_REVIEW / MORE_INFO / SHORTLISTED / ACCEPTED / REJECTED;
  UNDER_REVIEW > MORE_INFO / SHORTLISTED / ACCEPTED / REJECTED; SHORTLISTED > ACCEPTED /
  REJECTED; any non-final state > WITHDRAWN (the applicant); anything else `409
  INVALID_APPLICATION_TRANSITION` (`details: {from, to, allowed}`). `request-info` moves to
  MORE_INFO; the applicant edits and resubmits (allowed even when intake has closed since).
- **Privacy.** The applicant sees their own (another applicant's is `404`); the receiving side
  is OWNER and MANAGER, the listing's assigned AGENT, or the direct owner (`auth.MayReadApplication`);
  everybody else `404`. DRAFTs are never shown to the receiving side, notes never to the
  applicant. Every `GET .../applications/{id}` writes `APPLICATION_VIEWED` and every file
  download `APPLICATION_FILE_VIEWED` to `organisation_audit_events` (a direct owner has no
  organisation audit: the read is a log line).
- **Notifications** (outbox, emails through the templates `application-submitted`,
  `application-new`, `application-more-info`, `application-status-changed`, all triggered):
  `APPLICATION_SUBMITTED` (applicant, with the reference; the listing's agent, else the
  organisation's owners and managers, or the direct owner: the property-request audience),
  `APPLICATION_INFO_REQUESTED`, `APPLICATION_STATUS_CHANGED` (an email for SHORTLISTED,
  ACCEPTED, REJECTED) and `APPLICATION_WITHDRAWN` (no email). `APPLICATION_SUBMITTED` is also
  linked by the CRM to matching contacts (timeline kind `APPLICATION`) and recorded as the
  server-side analytics event `application_submit` (not again for a resubmission).
- **Retention.** Every hour the worker erases the answers and files of applications whose final
  state is older than their form version's `retention_days`; the row stays as a tombstone
  (reference, status, dates, listing) with an `ERASED` timeline entry, and each erasure is
  logged (`audit=true`). Deleting an account (self-service or by an administrator) deletes the
  person's drafts and withdraws, erases and unlinks (`Deleted applicant`) the others, in the
  account transaction. Removed files go to `application_file_purges`; the worker (or the API
  right away) deletes the objects, so a transaction never touches the storage.
- **Sample data.** Sample Estates' "Rental application": version 1 RETIRED (with the tenant's
  SUBMITTED application `RPA-APP-2026-900001`, a pets question with "Describe them" shown only
  for "yes", and a proof-of-income file), version 2 PUBLISHED (adds smoking and a previous
  landlord) and attached to two rent listings; tenant@example.test also has a DRAFT on version 2.

**Not verified / follow-ups:** migration `0031` and `applications/postgres.go` have only run
where `RP365_TEST_DATABASE_URL` is set. Abandoned drafts are not expired. No push
notifications yet (the outbox events are kept for them).

## Agent CRM: contact books (API 0.31.0)

Migration `0030`, module `internal/modules/crm` (BR-004, FR-014). Routes under
`/v1/agent/organisations/{id}/contacts` (the OpenAPI tag `contacts`).

- **Whose book.** An organisation's; or, for an approved direct owner (landlord, property
  seller), a personal book addressed with their own user ID in place of `{id}`. Who may do what
  is in `auth/capabilities.go` (`MayReadContacts`, `MayWriteContacts`, `MayManageContacts`,
  `OwnsContactBook`): every member of an organisation whose kind takes enquiries reads (STAFF
  included; the book is shared, the relationship owner is a field, not a visibility rule);
  OWNER, MANAGER and AGENT write (create, edit, notes, reminders, stage, listings, merge,
  one-click contact); OWNER and MANAGER, or the contact's current owner, reassign; OWNER and
  MANAGER import and export (not in a personal book). **Anyone else, platform administrators
  included, gets `404 CONTACT_NOT_FOUND` from every endpoint** (each has a wrong-organisation test).
- **Normalisation** (`crm/normalize.go`, no network): emails trimmed and lower-cased; phones to
  E.164. A number in international form (`+` or `00`) is accepted for any country (8 to 15
  digits; a `(0)` after the country code is dropped); a national number is read in the
  organisation's market (a direct owner's: their latest listing's) for ZW, ZA, BW, ZM, MZ, NA,
  MW, KE, TZ, UG, NG, GH, AE, GB, IE, US, CA, AU and IN (trunk prefix and lengths per country).
  Other countries' national numbers are refused with "must start with + and the country code".
- **Duplicates.** Create (and an edit that adds an email or phone) answers `409
  CONTACT_DUPLICATE` with `error.details.duplicates` (`[{id, name}]`) when a live contact of the
  same book shares a normalised email or phone, unless `?allow_duplicate=true`. Merge
  (`POST .../merge {keep_id, merge_ids}`) moves emails, phones, tags, notes (`merged_from`),
  reminders, links and the activity log onto `keep` and leaves the others as tombstones
  (`merged_into`, readable, `409 CONTACT_MERGED` on change, out of the list).
- **Automatic links.** `PROPERTY_REQUEST_CREATED`, `BUSINESS_ENQUIRY_CREATED` and
  `APPOINTMENT_REQUESTED` are also handled by the CRM in the worker (`outbox.Also`, idempotent):
  the item is linked to the live contacts of its organisation (or direct owner) whose email or
  phone equals the customer's entered email or phone, or their verified account email, after
  normalisation (phones read in the item's market). Creating or editing a contact links what
  already matches. Only links; nothing is merged. Without a database the API does this
  in-process.
- **Timeline** (`GET .../{contactId}/timeline`, newest first): notes (`NOTE_ADDED`), the activity
  log (`CREATED`, `EDITED`, `STAGE_CHANGED`, `OWNER_CHANGED`, `REMINDER_ADDED`, `REMINDER_DONE`,
  `REMINDER_REOPENED`, `LISTING_LINKED`, `LISTING_UNLINKED`, `MERGED`, `MERGED_INTO`) and the
  linked `PROPERTY_REQUEST`, `BUSINESS_ENQUIRY` and `APPOINTMENT` items, each with a `label`,
  `text`, `ref` and the raw `data`.
- **Lead stages** NEW, CONTACTED, VIEWING_REQUESTED, VIEWING_CONFIRMED, NEGOTIATING, CLOSED,
  UNSUCCESSFUL (a fixed enum, labels in code; any stage to any other, each move logged).
  **Types and sources** are the reference sets `contact_types` and `contact_sources`
  (admin-editable; sources may also be added).
- **Import** (`POST .../import`, multipart field `file`, UTF-8 CSV, 2 MB and 5,000 rows at most):
  `?dry_run=true` reports per row `CREATE`, `DUPLICATE` (of a contact, or of an earlier row) or
  `ERROR` (per field); a real run creates exactly those rows (`CREATED`), skips duplicates
  (`SKIPPED`) and never overwrites. **Export** (`GET .../export.csv`): live contacts, formula-safe
  cells, `Content-Disposition: attachment`.
- **Audit.** Reassignments (`CONTACT_OWNER_CHANGED`), merges (`CONTACTS_MERGED`), real imports
  (`CONTACTS_IMPORTED`) and exports (`CONTACTS_EXPORTED`, written before any row is sent) go to
  `organisation_audit_events` in the same transaction, shown by `GET .../audit`.
- **Reminders.** `GET .../contacts/reminders?due=today|overdue|week[&mine=true]` for the
  dashboard (market-local day). Every minute the worker writes `CONTACT_REMINDER_DUE` to the
  outbox for each reminder that fell due (once; acknowledged, no email yet).
- **Sample data.** Sample Estates has 8 contacts across the stages (tags, notes, an overdue
  reminder on Nyasha Banda, Tariro Moyo auto-linked to sample appointment 6, the duplicate pair
  Tendai Marufu / T. Marufu to merge); Second Street Realty has 2 (one shares Tariro's email, for
  the isolation test).

**Not verified / follow-ups:** migration `0030` and `crm/postgres.go` have only run where
`RP365_TEST_DATABASE_URL` is set (the store contract test skips otherwise). No email for due
reminders; a direct owner cannot import or export (the audit trail belongs to organisations).
Since 0.32.0 submitted applications are linked too (kind `APPLICATION`).

## Listing analytics (API 0.30.0)

Migration `0029`, module `internal/modules/analytics` (BR-006, FR-033 to FR-036, FSD section 5).
First-party measurement only: no third-party code, no cross-site identifier. **These are not
Google Analytics figures** (the web's `/analytics-definitions` says so and lists the differences).

| Method | Path | Who |
| --- | --- | --- |
| POST | `/v1/analytics/events` | Public, signed-in optional. `application/json` or `text/plain` (the `sendBeacon` fallback without a preflight), at most 32 KB and 50 events, its own per-IP bucket (burst 60, 120 a minute). `202 {accepted, duplicates, rejected}` |
| GET | `/v1/analytics/listings/{id}?period=24h\|7d\|30d\|custom&from=&to=` | The lister (approved direct owner), an OWNER / MANAGER of the listing's organisation, or its assigned agent; **404** for everyone else |
| GET | `/v1/analytics/organisations/{orgId}?period=...` | OWNER / MANAGER: every listing of the organisation; AGENT: only their assigned listings; not a member **404**; STAFF 403 |
| GET | `/v1/admin/analytics/summary?market=&period=...` | SUPER_ADMIN, PLATFORM_ADMIN, MARKET_ADMIN (own markets only; another market 403) |

**Events.** Browser: `property_card_impression` (with `placement` and `rank`),
`listing_detail_view`, `contact_click` (sent by the web when a listing's request form is
submitted, once per listing per page view) and `youtube_play_intent`. Server-side, recorded by
the worker from the outbox (`analytics.Recorder`, chained after the notification handlers with
`outbox.Dispatcher.Also`): `listing_save` (LISTING_SAVED), `enquiry_submit`
(PROPERTY_REQUEST_CREATED about a listing; a viewing request sent through the request form
therefore also counts as an enquiry), `viewing_request` (APPOINTMENT_REQUESTED for a listing),
`appointment_confirm` (APPOINTMENT_CONFIRMED for a listing) and `listing_relist`
(LISTING_RELISTED, a new outbox event of `POST /v1/me/listings/{id}/relist`). A server event's
`event_id` is a UUIDv5 of the outbox event's ID, so a redelivery records nothing new. A browser
sending a server-only type gets that event rejected. Without a database the API records them
in-process and runs the rollup itself (development only).

**Ingestion rules.** Idempotent on `event_id` (primary key; a repeat counts in `duplicates`).
The listing must exist and be public (`listings.Record.Public`); the market is the listing's.
`occurred_at` must lie from 24 h before to 5 min after receipt (a time ahead is clamped to the
receipt). Wrong enums, a session that is not 16-64 `[A-Za-z0-9_-]` or `consent.analytics:
false` reject the event. **SPA de-duplication:** a `listing_detail_view` of the same listing in
the same session less than 30 minutes from a stored one is a duplicate (atomic: an advisory lock
per session and listing in Postgres).

**Bots.** A user agent matching the list in `analytics/bots.go` (crawlers, link previews,
headless browsers, HTTP libraries, uptime monitors; an empty user agent too), or more than 300
events from one session or one network in 10 minutes (the excess), marks the event `bot=true`:
stored, excluded from every metric, counted in `filtered_bot_events`. The response does not
reveal it. The rate counter is in process memory (per API instance, like the rate limiters).

**Privacy.** Stored per event: type, times, listing, market, local day, placement / rank, source
class, device class, the session pseudonym and the **truncated network** (IPv4 /24, IPv6 /48; a
database check refuses anything finer). Never stored: the IP, the user agent, the referrer.
Server-side events have no session and no network. Do Not Track and GPC are honoured by the web
not sending; the API does not read them. **Raw events are never returned by any endpoint.**

**Metric definitions** (the same for advertisers and administrators, `AnalyticsReport`):

| Metric | Definition |
| --- | --- |
| views | valid (non-bot, accepted, de-duplicated) `listing_detail_view` events |
| unique_views | distinct sessions per listing per market-local day, summed over the period's days; a view without a session counts once (disclosed exception) |
| impressions, contact_clicks, video_plays | valid events of those types |
| saves, enquiries, viewing_requests, appointments_confirmed, relists | server-side events |
| conversion | enquiries / views, 4 decimals, `null` without views |
| ctr | contact clicks / views (the click is the request-form submit), 4 decimals, `null` without views |
| sources, devices | valid views by class (`unknown` when not sent) |
| filtered_bot_events | events marked bot |
| late_events_dropped | events dropped because they arrived late (counted on the local day of arrival) |

**Rollups, days and late events.** Each event's market-local day is worked out at ingestion from
`markets.timezone` (every country already had one, e.g. ZW Africa/Harare, GB Europe/London, MU
Indian/Mauritius; UTC when it does not load). The worker recomputes the last **3 local days**
(today included) of every market with recent events **every 15 minutes** and upserts
`analytics_listing_daily` (idempotent: a second run gives the same rows); `refreshed_at` is the
last run. An event that arrives **more than 3 days after it occurred** is dropped (only its
identity is kept) and counted in `late_events_dropped`, so a day is final once it leaves the
window. `period=24h` reads the raw events (`near_real_time: true`, "Near real time, may
change"); 7d / 30d are the last 7 / 30 local days including today; custom is `from`..`to` (at
most 366 days). Site-wide admin periods use UTC days for the window.

**Retention.** `ANALYTICS_RAW_RETENTION_DAYS` (default 90, 7..3650): the worker deletes raw and
late events older than that every 15 minutes. Rollups are kept.

**Tests.** The FR-035 fixture (`analytics/fixture_test.go`): 100 events across ZW, GB (around the
end of summer time, 25 October 2026) and MU, placed around local midnights, with duplicates, bot
user agents, an SPA double view, three rejections and a late event; exact totals per local day,
unique views, filtered, duplicate and late counts, on the memory store and (with
`RP365_TEST_DATABASE_URL`) on Postgres.

**Not verified / follow-ups:** migration `0029` and `analytics/postgres.go` have only run where
`RP365_TEST_DATABASE_URL` points at a database. `analytics_events` is a plain table with an
`(occurred_at)` index: range partitioning by month is a follow-up (the `event_id` key would then
have to include `occurred_at` or move to its own table). The bot rate counter and the zone cache
are per process.

## Saved properties (API 0.27.0)

Migration `0026`, module `internal/modules/saved` (FR-016 "Saved", the first part of the customer
activity dashboard; Enquired, Applied, Viewings and Consultations are the existing
`/v1/me/property-requests` and `/v1/me/appointments`).

| Method | Path | What |
| --- | --- | --- |
| PUT | `/v1/me/saved-listings/{listingId}` | Save a listing that is public right now. Idempotent: `200 {saved, saved_at, created}`; `created` is `false` when it was already saved and `saved_at` stays the first save. Anything not public now is `404 LISTING_NOT_FOUND` |
| DELETE | `/v1/me/saved-listings/{listingId}` | `204`, always: also for an unknown id, a listing that was not saved, or one that is no longer public |
| GET | `/v1/me/saved-listings?page=&page_size=` | Newest saved first (`page_size` 24 by default, at most 100): `{items, page, page_size, total, total_pages}` |
| GET | `/v1/me/saved-listings/ids` | `{ids}`: every saved listing that is public right now, newest first, at most 1000 (for the hearts on cards) |

- **Who** – any **customer-scope** session, whatever its roles (agents too). An admin-scope
  session gets `403 FORBIDDEN`, signed out `401`. One account never sees or removes another's
  saves (a test checks it). `PUT` and `DELETE` share the listing-changes rate-limit bucket (60
  a minute per IP).
- **Public now** is the listings module's own rule (`listings.Record.Public`: PUBLISHED,
  unexpired, verified advertiser), read through `listings.Service.CardStates`, which also gives
  the effective status (`Record.EffectiveStatus`) and the presented card (category, city and
  country labels, photos).
- **History** – each save keeps a snapshot of the card (`slug, reference, title, category,
  price, price_period, city, country, photo`), taken at save time and refreshed whenever the list
  is read while the listing is public. When the listing stops being public the item shows that
  snapshot with `available: false`, `listing.slug: null` and a reason: `EXPIRED`, `LET`, `SOLD`,
  or `WITHDRAWN` for everything else (archived, closed, paused, back in review, advertiser no
  longer verified). Labels: "Listing expired", "Let", "Sold", "No longer listed" (there is no
  reference set for listing statuses, so they are fixed in `saved/model.go`). The web never links
  an unavailable item to its listing or into search (FR-016's acceptance test).
- **Events** – `LISTING_SAVED` / `LISTING_UNSAVED` (aggregate `listing`, payload `{user_id,
  listing_id, at}`) are written in the same transaction as the change, and only when something
  changed (saving twice writes one event). Nothing is emailed; analytics counts `LISTING_SAVED` as
  `listing_save` (API 0.30.0).
- **Caching** – the catalogue (`/v1/listings`, `/v1/listings/{slug}`) stays unpersonalised; the
  saved endpoints answer `Cache-Control: private, no-store`.
- **Account deletion** – deleting an account (by an administrator or `DELETE /v1/me`) deletes its
  saves in the deletion transaction: the `users` row is anonymised, not deleted, so the `ON DELETE
  CASCADE` of `saved_listings.user_id` only covers a hard delete. No events are written for it.
- **Sample data** – the sample tenant (`tenant@example.test`) has two saves: a public sample
  listing (saved two days ago) and the EXPIRED listing `…000000000099` (saved while it was public),
  so the "Saved" tab shows one unavailable item.

**Not verified:** migration `0026` and `internal/modules/saved/postgres.go` have only run where
`RP365_TEST_DATABASE_URL` points at a database (`TestPostgresStore` skips otherwise).

## Map search and listing coordinates (API 0.29.0)

Migration `0028`, `internal/modules/listings` (`location.go`; BR-002, the FSD's listing
"geocoordinates"). A lister places a pin; the public sees the listing on a map and can search an
area of it.

- **Input** – `latitude` (-90..90) and `longitude` (-180..180) in `POST /v1/me/listings` and
  `PATCH /v1/me/listings/{id}`: JSON numbers with at most 6 decimals (about 10 cm), sent
  together, or both `null` to remove the pin. `location_precision` is `EXACT` (the default;
  `null` means it) or `APPROXIMATE`. Field errors (`400 VALIDATION_FAILED`): only one of the pair
  is an error on the missing one; out of range, more than 6 decimals (`1e-7` included), or not a
  number (a string, a boolean) on that coordinate; `0, 0` on `latitude` ("pick the property's
  location"); any other precision (lower case included) on `location_precision`. Coordinates are
  optional: `completeness` never asks for them, and a listing without them is simply not on a map.
- **Review** – a location is listing content: on a published listing it goes into the revision
  and is public only after approval; `GET /v1/admin/listings/{id}` shows `latitude`, `longitude`
  and `location_precision` in `content`, `live` and `changes`. `ListingContent` (the lister's
  and the reviewers' view) always has the **exact** values.
- **Public** – `Listing.location` is `{latitude, longitude, precision}` or `null`, on the detail
  and on every search card (both adapters; it is built from the database row with every hit).
  For **`APPROXIMATE` the server rounds the public coordinates to 2 decimals** (about 1 km; half
  away from zero on the decimal value, like Postgres `round(numeric, 2)`), in
  `Listing.location`, in the database adapter's `bbox` test and in the Elasticsearch document.
  The exact values are never in a public response or in the index; a test greps the public JSON
  for their digits. Saved-listing cards (`SavedListingCard`) carry no location, so nothing changes
  there.
- **Search** – `GET /v1/listings?bbox=west,south,east,north` (decimal degrees, edges included;
  west and east -180..180, south and north -90..90, `south < north`, `west < east`; a box across
  the antimeridian is not supported: `400` on `bbox`) keeps the listings whose **public**
  coordinates are inside; `has_location=true` keeps the listings with coordinates. Both narrow the
  hits, `total` and every facet, and combine with every other filter, sort and page. The response
  gains `mappable_total`: how many of the matching listings have coordinates ("12 of 20 results are
  on the map"; equal to `total` with `bbox` or `has_location`).
  - memory adapter: the same rule in Go (`locationMatches`);
  - database adapter: a numeric range on `latitude` / `longitude`, widened by 0.005 so the btree
    index `(latitude, longitude)` serves it, then the exact test on the public expression
    (`round(..., 2)` for APPROXIMATE); `mappable_total` is `count(l.latitude)` of the same query;
  - Elasticsearch: `geo_bounding_box` on the `location` geo_point (set from the public
    coordinates, `[lon, lat]`) and `exists` for `has_location`, both in the main filter;
    `mappable_total` is a `filter` aggregation (`exists: location` plus the post filters).
- **PostGIS not used: numeric range; why.** The project's Postgres is the official `postgres:16`
  image, which has `cube`, `earthdistance` and `btree_gist` but **not** `postgis` (the handover
  says "Validate PostGIS support before enabling geo filters": it is not there). A map box is two
  range predicates, which a plain btree index answers well at this scale, so no extension is
  created and none is needed. What that rules out today: distance ("within 5 km") search, polygons
  (draw an area), and accurate behaviour near the poles or across the antimeridian. If those are
  wanted, `earthdistance` (already available) or PostGIS (an infrastructure decision) are the
  options; Elasticsearch's geo_point supports them already.
- **Sample data** – most sample listings have plausible coordinates near their suburb or city
  (Harare suburbs, Bulawayo, Victoria Falls, Mutare, Kensington, Shoreditch, Manchester's Northern
  Quarter, Grand Baie): `internal/devseed/coordinates.go`. The Murambi plot
  (`one-acre-plot-murambi-mutare`) is `APPROXIMATE` (public `-18.95, 32.66`); the Ruwa stand
  (`residential-stand-ruwa`) has none.
- **Reindex** – the Elasticsearch document now carries `location` (mapping version **7**; the
  field was already mapped, only `_meta` changes): **run `cmd/reindex` once after the deploy**.
  Until then the documents have no `location`, so with the Elasticsearch driver a `bbox` or
  `has_location=true` finds nothing and `mappable_total` is 0. The database driver needs only
  migration `0028`.

**Not built (follow-ups):** checking that coordinates lie inside the listing's country (not
validated: a Harare listing can be pinned in London); geocoding an address; distance search and
polygons; clustering of markers on the server; a box across the antimeridian.

**Not verified:** migration `0028` and the new columns in `postgres.go` / `manage_postgres.go` /
`devseed/postgres.go` have only run where `RP365_TEST_DATABASE_URL` points at a database
(`TestPostgresStoreKeepsTheLocation` skips otherwise); the `geo_bounding_box` query has only been
checked against the fake cluster of the tests.

## YouTube video tours (API 0.28.0)

Migration `0027`, `internal/modules/listings` (BR-013, FR-011). An advertiser links one YouTube
video to a listing; the public listing page plays it in YouTube's own embedded player.

- **Input** – `youtube_url` in `POST /v1/me/listings` and `PATCH /v1/me/listings/{id}` (a string;
  `null` or `""` removes the video). `listings.ParseYouTubeURL` (`youtube.go`, a pure function
  with a table test) takes the video id from a watch or share link: `http` / `https` only; the
  hosts `youtube.com`, `www.youtube.com`, `m.youtube.com`, `music.youtube.com`, `youtu.be`,
  `youtube-nocookie.com` and `www.youtube-nocookie.com`, matched exactly; the paths
  `/watch?v=ID`, `youtu.be/ID`, `/shorts/ID`, `/embed/ID`, `/live/ID` and `/v/ID`; an id of
  exactly 11 characters of `A-Z a-z 0-9 _ -`. `t=` or `start=` (`90`, `90s`, `1m30s`, `1h2m3s`,
  at most 24 hours) becomes the start time. Everything else is `400 VALIDATION_FAILED` with
  `fields.youtube_url` = "must be a link to a YouTube video": other hosts and look-alikes
  (`youtube.com.evil.test`, `evilyoutube.com`), user information (`https://youtube.com@evil.test/...`),
  ports, percent-encoded hosts or paths, playlists without a video, channels and `@handles`,
  `javascript:`, `data:`, `ftp:`, `<iframe>` and any other markup or whitespace inside the link.
- **Stored** – only `listings.youtube_video_id` and `listings.youtube_start_seconds` (and the same
  two keys in a pending revision's jsonb `Content`); never the link as typed. The editor's
  `content` returns `youtube_url` (canonical: `https://www.youtube.com/watch?v=ID`, plus `&t=Ns`
  when the video starts later so that sending it back keeps the start), `youtube_video_id` and
  `youtube_start_seconds`. A video is optional: `completeness` never asks for one.
- **Review** – a video is listing content. On a draft it changes at once; on a published listing
  it goes into the revision like any other edit and is public only after a reviewer approves
  it. `GET /v1/admin/listings/{id}` shows the id and link in `content` and `live`, and
  `youtube_url` in `changes`.
- **Public** – `Listing.video` is `{provider: "YOUTUBE", id, start_seconds, embed_url, watch_url,
  thumbnail_url}` or `null`; `Listing.has_video` is on every listing, so on search results, and on
  `SavedListingCard` (snapshots saved before 0.28.0 read `false` until refreshed). `embed_url` is
  `https://www.youtube-nocookie.com/embed/ID?rel=0&modestbranding=1` (`&start=N` when set);
  `thumbnail_url` is `https://i.ytimg.com/vi/ID/hqdefault.jpg`.
- **No call to YouTube, ever.** The API makes no oEmbed request and checks no thumbnail; it does
  not know whether a video is public, removed, geo-blocked or embeddable. The browser does: the web
  shows a poster and a "Watch on YouTube" fallback, and loads the player only when the viewer
  presses play.
- **Search** – both adapters return `has_video` (it comes from the database row). The
  Elasticsearch document gains the boolean `has_video` (mapping version **6**): **run
  `cmd/reindex` once after the deploy** (see "Search"). There is no `has_video` filter yet.
- **Sample data** – two backend sample listings have a video, as stand-ins for real tours: the
  London house (`RP-GB-10001`, Big Buck Bunny `aqz-KE-bpKQ`, starting at 30 seconds) and the
  Victoria Falls lodge (`RP-ZW-10019`, "Me at the zoo" `jNQXAC9IVRw`). The seed stores the ids only.

**Not built:** more than one video per listing; other providers (Vimeo, Matterport); a
`has_video` search filter; any server-side check that a video still exists.

**Not verified:** migration `0027` and the new columns in `postgres.go` / `manage_postgres.go` /
`devseed/postgres.go` have only run where `RP365_TEST_DATABASE_URL` points at a database
(`TestPostgresStoreKeepsTheVideo` skips otherwise); the Elasticsearch mapping has only been
checked against the fake cluster of the tests.

## Administrators' area: accounts, subscriptions, reference data, reports (API 0.25.0)

Migration `0024`, modules `internal/modules/accounts`, `subscriptions`, `reports` and
`reference` (`admin.go`), the audit trail in `internal/platform/adminaudit`. Every `/v1/admin`
route needs an admin-scope session and one of the platform roles named below
(`auth.RequirePlatformRole` in `internal/app/app.go`, with the role lists from each module's
`*Roles()` helpers).

| Area | Read | Change |
| --- | --- | --- |
| Accounts (`/v1/admin/users`) | SUPER_ADMIN, PLATFORM_ADMIN, SUPPORT (`accounts.ReadRoles`) | SUPER_ADMIN, PLATFORM_ADMIN (`accounts.ManageRoles`) |
| Audit trail (`/v1/admin/audit`) | SUPER_ADMIN, PLATFORM_ADMIN (`accounts.AuditRoles`) | – (written by the actions themselves) |
| Subscription plans (`/v1/admin/subscription-plans`) | SUPER_ADMIN, FINANCE_ADMIN, PLATFORM_ADMIN, SUPPORT (`subscriptions.ReadRoles`) | SUPER_ADMIN, FINANCE_ADMIN (`subscriptions.PlanAdminRoles`) |
| Subscriptions (`/v1/admin/subscriptions`) | as for plans | SUPER_ADMIN, FINANCE_ADMIN (`subscriptions.DecisionRoles`) |
| Reference data (`/v1/admin/reference`) | SUPER_ADMIN, PLATFORM_ADMIN, CONTENT_ADMIN, MARKET_ADMIN (`reference.AdminRoles`) | the same; MARKET_ADMIN only the rows of its own market |
| Reports summary | SUPER_ADMIN, PLATFORM_ADMIN, FINANCE_ADMIN (`reports.SummaryRoles`) | – |
| `users.csv`, `listings.csv` | SUPER_ADMIN, PLATFORM_ADMIN (`reports.PeopleExportRoles`) | – |
| `subscriptions.csv` | SUPER_ADMIN, FINANCE_ADMIN, PLATFORM_ADMIN (`reports.SubscriptionExportRoles`) | – |
| Online payments (`/v1/admin/payments`, `/v1/admin/subscriptions/{id}/payments`; 0.42.0) | as for plans | refunds (`POST /v1/admin/subscriptions/{id}/refund`): SUPER_ADMIN, FINANCE_ADMIN |
| `payments.csv` (0.42.0) | SUPER_ADMIN, FINANCE_ADMIN (`payments.ExportRoles`) | – |

- **Accounts** – `GET /v1/admin/users?q=&role=&status=&market=` and `GET /{id}`;
  `POST /{id}/suspend {reason}`, `POST /{id}/reactivate`, `POST /{id}/delete {reason}`. Nobody
  acts on their own account (`403 SELF_ACTION_FORBIDDEN`), and only a SUPER_ADMIN acts on an
  account that holds a platform role (`403 ADMIN_ACCOUNT_PROTECTED`); a status that does not allow
  the action is `409 USER_STATE_CONFLICT`. **Suspension** revokes every session at once, and
  sign-in is refused while the account is not `ACTIVE`. **Deletion** is immediate: the account
  is anonymised in place (never removed, so audit records, signed agreements and leases keep
  their references), its sessions revoked, its memberships removed, its own listings withdrawn,
  its assigned listings unassigned, its subscriptions cancelled and its saved signature removed.
  It is refused for the only owner of an organisation that still has other members (`409
  SOLE_OWNER`). The person is emailed when suspended, reactivated and deleted.
- **`DELETE /v1/me {password}`** – the same deletion for the person themselves, after typing the
  password again (`401 INVALID_PASSWORD`); an administrator account cannot use it (`403
  ADMIN_ACCOUNT_CANNOT_SELF_DELETE`). It shares the sign-in rate limit.
- **Subscriptions** – invoices are paid by bank transfer and recorded by finance, or, **only when the
  server has a payment gateway** (`PAYMENT_GATEWAY`; off by default, no real provider built yet), online
  (see "Online payment of subscription invoices"). `GET /v1/me/subscription-plans` lists the
  plans the caller may take (a plan is offered to account roles the caller holds as approved, and
  an organisation plan needs an organisation they own). `POST /v1/me/subscriptions {plan_id,
  period, market_code?, organisation_id?}` creates a `PENDING_PAYMENT` subscription with an
  invoice number and amount, which is emailed; at most one pending request per subscriber. The
  subscriber sees their own and their owned organisations' subscriptions (`404` for anybody
  else's), may withdraw a pending request (`POST /{id}/cancel`, not emailed) and, from 30 days
  before the end, ask for the next period (`POST /{id}/renew`: a new invoice that starts when the
  current period ends). Nothing renews by itself. A payment made outside the platform is recorded by
  `POST /v1/admin/subscriptions/{id}/approve {payment_reference, amount, received_on, ...}`,
  which makes the subscription `ACTIVE` for 3 months (`QUARTERLY`) or 12 (`YEARLY`).
  `reject` and `cancel` take a reason and record `refund_due` (a flag and a note: no money moves;
  online payments are refunded with `POST /{id}/refund`).
  Past its end a subscription is expired by the subscription sweep (every 5 minutes, in the
  worker; in the API on in-memory stores), which emails the subscriber. A plan's limits are shown
  with the current usage; since 0.42.0 a plan with a `package_code` grants that listing package for
  the paid period, which is what is enforced (`limit_enforced: true`); without one the limit is only
  shown (`limit_enforced: false`).
- **Reference data** – `GET /v1/admin/reference`, `GET /{set}`, `POST /{set}` (a new row),
  `PATCH /{set}/{code}`, `POST /{set}/{code}/deactivate` and `/reactivate`. Every set can be
  relabelled, reordered, deactivated and reactivated; rows can be added only to property types,
  listing features, service trades, regions, cities and suburbs. Nothing is deleted: a
  deactivated row disappears from drop-downs and is refused in new input, while data that already
  uses it keeps its code.
- **Reports** – `GET /v1/admin/reports/summary?from=&to=&market=` (default the last 30 days, at
  most about three years) and the CSV exports `users.csv`, `listings.csv`, `subscriptions.csv`.
- **Audit trail** – every administrator action above (account suspensions, reactivations and
  deletions, a person's own deletion, plan changes, subscription decisions, reference data
  changes) is written to `admin_audit_events` in the same transaction, immutably;
  `GET /v1/admin/audit?target_type=&target_id=&action=&actor_user_id=` reads it. Administrator
  sign-ins are **not** in it: they are still only written to the process log.

**Not verified:** migration `0024` and the modules' `postgres.go` have not run against a database
here.

## Leases: templates and drafting (API 0.24.0; organisation templates and stamps 0.26.0)

Migrations `0022` and `0025`, module `internal/modules/leases` (BR-011, FR-030, FR-031). Signing
is the agreements module (see "Agreements"): `POST /v1/me/leases/{id}/send` freezes the lease,
renders its signing copy (no watermark) and sends it as an agreement; the lease then follows it.

**Lease templates are owned by organisations.** Agencies and landlords design their own lease
templates; the platform does not review or approve them, and no lawyer or administrator signs
them off. The platform keeps a small library of **sample** templates that anyone may use as they
are or copy and adapt. Every template in the API carries `organisation_id` (`null` for a sample),
`source` (`SAMPLE` | `ORGANISATION`) and `can_edit` (whether the caller may manage it).

- **What a template is** – one version of a family: `market_code` (the jurisdiction), `name`,
  `lease_kind` (`RESIDENTIAL` | `COMMERCIAL`), ordered `sections` (`heading`, `text`) and
  `optional_clauses` (`code`, `title`, `text`). Text uses `{{variable}}` placeholders from a fixed
  list (`leases.Variables()`, returned as `variables` with every template list): `reference`,
  `agreement_date`, `landlord_names`, `tenant_names`, `landlord_details`, `tenant_details`,
  `property_address`, `country_name`, `lease_kind`, `start_date`, `end_date`, `term`,
  `rent_amount`, `rent_currency`, `rent_period`, `payment_day`, `deposit_amount`,
  `notice_period_days`, `special_conditions`. Any other placeholder (or a stray `{{`) is
  `400 VALIDATION_FAILED`.
- **An organisation's templates (FR-030)** – `/v1/me/organisations/{orgId}/lease-templates`:
  `GET` (every version), `POST` (a new `DRAFT`, or a copy with `copy_from_template_id`: a
  published sample or any of this organisation's templates, named "Copy of <name>" unless a
  `name` is sent; anything else is `400` on `copy_from_template_id`), `GET` and `PATCH /{id}`,
  `POST /{id}/publish`, `POST /{id}/retire` and `GET /{id}/events`. Who may
  (`leases.MayManageTemplates`): the organisation's OWNER and MANAGER, of a kind of organisation
  that manages listings (an independent landlord owns their own organisation), create, edit,
  publish and retire them; its other members who may create leases (AGENT) read them; any other
  member gets `403 FORBIDDEN`, somebody who is not a member `404 ORGANISATION_NOT_FOUND`, and a
  template of another organisation (or a sample) under this path is `404
  LEASE_TEMPLATE_NOT_FOUND`. Publishing needs no review: the template becomes `APPROVED` (shown
  as "Published").
- **Sample templates** – `/v1/admin/lease-templates` for SUPER_ADMIN and CONTENT_ADMIN
  (admin-scope session) holds the samples only (`organisation_id: null`). On
  `POST .../approve` the body is optional: a `legal_review` (`reviewer_name`, `organisation`,
  `reviewed_on`, `note?`) may be recorded when one exists, but none is required.
- **Lifecycle** – `DRAFT` (edited in place) -> approve (samples) or publish (an organisation's)
  -> `APPROVED` -> retire -> `RETIRED`. PATCH on an approved template creates the family's next
  `DRAFT` version (`201`, at most one draft per family: `409 LEASE_TEMPLATE_DRAFT_EXISTS`);
  approving or publishing a version retires the family's previous approved version. An
  organisation may also retire a `DRAFT`; a sample can only be retired once approved. Leases keep
  the version they were made from. Every step is in `lease_template_events` (immutable; a copy
  records the template and version it was copied from).
- **Offered templates** – `GET /v1/me/lease-templates?market=&organisation_id=` returns the
  `APPROVED` samples plus the published templates of the organisations whose listings the caller
  manages (only `organisation_id`'s when it is given; `400` when the caller does not manage that
  organisation's listings). On create or change a lease accepts an approved template that is a
  sample or belongs to the lease's organisation (`400` on `template_id` for another
  organisation's; a lease without an organisation can use samples only); a draft, retired or
  other-country template is refused (`422 LEASE_TEMPLATE_NOT_AVAILABLE`,
  `422 LEASE_MARKET_MISMATCH`).
- **Drafting (FR-031)** – `/v1/me/leases` (list, create, get, PATCH, DELETE while `DRAFT`, `pdf`,
  `cancel`, `events`). Who may: `leases.MayCreateLeases` = `Principal.ManagesListings` on a customer
  session (the dashboard's `can_manage_listings`): OWNER, MANAGER and AGENT of a listing kind of
  organisation, approved landlords and sellers; STAFF, seekers and pending professionals `403`.
  OWNER and MANAGER see and act on every lease of their organisation; an AGENT the leases they
  created; a direct owner their own; anyone else `404`. A lease can only be drafted for a listing
  the caller may edit (`MayEditListing`, shown as `can_edit` on `GET /v1/me/listings` since
  0.26.1; an AGENT's colleagues' listings are read-only); any other `listing_id` is `404
  LISTING_NOT_FOUND`. The listing gives the organisation, the country and the address
  (suburb, city, region, country: listings have no street address). Every mandatory field is
  validated on every save (see the OpenAPI operation). References `RPL-<year>-<6 digits>` come
  from `lease_reference_seq`. `landlord_stamp: true` asks each landlord to apply their stamp as
  well as signing (see "Agreements": generated stamps).
- **PDF** – `github.com/go-pdf/fpdf` (MIT, pure Go) with the Go fonts
  (`golang.org/x/image/font/gofont`, BSD-3-Clause, embedded TTF; accents such as "Zoë
  Müller-Śmiałek" print as typed). Drafts carry a diagonal DRAFT watermark on every page; every
  page has a footer (reference, template name and version, page x of y) and a row of initials
  boxes (one per party); the end has a signature block per party (signature, name, date; with
  `landlord_stamp`, a 40 mm stamp box beside each landlord's name and date). The
  renderer reports where those boxes are (`leases.Layout`, PDF points from the top-left), so the
  agreement's fields are placed on them. Rendering is deterministic (sorted catalogue, fixed
  dates). Stored privately under `leases/<32 hex>`; `lease_documents` keeps per kind the input
  snapshot, the template version, the object key and the SHA-256. **Only the newest draft PDF is
  kept** (each save replaces the object; the audit trail keeps each version's SHA-256); the
  signing copy and the signed PDF are kept.
- Rate limit: lease and template changes 30 / 30 per minute. Every read and PDF is
  `Cache-Control: private, no-store`.
- **Dashboard** – `organisations[].can_create_leases`, `capabilities.creates_leases`, counters
  `leases.awaiting_signatures` and `leases.drafts`.

**Sample data** (memory store and `cmd/seed`, idempotent): an APPROVED sample template "Sample
residential lease" for Zimbabwe (`...e301`) and for the United Kingdom (`...e302`) whose
description and recorded legal review say "SAMPLE — not reviewed by a lawyer; for demonstration
on staging only" (a copy made by an organisation does not keep that description), and a DRAFT lease
`RPL-2026-900001` by `agent@example.test` on listing 2 with `tenant@example.test` as tenant and
`landlord@example.test` as landlord.

**Not built** (leases): deleting templates; per-country variable sets; Word import; renewals,
rent reviews and inventories; a currency check against the country (any ISO 4217 code is
accepted).

**Not verified:** migrations `0022` and `0025` and `internal/modules/leases/postgres.go` have not
run against a database here (`RP365_TEST_DATABASE_URL` tests are skipped without one).

## Agreements: electronic signing (API 0.24.0; stamps 0.26.0)

Migrations `0023` and `0025`, module `internal/modules/agreements` (FR-032), modelled on Adobe
Acrobat Sign. An agreement wraps one PDF: a lease's frozen signing copy (`POST /v1/me/leases/{id}/send`) or a
PDF the sender uploads (`PUT /v1/me/agreements/{id}/document`: at most 20 MB and 50 pages, must
open without a password and import page by page). Who may send: as for leases (whoever manages
listings; the organisation's OWNER and MANAGER see every agreement of it).

- **Lifecycle** – `DRAFT` -> `OUT_FOR_SIGNATURE` -> `COMPLETED` | `DECLINED` | `CANCELLED` |
  `EXPIRED` (default expiry 30 days; an agreement past it is expired on the next read or by the
  worker's sweep). A lease follows its agreement (`SIGNED`, `DECLINED`, `CANCELLED`, `EXPIRED`;
  its PDF becomes the signed one; its events list the agreement's as `AGREEMENT_*`).
- **Recipients** – `{name, email, role: SIGNER | APPROVER | CC, order}`. Same order = parallel;
  order n is emailed only once every earlier SIGNER and APPROVER finished; a CC is never asked to
  act and gets the completed copy. The sender can replace a pending recipient's address (their
  links die, a new one is sent) and remind them (a new link; earlier ones keep working).
- **Fields** – `PUT .../fields` while `DRAFT`: SIGNATURE, INITIALS, DATE_SIGNED (filled when they
  sign), NAME (filled), TEXT, CHECKBOX and STAMP, each for a SIGNER or APPROVER, on a page, inside
  it, in PDF points from the top-left; every SIGNER needs a SIGNATURE (`422
  AGREEMENT_FIELDS_INCOMPLETE`). TEXT, CHECKBOX and STAMP are required unless sent with
  `required: false`. A STAMP field is for a SIGNER only (an APPROVER has no SIGNATURE, INITIALS
  or STAMP fields) and at least 57 x 57 points (the default is 113 x 113, 40 mm square).
  `document.pages` gives every page's width and height for pdf.js. A lease's fields are placed
  automatically: each party's INITIALS on every page's footer row, SIGNATURE, NAME and DATE_SIGNED
  in the signature section, and with `landlord_stamp` a STAMP in each landlord's stamp box (the
  lease renderer reports where).
- **Signing by link** – `PUBLIC_WEB_URL/sign/{token}`; `GET /v1/signing/{token}` (agreement,
  recipient, their fields with the values so far, the others' names and status, page sizes),
  `/pdf`, `POST /code` (six digits, 10 minutes, 5 tries, one every 30 s, 10 per recipient; HMAC
  keyed with the link token is stored, never the code), `POST /submit` (`code`, `consent: true`,
  `field_values`, the adopted `signature` and `initials`: TYPED in one of three embedded styles,
  DRAWN PNG up to 200 KB, UPLOADED PNG/JPEG up to 500 KB, re-encoded; and `stamp`, required when
  the recipient has a required STAMP field), `POST /decline {reason}`. The typed styles are three
  script fonts vendored in `internal/modules/agreements/fonts` with their SIL Open Font Licence
  1.1 texts: Great Vibes (`FLOWING`), Kaushan Script (`BOLD`) and Allura (`CLASSIC`); a name with
  a letter a script font lacks (Cyrillic, Greek, ...) is drawn in a Go italic face instead. Stamps
  are set in DejaVu Sans Condensed Bold, vendored in the same folder with its licence
  (`DejaVuSansCondensed-Bold-LICENSE.txt`), with Go Bold for a letter it lacks.
  `AUTH_DEMO_CODE` is accepted in development and staging only, as for signing in, and is
  recorded as `DEMO_CODE` (the audit report says it is not a real verification). Links: one per
  recipient, only the SHA-256 stored, valid until the expiry, read-only for 90 days after
  completion; the token never reaches the access log (`httpx.LoggedPath`). Signed-in recipients:
  `GET /v1/me/signing-requests` (matched on the verified email) and
  `POST /v1/me/signing-requests/{recipientId}/link`; a saved signature, initials and stamp:
  `GET/PUT /v1/me/signature`.
- **Completion** – the original pages are imported (`github.com/phpdave11/gofpdi`, MIT) into fpdf,
  every value is drawn at its place (signature images, typed signatures in the chosen style,
  initials, dates, text, ticks, stamps), and an **Audit report** page is appended (name,
  reference, sender, dates, original SHA-256, the history: created, emailed, viewed with IP and
  device, email verified, signed or approved, a "Stamp applied" line, completed). The final PDF's SHA-256 is recorded (a file cannot
  print its own); `GET /v1/verify/{reference}` (fingerprints only, no names) and `POST /v1/verify`
  (upload a copy: `match`) check it. Everybody, CCs and the sender included, is emailed.
- **Generated stamps (0.26.0, `stamp.go`)** – a signer types the text of their company stamp and
  the server draws it like a rubber stamp, as a transparent PNG. It is a picture of a stamp, not a
  certificate seal: no key or certificate is involved. A stamp is `{shape, colour, top_text,
  centre_lines, bottom_text, include_date}`: `shape` `ROUND` (the default), `OVAL` or `RECTANGLE`;
  `colour` `BLUE` (the default), `RED`, `BLACK`, `GREEN` or `PURPLE`; `top_text` required (the
  name on the stamp, at most 60 characters); at most 3 `centre_lines` of at most 30 characters;
  `bottom_text` optional, at most 60. Text is trimmed and upper-cased. Round and oval stamps have
  a double ring with the top and bottom texts following the arcs and the centre lines (and the
  date) across the middle; a rectangle stacks the lines inside a double border. With
  `include_date` the date is printed between two rules as `16 NOV 2026`. The ink is slightly
  uneven on purpose, and rendering is deterministic (the same stamp and date give the same bytes).
  - `POST /v1/stamp-preview {stamp, date?}` (public, no session, so signers by link can use it;
    the signing links' rate limit) returns the PNG (600 px on its long side, `Cache-Control:
    private, no-store`); `date` is `YYYY-MM-DD`, today when left out; problems are `400
    VALIDATION_FAILED` under `stamp.*`.
  - A signed-in user can save a stamp with their signature: `PUT /v1/me/signature` with `stamp`
    (left out: kept; `null`: cleared). `GET /v1/signing/{token}` returns it as `saved_stamp` when
    the caller is signed in with the recipient's address and the document has a STAMP field for
    them.
  - On submit, the cleaned `stamp` is stored with the recipient (`agreement_recipients.stamp`).
    The completed PDF draws it, dated the day the signer signed, in each of their STAMP fields,
    and the audit report adds "Stamp applied: <top text>" (by whom; generated by RightProperty365
    from the text the signer typed).
- **Digital seal: not applied.** `github.com/digitorus/pdfsign` was not added: it was not
  evaluated or tested here without a certificate, so no `SIGNING_SEAL_*` setting exists and the
  audit report states that no seal is applied. Tamper evidence is the recorded SHA-256.
- **Reminders and expiry** – the worker runs `agreements.Service.Sweep` every 5 minutes
  (`app.RunAgreementSweeps`; the API does it itself on in-memory stores): one reminder per
  recipient and interval (`DAILY`, `EVERY_3_DAYS`, `WEEKLY`) since they were emailed, claimed once
  in `agreement_jobs` (`reminder:<agreement>:<recipient>:<n>`), so several runs or processes send
  it once.
- **Emails** – `agreement-signing-request`, `agreement-reminder`, `agreement-signing-code`,
  `agreement-completed`, `agreement-declined`: sent from the request path (they carry links or
  codes), never through the outbox; a failure is logged.
- Rate limits: signing links 20 / 20 per minute per IP, code sending 5 / 5 per minute per IP,
  verification 10 / 10 per minute; `POST /v1/stamp-preview` shares the signing links' limit;
  agreement changes share the lease bucket, uploads the upload bucket. Every read and PDF is
  `Cache-Control: private, no-store`.
- Wording: "Signed electronically on RightProperty365"; never "legally executed"; enforceability,
  witnessing, official stamping (such as stamp duty) and registration depend on the jurisdiction
  and are not provided. The audit report says a stamp added while signing is the signer's own
  company stamp.

**Sample data**: an uploaded "Sample inventory and condition report" (`RPA-2026-900001`, drawn by
the seed) out for signature to `tenant@example.test` (SIGNER, order 1), then
`landlord@example.test` (APPROVER, order 2), `estates.owner@example.test` in copy; and the lease
`RPL-2026-900002` on listing 5 out for signature (`RPA-2026-900002`) to the landlord and the
tenant. Both sent by `agent@example.test`. Nobody holds the seeded links: a signed-in recipient
opens the document from *Documents to sign* (`POST /v1/me/signing-requests/{id}/link`).

**Not built** (agreements): a digital seal (a certificate seal; the generated stamp is only a
picture); uploading a scanned stamp image; SMS, ID or knowledge-based verification; delegation; in-person signing; bulk send; reusable field layouts; reading the
uploaded PDF's own form fields; rotated pages (fields assume an unrotated MediaBox).

**Not verified:** migrations `0023` and `0025` and `internal/modules/agreements/postgres.go` have
not run against a database here.

## Agency teams (API 0.23.0)

Migration `0021`, module `internal/modules/teams` (assignment is in
`propertyrequests/assign.go`, join decisions in `onboarding/agency.go`, the audit trail in
`internal/platform/orgaudit`). An organisation's OWNER and MANAGER run their own team; the rules
are in `auth/capabilities.go` (`GrantableRoles`, `MayManageTeam`, `RoleHoldsLeads`).

- **Who may do what.** An OWNER invites, changes, suspends and removes MANAGER, AGENT and STAFF
  members; a MANAGER only AGENT and STAFF. Nobody grants OWNER, nobody changes or removes an
  OWNER (the last active owner: `409 LAST_OWNER`; another owner: `403 OWNER_PROTECTED`), nobody
  acts on themselves (`403 SELF_CHANGE_FORBIDDEN`). AGENT and STAFF get `403`, non-members `404`.
- **Invitations** – `POST /v1/agent/organisations/{id}/invitations` `{email, role, job_title?}`. A
  single-use link `PUBLIC_WEB_URL/invitations/{token}`, valid for 7 days, emailed from the request
  (the token is a credential, so it never goes through the outbox; only its SHA-256 is stored).
  Inviting a pending address again replaces the old link. `GET /v1/invitations/{token}` is public
  (organisation, role, inviter, masked address, expiry); `POST /v1/invitations/{token}/accept`
  needs a session of the invited address (`403 INVITATION_EMAIL_MISMATCH`), refuses a property
  seeker account (`409 SEEKER_ACCOUNT_CANNOT_JOIN`), and creates the membership, the matching
  account role (in an agency) and, for an AGENT, a verified agent profile linked to the agency.
- **Join requests** – the AGENT / SALES_AGENT / LETTINGS_AGENT / AGENCY_STAFF / AGENCY_ADMIN
  applications that name an agency are listed to that agency
  (`GET .../join-requests`) and can be approved or declined there (`POST .../approve`,
  `.../decline {reason}`) with exactly the platform reviewer's effects and emails; a MANAGER only
  for those that become AGENT or STAFF. Whoever decides first wins; the other gets `409`.
- **Change, suspend, remove** – `PATCH .../members/{userId}` `{role?, status?, job_title?}`,
  `DELETE .../members/{userId}` (membership `REMOVED`, no longer listed). Access is read from the
  ACTIVE memberships on every request (nothing is cached in the session), so it is gone from the
  member's next request on. Their open requests and appointments go back to the organisation
  inbox (`UNASSIGNED` timeline entries); a member made STAFF gives back their open requests.
- **Assignment** – `POST /v1/agent/property-requests/{id}/assign` `{user_id | null}`, OWNER and
  MANAGER only, to an ACTIVE OWNER, MANAGER or AGENT of the receiving organisation. Writes
  `ASSIGNED` (and `NEW_ENQUIRY` -> `ASSIGNED`), emails the assignee (outbox
  `PROPERTY_REQUEST_ASSIGNED`). Received requests show `assigned_to`.
- **Audit trail** – `organisation_audit_events`, written in the same transaction as each change
  (invites, cancellations, acceptances, join decisions by the agency or the platform, role,
  status and job title changes, removals with the released counts, assignments), immutable, and
  listed newest first by `GET .../audit` (OWNER and MANAGER).
- **Listing review** – a reviewer who is a member of the listing's organisation, or who sells, is
  the agent of, created or submitted it, gets `403 SELF_REVIEW_FORBIDDEN`.
- Rate limits: team changes bursts of 30 / 30 per minute; the invitation link and its accept 20 /
  20 per minute; assignment shares the request-work bucket. Every read is
  `Cache-Control: private, no-store`.

**Not built** (teams): transferring ownership or adding a second owner through the API (an owner
is made only by an approved AGENCY_OWNER application or by the seed); a removed agent's public
profile and their listings' assigned agent are not changed (the listings' requests go to the
organisation inbox meanwhile); bulk reassignment; resending an invitation without replacing it.

## Appointments: viewings and consultations

Professionals (estate agents, agency staff, property managers, solicitors, the other business
roles, and landlords and property sellers for their own listings) see and manage their
appointments through `/v1/inbox/appointments`; customers through `/v1/me/appointments`.

An appointment is a property **VIEWING** (always tied to a listing) or a **CONSULTATION** (with a
listing's advertiser, or with a directory profile such as a solicitor or an estate agent). It is
created in one of two ways:

1. **With a property request.** `POST /v1/property-requests` of type `VIEWING_REQUEST` or
   `CONSULTATION_REQUEST` **with** a `preferred_slot_utc` writes the appointment in the same
   transaction as the request (status `REQUESTED`, 30 minutes, linked to the request). The
   response gains `appointment: {id, reference} | null`: null without a slot, for the other request
   types, and when the request could not be routed.
2. **Explicitly, by a signed-in customer.** `POST /v1/appointments` with exactly one of
   `listing_id` and `recipient: {kind, slug}`, `start_utc`, optional `duration_minutes` (15–240,
   default 30) and `note`, `preferred_contact_method` (plus `phone` for PHONE or WHATSAPP), an
   idempotency key and `consent`. Idempotent like property requests (`200` replay,
   `409 IDEMPOTENCY_CONFLICT`); its own rate-limit bucket (burst 10, 10/min); `201 {appointment}`.

**Routing** is the routing of property requests and business enquiries, nothing new: the listing's
assigned agent, else the organisation, else the direct owner (landlord or property seller); for a
profile, its organisation, and for an agent profile the named agent. Unlike an enquiry, an
appointment nobody could confirm is refused (`410`), not stored.

```
REQUESTED ──confirm──> CONFIRMED ──complete──> COMPLETED
    │                     │  └─────no-show───> NO_SHOW
    └── either party proposes a new time ──> RESCHEDULE_REQUESTED ──the OTHER party accepts──> CONFIRMED
REQUESTED | CONFIRMED | RESCHEDULE_REQUESTED ──cancel──> CANCELLED
```

- Every transition is one immutable row in `appointment_timeline` (actor, from, to, note, time; a
  trigger refuses updates) and one outbox event (`APPOINTMENT_REQUESTED`, `_CONFIRMED`,
  `_RESCHEDULE_REQUESTED`, `_CANCELLED`, `_COMPLETED`, `_NO_SHOW`, `_ASSIGNED`) carrying
  identifiers, statuses and times only, written in the same transaction.
- `status` always comes with `status_label`: "Requested", "Confirmed", "Reschedule Requested",
  "Completed", "No Show", "Cancelled".
- Start times must be in the future (and at most 12 months ahead) to create, propose or confirm;
  `COMPLETED` and `NO_SHOW` only after the start time. An illegal transition is
  `409 INVALID_STATE`.
- **Proposing a new time** sets `proposed_start_utc` and `proposed_by` (`CUSTOMER` or
  `PROFESSIONAL`); `start_utc` changes only when the other party accepts. The customer accepts
  with `POST /v1/me/appointments/{id}/accept`; **the professional accepts a customer's proposal
  with `POST /v1/inbox/appointments/{id}/confirm` and no body.** A confirm with a *different*
  `start_utc` does not confirm: it becomes the professional's proposal. The party being asked may
  answer with a counter-proposal instead (an addition to the brief's state machine).
- **No double booking.** One professional — the assigned user, or the organisation when nobody is
  assigned — cannot have two `CONFIRMED` appointments that overlap; back-to-back is fine.
  Confirming, accepting or assigning into a clash is `409 SLOT_TAKEN`. In Postgres this is a
  **locked check inside the confirming transaction** (`pg_advisory_xact_lock` on the professional,
  then a search for an overlapping confirmed row), not an exclusion constraint, so no extension is
  needed; in memory it runs under the store lock.
- **Who may manage them** is decided in `internal/platform/auth/capabilities.go`
  (`MayManageAppointments`, `MayAssignWork`, `IsDirectOwner`): OWNER and MANAGER see all of their
  organisation's appointments and may assign them to a member; AGENT and STAFF see those assigned
  to them, the organisation's unassigned **viewings**, and consultations addressed to their own
  agent profile; an **approved** landlord or property seller sees those for their own listings.
  Anyone else gets `403`; another organisation's appointment is `404`.
- **Contact details.** The customer's name, email and phone are a private snapshot on the
  appointment, returned only by `/v1/inbox/appointments`. The customer sees the professional only
  as a public profile reference (`with: {kind, slug, name}`), never a phone number or an email
  address. Listing and profile names are a snapshot too, so **an appointment's history stays
  readable after its listing expires**.
- **Guests.** An appointment made from a guest's viewing request has no customer account, so the
  guest cannot act on it through the API. The professional can still confirm, propose another
  time or cancel, and the guest is told by email. (A guest cannot *accept* a proposed time: the
  professional has to confirm the original time or cancel.)
- **Emails** (outbox-driven, one per transition and recipient, sent once however often the worker
  runs): to the professional — appointment requested, cancelled by the customer, the customer
  proposed a new time; to the customer — confirmed, a new time proposed, cancelled. Times are
  written in the market's time zone with the zone named ("Tuesday 17 November 2026, 10:00
  (Central Africa Time)"). No email carries the other party's contact details. A confirmation
  always goes to the customer (also when it was the customer who accepted a proposed time); the
  professional is not emailed about it, nor is anyone about completed, no-show and assignments.
  The buttons open `/agent/appointments` and
  `/account/appointments` on the web app (`notifications.PathAppointmentsInbox`,
  `PathMyAppointments`): **those pages must exist there**.
- `GET /v1/inbox/summary` returns `{property_requests: {new}, business_enquiries: {new},
  appointments: {requested, reschedule_requested, upcoming_confirmed}}`, each scoped as its own
  inbox. A property request is `new` while its lead stage is `NEW_ENQUIRY`; business enquiries
  have no read status yet, so there `new` is all of them (capped at 100).
- **Sample data** (memory and `cmd/seed`): seven appointments relative to the moment of seeding —
  for `agent@example.test` (requested, confirmed, completed, a confirmed consultation),
  `estates.agent2@example.test` (a reschedule the consumer can accept, a guest's request), the
  solicitor `sample-law-chambers@example.test` (a consultation), all but the guest's for
  `consumer@example.test`.

**Not built** (appointments): venue and showroom hourly bookings, short-stay bookings,
appointment slot configuration (any future time can be requested), ICS invites,
calendar sync, reminders, push notifications (`GET /v1/agent/organisations/{id}/members` now
gives OWNER and MANAGER the user IDs `assign` needs), appointments created from business enquiries
(`business_enquiry_id` exists and is always null), and automatic expiry of requests that were
never answered.

**Not verified:** the Postgres store, migration `0011` (tables, the timeline trigger), the
advisory-lock overlap check and the `cmd/seed` rows have never run against a database. Everything
above is tested on the in-memory stores.

## Countries: the platform is global

> "There is no limit country it's a global but we start listing the countries based on the
> listings we have." (the project owner, 2026-11-14)

**Any country may be used by a lister. What the public sees in country lists is the countries
that have published listings.** Those are two different things, and the API keeps them apart:

| | Which countries | Where |
| --- | --- | --- |
| A lister, an applicant, a search filter | **every** country: the full ISO 3166-1 list, 249 rows | `GET /v1/countries` (with `?q=` for a searchable drop-down) |
| The public (country lists, the search's country facet, suggestions) | only the countries with at least one **published, unexpired** listing right now | `GET /v1/countries?has_listings=true`, `facets.country`, `suggest` |

- **The table.** `internal/modules/markets/countries.go` holds every officially assigned ISO
  3166-1 alpha-2 code with an English short name, its ISO 4217 currency and a default IANA time
  zone. It is data in the repository (compiled by hand from the three standards; never fetched
  at run time). Tests check the count (249), every code and currency's shape, that every zone
  loads, and that migration `0014` and `cmd/seed` write exactly these rows. **It has not been
  reconciled line by line against the standards**: check a row before relying on it for anything
  legal.
- **A market is a country.** The table is still called `markets` and `GET /v1/markets` still
  works: it returns the same response as `GET /v1/countries`. ZW, GB, AE, BW and ZA keep the IDs
  and rows they had; the others get an ID derived from their code.
- **Nothing is "planned" any more.** `status` is kept in the table and the JSON so existing
  clients do not break, but it is always `ACTIVE` and gates nothing. For the rare case a
  country must be switched off there is `markets.enabled` (default true): a disabled country is
  not listed and is refused as a `country` (`UPDATE markets SET enabled = false WHERE code = 'XX'`;
  there is no admin endpoint). Its existing listings and profiles are not hidden by that.
- **One time zone per country: the capital's.** Countries with several zones (the United States,
  Canada, Russia, Australia, Brazil, Indonesia, Mexico, ...) will need a **per-city time zone**
  before appointments or stays there are shown in local time. Not built.
- **One currency per country**, the one prices are normally quoted in. Zimbabwe stays `USD`, as
  it was seeded from the start. A listing may still be priced in any currency.
- **Each response item**: `{id, code, name, currency_code, timezone, status, listing_count,
  has_listings}`. Order: Zimbabwe, the United Kingdom, then by name; with `has_listings=true`,
  by listing count (highest first), then name. Cacheable for a minute with an ETag, so a count
  can lag a newly published listing by that long.

### Places: reference rows where they exist, free text where they do not

Regions, cities and suburbs stay reference rows per country, **but they can no longer be assumed
to exist**: only Zimbabwe and the United Kingdom have any (`GET /v1/reference/cities?market=MU`
is an empty list).

- On a **listing**, a **directory profile** and an **onboarding application** the country is
  required. Region, city and suburb are reference codes where the country has rows; where it
  has none, the city (and optionally the region) is **free text**: plain text, at most 80
  characters, no HTML (`city_name`, `region_name` in the tables; `listings.CheckPlaceName`).
  A row has a city code or free text, never both, and a suburb only under a city code.
- **In JSON a place is always `{code: string | null, label: string}`** (`listings.Place`):
  `{"code":"harare","label":"Harare"}` for a reference place,
  `{"code":null,"label":"Grand Baie"}` for free text. The fields keep their names (`city`,
  `suburb`, `region`).
  > **This changes two things both clients read.** `code` used to be always a string and is
  > now **nullable**. A listing's `suburb` used to be always an object and is now **null** when
  > the listing has none. Never build a `city=` filter from a null code.
- **Hierarchy validation applies only to codes.** A free-text place is found by `q` and
  returned on the item; it is **not** filterable, and it is not in the `city`, `suburb` or
  `region` facets or in suggestions.
- **Listings need a city**, as a code or as free text (`listings.CheckRecord`). The brief said
  region, city and suburb are optional; a listing without any city cannot be found by place, so
  the rule here is stricter. Profiles and applications follow the onboarding rule below.
- **Onboarding** (a business): `country` is required; then `city` (a code) when that country
  has cities in the reference data, otherwise `city_name`. A `city` sent without a `country`
  names its own country, so a client that only knows `city` keeps working. The application's
  market, which is what a `MARKET_ADMIN` is scoped by, is its country. `GET
  /v1/onboarding/requirements` now lists `country` (required), `city` and `city_name` (neither
  flagged required: exactly one is needed) where it listed `city` (required).
- **Directories**: `country` is the profile's own country and accepts any country code;
  it used to be derived from the profile's city.

### One currency per search

Prices are never converted, so a price range only means something inside one currency. The
currency of a search is: `currency` when given, else the currency of the selected `country`,
else the **only** currency the results are priced in.

> **This changed.** A search without a country used to assume **USD** silently: the price facet
> and `min_price` / `max_price` ignored every listing priced in another currency without saying
> so. Now, when no country or currency is chosen and the results span more than one currency:
>
> - `facets.price` is **`null`** and `facets.currencies` is `[{code, count}]`, so the client can
>   ask the user to pick a currency;
> - `min_price` / `max_price` are `400 VALIDATION_FAILED` on `currency`: "Choose a currency or a
>   country to filter by price."
>
> When the results share one currency, or a country or currency is given, nothing changed.
> `facets.currencies` is always present.

A priced search without a currency costs one extra search (without the price filter) to learn
which currencies the results are in.

### Elsewhere

- **Appointments** for a listing or profile are shown in its country's default zone, and
  emails name the zone correctly for any country: by season where the clocks change ("Eastern
  Daylight Time" / "Eastern Standard Time", "British Summer Time" / "Greenwich Mean Time"),
  and for a zone the name table (`internal/platform/mail/zones.go`) does not cover, by its
  offset and IANA name ("UTC+13:00, Pacific/Tongatapu").
- Phone numbers are E.164 everywhere.
- **Sample data**: one listing is in a country without reference places — a short stay in
  Grand Baie, Mauritius, priced in MUR, with a free-text city and region — owned by the
  existing host Sample Falls Stays.
- **Reference data** for the United Kingdom (`?market=GB`): regions `england`, `scotland`,
  `wales`, `northern-ireland`; cities London, Manchester, Birmingham, Leeds, Liverpool, Bristol
  (England), Edinburgh, Glasgow (Scotland), Cardiff (Wales), Belfast (Northern Ireland); areas
  of London (`camden`, `greenwich`, `kensington`, `shoreditch`, `wimbledon`) and one of
  Manchester (`northern-quarter`). Codes stay unique per set.

### Not built: what a country still needs before real trading

Being able to list in a country is not the same as being ready to trade there. The BRD asks
for these per country and **none is built or configured**:

- **legal texts** (terms, privacy notice, consent wording, advertising and agency rules) —
  one privacy version string is recorded today, the same everywhere;
- **payment gateways** and the currencies they settle in (there are no payments at all yet);
- **tax rules** (VAT / GST / sales tax on fees, invoices and receipts);
- required **onboarding documents** per country (today's are placeholders, the same everywhere);
- reference **regions, cities and suburbs** for every country but Zimbabwe and the United
  Kingdom, and an admin endpoint to add them (rows are added in the database today);
- a **per-city time zone** for countries with several zones;
- translated labels (everything is English).

**Not verified:** migration `0014`, the `enabled` column, the Postgres queries that read the
new columns (`city_name`, `region_name`, `country_code`), the per-currency price query and the
Elasticsearch per-currency aggregation have never run against a database or a cluster. They are
tested on the in-memory stores, by query-builder tests and against a fake transport only.

## Short stays

Holiday homes, serviced apartments, guest houses and rooms let by the night are the property
category **`short-stay`** ("Short stays"), next to `residential`, `commercial` and `land`. Its
property types are `holiday-home`, `serviced-apartment`, `guest-house`, `private-room` and
`lodge`.

> **Naming.** "Airbnb" is another company's trade mark, so no category, code, type or label is
> named after it. The word appears once, in the category's *description* ("... including
> Airbnb-style stays"), and free-text search matches a category's label and description. That
> is how `GET /v1/listings?q=airbnb` finds the short stays in all three search adapters.

- **Rules** (`listings.CheckRecord`, and CHECK constraints in migration `0013`): a short stay is
  always deal `rent` with `price_period` `night`; nothing else is priced per night. The price
  periods are reference data (`GET /v1/reference/price_periods`: `month` "per month", `night`
  "per night"; a sale price has none). Today the only thing that writes listings is the sample
  seed, which is validated with the same function; **listing create and publish endpoints still
  do not exist**.
- **Fields**: `max_guests` and `min_nights` on the listing JSON, null for every other category
  (`min_nights` is 1 when a short stay does not say).
- **Search**: `category=short-stay` like any category; `guests_min=n` keeps short stays with
  `max_guests >= n`; `q=airbnb` (see above). No guests facet.
- **Who lists them**: unchanged capability rules (`capabilities.go`): agencies, hosts (the
  `HOST` role and organisation, directory "Hosts and Venues"), property managers, developers,
  law firms and individual owners can own such listings. A listing's `advertiser.agency` is the
  owning organisation's own profile, so for a host it has `kind: hosts`.
- **Availability** — which nights are free, set by the lister and shown to the public — is
  built: see "Availability calendar for short stays". **Not built: booking.** There is no
  booking, no payment and no nightly-rate rules. A short stay is still a listing people
  enquire about, through `POST /v1/property-requests` (now with a "Request to stay" type).

## Availability calendar for short stays

> "Yes the lister must show it's availability for short stay" (the project owner, 2026-11-14)

Whoever lists a short stay (category `short-stay`) says which nights are free, and the public
sees it. **It is not a booking engine: there are no payments, no instant booking and no
holds.** A guest *asks* to stay; the lister answers and marks the nights themselves.

**How it works**

- **Dates are calendar nights in the time zone of the listing's country**, written
  `YYYY-MM-DD`, with no time of day. A stay from `check_in` to `check_out` occupies the nights
  `check_in` .. `check_out - 1`: the day of leaving is not a night.
- **Every night is available unless marked otherwise**, inside a booking window from today
  (in that time zone) to **365 days** ahead. Nights in the past or beyond the window are
  never available. "Today" is the listing's own: at 22:30 UTC it is already tomorrow in
  Harare and still today in London.
- The lister marks runs of nights **unavailable** with a private reason — `BLOCKED` (their
  choice) or `BOOKED` (taken) — and an optional private note (plain text, at most 200
  characters). Ranges `[first_night, last_night]` are stored per listing
  (`listing_availability`); neighbours with the same reason and note are merged; making
  nights available again removes or splits ranges (`availability.Apply`, a pure function).
- `listings.availability_updated_at` records the last change, and every change is one row of
  an audit trail (`listing_availability_audit`: who, which nights, what, when). **No endpoint
  returns the trail yet.**
- **Reasons, notes and who changed what are private to the lister.** The public gets
  `available: true | false` per night and nothing else; a test walks the public responses for
  it.

| Method | Path | Who | What |
| --- | --- | --- | --- |
| GET | `/v1/me/listings?category=&status=&page=&page_size=` | lister | The listings the caller may manage, as summaries. Since 0.26.1 each item has `can_edit` (`auth.MayEditListing`: an organisation's OWNER and MANAGER may edit all its listings, an AGENT only those they are the assigned agent of and sees the others read-only, a direct owner their own); a lease can only be drafted for a listing with `can_edit`. Creating and editing them: see "Listing management" (0.22.0) |
| GET | `/v1/me/listings/{id}/availability?from=&to=` | lister | The calendar with reasons, notes and the stored ranges (default today .. +90 days; at most 366 nights) |
| PUT | `/v1/me/listings/{id}/availability` | lister | `{"changes":[{"from","to","status":"AVAILABLE"\|"BLOCKED"\|"BOOKED","note"?}]}`: 1–50 changes, applied in order, atomically |
| PATCH | `/v1/me/listings/{id}/stay-rules` | lister | `{min_nights?, max_guests?}` (1–365 / 1–50) |
| GET | `/v1/listings/{slug}/availability?from=&to=` | public | `{listing: {slug}, timezone, min_nights, max_guests, from, to, days: [{date, available}], availability_updated_at}`; cacheable like the listing |

- **Who is a lister** (`internal/platform/auth/capabilities.go`: `MayManageListing`,
  `ManagesListings`, `ListingOrganisationIDs`): an `OWNER`, `MANAGER` or `AGENT` of the
  organisation that owns the listing (kinds that manage listings: agency, law firm, property
  manager, developer, host) — for **all** of that organisation's listings, not only the ones
  assigned to them — or the **approved** direct owner (Landlord or Property Seller) of the
  listing. `STAFF` and service providers manage no listings. A listing the caller may not
  manage is `404` (as if it did not exist); a caller who manages no listings at all gets
  `403` on the list.
- **Errors**: `400 VALIDATION_FAILED` with field paths such as `changes[1].to`; `409
  NOT_SHORT_STAY` for a listing of another category; `404` / `410` on the public endpoint
  exactly as for the listing itself. Changes are rate limited per IP (bursts of 30, 30 per
  minute) and follow the CSRF rule for cookie sessions.
- **On the listing**: `availability: {updated_at, next_available} | null` — null for every
  listing that is not a short stay; `updated_at` is null until the lister first changes the
  calendar; `next_available` is the first free night from today on, or null when the window
  has none.
- **Search by stay**: `GET /v1/listings?check_in=2026-12-24&check_out=2026-12-27` (both or
  neither; 1–90 nights; inside the window) keeps the short stays that are free for **every**
  night of the stay and whose `min_nights` it satisfies (add `guests_min` for the guests).
  It implies `category=short-stay`; another category is `400`. The listings that are not free
  are **resolved from the database on every search** and handed to the search adapter as an
  exclusion (`listings.Stay`: listing IDs, plus the countries where the stay is outside the
  window because today differs by time zone). So all three adapters exclude exactly the same
  listings, **Elasticsearch holds no availability data, and a calendar change needs no
  reindex**. Facet counts are computed with the exclusion applied in all three adapters (it
  is part of the main filter, not the post-filter). The one thing to know: the `category`
  facet leaves out its own filter, as every facet does, so it still lists the other
  categories next to `short-stay`.
- **Requests to stay**: `POST /v1/property-requests` with `request_type: "STAY_REQUEST"`
  ("Request to stay", a row of the `request_types` reference set) and `check_in`,
  `check_out`, `guests`. Valid only for a short stay; inside the window, at least
  `min_nights`, at most `max_guests`: otherwise `400`. Every night must be available right
  now: otherwise `409 DATES_UNAVAILABLE`, with the first unavailable night under
  `fields.check_in`. **It is still an enquiry: it does not block the dates and creates no
  appointment.** Two people can ask for the same nights. The inbox item
  (`GET /v1/agent/property-requests`) gains `stay: {check_in, check_out, nights, guests} |
  null`, and the two request emails print "Stay: Saturday 5 December 2026 to Tuesday 8
  December 2026 (3 nights, 2 guests)". The lister then marks the nights `BOOKED`.
- **Sample data** (memory and `cmd/seed`, relative to the moment of seeding): the Victoria
  Falls lodge (22) has a booked week (days +10 to +16) and a blocked weekend (the first
  Saturday and Sunday at least three weeks ahead); the London apartment (21) three booked
  nights (+5 to +7); the Harare guest house (23) the same blocked weekend; the villa in
  Mauritius (24) is free throughout. Sign in as `sample-falls-stays@example.test` to manage
  22 and 24.

**Not built** (availability): payments; instant booking; holds on dates and their expiry (a
request to stay reserves nothing); iCal import / export and calendar sync with other
platforms; seasonal pricing; per-night price overrides; a booking record or a guest's "my
stays"; an endpoint for the audit trail; changing anything else about a listing.

**Not verified:** migration `0015`, the Postgres store (the row lock, the replace-in-one-
transaction, the overlap query), the stay columns of `property_requests`, the `SetStayRules`
transaction and the `cmd/seed` rows have never run against a database, and the Elasticsearch
exclusion (`ids` and `terms` inside `must_not`) only against a fake transport. Everything
above is tested on the in-memory stores and by query-builder tests.

## Listings and directories: the public catalogue

The web app reads listings and directory profiles from the API. Everything here is public,
read-only, the same for every caller, and cacheable (`Cache-Control: public, max-age=30,
stale-while-revalidate=120`, a strong `ETag`, `304` on `If-None-Match`).

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/v1/listings` | Search: `{items, page, page_size, total, total_pages, facets}` |
| GET | `/v1/listings/suggest?q=` | Up to 8 `{type: suburb\|city\|region\|country\|listing, label, value}` |
| GET | `/v1/listings/{slug}` | `{listing}`; `410 LISTING_UNAVAILABLE` or `404 LISTING_NOT_FOUND` |
| GET | `/v1/directory` | `{kinds: [{code, label, singular_label, description, count}]}` |
| GET | `/v1/directory/{kind}` | One page of profiles, sponsored first |
| GET | `/v1/directory/{kind}/{slug}` | `{profile}` with its agents and public listings |
| GET | `/v1/sitemap` | Every public listing slug and profile kind/slug with `updated_at` |

**Since 0.17.0 a listing is also public only while its advertiser is verified** (see "Verified
advertisers"); such a listing answers `404`, not `410`.

**Only PUBLISHED and unexpired listings are ever returned (FR-006).** `status = 'PUBLISHED' AND
expires_at > now` is part of every query that can return a listing. Search sits behind a `Search`
port (`internal/modules/listings`): the Postgres and memory adapters implement every filter,
sort order, facet and suggestion, and an Elasticsearch adapter can be added behind the same port
later. An adapter only proposes IDs; the service re-reads every hit from the database, which
re-checks status and expiry, so an index that is behind cannot bring back an expired listing.

- **Filters** (all optional, combined with AND): `q`, `deal`, `category`, `property_type`, `city`,
  `suburb`, `min_price`, `max_price` (compared only against listings priced in `currency`;
  nothing is converted), `bedrooms_min`, `bathrooms_min`, `features` (repeatable or
  comma-separated; all must be present), `featured`. Codes are reference-data codes; an unknown
  or inactive code is `400 VALIDATION_FAILED` on that parameter.
- **Location: country, region, city, suburb.** One hierarchy, all reference data: a country is a
  country (`GET /v1/countries`; any country), a region is a row of the `regions` set (Zimbabwe's ten provinces),
  a city's `parent_code` is its region and a suburb's `parent_code` is its city. `country=ZW`
  (any case; `market` is an alias) and `region=matabeleland-north` filter listings; a listing's
  region is the region of its city, so nothing is stored twice. The levels must agree — a suburb
  with its city, a city with its region, a region with its country, also when a level in between
  is left out — and a mismatch is `400 VALIDATION_FAILED` on the **narrower** parameter
  (`?region=bulawayo&city=harare` -> `city`). When `country` is given and no `currency` is,
  the price filter and facet use that country's currency; without either, see "One currency
  per search". A listing in a country without reference places has a free-text city, which is
  searchable by `q` but is not a filter: see "Places".
- **Advanced filters**: `bedrooms_max`, `bathrooms_max`, `floor_area_min`, `floor_area_max`,
  `land_area_min`, `land_area_max` (square metres; a listing without the value never matches),
  `listed_within_days` (1–365), `advertiser_type` (`agency` or `private`), `reference` (exact,
  case-insensitive) and `has_photos=true`. A maximum below its minimum is `400` on the `_max`
  parameter.
- **Map** (0.29.0): `bbox=west,south,east,north` and `has_location=true`; the page carries
  `mappable_total`. See "Map search and listing coordinates".
- **Sort**: `newest` (default), `relevance` (default when `q` is given), `price_asc`, `price_desc`,
  `oldest`, `area_desc` (largest floor area first, listings without one last).
  **Paging**: `page` from 1, `page_size` 1–48 (default 12).
- **Facets**: `deal`, `category`, `property_type`, `city`, `suburb`, `bedrooms`, `features`,
  `country`, `region` as `[{code, label, count}]`, `price: {currency, min, max} | null` and
  `currencies: [{code, count}]` (see "One currency per search"). Each facet
  counts the listings that match every *other* filter, so choosing a value does not hide its
  alternatives. Each `suburb` value also carries `city` (its city's code), so a client can group
  suburbs by city.
- **Suggestions** also offer regions (`Matabeleland North, Zimbabwe`) and countries that have at
  least one public listing, after the suburbs and cities and before the listing titles.
- **By slug**: an expired, sold, let, paused or closed listing is `410` (the page can say "no
  longer available"); an unknown slug **and a draft that was never public** are both `404`, so
  drafts are not revealed.
- **A listing** carries id, slug, reference, title, `deal` / `category` / `property_type` as
  `{code, label}`, `city: {code | null, label}`, `suburb` and `region` as
  `{code | null, label} | null` (see "Places") and `country: {code, name}` (the listing's own
  country; `market_code` is kept),
  `price {amount, currency}` (a decimal string, never a
  float), `price_period`, market, bedrooms, bathrooms, floor and land area (m²), summary,
  description, features, featured, photos `{url, alt, width, height}`, the three dates, and
  `advertiser`.
- **`advertiser` is public profile references only**: `{type, name, verified, agency, agent}`
  where `agency` and `agent` are `{kind, slug, name, verified}` (both null for a private seller).
  **No phone number, email address or street address appears in any public response**; a test
  walks every public GET (reference, listings, each listing, directories, each profile, sitemap)
  and fails on any key named like contact data or any value that looks like an email address or a
  phone number.
- **Directories** take `country` (or `market`; any country code) and `region` as well as
  `city`, with the same hierarchy check; a profile's `country` is its own and its `region` is
  the region of its city.
- **Profiles** carry name, role, tagline, description, city, region, country, areas, services, offerings, trade,
  verified, sponsored, since, `agency` (for agents), `listing_count`; the detail adds an agency's
  `agents` and the profile's public `listings`. `logo` is `{url, width, height}` or null (see
  "Logos").
- **Photos.** The sample listings' `photos[].url` is a site-relative path
  (`/assets/images/<file>.jpg`) naming the web app's own sample image files, returned exactly as
  stored. Since 0.22.0 advertisers upload photos (see "Listing management"); an uploaded photo's
  `url` is an API path, and every photo has `variants` (empty for the sample images).

The dev seed (memory store and `cmd/seed`) holds the web's 18 listings and 29 profiles with their
real titles, prices, descriptions, features, photos, slugs and references
(`internal/devseed/data/sample.json`, converted from the web's sample data). Publication dates are
relative to the moment of seeding, so the sample listings are always recent; the one expired
listing (`expired-two-bedroom-flat-avenues`) answers `410`.

## Verified advertisers: what "verified" means here, and what enforces it

The site says that its listings and sellers are verified. This section is what makes that
statement true, and where it stops being true.

**The rule.** A listing is public only while ALL of these hold:

1. its status is `PUBLISHED`;
2. it has not expired;
3. **its advertiser is verified**: the organisation that owns it is `ACTIVE` and has a profile
   of its own (an agency's, a law firm's, a property manager's, a developer's, a host's; never an
   agent's) that is published (`ACTIVE`) and `verified`; or, for a direct owner, the owner's
   account is `ACTIVE` and their `PRIVATE_SELLER` or `LANDLORD` account role is `ACTIVE`
   (approved through onboarding).

An assigned agent's own profile does **not** gate the listing: if the agent is not verified the
listing stays public under its verified agency and the agent is simply not shown as verified.

**Where it is enforced.** In one place per store, which everything public goes through:

| Path | How |
| --- | --- |
| `GET /v1/listings` (database search), facets, `suggest`, the sitemap, profile pages, country counts | PostgreSQL: `publicCondition` in `listings/postgres.go` is status, expiry AND `advertiserVerifiedCondition` (two `EXISTS` sub-queries on the profiles and the account roles). Memory: `Record.Public`, with the advertiser resolved on every read |
| `GET /v1/listings` (Elasticsearch) | The document has one boolean, `advertiser_verified` (never who), and the query excludes the documents that say `false`, so totals and facets are right. **Every hit is then re-read from the database**, so a document that is behind is dropped |
| `GET /v1/listings/{slug}`, `/availability` | `404 LISTING_NOT_FOUND`, never `410`: "no longer available" would reveal that an advertiser lost its verification |
| `POST /v1/property-requests` (requests to stay included), `POST /v1/appointments` | `404 LISTING_NOT_FOUND` (`app.verifiedListings` wraps the request store) |
| `GET /v1/directory/...` | Profiles were already public only when published and verified |

The verification is **never copied**: not onto the listing, and the copy in the search index
is not trusted. It is read with the listing, every time, so an advertiser who stops being
verified disappears from every public endpoint with the next request, and reappears when they
are verified again. Their people keep their inboxes and their own pages (`/v1/me/...`).

**What a listing says.** `advertiser.verified` / `advertiser.display.verified` (always true on
anything returned publicly) and

```json
"verification": {"advertiser_verified": true, "advertiser_verified_at": "2026-11-01T00:00:00Z",
                 "listing_reviewed": true, "listing_reviewed_at": "2026-11-10T08:00:00Z"}
```

`advertiser_verified_at` is `directory_profiles.verified_at` (set by an approval; backfilled
with the profile's creation time by migration 0016 for profiles that were already verified) or,
for a direct owner, the time their role row last changed to `ACTIVE`. In the memory store an
owner's time is the account's creation time.

**The numbers.** `GET /v1/trust/summary` returns `{verified_advertisers: {organisations,
direct_owners}, public_listings, listings_from_verified_advertisers, listings_reviewed,
generated_at}`. `listings_from_verified_advertisers` is counted on its own and equals
`public_listings`; a test asserts it.

### What is NOT true yet: read before writing "all listings are verified"

- **Listings are reviewed since 0.22.0.** A listing created by an advertiser is public only
  after a reviewer approved it, and so are its photos and every later change (see "Listing
  management"); approvals set `listings.reviewed_at` / `reviewed_by` (`listing_reviewed`). The
  sample listings count as reviewed by the seed. Logos are still not reviewed.
- **No administrator endpoint to unverify or re-verify.** Requested and **not started**:
  `POST /v1/admin/organisations/{id}/unverify` / `verify` and
  `POST /v1/admin/users/{id}/roles/{role}/suspend` / `reinstate`, with a required reason, an
  audit line and a history row. Today a profile becomes verified only by an onboarding approval,
  and nothing in the API takes verification away; doing so is a database change
  (`UPDATE directory_profiles SET verified = false ...`), which the rule above then honours at
  once. With Elasticsearch, also touch the listings (`UPDATE listings SET updated_at = now()
  WHERE organisation_id = ...`) and run `cmd/reindex`, otherwise the totals and facets count
  the hidden listings until the next reindex (the listings themselves are never shown: the
  database re-check drops them).
- **An account role has no `SUSPENDED` status.** A direct owner is "not verified" when their
  role is `PENDING_VERIFICATION` or `REJECTED`.
- **Verification is the placeholder process of "Professional onboarding"**: which documents
  prove what, per country, is still an undecided business and legal question.
- **Index events on a verification change** are written in one case only, because it is the only
  change the API can make: when a direct owner's role is approved, their listings are touched
  and a `LISTING_UPDATED` event is written for each, in the approval's transaction (PostgreSQL;
  never run). A newly approved organisation has no listings yet.

## Logos: businesses, agents and the listing card

Every public profile can have a logo; for an agent it is their photo. Listings show it.

**What a client reads.** `Listing.advertiser.display` is the card, already resolved:

```json
"display": {"name": "Sample Estates", "secondary_name": "Tendai Ncube", "role_label": "Real Estate Agency",
            "logo": {"url": "/v1/directory/agencies/sample-estates/logo?v=3f2a9c0d1b7e", "width": 256, "height": 256},
            "verified": true, "profile": {"kind": "agencies", "slug": "sample-estates"}},
"agent_display": {"name": "Tendai Ncube", "role_label": "Estate Agent", "verified": true, "logo": null,
                  "profile": {"kind": "agents", "slug": "tendai-ncube"}}
```

| The listing belongs to | `display.name` | `secondary_name` | `role_label` | `logo` | `profile` |
| --- | --- | --- | --- | --- | --- |
| a business with an assigned agent | the business | the agent | the business's kind | the business's, else the agent's photo, else null | the business's page |
| a business, no agent profile | the business | null | the business's kind | the business's, else null | the business's page |
| a direct owner, for sale | the owner's account name | null | Property Seller (Vendor) | null | null |
| a direct owner, to rent | the owner's account name | null | Landlord | null | null |

`role_label` comes from the reference data (`organisation_kinds` for a business, `account_roles`
for a person), so renaming a row renames it everywhere. `agent_display` is the agent as a person,
with their own verified flag (true only when their profile is verified **and** the business is).
`advertiser.name` and `advertiser.verified` repeat `display`'s for older clients.

**A direct owner's name is now public.** `advertiser.name` used to be the fixed text "Private
seller"; it is now the owner's account name (the sample: "Grace Mhlanga"). **The name is the only
personal data of a direct owner that is public**: never an email address, a phone number or an
address (the leak test walks every public response). A "display name" the owner could choose
instead of their account name is **not built**.

**The URL is relative to the API, on purpose.** `logo.url` is a path such as
`/v1/directory/agencies/sample-estates/logo?v=3f2a9c0d1b7e`. Clients prefix **the API base URL
they already use** (not the web site's origin: the web calls the API on another origin, and an
`<img src>` taken as it is would resolve against the web site). It is not an absolute URL
because the API does not know the public address it is reached at behind a proxy (there is no
`PUBLIC_API_URL` setting, and a wrong one would break every image), and because the catalogue
JSON is cached publicly and must be the same whoever asks. `v` is the first 12 characters of the
image's SHA-256: it changes when the logo changes, so the image can be cached hard.

**Serving.** `GET /v1/directory/{kind}/{slug}/logo` streams the stored image with its stored
content type, an `ETag` (the checksum; `If-None-Match` answers `304`),
`X-Content-Type-Options: nosniff` and `Cross-Origin-Resource-Policy: cross-origin`.
`Cache-Control` depends on `v` (the same on a `200` and a `304`): when `v` is exactly the
version of the **current** logo, which is what every `logo.url` carries, it is `public,
max-age=31536000, immutable`, so a client that uses `logo.url` unchanged fetches each logo once;
without `v`, or with the `v` of another image (an old URL still serves the current logo), it is
`public, max-age=300, stale-while-revalidate=86400`, so a wrong image is never pinned. No session is read and no cookie is set. `404
LOGO_NOT_FOUND` when the profile has none, `404 PROFILE_NOT_FOUND` when the profile is not
public: the logo of an unverified or suspended profile stops being served although it is still
stored.

**Where the image lives.** In the same **private** object storage as the onboarding documents,
under its own random key (`logos/<32 hex digits>`). Nothing makes an object public and no
storage URL is ever returned: the API route is the only way out. With `STORAGE_DRIVER=s3` the
bucket therefore stays private, and every logo request is one object read through the API.

**Uploading.**

- *At onboarding:* an optional document type `LOGO` ("Logo or profile photo") for every role
  whose approval creates a public profile (the organisation roles, and AGENT / SALES_AGENT /
  LETTINGS_AGENT). **It is the one onboarding upload that becomes public**, and the requirements
  endpoint says so in its description so that a client can warn the applicant. On approval it is
  **copied** to a `logos/` object and attached to the new profile, in the approval's
  transaction; the uploaded document itself stays a private document like the others (the
  reviewer sees it in the review detail). If the copy fails the approval still goes through
  without a logo, and the reason is logged.
- *Afterwards:* `GET /v1/me/profiles` lists the profiles the caller may manage;
  `PUT /v1/me/profiles/{kind}/{slug}/logo` (multipart `file`) replaces the logo and `DELETE`
  removes it. Who may is decided in `auth/capabilities.go` (`MayManageProfile`): an OWNER or
  MANAGER of the organisation for a business profile; the agent themselves for an agent profile.
  Another organisation's profile is `404`; a member whose role may not (an AGENT and the agency's
  logo) is `403`. Rate limited with the other uploads (20 per minute per IP); a cookie session
  needs the web app's `Origin`.

**Stored size.** Logos are shown at 32 to 112 CSS pixels, so a large upload is made small once,
when it is stored (`directory.PrepareLogo`, used by both upload paths): an image whose longer
side is more than **512** pixels is scaled down to fit 512x512 (same proportions, Catmull-Rom
from `golang.org/x/image/draw`, never cropped, never enlarged) and encoded again in its own
format: PNG stays PNG, so transparency survives; JPEG stays JPEG at quality 85, turned upright
first when its EXIF orientation says the photo was taken sideways (encoding again drops EXIF).
No side is taken under 128 pixels (the database's CHECK and the lower limit of a logo), so an
image more than four times as wide as it is high keeps a longer side above 512: 1200x200 becomes
768x128, and 4096x128 is stored as it is. An image of 512 pixels or less is stored **byte for
byte** as uploaded. The recorded size, width, height, checksum (hence the `ETag` and `v`) and
content type are those of the **stored** file. The limits below are checked on the upload,
before anything is decoded in full, so at most 2 MB and 4096x4096 pixels are ever decoded. The
onboarding `LOGO` document itself is kept as uploaded; its public copy is scaled on approval.
Logos stored before this was built stay as they are until they are uploaded again.

**Limits** (the same in both places): PNG or JPEG only, decided by sniffing the content and
decoding the image header (which is also where width and height come from); at most **2 MB**;
between **128x128** and **4096x4096** pixels. **SVG is refused** because it can carry scripts;
GIF, WebP, animated PNG, PDF and text named `.png` are refused.

**Sample data.** The seed draws a 256x256 PNG for every sample business with the standard
library: a coloured square with the business's initials in a small bitmap font written for the
purpose (`internal/devseed/logos.go`; no third-party font or artwork). Two gaps are deliberate, so
that clients exercise their fallbacks: **`agencies/sample-realty` has NO logo** (show initials),
and **no sample agent has a photo** (fall back to the agency's logo).

**Not built:** thumbnails per display size and format conversion (one image of at most 512
pixels is served to every card; no WebP / AVIF); cropping; shrinking the logos that were stored
before uploads were scaled; **moderation of logos** (a logo is public the moment its owner
uploads it, with no review); EXIF / metadata stripping for images that are not scaled (a scaled
one is encoded again, which drops its metadata and any colour profile);
a CDN or any cache in front of the API; cleaning up the old object if deleting it fails (it is
logged); malware scanning (as for the documents).

## Listing management: advertisers' listings, photos and review (API 0.22.0)

> "Listings by advertisers: max of 20 photos per property." (the project owner)

Advertisers create and edit their own listings, with up to **20 photos each**, and platform
reviewers approve them before they are public. Code: `internal/modules/listings/manage_*.go`
and `photos.go`; rules in `internal/platform/auth/capabilities.go`; migration `0020`.

| Method | Path | What |
| --- | --- | --- |
| POST | `/v1/me/listings` | Create a `DRAFT` (any editable field, plus `organisation_id` / `as_direct_owner`) |
| GET, PATCH, DELETE | `/v1/me/listings/{id}` | The full editable record; partial update; delete a never-published listing |
| POST | `/v1/me/listings/{id}/submit`, `/archive`, `/relist` | Send for review; withdraw; bring back (renew) |
| POST | `/v1/me/listings/{id}/photos` | Upload one photo (multipart `file`) |
| PUT | `/v1/me/listings/{id}/photos/order` | `{"photo_ids": [...]}`, every photo once |
| PATCH, DELETE | `/v1/me/listings/{id}/photos/{photoId}` | `{"alt": "..."}`; delete (and its files) |
| GET | `/v1/listing-photos/{photoId}/{size}` | The image (`large`, `medium`, `small`) |
| GET | `/v1/admin/listings`, `/v1/admin/listings/{id}` | Review queue (`status`, `kind`, `country`, `q`, paged) and one listing |
| POST | `/v1/admin/listings/{id}/approve`, `/request-changes`, `/reject` | Decide (`{"reason"}` required for the last two) |

### Who may (auth/capabilities.go)

| Caller | Create | See | Change (content, photos, submit, archive, relist, delete) |
| --- | --- | --- | --- |
| OWNER, MANAGER of an agency, law firm, property manager, developer or host | yes, for the organisation (no agent) | every listing of the organisation | every listing of the organisation |
| AGENT of such an organisation | yes, becomes its agent | every listing of the organisation (as `GET /v1/me/listings`) | only the listings they are the agent of (`can_edit: false` on the others, `403` if tried) |
| STAFF | no (`403`) | no (`403` for the organisation's listings) | no |
| Approved landlord or property seller | yes, for themselves | their own | their own |
| Seeker, professional whose verification is pending, service provider | no (`403`) | no (`404`) | no |
| Another organisation | - | `404 LISTING_NOT_FOUND`, the same as an unknown listing | `404` |

Creating follows `MayManageListings`, which is `can_manage_listings` of `GET /v1/me/dashboard`,
so the two always agree. A membership only exists once an onboarding application was approved,
so "verified professional" is enforced by having a membership; the public rule (the
organisation's own profile verified) still decides whether a listing is shown.

**Reviewers:** an admin-scope session with SUPER_ADMIN, PLATFORM_ADMIN or MODERATOR (every
country) or MARKET_ADMIN (its own countries; another country's listing is `404`). The overview's
listings section now uses the same function (`Principal.ListingReviewMarkets`).

### Lifecycle

```
DRAFT ──submit──> PENDING_REVIEW ──approve──> PUBLISHED ──(90 days)──> EXPIRED (at query time)
                       │  └──request changes (reason)──> CHANGES_REQUESTED ──edit, submit──> PENDING_REVIEW
                       └──reject (reason)──> REJECTED (final: delete only)
DRAFT | PENDING_REVIEW | CHANGES_REQUESTED | PUBLISHED | EXPIRED | PAUSED ──archive──> ARCHIVED
ARCHIVED | EXPIRED ──relist──> PENDING_REVIEW (published before; a new 90-day period on approval)
                            └─> DRAFT (never published)
```

- The statuses that existed (`DRAFT`, `PENDING_REVIEW`, `PUBLISHED`, `EXPIRED`, `PAUSED`, `SOLD`,
  `LET`, `CLOSED`) are reused; `CHANGES_REQUESTED`, `REJECTED` and `ARCHIVED` are added to the
  enum. `EXPIRED` stays what it was: a `PUBLISHED` listing past `expires_at` is expired at query
  time (shown as `EXPIRED` to its managers); nothing rewrites the status.
- **Editing a published listing (the design chosen).** The listing stays `PUBLISHED` with its
  approved content. The advertiser's changes go into a **revision** (`listings.revision`,
  `pending_changes` in the JSON: `EDITING` -> submit -> `PENDING_REVIEW` -> approved (written
  onto the listing; dates unchanged), `CHANGES_REQUESTED`, or `REJECTED` (dropped, with the new
  photos and their files)). New photos are `PENDING_REVIEW` and not public until the approval; a
  new description of a public photo waits with the revision. Two things apply at once because
  they publish nothing new: **reordering** photos and **deleting** a photo (the last public
  photo of a published listing cannot be deleted, `409 PHOTO_REQUIRED`). The country of a
  published listing cannot change. Archiving or relisting a published listing writes its
  revision onto it (it is not public then) for the next review.
- **Submit** needs a complete listing (`completeness` in every response): title (5+ characters),
  description (30+), deal, category, property type, a price above 0 and its currency, the rental
  period, the country and the city (a code where the country has reference cities, else free
  text), `max_guests` for a short stay, **at least one photo** (one rather than three: small
  advertisers and land plots must not be blocked) and a description for every photo.
  `422 LISTING_INCOMPLETE` lists the missing fields.
- `DELETE` only for a listing that was never published (any status); otherwise archive it.
- A listing waiting for review, or a rejected one, cannot be changed (`409 LISTING_IN_REVIEW`,
  `409 LISTING_STATE_CONFLICT`). Two writers at the same moment: the second gets
  `409 LISTING_CHANGED` (every write is checked against the `updated_at` it was based on).
- Slug and reference are generated: `RP-<country>-<6 digits>`, slug `<title>-<country>-<digits>`.
  The slug follows the title until the first publication, then never changes.
- Every step is a row of `listing_timeline` (immutable, like `appointment_timeline`) and an
  audit log line; approvals set `reviewed_at` / `reviewed_by`, so `listing_reviewed` is now true
  for real listings too. Public answers: an archived listing that was public, and one waiting
  for review after a relist, are `410`; a listing that was never public stays `404`.

### Validation of the fields

Every save is checked against the reference data (`deal_types`, `property_categories`,
`property_types`, `cities`, `suburbs`, `listing_features`, the countries and their currencies),
the free-text place rules of "Places" and the consistency the database enforces (a short stay is
`rent` per `night`; only a short stay has `max_guests` / `min_nights`; a sale has no period; a
city code and free text never together; a suburb belongs to its city). So that a form can be
saved one field at a time, a field the PATCH does not set itself follows the one it does (a short
stay becomes `rent` / `night`; a new country clears the old country's places; and so on, see the
PATCH operation). Text is plain (no HTML, no control characters); a price has at most two
decimals.

### Photos

- **Limit: 20 per listing.** The 21st upload is `409 PHOTO_LIMIT_REACHED`, "A property can have
  up to 20 photos." It is checked before the image is processed and again, atomically, when the
  photo is added (the Postgres store locks the listing row).
- **Accepted:** JPEG, PNG and **WebP** (decoded with the standard library and
  `golang.org/x/image/webp`), decided by the bytes; SVG, GIF, animated PNG and anything that does
  not decode are `415 UNSUPPORTED_FILE_TYPE`. At most **15 MB** (`413 FILE_TOO_LARGE`). At least
  **800 pixels** on the longer side, at most 10000x10000 and 40 megapixels (checked on the header
  before decoding; `400 VALIDATION_FAILED` on `file`).
- **Stored:** the EXIF orientation of a JPEG is applied, transparency is flattened onto white, and
  three new JPEGs (quality 82) are written: `large` (longer side at most 2048), `medium` (at most
  1280 wide), `small` (at most 640 wide); never enlarged. Encoding again drops every piece of
  metadata (EXIF, GPS, colour profile). Keys `listingphotos/<32 hex>` in the same **private**
  object storage as the documents and logos (Amazon S3 in staging and production); no storage
  URL ever leaves the API. Deleting a photo, a listing or a rejected revision deletes the files
  (a failure is logged, as for logos).
- **Served** by `GET /v1/listing-photos/{photoId}/{size}?v=<12 hex of the checksum>`: to anyone
  while the listing is public and the photo approved (`Cache-Control: public, max-age=31536000,
  immutable` with the current `v`, five minutes without), otherwise only to the listing's
  managers and its reviewers (`private, no-store`) and `404 PHOTO_NOT_FOUND` to everyone else.
  Like logos, the URL is relative to the API origin. An `<img>` on the web's origin does not send
  the API session, so the editor shows a photo that is not public yet by fetching it with
  credentials (or the bearer token) and displaying the blob.
- **Public JSON (compatible).** `Listing.photos[]` keeps `url`, `alt`, `width`, `height` (the
  large size) and gains `variants: [{width, height, url}]`, widest first, for a `srcset`; a
  bundled sample photo (`/assets/images/...`) keeps its URL and has `variants: []`. Only approved
  photos appear; `has_photos` counts them.

### Side effects

- **Emails** through the outbox and the mail templates, to the advertiser only (the listing's
  agent, else the organisation's owners and managers, or the direct owner): `listing-approved`,
  `listing-changes-requested` (with the reason), `listing-rejected` (with the reason), for a new
  listing and for changes to a published one. Events `LISTING_APPROVED`,
  `LISTING_CHANGES_REQUESTED`, `LISTING_REJECTED`; the worker and the development server (in
  memory) handle them.
- **Search index:** `LISTING_PUBLISHED` on approval, `LISTING_UPDATED` when approved changes,
  a reordering or a deleted public photo change what is public, `LISTING_UNPUBLISHED` on archive
  and relist. The index document is unchanged (it already had `has_photos`), so
  **`MappingVersion` stays 5** and no reindex is required by this release; running `/app/reindex`
  after the deploy is harmless and makes `has_photos` count approved photos only for documents
  written before.
- **Dashboards:** the lister's counters always include `listings.status.PUBLISHED`, `.DRAFT`,
  `.PENDING_REVIEW` and `.CHANGES_REQUESTED`, with the next action `UPDATE_LISTINGS_FOR_REVIEW`;
  the admin overview counts `CHANGES_REQUESTED`, `REJECTED` and `ARCHIVED` too. The trust summary
  is unchanged (it counts public listings; `listings_reviewed` now includes real reviews).

### Sample data

Two Sample Estates listings that are not public, so the screens have data (memory store and
`cmd/seed`; the seed stays idempotent: a re-run resets them, and drops pending changes and review
reasons of the sample listings):

| ID | Reference | Status | Agent | Note |
| --- | --- | --- | --- | --- |
| `0195c0de-0000-7000-8000-00000000e101` | `RP-ZW-200101` | `DRAFT` | Tendai Ncube (`agent@example.test`) | Two bundled photos, the second without a description: `completeness` asks for it |
| `0195c0de-0000-7000-8000-00000000e102` | `RP-ZW-200102` | `PENDING_REVIEW` | Chipo Dube (`estates.agent2@example.test`) | In the review queue with one bundled photo |

**Not built** (listing management): assigning or changing the agent of a listing (an owner's or
manager's listing has none; there is no member picker yet); duplicating a listing; pausing,
marking sold or let, and closing (the statuses exist, nothing sets them); presigned direct-to-S3 uploads (photos go through the API, 15 MB each);
cropping, a chosen cover photo other than the first, captions other than `alt`; AVIF / WebP
output; malware scanning of uploads; floor plans (YouTube video tours: 0.28.0); moderation of a reviewer's own
organisation's listings (nothing stops a reviewer from approving a listing of an organisation
they belong to); a per-field change history (the timeline records steps, `changes` shows the
pending diff only); a diff for a resubmission after "changes requested" (only edits of published
listings are diffed); notifying reviewers of new submissions; in-memory development: a listing
created through the API and published cannot take property requests (the in-memory request store
has its own listing table; Postgres reads one table).

**Not verified:** migration `0020` (including `ALTER TYPE ... ADD VALUE` inside the migration's
transaction, which needs PostgreSQL 12 or later), every query of
`internal/modules/listings/manage_postgres.go`, the photo approval filter of the public queries
and the seed changes have never run against a database; uploads have never been written to real
Amazon S3. Everything above is tested on the in-memory stores and the local file storage.

## Search: Elasticsearch behind a port, the database underneath

> **Open decision for the project owner: whether, where and under which licence to run a search
> engine.** Elasticsearch is not in the BRD/FSD stack. It was added on the instruction "filtering
> let is use elastic serach" (2026-11-14), and it has an operating cost: a cluster to host, secure,
> snapshot, upgrade and monitor. The choices are (a) self-hosted Elasticsearch on the project's own
> server (Elastic licence / SSPL / AGPL terms apply, depending on version and features), (b) Elastic
> Cloud (managed, billed), or (c) OpenSearch (Apache 2.0 fork; the queries used here are plain
> query DSL, but the client and `search_as_you_type` would need checking). **None is set up.** Until
> one is, the API searches in PostgreSQL, which is fully supported and needs nothing extra.

`GET /v1/listings` and `GET /v1/listings/suggest` are answered through a `Search` port
(`internal/modules/listings`). There are three adapters: memory (development), PostgreSQL, and
Elasticsearch. **The HTTP contract is the same whichever one answers.**

| Variable | Default | Notes |
| --- | --- | --- |
| `SEARCH_DRIVER` | _(automatic)_ | `database` or `elasticsearch`. Unset: `elasticsearch` when `ELASTICSEARCH_URL` is set, otherwise `database`. `elasticsearch` without a URL **fails startup**; `database` forces the database adapter even when a URL is set |
| `ELASTICSEARCH_URL` | _(empty)_ | Cluster address, without credentials |
| `ELASTICSEARCH_API_KEY` | _(empty)_ | Secret (the base64 `id:api_key` value). Never logged; redacted in every formatting path. Empty only for the local node with security off |
| `SEARCH_INDEX_PREFIX` | `rp365-<APP_ENV>` | Indices are `<prefix>-listings-<timestamp>`, read and written through the alias `<prefix>-listings-current` |
| `SEARCH_TIMEOUT` | `800ms` | One request to Elasticsearch may take this long (50 ms–5 s) before it is abandoned and answered from the database |

The API and the worker each log one line at startup naming the driver:
`search driver selected: database (...)` or `search driver selected: elasticsearch (...)`.

**PostgreSQL stays the source of truth.**

> **After deploying map search (mapping version 7), run `cmd/reindex` once.** Documents now
> carry `location` (the public coordinates). The field was already in the mapping, so old
> documents are still accepted, but until the index is rebuilt they have no `location`: with the
> Elasticsearch driver `bbox` and `has_location=true` find nothing and `mappable_total` is 0.
> `Listing.location` itself is right at once with either driver (it is read from the database
> with every hit). The database driver needs only migration `0028`.

> **After deploying YouTube video tours (mapping version 6), run `cmd/reindex` once.** Documents
> now carry the boolean `has_video`. The mapping is strict, so until the index is rebuilt the old
> index **refuses every new or changed document** (the worker's index events fail and are
> retried). Searches keep working meanwhile, and `has_video` on results is right at once with
> either driver: it is read from the database with every hit, not from the index. The database
> driver needs only migration `0027`.

> **After deploying verified advertisers (mapping version 5), run `cmd/reindex` once.** Documents
> now carry `advertiser_verified` (one boolean) and every search excludes the documents that say
> `false`. The mapping is strict, so until the index is rebuilt the old index **refuses every
> new or changed document** (the worker's index events fail and are retried). Searches keep
> working in the meantime: an old document has no such field, is not excluded, and is re-checked
> against the database like every hit.
>
> **After deploying the global countries (mapping version 4), run `cmd/reindex` once.**
> Documents gained `city_name` and `region_name` (the free-text city and region of a listing in
> a country without reference places) and `city` / `suburb` may now be absent. The mapping is
> strict: until the index is rebuilt, a listing with a free-text city is refused by the old
> index (its worker event fails and is retried) and so is missing from Elasticsearch results,
> and `q` does not match free-text places there. Listings that have a city code are unaffected.
> The price facet is now aggregated **per currency** (a `terms` aggregation on `currency` with
> `min` / `max` inside); that needs no mapping change. The database driver needs only migration
> `0014`.

- Elasticsearch only proposes listing IDs, a total and facet counts. **Every hit is re-read from
  the database**, which checks `status = 'PUBLISHED' AND expires_at > now` again, before anything
  is returned. An index that is behind cannot bring back an expired or unpublished listing.
- The Elasticsearch query itself also filters `status = PUBLISHED` and `expires_at > now`, so an
  expired listing drops out of hits, totals and facets the moment it expires, without waiting for
  the index to be updated.
- **Trade-off, stated plainly:** a listing that was *unpublished* (paused, sold, closed) stays in
  the index until the worker has processed its event. During that window it is never returned
  (the database re-check removes it and takes it off `total`), but it can still be counted in the
  **facet counts** and in `total` on pages where it is not among the hits. The window is the
  outbox delay, normally seconds.
- **Fallback.** If Elasticsearch is unreachable, slow (over `SEARCH_TIMEOUT`), returns an error, an
  unreadable answer or partial results (`timed_out` or a failed shard), that request is answered by the database adapter instead. Search does not
  fail. Each fallback is logged as a warning (`search engine failed; answering from the database
  instead`) with a running count `search_fallbacks_total`; there is no metrics endpoint yet. The
  client does not retry: the database is the retry.

> **After deploying the short stays (mapping version 3), run `cmd/reindex` once.** Documents now
> carry `max_guests`, `min_nights` and `keywords` (the category's label and description, which is
> what `q=airbnb` matches). The mapping is strict: until the index is rebuilt the worker's
> listing events are refused by the old index (and retried), `guests_min` matches nothing in
> Elasticsearch and `q=airbnb` finds nothing there. The database driver needs only migration
> `0013`.

> **After deploying the country / region / advanced filters, run `go run ./cmd/reindex` once.**
> The mapping changed (mapping version 2, recorded in the mapping's `_meta` and in
> `listings.MappingVersion`): documents now carry `region`, `advertiser_type` and `has_photos`.
> The mapping is strict, so the old index refuses the new documents (the worker's listing events
> fail and are retried) and knows nothing of the new fields (a search that uses them finds
> nothing in Elasticsearch) until the index has been rebuilt. Searches keep working during the
> rebuild. With the database driver nothing needs doing beyond migration `0012`.

**The index.** One index per environment behind an alias, with an explicit, strict mapping
(`internal/modules/listings/es_mapping.json`): `keyword` for codes (deal, category, property type,
city, suburb, region, features, market — the country —, currency, advertiser type), the `boolean`s
`has_photos` and `has_video`, `text` with a `keyword` and a `search_as_you_type`
sub-field for title, suburb and city labels, `scaled_float` for the price with the currency kept
separately, integers for bedrooms and bathrooms, dates, a `version`, and a `geo_point` `location`
(since 0.29.0: the public coordinates, rounded for an APPROXIMATE listing; absent without any). **A document holds public listing data only** — no customer, no contact
detail, no owner account, and nothing that says who the advertiser is (only `advertiser_type`:
`agency` or `private`, and the boolean `advertiser_verified`); a test checks every field name and that every field is in the mapping.
A document's `region` is read from the reference data when it is built, so **moving a city to
another region needs a reindex** for Elasticsearch to follow (the database adapter follows at
once).

**Keeping it in step.** Listing changes write an outbox event (`LISTING_PUBLISHED`,
`LISTING_UPDATED`, `LISTING_EXPIRED`, `LISTING_UNPUBLISHED`) carrying only the listing ID. The
worker reads the listing's *current* state from the database and writes the document if it is
public, or deletes it if it is not. That makes the handler idempotent and independent of event
order; writes and deletes also use external versioning (`updated_at` in microseconds), so
Elasticsearch refuses an older write (HTTP 409, which the handler treats as success, not as a
failure to retry). Other failures are retried with the outbox backoff and
dead-lettered like any other event. With the database driver the worker acknowledges these events
and does nothing.

> Listing management (0.22.0) writes these events in the same transaction as each change, and
> the hourly expiry sweep (0.45.0) writes `LISTING_EXPIRED` when it stores a listing `EXPIRED`.
> Expiry needs no event to be *correct* (the query and the re-check both use `expires_at`), only
> to remove the stale document. Pausing does not exist yet.

**Rebuilding.** `go run ./cmd/reindex` (or `make reindex`) creates a new index from the database,
bulk-indexes every public listing, refreshes it, moves the alias to it in **one atomic action**,
and only then deletes the index the alias used to point to. Searches keep working throughout, and
a failure before the swap leaves the alias untouched and removes the half-built index. The aliases API can answer 200 with `"errors": true` when one action
failed; the command treats that as a failure, deletes nothing the alias might still use, and asks
for the alias to be checked by hand. It refuses
to run without `DATABASE_URL` and `ELASTICSEARCH_URL`. Run it once after creating a cluster and
after any mapping change.

**Query.** Free text over title, suburb, city, property type (and summary, description,
reference) with typo tolerance (`fuzziness: AUTO`, first letter fixed, every word must match);
the same filters, sort orders and paging as the database; facets through filtered aggregations
with `post_filter`, so each facet ignores its own filter exactly as the database adapter does.
`from + size` is capped below Elasticsearch's 10,000-result window. Suggestions use the
`search_as_you_type` fields.

**Where the adapters can differ** (the response shape never does):

| Behaviour | Database / memory | Elasticsearch |
| --- | --- | --- |
| Which listings match `q` | Exact substring of each word (`bor` matches Borrowdale) | Whole analysed words, with typos tolerated (`borowdale` matches; `bor` alone does not) |
| Order under `relevance` | Fixed weights: title 4, place 3, type 2, rest 1 | Elasticsearch's BM25 score with those field boosts |
| Words split on punctuation | No (`RP-ZW-10003` is one word) | Yes (analysed into `rp`, `zw`, `10003`, all required) |
| Suggestions | Substring anywhere in the name; prefix matches first | Word-prefix matches from `search_as_you_type`; order by document count |
| Price facet and price filter | Exact decimals | `scaled_float` with two decimals (rounded to the cent) |
| Facet lists | Every value | Top N values per facet (50–1000 by facet) |
| `region` after a city is moved to another region | Immediate (joined at query time) | After the next reindex (stored on the document) |
| `reference` filter | `upper(reference) = upper(?)` | `term` on `reference.keyword` with `case_insensitive` |
| Freshness | Immediate | Behind by the outbox delay, plus Elasticsearch's refresh interval (1 s by default) |

**Target and what is not measured.** The specification's target is a p95 of one second for
search; the 800 ms timeout is set with that in mind. **Nothing has been measured.**

**Local.** `docker compose up -d elasticsearch` starts a single node with security **off**, bound
to this machine only (development only; never run it like this elsewhere). Then set
`ELASTICSEARCH_URL=http://localhost:9200` and run `go run ./cmd/reindex`.

**Not verified:** there is no Elasticsearch, Docker or PostgreSQL on the machine this was written
on. The Elasticsearch adapter, the indexing handler and `cmd/reindex` are tested **only against a
fake transport**; they have never run against a real cluster. The official Go client
(`github.com/elastic/go-elasticsearch/v9`, the current stable major, used for its transport only)
is likewise only exercised against a fake.
The country, region and advanced filters are in the same position: they run and are tested on
the memory adapter; the database adapter's SQL (the joins to `markets` and to the city's region,
the new conditions and sort orders) and the Elasticsearch query, aggregations and documents are
checked by query-builder and golden-file tests only. Migration `0012` has never run.

**Operations: not set up, no owner.** Snapshots and restore testing, upgrades (the client major
must match the cluster major), monitoring (cluster health, disk watermarks, slow queries), index
lifecycle, authentication and TLS for a real cluster, and capacity planning.

## Roles: real estate terminology

The role names follow the functional specification's real estate terminology. **Role codes did
not change** (the web and the mobile app depend on them); labels, descriptions and groups did, and
four roles were added. All of it is reference data (`GET /v1/reference/account_roles`).

| Group | Code | Label | Verification | Approval creates |
| --- | --- | --- | --- | --- |
| Property Seekers | `CONSUMER` | Prospective Buyer | no | (no onboarding) |
| | `TENANT` | Prospective Tenant | no | (no onboarding) |
| Real Estate Professionals | `AGENT` | Estate Agent | yes | AGENT membership in a verified agency + `agents` profile |
| | `SALES_AGENT` (new) | Sales Agent | yes | as AGENT, profile `specialism` SALES |
| | `LETTINGS_AGENT` (new) | Lettings Agent | yes | as AGENT, profile `specialism` LETTINGS |
| | `PROPERTY_MANAGER` (new) | Property Manager | yes | organisation `PROPERTY_MANAGER` + OWNER + `property-managers` profile |
| | `SOLICITOR` (new) | Solicitor | yes | organisation `LAW_FIRM` + OWNER + `solicitors` profile |
| Real Estate Businesses | `AGENCY_OWNER` | Agency Principal | yes | organisation `AGENCY` + OWNER + `agencies` profile |
| | `AGENCY_ADMIN` | Agency Administrator | yes | MANAGER membership in a verified agency |
| | `AGENCY_STAFF` | Agency Staff | yes | STAFF membership in a verified agency |
| Property Owners | `LANDLORD` | Landlord | yes | nothing: an individual owner; the role becomes ACTIVE |
| | `PRIVATE_SELLER` | Property Seller (Vendor) | yes | nothing: an individual owner; the role becomes ACTIVE |
| Other property businesses | `HOST` | Host | yes | organisation `HOST` + OWNER + `hosts` profile |
| | `DEVELOPER` | Property Developer | yes | organisation `DEVELOPER` + OWNER + `developers` profile |
| | `SERVICE_PROVIDER` | Property Service Provider | yes | organisation `SERVICE_PROVIDER` + OWNER + `service-providers` profile |

Labels are written as titles, exactly as the specification and the mobile app write them.

**A property seeker account is only that.** A registration chooses Property Seekers roles only
(`CONSUMER`, `TENANT` or both) **or** roles from the other four groups only; a seeker role
together with any other role is refused (see "Roles at registration" under "Authentication and
sessions").

| Organisation kind (`GET /v1/reference/organisation_kinds`) | Label |
| --- | --- |
| `AGENCY` | Real Estate Agency |
| `LAW_FIRM` | Law Firm |
| `PROPERTY_MANAGER` | Property Manager |
| `HOST` | Host |
| `DEVELOPER` | Property Developer |
| `SERVICE_PROVIDER` | Property Service Provider |

| Directory (`GET /v1/reference/directory_kinds`) | Label | Singular |
| --- | --- | --- |
| `agencies` | Real Estate Agencies | Real Estate Agency |
| `agents` | Estate Agents | Estate Agent |
| `developers` | Property Developers | Property Developer |
| `property-managers` | Property Managers | Property Manager |
| `solicitors` (new) | Solicitors | Solicitor |
| `hosts` | Hosts and Venues | Host |
| `service-providers` | Property Services | Property Service Provider |

Each membership in `GET /v1/me` has a `role_label`: in an agency OWNER is "Agency Principal",
MANAGER "Agency Administrator", AGENT "Estate Agent", STAFF "Agency Staff"; in any other
organisation "Principal", "Manager", "Estate Agent", "Staff". `role` itself is unchanged.

- **Awaiting the project owner's decision:** the group "Other property businesses" (HOST,
  DEVELOPER, SERVICE_PROVIDER) is not in the functional specification. It is kept as it was.
- **SOLICITOR** is placed with the professionals, as asked, although approving it creates a firm
  (an organisation of kind `LAW_FIRM`), like the business roles. The role code is `SOLICITOR`, the
  organisation kind is `LAW_FIRM`, the directory is `solicitors`.
- A person may hold several roles, for example Landlord and Property seller.
- Platform roles are never in this set. Where the backend shows one it says "Super
  Administrator", "Platform Administrator", "Market Administrator", "Finance Administrator",
  "Content Administrator", "Moderator" or "Support".
- **Landlords and property sellers are individual owners.** Either may open
  `GET /v1/agent/property-requests`; they see only requests for listings they own directly.
- **What changed for LANDLORD:** it used to create a business in "property-managers". Those
  businesses are now `PROPERTY_MANAGER` organisations and their owners hold the `PROPERTY_MANAGER`
  role (migration 0010 moves existing rows; the sample data was moved the same way).
- **Onboarding status wording:** every onboarding `status` comes with a `status_label`: DRAFT =
  "Pending Verification", SUBMITTED = "Under Review", CHANGES_REQUESTED = "Changes Requested",
  APPROVED = "Approved", REJECTED = "Rejected" (title case, as the mobile app writes them). The specification also has "Suspended";
  suspending is **not built**.

### Solicitors

A solicitor's firm is an organisation of kind `LAW_FIRM` ("Law Firm") with a verified profile in the public
`solicitors` directory. It receives enquiries through `POST /v1/business-enquiries` (recipient
kind `solicitors`) and reads them at `GET /v1/inbox/business-enquiries`, which is what the mobile
app uses. Required documents (placeholders, like the rest): PROOF_OF_IDENTITY,
BUSINESS_REGISTRATION, PROOF_OF_ADDRESS and PRACTISING_CERTIFICATE (new: "Current practising
certificate from the Law Society"); TAX_CLEARANCE is optional.

**Listing, appointment and booking rights.** One place in the code answers "may this caller
manage listings or appointments": `internal/platform/auth/capabilities.go`.

| Who | Listings | Appointments |
| --- | --- | --- |
| OWNER, MANAGER, AGENT of an `AGENCY`, `LAW_FIRM`, `PROPERTY_MANAGER`, `DEVELOPER` or `HOST` organisation | yes | yes |
| STAFF of those organisations | no | yes |
| Any member of a `SERVICE_PROVIDER` organisation | no | enquiries and consultations only |
| An **approved** (ACTIVE) Landlord or Property Seller | their own listings | for their own listings |

So a verified solicitor in a law firm has the same rights as an agent, and a `LAW_FIRM` can own
listings on the same terms as an agency: property requests for such a listing reach the firm's
inbox (tested). Appointments (viewings and consultations) are built on these rules: see
"Appointments"; listings are created, edited and sent for review through `/v1/me/listings` (see
"Listing management"). **There are no venue or short-stay bookings, for any role yet.**

### Specified but not built

So that nobody assumes these exist. They are in the updated functional specification and are
**not** in this backend:

- replies to enquiries and a customer notification when a request moves (the eight lead
  stages, New enquiry to Closed, are built: see "Dashboards and working a request");
- follow-up dates on a lead (assigning a request to a member exists since 0.23.0:
  `POST /v1/agent/property-requests/{id}/assign`, see "Agency teams");
- reassigning a routing exception to a recipient (administrators can list and resolve them);
- rental applications;
- appointments beyond viewings and consultations (see "Appointments": venue and short-stay
  bookings, slot configuration, ICS invites, calendar sync, reminders);
- short-stay **bookings**: payments, instant booking, holds, iCal sync, seasonal and per-night
  pricing (the availability calendar itself is built: see "Availability calendar for short
  stays");
- assigning an agent to a listing, duplicating, pausing and marking a listing sold or let
  (creating, editing, photos, the review workflow, package limits and the expiry job with its
  reminders are built: see "Listing management" and "Listing expiry and reminders");
- administrator endpoints to unverify / re-verify an organisation and to suspend / reinstate a
  direct owner's role (requested, not started);
- a display name a direct owner can choose (their account name is shown on their listings);
- per-size thumbnails, cropping and moderation of logos, and a CDN (see "Logos");
- receipts for subscription payments (subscription plans, invoices and admin-recorded payments
  exist since 0.25.0; online payment through a provider-neutral gateway since 0.42.0, with only the
  `fake` driver: **a real payment provider is not chosen or built**);
- per-country **legal texts, payment gateways and tax rules** (see "Countries": any country can
  be listed in, but none of these is configured for any country);
- a per-city time zone for countries with several zones;
- adaptive bot protection (registration has a simple built-in arithmetic captcha, see
  "Registration captcha"; nothing stronger), and duplicate or spam detection;
- hardware security keys (WebAuthn) for administrators (an authenticator app with recovery codes
  exists since 0.40.0, see "Administrator MFA"); MFA for customers;
- a "Suspended" onboarding status (applications are `DRAFT`, `SUBMITTED`, `CHANGES_REQUESTED`,
  `APPROVED` or `REJECTED`; account suspension itself exists since 0.25.0, see "Administrators'
  area") and abuse reporting;
- push notifications.

**Conflict with the specification, kept as the user decided:** the specification's technology
table says "Neon PostgreSQL". The user decided on 2026-11-14 to use **self-hosted PostgreSQL**;
this repository follows that decision.

## Professional onboarding and verification

A professional role chosen at sign-up is only a request. Each such role gets an **onboarding
application**: the person fills in details, attaches documents and submits; a platform
administrator approves, rejects or asks for changes. **Only an approval** makes the role ACTIVE
and creates the organisation, membership and public profile that go with it. CONSUMER and TENANT
have no onboarding. A platform role can never be obtained this way: platform roles have no outcome
row, the tables refuse them, and the service ignores one even if it were inserted.

```
registration ──> DRAFT ──submit──> SUBMITTED ──approve──────────> APPROVED   (role ACTIVE, things created)
                   ^                  │  ├──────reject──────────> REJECTED   (role REJECTED; final)
                   │                  │  └──request-changes──> CHANGES_REQUESTED ──submit──> SUBMITTED
                   └── PUT after a rejection starts a NEW application (the rejected one is kept)
```

Every transition is one row in `onboarding_history` (actor, from, to, reason, time; a trigger
refuses updates) and one `audit=true` log line. An approval is **one transaction** in Postgres:
application, account role, organisation, membership, profile, history row and outbox event commit
together.

**What an approval creates** is a table (`onboarding_role_outcomes`), not code:

| Role | Outcome | Created on approval |
| --- | --- | --- |
| `AGENT`, `SALES_AGENT`, `LETTINGS_AGENT` | AGENCY_MEMBER | AGENT membership in the chosen **existing verified agency** + profile in `agents`, linked to it (specialism: none, SALES, LETTINGS) |
| `PROPERTY_MANAGER` | ORGANISATION | organisation `PROPERTY_MANAGER` + OWNER + profile in `property-managers` |
| `SOLICITOR` | ORGANISATION | organisation `LAW_FIRM` + OWNER + profile in `solicitors` |
| `AGENCY_OWNER` | ORGANISATION | organisation `AGENCY` + OWNER membership + profile in `agencies` |
| `AGENCY_ADMIN` | AGENCY_MEMBER | MANAGER membership in the chosen agency; no profile |
| `AGENCY_STAFF` | AGENCY_MEMBER | STAFF membership in the chosen agency; no profile |
| `LANDLORD`, `PRIVATE_SELLER` | NONE | nothing; the role becomes ACTIVE |
| `HOST` | ORGANISATION | organisation `HOST` + OWNER + profile in `hosts` |
| `DEVELOPER` | ORGANISATION | organisation `DEVELOPER` + OWNER + profile in `developers` |
| `SERVICE_PROVIDER` | ORGANISATION | organisation `SERVICE_PROVIDER` + OWNER + profile in `service-providers` (with its trade) |

A created profile is published and `verified = true`, with a slug derived from the name (`-2`,
`-3`, ... on a collision). **Public directory endpoints and the sitemap only ever return profiles
that are published AND verified**; the 32 sample profiles are all verified. A profile gets public
content only: the contact phone, the registration number and the documents never leave the
application (the public leak test walks an approved profile).

**Details** (validated as they are saved; required ones checked on submit):

| Outcome | Fields (required in bold) |
| --- | --- |
| ORGANISATION | **`business_name`** (2–120), `tagline` (≤140), **`description`** (≤2000, plain text), **`city`** (`cities` code), `areas` (0–12), `services` (0–12), **`trade`** (`service_trades` code; SERVICE_PROVIDER only), `registration_number`, **`contact_phone`** (E.164) |
| AGENCY_MEMBER | **`agency_slug`** (an existing verified agency), `job_title`, `bio` (AGENT only, ≤1000), `areas`, **`contact_phone`** |
| NONE | **`contact_phone`** |

`contact_phone` and `registration_number` are **private**: the applicant and authorised
administrators see them, nobody else, and nothing copies them to a profile.

### Documents each role must attach: PLACEHOLDERS, need confirmation

> **Open decision for the project owner.** The table below is a placeholder so the flow can be
> built and tried. Which documents each market really requires is a **business and legal
> decision** (regulator rules, data-protection law, what the Estate Agents Council issues) that
> has not been taken. The rows live in the database (`onboarding_role_documents` and the
> `onboarding_documents` reference set), so they can be changed without a release.

| Role | Required | Optional |
| --- | --- | --- |
| `AGENCY_OWNER` | PROOF_OF_IDENTITY, BUSINESS_REGISTRATION, PROOF_OF_ADDRESS, ESTATE_AGENTS_COUNCIL_CERTIFICATE | TAX_CLEARANCE |
| `PROPERTY_MANAGER`, `HOST`, `DEVELOPER` | PROOF_OF_IDENTITY, BUSINESS_REGISTRATION, PROOF_OF_ADDRESS | TAX_CLEARANCE |
| `SOLICITOR` | PROOF_OF_IDENTITY, BUSINESS_REGISTRATION, PROOF_OF_ADDRESS, PRACTISING_CERTIFICATE | TAX_CLEARANCE |
| `SERVICE_PROVIDER` | PROOF_OF_IDENTITY, BUSINESS_REGISTRATION, PROOF_OF_ADDRESS | TAX_CLEARANCE, TRADE_LICENCE |
| `AGENT`, `SALES_AGENT`, `LETTINGS_AGENT` | PROOF_OF_IDENTITY, ESTATE_AGENTS_COUNCIL_CERTIFICATE | |
| `AGENCY_STAFF`, `AGENCY_ADMIN` | PROOF_OF_IDENTITY, PROOF_OF_EMPLOYMENT | |
| `LANDLORD`, `PRIVATE_SELLER` | PROOF_OF_IDENTITY, PROOF_OF_OWNERSHIP | |

### File storage

Profile logos live in the same storage and are public **through the API only** (see "Logos");
everything below is about the onboarding documents.

Documents are **private objects**. There is no public URL for one: the only way to read a document
is an authenticated API route (the owner's, or an authorised administrator's, whose every access
is audit-logged) that streams it with `Content-Disposition: attachment`,
`X-Content-Type-Options: nosniff` and `Cache-Control: private, no-store`.

| Variable | Default | Notes |
| --- | --- | --- |
| `STORAGE_DRIVER` | `local` in development | `local` or `s3`. **Staging and production must use `s3`: startup fails otherwise** |
| `STORAGE_LOCAL_DIR` | `tmp/uploads` | `local` only. Git-ignored. Lost with the machine; not shared between replicas |
| `S3_ENDPOINT` | _(empty)_ | `s3`: http(s) URL of the S3-compatible store (MinIO), without credentials |
| `S3_BUCKET` | _(empty)_ | `s3`: an existing **private** bucket |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | _(empty)_ | `s3`. The secret key is a redacting type: never logged, never formatted |
| `S3_REGION` | `us-east-1` | `s3` |
| `S3_USE_PATH_STYLE` | `true` | `s3`: `<endpoint>/<bucket>/<key>`, which MinIO needs |

The API logs one line at startup naming the storage driver. The S3 adapter uses
`github.com/minio/minio-go/v7`: a small client written for MinIO, which is what the stack runs, in
place of the much larger AWS SDK. The automated tests use a fake HTTP transport; put, get and
delete have also been run against MinIO and Amazon S3.

**Uploads go through the API, not presigned URLs. This is a deliberate deviation from FR-010**
(presigned PUT with asynchronous scanning), so that validation is enforced server-side today:

- one `multipart/form-data` request with `type` and `file`; the route alone has a larger body
  limit (10 MB + framing) and a longer timeout (2 minutes); uploads are rate limited per IP;
- at most **10 MB** per file and **10 documents** per application; an empty file is refused;
- only **PDF, JPEG and PNG**, decided by **sniffing the content**. The client's `Content-Type` and
  the file extension are ignored: a PNG named `bill.pdf` is accepted **as a PNG** and shown as
  `bill.png`; text, HTML, SVG, archives and executables are refused whatever they are called;
- the display name is sanitised and capped at 100 characters, and is never part of a storage key;
- object keys are random (`onboarding/<32 hex digits>`) and contain no user input;
- the SHA-256 of the content is stored;
- attaching a type again replaces the document and deletes the old object.

**Malware scanning** (API 0.41.0, when `MALWARE_SCANNER=clamav`): a document is quarantined until
the worker has scanned it and cannot be downloaded meanwhile; see "Uploads and malware scanning".
With scanning off a file is stored and can be downloaded unscanned: **open it with care**.

**Not built:** image re-encoding and EXIF/metadata stripping,
presigned direct uploads, a retention and deletion policy (documents are kept indefinitely,
including after a rejection), cleanup of orphan objects if a delete fails (it is logged), and
encryption settings for the bucket (an infrastructure decision).

### Who may review

`GET /v1/admin/onboarding...` needs an **admin-scope** session and one of SUPER_ADMIN,
PLATFORM_ADMIN, MODERATOR or MARKET_ADMIN; anyone else gets 403. A MARKET_ADMIN only sees and
decides applications of its own market (the market of the application's city, or of the agency
being joined); for another market's application it gets 404. Applications that belong to no
market (a draft without a city, a private seller) are for the global roles only.

### Emails

Four emails, to the applicant only, through the outbox: application received, approved (says what
they can now do and links to their profile, or to `/account`), changes requested (with the reason,
links to `/onboarding`) and not approved (with the reason). They never contain document names or
private details. **Administrators are not emailed** about new submissions yet; they see the queue.

### Not built yet (onboarding)

- **Suspending or unpublishing a profile** from the API: not built. A profile is hidden by setting
  its `status` or `verified` in the database.
- **Editing a profile after approval**, adding a logo, or changing the organisation: not built.
- **Revoking an approved role**, removing a membership, or approving on behalf of an agency (the
  agency itself does not confirm a new member; a platform administrator does).
- **Admin email / notifications** for new submissions; an SLA or reminder for the queue.
- **Admin endpoints for the requirement tables**: they are changed in the database.
- **A durable audit trail**: decisions and document accesses are log lines (plus the history table).

## Reference data: drop-downs and lists come from the database

Nothing that fills a drop-down, a filter or a list is hard-coded in the clients. It is all rows in
`reference_items` (migration `0007`), served by two public endpoints:

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/v1/reference` | `{"market": null, "sets": {"deal_types": [...], ...}}` — the active rows of every set |
| GET | `/v1/reference/{set}` | `{"set": "cities", "market": "ZW", "items": [...]}` — one set; unknown set → `404 REFERENCE_SET_NOT_FOUND` |

Optional `?market=ZW` keeps that market's rows plus the rows that apply everywhere (unknown market
→ `400`, field `market`). Both responses are public and the same for every caller:
`Cache-Control: public, max-age=60, stale-while-revalidate=300`, a strong `ETag`, and
`304 Not Modified` on `If-None-Match`.

| Set | Baseline rows | Notes |
| --- | --- | --- |
| `deal_types` | 2 | `sale`, `rent` (the web's URL values) |
| `property_categories` | 4 | `residential`, `commercial`, `land`, `short-stay` (with a description) |
| `property_types` | 12 | `parent_code` is the category; five are short-stay types |
| `price_periods` | 2 | `month` "per month", `night` "per night" |
| `request_types` | 5 | the `request_type` of a property request; `STAY_REQUEST` ("Request to stay") is for short stays only |
| `contact_methods` | 3 | `EMAIL`, `PHONE`, `WHATSAPP` |
| `account_roles` | 11 | with `group` (code, label, sort) and `requires_verification`; **never** a platform role |
| `directory_kinds` | 6 | with `singular_label` and `description` |
| `service_trades` | 7 | service-provider trades |
| `regions` | 14 | `ZW`: Zimbabwe's ten provinces (codes are slugs, e.g. `matabeleland-north`); `GB`: `england`, `scotland`, `wales`, `northern-ireland`. Every other country has none yet |
| `cities` | 14 | `ZW`: 4; `GB`: 10. `parent_code` is the region (Harare -> `harare`, Bulawayo -> `bulawayo`, Mutare -> `manicaland`, Victoria Falls -> `matabeleland-north`; London -> `england`, Edinburgh -> `scotland`, ...) |
| `suburbs` | 24 | `ZW`: 18; `GB`: 6. `parent_code` is the city |
| `listing_features` | 59 | features and amenities |

Each row has a stable `code` (what clients send and tables store), a `label` (what people read),
`sort_order` and an internal `active` flag; only active rows are returned. Countries are not a set:
they have `GET /v1/countries`. The baseline is exactly what the web app used to hard-code, defined
once in `internal/devseed/reference.go`, inserted by migration `0007` and upserted by `cmd/seed`
(a test keeps the migration and the Go data in step).

**Validation uses the same rows.** Registration roles, request types, contact methods and directory
kinds are checked against the active rows on every request (nothing is cached in the API), so a
row that an administrator deactivates disappears from the endpoint and is refused with
`400 VALIDATION_FAILED` at the same moment. The fixed lists that used to be `CHECK` constraints on
`property_requests`, `business_enquiries`, `directory_profiles` and `user_account_roles` were
dropped by migration `0007` for that reason.

**Platform roles stay impossible at sign-up.** The seven platform roles are refused by a hard
deny-list in the code *before* the reference data is read, so inserting `SUPER_ADMIN` into
`account_roles` would not make it selectable (there is a test for exactly that). The table and
`user_account_roles` also carry a `CHECK` that rejects those seven codes.

What stays in code, because it is behaviour rather than a list: a phone number is required for
`PHONE` and `WHATSAPP`; a preferred slot is only allowed for `VIEWING_REQUEST` and
`CONSULTATION_REQUEST`. A new contact method or request type added as a row is accepted, but gets
none of those rules.

**Not built:** admin endpoints to create, edit, reorder or deactivate reference rows. Today that
is done directly in the database.

## Email

All email goes through [Resend](https://resend.com) (`POST https://api.resend.com/emails`, plain
`net/http`, no SDK), rendered from Go templates in `internal/platform/mail/templates`.

> **A local run can send real email.** The binaries read `.env`. If `RESEND_API_KEY` is set there,
> the API and the worker select Resend and every trigger (register, password reset, a property
> request, an enquiry, an administrator sign-in) sends a real message through that account. To be
> safe, force the log mailer: `MAIL_DRIVER=log go run ./cmd/api`. The first lines of the log say
> which mailer is in use — `mailer selected: log ...` or `mailer selected: resend ...` — check
> that line before triggering anything.

### Prerequisites (your steps, not done by this repository)

1. **Verify the sending domain in Resend.** Resend only sends from a domain whose DNS records
   (SPF, DKIM, and ideally DMARC) are verified in the Resend account. The default sender is
   `RightProperty365 <no-reply@rightproperty365.com>`, so `rightproperty365.com` must be verified
   there. Until it is, Resend rejects every send with `403 validation_error`.
2. **Create a Resend API key** (a "sending access" key is enough) and put it in `RESEND_API_KEY`:
   in your local `.env` (git-ignored) for development, in a **Kubernetes Secret** in production.
   It must never be committed, logged or pasted anywhere.
3. **Turn off open and click tracking for the domain** in the Resend dashboard if you do not want
   it. The Resend API has no per-message switch for it, so this code cannot do it for you. Nothing
   in the templates adds a tracking pixel or rewrites links.
4. **Serve the logo images** from the web app at `{PUBLIC_WEB_URL}/email/logo-on-indigo.png` (they
   are in the web repository under `public/email/`). Emails read correctly without them.

### Configuration

| Variable | Default | Notes |
| --- | --- | --- |
| `RESEND_API_KEY` | _(empty)_ | Resend API key. Secret. Required in staging and production: startup **fails** without it |
| `MAIL_DRIVER` | _(automatic)_ | `resend` or `log`. Unset: `resend` when a key is set, otherwise `log` in development and a startup error elsewhere. `log` forces the log mailer even when a key is set, and is **refused** in staging/production |
| `EMAIL_FROM` | `RightProperty365 <no-reply@rightproperty365.com>` | Sender. Its domain must be verified in Resend |
| `EMAIL_REPLY_TO` | _(empty)_ | Optional Reply-To address |
| `PUBLIC_WEB_URL` | `http://localhost:3000` | Public origin of the web app. **Every** link and image URL in an email is built from it, never from request input. `WEB_BASE_URL` is still read as a deprecated alias when `PUBLIC_WEB_URL` is unset |

Precedence: a variable present in the process environment always wins over `.env`, **even when it
is set to an empty string**. So `RESEND_API_KEY= go run ./cmd/api` runs without a key although
`.env` has one. An empty `MAIL_DRIVER` means "automatic". Both are covered by tests.

The log mailer (development only) sends nothing and writes one line per email, including the link:
`"msg":"DEV MAIL (not sent): verify-email link", ..., "link":"http://localhost:3000/verify-email?token=..."`.

### How an email is sent

- **One email per logical notification.** Each notification has a key such as
  `property-request-received/<outbox event id>/customer`. It is recorded in `email_messages`
  (migration `0006`; a memory log in development) and sent to Resend as the `Idempotency-Key`
  header. A notification that the provider already accepted is never sent again, and a retry after
  an ambiguous failure cannot produce a second email (Resend keeps idempotency keys for 24 hours;
  the `email_messages` row covers the time after that).
- **Statuses**: `ATTEMPTED`, `PROVIDER_ACCEPTED` (with Resend's message ID) and `FAILED` (with a
  short reason and whether it is terminal). **`PROVIDER_ACCEPTED` is not "delivered"**: Resend
  accepting a message says nothing about whether it reached an inbox, and nothing here claims it.
  There is no webhook handling for bounces or complaints yet.
- **Retries.** One send makes up to 3 attempts with backoff for transient failures (network
  errors, `429 rate_limit_exceeded`, `5xx`, a concurrent request with the same key), honouring
  `Retry-After`. Anything else is terminal.
- **Notification emails go through the transactional outbox and the worker**: the change and its
  event are committed together; the worker sends the emails; a transient failure is retried with
  the outbox backoff (30 s doubling, up to 8 attempts) and a terminal one dead-letters the event
  immediately.
- **Emails with a secret link are sent from the request path**, not the outbox: the verification
  or reset token must never be stored in an outbox payload. They get the bounded retry above. The
  token is in the email only — not in the notification key, the `email_messages` row or the logs.
  If the send still fails the API answers `202` as before and the person can ask again.
- **Not stored, not logged**: the recipient address (only its SHA-256), subject, body, links,
  request headers and the API key. Provider error text is reduced to a status code and Resend's
  short error name.
- **Without a database** (development) there is no outbox table, so the API handles notification
  events itself, in-process, from the in-memory stores: no retries, nothing durable. The worker is
  idle in that mode.

### What is sent today

Every template that `mail.Templates()` marks as triggered (template names in brackets):

| Email | Trigger | Path |
| --- | --- | --- |
| Confirm your email address (`verify-email`) | Register, resend verification | request |
| Reset your password (`reset-password`) | Password-reset request (never for administrators) | request |
| You already have an account (`account-exists`) | Register with an address that is already verified | request |
| Your sign-in code (`sign-in-code`) | `POST /v1/auth/login` and `/v1/admin/auth/login` with the right password, and their `/login/resend` | request |
| Your account is ready (`welcome`; lists the roles chosen and which are waiting for verification) | Email verified for the first time | outbox `USER_EMAIL_VERIFIED` |
| Your password was changed (`password-changed`) | Password reset confirmed; `adminctl set-password` | outbox `PASSWORD_CHANGED` |
| Administrator sign-in (`admin-sign-in`) | An administrator session started (`POST /v1/admin/auth/login/verify` with the right code, or `/login/mfa` for an administrator with an authenticator) | outbox `ADMIN_SIGNED_IN` |
| Recovery code used (`admin-mfa-recovery-code-used`) | An administrator signed in with a recovery code (API 0.40.0) | outbox `ADMIN_MFA_RECOVERY_CODE_USED` |
| Two-step sign-in reset (`admin-mfa-reset`) | A SUPER_ADMIN reset the administrator's MFA (API 0.40.0) | outbox `ADMIN_MFA_RESET` |
| Request received (`property-request-received`; to the customer, with the reference) | Property request created | outbox `PROPERTY_REQUEST_CREATED` |
| New property request (`property-request-new`; to the assigned agent or direct seller, otherwise the organisation's owners and managers); names the listing by title and reference | Property request created | same event |
| Enquiry received (`business-enquiry-received`; to the customer) | Business enquiry created | outbox `BUSINESS_ENQUIRY_CREATED` |
| New enquiry (`business-enquiry-new`; to the named agent, otherwise the owning organisation's owners and managers) | Business enquiry created | same event |
| Application received (`onboarding-submitted`; to the applicant) | `POST /v1/me/onboarding/{role}/submit` | outbox `ONBOARDING_SUBMITTED` |
| Application approved (`onboarding-approved`; to the applicant, with a link to their profile) | `POST /v1/admin/onboarding/{id}/approve` | outbox `ONBOARDING_APPROVED` |
| Application needs changes (`onboarding-changes-requested`; with the reason) | `POST /v1/admin/onboarding/{id}/request-changes` | outbox `ONBOARDING_CHANGES_REQUESTED` |
| Application not approved (`onboarding-rejected`; with the reason) | `POST /v1/admin/onboarding/{id}/reject` | outbox `ONBOARDING_REJECTED` |
| New appointment request (`appointment-requested`; to the assigned professional, otherwise the organisation's owners and managers) | Appointment created | outbox `APPOINTMENT_REQUESTED` |
| Appointment confirmed (`appointment-confirmed`; to the customer) | Confirmed by the professional, or a proposed time accepted | outbox `APPOINTMENT_CONFIRMED` |
| New time proposed (`appointment-new-time-proposed`; to the customer) / The customer proposed a new time (`appointment-reschedule-proposed`; to the professional) | A new time proposed by the other party | outbox `APPOINTMENT_RESCHEDULE_REQUESTED` |
| Appointment cancelled (`appointment-cancelled`; to the customer) / cancelled by the customer (`appointment-cancelled-by-customer`; to the professional) | Cancelled by the other party | outbox `APPOINTMENT_CANCELLED` |
| Listing approved (`listing-approved`; to the advertiser: the listing's agent, else the organisation's owners and managers, or the direct owner) | `POST /v1/admin/listings/{id}/approve`, for a new listing or changes to a published one (a relist: `listing-relisted`, below) | outbox `LISTING_APPROVED` |
| Listing needs changes (`listing-changes-requested`; with the reason) | `POST /v1/admin/listings/{id}/request-changes` | outbox `LISTING_CHANGES_REQUESTED` |
| Listing not approved (`listing-rejected`; with the reason) | `POST /v1/admin/listings/{id}/reject` | outbox `LISTING_REJECTED` |
| Your listing expires in 14 / 7 / 1 days (`listing-expiring`; to the advertiser as above; API 0.45.0) | The hourly listing expiry sweep: once per threshold and publication cycle (only the most recent one when a worker was down) | outbox `LISTING_EXPIRING` |
| Your listing has expired (`listing-expired`; to the advertiser) | The listing expiry sweep stores the listing `EXPIRED` | outbox `LISTING_EXPIRED` |
| Your listing is live again (`listing-relisted`; to the advertiser, with the new end date; instead of `listing-approved`) | `POST /v1/admin/listings/{id}/approve` of a relist (a NEW review of a listing published before) | outbox `LISTING_APPROVED` (`relist: true`) |
| Invitation to join a team (`team-invitation`; to the invited address; the link is single-use, 7 days) | `POST /v1/agent/organisations/{id}/invitations` | request |
| A request was assigned to you (`property-request-assigned`; to the assignee; reference, listing, preview) | `POST /v1/agent/property-requests/{id}/assign` | outbox `PROPERTY_REQUEST_ASSIGNED` |
| A document to sign or approve (`agreement-signing-request`; to a recipient, with their link) | An agreement sent (`POST /v1/me/agreements/{id}/send`, `POST /v1/me/leases/{id}/send`), the next signing order reached, a recipient replaced | request |
| Reminder (`agreement-reminder`; with a new link) | `POST /v1/me/agreements/{id}/recipients/{rid}/remind`; the reminder interval, by the agreements sweep | request / sweep |
| Code to confirm your email address (`agreement-signing-code`) | `POST /v1/signing/{token}/code` | request |
| Everybody signed (`agreement-completed`; to every recipient, the CCs and the sender) | The last signer or approver finishes | request |
| A recipient declined (`agreement-declined`; to the sender) | `POST /v1/signing/{token}/decline` | request |
| Account suspended (`account-suspended`; with the administrator's reason) | `POST /v1/admin/users/{id}/suspend` | outbox `ACCOUNT_SUSPENDED` |
| Account reactivated (`account-reactivated`) | `POST /v1/admin/users/{id}/reactivate` | outbox `ACCOUNT_REACTIVATED` |
| Account deleted (`account-deletion-completed`; to the address the account had) | `DELETE /v1/me`, `POST /v1/admin/users/{id}/delete` | request (the address is erased with the account) |
| Subscription invoice to pay (`subscription-invoice`; to the requester) | `POST /v1/me/subscriptions`, `POST /v1/me/subscriptions/{id}/renew` | outbox `SUBSCRIPTION_REQUESTED` |
| Subscription active (`subscription-activated`; payment recorded) | `POST /v1/admin/subscriptions/{id}/approve` | outbox `SUBSCRIPTION_ACTIVATED` |
| Subscription not approved (`subscription-rejected`; with the reason) | `POST /v1/admin/subscriptions/{id}/reject` | outbox `SUBSCRIPTION_REJECTED` |
| Subscription cancelled (`subscription-cancelled`; by an administrator, with the reason) | `POST /v1/admin/subscriptions/{id}/cancel` (a subscriber withdrawing their own pending request is not emailed) | outbox `SUBSCRIPTION_CANCELLED` |
| Subscription ended (`subscription-expired`) | An active subscription's end passed (the subscription sweep, every 5 minutes) | outbox `SUBSCRIPTION_EXPIRED` |
| Application sent (`application-submitted`; to the applicant, with the reference; "resubmitted" after more information) | `POST /v1/me/applications/{id}/submit` | outbox `APPLICATION_SUBMITTED` |
| New application (`application-new`; to the listing's agent, otherwise the organisation's owners and managers, or the direct owner; no answers or contact details) | same | same event |
| More information needed (`application-more-info`; with the request, contact details hidden) | `POST .../applications/{id}/request-info` | outbox `APPLICATION_INFO_REQUESTED` |
| Application update (`application-status-changed`; shortlisted, accepted or not successful) | `POST .../applications/{id}/status` | outbox `APPLICATION_STATUS_CHANGED` |

**Designed, written and tested, but NOT sent** (`Triggered: false` in `mail.Templates()`):

- `request-update` (a reply or status update to the customer): replies to customers are not built.
- `renewal-invoice` and `payment-received`: superseded by `subscription-invoice` and
  `subscription-activated`.
- `account-deletion-requested`: deletion (`DELETE /v1/me` and the administrators' delete) is
  immediate, so there is no request to confirm; `account-deletion-completed` is sent instead.

The wording about payments, invoices and deletion is a draft: the house rules say legal and
payment wording needs sign-off per market before release.

Privacy: an email to an agent, seller or owner carries the reference, a short preview with phone
numbers, email addresses and links removed, and a button to the inbox
(`{PUBLIC_WEB_URL}/agent/requests`). **It never contains the customer's phone number or email
address.** An email to a customer never contains the business's contact details. A routing
exception notifies the customer only.

### Templates and preview

One shared layout plus one HTML and one plain-text template per email, rendered with
`html/template` (auto-escaping on; nothing a person typed is ever treated as markup) and
`text/template`. The look follows the brand kit: indigo `#1900AC` header band with the reverse
logo, a 3px indigo-to-turquoise rule (solid turquoise where gradients are not supported), a white
card on a light slate page, one button per email (48px tall), a short footer saying why the email
was sent. Table layout, inline styles, a fluid container (`width:100%; max-width:600px`, so there
is no horizontal scroll at any width; measured from 320px to 800px for every template), a hidden preheader, `lang="en"`, and a
dark-mode treatment (`color-scheme` plus `prefers-color-scheme`). No JavaScript, no external CSS,
no web fonts (Inter with a system fallback), no tracking pixel, and one image (the logo, with
width, height and alt text).

```powershell
go run ./cmd/mailpreview        # or: make mail-preview
```

writes every template with sample data to `tmp/mail-preview/` (git-ignored) as `<name>.html` and
`<name>.txt`, plus an `index.html` linking them all. It sends nothing and needs no key. The exact
output of every template is pinned by golden files in `internal/platform/mail/testdata/golden`
(regenerate deliberately with `go test ./internal/platform/mail -update`).

**Not verified:** the templates have not been opened in real mail clients (Outlook, Gmail, Apple
Mail), and no email has ever been sent through Resend from this code — it is tested against a local
fake of the API only.

## Authentication and sessions

- **Passwords** – Argon2id (`golang.org/x/crypto/argon2`, 19 MiB, 2 passes), stored as a PHC
  string (`$argon2id$v=19$m=...,t=...,p=...$salt$hash`) and compared in constant time. 10–128
  characters.
- **Sessions** – opaque 32-byte random tokens (base64url). Only the SHA-256 is stored. A session
  ends 30 days after sign-in (absolute) or after 14 days without use (idle); `last_seen` is
  written at most once a minute, not on every request. Sessions are revocable (logout, password
  reset).
- **Web** – the token lives in the HttpOnly cookie `rp_session` (`SameSite=Lax`, `Path=/`,
  `Secure` unless `APP_ENV=development`). The frontend must call the API with
  `credentials: "include"`.
- **Mobile / API clients** – log in with `"token_delivery": "bearer"`: the response carries
  `access_token`, no cookie is set, and the client sends `Authorization: Bearer <token>`. When an
  `Authorization` header is present the cookie is ignored.
- **CORS** – allowlisted origins get `Access-Control-Allow-Credentials: true` with their exact
  origin echoed (never `*`).
- **CSRF** – a request authenticated by the **cookie** with a method other than GET/HEAD/OPTIONS
  must carry an allowlisted `Origin` header, else `403 CSRF_ORIGIN`. Bearer requests are exempt
  (browsers never attach that header by themselves). With curl, add
  `-H "Origin: http://localhost:3000"` to cookie-authenticated POSTs.
- **No account enumeration** – register, resend-verification and password-reset/request give the
  same answer whether or not the email has an account; login answers `401 INVALID_CREDENTIALS`
  with an identical body for an unknown email and a wrong password (an Argon2 verification runs in
  both cases), and `403 EMAIL_NOT_VERIFIED` only after the correct password.
- **Registering an email that already exists** – verified account: nothing changes and the owner
  gets a "you already have an account" email. Unverified account: the new registration replaces
  the pending name, password and role selection and earlier verification links stop working.
- **Roles at registration** – `roles` is required (see "Role model" below): 1–13 values from a
  server-side allowlist. Platform administrator roles, unknown values, non-string entries and
  **duplicates are rejected** with `400 VALIDATION_FAILED` and a message under `fields.roles`.
  **A property seeker account is only that:** the selection is either Property Seekers roles
  only (`CONSUMER`, `TENANT`, or both) or roles from the other groups only. A mix such as
  `CONSUMER` + `AGENT` is refused with `fields.roles` = "A property seeker account cannot also
  have a professional, business or owner role. Choose one or the other." Registration is the
  only place a person chooses roles (a repeated registration of a still-unverified email goes
  through the same check); see "Role model" for the paths that were checked.
- **Captcha at registration** – `captcha` is required; see "Registration captcha" below.
- **Signing in is two steps** – the password, then a six-digit code sent by email; see "Sign-in
  code" below.
- **One-time tokens** – email verification (24 h) and password reset (1 h) are single-use and
  stored as SHA-256. A successful reset also marks the email verified, because the link proves
  control of the mailbox.
- **Rate limit** – all `POST /v1/auth/*` routes share one per-IP bucket (burst 20, refill 20/min),
  separate from the property-request bucket, except the sign-in code routes
  (`/v1/auth/login/verify`, `/v1/auth/login/resend`), which have their own (burst 20, refill
  20/min; the administrator ones another of the same size). `GET /v1/auth/captcha` has its own (burst 30,
  refill 30/min).
- **Email delivery** – through Resend; see "Email". Verification and reset emails are sent from
  the request with a bounded retry (their link is a credential and is never written to the
  outbox); if the send still fails the person has to ask again. Everything else is durable through
  the outbox. In development without a key the log mailer writes one line with the link
  (`http://localhost:3000/verify-email?token=...`, `.../reset-password?token=...`).
- Logs carry `user_id` on every line of an authenticated request. Passwords, tokens and cookie
  values are never logged (except the development mail link described above).

### Sign-in code (two-step sign-in, API 0.21.0)

Signing in is two steps for everyone, customers and administrators. The product owner asked for
a one-time code at sign-in; the specification requires MFA for administrators and allows it as an
option for users. The emailed code proves control of the mailbox; since API 0.40.0 an
administrator with an authenticator app also answers a third step (see "Administrator MFA").

```
POST /v1/auth/login            {"email": "...", "password": "...", "token_delivery": "cookie"}
200 {"status":"CODE_REQUIRED","challenge_id":"<opaque>","sent_to":"c•••@example.test",
     "expires_at":"2026-11-14T09:10:00Z","code_length":6}         (no cookie, no token)

POST /v1/auth/login/verify     {"challenge_id": "<opaque>", "code": "123456"}
200 exactly what POST /v1/auth/login answered before 0.21.0 (cookie set, or access_token)

POST /v1/auth/login/resend     {"challenge_id": "<opaque>"}
200 the same shape as the login answer, with a new expires_at
```

The administrator sign-in is the same under `/v1/admin/auth/login`, `/login/verify` and
`/login/resend` (the verify answer is the admin-scope cookie session).

- **The code** – 6 digits from `crypto/rand`, valid for **10 minutes**, usable **once**. It is
  emailed with the `sign-in-code` template ("Your sign-in code"; the code large; "If this wasn't
  you, ..."), from the request path like the verification and reset links: a code is a credential
  and is never written to the outbox, a log line or the delivery record.
- **Failures** – `400 VALIDATION_FAILED` with `fields.code` = "Enter the code we sent you."
  (missing, blank, not digits) or "That code is not right." (wrong, attempts left).
  `400 CODE_EXPIRED` ("That code has expired or was used too many times. Sign in again.") when
  the challenge expired, is unknown, was used, had **5 wrong codes** (it is then dead), or belongs
  to the other sign-in: a customer challenge is refused by the administrator verify endpoint and
  the reverse.
- **Resend** – at most once every **30 seconds** and **5 times** per challenge, otherwise
  `429 RATE_LIMITED` with `Retry-After`. The earlier code stops working; wrong codes already
  entered still count.
- **Unchanged** – a wrong email or password is the same `401 INVALID_CREDENTIALS` as before (same
  body, same Argon2 timing protection, no hint whether the account exists), and an unverified
  address is still `403 EMAIL_NOT_VERIFIED`; neither starts a challenge or sends a code.
  `POST /v1/auth/verify-email` still signs the customer in directly (the link already proves the
  mailbox).
- **Storage** – `sign_in_challenges` (migration `0019`): the SHA-256 of the challenge id, the
  account, the scope, the token delivery, an HMAC-SHA256 of the code keyed with the raw challenge
  id (so the table alone cannot be used to test codes), the expiry, wrong attempts, resends, when
  the last code was sent and when it was used. Expired rows are deleted whenever a new challenge
  starts.
- **Audit (log-only)** – `admin_sign_in` is logged with `outcome` `code_sent` at the password
  step and `success` at the code step; wrong codes are logged as `sign_in_code` / `failure`
  with the attempt count.
- **Demo code: `AUTH_DEMO_CODE`** (development and staging only). When set (for example
  `1234`), that exact code is also accepted for any live challenge, which is how testers sign in
  to the sample accounts on staging (their `@example.test` addresses are never emailed). A code
  of another length than 6 digits is accepted only when it is the demo code. **If it is set while
  `APP_ENV=production`, the API and the worker refuse to start.** The API logs one warning at
  startup while it is active, never the code. There is no other bypass.
- **Development** – with the log mailer, the code is printed on the `DEV MAIL (not sent):
  sign-in-code` line (`code` attribute).
- **Reserved test domains are never emailed.** With the Resend transport, any email to
  `example.com`, `example.org`, `example.net`, any `*.example.*` domain, or a domain ending in
  `.test`, `.example`, `.invalid` or `.localhost` is skipped with an info log line that does
  not name the address (`mail.IsReservedTestAddress`; applies to every template). The log
  transport still prints them.
- **Tests** sign in through both steps: the router test helper reads the code from the captured
  email and calls the verify endpoint, exactly as a person would.
- **Clients** – the web app, the mobile app and the administrator console must show a code step
  after the password and accept 4 to 6 digits (the demo code may be shorter than
  `code_length`).

### Registration captcha

Registration asks a small arithmetic question. It is built in: no third-party service, no
secret, no environment variable, nothing loaded from another origin.

```
GET /v1/auth/captcha
200 {"captcha_id": "<opaque id>", "question": "What is 7 + 5?", "expires_at": "2026-11-14T09:10:00Z"}

POST /v1/auth/register
{ ...the other fields..., "captcha": {"captcha_id": "<that id>", "answer": "12"} }
```

- The question is an addition, or a subtraction that never goes below zero, of numbers from 1
  to 10. It is valid for **10 minutes**. `answer` is a string; spaces around it are ignored and
  only digits are an answer.
- Refusals are the usual `400 VALIDATION_FAILED`, under `fields.captcha`: "Answer the question
  to show you are a person." (missing), "That answer is not right. Try the new question."
  (wrong) or "That question has expired. Try the new question." (expired, unknown or already
  presented).
- **Single use.** The captcha is judged only after every other field is acceptable, so a typo in
  the email does not use it up. From then on the first registration that presents it uses it up,
  right or wrong: answers cannot be found by retrying. A client asks for a new question after
  any refused registration.
- It is judged before the password is hashed, before the account is looked up and before
  anything is written or emailed, so the answer is the same whether or not the email already
  has an account.
- **Storage.** `registration_captchas` (migration `0017`): the id, a SHA-256 of the id and the
  answer, the expiry and the time it was used. Neither the question nor the answer is stored.
  Expired rows are deleted whenever a new question is asked; there is no worker job. In
  development without a database the questions are kept in memory.
- **What it is not.** Anything that can read the question can answer it. This stops the
  simplest scripts and nothing more; the per-IP rate limits remain the real limit. There is no
  switch that turns it off: tests ask for a question and answer it, like any client.
- `cmd/seed` and `cmd/adminctl` create accounts directly in the database and do not go through
  the captcha.

### Administrator login and session scopes

Platform administrators have their own login, and every session has a **scope**:

| | `customer` | `admin` |
| --- | --- | --- |
| Issued by | `POST /v1/auth/login/verify` (after `POST /v1/auth/login`), `POST /v1/auth/verify-email` | `POST /v1/admin/auth/login/verify` (after `POST /v1/admin/auth/login`) only |
| Who | any verified account **without** a platform role | verified accounts **with** a platform role |
| Delivery | cookie or bearer | cookie only — never a bearer token, and an admin token sent as `Authorization: Bearer` does not authenticate |
| Lifetime | 30 days absolute, 14 days idle | 12 h absolute, 30 min idle (`ADMIN_SESSION_TTL`, `ADMIN_SESSION_IDLE_TTL`) |
| Rate limit (per IP) | burst 20, 20/min across `/v1/auth/*` | burst 5, 5/min, separate bucket |

- `POST /v1/admin/auth/login` takes `{email, password}` and answers `CODE_REQUIRED` (see
  "Sign-in code"); `POST /v1/admin/auth/login/verify` then answers
  `{"user": {...}, "session": {"scope":"admin","expires_at":"..."}}`. `token_delivery` is not
  accepted (strict JSON → `400 INVALID_JSON`) and `access_token` is never returned.
- **It does not reveal who is an administrator.** Unknown email, wrong password, a consumer, a
  self-registered professional and an unverified account all get the same
  `401 INVALID_CREDENTIALS` body, after exactly one Argon2 verification (real or dummy).
- **Administrators cannot use the customer login**: `POST /v1/auth/login` answers the same generic
  `401` for any account holding a platform role, and `POST /v1/auth/verify-email` verifies such an
  account but starts no session (it returns `user` only).
- **`RequirePlatformRole` requires an admin-scope session.** A platform role on a customer-scope
  session grants nothing (`403 FORBIDDEN`). `GET /v1/admin/me` is the one admin-only route so far
  and exists to prove the gate.
- An admin session stops working as soon as its user no longer holds a platform role. `last_seen`
  is refreshed at most once a minute (and never less often than a tenth of the idle window).
- `GET /v1/me` and the login responses include `session.scope` so the frontend can tell which kind
  of session it has. Sign out of either with `POST /v1/auth/logout`.
- **No email password reset for administrators.** `POST /v1/auth/password-reset/request` still
  answers `202` for a platform-role account but sends nothing: a compromised mailbox must not
  become an administrator takeover (MFA is only enforced where `ADMIN_MFA_REQUIRED` is on). Use
  `adminctl set-password`.
- **Platform roles cannot be obtained through any public endpoint.** Registration only accepts the
  account-role allowlist; the only writers of platform roles are `cmd/adminctl` and the
  development seed.
- **Audit (log-only).** Every administrator sign-in attempt writes one structured log line with
  `"audit":true`: `event` (`admin_sign_in`), `outcome` (`success`/`failure`), an internal `reason`
  on failure, `user_id` when the account is known, `email_sha256`, `ip` and `trace_id`. A refused
  customer-login attempt on a platform account and a refused password-reset request are logged the
  same way. The submitted email is logged as a **SHA-256 fingerprint, not as text**: what people
  type into a login form is personal data and is sometimes a password pasted into the wrong field;
  the fingerprint still groups repeated attempts and lets you test a known address. The password is
  never logged. **This is not a durable audit record** — there is no audit table; the lines live
  only wherever the process logs are shipped and retained. Requests rejected earlier (rate limit
  `429`, malformed body `400`) are in the access log but have no audit line.

> **Administrator MFA (API 0.40.0).** The specification requires MFA for administrator roles. An
> administrator account is protected by a password, the emailed sign-in code (which proves control
> of the mailbox), a per-IP rate limit, a short session and, once enrolled, an **authenticator app
> (TOTP) or a one-time recovery code** on a separate device (see "Administrator MFA"). Production
> requires it by default (`ADMIN_MFA_REQUIRED=true`, which also needs `MFA_ENCRYPTION_KEY`):
> administrators without an authenticator can only enrol. With `ADMIN_MFA_REQUIRED=false`
> (development and staging by default) MFA is optional, and an administrator who has not enrolled
> is protected only by the password and the emailed code: do not run a real platform that way.
> There are no hardware keys (WebAuthn).

#### Creating administrators: `adminctl`

Against a real database the first administrator is created with `cmd/adminctl` (Postgres only; it
uses `DATABASE_URL`, or the optional `DATABASE_DIRECT_URL` override, and fails with a clear message
when neither is set):

```powershell
go run ./cmd/adminctl create --email ops@example.com --name "Ops Admin" --role SUPER_ADMIN
go run ./cmd/adminctl create --email zw@example.com --name "ZW Admin" --role MARKET_ADMIN --market ZW
go run ./cmd/adminctl set-password --email ops@example.com
```

- The password is typed at a no-echo prompt (asked twice), piped on stdin, or taken from the
  `ADMINCTL_PASSWORD` environment variable. It is **never a flag**, never echoed and never logged.
  Same rule as everywhere else: 10–128 characters, stored as Argon2id.
- `create` marks the email verified, accepts only the seven platform roles, requires `--market`
  for `MARKET_ADMIN` (and rejects it otherwise), and **refuses an email that already has an
  account** — it does not promote existing customer accounts.
- `set-password` works only for accounts holding a platform role and revokes all of that
  account's sessions.
- **The database path of `adminctl` has never been run** (no Postgres on the development machine);
  only its argument and password validation is unit-tested, against the in-memory store.
- In development without a database there is nothing to create: use the seeded
  `admin@example.test` (`SUPER_ADMIN`).

### Role model

There are three separate things, all returned by `GET /v1/me`, and only two of them grant anything:

| | What it is | Who sets it | Grants access? |
| --- | --- | --- | --- |
| `account_roles` | What the person says they are, chosen at registration | The person | **No** |
| `memberships` | A role in one organisation and market | Not self-service (database / dev seed today) | Yes — to that organisation's data |
| `platform_roles` | Platform staff role | Not self-service (database / dev seed today) | Yes — platform administration |

**Account roles** are chosen at registration from this allowlist (exact API codes): `CONSUMER`,
`TENANT`, `AGENT`, `SALES_AGENT`, `LETTINGS_AGENT`, `PROPERTY_MANAGER`, `SOLICITOR`,
`AGENCY_OWNER`, `AGENCY_ADMIN`, `AGENCY_STAFF`, `LANDLORD`, `PRIVATE_SELLER`, `HOST`, `DEVELOPER`,
`SERVICE_PROVIDER`. `CONSUMER` and `TENANT` are the Property Seekers roles, and **a property
seeker account is only that**: they can be chosen alone or together, never with a role from
another group, and an account with a role from another group cannot have them.

That rule is enforced in the one place roles are chosen, `parseAccountRoles` in
`internal/modules/identity/service.go`, which both a first registration and a repeated
registration of a still-unverified email go through. Nothing else gives an account a role:
there is no "add a role" endpoint, onboarding and the administrator review only change the
**status** of a role the account already holds, `cmd/adminctl` creates administrators with a
platform role and no account role, and `cmd/seed` writes the sample accounts, which follow the
rule. If an endpoint that adds a role is ever built, it must apply `identity.IsSeekerRole` the
same way. Accounts that already held a seeker role and another role before this rule keep both:
no migration changes them. The two seeker codes are fixed in the code; moving another role into
the Property Seekers group in the reference data does not make it a seeker role for this rule.

Each role has a status: `CONSUMER` and `TENANT` are
`ACTIVE`; every other role starts as `PENDING_VERIFICATION` and becomes `ACTIVE` or `REJECTED` when
a platform administrator decides its onboarding application (see "Professional onboarding and
verification"; the database also reserves `REVOKED`, which nothing sets yet). `GET /v1/me` shows
each role's `onboarding_status`. Platform roles can never be chosen.

Selecting a professional role at sign-up **only records a request**. It does not create an
organisation or a membership, gives no access to any organisation's data, and is never consulted by
`RequireMembership` or `RequirePlatformRole` — a self-registered "AGENT" gets `403 FORBIDDEN` from
`GET /v1/agent/property-requests` until their onboarding application is approved, which is what
creates the membership.

There is exactly **one** place an account role affects access: a user who declared
`PRIVATE_SELLER` (in any status) may open `GET /v1/agent/property-requests`. That only lets a
would-be seller see an inbox that is **empty** until they actually are the direct seller of a
listing; the account role never selects or widens the data returned. No other authorisation
decision reads account roles.

Memberships and platform roles use these vocabularies:

| Kind | Values | Scope |
| --- | --- | --- |
| Organisation kind | `AGENCY`, `LANDLORD`, `HOST`, `DEVELOPER`, `SERVICE_PROVIDER` | — |
| Membership role | `OWNER`, `MANAGER`, `AGENT`, `STAFF` | one organisation + market |
| Platform role | `SUPER_ADMIN`, `PLATFORM_ADMIN`, `MARKET_ADMIN`, `FINANCE_ADMIN`, `CONTENT_ADMIN`, `MODERATOR`, `SUPPORT` | global, except `MARKET_ADMIN` which carries a `market_code` |

Authorisation is always decided on the server from the session. `internal/platform/auth` provides
the middleware every module uses:

- `Authenticate` – resolves the cookie or bearer token into a `Principal` (`UserID`, `Name`,
  `Email`, `PlatformRoles`, `Memberships`) in the request context; an invalid or expired token is
  simply anonymous.
- `RequireAuth` – `401 UNAUTHENTICATED`.
- `RequireMembership(roles...)` – `403 FORBIDDEN` unless the caller holds one of the roles in some
  organisation (any membership when no roles are given).
- `RequirePlatformRole(roles...)` – `403 FORBIDDEN` unless the caller holds one of the platform
  roles; `SUPER_ADMIN` always qualifies.

The middleware only answers "may this caller use the route at all". Handlers must still scope
their data with `Principal.OrganisationIDs(...)` (and `PlatformRole.MarketCode` for
`MARKET_ADMIN`).

`GET /v1/agent/property-requests` shows how. Each item carries `listing: {slug, title, reference}`
(or null) so the inbox can show which property a request is about.

- **Who may call it:** a user with any organisation membership, **or** the direct seller of at
  least one listing, **or** a user who declared the `PRIVATE_SELLER` account role. Everyone else
  gets `403 FORBIDDEN` (`401` when anonymous).
- **What it returns** (the union, built only from the caller's user ID and memberships):
  `OWNER`/`MANAGER` see every request whose recipient organisation is one of theirs;
  `AGENT`/`STAFF` see the requests of their organisation that are assigned to them personally or
  to the organisation inbox, not those assigned to a colleague; a direct seller sees the requests
  routed to them for their own listings.
- Requests of other organisations and of other sellers are never returned. Newest first, 20 per
  page by default (at most 100), with filters that only narrow this scope (see "Dashboards and
  working a request").

### Not built yet

- **Hardware keys (WebAuthn) for administrators** – administrator MFA exists since 0.40.0 with an
  authenticator app and recovery codes (see "Administrator MFA"); hardware keys were left out
  (no lightweight dependency). The administrator login is production-ready only with
  `ADMIN_MFA_REQUIRED=true` (the production default) and `MFA_ENCRYPTION_KEY` set.
- **Administrator sign-ins in the durable audit trail** – `admin_audit_events` (migration `0024`)
  records what administrators do (see "Administrators' area") and, since 0.40.0, their MFA
  changes, recovery code use and locked second-factor challenges; the sign-ins themselves are
  still only written to the process log.
- **WhatsApp and in-app notifications.** Recipients and customers are notified by email only.
- **Bounce and complaint handling** – there is no Resend webhook endpoint, so a message that
  Resend accepted and later failed to deliver is never noticed; no suppression list.
- **The emails nothing sends** (status updates to the customer, the
  older renewal-invoice and payment-received templates, account deletion requested) are written
  and tested but not triggered; see "What is sent today".
- **Unsubscribe / notification preferences** – every email today is transactional.
- **Profile management** – directory profiles are created only by an approved onboarding
  application or the seed; apart from their logos (`/v1/me/profiles/{kind}/{slug}/logo`) there is
  no API to edit, verify or suspend them. (Listings are managed since 0.22.0: create, edit,
  photos, review, archive and relist; see "Listing management".)
- **A running search cluster** – the Elasticsearch adapter, indexing handler and `cmd/reindex` are
  written and tested against a fake, but no cluster exists and hosting/licence is an open
  decision; search runs in Postgres until then.
- **Pausing a listing** – the status exists, nothing sets it. (The expiry job and its reminders
  are built since 0.45.0: see "Listing expiry and reminders".)
- **Metrics** – the search-fallback count is only a log field.
- **Revoking an account role or a platform role** – platform roles are created only by
  `cmd/adminctl`, and nothing takes an approved account role away. (Memberships are managed since
  0.23.0: an organisation's OWNER and MANAGER invite, approve join requests, change, suspend and
  remove members; see "Agency teams".)
- **Data export.** (Account deletion exists since 0.25.0: `DELETE /v1/me` and the
  administrators' `POST /v1/admin/users/{id}/delete`, immediate, the row anonymised in place.)
- **Consent preference centre** – registration consent is recorded, but there is no way to review
  or change consents (including `marketing_opt_in`) afterwards.
- **Durable delivery of verification and reset emails** – notification emails are durable through
  the outbox, but these two are sent from the request (so their token is never stored) with a
  short retry only.
- **Login lockout** beyond the per-IP rate limit (no per-account throttling or lockout).
- Session management for users (list / revoke other devices), password change while signed in,
  and email change.
- Editing or deleting an internal note on a request, and a durable audit table for stage changes
  beyond the request's own timeline.

## Layout

```
cmd/api        HTTP API binary
cmd/worker     background worker (outbox dispatcher)
cmd/migrate    migration runner: up | down | status
cmd/adminctl   create platform administrators / set their password (Postgres only)
cmd/seed       development sample data into Postgres (refuses outside APP_ENV=development)
cmd/mailpreview  render every email template to tmp/mail-preview/ (sends nothing)
internal/modules/onboarding  professional onboarding: applications, documents, review, approval
internal/platform/storage    private file storage (onboarding documents, profile logos): local folder (development) and S3/MinIO
cmd/reindex    rebuild the Elasticsearch listings index and swap the alias (needs a cluster)
internal/platform/config     env configuration + validation
internal/platform/logging    slog JSON logger, request-ID propagation
internal/platform/httpx      middleware, error envelope, JSON decoding, rate limiter
internal/platform/auth       Principal + Authenticate / RequireAuth / RequireMembership / RequirePlatformRole
internal/app                 router wiring (middleware order, routes); end-to-end tests live here
internal/adminctl            logic of cmd/adminctl (argument and password validation)
internal/devseed             the development sample data (organisations, people, 18 listings, 32 directory profiles)
internal/platform/database   pgxpool setup (session or transaction pool mode)
internal/platform/outbox     outbox dispatcher (lease, retry, dead-letter)
internal/platform/elastic    Elasticsearch client (official Go client, transport only)
internal/platform/mail       email templates, renderer, Resend client, log mailer, delivery log
internal/platform/migrate    embedded-SQL migration runner
internal/modules/health            /healthz, /readyz
internal/modules/markets           GET /v1/countries, /v1/markets (every ISO country)
internal/modules/availability      /v1/me/listings, availability calendars of short stays, GET /v1/listings/{slug}/availability
internal/modules/propertyrequests  POST /v1/property-requests, /v1/agent/property-requests[/{id}[/notes]], /v1/me/property-requests, /v1/admin/routing-exceptions (handler, service, store: postgres + memory)
internal/modules/dashboard         GET /v1/me/dashboard, /v1/agent/organisations/{id}/members, /v1/admin/overview (owns no data: reads through the other modules)
internal/modules/businessenquiries POST /v1/business-enquiries, GET /v1/inbox/business-enquiries (handler, service, store: postgres + memory)
internal/modules/appointments      /v1/appointments, /v1/me/appointments, /v1/inbox/appointments, /v1/inbox/summary (handler, service, store: postgres + memory)
internal/modules/notifications     outbox events -> emails; verification/reset mailer; data lookups (postgres + memory)
internal/modules/listings          GET /v1/listings, /v1/listings/suggest, /v1/listings/{slug}; Search port; listing management (/v1/me/listings create, edit, photos, submit, archive, relist), /v1/listing-photos, /v1/admin/listings review (postgres + memory)
internal/modules/directory         GET /v1/directory, /v1/directory/{kind}[/{slug}], /v1/sitemap; profile logos: the public image route and /v1/me/profiles (postgres + memory)
internal/modules/trust             GET /v1/trust/summary: verified advertisers and public listings, counted
internal/modules/reference         GET /v1/reference, /v1/reference/{set}; the rows other modules validate against; /v1/admin/reference (admin.go)
internal/modules/identity          /v1/auth/*, /v1/me: accounts, sessions, tokens, roles, Mailer (store: postgres + memory)
internal/modules/leases            /v1/me/leases, /v1/me/lease-templates, /v1/me/organisations/{orgId}/lease-templates, /v1/admin/lease-templates, /v1/admin/leases; lease PDFs (postgres + memory)
internal/modules/agreements        /v1/me/agreements, /v1/signing/{token}, /v1/me/signature, /v1/stamp-preview, /v1/verify; signing PDFs, generated stamps, vendored fonts (postgres + memory)
internal/modules/teams             /v1/agent/organisations/{id}/members, /invitations, /join-requests, /audit; /v1/invitations/{token} (postgres + memory)
internal/modules/accounts          /v1/admin/users (suspend, reactivate, delete), /v1/admin/audit, DELETE /v1/me (postgres + memory)
internal/modules/subscriptions     /v1/me/subscription-plans, /v1/me/subscriptions, /v1/admin/subscription-plans, /v1/admin/subscriptions; the expiry sweep (postgres + memory)
internal/modules/reports           /v1/admin/reports/summary and the CSV exports (postgres + memory)
internal/modules/saved             /v1/me/saved-listings[/ids|/{listingId}]: a customer's saved listings and their snapshots (postgres + memory)
internal/modules/analytics         /v1/analytics/* and /v1/admin/analytics/summary: events, bots, rollups, reports (postgres + memory)
internal/modules/crm               /v1/agent/organisations/{id}/contacts/*: contact books, duplicates, merge, import/export, links (postgres + memory)
internal/modules/ledger            /v1/agent/organisations/{id}/ledger/*: the agent books, reports per currency, immutable entries (postgres + memory)
internal/platform/adminaudit       the administrators' audit trail (admin_audit_events)
internal/platform/orgaudit         an organisation's team audit trail (organisation_audit_events)
internal/platform/money            exact decimal amounts and the ISO 4217 minor units (math/big)
internal/platform/imaging          image helpers for the re-encoded uploads (profile logos, listing photos)
migrations/    embedded SQL (NNNN_name.up.sql / .down.sql)
api/openapi.yaml             OpenAPI 3.1 contract
deploy/docker/Dockerfile     one multi-stage file, targets: api, worker, migrate
docker-compose.yml           Postgres 17 + MinIO for local dev when Docker is available
```

Modules depend on `internal/platform`, never on each other's internals. Each module owns its
handler, service and store interface.

### Planned modules (not created yet)

Deliberately no empty stubs. These will be added under `internal/modules/` as they are built:

- `identity` (exists) has administrator MFA (authenticator app) but no hardware keys — see "Not
  built yet" (membership management and invitations are in `teams`)
- `listings` (exists, with management since 0.22.0 and the expiry job since 0.45.0) still lacks pausing
- uploads have no module of their own: each module stores its files through
  `internal/platform/storage` (local folder or S3/MinIO)
- `subscriptions` (exists since 0.25.0) covers plans, invoices and admin-recorded payments
  (exact decimal money + ISO 4217); online payment is in `payments` behind `platform/paygateway`
  (0.42.0, fake driver only); receipts are not built
- `notifications` (exists for email) – WhatsApp and in-app delivery
- `backoffice` – moderation, and reassigning routing exceptions (listing and resolving them is
  built in `propertyrequests`)

## Database

> **Decision (user, 2026-11-14): the database is self-hosted PostgreSQL on the project's own
> server, not Neon.** In the user's words: "no dont use neon we using postgres db on server". This
> overrides the BRD, the FSD and the handover notes wherever they name Neon or describe
> Neon-specific features (branching, autosuspend, pooled/direct endpoints, plan-based point-in-time
> recovery). None of those are assumed anywhere in this repository.

- **One connection string.** `DATABASE_URL` is the only required database setting. The API, the
  worker, `cmd/migrate`, `cmd/adminctl` and `cmd/seed` all use it.
- **Pool mode.** `DB_POOL_MODE=session` (the default) means `DATABASE_URL` connects straight to
  PostgreSQL and pgx runs normally, with its per-connection prepared-statement cache. If PgBouncer
  in transaction-pooling mode is ever put in front of the server, set `DB_POOL_MODE=transaction`:
  the pool then avoids named prepared statements (`QueryExecModeCacheDescribe`). The application
  code uses no session state (`SET`, `LISTEN/NOTIFY`, session advisory locks) either way, so
  switching needs no code change.
- **`DATABASE_DIRECT_URL` is optional** and only matters with `DB_POOL_MODE=transaction`: the
  migration runner takes a session-level advisory lock and runs multi-statement scripts, which
  cannot go through a transaction pooler, so `cmd/migrate` then needs a direct PostgreSQL
  connection (it refuses to run otherwise). `adminctl` and `seed` use the override too when it is
  set. Without a pooler, leave it empty. Applied migration versions are tracked in
  `schema_migrations`.
- **Sizing `DB_MAX_CONNS`.** It is a cap **per process** (1–100, default 8); every API replica and
  the worker open their own pool. Keep

  `API replicas × DB_MAX_CONNS + workers × DB_MAX_CONNS + ~5 (migrations, adminctl, backups, psql, monitoring)`

  comfortably below the server's `max_connections` (PostgreSQL's default is 100, a few of which are
  reserved for superusers). Example: 3 API replicas and 1 worker at the default 8 use at most 32.
  Raising replica counts (autoscaling) raises this total; either lower `DB_MAX_CONNS`, raise
  `max_connections` (which costs server memory), or add PgBouncer and `DB_POOL_MODE=transaction`.
- **TLS.** The code does not set or override `sslmode`; whatever is in `DATABASE_URL` is used as
  written. **Assumption made here:** the API and worker run on different hosts from the database
  (a cluster talking to a database server), so the connection string should carry
  `sslmode=require` at minimum, or `sslmode=verify-full` with the server's CA certificate
  (`sslrootcert`) to also authenticate the server. With no `sslmode` the client defaults to
  `prefer`, which silently falls back to an unencrypted connection. `sslmode=disable` is only
  acceptable when both ends are on the same machine (as with the local docker-compose Postgres).
  At startup the API and worker log a **warning** (never the URL) if `DATABASE_URL` points at
  another host without `require`/`verify-ca`/`verify-full`; they do not refuse to start and do not
  change the connection. The PostgreSQL server must have TLS enabled for `require` to work — that
  is server configuration and is not done by this repository.
- **Deployment.** The GitOps manifests live in the user's separate `adminweighty/weightygitops`
  repository (not touched from here). They need to provide `DATABASE_URL`, `RESEND_API_KEY` (and
  `ELASTICSEARCH_API_KEY` if Elasticsearch is used) to
  the API and worker from a **Kubernetes Secret** (and `DB_POOL_MODE` / `DATABASE_DIRECT_URL` only if a pooler is
  used). No connection string belongs in this repository, in an image or in a ConfigMap.
- **Local development** keeps using the docker-compose Postgres (`docker compose up -d`) or no
  database at all (in-memory stores).

### Database operations: NOT set up, and unowned

Running PostgreSQL yourself means the following are the project's responsibility. **None of them
exists today, nothing in this repository provides them, and nobody has been named as owner.**

| Area | What is needed | Status | Owner |
| --- | --- | --- | --- |
| Backups | Regular logical dumps (`pg_dump` / `pg_dumpall`) and/or physical base backups (`pg_basebackup`) with continuous WAL archiving for point-in-time recovery; copies kept off the database server | Not set up | Unassigned |
| Restore testing | Periodically restoring a backup into a scratch server and checking it; a backup that has never been restored is not known to work | Not set up | Unassigned |
| Monitoring and alerting | Disk space, connection count vs `max_connections`, replication/WAL archiving lag, long-running queries, failed logins, server availability | Not set up | Unassigned |
| Upgrades and patching | PostgreSQL minor releases (security fixes), planned major-version upgrades, OS patching of the database server | Not set up | Unassigned |
| Access and secrets | A dedicated least-privilege application role, a separate migration/owner role if wanted, password rotation, `pg_hba.conf` restricted to the cluster, TLS certificates | Not set up | Unassigned |
| High availability | A standby / failover plan. A single server is a single point of failure for the whole platform | Not decided | Unassigned |

Until these have owners, a disk failure or a bad migration on the server means data loss.

### Schema

- `0046_developments` (API 0.44.0): `developments` (organisation CASCADE, developer profile CASCADE, `market_code` FK `markets.code`, `slug` `developments_slug_format` unique per market `developments_market_slug_unique`, `name` `developments_name_length`, `summary` `developments_summary_length`, `description` `developments_description_length`, construction `status` PLANNED / UNDER_CONSTRUCTION / SELLING / COMPLETED / SOLD_OUT `developments_status_allowed`, `moderation_status` DRAFT / PENDING_REVIEW / PUBLISHED / REJECTED `developments_moderation_allowed`, `completion_estimate` quarter or date `developments_completion_format`, `city_code` / `suburb_code` / free `city_name` (`developments_city_name_length`, `developments_city_one_kind`), `latitude` / `longitude` numeric(9,6) (`developments_latitude_range`, `developments_longitude_range`, `developments_coordinates_pair`), `location_precision` `developments_precision_allowed`, `amenities` text[], `highlights` jsonb, `youtube_video_id` `developments_youtube_format` / `youtube_start_seconds` `developments_youtube_start_valid`, `review_reason` `developments_review_reason_length`, submitted / reviewed / created by (SET NULL) and times, `published_at` `developments_published_when_public`; NO phone, email or street-address column), `development_phases` (name, status, completion, position; unique `(development_id, id)`), `development_unit_types` (phase by composite FK `development_unit_types_phase_fk` CASCADE, bedrooms / bathrooms 0-50, size range `development_unit_types_size_order`, price from / to numeric(20,4) with currency or `price_on_request` `development_unit_types_price_shape`, units total / available `development_unit_types_units_order`), `development_photos` (as `listing_photos` after 0020: url or variants `development_photos_one_source`, checksum, alt ≤ 250, `approved_at`, `uploaded_by` SET NULL) and `development_listings` (listing PRIMARY KEY CASCADE: one project per listing; unit type by composite FK `development_listings_unit_type_fk` ON DELETE SET NULL (unit_type_id), Postgres 15+). `business_enquiries` gains `development_id` / `development_unit_type_id` (SET NULL) and the snapshots `development_name`, `development_market`, `development_slug`, `development_unit_type_name` (`business_enquiries_development_snapshot`). `reference_items_set_check` + `development_amenities` (14 rows). `admin_audit_events_target_type_allowed` + DEVELOPMENT. Not in Elasticsearch (no reindex). The development and staging seed (`devseed.SeedPostgresDevelopments`) adds the Sample Developments projects. Account deletion unlinks the person (`developments.ForgetUserTx`). New tables, new nullable columns, wider checks; re-runnable.
- `0045_markets_legal_tax` (API 0.43.0): `legal_document_versions` (kind TERMS / PRIVACY / COOKIES / REFUND / EULA / AGENT_TERMS `legal_document_versions_kind_allowed`, `market_code` GLOBAL or ISO 3166-1 alpha-2 `legal_document_versions_market_format`, `lang` `legal_document_versions_lang_format`, `version_label` 1-32 printable ASCII `legal_document_versions_label_format`, unique per kind + market + lang `legal_document_versions_label_unique`, `effective_from`, status DRAFT / PUBLISHED / RETIRED `legal_document_versions_status_allowed`, `body_md` 1-200000 `legal_document_versions_body_length`, `summary_of_changes` `legal_document_versions_summary_length`, `requires_reacceptance`, `draft_notice`, `created_by` / `published_by` SET NULL with names, `published_at` `legal_document_versions_published_when_done`, `retired_at` `legal_document_versions_retired_when_done`; trigger `legal_document_versions_frozen`: a PUBLISHED / RETIRED row only retires or is unlinked, only a DRAFT is deleted; partial index `legal_document_versions_resolve_idx`). `user_consents`: `user_consents_type_check` replaced by `user_consents_type_allowed` (+ TERMS, COOKIES, REFUND, EULA, AGENT_TERMS), new nullable `market_code` (`user_consents_market_format`) and `context` REGISTRATION / REACCEPTANCE (`user_consents_context_allowed`), index `user_consents_user_type_idx`. `tax_rules` (market FK `markets.code`, `tax_name` `tax_rules_name_length`, `rate numeric(7,4)` 0-100 `tax_rules_rate_range`, `applies_to` SUBSCRIPTIONS / VENUE_BOOKINGS / LISTING_FEES `tax_rules_applies_to_allowed`, `inclusive`, `effective_from` / `effective_to` exclusive `tax_rules_period_order`, `registration_number_label` `tax_rules_registration_label_length`, status ACTIVE / RETIRED `tax_rules_status_allowed`, `is_sample`, `notes` `tax_rules_notes_length`; overlap refused by the service under an advisory lock). `admin_audit_events_target_type_allowed` + LEGAL_DOCUMENT_VERSION, TAX_RULE. **Inserts no rows** (production has no legal document and no tax rule); the development and staging seed (`devseed.SeedPostgresLegalTax`) adds the GLOBAL placeholders (PUBLISHED, body "DRAFT — awaiting legal review"), ZW / GB DRAFT copies and the SAMPLE tax rules. Account deletion keeps the acceptances and unlinks creators / publishers (`legal.ForgetUserTx`). New tables, new nullable columns, a wider check; re-runnable.
- `0044_online_payment` (API 0.42.0): `payments` (subscription RESTRICT, invoice number and subscriber snapshot, market, `provider` `payments_provider_format`, `provider_ref` / `payment_ref` (unique per provider `payments_provider_ref_unique`), `idempotency_key` `payments_idempotency_key_unique`, invoice `amount` / `currency_code`, `paid_amount` / `paid_currency_code`, `refunded_amount` `payments_refunded_in_range`, `status` OPEN / SUCCEEDED / FAILED / EXPIRED / REVIEW / PARTIALLY_REFUNDED / REFUNDED `payments_status_allowed`, hosted URL, `expires_at`, return and cancel URLs, review and failure reasons, `opened_by` SET NULL and name, timestamps; at most one OPEN per subscription `payments_one_open_per_subscription`), `payment_refunds` (payment RESTRICT, amount, currency, reason, PENDING / SUCCEEDED / FAILED `payment_refunds_status_allowed`, `provider_refund_ref` unique per payment, `idempotency_key` unique, requester SET NULL and name, completed) and `payment_events` (provider, `event_id` unique per provider `payment_events_one_per_provider_event`, type, references, amount, `processing_started_at` lease, `processed_at`, outcome, payment SET NULL). `subscription_plans.package_code` (`subscription_plans_package_code_format`); `subscriptions.payment_recorded_by_kind` ADMIN / GATEWAY (`subscriptions_payment_recorder_allowed`) and `payment_provider`; `subscription_events` CHECKs re-created as `subscription_events_action_allowed` (+ REFUNDED) and `subscription_events_actor_allowed` (+ GATEWAY); `listing_package_assignments.subscription_id` (SET NULL, INVOICE only `listing_package_assignments_subscription_is_invoice`, one per subscription `listing_package_assignments_one_per_subscription`). Account deletion keeps the payments and unlinks the person explicitly. New tables, new nullable or defaulted columns and wider checks.
- `0043_malware_scanning` (API 0.41.0): `file_scans` (one per uploaded document by `object_key` `file_scans_object_key_unique` / `file_scans_object_key_format`, `kind` ONBOARDING_DOCUMENT / APPLICATION_FILE / LEDGER_ATTACHMENT / AGREEMENT_DOCUMENT `file_scans_kind_known`, `uploaded_by` SET NULL, status PENDING / CLEAN / INFECTED / LEGACY_UNSCANNED `file_scans_status_known`, `signature` only when INFECTED `file_scans_signature_when_infected` / `file_scans_signature_size`, `size_bytes`, `attempts`, `last_error`, `next_attempt_at` (required while PENDING `file_scans_pending_scheduled`), `scanned_at` (`file_scans_scanned_when_done`); indexes `file_scans_due_idx`, `file_scans_status_idx`, `file_scans_uploaded_by_idx`). Every document stored before it is backfilled as LEGACY_UNSCANNED. Account deletion unlinks the uploader (`malware.ForgetUploaderTx`). New table; re-runnable.
- `0042_admin_mfa` (API 0.40.0): `admin_mfa_totp` (one per user, primary key and cascade on `users`; `secret_sealed` AES-256-GCM sealed secret 29-512 bytes `admin_mfa_totp_sealed_size`; `confirmed_at` NULL while the setup is pending; `last_used_step` for replay protection, never negative `admin_mfa_totp_step_not_negative`; `created_at`, `updated_at`) and `admin_mfa_recovery_codes` (id, user, Argon2id `code_hash` 20-400 characters `admin_mfa_recovery_codes_hash_size`, `used_at`, `created_at`; index `admin_mfa_recovery_codes_user_idx`). `sign_in_challenges` gains `code_verified_at` (the emailed code was accepted; administrators only, `sign_in_challenges_mfa_admin_only`) and `mfa_attempts` (`sign_in_challenges_mfa_attempts_min`). Account deletion deletes the MFA rows explicitly. New tables and new columns with defaults.
- API 0.39.0 (calendar invitations) adds no migration: SEQUENCE is derived from the existing timelines.

- `0041_marketing_consent` (API 0.37.1): `marketing_consent_events` (user, `opt_in`, `changed_at`, `source` web / app / api `marketing_consent_events_source_allowed`, `context` REGISTRATION / PREFERENCES / MIGRATION / ADMIN / UNSUBSCRIBE `marketing_consent_events_context_allowed`, `consent_text_version` 1-40 characters `marketing_consent_events_version_length` and only on opt-in `marketing_consent_events_version_on_opt_in`; index `marketing_consent_events_by_user`), append-only through the trigger `marketing_consent_events_no_update`. Backfills one MIGRATION record (at the account's `created_at`, source api) for every account already opted in. `marketing_consent_texts` (`version` primary key, `text`, `status` DRAFT / APPROVED, `created_at`; immutable through the trigger `marketing_consent_texts_immutable`), with `marketing-2026-01` inserted as DRAFT. New tables, triggers and inserts.
- `0040_listing_packages` (API 0.37.0): `listing_packages` (market by code, code unique per market `listing_packages_code_per_market`, name, description, `listing_limit` 0-100000, active, `is_default` with at most one per market `listing_packages_one_default` and only when active `listing_packages_default_is_active`, sort, creator), `listing_package_prices` (per package and billing term MONTHLY / QUARTERLY / ANNUAL, `listing_package_prices_one_per_term`; exact numeric amount, currency, `sample`; cascade with the package) and `listing_package_assignments` (organisation XOR user `listing_package_assignments_one_account`, market, package RESTRICT, `starts_on` / `ends_on` UTC dates inclusive `listing_package_assignments_dates_ordered`, source ADMIN / INVOICE, note, assigner and name, `cancelled_at` / `cancelled_by`). The admin audit target CHECK `admin_audit_events_target_type_allowed` gains `LISTING_PACKAGE` and `ORGANISATION`. Nothing is seeded by the migration. New tables and a wider check.
- `0039_provider_reviews` (API 0.36.4): `business_enquiries` gains `replied_at` / `replied_by` (`business_enquiries_replied_after_created`). `provider_reviews` (the profile with a snapshot of its organisation, kind, slug and name; reviewer kind CUSTOMER / ORGANISATION / DIRECT_OWNER, user, organisation (`provider_reviews_reviewer_shape`), public name, author; basis kind and id, unique together (`provider_reviews_one_per_basis`); rating 1-5, text 20-2000; status PENDING / PUBLISHED / HIDDEN / REJECTED; the last moderation; the reply and its removal; `version`) and `provider_review_reports` (one OPEN report per reporter and review, `provider_review_reports_one_open`; cascade with the review). The admin audit target CHECK is re-created as `admin_audit_events_target_type_allowed` with `PROVIDER_REVIEW` (the old `admin_audit_events_target_type_check` is dropped). New tables, nullable columns and a wider check.
- `0038_inspections` (API 0.36.3): `inspections` (book XOR, property, unit tied to its property by a composite key, tenancy ON DELETE SET NULL, type, status with completion / cancellation state CHECKs, `scheduled_for`, inspector, template code and label, the checklist with its findings as JSONB, summary, sign-off names, cancel reason, `version`; reference `RPI-<year>-<6 digits>` from `inspection_reference_seq`, unique) with the trigger `inspections_freeze` (a COMPLETED or CANCELLED row changes only by unlinking people or its tenancy link being cleared), and `inspection_photos` (per checklist item, JPEG only, unique object key, cascade with the inspection). `reference_items` gains `inspection_checklist_templates` with 59 rows (2 templates); the organisation audit checks gain `INSPECTION_COMPLETED`, `INSPECTION_CANCELLED` and the `INSPECTION` target. New tables, a trigger and wider checks.
- `0037_maintenance` (API 0.36.2): `maintenance_requests` (book XOR, property, unit tied to its property by a composite key, tenancy ON DELETE SET NULL, category / priority codes with labels, status, raiser, the current provider and enquiry, assignment count; reference `RPM-<year>-<6 digits>` from `maintenance_reference_seq`, unique), `maintenance_events` (the timeline), `maintenance_photos` (JPEG only, unique object key) and `maintenance_costs` (exactly one `ledger_entry_id`, unique). `ledger_entries` gains `property_id` (EXPENSE and REVERSAL only, `ledger_entries_property_type_rule`, immutable through the replaced guard function); `reference_items` gains `maintenance_categories` and `maintenance_priorities` with their rows; the organisation audit checks gain the `MAINTENANCE_*` actions and the `MAINTENANCE_REQUEST` target. New tables, a nullable column and wider checks; the down migration drops the column (the EXPENSE entries stay).
- `0036_rent_schedule` (API 0.36.1): `rent_dues` (one per tenancy and due date, `numeric(20,4)` + ISO 4217, a waiver with its reason; unique `(id, tenancy_id)`), `rent_receipts` (method, received date, reference, the tenant contact DEFERRABLE INITIALLY DEFERRED, exactly one `ledger_entry_id` (unique), the reversal: `reversed_at` / `reversal_entry_id` / `reversed_on` all set or all NULL) and `rent_allocations` (receipt and due tied to the same tenancy by composite foreign keys). `ledger_entries`: the CHECK that kept `tenancy_id` NULL is replaced by `ledger_entries_tenancy_type_check` (RENT_RECEIVED and REVERSAL only) plus an index; the organisation audit check gains `RENT_RECEIPT_RECORDED` and `RENT_DUE_WAIVED`. New tables and a wider check; the down migration restores the old CHECK `NOT VALID` (posted entries are never deleted).
- `0035_properties_tenancies` (API 0.36.0): `properties` (organisation_id XOR book_user_id, archived by `deleted_at`, optional `listing_id` ON DELETE SET NULL), `property_units` (live label unique per property, case-insensitive; unique `(id, property_id)`), `tenancies` (rent `numeric(20,4)` + ISO 4217, frequency, due day, deposit in the rent currency, dates, DRAFT / ACTIVE / ENDED / TERMINATED with state CHECKs, one ACTIVE per unit or whole property by a partial unique index, the unit tied to its property by a composite foreign key, `lease_id` ON DELETE SET NULL) and `tenancy_parties` (TENANT / LANDLORD contacts; the contact key DEFERRABLE INITIALLY DEFERRED for the development re-seed). `ledger_entries.tenancy_id` gains its foreign key; the organisation audit checks gain the `TENANCY_*` actions and the `TENANCY` target. New tables, a key on an always-NULL column and wider checks.
- `0034_notifications_push` (API 0.35.0): `user_devices` (unique `token`, `session_id` of the
  registering session), `notifications` (unique `(source_event_id, user_id)`: one per event and
  recipient; safe text; `read_at`), `notification_deliveries` (PUSH per device / EMAIL; PENDING,
  ATTEMPTED, PROVIDER_ACCEPTED, FAILED, DEAD; unique `(notification_id, channel, target)`;
  `device_id` ON DELETE SET NULL), `notification_preferences` (email / push per user and type with
  `changed_at` and `source`) and `notification_preference_changes` (the history). New tables only;
  the down migration drops them (LOSES devices, notifications, delivery records and preferences).
- `0033_agent_ledger` (API 0.34.0): `ledger_categories` (per book: organisation_id XOR
  book_user_id, generated `book_id`), `ledger_entries` (amount `numeric(20,4)` > 0, `char(3)`
  currency, the reversal / correction / transfer / settlement self-links, the guard trigger
  `ledger_entries_guard` refusing UPDATE of what was posted and DELETE / TRUNCATE, the CHECK that
  GATEWAY_CONFIRMED needs a `gateway_reference`, the partial unique index of one live opening
  balance per book, currency, method and branch), `ledger_entry_notes` and `ledger_attachments`;
  widens the `organisation_audit_events` action and target CHECKs (`LEDGER_*`, `LEDGER_ENTRY`,
  `LEDGER_BOOK`). The down migration drops the tables (LOSES every book).
- `0032_venue_hourly` (API 0.33.0): `CREATE EXTENSION IF NOT EXISTS btree_gist`; adds
  `reference_items.hourly_bookable` (false on property types, NULL elsewhere) and the hourly
  property types (showroom, event-space, hall, conference-room, studio; office flagged);
  `venue_settings` (also the per-listing lock row), `venue_fees`, `venue_quotes`,
  `venue_bookings` (`occupied` tstzrange generated from `start_at` and `block_end_at`, GiST
  index on `(listing_id, occupied)` for the rows that hold capacity) and
  `venue_booking_events`; widens the `organisation_audit_events` action and target CHECKs. The
  down migration drops the tables and the column and deactivates the new types.
- `0031_questionnaires_applications` (API 0.32.0): `forms` (versions of a family, one DRAFT and
  one PUBLISHED at most, organisation_id XOR owner_user_id, `sections` jsonb, retention 30 to
  3650 days) with the trigger `forms_frozen_after_submission`, `listing_application_intake`,
  `applications` (answers and listing snapshot jsonb, one per applicant, listing and version;
  `application_reference_seq`), `application_files`, `application_file_purges`,
  `application_events` and `application_notes`; widens the `contact_links` kind CHECK
  (APPLICATION) and the `organisation_audit_events` action and target CHECKs. The down migration
  drops the tables and loses every questionnaire and application (stored objects stay).
- `0030_agent_crm` (API 0.31.0): `contacts` (organisation_id XOR book_user_id, stage CHECK,
  `marketing` jsonb, `deleted_at`, `merged_into`), `contact_emails` / `contact_phones`
  (normalised, indexed per book), `contact_tags`, `contact_notes`, `contact_reminders`,
  `contact_events` (activity log) and `contact_links` (kind + id); widens the
  `organisation_audit_events` action and target CHECKs and the reference set CHECK, and adds the
  `contact_types` and `contact_sources` rows. The down migration drops the tables and loses every
  contact book and the contact audit records.
- `0029_listing_analytics` (API 0.30.0): `analytics_events` (raw events; `event_id` primary key,
  `local_day date`, `network cidr` CHECK IPv4 /24 or IPv6 /48 only, enum CHECKs, server events
  without session or network; indexes on `occurred_at`, `(market_code, local_day)`,
  `(listing_id, local_day)` and a partial SPA index), `analytics_late_events`,
  `analytics_listing_daily` (rollup, primary key `(listing_id, day)`) and `analytics_rollup_runs`
  (last run). No foreign key to listings. New tables only; the down migration drops them and loses
  every figure.
- `0028_listing_coordinates` (API 0.29.0): `listings.latitude numeric(9, 6)` and
  `listings.longitude numeric(9, 6)` (nullable; CHECK -90..90 and -180..180, and both or neither)
  and `listings.location_precision text NOT NULL DEFAULT 'EXACT'` (CHECK `EXACT` /
  `APPROXIMATE`); btree index `listings_coordinates_idx (latitude, longitude) WHERE latitude IS
  NOT NULL`. No extension (no PostGIS). Pending revisions carry `latitude`, `longitude`,
  `location_precision` in their jsonb. Additive only. The down migration drops them and loses every
  listing's coordinates.
- `0027_listing_youtube` (API 0.28.0): `listings.youtube_video_id text` (CHECK
  `^[A-Za-z0-9_-]{11}$`) and `listings.youtube_start_seconds integer` (CHECK `>= 0`, and only with a
  video), both nullable. Pending revisions carry the same keys in their jsonb. Two nullable
  columns only. The down migration drops them and loses every listing's video.
- `0026_saved_listings` (API 0.27.0): `saved_listings (user_id, listing_id, saved_at, snapshot
  jsonb, snapshot_at)`, primary key `(user_id, listing_id)`, index `saved_listings_user_saved_idx`
  on `(user_id, saved_at DESC)`. `user_id` references `users` `ON DELETE CASCADE` (account
  deletion, which anonymises the row, deletes the saves itself); `listing_id` references
  `listings` without a cascade (kept as history; only never-published listings can be deleted).
  A new table only. The down migration drops it and loses every save.
- `0025_templates_and_stamps` (API 0.26.0): lease templates owned by organisations
  (`lease_templates.organisation_id`, `NULL` for a sample; index
  `lease_templates_organisation_idx`), `lease_templates_review_check` relaxed so approval no longer
  needs a legal review, `agreement_fields_type_check` widened to `STAMP`, and the jsonb columns
  `agreement_recipients.stamp` (the stamp a signer applied) and `user_signatures.stamp` (a saved
  stamp). Only new nullable columns and changed checks. The down migration retires organisation
  templates first (so they are never offered as samples) and loses every saved and applied stamp
  and the STAMP fields (a completed PDF keeps the stamp drawn on its pages).
- `0024_admin_area` (API 0.25.0): the administrators' area. Suspension and deletion on `users`
  (`suspended_at`, `suspension_reason`, `suspended_by`, `deleted_at`; a deleted account is
  anonymised in place, never removed), `admin_audit_events` (immutable), `subscription_plans` and
  `subscription_plan_prices`, `subscriptions` (at most one `PENDING_PAYMENT` per subscriber) and
  `subscription_events` (immutable). New nullable columns, tables, a sequence and indexes; the one
  `UPDATE` backfills `suspended_at` / `deleted_at` for accounts already suspended or deleted. The
  down migration loses plans, subscriptions, the audit trail and suspension details, but never
  gives a suspended or deleted account back its access or data.
- `0023_agreements` adds only new objects, each `IF NOT EXISTS`: `agreement_reference_seq`;
  `agreements` (unique `reference`, partial indexes on both SHA-256 for verification),
  `agreement_recipients` (unique `(agreement_id, email)`), `agreement_fields`, `agreement_links`
  (`agreement_links_token_hash_key`), `agreement_events` (immutable, trigger
  `agreement_events_no_update`), `agreement_jobs` (reminder idempotency), `user_signatures`; and
  the foreign key `leases_agreement_id_fkey` (added in a `DO` block only when missing). The down
  migration drops them (not the objects in the bucket).
- `0022_leases` adds only new objects, each `IF NOT EXISTS`: the sequence `lease_reference_seq`;
  `lease_templates` (unique `(family_id, version)` `lease_templates_family_version_key`, at most one
  `DRAFT` and one `APPROVED` per family: partial unique indexes `lease_templates_one_draft_key`,
  `lease_templates_one_approved_key`); `lease_template_events` and `lease_events` (immutable rows,
  triggers `*_no_update`); `leases` (unique `reference`, `agreement_id` without a foreign key until
  0023); `lease_documents` (one row per lease and kind, `lease_documents_lease_kind_key`). The down
  migration drops them all (it does not delete the PDF objects in the bucket).
- `0021_agency_teams` widens `memberships_status_check` to `ACTIVE`, `INVITED`, `SUSPENDED`,
  `REMOVED` and adds `memberships.job_title` (1-80 characters, nullable); new tables
  `organisation_invitations` (token stored as its SHA-256, `organisation_invitations_token_hash_key`;
  at most one `PENDING` invitation per organisation and address, partial unique index
  `organisation_invitations_one_pending_key`) and `organisation_audit_events` (immutable rows,
  trigger `organisation_audit_events_no_update`, index `(organisation_id, created_at DESC, id DESC)`);
  `appointment_timeline_event_check` gains `UNASSIGNED`. Every object is created `IF NOT EXISTS`
  and no existing row is rewritten. The down migration drops the two tables and the job titles,
  deletes `UNASSIGNED` timeline rows and turns `SUSPENDED` memberships into `REMOVED` (never back
  into `ACTIVE`).
- `0020_listing_management` adds the listing statuses `CHANGES_REQUESTED`, `REJECTED` and
  `ARCHIVED` (`ALTER TYPE ... ADD VALUE`, unused in the same transaction), to `listings`
  `created_by`, `submitted_at`, `submitted_by`, `review_reason` (1-1000 characters),
  `revision` (jsonb, the pending changes of a published listing) and `revision_status`
  (`EDITING`, `PENDING_REVIEW`, `CHANGES_REQUESTED`, `REJECTED`; present exactly when
  `revision` is), makes `price_amount` nullable (a draft without a price), and adds the partial
  index `listings_review_queue_idx`. `listing_photos` gets `checksum`, `variants` (jsonb:
  size, object key, width, height, bytes), `approved_at` (every existing photo is approved) and
  `uploaded_by`; `url` becomes nullable (an upload has variants instead, exactly one of the
  two). New table `listing_timeline` (immutable rows, trigger `listing_timeline_no_update`).
  The down migration deletes uploaded photo rows (their files stay in the bucket), sets the new
  statuses back to `DRAFT` and cannot remove the enum values.
- `0018_request_work` adds to `property_requests` the lead `stage` (text, NOT NULL, default
  `NEW_ENQUIRY`, CHECK on the eight stages; every existing row becomes `NEW_ENQUIRY`),
  `stage_changed_at`, `stage_changed_by`, `exception_resolved_at`, `exception_resolved_by`,
  `exception_resolution_note` (at most 2000 characters; resolved only for a
  `ROUTING_EXCEPTION`), and the index `property_requests_recipient_org_stage_idx`; widens
  `request_timeline_actor_type_check` with `ADMIN`; and creates `property_request_notes` (`id`,
  `request_id` cascade, `author_user_id`, `author_name`, `body` 1 to 2000 characters,
  `created_at`) with the index `property_request_notes_request_idx`. Stage history lives in
  `request_timeline` (`STAGE_CHANGED`, detail `{from, to, actor_name}`). **The API of this
  release needs it: it selects these columns.** Its down migration loses every note, every
  stage and every resolution, and deletes `ADMIN` timeline rows. **Not run by me**, and the
  Postgres queries that use it are compiled only.
- `0017_registration_captchas` adds `registration_captchas` (`id` text primary key,
  `answer_hash`, `expires_at`, `used_at`, `created_at`) and the index
  `registration_captchas_expires_at_idx`. One new table; no existing table or row is touched.
  **The API of this release needs it: without it `GET /v1/auth/captcha` and every registration
  fail.** Its down migration drops the table (nothing of value is lost). **Not run by me**, and
  the two Postgres queries that use it are compiled only.
- `0016_logos_and_trust` adds the logo columns of `directory_profiles` (`logo_object_key`,
  `logo_content_type`, `logo_width`, `logo_height`, `logo_checksum`, `logo_size_bytes`,
  `logo_updated_at`, with CHECK constraints: all or none, PNG or JPEG, 128 to 4096 pixels, at most
  2 MB) and `verified_at`, `listings.reviewed_at` / `reviewed_by`, the `LOGO` reference row and
  its nine per-role rows (upserts: running them twice changes nothing). It only adds nullable
  columns; one `UPDATE` backfills `verified_at` with `created_at` on profiles that are already
  verified. **The API of this release needs it: it selects these columns.** Its down migration
  drops every logo reference (the objects stay in storage). **Not run by me.**
- `0015_availability` adds `listing_availability` (unavailable ranges of nights per listing,
  with the private reason and note), `listing_availability_audit` (the trail),
  `listings.availability_updated_at`, two indexes for "my listings", the stay columns of
  `property_requests` (`check_in`, `check_out`, `guests`, all NULL for other request types) and
  the `STAY_REQUEST` reference row (an upsert). It only adds tables and nullable columns. Ranges
  are kept non-overlapping by the API inside a transaction that locks the listing row; there is
  no exclusion constraint (it would need the `btree_gist` extension). Its down migration deletes
  every calendar. **Not run by me.**
- `0014_global_countries` upserts every ISO 3166-1 country into `markets` (by code: the five
  existing rows keep their IDs; AE, BW and ZA become ACTIVE), adds `markets.enabled`, the
  free-text place columns `city_name` / `region_name` on `listings` and `directory_profiles`
  (with CHECK constraints: 1–80 characters, no markup, never next to a city code),
  `country_code` and `city_name` on `onboarding_applications` (a business application's
  country is backfilled from the market it already had) and an index on
  `directory_profiles (market_id, kind)`. It rewrites no other existing value. Its down
  migration is lossy and says how: the added countries are kept and go back to PLANNED, and a
  listing or profile whose city was free text is taken off the site. Written for the staging
  database that already holds data, but **not run by me**.
- `0013_uk_short_stays` adds the United Kingdom market (ACTIVE) and a display order for markets;
  the short-stay category, its five property types, the `price_periods` set and the United
  Kingdom's regions, cities and areas (upserts that keep a row's `active` flag); `max_guests` and
  `min_nights` on listings; `night` as a price period; and CHECK constraints for the short-stay
  rules. Its down migration is lossy and says how: short-stay listings are closed, not deleted,
  and the UK market returns to PLANNED instead of being deleted. Written for the staging
  database that already holds data, but **not run by me**.
- `0012_locations` adds the `regions` reference set with Zimbabwe's ten provinces, links each city
  to its region (`parent_code`), and adds indexes for the country and floor-area filters. Listings
  and profiles get no new column: a region is always the region of the city. **Never run.**
- `0011_appointments` adds `appointments` (type, status, the listing or profile with a snapshot of
  their public names, the link to the property request, the customer's private contact snapshot,
  the routed organisation and assigned user, `start_at` / `duration_minutes` / `end_at`, the
  proposed time and who proposed it, cancellation, the idempotency key of explicit creation) and
  the immutable `appointment_timeline` (a trigger refuses updates). The no-overlap rule for
  confirmed appointments is a locked check in the store, not a constraint (see "Appointments").
  **Never run.**
- `0010_real_estate_roles` renames and regroups the account roles (codes unchanged), adds
  SALES_AGENT, LETTINGS_AGENT, PROPERTY_MANAGER and SOLICITOR, the organisation kinds
  `PROPERTY_MANAGER` and `LAW_FIRM`, the `organisation_kinds` reference set, the `solicitors` directory and the PRACTISING_CERTIFICATE
  document type, `specialism` on agent profiles and on the outcome table; it turns every
  `LANDLORD` organisation into `PROPERTY_MANAGER` and moves its owners' LANDLORD role and
  applications to PROPERTY_MANAGER, makes LANDLORD an individual-owner role, and **resets the
  per-role document requirements** to the new baseline. LANDLORD applications that were not
  approved stay LANDLORD under the new requirements. Its down migration is lossy. **Never run.**
- `0009_onboarding` adds the `onboarding_documents` reference set, `onboarding_role_outcomes`,
  `onboarding_role_documents`, `onboarding_applications` (one current application per user and
  role), `onboarding_documents` and the immutable `onboarding_history`; it backfills a DRAFT
  application for every role that is already `PENDING_VERIFICATION`, and narrows the public
  profile index to verified profiles. **Never run**, like every migration after 0001: its SQL, the
  onboarding Postgres store and the one-transaction approval have only been compiled.
- Money is `numeric` + ISO 4217 currency code; all timestamps are `timestamptz` stored in UTC.
- `0002_identity` adds credentials and verification to `users`, `user_consents`, organisation
  `kind`, the `OWNER/MANAGER/AGENT/STAFF` membership roles with a market, `platform_role_assignments`,
  `sessions`, `email_verification_tokens` and `password_reset_tokens`. It renames the old `ADMIN`
  membership role to `MANAGER`; its down migration is lossy (`MANAGER`→`ADMIN`, `STAFF`→`AGENT`).
  **This migration and the Postgres identity/inbox queries have only been compiled, never run
  against a database** (no Postgres on the development machine) — run them against a scratch
  database before relying on them.
- `0003_account_roles` adds `user_account_roles` (self-declared roles with a status) and records
  every pre-existing user as `CONSUMER` / `ACTIVE`. Like `0002`, it and its store queries are
  **compiled only, never run against a database**.
- `0004_session_scope` adds `sessions.scope` (`customer` / `admin`, existing rows become
  `customer`) and a constraint that admin sessions are cookie-only. Also **never run**.
- `0005_business_enquiries` adds `directory_profiles` (no contact columns), `business_enquiries`,
  `business_enquiry_timeline` and `customer_activity.business_enquiry_id`. Also **never run**,
  like its store queries.
- `0006_email_messages` adds the per-email delivery record (`ATTEMPTED` / `PROVIDER_ACCEPTED` /
  `FAILED`, provider message ID; no address, subject or body). Also **never run**.
- `0007_reference_data` adds `reference_items`, inserts the baseline rows, and replaces the fixed
  vocabulary `CHECK` constraints with API validation (plus a guard against platform roles). Also
  **never run**.
- `0008_listings_directory` adds the public content of listings (slug, reference, deal, category,
  type, location, rooms, areas, text, features, photos) and of directory profiles (role, tagline,
  about, city, trade, verified, sponsored, areas, services, offerings). No contact columns. Also
  **never run**, like the Postgres search and directory queries.
- `0001_init` seeds the four markets. It does **not** seed listings or profiles: against a real
  database `POST /v1/property-requests` and `POST /v1/business-enquiries` return 404 until they
  exist (in development, `cmd/seed` creates the sample ones).

```powershell
$env:DATABASE_URL = "<connection string>"   # never commit or paste a real one
go run ./cmd/migrate status
go run ./cmd/migrate up
```

## Worker and outbox

`cmd/worker` polls `outbox_events` with `FOR UPDATE SKIP LOCKED`, leases a batch (row-level
`locked_until`, so a crashed worker's events are retried when the lease expires), dispatches by
event type, then marks events processed. Failures back off exponentially (30 s doubling, capped at
1 h) and are dead-lettered (`dead_letter_at`) after 8 attempts.

Delivery is **at-least-once**, so handlers must be idempotent — key side effects on the event ID.
The worker handles `PROPERTY_REQUEST_CREATED`, `BUSINESS_ENQUIRY_CREATED`, `USER_EMAIL_VERIFIED`,
`PASSWORD_CHANGED` and `ADMIN_SIGNED_IN` by sending the notification emails described under
"Email"; each email is keyed on the event ID and recipient, so a redelivered event sends nothing
twice. `PROPERTY_REQUEST_STAGE_CHANGED` and `PROPERTY_REQUEST_EXCEPTION_RESOLVED` (identifiers
and stages only), `LISTING_SAVED` and `LISTING_UNSAVED` (identifiers only) are acknowledged without sending anything: no email exists for them yet. `LISTING_SAVED`, `PROPERTY_REQUEST_CREATED`, `APPOINTMENT_REQUESTED`, `APPOINTMENT_CONFIRMED` and `LISTING_RELISTED` are also recorded as server-side analytics events (after the email handler, `outbox.Chain`: a transient failure of either retries both, each idempotent). The same three creation events link the new item to the matching contacts of its organisation (see "Agent CRM"), and every minute the worker writes `CONTACT_REMINDER_DUE` for due contact reminders (acknowledged). Every 15 minutes the worker recomputes the analytics rollups and applies `ANALYTICS_RAW_RETENTION_DAYS` (see "Listing analytics"). The four application events send their emails, `APPLICATION_SUBMITTED` is also linked by the CRM and counted as `application_submit`, and every hour the worker runs the application retention job and removes deleted application files from the object storage (the worker opens the file storage for this; see "Questionnaires and applications"). Every notification email's recipient with an account also gets one in-app notification per event (unique per event and recipient), and the worker's push sender sends the queued pushes after the commit (see "Notifications and push"); the emails of non-essential types follow the recipient's preferences. A handler can mark a failure permanent, which dead-letters the event at once. **The worker
sends real email when `RESEND_API_KEY` is set.** Without `DATABASE_URL` in development the worker
logs that it is idle and waits for shutdown (the API then handles the events in-process).
Every 5 minutes the worker also runs the payment reconciliation (see "Online payment of
subscription invoices"): expired checkouts, provider look-ups and interrupted follow-ups.
At start and then every hour it runs the listing expiry sweep (see "Listing expiry and
reminders"): `LISTING_EXPIRING` and `LISTING_EXPIRED` send the advertiser's emails, and
`LISTING_EXPIRED` also updates the search index (both handlers chained with `Dispatcher.Also`).

## Operational notes and known limitations

- **Rate limiter is per process.** `POST /v1/property-requests` is limited per client IP (burst 10,
  refill 10/min), `POST /v1/auth/*` shares a second bucket (burst 20, refill 20/min),
  `GET /v1/auth/captcha` has its own (burst 30, refill 30/min) and
  `POST /v1/admin/auth/login` has a third (burst 5, refill 5/min) and `POST /v1/business-enquiries`
  a fourth (burst 10, refill 10/min), all in-memory token buckets. With more than one API instance each has its own
  buckets, so this **must be replaced by a shared limiter (Redis/Valkey or the edge) before
  autoscaling**.
- **Client IP.** The limiter and access log use the TCP peer address and deliberately ignore
  `X-Forwarded-For`. Behind a load balancer every client would share the balancer's IP; add
  trusted-proxy handling in `httpx.ClientIP` when the deployment topology is known.
- **Idempotency keys are still global.** The customer's user ID is part of the compared content, so
  a key reused by a different customer is a `409`, never a replay of someone else's receipt; the
  key space itself is not yet partitioned per customer.
- `trace_id` is the request ID for now; it becomes a real trace ID when tracing is introduced.
- Logs are JSON on stdout. Message text and contact details are personal data and are never logged.

## Docker

```
docker build -f deploy/docker/Dockerfile --target api    -t rp365-api .
docker build -f deploy/docker/Dockerfile --target worker -t rp365-worker .
docker compose up -d        # local Postgres 17 + MinIO
```

Images are distroless and run as a non-root user.

## Branches

- `main` – integration baseline
- `dev` – day-to-day development
- `production` – what is deployed

CI (`.github/workflows/ci.yml`) runs gofmt, `go vet`, `go test -race` and `go build` on pushes to
these branches and on pull requests.
