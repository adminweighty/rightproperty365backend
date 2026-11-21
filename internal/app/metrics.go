package app

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/platform/metrics"
)

// RegisterQueueGauges declares the gauges read from the database at scrape
// time (API 0.61.0, FR-056, FR-065): the outbox backlog and its oldest
// waiting event, dead letters, push deliveries waiting or dead, unresolved
// routing exceptions, email bounces and complaints in the last hour, and
// upload intents waiting. Each is one cheap aggregate on an indexed column.
func RegisterQueueGauges(p *metrics.Process, pool *pgxpool.Pool) {
	one := func(sql string) metrics.GaugeFunc {
		return func(ctx context.Context) ([]metrics.Sample, error) {
			var v float64
			if err := pool.QueryRow(ctx, sql).Scan(&v); err != nil {
				return nil, err
			}
			return []metrics.Sample{{Value: v}}, nil
		}
	}
	byLabel := func(label, sql string) metrics.GaugeFunc {
		return func(ctx context.Context) ([]metrics.Sample, error) {
			rows, err := pool.Query(ctx, sql)
			if err != nil {
				return nil, err
			}
			defer rows.Close()
			out := []metrics.Sample{}
			for rows.Next() {
				var k string
				var v float64
				if err := rows.Scan(&k, &v); err != nil {
					return nil, err
				}
				out = append(out, metrics.Sample{Labels: map[string]string{label: k}, Value: v})
			}
			return out, rows.Err()
		}
	}
	p.Gauge("rp365_outbox_pending", "Outbox events waiting to be handled, by event type.",
		byLabel("event_type", `SELECT event_type, count(*) FROM outbox_events WHERE processed_at IS NULL AND dead_letter_at IS NULL GROUP BY event_type`))
	p.Gauge("rp365_outbox_oldest_pending_age_seconds", "Age of the oldest outbox event waiting (0 when none).",
		one(`SELECT COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0) FROM outbox_events WHERE processed_at IS NULL AND dead_letter_at IS NULL`))
	p.Gauge("rp365_outbox_dead_letters", "Outbox events given up on (dead letters), by event type.",
		byLabel("event_type", `SELECT event_type, count(*) FROM outbox_events WHERE dead_letter_at IS NOT NULL GROUP BY event_type`))
	p.Gauge("rp365_push_deliveries", "Push and in-app deliveries waiting or dead, by status.",
		byLabel("status", `SELECT status, count(*) FROM notification_deliveries WHERE status IN ('PENDING', 'ATTEMPTED', 'DEAD') GROUP BY status`))
	p.Gauge("rp365_routing_exceptions_open", "Property requests nobody could receive, not yet resolved.",
		one(`SELECT count(*) FROM property_requests WHERE status = 'ROUTING_EXCEPTION' AND exception_resolved_at IS NULL`))
	p.Gauge("rp365_email_delivery_events_last_hour", "Email bounces and complaints received in the last hour, by type.",
		byLabel("event_type", `SELECT event_type, count(*) FROM email_delivery_events WHERE received_at > now() - interval '1 hour' AND event_type IN ('BOUNCED', 'COMPLAINED') GROUP BY event_type`))
	p.Gauge("rp365_email_accepted_last_hour", "Emails the provider accepted in the last hour.",
		one(`SELECT count(*) FROM email_messages WHERE status = 'PROVIDER_ACCEPTED' AND accepted_at > now() - interval '1 hour'`))
	p.Gauge("rp365_upload_intents_open", "Upload intents waiting to be uploaded or used.",
		one(`SELECT count(*) FROM upload_intents WHERE status IN ('PENDING_UPLOAD', 'READY')`))
}
