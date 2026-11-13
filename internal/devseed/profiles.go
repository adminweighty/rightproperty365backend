package devseed

import (
	"fmt"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/auth"
)

// ProfileCount is the number of sample directory profiles. The kind + slug
// pairs and display names mirror the web app's src/lib/data/directory.ts.
const ProfileCount = 29

// Profile is a sample directory profile: who it is and who receives its
// enquiries. It has no contact details — profiles are reached only by enquiry.
type Profile struct {
	ID   uuid.UUID
	Kind string
	Slug string
	Name string
	// Organisation owns the profile and receives its enquiries.
	Organisation *Organisation
	// AgentUserID is set for kind "agents": the agent the profile is about.
	AgentUserID *uuid.UUID
}

// Key returns "kind/slug", the profile's public address.
func (p Profile) Key() string { return p.Kind + "/" + p.Slug }

// business is a directory profile that is its own organisation with a single
// OWNER account, <slug>@example.test.
type business struct {
	kind, slug, name string
}

// businesses lists the 17 non-agency profiles, in directory order.
var businesses = []business{
	{businessenquiries.KindDevelopers, "sample-developments", "Sample Developments"},
	{businessenquiries.KindDevelopers, "sample-urban-builders", "Sample Urban Builders"},
	{businessenquiries.KindDevelopers, "sample-heights", "Sample Heights"},

	{businessenquiries.KindPropertyManagers, "sample-property-management", "Sample Property Management"},
	{businessenquiries.KindPropertyManagers, "sample-rentals-bulawayo", "Sample Rentals Bulawayo"},

	{businessenquiries.KindHosts, "sample-falls-stays", "Sample Falls Stays"},
	{businessenquiries.KindHosts, "sample-showrooms", "Sample Showrooms"},

	{businessenquiries.KindServiceProviders, "sample-plumbers", "Sample Plumbers"},
	{businessenquiries.KindServiceProviders, "sample-rapid-plumbing", "Sample Rapid Plumbing"},
	{businessenquiries.KindServiceProviders, "sample-electricians", "Sample Electricians"},
	{businessenquiries.KindServiceProviders, "sample-sparks", "Sample Sparks"},
	{businessenquiries.KindServiceProviders, "sample-solar-installers", "Sample Solar Installers"},
	{businessenquiries.KindServiceProviders, "sample-sun-power", "Sample Sun Power"},
	{businessenquiries.KindServiceProviders, "sample-borehole-drillers", "Sample Borehole Drillers"},
	{businessenquiries.KindServiceProviders, "sample-conveyancers", "Sample Conveyancers"},
	{businessenquiries.KindServiceProviders, "sample-valuers", "Sample Valuers"},
	{businessenquiries.KindServiceProviders, "sample-movers", "Sample Movers"},
}

// businessKinds maps a directory kind to the organisation kind and the
// matching self-declared account role of its owner.
var businessKinds = map[string]struct{ organisationKind, accountRole string }{
	businessenquiries.KindDevelopers:       {auth.KindDeveloper, identity.AccountRoleDeveloper},
	businessenquiries.KindPropertyManagers: {auth.KindLandlord, identity.AccountRoleLandlord},
	businessenquiries.KindHosts:            {auth.KindHost, identity.AccountRoleHost},
	businessenquiries.KindServiceProviders: {auth.KindServiceProvider, identity.AccountRoleServiceProvider},
}

// BusinessOwnerEmail is the sign-in email of a business profile's owner.
func BusinessOwnerEmail(slug string) string { return slug + "@example.test" }

func businessOrganisations() []*Organisation {
	out := make([]*Organisation, 0, len(businesses))
	for i, b := range businesses {
		out = append(out, &Organisation{
			ID: id(fmt.Sprintf("b1%02d", i+1)), Name: b.name, Kind: businessKinds[b.kind].organisationKind,
			EnquiryEmail: "enquiries@" + b.slug + ".example.test",
		})
	}
	return out
}

func businessAccounts() []Account {
	orgs := businessOrganisations()
	out := make([]Account, 0, len(businesses))
	for i, b := range businesses {
		out = append(out, Account{
			ID: id(fmt.Sprintf("d1%02d", i+1)), Name: b.name + " Owner", Email: BusinessOwnerEmail(b.slug),
			Organisation: orgs[i], MembershipRole: auth.RoleOwner,
			AccountRoles: []string{identity.AccountRoleConsumer, businessKinds[b.kind].accountRole},
			Profiles:     []string{b.kind + "/" + b.slug},
		})
	}
	return out
}

// agencyProfiles maps the agency profiles to the seeded agencies (by name).
var agencyProfiles = []struct {
	slug string
	org  *Organisation
}{
	{"sample-estates", SampleEstates},
	{"sample-realty", SampleRealty},
	{"sample-commercial", SampleCommercial},
	{"sample-land-agents", SampleLandAgents},
	{"sample-homes-bulawayo", SampleHomesBulawayo},
}

// agentProfiles maps the agent profiles to the seeded people (by name), each
// within their agency.
var agentProfiles = []struct {
	slug, name string
	userID     uuid.UUID
	org        *Organisation
}{
	{"rudo-moyo", "Rudo Moyo", EstatesOwnerID, SampleEstates},
	{"tendai-ncube", "Tendai Ncube", identity.SeedAgentUserID, SampleEstates},
	{"chipo-dube", "Chipo Dube", EstatesAgent2ID, SampleEstates},
	{"tatenda-mutasa", "Tatenda Mutasa", RealtyAgentID, SampleRealty},
	{"farai-chikwanha", "Farai Chikwanha", CommercialAgentID, SampleCommercial},
	{"nomsa-sibanda", "Nomsa Sibanda", LandAgentID, SampleLandAgents},
	{"sipho-nkomo", "Sipho Nkomo", BulawayoAgentID, SampleHomesBulawayo},
}

// Profiles returns the 29 sample directory profiles, all ACTIVE: 5 agencies,
// 7 agents and 17 businesses.
func Profiles() []Profile {
	out := make([]Profile, 0, ProfileCount)
	next := func() uuid.UUID { return id(fmt.Sprintf("e0%02d", len(out)+1)) }

	for _, a := range agencyProfiles {
		out = append(out, Profile{ID: next(), Kind: businessenquiries.KindAgencies, Slug: a.slug, Name: a.org.Name, Organisation: a.org})
	}
	for _, a := range agentProfiles {
		user := a.userID
		out = append(out, Profile{ID: next(), Kind: businessenquiries.KindAgents, Slug: a.slug, Name: a.name, Organisation: a.org, AgentUserID: &user})
	}
	orgs := businessOrganisations()
	for i, b := range businesses {
		out = append(out, Profile{ID: next(), Kind: b.kind, Slug: b.slug, Name: b.name, Organisation: orgs[i]})
	}
	return out
}

// profilesOf lists the "kind/slug" of the profiles a seeded person or agency
// answers for, for the README table: an agent's own profile, plus the agency
// profile for the agency's owner.
func profilesOf(userID uuid.UUID, org *Organisation, membershipRole string) []string {
	var out []string
	for _, a := range agentProfiles {
		if a.userID == userID {
			out = append(out, businessenquiries.KindAgents+"/"+a.slug)
		}
	}
	if org != nil && membershipRole == auth.RoleOwner {
		for _, a := range agencyProfiles {
			if a.org == org {
				out = append(out, businessenquiries.KindAgencies+"/"+a.slug)
			}
		}
	}
	return out
}

// SeedMemoryProfiles puts the sample profiles into the in-memory store. Every
// profile is ACTIVE and has someone to receive its enquiries.
func SeedMemoryProfiles(store *businessenquiries.MemoryStore) {
	for _, p := range Profiles() {
		store.PutProfile(businessenquiries.Profile{
			ID: p.ID, Kind: p.Kind, Slug: p.Slug, Name: p.Name,
			MarketID: markets.ZimbabweID, Status: businessenquiries.ProfileActive,
			OrganisationID: p.Organisation.ID, OrganisationEligible: true,
			AgentUserID: p.AgentUserID, AgentEligible: p.AgentUserID != nil,
		})
	}
}
