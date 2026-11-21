# Enabling the switched-off features

These features are built and tested but off, because each needs an account, a secret or an approval that the
developers do not hold. Secrets go into Kubernetes Secrets (or the host's secret store) and are referenced from the
deployment by `secretKeyRef`: **never into a repository, a ticket or a chat**. Only names appear here, never values.

## Push notifications (FCM; iOS through APNs in Firebase)

- **Needs:** a Firebase project with the Android and iOS apps registered; an APNs key uploaded to Firebase; a
  service account allowed to send FCM messages.
- **Set on the API and the worker:**
  - `FCM_PROJECT_ID`;
  - one of `FCM_SERVICE_ACCOUNT_JSON` (secret) or `FCM_SERVICE_ACCOUNT_FILE` (a path to a mounted secret file).
- **Mobile:** the Firebase configuration files for both apps, then a new build.
- **Check:** the worker logs that push is enabled. A test device receives a push for a new request, and the push text
  holds no contact details. Then watch `rp365_push_deliveries{status="DEAD"}`.

## Malware scanning (ClamAV)

- **Needs:** a `clamd` service reachable from the cluster only, with `freshclam` keeping its signatures current.
- **Set on the API and the worker:**
  - `MALWARE_SCANNER=clamav`;
  - `CLAMD_ADDRESS` (host:port);
  - optionally `MALWARE_SCAN_TIMEOUT` and `MALWARE_SCAN_MAX_BYTES`, which must equal clamd's `StreamMaxLength`.
- **Check:** upload the EICAR test file as a document. It must end INFECTED and be deleted. See
  `malware-scanner-outage.md`.

## Online payment of invoices

- **Needs code:** only the `fake` gateway driver exists, and the API refuses it in production. A real provider (the
  owner's choice per market, decision D-09) needs a driver behind the `paygateway` interface: hosted checkout,
  signed webhook, look-up and refund.
- **Then set:**
  - `PAYMENT_GATEWAY=<driver>`;
  - the driver's credentials and webhook secret (secrets);
  - `PAYMENT_RECONCILE_AFTER` if the default does not suit.
- **Check:** UAT-20, 21 and 22 against the provider's sandbox: an emailed link, and duplicate and out-of-order webhooks.

## App Links and Universal Links (mobile deep links)

- **Needs:** the Android release signing certificate's SHA-256 (Play App Signing's "App signing key certificate"),
  and the Apple Developer Team ID.
- **Set on the web server:**
  - `ANDROID_APP_SHA256_FINGERPRINTS` (comma-separated);
  - `APPLE_TEAM_ID`;
  - `ANDROID_APP_PACKAGE` and `IOS_BUNDLE_ID` only if they differ from `com.rightproperty365.rightproperty365`.
- **Check:** both `/.well-known/assetlinks.json` and `/.well-known/apple-app-site-association` answer 200 with
  JSON (404 until set), on both hosts, without a redirect. Then a listing link opens the app on a test device.

## Resend delivery webhook (bounces and complaints)

1. In Resend, create a webhook to `https://<api host>/v1/webhooks/email/resend` for `email.delivered`,
   `email.delivery_delayed`, `email.bounced` and `email.complained`.
2. Copy its signing secret into `RESEND_WEBHOOK_SECRET` on the API (a secret).

- **Check:** the endpoint answers 401 to an unsigned request instead of 503, and
  **Administration → Email delivery** no longer says "Delivery reports are off".
- Staging deliberately runs without it.

## Direct browser uploads to S3

- **Needs:** a CORS rule on the bucket allowing `PUT` from the web origins, with the `Content-Type` header.
- Until then, browsers send photos through the API automatically.
- **Check:** in the browser's network panel, a photo upload PUTs to the bucket host and the intent completes.

## Metrics, alerts and administrator MFA

- **Metrics:** `METRICS_ADDR` is already set on staging (see `worker-sweeps-metrics-alerts.md`).
- **Alerts:** need a Prometheus. The rules are in the gitops repository under `monitoring/`, not applied.
- **Administrator MFA:**
  - In production `ADMIN_MFA_REQUIRED` defaults to true and needs `MFA_ENCRYPTION_KEY` (a secret: 32 random bytes,
    base64).
  - In staging it is optional; enrol the administrators before requiring it.
