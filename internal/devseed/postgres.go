package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// Placeholder commercial terms for the sample listings: this backend does not
// own listing content yet, only what request routing needs.
const (
	sampleListingPrice    = "100000"
	sampleListingCurrency = "USD"
)

// SeedPostgres writes the same sample data into a development Postgres
// database, in ONE transaction. It is idempotent: every row has a fixed ID (or
// a natural key) and is upserted, so re-running it only resets the sample rows
// to their defined state (including the shared password). Rows it does not
// own are never touched. Callers must have passed CheckEnvironment.
//
// It requires migrations 0001-0007. If a sample email already belongs to a
// different user row the run fails and changes nothing.
//
// NOTE: compiled but NEVER run — there is no Postgres on the machine this was
// written on. Treat the first run as a test.
func SeedPostgres(ctx context.Context, pool *pgxpool.Pool, hasher identity.Hasher, password string, now time.Time) error {
	if err := identity.CheckPassword(password); err != nil {
		return fmt.Errorf("DEV_SEED_PASSWORD %w", err)
	}
	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	// Reference data: the baseline rows (also inserted by migration 0007).
	// Existing rows keep their `active` flag, so re-seeding does not undo an
	// administrator's deactivation; labels and order are reset to the baseline.
	for set, items := range ReferenceItems() {
		for _, it := range items {
			var groupCode, groupLabel *string
			var groupSort *int
			if it.Group != nil {
				groupCode, groupLabel, groupSort = &it.Group.Code, &it.Group.Label, &it.Group.SortOrder
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO reference_items (set_code, code, label, singular_label, description, sort_order, market_id,
				                             parent_code, group_code, group_label, group_sort_order, requires_verification)
				VALUES ($1, $2, $3, $4, $5, $6, (SELECT id FROM markets WHERE code = $7), $8, $9, $10, $11, $12)
				ON CONFLICT (set_code, code) DO UPDATE
				SET label = EXCLUDED.label, singular_label = EXCLUDED.singular_label, description = EXCLUDED.description,
				    sort_order = EXCLUDED.sort_order, market_id = EXCLUDED.market_id, parent_code = EXCLUDED.parent_code,
				    group_code = EXCLUDED.group_code, group_label = EXCLUDED.group_label,
				    group_sort_order = EXCLUDED.group_sort_order, requires_verification = EXCLUDED.requires_verification,
				    updated_at = now()`,
				set, it.Code, it.Label, it.SingularLabel, it.Description, it.SortOrder, it.MarketCode,
				it.ParentCode, groupCode, groupLabel, groupSort, it.RequiresVerification); err != nil {
				return fmt.Errorf("seed reference %s/%s: %w", set, it.Code, err)
			}
		}
	}

	for _, o := range Organisations() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO organisations (id, market_id, name, enquiry_email, kind, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, 'ACTIVE', $6, $6)
			ON CONFLICT (id) DO UPDATE
			SET market_id = EXCLUDED.market_id, name = EXCLUDED.name, enquiry_email = EXCLUDED.enquiry_email,
			    kind = EXCLUDED.kind, status = 'ACTIVE', updated_at = EXCLUDED.updated_at`,
			o.ID, markets.ZimbabweID, o.Name, o.EnquiryEmail, o.Kind, now); err != nil {
			return fmt.Errorf("seed organisation %s: %w", o.Name, err)
		}
	}

	for _, a := range Accounts() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, email, full_name, status, password_hash, email_verified_at, created_at, updated_at)
			VALUES ($1, $2, $3, 'ACTIVE', $4, $5, $5, $5)
			ON CONFLICT (id) DO UPDATE
			SET email = EXCLUDED.email, full_name = EXCLUDED.full_name, status = 'ACTIVE',
			    password_hash = EXCLUDED.password_hash,
			    email_verified_at = COALESCE(users.email_verified_at, EXCLUDED.email_verified_at),
			    updated_at = EXCLUDED.updated_at`,
			a.ID, a.Email, a.Name, hash, now); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("seed %s: the email already belongs to a different account in this database", a.Email)
			}
			return fmt.Errorf("seed user %s: %w", a.Email, err)
		}

		// Registration consent, once.
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_consents (id, user_id, consent_type, version, accepted_at, created_at)
			SELECT $1, $2, $3, $4, $5, $5
			WHERE NOT EXISTS (SELECT 1 FROM user_consents WHERE user_id = $2 AND consent_type = $3)`,
			uuid.New(), a.ID, identity.ConsentPrivacyNotice, privacyVersion, now); err != nil {
			return fmt.Errorf("seed consent for %s: %w", a.Email, err)
		}

		for _, role := range a.AccountRoles {
			if _, err := tx.Exec(ctx, `
				INSERT INTO user_account_roles (id, user_id, role, status, created_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, $5)
				ON CONFLICT (user_id, role) DO UPDATE SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at`,
				uuid.New(), a.ID, role, identity.AccountRoleActive, now); err != nil {
				return fmt.Errorf("seed account role %s for %s: %w", role, a.Email, err)
			}
		}

		if a.Organisation != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO memberships (id, organisation_id, user_id, role, status, market_id, created_at, updated_at)
				VALUES ($1, $2, $3, $4, 'ACTIVE', $5, $6, $6)
				ON CONFLICT (organisation_id, user_id) DO UPDATE
				SET role = EXCLUDED.role, status = 'ACTIVE', market_id = EXCLUDED.market_id, updated_at = EXCLUDED.updated_at`,
				uuid.New(), a.Organisation.ID, a.ID, a.MembershipRole, markets.ZimbabweID, now); err != nil {
				return fmt.Errorf("seed membership for %s: %w", a.Email, err)
			}
		}

		if a.PlatformRole != "" {
			// Global platform roles only (no market); see the partial unique index.
			if _, err := tx.Exec(ctx, `
				INSERT INTO platform_role_assignments (id, user_id, role, created_at)
				SELECT $1, $2, $3, $4
				WHERE NOT EXISTS (
					SELECT 1 FROM platform_role_assignments WHERE user_id = $2 AND role = $3 AND market_id IS NULL)`,
				uuid.New(), a.ID, a.PlatformRole, now); err != nil {
				return fmt.Errorf("seed platform role for %s: %w", a.Email, err)
			}
		}
	}

	for _, l := range Listings() {
		var publishedAt *time.Time
		if l.Status == propertyrequests.ListingStatusPublished {
			publishedAt = &now
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO listings (id, market_id, title, status, price_amount, currency_code,
			                      organisation_id, agent_user_id, seller_user_id, published_at, expires_at, created_at, updated_at)
			VALUES ($1, $2, $3, $4::text::listing_status, $5::text::numeric, $6, $7, $8, $9, $10, $11, $12, $12)
			ON CONFLICT (id) DO UPDATE
			SET market_id = EXCLUDED.market_id, status = EXCLUDED.status,
			    organisation_id = EXCLUDED.organisation_id, agent_user_id = EXCLUDED.agent_user_id,
			    seller_user_id = EXCLUDED.seller_user_id, expires_at = EXCLUDED.expires_at,
			    published_at = COALESCE(listings.published_at, EXCLUDED.published_at),
			    updated_at = EXCLUDED.updated_at`,
			l.ID, l.MarketID, "Sample listing "+l.ID.String()[len(l.ID.String())-2:], l.Status, sampleListingPrice, sampleListingCurrency,
			l.OrganisationID, l.AgentUserID, l.SellerUserID, publishedAt, l.ExpiresAt, now); err != nil {
			return fmt.Errorf("seed listing %s: %w", l.ID, err)
		}
	}

	for _, p := range Profiles() {
		if _, err := tx.Exec(ctx, `
			INSERT INTO directory_profiles (id, market_id, kind, slug, name, organisation_id, agent_user_id, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, 'ACTIVE', $8, $8)
			ON CONFLICT (id) DO UPDATE
			SET market_id = EXCLUDED.market_id, kind = EXCLUDED.kind, slug = EXCLUDED.slug, name = EXCLUDED.name,
			    organisation_id = EXCLUDED.organisation_id, agent_user_id = EXCLUDED.agent_user_id,
			    status = 'ACTIVE', updated_at = EXCLUDED.updated_at`,
			p.ID, markets.ZimbabweID, p.Kind, p.Slug, p.Name, p.Organisation.ID, p.AgentUserID, now); err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return fmt.Errorf("seed profile %s: that kind and slug already belong to a different profile in this database", p.Key())
			}
			return fmt.Errorf("seed profile %s: %w", p.Key(), err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
