package agreements

import (
	"bytes"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/storage"
)

type fakeMailer struct {
	mu        sync.Mutex
	signing   []SigningMail
	to        []string
	codes     map[string]string
	completed int
}

func (m *fakeMailer) SendSigning(_ context.Context, to string, s SigningMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signing, m.to = append(m.signing, s), append(m.to, to)
	return nil
}
func (m *fakeMailer) SendCode(_ context.Context, to string, c CodeMail) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.codes == nil {
		m.codes = map[string]string{}
	}
	m.codes[to] = c.Code
	return nil
}
func (m *fakeMailer) SendCompleted(context.Context, string, CompletedMail) error {
	m.completed++
	return nil
}
func (m *fakeMailer) SendDeclined(context.Context, string, DeclinedMail) error { return nil }

func testPDF(t *testing.T, pages int) []byte {
	f := fpdf.New("P", "pt", "Letter", "")
	for i := 0; i < pages; i++ {
		f.AddPage()
		f.SetFont("Helvetica", "", 12)
		f.Text(72, 72, "Page")
	}
	var buf bytes.Buffer
	if err := f.Output(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type harness struct {
	svc  *Service
	mail *fakeMailer
	now  time.Time
	p    *auth.Principal
}

func newHarness(t *testing.T) *harness {
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{mail: &fakeMailer{}, now: time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)}
	h.svc = NewService(Deps{Store: NewMemoryStore(), Files: files, Mailer: h.mail, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return h.now }, PublicWebURL: "https://example.test"})
	org := uuid.New()
	h.p = &auth.Principal{UserID: uuid.New(), Name: "Sam Sender", Email: "sender@example.test", SessionScope: auth.ScopeCustomer,
		Memberships: []auth.Membership{{OrganisationID: org, OrganisationName: "Org", OrganisationKind: auth.KindAgency, Role: auth.RoleOwner}}}
	return h
}

func (h *harness) sent(t *testing.T, reminder string) View {
	ctx := context.Background()
	name, freq := "Contract", reminder
	v, err := h.svc.Create(ctx, h.p, Input{Name: &name, ReminderFrequency: &freq, Recipients: &[]RecipientInput{
		{Name: "A", Email: "a@example.test", Role: RoleSigner, Order: 1}, {Name: "B", Email: "b@example.test", Role: RoleSigner, Order: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	id := v.ID
	if v, err = h.svc.AddDocument(ctx, h.p, id, "c.pdf", testPDF(t, 2)); err != nil {
		t.Fatal(err)
	}
	var fields []FieldInput
	for _, r := range v.Recipients {
		fields = append(fields, FieldInput{RecipientID: r.ID.String(), Type: FieldSignature, Page: 2, X: 72, Y: 600, W: 200, H: 40})
	}
	if _, err := h.svc.PutFields(ctx, h.p, id, fields); err != nil {
		t.Fatal(err)
	}
	if v, err = h.svc.Send(ctx, h.p, id); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInspectPDF(t *testing.T) {
	pages, err := InspectPDF(testPDF(t, 3))
	if err != nil || len(pages) != 3 || pages[0].Width != 612 || pages[0].Height != 792 {
		t.Fatalf("pages %v %v", pages, err)
	}
	for _, bad := range [][]byte{[]byte("hello"), []byte("%PDF-1.7\n%%EOF"), append([]byte("%PDF-"), make([]byte, 100)...)} {
		if _, err := InspectPDF(bad); err == nil {
			t.Fatalf("%q must be refused", bad[:5])
		}
	}
	var verr *ValidationError
	if _, err := InspectPDF(testPDF(t, MaxPDFPages+1)); !errors.As(err, &verr) {
		t.Fatalf("too many pages: %v", err)
	}
}

// Reminders are sent once per interval and recipient however often the
// sweep runs; an expired agreement's links stop.
func TestSweepRemindersAndExpiry(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.sent(t, ReminderDaily)
	if len(h.mail.signing) != 2 {
		t.Fatalf("%d signing emails, want 2 (parallel)", len(h.mail.signing))
	}
	if _, reminded, _ := h.svc.Sweep(ctx); reminded != 0 {
		t.Fatal("no reminder before a day")
	}
	h.now = h.now.Add(25 * time.Hour)
	for range 3 {
		if _, _, err := h.svc.Sweep(ctx); err != nil {
			t.Fatal(err)
		}
	}
	reminders := 0
	for _, s := range h.mail.signing {
		if s.Reminder {
			reminders++
		}
	}
	if reminders != 2 {
		t.Fatalf("%d reminders, want one per recipient", reminders)
	}
	token := h.mail.signing[0].Token
	if _, err := h.svc.Signing(ctx, token, "", ""); err != nil {
		t.Fatal(err)
	}
	h.now = v.ExpiresAt.Add(time.Minute)
	expired, _, err := h.svc.Sweep(ctx)
	if err != nil || expired != 1 {
		t.Fatalf("expired %d %v", expired, err)
	}
	if _, err := h.svc.Signing(ctx, token, "", ""); !errors.Is(err, ErrLinkExpired) {
		t.Fatalf("an expired agreement's link: %v", err)
	}
	got, _ := h.svc.Get(ctx, h.p, v.ID)
	if got.Status != StatusExpired {
		t.Fatalf("status %s", got.Status)
	}
}

// Completion draws the values on the pages and appends the audit report.
func TestCompletionRendersFinalDocument(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	v := h.sent(t, ReminderNone)
	for i, s := range h.mail.signing {
		if _, err := h.svc.SendCode(ctx, s.Token, "192.0.2.7", "UA"); err != nil {
			t.Fatal(err)
		}
		res, err := h.svc.Submit(ctx, s.Token, h.mail.codes[h.mail.to[i]], SubmitInput{Consent: true,
			Signature: &AdoptInput{Kind: AdoptTyped, Text: "Zoë Müller", Font: "BOLD"}}, "192.0.2.7", "UA")
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && res.AgreementStatus != StatusCompleted {
			t.Fatalf("not completed: %+v", res)
		}
	}
	file, err := h.svc.PDF(ctx, h.p, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := io.ReadAll(file.Content)
	pages, err := InspectPDF(content)
	if err != nil || len(pages) != 3 {
		t.Fatalf("final document: %d pages, %v (two pages and the audit report)", len(pages), err)
	}
	got, _ := h.svc.Get(ctx, h.p, v.ID)
	if got.FinalDocument == nil || got.FinalDocument.SHA256 != Checksum(content) || h.mail.completed != 3 {
		t.Fatalf("final %+v, %d completion emails", got.FinalDocument, h.mail.completed)
	}
	verified, err := h.svc.VerifyFile(ctx, content)
	if err != nil || verified.Match == nil || !*verified.Match {
		t.Fatalf("verify %+v %v", verified, err)
	}
}

// pageTexts returns the decompressed content stream of every page, in order.
func pageTexts(t *testing.T, content []byte) []string {
	t.Helper()
	objects := map[string][]byte{}
	for _, m := range regexp.MustCompile(`(?s)(\d+) 0 obj(.*?)endobj`).FindAllSubmatch(content, -1) {
		objects[string(m[1])] = m[2]
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?s)\d+ 0 obj\s*<<[^>]*?/Type /Page\b.*?endobj`).FindAll(content, -1) {
		ref := regexp.MustCompile(`/Contents (\d+) 0 R`).FindSubmatch(m)
		if ref == nil {
			t.Fatal("a page without contents")
		}
		obj := objects[string(ref[1])]
		s := regexp.MustCompile(`(?s)stream\r?\n(.*?)\r?\nendstream`).FindSubmatch(obj)
		if s == nil {
			t.Fatal("contents without a stream")
		}
		r, err := zlib.NewReader(bytes.NewReader(s[1]))
		if err != nil {
			out = append(out, string(s[1]))
			continue
		}
		raw, _ := io.ReadAll(r)
		out = append(out, string(raw))
	}
	return out
}

// The audit report's footer is on its own pages only, never on the
// original pages (including the last one), whatever their number.
func TestAuditFooterOnlyOnAuditPages(t *testing.T) {
	for _, n := range []int{1, 2, 3} {
		original := testPDF(t, n)
		pages, err := InspectPDF(original)
		if err != nil {
			t.Fatal(err)
		}
		content, err := RenderFinal(FinalInput{Original: original, Pages: pages, Recipients: map[string]Recipient{},
			Audit: AuditReport{Name: "Contract", Reference: "RPA-2026-000002", CompletedAt: time.Now(), Pages: n}})
		if err != nil {
			t.Fatal(err)
		}
		texts := pageTexts(t, content)
		if len(texts) != n+1 {
			t.Fatalf("%d pages: %d page streams, want %d", n, len(texts), n+1)
		}
		for i, text := range texts {
			has := strings.Contains(text, "(Audit report  RPA-2026-000002  page ")
			if i < n && has {
				t.Fatalf("%d-page document: the audit footer is drawn on original page %d", n, i+1)
			}
			if i >= n && !strings.Contains(text, "(Audit report  RPA-2026-000002  page 1)Tj") {
				t.Fatalf("%d-page document: the audit page lacks its footer (page 1 of the report)", n)
			}
		}
	}
}
