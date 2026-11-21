# Incident checklist

## 1. Stabilise (first 15 minutes)

- [ ] Name one incident lead and one note-taker. Start a timeline with UTC times.
- [ ] What is broken, for whom, since when? Public site, API, admin, email, mobile?
- [ ] Recent change? Check the last gitops commit and the last migration (`kubectl -n rightproperty-staging logs job/migrate`).
      If a deploy matches the start time, **roll back the image** (`deploy-and-rollback.md`).
- [ ] Pods: `kubectl -n rightproperty-staging get pods`, then `describe` and `logs` on any that are not Running and Ready.
- [ ] API health: `GET /healthz` (process) and `GET /readyz` (database reachable).
- [ ] Database: is the `postgres` pod up, and how many connections are in use (`pg_stat_activity`)?
- [ ] Workers: are `rp365_outbox_oldest_pending_age_seconds` and the dead letters growing? Is the worker log full of one error?
- [ ] External providers: Resend (email), S3, Elasticsearch (search falls back to PostgreSQL), ClamAV, FCM.

## 2. Protect data

- [ ] Suspected data exposure: stop it first (roll back, disable the route at the ingress, revoke tokens).
      Record what was exposed and to whom.
- [ ] Suspected leaked secret: rotate it now (`rotating-secrets.md`).
- [ ] Before any manual database change: take a backup (`database-migrations-backup-restore.md`), and have a second
      person read the SQL.
- [ ] Never replay or delete outbox events, or mass-edit records, before the cause is understood
      (`outbox-dead-letters.md`).

## 3. Communicate

- [ ] Tell the product owner what is affected and when the next update will come.
- [ ] Customer-facing requests are never lost. Even when email or push fails, requests stay in the inbox and in My
      Activity. Say so to support.
- [ ] Unroutable requests: **Administration → Requests nobody could receive**. Reassign them.

## 4. Recover and close

- [ ] Confirm recovery with the same signals that showed the fault (metrics, a smoke test).
- [ ] Drain any backlog: outbox pending back to normal, quarantined uploads scanned, routing exceptions handled.
- [ ] Within 5 working days, write a short review:
  - the timeline;
  - the cause;
  - what worked and what did not;
  - follow-up tasks, each with an owner.

  Update these runbooks if a step was missing or wrong.

## Useful places

| What | Where |
| --- | --- |
| Admin audit | Administration → Audit log |
| Organisation audit | Agent workspace → Team → Activity (owners and managers) |
| Email problems | Administration → Email delivery |
| Upload scanning | Administration → Upload scanning |
| Unroutable requests | Administration → Requests nobody could receive |
| Metrics | `METRICS_ADDR` port on each pod (`worker-sweeps-metrics-alerts.md`) |
