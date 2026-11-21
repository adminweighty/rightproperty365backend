# Developer return package

What the FSD v2.2 (section 12) and the Developer Handover Specification v2.2 (section 17) ask the delivery team to
return, as built on 2026-11-21 (API 0.61.0, staging only). Written for the product owner, the operations team and
whoever maintains the platform next.

| File | What it is |
| --- | --- |
| [traceability-matrix.md](traceability-matrix.md) | Every BR, FR and UAT ID with its status, where it lives, the API version, how it was verified and what was not. Also the deliberate deviations, the open decisions and the section 17 items that are not in this package. |
| [environment-variables.md](environment-variables.md) | Every setting of the backend, web, mobile and CI: purpose, default, and whether it is a secret. Names only, never values. |
| [runbooks/deploy-and-rollback.md](runbooks/deploy-and-rollback.md) | Releasing to staging (including the manual gitops bump), rolling back, the first production release. |
| [runbooks/database-migrations-backup-restore.md](runbooks/database-migrations-backup-restore.md) | Migrations, the pre-migration backup, restoring. |
| [runbooks/worker-sweeps-metrics-alerts.md](runbooks/worker-sweeps-metrics-alerts.md) | What the worker runs, every metric and alert, and the first action for each. |
| [runbooks/outbox-dead-letters.md](runbooks/outbox-dead-letters.md) | Finding, fixing and replaying or retiring dead-lettered events. |
| [runbooks/email-bounce-spike.md](runbooks/email-bounce-spike.md) | Bounce and complaint spikes, suppressions. |
| [runbooks/malware-scanner-outage.md](runbooks/malware-scanner-outage.md) | When ClamAV is unreachable. |
| [runbooks/enabling-switched-off-features.md](runbooks/enabling-switched-off-features.md) | Push, virus scanning, online payment, app links, the email webhook, direct uploads, alerts, MFA. |
| [runbooks/rotating-secrets.md](runbooks/rotating-secrets.md) | Each secret, where it lives, how to rotate it. |
| [runbooks/adding-a-market.md](runbooks/adding-a-market.md) | Bringing a new country live without a release. |
| [runbooks/incident-checklist.md](runbooks/incident-checklist.md) | What to do, in order, when something breaks. |
| [guides/admin-guide.md](guides/admin-guide.md) | Task guide for administrators, with what each role can and cannot do. |
| [guides/agent-guide.md](guides/agent-guide.md) | Task guide for agents, agencies, landlords and sellers. |

The detailed behaviour of every feature is in the repository README, one section per API version; the exact
requests and responses are in `api/openapi.yaml`.

**No secrets are in these files.** Where a command needs a credential, the credential is read inside the pod from
its environment.
