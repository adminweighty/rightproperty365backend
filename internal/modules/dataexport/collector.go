package dataexport

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// section is one query of the Postgres collector: rows of a table scoped
// to the person, with the columns that must never leave (secrets, tokens,
// other people's identities) removed.
type section struct {
	name  string
	from  string // FROM ... t [JOIN ...] WHERE <scoped to $1>
	order string
	strip []string
	// extra adds fields from joined tables: SQL that builds a jsonb object.
	extra string
}

// sections is everything the archive holds, in order. Organisation data
// (contact books, ledgers, tenancies, other people's requests) is not the
// requester's personal data and is left out on purpose.
var sections = []section{
	{name: "account", from: `users t WHERE t.id = $1`, order: "t.created_at",
		strip: []string{"password_hash", "suspended_by", "deleted_by"}},
	{name: "account_roles", from: `user_account_roles t WHERE t.user_id = $1`, order: "t.user_id"},
	{name: "platform_roles", from: `platform_role_assignments t WHERE t.user_id = $1`, order: "t.user_id", strip: []string{"granted_by"}},
	{name: "memberships", from: `memberships t JOIN organisations o ON o.id = t.organisation_id WHERE t.user_id = $1`, order: "t.created_at",
		strip: []string{"invited_by"}, extra: `jsonb_build_object('organisation_name', o.name)`},
	{name: "consents", from: `user_consents t WHERE t.user_id = $1`, order: "t.user_id"},
	{name: "marketing_consent_history", from: `marketing_consent_events t WHERE t.user_id = $1`, order: "t.changed_at"},
	{name: "notification_preferences", from: `notification_preferences t WHERE t.user_id = $1`, order: "t.user_id"},
	{name: "notification_preference_history", from: `notification_preference_changes t WHERE t.user_id = $1`, order: "t.user_id"},
	{name: "in_app_notifications", from: `notifications t WHERE t.user_id = $1`, order: "t.created_at"},
	{name: "signed_in_devices", from: `sessions t WHERE t.user_id = $1`, order: "t.created_at", strip: []string{"token_hash"}},
	{name: "push_devices", from: `user_devices t WHERE t.user_id = $1`, order: "t.user_id", strip: []string{"token"}},
	{name: "saved_listings", from: `saved_listings t JOIN listings l ON l.id = t.listing_id WHERE t.user_id = $1`, order: "t.user_id",
		extra: `jsonb_build_object('listing_slug', l.slug, 'listing_title', l.title, 'listing_reference', l.reference)`},
	{name: "saved_searches", from: `saved_searches t WHERE t.user_id = $1`, order: "t.created_at", strip: []string{"unsubscribe_token"}},
	{name: "property_requests", from: `property_requests t WHERE t.customer_user_id = $1`, order: "t.created_at",
		strip: []string{"idempotency_key", "request_hash", "recipient_user_id", "stage", "stage_changed_by", "assigned_by"}},
	{name: "property_request_messages", from: `property_request_messages t JOIN property_requests r ON r.id = t.request_id WHERE r.customer_user_id = $1`,
		order: "t.created_at", strip: []string{"author_user_id"},
		extra: `jsonb_build_object('from', CASE WHEN t.author_user_id = $1 THEN 'you' ELSE 'the advertiser' END)`},
	{name: "appointments", from: `appointments t WHERE t.customer_user_id = $1`, order: "t.created_at",
		strip: []string{"idempotency_key", "request_hash", "recipient_user_id", "recipient_organisation_id"}},
	{name: "business_enquiries", from: `business_enquiries t WHERE t.customer_user_id = $1`, order: "t.created_at",
		strip: []string{"idempotency_key", "request_hash", "recipient_user_id"}},
	{name: "applications", from: `applications t WHERE t.applicant_user_id = $1`, order: "t.created_at", strip: []string{"owner_user_id"}},
	{name: "reviews_written", from: `provider_reviews t WHERE t.reviewer_user_id = $1`, order: "t.created_at", strip: []string{"moderated_by", "reply_by"}},
	{name: "listing_reports_made", from: `listing_reports t WHERE t.reporter_user_id = $1`, order: "t.created_at", strip: []string{"decided_by", "decided_by_name"}},
	{name: "venue_bookings", from: `venue_bookings t WHERE t.customer_user_id = $1`, order: "t.created_at", strip: []string{"seller_user_id"}},
	{name: "signature", from: `user_signatures t WHERE t.user_id = $1`, order: "t.user_id", strip: []string{"signature"}},
	{name: "onboarding_applications", from: `onboarding_applications t WHERE t.user_id = $1`, order: "t.created_at"},
	{name: "listings_sold_directly", from: `listings t WHERE t.seller_user_id = $1 AND t.organisation_id IS NULL`, order: "t.created_at",
		strip: []string{"reviewed_by", "agent_user_id"}},
	{name: "subscriptions", from: `subscriptions t WHERE t.subscriber_kind = 'INDIVIDUAL' AND t.subscriber_id = $1`, order: "t.created_at",
		strip: []string{"payment_recorded_by"}},
	{name: "invoices", from: `invoices t WHERE t.subscriber_kind = 'INDIVIDUAL' AND t.subscriber_id = $1`, order: "t.created_at",
		strip: []string{"created_by", "issued_by"}},
	{name: "emails_sent_to_you", from: `email_messages t WHERE t.recipient_user_id = $1`, order: "t.created_at",
		strip: []string{"recipient_sha", "provider_message_id", "last_error", "notification_key"}},
	{name: "activity", from: `customer_activity t WHERE t.user_id = $1`, order: "t.created_at"},
	{name: "data_exports", from: `data_exports t WHERE t.user_id = $1`, order: "t.requested_at", strip: []string{"object_key", "lease_until"}},
}

// PostgresCollector reads the sections from the database.
type PostgresCollector struct{ pool *pgxpool.Pool }

// NewPostgresCollector returns the collector.
func NewPostgresCollector(pool *pgxpool.Pool) *PostgresCollector {
	return &PostgresCollector{pool: pool}
}

// Collect implements Collector.
func (c *PostgresCollector) Collect(ctx context.Context, userID uuid.UUID) ([]Section, error) {
	out := make([]Section, 0, len(sections))
	for _, s := range sections {
		row := `to_jsonb(t) - $2::text[]`
		if s.extra != "" {
			row = `(` + row + `) || ` + s.extra
		}
		strip := s.strip
		if strip == nil {
			strip = []string{}
		}
		var raw []byte
		q := fmt.Sprintf(`SELECT COALESCE(jsonb_agg(%s ORDER BY %s), '[]'::jsonb) FROM %s`, row, s.order, s.from)
		if err := c.pool.QueryRow(ctx, q, userID, strip).Scan(&raw); err != nil {
			return nil, fmt.Errorf("export section %s: %w", s.name, err)
		}
		out = append(out, Section{Name: s.name, Rows: json.RawMessage(raw)})
	}
	return out, nil
}
