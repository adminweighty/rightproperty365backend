package app

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/agreements"
	"rightproperty365.com/backend/internal/platform/config"
)

type agreementBody struct {
	ID         string `json:"id"`
	Reference  string `json:"reference"`
	Status     string `json:"status"`
	LeaseID    string `json:"lease_id"`
	Recipients []struct {
		ID     string `json:"id"`
		Email  string `json:"email"`
		Role   string `json:"role"`
		Order  int    `json:"order"`
		Status string `json:"status"`
	} `json:"recipients"`
	Fields []struct {
		ID          string  `json:"id"`
		RecipientID string  `json:"recipient_id"`
		Type        string  `json:"type"`
		Page        int     `json:"page"`
		X           float64 `json:"x"`
	} `json:"fields"`
	Document *struct {
		SHA256 string `json:"sha256"`
		Pages  []struct {
			Width, Height float64
		} `json:"pages"`
	} `json:"document"`
	FinalDocument *struct {
		SHA256 string `json:"sha256"`
	} `json:"final_document"`
	WaitingForYou bool `json:"waiting_for_you"`
}

func (a agreementBody) recipient(email string) string {
	for _, r := range a.Recipients {
		if r.Email == email {
			return r.ID
		}
	}
	return ""
}

type signingBody struct {
	Signing struct {
		Agreement struct {
			Status string `json:"status"`
		} `json:"agreement"`
		Recipient struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"recipient"`
		Fields []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"fields"`
		OtherRecipients []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"other_recipients"`
		CanAct   bool `json:"can_act"`
		ReadOnly bool `json:"read_only"`
	} `json:"signing"`
}

// signer calls the public signing routes from a fresh address each time
// (the routes are rate-limited per IP).
func (e *env) signer(method, path string, body any) *httptest.ResponseRecorder {
	e.seq++
	return e.do(method, path, body, withIP(fmt.Sprintf("203.0.%d.%d", 113+e.seq/250, e.seq%250+1)), withHeader("User-Agent", "TestBrowser/1.0"))
}

func (e *env) agreement(id string, as reqOpt) agreementBody {
	e.t.Helper()
	rec := e.get("/v1/me/agreements/"+id, as)
	wantStatus(e.t, rec, http.StatusOK)
	return decode[struct {
		Agreement agreementBody `json:"agreement"`
	}](e.t, rec).Agreement
}

// signWith asks for a code and submits for the recipient behind token.
func (e *env) signWith(token, email string, extra obj) *httptest.ResponseRecorder {
	e.t.Helper()
	if rec := e.signer(http.MethodPost, "/v1/signing/"+token+"/code", nil); rec.Code != http.StatusOK {
		return rec
	}
	body := obj{"code": e.mail.codeFor(email), "consent": true, "signature": obj{"kind": "TYPED", "text": "Signed Name", "font": "FLOWING"}}
	for k, v := range extra {
		body[k] = v
	}
	return e.signer(http.MethodPost, "/v1/signing/"+token+"/submit", body)
}

func (e *env) uploadPDF(path, method string, content []byte, opts ...reqOpt) *httptest.ResponseRecorder {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, _ := w.CreateFormFile("file", "contract.pdf")
	_, _ = part.Write(content)
	_ = w.Close()
	req := httptest.NewRequest(method, path, &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	e.seq++
	req.RemoteAddr = fmt.Sprintf("198.18.%d.%d:40000", e.seq/250, e.seq%250+1)
	for _, opt := range opts {
		opt(req)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func drawnPNG(t *testing.T) string {
	img := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	for x := 20; x < 280; x++ {
		img.Set(x, 50+(x%20)-10, color.NRGBA{0, 0, 120, 255})
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// A lease sent for signing: an agreement with every party as a parallel
// signer, fields placed by the renderer; code verification; completion,
// the final PDF without the watermark, the lease following, verification.
func TestLeaseSigningThroughAnAgreement(t *testing.T) {
	e := newEnv(t)
	agent := e.as("agent@example.test")
	rec := e.post("/v1/me/leases", leaseInput(sampleZW), agent)
	wantStatus(t, rec, http.StatusCreated)
	l := decode[leaseEnvelope](t, rec).Lease
	rec = e.post("/v1/me/leases/"+l.ID+"/send", obj{"message": "Please sign this week.", "reminder_frequency": "WEEKLY"}, agent)
	wantStatus(t, rec, http.StatusOK)
	var sent struct {
		Lease struct {
			Status      string `json:"status"`
			AgreementID string `json:"agreement_id"`
		} `json:"lease"`
	}
	sent = decode[struct {
		Lease struct {
			Status      string `json:"status"`
			AgreementID string `json:"agreement_id"`
		} `json:"lease"`
	}](t, rec)
	if sent.Lease.Status != "SENT_FOR_SIGNING" || sent.Lease.AgreementID == "" {
		t.Fatalf("sent %+v", sent)
	}
	wantError(t, e.patch("/v1/me/leases/"+l.ID, obj{"rent_amount": "1"}, agent), http.StatusConflict, "LEASE_NOT_EDITABLE")
	a := e.agreement(sent.Lease.AgreementID, agent)
	if a.Status != "OUT_FOR_SIGNATURE" || len(a.Recipients) != 2 || a.LeaseID != l.ID {
		t.Fatalf("agreement %+v", a)
	}
	count := map[string]int{}
	for _, f := range a.Fields {
		count[f.Type]++
	}
	pages := len(a.Document.Pages)
	if count["INITIALS"] != 2*pages || count["SIGNATURE"] != 2 || count["NAME"] != 2 || count["DATE_SIGNED"] != 2 {
		t.Fatalf("fields %v for %d pages", count, pages)
	}
	// Both signers are emailed at once (same order: any order).
	landlord, tenant := e.mail.signingFor("landlord@example.test"), e.mail.signingFor("tenant@example.test")
	if landlord == "" || tenant == "" {
		t.Fatal("both parties are emailed a link")
	}

	// The signing page.
	rec = e.signer(http.MethodGet, "/v1/signing/"+tenant, nil)
	wantStatus(t, rec, http.StatusOK)
	view := decode[signingBody](t, rec).Signing
	if !view.CanAct || len(view.Fields) != pages+3 || len(view.OtherRecipients) != 1 || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("signing view %+v", view)
	}
	if strings.Contains(e.logs.String(), tenant) {
		t.Fatal("a signing token must never reach the access log")
	}
	rec = e.signer(http.MethodGet, "/v1/signing/"+tenant+"/pdf", nil)
	if rec.Code != http.StatusOK || agreementsChecksum(rec.Body.Bytes()) != a.Document.SHA256 {
		t.Fatal("the link streams the frozen signing copy")
	}

	// A code is required; a wrong one is refused; the demo code is not
	// accepted in development unless configured.
	wantError(t, e.signer(http.MethodPost, "/v1/signing/"+tenant+"/submit", obj{"consent": true, "code": "123456",
		"signature": obj{"kind": "TYPED", "text": "Tariro Tenant"}}), http.StatusBadRequest, "CODE_EXPIRED")
	wantStatus(t, e.signer(http.MethodPost, "/v1/signing/"+tenant+"/code", nil), http.StatusOK)
	wantError(t, e.signer(http.MethodPost, "/v1/signing/"+tenant+"/code", nil), http.StatusTooManyRequests, "RATE_LIMITED")
	right := e.mail.codeFor("tenant@example.test")
	wrong := "000000"
	if right == wrong {
		wrong = "111111"
	}
	env := wantError(t, e.signer(http.MethodPost, "/v1/signing/"+tenant+"/submit", obj{"consent": true, "code": wrong,
		"signature": obj{"kind": "TYPED", "text": "Tariro Tenant"}}), http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["code"] != "That code is not right." {
		t.Fatalf("fields %v", env.Error.Fields)
	}
	// Missing consent and signature.
	env = wantError(t, e.signer(http.MethodPost, "/v1/signing/"+tenant+"/submit", obj{"code": right}), http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["consent"] == "" || env.Error.Fields["signature"] == "" {
		t.Fatalf("fields %v", env.Error.Fields)
	}
	rec = e.signer(http.MethodPost, "/v1/signing/"+tenant+"/submit", obj{"code": right, "consent": true,
		"signature": obj{"kind": "DRAWN", "image_base64": drawnPNG(t)}, "initials": obj{"kind": "TYPED", "text": "TT", "font": "BOLD"}})
	wantStatus(t, rec, http.StatusOK)
	if got := decode[agreements.SubmitResult](t, rec); got.RecipientStatus != "SIGNED" || got.AgreementStatus != "OUT_FOR_SIGNATURE" {
		t.Fatalf("after one signature %+v", got)
	}
	wantError(t, e.signWith(tenant, "tenant@example.test", nil), http.StatusConflict, "ALREADY_SIGNED")

	// The last signature completes it.
	rec = e.signWith(landlord, "landlord@example.test", nil)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[agreements.SubmitResult](t, rec); got.AgreementStatus != "COMPLETED" {
		t.Fatalf("completed? %+v", got)
	}
	a = e.agreement(sent.Lease.AgreementID, agent)
	if a.Status != "COMPLETED" || a.FinalDocument == nil {
		t.Fatalf("agreement %+v", a)
	}
	// The lease follows; its PDF is now the signed one, without the
	// watermark, with the audit report; its hash is the recorded one.
	lease := decode[leaseEnvelope](t, e.get("/v1/me/leases/"+l.ID, agent)).Lease
	if lease.Status != "SIGNED" {
		t.Fatalf("lease status %s", lease.Status)
	}
	rec = e.get("/v1/me/leases/"+l.ID+"/pdf", agent)
	final := rec.Body.Bytes()
	if agreementsChecksum(final) != a.FinalDocument.SHA256 {
		t.Fatal("the lease's signed PDF is the agreement's final document")
	}
	if strings.Contains(pdfStreams(final), "(DRAFT) Tj") {
		t.Fatal("the final document has no DRAFT watermark")
	}
	if strings.Contains(strings.ToLower(pdfStreams(final)), "legally executed") {
		t.Fatal("never 'legally executed'")
	}
	events := e.get("/v1/me/leases/"+l.ID+"/events", agent).Body.String()
	for _, action := range []string{"AGREEMENT_EMAILED", "AGREEMENT_VIEWED", "AGREEMENT_CODE_SENT", "AGREEMENT_EMAIL_VERIFIED", "AGREEMENT_SIGNED", "AGREEMENT_COMPLETED", `"SIGNED"`} {
		if !strings.Contains(events, action) {
			t.Fatalf("lease events lack %s", action)
		}
	}
	// Everybody (the parties and the sender) is told; links are read-only.
	if len(e.mail.completions) != 3 {
		t.Fatalf("%d completion emails, want 3", len(e.mail.completions))
	}
	rec = e.signer(http.MethodGet, "/v1/signing/"+tenant, nil)
	wantStatus(t, rec, http.StatusOK)
	if v := decode[signingBody](t, rec).Signing; !v.ReadOnly || v.CanAct || v.Agreement.Status != "COMPLETED" {
		t.Fatalf("read-only view %+v", v)
	}
	if agreementsChecksum(e.signer(http.MethodGet, "/v1/signing/"+tenant+"/pdf", nil).Body.Bytes()) != a.FinalDocument.SHA256 {
		t.Fatal("after completion the link streams the signed document")
	}
	// Verification.
	rec = e.uploadPDF("/v1/verify", http.MethodPost, final)
	wantStatus(t, rec, http.StatusOK)
	if v := decode[agreements.Verification](t, rec); v.Match == nil || !*v.Match || *v.Document != "FINAL" || v.Reference != a.Reference {
		t.Fatalf("verify %+v", v)
	}
	rec = e.uploadPDF("/v1/verify", http.MethodPost, append(append([]byte{}, final...), ' '))
	if v := decode[agreements.Verification](t, rec); v.Match == nil || *v.Match {
		t.Fatal("a changed file does not match")
	}
	rec = e.get("/v1/verify/" + a.Reference)
	wantStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "tenant@example.test") || !strings.Contains(rec.Body.String(), a.FinalDocument.SHA256) {
		t.Fatalf("verify by reference: %s", rec.Body)
	}
	wantError(t, e.get("/v1/verify/RPA-2026-999999"), http.StatusNotFound, "AGREEMENT_NOT_FOUND")
}

func agreementsChecksum(b []byte) string { return agreements.Checksum(b) }

// An uploaded PDF: validation, fields, sequential order, an approver and a
// CC, recipient replacement, permissions.
func TestUploadedAgreementSequentialSigning(t *testing.T) {
	e := newEnv(t)
	agent := e.as("agent@example.test")
	rec := e.post("/v1/me/agreements", obj{"name": "Garden service contract", "reminder_frequency": "DAILY", "recipients": []obj{
		{"name": "Tariro Tenant", "email": "tenant@example.test", "role": "SIGNER", "order": 1},
		{"name": "Lloyd Landlord", "email": "landlord@example.test", "role": "APPROVER", "order": 2},
		{"name": "Rudo Moyo", "email": "estates.owner@example.test", "role": "CC", "order": 3},
	}}, agent)
	wantStatus(t, rec, http.StatusCreated)
	a := decode[struct {
		Agreement agreementBody `json:"agreement"`
	}](t, rec).Agreement
	base := "/v1/me/agreements/" + a.ID
	wantError(t, e.post(base+"/send", nil, agent), http.StatusConflict, "AGREEMENT_DOCUMENT_REQUIRED")
	wantError(t, e.uploadPDF(base+"/document", http.MethodPut, []byte("not a pdf"), agent), http.StatusUnsupportedMediaType, "UNSUPPORTED_FILE_TYPE")
	wantError(t, e.uploadPDF(base+"/document", http.MethodPut, []byte("%PDF-1.4 broken"), agent), http.StatusUnsupportedMediaType, "UNSUPPORTED_FILE_TYPE")
	content, err := devseed.SamplePDF("Garden service contract")
	if err != nil {
		t.Fatal(err)
	}
	rec = e.uploadPDF(base+"/document", http.MethodPut, content, agent)
	wantStatus(t, rec, http.StatusOK)
	a = decode[struct {
		Agreement agreementBody `json:"agreement"`
	}](t, rec).Agreement
	if a.Document == nil || len(a.Document.Pages) != 1 || a.Document.Pages[0].Width != 595.28 {
		t.Fatalf("document %+v", a.Document)
	}
	signer, approver, cc := a.recipient("tenant@example.test"), a.recipient("landlord@example.test"), a.recipient("estates.owner@example.test")

	// Field validation.
	env := wantError(t, e.do(http.MethodPut, base+"/fields", obj{"fields": []obj{
		{"recipient_id": signer, "type": "SIGNATURE", "page": 2, "x": 10, "y": 10, "w": 100, "h": 30},
		{"recipient_id": cc, "type": "TEXT", "page": 1, "x": 10, "y": 10, "w": 100, "h": 30},
		{"recipient_id": approver, "type": "SIGNATURE", "page": 1, "x": 10, "y": 10, "w": 100, "h": 30},
		{"recipient_id": signer, "type": "TEXT", "page": 1, "x": 550, "y": 10, "w": 100, "h": 30},
	}}, agent), http.StatusUnprocessableEntity, "AGREEMENT_FIELDS_INCOMPLETE")
	for _, key := range []string{"fields[0].page", "fields[1].recipient_id", "fields[2].type", "fields[3]"} {
		if env.Error.Fields[key] == "" {
			t.Fatalf("missing %s: %v", key, env.Error.Fields)
		}
	}
	wantStatus(t, e.do(http.MethodPut, base+"/fields", obj{"fields": []obj{
		{"recipient_id": signer, "type": "SIGNATURE", "page": 1, "x": 56, "y": 520, "w": 220, "h": 40},
		{"recipient_id": signer, "type": "DATE_SIGNED", "page": 1, "x": 56, "y": 590, "w": 220, "h": 40},
		{"recipient_id": signer, "type": "CHECKBOX", "page": 1, "x": 300, "y": 520, "w": 20, "h": 20, "required": true, "label": "I accept"},
		{"recipient_id": approver, "type": "TEXT", "page": 1, "x": 56, "y": 660, "w": 220, "h": 40, "required": false, "label": "Comment"},
	}}, agent), http.StatusOK)

	// Permissions.
	wantError(t, e.get(base, e.as("estates.agent2@example.test")), http.StatusNotFound, "AGREEMENT_NOT_FOUND")
	wantStatus(t, e.get(base, e.as("estates.owner@example.test")), http.StatusOK)
	wantError(t, e.get("/v1/me/agreements", e.as("agency.staff@example.test")), http.StatusForbidden, "FORBIDDEN")
	wantError(t, e.get("/v1/me/agreements", e.as("tenant@example.test")), http.StatusForbidden, "FORBIDDEN")
	wantStatus(t, e.get("/v1/admin/agreements?status=DRAFT", e.asAdmin(devseed.SupportEmail)), http.StatusOK)
	wantError(t, e.get("/v1/admin/agreements", e.asAdmin(devseed.ModeratorEmail)), http.StatusForbidden, "FORBIDDEN")

	// Sequential: only order 1 is emailed; the CC is never asked to act.
	wantStatus(t, e.post(base+"/send", nil, agent), http.StatusOK)
	if e.mail.signingFor("landlord@example.test") != "" || e.mail.signingFor("estates.owner@example.test") != "" {
		t.Fatal("later orders and CCs are not emailed yet")
	}
	if !pageHas(decode[agreements.Page](t, e.get("/v1/me/agreements?view=WAITING_FOR_OTHERS", agent)), a.ID) {
		t.Fatal("the sender's 'waiting for others'")
	}
	// Replace the signer's address: the old link dies, the new one works.
	old := e.mail.signingFor("tenant@example.test")
	wantStatus(t, e.post(base+"/recipients/"+signer+"/replace", obj{"email": "tariro.new@example.test"}, agent), http.StatusOK)
	wantError(t, e.signer(http.MethodGet, "/v1/signing/"+old, nil), http.StatusGone, "SIGNING_LINK_NO_LONGER_VALID")
	token := e.mail.signingFor("tariro.new@example.test")
	// A required checkbox must be ticked.
	view := decode[signingBody](t, e.signer(http.MethodGet, "/v1/signing/"+token, nil)).Signing
	checkbox := ""
	for _, f := range view.Fields {
		if f.Type == "CHECKBOX" {
			checkbox = f.ID
		}
	}
	wantError(t, e.signWith(token, "tariro.new@example.test", nil), http.StatusBadRequest, "VALIDATION_FAILED")
	wantStatus(t, e.post(base+"/recipients/"+signer+"/remind", nil, agent), http.StatusOK)
	token = e.mail.signingFor("tariro.new@example.test")
	e.now = e.now.Add(time.Minute)
	rec = e.signWith(token, "tariro.new@example.test", obj{"field_values": []obj{{"field_id": checkbox, "value": "true"}}})
	wantStatus(t, rec, http.StatusOK)
	// Now the approver is emailed; they approve without a signature.
	approve := e.mail.signingFor("landlord@example.test")
	if approve == "" {
		t.Fatal("order 2 is emailed once order 1 finished")
	}
	wantStatus(t, e.signer(http.MethodPost, "/v1/signing/"+approve+"/code", nil), http.StatusOK)
	rec = e.signer(http.MethodPost, "/v1/signing/"+approve+"/submit", obj{"code": e.mail.codeFor("landlord@example.test"), "consent": true})
	wantStatus(t, rec, http.StatusOK)
	if got := decode[agreements.SubmitResult](t, rec); got.RecipientStatus != "APPROVED" || got.AgreementStatus != "COMPLETED" {
		t.Fatalf("approved %+v", got)
	}
	// The CC gets the completed copy, read-only.
	ccToken := ""
	for _, c := range e.mail.completions {
		if c.to == "estates.owner@example.test" {
			ccToken = c.token
		}
	}
	if ccToken == "" {
		t.Fatal("the CC gets the completed copy")
	}
	wantError(t, e.signer(http.MethodPost, "/v1/signing/"+ccToken+"/code", nil), http.StatusConflict, "NOT_OPEN_FOR_SIGNING")
	_ = cc
}

func pageHas(p agreements.Page, id string) bool {
	for _, a := range p.Items {
		if a.ID.String() == id {
			return true
		}
	}
	return false
}

// Parallel signers: one declines (the sender is told, the other can no
// longer sign); cancelling stops every link.
func TestAgreementDeclineAndCancel(t *testing.T) {
	e := newEnv(t)
	agent := e.as("agent@example.test")
	send := func() (agreementBody, string, string) {
		rec := e.post("/v1/me/leases", leaseInput(sampleZW), agent)
		wantStatus(t, rec, http.StatusCreated)
		l := decode[leaseEnvelope](t, rec).Lease
		rec = e.post("/v1/me/leases/"+l.ID+"/send", nil, agent)
		wantStatus(t, rec, http.StatusOK)
		id := decode[struct {
			Lease struct {
				AgreementID string `json:"agreement_id"`
			} `json:"lease"`
		}](t, rec).Lease.AgreementID
		return e.agreement(id, agent), l.ID, e.mail.signingFor("tenant@example.test")
	}
	a, leaseID, tenant := send()
	landlord := e.mail.signingFor("landlord@example.test")
	wantError(t, e.signer(http.MethodPost, "/v1/signing/"+tenant+"/decline", obj{"reason": ""}), http.StatusBadRequest, "VALIDATION_FAILED")
	wantStatus(t, e.signer(http.MethodPost, "/v1/signing/"+tenant+"/decline", obj{"reason": "The rent is wrong."}), http.StatusOK)
	if len(e.mail.declines) != 1 || e.mail.declines[0] != "agent@example.test" {
		t.Fatalf("declines %v", e.mail.declines)
	}
	wantError(t, e.signWith(landlord, "landlord@example.test", nil), http.StatusConflict, "NOT_OPEN_FOR_SIGNING")
	if got := e.agreement(a.ID, agent).Status; got != "DECLINED" {
		t.Fatalf("status %s", got)
	}
	if got := decode[leaseEnvelope](t, e.get("/v1/me/leases/"+leaseID, agent)).Lease.Status; got != "DECLINED" {
		t.Fatalf("lease %s", got)
	}

	a, leaseID, tenant = send()
	wantStatus(t, e.post("/v1/me/leases/"+leaseID+"/cancel", obj{"reason": "Wrong unit"}, agent), http.StatusOK)
	wantError(t, e.signer(http.MethodGet, "/v1/signing/"+tenant, nil), http.StatusGone, "SIGNING_LINK_NO_LONGER_VALID")
	if got := e.agreement(a.ID, agent).Status; got != "CANCELLED" {
		t.Fatalf("status %s", got)
	}
	if got := decode[leaseEnvelope](t, e.get("/v1/me/leases/"+leaseID, agent)).Lease.Status; got != "CANCELLED" {
		t.Fatalf("lease %s", got)
	}
	wantError(t, e.signer(http.MethodGet, "/v1/signing/not-a-token", nil), http.StatusNotFound, "SIGNING_LINK_NOT_FOUND")
}

// The demo code is accepted in staging only, as for signing in; signed-in
// recipients see their documents and open them; a saved signature.
func TestSigningRequestsAndDemoCode(t *testing.T) {
	demo := "24" + "68"
	e := newEnvWithConfig(t, func(c *config.Config) { c.Env, c.AuthDemoCode = config.EnvStaging, config.Secret(demo) })
	tenant := e.as("tenant@example.test")
	rec := e.get("/v1/me/signing-requests", tenant)
	wantStatus(t, rec, http.StatusOK)
	list := decode[struct {
		Items []agreements.SigningRequest `json:"items"`
	}](t, rec).Items
	var lease *agreements.SigningRequest
	for i := range list {
		if list[i].Reference == devseed.SampleLeaseAgreementReference {
			lease = &list[i]
		}
	}
	if len(list) != 2 || lease == nil || !lease.CanSign {
		t.Fatalf("signing requests %+v", list)
	}
	rec = e.post("/v1/me/signing-requests/"+lease.RecipientID.String()+"/link", nil, tenant)
	wantStatus(t, rec, http.StatusOK)
	link := decode[agreements.OwnLink](t, rec).Token
	wantStatus(t, e.signer(http.MethodPost, "/v1/signing/"+link+"/code", nil), http.StatusOK)
	rec = e.signer(http.MethodPost, "/v1/signing/"+link+"/submit", obj{"code": demo, "consent": true,
		"signature": obj{"kind": "UPLOADED", "image_base64": drawnPNG(t)}, "initials": obj{"kind": "TYPED", "text": "TT"}})
	wantStatus(t, rec, http.StatusOK)
	events := e.get("/v1/me/agreements/"+devseed.SampleLeaseAgreementID.String()+"/events", e.as("agent@example.test")).Body.String()
	if !strings.Contains(events, `"DEMO_CODE"`) {
		t.Fatalf("the demo code is recorded as such: %s", events)
	}
	if strings.Contains(e.logs.String(), demo+`"`) {
		t.Fatal("the demo code is never logged")
	}
	// In development without it configured, the demo value is just a wrong code.
	dev := newEnv(t)
	rec = dev.post("/v1/me/signing-requests/"+lease.RecipientID.String()+"/link", nil, dev.as("tenant@example.test"))
	link = decode[agreements.OwnLink](t, rec).Token
	wantStatus(t, dev.signer(http.MethodPost, "/v1/signing/"+link+"/code", nil), http.StatusOK)
	wantError(t, dev.signer(http.MethodPost, "/v1/signing/"+link+"/submit", obj{"code": demo, "consent": true,
		"signature": obj{"kind": "TYPED", "text": "T"}}), http.StatusBadRequest, "VALIDATION_FAILED")

	// A saved signature.
	wantStatus(t, e.do(http.MethodPut, "/v1/me/signature", obj{"signature": obj{"kind": "DRAWN", "image_base64": drawnPNG(t)},
		"initials": obj{"kind": "TYPED", "text": "TT", "font": "CLASSIC"}}, tenant), http.StatusOK)
	body := e.get("/v1/me/signature", tenant).Body.String()
	if !strings.Contains(body, `"image_base64":"`) || !strings.Contains(body, `"CLASSIC"`) {
		t.Fatalf("saved signature: %s", body)
	}
	wantError(t, e.do(http.MethodPut, "/v1/me/signature", obj{"signature": obj{"kind": "DRAWN", "image_base64": "bm90IGEgcG5n"}}, tenant),
		http.StatusBadRequest, "VALIDATION_FAILED")
	// The dashboard of the tenant counts the document still waiting.
	if !strings.Contains(e.get("/v1/me/dashboard", tenant).Body.String(), `"agreements.waiting_for_you"`) {
		t.Fatal("dashboard counter")
	}
}
