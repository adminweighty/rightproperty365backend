package devseed

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
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
	if len(accounts) != len(want)+len(businesses) {
		t.Fatalf("%d accounts, want %d people + %d business owners", len(accounts), len(want), len(businesses))
	}
	ids := map[string]bool{}
	for _, a := range accounts {
		if ids[a.ID.String()] {
			t.Fatalf("duplicate user id %s", a.ID)
		}
		w, ok := want[a.Email]
		if !ok {
			// A business owner: covered by TestBusinessProfiles.
			ids[a.ID.String()] = true
			continue
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
	const long = "a-much-longer-staging-seed-password"
	tests := []struct {
		name, env, password, allow, wantText string
	}{
		{"development with a password", "development", good, "", ""},
		{"development ignores the staging opt-in", "development", good, StagingSeedOptIn, ""},
		{"production", "production", good, "", "APP_ENV"},
		{"production even with the opt-in", "production", long, StagingSeedOptIn, "APP_ENV"},
		{"staging without the opt-in", "staging", long, "", "SEED_ALLOW_STAGING"},
		{"staging with a wrong opt-in", "staging", long, "true", "SEED_ALLOW_STAGING"},
		{"staging with a short password", "staging", good, StagingSeedOptIn, "at least 24 characters"},
		{"staging with the opt-in and a long password", "staging", long, StagingSeedOptIn, ""},
		{"unset environment", "", good, "", "APP_ENV"},
		{"upper-case is not development", "DEVELOPMENT", good, "", "APP_ENV"},
		{"no password", "development", "", "", "DEV_SEED_PASSWORD is not set"},
		{"short password", "development", "short", "", "DEV_SEED_PASSWORD"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckEnvironment(tt.env, tt.password, tt.allow)
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

// The 29 kind + slug pairs and display names are the contract shared with the
// web app's directory (src/lib/data/directory.ts): its enquiry form posts
// exactly these.
func TestProfilesMatchTheWebDirectory(t *testing.T) {
	want := []struct{ kind, slug, name string }{
		{"agencies", "sample-estates", "Sample Estates"},
		{"agencies", "sample-realty", "Sample Realty"},
		{"agencies", "sample-commercial", "Sample Commercial"},
		{"agencies", "sample-land-agents", "Sample Land Agents"},
		{"agencies", "sample-homes-bulawayo", "Sample Homes Bulawayo"},
		{"agents", "rudo-moyo", "Rudo Moyo"},
		{"agents", "tendai-ncube", "Tendai Ncube"},
		{"agents", "chipo-dube", "Chipo Dube"},
		{"agents", "tatenda-mutasa", "Tatenda Mutasa"},
		{"agents", "farai-chikwanha", "Farai Chikwanha"},
		{"agents", "nomsa-sibanda", "Nomsa Sibanda"},
		{"agents", "sipho-nkomo", "Sipho Nkomo"},
		{"developers", "sample-developments", "Sample Developments"},
		{"developers", "sample-urban-builders", "Sample Urban Builders"},
		{"developers", "sample-heights", "Sample Heights"},
		{"property-managers", "sample-property-management", "Sample Property Management"},
		{"property-managers", "sample-rentals-bulawayo", "Sample Rentals Bulawayo"},
		{"hosts", "sample-falls-stays", "Sample Falls Stays"},
		{"hosts", "sample-showrooms", "Sample Showrooms"},
		{"service-providers", "sample-plumbers", "Sample Plumbers"},
		{"service-providers", "sample-rapid-plumbing", "Sample Rapid Plumbing"},
		{"service-providers", "sample-electricians", "Sample Electricians"},
		{"service-providers", "sample-sparks", "Sample Sparks"},
		{"service-providers", "sample-solar-installers", "Sample Solar Installers"},
		{"service-providers", "sample-sun-power", "Sample Sun Power"},
		{"service-providers", "sample-borehole-drillers", "Sample Borehole Drillers"},
		{"service-providers", "sample-conveyancers", "Sample Conveyancers"},
		{"service-providers", "sample-valuers", "Sample Valuers"},
		{"service-providers", "sample-movers", "Sample Movers"},
		// Not in the web's files yet: the solicitors directory.
		{"solicitors", "sample-law-chambers", "Sample Law Chambers"},
		{"solicitors", "sample-conveyancing-attorneys", "Sample Conveyancing Attorneys"},
		{"solicitors", "sample-property-lawyers", "Sample Property Lawyers"},
	}
	if len(want) != ProfileCount {
		t.Fatalf("test table has %d rows, want %d", len(want), ProfileCount)
	}
	profiles := Profiles()
	if len(profiles) != ProfileCount {
		t.Fatalf("%d profiles, want %d", len(profiles), ProfileCount)
	}
	byKey := map[string]Profile{}
	ids := map[string]bool{}
	for _, p := range profiles {
		if _, dup := byKey[p.Key()]; dup || ids[p.ID.String()] {
			t.Fatalf("duplicate profile %s", p.Key())
		}
		byKey[p.Key()], ids[p.ID.String()] = p, true
		if p.Organisation == nil {
			t.Fatalf("%s has no owning organisation", p.Key())
		}
		if (p.Kind == businessenquiries.KindAgents) != (p.AgentUserID != nil) {
			t.Fatalf("%s: only agent profiles name an agent", p.Key())
		}
	}
	for _, w := range want {
		p, ok := byKey[w.kind+"/"+w.slug]
		if !ok || p.Name != w.name {
			t.Fatalf("profile %s/%s = %+v, want name %q", w.kind, w.slug, p, w.name)
		}
	}
}

func TestAgencyAndAgentProfileMapping(t *testing.T) {
	byKey := map[string]Profile{}
	for _, p := range Profiles() {
		byKey[p.Key()] = p
	}
	emailOf := map[string]string{}
	for _, a := range Accounts() {
		emailOf[a.ID.String()] = a.Email
	}
	for key, org := range map[string]string{
		"agencies/sample-estates":        "Sample Estates",
		"agencies/sample-realty":         "Sample Realty",
		"agencies/sample-commercial":     "Sample Commercial",
		"agencies/sample-land-agents":    "Sample Land Agents",
		"agencies/sample-homes-bulawayo": "Sample Homes Bulawayo",
	} {
		if p := byKey[key]; p.Organisation.Name != org || p.Name != org {
			t.Fatalf("%s is owned by %q, want %q", key, p.Organisation.Name, org)
		}
	}
	for key, w := range map[string][2]string{
		"agents/rudo-moyo":       {"estates.owner@example.test", "Sample Estates"},
		"agents/tendai-ncube":    {"agent@example.test", "Sample Estates"},
		"agents/chipo-dube":      {"estates.agent2@example.test", "Sample Estates"},
		"agents/tatenda-mutasa":  {"realty.agent@example.test", "Sample Realty"},
		"agents/farai-chikwanha": {"commercial.agent@example.test", "Sample Commercial"},
		"agents/nomsa-sibanda":   {"land.agent@example.test", "Sample Land Agents"},
		"agents/sipho-nkomo":     {"bulawayo.agent@example.test", "Sample Homes Bulawayo"},
	} {
		p := byKey[key]
		if p.AgentUserID == nil || emailOf[p.AgentUserID.String()] != w[0] || p.Organisation.Name != w[1] {
			t.Fatalf("%s -> agent %q in %q, want %q in %q", key, emailOf[p.AgentUserID.String()], p.Organisation.Name, w[0], w[1])
		}
	}
}

func TestBusinessProfiles(t *testing.T) {
	wantKinds := map[string][2]string{
		businessenquiries.KindDevelopers:       {"DEVELOPER", "DEVELOPER"},
		businessenquiries.KindPropertyManagers: {"PROPERTY_MANAGER", "PROPERTY_MANAGER"},
		businessenquiries.KindSolicitors:       {"LAW_FIRM", "SOLICITOR"},
		businessenquiries.KindHosts:            {"HOST", "HOST"},
		businessenquiries.KindServiceProviders: {"SERVICE_PROVIDER", "SERVICE_PROVIDER"},
	}
	accounts := map[string]Account{}
	for _, a := range Accounts() {
		accounts[a.Email] = a
	}
	n := 0
	orgs := map[string]bool{}
	for _, p := range Profiles() {
		w, ok := wantKinds[p.Kind]
		if !ok {
			continue
		}
		n++
		if orgs[p.Organisation.ID.String()] {
			t.Fatalf("%s shares an organisation with another profile", p.Key())
		}
		orgs[p.Organisation.ID.String()] = true
		a, ok := accounts[p.Slug+"@example.test"]
		if !ok {
			t.Fatalf("%s has no owner account %s@example.test", p.Key(), p.Slug)
		}
		if p.Organisation.Kind != w[0] || p.Organisation.Name != p.Name || a.Organisation.ID != p.Organisation.ID ||
			a.MembershipRole != "OWNER" || strings.Join(a.AccountRoles, ",") != "CONSUMER,"+w[1] ||
			len(a.Profiles) != 1 || a.Profiles[0] != p.Key() || len(a.Listings) != 0 {
			t.Fatalf("%s: organisation %+v, owner %+v", p.Key(), p.Organisation, a)
		}
	}
	if n != 20 {
		t.Fatalf("%d business profiles, want 20", n)
	}
	if got := accounts["sample-plumbers@example.test"].Name; got != "Sample Plumbers Owner" {
		t.Fatalf("owner name = %q", got)
	}
}

func TestSeedMemoryProfiles(t *testing.T) {
	store := businessenquiries.NewMemoryStore()
	SeedMemoryProfiles(store)
	for _, p := range Profiles() {
		got, err := store.GetProfile(context.Background(), p.Kind, p.Slug)
		if err != nil {
			t.Fatalf("%s: %v", p.Key(), err)
		}
		if got.Status != businessenquiries.ProfileActive || businessenquiries.ResolveRecipient(got) == nil {
			t.Fatalf("%s is not ACTIVE with a recipient: %+v", p.Key(), got)
		}
		if r := businessenquiries.ResolveRecipient(got); (r.UserID != nil) != (p.Kind == businessenquiries.KindAgents) {
			t.Fatalf("%s: only agent profiles route to a person", p.Key())
		}
	}
}
