package devseed

import (
	_ "embed"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// sampleJSON is the web app's sample data (src/lib/data/listings.ts and
// directory.ts), converted to JSON once so both sides show the same listings
// and profiles. Photo entries name the web app's own image files and their
// pixel sizes.
//
//go:embed data/sample.json
var sampleJSON []byte

// SampleListing is one listing of the web sample data.
type SampleListing struct {
	ID    string `json:"id"`
	Slug  string `json:"slug"`
	Image struct {
		File   string `json:"file"`
		Alt    string `json:"alt"`
		Width  int    `json:"width"`
		Height int    `json:"height"`
	} `json:"image"`
	Reference    string `json:"reference"`
	ListedBy     string `json:"listedBy"`
	PublishedAt  string `json:"publishedAt"`
	Title        string `json:"title"`
	Deal         string `json:"deal"`
	Category     string `json:"category"`
	PropertyType string `json:"propertyType"`
	Price        struct {
		Amount   string `json:"amount"`
		Currency string `json:"currency"`
	} `json:"price"`
	PricePeriod string   `json:"pricePeriod"`
	Suburb      string   `json:"suburb"`
	City        string   `json:"city"`
	MarketCode  string   `json:"marketCode"`
	Bedrooms    *int     `json:"bedrooms"`
	Bathrooms   *int     `json:"bathrooms"`
	FloorArea   *int     `json:"floorArea"`
	LandArea    *int     `json:"landArea"`
	Summary     string   `json:"summary"`
	Description string   `json:"description"`
	Features    []string `json:"features"`
	Advertiser  struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		Verified bool   `json:"verified"`
	} `json:"advertiser"`
	Featured bool `json:"featured"`
}

// SampleOffering is one service a profile describes in detail.
type SampleOffering struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// SampleProfile is one directory profile of the web sample data. It has no
// contact details: the web shows none.
type SampleProfile struct {
	Slug       string           `json:"slug"`
	Kind       string           `json:"kind"`
	Name       string           `json:"name"`
	Role       string           `json:"role"`
	Tagline    string           `json:"tagline"`
	About      string           `json:"about"`
	City       string           `json:"city"`
	Areas      []string         `json:"areas"`
	Services   []string         `json:"services"`
	Category   string           `json:"category"`
	Offerings  []SampleOffering `json:"offerings"`
	Verified   bool             `json:"verified"`
	Sponsored  bool             `json:"sponsored"`
	Since      *int             `json:"since"`
	AgencySlug string           `json:"agencySlug"`
}

// SampleKind is one directory kind with its headings.
type SampleKind struct {
	Code        string `json:"code"`
	Title       string `json:"title"`
	Singular    string `json:"singular"`
	Description string `json:"description"`
}

// SampleData is everything in data/sample.json.
type SampleData struct {
	Listings []SampleListing `json:"listings"`
	Profiles []SampleProfile `json:"profiles"`
	Kinds    []SampleKind    `json:"kinds"`
}

var sample = sync.OnceValue(func() SampleData {
	var d SampleData
	if err := json.Unmarshal(sampleJSON, &d); err != nil {
		panic("devseed: data/sample.json is not valid: " + err.Error())
	}
	return d
})

// Sample returns the web sample data.
func Sample() SampleData { return sample() }

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Code turns a label into a stable code: "Mount Pleasant" -> "mount-pleasant".
func Code(label string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(label), "-"), "-")
}
