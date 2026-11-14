package devseed

import (
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
)

// PhotoPathPrefix is where the web app is expected to serve the sample
// photos. The files are the web repository's src/assets/images/*.jpg; object
// storage and uploads do not exist yet.
const PhotoPathPrefix = "/assets/images/"

// PublicationDays is the length of a publication cycle (FR-006).
const PublicationDays = 90

// sampleEpoch is the day the web sample data was written. A sample listing
// published N days before it is seeded as published N days before "now", so
// the sample listings are always recent and never all expire at once.
var sampleEpoch = time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)

// ExpiredListingSlug is the slug of the one sample listing that is no longer available.
const ExpiredListingSlug = "expired-two-bedroom-flat-avenues"

// ListingRecords returns the 18 sample listings with the web's titles,
// prices, descriptions, features, photos, slugs and references, each tied to
// its organisation and assigned agent (or direct seller), plus one EXPIRED
// listing. Publication dates are relative to now.
func ListingRecords(now time.Time) []listings.Record {
	now = now.UTC().Truncate(time.Hour)
	owners := map[uuid.UUID]propertyrequests.Listing{}
	for _, l := range Listings() {
		owners[l.ID] = l
	}
	agencies, agents := map[uuid.UUID]listings.ProfileRef{}, map[uuid.UUID]listings.ProfileRef{}
	verified := map[string]bool{}
	for _, p := range Sample().Profiles {
		verified[p.Kind+"/"+p.Slug] = p.Verified
	}
	for _, p := range Profiles() {
		ref := listings.ProfileRef{Kind: p.Kind, Slug: p.Slug, Name: p.Name, Verified: verified[p.Key()]}
		switch {
		case p.AgentUserID != nil:
			agents[*p.AgentUserID] = ref
		default:
			// The organisation's own profile: an agency's, or a host's.
			agencies[p.Organisation.ID] = ref
		}
	}

	var out []listings.Record
	for _, l := range AllListings() {
		id := uuid.MustParse(l.ID)
		owner := owners[id]
		published := now
		if t, err := time.Parse(time.RFC3339, l.PublishedAt); err == nil {
			published = now.Add(-sampleEpoch.Sub(t))
		}
		r := listings.Record{
			ID: id, Slug: l.Slug, Reference: l.Reference, Title: l.Title, Deal: l.Deal, Category: l.Category,
			PropertyType: Code(l.PropertyType), Price: listings.Money{Amount: l.Price.Amount, Currency: l.Price.Currency},
			PricePeriod: l.PricePeriod, Suburb: Code(l.Suburb), City: Code(l.City), MarketCode: l.MarketCode,
			Bedrooms: l.Bedrooms, Bathrooms: l.Bathrooms, FloorArea: l.FloorArea, LandArea: l.LandArea,
			MaxGuests: l.MaxGuests, MinNights: l.MinNights,
			Summary: l.Summary, Description: l.Description, Featured: l.Featured,
			Photos: []listings.Photo{{URL: PhotoPathPrefix + l.Image.File, Alt: l.Image.Alt, Width: l.Image.Width, Height: l.Image.Height}},
			Status: listings.StatusPublished, PublishedAt: published, ExpiresAt: published.AddDate(0, 0, PublicationDays), UpdatedAt: published,
			OrganisationID: owner.OrganisationID, AgentUserID: owner.AgentUserID, SellerUserID: owner.SellerUserID,
		}
		for _, f := range l.Features {
			r.Features = append(r.Features, Code(f))
		}
		if owner.OrganisationID != nil {
			agency := agencies[*owner.OrganisationID]
			r.Advertiser = listings.Advertiser{Type: listings.AdvertiserAgency, Name: agency.Name, Verified: agency.Verified, Agency: &agency}
			if owner.AgentUserID != nil {
				if agent, ok := agents[*owner.AgentUserID]; ok {
					r.Advertiser.Agent = &agent
				}
			}
		} else {
			// A direct seller has no directory profile: only a display name.
			r.Advertiser = listings.Advertiser{Type: listings.AdvertiserPrivateSeller, Name: l.Advertiser.Name, Verified: l.Advertiser.Verified}
		}
		out = append(out, r)
	}

	// The one listing that is no longer available (requests for it get 410).
	expired := owners[ExpiredListingID]
	two, one := 2, 1
	agency := agencies[SampleEstates.ID]
	out = append(out, listings.Record{
		ID: ExpiredListingID, Slug: ExpiredListingSlug, Reference: "RP-ZW-10099", Title: "Two-bedroom flat near the park",
		Deal: "rent", Category: "residential", PropertyType: "apartment", Price: listings.Money{Amount: "700.00", Currency: "USD"},
		PricePeriod: "month", Suburb: "avenues", City: "harare", MarketCode: MarketCode, Bedrooms: &two, Bathrooms: &one,
		Summary: "Sample listing whose publication period has ended.", Description: "This sample listing expired and is no longer shown.",
		Photos: []listings.Photo{}, Status: "EXPIRED",
		PublishedAt: now.AddDate(0, 0, -120), ExpiresAt: now.AddDate(0, 0, -30), UpdatedAt: now.AddDate(0, 0, -30),
		OrganisationID: expired.OrganisationID,
		Advertiser:     listings.Advertiser{Type: listings.AdvertiserAgency, Name: agency.Name, Verified: agency.Verified, Agency: &agency},
	})
	return out
}

// ProfileRecords returns the 29 sample profiles with the web's content.
func ProfileRecords(now time.Time) []directory.Record {
	updated := now.UTC().Truncate(24 * time.Hour)
	seeded := map[string]Profile{}
	for _, p := range Profiles() {
		seeded[p.Key()] = p
	}
	var out []directory.Record
	for _, p := range Sample().Profiles {
		s := seeded[p.Kind+"/"+p.Slug]
		r := directory.Record{
			ID: s.ID, Kind: p.Kind, Slug: p.Slug, Name: p.Name, Role: p.Role, Tagline: p.Tagline, Description: p.About,
			CityCode: Code(p.City), Areas: p.Areas, Services: p.Services, Verified: p.Verified, Sponsored: p.Sponsored,
			Since: p.Since, Status: directory.StatusActive, UpdatedAt: updated,
			OrganisationID: s.Organisation.ID, AgentUserID: s.AgentUserID,
		}
		if p.Category != "" {
			r.TradeCode = Code(p.Category)
		}
		for _, o := range p.Offerings {
			r.Offerings = append(r.Offerings, directory.Offering{Name: o.Name, Description: o.Description})
		}
		out = append(out, r)
	}
	return out
}

// SeedMemoryContent puts the sample listings and profiles into the in-memory stores.
func SeedMemoryContent(ls *listings.MemoryStore, ds *directory.MemoryStore, now time.Time) {
	for _, r := range ListingRecords(now) {
		ls.Put(r)
	}
	for _, r := range ProfileRecords(now) {
		ds.Put(r)
	}
}
