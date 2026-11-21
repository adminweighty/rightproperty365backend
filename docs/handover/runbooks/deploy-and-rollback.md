# Deploy and roll back

## What is where

| Piece | Where |
| --- | --- |
| Backend (API, worker, migrate, reindex, seed, adminctl) | This repository, branch `dev` → staging, branch `production` → production. One image: `ghcr.io/adminweighty/rightproperty365backend:sha-<full commit>`. |
| Web | `rightproperty365-frontend` repository, same branches. Image `ghcr.io/adminweighty/rightproperty365-frontend:sha-<full commit>`. |
| Deployment | `adminweighty/gitops`, branch `main`. Argo CD Application `rightproperty-staging` (namespace `rightproperty-staging`) syncs `environments/staging`. The production Application has **never been synced**. |
| Migrations | Argo CD PreSync Job `migrate` runs `/app/migrate up` with the new API image before the new pods start. A failed migration stops the sync and leaves the running pods untouched. It does **not** take a backup. |

## Normal deploy (staging)

1. Merge or push to `dev` in the backend and/or web repository. CI (`ci.yml`) runs gofmt, `go vet`,
   `go test -race` and the build. `publish-and-update-gitops.yml` builds and pushes the image.
2. **Known issue:** the workflow's last step, updating the gitops repository, fails on every run because the
   repository secret `GITOPS_TOKEN` (write access to `adminweighty/gitops`) is not configured. The image is
   still published. Until the secret exists, bump gitops by hand (step 4).
3. Wait until both images exist (about 10–15 minutes for the backend):
   `docker manifest inspect ghcr.io/adminweighty/rightproperty365backend:sha-<commit>`.
4. If the release has a migration, **take the backup first** (see `database-migrations-backup-restore.md`).
5. In the gitops repository, set the two `newTag` values in `environments/staging/kustomization.yaml`
   (or run `scripts/set-image.py`), commit ("staging: API x.y.z …"), push to `main`.
6. Ask Argo CD to look now:
   `kubectl -n argocd annotate application.argoproj.io/rightproperty-staging argocd.argoproj.io/refresh=hard --overwrite`.
7. Watch: `kubectl -n rightproperty-staging rollout status deploy/api` (then `frontend`, `worker`), and
   `kubectl -n rightproperty-staging logs job/migrate` — the last line names the last migration applied.
8. If the release changed the search mapping (`listings.MappingVersion` in `internal/modules/listings/es_index.go`),
   rebuild the index: `kubectl -n rightproperty-staging exec deploy/worker -- /app/reindex`. Skipping this makes
   every listing event fail with `strict_dynamic_mapping_exception` (see `outbox-dead-letters.md`).
9. Smoke test, read-only: `GET /healthz`, a public search, sign in as a sample account, open the pages the
   release touched. Create nothing permanent on staging.
10. Staging sample data (`/app/seed`) runs only with `SEED_ALLOW_STAGING=yes-create-sample-accounts`; the server's
    helper script wraps it. Never run it against production.

## Roll back

- **Application only (no migration in the release):** set the previous `newTag` values in gitops, commit, push,
  refresh Argo CD. Pods roll back in a minute or two.
- **Release with a migration:** migrations are written to be additive, so the previous image normally still runs on
  the newer schema; roll back the image first. Only if the schema itself must go back:
  1. Scale `api` and `worker` to 0.
  2. Either run the migration's `.down.sql` with `/app/migrate down` (one step at a time), or restore the
     pre-migration backup (see `database-migrations-backup-restore.md`). Some down migrations deliberately keep
     widened audit constraints because audit rows may already use them.
  3. Set the previous image tags in gitops and sync.
- **Never** force-push gitops `main`; roll forward with a new commit so the history shows what ran.

## Production (when it is first enabled)

Production needs, before the first sync: the `api-database` secret, S3 credentials and storage settings, the email
settings, `MFA_ENCRYPTION_KEY` (administrator MFA is required in production), real image tags in
`environments/production/kustomization.yaml`, and a reviewed decision on each switched-off feature. `AUTH_DEMO_CODE`
must not be set: the API refuses to start with it in production. Take a backup and agree a rollback owner before
every production migration.
