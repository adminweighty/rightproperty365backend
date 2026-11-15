package mail

import (
	"bytes"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"
	// The zone database is embedded so that market time zones resolve on a
	// machine without one (Windows, minimal containers).
	_ "time/tzdata"
	"unicode/utf8"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

// Logo shown in the indigo header band: the reverse (white + turquoise)
// lockup, hosted by the web app. The source PNG is 1600 x 310; it is displayed
// at 160 x 31 so it stays sharp on high-density screens.
const (
	logoPath   = "/email/logo-on-indigo.png"
	logoWidth  = 160
	logoHeight = 31
)

// Template is the description of one email template.
type Template struct {
	// Name is the template (and Message.Kind) name, e.g. "verify-email".
	Name string
	// Title is a short description for the preview index.
	Title string
	// Triggered is false for templates that are designed and tested but not
	// sent by anything yet, because the feature behind them is not built.
	Triggered bool
}

// Templates lists every email template, triggered ones first.
func Templates() []Template {
	return []Template{
		{"verify-email", "Confirm your email address", true},
		{"reset-password", "Reset your password", true},
		{"password-changed", "Password changed (security notice)", true},
		{"welcome", "Welcome, after verification", true},
		{"admin-sign-in", "Administrator sign-in notice", true},
		{"account-exists", "Sign-up attempted with an existing address", true},
		{"sign-in-code", "Sign-in code (second step of signing in)", true},
		{"property-request-received", "Property request received (to the customer)", true},
		{"property-request-new", "New property request (to the agent or seller)", true},
		{"business-enquiry-received", "Business enquiry received (to the customer)", true},
		{"business-enquiry-new", "New business enquiry (to the profile owner)", true},
		{"onboarding-submitted", "Onboarding application received (to the applicant)", true},
		{"onboarding-approved", "Onboarding application approved (to the applicant)", true},
		{"onboarding-changes-requested", "Onboarding application needs changes (to the applicant)", true},
		{"onboarding-rejected", "Onboarding application not approved (to the applicant)", true},
		{"appointment-requested", "Appointment requested (to the professional)", true},
		{"appointment-cancelled-by-customer", "Appointment cancelled by the customer (to the professional)", true},
		{"appointment-reschedule-proposed", "Customer proposed a new time (to the professional)", true},
		{"appointment-confirmed", "Appointment confirmed (to the customer)", true},
		{"appointment-new-time-proposed", "New time proposed (to the customer)", true},
		{"appointment-cancelled", "Appointment cancelled (to the customer)", true},
		{"listing-approved", "Listing or its changes approved (to the advertiser)", true},
		{"listing-changes-requested", "Listing needs changes (to the advertiser)", true},
		{"listing-rejected", "Listing or its changes not approved (to the advertiser)", true},
		{"team-invitation", "Invitation to join an organisation's team (to the invited address)", true},
		{"property-request-assigned", "Property request assigned (to the member it was given to)", true},
		{"agreement-signing-request", "A document to sign or approve (to a recipient, with their link)", true},
		{"agreement-reminder", "Reminder of a document waiting (to a recipient, with a new link)", true},
		{"agreement-signing-code", "Code to confirm the email address before signing (to a recipient)", true},
		{"agreement-completed", "Everybody signed (to every recipient, the CCs and the sender)", true},
		{"agreement-declined", "A recipient declined (to the sender)", true},

		{"request-update", "Reply or status update to the customer", false},
		{"listing-expiring", "Listing expiring in 14, 7 or 1 days", false},
		{"listing-expired", "Listing expired", false},
		{"listing-relisted", "Relist confirmed", false},
		{"renewal-invoice", "Renewal invoice issued", false},
		{"payment-received", "Payment received", false},
		{"account-deletion-requested", "Account deletion requested", false},
		{"account-deletion-completed", "Account deletion completed", false},
	}
}

// Role is one account role shown in the welcome email.
type Role struct {
	// Code is the role code; Name is its label.
	Code    string
	Name    string
	Pending bool
}

// Data is what a template can show. Every value is treated as plain text:
// html/template escapes it, so nothing here can inject markup. Callers set
// only the fields their template uses.
type Data struct {
	// Name of the person the email is addressed to ("" gives "Hello,").
	Name string
	// Path of the page the button opens, relative to the public web URL
	// (for example "/sign-in"), with Query appended. Never an absolute URL:
	// links are always built from the configured PUBLIC_WEB_URL.
	Path  string
	Query url.Values

	Reference   string
	RequestType string
	// Stay is set for a request to stay: the dates, nights and guests asked
	// for, already written out (see StayText).
	Stay         string
	ListingRef   string
	ListingTitle string
	ProfileName  string
	Service      string
	Preview      string
	StatusText   string
	// RoleName is the label of the account role an onboarding email is about;
	// Reason is the verification team's reason (changes requested, rejected).
	RoleName      string
	Reason        string
	Roles         []Role
	When          time.Time
	ExpiresOn     time.Time
	DueOn         time.Time
	DeletionOn    time.Time
	Days          int
	Amount        string // with the currency code, e.g. "USD 25.00"
	InvoiceNumber string

	// Appointment emails. AppointmentTime and ProposedTime are already
	// formatted in the market's time zone with the zone named (see
	// LocalDateTime); WithName is the public name of the professional or
	// business; Location is the professional's note with contact details removed.
	AppointmentType string
	AppointmentTime string
	ProposedTime    string
	WithName        string
	Location        string

	// Edit is true for a listing review email about the changes to a
	// published listing (not about the listing itself).
	Edit bool

	// Team emails: the organisation, the person who acted (the inviter, the
	// manager who assigned a request) and the invited job title.
	OrganisationName string
	ActorName        string
	JobTitle         string

	// Code is the one-time sign-in code (sign-in-code). It is a credential:
	// it is only rendered into the message and never stored.
	Code string

	// Agreement emails: the document's name and the sender's message.
	DocumentName string
	Message      string
}

// LocalDateTime writes an instant in an IANA time zone and names the zone,
// because the reader may be somewhere else: "Tuesday 17 November 2026, 10:00
// (Central Africa Time)". The zone is any country's default zone: it is
// named by season where its clocks change, and by its offset where the table
// in zones.go has no name for it. An unknown zone is shown in UTC.
func LocalDateTime(t time.Time, zone string) string {
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" {
		loc, zone = time.UTC, "UTC"
	}
	local := t.In(loc)
	return local.Format("Monday 2 January 2006, 15:04") + " (" + ZoneName(zone, local) + ")"
}

// appointmentTypeNames are the plain-language names of appointment types.
var appointmentTypeNames = map[string]string{"VIEWING": "Property viewing", "CONSULTATION": "Consultation"}

// AppointmentTypeName returns the plain-language name of an appointment type.
func AppointmentTypeName(code string) string {
	if name, ok := appointmentTypeNames[code]; ok {
		return name
	}
	return strings.ToLower(strings.ReplaceAll(code, "_", " "))
}

// view is Data plus the values the layout needs.
type view struct {
	Data
	PublicURL  string
	ActionURL  string
	LogoURL    string
	LogoWidth  int
	LogoHeight int
}

// HasPendingRoles reports whether any chosen role still awaits verification.
func (v view) HasPendingRoles() bool {
	for _, r := range v.Roles {
		if r.Pending {
			return true
		}
	}
	return false
}

// Renderer renders emails. It is safe for concurrent use.
type Renderer struct {
	publicURL string
	html      map[string]*htmltemplate.Template
	text      map[string]*texttemplate.Template
}

func formatDate(t time.Time) string { return t.UTC().Format("2 January 2006") }

// formatDateTime always names the time zone, because the reader may be in another one.
func formatDateTime(t time.Time) string { return t.UTC().Format("2 January 2006, 15:04") + " UTC" }

// NewRenderer parses every template. publicWebURL is the base of every link
// and image (PUBLIC_WEB_URL), e.g. "https://rightproperty365.com".
func NewRenderer(publicWebURL string) (*Renderer, error) {
	base := strings.TrimRight(strings.TrimSpace(publicWebURL), "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("public web URL must be an http(s) URL")
	}
	r := &Renderer{publicURL: base, html: map[string]*htmltemplate.Template{}, text: map[string]*texttemplate.Template{}}

	htmlFuncs := htmltemplate.FuncMap{"date": formatDate, "dateTime": formatDateTime}
	textFuncs := texttemplate.FuncMap{"date": formatDate, "dateTime": formatDateTime}
	for _, t := range Templates() {
		h, err := htmltemplate.New("layout.html.tmpl").Funcs(htmlFuncs).ParseFS(templateFS, "templates/layout.html.tmpl", "templates/"+t.Name+".html.tmpl")
		if err != nil {
			return nil, fmt.Errorf("parse %s html template: %w", t.Name, err)
		}
		x, err := texttemplate.New("layout.txt.tmpl").Funcs(textFuncs).ParseFS(templateFS, "templates/layout.txt.tmpl", "templates/"+t.Name+".txt.tmpl")
		if err != nil {
			return nil, fmt.Errorf("parse %s text template: %w", t.Name, err)
		}
		r.html[t.Name], r.text[t.Name] = h, x
	}
	return r, nil
}

// PublicURL returns the base URL of every link.
func (r *Renderer) PublicURL() string { return r.publicURL }

// Link builds an absolute link from a site-relative path and query. The host
// always comes from the configured public web URL, never from input.
func (r *Renderer) Link(path string, query url.Values) string {
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	link := r.publicURL + path
	if len(query) > 0 {
		link += "?" + query.Encode()
	}
	return link
}

var blankLines = regexp.MustCompile(`\n{3,}`)

// Render produces the message for template name addressed to `to`. The
// result has both an HTML part and a plain-text alternative.
func (r *Renderer) Render(name, to string, d Data) (Message, error) {
	h, ok := r.html[name]
	if !ok {
		return Message{}, fmt.Errorf("unknown email template %q", name)
	}
	path := d.Path
	if path == "" {
		path = "/"
	}
	v := view{
		Data: d, PublicURL: r.publicURL, ActionURL: r.Link(path, d.Query),
		LogoURL: r.publicURL + logoPath, LogoWidth: logoWidth, LogoHeight: logoHeight,
	}

	var htmlBuf, textBuf, subjectBuf bytes.Buffer
	if err := h.ExecuteTemplate(&htmlBuf, "layout.html.tmpl", v); err != nil {
		return Message{}, fmt.Errorf("render %s html: %w", name, err)
	}
	x := r.text[name]
	if err := x.ExecuteTemplate(&textBuf, "layout.txt.tmpl", v); err != nil {
		return Message{}, fmt.Errorf("render %s text: %w", name, err)
	}
	if err := x.ExecuteTemplate(&subjectBuf, "subject", v); err != nil {
		return Message{}, fmt.Errorf("render %s subject: %w", name, err)
	}
	// A subject is a single header line: no line breaks can come from data.
	subject := strings.Join(strings.Fields(subjectBuf.String()), " ")
	text := strings.TrimSpace(blankLines.ReplaceAllString(strings.ReplaceAll(textBuf.String(), "\r\n", "\n"), "\n\n")) + "\n"

	return Message{To: to, Subject: subject, HTML: htmlBuf.String(), Text: text, Kind: name, DevLink: v.ActionURL, DevCode: d.Code}, nil
}

var (
	previewEmailRe = regexp.MustCompile(`[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>"]+`)
	// A run of 7 or more digits, allowing the separators people type in phone numbers.
	previewPhoneRe = regexp.MustCompile(`\+?\d(?:[\s().-]?\d){6,}`)
	previewLinkRe  = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)
)

// SafePreview shortens a customer's message for a notification email. It
// collapses whitespace, hides anything that looks like an email address, a
// phone number or a link, and cuts the result to max characters. The full
// message stays in the recipient's inbox; an email is the wrong place for
// contact details (it is forwarded, indexed and kept).
func SafePreview(message string, max int) string {
	s := strings.Join(strings.Fields(message), " ")
	s = previewLinkRe.ReplaceAllString(s, "[link hidden]")
	s = previewEmailRe.ReplaceAllString(s, "[email hidden]")
	s = previewPhoneRe.ReplaceAllString(s, "[number hidden]")
	if utf8.RuneCountInString(s) > max {
		runes := []rune(s)
		s = strings.TrimSpace(string(runes[:max])) + "…"
	}
	return s
}

// roleNames are the plain-language names of the account roles. They are the
// reference-data labels (account_roles) and only a fallback: the notification
// handlers read the current labels from the reference data when sending.
var roleNames = map[string]string{
	"CONSUMER":         "Prospective Buyer",
	"TENANT":           "Prospective Tenant",
	"AGENT":            "Estate Agent",
	"SALES_AGENT":      "Sales Agent",
	"LETTINGS_AGENT":   "Lettings Agent",
	"PROPERTY_MANAGER": "Property Manager",
	"SOLICITOR":        "Solicitor",
	"AGENCY_OWNER":     "Agency Principal",
	"AGENCY_ADMIN":     "Agency Administrator",
	"AGENCY_STAFF":     "Agency Staff",
	"LANDLORD":         "Landlord",
	"PRIVATE_SELLER":   "Property Seller (Vendor)",
	"HOST":             "Host",
	"DEVELOPER":        "Property Developer",
	"SERVICE_PROVIDER": "Property Service Provider",
	// Platform roles are never selectable; these are their display names.
	"SUPER_ADMIN":    "Super Administrator",
	"PLATFORM_ADMIN": "Platform Administrator",
	"MARKET_ADMIN":   "Market Administrator",
	"FINANCE_ADMIN":  "Finance Administrator",
	"CONTENT_ADMIN":  "Content Administrator",
	"MODERATOR":      "Moderator",
	"SUPPORT":        "Support",
}

// RoleName returns the plain-language name of an account role code.
func RoleName(code string) string {
	if name, ok := roleNames[code]; ok {
		return name
	}
	return strings.ToLower(strings.ReplaceAll(code, "_", " "))
}

// requestTypeNames are the plain-language names of property request types.
var requestTypeNames = map[string]string{
	"ENQUIRY":              "General enquiry",
	"PROPERTY_INFORMATION": "More information",
	"VIEWING_REQUEST":      "Viewing request",
	"CONSULTATION_REQUEST": "Consultation request",
	"STAY_REQUEST":         "Request to stay",
}

// StayText writes the dates, nights and guests of a request to stay:
// "Saturday 5 December 2026 to Tuesday 8 December 2026 (3 nights, 2 guests)".
// The dates are calendar dates (YYYY-MM-DD) in the listing's time zone; a
// date that does not parse is shown as it is.
func StayText(checkIn, checkOut string, nights, guests int) string {
	day := func(raw string) string {
		t, err := time.Parse("2006-01-02", raw)
		if err != nil {
			return raw
		}
		return t.Format("Monday 2 January 2006")
	}
	plural := func(n int, word string) string {
		if n == 1 {
			return "1 " + word
		}
		return strconv.Itoa(n) + " " + word + "s"
	}
	return day(checkIn) + " to " + day(checkOut) + " (" + plural(nights, "night") + ", " + plural(guests, "guest") + ")"
}

// RequestTypeName returns the plain-language name of a request type code.
func RequestTypeName(code string) string {
	if name, ok := requestTypeNames[code]; ok {
		return name
	}
	return strings.ToLower(strings.ReplaceAll(code, "_", " "))
}

// TemplateNames returns the template names in a stable order.
func TemplateNames() []string {
	names := make([]string, 0, len(Templates()))
	for _, t := range Templates() {
		names = append(names, t.Name)
	}
	sort.Strings(names)
	return names
}
