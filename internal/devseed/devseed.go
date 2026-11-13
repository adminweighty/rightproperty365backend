// Package devseed is the single source of the DEVELOPMENT sample data: the
// organisations, the people and which of the 18 sample listings each of them
// is responsible for. The same data seeds the in-memory stores (cmd/api in
// development) and, through cmd/seed, a development Postgres database.
//
// The names, emails and listing assignments match the web app's sample data
// and must not be changed on one side only.
//
// Every account shares the password in DEV_SEED_PASSWORD. Nothing here may run
// outside APP_ENV=development (see CheckEnvironment); the password is never
// logged.
package devseed

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/config"
)

const (
	// MarketCode is the market every sample organisation and listing is in.
	MarketCode = "ZW"
	// ListingCount is the number of PUBLISHED sample listings (IDs 1..18).
	ListingCount = 18

	privacyVersion = "dev-seed"
)

// Organisation is a sample agency.
type Organisation struct {
	ID           uuid.UUID
	Name         string
	Kind         string
	EnquiryEmail string
}

// Account is a sample person.
type Account struct {
	ID    uuid.UUID
	Name  string
	Email string
	// Organisation and MembershipRole are set for organisation members.
	Organisation   *Organisation
	MembershipRole string
	// PlatformRole is set for the administrator only.
	PlatformRole string
	// AccountRoles are the self-declared roles; seeded ones are all ACTIVE.
	AccountRoles []string
	// Listings are the sample listing numbers (1..18) this person is the
	// assigned agent of — or, for the direct seller, the seller of.
	Listings []int
	// Profiles are the directory profiles ("kind/slug") this person answers
	// for: their own agent profile, their agency's profile if they own it, or
	// their business's profile.
	Profiles []string
	// DirectSeller marks the person who sells without an organisation.
	DirectSeller bool
}

func id(suffix string) uuid.UUID {
	return uuid.MustParse("0195c0de-0000-7000-8000-00000000" + suffix)
}

// Sample organisations (all AGENCY, market ZW).
var (
	SampleEstates       = &Organisation{ID: propertyrequests.SeedOrganisationID, Name: "Sample Estates", Kind: auth.KindAgency, EnquiryEmail: "enquiries@sample-estates.example.test"}
	SecondStreetRealty  = &Organisation{ID: identity.SeedSecondOrganisationID, Name: identity.SeedSecondOrganisationName, Kind: auth.KindAgency, EnquiryEmail: "enquiries@second-street.example.test"}
	SampleCommercial    = &Organisation{ID: id("b003"), Name: "Sample Commercial", Kind: auth.KindAgency, EnquiryEmail: "enquiries@sample-commercial.example.test"}
	SampleLandAgents    = &Organisation{ID: id("b004"), Name: "Sample Land Agents", Kind: auth.KindAgency, EnquiryEmail: "enquiries@sample-land.example.test"}
	SampleRealty        = &Organisation{ID: id("b005"), Name: "Sample Realty", Kind: auth.KindAgency, EnquiryEmail: "enquiries@sample-realty.example.test"}
	SampleHomesBulawayo = &Organisation{ID: id("b006"), Name: "Sample Homes Bulawayo", Kind: auth.KindAgency, EnquiryEmail: "enquiries@sample-homes-bulawayo.example.test"}
)

// Organisations returns every sample organisation: the six agencies and one
// organisation per business profile (developers, property managers, hosts and
// service providers).
func Organisations() []*Organisation {
	agencies := []*Organisation{SampleEstates, SecondStreetRealty, SampleCommercial, SampleLandAgents, SampleRealty, SampleHomesBulawayo}
	return append(agencies, businessOrganisations()...)
}

// Emails of the sample accounts added with the 18-listing data set. The
// original four are identity.SeedConsumerEmail, SeedAgentEmail, SeedAdminEmail
// and SeedAgentBEmail.
const (
	EstatesOwnerEmail    = "estates.owner@example.test"
	EstatesAgent2Email   = "estates.agent2@example.test"
	CommercialAgentEmail = "commercial.agent@example.test"
	LandAgentEmail       = "land.agent@example.test"
	RealtyAgentEmail     = "realty.agent@example.test"
	BulawayoAgentEmail   = "bulawayo.agent@example.test"
	SellerEmail          = "seller@example.test"
)

// Fixed user IDs of the accounts above.
var (
	EstatesOwnerID    = id("d005")
	EstatesAgent2ID   = id("d006")
	CommercialAgentID = id("d007")
	LandAgentID       = id("d008")
	RealtyAgentID     = id("d009")
	BulawayoAgentID   = id("d00a")
	// SellerID is the direct seller of listing 6.
	SellerID = propertyrequests.SeedSellerUserID
)

var (
	consumerRoles = []string{identity.AccountRoleConsumer}
	agentRoles    = []string{identity.AccountRoleConsumer, identity.AccountRoleAgent}
	ownerRoles    = []string{identity.AccountRoleConsumer, identity.AccountRoleAgencyOwner}
	sellerRoles   = []string{identity.AccountRoleConsumer, identity.AccountRolePrivateSeller}
)

// Accounts returns every sample account. All are email-verified.
func Accounts() []Account {
	accounts := append(peopleAccounts(), businessAccounts()...)
	for i := range accounts {
		if accounts[i].Profiles == nil {
			accounts[i].Profiles = profilesOf(accounts[i].ID, accounts[i].Organisation, accounts[i].MembershipRole)
		}
	}
	return accounts
}

// peopleAccounts are the consumer, the administrator, the agency people and
// the direct seller.
func peopleAccounts() []Account {
	return []Account{
		{ID: identity.SeedConsumerUserID, Name: "Casey Consumer", Email: identity.SeedConsumerEmail, AccountRoles: consumerRoles},
		{ID: identity.SeedAdminUserID, Name: "Sam Admin", Email: identity.SeedAdminEmail, PlatformRole: auth.PlatformSuperAdmin, AccountRoles: consumerRoles},

		{ID: EstatesOwnerID, Name: "Rudo Moyo", Email: EstatesOwnerEmail, Organisation: SampleEstates, MembershipRole: auth.RoleOwner, AccountRoles: ownerRoles},
		{ID: identity.SeedAgentUserID, Name: "Tendai Ncube", Email: identity.SeedAgentEmail, Organisation: SampleEstates, MembershipRole: auth.RoleAgent, AccountRoles: agentRoles, Listings: []int{1, 2, 5}},
		{ID: EstatesAgent2ID, Name: "Chipo Dube", Email: EstatesAgent2Email, Organisation: SampleEstates, MembershipRole: auth.RoleAgent, AccountRoles: agentRoles, Listings: []int{7, 10, 17}},
		{ID: CommercialAgentID, Name: "Farai Chikwanha", Email: CommercialAgentEmail, Organisation: SampleCommercial, MembershipRole: auth.RoleOwner, AccountRoles: ownerRoles, Listings: []int{3, 13}},
		{ID: LandAgentID, Name: "Nomsa Sibanda", Email: LandAgentEmail, Organisation: SampleLandAgents, MembershipRole: auth.RoleOwner, AccountRoles: ownerRoles, Listings: []int{4, 14, 15}},
		{ID: RealtyAgentID, Name: "Tatenda Mutasa", Email: RealtyAgentEmail, Organisation: SampleRealty, MembershipRole: auth.RoleOwner, AccountRoles: ownerRoles, Listings: []int{8, 9, 16, 18}},
		{ID: BulawayoAgentID, Name: "Sipho Nkomo", Email: BulawayoAgentEmail, Organisation: SampleHomesBulawayo, MembershipRole: auth.RoleOwner, AccountRoles: ownerRoles, Listings: []int{11, 12}},
		{ID: SellerID, Name: "Grace Mhlanga", Email: SellerEmail, AccountRoles: sellerRoles, Listings: []int{6}, DirectSeller: true},

		// Kept from the first seed: a member of an organisation that owns no
		// listings, for checking that organisations cannot see each other's data.
		{ID: identity.SeedAgentBUserID, Name: "Blake Agent", Email: identity.SeedAgentBEmail, Organisation: SecondStreetRealty, MembershipRole: auth.RoleAgent, AccountRoles: []string{identity.AccountRoleAgent}},
	}
}

// ExpiredListingID is the one sample listing that no longer accepts requests.
var ExpiredListingID = propertyrequests.SeedExpiredListing

var expiredAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Listings returns the 18 PUBLISHED sample listings (each with its
// organisation and assigned agent, or its direct seller) plus the EXPIRED one.
func Listings() []propertyrequests.Listing {
	out := make([]propertyrequests.Listing, 0, ListingCount+1)
	for _, a := range Accounts() {
		for _, n := range a.Listings {
			l := propertyrequests.Listing{
				ID: propertyrequests.SeedListingID(n), MarketID: markets.ZimbabweID, Status: propertyrequests.ListingStatusPublished,
			}
			person := a.ID
			if a.DirectSeller {
				l.SellerUserID, l.SellerEligible = &person, true
			} else {
				org := a.Organisation.ID
				l.OrganisationID, l.OrganisationEligible = &org, true
				l.AgentUserID, l.AgentEligible = &person, true
			}
			out = append(out, l)
		}
	}
	org, expired := SampleEstates.ID, expiredAt
	out = append(out, propertyrequests.Listing{
		ID: ExpiredListingID, MarketID: markets.ZimbabweID, Status: "EXPIRED", ExpiresAt: &expired,
		OrganisationID: &org, OrganisationEligible: true,
	})
	return out
}

// CheckEnvironment refuses to seed anywhere but development and without the
// seed password. It is the guard of cmd/seed.
func CheckEnvironment(env, password string) error {
	if env != config.EnvDevelopment {
		return fmt.Errorf("refusing to seed: APP_ENV is %q, and sample data with a shared, published password may only be created when APP_ENV=development", env)
	}
	if password == "" {
		return errors.New("refusing to seed: DEV_SEED_PASSWORD is not set (see .env.example)")
	}
	if err := identity.CheckPassword(password); err != nil {
		return fmt.Errorf("DEV_SEED_PASSWORD %w", err)
	}
	return nil
}

// SeedMemoryListings puts the sample listings into the in-memory store,
// replacing its built-in fixture listings with the same IDs.
func SeedMemoryListings(store *propertyrequests.MemoryStore) {
	for _, l := range Listings() {
		store.PutListing(l)
	}
}

// SeedMemoryUsers creates the sample organisations and VERIFIED accounts in
// the in-memory identity store, all sharing password.
func SeedMemoryUsers(ctx context.Context, store *identity.MemoryStore, hasher identity.Hasher, password string, now time.Time) error {
	if err := identity.CheckPassword(password); err != nil {
		return fmt.Errorf("DEV_SEED_PASSWORD %w", err)
	}
	// One hash for every account keeps startup fast.
	hash, err := hasher.Hash(password)
	if err != nil {
		return err
	}
	now = now.UTC().Truncate(time.Microsecond)

	for _, o := range Organisations() {
		store.PutOrganisation(identity.MemoryOrganisation{ID: o.ID, Name: o.Name, Kind: o.Kind, MarketCode: MarketCode})
	}
	for _, a := range Accounts() {
		verified := now
		if err := store.CreateUser(ctx, identity.NewUser{
			ID: a.ID, Name: a.Name, Email: a.Email, PasswordHash: hash,
			PrivacyVersion: privacyVersion, AccountRoles: activeRoles(a.AccountRoles),
			EmailVerifiedAt: &verified, CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("seed %s: %w", a.Email, err)
		}
		if a.Organisation != nil {
			if err := store.AddMembership(a.ID, a.Organisation.ID, MarketCode, a.MembershipRole); err != nil {
				return err
			}
		}
		if a.PlatformRole != "" {
			if err := store.AddPlatformRole(a.ID, a.PlatformRole, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func activeRoles(roles []string) []identity.AccountRole {
	out := make([]identity.AccountRole, 0, len(roles))
	for _, r := range roles {
		out = append(out, identity.AccountRole{Role: r, Status: identity.AccountRoleActive})
	}
	return out
}

// Emails returns the email of every sample account, for a startup log line.
func Emails() []string {
	accounts := Accounts()
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.Email)
	}
	return out
}
