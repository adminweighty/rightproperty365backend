# RightProperty365 Backend

Go backend for RightProperty365, a multi-country property marketplace (Zimbabwe first).

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
curl.exe http://localhost:8080/v1/markets
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
once in `internal/devseed`; the web app's sample data uses the same names): 23 organisations, 28
accounts, 18 listings and 29 directory profiles. Listing IDs are
`0195c0de-0000-7000-8000-000000000001` … `…000000000018`: 18 **PUBLISHED** listings in market ZW,
each with an organisation **and** a named assigned agent, or a direct seller. Listing
`…000000000099` is **EXPIRED** (requests get `410`).

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
| `sample-developments@example.test` | Sample Developments Owner | Sample Developments | `DEVELOPER` | `OWNER` | `developers/sample-developments` |
| `sample-urban-builders@example.test` | Sample Urban Builders Owner | Sample Urban Builders | `DEVELOPER` | `OWNER` | `developers/sample-urban-builders` |
| `sample-heights@example.test` | Sample Heights Owner | Sample Heights | `DEVELOPER` | `OWNER` | `developers/sample-heights` |
| `sample-property-management@example.test` | Sample Property Management Owner | Sample Property Management | `LANDLORD` | `OWNER` | `property-managers/sample-property-management` |
| `sample-rentals-bulawayo@example.test` | Sample Rentals Bulawayo Owner | Sample Rentals Bulawayo | `LANDLORD` | `OWNER` | `property-managers/sample-rentals-bulawayo` |
| `sample-falls-stays@example.test` | Sample Falls Stays Owner | Sample Falls Stays | `HOST` | `OWNER` | `hosts/sample-falls-stays` |
| `sample-showrooms@example.test` | Sample Showrooms Owner | Sample Showrooms | `HOST` | `OWNER` | `hosts/sample-showrooms` |
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

Account roles of the sample accounts are all `ACTIVE`: `CONSUMER` + `AGENT` for the agents,
`CONSUMER` + `AGENCY_OWNER` for the agency owners, `CONSUMER` + `PRIVATE_SELLER` for Grace,
`CONSUMER` + `DEVELOPER` / `LANDLORD` / `HOST` / `SERVICE_PROVIDER` for the business owners
(matching their organisation's kind), `CONSUMER` for the consumer and the administrator, and
`AGENT` only for `agent-b`.

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
`DATABASE_URL`. It **refuses to run unless `APP_ENV=development` is set
explicitly and `DEV_SEED_PASSWORD` is set**, runs in one transaction and is idempotent (re-running
it resets the sample rows, including the shared password, and touches nothing else). Sample
listings get a placeholder title and price, because this backend does not own listing content yet.
**It has never been run** — there is no Postgres on the development machine; only its
environment guard is unit-tested. Treat the first run as a test.

Sign in and read the organisation inbox (PowerShell; `-SessionVariable` keeps the cookie, and the
`Origin` header is required on cookie-authenticated POSTs):

```powershell
$pw = (Get-Content .env.example | Select-String '^DEV_SEED_PASSWORD=').Line.Split('=', 2)[1]
$login = @{ email = "agent@example.test"; password = $pw } | ConvertTo-Json
Invoke-RestMethod -Method Post -Uri http://localhost:8080/v1/auth/login -ContentType "application/json" -Body $login -SessionVariable s
Invoke-RestMethod -Uri http://localhost:8080/v1/me -WebSession $s
Invoke-RestMethod -Uri http://localhost:8080/v1/agent/property-requests -WebSession $s
```

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
| `ADMIN_SESSION_TTL` | `12h` | Absolute lifetime of an administrator session (Go duration, 1m–24h) |
| `ADMIN_SESSION_IDLE_TTL` | `30m` | Administrator inactivity timeout (Go duration; must not exceed `ADMIN_SESSION_TTL`) |
| `STORAGE_DRIVER`, `STORAGE_LOCAL_DIR`, `S3_*` | | Private file storage for onboarding documents: see "File storage". **Staging and production must use `STORAGE_DRIVER=s3`** |
| `ADMINCTL_PASSWORD` | _(empty)_ | `cmd/adminctl` only: the administrator password, as an alternative to stdin |

## Endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/healthz` | Liveness (always 200) |
| GET | `/readyz` | Readiness (pings the database when configured; reports the store in use) |
| GET | `/v1/markets` | Markets: ZW active; BW, ZA, AE planned |
| GET | `/v1/reference`, `/v1/reference/{set}` | Reference data behind every drop-down, filter and list (public, cacheable) |
| GET | `/v1/listings`, `/v1/listings/suggest`, `/v1/listings/{slug}` | Public listing search, suggestions and detail |
| GET | `/v1/directory`, `/v1/directory/{kind}`, `/v1/directory/{kind}/{slug}`, `/v1/sitemap` | Public directories, profiles and sitemap data |
| POST | `/v1/property-requests` | The single entry point for every customer enquiry (guest or signed-in) |
| POST | `/v1/business-enquiries` | The only way to contact a directory profile (guest or signed-in) |
| GET | `/v1/inbox/business-enquiries` | Business enquiries received by the caller's organisation(s) |
| POST | `/v1/auth/register` | Create an account; always `202 VERIFICATION_SENT` for valid input |
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
| GET | `/v1/agent/property-requests` | Requests received by the caller: their organisation's (scoped by role) and/or their own as a direct seller |

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

## Listings and directories: the public catalogue

The web app reads listings and directory profiles from the API. Everything here is public,
read-only, the same for every caller, and cacheable (`Cache-Control: public, max-age=30,
stale-while-revalidate=120`, a strong `ETag`, `304` on `If-None-Match`).

| Method | Path | Returns |
| --- | --- | --- |
| GET | `/v1/listings` | Search: `{items, page, page_size, total, total_pages, facets}` |
| GET | `/v1/listings/suggest?q=` | Up to 8 `{type: suburb\|city\|listing, label, value}` |
| GET | `/v1/listings/{slug}` | `{listing}`; `410 LISTING_UNAVAILABLE` or `404 LISTING_NOT_FOUND` |
| GET | `/v1/directory` | `{kinds: [{code, label, singular_label, description, count}]}` |
| GET | `/v1/directory/{kind}` | One page of profiles, sponsored first |
| GET | `/v1/directory/{kind}/{slug}` | `{profile}` with its agents and public listings |
| GET | `/v1/sitemap` | Every public listing slug and profile kind/slug with `updated_at` |

**Only PUBLISHED and unexpired listings are ever returned (FR-006).** `status = 'PUBLISHED' AND
expires_at > now` is part of every query that can return a listing. Search sits behind a `Search`
port (`internal/modules/listings`): the Postgres and memory adapters implement every filter,
sort order, facet and suggestion, and an Elasticsearch adapter can be added behind the same port
later. An adapter only proposes IDs; the service re-reads every hit from the database, which
re-checks status and expiry, so an index that is behind cannot bring back an expired listing.

- **Filters** (all optional, combined with AND): `q`, `deal`, `category`, `property_type`, `city`,
  `suburb`, `min_price`, `max_price` (compared only against listings priced in `currency`,
  default `USD`; nothing is converted), `bedrooms_min`, `bathrooms_min`, `features` (repeatable or
  comma-separated; all must be present), `featured`. Codes are reference-data codes; an unknown
  or inactive code is `400 VALIDATION_FAILED` on that parameter.
- **Sort**: `newest` (default), `relevance` (default when `q` is given), `price_asc`, `price_desc`.
  **Paging**: `page` from 1, `page_size` 1–48 (default 12).
- **Facets**: `deal`, `category`, `property_type`, `city`, `suburb`, `bedrooms`, `features` as
  `[{code, label, count}]`, and `price: {currency, min, max}`. Each facet counts the listings that
  match every *other* filter, so choosing a value does not hide its alternatives.
- **By slug**: an expired, sold, let, paused or closed listing is `410` (the page can say "no
  longer available"); an unknown slug **and a draft that was never public** are both `404`, so
  drafts are not revealed.
- **A listing** carries id, slug, reference, title, `deal` / `category` / `property_type` /
  `suburb` / `city` as `{code, label}`, `price {amount, currency}` (a decimal string, never a
  float), `price_period`, market, bedrooms, bathrooms, floor and land area (m²), summary,
  description, features, featured, photos `{url, alt, width, height}`, the three dates, and
  `advertiser`.
- **`advertiser` is public profile references only**: `{type, name, verified, agency, agent}`
  where `agency` and `agent` are `{kind, slug, name, verified}` (both null for a private seller).
  **No phone number, email address or street address appears in any public response**; a test
  walks every public GET (reference, listings, each listing, directories, each profile, sitemap)
  and fails on any key named like contact data or any value that looks like an email address or a
  phone number.
- **Profiles** carry name, role, tagline, description, city, areas, services, offerings, trade,
  verified, sponsored, since, `agency` (for agents), `listing_count`; the detail adds an agency's
  `agents` and the profile's public `listings`. `logo` is always null (there are none).
- **Photos are not uploaded anywhere.** `photos[].url` is a site-relative path
  (`/assets/images/<file>.jpg`) naming the web app's own sample image files, returned exactly as
  stored. **Object storage (MinIO) and uploads are not built.**

The dev seed (memory store and `cmd/seed`) holds the web's 18 listings and 29 profiles with their
real titles, prices, descriptions, features, photos, slugs and references
(`internal/devseed/data/sample.json`, converted from the web's sample data). Publication dates are
relative to the moment of seeding, so the sample listings are always recent; the one expired
listing (`expired-two-bedroom-flat-avenues`) answers `410`.

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

**The index.** One index per environment behind an alias, with an explicit, strict mapping
(`internal/modules/listings/es_mapping.json`): `keyword` for codes (deal, category, property type,
city, suburb, features, market, currency), `text` with a `keyword` and a `search_as_you_type`
sub-field for title, suburb and city labels, `scaled_float` for the price with the currency kept
separately, integers for bedrooms and bathrooms, dates, a `version`, and a `geo_point` `location`
that is mapped but unused. **A document holds public listing data only** — no customer, no contact
detail, no owner account, not even the advertiser; a test checks every field name and that every
field is in the mapping.

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

**Operations: not set up, no owner.** Snapshots and restore testing, upgrades (the client major
must match the cluster major), monitoring (cluster health, disk watermarks, slow queries), index
lifecycle, authentication and TLS for a real cluster, and capacity planning.

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
| `AGENCY_OWNER` | ORGANISATION | organisation `AGENCY` + OWNER membership + profile in `agencies` |
| `LANDLORD` | ORGANISATION | organisation `LANDLORD` + OWNER + profile in `property-managers` |
| `HOST` | ORGANISATION | organisation `HOST` + OWNER + profile in `hosts` |
| `DEVELOPER` | ORGANISATION | organisation `DEVELOPER` + OWNER + profile in `developers` |
| `SERVICE_PROVIDER` | ORGANISATION | organisation `SERVICE_PROVIDER` + OWNER + profile in `service-providers` (with its trade) |
| `AGENT` | AGENCY_MEMBER | AGENT membership in the chosen **existing verified agency** + profile in `agents`, linked to it |
| `AGENCY_STAFF` | AGENCY_MEMBER | STAFF membership in the chosen agency; no profile |
| `AGENCY_ADMIN` | AGENCY_MEMBER | MANAGER membership in the chosen agency; no profile |
| `PRIVATE_SELLER` | NONE | nothing; the role becomes ACTIVE |

A created profile is published and `verified = true`, with a slug derived from the name (`-2`,
`-3`, ... on a collision). **Public directory endpoints and the sitemap only ever return profiles
that are published AND verified**; the 29 sample profiles are all verified. A profile gets public
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
| `LANDLORD`, `HOST`, `DEVELOPER` | PROOF_OF_IDENTITY, BUSINESS_REGISTRATION, PROOF_OF_ADDRESS | TAX_CLEARANCE |
| `SERVICE_PROVIDER` | PROOF_OF_IDENTITY, BUSINESS_REGISTRATION, PROOF_OF_ADDRESS | TAX_CLEARANCE, TRADE_LICENCE |
| `AGENT` | PROOF_OF_IDENTITY, ESTATE_AGENTS_COUNCIL_CERTIFICATE | |
| `AGENCY_STAFF`, `AGENCY_ADMIN` | PROOF_OF_IDENTITY, PROOF_OF_EMPLOYMENT | |
| `PRIVATE_SELLER` | PROOF_OF_IDENTITY, PROOF_OF_OWNERSHIP | |

### File storage

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
place of the much larger AWS SDK. **It is tested against a fake HTTP transport only and has never
talked to MinIO**; MinIO is in `docker-compose.yml`, but there is no Docker on the machine this
was written on.

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
| `property_categories` | 3 | `residential`, `commercial`, `land` |
| `property_types` | 7 | `parent_code` is the category |
| `request_types` | 4 | the `request_type` of a property request |
| `contact_methods` | 3 | `EMAIL`, `PHONE`, `WHATSAPP` |
| `account_roles` | 11 | with `group` (code, label, sort) and `requires_verification`; **never** a platform role |
| `directory_kinds` | 6 | with `singular_label` and `description` |
| `service_trades` | 7 | service-provider trades |
| `cities` | 4 | market `ZW` |
| `suburbs` | 18 | market `ZW`; `parent_code` is the city |
| `listing_features` | 59 | features and amenities |

Each row has a stable `code` (what clients send and tables store), a `label` (what people read),
`sort_order` and an internal `active` flag; only active rows are returned. Markets are not a set:
they keep `GET /v1/markets`. The baseline is exactly what the web app used to hard-code, defined
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
- **Roles at registration** – `roles` is required (see "Role model" below): 1–11 values from a
  server-side allowlist. Platform administrator roles, unknown values, non-string entries and
  **duplicates are rejected** with `400 VALIDATION_FAILED` and a message under `fields.roles`.
- **One-time tokens** – email verification (24 h) and password reset (1 h) are single-use and
  stored as SHA-256. A successful reset also marks the email verified, because the link proves
  control of the mailbox.
- **Rate limit** – all `POST /v1/auth/*` routes share one per-IP bucket (burst 20, refill 20/min),
  separate from the property-request bucket.
- **Email delivery** – through Resend; see "Email". Verification and reset emails are sent from
  the request with a bounded retry (their link is a credential and is never written to the
  outbox); if the send still fails the person has to ask again. Everything else is durable through
  the outbox. In development without a key the log mailer writes one line with the link
  (`http://localhost:3000/verify-email?token=...`, `.../reset-password?token=...`).
- Logs carry `user_id` on every line of an authenticated request. Passwords, tokens and cookie
  values are never logged (except the development mail link described above).

### Administrator login and session scopes

Platform administrators have their own login, and every session has a **scope**:

| | `customer` | `admin` |
| --- | --- | --- |
| Issued by | `POST /v1/auth/login`, `POST /v1/auth/verify-email` | `POST /v1/admin/auth/login` only |
| Who | any verified account **without** a platform role | verified accounts **with** a platform role |
| Delivery | cookie or bearer | cookie only — never a bearer token, and an admin token sent as `Authorization: Bearer` does not authenticate |
| Lifetime | 30 days absolute, 14 days idle | 12 h absolute, 30 min idle (`ADMIN_SESSION_TTL`, `ADMIN_SESSION_IDLE_TTL`) |
| Rate limit (per IP) | burst 20, 20/min across `/v1/auth/*` | burst 5, 5/min, separate bucket |

- `POST /v1/admin/auth/login` takes `{email, password}` and answers
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
> administrator roles and MFA is not built. Until it is, an administrator account is protected by
> a password, a per-IP rate limit and a short session only.

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
`TENANT`, `PRIVATE_SELLER`, `LANDLORD`, `HOST`, `DEVELOPER`, `SERVICE_PROVIDER`, `AGENT`,
`AGENCY_STAFF`, `AGENCY_ADMIN`, `AGENCY_OWNER`. Each has a status: `CONSUMER` and `TENANT` are
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
- Requests of other organisations and of other sellers are never returned. Newest first, capped at
  100 — **no pagination yet**.

### Not built yet

- **MFA for administrators** – the specification requires it for admin roles and it is **not
  built**; platform administrators sign in with a password only, so the administrator login must
  not be treated as production-ready.
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
- Pagination and filters on the organisation inbox.

## Layout

```
cmd/api        HTTP API binary
cmd/worker     background worker (outbox dispatcher)
cmd/migrate    migration runner: up | down | status
cmd/adminctl   create platform administrators / set their password (Postgres only)
cmd/seed       development sample data into Postgres (refuses outside APP_ENV=development)
cmd/mailpreview  render every email template to tmp/mail-preview/ (sends nothing)
internal/modules/onboarding  professional onboarding: applications, documents, review, approval
internal/platform/storage    private file storage: local folder (development) and S3/MinIO
cmd/reindex    rebuild the Elasticsearch listings index and swap the alias (needs a cluster)
internal/platform/config     env configuration + validation
internal/platform/logging    slog JSON logger, request-ID propagation
internal/platform/httpx      middleware, error envelope, JSON decoding, rate limiter
internal/platform/auth       Principal + Authenticate / RequireAuth / RequireMembership / RequirePlatformRole
internal/app                 router wiring (middleware order, routes); end-to-end tests live here
internal/adminctl            logic of cmd/adminctl (argument and password validation)
internal/devseed             the development sample data (organisations, people, 18 listings, 29 directory profiles)
internal/platform/database   pgxpool setup (session or transaction pool mode)
internal/platform/outbox     outbox dispatcher (lease, retry, dead-letter)
internal/platform/elastic    Elasticsearch client (official Go client, transport only)
internal/platform/mail       email templates, renderer, Resend client, log mailer, delivery log
internal/platform/migrate    embedded-SQL migration runner
internal/modules/health            /healthz, /readyz
internal/modules/markets           GET /v1/markets
internal/modules/propertyrequests  POST /v1/property-requests, GET /v1/agent/property-requests (handler, service, store: postgres + memory)
internal/modules/businessenquiries POST /v1/business-enquiries, GET /v1/inbox/business-enquiries (handler, service, store: postgres + memory)
internal/modules/notifications     outbox events -> emails; verification/reset mailer; data lookups (postgres + memory)
internal/modules/listings          GET /v1/listings, /v1/listings/suggest, /v1/listings/{slug}; Search port (postgres + memory)
internal/modules/directory         GET /v1/directory, /v1/directory/{kind}[/{slug}], /v1/sitemap (postgres + memory)
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
- `backoffice` – moderation and routing-exception handling

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
twice. A handler can mark a failure permanent, which dead-letters the event at once. **The worker
sends real email when `RESEND_API_KEY` is set.** Without `DATABASE_URL` in development the worker
logs that it is idle and waits for shutdown (the API then handles the events in-process).

## Operational notes and known limitations

- **Rate limiter is per process.** `POST /v1/property-requests` is limited per client IP (burst 10,
  refill 10/min), `POST /v1/auth/*` shares a second bucket (burst 20, refill 20/min) and
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
