package devseed

import (
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/reference"
)

func str(s string) *string { return &s }
func flag(b bool) *bool    { return &b }

// Account-role groups, as the sign-up form shows them.
var (
	groupLooking = &reference.Group{Code: "looking", Label: "Looking for property", SortOrder: 1}
	groupListing = &reference.Group{Code: "listing", Label: "Listing or managing property", SortOrder: 2}
	groupAgency  = &reference.Group{Code: "agency", Label: "Working at an agency", SortOrder: 3}
)

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
	role := func(code, label string, group *reference.Group) reference.Item {
		verify := false
		for _, r := range identity.DefaultSelectableRoles() {
			if r.Code == code {
				verify = r.RequiresVerification
			}
		}
		return reference.Item{Code: code, Label: label, Group: group, RequiresVerification: flag(verify)}
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
			role(identity.AccountRoleConsumer, "Buyer or renter", groupLooking),
			role(identity.AccountRoleTenant, "Tenant", groupLooking),
			role(identity.AccountRolePrivateSeller, "Private seller", groupListing),
			role(identity.AccountRoleLandlord, "Landlord or property manager", groupListing),
			role(identity.AccountRoleHost, "Host: short stays and venues", groupListing),
			role(identity.AccountRoleDeveloper, "Property developer", groupListing),
			role(identity.AccountRoleServiceProvider, "Service provider", groupListing),
			role(identity.AccountRoleAgent, "Estate agent", groupAgency),
			role(identity.AccountRoleAgencyStaff, "Agency staff", groupAgency),
			role(identity.AccountRoleAgencyAdmin, "Agency administrator", groupAgency),
			role(identity.AccountRoleAgencyOwner, "Agency owner", groupAgency),
		),
	}

	data := Sample()

	var kinds []reference.Item
	for _, k := range data.Kinds {
		kinds = append(kinds, reference.Item{Code: k.Code, Label: k.Title, SingularLabel: str(k.Singular), Description: str(k.Description)})
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
		add(reference.SetCities, reference.Item{Code: Code(p.City), Label: p.City, MarketCode: zw})
		if p.Category != "" {
			add(reference.SetServiceTrades, reference.Item{Code: Code(p.Category), Label: p.Category})
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
	for _, set := range []string{reference.SetPropertyTypes, reference.SetCities, reference.SetSuburbs, reference.SetServiceTrades, reference.SetListingFeatures} {
		sets[set] = list(sets[set]...)
	}
	return sets
}

// NewMemoryReferenceStore returns an in-memory reference store holding the baseline data.
func NewMemoryReferenceStore() *reference.MemoryStore {
	return reference.NewMemoryStore(ReferenceItems())
}
