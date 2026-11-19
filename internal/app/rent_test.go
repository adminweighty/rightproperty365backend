package app

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/ledger/ledgertest"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/httpx"
)

func bookPath(book, rest string) string { return "/v1/agent/organisations/" + book + rest }

func duesStatuses(items []tenancies.DueView, n int) string {
	var out []string
	for _, d := range items[:n] {
		out = append(out, d.Status)
	}
	return strings.Join(out, ",")
}

// The rent seed over HTTP (e.now is 2026-11-01): schedules, receipts,
// arrears, statements and the ledger's receivables.
func TestRentSeedOverHTTP(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	cottage, flat1 := devseed.TenancyCottageID.String(), devseed.TenancyFlat1ID.String()

	rec := e.get(tenanciesPath(org, "/"+cottage+"/dues"), staff)
	wantStatus(t, rec, http.StatusOK)
	if rec.Header().Get("Cache-Control") != "private, no-store" || rec.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("headers: %v", rec.Header())
	}
	if err := ledgertest.PerCurrencyProblem(rec.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	dues := decode[tenancies.DuesPage](t, rec)
	if st := duesStatuses(dues.Items, 5); st != "PAID,PAID,DUE,PART_PAID,DUE" {
		t.Fatalf("cottage statuses: %s", st)
	}
	if d := dues.Items[2]; !d.Overdue || d.DaysOverdue != 45 || d.Outstanding != "700.00" || d.Can.Waive {
		t.Fatalf("the 45-day overdue due: %+v", d)
	}
	if d := dues.Items[3]; d.Paid != "300.00" || d.Outstanding != "400.00" || d.DaysOverdue < 14 || d.DaysOverdue > 17 {
		t.Fatalf("the part-paid due: %+v", d)
	}
	if s := dues.Summary; s.Overdue != "1100.00" || s.OverdueCount != 2 || s.Paid != "1700.00" || dues.Schedule == nil || !dues.Schedule.Rolling {
		t.Fatalf("cottage summary: %+v %+v", s, dues.Schedule)
	}
	if f1 := decode[tenancies.DuesPage](t, e.get(tenanciesPath(org, "/"+flat1+"/dues"), staff)); duesStatuses(f1.Items, 3) != "PAID,PAID,DUE" ||
		f1.Items[2].Overdue || f1.Schedule.Rolling || f1.Schedule.Through != "2027-08-31" {
		t.Fatalf("flat 1: %+v %+v", f1.Items[:3], f1.Schedule)
	}
	recs := decode[struct {
		Items []tenancies.ReceiptView `json:"items"`
	}](t, e.get(tenanciesPath(org, "/"+cottage+"/receipts"), staff)).Items
	if len(recs) != 3 || recs[0].Amount != "300.00" || recs[0].LedgerStatus != ledger.StatusUnverified || recs[0].Can.Reverse || recs[0].Tenant == nil ||
		recs[0].Tenant.Name != "Tariro Moyo" {
		t.Fatalf("cottage receipts: %+v", recs)
	}
	wantStatus(t, e.get(tenanciesPath(org, "/"+cottage+"/receipts/"+recs[0].ID.String()), staff), http.StatusOK)
	wantError(t, e.get(tenanciesPath(org, "/"+flat1+"/receipts/"+recs[0].ID.String()), staff), http.StatusNotFound, tenancies.CodeReceiptNotFound)

	// Arrears: the cottage's two dues, 45 and 15 days overdue.
	rec = e.get(bookPath(org.String(), "/arrears"), staff)
	wantStatus(t, rec, http.StatusOK)
	if err := ledgertest.PerCurrencyProblem(rec.Body.Bytes()); err != nil {
		t.Fatal(err)
	}
	arr := decode[tenancies.Arrears](t, rec)
	if arr.AsOf != "2026-11-01" || len(arr.Currencies) != 1 || arr.Currencies[0].Outstanding != "1100.00" || len(arr.Currencies[0].Tenancies) != 1 ||
		arr.Currencies[0].Tenancies[0].DaysOverdue != 45 || arr.Currencies[0].Buckets[0].Outstanding != "400.00" ||
		arr.Currencies[0].Buckets[1].Outstanding != "700.00" || arr.Currencies[0].Tenancies[0].Tenancy.Property.Name != "Borrowdale Cottage" {
		t.Fatalf("arrears: %+v", arr)
	}
	wantError(t, e.get(bookPath(org.String(), "/arrears?as_of=yesterday"), staff), http.StatusBadRequest, httpx.CodeValidationFailed)

	// The statement: JSON, CSV and PDF.
	rec = e.get(tenanciesPath(org, "/"+cottage+"/statement"), staff)
	wantStatus(t, rec, http.StatusOK)
	st := decode[tenancies.Statement](t, rec)
	if len(st.Currencies) != 1 || st.Currencies[0].ClosingBalance != "1100.00" || st.Currencies[0].Charged != "2800.00" || st.Tenancy.ManagedBy.Name != "Sample Estates" {
		t.Fatalf("statement: %+v", st)
	}
	csv := e.get(tenanciesPath(org, "/"+cottage+"/statement?format=csv&from=2026-09-01"), staff)
	wantStatus(t, csv, http.StatusOK)
	if csv.Header().Get("Content-Type") != "text/csv; charset=utf-8" || !strings.Contains(csv.Header().Get("Content-Disposition"), "statement-") ||
		csv.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(csv.Body.String(), "USD,OPENING,2026-09-01,,Opening balance,,,,0.00") {
		t.Fatalf("csv: %v %s", csv.Header(), csv.Body)
	}
	pdf := e.get(tenanciesPath(org, "/"+cottage+"/statement?format=pdf"), staff)
	wantStatus(t, pdf, http.StatusOK)
	if pdf.Header().Get("Content-Type") != "application/pdf" || !bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF-")) || pdf.Header().Get("X-Robots-Tag") != "noindex" {
		t.Fatalf("pdf: %v", pdf.Header())
	}
	wantError(t, e.get(tenanciesPath(org, "/"+cottage+"/statement?format=xlsx"), staff), http.StatusBadRequest, httpx.CodeValidationFailed)
}

// Recording, reversing (through the ledger) and waiving over HTTP.
func TestRentWritesOverHTTP(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID
	owner := browser(e.loginCookie(devseed.EstatesOwnerEmail, seedPassword))
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	flat1 := devseed.TenancyFlat1ID.String()
	body := obj{"amount": "950.00", "currency": "USD", "method": "BANK", "received_on": "2026-11-01", "reference": "EFT KS-0003"}
	wantError(t, e.post(tenanciesPath(org, "/"+flat1+"/receipts"), body, staff), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(tenanciesPath(org, "/"+flat1+"/receipts"), obj{"amount": "950.00", "currency": "ZAR", "method": "BANK",
		"received_on": "2026-11-01"}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post(tenanciesPath(org, "/"+flat1+"/receipts"), obj{"amount": 950, "currency": "USD"}, owner), http.StatusBadRequest,
		httpx.CodeInvalidJSON)
	rec := e.post(tenanciesPath(org, "/"+flat1+"/receipts"), body, owner)
	wantStatus(t, rec, http.StatusCreated)
	res := decode[tenancies.ReceiptResult](t, rec)
	if res.Receipt.Allocations[0].DueDate != "2026-11-01" || len(res.Dues) != 1 || res.Dues[0].Status != tenancies.DuePaid || !res.Receipt.Can.Reverse {
		t.Fatalf("receipt: %+v", res)
	}
	// The ledger shows the entry, linked to the tenancy.
	entry := decode[struct {
		Entry struct {
			Type      string  `json:"type"`
			Status    string  `json:"status"`
			TenancyID *string `json:"tenancy_id"`
			Contact   *struct {
				Name string `json:"name"`
			} `json:"contact"`
			Can struct {
				Correct bool `json:"correct"`
			} `json:"can"`
		} `json:"entry"`
	}](t, e.get(ledgerPath(org, "/entries/"+res.Receipt.LedgerEntryID.String()), owner)).Entry
	if entry.Type != "RENT_RECEIVED" || entry.Status != "USER_RECORDED_UNVERIFIED" || entry.TenancyID == nil || *entry.TenancyID != flat1 ||
		entry.Contact == nil || entry.Contact.Name != "Kudzai Sithole" || entry.Can.Correct {
		t.Fatalf("ledger entry: %+v", entry)
	}
	// A manual ledger entry may not claim a tenancy; a receipt's entry is not corrected.
	wantError(t, e.post(ledgerPath(org, "/entries"), obj{"type": "RENT_RECEIVED", "method": "CASH", "amount": "1.00", "currency": "USD",
		"effective_date": "2026-11-01", "tenancy_id": flat1}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	wantError(t, e.post(ledgerPath(org, "/entries/"+res.Receipt.LedgerEntryID.String()+"/correct"), obj{"type": "RENT_RECEIVED", "method": "CASH",
		"amount": "1.00", "currency": "USD", "effective_date": "2026-11-01", "reason": "typo"}, owner), http.StatusConflict, ledger.CodeRentReceipt)
	// Reversing the entry undoes the receipt.
	wantStatus(t, e.post(ledgerPath(org, "/entries/"+res.Receipt.LedgerEntryID.String()+"/reverse"), obj{"reason": "Bounced"}, owner), http.StatusCreated)
	got := decode[struct {
		Receipt tenancies.ReceiptView `json:"receipt"`
	}](t, e.get(tenanciesPath(org, "/"+flat1+"/receipts/"+res.Receipt.ID.String()), owner)).Receipt
	if got.Status != tenancies.ReceiptReversed || got.Reversal == nil || got.Can.Reverse {
		t.Fatalf("reversed: %+v", got)
	}
	dues := decode[tenancies.DuesPage](t, e.get(tenanciesPath(org, "/"+flat1+"/dues?to=2026-11-01"), owner))
	if duesStatuses(dues.Items, 3) != "PAID,PAID,DUE" {
		t.Fatalf("after the reversal: %s", duesStatuses(dues.Items, 3))
	}
	// Waive it, then a second time is 409.
	due := dues.Items[2].ID.String()
	wantError(t, e.post(bookPath(org.String(), "/dues/"+due+"/waive"), obj{"reason": "Goodwill"}, staff), http.StatusForbidden, httpx.CodeForbidden)
	wantError(t, e.post(bookPath(org.String(), "/dues/"+due+"/waive"), obj{"reason": ""}, owner), http.StatusBadRequest, httpx.CodeValidationFailed)
	rec = e.post(bookPath(org.String(), "/dues/"+due+"/waive"), obj{"reason": "Goodwill"}, owner)
	wantStatus(t, rec, http.StatusOK)
	if w := decode[struct {
		Due tenancies.DueView `json:"due"`
	}](t, rec).Due; w.Status != tenancies.DueWaived || w.Waiver == nil || w.Waiver.Reason != "Goodwill" {
		t.Fatalf("waived: %+v", w)
	}
	wantError(t, e.post(bookPath(org.String(), "/dues/"+due+"/waive"), obj{"reason": "again"}, owner), http.StatusConflict, tenancies.CodeDueNotWaivable)
	wantError(t, e.post(bookPath(org.String(), "/dues/nope/waive"), obj{"reason": "x"}, owner), http.StatusNotFound, tenancies.CodeDueNotFound)
}

// Every rent endpoint answers another organisation's member, a consumer,
// the tenant and an administrator 404 BOOK_NOT_FOUND, and never data; a
// tenant reads only their own tenancy's dues, receipts and statement.
func TestRentWrongBookAndTenantView(t *testing.T) {
	e := newEnv(t)
	org := devseed.SampleEstates.ID.String()
	flat1, cottage := devseed.TenancyFlat1ID.String(), devseed.TenancyCottageID.String()
	staff := browser(e.loginCookie(devseed.AgencyStaffEmail, seedPassword))
	dueID := decode[tenancies.DuesPage](t, e.get(bookPath(org, "/tenancies/"+cottage+"/dues"), staff)).Items[2].ID.String()
	receiptID := devseed.RentCottageFirstID.String()
	blake := browser(e.loginCookie(identity.SeedAgentBEmail, seedPassword))
	consumer := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	tenant := browser(e.loginCookie(devseed.TenantEmail, seedPassword))
	admin := browser(e.adminLogin(identity.SeedAdminEmail, seedPassword))
	type call struct {
		method, path string
		body         any
	}
	receipt := obj{"amount": "1.00", "currency": "USD", "method": "CASH", "received_on": "2026-11-01"}
	calls := []call{
		{http.MethodGet, "/tenancies/" + cottage + "/dues", nil},
		{http.MethodGet, "/tenancies/" + cottage + "/dues?status=bad", nil},
		{http.MethodGet, "/tenancies/" + cottage + "/receipts", nil},
		{http.MethodGet, "/tenancies/" + cottage + "/receipts/" + receiptID, nil},
		{http.MethodPost, "/tenancies/" + cottage + "/receipts", receipt},
		{http.MethodPost, "/tenancies/" + cottage + "/receipts", obj{"amount": 1}},
		{http.MethodGet, "/tenancies/" + cottage + "/statement", nil},
		{http.MethodGet, "/tenancies/" + cottage + "/statement?format=csv", nil},
		{http.MethodGet, "/tenancies/" + cottage + "/statement?format=pdf", nil},
		{http.MethodGet, "/tenancies/not-a-uuid/dues", nil},
		{http.MethodPost, "/dues/" + dueID + "/waive", obj{"reason": "intruder"}},
		{http.MethodPost, "/dues/not-a-uuid/waive", obj{"reason": "intruder"}},
		{http.MethodGet, "/arrears", nil},
		{http.MethodGet, "/arrears?as_of=bad", nil},
		{http.MethodGet, "/ledger/reports/receivables", nil},
	}
	for who, as := range map[string]reqOpt{"another organisation": blake, "consumer": consumer, "tenant": tenant, "administrator": admin} {
		for _, c := range calls {
			rec := e.do(c.method, bookPath(org, c.path), c.body, as)
			wantError(t, rec, http.StatusNotFound, tenancies.CodeBookNotFound)
			if b := rec.Body.String(); strings.Contains(b, "Borrowdale") || strings.Contains(b, "Tariro") || strings.Contains(b, "700.00") {
				t.Fatalf("%s %s %s: %s", who, c.method, c.path, b)
			}
		}
	}
	// Blake's own book does not reach Sample Estates' tenancies, dues and receipts.
	second := devseed.SecondStreetRealty.ID.String()
	wantError(t, e.get(bookPath(second, "/tenancies/"+cottage+"/dues"), blake), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.get(bookPath(second, "/tenancies/"+cottage+"/statement?format=pdf"), blake), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.post(bookPath(second, "/tenancies/"+cottage+"/receipts"), receipt, blake), http.StatusNotFound, tenancies.CodeTenancyNotFound)
	wantError(t, e.post(bookPath(second, "/dues/"+dueID+"/waive"), obj{"reason": "x"}, blake), http.StatusNotFound, tenancies.CodeDueNotFound)
	if body := e.get(bookPath(second, "/arrears"), blake).Body.String(); !strings.Contains(body, `"currencies":[]`) {
		t.Fatalf("second street arrears: %s", body)
	}
	// Signed out: 401.
	wantError(t, e.get(bookPath(org, "/arrears")), http.StatusUnauthorized, httpx.CodeUnauthenticated)

	// The tenant: Flat 1's dues, receipts and statement; nothing of the cottage.
	rec := e.get("/v1/me/tenancies/"+flat1+"/dues", tenant)
	wantStatus(t, rec, http.StatusOK)
	mine := decode[tenancies.MyDuesPage](t, rec)
	if len(mine.Items) != 12 || mine.Items[0].Status != tenancies.DuePaid || mine.Summary.Paid != "1900.00" {
		t.Fatalf("my dues: %+v", mine.Summary)
	}
	rec2 := e.get("/v1/me/tenancies/"+flat1+"/receipts", tenant)
	wantStatus(t, rec2, http.StatusOK)
	myRecs := decode[struct {
		Items []tenancies.MyReceiptView `json:"items"`
	}](t, rec2).Items
	if len(myRecs) != 2 || myRecs[0].Reference != "EFT KS-0002" {
		t.Fatalf("my receipts: %+v", myRecs)
	}
	for _, r := range []string{rec.Body.String(), rec2.Body.String()} {
		for _, leak := range []string{`"can"`, "ledger_entry_id", "recorded_by", "Chipo Dube", devseed.ContactKudzaiID.String()} {
			if strings.Contains(r, leak) {
				t.Fatalf("the tenant view leaks %q: %s", leak, r)
			}
		}
	}
	for _, format := range []string{"json", "csv", "pdf"} {
		wantStatus(t, e.get("/v1/me/tenancies/"+flat1+"/statement?format="+format, tenant), http.StatusOK)
	}
	for _, path := range []string{"/dues", "/receipts", "/statement", "/statement?format=pdf"} {
		wantError(t, e.get("/v1/me/tenancies/"+cottage+path, tenant), http.StatusNotFound, tenancies.CodeTenancyNotFound)
		wantError(t, e.get("/v1/me/tenancies/"+flat1+path, consumer), http.StatusNotFound, tenancies.CodeTenancyNotFound)
		wantError(t, e.get("/v1/me/tenancies/nope"+path, tenant), http.StatusNotFound, tenancies.CodeTenancyNotFound)
		wantError(t, e.get("/v1/me/tenancies/"+flat1+path), http.StatusUnauthorized, httpx.CodeUnauthenticated)
	}
}
