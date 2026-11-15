package mail

import (
	"flag"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

const testPublicURL = "https://rightproperty365.com"

func testRenderer(t *testing.T) *Renderer {
	t.Helper()
	r, err := NewRenderer(testPublicURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Golden files: the exact HTML and text of every template with fixed sample
// data. Regenerate deliberately with: go test ./internal/platform/mail -update
func TestTemplatesMatchGoldenFiles(t *testing.T) {
	r := testRenderer(t)
	for _, tmpl := range Templates() {
		t.Run(tmpl.Name, func(t *testing.T) {
			msg, err := r.Render(tmpl.Name, "preview@example.test", SampleData(tmpl.Name))
			if err != nil {
				t.Fatal(err)
			}
			for ext, got := range map[string]string{".html": msg.HTML, ".txt": "Subject: " + msg.Subject + "\n\n" + msg.Text} {
				path := filepath.Join("testdata", "golden", tmpl.Name+ext)
				if *update {
					if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("missing golden file (run with -update): %v", err)
				}
				if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
					t.Fatalf("%s differs from %s; if the change is intended, run: go test ./internal/platform/mail -update", tmpl.Name+ext, path)
				}
			}
		})
	}
}

// Rules every email must meet, whatever its content.
func TestEveryTemplateMeetsTheHouseRules(t *testing.T) {
	r := testRenderer(t)
	if len(Templates()) != 39 {
		t.Fatalf("%d templates, want 39", len(Templates()))
	}
	hrefRe := regexp.MustCompile(`(?:href|src)="([^"]*)"`)
	bannedWords := regexp.MustCompile(`(?i)\b(delivered|amazing|simply|oops|awesome)\b`)

	for _, tmpl := range Templates() {
		t.Run(tmpl.Name, func(t *testing.T) {
			msg, err := r.Render(tmpl.Name, "preview@example.test", SampleData(tmpl.Name))
			if err != nil {
				t.Fatal(err)
			}
			if msg.Kind != tmpl.Name || msg.To != "preview@example.test" {
				t.Fatalf("kind/to = %q/%q", msg.Kind, msg.To)
			}
			if msg.Subject == "" || strings.ContainsAny(msg.Subject, "\r\n") || len(msg.Subject) > 90 {
				t.Fatalf("subject = %q", msg.Subject)
			}

			html, text := msg.HTML, msg.Text
			for _, must := range []string{
				`<html lang="en"`, `name="color-scheme" content="light dark"`, `prefers-color-scheme: dark`,
				`role="presentation"`, `width="100%" cellpadding="0" cellspacing="0" border="0" style="width:100%;max-width:600px;`, `#1900ac`, `#21ddba`, `linear-gradient(90deg,#1900ac 0%,#21ddba 100%)`,
				`src="` + testPublicURL + `/email/logo-on-indigo.png"`, `width="160"`, `height="31"`, `alt="RightProperty365"`,
				`display:none`, "Sent by RightProperty365.",
			} {
				if !strings.Contains(html, must) {
					t.Fatalf("HTML lacks %q", must)
				}
			}
			for _, banned := range []string{"<script", "javascript:", "<link ", "@import", "<iframe", "<form", "onclick=", "righty" + "property", "open.gif", "track"} {
				if strings.Contains(strings.ToLower(html), banned) {
					t.Fatalf("HTML contains %q", banned)
				}
			}
			// Fluid layout: nothing has a fixed pixel width wider than a phone, so
			// there is no horizontal scroll from 320px to 800px. (The only fixed
			// width is the 600px ghost table that Outlook alone reads.)
			if regexp.MustCompile(`[^-]width:600px`).MatchString(html) || regexp.MustCompile(`<table[^>]*\swidth="600"[^>]*class=`).MatchString(html) {
				t.Fatal("the container must be fluid (width:100%; max-width:600px), not a fixed 600px")
			}
			// Exactly one button, at least 44px tall: 14 + 20 + 14 padding and line height.
			if n := strings.Count(html, `class="btn"`); n != 1 {
				t.Fatalf("%d buttons, want exactly one", n)
			}
			if !strings.Contains(html, "padding:14px 28px") || !strings.Contains(html, "line-height:20px") {
				t.Fatal("the button is not at least 44px tall")
			}
			// Exactly one image (the logo): no tracking pixel.
			if n := strings.Count(html, "<img"); n != 1 {
				t.Fatalf("%d images, want only the logo", n)
			}
			// Every link and image is on the configured public web URL.
			for _, m := range hrefRe.FindAllStringSubmatch(html, -1) {
				if !strings.HasPrefix(m[1], testPublicURL) {
					t.Fatalf("link or image outside PUBLIC_WEB_URL: %q", m[1])
				}
			}
			if !strings.Contains(text, testPublicURL) || strings.Contains(text, "<") || strings.Contains(text, "&amp;") {
				t.Fatalf("the text part must be plain text with the link:\n%s", text)
			}
			if !strings.Contains(text, "Sent by RightProperty365.") || strings.Contains(text, "\n\n\n") {
				t.Fatalf("unexpected text part:\n%s", text)
			}
			// House voice: no hype, no exclamation marks, never "delivered".
			for where, s := range map[string]string{"subject": msg.Subject, "text": text} {
				if strings.Contains(s, "!") || bannedWords.MatchString(s) {
					t.Fatalf("%s breaks the house voice: %q", where, s)
				}
			}
			// The button label appears in both parts and the link follows it in the text part.
			if !strings.Contains(text, msg.DevLink) || !strings.Contains(html, strings.ReplaceAll(msg.DevLink, "&", "&amp;")) {
				t.Fatalf("the action link %q is missing from one of the parts", msg.DevLink)
			}
		})
	}
}

// Anything that came from a person is text, never markup.
func TestTemplatesEscapeUserInput(t *testing.T) {
	r := testRenderer(t)
	const evil = `<script>alert("x")</script><img src=x onerror=alert(1)>`
	for _, name := range []string{"property-request-new", "business-enquiry-new", "welcome", "verify-email", "listing-expiring"} {
		d := SampleData(name)
		d.Name, d.Preview, d.ProfileName, d.Service, d.Reference, d.ListingTitle = evil, evil, evil, evil, evil, evil
		d.Roles = []Role{{Name: evil, Pending: true}}
		msg, err := r.Render(name, "a@example.com", d)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(msg.HTML, "<script>") || strings.Contains(msg.HTML, "<img src=x") || strings.Contains(msg.HTML, "onerror=alert(1)>") {
			t.Fatalf("%s: user input was rendered as markup", name)
		}
		if !strings.Contains(msg.HTML, "&lt;script&gt;alert(&#34;x&#34;)&lt;/script&gt;") {
			t.Fatalf("%s: the name should be shown as escaped text", name)
		}
		// The plain-text part shows it literally, which is harmless there.
		if !strings.Contains(msg.Text, evil) {
			t.Fatalf("%s: the text part should show the literal input", name)
		}
		if strings.ContainsAny(msg.Subject, "\r\n") {
			t.Fatalf("%s: subject has a line break", name)
		}
	}

	// A line break in data cannot split the subject header.
	d := SampleData("property-request-received")
	d.Reference = "RP-1\r\nBcc: someone@example.com"
	msg, err := r.Render("property-request-received", "a@example.com", d)
	if err != nil || strings.ContainsAny(msg.Subject, "\r\n") {
		t.Fatalf("subject = %q, %v", msg.Subject, err)
	}
}

func TestLinksComeOnlyFromThePublicURL(t *testing.T) {
	r := testRenderer(t)
	if got := r.Link("/verify-email", url.Values{"token": {"abc_DEF-1"}}); got != testPublicURL+"/verify-email?token=abc_DEF-1" {
		t.Fatalf("link = %s", got)
	}
	if got := r.Link("sign-in", nil); got != testPublicURL+"/sign-in" {
		t.Fatalf("link = %s", got)
	}
	// A hostile "path" cannot move the link to another host.
	for _, path := range []string{"//evil.example/x", "https://evil.example/x", "/\\evil.example"} {
		d := SampleData("verify-email")
		d.Path = path
		msg, err := r.Render("verify-email", "a@example.com", d)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(msg.DevLink, testPublicURL+"/") {
			t.Fatalf("path %q produced %q", path, msg.DevLink)
		}
		u, err := url.Parse(msg.DevLink)
		if err != nil || u.Host != "rightproperty365.com" {
			t.Fatalf("path %q produced host %q", path, u.Host)
		}
	}
	msg, _ := r.Render("verify-email", "a@example.com", SampleData("verify-email"))
	if msg.DevLink != testPublicURL+"/verify-email?token=SAMPLE-TOKEN-not-a-real-credential" {
		t.Fatalf("verify link = %s", msg.DevLink)
	}
	for _, bad := range []string{"", "rightproperty365.com", "ftp://x.example", "javascript:alert(1)"} {
		if _, err := NewRenderer(bad); err == nil {
			t.Fatalf("public URL %q must be refused", bad)
		}
	}
	if _, err := r.Render("no-such-template", "a@example.com", Data{}); err == nil {
		t.Fatal("an unknown template must be an error")
	}
}

func TestWelcomeListsRolesAndWhichArePending(t *testing.T) {
	r := testRenderer(t)
	msg, err := r.Render("welcome", "a@example.com", SampleData("welcome"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"  - Prospective Buyer: active", "  - Landlord: waiting for verification", "  - Estate Agent: waiting for verification", "does not give access"} {
		if !strings.Contains(msg.Text, want) {
			t.Fatalf("text lacks %q:\n%s", want, msg.Text)
		}
	}
	d := SampleData("welcome")
	d.Roles = []Role{{Name: "Buyer or renter"}}
	msg, _ = r.Render("welcome", "a@example.com", d)
	if strings.Contains(msg.Text, "waiting for verification") || strings.Contains(msg.HTML, "does not give access") {
		t.Fatalf("no pending note is needed when nothing is pending:\n%s", msg.Text)
	}
}

func TestAppointmentTimesNameTheMarketTimeZone(t *testing.T) {
	at := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	if got := LocalDateTime(at, "Africa/Harare"); got != "Tuesday 17 November 2026, 10:00 (Central Africa Time)" {
		t.Fatalf("LocalDateTime = %q", got)
	}
	if got := LocalDateTime(at, "Nowhere/Unknown"); got != "Tuesday 17 November 2026, 08:00 (Coordinated Universal Time)" {
		t.Fatalf("an unknown zone must fall back to UTC, got %q", got)
	}
	r := testRenderer(t)
	for _, name := range []string{"appointment-requested", "appointment-confirmed", "appointment-new-time-proposed"} {
		msg, err := r.Render(name, "a@example.com", SampleData(name))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(msg.Text, "Tuesday 17 November 2026, 10:00 (Central Africa Time)") {
			t.Fatalf("%s must show the time in the market's zone and name it:\n%s", name, msg.Text)
		}
	}
}

func TestDatesAndAmountsFollowTheHouseStyle(t *testing.T) {
	r := testRenderer(t)
	msg, _ := r.Render("admin-sign-in", "a@example.com", SampleData("admin-sign-in"))
	if !strings.Contains(msg.Text, "Signed in: 14 November 2026, 09:05 UTC") {
		t.Fatalf("a time shown to a reader must name its time zone:\n%s", msg.Text)
	}
	msg, _ = r.Render("renewal-invoice", "a@example.com", SampleData("renewal-invoice"))
	if !strings.Contains(msg.Text, "Amount: USD 25.00") || !strings.Contains(msg.Text, "Due: 28 November 2026") {
		t.Fatalf("amounts carry the currency code and dates are written out:\n%s", msg.Text)
	}
	msg, _ = r.Render("listing-expiring", "a@example.com", SampleData("listing-expiring"))
	if msg.Subject != "Your listing expires in 7 days" {
		t.Fatalf("subject = %q", msg.Subject)
	}
	d := SampleData("listing-expiring")
	d.Days = 1
	msg, _ = r.Render("listing-expiring", "a@example.com", d)
	if msg.Subject != "Your listing expires in 1 day" {
		t.Fatalf("subject = %q", msg.Subject)
	}
}

func TestSafePreview(t *testing.T) {
	tests := []struct{ in, want string }{
		{"Is  this\nhouse   still available?", "Is this house still available?"},
		{"Call me on +263 77 123 4567 please", "Call me on [number hidden] please"},
		{"Phone 0771234567.", "Phone [number hidden]."},
		{"Write to Tariro.Moyo@Example.com today", "Write to [email hidden] today"},
		{"See https://evil.example/x?y=1 or www.evil.example now", "See [link hidden] or [link hidden] now"},
		{"Budget is USD 250,000 for 3 beds", "Budget is USD 250,000 for 3 beds"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := SafePreview(tt.in, 140); got != tt.want {
			t.Fatalf("SafePreview(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	long := strings.Repeat("word ", 100)
	got := SafePreview(long, 140)
	if n := len([]rune(got)); n > 141 || !strings.HasSuffix(got, "…") {
		t.Fatalf("preview is %d characters: %q", n, got)
	}
}

func TestRoleAndRequestTypeNames(t *testing.T) {
	if RoleName("AGENCY_OWNER") != "Agency Principal" || RoleName("PRIVATE_SELLER") != "Property Seller (Vendor)" || RoleName("SOLICITOR") != "Solicitor" ||
		RoleName("PLATFORM_ADMIN") != "Platform Administrator" || RoleName("SUPER_ADMIN") != "Super Administrator" || RoleName("SOMETHING_NEW") != "something new" {
		t.Fatal("role names")
	}
	if RequestTypeName("VIEWING_REQUEST") != "Viewing request" || RequestTypeName("OTHER_KIND") != "other kind" {
		t.Fatal("request type names")
	}
}
