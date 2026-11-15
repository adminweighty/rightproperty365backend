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
		"admin@example.test":            {"Sam Admin", "", "", "", nil},
		"estates.owner@example.test":    {"Rudo Moyo", "Sample Estates", "OWNER", "AGENCY_OWNER", nil},
		"agent@example.test":            {"Tendai Ncube", "Sample Estates", "AGENT", "AGENT", []int{1, 2, 5}},
		"estates.agent2@example.test":   {"Chipo Dube", "Sample Estates", "AGENT", "AGENT", []int{7, 10, 17}},
		"commercial.agent@example.test": {"Farai Chikwanha", "Sample Commercial", "OWNER", "AGENCY_OWNER", []int{3, 13}},
		"land.agent@example.test":       {"Nomsa Sibanda", "Sample Land Agents", "OWNER", "AGENCY_OWNER", []int{4, 14, 15}},
		"realty.agent@example.test":     {"Tatenda Mutasa", "Sample Realty", "OWNER", "AGENCY_OWNER", []int{8, 9, 16, 18}},
		"bulawayo.agent@example.test":   {"Sipho Nkomo", "Sample Homes Bulawayo", "OWNER", "AGENCY_OWNER", []int{11, 12}},
		"seller@example.test":           {"Grace Mhlanga", "", "", "PRIVATE_SELLER", []int{6}},
		"agent-b@example.test":          {"Blake Agent", "Second Street Realty", "AGENT", "AGENT", nil},
		// The United Kingdom agency (backend_samples.go).
		"sample-homes-london@example.test": {"Sample Homes London Owner", "Sample Homes London", "OWNER", "AGENCY_OWNER", []int{19, 20, 21}},
		// One account for each remaining role (role_samples.go).
		"tenant@example.test":          {"Tariro Tenant", "", "", "TENANT", nil},
		"sales.agent@example.test":     {"Simba Sales", "Sample Estates", "AGENT", "SALES_AGENT", nil},
		"lettings.agent@example.test":  {"Lindiwe Lettings", "Sample Estates", "AGENT", "LETTINGS_AGENT", nil},
		"agency.admin@example.test":    {"Ruvimbo Chari", "Sample Estates", "MANAGER", "AGENCY_ADMIN", nil},
		"agency.staff@example.test":    {"Kuda Staff", "Sample Estates", "STAFF", "AGENCY_STAFF", nil},
		"landlord@example.test":        {"Lloyd Landlord", "", "", "LANDLORD", nil},
		"admin.platform@example.test":  {"Pat Platform", "", "", "", nil},
		"admin.market@example.test":    {"Mika Market", "", "", "", nil},
		"admin.moderator@example.test": {"Morgan Moderator", "", "", "", nil},
		"admin.support@example.test":   {"Sam Support", "", "", "", nil},
		"admin.finance@example.test":   {"Fiona Finance", "", "", "", nil},
		"admin.content@example.test":   {"Connor Content", "", "", "", nil},
	}

	accounts := Accounts()
	if len(accounts) != len(want)+len(allBusinesses()) {
		t.Fatalf("%d accounts, want %d people + %d business owners", len(accounts), len(want), len(allBusinesses()))
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
		if (a.PlatformRole != "") != (a.Email == "admin@example.test" || strings.HasPrefix(a.Email, "admin.")) {
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
	if len(listings) != ListingCount+BackendListingCount+1 {
		t.Fatalf("%d listings, want %d published + 1 expired", len(listings), ListingCount+BackendListingCount)
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
		if (len(u.Memberships) == 1) != (a.Organisation != nil) || (len(u.PlatformRoles) == 1) != (a.PlatformRole != "") ||
			(a.PlatformRole != "" && (u.PlatformRoles[0].Role != a.PlatformRole || u.PlatformRoles[0].MarketCode != a.PlatformMarket)) {
			t.Fatalf("%s: memberships=%v platform=%v", a.Email, u.Memberships, u.PlatformRoles)
		}
		if a.Organisation != nil && (u.Memberships[0].OrganisationName != a.Organisation.Name || u.Memberships[0].Role != a.MembershipRole || u.Memberships[0].MarketCode != MarketOf(a.Organisation)) {
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
		// Not in the web's files: the United Kingdom's host and agency.
		{"hosts", "sample-stays-edinburgh", "Sample Stays Edinburgh"},
		{"agencies", "sample-homes-london", "Sample Homes London"},
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
			a.MembershipRole != "OWNER" || strings.Join(a.AccountRoles, ",") != w[1] ||
			len(a.Profiles) != 1 || a.Profiles[0] != p.Key() || len(a.Listings) != len(businessListings[p.Slug]) {
			t.Fatalf("%s: organisation %+v, owner %+v", p.Key(), p.Organisation, a)
		}
	}
	if n != 21 {
		t.Fatalf("%d business profiles, want 21", n)
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

// The sample logos are drawn here, from a font written for this purpose.
func TestSampleLogosAreDrawn(t *testing.T) {
	if len(glyphs) != 26 {
		t.Fatalf("%d glyphs", len(glyphs))
	}
	for letter, rows := range glyphs {
		ink := 0
		for _, row := range rows {
			if len(row) != 5 || strings.Trim(row, "#.") != "" {
				t.Fatalf("glyph %c has the row %q", letter, row)
			}
			ink += strings.Count(row, "#")
		}
		if ink < 9 {
			t.Fatalf("glyph %c is almost empty", letter)
		}
	}
	logos := SampleLogos()
	businesses := 0
	for _, p := range Profiles() {
		logo, has := logos[p.Key()]
		switch {
		case p.AgentUserID != nil:
			if has {
				t.Fatalf("sample agent %s has a photo", p.Key())
			}
		case p.Key() == NoLogoProfile:
			if has {
				t.Fatalf("%s must have no logo", NoLogoProfile)
			}
		default:
			businesses++
			if !has || logo.Logo.ContentType != "image/png" || logo.Logo.Width != SampleLogoSize || logo.Logo.Size != int64(len(logo.Content)) ||
				len(logo.Logo.Checksum) != 64 || !strings.HasPrefix(logo.Logo.ObjectKey, "logos/sample") || len(Initials(p.Name)) != 2 {
				t.Fatalf("%s: %+v", p.Key(), logo.Logo)
			}
		}
	}
	if businesses != len(logos) {
		t.Fatalf("%d logos for %d businesses", len(logos), businesses)
	}
	// Deterministic: the same business, the same bytes, on every start.
	if string(DrawLogo("SE", logoColours[1], 0)) != string(DrawLogo("SE", logoColours[1], 0)) ||
		string(DrawLogo("SE", logoColours[1], 0)) == string(DrawLogo("SE", logoColours[1], 1)) {
		t.Fatal("DrawLogo is not deterministic, or ignores the variant")
	}
	// Every sample profile record carries its logo and, when verified, a time.
	for _, r := range ProfileRecords(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)) {
		_, has := logos[r.Kind+"/"+r.Slug]
		if (r.Logo != nil) != has || (r.Verified && r.VerifiedAt == nil) {
			t.Fatalf("%s/%s: logo %v, verified at %v", r.Kind, r.Slug, r.Logo != nil, r.VerifiedAt)
		}
	}
	for _, r := range ListingRecords(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)) {
		if (r.ReviewedAt != nil) != (r.Status == "PUBLISHED") || (r.ReviewedAt != nil) != (r.ReviewedBy != nil) {
			t.Fatalf("listing %s: reviewed at %v", r.Slug, r.ReviewedAt)
		}
	}
}

// A property seeker account is only that, and the sample accounts follow the
// rule registration enforces: seeker roles (buyer, tenant) never share an
// account with a professional, business or owner role, or with a platform role.
func TestSampleAccountsFollowTheSeekerRule(t *testing.T) {
	check := func(email string, roles []string, platformRole string) {
		t.Helper()
		seekers := 0
		for _, r := range roles {
			if identity.IsSeekerRole(r) {
				seekers++
			}
		}
		if seekers > 0 && (seekers < len(roles) || platformRole != "") {
			t.Fatalf("%s mixes a property seeker role with another role: %v %s", email, roles, platformRole)
		}
	}
	seekerAccounts := 0
	for _, a := range Accounts() {
		check(a.Email, a.AccountRoles, a.PlatformRole)
		if len(a.AccountRoles) > 0 && identity.IsSeekerRole(a.AccountRoles[0]) {
			seekerAccounts++
			if a.Organisation != nil || len(a.Listings) > 0 || len(a.Profiles) > 0 || a.DirectSeller {
				t.Fatalf("%s is a property seeker with an organisation, listings or a profile", a.Email)
			}
		}
	}
	if seekerAccounts == 0 {
		t.Fatal("the sample data has no property seeker account")
	}
	for _, a := range OnboardingAccounts() {
		var roles []string
		for _, r := range pendingRoles(a.Role) {
			roles = append(roles, r.Role)
		}
		check(a.Email, roles, "")
	}
}

// Every account role and every platform role has at least one verified,
// working sample account (development and staging seed), and there is a
// professional still awaiting verification.
func TestEveryRoleHasASampleAccount(t *testing.T) {
	ctx := context.Background()
	store := identity.NewMemoryStore()
	hasher := identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1}
	now := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	if err := SeedMemoryUsers(ctx, store, hasher, "a-test-seed-password", now); err != nil {
		t.Fatal(err)
	}
	accountRoles, platformRoles := map[string][]string{}, map[string][]string{}
	for _, a := range Accounts() {
		u, err := store.UserByEmail(ctx, a.Email)
		if err != nil || !u.Verified() || u.Status != identity.StatusActive || u.PasswordHash == "" {
			t.Fatalf("%s is not a working verified account: %v", a.Email, err)
		}
		if !strings.HasSuffix(a.Email, "@example.test") {
			t.Fatalf("%s: sample accounts use @example.test", a.Email)
		}
		for _, r := range u.AccountRoles {
			if r.Status == identity.AccountRoleActive {
				accountRoles[r.Role] = append(accountRoles[r.Role], a.Email)
			}
		}
		for _, r := range u.PlatformRoles {
			platformRoles[r.Role] = append(platformRoles[r.Role], a.Email)
			if (r.Role == auth.PlatformMarketAdmin) != (r.MarketCode != "") {
				t.Fatalf("%s: %s with market %q", a.Email, r.Role, r.MarketCode)
			}
		}
	}
	for _, role := range identity.SelectableAccountRoles() {
		if len(accountRoles[role]) == 0 {
			t.Errorf("no verified sample account holds the account role %s", role)
		}
	}
	for _, role := range auth.AllPlatformRoles() {
		if len(platformRoles[role]) == 0 {
			t.Errorf("no sample account holds the platform role %s", role)
		}
	}
	pending := 0
	for _, a := range OnboardingAccounts() {
		if a.Role != "" {
			pending++
		}
	}
	if pending == 0 {
		t.Error("there must be a professional awaiting verification")
	}
	// Emails are unique across every sample account.
	seen := map[string]bool{}
	for _, email := range append(Emails(), PendingAgentEmail, PendingProviderEmail) {
		if seen[email] {
			t.Fatalf("duplicate sample email %s", email)
		}
		seen[email] = true
	}
}
