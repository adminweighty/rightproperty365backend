package app

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/legal"
	"rightproperty365.com/backend/internal/modules/tax"
	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// The seeded legal documents take effect on 2026-11-14; the usual test clock
// (2026-11-01) is before that.
func newLegalEnv(t *testing.T) *env {
	e := newEnv(t)
	e.now = time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	return e
}

// legalAdmin signs an administrator in from its own address (the admin
// login bucket allows 5 a minute per address).
func (e *env) legalAdmin(email string) reqOpt {
	e.seq++
	return browser(e.adminLogin(email, seedPassword, withIP(fmt.Sprintf("10.43.%d.%d", e.seq/200, e.seq%200+1))))
}

type documentBody struct {
	Document struct {
		ID                   string  `json:"id"`
		Kind                 string  `json:"kind"`
		Title                string  `json:"title"`
		MarketCode           string  `json:"market_code"`
		Version              string  `json:"version"`
		Status               string  `json:"status"`
		Body                 *string `json:"body_markdown"`
		DraftNotice          bool    `json:"draft_notice"`
		RequiresReacceptance bool    `json:"requires_reacceptance"`
	} `json:"document"`
	RequestedMarket string `json:"requested_market"`
	FallbackMarket  bool   `json:"fallback_market"`
	FallbackLang    bool   `json:"fallback_lang"`
	Preview         bool   `json:"preview"`
}

func TestLegalPublicRoutesFallBackToGlobal(t *testing.T) {
	e := newLegalEnv(t)

	rec := e.get("/v1/legal/terms?market=zw&lang=fr")
	wantStatus(t, rec, http.StatusOK)
	d := decode[documentBody](t, rec)
	if d.Document.MarketCode != legal.MarketGlobal || !d.FallbackMarket || d.RequestedMarket != "ZW" || d.Document.Version != legal.SeedVersion ||
		d.Document.Status != legal.StatusPublished || d.Document.Title != "Terms and Conditions" {
		t.Fatalf("resolution: %+v", d)
	}
	// Every placeholder is clearly a draft and never presented as final.
	if !d.Document.DraftNotice || d.Document.Body == nil || !strings.Contains(*d.Document.Body, "DRAFT — awaiting legal review") ||
		!strings.Contains(*d.Document.Body, "not legal advice") {
		t.Fatalf("placeholder: %+v", d.Document)
	}
	if rec.Header().Get("Content-Language") != "en" || !strings.Contains(strings.Join(rec.Header().Values("Vary"), ","), "Accept-Language") ||
		!strings.HasPrefix(rec.Header().Get("Cache-Control"), "public") {
		t.Fatalf("headers: %v", rec.Header())
	}

	wantError(t, e.get("/v1/legal/cookies"), http.StatusNotFound, legal.CodeDocumentNotFound)
	wantError(t, e.get("/v1/legal/nonsense"), http.StatusNotFound, legal.CodeDocumentNotFound)
	wantError(t, e.get("/v1/legal/terms?market=XX"), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.get("/v1/legal/terms?version=no-such"), http.StatusNotFound, legal.CodeDocumentNotFound)
	if d := decode[documentBody](t, e.get("/v1/legal/privacy?version="+legal.SeedVersion+"&market=GB")); d.Document.Kind != legal.KindPrivacy ||
		!d.FallbackMarket {
		t.Fatalf("by version: %+v", d)
	}

	list := e.get("/v1/legal?market=GB").Body.String()
	for _, kind := range []string{`"kind":"TERMS"`, `"kind":"PRIVACY"`, `"kind":"REFUND"`, `"kind":"EULA"`} {
		if !strings.Contains(list, kind) {
			t.Fatalf("list lacks %s: %s", kind, list)
		}
	}
	if strings.Contains(list, "body_markdown") || strings.Contains(list, `"status":"DRAFT"`) {
		t.Fatalf("list: %s", list)
	}

	// The history: published only; the ZW and GB DRAFT copies never appear.
	hist := e.get("/v1/legal/terms/versions?market=ZW").Body.String()
	if !strings.Contains(hist, `"market_code":"GLOBAL"`) || !strings.Contains(hist, `"current":true`) || strings.Contains(hist, `"DRAFT"`) ||
		strings.Contains(hist, `"market_code":"ZW"`) {
		t.Fatalf("history: %s", hist)
	}
}

func TestLegalDraftsAreHiddenAndPreviewIsForAdmins(t *testing.T) {
	e := newLegalEnv(t)
	// The ZW DRAFT copy is not what the public sees...
	if d := decode[documentBody](t, e.get("/v1/legal/terms?market=ZW")); d.Document.Status != legal.StatusPublished || d.Document.MarketCode != "GLOBAL" {
		t.Fatalf("public: %+v", d)
	}
	// ...and preview is refused to anonymous callers, customers and other administrators.
	wantError(t, e.get("/v1/legal/terms?market=ZW&preview=true"), http.StatusForbidden, legal.CodePreviewForbidden)
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	wantError(t, e.get("/v1/legal/terms?market=ZW&preview=true", consumer), http.StatusForbidden, legal.CodePreviewForbidden)
	wantError(t, e.get("/v1/legal/terms?market=ZW&preview=true", e.legalAdmin(devseed.FinanceAdminEmail)), http.StatusForbidden, legal.CodePreviewForbidden)
	// A market administrator previews its own market only.
	market := e.legalAdmin(devseed.MarketAdminEmail)
	wantError(t, e.get("/v1/legal/terms?market=GB&preview=true", market), http.StatusForbidden, legal.CodePreviewForbidden)

	for _, who := range []reqOpt{e.legalAdmin(devseed.ContentAdminEmail), market} {
		rec := e.get("/v1/legal/terms?market=ZW&preview=1", who)
		wantStatus(t, rec, http.StatusOK)
		d := decode[documentBody](t, rec)
		if !d.Preview || d.Document.Status != legal.StatusDraft || d.Document.MarketCode != "ZW" || rec.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatalf("preview: %+v %v", d, rec.Header())
		}
	}
}

type adminVersionBody struct {
	Version struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Version     string `json:"version"`
		MarketCode  string `json:"market_code"`
		PublishedBy *struct {
			Name string `json:"name"`
		} `json:"published_by"`
		CreatedBy struct {
			Name string `json:"name"`
		} `json:"created_by"`
	} `json:"version"`
}

func TestLegalAdministrationAndReacceptance(t *testing.T) {
	e := newLegalEnv(t)
	content := e.legalAdmin(devseed.ContentAdminEmail)
	consumerCookie := e.loginCookie(identity.SeedConsumerEmail, seedPassword)
	consumer := browser(consumerCookie)

	pending := func() string {
		rec := e.get("/v1/me", consumer)
		wantStatus(t, rec, http.StatusOK)
		b := rec.Body.String()
		return b[strings.Index(b, `"pending_legal_acceptances"`):]
	}
	if p := pending(); p != `"pending_legal_acceptances":[]}`+"\n" {
		t.Fatalf("nothing pending at first: %s", p)
	}

	// A new TERMS version that requires re-acceptance, as a DRAFT.
	effective := e.now.Add(-time.Hour).Format(time.RFC3339)
	body := obj{"kind": "TERMS", "market_code": "GLOBAL", "version": "2026-11-20", "effective_from": effective,
		"body_markdown": "# Terms\n\nDRAFT — awaiting legal review.", "summary_of_changes": "Clearer wording.", "requires_reacceptance": true}
	rec := e.post("/v1/admin/legal", body, content)
	wantStatus(t, rec, http.StatusCreated)
	v := decode[adminVersionBody](t, rec).Version
	if v.Status != legal.StatusDraft || v.CreatedBy.Name != "Connor Content" || v.PublishedBy != nil {
		t.Fatalf("created: %+v", v)
	}
	wantError(t, e.post("/v1/admin/legal", body, content), http.StatusConflict, legal.CodeVersionExists)
	bad := obj{"kind": "TERMS", "market_code": "GLOBAL", "version": "x", "effective_from": effective, "body_markdown": "<script>alert(1)</script>"}
	if env := wantError(t, e.post("/v1/admin/legal", bad, content), http.StatusBadRequest, httpx.CodeValidationFailed); env.Error.Fields["body_markdown"] == "" {
		t.Fatalf("html: %+v", env)
	}
	// Still a draft: the public and /v1/me do not see it.
	if d := decode[documentBody](t, e.get("/v1/legal/terms")); d.Document.Version != legal.SeedVersion {
		t.Fatalf("draft leaked: %+v", d)
	}
	if p := pending(); strings.Contains(p, "2026-11-20") {
		t.Fatal("a draft is never pending")
	}
	wantError(t, e.post("/v1/me/legal-acceptances", obj{"kind": "TERMS", "version": "2026-11-20"}, consumer), http.StatusNotFound, legal.CodeDocumentNotFound)

	wantStatus(t, e.patch("/v1/admin/legal/"+v.ID, obj{"summary_of_changes": "Clearer wording, and contact details."}, content), http.StatusOK)
	wantError(t, e.patch("/v1/admin/legal/"+v.ID, obj{"market_code": "ZW"}, content), http.StatusBadRequest, httpx.CodeValidationFailed)

	// Publish: current, immutable, pending for the consumer.
	rec = e.post("/v1/admin/legal/"+v.ID+"/publish", obj{}, content)
	wantStatus(t, rec, http.StatusOK)
	if p := decode[adminVersionBody](t, rec).Version; p.Status != legal.StatusPublished || p.PublishedBy == nil || p.PublishedBy.Name != "Connor Content" {
		t.Fatalf("published: %+v", p)
	}
	if d := decode[documentBody](t, e.get("/v1/legal/terms?market=GB")); d.Document.Version != "2026-11-20" || !d.Document.RequiresReacceptance {
		t.Fatalf("current: %+v", d)
	}
	wantError(t, e.patch("/v1/admin/legal/"+v.ID, obj{"body_markdown": "changed"}, content), http.StatusConflict, legal.CodeVersionImmutable)
	wantError(t, e.del("/v1/admin/legal/"+v.ID, nil, content), http.StatusConflict, legal.CodeVersionImmutable)
	wantError(t, e.post("/v1/admin/legal/"+v.ID+"/publish", obj{}, content), http.StatusConflict, legal.CodeVersionImmutable)
	if p := pending(); !strings.Contains(p, `"kind":"TERMS"`) || !strings.Contains(p, `"version":"2026-11-20"`) ||
		!strings.Contains(p, `"title":"Terms and Conditions"`) || !strings.Contains(p, `"market_code":"GLOBAL"`) {
		t.Fatalf("pending: %s", p)
	}
	// Nothing is blocked while it is pending.
	wantStatus(t, e.get("/v1/me/saved-listings", consumer), http.StatusOK)

	// Accept (idempotent): no longer pending; listed among the acceptances.
	rec = e.post("/v1/me/legal-acceptances", obj{"kind": "terms", "version": "2026-11-20"}, consumer)
	wantStatus(t, rec, http.StatusCreated)
	if !strings.Contains(rec.Body.String(), `"context":"REACCEPTANCE"`) || !strings.Contains(rec.Body.String(), `"market_code":"GLOBAL"`) {
		t.Fatalf("accepted: %s", rec.Body)
	}
	wantStatus(t, e.post("/v1/me/legal-acceptances", obj{"kind": "TERMS", "version": "2026-11-20", "market_code": "GLOBAL"}, consumer), http.StatusOK)
	if p := pending(); p != `"pending_legal_acceptances":[]}`+"\n" {
		t.Fatalf("still pending: %s", p)
	}
	if mine := e.get("/v1/me/legal-acceptances", consumer).Body.String(); !strings.Contains(mine, `"version":"2026-11-20"`) {
		t.Fatalf("mine: %s", mine)
	}
	wantError(t, e.post("/v1/me/legal-acceptances", obj{"kind": "TERMS", "version": "2026-11-20"}), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	wantError(t, e.post("/v1/me/legal-acceptances", obj{"kind": "BOGUS", "version": "1"}, consumer), http.StatusBadRequest, httpx.CodeValidationFailed)

	// Retire: GLOBAL falls back to the seed; a retired version cannot be accepted.
	wantStatus(t, e.post("/v1/admin/legal/"+v.ID+"/retire", obj{}, content), http.StatusOK)
	if d := decode[documentBody](t, e.get("/v1/legal/terms")); d.Document.Version != legal.SeedVersion {
		t.Fatalf("after retire: %+v", d)
	}
	wantError(t, e.post("/v1/me/legal-acceptances", obj{"kind": "TERMS", "version": "2026-11-20"}, consumer), http.StatusConflict, legal.CodeVersionNotAllowed)
	if hist := e.get("/v1/legal/terms/versions").Body.String(); !strings.Contains(hist, `"status":"RETIRED"`) {
		t.Fatalf("history keeps the retired version: %s", hist)
	}

	// Every change is in the administrators' audit trail.
	audit := e.get("/v1/admin/audit?target_type=LEGAL_DOCUMENT_VERSION", e.legalAdmin(identity.SeedAdminEmail)).Body.String()
	for _, action := range []string{legal.ActionVersionCreated, legal.ActionVersionUpdated, legal.ActionVersionPublished, legal.ActionVersionRetired} {
		if !strings.Contains(audit, `"action":"`+action+`"`) {
			t.Fatalf("audit lacks %s: %s", action, audit)
		}
	}
}

func TestLegalAdminPermissions(t *testing.T) {
	e := newLegalEnv(t)
	effective := e.now.Format(time.RFC3339)
	draft := func(market, label string) obj {
		return obj{"kind": "REFUND", "market_code": market, "version": label, "effective_from": effective, "body_markdown": "DRAFT"}
	}
	// Not for finance, support or customers.
	wantError(t, e.get("/v1/admin/legal", e.legalAdmin(devseed.FinanceAdminEmail)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/admin/legal", e.legalAdmin(devseed.SupportEmail)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/admin/legal"), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// A market administrator: its own market only, never GLOBAL.
	market := e.legalAdmin(devseed.MarketAdminEmail)
	wantStatus(t, e.post("/v1/admin/legal", draft("ZW", "zw-1"), market), http.StatusCreated)
	wantError(t, e.post("/v1/admin/legal", draft("GLOBAL", "g-1"), market), http.StatusForbidden, legal.CodeMarketForbidden)
	wantError(t, e.post("/v1/admin/legal", draft("GB", "gb-1"), market), http.StatusForbidden, legal.CodeMarketForbidden)
	var gbDraft string
	for _, v := range decode[struct {
		Versions []struct {
			ID         string `json:"id"`
			MarketCode string `json:"market_code"`
			Kind       string `json:"kind"`
			Status     string `json:"status"`
		} `json:"versions"`
	}](t, e.get("/v1/admin/legal?market=GB&status=DRAFT", market)).Versions {
		if v.Kind == "TERMS" {
			gbDraft = v.ID
		}
	}
	if gbDraft == "" {
		t.Fatal("the GB DRAFT copy is listed")
	}
	wantError(t, e.post("/v1/admin/legal/"+gbDraft+"/publish", obj{}, market), http.StatusForbidden, legal.CodeMarketForbidden)
	// The super administrator and the content administrator: everywhere.
	wantStatus(t, e.post("/v1/admin/legal", draft("GLOBAL", "g-2"), e.legalAdmin(identity.SeedAdminEmail)), http.StatusCreated)
	rec := e.post("/v1/admin/legal", draft("GB", "gb-2"), e.legalAdmin(devseed.ContentAdminEmail))
	wantStatus(t, rec, http.StatusCreated)
	wantStatus(t, e.del("/v1/admin/legal/"+decode[adminVersionBody](t, rec).Version.ID, nil, e.legalAdmin(devseed.ContentAdminEmail)), http.StatusNoContent)
	if !strings.Contains(e.get("/v1/admin/audit?target_type=LEGAL_DOCUMENT_VERSION", e.legalAdmin(identity.SeedAdminEmail)).Body.String(),
		`"action":"`+legal.ActionVersionDeleted+`"`) {
		t.Fatal("delete not audited")
	}
}

func TestRegistrationRecordsTermsAndPrivacy(t *testing.T) {
	e := newLegalEnv(t)
	body := registerBody("legal.reader@example.com")
	body["consent"] = obj{"privacy_version": legal.SeedVersion, "accepted": true, "terms_version": legal.SeedVersion, "market_code": "zw"}
	wantStatus(t, e.post("/v1/auth/register", body), http.StatusAccepted)
	var kinds []string
	for _, c := range e.identity.Consents() {
		if c.Version == legal.SeedVersion && c.MarketCode != nil && *c.MarketCode == "ZW" && c.Context != nil && *c.Context == "REGISTRATION" {
			kinds = append(kinds, c.ConsentType)
		}
	}
	if strings.Join(kinds, ",") != "PRIVACY_NOTICE,TERMS" {
		t.Fatalf("registration consents: %v", kinds)
	}
	// Without terms_version (the web today): only the privacy notice, as before.
	n := len(e.identity.Consents())
	wantStatus(t, e.post("/v1/auth/register", registerBody("legal.other@example.com")), http.StatusAccepted)
	if c := e.identity.Consents(); len(c) != n+1 || c[n].ConsentType != identity.ConsentPrivacyNotice {
		t.Fatalf("legacy registration: %+v", c[n:])
	}
	for field, consent := range map[string]obj{
		"consent.terms_version": {"privacy_version": "v", "accepted": true, "terms_version": "Ünïcode"},
		"consent.market_code":   {"privacy_version": "v", "accepted": true, "market_code": "Zimbabwe"},
	} {
		b := registerBody("legal.bad@example.com")
		b["consent"] = consent
		if env := wantError(t, e.post("/v1/auth/register", b), http.StatusBadRequest, httpx.CodeValidationFailed); env.Error.Fields[field] == "" {
			t.Fatalf("%s: %+v", field, env)
		}
	}
}

func TestLanguagesAndTranslatedErrors(t *testing.T) {
	e := newEnv(t)
	rec := e.get("/v1/i18n/languages")
	wantStatus(t, rec, http.StatusOK)
	if got := strings.TrimSpace(rec.Body.String()); got != `{"default":"en","languages":[{"code":"en","name":"English","native_name":"English"}]}` {
		t.Fatalf("languages: %s", got)
	}
	// The error envelope's message comes from the catalogue; an unsupported
	// language falls back to English.
	for _, opt := range []reqOpt{withHeader("Accept-Language", "fr-FR, de;q=0.5"), withHeader("Accept-Language", "en-GB")} {
		env := wantError(t, e.get("/v1/no-such-route?lang=xx", opt), http.StatusNotFound, httpx.CodeNotFound)
		if env.Error.Message != "The requested resource was not found." {
			t.Fatalf("message: %q", env.Error.Message)
		}
	}
	if env := wantError(t, e.get("/v1/me"), http.StatusUnauthorized, httpx.CodeUnauthenticated); env.Error.Message != "You need to sign in to do this." {
		t.Fatalf("message: %q", env.Error.Message)
	}
}

func TestTaxRulesAdministration(t *testing.T) {
	e := newEnv(t)
	finance := e.legalAdmin(devseed.FinanceAdminEmail)
	wantError(t, e.get("/v1/admin/tax-rules", e.legalAdmin(devseed.ContentAdminEmail)), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get("/v1/admin/tax-rules", e.legalAdmin(devseed.MarketAdminEmail)), http.StatusForbidden, httpx.CodeForbidden)

	list := e.get("/v1/admin/tax-rules", finance).Body.String()
	for _, want := range []string{`"market_code":"ZW","tax_name":"VAT","rate":"15"`, `"market_code":"GB","tax_name":"VAT","rate":"20"`, `"is_sample":true`} {
		if !strings.Contains(list, want) {
			t.Fatalf("seed lacks %s: %s", want, list)
		}
	}
	if b := e.get("/v1/admin/tax-rules/lookup?market=zw&applies_to=subscriptions&at=2026-11-19T00:00:00Z", finance).Body.String(); !strings.Contains(b, `"rate":"15"`) {
		t.Fatalf("lookup: %s", b)
	}
	if b := e.get("/v1/admin/tax-rules/lookup?market=ZW&applies_to=VENUE_BOOKINGS", finance).Body.String(); strings.TrimSpace(b) != `{"tax_rule":null}` {
		t.Fatalf("no rule: %s", b)
	}
	wantError(t, e.get("/v1/admin/tax-rules/lookup?market=ZW", finance), http.StatusBadRequest, httpx.CodeValidationFailed)

	rec := e.post("/v1/admin/tax-rules", obj{"market_code": "GB", "tax_name": "VAT", "rate": "20", "applies_to": "SUBSCRIPTIONS",
		"effective_from": "2027-01-01T00:00:00Z"}, finance)
	wantError(t, rec, http.StatusConflict, tax.CodeRuleOverlap)
	rec = e.post("/v1/admin/tax-rules", obj{"market_code": "GB", "tax_name": "VAT", "rate": "20.00", "applies_to": "LISTING_FEES",
		"effective_from": "2027-01-01T00:00:00Z", "registration_number_label": "VAT number"}, e.legalAdmin(identity.SeedAdminEmail))
	wantStatus(t, rec, http.StatusCreated)
	if !strings.Contains(rec.Body.String(), `"rate":"20"`) {
		t.Fatalf("created: %s", rec.Body)
	}
	wantError(t, e.post("/v1/admin/tax-rules", obj{"market_code": "XX", "tax_name": "", "rate": "abc", "applies_to": "EVERYTHING"}, finance),
		http.StatusBadRequest, httpx.CodeValidationFailed)
	id := tax.SeedZWVATID.String()
	wantStatus(t, e.patch("/v1/admin/tax-rules/"+id, obj{"is_sample": false, "effective_to": nil}, finance), http.StatusOK)
	wantStatus(t, e.post("/v1/admin/tax-rules/"+id+"/retire", obj{}, finance), http.StatusOK)
	wantError(t, e.post("/v1/admin/tax-rules/"+id+"/retire", obj{}, finance), http.StatusConflict, tax.CodeRuleRetired)
	audit := e.get("/v1/admin/audit?target_type=TAX_RULE", e.legalAdmin(identity.SeedAdminEmail)).Body.String()
	for _, action := range []string{tax.ActionRuleCreated, tax.ActionRuleUpdated, tax.ActionRuleRetired} {
		if !strings.Contains(audit, `"action":"`+action+`"`) {
			t.Fatalf("audit lacks %s: %s", action, audit)
		}
	}
}

// The only tax computed today is the venue quote's TAX fee, set by each venue
// owner. The seeded ZW rule matches it (15%) and the quote does not read the
// tax rules: its result is identical whatever the rules say.
func TestExistingTaxResultsUnchanged(t *testing.T) {
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	settings := devseed.ShowroomSettings(now)
	var fee venues.Fee
	for _, f := range settings.Fees {
		if f.Code == "TAX" {
			fee = f
		}
	}
	rule, ok := tax.TaxFor(tax.SeedRules(now), "ZW", tax.AppliesSubscriptions, now)
	if !ok || rule.Rate != fee.Amount || rule.TaxName+" "+rule.Rate+"%" != fee.Label {
		t.Fatalf("seeded %+v vs venue fee %+v", rule, fee)
	}
	start := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)
	before, err := venues.Price(settings, start, start.Add(4*time.Hour), devseed.ShowroomZone)
	if err != nil {
		t.Fatal(err)
	}
	if before.TotalExcludingDeposit != "230.00" {
		t.Fatalf("price: %+v", before)
	}
	e := newEnv(t)
	quote := func() string {
		rec := e.post("/v1/venues/"+devseed.ShowroomListingID.String()+"/quotes", obj{"start": "2026-11-10T08:00:00Z", "end": "2026-11-10T12:00:00Z"})
		wantStatus(t, rec, http.StatusCreated)
		return rec.Body.String()
	}
	want := `{"code":"TAX","kind":"PERCENT","label":"VAT 15%","amount":"30.00"}`
	if q := quote(); !strings.Contains(q, want) || !strings.Contains(q, `"total_excluding_deposit":"230.00"`) {
		t.Fatalf("quote before: %s", q)
	}
	// Changing the platform's tax rules does not move a venue quote.
	finance := e.legalAdmin(devseed.FinanceAdminEmail)
	wantStatus(t, e.patch("/v1/admin/tax-rules/"+tax.SeedZWVATID.String(), obj{"rate": "99"}, finance), http.StatusOK)
	wantStatus(t, e.post("/v1/admin/tax-rules", obj{"market_code": "ZW", "tax_name": "VAT", "rate": "50", "applies_to": "VENUE_BOOKINGS",
		"effective_from": "2026-01-01T00:00:00Z"}, finance), http.StatusCreated)
	if q := quote(); !strings.Contains(q, want) || !strings.Contains(q, `"total_excluding_deposit":"230.00"`) {
		t.Fatalf("quote after: %s", q)
	}
	after, err := venues.Price(settings, start, start.Add(4*time.Hour), devseed.ShowroomZone)
	if err != nil || after.TotalExcludingDeposit != before.TotalExcludingDeposit {
		t.Fatalf("price changed: %+v -> %+v %v", before, after, err)
	}
}
