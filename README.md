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
`…000000000099` is **EXPIRED** (requests get `410`).

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
| `STORAGE_DRIVER`, `STORAGE_LOCAL_DIR`, `S3_*` | | Private file storage for onboarding documents: see "File storage". **Staging and production must use `STORAGE_DRIVER=s3`** |
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
| GET | `/v1/me/profiles` | The directory profiles the caller may manage |
| PUT, DELETE | `/v1/me/profiles/{kind}/{slug}/logo` | Set (multipart) or remove a profile's logo |
| GET | `/v1/trust/summary` | How many advertisers are verified and how many listings are public (public, cacheable) |
| POST | `/v1/property-requests` | The single entry point for every customer enquiry (guest or signed-in) |
| POST | `/v1/business-enquiries` | The only way to contact a directory profile (guest or signed-in) |
| GET | `/v1/inbox/business-enquiries` | Business enquiries received by the caller's organisation(s) |
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
| GET | `/v1/admin/overview` | Counts an administrator starts from, sections by platform role |
| GET, POST | `/v1/admin/routing-exceptions`, `/{id}/resolve` | Requests nobody could receive, and marking one as dealt with |
| POST | `/v1/appointments` | A signed-in customer asks for a viewing or a consultation |
| GET, POST | `/v1/me/appointments`, `/{id}`, `/{id}/cancel`, `/{id}/reschedule`, `/{id}/accept` | The customer's own appointments |
| GET, POST | `/v1/inbox/appointments`, `/{id}`, `/{id}/confirm`, `/reschedule`, `/cancel`, `/complete`, `/no-show`, `/assign` | Appointments received by a professional (list, calendar and actions) |
| GET | `/v1/inbox/summary` | Badge counts: new requests, new enquiries, appointments |
| GET | `/v1/listings/{slug}/availability` | A short stay's calendar as the public sees it: available or not per night |
| GET | `/v1/me/listings` | The listings the caller may manage (read-only summaries) |
| GET, PUT | `/v1/me/listings/{id}/availability` | The lister's calendar of a short stay: read it, mark nights available, blocked or booked |
| PATCH | `/v1/me/listings/{id}/stay-rules` | A short stay's `min_nights` and `max_guests` |

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
- **Team list** – `GET /v1/agent/organisations/{id}/members`, OWNER and MANAGER only (another
  member `403`, a non-member `404`); name, role label, status, no contact details.
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
change; assigning a request to a person; follow-up dates and reminders; editing or deleting a
note; listing analytics, conversion funnels and response-time KPIs (no event data exists);
seeker favourites, saved searches and alerts; inviting, changing or removing members.

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
| GET | `/v1/me/listings?category=&status=&page=&page_size=` | lister | The listings the caller may manage, as summaries. **Read-only: creating and editing listings is still not built** |
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
- **Photos are not uploaded anywhere.** `photos[].url` is a site-relative path
  (`/assets/images/<file>.jpg`) naming the web app's own sample image files, returned exactly as
  stored. **Object storage (MinIO) and uploads are not built.**

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

- **The listing itself is not reviewed.** `listing_reviewed` exists (`listings.reviewed_at`,
  `reviewed_by`) but **only the sample-data seed sets it**: there are still no endpoints to
  create, submit, review or publish a listing. The **listing moderation workflow** (an advertiser
  submits -> `PENDING_REVIEW` -> a Moderator approves or rejects with a reason, with an audit
  trail) is specified and not built. Until it exists, **the only verified claim that is enforced
  end to end is ADVERTISER verification**. A real listing created by any future endpoint will say
  `listing_reviewed: false` until that workflow sets it.
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
city, suburb, region, features, market — the country —, currency, advertiser type), a `boolean`
`has_photos`, `text` with a `keyword` and a `search_as_you_type`
sub-field for title, suburb and city labels, `scaled_float` for the price with the currency kept
separately, integers for bedrooms and bathrooms, dates, a `version`, and a `geo_point` `location`
that is mapped but unused. **A document holds public listing data only** — no customer, no contact
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

> **Listing lifecycle endpoints do not exist yet**, and there is no job that marks listings
> expired. The only code that changes listings today is `cmd/seed`, which now writes these events.
> When publish / edit / pause / relist are built, they must write the matching event in the same
> transaction as the change. Expiry needs no event to be *correct* (the query and the re-check both
> use `expires_at`), only to remove the stale document.

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
"Appointments". **But there are no endpoints to create or publish a listing, and no venue or
short-stay bookings, for any role yet.**

### Specified but not built

So that nobody assumes these exist. They are in the updated functional specification and are
**not** in this backend:

- replies to enquiries and a customer notification when a request moves (the eight lead
  stages, New enquiry to Closed, are built: see "Dashboards and working a request");
- agency principals assigning leads to a person (the `ASSIGNED` stage exists, but it only
  records the stage), follow-up dates on a lead;
- reassigning a routing exception to a recipient (administrators can list and resolve them);
- rental applications;
- appointments beyond viewings and consultations (see "Appointments": venue and short-stay
  bookings, slot configuration, ICS invites, calendar sync, reminders);
- short-stay **bookings**: payments, instant booking, holds, iCal sync, seasonal and per-night
  pricing (the availability calendar itself is built: see "Availability calendar for short
  stays");
- creating, editing and publishing listings (only a short stay's calendar and stay rules can
  be changed through the API);
- the **listing moderation workflow**: submit -> `PENDING_REVIEW` -> approve or reject by a
  Moderator, with reasons and an audit trail. Until it exists `listing_reviewed` is set by the
  sample-data seed only, and the only verified claim enforced end to end is ADVERTISER
  verification (see "Verified advertisers");
- administrator endpoints to unverify / re-verify an organisation and to suspend / reinstate a
  direct owner's role (requested, not started);
- a display name a direct owner can choose (their account name is shown on their listings);
- per-size thumbnails, cropping and moderation of logos, and a CDN (see "Logos");
- subscription plans, and admin-recorded payments, invoices and receipts (the specification has no
  online payment in the mobile app);
- per-country **legal texts, payment gateways and tax rules** (see "Countries": any country can
  be listed in, but none of these is configured for any country);
- a per-city time zone for countries with several zones;
- account deletion;
- adaptive bot protection (registration has a simple built-in arithmetic captcha, see
  "Registration captcha"; nothing stronger), and duplicate or spam detection;
- administrator MFA beyond the emailed sign-in code (an authenticator app or hardware key);
- account suspension (including the "Suspended" onboarding status) and abuse reporting;
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

**Not built:** malware scanning and quarantine (a file is stored and can be downloaded by a
reviewer unscanned: **open it with care**), image re-encoding and EXIF/metadata stripping,
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

| Email | Trigger | Path |
| --- | --- | --- |
| Confirm your email address | Register, resend verification | request |
| Reset your password | Password-reset request (never for administrators) | request |
| You already have an account | Register with an address that is already verified | request |
| Your account is ready (welcome; lists the roles chosen and which are waiting for verification) | Email verified for the first time | outbox `USER_EMAIL_VERIFIED` |
| Your password was changed | Password reset confirmed; `adminctl set-password` | outbox `PASSWORD_CHANGED` |
| Administrator sign-in | Successful `POST /v1/admin/auth/login` | outbox `ADMIN_SIGNED_IN` |
| Request received (to the customer, with the reference) | Property request created | outbox `PROPERTY_REQUEST_CREATED` |
| New property request (to the assigned agent or direct seller, otherwise the organisation's owners and managers); names the listing by title and reference | Property request created | same event |
| Enquiry received (to the customer) | Business enquiry created | outbox `BUSINESS_ENQUIRY_CREATED` |
| New enquiry (to the named agent, otherwise the owning organisation's owners and managers) | Business enquiry created | same event |
| New appointment request (to the assigned professional, otherwise the organisation's owners and managers) | Appointment created | outbox `APPOINTMENT_REQUESTED` |
| Appointment confirmed (to the customer) | Confirmed by the professional, or a proposed time accepted | outbox `APPOINTMENT_CONFIRMED` |
| New time proposed (to the customer) / The customer proposed a new time (to the professional) | A new time proposed by the other party | outbox `APPOINTMENT_RESCHEDULE_REQUESTED` |
| Appointment cancelled (to the customer) / cancelled by the customer (to the professional) | Cancelled by the other party | outbox `APPOINTMENT_CANCELLED` |

**Designed, written and tested, but NOT triggered by anything** (the features do not exist yet):
reply or status update to the customer, listing expiring in 14 / 7 / 1 days, listing expired, relist
confirmed, renewal invoice issued, payment received, account deletion requested, account deletion
completed. Their wording about payments, invoices and deletion is a draft: the house rules say
legal and payment wording needs sign-off per market before release.

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
option for users, and this emailed code is the first step towards that (it proves control of the
mailbox; there is no authenticator app).

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
  answers `202` for a platform-role account but sends nothing: without MFA, a compromised mailbox
  must not become an administrator takeover. Use `adminctl set-password`.
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

> **The administrator login is not production-ready.** The specification requires MFA for
> administrator roles. An administrator account is protected by a password, the emailed sign-in
> code (which proves control of the mailbox, not of a separate device), a per-IP rate limit and a
> short session. There is no authenticator app or hardware key yet.

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

- **MFA for administrators** – the specification requires it for admin roles. Only the emailed
  sign-in code exists (see "Sign-in code"); there is no second factor on a separate device, so the
  administrator login must not be treated as production-ready.
- **Durable audit trail** – administrator sign-ins are only written to the process log.
- **Admin endpoints for reference data** – rows are served and enforced, but can only be changed
  in the database.
- **WhatsApp and in-app notifications.** Recipients and customers are notified by email only.
- **Bounce and complaint handling** – there is no Resend webhook endpoint, so a message that
  Resend accepted and later failed to deliver is never noticed; no suppression list.
- **The emails for features that do not exist yet** (status updates, listing expiry and relisting,
  invoices, payments, account deletion) are written and tested but nothing sends them.
- **Unsubscribe / notification preferences** – every email today is transactional.
- **Listing and profile management** – listings and profiles are read-only: there is no API to
  create, edit, publish, relist, verify or suspend them. They exist only as seeded rows.
- **Photo upload and object storage (MinIO)** – photo URLs are paths to the web app's own static
  sample images.
- **A running search cluster** – the Elasticsearch adapter, indexing handler and `cmd/reindex` are
  written and tested against a fake, but no cluster exists and hosting/licence is an open
  decision; search runs in Postgres until then.
- **Listing lifecycle (publish, edit, pause, relist) and an expiry job** – so nothing but the seed
  emits listing events yet.
- **Metrics** – the search-fallback count is only a log field.
- **Revoking a role or a membership, and invitations** – a membership is created only by an
  approved onboarding application (or the development seed); nothing removes one, and an agency
  cannot invite or confirm its own members. Platform roles are created only by `cmd/adminctl`.
- **Account deletion** and data export.
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
internal/modules/listings          GET /v1/listings, /v1/listings/suggest, /v1/listings/{slug}; Search port (postgres + memory)
internal/modules/directory         GET /v1/directory, /v1/directory/{kind}[/{slug}], /v1/sitemap; profile logos: the public image route and /v1/me/profiles (postgres + memory)
internal/modules/trust             GET /v1/trust/summary: verified advertisers and public listings, counted
internal/modules/reference         GET /v1/reference, /v1/reference/{set}; the rows other modules validate against
internal/modules/identity          /v1/auth/*, /v1/me: accounts, sessions, tokens, roles, Mailer (store: postgres + memory)
migrations/    embedded SQL (NNNN_name.up.sql / .down.sql)
api/openapi.yaml             OpenAPI 3.1 contract
deploy/docker/Dockerfile     one multi-stage file, targets: api, worker, migrate
docker-compose.yml           Postgres 17 + MinIO for local dev when Docker is available
```

Modules depend on `internal/platform`, never on each other's internals. Each module owns its
handler, service and store interface.

### Planned modules (not created yet)

Deliberately no empty stubs. These will be added under `internal/modules/` as they are built:

- `identity` (exists) still lacks organisation membership management, invitations and MFA — see
  "Not built yet"
- `listings` (exists, read-only) – listing CRUD, publication cycles, expiry/relisting
- `media` – image/document upload to object storage (MinIO locally)
- `billing` – plans, payments, invoices (exact decimal money + ISO 4217)
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
and stages only) are acknowledged without sending anything: no email exists for them yet. A handler can mark a failure permanent, which dead-letters the event at once. **The worker
sends real email when `RESEND_API_KEY` is set.** Without `DATABASE_URL` in development the worker
logs that it is idle and waits for shutdown (the API then handles the events in-process).

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
