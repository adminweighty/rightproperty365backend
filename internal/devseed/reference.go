package devseed

import (
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/modules/reference"
)

// ZimbabweProvinces are the ten provinces of Zimbabwe, the `regions` of
// market ZW. Their codes are the slugs of the names ("matabeleland-north").
var ZimbabweProvinces = []string{
	"Harare", "Bulawayo", "Manicaland", "Mashonaland Central", "Mashonaland East", "Mashonaland West",
	"Masvingo", "Matabeleland North", "Matabeleland South", "Midlands",
}

// CityRegions is the region (province) of each sample city.
var CityRegions = map[string]string{
	"harare": "harare", "bulawayo": "bulawayo", "mutare": "manicaland", "victoria-falls": "matabeleland-north",
}

func str(s string) *string { return &s }
func flag(b bool) *bool    { return &b }

// Account-role groups, as the sign-up form shows them.
// The groups of the sign-up form, in order (real estate terminology from the
// functional specification).
var (
	groupSeekers       = &reference.Group{Code: "PROPERTY_SEEKERS", Label: "Property Seekers", SortOrder: 1}
	groupProfessionals = &reference.Group{Code: "REAL_ESTATE_PROFESSIONALS", Label: "Real Estate Professionals", SortOrder: 2}
	groupBusinesses    = &reference.Group{Code: "REAL_ESTATE_BUSINESSES", Label: "Real Estate Businesses", SortOrder: 3}
	groupOwners        = &reference.Group{Code: "PROPERTY_OWNERS", Label: "Property Owners", SortOrder: 4}
	// Not in the functional specification; kept until the project owner decides.
	groupOther = &reference.Group{Code: "OTHER_PROPERTY_BUSINESSES", Label: "Other property businesses", SortOrder: 5}
)

// directoryKindLabels are the directory headings (plural, singular) in real
// estate terms. The
// kind codes (URL slugs) are unchanged.
var directoryKindLabels = map[string][2]string{
	"agencies":          {"Real Estate Agencies", "Real Estate Agency"},
	"agents":            {"Estate Agents", "Estate Agent"},
	"developers":        {"Property Developers", "Property Developer"},
	"property-managers": {"Property Managers", "Property Manager"},
	"solicitors":        {"Solicitors", "Solicitor"},
	"hosts":             {"Hosts and Venues", "Host"},
	"service-providers": {"Property Services", "Property Service Provider"},
}

// ReferenceItems returns the baseline reference data: exactly the values the
// web app used to hard-code, with the same codes and labels. Codes that
// appear in URLs (deal, category, directory kind) keep the web's URL values.
//
// Cities, suburbs, property types, service trades and listing features are
// derived from the sample listings and profiles, so every value a sample row
// uses exists as a reference row. Cities and suburbs belong to market ZW.
func ReferenceItems() map[string][]reference.Item {
	zw := str(MarketCode)
	list := func(rows ...reference.Item) []reference.Item {
		for i := range rows {
			if rows[i].SortOrder == 0 {
				rows[i].SortOrder = (i + 1) * 10
			}
			rows[i].Active = true
		}
		return rows
	}
	role := func(code, label, description string, group *reference.Group) reference.Item {
		verify := false
		for _, r := range identity.DefaultSelectableRoles() {
			if r.Code == code {
				verify = r.RequiresVerification
			}
		}
		return reference.Item{Code: code, Label: label, Description: str(description), Group: group, RequiresVerification: flag(verify)}
	}

	sets := map[string][]reference.Item{
		reference.SetDealTypes: list(
			reference.Item{Code: "sale", Label: "For sale"},
			reference.Item{Code: "rent", Label: "To rent"},
		),
		reference.SetPropertyCategories: list(
			reference.Item{Code: "residential", Label: "Residential"},
			reference.Item{Code: "commercial", Label: "Commercial"},
			reference.Item{Code: "land", Label: "Land"},
		),
		reference.SetRequestTypes: list(
			reference.Item{Code: "ENQUIRY", Label: "General enquiry"},
			reference.Item{Code: "PROPERTY_INFORMATION", Label: "More information"},
			reference.Item{Code: "VIEWING_REQUEST", Label: "Book a viewing"},
			reference.Item{Code: "CONSULTATION_REQUEST", Label: "Speak to the agent"},
		),
		reference.SetContactMethods: list(
			reference.Item{Code: "EMAIL", Label: "Email"},
			reference.Item{Code: "PHONE", Label: "Phone call"},
			reference.Item{Code: "WHATSAPP", Label: "WhatsApp"},
		),
		reference.SetAccountRoles: list(
			role(identity.AccountRoleConsumer, "Prospective Buyer", "Looking for a property to buy.", groupSeekers),
			role(identity.AccountRoleTenant, "Prospective Tenant", "Looking for a property to rent or lease.", groupSeekers),

			role(identity.AccountRoleAgent, "Estate Agent", "Markets properties, manages enquiries, arranges viewings and handles sales or lettings.", groupProfessionals),
			role(identity.AccountRoleSalesAgent, "Sales Agent", "An estate agent who specialises in properties for sale.", groupProfessionals),
			role(identity.AccountRoleLettingsAgent, "Lettings Agent", "An estate agent who specialises in rentals and leases.", groupProfessionals),
			role(identity.AccountRolePropertyManager, "Property Manager", "Manages rental properties, tenants and viewings on behalf of owners.", groupProfessionals),
			role(identity.AccountRoleSolicitor, "Solicitor", "A property lawyer or conveyancer who handles transfers, leases and other legal work for buyers, sellers, landlords and tenants.", groupProfessionals),

			role(identity.AccountRoleAgencyOwner, "Agency Principal", "Registers a real estate agency or brokerage and is responsible for its account.", groupBusinesses),
			role(identity.AccountRoleAgencyAdmin, "Agency Administrator", "Manages an agency's agents, listings and enquiries on the principal's behalf.", groupBusinesses),
			role(identity.AccountRoleAgencyStaff, "Agency Staff", "Works at an agency in a support role.", groupBusinesses),

			role(identity.AccountRoleLandlord, "Landlord", "Owns residential or commercial property offered for rent or lease.", groupOwners),
			role(identity.AccountRolePrivateSeller, "Property Seller (Vendor)", "Owns a property offered for sale. Also called a vendor.", groupOwners),

			role(identity.AccountRoleHost, "Host", "Offers short stays or venues for hire.", groupOther),
			role(identity.AccountRoleDeveloper, "Property Developer", "Builds and sells new developments.", groupOther),
			role(identity.AccountRoleServiceProvider, "Property Service Provider", "Offers a property-related trade or professional service.", groupOther),
		),
	}

	// Organisation kinds and how they are shown, so clients do not hard-code them.
	sets[reference.SetOrganisationKinds] = list(
		reference.Item{Code: "AGENCY", Label: "Real Estate Agency"},
		reference.Item{Code: "LAW_FIRM", Label: "Law Firm"},
		reference.Item{Code: "PROPERTY_MANAGER", Label: "Property Manager"},
		reference.Item{Code: "HOST", Label: "Host"},
		reference.Item{Code: "DEVELOPER", Label: "Property Developer"},
		reference.Item{Code: "SERVICE_PROVIDER", Label: "Property Service Provider"},
	)

	data := Sample()

	var kinds []reference.Item
	for _, k := range data.Kinds {
		label, singular := k.Title, k.Singular
		if l, ok := directoryKindLabels[k.Code]; ok {
			label, singular = l[0], l[1]
		}
		kinds = append(kinds, reference.Item{Code: k.Code, Label: label, SingularLabel: str(singular), Description: str(k.Description)})
	}
	sets[reference.SetDirectoryKinds] = list(kinds...)

	// Derived sets keep first-seen order, which is the order of the web data.
	seen := map[string]bool{}
	add := func(set string, item reference.Item) {
		if key := set + "/" + item.Code; !seen[key] {
			seen[key] = true
			sets[set] = append(sets[set], item)
		}
	}
	categoryOrder := map[string]int{"residential": 0, "commercial": 1, "land": 2}
	for pass := 0; pass < 3; pass++ { // property types grouped by category
		for _, l := range data.Listings {
			if categoryOrder[l.Category] == pass {
				add(reference.SetPropertyTypes, reference.Item{Code: Code(l.PropertyType), Label: l.PropertyType, ParentCode: str(l.Category)})
			}
		}
	}
	for _, l := range data.Listings {
		add(reference.SetCities, reference.Item{Code: Code(l.City), Label: l.City, MarketCode: zw})
	}
	for _, p := range data.Profiles {
		if p.Market == MarketUK {
			continue // the UK cities are listed explicitly, with their regions
		}
		add(reference.SetCities, reference.Item{Code: Code(p.City), Label: p.City, MarketCode: zw})
		if p.Category != "" {
			add(reference.SetServiceTrades, reference.Item{Code: Code(p.Category), Label: p.Category})
		}
	}
	// Regions: Zimbabwe's ten provinces. Planned markets have none yet. Each
	// city belongs to one (its parent_code).
	for _, name := range ZimbabweProvinces {
		add(reference.SetRegions, reference.Item{Code: Code(name), Label: name, MarketCode: zw})
	}
	sets[reference.SetRegions] = list(sets[reference.SetRegions]...)
	for i, city := range sets[reference.SetCities] {
		if region, ok := CityRegions[city.Code]; ok {
			sets[reference.SetCities][i].ParentCode = str(region)
		}
	}
	for _, city := range sets[reference.SetCities] {
		for _, l := range data.Listings {
			if l.City == city.Label {
				add(reference.SetSuburbs, reference.Item{Code: Code(l.Suburb), Label: l.Suburb, MarketCode: zw, ParentCode: str(city.Code)})
			}
		}
	}
	for _, l := range data.Listings {
		for _, f := range l.Features {
			add(reference.SetListingFeatures, reference.Item{Code: Code(f), Label: f})
		}
	}
	for _, t := range onboarding.DefaultDocumentTypes() {
		add(reference.SetOnboardingDocuments, reference.Item{Code: t.Code, Label: t.Label, Description: str(t.Description)})
	}
	sets[reference.SetOnboardingDocuments] = list(sets[reference.SetOnboardingDocuments]...)
	for _, set := range []string{reference.SetPropertyTypes, reference.SetCities, reference.SetSuburbs, reference.SetServiceTrades, reference.SetListingFeatures} {
		sets[set] = list(sets[set]...)
	}
	// Short stays, price periods and the United Kingdom, after the baseline.
	addBackendReference(sets)
	// The agent CRM's contact types and sources (migration 0030).
	addCRMReference(sets)
	// The inspection checklist templates (migration 0038).
	sets[reference.SetInspectionChecklistTemplates] = InspectionTemplateRows()
	// The development amenities (migration 0046).
	sets[reference.SetDevelopmentAmenities] = DevelopmentAmenityRows()
	return sets
}

// NewMemoryReferenceStore returns an in-memory reference store holding the baseline data.
func NewMemoryReferenceStore() *reference.MemoryStore {
	return reference.NewMemoryStore(ReferenceItems())
}
