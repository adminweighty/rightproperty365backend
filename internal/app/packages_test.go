package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/packages"
	"rightproperty365.com/backend/internal/modules/propertyrequests"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

type priceBody struct {
	BillingTerm string `json:"billing_term"`
	Amount      string `json:"amount"`
	Currency    string `json:"currency"`
	MinorUnit   int    `json:"minor_unit"`
	Sample      bool   `json:"sample"`
}

type packageBody struct {
	ID     string `json:"id"`
	Code   string `json:"code"`
	Name   string `json:"name"`
	Market struct {
		Code     string `json:"code"`
		Currency string `json:"currency"`
	} `json:"market"`
	ListingLimit       int         `json:"listing_limit"`
	Active             bool        `json:"active"`
	IsDefault          bool        `json:"is_default"`
	Prices             []priceBody `json:"prices"`
	CurrentAssignments int         `json:"current_assignments"`
	Can                struct {
		Delete bool `json:"delete"`
	} `json:"can"`
}

type usageBody struct {
	Market struct {
		Code string `json:"code"`
		Name string `json:"name"`
	} `json:"market"`
	Package *struct {
		ID   string `json:"id"`
		Code string `json:"code"`
	} `json:"package"`
	Basis      string `json:"basis"`
	Assignment *struct {
		ID       string  `json:"id"`
		StartsOn string  `json:"starts_on"`
		EndsOn   *string `json:"ends_on"`
	} `json:"assignment"`
	Scheduled *struct {
		ID      string `json:"id"`
		Package struct {
			Code string `json:"code"`
		} `json:"package"`
		StartsOn string `json:"starts_on"`
	} `json:"scheduled"`
	Limit      *int       `json:"limit"`
	Used       int        `json:"used"`
	Remaining  *int       `json:"remaining"`
	AtLimit    bool       `json:"at_limit"`
	OverLimit  bool       `json:"over_limit"`
	OverBy     int        `json:"over_by"`
	GraceUntil *time.Time `json:"grace_until"`
	Message    string     `json:"message"`
}

type accountUsageBody struct {
	Account struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"account"`
	Markets     []usageBody `json:"markets"`
	Assignments []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Source string `json:"source"`
	} `json:"assignments"`
}

func (b accountUsageBody) market(t *testing.T, code string) usageBody {
	t.Helper()
	for _, m := range b.Markets {
		if m.Market.Code == code {
			return m
		}
	}
	t.Fatalf("no usage for %s in %+v", code, b.Markets)
	return usageBody{}
}

type quotaListingBody struct {
	ID           string     `json:"id"`
	Status       string     `json:"status"`
	Slug         string     `json:"slug"`
	PackageUsage *usageBody `json:"package_usage"`
	QuotaWarning *string    `json:"quota_warning"`
}

func quotaListing(t *testing.T, rec *httptest.ResponseRecorder) quotaListingBody {
	t.Helper()
	wantStatus(t, rec, http.StatusOK)
	return decode[struct {
		Listing quotaListingBody `json:"listing"`
	}](t, rec).Listing
}

type quotaErrorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details struct {
			Limit   int `json:"limit"`
			Used    int `json:"used"`
			Package struct {
				Code string `json:"code"`
			} `json:"package"`
			MarketCode string `json:"market_code"`
			Account    struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"account"`
		} `json:"details"`
	} `json:"error"`
}

// createPackage creates a package in ZW as the super administrator.
func (e *env) createPackage(as reqOpt, code string, limit int) packageBody {
	e.t.Helper()
	rec := e.post("/v1/admin/packages", obj{"code": code, "name": code, "market_code": "ZW", "listing_limit": limit,
		"prices": []obj{{"billing_term": "MONTHLY", "amount": "10"}}}, as)
	wantStatus(e.t, rec, http.StatusCreated)
	return decode[struct {
		Package packageBody `json:"package"`
	}](e.t, rec).Package
}

// assign gives an organisation or a user (path "organisations/<id>" or
// "users/<id>") a package from today.
func (e *env) assign(as reqOpt, path, packageID string) {
	e.t.Helper()
	wantStatus(e.t, e.post("/v1/admin/"+path+"/package", obj{"package_id": packageID}, as), http.StatusCreated)
}

func (e *env) approveListing(as reqOpt, id string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.post("/v1/admin/listings/"+id+"/approve", nil, as)
}

func wantQuota(t *testing.T, rec *httptest.ResponseRecorder, limit, used int, code string) quotaErrorBody {
	t.Helper()
	wantStatus(t, rec, http.StatusConflict)
	b := decode[quotaErrorBody](t, rec)
	if b.Error.Code != packages.CodeQuotaReached || b.Error.Details.Limit != limit || b.Error.Details.Used != used ||
		b.Error.Details.Package.Code != code || b.Error.Details.MarketCode != "ZW" || b.Error.Message == "" {
		t.Fatalf("quota error: %+v", b)
	}
	return b
}

// ---------------------------------------------------------------------------
// Public pricing
// ---------------------------------------------------------------------------

func TestPublicPackages(t *testing.T) {
	e := newEnv(t)
	admin := browser(e.admin())
	type publicBody struct {
		Market struct {
			Code     string `json:"code"`
			Currency string `json:"currency"`
		} `json:"market"`
		BillingTerms []struct {
			Code   string `json:"code"`
			Months int    `json:"months"`
		} `json:"billing_terms"`
		Items []struct {
			Code         string      `json:"code"`
			ListingLimit int         `json:"listing_limit"`
			IsDefault    bool        `json:"is_default"`
			Prices       []priceBody `json:"prices"`
		} `json:"items"`
	}
	rec := e.get("/v1/markets/zw/packages")
	wantStatus(t, rec, http.StatusOK)
	if cc := rec.Header().Get("Cache-Control"); !strings.HasPrefix(cc, "public") || rec.Header().Get("ETag") == "" {
		t.Fatalf("cacheable: %q", cc)
	}
	b := decode[publicBody](t, rec)
	if b.Market.Code != "ZW" || b.Market.Currency != "USD" || len(b.BillingTerms) != 3 || b.BillingTerms[2].Code != "ANNUAL" || b.BillingTerms[2].Months != 12 {
		t.Fatalf("market: %+v", b)
	}
	want := []struct {
		code  string
		limit int
		def   bool
	}{{"STARTER", 20, true}, {"BUSINESS", 40, false}, {"PROFESSIONAL", 60, false}}
	if len(b.Items) != 3 {
		t.Fatalf("items: %+v", b.Items)
	}
	for i, w := range want {
		it := b.Items[i]
		if it.Code != w.code || it.ListingLimit != w.limit || it.IsDefault != w.def || len(it.Prices) != 3 || it.Prices[0].BillingTerm != "MONTHLY" ||
			!it.Prices[0].Sample || it.Prices[0].Currency != "USD" || it.Prices[0].MinorUnit != 2 {
			t.Fatalf("item %d: %+v", i, it)
		}
	}
	if gb := decode[publicBody](t, e.get("/v1/markets/GB/packages")); gb.Market.Currency != "GBP" || len(gb.Items) != 3 || gb.Items[0].Prices[0].Amount != "20.00" {
		t.Fatalf("GB: %+v", gb)
	}
	// A market without packages answers an empty list; an unknown one 404.
	if fr := decode[publicBody](t, e.get("/v1/markets/FR/packages")); len(fr.Items) != 0 {
		t.Fatalf("FR: %+v", fr)
	}
	wantError(t, e.get("/v1/markets/XX/packages"), http.StatusNotFound, packages.CodeMarketNotFound)
	// An inactive package is not offered.
	created := e.createPackage(admin, "HIDDEN", 5)
	wantStatus(t, e.patch("/v1/admin/packages/"+created.ID, obj{"active": false}, admin), http.StatusOK)
	if zw := decode[publicBody](t, e.get("/v1/markets/ZW/packages")); len(zw.Items) != 3 {
		t.Fatalf("an inactive package is public: %+v", zw.Items)
	}
}

// ---------------------------------------------------------------------------
// Administration and its market scoping
// ---------------------------------------------------------------------------

func TestPackageAdministration(t *testing.T) {
	e := newEnv(t)
	admin := browser(e.admin())
	super := admin
	market := e.asAdmin(devseed.MarketAdminEmail) // MARKET_ADMIN of ZW
	platform := e.asAdmin(devseed.PlatformAdminEmail)
	type listBody struct {
		Items   []packageBody `json:"items"`
		Markets []string      `json:"markets"`
	}
	type oneBody struct {
		Package packageBody `json:"package"`
	}

	all := decode[listBody](t, e.get("/v1/admin/packages", super))
	if len(all.Items) != 6 || all.Markets != nil {
		t.Fatalf("super: %+v", all)
	}
	var starterZW, businessZW, starterGB packageBody
	for _, p := range all.Items {
		switch {
		case p.Market.Code == "ZW" && p.Code == "STARTER":
			starterZW = p
		case p.Market.Code == "ZW" && p.Code == "BUSINESS":
			businessZW = p
		case p.Market.Code == "GB" && p.Code == "STARTER":
			starterGB = p
		}
	}
	if !starterZW.IsDefault || starterZW.Can.Delete || businessZW.CurrentAssignments != 1 || businessZW.Can.Delete {
		t.Fatalf("seed: %+v %+v", starterZW, businessZW)
	}

	// The market administrator sees and changes only ZW.
	mine := decode[listBody](t, e.get("/v1/admin/packages", market))
	if len(mine.Items) != 3 || len(mine.Markets) != 1 || mine.Markets[0] != "ZW" {
		t.Fatalf("market admin: %+v", mine)
	}
	wantError(t, e.get("/v1/admin/packages?market_code=GB", market), http.StatusForbidden, packages.CodeMarketForbidden)
	wantError(t, e.get("/v1/admin/packages/"+starterGB.ID, market), http.StatusNotFound, packages.CodePackageNotFound)
	wantError(t, e.patch("/v1/admin/packages/"+starterGB.ID, obj{"name": "Mine"}, market), http.StatusNotFound, packages.CodePackageNotFound)
	wantError(t, e.post("/v1/admin/packages", obj{"code": "LOCAL", "name": "Local", "market_code": "GB", "listing_limit": 5}, market),
		http.StatusForbidden, packages.CodeMarketForbidden)
	rec := e.post("/v1/admin/packages", obj{"code": "local", "name": "Local", "market_code": "ZW", "listing_limit": 5,
		"prices": []obj{{"billing_term": "ANNUAL", "amount": "99.5"}, {"billing_term": "MONTHLY", "amount": "10"}}}, market)
	wantStatus(t, rec, http.StatusCreated)
	local := decode[oneBody](t, rec).Package
	if local.Code != "LOCAL" || local.IsDefault || len(local.Prices) != 2 || local.Prices[0].BillingTerm != "MONTHLY" ||
		local.Prices[0].Amount != "10.00" || local.Prices[1].Amount != "99.50" || local.Prices[1].Sample || !local.Can.Delete {
		t.Fatalf("created: %+v", local)
	}
	// A platform administrator manages every market.
	wantStatus(t, e.post("/v1/admin/packages", obj{"code": "LOCAL", "name": "Local", "market_code": "GB", "listing_limit": 5}, platform), http.StatusCreated)
	// Other platform roles may not.
	for _, email := range []string{devseed.ModeratorEmail, devseed.FinanceAdminEmail} {
		wantError(t, e.get("/v1/admin/packages", e.asAdmin(email)), http.StatusForbidden, httpx.CodeForbidden)
	}

	// Validation.
	env := wantError(t, e.post("/v1/admin/packages", obj{"code": "9x", "name": "", "market_code": "ZW", "listing_limit": -1,
		"prices": []obj{{"billing_term": "WEEKLY", "amount": "1"}, {"billing_term": "MONTHLY", "amount": "1.234"}}}, super),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	for _, f := range []string{"code", "name", "listing_limit", "prices[0].billing_term", "prices[1].amount"} {
		if env.Error.Fields[f] == "" {
			t.Fatalf("field %s not refused: %v", f, env.Error.Fields)
		}
	}
	wantError(t, e.post("/v1/admin/packages", obj{"code": "NUM", "name": "Num", "market_code": "ZW", "listing_limit": 1,
		"prices": []obj{{"billing_term": "MONTHLY", "amount": 10}}}, super), http.StatusBadRequest, httpx.CodeInvalidJSON)
	wantError(t, e.post("/v1/admin/packages", obj{"code": "LOCAL", "name": "Again", "market_code": "ZW", "listing_limit": 1}, super),
		http.StatusConflict, packages.CodeCodeTaken)
	env = wantError(t, e.patch("/v1/admin/packages/"+local.ID, obj{"code": "OTHER", "market_code": "GB"}, super), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["code"] == "" || env.Error.Fields["market_code"] == "" {
		t.Fatalf("immutable fields: %v", env.Error.Fields)
	}

	// The default: never deactivated, moved by making another the default.
	wantError(t, e.patch("/v1/admin/packages/"+starterZW.ID, obj{"active": false}, market), http.StatusConflict, packages.CodeIsDefault)
	wantError(t, e.deleteReq("/v1/admin/packages/"+starterZW.ID, market), http.StatusConflict, packages.CodeIsDefault)
	moved := decode[oneBody](t, e.patch("/v1/admin/packages/"+local.ID, obj{"is_default": true, "listing_limit": 7,
		"prices": []obj{{"billing_term": "QUARTERLY", "amount": "25"}}}, market)).Package
	if !moved.IsDefault || moved.ListingLimit != 7 || len(moved.Prices) != 1 || moved.Prices[0].BillingTerm != "QUARTERLY" {
		t.Fatalf("moved: %+v", moved)
	}
	if old := decode[oneBody](t, e.get("/v1/admin/packages/"+starterZW.ID, market)).Package; old.IsDefault {
		t.Fatal("the old default kept its flag")
	}

	// Delete: refused while assigned, allowed when unused.
	b := decode[quotaErrorBody](t, e.deleteReq("/v1/admin/packages/"+businessZW.ID, market))
	if b.Error.Code != packages.CodeInUse {
		t.Fatalf("delete assigned: %+v", b)
	}
	wantStatus(t, e.deleteReq("/v1/admin/packages/"+starterZW.ID, market), http.StatusNoContent)
	wantError(t, e.get("/v1/admin/packages/"+starterZW.ID, market), http.StatusNotFound, packages.CodePackageNotFound)
	wantError(t, e.get("/v1/admin/packages/not-a-uuid", market), http.StatusNotFound, packages.CodePackageNotFound)

	// Every change is in the admin audit.
	actions := map[string]int{}
	for _, ev := range e.audit.All() {
		if ev.TargetType == adminaudit.TargetListingPackage {
			actions[ev.Action]++
		}
	}
	if actions[adminaudit.ActionPackageCreated] < 2 || actions[adminaudit.ActionPackageUpdated] < 1 || actions[adminaudit.ActionPackageDeleted] != 1 {
		t.Fatalf("audit: %v", actions)
	}
}

// ---------------------------------------------------------------------------
// Usage, the default package and who may see it
// ---------------------------------------------------------------------------

func TestPackageUsageAndDefault(t *testing.T) {
	e := newEnv(t)
	c := newCast(e)
	estates := "/v1/agent/organisations/" + devseed.SampleEstates.ID.String() + "/package"
	second := "/v1/agent/organisations/" + identity.SeedSecondOrganisationID.String() + "/package"

	// Sample Estates holds Business (assigned by the seed).
	b := decode[accountUsageBody](t, e.get(estates, c.as(identity.SeedAgentEmail)))
	zw := b.market(t, "ZW")
	if b.Account.Kind != packages.KindOrganisation || b.Account.Name != "Sample Estates" || zw.Basis != packages.BasisAssigned ||
		zw.Package == nil || zw.Package.Code != "BUSINESS" || *zw.Limit != 40 || zw.Used == 0 || *zw.Remaining != 40-zw.Used || zw.AtLimit ||
		zw.Assignment == nil || zw.Message == "" {
		t.Fatalf("Sample Estates: %+v", b)
	}
	// STAFF may read it too; a member of another organisation may not.
	wantStatus(t, e.get(estates, c.as(devseed.AgencyStaffEmail)), http.StatusOK)
	wantError(t, e.get(estates, c.as(identity.SeedAgentBEmail)), http.StatusNotFound, packages.CodeAccountNotFound)
	wantError(t, e.get("/v1/agent/organisations/not-a-uuid/package", c.as(identity.SeedAgentBEmail)), http.StatusNotFound, packages.CodeAccountNotFound)

	// Second Street Realty has no assignment: the ZW default, Starter.
	s := decode[accountUsageBody](t, e.get(second, c.as(identity.SeedAgentBEmail))).market(t, "ZW")
	if s.Basis != packages.BasisDefault || s.Package.Code != "STARTER" || *s.Limit != 20 || s.Used != 0 || s.Assignment != nil {
		t.Fatalf("Second Street: %+v", s)
	}

	// The direct owner: /v1/me/package and their own id as the book.
	seller := c.as(devseed.SellerEmail)
	mine := decode[accountUsageBody](t, e.get("/v1/me/package", seller))
	if mine.Account.Kind != packages.KindDirectOwner || mine.Account.ID != devseed.SellerID.String() || mine.market(t, "ZW").Used != 1 ||
		mine.market(t, "ZW").Basis != packages.BasisDefault {
		t.Fatalf("seller: %+v", mine)
	}
	wantStatus(t, e.get("/v1/agent/organisations/"+devseed.SellerID.String()+"/package", seller), http.StatusOK)
	wantError(t, e.get("/v1/agent/organisations/"+devseed.SellerID.String()+"/package", c.as(identity.SeedAgentEmail)),
		http.StatusNotFound, packages.CodeAccountNotFound)
	wantError(t, e.get("/v1/me/package", c.as(identity.SeedConsumerEmail)), http.StatusForbidden, httpx.CodeForbidden)

	// A market with no packages is not limited.
	fr := decode[accountUsageBody](t, e.get("/v1/me/package?market_code=FR", seller))
	if len(fr.Markets) != 1 || fr.Markets[0].Basis != packages.BasisNone || fr.Markets[0].Limit != nil || fr.Markets[0].Package != nil ||
		fr.Markets[0].AtLimit {
		t.Fatalf("FR: %+v", fr)
	}
	wantError(t, e.get("/v1/me/package?market_code=XX", seller), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// ---------------------------------------------------------------------------
// Enforcement on every go-live path
// ---------------------------------------------------------------------------

func TestQuotaEnforcedOnEveryGoLive(t *testing.T) {
	e := newEnv(t)
	admin := browser(e.admin())
	c := newCast(e)
	agentB := c.as(identity.SeedAgentBEmail)
	org := identity.SeedSecondOrganisationID.String()
	tiny := e.createPackage(admin, "TINY", 1)
	e.assign(admin, "organisations/"+org, tiny.ID)

	// A first publication within the limit.
	a := e.readyListing(agentB, "Quota listing A")
	wantStatus(t, e.post("/v1/me/listings/"+a.ID+"/submit", nil, agentB), http.StatusOK)
	wantStatus(t, e.approveListing(admin, a.ID), http.StatusOK)

	// At the limit: submitting is allowed, with a warning; approval is refused.
	bl := e.readyListing(agentB, "Quota listing B")
	submitted := quotaListing(t, e.post("/v1/me/listings/"+bl.ID+"/submit", nil, agentB))
	if submitted.Status != "PENDING_REVIEW" || submitted.QuotaWarning == nil || submitted.PackageUsage == nil || !submitted.PackageUsage.AtLimit ||
		submitted.PackageUsage.Used != 1 || !strings.Contains(*submitted.QuotaWarning, "TINY package allows 1") {
		t.Fatalf("submitted at the limit: %+v", submitted)
	}
	review := quotaListing(t, e.get("/v1/admin/listings/"+bl.ID, admin))
	if review.QuotaWarning == nil || !strings.Contains(*review.QuotaWarning, "Approving this listing will be refused") || review.PackageUsage.Used != 1 {
		t.Fatalf("review screen: %+v", review)
	}
	q := wantQuota(t, e.approveListing(admin, bl.ID), 1, 1, tiny.Code)
	if q.Error.Details.Account.Kind != packages.KindOrganisation || q.Error.Details.Account.ID != org ||
		q.Error.Details.Account.Name != identity.SeedSecondOrganisationName || !strings.Contains(q.Error.Message, "Zimbabwe") {
		t.Fatalf("details: %+v", q)
	}
	if still := quotaListing(t, e.get("/v1/me/listings/"+bl.ID, agentB)); still.Status != "PENDING_REVIEW" {
		t.Fatalf("a refused approval changed the listing: %s", still.Status)
	}

	// An edit of a published listing is never blocked.
	wantStatus(t, e.patch("/v1/me/listings/"+a.ID, obj{"title": "Quota listing A, renamed"}, agentB), http.StatusOK)
	wantStatus(t, e.post("/v1/me/listings/"+a.ID+"/submit", nil, agentB), http.StatusOK)
	wantStatus(t, e.approveListing(admin, a.ID), http.StatusOK)

	// A slot frees up: B is approved.
	wantStatus(t, e.post("/v1/me/listings/"+a.ID+"/archive", nil, agentB), http.StatusOK)
	wantStatus(t, e.approveListing(admin, bl.ID), http.StatusOK)

	// Relist (BR/FR-007): back to review with a warning; approval refused
	// while B is live, accepted once B is archived.
	relisted := quotaListing(t, e.post("/v1/me/listings/"+a.ID+"/relist", nil, agentB))
	if relisted.Status != "PENDING_REVIEW" || relisted.QuotaWarning == nil {
		t.Fatalf("relisted: %+v", relisted)
	}
	wantQuota(t, e.approveListing(admin, a.ID), 1, 1, tiny.Code)
	wantStatus(t, e.post("/v1/me/listings/"+bl.ID+"/archive", nil, agentB), http.StatusOK)
	wantStatus(t, e.approveListing(admin, a.ID), http.StatusOK)

	// A direct owner is counted on their own: the seller has listing 6 live.
	sellerPath := "users/" + devseed.SellerID.String()
	e.assign(admin, sellerPath, tiny.ID)
	seller := c.as(devseed.SellerEmail)
	own := e.readyListing(seller, "Seller's second house")
	submittedOwn := quotaListing(t, e.post("/v1/me/listings/"+own.ID+"/submit", nil, seller))
	if submittedOwn.QuotaWarning == nil {
		t.Fatalf("seller at the limit: %+v", submittedOwn)
	}
	sq := wantQuota(t, e.approveListing(admin, own.ID), 1, 1, tiny.Code)
	if sq.Error.Details.Account.Kind != packages.KindDirectOwner || sq.Error.Details.Account.ID != devseed.SellerID.String() {
		t.Fatalf("seller details: %+v", sq)
	}
}

// TestParallelApprovalsAtTheLimit sends two approvals at once for an account
// at limit - 1: exactly one is approved (the Postgres version of this test,
// with the advisory lock, is in the packages module).
func TestParallelApprovalsAtTheLimit(t *testing.T) {
	e := newEnv(t)
	admin := browser(e.admin())
	c := newCast(e)
	agentB := c.as(identity.SeedAgentBEmail)
	org := identity.SeedSecondOrganisationID.String()
	two := e.createPackage(admin, "PAIR", 2)
	e.assign(admin, "organisations/"+org, two.ID)
	first := e.readyListing(agentB, "Parallel listing 1")
	wantStatus(t, e.post("/v1/me/listings/"+first.ID+"/submit", nil, agentB), http.StatusOK)
	wantStatus(t, e.approveListing(admin, first.ID), http.StatusOK)
	var ids []string
	for _, title := range []string{"Parallel listing 2", "Parallel listing 3"} {
		l := e.readyListing(agentB, title)
		wantStatus(t, e.post("/v1/me/listings/"+l.ID+"/submit", nil, agentB), http.StatusOK)
		ids = append(ids, l.ID)
	}
	cookie := e.admin()
	codes := make([]int, len(ids))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/admin/listings/"+id+"/approve", nil)
			req.RemoteAddr = "198.51.100.200:40000"
			browser(cookie)(req)
			rec := httptest.NewRecorder()
			<-start
			e.handler.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i, id)
	}
	close(start)
	wg.Wait()
	ok, refused := 0, 0
	for _, code := range codes {
		switch code {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			refused++
		default:
			t.Fatalf("unexpected status %d", code)
		}
	}
	if ok != 1 || refused != 1 {
		t.Fatalf("parallel approvals: %v", codes)
	}
	u := decode[accountUsageBody](t, e.get("/v1/agent/organisations/"+org+"/package", agentB)).market(t, "ZW")
	if u.Used != 2 || !u.AtLimit || u.OverLimit {
		t.Fatalf("after: %+v", u)
	}
}

// ---------------------------------------------------------------------------
// Grace rule and assignment rules
// ---------------------------------------------------------------------------

func TestQuotaGraceRule(t *testing.T) {
	e := newEnv(t)
	admin := browser(e.admin())
	c := newCast(e)
	tendai := c.as(identity.SeedAgentEmail)
	estates := "organisations/" + devseed.SampleEstates.ID.String()
	before := decode[accountUsageBody](t, e.get("/v1/agent/"+estates+"/package", tendai)).market(t, "ZW")
	if before.Used < 2 {
		t.Fatalf("the sample agency needs live listings: %+v", before)
	}
	// A downgrade below the current count.
	small := e.createPackage(admin, "SMALL", before.Used-1)
	e.assign(admin, estates, small.ID)
	after := decode[accountUsageBody](t, e.get("/v1/agent/"+estates+"/package", tendai)).market(t, "ZW")
	if !after.OverLimit || after.OverBy != 1 || after.Used != before.Used || *after.Remaining != 0 || after.GraceUntil == nil ||
		!after.GraceUntil.After(e.now) || !strings.Contains(after.Message, "stay live") {
		t.Fatalf("over the limit: %+v", after)
	}
	// The listings already live stay live.
	if pub := decode[struct {
		Total int `json:"total"`
	}](t, e.get("/v1/listings?page_size=48")); pub.Total == 0 {
		t.Fatal("no public listings")
	}
	if live := quotaListing(t, e.get("/v1/me/listings/"+propertyrequests.SeedListingID(1).String(), tendai)); live.Status != "PUBLISHED" {
		t.Fatalf("a live listing was unpublished by the downgrade: %s", live.Status)
	}
	// Only new go-lives are refused: the sample listing waiting for review.
	wantQuota(t, e.approveListing(admin, devseed.PendingListingID.String()), before.Used-1, before.Used, small.Code)

	// An assignment that ends falls back to the default (Starter, 20).
	wantStatus(t, e.post("/v1/admin/"+estates+"/package", obj{"package_id": small.ID, "ends_on": e.now.Format("2006-01-02")},
		admin), http.StatusCreated)
	e.now = e.now.Add(48 * time.Hour)
	back := decode[accountUsageBody](t, e.get("/v1/agent/"+estates+"/package", c.as(devseed.AgencyAdminEmail))).market(t, "ZW")
	if back.Basis != packages.BasisDefault || back.Package.Code != "STARTER" || *back.Limit != 20 {
		t.Fatalf("after expiry: %+v", back)
	}
}

func TestPackageAssignmentRules(t *testing.T) {
	e := newEnv(t)
	admin := browser(e.admin())
	super := admin
	market := e.asAdmin(devseed.MarketAdminEmail)
	estates := "/v1/admin/organisations/" + devseed.SampleEstates.ID.String() + "/package"
	all := decode[struct {
		Items []packageBody `json:"items"`
	}](t, e.get("/v1/admin/packages", super)).Items
	ids := map[string]string{}
	for _, p := range all {
		ids[p.Market.Code+"/"+p.Code] = p.ID
	}
	today := e.now.Format("2006-01-02")

	// Validation and the 404s.
	env := wantError(t, e.post(estates, obj{"package_id": ids["ZW/PROFESSIONAL"], "starts_on": "2020-01-01", "ends_on": "2019-01-01",
		"source": "INVOICE"}, super), http.StatusBadRequest, httpx.CodeValidationFailed)
	for _, f := range []string{"starts_on", "source"} {
		if env.Error.Fields[f] == "" {
			t.Fatalf("field %s not refused: %v", f, env.Error.Fields)
		}
	}
	wantError(t, e.post(estates, obj{"package_id": ids["ZW/PROFESSIONAL"], "ends_on": "2019-01-01"}, super), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post(estates, obj{"package_id": uuid.NewString()}, super), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post("/v1/admin/organisations/"+uuid.NewString()+"/package", obj{"package_id": ids["ZW/PROFESSIONAL"]}, super),
		http.StatusNotFound, packages.CodeOrganisationNotFound)
	wantError(t, e.get("/v1/admin/users/"+uuid.NewString()+"/package", super), http.StatusNotFound, packages.CodeUserNotFound)
	wantError(t, e.get("/v1/admin/organisations/nope/package", super), http.StatusNotFound, packages.CodeOrganisationNotFound)
	// The market administrator may not assign another market's package.
	wantError(t, e.post(estates, obj{"package_id": ids["GB/BUSINESS"]}, market), http.StatusForbidden, packages.CodeMarketForbidden)
	// An inactive package cannot be assigned.
	hidden := e.createPackage(admin, "HIDDEN", 3)
	wantStatus(t, e.patch("/v1/admin/packages/"+hidden.ID, obj{"active": false}, super), http.StatusOK)
	wantError(t, e.post(estates, obj{"package_id": hidden.ID}, super), http.StatusConflict, packages.CodeInactive)

	// A scheduled upgrade, then a replacement from today.
	later := e.now.AddDate(0, 0, 10).Format("2006-01-02")
	rec := e.post(estates, obj{"package_id": ids["ZW/PROFESSIONAL"], "starts_on": later, "note": "Upgrade next month"}, market)
	wantStatus(t, rec, http.StatusCreated)
	res := decode[struct {
		Assignment struct {
			Status   string `json:"status"`
			StartsOn string `json:"starts_on"`
			Source   string `json:"source"`
		} `json:"assignment"`
		Usage usageBody `json:"usage"`
	}](t, rec)
	if res.Assignment.Status != packages.AssignmentScheduled || res.Assignment.Source != packages.SourceAdmin || res.Usage.Package.Code != "BUSINESS" ||
		res.Usage.Scheduled == nil || res.Usage.Scheduled.Package.Code != "PROFESSIONAL" || res.Usage.Assignment == nil ||
		res.Usage.Assignment.EndsOn == nil || *res.Usage.Assignment.EndsOn != e.now.AddDate(0, 0, 9).Format("2006-01-02") {
		t.Fatalf("scheduled: %+v", res)
	}
	wantStatus(t, e.post(estates, obj{"package_id": ids["ZW/STARTER"], "starts_on": today}, market), http.StatusCreated)
	hist := decode[accountUsageBody](t, e.get(estates, market))
	statuses := map[string]int{}
	for _, a := range hist.Assignments {
		statuses[a.Status]++
	}
	if statuses[packages.AssignmentCurrent] != 1 || statuses[packages.AssignmentCancelled] != 2 || hist.market(t, "ZW").Package.Code != "STARTER" ||
		hist.market(t, "ZW").Basis != packages.BasisAssigned {
		t.Fatalf("history: %+v", hist)
	}

	// End: the default applies again; nothing left to end.
	rec = e.post(estates+"/end", obj{"market_code": "ZW", "note": "Contract over"}, market)
	wantStatus(t, rec, http.StatusOK)
	if u := decode[struct {
		Usage usageBody `json:"usage"`
	}](t, rec).Usage; u.Basis != packages.BasisDefault {
		t.Fatalf("after end: %+v", u)
	}
	wantError(t, e.post(estates+"/end", obj{"market_code": "ZW"}, market), http.StatusNotFound, packages.CodeAssignmentNotFound)
	wantError(t, e.post(estates+"/end", obj{"market_code": "GB"}, market), http.StatusForbidden, packages.CodeMarketForbidden)

	// A direct owner's package, and what account deletion does to it.
	seller := "/v1/admin/users/" + devseed.SellerID.String() + "/package"
	wantStatus(t, e.post(seller, obj{"package_id": ids["ZW/PROFESSIONAL"]}, super), http.StatusCreated)
	if u := decode[accountUsageBody](t, e.get(seller, super)); u.Account.Kind != packages.KindDirectOwner || u.market(t, "ZW").Basis != packages.BasisAssigned {
		t.Fatalf("seller: %+v", u)
	}
	e.packages.ForgetUser(devseed.SellerID, "Deleted account", e.now)
	hist2, _ := e.packages.Assignments(t.Context(), packages.Account{Kind: packages.KindDirectOwner, ID: devseed.SellerID})
	if len(hist2) != 1 || hist2[0].CancelledAt == nil {
		t.Fatalf("a deleted owner keeps an assignment: %+v", hist2)
	}

	// Audited, with the account as the target.
	n := 0
	for _, ev := range e.audit.All() {
		if (ev.Action == adminaudit.ActionPackageAssigned || ev.Action == adminaudit.ActionPackageAssignmentEnded) &&
			(ev.TargetType == adminaudit.TargetOrganisation || ev.TargetType == adminaudit.TargetUser) {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("assignment audit records: %d", n)
	}
}
