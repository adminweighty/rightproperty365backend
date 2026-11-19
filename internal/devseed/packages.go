package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/modules/packages"
)

// Fixed IDs of the sample listing packages (API 0.37.0) and of Sample
// Estates' sample assignment, so the seed is idempotent.
var (
	PackageStarterZW      = id("6001")
	PackageBusinessZW     = id("6002")
	PackageProfessionalZW = id("6003")
	PackageStarterGB      = id("6011")
	PackageBusinessGB     = id("6012")
	PackageProfessionalGB = id("6013")
	SampleEstatesPackage  = id("6101")
)

// SamplePackageNote is the note of the seed's sample assignment.
const SamplePackageNote = "Sample assignment (development seed)"

// SamplePackages are the development listing packages: Starter (20, the
// default), Business (40) and Professional (60) in ZW (USD) and GB (GBP),
// the markets with sample data. Every price is a SAMPLE (Price.Sample): the
// real, approved prices are entered by an administrator. Other markets have
// no packages, so they are not limited.
func SamplePackages(now time.Time) []packages.Package {
	now = now.UTC().Truncate(time.Microsecond)
	type tier struct {
		code, name, description string
		limit, sort             int
		def                     bool
	}
	tiers := []tier{
		{"STARTER", "Starter", "For a small agency or a landlord with a few properties.", 20, 10, true},
		{"BUSINESS", "Business", "For a growing agency with a steady stock of listings.", 40, 20, false},
		{"PROFESSIONAL", "Professional", "For a large agency or developer with many listings at once.", 60, 30, false},
	}
	// Sample prices per market: monthly, quarterly, annual.
	prices := map[string][3][3]string{
		MarketCode: {{"25.00", "70.00", "250.00"}, {"45.00", "125.00", "450.00"}, {"65.00", "180.00", "650.00"}},
		MarketUK:   {{"20.00", "55.00", "200.00"}, {"35.00", "95.00", "350.00"}, {"50.00", "140.00", "500.00"}},
	}
	currency := map[string]string{MarketCode: "USD", MarketUK: "GBP"}
	ids := map[string][3]uuid.UUID{
		MarketCode: {PackageStarterZW, PackageBusinessZW, PackageProfessionalZW},
		MarketUK:   {PackageStarterGB, PackageBusinessGB, PackageProfessionalGB},
	}
	var out []packages.Package
	for _, market := range []string{MarketUK, MarketCode} {
		for i, t := range tiers {
			p := packages.Package{ID: ids[market][i], MarketCode: market, Code: t.code, Name: t.name, Description: t.description,
				ListingLimit: t.limit, Active: true, Default: t.def, Sort: t.sort, CreatedAt: now, UpdatedAt: now}
			for j, term := range packages.Terms() {
				p.Prices = append(p.Prices, packages.Price{Term: term, Amount: prices[market][i][j], Currency: currency[market], Sample: true})
			}
			out = append(out, p)
		}
	}
	return out
}

// SampleAssignment is Sample Estates on Business in ZW, open-ended from the
// seed date.
func SampleAssignment(now time.Time) packages.Assignment {
	now = now.UTC().Truncate(time.Microsecond)
	return packages.Assignment{ID: SampleEstatesPackage, Account: packages.Account{Kind: packages.KindOrganisation, ID: SampleEstates.ID},
		MarketCode: MarketCode, PackageID: PackageBusinessZW, StartsOn: packages.Day(now), Source: packages.SourceAdmin,
		Note: SamplePackageNote, AssignedByName: "Development seed", CreatedAt: now}
}

// SeedMemoryPackages puts the sample packages and the sample assignment into
// the memory store (without audit records).
func SeedMemoryPackages(store *packages.MemoryStore, now time.Time) {
	for _, p := range SamplePackages(now) {
		store.PutPackage(p)
	}
	store.PutAssignment(SampleAssignment(now))
}

// seedPostgresPackages upserts the sample packages by market and code
// (re-running resets their name, limit, description, order and the
// Starter default; a price an administrator entered is kept, a sample price
// is reset) and adds the sample assignment unless Sample Estates already
// holds another package in ZW.
func seedPostgresPackages(ctx context.Context, tx pgx.Tx, now time.Time) error {
	actual := map[uuid.UUID]uuid.UUID{}
	for _, p := range SamplePackages(now) {
		if p.Default {
			if _, err := tx.Exec(ctx, `UPDATE listing_packages SET is_default = false, updated_at = $3
				WHERE market_code = $1 AND is_default AND code <> $2`, p.MarketCode, p.Code, now); err != nil {
				return fmt.Errorf("seed packages: %w", err)
			}
		}
		var got uuid.UUID
		if err := tx.QueryRow(ctx, `
			INSERT INTO listing_packages (id, market_code, code, name, description, listing_limit, active, is_default, sort_order, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, true, $7, $8, $9, $9)
			ON CONFLICT (market_code, code) DO UPDATE
			SET name = EXCLUDED.name, description = EXCLUDED.description, listing_limit = EXCLUDED.listing_limit, active = true,
			    is_default = EXCLUDED.is_default, sort_order = EXCLUDED.sort_order, updated_at = EXCLUDED.updated_at
			RETURNING id`,
			p.ID, p.MarketCode, p.Code, p.Name, p.Description, p.ListingLimit, p.Default, p.Sort, now).Scan(&got); err != nil {
			return fmt.Errorf("seed package %s/%s: %w", p.MarketCode, p.Code, err)
		}
		actual[p.ID] = got
		for _, pr := range p.Prices {
			if _, err := tx.Exec(ctx, `
				INSERT INTO listing_package_prices (package_id, billing_term, amount, currency_code, sample)
				VALUES ($1, $2, $3::text::numeric, $4, true)
				ON CONFLICT (package_id, billing_term) DO UPDATE
				SET amount = EXCLUDED.amount, currency_code = EXCLUDED.currency_code
				WHERE listing_package_prices.sample`, got, pr.Term, pr.Amount, pr.Currency); err != nil {
				return fmt.Errorf("seed package price %s/%s: %w", p.Code, pr.Term, err)
			}
		}
	}
	a := SampleAssignment(now)
	if _, err := tx.Exec(ctx, `
		INSERT INTO listing_package_assignments (id, organisation_id, market_code, package_id, starts_on, source, note, assigned_by_name, created_at)
		SELECT $1, $2, $3, $4, $5::date, $6, $7, $8, $9
		WHERE NOT EXISTS (SELECT 1 FROM listing_package_assignments
		                  WHERE organisation_id = $2 AND market_code = $3 AND cancelled_at IS NULL AND (ends_on IS NULL OR ends_on >= $5::date))
		ON CONFLICT (id) DO NOTHING`,
		a.ID, a.Account.ID, a.MarketCode, actual[a.PackageID], a.StartsOn, a.Source, a.Note, a.AssignedByName, a.CreatedAt); err != nil {
		return fmt.Errorf("seed the sample package assignment: %w", err)
	}
	return nil
}
