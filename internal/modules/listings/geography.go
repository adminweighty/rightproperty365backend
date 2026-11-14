package listings

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// SetRegions is the reference set of regions (provinces, states) of a market.
const SetRegions = "regions"

// Country is a market shown as a country.
type Country struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// CountryInfo is what search needs to know about a market.
type CountryInfo struct {
	Name string
	// Currency is the market's currency: the default for price filters when
	// the search is narrowed to that country.
	Currency string
}

// Geography is the location hierarchy, all of it reference data:
//
//	country (a market) > region > city > suburb
//
// A listing stores its market, city and suburb; its region is the region of
// its city. Every map is keyed by the child's code.
type Geography struct {
	Countries     map[string]CountryInfo
	RegionCountry map[string]string
	CityRegion    map[string]string
	// CityCountry is the market a city belongs to ("" when it applies everywhere).
	CityCountry map[string]string
	SuburbCity  map[string]string
}

// Places supplies the current location hierarchy (the reference data and the
// markets). Nothing is cached: an administrator's change applies at once.
type Places interface {
	Geography(ctx context.Context) (Geography, error)
}

// countryOfCity returns the country of a city: its own market, else its region's.
func (g Geography) countryOfCity(city string) string {
	if c := g.CityCountry[city]; c != "" {
		return c
	}
	return g.RegionCountry[g.CityRegion[city]]
}

// Keys of the hierarchy maps inside a label bundle (see loadLabels). They are
// not reference sets, hence the prefix no set code can have.
const (
	keyCityRegion   = "~city_region"
	keySuburbCity   = "~suburb_city"
	keyCountryNames = "~country_names"
)

// labelledSets are the reference sets whose labels a listing shows.
var labelledSets = []string{SetDealTypes, SetPropertyCategories, SetPropertyTypes, SetCities, SetSuburbs, SetListingFeatures, SetRegions}

// loadLabels loads the labels of every set a listing uses, plus the location
// hierarchy, as one bundle. places may be nil (no regions, no country names).
func loadLabels(ctx context.Context, labels Labels, places Places) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, set := range labelledSets {
		l, err := labels.Labels(ctx, set)
		if err != nil {
			return nil, fmt.Errorf("load %s labels: %w", set, err)
		}
		out[set] = l
	}
	out[keyCityRegion], out[keySuburbCity], out[keyCountryNames] = map[string]string{}, map[string]string{}, map[string]string{}
	if places == nil {
		return out, nil
	}
	g, err := places.Geography(ctx)
	if err != nil {
		return nil, fmt.Errorf("load the location hierarchy: %w", err)
	}
	out[keyCityRegion], out[keySuburbCity] = g.CityRegion, g.SuburbCity
	for code, c := range g.Countries {
		out[keyCountryNames][code] = c.Name
	}
	return out, nil
}

// geography returns the hierarchy, empty when no Places is configured.
func (s *Service) geography(ctx context.Context) (Geography, error) {
	if s.places == nil {
		return Geography{}, nil
	}
	g, err := s.places.Geography(ctx)
	if err != nil {
		return Geography{}, fmt.Errorf("load the location hierarchy: %w", err)
	}
	return g, nil
}

// checkPlaces validates country, region, city and suburb against the
// hierarchy and each other. A value that does not belong to the wider one
// is reported on the NARROWER parameter. It also fills what the adapters
// need: the city -> region map and the country's currency as the default.
func (s *Service) checkPlaces(ctx context.Context, q *Query, fields map[string]string) error {
	if q.Country == "" && q.Region == "" && q.City == "" && q.Suburb == "" {
		return nil
	}
	g, err := s.geography(ctx)
	if err != nil {
		return err
	}
	q.CityRegion = g.CityRegion
	if q.Country != "" {
		info, ok := g.Countries[q.Country]
		if !ok {
			fields["country"] = "is not a known country; see GET /v1/markets"
			return nil
		}
		// Prices are compared within one currency: the country's, unless asked otherwise.
		if q.Currency == "" {
			q.Currency = info.Currency
		}
	}
	if q.Region != "" {
		active, err := s.labels.ActiveCodes(ctx, SetRegions)
		if err != nil {
			return err
		}
		switch {
		case !containsString(active, q.Region):
			fields["region"] = "is not a known value; see GET /v1/reference/" + SetRegions
		case q.Country != "" && g.RegionCountry[q.Region] != q.Country:
			fields["region"] = "is not a region of that country"
		}
	}
	if _, bad := fields["city"]; q.City != "" && !bad {
		switch {
		case q.Region != "" && g.CityRegion[q.City] != q.Region:
			fields["city"] = "is not a city of that region"
		case q.Country != "" && g.countryOfCity(q.City) != q.Country:
			fields["city"] = "is not a city of that country"
		}
	}
	if _, bad := fields["suburb"]; q.Suburb != "" && !bad {
		city := g.SuburbCity[q.Suburb]
		switch {
		case q.City != "" && city != q.City:
			fields["suburb"] = "is not a suburb of that city"
		case q.Region != "" && g.CityRegion[city] != q.Region:
			fields["suburb"] = "is not a suburb of that region"
		case q.Country != "" && g.countryOfCity(city) != q.Country:
			fields["suburb"] = "is not a suburb of that country"
		}
	}
	return nil
}

// placeSuggestions returns the regions and countries whose name contains the
// term and that have at least one public listing right now (asked of the
// search engine through its facets, so every adapter answers the same way).
// Names that start with the term come first.
func (s *Service) placeSuggestions(ctx context.Context, term string) ([]Suggestion, error) {
	g, err := s.geography(ctx)
	if err != nil {
		return nil, err
	}
	regions, err := s.labels.Labels(ctx, SetRegions)
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(term)
	type candidate struct {
		Suggestion
		prefix bool
	}
	var found []candidate
	for code, label := range regions {
		if h := strings.ToLower(label); strings.Contains(h, needle) {
			if country, ok := g.Countries[g.RegionCountry[code]]; ok {
				label += ", " + country.Name
			}
			found = append(found, candidate{Suggestion{Type: "region", Label: label, Value: code}, strings.HasPrefix(h, needle)})
		}
	}
	for code, c := range g.Countries {
		if h := strings.ToLower(c.Name); strings.Contains(h, needle) {
			found = append(found, candidate{Suggestion{Type: "country", Label: c.Name, Value: code}, strings.HasPrefix(h, needle)})
		}
	}
	if len(found) == 0 {
		return nil, nil
	}
	hits, err := s.search.Search(ctx, Query{Sort: SortNewest, Page: 1, PageSize: 1, CityRegion: g.CityRegion}, s.now().UTC())
	if err != nil {
		return nil, err
	}
	sort.SliceStable(found, func(i, j int) bool {
		a, b := found[i], found[j]
		if a.Type != b.Type {
			return a.Type == "region"
		}
		if a.prefix != b.prefix {
			return a.prefix
		}
		return a.Label < b.Label
	})
	var out []Suggestion
	for _, c := range found {
		counts := hits.Facets.Region
		if c.Type == "country" {
			counts = hits.Facets.Country
		}
		if counts[c.Value] > 0 {
			out = append(out, c.Suggestion)
		}
	}
	return out, nil
}
