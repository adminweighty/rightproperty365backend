package devseed

import "rightproperty365.com/backend/internal/modules/listings"

// SampleCoordinate is the map position of a sample listing (API 0.29.0).
type SampleCoordinate struct {
	Latitude, Longitude float64
	Precision           string
}

// Slugs of the sample listings with a special location: the one whose
// public position is APPROXIMATE (a plot outside Mutare: the public sees it
// rounded to two decimals) and the one without coordinates (not on the map).
const (
	ApproximateListingSlug = "one-acre-plot-murambi-mutare"
	NoLocationListingSlug  = "residential-stand-ruwa"
)

// sampleCoordinates are plausible positions near each sample's suburb or
// city. They are sample data, not surveyed addresses.
var sampleCoordinates = map[string]SampleCoordinate{
	// Harare.
	"four-bedroom-family-home-borrowdale":    {-17.745210, 31.091840, listings.PrecisionExact},
	"two-bedroom-garden-apartment-avondale":  {-17.801320, 31.038950, listings.PrecisionExact},
	"office-suite-eastlea":                   {-17.825470, 31.074620, listings.PrecisionExact},
	"three-bedroom-townhouse-mount-pleasant": {-17.771180, 31.046230, listings.PrecisionExact},
	"four-bedroom-house-highlands":           {-17.796040, 31.088310, listings.PrecisionExact},
	"five-bedroom-house-glen-lorne":          {-17.755620, 31.128940, listings.PrecisionExact},
	"one-bedroom-apartment-avenues":          {-17.818350, 31.050120, listings.PrecisionExact},
	"three-bedroom-apartment-belgravia":      {-17.804110, 31.044870, listings.PrecisionExact},
	"office-floor-harare-cbd":                {-17.829160, 31.051340, listings.PrecisionExact},
	"three-bedroom-townhouse-greendale":      {-17.817240, 31.121060, listings.PrecisionExact},
	"garden-cottage-greystone-park":          {-17.735480, 31.104720, listings.PrecisionExact},
	"garden-guest-house-avondale":            {-17.799060, 31.041280, listings.PrecisionExact},
	// Bulawayo.
	"residential-stand-hillside-bulawayo":   {-20.181430, 28.605270, listings.PrecisionExact},
	"three-bedroom-house-burnside-bulawayo": {-20.204180, 28.602350, listings.PrecisionExact},
	"four-bedroom-house-suburbs-bulawayo":   {-20.150260, 28.596140, listings.PrecisionExact},
	// Victoria Falls.
	"five-bedroom-house-victoria-falls": {-17.929320, 25.842170, listings.PrecisionExact},
	"safari-lodge-victoria-falls":       {-17.933410, 25.836280, listings.PrecisionExact},
	// Mutare: the plot is APPROXIMATE (public: -18.95, 32.66).
	ApproximateListingSlug:                   {-18.951234, 32.657891, listings.PrecisionApproximate},
	"three-bedroom-house-morningside-mutare": {-18.985130, 32.665420, listings.PrecisionExact},
	// United Kingdom.
	"three-bedroom-terraced-house-kensington": {51.499120, -0.193050, listings.PrecisionExact},
	"two-bedroom-apartment-northern-quarter":  {53.484150, -2.235980, listings.PrecisionExact},
	"serviced-apartment-shoreditch":           {51.526040, -0.078910, listings.PrecisionExact},
	// Mauritius.
	"beach-villa-grand-baie": {-20.013260, 57.580120, listings.PrecisionExact},
	// NoLocationListingSlug has none.
}

// SampleCoordinates returns the position of a sample listing, if it has one.
func SampleCoordinates(slug string) (SampleCoordinate, bool) {
	c, ok := sampleCoordinates[slug]
	return c, ok
}
