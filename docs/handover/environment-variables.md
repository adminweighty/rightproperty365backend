# Environment variable reference

Names, purpose and defaults as read by the code on 2026-11-21 (backend `internal/platform/config/config.go` and
`cmd/*`; web `src/lib/env.ts` and the route handlers; mobile `lib/core/config.dart`). **Secret** means the value
must come from a secret store and never appear in a repository, a log or a ticket. No values are given here.

## Backend (API and worker; the same image also runs migrate, reindex, seed and adminctl)

| Name | Purpose | Default | Secret |
| --- | --- | --- | --- |
| `APP_ENV` | `development`, `staging` or `production`. Many safety checks depend on it | `development` | no |
| `HTTP_ADDR` | API listen address | `:8080` | no |
| `LOG_LEVEL` | `debug`, `info`, `warn`, `error` | `info` | no |
| `DATABASE_URL` | PostgreSQL connection. Required outside development | — | **yes** |
| `DATABASE_DIRECT_URL` | Direct connection for migrate, seed and adminctl when `DB_POOL_MODE=transaction` | `DATABASE_URL` | **yes** |
| `DB_POOL_MODE` | `session` (direct) or `transaction` (through PgBouncer) | `session` | no |
| `DB_MAX_CONNS` | Pool size per process (1–100) | `8` | no |
| `CORS_ALLOWED_ORIGINS` | Exact web origins, comma-separated; also the CSRF allowlist for cookie sessions | `http://localhost:3000` | no |
| `PUBLIC_WEB_URL` (alias `WEB_BASE_URL`, deprecated) | Public web origin; every link in email is built from it | `http://localhost:3000` | no |
| `MAIL_DRIVER` | `resend` or `log`. Unset: `resend` when a key is set, else `log` in development | — | no |
| `RESEND_API_KEY` | Resend sending key | — | **yes** |
| `EMAIL_FROM`, `EMAIL_REPLY_TO` | Sender and reply-to addresses (the domain must be verified in Resend) | built-in sender | no |
| `RESEND_WEBHOOK_SECRET` | Signing secret of Resend's delivery webhook. Unset: the webhook answers 503 | — | **yes** |
| `AUTH_DEMO_CODE` | A demo sign-in code accepted besides the emailed one. **Development and staging only**: the API refuses to start with it in production | — | yes (staging) |
| `ADMIN_SESSION_TTL` | Administrator session lifetime (Go duration, up to 24 h) | `12h` | no |
| `ADMIN_SESSION_IDLE_TTL` | Administrator inactivity timeout; must not exceed the TTL | `30m` | no |
| `ADMIN_MFA_REQUIRED` | Require an authenticator app for administrators | `true` in production, else `false` | no |
| `MFA_ENCRYPTION_KEY` | Seals the administrators' TOTP secrets (32 bytes, base64). Required in production | — | **yes** |
| `STORAGE_DRIVER` | `local` (development only) or `s3`. Staging and production must use `s3` | `local` in development | no |
| `STORAGE_LOCAL_DIR` | Directory for `local` storage | `tmp/uploads` | no |
| `S3_ENDPOINT`, `S3_BUCKET`, `S3_REGION`, `S3_USE_PATH_STYLE` | S3 bucket settings (staging maps them from `aws-s3-credentials`) | region `us-east-1` | no |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | S3 credentials | — | **yes** |
| `SEARCH_DRIVER` | `database` or `elasticsearch`. Unset: Elasticsearch when its URL is set | — | no |
| `ELASTICSEARCH_URL` | Elasticsearch address | — | no |
| `ELASTICSEARCH_API_KEY` | Scoped API key for the listings index | — | **yes** |
| `SEARCH_INDEX_PREFIX` | Index name prefix | `rp365-<APP_ENV>` | no |
| `SEARCH_TIMEOUT` | One search request before falling back to PostgreSQL (50 ms – max) | `800ms` | no |
| `ANALYTICS_RAW_RETENTION_DAYS` | Days raw analytics events are kept (7–3650) | `90` | no |
| `MALWARE_SCANNER` | `none` or `clamav` | `none` | no |
| `CLAMD_ADDRESS` | `host:port` of clamd. Required with `clamav` | — | no |
| `MALWARE_SCAN_TIMEOUT` | One scan | `60s` | no |
| `MALWARE_SCAN_MAX_BYTES` | Must equal clamd's `StreamMaxLength` | 25 MB | no |
| `PAYMENT_GATEWAY` | `none` or a driver; only `fake` exists, and production refuses it | `none` | no |
| `PAYMENT_FAKE_WEBHOOK_SECRET` | The fake driver's webhook secret (random per process when unset) | random | yes |
| `PAYMENT_RECONCILE_AFTER` | When an open checkout is looked up with the provider | `15m` | no |
| `FCM_PROJECT_ID` | Firebase project for push | — | no |
| `FCM_SERVICE_ACCOUNT_JSON` or `FCM_SERVICE_ACCOUNT_FILE` | FCM service account (inline, or a mounted file path). Unset: push disabled | — | **yes** |
| `METRICS_ADDR` | Private Prometheus listener, e.g. `:9090`. Unset: no endpoint | — | no |
| `DEV_SEED_PASSWORD` | Password of the sample accounts made by `seed`. Development and staging only | — | **yes** |
| `SEED_ALLOW_STAGING` | Must hold the exact opt-in phrase before `seed` touches staging | — | no |
| `ADMINCTL_PASSWORD` | `adminctl` only: the new administrator's password (or from standard input) | — | **yes** |
| `STAMP_DUMP_DIR` | Development tool: write generated stamp images to a directory | — | no |
| `RP365_TEST_DATABASE_URL` | Tests only: a throwaway PostgreSQL database for contract tests | — | yes |

## Web (Next.js)

| Name | Purpose | Default | Secret |
| --- | --- | --- | --- |
| `NEXT_PUBLIC_API_URL` | API origin used by the browser | `http://localhost:8080` | no |
| `API_URL` | API origin used by the web server (server-side calls), when different | `NEXT_PUBLIC_API_URL` | no |
| `NEXT_PUBLIC_SITE_URL` | Public web origin (canonical links, sitemap) | `http://localhost:3000` | no |
| `NEXT_PUBLIC_TEST_SITE` | `1` or `0` at build time: a test copy of the site (banner, no indexing). Unset: true when the host starts with `staging.` | by host | no |
| `NEXT_PUBLIC_MAP_TILE_URL`, `NEXT_PUBLIC_MAP_ATTRIBUTION` | Map tiles URL template and attribution | — | no |
| `REVALIDATE_SECRET` | Enables `/api/revalidate` (404 when unset) | — | **yes** |
| `RP_API_AT_BUILD`, `BUILD_WITHOUT_API` | Whether the build may call the API to prerender | API at build on | no |
| `ANDROID_APP_SHA256_FINGERPRINTS` | Android App Links (`/.well-known/assetlinks.json`; 404 until set) | — | no |
| `ANDROID_APP_PACKAGE` | Android package for App Links | `com.rightproperty365.rightproperty365` | no |
| `APPLE_TEAM_ID` | iOS Universal Links (`/.well-known/apple-app-site-association`; 404 until set) | — | no |
| `IOS_BUNDLE_ID` | iOS bundle for Universal Links | `com.rightproperty365.rightproperty365` | no |

## Mobile (Flutter, `--dart-define` at build time)

| Name | Purpose | Default | Secret |
| --- | --- | --- | --- |
| `API_BASE_URL` | API origin | `http://10.0.2.2:8080` (Android emulator) | no |
| `WEB_BASE_URL` | Website origin (links the app opens in a browser tab) | `https://rightproperty365.com` | no |
| `APP_VERSION` | Version shown in the app | `1.0.0` | no |
| `MAP_TILE_URL`, `MAP_ATTRIBUTION` | Map tiles URL template and attribution. Empty: no map, search stays a list | — | no (a provider key in the URL would be) |
| `DEMO_SIGNING_CODE` | Test builds only: shows the staging demo signing code as a hint | — | no |

Release signing for the stores is read from local keystore files or CI secrets that are never committed (see the
mobile repository's release notes).

## CI (GitHub repository secrets)

| Name | Purpose |
| --- | --- |
| `GITOPS_TOKEN` | Write access to `adminweighty/gitops`, used by `publish-and-update-gitops.yml` to bump image tags. **Not configured**: the step fails and tags are bumped by hand. |
