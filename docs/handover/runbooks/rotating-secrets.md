# Rotating secrets

Secrets live only in Kubernetes Secrets in the environment's namespace (staging: `rightproperty-staging`) or in the
CI repository settings. Never paste a value into a repository, a ticket, a chat or a shell history; read new values
from a file and delete the file. After changing a Secret, restart the pods that read it:
`kubectl -n rightproperty-staging rollout restart deploy/api deploy/worker`.

| Secret (env name) | Kubernetes Secret on staging | How to rotate | Effect on users |
| --- | --- | --- | --- |
| `DATABASE_URL` (and `DATABASE_DIRECT_URL`) | `api-database` | Create a new database role or password, update the Secret, restart api and worker and the migrate job's next run, then drop the old password | None if done in that order |
| PostgreSQL superuser | `postgres-credentials` | `ALTER ROLE … PASSWORD …` inside the pod, update the Secret | None |
| `RESEND_API_KEY` | `resend-credentials` | Create a new key in Resend, update, restart, revoke the old key | Emails sent during the switch are retried |
| `RESEND_WEBHOOK_SECRET` | (not set on staging) | Roll the secret in Resend, update, restart the API | Webhooks during the switch get 401 and are retried by Resend |
| S3 access keys (`S3_ACCESS_KEY`, `S3_SECRET_KEY`; staging maps them from the AWS names) | `aws-s3-credentials` | New IAM access key, update, restart, deactivate the old key | None |
| `ELASTICSEARCH_API_KEY` | `elasticsearch-credentials` | Create a new scoped API key in Elasticsearch, update, restart, invalidate the old | Search falls back to PostgreSQL if a key fails |
| `MFA_ENCRYPTION_KEY` | `admin-mfa` | **Not a simple swap:** it seals the administrators' authenticator secrets. Rotating it means every administrator enrols again. Plan it; keep the old key until done | Administrators re-enrol |
| `FCM_SERVICE_ACCOUNT_JSON` | (not set) | New service-account key, update, restart, delete the old key in Google Cloud | None |
| Payment gateway credentials | (not set) | Per provider | — |
| `REVALIDATE_SECRET` (web) | web deployment | New random value on the web server and on whatever calls `/api/revalidate` | None |
| `GITOPS_TOKEN` (CI) | GitHub repository secret | New fine-grained token with write access to `adminweighty/gitops` only | CI only |
| Seed password (`DEV_SEED_PASSWORD`) | `seed-credentials` | Development and staging sample accounts only; change and re-seed | Sample accounts only |
| `AUTH_DEMO_CODE` | deployment env (staging only) | Never set in production: the API refuses to start | Staging testers |

If a secret has leaked, rotate it first, then find out how it leaked, and record both.
