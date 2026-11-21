# Database migrations, backup and restore

PostgreSQL is self-hosted (not Neon). On staging it is the `postgres` deployment in `rightproperty-staging`, with data
in a hostPath volume on the VM. There is no point-in-time recovery: the backups below are the safety net.

## Migrations

- Files: `migrations/NNNN_name.up.sql` and `.down.sql`, embedded in the binaries. `cmd/migrate up` applies what is
  missing, in order, each in its own transaction, and records it in `schema_migrations`.
- On Kubernetes they run as the Argo CD PreSync Job `migrate` with the new image. With PgBouncer in transaction mode
  set `DATABASE_DIRECT_URL` so migrations use a direct connection.
- Write migrations additive (new tables, new nullable columns, widened constraints) so the previous image keeps
  working; drop things only in a later release.

## Before every migration: back up

Run on the server, before bumping gitops:

```sh
f=/var/lib/rightproperty/backups/pre-NNNN-$(date +%Y%m%d-%H%M%S).dump
kubectl -n rightproperty-staging exec deploy/postgres -- \
  sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' > "$f"
ls -la "$f"     # a staging dump is about 0.7 MB in November 2026; an empty file means it failed
```

`$POSTGRES_USER` and `$POSTGRES_DB` are expanded **inside** the pod (single quotes), so no credential appears on the
command line. Keep at least the last ten dumps; copy them off the VM (the VM disk is a single point of failure).

## Restore

1. Scale writers down: `kubectl -n rightproperty-staging scale deploy/api deploy/worker --replicas=0`.
2. Restore into a fresh database and switch, or into the same one:
   ```sh
   kubectl -n rightproperty-staging exec -i deploy/postgres -- \
     sh -c 'pg_restore -U "$POSTGRES_USER" -d "$POSTGRES_DB" --clean --if-exists --no-owner' < /var/lib/rightproperty/backups/<file>.dump
   ```
3. Deploy the image that matches the restored schema (the tag the backup was taken under).
4. Scale back up, rebuild the search index (`/app/reindex`), smoke test.
5. Outbox events created after the backup are lost with it; notifications for them will not be sent. Say so to support.

## Not done yet (see the matrix, FR-057 and UAT-35)

- No restore drill has been run; RPO and RTO are not agreed. Run one on a copy before production.
- No scheduled backups: only the manual pre-migration dumps. Add a nightly CronJob with off-VM copies before production.
