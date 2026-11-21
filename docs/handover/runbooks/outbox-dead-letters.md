# Outbox dead letters

An outbox event is dead-lettered (`dead_letter_at` set) after 8 failed attempts, or at once when its handler marks
the failure permanent. Nothing retries it afterwards. The metric is `rp365_outbox_dead_letters{event_type}`.

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

## Known case: `strict_dynamic_mapping_exception` on LISTING_* events

The search index mapping is strict. A release that raises `listings.MappingVersion` adds fields that the old index
refuses with HTTP 400, and the indexer treats every 400 as permanent. Fix: run `/app/reindex` (see
`deploy-and-rollback.md` step 8). The index is then complete again, so these dead letters are superseded and can be
retired; replaying them is also harmless. Staging has 47 such events from 2026-11-14 (seed run during mapping
versions 2–4).
