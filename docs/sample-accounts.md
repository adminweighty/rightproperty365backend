# Sample accounts

Every account below exists in **development** (the API's in-memory stores, or a development
database seeded with `go run ./cmd/seed`) and on **staging** once `cmd/seed` has been run there
with `SEED_ALLOW_STAGING=yes-create-sample-accounts`. They are defined once, in
`internal/devseed`; `TestEveryRoleHasASampleAccount` fails if an account role or a platform role
loses its sample account.

## Signing in

- **Password.** Every sample account has the same password: the value of `DEV_SEED_PASSWORD`.
  - Development: the `DEV_SEED_PASSWORD` line of [`.env.example`](../.env.example) (copy it to
    `.env`).
  - Staging: the `seed-credentials` secret (at least 24 characters, never the development value).
    Ask whoever runs the staging deployment; it is not written down anywhere in this repository.
- **Sign-in code.** Signing in is two steps: the password, then a code. The sample addresses are
  `@example.test`, which can never receive mail and are never emailed (see "Sign-in code" in the
  README), so:
  - on **staging**, enter the **demo code** (`AUTH_DEMO_CODE`, set for staging by the team);
  - in **development**, either set `AUTH_DEMO_CODE` in `.env`, or read the six-digit code from the
    API log line `DEV MAIL (not sent): sign-in-code` (attribute `code`).
- **Where.** Customers and professionals sign in on the web's normal sign-in page
  (`POST /v1/auth/login`). Platform administrators can only use the administrator sign-in
  (`/admin/sign-in`, `POST /v1/admin/auth/login`); the normal sign-in refuses them.
- All accounts are email-verified and active. No account mixes a property seeker role
  (`CONSUMER`, `TENANT`) with any other role.

## Property seekers

| Email | Role(s) | What to look at when signed in |
| --- | --- | --- |
| `consumer@example.test` | `CONSUMER` (Prospective Buyer) | Seeker dashboard; *My appointments*: five upcoming (one with a new time proposed by the agent, to accept) and one past |
| `tenant@example.test` | `TENANT` (Prospective Tenant) | Seeker dashboard; *My appointments*: consultations with a property manager, a developer (confirmed) and a plumber |

## Real estate professionals and agencies

| Email | Role(s) | What to look at when signed in |
| --- | --- | --- |
| `agent@example.test` | `AGENT`; `AGENT` at Sample Estates | Professional dashboard; listings 1, 2 and 5; appointments inbox (viewings and a consultation); profile `agents/tendai-ncube` |
| `estates.agent2@example.test` | `AGENT`; `AGENT` at Sample Estates | Listings 7, 10 and 17; two viewing requests (one with a new time proposed) |
| `sales.agent@example.test` | `SALES_AGENT`; `AGENT` at Sample Estates | Professional dashboard of an agent with no assigned listings yet (empty inbox) |
| `lettings.agent@example.test` | `LETTINGS_AGENT`; `AGENT` at Sample Estates | As above, for a lettings agent |
| `estates.owner@example.test` | `AGENCY_OWNER`; `OWNER` of Sample Estates | Every request and appointment of the agency (and assigning them to members); *Team*: members (change role, suspend, remove), a pending invitation, a pending join request, the audit trail; profiles `agencies/sample-estates`, `agents/rudo-moyo`; logo upload |
| `agency.admin@example.test` | `AGENCY_ADMIN`; `MANAGER` at Sample Estates | The agency's requests and appointments (a manager sees them all and assigns them), *Team* within a manager's limits (invites and manages agents and staff only, decides their join requests); receives agency enquiries |
| `agency.staff@example.test` | `AGENCY_STAFF`; `STAFF` at Sample Estates | A staff member's view: little to act on, no team pages (403), cannot be assigned requests |
| `commercial.agent@example.test` | `AGENCY_OWNER`; `OWNER` of Sample Commercial | Listings 3 and 13 |
| `land.agent@example.test` | `AGENCY_OWNER`; `OWNER` of Sample Land Agents | Listings 4, 14 and 15 |
| `realty.agent@example.test` | `AGENCY_OWNER`; `OWNER` of Sample Realty | Listings 8, 9, 16 and 18 |
| `bulawayo.agent@example.test` | `AGENCY_OWNER`; `OWNER` of Sample Homes Bulawayo | Listings 11 and 12 |
| `sample-homes-london@example.test` | `AGENCY_OWNER`; `OWNER` of Sample Homes London (GB) | The UK listings 19, 20 and 21 (one is a short stay) |
| `agent-b@example.test` | `AGENT`; `AGENT` at Second Street Realty | An agency with no listings: proves organisations cannot see each other's data |
| `sample-property-management@example.test` | `PROPERTY_MANAGER`; owner of Sample Property Management | Profile `property-managers/sample-property-management`; a consultation request from the tenant |
| `sample-law-chambers@example.test` | `SOLICITOR`; owner of Sample Law Chambers (law firm) | Profile `solicitors/sample-law-chambers`; a consultation request from the consumer |

## Property owners

| Email | Role(s) | What to look at when signed in |
| --- | --- | --- |
| `seller@example.test` | `PRIVATE_SELLER` (approved direct owner) | Direct-owner dashboard; listing 6 (requests sent for it arrive in this inbox) |
| `landlord@example.test` | `LANDLORD` (approved direct owner) | Direct-owner dashboard with nothing listed yet |

## Other property businesses

| Email | Role(s) | What to look at when signed in |
| --- | --- | --- |
| `sample-falls-stays@example.test` | `HOST`; owner of Sample Falls Stays | Short stays 22 and 24 (24 is in Mauritius); availability calendars |
| `sample-showrooms@example.test` | `HOST`; owner of Sample Showrooms | Short stay 23 and its calendar |
| `sample-developments@example.test` | `DEVELOPER`; owner of Sample Developments | Profile `developers/sample-developments`; a confirmed consultation with the tenant |
| `sample-plumbers@example.test` | `SERVICE_PROVIDER`; owner of Sample Plumbers | Profile `service-providers/sample-plumbers`; a consultation request from the tenant |

Every other business profile has an owner too, at `<profile slug>@example.test` (for example
`sample-electricians@example.test`, `sample-conveyancing-attorneys@example.test`,
`sample-stays-edinburgh@example.test`): see the README table.

## Awaiting verification

| Email | Role(s) | What to look at when signed in |
| --- | --- | --- |
| `pending.provider@example.test` | `SERVICE_PROVIDER`, **pending** | Its onboarding application is **submitted** with sample documents: review it as an administrator |
| `pending.agent@example.test` | `AGENT`, **pending** | Its onboarding application is still a **draft**: fill it in and submit |
| `joining.agent@example.test` | `AGENT`, **pending** (Jamie Joiner) | A **join request**: its application to join Sample Estates is **submitted** with sample documents. Sample Estates' owner or manager decides it on the *Team* page (`/v1/agent/organisations/{id}/join-requests`), or an administrator does; whoever is first wins |

## Team samples (Sample Estates)

- **A pending invitation** to `invited.agent@example.test` (no account) as `AGENT`, sent by Rudo
  Moyo, shown in the *Team* page's invitations. Only the SHA-256 of a random token is stored, so
  nobody has its link. To try accepting an invitation, invite an address yourself: in development
  the link is printed by the API log line `DEV MAIL (not sent): team-invitation`; sign up with that
  address (any non-seeker role), then open the link.
- **A pending join request** from `joining.agent@example.test` (above).
- Seeded in memory by the API in development and by `cmd/seed` (needs migration 0021); neither
  emails anyone.

## Lease samples

- **Approved sample templates** "Sample residential lease (Zimbabwe)" and "... (United Kingdom)". Their legal
  review reads "SAMPLE — not reviewed by a lawyer; for demonstration on staging only": they exist to try the
  screens and must never be offered to real customers. Manage them as `admin.content@example.test` (or
  `admin@example.test`) under `/v1/admin/lease-templates`.
- **A draft lease** `RPL-2026-900001` by `agent@example.test` (Tendai Ncube) on listing 2 (Two-bedroom garden
  apartment), landlord `landlord@example.test`, tenant `tenant@example.test`: open it under *Leases*, edit it,
  download its PDF. Its agency's owner and manager see it too; another agent of the agency does not.
- **Documents out for signature** (sent by `agent@example.test`): the uploaded "Sample inventory and condition
  report" (`RPA-2026-900001`: `tenant@example.test` signs first, then `landlord@example.test` approves,
  `estates.owner@example.test` in copy) and the lease `RPL-2026-900002` on listing 5 (`RPA-2026-900002`: landlord
  and tenant sign in any order). Nobody has the seeded links: sign in as the tenant or the landlord, open
  *Documents to sign* (`GET /v1/me/signing-requests`, then `POST /v1/me/signing-requests/{id}/link`), ask for the
  code and enter the **demo code** on staging (in development, the code is in the API log line
  `DEV MAIL (not sent): agreement-signing-code`, or set `AUTH_DEMO_CODE`).
- Seeded in memory by the API in development and by `cmd/seed` (needs migrations 0022 and 0023).

## Platform administrators (administrator sign-in only)

| Email | Platform role | What to look at when signed in |
| --- | --- | --- |
| `admin@example.test` | `SUPER_ADMIN` | Every section of the admin overview; onboarding review; routing exceptions |
| `admin.platform@example.test` | `PLATFORM_ADMIN` | Admin overview; onboarding review; routing exceptions |
| `admin.market@example.test` | `MARKET_ADMIN` for market **ZW** | The same, limited to Zimbabwe |
| `admin.moderator@example.test` | `MODERATOR` | Onboarding review (approve, reject, request changes) |
| `admin.support@example.test` | `SUPPORT` | Routing exceptions; every lease, read-only (`/v1/admin/leases`); the overview sections support covers |
| `admin.finance@example.test` | `FINANCE_ADMIN` | The overview sections finance covers (no finance screens are built yet) |
| `admin.content@example.test` | `CONTENT_ADMIN` | The lease template library (`/v1/admin/lease-templates`); the overview sections content covers |

Administrators cannot reset their password by email; on a database use
`go run ./cmd/adminctl set-password --email ...`.
