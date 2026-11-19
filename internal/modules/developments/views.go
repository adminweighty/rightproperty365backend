package developments

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/reference"
)

// What the public and the developer see. Nothing here may carry a contact
// detail: the developer card is a listings.ProfileRef (kind, slug, name,
// verified, logo) and a location is a country, region, city, suburb and
// coordinates.

// Location is where a project is.
type Location struct {
	Country *listings.Country `json:"country"`
	Region  *listings.Place   `json:"region"`
	City    listings.Place    `json:"city"`
	Suburb  *listings.Place   `json:"suburb"`
	// Coordinates are the public ones (rounded when APPROXIMATE) in public
	// answers, the stored ones in the developer's view.
	Coordinates *listings.Location `json:"coordinates"`
}

// Price is a unit type's price: from / to with the currency, or on request.
type Price struct {
	OnRequest bool    `json:"on_request"`
	From      *string `json:"from"`
	To        *string `json:"to"`
	Currency  *string `json:"currency"`
}

// SizeRange is a unit type's size in square metres.
type SizeRange struct {
	Min *int `json:"min"`
	Max *int `json:"max"`
}

// UnitTypeView is a unit type as the public sees it.
type UnitTypeView struct {
	ID             uuid.UUID  `json:"id"`
	Name           string     `json:"name"`
	Bedrooms       *int       `json:"bedrooms"`
	Bathrooms      *int       `json:"bathrooms"`
	Size           *SizeRange `json:"size_m2"`
	Price          Price      `json:"price"`
	UnitsTotal     *int       `json:"units_total"`
	UnitsAvailable *int       `json:"units_available"`
	// AvailableListingCount is the number of its linked listings that are
	// public right now.
	AvailableListingCount int `json:"available_listing_count"`
}

// PhaseView is a phase as the public sees it.
type PhaseView struct {
	ID         uuid.UUID      `json:"id"`
	Name       string         `json:"name"`
	Status     listings.Coded `json:"status"`
	Completion *Completion    `json:"completion"`
	UnitTypes  []UnitTypeView `json:"unit_types"`
}

// Highlights are the developer's investment statements, always framed as such.
type Highlights struct {
	StatedBy   string      `json:"stated_by"`
	Disclaimer string      `json:"disclaimer"`
	Items      []Highlight `json:"items"`
}

func highlightsOf(items []Highlight) Highlights {
	return Highlights{StatedBy: StatedByDeveloper, Disclaimer: HighlightsDisclaimer, Items: append([]Highlight{}, items...)}
}

// Card is a public project in a list (and on its developer's profile).
type Card struct {
	ID             uuid.UUID           `json:"id"`
	Market         listings.Country    `json:"market"`
	Slug           string              `json:"slug"`
	Name           string              `json:"name"`
	Summary        string              `json:"summary"`
	Status         listings.Coded      `json:"status"`
	Completion     *Completion         `json:"completion"`
	Location       Location            `json:"location"`
	CoverPhoto     *listings.Photo     `json:"cover_photo"`
	PriceFrom      *listings.Money     `json:"price_from"`
	PriceOnRequest bool                `json:"price_on_request"`
	UnitTypeCount  int                 `json:"unit_type_count"`
	Developer      listings.ProfileRef `json:"developer"`
	PublishedAt    *time.Time          `json:"published_at"`
	UpdatedAt      time.Time           `json:"updated_at"`
}

// AvailableUnit is a public listing of the project.
type AvailableUnit struct {
	UnitTypeID *uuid.UUID       `json:"unit_type_id"`
	Listing    listings.Listing `json:"listing"`
}

// Detail is a public project page.
type Detail struct {
	Card
	Description          string           `json:"description"`
	Amenities            []listings.Coded `json:"amenities"`
	InvestmentHighlights Highlights       `json:"investment_highlights"`
	Photos               []listings.Photo `json:"photos"`
	Video                *listings.Video  `json:"video"`
	Phases               []PhaseView      `json:"phases"`
	AvailableUnits       []AvailableUnit  `json:"available_units"`
}

// Page is one page of the public list.
type Page struct {
	Items      []Card `json:"items"`
	Page       int    `json:"page"`
	PageSize   int    `json:"page_size"`
	Total      int    `json:"total"`
	TotalPages int    `json:"total_pages"`
}

// ManagedPhoto is a photo as the developer sees it.
type ManagedPhoto struct {
	ID       uuid.UUID `json:"id"`
	Approved bool      `json:"approved"`
	Position int       `json:"position"`
	listings.Photo
}

// ManagedUnitType is a unit type as the developer sees it.
type ManagedUnitType struct {
	UnitTypeView
	PhaseID  uuid.UUID `json:"phase_id"`
	Position int       `json:"position"`
}

// ManagedPhase is a phase as the developer sees it.
type ManagedPhase struct {
	ID         uuid.UUID         `json:"id"`
	Name       string            `json:"name"`
	Status     listings.Coded    `json:"status"`
	Completion *Completion       `json:"completion"`
	Position   int               `json:"position"`
	UnitTypes  []ManagedUnitType `json:"unit_types"`
}

// LinkedListing is a linked listing as the developer sees it.
type LinkedListing struct {
	ListingID  uuid.UUID  `json:"listing_id"`
	UnitTypeID *uuid.UUID `json:"unit_type_id"`
	Title      string     `json:"title"`
	Slug       string     `json:"slug"`
	Reference  string     `json:"reference"`
	Status     string     `json:"status"`
	Public     bool       `json:"public"`
}

// Managed is a project as its developer and its reviewers see it.
type Managed struct {
	ID                   uuid.UUID           `json:"id"`
	OrganisationID       uuid.UUID           `json:"organisation_id"`
	Developer            listings.ProfileRef `json:"developer"`
	Market               listings.Country    `json:"market"`
	Slug                 string              `json:"slug"`
	Name                 string              `json:"name"`
	Summary              string              `json:"summary"`
	Description          string              `json:"description"`
	Status               listings.Coded      `json:"status"`
	Completion           *Completion         `json:"completion"`
	ModerationStatus     string              `json:"moderation_status"`
	ReviewReason         *string             `json:"review_reason"`
	PublicURLPath        *string             `json:"public_url_path"`
	Location             Location            `json:"location"`
	CityName             *string             `json:"city_name"`
	Amenities            []listings.Coded    `json:"amenities"`
	InvestmentHighlights Highlights          `json:"investment_highlights"`
	YouTubeURL           *string             `json:"youtube_url"`
	Video                *listings.Video     `json:"video"`
	Photos               []ManagedPhoto      `json:"photos"`
	Phases               []ManagedPhase      `json:"phases"`
	LinkedListings       []LinkedListing     `json:"linked_listings"`
	Actions              []string            `json:"actions"`
	SubmittedAt          *time.Time          `json:"submitted_at"`
	ReviewedAt           *time.Time          `json:"reviewed_at"`
	PublishedAt          *time.Time          `json:"published_at"`
	CreatedAt            time.Time           `json:"created_at"`
	UpdatedAt            time.Time           `json:"updated_at"`
}

// QueuePage is one page of the review queue.
type QueuePage struct {
	Items      []Managed `json:"items"`
	Page       int       `json:"page"`
	PageSize   int       `json:"page_size"`
	Total      int       `json:"total"`
	TotalPages int       `json:"total_pages"`
}

// names are the reference labels and the location hierarchy a view needs.
type names struct {
	geo       listings.Geography
	cities    map[string]string
	suburbs   map[string]string
	regions   map[string]string
	amenities map[string]string
}

func (s *Service) names(ctx context.Context) (names, error) {
	var n names
	var err error
	if s.places != nil {
		if n.geo, err = s.places.Geography(ctx); err != nil {
			return names{}, fmt.Errorf("load the location hierarchy: %w", err)
		}
	}
	for _, x := range []struct {
		set string
		dst *map[string]string
	}{{reference.SetCities, &n.cities}, {reference.SetSuburbs, &n.suburbs}, {reference.SetRegions, &n.regions},
		{reference.SetDevelopmentAmenities, &n.amenities}} {
		if *x.dst, err = s.labels.Labels(ctx, x.set); err != nil {
			return names{}, err
		}
	}
	return n, nil
}

func labelOr(labels map[string]string, code string) string {
	if l, ok := labels[code]; ok {
		return l
	}
	return code
}

func (n names) country(code string) listings.Country {
	c := listings.Country{Code: code, Name: code}
	if info, ok := n.geo.Countries[code]; ok && info.Name != "" {
		c.Name = info.Name
	}
	return c
}

// countryOf is the country of a reference city: its own, else its region's.
func (n names) countryOf(city string) string {
	if c := n.geo.CityCountry[city]; c != "" {
		return c
	}
	return n.geo.RegionCountry[n.geo.CityRegion[city]]
}

// hasCities reports whether the country has reference cities.
func (n names) hasCities(country string) bool {
	for city := range n.cities {
		if n.countryOf(city) == country {
			return true
		}
	}
	return false
}

func (n names) location(d Development, exact bool) Location {
	country := n.country(d.MarketCode)
	l := Location{Country: &country, City: listings.PlaceOf(n.cities, d.CityCode, d.CityName)}
	if region := n.geo.CityRegion[d.CityCode]; d.CityCode != "" && region != "" {
		l.Region = &listings.Place{Code: &region, Label: labelOr(n.regions, region)}
	}
	if d.SuburbCode != "" {
		code := d.SuburbCode
		l.Suburb = &listings.Place{Code: &code, Label: labelOr(n.suburbs, code)}
	}
	if exact {
		if d.Latitude != nil && d.Longitude != nil {
			l.Coordinates = &listings.Location{Latitude: *d.Latitude, Longitude: *d.Longitude, Precision: listings.NormalPrecision(d.LocationPrecision)}
		}
	} else {
		l.Coordinates = listings.PublicLocationOf(d.Latitude, d.Longitude, d.LocationPrecision)
	}
	return l
}

func (n names) amenityList(codes []string) []listings.Coded {
	out := []listings.Coded{}
	for _, c := range codes {
		out = append(out, listings.Coded{Code: c, Label: labelOr(n.amenities, c)})
	}
	return out
}

func statusCoded(status string) listings.Coded {
	return listings.Coded{Code: status, Label: StatusLabel(status)}
}

func priceOf(u UnitType) Price {
	if u.PriceOnRequest {
		return Price{OnRequest: true}
	}
	p := Price{}
	if u.PriceFrom != "" {
		v := u.PriceFrom
		p.From = &v
	}
	if u.PriceTo != "" {
		v := u.PriceTo
		p.To = &v
	}
	if u.Currency != "" {
		v := u.Currency
		p.Currency = &v
	}
	return p
}

func unitTypeView(u UnitType, available map[uuid.UUID]int) UnitTypeView {
	v := UnitTypeView{ID: u.ID, Name: u.Name, Bedrooms: copyInt(u.Bedrooms), Bathrooms: copyInt(u.Bathrooms), Price: priceOf(u),
		UnitsTotal: copyInt(u.UnitsTotal), UnitsAvailable: copyInt(u.UnitsAvailable), AvailableListingCount: available[u.ID]}
	if u.SizeMin != nil || u.SizeMax != nil {
		v.Size = &SizeRange{Min: copyInt(u.SizeMin), Max: copyInt(u.SizeMax)}
	}
	return v
}

func sortedPhases(d Development) []Phase {
	out := append([]Phase(nil), d.Phases...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	for i := range out {
		units := append([]UnitType(nil), out[i].UnitTypes...)
		sort.SliceStable(units, func(a, b int) bool {
			if units[a].Position != units[b].Position {
				return units[a].Position < units[b].Position
			}
			return units[a].CreatedAt.Before(units[b].CreatedAt)
		})
		out[i].UnitTypes = units
	}
	return out
}

func sortedPhotos(d Development) []listings.StoredPhoto {
	out := append([]listings.StoredPhoto(nil), d.Photos...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Position != out[j].Position {
			return out[i].Position < out[j].Position
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// publicPhotos are the approved photos, in order.
func publicPhotos(d Development) []listings.Photo {
	out := []listings.Photo{}
	for _, ph := range sortedPhotos(d) {
		if ph.ApprovedAt != nil {
			out = append(out, listings.PhotoAt(ph, PhotoPath))
		}
	}
	return out
}

// priceFrom is the lowest "from" price of the unit types in the currency
// of the first priced one; nil when none has a price.
func priceFrom(d Development) *listings.Money {
	var best *big.Rat
	var out *listings.Money
	for _, p := range sortedPhases(d) {
		for _, u := range p.UnitTypes {
			if u.PriceOnRequest || u.PriceFrom == "" {
				continue
			}
			if out != nil && u.Currency != out.Currency {
				continue
			}
			r, err := listings.ParseAmount(u.PriceFrom)
			if err != nil {
				continue
			}
			if best == nil || r.Cmp(best) < 0 {
				best, out = r, &listings.Money{Amount: u.PriceFrom, Currency: u.Currency}
			}
		}
	}
	return out
}

func unitTypeCount(d Development) int {
	n := 0
	for _, p := range d.Phases {
		n += len(p.UnitTypes)
	}
	return n
}

func (n names) card(d Development, developer listings.ProfileRef) Card {
	c := Card{ID: d.ID, Market: n.country(d.MarketCode), Slug: d.Slug, Name: d.Name, Summary: d.Summary, Status: statusCoded(d.Status),
		Completion: completionOf(d.Completion), Location: n.location(d, false), PriceFrom: priceFrom(d), UnitTypeCount: unitTypeCount(d),
		Developer: developer, PublishedAt: utc(d.PublishedAt), UpdatedAt: d.UpdatedAt.UTC()}
	c.PriceOnRequest = c.PriceFrom == nil && c.UnitTypeCount > 0
	if photos := publicPhotos(d); len(photos) > 0 {
		c.CoverPhoto = &photos[0]
	}
	return c
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.UTC()
	return &v
}

func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
