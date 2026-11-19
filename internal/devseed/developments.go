package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/developments"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/reference"
)

// The development showcase samples (API 0.44.0, migration 0046), all of
// Sample Developments (sample-developments@example.test, the OWNER of the
// DEVELOPER organisation):
//
//	ZW/borrowdale-heights  "Borrowdale Heights"   PUBLISHED, SELLING, 2 phases, 3 unit types
//	                        (one price on request), 3 sample photos, a YouTube tour, and
//	                        one linked published listing (a cluster home of unit type 1)
//	ZW/ruwa-gardens-estate "Ruwa Gardens Estate"  DRAFT (not public), 1 phase, 1 unit type
//
// The linked listing is seeded with the projects (not with the other sample
// listings), so the sample listing counts are unchanged.

// Fixed IDs of the samples (f6xx).
var (
	BorrowdaleHeightsID    = id("f601")
	RuwaGardensID          = id("f602")
	BorrowdalePhase1ID     = id("f611")
	BorrowdalePhase2ID     = id("f612")
	RuwaPhase1ID           = id("f613")
	ClusterHomeUnitTypeID  = id("f621")
	ApartmentUnitTypeID    = id("f622")
	PenthouseUnitTypeID    = id("f623")
	RuwaStandUnitTypeID    = id("f624")
	DevelopmentListingID   = id("f631")
	borrowdalePhotoIDs     = []uuid.UUID{id("f641"), id("f642"), id("f643")}
	ruwaPhotoID            = id("f644")
	DevelopmentListingSlug = "three-bedroom-cluster-home-at-borrowdale-heights-zw-200401"
)

// Addresses of the samples.
const (
	BorrowdaleHeightsSlug = "borrowdale-heights"
	RuwaGardensSlug       = "ruwa-gardens-estate"
	SampleDevelopersSlug  = "sample-developments"
)

// DevelopmentAmenityRows are the development amenities (migration 0046).
func DevelopmentAmenityRows() []reference.Item {
	pairs := []string{
		"gated-community", "Gated community", "security-24h", "24-hour security", "clubhouse", "Clubhouse",
		"swimming-pool", "Swimming pool", "gym", "Gym", "childrens-play-area", "Children's play area",
		"borehole-water", "Borehole water", "solar-backup", "Solar back-up power", "fibre-internet", "Fibre internet",
		"paved-roads", "Paved roads", "landscaped-gardens", "Landscaped gardens", "walking-trails", "Walking trails",
		"visitor-parking", "Visitor parking", "retail-centre", "Retail centre",
	}
	var out []reference.Item
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, reference.Item{Code: pairs[i], Label: pairs[i+1], SortOrder: (i/2 + 1) * 10, Active: true})
	}
	return out
}

// sampleDeveloper returns the Sample Developments profile.
func sampleDeveloper() Profile {
	for _, p := range Profiles() {
		if p.Kind == developments.KindDevelopers && p.Slug == SampleDevelopersSlug {
			return p
		}
	}
	panic("devseed: the Sample Developments profile is missing")
}

// DevelopmentOrganisationID is the organisation of Sample Developments.
func DevelopmentOrganisationID() uuid.UUID { return sampleDeveloper().Organisation.ID }

// DevelopmentOwnerEmail is the sign-in of the sample developer.
func DevelopmentOwnerEmail() string { return BusinessOwnerEmail(SampleDevelopersSlug) }

func sampleDevPhoto(photoID uuid.UUID, position int, file, alt string, w, h int, approved *time.Time, at time.Time) listings.StoredPhoto {
	return listings.StoredPhoto{ID: photoID, Position: position, URL: PhotoPathPrefix + file, Alt: alt, Width: w, Height: h,
		ApprovedAt: approved, CreatedAt: at}
}

// DevelopmentRecords returns the two sample projects.
func DevelopmentRecords(now time.Time) []developments.Development {
	now = now.UTC().Truncate(time.Hour)
	dev := sampleDeveloper()
	var owner uuid.UUID
	for _, a := range Accounts() {
		if a.Email == DevelopmentOwnerEmail() {
			owner = a.ID
		}
	}
	reviewer := identity.SeedAdminUserID
	created, submitted, published := now.AddDate(0, 0, -30), now.AddDate(0, 0, -21), now.AddDate(0, 0, -20)
	lat, lon := -17.7531, 31.0927
	three, two, four, one := 3, 2, 4, 1
	i := func(v int) *int { return &v }
	return []developments.Development{
		{
			ID: BorrowdaleHeightsID, OrganisationID: dev.Organisation.ID, ProfileID: dev.ID, MarketCode: MarketCode,
			Slug: BorrowdaleHeightsSlug, Name: "Borrowdale Heights",
			Summary:     "Secure cluster homes and apartments a short drive from Borrowdale Village.",
			Description: "Borrowdale Heights is a sample gated development of cluster homes and low-rise apartments in Borrowdale, Harare.\n\nPhase 1 (cluster homes) is selling now; phase 2 adds apartments and penthouses around a clubhouse and landscaped gardens. Staged payment plans are available.",
			Status:      developments.StatusSelling, Moderation: developments.ModerationPublished, Completion: "2027-Q2",
			CityCode: "harare", SuburbCode: "borrowdale", Latitude: &lat, Longitude: &lon, LocationPrecision: listings.PrecisionApproximate,
			Amenities: []string{"gated-community", "security-24h", "clubhouse", "swimming-pool", "borehole-water", "solar-backup", "landscaped-gardens"},
			Highlights: []developments.Highlight{
				{Label: "Expected rental yield", Value: "8–10% a year"},
				{Label: "Payment plan", Value: "30% deposit, balance over 24 months"},
			},
			YouTubeVideoID: "aqz-KE-bpKQ",
			SubmittedAt:    &submitted, SubmittedBy: &owner, ReviewedAt: &published, ReviewedBy: &reviewer, PublishedAt: &published,
			CreatedBy: &owner, CreatedAt: created, UpdatedAt: published,
			Phases: []developments.Phase{
				{ID: BorrowdalePhase1ID, Name: "Phase 1: cluster homes", Status: developments.StatusSelling, Completion: "2026-Q4", Position: 0, CreatedAt: created,
					UnitTypes: []developments.UnitType{
						{ID: ClusterHomeUnitTypeID, PhaseID: BorrowdalePhase1ID, Name: "Three-bedroom cluster home", Bedrooms: &three, Bathrooms: &two,
							SizeMin: i(180), SizeMax: i(210), PriceFrom: "185000.00", PriceTo: "240000.00", Currency: "USD",
							UnitsTotal: i(24), UnitsAvailable: i(9), Position: 0, CreatedAt: created},
					}},
				{ID: BorrowdalePhase2ID, Name: "Phase 2: apartments", Status: developments.StatusUnderConstruction, Completion: "2027-06-30", Position: 1, CreatedAt: created,
					UnitTypes: []developments.UnitType{
						{ID: ApartmentUnitTypeID, PhaseID: BorrowdalePhase2ID, Name: "Two-bedroom apartment", Bedrooms: &two, Bathrooms: &one,
							SizeMin: i(85), SizeMax: i(95), PriceFrom: "95000.00", Currency: "USD", UnitsTotal: i(40), UnitsAvailable: i(31),
							Position: 0, CreatedAt: created},
						{ID: PenthouseUnitTypeID, PhaseID: BorrowdalePhase2ID, Name: "Four-bedroom penthouse", Bedrooms: &four, Bathrooms: &three,
							SizeMin: i(240), PriceOnRequest: true, UnitsTotal: i(4), UnitsAvailable: i(4), Position: 1, CreatedAt: created},
					}},
			},
			Photos: []listings.StoredPhoto{
				sampleDevPhoto(borrowdalePhotoIDs[0], 0, "listing-house-borrowdale.jpg", "Two-storey cluster home of the first phase", 1200, 900, &published, created),
				sampleDevPhoto(borrowdalePhotoIDs[1], 1, "listing-townhouse-mount-pleasant.jpg", "Row of cluster homes beside the estate road", 1200, 796, &published, created),
				sampleDevPhoto(borrowdalePhotoIDs[2], 2, "listing-house-glen-lorne.jpg", "Show house beside the pool", 1200, 800, &published, created),
			},
			Links: []developments.Link{{ListingID: DevelopmentListingID, UnitTypeID: &ClusterHomeUnitTypeID, CreatedAt: published}},
		},
		{
			ID: RuwaGardensID, OrganisationID: dev.Organisation.ID, ProfileID: dev.ID, MarketCode: MarketCode,
			Slug: RuwaGardensSlug, Name: "Ruwa Gardens Estate",
			Summary:     "Serviced residential stands in Ruwa, being prepared.",
			Description: "Sample draft: serviced stands with paved roads and borehole water in Ruwa. The developer is still writing it.",
			Status:      developments.StatusPlanned, Moderation: developments.ModerationDraft, Completion: "2028-Q1",
			CityCode: "harare", SuburbCode: "ruwa", LocationPrecision: listings.PrecisionExact,
			Amenities: []string{"paved-roads", "borehole-water"}, Highlights: []developments.Highlight{},
			CreatedBy: &owner, CreatedAt: now.AddDate(0, 0, -3), UpdatedAt: now.AddDate(0, 0, -2),
			Phases: []developments.Phase{
				{ID: RuwaPhase1ID, Name: "Phase 1: serviced stands", Status: developments.StatusPlanned, Position: 0, CreatedAt: now.AddDate(0, 0, -3),
					UnitTypes: []developments.UnitType{
						{ID: RuwaStandUnitTypeID, PhaseID: RuwaPhase1ID, Name: "Residential stand", SizeMin: i(400), SizeMax: i(600),
							PriceFrom: "18000.00", PriceTo: "26000.00", Currency: "USD", UnitsTotal: i(120), UnitsAvailable: i(120), CreatedAt: now.AddDate(0, 0, -3)},
					}},
			},
			Photos: []listings.StoredPhoto{
				sampleDevPhoto(ruwaPhotoID, 0, "listing-stand-ruwa.jpg", "", 1200, 706, nil, now.AddDate(0, 0, -3)),
			},
			Links: []developments.Link{},
		},
	}
}

// DevelopmentListingRecord is the published listing linked to Borrowdale
// Heights: a cluster home of the first phase, owned by Sample Developments.
func DevelopmentListingRecord(now time.Time) listings.Record {
	now = now.UTC().Truncate(time.Hour)
	dev := sampleDeveloper()
	org := dev.Organisation.ID
	published := now.AddDate(0, 0, -10)
	three, two, area := 3, 2, 195
	reviewer := identity.SeedAdminUserID
	ref := listings.ProfileRef{Kind: dev.Kind, Slug: dev.Slug, Name: dev.Name, Verified: true}
	return listings.Record{
		ID: DevelopmentListingID, Slug: DevelopmentListingSlug, Reference: "RP-ZW-200401",
		Title: "Three-bedroom cluster home at Borrowdale Heights", Deal: "sale", Category: "residential", PropertyType: "townhouse",
		Price: listings.Money{Amount: "198000.00", Currency: "USD"}, Suburb: "borrowdale", City: "harare", MarketCode: MarketCode,
		Bedrooms: &three, Bathrooms: &two, FloorArea: &area,
		Summary:     "New cluster home in a secure estate, ready in the first phase of Borrowdale Heights.",
		Description: "Sample listing of a new three-bedroom cluster home at Borrowdale Heights, with a private garden and a double carport.",
		Features:    []string{},
		Photos:      []listings.Photo{{URL: PhotoPathPrefix + "listing-house-borrowdale.jpg", Alt: "Two-storey cluster home of the first phase", Width: 1200, Height: 900}},
		Status:      listings.StatusPublished, PublishedAt: published, ExpiresAt: published.AddDate(0, 0, PublicationDays), UpdatedAt: published,
		OrganisationID: &org, ReviewedAt: &published, ReviewedBy: &reviewer,
		Advertiser: listings.Advertiser{Type: listings.AdvertiserAgency, Name: dev.Name, Verified: true, Agency: &ref},
	}
}

// SeedMemoryDevelopments puts the sample projects (and their linked
// listing) into the memory stores.
func SeedMemoryDevelopments(store *developments.MemoryStore, ls *listings.MemoryStore, now time.Time) {
	if ls != nil {
		ls.Put(DevelopmentListingRecord(now))
	}
	for _, d := range DevelopmentRecords(now) {
		store.Put(d)
	}
}

// SeedPostgresDevelopments writes the sample projects and their linked
// listing (needs migration 0046; after SeedPostgres, which writes the
// organisation, its profile and its owner). Re-runnable: the listing is
// upserted, and a project that exists is saved over (its children upserted
// by their fixed IDs, so enquiries that quote them keep their links).
func SeedPostgresDevelopments(ctx context.Context, pool *pgxpool.Pool, now time.Time) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := seedListingTx(ctx, tx, DevelopmentListingRecord(now), now.UTC().Truncate(time.Microsecond)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	store := developments.NewPostgresStore(pool)
	for _, d := range DevelopmentRecords(now) {
		existing, err := store.Get(ctx, d.ID)
		switch {
		case errors.Is(err, developments.ErrNotFound):
			err = store.Create(ctx, d)
		case err == nil:
			_, err = store.Save(ctx, d, existing.UpdatedAt, nil)
		}
		if errors.Is(err, developments.ErrSlugTaken) {
			return fmt.Errorf("seed development %s/%s: that slug already belongs to a different development in this database", d.MarketCode, d.Slug)
		}
		if err != nil {
			return fmt.Errorf("seed development %s: %w", d.Slug, err)
		}
	}
	return nil
}
