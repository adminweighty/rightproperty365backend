package app

import (
	"bytes"
	"encoding/csv"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/ledger/ledgertest"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

func ledgerPath(org uuid.UUID, rest string) string {
	return "/v1/agent/organisations/" + org.String() + "/ledger" + rest
}

type ledgerEntryBody struct {
	ID           string  `json:"id"`
	Type         string  `json:"type"`
	Direction    string  `json:"direction"`
	Amount       string  `json:"amount"`
	Currency     string  `json:"currency"`
	Status       string  `json:"status"`
	StatusLabel  string  `json:"status_label"`
	ReversalOf   *string `json:"reversal_of"`
	ReversedBy   *string `json:"reversed_by"`
	CorrectionOf *string `json:"correction_of"`
	CorrectedBy  *string `json:"corrected_by"`
	TransferPair *string `json:"transfer_pair"`
	IsReversed   bool    `json:"is_reversed"`
	Author       struct {
		Name string `json:"name"`
	} `json:"author"`
}

type ledgerPosted struct {
	Entry   ledgerEntryBody   `json:"entry"`
	Created []ledgerEntryBody `json:"created"`
	Updated []ledgerEntryBody `json:"updated"`
}

// The seed: Sample Estates' books in USD and ZAR; every report answers per
// currency only (FR-029), over HTTP.
func TestLedgerSeedAndPerCurrencyReports(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	tariro := devseed.ContactTariroID.String()
	paths := []string{
		"/reports/cashbook?from=2026-09-01&to=2026-11-30", "/reports/cashbook?from=2026-10-01&to=2026-11-30&by=method",
		"/reports/cashbook?from=2026-10-01&to=2026-11-30&by=branch", "/reports/categories?from=2026-09-01&to=2026-11-30",
		"/reports/statement?from=2026-09-01&to=2026-11-30&contact_id=" + tariro, "/reports/receivables",
		"/entries?page_size=100", "/entries/" + devseed.LedgerCorrectedID.String(),
	}
	for _, p := range paths {
		rec := e.get(ledgerPath(org, p), staff)
		wantStatus(t, rec, http.StatusOK)
		if rec.Header().Get("Cache-Control") != "private, no-store" || rec.Header().Get("X-Robots-Tag") != "noindex" {
			t.Fatalf("%s: headers %v", p, rec.Header())
		}
		if err := ledgertest.PerCurrencyProblem(rec.Body.Bytes()); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	cb := decode[ledger.Cashbook](t, e.get(ledgerPath(org, "/reports/cashbook?from=2026-09-01&to=2026-11-30"), staff))
	if len(cb.Currencies) != 2 || cb.Currencies[0].Currency != "USD" || cb.Currencies[1].Currency != "ZAR" {
		t.Fatalf("seed currencies: %+v", cb.Currencies)
	}
	// USD: 500 + 12000 opening, rent 700 (reconciled) + 700 deposit + (70 - 70 + 700 corrected) + 1250 + 300 - 85.50 - 120; transfer
	// nets 0. Plus the rent receipts (API 0.36.1): the cottage's first two (2 x 700, before the period: the opening balance), Flat 1's
	// two (2 x 950) and the cottage's part payment (300).
	if usd := cb.Currencies[0]; usd.OpeningBalance != "1400.00" || usd.ClosingBalance != "19544.50" {
		t.Fatalf("USD cashbook: opening %s closing %s", usd.OpeningBalance, usd.ClosingBalance)
	}
	if zar := cb.Currencies[1]; zar.ClosingBalance != "29500.00" {
		t.Fatalf("ZAR cashbook: %s", zar.ClosingBalance)
	}
	// Receivables: 850.00 of ledger receivables plus the cottage's rent arrears (700.00 45 days overdue, 400.00 of the part-paid due).
	recv := decode[ledger.Receivables](t, e.get(ledgerPath(org, "/reports/receivables"), staff))
	if len(recv.Currencies) != 1 || recv.Currencies[0].Outstanding != "1950.00" || recv.Currencies[0].RentOutstanding != "1100.00" ||
		len(recv.Currencies[0].RentItems) != 2 || recv.Currencies[0].RentItems[0].DaysOverdue != 45 || recv.Currencies[0].RentItems[0].Bucket != "31-60" {
		t.Fatalf("receivables: %+v", recv)
	}
	d := decode[struct {
		Entry   ledgerEntryBody   `json:"entry"`
		Related []ledgerEntryBody `json:"related"`
	}](t, e.get(ledgerPath(org, "/entries/"+devseed.LedgerCorrectedID.String()), staff))
	if !d.Entry.IsReversed || len(d.Related) != 2 {
		t.Fatalf("the seeded correction: %+v", d)
	}
	// Second Street Realty's book is its own.
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	page := decode[ledger.Page](t, e.get(ledgerPath(devseed.SecondStreetRealty.ID, "/entries"), blake))
	if page.Total != 1 || page.Items[0].ID != devseed.LedgerSecondStreetID {
		t.Fatalf("Second Street: %+v", page)
	}
}

// FR-027 over HTTP: an agent records a payment, the owner corrects it; the
// predecessor, the reversal and the new entry stay; the audit trail names both.
func TestLedgerCorrectionOverHTTP(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	body := obj{"type": "RENT_RECEIVED", "method": "CASH", "amount": "65", "currency": "USD", "effective_date": "2026-11-01",
		"contact_id": devseed.ContactTariroID, "description": "Rent"}
	rec := e.post(ledgerPath(org, "/entries"), body, agent)
	wantStatus(t, rec, http.StatusCreated)
	orig := decode[ledgerPosted](t, rec).Entry
	if orig.Status != "USER_RECORDED_UNVERIFIED" || orig.StatusLabel != "Recorded by you, not verified" || orig.Amount != "65.00" {
		t.Fatalf("posted: %+v", orig)
	}
	// FR-028: a body cannot claim gateway confirmation.
	claim := obj{"type": "RENT_RECEIVED", "method": "BANK", "amount": "1", "currency": "USD", "effective_date": "2026-11-01", "status": "GATEWAY_CONFIRMED"}
	env := wantError(t, e.post(ledgerPath(org, "/entries"), claim, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["status"] == "" {
		t.Fatalf("fields: %v", env.Error.Fields)
	}
	// Amounts are strings: a JSON number is refused.
	wantError(t, e.post(ledgerPath(org, "/entries"), obj{"type": "INCOME", "method": "BANK", "amount": 1.5, "currency": "USD",
		"effective_date": "2026-11-01"}, owner), http.StatusBadRequest, httpx.CodeInvalidJSON)
	fixed := obj{"type": "RENT_RECEIVED", "method": "CASH", "amount": "650", "currency": "USD", "effective_date": "2026-11-01",
		"contact_id": devseed.ContactTariroID, "description": "Rent", "reason": "Missing a zero"}
	rec = e.post(ledgerPath(org, "/entries/"+orig.ID+"/correct"), fixed, owner)
	wantStatus(t, rec, http.StatusCreated)
	c := decode[ledgerPosted](t, rec)
	if len(c.Created) != 2 || c.Created[0].Type != "REVERSAL" || *c.Created[0].ReversalOf != orig.ID || *c.Entry.CorrectionOf != orig.ID ||
		c.Entry.Amount != "650.00" || len(c.Updated) != 1 || *c.Updated[0].CorrectedBy != c.Entry.ID || c.Updated[0].Amount != "65.00" {
		t.Fatalf("correction: %+v", c)
	}
	wantError(t, e.post(ledgerPath(org, "/entries/"+orig.ID+"/reverse"), obj{"reason": "again"}, owner), http.StatusConflict, ledger.CodeAlreadyReversed)
	wantError(t, e.post(ledgerPath(org, "/entries/"+c.Created[0].ID+"/reverse"), obj{"reason": "again"}, owner), http.StatusConflict, ledger.CodeNotReversible)
	audit := decode[auditBody](t, e.get("/v1/agent/organisations/"+org.String()+"/audit?page_size=100", owner))
	who := map[string]string{}
	for _, it := range audit.Items {
		if strings.HasPrefix(it.Action, "LEDGER_") {
			who[it.Action] = it.Actor.Name
		}
	}
	if who[orgaudit.ActionLedgerEntryCorrected] != "Rudo Moyo" || who[orgaudit.ActionLedgerEntryCreated] == "" {
		t.Fatalf("audit: %v", who)
	}
	// No PATCH and no DELETE on entries.
	for _, m := range []string{http.MethodPatch, http.MethodDelete, http.MethodPut} {
		if rec := e.do(m, ledgerPath(org, "/entries/"+orig.ID), obj{"amount": "1"}, owner); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: %d", m, rec.Code)
		}
	}
}

// The role gate of the export, its audit, and the roles that may write.
func TestLedgerRolesAndExport(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	manager := browser(e.loginCookie(devseed.AgencyAdminEmail, seedPassword))
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	export := ledgerPath(org, "/reports/export.csv?report=cashbook&from=2026-09-01&to=2026-11-30")
	wantError(t, e.get(export, agent), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get(export, staff), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.get(ledgerPath(org, "/reports/export.csv?report=cashbook&from=bad"), agent), http.StatusForbidden, httpx.CodeForbidden)
	rec := e.get(export, manager)
	wantStatus(t, rec, http.StatusOK)
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="ledger-cashbook-2026-09-01-2026-11-30.csv"` ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("headers: %v", rec.Header())
	}
	rows, err := csv.NewReader(bytes.NewReader(rec.Body.Bytes())).ReadAll()
	if err != nil || rows[0][0] != "currency" || len(rows) < 4 {
		t.Fatalf("csv: %v %v", rows, err)
	}
	for _, r := range rows[1:] {
		if r[0] != "USD" && r[0] != "ZAR" {
			t.Fatalf("a row without a currency: %v", r)
		}
	}
	for _, report := range []string{"entries", "categories", "statement&contact_id=" + devseed.ContactTariroID.String()} {
		wantStatus(t, e.get(ledgerPath(org, "/reports/export.csv?report="+report), owner), http.StatusOK)
	}
	wantError(t, e.get(ledgerPath(org, "/reports/export.csv?report=everything"), owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	audit := decode[auditBody](t, e.get("/v1/agent/organisations/"+org.String()+"/audit?page_size=100", owner))
	exports := 0
	for _, it := range audit.Items {
		if it.Action == orgaudit.ActionLedgerExported {
			exports++
			if it.Target.Type != orgaudit.TargetLedgerBook {
				t.Fatalf("target: %+v", it.Target)
			}
		}
	}
	if exports != 4 {
		t.Fatalf("%d export audit records", exports)
	}
	// Writers and readers.
	entry := obj{"type": "EXPENSE", "category": "maintenance", "method": "CASH", "amount": "12.5", "currency": "USD", "effective_date": "2026-11-10"}
	wantError(t, e.post(ledgerPath(org, "/entries"), entry, staff), http.StatusForbidden, httpx.CodeForbidden)
	rec = e.post(ledgerPath(org, "/entries"), entry, agent)
	wantStatus(t, rec, http.StatusCreated)
	mine := decode[ledgerPosted](t, rec).Entry
	wantError(t, e.post(ledgerPath(org, "/entries/"+mine.ID+"/reconcile"), obj{"reconciled": true}, agent), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(ledgerPath(org, "/entries/"+devseed.LedgerReconciledID.String()+"/reverse"), obj{"reason": "x"}, agent),
		http.StatusForbidden, httpx.CodeForbidden)
	rec = e.post(ledgerPath(org, "/entries/"+mine.ID+"/reconcile"), obj{"reconciled": true, "note": "Statement line 4"}, manager)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[struct {
		Entry ledgerEntryBody `json:"entry"`
	}](t, rec).Entry; got.Status != "RECONCILED" || got.StatusLabel != "Marked reconciled by your team" {
		t.Fatalf("reconciled: %+v", got)
	}
	wantError(t, e.post(ledgerPath(org, "/entries/"+mine.ID+"/reverse"), obj{"reason": "x"}, agent), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(ledgerPath(org, "/categories"), obj{"label": "Bonus", "kind": "IN"}, agent), http.StatusForbidden, httpx.CodeForbidden)
	wantStatus(t, e.post(ledgerPath(org, "/categories"), obj{"label": "Bonus", "kind": "IN"}, manager), http.StatusCreated)
	wantStatus(t, e.do(http.MethodPatch, ledgerPath(org, "/categories/bonus"), obj{"active": false}, owner), http.StatusOK)
	wantError(t, e.do(http.MethodPatch, ledgerPath(org, "/categories/nope"), obj{"active": false}, owner), http.StatusNotFound, ledger.CodeCategoryNotFound)
	// Transfer and opening balance over HTTP.
	rec = e.post(ledgerPath(org, "/entries"), obj{"type": "TRANSFER", "method": "CASH", "amount": "10", "currency": "USD", "effective_date": "2026-11-11",
		"transfer_to": obj{"method": "BANK"}}, agent)
	wantStatus(t, rec, http.StatusCreated)
	if p := decode[ledgerPosted](t, rec); len(p.Created) != 2 || *p.Created[0].TransferPair != p.Created[1].ID {
		t.Fatalf("transfer: %+v", p)
	}
	rec = e.post(ledgerPath(org, "/entries"), obj{"type": "OPENING_BALANCE", "direction": "IN", "method": "CASH", "amount": "1", "currency": "USD",
		"effective_date": "2026-10-01"}, owner)
	env := wantError(t, rec, http.StatusConflict, ledger.CodeOpeningBalanceExists)
	_ = env
	if !strings.Contains(rec.Body.String(), devseed.LedgerOpeningUSDCashID.String()) {
		t.Fatalf("details: %s", rec.Body)
	}
	// JPY has no decimals.
	env = wantError(t, e.post(ledgerPath(org, "/entries"), obj{"type": "INCOME", "method": "CASH", "amount": "10.5", "currency": "JPY",
		"effective_date": "2026-11-11"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	if !strings.Contains(env.Error.Fields["amount"], "whole number") {
		t.Fatalf("JPY: %v", env.Error.Fields)
	}
	// Another book's contact.
	env = wantError(t, e.post(ledgerPath(org, "/entries"), obj{"type": "INCOME", "method": "CASH", "amount": "1", "currency": "USD",
		"effective_date": "2026-11-11", "contact_id": devseed.SecondStreetContactIDs[0]}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	if env.Error.Fields["contact_id"] == "" {
		t.Fatalf("contact: %v", env.Error.Fields)
	}
}

// Attachments: the private storage, sniffed types, audited downloads.
func TestLedgerAttachments(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	id := devseed.LedgerReconciledID.String()
	path := ledgerPath(org, "/entries/"+id+"/attachments")
	body, ct := multipartFile(t, "file", "receipt.txt", "just text")
	wantError(t, e.raw(http.MethodPost, path, body, ct, owner), http.StatusBadRequest, ledger.CodeUnsupportedFileType)
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89"
	body, ct = multipartFile(t, "file", "slip.png", png)
	rec := e.raw(http.MethodPost, path, body, ct, owner)
	wantStatus(t, rec, http.StatusCreated)
	att := decode[struct {
		Attachment struct {
			ID           string `json:"id"`
			ContentType  string `json:"content_type"`
			DownloadPath string `json:"download_path"`
		} `json:"attachment"`
	}](t, rec).Attachment
	if att.ContentType != "image/png" || !strings.HasSuffix(att.DownloadPath, att.ID) {
		t.Fatalf("attachment: %+v", att)
	}
	body, ct = multipartFile(t, "file", "slip.png", png)
	wantError(t, e.raw(http.MethodPost, path, body, ct, staff), http.StatusForbidden, httpx.CodeForbidden)
	rec = e.get(att.DownloadPath, staff)
	wantStatus(t, rec, http.StatusOK)
	if got, _ := io.ReadAll(rec.Body); string(got) != png || rec.Header().Get("Content-Security-Policy") != "default-src 'none'; sandbox" ||
		!strings.HasPrefix(rec.Header().Get("Content-Disposition"), "attachment;") {
		t.Fatalf("download: %v", rec.Header())
	}
	wantError(t, e.get(ledgerPath(org, "/entries/"+id+"/attachments/"+uuid.NewString()), staff), http.StatusNotFound, ledger.CodeAttachmentNotFound)
	audit := decode[auditBody](t, e.get("/v1/agent/organisations/"+org.String()+"/audit?page_size=100", owner))
	seen := map[string]bool{}
	for _, it := range audit.Items {
		seen[it.Action] = true
	}
	if !seen[orgaudit.ActionLedgerAttachmentAdded] || !seen[orgaudit.ActionLedgerAttachmentViewed] {
		t.Fatalf("audit: %v", seen)
	}
}

// Every endpoint answers another organisation's member, a consumer and an
// administrator 404 BOOK_NOT_FOUND, and never data.
func TestLedgerWrongOrganisation(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	eid := devseed.LedgerReconciledID.String()
	aid := uuid.NewString()
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	admin := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword))
	entry := obj{"type": "INCOME", "method": "CASH", "amount": "1", "currency": "USD", "effective_date": "2026-11-01"}
	type call struct {
		method, path string
		body         any
	}
	calls := []call{
		{http.MethodGet, ledgerPath(org, "/options"), nil},
		{http.MethodGet, ledgerPath(org, "/entries"), nil},
		{http.MethodGet, ledgerPath(org, "/entries?page=bad"), nil},
		{http.MethodPost, ledgerPath(org, "/entries"), entry},
		{http.MethodPost, ledgerPath(org, "/entries"), obj{"amount": 1}},
		{http.MethodGet, ledgerPath(org, "/entries/"+eid), nil},
		{http.MethodGet, ledgerPath(org, "/entries/not-a-uuid"), nil},
		{http.MethodPost, ledgerPath(org, "/entries/"+eid+"/reverse"), obj{"reason": "x"}},
		{http.MethodPost, ledgerPath(org, "/entries/"+eid+"/correct"), entry},
		{http.MethodPost, ledgerPath(org, "/entries/"+eid+"/reconcile"), obj{"reconciled": false}},
		{http.MethodPost, ledgerPath(org, "/entries/"+eid+"/notes"), obj{"text": "hello"}},
		{http.MethodGet, ledgerPath(org, "/entries/"+eid+"/attachments/"+aid), nil},
		{http.MethodGet, ledgerPath(org, "/reports/cashbook"), nil},
		{http.MethodGet, ledgerPath(org, "/reports/cashbook?from=bad"), nil},
		{http.MethodGet, ledgerPath(org, "/reports/categories"), nil},
		{http.MethodGet, ledgerPath(org, "/reports/statement?contact_id="+devseed.ContactTariroID.String()), nil},
		{http.MethodGet, ledgerPath(org, "/reports/receivables"), nil},
		{http.MethodGet, ledgerPath(org, "/reports/export.csv?report=entries"), nil},
		{http.MethodGet, ledgerPath(org, "/categories"), nil},
		{http.MethodPost, ledgerPath(org, "/categories"), obj{"label": "Intruder", "kind": "IN"}},
		{http.MethodPatch, ledgerPath(org, "/categories/rent"), obj{"active": false}},
	}
	for who, as := range map[string]reqOpt{"another organisation": blake, "consumer": consumer, "administrator": admin} {
		for _, c := range calls {
			rec := e.do(c.method, c.path, c.body, as)
			wantError(t, rec, http.StatusNotFound, ledger.CodeBookNotFound)
			if strings.Contains(rec.Body.String(), "RCPT-1001") {
				t.Fatalf("%s %s %s: %s", who, c.method, c.path, rec.Body.String())
			}
		}
		file, ct := multipartFile(t, "file", "x.pdf", "%PDF-1.4\n%%EOF\n")
		wantError(t, e.raw(http.MethodPost, ledgerPath(org, "/entries/"+eid+"/attachments"), file, ct, as), http.StatusNotFound, ledger.CodeBookNotFound)
	}
	// A malformed book ID is the same 404; signed out is 401.
	wantError(t, e.get("/v1/agent/organisations/nope/ledger/entries", blake), http.StatusNotFound, ledger.CodeBookNotFound)
	wantError(t, e.get(ledgerPath(org, "/entries")), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	// Blake's own book does not reach Sample Estates' entries.
	wantError(t, e.get(ledgerPath(devseed.SecondStreetRealty.ID, "/entries/"+eid), blake), http.StatusNotFound, ledger.CodeEntryNotFound)
	// A direct owner's personal book: their own user ID only.
	seller := browser(e.loginCookie(devseed.SellerEmail, seedPassword))
	wantStatus(t, e.get(ledgerPath(devseed.SellerID, "/entries"), seller), http.StatusOK)
	wantError(t, e.get(ledgerPath(devseed.SellerID, "/entries"), blake), http.StatusNotFound, ledger.CodeBookNotFound)
	wantError(t, e.get(ledgerPath(org, "/entries"), seller), http.StatusNotFound, ledger.CodeBookNotFound)
}
