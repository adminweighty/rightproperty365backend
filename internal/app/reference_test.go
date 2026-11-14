package app

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

type refItem struct {
	Code          string  `json:"code"`
	Label         string  `json:"label"`
	SingularLabel *string `json:"singular_label"`
	Description   *string `json:"description"`
	SortOrder     int     `json:"sort_order"`
	MarketCode    *string `json:"market_code"`
	ParentCode    *string `json:"parent_code"`
	Group         *struct {
		Code      string `json:"code"`
		Label     string `json:"label"`
		SortOrder int    `json:"sort_order"`
	} `json:"group"`
	RequiresVerification *bool `json:"requires_verification"`
}

type refAll struct {
	Market *string              `json:"market"`
	Sets   map[string][]refItem `json:"sets"`
}

type refSet struct {
	Set    string    `json:"set"`
	Market *string   `json:"market"`
	Items  []refItem `json:"items"`
}

func codes(items []refItem) string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Code)
	}
	return strings.Join(out, ",")
}

// Row counts of the baseline data: a change here is a change to what the web shows.
var wantSetSizes = map[string]int{
	"deal_types": 2, "property_categories": 4, "property_types": 12, "request_types": 5, "contact_methods": 3,
	"account_roles": 15, "directory_kinds": 7, "service_trades": 7, "cities": 14, "suburbs": 24, "listing_features": 59,
	"onboarding_documents": 10, "organisation_kinds": 6, "regions": 14, "price_periods": 2,
}

func TestReferenceAllSets(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/reference")
	wantStatus(t, rec, http.StatusOK)
	all := decode[refAll](t, rec)
	if all.Market != nil {
		t.Fatalf("market = %v, want null", *all.Market)
	}
	if len(all.Sets) != len(reference.Sets()) || len(reference.Sets()) != len(wantSetSizes) {
		t.Fatalf("%d sets, want %d", len(all.Sets), len(wantSetSizes))
	}
	for set, want := range wantSetSizes {
		items, ok := all.Sets[set]
		if !ok || len(items) != want {
			t.Fatalf("set %s has %d rows, want %d", set, len(items), want)
		}
		seen := map[string]bool{}
		for i, it := range items {
			if it.Code == "" || it.Label == "" || seen[it.Code] {
				t.Fatalf("%s[%d] = %+v", set, i, it)
			}
			seen[it.Code] = true
			if i > 0 && it.Group == nil && items[i-1].SortOrder > it.SortOrder {
				t.Fatalf("%s is not ordered by sort_order", set)
			}
		}
		// Each set equals GET /v1/reference/{set}.
		one := e.get("/v1/reference/" + set)
		wantStatus(t, one, http.StatusOK)
		body := decode[refSet](t, one)
		if body.Set != set || codes(body.Items) != codes(items) {
			t.Fatalf("/v1/reference/%s differs from the combined response", set)
		}
	}

	// Exact codes and labels the web relies on (URL values keep the web's spelling).
	for set, want := range map[string]string{
		"deal_types":          "sale,rent",
		"property_categories": "residential,commercial,land,short-stay",
		"property_types":      "house,apartment,townhouse,cottage,office,stand,plot,holiday-home,serviced-apartment,guest-house,private-room,lodge",
		"request_types":       "ENQUIRY,PROPERTY_INFORMATION,VIEWING_REQUEST,CONSULTATION_REQUEST,STAY_REQUEST",
		"contact_methods":     "EMAIL,PHONE,WHATSAPP",
		"account_roles":       strings.Join(identity.SelectableAccountRoles(), ","),
		"directory_kinds":     "agencies,agents,developers,property-managers,solicitors,hosts,service-providers",
		"cities":              "harare,bulawayo,victoria-falls,mutare,london,manchester,birmingham,leeds,liverpool,bristol,edinburgh,glasgow,cardiff,belfast",
	} {
		if got := codes(all.Sets[set]); got != want {
			t.Fatalf("%s codes = %s\nwant      %s", set, got, want)
		}
	}
	labels := map[string]string{}
	for set, items := range all.Sets {
		for _, it := range items {
			labels[set+"/"+it.Code] = it.Label
		}
	}
	for key, want := range map[string]string{
		"deal_types/sale": "For sale", "deal_types/rent": "To rent", "property_categories/land": "Land",
		"request_types/VIEWING_REQUEST": "Book a viewing", "request_types/CONSULTATION_REQUEST": "Speak to the agent",
		"contact_methods/PHONE": "Phone call", "account_roles/LANDLORD": "Landlord",
		"account_roles/HOST": "Host", "account_roles/CONSUMER": "Prospective Buyer",
		"account_roles/PRIVATE_SELLER": "Property Seller (Vendor)", "account_roles/SERVICE_PROVIDER": "Property Service Provider",
		"organisation_kinds/AGENCY": "Real Estate Agency", "organisation_kinds/LAW_FIRM": "Law Firm", "directory_kinds/hosts": "Hosts and Venues",
		"directory_kinds/property-managers": "Property Managers", "service_trades/solar-power": "Solar power",
		"suburbs/mount-pleasant": "Mount Pleasant", "listing_features/8-parking-bays": "8 parking bays",
	} {
		if labels[key] != want {
			t.Fatalf("label of %s = %q, want %q", key, labels[key], want)
		}
	}

	t.Run("set-specific attributes", func(t *testing.T) {
		for _, it := range all.Sets["account_roles"] {
			if it.Group == nil || it.RequiresVerification == nil || it.MarketCode != nil {
				t.Fatalf("account role %+v lacks its group or verification flag", it)
			}
			wantVerify := it.Code != "CONSUMER" && it.Code != "TENANT"
			if *it.RequiresVerification != wantVerify {
				t.Fatalf("%s requires_verification = %v", it.Code, *it.RequiresVerification)
			}
			if auth.IsPlatformRole(it.Code) {
				t.Fatalf("platform role %s is offered as selectable", it.Code)
			}
		}
		groups := map[string]string{}
		for _, it := range all.Sets["account_roles"] {
			groups[it.Code] = it.Group.Code + "|" + it.Group.Label
		}
		if groups["TENANT"] != "PROPERTY_SEEKERS|Property Seekers" || groups["HOST"] != "OTHER_PROPERTY_BUSINESSES|Other property businesses" || groups["AGENT"] != "REAL_ESTATE_PROFESSIONALS|Real Estate Professionals" {
			t.Fatalf("groups = %v", groups)
		}
		for _, it := range all.Sets["property_types"] {
			if it.ParentCode == nil || !strings.Contains("residential,commercial,land,short-stay", *it.ParentCode) {
				t.Fatalf("property type %s has no category", it.Code)
			}
		}
		cityMarket := map[string]string{}
		for _, it := range all.Sets["cities"] {
			if it.MarketCode == nil || (*it.MarketCode != "ZW" && *it.MarketCode != "GB") {
				t.Fatalf("city %+v must belong to market ZW or GB", it)
			}
			cityMarket[it.Code] = *it.MarketCode
		}
		for _, it := range all.Sets["suburbs"] {
			if it.ParentCode == nil || it.MarketCode == nil || cityMarket[*it.ParentCode] != *it.MarketCode {
				t.Fatalf("suburb %+v must belong to a city of its own market", it)
			}
		}
		for _, it := range all.Sets["directory_kinds"] {
			if it.SingularLabel == nil || it.Description == nil {
				t.Fatalf("directory kind %s lacks its singular label or description", it.Code)
			}
		}
		// Keys that do not apply are present and null, never missing.
		body := rec.Body.String()
		for _, key := range []string{`"singular_label":null`, `"parent_code":null`, `"group":null`, `"requires_verification":null`, `"market_code":null`} {
			if !strings.Contains(body, key) {
				t.Fatalf("response lacks %s", key)
			}
		}
		if strings.Contains(body, `"active"`) {
			t.Fatal("the active flag is internal")
		}
	})

	t.Run("every value used by the sample data is a reference row", func(t *testing.T) {
		has := func(set, code string) bool { _, ok := labels[set+"/"+code]; return ok }
		for _, l := range devseed.Sample().Listings {
			if !has("deal_types", l.Deal) || !has("property_categories", l.Category) || !has("property_types", devseed.Code(l.PropertyType)) ||
				!has("cities", devseed.Code(l.City)) || !has("suburbs", devseed.Code(l.Suburb)) {
				t.Fatalf("listing %s uses a value that is not reference data", l.Slug)
			}
			for _, f := range l.Features {
				if !has("listing_features", devseed.Code(f)) {
					t.Fatalf("feature %q is not reference data", f)
				}
			}
		}
		for _, p := range devseed.Sample().Profiles {
			if !has("directory_kinds", p.Kind) || !has("cities", devseed.Code(p.City)) || (p.Category != "" && !has("service_trades", devseed.Code(p.Category))) {
				t.Fatalf("profile %s uses a value that is not reference data", p.Slug)
			}
		}
	})
}

func TestReferenceMarketFilterAndErrors(t *testing.T) {
	e := newEnv(t)

	zw := decode[refAll](t, e.get("/v1/reference?market=zw"))
	if zw.Market == nil || *zw.Market != "ZW" || len(zw.Sets["cities"]) != 4 || len(zw.Sets["deal_types"]) != 2 {
		t.Fatalf("market=ZW: %+v cities=%d", zw.Market, len(zw.Sets["cities"]))
	}
	// A market with no cities yet still gets the rows that apply everywhere.
	bw := decode[refSet](t, e.get("/v1/reference/cities?market=BW"))
	if len(bw.Items) != 0 || bw.Market == nil || *bw.Market != "BW" {
		t.Fatalf("BW cities = %+v", bw.Items)
	}
	if got := strings.TrimSpace(e.get("/v1/reference/cities?market=BW").Body.String()); got != `{"set":"cities","market":"BW","items":[]}` {
		t.Fatalf("body = %s", got)
	}
	if n := len(decode[refSet](t, e.get("/v1/reference/contact_methods?market=BW")).Items); n != 3 {
		t.Fatalf("BW contact methods = %d, want 3", n)
	}

	env := wantError(t, e.get("/v1/reference?market=XX"), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["market"] == "" {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	wantError(t, e.get("/v1/reference/cities?market=XX"), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.get("/v1/reference/no_such_set"), http.StatusNotFound, reference.CodeUnknownSet)
	wantError(t, e.get("/v1/reference/markets"), http.StatusNotFound, reference.CodeUnknownSet)
	wantError(t, e.post("/v1/reference", obj{}), http.StatusMethodNotAllowed, httpx.CodeMethodNotAllowed)
}

func TestReferenceCaching(t *testing.T) {
	e := newEnv(t)
	for _, path := range []string{"/v1/reference", "/v1/reference/deal_types", "/v1/reference/cities?market=ZW"} {
		first := e.get(path)
		wantStatus(t, first, http.StatusOK)
		etag := first.Header().Get("ETag")
		if !strings.HasPrefix(etag, `"`) || len(etag) < 10 {
			t.Fatalf("%s ETag = %q", path, etag)
		}
		if cc := first.Header().Get("Cache-Control"); cc != "public, max-age=60, stale-while-revalidate=300" {
			t.Fatalf("%s Cache-Control = %q", path, cc)
		}
		// Public and the same for everyone: never varies by cookie or authorization.
		vary := strings.ToLower(strings.Join(first.Header().Values("Vary"), ","))
		if strings.Contains(vary, "cookie") || strings.Contains(vary, "authorization") || first.Header().Get("Set-Cookie") != "" {
			t.Fatalf("%s must not vary by session: Vary=%q", path, vary)
		}

		for _, header := range []string{etag, "W/" + etag, `"other", ` + etag, "*"} {
			again := e.get(path, withHeader("If-None-Match", header))
			wantStatus(t, again, http.StatusNotModified)
			if again.Body.Len() != 0 || again.Header().Get("ETag") != etag || again.Header().Get("Cache-Control") == "" {
				t.Fatalf("%s: a 304 has no body and repeats the validators", path)
			}
		}
		wantStatus(t, e.get(path, withHeader("If-None-Match", `"stale"`)), http.StatusOK)

		// A signed-in caller gets the identical response.
		signedIn := e.get(path, withCookie(e.loginCookie(identity.SeedConsumerEmail, seedPassword)))
		if signedIn.Header().Get("ETag") != etag || signedIn.Body.String() != first.Body.String() {
			t.Fatalf("%s differs for a signed-in caller", path)
		}
	}

	// An edit changes the ETag, so caches revalidate to the new content.
	before := e.get("/v1/reference/deal_types").Header().Get("ETag")
	e.reference.Put(reference.SetDealTypes, reference.Item{Code: "sale", Label: "Buy", SortOrder: 10, Active: true})
	after := e.get("/v1/reference/deal_types", withHeader("If-None-Match", before))
	wantStatus(t, after, http.StatusOK)
	if after.Header().Get("ETag") == before || !strings.Contains(after.Body.String(), `"label":"Buy"`) {
		t.Fatalf("the ETag did not change with the content")
	}
	// An allowlisted browser origin can read the ETag.
	cors := e.get("/v1/reference", withOrigin(testOrigin))
	if !strings.Contains(cors.Header().Get("Access-Control-Expose-Headers"), "ETag") {
		t.Fatalf("ETag is not exposed to browsers: %q", cors.Header().Get("Access-Control-Expose-Headers"))
	}
}

func TestInactiveReferenceRowsAreHiddenAndRefused(t *testing.T) {
	t.Run("hidden from both endpoints, and reordering is respected", func(t *testing.T) {
		e := newEnv(t)
		if !e.reference.SetActive(reference.SetContactMethods, "WHATSAPP", false) {
			t.Fatal("row not found")
		}
		e.reference.Put(reference.SetContactMethods, reference.Item{Code: "PHONE", Label: "Phone call", SortOrder: 1, Active: true})
		e.reference.Put(reference.SetContactMethods, reference.Item{Code: "SMS", Label: "Text message", SortOrder: 99, Active: true})
		if got := codes(decode[refSet](t, e.get("/v1/reference/contact_methods")).Items); got != "PHONE,EMAIL,SMS" {
			t.Fatalf("contact methods = %s, want PHONE,EMAIL,SMS", got)
		}
		if got := codes(decode[refAll](t, e.get("/v1/reference")).Sets["contact_methods"]); got != "PHONE,EMAIL,SMS" {
			t.Fatalf("combined contact methods = %s", got)
		}
	})

	t.Run("a deactivated account role cannot be chosen at sign-up", func(t *testing.T) {
		e := newEnv(t)
		body := registerBody("landlord@example.com")
		body["roles"] = []string{"CONSUMER", "LANDLORD"}
		e.reference.SetActive(reference.SetAccountRoles, "LANDLORD", false)
		env := wantError(t, e.post("/v1/auth/register", body), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["roles"] == "" || len(env.Error.Fields) != 1 {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
		e.reference.SetActive(reference.SetAccountRoles, "LANDLORD", true)
		wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
	})

	t.Run("the verification flag comes from the reference data", func(t *testing.T) {
		e := newEnv(t)
		no := false
		e.reference.Put(reference.SetAccountRoles, reference.Item{Code: "HOST", Label: "Host", SortOrder: 50, Active: true, RequiresVerification: &no})
		c := e.selfRegister("host@example.com", "HOST", "DEVELOPER")
		user, _ := e.me(c)
		got := map[string]string{}
		for _, r := range user.AccountRoles {
			got[r.Role] = r.Status
		}
		if got["HOST"] != "ACTIVE" || got["DEVELOPER"] != "PENDING_VERIFICATION" {
			t.Fatalf("statuses = %v", got)
		}
	})

	t.Run("a deactivated contact method is refused on a property request and an enquiry", func(t *testing.T) {
		e := newEnv(t)
		e.reference.SetActive(reference.SetContactMethods, "WHATSAPP", false)
		pr := e.propertyRequest(1)
		pr["preferred_contact_method"] = "WHATSAPP"
		env := wantError(t, e.post("/v1/property-requests", pr), http.StatusBadRequest, httpx.CodeValidationFailed)
		if got := env.Error.Fields["preferred_contact_method"]; got != "must be one of EMAIL, PHONE" || len(env.Error.Fields) != 1 {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
		be := e.enquiry("service-providers", "sample-plumbers")
		be["preferred_contact_method"] = "WHATSAPP"
		env = wantError(t, e.post("/v1/business-enquiries", be), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["preferred_contact_method"] == "" {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
		if len(e.requests.Requests()) != 0 || len(e.enquiries.Enquiries()) != 0 {
			t.Fatal("a refused request was stored")
		}
		pr["preferred_contact_method"] = "PHONE"
		wantStatus(t, e.post("/v1/property-requests", pr), http.StatusCreated)
	})

	t.Run("a deactivated request type is refused", func(t *testing.T) {
		e := newEnv(t)
		e.reference.SetActive(reference.SetRequestTypes, "CONSULTATION_REQUEST", false)
		pr := e.propertyRequest(1)
		pr["request_type"] = "CONSULTATION_REQUEST"
		env := wantError(t, e.post("/v1/property-requests", pr), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["request_type"] != "must be one of ENQUIRY, PROPERTY_INFORMATION, VIEWING_REQUEST, STAY_REQUEST" {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
	})

	t.Run("a deactivated directory kind is refused", func(t *testing.T) {
		e := newEnv(t)
		e.reference.SetActive(reference.SetDirectoryKinds, "hosts", false)
		env := wantError(t, e.post("/v1/business-enquiries", e.enquiry("hosts", "sample-showrooms")), http.StatusBadRequest, httpx.CodeValidationFailed)
		if env.Error.Fields["recipient.kind"] == "" {
			t.Fatalf("fields = %v", env.Error.Fields)
		}
		wantStatus(t, e.post("/v1/business-enquiries", e.enquiry("service-providers", "sample-plumbers")), http.StatusCreated)
	})
}

// Defence in depth: a platform role can never be chosen at sign-up, even if
// someone inserted it into the reference table as a selectable account role.
func TestPlatformRolesAreDeniedEvenIfInTheReferenceTable(t *testing.T) {
	e := newEnv(t)
	no := false
	for i, role := range auth.AllPlatformRoles() {
		e.reference.Put(reference.SetAccountRoles, reference.Item{
			Code: role, Label: "Injected " + role, SortOrder: 500 + i, Active: true, RequiresVerification: &no,
			Group: &reference.Group{Code: "looking", Label: "Looking for property", SortOrder: 1},
		})
	}
	// The poisoned rows really are being served: the deny-list is what stops them.
	if !strings.Contains(e.get("/v1/reference/account_roles").Body.String(), `"code":"SUPER_ADMIN"`) {
		t.Fatal("test setup: the injected row is not in the reference data")
	}
	before := e.identity.UserCount()
	for i, role := range auth.AllPlatformRoles() {
		for _, roles := range [][]string{{role}, {"CONSUMER", role}} {
			body := registerBody(fmt.Sprintf("injected%d@example.com", i))
			body["roles"] = roles
			env := wantError(t, e.post("/v1/auth/register", body, withIP(fmt.Sprintf("198.51.100.%d", i+1))), http.StatusBadRequest, httpx.CodeValidationFailed)
			if env.Error.Fields["roles"] != "That role cannot be chosen at sign-up." {
				t.Fatalf("%v: fields = %v", roles, env.Error.Fields)
			}
		}
	}
	if e.identity.UserCount() != before {
		t.Fatal("a registration with a platform role created an account")
	}
	// And nobody ends up with a platform role or an injected account role.
	c := e.selfRegister("honest@example.com", "CONSUMER")
	user, _ := e.me(c)
	if len(user.PlatformRoles) != 0 || len(user.AccountRoles) != 1 {
		t.Fatalf("unexpected roles: %+v", user)
	}
}

// The reference rows the migrations leave behind (0007, 0009 for the
// onboarding documents, 0010 for the real estate role names and the
// solicitors directory) are the same rows the memory store and cmd/seed use.
func TestMigrationSeedsTheSameReferenceRows(t *testing.T) {
	sql := ""
	for _, name := range []string{"0007_reference_data.up.sql", "0009_onboarding.up.sql", "0010_real_estate_roles.up.sql", "0012_locations.up.sql", "0013_uk_short_stays.up.sql", "0015_availability.up.sql",
		"0016_logos_and_trust.up.sql"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		sql += string(raw)
	}
	// Replay inserts (and upserts) and label updates in order.
	statement := regexp.MustCompile(`(?m)^(?:INSERT INTO reference_items \([^)]*\) VALUES \('([a-z_]+)', '([^']+)', '((?:[^']|'')*)'|UPDATE reference_items SET label = '((?:[^']|'')*)'.* WHERE set_code = '([a-z_]+)' AND code = '([^']+)';)`)
	labels := map[string]string{}
	for _, m := range statement.FindAllStringSubmatch(sql, -1) {
		if m[1] != "" {
			labels[m[1]+"/"+m[2]] = strings.ReplaceAll(m[3], "''", "'")
		} else {
			labels[m[5]+"/"+m[6]] = strings.ReplaceAll(m[4], "''", "'")
		}
	}
	total := 0
	for set, items := range devseed.ReferenceItems() {
		for _, it := range items {
			total++
			if got, ok := labels[set+"/"+it.Code]; !ok || got != it.Label {
				t.Fatalf("the migrations leave %s/%s labelled %q, the baseline says %q", set, it.Code, got, it.Label)
			}
			if (set == reference.SetOnboardingDocuments || set == reference.SetAccountRoles) &&
				(it.Description == nil || !strings.Contains(sql, "'"+strings.ReplaceAll(*it.Description, "'", "''")+"'")) {
				t.Fatalf("the migrations lack the description of %s/%s", set, it.Code)
			}
			// 0012: every city is linked to its region.
			// (0013 inserts the United Kingdom's cities with their region.)
			if set == reference.SetCities && (it.ParentCode == nil ||
				(!strings.Contains(sql, fmt.Sprintf("UPDATE reference_items SET parent_code = '%s', updated_at = now() WHERE set_code = 'cities' AND code = '%s';", *it.ParentCode, it.Code)) &&
					!strings.Contains(sql, fmt.Sprintf("VALUES ('cities', '%s', '%s', NULL, %d, (SELECT id FROM markets WHERE code = 'GB'), '%s')", it.Code, it.Label, it.SortOrder, *it.ParentCode)))) {
				t.Fatalf("the migrations do not link city %s to its region", it.Code)
			}
			if it.Group != nil && !strings.Contains(sql, fmt.Sprintf("'%s', '%s', %d,", it.Group.Code, it.Group.Label, it.Group.SortOrder)) {
				t.Fatalf("the migrations lack the group of %s/%s: %+v", set, it.Code, *it.Group)
			}
		}
	}
	if len(labels) != total {
		t.Fatalf("the migrations leave %d rows, the baseline has %d", len(labels), total)
	}
	for _, role := range auth.AllPlatformRoles() {
		if strings.Contains(sql, "VALUES ('account_roles', '"+role+"'") {
			t.Fatalf("a migration seeds platform role %s as selectable", role)
		}
	}
}
