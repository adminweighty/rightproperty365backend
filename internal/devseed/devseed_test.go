package devseed

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/auth"
)

// The mapping below is the contract shared with the web app's sample data.
func TestSampleDataMapping(t *testing.T) {
	type row struct {
		name, organisation, membership string
		accountRoles                   string
		listings                       []int
	}
	want := map[string]row{
		"consumer@example.test":         {"Casey Consumer", "", "", "CONSUMER", nil},
		"admin@example.test":            {"Sam Admin", "", "", "CONSUMER", nil},
		"estates.owner@example.test":    {"Rudo Moyo", "Sample Estates", "OWNER", "CONSUMER,AGENCY_OWNER", nil},
		"agent@example.test":            {"Tendai Ncube", "Sample Estates", "AGENT", "CONSUMER,AGENT", []int{1, 2, 5}},
		"estates.agent2@example.test":   {"Chipo Dube", "Sample Estates", "AGENT", "CONSUMER,AGENT", []int{7, 10, 17}},
		"commercial.agent@example.test": {"Farai Chikwanha", "Sample Commercial", "OWNER", "CONSUMER,AGENCY_OWNER", []int{3, 13}},
		"land.agent@example.test":       {"Nomsa Sibanda", "Sample Land Agents", "OWNER", "CONSUMER,AGENCY_OWNER", []int{4, 14, 15}},
		"realty.agent@example.test":     {"Tatenda Mutasa", "Sample Realty", "OWNER", "CONSUMER,AGENCY_OWNER", []int{8, 9, 16, 18}},
		"bulawayo.agent@example.test":   {"Sipho Nkomo", "Sample Homes Bulawayo", "OWNER", "CONSUMER,AGENCY_OWNER", []int{11, 12}},
		"seller@example.test":           {"Grace Mhlanga", "", "", "CONSUMER,PRIVATE_SELLER", []int{6}},
		"agent-b@example.test":          {"Blake Agent", "Second Street Realty", "AGENT", "AGENT", nil},
	}

	accounts := Accounts()
	if len(accounts) != len(want) {
		t.Fatalf("%d accounts, want %d", len(accounts), len(want))
	}
	ids := map[string]bool{}
	for _, a := range accounts {
		w, ok := want[a.Email]
		if !ok {
			t.Fatalf("unexpected account %s", a.Email)
		}
		org := ""
		if a.Organisation != nil {
			org = a.Organisation.Name
			if a.Organisation.Kind != auth.KindAgency {
				t.Fatalf("%s: organisation kind %s, want AGENCY", a.Email, a.Organisation.Kind)
			}
		}
		if a.Name != w.name || org != w.organisation || a.MembershipRole != w.membership ||
			strings.Join(a.AccountRoles, ",") != w.accountRoles || !equal(a.Listings, w.listings) {
			t.Fatalf("%s = %+v, want %+v", a.Email, a, w)
		}
		if ids[a.ID.String()] {
			t.Fatalf("duplicate user id %s", a.ID)
		}
		ids[a.ID.String()] = true
		if (a.PlatformRole != "") != (a.Email == "admin@example.test") {
			t.Fatalf("%s: unexpected platform role %q", a.Email, a.PlatformRole)
		}
		if a.DirectSeller != (a.Email == "seller@example.test") {
			t.Fatalf("%s: unexpected direct-seller flag", a.Email)
		}
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestListings(t *testing.T) {
	listings := Listings()
	if len(listings) != ListingCount+1 {
		t.Fatalf("%d listings, want %d published + 1 expired", len(listings), ListingCount)
	}
	var published []string
	for _, l := range listings {
		if l.ID == ExpiredListingID {
			if l.Status != "EXPIRED" || l.ExpiresAt == nil {
				t.Fatalf("expired listing: %+v", l)
			}
			continue
		}
		published = append(published, l.ID.String())
		if l.Status != propertyrequests.ListingStatusPublished || l.ExpiresAt != nil {
			t.Fatalf("listing %s is not PUBLISHED", l.ID)
		}
		// Each listing has an organisation AND an assigned agent, or the direct seller.
		org := l.OrganisationID != nil && l.OrganisationEligible && l.AgentUserID != nil && l.AgentEligible && l.SellerUserID == nil
		seller := l.SellerUserID != nil && l.SellerEligible && l.OrganisationID == nil && l.AgentUserID == nil
		if org == seller {
			t.Fatalf("listing %s must have an organisation and agent, or a direct seller: %+v", l.ID, l)
		}
		if seller != (l.ID == propertyrequests.SeedListingID(6)) {
			t.Fatalf("only listing 6 is sold directly; got %s", l.ID)
		}
		if r := propertyrequests.ResolveRecipient(l); r == nil || r.UserID == nil {
			t.Fatalf("listing %s routes to nobody", l.ID)
		}
	}
	sort.Strings(published)
	for n := 1; n <= ListingCount; n++ {
		if published[n-1] != propertyrequests.SeedListingID(n).String() {
			t.Fatalf("published[%d] = %s, want listing %d", n-1, published[n-1], n)
		}
	}
	if propertyrequests.SeedListingID(18).String() != "0195c0de-0000-7000-8000-000000000018" {
		t.Fatalf("listing 18 id = %s", propertyrequests.SeedListingID(18))
	}
}

func TestSeedMemory(t *testing.T) {
	ctx := context.Background()
	store := identity.NewMemoryStore()
	hasher := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	if err := SeedMemoryUsers(ctx, store, hasher, "a-test-seed-password", now); err != nil {
		t.Fatal(err)
	}
	if store.UserCount() != len(Accounts()) {
		t.Fatalf("%d users, want %d", store.UserCount(), len(Accounts()))
	}
	for _, a := range Accounts() {
		u, err := store.UserByEmail(ctx, a.Email)
		if err != nil {
			t.Fatalf("%s: %v", a.Email, err)
		}
		if !u.Verified() || u.Name != a.Name || len(u.AccountRoles) != len(a.AccountRoles) {
			t.Fatalf("%s: %+v", a.Email, u)
		}
		for _, r := range u.AccountRoles {
			if r.Status != identity.AccountRoleActive {
				t.Fatalf("%s: account role %s is %s, want ACTIVE", a.Email, r.Role, r.Status)
			}
		}
		if (len(u.Memberships) == 1) != (a.Organisation != nil) || (len(u.PlatformRoles) == 1) != (a.PlatformRole != "") {
			t.Fatalf("%s: memberships=%v platform=%v", a.Email, u.Memberships, u.PlatformRoles)
		}
		if a.Organisation != nil && (u.Memberships[0].OrganisationName != a.Organisation.Name || u.Memberships[0].Role != a.MembershipRole || u.Memberships[0].MarketCode != "ZW") {
			t.Fatalf("%s: membership %+v", a.Email, u.Memberships[0])
		}
	}
	if err := SeedMemoryUsers(ctx, identity.NewMemoryStore(), hasher, "short", now); err == nil {
		t.Fatal("a too-short seed password must be refused")
	}

	requests := propertyrequests.NewMemoryStore()
	SeedMemoryListings(requests)
	for _, l := range Listings() {
		got, err := requests.GetListing(ctx, l.ID)
		if err != nil || (got.AgentUserID == nil) != (l.AgentUserID == nil) {
			t.Fatalf("listing %s not seeded as defined: %+v %v", l.ID, got, err)
		}
	}
}

func TestCheckEnvironment(t *testing.T) {
	const good = "a-test-seed-password"
	tests := []struct {
		name, env, password, wantText string
	}{
		{"development with a password", "development", good, ""},
		{"production", "production", good, "APP_ENV"},
		{"staging", "staging", good, "APP_ENV"},
		{"unset environment", "", good, "APP_ENV"},
		{"upper-case is not development", "DEVELOPMENT", good, "APP_ENV"},
		{"no password", "development", "", "DEV_SEED_PASSWORD is not set"},
		{"short password", "development", "short", "DEV_SEED_PASSWORD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckEnvironment(tt.env, tt.password)
			if tt.wantText == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.wantText)
			}
			if tt.password != "" && strings.Contains(err.Error(), tt.password) {
				t.Fatal("the error contains the password")
			}
		})
	}
}
