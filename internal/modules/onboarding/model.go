// Package onboarding is the verification step for professional accounts.
//
// Choosing a professional role at sign-up only records a request. This module
// holds the application behind that request: the details the applicant gives,
// the documents they attach, the decision a platform administrator takes, and
// what an approval creates (an organisation, a membership, a public directory
// profile with the verified badge).
//
// Rules that hold everywhere in this package:
//   - an application belongs to one user and is only ever shown to that user
//     and to authorised platform administrators;
//   - documents are private: they are read only through an authenticated
//     route, never from a public URL;
//   - contact phone, registration number and documents never reach a public
//     profile — with ONE exception the applicant is told about: the optional
//     LOGO document (a logo or profile photo) becomes the profile's public
//     logo when the application is approved. It is copied to its own object;
//     the uploaded document itself stays private like the others;
//   - a platform role can never be obtained here.
package onboarding

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Application statuses.
const (
	StatusDraft            = "DRAFT"
	StatusSubmitted        = "SUBMITTED"
	StatusChangesRequested = "CHANGES_REQUESTED"
	StatusApproved         = "APPROVED"
	StatusRejected         = "REJECTED"
)

// Statuses returns every status in the order the admin counts list them.
func Statuses() []string {
	return []string{StatusSubmitted, StatusChangesRequested, StatusApproved, StatusRejected, StatusDraft}
}

// Account role statuses this module sets (the values of identity's constants).
const (
	RoleStatusActive   = "ACTIVE"
	RoleStatusPending  = "PENDING_VERIFICATION"
	RoleStatusRejected = "REJECTED"
)

// Outcomes: what kind of thing an approval creates.
const (
	OutcomeOrganisation = "ORGANISATION"
	OutcomeAgencyMember = "AGENCY_MEMBER"
	OutcomeNone         = "NONE"
)

// Directory kinds with special handling.
const (
	KindAgencies         = "agencies"
	KindAgents           = "agents"
	KindServiceProviders = "service-providers"
)

// Outbox events, one per transition the applicant is told about. Payloads
// carry the application ID and a time only.
const (
	EventSubmitted        = "ONBOARDING_SUBMITTED"
	EventApproved         = "ONBOARDING_APPROVED"
	EventChangesRequested = "ONBOARDING_CHANGES_REQUESTED"
	EventRejected         = "ONBOARDING_REJECTED"

	AggregateApplication = "onboarding_application"
)

// EventTypes lists the events above.
func EventTypes() []string {
	return []string{EventSubmitted, EventApproved, EventChangesRequested, EventRejected}
}

// Limits.
const (
	MaxDocumentBytes        = 10 << 20 // 10 MB per file
	MaxDocumentsPerApp      = 10
	MaxFileNameRunes        = 100
	DocumentKeyPrefix       = "onboarding"
	ReferenceSetDocuments   = "onboarding_documents"
	maxListItems            = 12
	maxListItemRunes        = 60
	minReasonRunes          = 5
	maxReasonRunes          = 500
	maxRegistrationNumRunes = 60
)

// Error codes.
const (
	CodeNotFound          = "ONBOARDING_NOT_FOUND"
	CodeDocumentNotFound  = "DOCUMENT_NOT_FOUND"
	CodeInvalidState      = "INVALID_STATE"
	CodeAgencyNotVerified = "AGENCY_NOT_VERIFIED"
	CodeFileTooLarge      = "FILE_TOO_LARGE"
	CodeUnsupportedFile   = "UNSUPPORTED_FILE_TYPE"
	CodeTooManyDocuments  = "TOO_MANY_DOCUMENTS"
)

// DocumentLogo is the one document type that becomes PUBLIC: an optional
// logo (for a business) or profile photo (for an agent). Unlike the other
// documents it must be a PNG or JPEG image within directory's logo limits.
const DocumentLogo = "LOGO"

// Errors.
var (
	ErrNotFound          = errors.New("onboarding application not found")
	ErrDocumentNotFound  = errors.New("onboarding document not found")
	ErrInvalidState      = errors.New("the application is not in a state that allows this")
	ErrForbiddenRole     = errors.New("this role has no onboarding")
	ErrAgencyNotVerified = errors.New("the agency is no longer a verified agency")
	ErrTooManyDocuments  = errors.New("too many documents")
	ErrFileTooLarge      = errors.New("file too large")
	ErrUnsupportedFile   = errors.New("unsupported file type")
	// ErrLogoTooLarge and ErrUnsupportedLogo are the same refusals for the
	// LOGO document, whose limits are stricter.
	ErrLogoTooLarge    = errors.New("logo too large")
	ErrUnsupportedLogo = errors.New("a logo must be a PNG or JPEG image")
)

// ValidationError carries per-field messages for a 400 response.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// RoleOutcome says what approving an application for a role creates. The
// rows live in the database (onboarding_role_outcomes); DefaultOutcomes is
// the baseline they are seeded from.
type RoleOutcome struct {
	Role    string
	Outcome string
	// OrganisationKind is set for ORGANISATION outcomes.
	OrganisationKind string
	// DirectoryKind is the public profile created ("" = none).
	DirectoryKind string
	// MembershipRole is the membership created ("" = none).
	MembershipRole string
	// Specialism is recorded on the agent profile created: SpecialismSales,
	// SpecialismLettings or "".
	Specialism string
}

// Agent specialisms.
const (
	SpecialismSales    = "SALES"
	SpecialismLettings = "LETTINGS"
)

// StatusLabel is the wording shown to people for a stored status. DRAFT is
// "Pending verification": the role waits, and nothing has been submitted yet.
func StatusLabel(status string) string {
	switch status {
	case StatusDraft:
		return "Pending Verification"
	case StatusSubmitted:
		return "Under Review"
	case StatusChangesRequested:
		return "Changes Requested"
	case StatusApproved:
		return "Approved"
	case StatusRejected:
		return "Rejected"
	}
	return status
}

// DefaultOutcomes is the baseline role -> outcome table.
func DefaultOutcomes() []RoleOutcome {
	return []RoleOutcome{
		{"AGENT", OutcomeAgencyMember, "", KindAgents, auth.RoleAgent, ""},
		{"SALES_AGENT", OutcomeAgencyMember, "", KindAgents, auth.RoleAgent, SpecialismSales},
		{"LETTINGS_AGENT", OutcomeAgencyMember, "", KindAgents, auth.RoleAgent, SpecialismLettings},
		{"PROPERTY_MANAGER", OutcomeOrganisation, auth.KindPropertyManager, "property-managers", auth.RoleOwner, ""},
		{"SOLICITOR", OutcomeOrganisation, auth.KindLawFirm, "solicitors", auth.RoleOwner, ""},
		{"AGENCY_OWNER", OutcomeOrganisation, auth.KindAgency, KindAgencies, auth.RoleOwner, ""},
		{"AGENCY_ADMIN", OutcomeAgencyMember, "", "", auth.RoleManager, ""},
		{"AGENCY_STAFF", OutcomeAgencyMember, "", "", auth.RoleStaff, ""},
		// Individual owners: nothing is created, the role becomes ACTIVE.
		{"LANDLORD", OutcomeNone, "", "", "", ""},
		{"PRIVATE_SELLER", OutcomeNone, "", "", "", ""},
		{"HOST", OutcomeOrganisation, auth.KindHost, "hosts", auth.RoleOwner, ""},
		{"DEVELOPER", OutcomeOrganisation, auth.KindDeveloper, "developers", auth.RoleOwner, ""},
		{"SERVICE_PROVIDER", OutcomeOrganisation, auth.KindServiceProvider, KindServiceProviders, auth.RoleOwner, ""},
	}
}

// DocumentType is one kind of attachment (reference set onboarding_documents).
type DocumentType struct {
	Code        string
	Label       string
	Description string
}

// DefaultDocumentTypes is the baseline of the onboarding_documents reference
// set. PLACEHOLDERS: which documents each market really needs is a business
// and legal decision that has not been taken.
func DefaultDocumentTypes() []DocumentType {
	return []DocumentType{
		{"PROOF_OF_IDENTITY", "Proof of identity", "National ID, passport or driver's licence of the person applying."},
		{"BUSINESS_REGISTRATION", "Business registration", "Certificate of incorporation or business registration."},
		{"PROOF_OF_ADDRESS", "Proof of address", "A utility bill or bank statement for the business address, less than three months old."},
		{"TAX_CLEARANCE", "Tax clearance", "A current tax clearance certificate."},
		{"ESTATE_AGENTS_COUNCIL_CERTIFICATE", "Estate Agents Council certificate", "Current registration or compliance certificate from the Estate Agents Council."},
		{"PROOF_OF_EMPLOYMENT", "Proof of employment", "A letter from the agency confirming that you work there."},
		{"TRADE_LICENCE", "Trade licence", "Licence or certification for your trade, if your trade has one."},
		{"PROOF_OF_OWNERSHIP", "Proof of ownership", "Title deed or other proof that you own the property you intend to sell or let."},
		{"PRACTISING_CERTIFICATE", "Practising certificate", "Current practising certificate from the Law Society."},
		{DocumentLogo, "Logo or profile photo", LogoDescription},
	}
}

// LogoDescription is what an applicant is told about the LOGO document.
const LogoDescription = "Optional. Your business logo, or your own photo if you are an agent. Unlike your other documents it is shown " +
	"PUBLICLY, on your profile and on your listings, once your application is approved. PNG or JPEG, at most 2 MB, " +
	"between 128x128 and 4096x4096 pixels."

// Requirement says a role needs (or may attach) a document type. The rows
// live in the database (onboarding_role_documents).
type Requirement struct {
	Role     string
	Document string
	Required bool
}

// DefaultRequirements is the baseline role -> document table. PLACEHOLDERS,
// like DefaultDocumentTypes.
func DefaultRequirements() []Requirement {
	var out []Requirement
	for _, o := range DefaultOutcomes() {
		out = append(out, Requirement{o.Role, "PROOF_OF_IDENTITY", true})
		if o.Outcome == OutcomeOrganisation {
			out = append(out,
				Requirement{o.Role, "BUSINESS_REGISTRATION", true},
				Requirement{o.Role, "PROOF_OF_ADDRESS", true},
				Requirement{o.Role, "TAX_CLEARANCE", false})
		}
		switch o.Role {
		case "AGENCY_OWNER", "AGENT", "SALES_AGENT", "LETTINGS_AGENT":
			out = append(out, Requirement{o.Role, "ESTATE_AGENTS_COUNCIL_CERTIFICATE", true})
		case "AGENCY_STAFF", "AGENCY_ADMIN":
			out = append(out, Requirement{o.Role, "PROOF_OF_EMPLOYMENT", true})
		case "SERVICE_PROVIDER":
			out = append(out, Requirement{o.Role, "TRADE_LICENCE", false})
		case "SOLICITOR":
			out = append(out, Requirement{o.Role, "PRACTISING_CERTIFICATE", true})
		case "PRIVATE_SELLER", "LANDLORD":
			out = append(out, Requirement{o.Role, "PROOF_OF_OWNERSHIP", true})
		}
		// A role whose approval creates a public profile may attach its logo.
		if o.DirectoryKind != "" {
			out = append(out, Requirement{o.Role, DocumentLogo, false})
		}
	}
	return out
}

// Field is one detail an applicant fills in for a role.
type Field struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// Detail field names.
const (
	FieldBusinessName = "business_name"
	FieldTagline      = "tagline"
	FieldDescription  = "description"
	FieldCity         = "city"
	// FieldCountry is the ISO 3166-1 alpha-2 code of the country the business
	// is in; FieldCityName is its city as free text, for a country that has
	// no cities in the reference data (FieldCity is the code where it has).
	FieldCountry            = "country"
	FieldCityName           = "city_name"
	FieldAreas              = "areas"
	FieldServices           = "services"
	FieldTrade              = "trade"
	FieldRegistrationNumber = "registration_number"
	FieldContactPhone       = "contact_phone"
	FieldAgencySlug         = "agency_slug"
	FieldJobTitle           = "job_title"
	FieldBio                = "bio"
)

// FieldsFor returns the details that apply to a role, derived from its
// outcome row: a trade only for the service-provider directory, a bio only
// for a role that gets an agent profile.
func FieldsFor(o RoleOutcome) []Field {
	switch o.Outcome {
	case OutcomeOrganisation:
		fields := []Field{
			{FieldBusinessName, true}, {FieldTagline, false}, {FieldDescription, true}, {FieldCountry, true},
			// One of the two is required: `city` (a code) where the country
			// has cities in the reference data, otherwise `city_name`.
			{FieldCity, false}, {FieldCityName, false},
			{FieldAreas, false}, {FieldServices, false},
		}
		if o.DirectoryKind == KindServiceProviders {
			fields = append(fields, Field{FieldTrade, true})
		}
		return append(fields, Field{FieldRegistrationNumber, false}, Field{FieldContactPhone, true})
	case OutcomeAgencyMember:
		fields := []Field{{FieldAgencySlug, true}, {FieldJobTitle, false}}
		if o.DirectoryKind == KindAgents {
			fields = append(fields, Field{FieldBio, false})
		}
		return append(fields, Field{FieldAreas, false}, Field{FieldContactPhone, true})
	}
	return []Field{{FieldContactPhone, true}}
}

// Details are what the applicant tells us. Every key is always present in
// JSON; a text field that is not set is null and a list is always an array.
//
// contact_phone and registration_number are PRIVATE: they are for the
// verification team and are never copied to a public profile.
type Details struct {
	BusinessName *string `json:"business_name"`
	Tagline      *string `json:"tagline"`
	Description  *string `json:"description"`
	City         *string `json:"city"`
	// Country is the ISO 3166-1 alpha-2 code of the business's country and
	// CityName its city as free text (set instead of City, never with it).
	Country            *string  `json:"country"`
	CityName           *string  `json:"city_name"`
	Areas              []string `json:"areas"`
	Services           []string `json:"services"`
	Trade              *string  `json:"trade"`
	RegistrationNumber *string  `json:"registration_number"`
	ContactPhone       *string  `json:"contact_phone"`
	AgencySlug         *string  `json:"agency_slug"`
	JobTitle           *string  `json:"job_title"`
	Bio                *string  `json:"bio"`
}

func (d *Details) text(name string) **string {
	switch name {
	case FieldBusinessName:
		return &d.BusinessName
	case FieldTagline:
		return &d.Tagline
	case FieldDescription:
		return &d.Description
	case FieldCity:
		return &d.City
	case FieldCountry:
		return &d.Country
	case FieldCityName:
		return &d.CityName
	case FieldTrade:
		return &d.Trade
	case FieldRegistrationNumber:
		return &d.RegistrationNumber
	case FieldContactPhone:
		return &d.ContactPhone
	case FieldAgencySlug:
		return &d.AgencySlug
	case FieldJobTitle:
		return &d.JobTitle
	case FieldBio:
		return &d.Bio
	}
	return nil
}

// isSet reports whether a field has a value (lists are optional everywhere,
// so only text fields can be "missing").
func (d *Details) isSet(name string) bool {
	if p := d.text(name); p != nil {
		return *p != nil && **p != ""
	}
	return true
}

// Document is one attachment. ObjectKey is random and never leaves the API.
type Document struct {
	ID            uuid.UUID
	ApplicationID uuid.UUID
	Type          string
	FileName      string
	ContentType   string
	Size          int64
	SHA256        string
	ObjectKey     string
	UploadedAt    time.Time
}

// Applicant is the person behind an application.
type Applicant struct {
	ID            uuid.UUID
	Name          string
	Email         string
	EmailVerified bool
	CreatedAt     time.Time
}

// Application is one onboarding application.
type Application struct {
	ID     uuid.UUID
	UserID uuid.UUID
	Role   string
	Status string
	Details
	// MarketCode is the market the application belongs to (from its city or
	// its agency); "" until that is known, and always "" for roles without
	// either. It scopes what a MARKET_ADMIN may see.
	MarketCode string
	// AgencyName is the name of the chosen agency, kept for the admin list.
	AgencyName     string
	Documents      []Document
	DecisionReason string
	DecidedAt      *time.Time
	SubmittedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// Profile is the public profile an approval created, if any.
	Profile *ProfileRef
	// Applicant is filled by the store on reads.
	Applicant Applicant
}

// Editable reports whether the applicant may change the application.
func (a Application) Editable() bool {
	return a.Status == StatusDraft || a.Status == StatusChangesRequested
}

// HistoryEntry is one immutable transition record.
type HistoryEntry struct {
	From      string
	To        string
	ActorID   uuid.UUID
	ActorName string
	Reason    string
	At        time.Time
}

// Transition is a status change without side effects beyond the account role.
type Transition struct {
	ApplicationID uuid.UUID
	// From lists the statuses the change is allowed from.
	From    []string
	To      string
	ActorID uuid.UUID
	Reason  string
	At      time.Time
	// Event is the outbox event written with the change.
	Event string
	// AccountRoleStatus, when set, becomes the status of the user's account role.
	AccountRoleStatus string
}

// NewOrganisation, NewMembership and NewProfile are what an approval creates.
type NewOrganisation struct {
	ID         uuid.UUID
	Name       string
	Kind       string
	MarketCode string
}

type NewMembership struct {
	OrganisationID uuid.UUID
	MarketCode     string
	Role           string
}

type NewProfile struct {
	ID   uuid.UUID
	Kind string
	// Slug is the wanted slug; the store appends -2, -3, ... on a collision.
	Slug        string
	Name        string
	RoleTitle   string
	Tagline     string
	Description string
	CityCode    string
	// CityName is the free-text city (set instead of CityCode).
	CityName       string
	TradeCode      string
	Areas          []string
	Services       []string
	OrganisationID uuid.UUID
	AgentUserID    *uuid.UUID
	MarketCode     string
	// Specialism is set for a sales or lettings agent's profile.
	Specialism string
	// Logo is the profile's public logo, promoted from the applicant's LOGO
	// document (already stored under its own key); nil when there is none.
	Logo *directory.Logo
}

// Approval is everything an approval does, applied by the store in ONE
// transaction: the application becomes APPROVED, the account role ACTIVE,
// the organisation, membership and profile are created, a history row and an
// outbox event are written.
type Approval struct {
	ApplicationID uuid.UUID
	UserID        uuid.UUID
	Role          string
	ActorID       uuid.UUID
	Note          string
	At            time.Time
	Organisation  *NewOrganisation
	Membership    *NewMembership
	Profile       *NewProfile
	// AgencySlug is set for agency-member outcomes: the agency must still be
	// a verified, public agency when the transaction commits.
	AgencySlug string
}

// ProfileRef is a public profile address.
type ProfileRef struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
}

// Created is what an approval created.
type Created struct {
	OrganisationID *uuid.UUID  `json:"organisation_id"`
	Profile        *ProfileRef `json:"profile"`
	MembershipRole *string     `json:"membership_role"`
}

// ListFilter narrows the admin list.
type ListFilter struct {
	Status string
	Role   string
	Q      string
	// Markets limits the list to these market codes; nil means every market.
	Markets []string
	Limit   int
	Offset  int
}

// Store persists applications. Reads return the applicant with each
// application and the documents newest first.
type Store interface {
	// Outcomes returns the role -> outcome table.
	Outcomes(ctx context.Context) ([]RoleOutcome, error)
	// Requirements returns the role -> document table.
	Requirements(ctx context.Context) ([]Requirement, error)

	// Current returns the newest application per role of a user.
	Current(ctx context.Context, userID uuid.UUID) ([]Application, error)
	// Get returns one application or ErrNotFound.
	Get(ctx context.Context, id uuid.UUID) (Application, error)
	// Reapply creates a new DRAFT application after a rejection and sets the
	// account role back to PENDING_VERIFICATION. ErrInvalidState unless the
	// user's newest application for the role is REJECTED.
	Reapply(ctx context.Context, app Application) error
	// SaveDetails replaces the details of an editable application.
	// ErrInvalidState when it is not editable.
	SaveDetails(ctx context.Context, id uuid.UUID, d Details, marketCode, agencyName string, now time.Time) error
	// AddDocument stores a document on an editable application. A document
	// of the same type is replaced: the object keys of what it replaced are
	// returned so the caller can delete the files. ErrTooManyDocuments when
	// the application would exceed MaxDocumentsPerApp.
	AddDocument(ctx context.Context, doc Document) (replaced []string, err error)
	// DeleteDocument removes a document from an editable application and
	// returns its object key.
	DeleteDocument(ctx context.Context, applicationID, documentID uuid.UUID, now time.Time) (objectKey string, err error)
	// Transition changes the status. ErrInvalidState when the current status
	// is not in t.From.
	Transition(ctx context.Context, t Transition) error
	// Approve applies an approval atomically. ErrInvalidState unless
	// SUBMITTED; ErrAgencyNotVerified when the chosen agency stopped being a
	// verified agency.
	Approve(ctx context.Context, a Approval) (Created, error)
	// List returns one page for administrators, oldest submission first, the
	// total, and the number of applications per status in the same scope.
	List(ctx context.Context, f ListFilter) (items []Application, total int, counts map[string]int, err error)
	// History returns the transitions of an application, oldest first.
	History(ctx context.Context, id uuid.UUID) ([]HistoryEntry, error)
}

// ---------------------------------------------------------------------------
// Text helpers
// ---------------------------------------------------------------------------

var (
	htmlTagRe = regexp.MustCompile(`<[^>]*>`)
	phoneRe   = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	slugRe    = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// cleanText trims a value and reports why it is not acceptable ("" = fine).
// Line breaks are allowed only in multi-line fields.
func cleanText(v string, min, max int, multiline bool) (string, string) {
	v = strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
	n := utf8.RuneCountInString(v)
	switch {
	case !utf8.ValidString(v):
		return "", "contains characters that are not allowed"
	case n < min && n > 0:
		return "", "is too short"
	case n > max:
		return "", "is too long"
	case htmlTagRe.MatchString(v):
		return "", "must be plain text without HTML"
	}
	for _, r := range v {
		if unicode.IsControl(r) && !(multiline && r == '\n') {
			return "", "contains characters that are not allowed"
		}
	}
	return v, ""
}

// Slugify derives a URL slug from a name: lower-case letters and digits
// joined by single dashes, at most 70 characters ("profile" when nothing is
// left).
func Slugify(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
		if b.Len() >= 70 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "profile"
	}
	return slug
}

// SanitiseFileName turns a client-supplied file name into a short display
// name: the last path element, letters, digits, spaces and . _ - ( ) only,
// at most MaxFileNameRunes, with the extension that matches the detected
// content type. It is only ever shown and used in Content-Disposition; it is
// never part of a storage key.
func SanitiseFileName(name, contentType string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r), r == ' ', r == '.', r == '_', r == '-', r == '(', r == ')':
			b.WriteRune(r)
		}
	}
	base := strings.Trim(strings.TrimSpace(b.String()), ".")
	if i := strings.LastIndex(base, "."); i > 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = "document"
	}
	ext := extensions[contentType]
	if runes := []rune(base); len(runes) > MaxFileNameRunes-len(ext) {
		base = strings.TrimSpace(string(runes[:MaxFileNameRunes-len(ext)]))
	}
	return base + ext
}

// extensions maps the allowed content types to the file extension shown.
var extensions = map[string]string{
	"application/pdf": ".pdf",
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
}

// AllowedContentTypes lists the content types an upload may have.
func AllowedContentTypes() []string { return []string{"application/pdf", "image/jpeg", "image/png"} }
