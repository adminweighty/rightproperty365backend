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
	PricePeriod string `json:"pricePeriod"`
	Suburb      string `json:"suburb"`
	City        string `json:"city"`
	MarketCode  string `json:"marketCode"`
	Bedrooms    *int   `json:"bedrooms"`
	Bathrooms   *int   `json:"bathrooms"`
	FloorArea   *int   `json:"floorArea"`
	LandArea    *int   `json:"landArea"`
	// MaxGuests and MinNights are set for short stays only (backend samples).
	MaxGuests   *int     `json:"maxGuests"`
	MinNights   *int     `json:"minNights"`
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
	// Market is set for the backend UK profiles; empty means Zimbabwe.
	Market string `json:"-"`
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
	return withSolicitors(d)
})

// solicitorKind and solicitorProfiles are sample data that is NOT in the web
// app's files (data/sample.json mirrors those): the solicitors directory was
// added on the backend first.
var solicitorKind = SampleKind{
	Code: "solicitors", Title: "Solicitors", Singular: "Solicitor",
	Description: "Property lawyers and conveyancers for transfers, leases and title work.",
}

func since(year int) *int { return &year }

var solicitorProfiles = []SampleProfile{
	{
		Slug: "sample-law-chambers", Kind: "solicitors", Name: "Sample Law Chambers", Role: "Property law firm",
		Tagline: "Conveyancing, leases and title work for buyers, sellers and landlords.",
		About: "Sample Law Chambers is a property law practice in Harare. The firm handles transfers of residential and commercial property, " +
			"drafts and reviews lease agreements, and carries out title searches before a purchase.",
		City: "Harare", Areas: []string{"Harare", "Borrowdale", "Avondale"},
		Services: []string{"Conveyancing", "Lease drafting", "Title searches"},
		Offerings: []SampleOffering{
			{Name: "Conveyancing", Description: "Transfer of ownership from agreement of sale to registration of the title deed."},
			{Name: "Lease drafting", Description: "Residential and commercial lease agreements, drafted or reviewed."},
			{Name: "Title searches", Description: "Checking the title deed, caveats and bonds before you commit to a purchase."},
		},
		Verified: true, Since: since(2009),
	},
	{
		Slug: "sample-conveyancing-attorneys", Kind: "solicitors", Name: "Sample Conveyancing Attorneys", Role: "Conveyancers",
		Tagline: "Property transfers and bond registrations in Bulawayo.",
		About: "Sample Conveyancing Attorneys is a conveyancing practice in Bulawayo that acts for buyers, sellers and lenders on " +
			"property transfers, mortgage bond registrations and deceased-estate transfers.",
		City: "Bulawayo", Areas: []string{"Bulawayo"},
		Services: []string{"Property transfers", "Bond registration", "Estate transfers"},
		Offerings: []SampleOffering{
			{Name: "Property transfers", Description: "Acting for the buyer or the seller through to registration."},
			{Name: "Bond registration", Description: "Registering and cancelling mortgage bonds."},
			{Name: "Estate transfers", Description: "Transferring property out of a deceased estate to the heirs."},
		},
		Verified: true, Since: since(2014),
	},
	{
		Slug: "sample-property-lawyers", Kind: "solicitors", Name: "Sample Property Lawyers", Role: "Property lawyers",
		Tagline: "Transfers, leases and property disputes in Mutare and Manicaland.",
		About: "Sample Property Lawyers is a practice in Mutare that advises buyers, sellers, landlords and tenants: " +
			"conveyancing and transfers, lease agreements, and help when a boundary, a lease or a sale is in dispute.",
		City: "Mutare", Areas: []string{"Mutare"},
		Services: []string{"Conveyancing and transfers", "Lease drafting", "Property disputes"},
		Offerings: []SampleOffering{
			{Name: "Conveyancing and transfers", Description: "Agreements of sale and transfer of title for homes, stands and commercial property."},
			{Name: "Lease drafting", Description: "Clear lease agreements for landlords and tenants."},
			{Name: "Property disputes", Description: "Advice and representation in boundary, lease and sale disputes."},
		},
		Verified: true, Since: since(2016),
	},
}

// withSolicitors adds the solicitors directory after the property managers.
func withSolicitors(d SampleData) SampleData {
	var kinds []SampleKind
	for _, k := range d.Kinds {
		kinds = append(kinds, k)
		if k.Code == "property-managers" {
			kinds = append(kinds, solicitorKind)
		}
	}
	d.Kinds = kinds
	d.Profiles = append(d.Profiles, solicitorProfiles...)
	// ...and the profiles of the UK sample organisations (backend_samples.go).
	d.Profiles = append(d.Profiles, backendProfiles...)
	return d
}

// Sample returns the web sample data, plus the solicitors directory.
func Sample() SampleData { return sample() }

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// Code turns a label into a stable code: "Mount Pleasant" -> "mount-pleasant".
func Code(label string) string {
	return strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(label), "-"), "-")
}
