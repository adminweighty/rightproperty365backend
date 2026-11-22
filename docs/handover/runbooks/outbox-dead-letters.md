# Outbox dead letters

An outbox event is dead-lettered (`dead_letter_at` set) after 8 failed attempts, or at once when its handler marks
the failure permanent. Nothing retries it afterwards. A handler can instead mark a failure **deferred** (API 0.61.2,
`outbox.Defer`): an operator must act, so the event keeps being retried (backoff capped at an hour) and is never
dead-lettered for running out of attempts. The metric is `rp365_outbox_dead_letters{event_type}`; since 0.61.2 it
counts only dead letters not yet retired (`processed_at IS NULL`), and with none it has no sample at all.

## Look (read-only)

```sql
SELECT event_type, count(*), min(created_at), max(created_at), max(attempts)
FROM outbox_events WHERE dead_letter_at IS NOT NULL GROUP BY 1;

SELECT left(last_error, 200) AS error, count(*)
FROM outbox_events WHERE dead_letter_at IS NOT NULL GROUP BY 1 ORDER BY 2 DESC;
```

Run with `kubectl -n rightproperty-staging exec -i deploy/postgres -- sh -c 'psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"'`.

## Decide

1. **Find the cause first.** The error text names the handler: `elasticsearch: …` is the search indexer, an email
   error is the mailer, `push …` is the push sender.
2. **Fix the cause** (a configuration, a provider outage, a code bug).
3. **Is the work still needed?** Many events are superseded. For example, a listing event is superseded when the
   index has since been rebuilt, and an email may be pointless days later.
4. Only then **replay** (the handlers are idempotent, so a replay is safe) or **retire** (mark the event handled):
   ```sql
   -- replay: hand the events back to the workers
   UPDATE outbox_events SET dead_letter_at = NULL, attempts = 0, available_at = now(), last_error = NULL
   WHERE id IN (...);
   -- retire: record that they need nothing more (keep them; do not delete history)
   UPDATE outbox_events SET processed_at = now() WHERE id IN (...) AND dead_letter_at IS NOT NULL;
   ```
   Back up first, and write down what was replayed or retired and why.

## Known case: `strict_dynamic_mapping_exception` on LISTING_* events (a stale search index)

The search index mapping is strict. A release that raises `listings.MappingVersion` adds fields that the old index
refuses with HTTP 400 `strict_dynamic_mapping_exception`.

**Since API 0.61.2** these events are no longer dead-lettered:

- The indexer marks that 400 as deferred: the events wait and are retried until the index is rebuilt. Every other
  400 from Elasticsearch is still permanent.
- The worker reads `_meta.mapping_version` of the index behind `<prefix>-listings-current` at start and every 5
  minutes. Older than the code, or no index: one ERROR line `search index is stale: rebuild it with /app/reindex`
  and `rp365_search_index_stale 1` (alert `RP365SearchIndexStale` after 15 min). A refused write sets it too.
- **What to do:** run `/app/reindex` (`deploy-and-rollback.md` step 8). Within 5 minutes the worker logs `search
  index is current again` and the gauge returns to 0; the waiting events then succeed on their next retry (at most
  an hour later; the reindex has already written the current state, so nothing is missing meanwhile).
- The worker does **not** reindex by itself: two replicas would build and swap two indices at once, a
  crash-looping worker would rebuild on every start, and a reindex is an operator action with a backup.

Old dead letters of this kind (from before 0.61.2) are superseded once the index has been rebuilt: retire them
(replaying is also harmless). Check first that public search returns every public listing.

### Done on staging, 2026-11-22

- 47 `LISTING_PUBLISHED` dead letters, created 2026-11-14 15:36-17:43 UTC (the seed ran while the index mapping
  was at versions 2-4), 1 attempt each, all `elasticsearch: HTTP 400 strict_dynamic_mapping_exception`, for 24
  listings.
- Checked before: the index was current (`rp365_search_index_stale 0`, mapping version 7), and public search
  returned all 26 published listings, including all 24.
- Backup: `/var/lib/rightproperty/backups/pre-0612-20261122-143016.dump`.
- Retired in one transaction that would roll back unless exactly 47 rows changed (rows kept, nothing deleted):
  ```sql
  UPDATE outbox_events SET processed_at = now()
  WHERE dead_letter_at IS NOT NULL AND processed_at IS NULL AND event_type = 'LISTING_PUBLISHED'
    AND last_error = 'elasticsearch: HTTP 400 strict_dynamic_mapping_exception' AND created_at < '2026-11-15';
  ```
- After: 0 dead letters not retired, so `rp365_outbox_dead_letters` has no sample, and search still returns 26.
  The 47 event ids were recorded outside the repository, in the session notes of that change. Production was not
  touched.
