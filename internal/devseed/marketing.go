package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/identity"
)

// Fixed IDs of the sample marketing consent records (API 0.37.1), so the
// seed is idempotent.
var (
	ConsumerMarketingOptIn = id("7001")
	TenantMarketingOptIn   = id("7011")
	TenantMarketingOptOut  = id("7012")
)

// SampleMarketingConsents are the development marketing consent records:
//   - consumer@example.test opted in at registration (web, current wording);
//   - tenant@example.test opted in at registration (web), then opted out from
//     the app: two records, showing the history.
//
// Every other sample account never chose (marketing_opt_in false, no record).
func SampleMarketingConsents(now time.Time) []identity.MarketingConsentEvent {
	now = now.UTC().Truncate(time.Second)
	day := 24 * time.Hour
	return []identity.MarketingConsentEvent{
		{ID: ConsumerMarketingOptIn, UserID: identity.SeedConsumerUserID, OptIn: true, ChangedAt: now.Add(-30 * day),
			Source: "web", Context: identity.MarketingContextRegistration, ConsentTextVersion: identity.CurrentMarketingConsentVersion},
		{ID: TenantMarketingOptIn, UserID: TenantID, OptIn: true, ChangedAt: now.Add(-20 * day),
			Source: "web", Context: identity.MarketingContextRegistration, ConsentTextVersion: identity.CurrentMarketingConsentVersion},
		{ID: TenantMarketingOptOut, UserID: TenantID, OptIn: false, ChangedAt: now.Add(-2 * day),
			Source: "app", Context: identity.MarketingContextPreferences},
	}
}

// SeedMemoryMarketing puts the sample marketing consent records into the
// in-memory identity store (after SeedMemoryUsers). Re-running replaces them.
func SeedMemoryMarketing(store *identity.MemoryStore, now time.Time) error {
	if err := store.PutMarketingConsent(SampleMarketingConsents(now)...); err != nil {
		return fmt.Errorf("seed marketing consent: %w", err)
	}
	return nil
}

// seedPostgresMarketing resets the sample accounts' marketing consent to the
// sample state (needs migration 0041): their records are deleted and the
// sample records inserted again (the table refuses updates), and each sample
// account's marketing_opt_in is set to its newest sample record's value, or
// false when it has none. Accounts that are not samples are never touched.
func seedPostgresMarketing(ctx context.Context, tx pgx.Tx, now time.Time) error {
	accounts := Accounts()
	ids := make([]uuid.UUID, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM marketing_consent_events WHERE user_id = ANY($1)`, ids); err != nil {
		return fmt.Errorf("reset sample marketing consent: %w", err)
	}
	optIn := map[uuid.UUID]identity.MarketingConsentEvent{}
	for _, e := range SampleMarketingConsents(now) {
		var version *string
		if e.ConsentTextVersion != "" {
			version = &e.ConsentTextVersion
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO marketing_consent_events (id, user_id, opt_in, changed_at, source, context, consent_text_version)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			e.ID, e.UserID, e.OptIn, e.ChangedAt, e.Source, e.Context, version); err != nil {
			return fmt.Errorf("seed marketing consent %s: %w", e.ID, err)
		}
		if latest, ok := optIn[e.UserID]; !ok || e.ChangedAt.After(latest.ChangedAt) {
			optIn[e.UserID] = e
		}
	}
	for _, a := range accounts {
		if _, err := tx.Exec(ctx, `UPDATE users SET marketing_opt_in = $2 WHERE id = $1`, a.ID, optIn[a.ID].OptIn); err != nil {
			return fmt.Errorf("seed marketing opt-in of %s: %w", a.Email, err)
		}
	}
	return nil
}
