# Worker sweeps, metrics and alerts

## The worker

`cmd/worker` (staging: deployment `worker`, 2 replicas) does two kinds of work:

1. **Outbox:** polls `outbox_events` with `FOR UPDATE SKIP LOCKED` and a row lease, dispatches by event type,
   retries with exponential backoff (30 s doubling, capped at 1 h), and dead-letters after 8 attempts, or at once
   when a handler says the failure is permanent. Delivery is at least once; every handler is idempotent.
2. **Sweeps:** each runs at start and then on a ticker. Several workers are safe (row locks, compare-and-set
   updates or advisory locks).

| Sweep | Interval | What it does | `sweep` label |
| --- | --- | --- | --- |
| Listing expiry | 1 h | 14/7/1-day reminders, expiry at 90 days, index update | — |
| Saved-search alerts | 2 min | Matches new listings, sends alerts | `saved_searches` |
| Appointment reminders | 5 min | Reminder emails and in-app notices | — |
| Agreements | 5 min | Signing reminders and expiry | `agreements` |
| Subscriptions | 5 min | Records the expiry of subscriptions whose end has passed | `subscriptions` |
| Payment reconciliation | 5 min | Expired checkouts, provider look-ups (when a gateway is on) | — |
| Placement sync | 5 min | Starts and ends featured/sponsored placements | — |
| Email delivery spikes | 5 min | Bounce and complaint spike alerts | `email_delivery` |
| Upload intents | 15 min | Deletes intents unused after 24 h, and their objects | `upload_intents` |
| Analytics | 15 min | Rollups and raw-event retention | — |
| Invoices | 1 h | Marks unpaid invoices past their due date OVERDUE, once, with a reminder email | `invoices` |
| Data exports | 1 min | Builds requested exports, deletes expired ones | `data_export` |
| Application retention | 1 h | Retention job, removes deleted files | — |
| Rent schedule | 24 h | Rent dues and arrears | — |
| CRM reminders | 1 min | Due contact reminders | — |
| Malware scan worker | continuous | Scans quarantined documents (when a scanner is configured) | — |
| Push sender | continuous | Sends queued pushes (when FCM is configured) | — |

Sweeps with a `sweep` label report to the metrics below; the others log every run and every failure (JSON on stdout,
`kubectl -n rightproperty-staging logs deploy/worker`).

## Metrics

With `METRICS_ADDR` set (staging: `:9090`), the API and the worker serve `GET /metrics` on that port **inside the
pod only**: no Service port, no ingress. To look:

```sh
kubectl -n rightproperty-staging port-forward deploy/worker 19091:9090 &
curl -s localhost:19091/metrics | grep ^rp365_
```

| Metric | Means | First action when it looks wrong |
| --- | --- | --- |
| `rp365_http_requests_total{method,route,status}` | Requests by route pattern and status class (API pods) | Rising `5xx`: read the API logs for the route; check the database and S3. |
| `rp365_http_request_duration_seconds` | Latency histogram per route | p95 up: look for slow queries (`pg_stat_activity`), search fallbacks, scanner timeouts. |
| `rp365_outbox_pending{event_type}` | Events waiting | Growing: are the workers running? `kubectl get pods`, worker logs. |
| `rp365_outbox_oldest_pending_age_seconds` | Age of the oldest waiting event | Over 300 s: the workers are stuck or a handler keeps failing transiently (email provider down?). |
| `rp365_outbox_dead_letters{event_type}` | Events given up on | New ones: see `outbox-dead-letters.md`. |
| `rp365_push_deliveries{status}` | Push and in-app deliveries waiting or dead | DEAD rising: FCM credentials or tokens; see `enabling-switched-off-features.md`. |
| `rp365_routing_exceptions_open` | Requests nobody could receive | Above 0 for 30 min: Administration → Requests nobody could receive; reassign. |
| `rp365_email_delivery_events_last_hour{event_type}`, `rp365_email_accepted_last_hour` | Bounces and complaints against email sent | See `email-bounce-spike.md`. |
| `rp365_upload_intents_open` | Upload intents waiting | Large and growing: the sweep is failing, or a client is not completing uploads. |
| `rp365_sweep_runs_total{sweep,result}` | Sweep runs, ok or error | Errors: the worker log has the error text for that sweep. |
| `rp365_sweep_last_success_timestamp_seconds{sweep}` | Last good run | Older than 3 h: the sweep is failing or the worker is down. |
| `rp365_sweep_last_run_duration_seconds{sweep}` | How long the last run took | Approaching the interval: the sweep cannot keep up. |

## Alerts

The rules are in the gitops repository, `monitoring/prometheus-rules.yaml`, with a scrape example in
`monitoring/scrape-example.yaml`. **They are not applied**: no kustomization references `monitoring/`, and no
Prometheus is installed. To enable them: install a Prometheus (or the Prometheus Operator) in the cluster, add the
scrape config, review the thresholds, then reference the rules from an environment.

| Alert | Fires when | First actions |
| --- | --- | --- |
| RP365ApiErrorRateHigh | 5xx above 2% for 10 min | API logs; database reachable; recent deploy? roll back. |
| RP365ApiLatencyHigh | p95 above 1.5 s for 15 min | Database load, search fallbacks, scanner timeouts. |
| RP365ApiDown / RP365WorkerDown | A pod cannot be scraped for 5 min | `kubectl get pods`, events, last deploy. |
| RP365OutboxBacklog | Oldest event waiting over 5 min | Workers running? Email provider up? |
| RP365DeadLetters | New dead letters in the hour | `outbox-dead-letters.md`. |
| RP365PushDeadDeliveries | Over 10 dead pushes in an hour | FCM credentials, expired tokens. |
| RP365RoutingExceptionsOpen | Unrouted requests for 30 min | Reassign them in the admin area. |
| RP365EmailBounceSpike / RP365EmailComplaintSpike | Bounces ≥ 5 and ≥ 5% / complaints ≥ 3 in an hour | `email-bounce-spike.md`. |
| RP365SweepStale / RP365SweepFailing | No success in 3 h / 3 errors in an hour | Worker logs for that sweep. |
