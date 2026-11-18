package devseed

import (
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/auth"
)

// This file holds the sample data that is NOT in the web app's files
// (data/sample.json mirrors those and stays as it is): the United Kingdom
// market and the short stays. Everything here is appended after the web's
// data, so no existing sample ID moves.

// MarketUK is the United Kingdom market.
const MarketUK = "GB"

// MarketMauritius is a country WITHOUT reference places: the sample listing
// there names its city and region as free text.
const MarketMauritius = "MU"

// BackendListingCount is the number of PUBLISHED sample listings added here
// (IDs 19..25), on top of the web's ListingCount.
const BackendListingCount = 7

// MarketID returns the ID of a country by its code (Zimbabwe when the code
// is not a country code). Every ISO country has one: see markets.IDFor.
func MarketID(code string) uuid.UUID {
	if id := markets.IDFor(code); id != uuid.Nil {
		return id
	}
	return markets.ZimbabweID
}

// listingMarkets is the country of every sample listing, by listing ID. A
// listing's country is its own: an organisation in one country may list a
// property in another.
func listingMarkets() map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, l := range AllListings() {
		out[uuid.MustParse(l.ID)] = l.MarketCode
	}
	return out
}

// MarketOf returns the market code of an organisation (ZW unless it says otherwise).
func MarketOf(o *Organisation) string {
	if o != nil && o.Market != "" {
		return o.Market
	}
	return MarketCode
}

// The UK sample organisations and the owner of the agency.
var (
	SampleHomesLondon = &Organisation{ID: id("b007"), Name: "Sample Homes London", Kind: auth.KindAgency, EnquiryEmail: "enquiries@sample-homes-london.example.test", Market: MarketUK}

	SampleHomesLondonOwnerID    = id("d00b")
	SampleHomesLondonOwnerEmail = "sample-homes-london@example.test"
)

// Slugs of the sample hosts that own the Zimbabwean short stays, and of the
// UK host (a business like the others: its own organisation and one OWNER).
const (
	hostFallsStays     = "sample-falls-stays"
	hostShowrooms      = "sample-showrooms"
	hostStaysEdinburgh = "sample-stays-edinburgh"
)

// backendBusinesses are appended to the web's businesses, so their IDs follow.
var backendBusinesses = []business{
	{businessenquiries.KindHosts, hostStaysEdinburgh, "Sample Stays Edinburgh"},
}

// businessListings says which sample listings a business's owner is
// responsible for: the hosts own the short stays.
var businessListings = map[string][]int{
	hostFallsStays: {22, 24},
	hostShowrooms:  {23, ShowroomListingNumber},
}

// backendProfiles are the directory profiles of the UK organisations.
var backendProfiles = []SampleProfile{
	{
		Slug: "sample-homes-london", Kind: "agencies", Name: "Sample Homes London", Role: "Estate agency", Market: MarketUK,
		Tagline: "Sales, lettings and short stays in London and Manchester.",
		About: "Sample Homes London is a sample estate agency for the United Kingdom market. It sells and lets homes in London " +
			"and Manchester and looks after a small number of short-stay apartments.",
		City: "London", Areas: []string{"Kensington", "Shoreditch", "Manchester"},
		Services: []string{"Residential sales", "Lettings", "Short-stay management"},
		Verified: true, Since: since(2012),
	},
	{
		Slug: hostStaysEdinburgh, Kind: "hosts", Name: "Sample Stays Edinburgh", Role: "Short-stay host", Market: MarketUK,
		Tagline: "Serviced apartments and holiday homes in Edinburgh.",
		About: "Sample Stays Edinburgh is a sample host for the United Kingdom market, offering serviced apartments and holiday " +
			"homes by the night in the Old Town and the New Town.",
		City: "Edinburgh", Areas: []string{"Old Town", "New Town"},
		Services: []string{"Serviced apartments", "Holiday homes"},
		Verified: true, Since: since(2018),
	},
}

func guests(n int) *int { return &n }

// backendListing fills a SampleListing; the fields that never vary are set here.
func backendListing(n int, market, reference, slug, image string, width, height int, published string) SampleListing {
	l := SampleListing{
		ID: propertyrequests.SeedListingID(n).String(), Slug: slug, Reference: reference, MarketCode: market, PublishedAt: published,
	}
	l.Image.File, l.Image.Width, l.Image.Height = image, width, height
	return l
}

// Video ids of the sample video tours: well-known public videos that have
// been on YouTube for many years (Big Buck Bunny by the Blender Foundation,
// and "Me at the zoo", the first YouTube video). They stand in for real
// property tours; the seed stores the ids only and never calls YouTube.
const (
	SampleYouTubeVideoID  = "aqz-KE-bpKQ"
	SampleYouTubeVideoID2 = "jNQXAC9IVRw"
)

// BackendListings returns the six sample listings added on the backend:
// three in the United Kingdom, priced in GBP (a sale, a monthly rent and a
// short stay), two short stays in Zimbabwe, priced in USD per night and
// owned by the sample hosts, and one short stay in Mauritius, priced in MUR,
// whose city and region are free text. They are published before the web's listings,
// so "newest first" still starts with those.
func BackendListings() []SampleListing {
	london := backendListing(19, MarketUK, "RP-GB-10001", "three-bedroom-terraced-house-kensington", "listing-townhouse-greendale.jpg", 1200, 813, "2026-10-20T08:00:00Z")
	london.Title, london.Deal, london.Category, london.PropertyType = "Three-bedroom terraced house near the park", "sale", "residential", "Townhouse"
	london.Price.Amount, london.Price.Currency = "1250000.00", "GBP"
	london.Suburb, london.City = "Kensington", "London"
	london.Bedrooms, london.Bathrooms, london.FloorArea = guests(3), guests(2), guests(140)
	london.Summary = "Sample listing: a terraced family house on a quiet street in Kensington."
	london.Description = "A sample listing for the United Kingdom market. Three bedrooms over three floors, a private garden and a study, a short walk from the park and the Underground."
	london.Features, london.Image.Alt = []string{"Private garden", "Study", "Fibre ready"}, "Terraced house with a small front garden"
	// A video tour (FR-011): a stand-in public video, starting 30 seconds in.
	london.YouTubeVideoID, london.YouTubeStartSeconds = SampleYouTubeVideoID, guests(30)

	manchester := backendListing(20, MarketUK, "RP-GB-10002", "two-bedroom-apartment-northern-quarter", "listing-apartment-avenues.jpg", 1200, 800, "2026-10-18T08:00:00Z")
	manchester.Title, manchester.Deal, manchester.Category, manchester.PropertyType = "Two-bedroom apartment in a converted warehouse", "rent", "residential", "Apartment"
	manchester.Price.Amount, manchester.Price.Currency, manchester.PricePeriod = "1650.00", "GBP", listings.PricePeriodMonth
	manchester.Suburb, manchester.City = "Northern Quarter", "Manchester"
	manchester.Bedrooms, manchester.Bathrooms, manchester.FloorArea = guests(2), guests(2), guests(85)
	manchester.Summary = "Sample listing: a furnished apartment to rent in Manchester's Northern Quarter."
	manchester.Description = "A sample listing for the United Kingdom market. Two double bedrooms, a balcony and secure parking in a converted warehouse, close to the trams."
	manchester.Features, manchester.Image.Alt = []string{"Balcony", "Secure parking", "Lift"}, "Apartment block seen from the street"

	shoreditch := backendListing(21, MarketUK, "RP-GB-10003", "serviced-apartment-shoreditch", "listing-apartment-belgravia.jpg", 1200, 900, "2026-10-16T08:00:00Z")
	shoreditch.Title, shoreditch.Deal, shoreditch.Category, shoreditch.PropertyType = "Serviced one-bedroom apartment for short stays", "rent", listings.CategoryShortStay, "Serviced apartment"
	shoreditch.Price.Amount, shoreditch.Price.Currency, shoreditch.PricePeriod = "185.00", "GBP", listings.PricePeriodNight
	shoreditch.Suburb, shoreditch.City = "Shoreditch", "London"
	shoreditch.Bedrooms, shoreditch.Bathrooms, shoreditch.FloorArea = guests(1), guests(1), guests(48)
	shoreditch.MaxGuests, shoreditch.MinNights = guests(2), guests(2)
	shoreditch.Summary = "Sample listing: a serviced apartment let by the night in Shoreditch."
	shoreditch.Description = "A sample short stay for the United Kingdom market. A one-bedroom serviced apartment for up to two guests, cleaned weekly, with fast broadband. Minimum stay two nights."
	shoreditch.Features, shoreditch.Image.Alt = []string{"Serviced", "Fibre ready", "Lift"}, "Modern apartment building"

	falls := backendListing(22, MarketCode, "RP-ZW-10019", "safari-lodge-victoria-falls", "listing-house-victoria-falls.jpg", 1200, 900, "2026-10-14T08:00:00Z")
	falls.Title, falls.Deal, falls.Category, falls.PropertyType = "Thatched lodge near the Falls for up to eight guests", "rent", listings.CategoryShortStay, "Lodge"
	falls.Price.Amount, falls.Price.Currency, falls.PricePeriod = "320.00", "USD", listings.PricePeriodNight
	falls.Suburb, falls.City = "Low Density", "Victoria Falls"
	falls.Bedrooms, falls.Bathrooms, falls.FloorArea, falls.LandArea = guests(4), guests(3), guests(260), guests(2000)
	falls.MaxGuests, falls.MinNights = guests(8), guests(2)
	falls.Summary = "Sample listing: a self-catering lodge let by the night in Victoria Falls."
	falls.Description = "A sample short stay. A thatched four-bedroom lodge with a swimming pool and a veranda, ten minutes from the Falls. Sleeps eight. Minimum stay two nights."
	falls.Features, falls.Image.Alt = []string{"Swimming pool", "Thatched roof", "Veranda", "Borehole"}, "Thatched house in a garden"
	falls.YouTubeVideoID = SampleYouTubeVideoID2

	harare := backendListing(23, MarketCode, "RP-ZW-10020", "garden-guest-house-avondale", "listing-cottage-greystone-park.jpg", 1200, 900, "2026-10-12T08:00:00Z")
	harare.Title, harare.Deal, harare.Category, harare.PropertyType = "Garden guest house with a private entrance", "rent", listings.CategoryShortStay, "Guest house"
	harare.Price.Amount, harare.Price.Currency, harare.PricePeriod = "75.00", "USD", listings.PricePeriodNight
	harare.Suburb, harare.City = "Avondale", "Harare"
	harare.Bedrooms, harare.Bathrooms, harare.FloorArea = guests(2), guests(1), guests(70)
	harare.MaxGuests, harare.MinNights = guests(4), guests(1)
	harare.Summary = "Sample listing: a guest house let by the night in Avondale."
	harare.Description = "A sample short stay. A two-bedroom guest house in a walled garden with its own entrance, solar backup and water included. Sleeps four."
	harare.Features, harare.Image.Alt = []string{"Private entrance", "Solar backup", "Water included", "Walled and gated"}, "Cottage in a garden"

	// A country without reference places: the city and the region are free
	// text, and the price is in that country's currency. Owned by an existing
	// host (a business may list a property abroad).
	mauritius := backendListing(24, MarketMauritius, "RP-MU-10001", "beach-villa-grand-baie", "listing-house-borrowdale.jpg", 1200, 900, "2026-10-10T08:00:00Z")
	mauritius.Title, mauritius.Deal, mauritius.Category, mauritius.PropertyType = "Beach villa with a pool for up to six guests", "rent", listings.CategoryShortStay, "Holiday home"
	mauritius.Price.Amount, mauritius.Price.Currency, mauritius.PricePeriod = "8500.00", "MUR", listings.PricePeriodNight
	mauritius.CityName, mauritius.RegionName = "Grand Baie", "Rivière du Rempart"
	mauritius.Bedrooms, mauritius.Bathrooms, mauritius.FloorArea = guests(3), guests(2), guests(180)
	mauritius.MaxGuests, mauritius.MinNights = guests(6), guests(3)
	mauritius.Summary = "Sample listing: a holiday villa let by the night in Grand Baie, Mauritius."
	mauritius.Description = "A sample short stay in a country that has no list of cities yet, so its city and region are written as free text. A three-bedroom villa with a private pool, a short walk from the beach. Sleeps six. Minimum stay three nights."
	mauritius.Features, mauritius.Image.Alt = []string{"Swimming pool", "Veranda", "Walled and gated"}, "House with a garden and a pool"

	// A showroom to hire by the hour (API 0.33.0), owned by the showrooms
	// host; its hourly settings and sample bookings are in venues.go.
	showroom := backendListing(ShowroomListingNumber, MarketCode, "RP-ZW-10021", ShowroomListingSlug, "listing-office-eastlea.jpg", 1200, 801, "2026-10-08T08:00:00Z")
	showroom.Title, showroom.Deal, showroom.Category, showroom.PropertyType = "Glass-fronted showroom to hire by the hour", "rent", "commercial", "Showroom"
	showroom.Price.Amount, showroom.Price.Currency, showroom.PricePeriod = "2800.00", "USD", listings.PricePeriodMonth
	showroom.Suburb, showroom.City = "Eastlea", "Harare"
	showroom.FloorArea = guests(240)
	showroom.Summary = "Sample listing: a ground-floor showroom for launches, pop-up shops and events, by the hour or by the month."
	showroom.Description = "A sample showroom. A glass-fronted ground-floor space with display lighting, a small kitchen and parking for customers. Book it by the hour for a launch or an exhibition, or rent it by the month."
	showroom.Features, showroom.Image.Alt = []string{"Fibre-ready", "Backup generator", "8 parking bays"}, "Bright room with a long table and chairs"

	return []SampleListing{london, manchester, shoreditch, falls, harare, mauritius, showroom}
}

// The sample showroom (hourly rental, API 0.33.0).
const (
	ShowroomListingNumber = 25
	ShowroomListingSlug   = "glass-fronted-showroom-eastlea"
)

// AllListings returns the web's sample listings followed by the backend's.
func AllListings() []SampleListing {
	return append(append([]SampleListing{}, Sample().Listings...), BackendListings()...)
}

// ShortStayDescription is the description of the short-stay category. The
// brand name appears only here, as a description of what people look for, so
// that a search for it finds the short stays; no code, type or label is
// named after another company's trade mark.
const ShortStayDescription = "Holiday homes, serviced apartments, guest houses and rooms let by the night, including Airbnb-style stays."

// UK reference data: the regions, a starter set of cities (each with its
// region) and a few areas. Codes are slugs of the labels; none collides with
// an existing code of the same set.
var (
	ukRegions = []string{"England", "Scotland", "Wales", "Northern Ireland"}
	ukCities  = []struct{ name, region string }{
		{"London", "england"}, {"Manchester", "england"}, {"Birmingham", "england"}, {"Leeds", "england"},
		{"Liverpool", "england"}, {"Bristol", "england"}, {"Edinburgh", "scotland"}, {"Glasgow", "scotland"},
		{"Cardiff", "wales"}, {"Belfast", "northern-ireland"},
	}
	ukSuburbs = []struct{ name, city string }{
		{"Camden", "london"}, {"Greenwich", "london"}, {"Kensington", "london"}, {"Shoreditch", "london"}, {"Wimbledon", "london"},
		// One area outside London, because a sample listing is there.
		{"Northern Quarter", "manchester"},
	}
	shortStayTypes = []string{"Holiday home", "Serviced apartment", "Guest house", "Private room", "Lodge"}
)

// HourlyPropertyTypes are the property types added for hourly rental
// (code, label), as migration 0032 adds them.
var HourlyPropertyTypes = [][2]string{
	{"showroom", "Showroom"}, {"event-space", "Venue / event space"}, {"hall", "Hall"},
	{"conference-room", "Conference room"}, {"studio", "Studio"},
}

// addBackendReference appends the rows this file needs to the baseline:
// the short-stay category and its property types, the price periods and the
// United Kingdom's regions, cities and areas. sortAfter continues each set's
// numbering after the existing rows.
func addBackendReference(sets map[string][]reference.Item) {
	gb := str(MarketUK)
	appendRows := func(set string, rows ...reference.Item) {
		for _, row := range rows {
			row.SortOrder, row.Active = (len(sets[set])+1)*10, true
			sets[set] = append(sets[set], row)
		}
	}
	appendRows(reference.SetPropertyCategories, reference.Item{Code: listings.CategoryShortStay, Label: "Short stays", Description: str(ShortStayDescription)})
	for _, name := range shortStayTypes {
		appendRows(reference.SetPropertyTypes, reference.Item{Code: Code(name), Label: name, ParentCode: str(listings.CategoryShortStay)})
	}
	// Hourly rental (migration 0032): the commercial types that may be
	// booked by the hour; office becomes one of them. Every other property
	// type says hourly_bookable false.
	for _, t := range HourlyPropertyTypes {
		appendRows(reference.SetPropertyTypes, reference.Item{Code: t[0], Label: t[1], ParentCode: str("commercial")})
	}
	for i, it := range sets[reference.SetPropertyTypes] {
		hourly := it.Code == "office"
		for _, t := range HourlyPropertyTypes {
			hourly = hourly || t[0] == it.Code
		}
		sets[reference.SetPropertyTypes][i].HourlyBookable = flag(hourly)
	}
	appendRows(reference.SetPricePeriods,
		reference.Item{Code: listings.PricePeriodMonth, Label: "per month"},
		reference.Item{Code: listings.PricePeriodNight, Label: "per night"})
	// A request to stay at a short stay (the availability calendar).
	appendRows(reference.SetRequestTypes, reference.Item{Code: propertyrequests.TypeStayRequest, Label: "Request to stay"})
	for _, name := range ukRegions {
		appendRows(reference.SetRegions, reference.Item{Code: Code(name), Label: name, MarketCode: gb})
	}
	for _, c := range ukCities {
		appendRows(reference.SetCities, reference.Item{Code: Code(c.name), Label: c.name, MarketCode: gb, ParentCode: str(c.region)})
	}
	for _, s := range ukSuburbs {
		appendRows(reference.SetSuburbs, reference.Item{Code: Code(s.name), Label: s.name, MarketCode: gb, ParentCode: str(s.city)})
	}
}

// backendPeople are the accounts added here: the owner of the UK agency.
func backendPeople() []Account {
	return []Account{{
		ID: SampleHomesLondonOwnerID, Name: "Sample Homes London Owner", Email: SampleHomesLondonOwnerEmail,
		Organisation: SampleHomesLondon, MembershipRole: auth.RoleOwner,
		AccountRoles: []string{identity.AccountRoleAgencyOwner},
		Listings:     []int{19, 20, 21}, Profiles: []string{"agencies/sample-homes-london"},
	}}
}

// CheckListings validates every sample listing against the listing rules
// (a short stay is deal rent priced per night, and so on).
func CheckListings() error {
	for _, r := range ListingRecords(sampleEpoch) {
		if err := listings.CheckRecord(r); err != nil {
			return fmt.Errorf("sample data: %w", err)
		}
	}
	return nil
}
