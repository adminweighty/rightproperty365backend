package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/legal"
	"rightproperty365.com/backend/internal/modules/tax"
)

// SeedPostgresLegalTax writes the development and staging legal documents
// and tax rules (needs migration 0045, which itself inserts none, so
// production has nothing live):
//
//   - legal.SeedVersions: GLOBAL TERMS, PRIVACY, REFUND and EULA PUBLISHED as
//     placeholders whose bodies say "DRAFT — awaiting legal review" (with
//     draft_notice), and DRAFT copies for ZW and GB;
//   - tax.SeedRules: ZW VAT 15% and GB VAT 20% for SUBSCRIPTIONS, ACTIVE and
//     is_sample (applied nowhere).
//
// Re-runnable: rows already present (same ID, or the same kind, market,
// language and label) are left alone, and a sample tax rule is skipped when
// its market already has an ACTIVE rule for the same use.
func SeedPostgresLegalTax(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	for _, v := range legal.SeedVersions(now) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO legal_document_versions (id, kind, market_code, lang, version_label, effective_from, status, body_md, summary_of_changes,
				requires_reacceptance, draft_notice, created_by_name, published_by_name, published_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULLIF($13, ''), $14, $15, $15)
			ON CONFLICT DO NOTHING`,
			v.ID, v.Kind, v.MarketCode, v.Lang, v.Label, v.EffectiveFrom, v.Status, v.Body, v.Summary, v.RequiresReacceptance,
			v.DraftNotice, v.CreatedByName, v.PublishedByName, v.PublishedAt, v.CreatedAt); err != nil {
			return fmt.Errorf("seed legal version %s %s: %w", v.Kind, v.MarketCode, err)
		}
	}
	for _, r := range tax.SeedRules(now) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO tax_rules (id, market_code, tax_name, rate, applies_to, inclusive, effective_from, effective_to, registration_number_label,
				status, is_sample, notes, created_at, updated_at)
			SELECT $1, $2, $3, $4::text::numeric, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13
			WHERE EXISTS (SELECT 1 FROM markets WHERE code = $2)
			  AND NOT EXISTS (SELECT 1 FROM tax_rules WHERE market_code = $2 AND applies_to = $5 AND status = 'ACTIVE')
			ON CONFLICT (id) DO NOTHING`,
			r.ID, r.MarketCode, r.TaxName, r.Rate, r.AppliesTo, r.Inclusive, r.EffectiveFrom, r.EffectiveTo, r.RegistrationNumberLabel,
			r.Status, r.IsSample, r.Notes, r.CreatedAt); err != nil {
			return fmt.Errorf("seed tax rule %s: %w", r.MarketCode, err)
		}
	}
	return nil
}
