package onboarding

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/directory"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Vocabulary supplies the reference data this module validates against and
// labels with. Every call reads the reference store, so a deactivated row
// stops being accepted immediately.
type Vocabulary interface {
	// RoleLabels returns the label of every account role.
	RoleLabels(ctx context.Context) (map[string]string, error)
	// CityMarkets returns the market code of every active city ("" when a
	// city applies to every market).
	CityMarkets(ctx context.Context) (map[string]string, error)
	// Countries returns the codes of the enabled countries (ISO 3166-1 alpha-2).
	Countries(ctx context.Context) ([]string, error)
	// TradeCodes returns the active service trades.
	TradeCodes(ctx context.Context) ([]string, error)
	// DocumentTypes returns the active onboarding document types in display order.
	DocumentTypes(ctx context.Context) ([]DocumentType, error)
}

// Agency is a verified, public agency an applicant may join.
type Agency struct {
	Slug           string
	Name           string
	OrganisationID uuid.UUID
	CityCode       string
	MarketCode     string
}

// Agencies looks up agencies in the public directory.
type Agencies interface {
	// VerifiedAgency returns the agency with that slug if it is public and
	// verified, or nil.
	VerifiedAgency(ctx context.Context, slug string) (*Agency, error)
	// AgencyOf returns the public, verified agency profile of an
	// organisation, or nil when it has none.
	AgencyOf(ctx context.Context, organisationID uuid.UUID) (*Agency, error)
}

// Service implements the onboarding use cases.
type Service struct {
	store    Store
	files    storage.Storage
	vocab    Vocabulary
	agencies Agencies
	logger   *slog.Logger
	now      func() time.Time
}

// NewService builds a Service.
func NewService(store Store, files storage.Storage, vocab Vocabulary, agencies Agencies, logger *slog.Logger) *Service {
	return &Service{store: store, files: files, vocab: vocab, agencies: agencies, logger: logger, now: time.Now}
}

// SetClock overrides the time source (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// audit writes one structured audit line (audit=true). Log-only, like the
// identity audit lines: it is not a durable audit record. It never carries
// document content, file names or private details.
func (s *Service) audit(ctx context.Context, event string, attrs ...slog.Attr) {
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx))}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

// catalogue is the reference data one request works with.
type catalogue struct {
	outcomes   map[string]RoleOutcome
	order      []string
	required   map[string][]Requirement
	docTypes   map[string]DocumentType
	docOrder   []string
	roleLabels map[string]string
	// cityCountries are the countries that have cities in the reference
	// data: there a city is chosen by code, elsewhere it is typed.
	cityCountries map[string]bool
}

func (s *Service) catalogue(ctx context.Context) (*catalogue, error) {
	outcomes, err := s.store.Outcomes(ctx)
	if err != nil {
		return nil, fmt.Errorf("load onboarding outcomes: %w", err)
	}
	reqs, err := s.store.Requirements(ctx)
	if err != nil {
		return nil, fmt.Errorf("load onboarding requirements: %w", err)
	}
	types, err := s.vocab.DocumentTypes(ctx)
	if err != nil {
		return nil, fmt.Errorf("load document types: %w", err)
	}
	labels, err := s.vocab.RoleLabels(ctx)
	if err != nil {
		return nil, fmt.Errorf("load role labels: %w", err)
	}
	cities, err := s.vocab.CityMarkets(ctx)
	if err != nil {
		return nil, fmt.Errorf("load cities: %w", err)
	}
	c := &catalogue{outcomes: map[string]RoleOutcome{}, required: map[string][]Requirement{}, docTypes: map[string]DocumentType{}, roleLabels: labels,
		cityCountries: map[string]bool{}}
	for _, country := range cities {
		if country != "" {
			c.cityCountries[country] = true
		}
	}
	for _, t := range types {
		c.docTypes[t.Code] = t
		c.docOrder = append(c.docOrder, t.Code)
	}
	for _, o := range outcomes {
		// A platform role can never have an outcome, whatever the table says.
		if auth.IsPlatformRole(o.Role) {
			continue
		}
		c.outcomes[o.Role] = o
		c.order = append(c.order, o.Role)
	}
	for _, r := range reqs {
		// A requirement naming an inactive or unknown document type is ignored.
		if _, ok := c.docTypes[r.Document]; ok {
			c.required[r.Role] = append(c.required[r.Role], r)
		}
	}
	for role := range c.required {
		rs := c.required[role]
		sort.SliceStable(rs, func(i, j int) bool {
			return slices.Index(c.docOrder, rs[i].Document) < slices.Index(c.docOrder, rs[j].Document)
		})
	}
	return c, nil
}

func (c *catalogue) roleLabel(role string) string {
	if l, ok := c.roleLabels[role]; ok {
		return l
	}
	return role
}

func (c *catalogue) allows(role, document string) bool {
	return slices.ContainsFunc(c.required[role], func(r Requirement) bool { return r.Document == document })
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// RequirementDocument is one document a role needs or may attach.
type RequirementDocument struct {
	Code        string `json:"code"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// RoleRequirements is what one role has to provide.
type RoleRequirements struct {
	Role             string                `json:"role"`
	Label            string                `json:"label"`
	Outcome          string                `json:"outcome"`
	OrganisationKind *string               `json:"organisation_kind"`
	DirectoryKind    *string               `json:"directory_kind"`
	Fields           []Field               `json:"fields"`
	Documents        []RequirementDocument `json:"documents"`
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (c *catalogue) documentsFor(role string) []RequirementDocument {
	out := []RequirementDocument{}
	for _, r := range c.required[role] {
		t := c.docTypes[r.Document]
		out = append(out, RequirementDocument{Code: t.Code, Label: t.Label, Description: t.Description, Required: r.Required})
	}
	return out
}

// Requirements returns what every onboarding role has to provide.
func (s *Service) Requirements(ctx context.Context) ([]RoleRequirements, error) {
	c, err := s.catalogue(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]RoleRequirements, 0, len(c.order))
	for _, role := range c.order {
		o := c.outcomes[role]
		out = append(out, RoleRequirements{
			Role: role, Label: c.roleLabel(role), Outcome: o.Outcome,
			OrganisationKind: nullable(o.OrganisationKind), DirectoryKind: nullable(o.DirectoryKind),
			Fields: FieldsFor(o), Documents: c.documentsFor(role),
		})
	}
	return out, nil
}

// CodedLabel is a code with its label.
type CodedLabel struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

// DocumentView is a document as the API shows it. The storage key and the
// checksum are never included.
type DocumentView struct {
	ID          uuid.UUID  `json:"id"`
	Type        CodedLabel `json:"type"`
	FileName    string     `json:"file_name"`
	ContentType string     `json:"content_type"`
	SizeBytes   int64      `json:"size_bytes"`
	UploadedAt  time.Time  `json:"uploaded_at"`
}

// Missing lists what still has to be provided before submitting.
type Missing struct {
	Fields    []string `json:"fields"`
	Documents []string `json:"documents"`
}

// Decision is the last decision an administrator took.
type Decision struct {
	Reason    *string    `json:"reason"`
	DecidedAt *time.Time `json:"decided_at"`
}

// ApplicationView is an application as its owner sees it.
type ApplicationView struct {
	ID        uuid.UUID `json:"id"`
	Role      string    `json:"role"`
	RoleLabel string    `json:"role_label"`
	Status    string    `json:"status"`
	// StatusLabel is the wording to show for Status (see StatusLabel).
	StatusLabel string         `json:"status_label"`
	Details     Details        `json:"details"`
	Documents   []DocumentView `json:"documents"`
	Missing     Missing        `json:"missing"`
	CanSubmit   bool           `json:"can_submit"`
	CanEdit     bool           `json:"can_edit"`
	// CanReapply is true after a rejection: saving details again starts a new application.
	CanReapply  bool        `json:"can_reapply"`
	Decision    Decision    `json:"decision"`
	Profile     *ProfileRef `json:"profile"`
	SubmittedAt *time.Time  `json:"submitted_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

func (c *catalogue) documentViews(docs []Document) []DocumentView {
	out := make([]DocumentView, 0, len(docs))
	for _, d := range docs {
		label := d.Type
		if t, ok := c.docTypes[d.Type]; ok {
			label = t.Label
		}
		out = append(out, DocumentView{ID: d.ID, Type: CodedLabel{Code: d.Type, Label: label}, FileName: d.FileName,
			ContentType: d.ContentType, SizeBytes: d.Size, UploadedAt: d.UploadedAt.UTC()})
	}
	return out
}

func (c *catalogue) missing(a Application) Missing {
	m := Missing{Fields: []string{}, Documents: []string{}}
	for _, f := range FieldsFor(c.outcomes[a.Role]) {
		if f.Required && !a.Details.isSet(f.Name) {
			m.Fields = append(m.Fields, f.Name)
		}
	}
	// A business needs a city: its code where the country has cities in the
	// reference data, free text elsewhere.
	if c.outcomes[a.Role].Outcome == OutcomeOrganisation && !a.Details.isSet(FieldCity) && !a.Details.isSet(FieldCityName) {
		if a.Country != nil && !c.cityCountries[*a.Country] {
			m.Fields = append(m.Fields, FieldCityName)
		} else {
			m.Fields = append(m.Fields, FieldCity)
		}
	}
	for _, r := range c.required[a.Role] {
		if r.Required && !slices.ContainsFunc(a.Documents, func(d Document) bool { return d.Type == r.Document }) {
			m.Documents = append(m.Documents, r.Document)
		}
	}
	return m
}

func normalised(d Details) Details {
	if d.Areas == nil {
		d.Areas = []string{}
	}
	if d.Services == nil {
		d.Services = []string{}
	}
	return d
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func (c *catalogue) view(a Application) ApplicationView {
	m := c.missing(a)
	return ApplicationView{
		ID: a.ID, Role: a.Role, RoleLabel: c.roleLabel(a.Role), Status: a.Status, StatusLabel: StatusLabel(a.Status),
		Details: normalised(a.Details), Documents: c.documentViews(a.Documents), Missing: m,
		CanSubmit:  a.Editable() && len(m.Fields) == 0 && len(m.Documents) == 0,
		CanEdit:    a.Editable(),
		CanReapply: a.Status == StatusRejected,
		Decision:   Decision{Reason: nullable(a.DecisionReason), DecidedAt: utcPtr(a.DecidedAt)},
		Profile:    a.Profile, SubmittedAt: utcPtr(a.SubmittedAt), UpdatedAt: a.UpdatedAt.UTC(),
	}
}

// ---------------------------------------------------------------------------
// The applicant's side
// ---------------------------------------------------------------------------

// Mine returns the caller's current application for every onboarding role
// they hold.
func (s *Service) Mine(ctx context.Context, p *auth.Principal) ([]ApplicationView, error) {
	c, err := s.catalogue(ctx)
	if err != nil {
		return nil, err
	}
	apps, err := s.store.Current(ctx, p.UserID)
	if err != nil {
		return nil, err
	}
	out := []ApplicationView{}
	for _, role := range c.order {
		for _, a := range apps {
			if a.Role == role && p.HasAccountRole(role) {
				out = append(out, c.view(a))
			}
		}
	}
	return out, nil
}

// mine resolves the caller's current application for a role.
//
//   - a platform role, or an account role that has no onboarding (CONSUMER,
//     TENANT): ErrForbiddenRole;
//   - a role the caller does not hold, an unknown role, or no application:
//     ErrNotFound.
func (s *Service) mine(ctx context.Context, p *auth.Principal, role string) (*catalogue, Application, error) {
	c, err := s.catalogue(ctx)
	if err != nil {
		return nil, Application{}, err
	}
	if _, ok := c.outcomes[role]; !ok {
		if _, known := c.roleLabels[role]; known || auth.IsPlatformRole(role) {
			return nil, Application{}, ErrForbiddenRole
		}
		return nil, Application{}, ErrNotFound
	}
	if !p.HasAccountRole(role) {
		return nil, Application{}, ErrNotFound
	}
	apps, err := s.store.Current(ctx, p.UserID)
	if err != nil {
		return nil, Application{}, err
	}
	for _, a := range apps {
		if a.Role == role {
			return c, a, nil
		}
	}
	return nil, Application{}, ErrNotFound
}

func (s *Service) reload(ctx context.Context, c *catalogue, id uuid.UUID) (ApplicationView, error) {
	a, err := s.store.Get(ctx, id)
	if err != nil {
		return ApplicationView{}, err
	}
	return c.view(a), nil
}

// Save applies a partial update of the details. A key that is absent is left
// alone; null (or "") clears a text field. After a rejection it starts a new
// application for the same role.
func (s *Service) Save(ctx context.Context, p *auth.Principal, role string, patch map[string]json.RawMessage) (ApplicationView, error) {
	c, app, err := s.mine(ctx, p, role)
	if err != nil {
		return ApplicationView{}, err
	}
	if !app.Editable() && app.Status != StatusRejected {
		return ApplicationView{}, ErrInvalidState
	}
	outcome := c.outcomes[role]
	details := app.Details
	if app.Status == StatusRejected {
		details = Details{}
	}
	marketCode, agencyName := app.MarketCode, app.AgencyName
	fields, err := s.applyPatch(ctx, outcome, &details, &marketCode, &agencyName, patch)
	if err != nil {
		return ApplicationView{}, err
	}
	if len(fields) > 0 {
		return ApplicationView{}, &ValidationError{Fields: fields}
	}
	now := s.clock()
	if app.Status == StatusRejected {
		id, err := uuid.NewV7()
		if err != nil {
			return ApplicationView{}, err
		}
		app = Application{ID: id, UserID: p.UserID, Role: role, Status: StatusDraft, CreatedAt: now, UpdatedAt: now}
		if err := s.store.Reapply(ctx, app); err != nil {
			return ApplicationView{}, err
		}
		s.audit(ctx, "onboarding.reapplied", slog.String("application_id", id.String()), slog.String("role", role), slog.String("actor_user_id", p.UserID.String()))
	}
	if err := s.store.SaveDetails(ctx, app.ID, details, marketCode, agencyName, now); err != nil {
		return ApplicationView{}, err
	}
	return s.reload(ctx, c, app.ID)
}

func (s *Service) applyPatch(ctx context.Context, outcome RoleOutcome, d *Details, marketCode, agencyName *string, patch map[string]json.RawMessage) (map[string]string, error) {
	fields := map[string]string{}
	allowed := map[string]bool{}
	for _, f := range FieldsFor(outcome) {
		allowed[f.Name] = true
	}
	text := func(name string, min, max int, multiline bool) (value *string, present bool) {
		raw, ok := patch[name]
		if !ok {
			return nil, false
		}
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil {
			fields[name] = "must be text or null"
			return nil, false
		}
		if v == nil || strings.TrimSpace(*v) == "" {
			return nil, true
		}
		clean, problem := cleanText(*v, min, max, multiline)
		if problem != "" {
			fields[name] = problem
			return nil, false
		}
		return &clean, true
	}
	list := func(name string) ([]string, bool) {
		raw, ok := patch[name]
		if !ok {
			return nil, false
		}
		var items []string
		if err := json.Unmarshal(raw, &items); err != nil {
			fields[name] = "must be a list of text values"
			return nil, false
		}
		out := []string{}
		for _, it := range items {
			clean, problem := cleanText(it, 1, maxListItemRunes, false)
			switch {
			case problem != "":
				fields[name] = "has an item that " + problem
				return nil, false
			case clean == "":
				continue
			case !slices.Contains(out, clean):
				out = append(out, clean)
			}
		}
		if len(out) > maxListItems {
			fields[name] = fmt.Sprintf("must have at most %d items", maxListItems)
			return nil, false
		}
		return out, true
	}
	for name := range patch {
		if !allowed[name] {
			if (&Details{}).text(name) != nil || name == FieldAreas || name == FieldServices {
				fields[name] = "does not apply to this role"
			} else {
				fields[name] = "is not a known field"
			}
			delete(patch, name)
		}
	}

	if v, ok := text(FieldBusinessName, 2, 120, false); ok {
		d.BusinessName = v
	}
	if v, ok := text(FieldTagline, 0, 140, false); ok {
		d.Tagline = v
	}
	if v, ok := text(FieldDescription, 0, 2000, true); ok {
		d.Description = v
	}
	if v, ok := text(FieldRegistrationNumber, 0, maxRegistrationNumRunes, false); ok {
		d.RegistrationNumber = v
	}
	if v, ok := text(FieldJobTitle, 0, 80, false); ok {
		d.JobTitle = v
	}
	if v, ok := text(FieldBio, 0, 1000, true); ok {
		d.Bio = v
	}
	if v, ok := list(FieldAreas); ok {
		d.Areas = v
	}
	if v, ok := list(FieldServices); ok {
		d.Services = v
	}
	if v, ok := text(FieldContactPhone, 0, 20, false); ok {
		if v != nil && !phoneRe.MatchString(*v) {
			fields[FieldContactPhone] = "must be a phone number in international format, for example +263771234567"
		} else {
			d.ContactPhone = v
		}
	}
	if err := s.applyPlace(ctx, d, marketCode, fields, text); err != nil {
		return nil, err
	}
	if v, ok := text(FieldTrade, 0, 80, false); ok {
		trades, err := s.vocab.TradeCodes(ctx)
		if err != nil {
			return nil, err
		}
		if v != nil && !slices.Contains(trades, *v) {
			fields[FieldTrade] = "is not a known value; see GET /v1/reference/service_trades"
		} else {
			d.Trade = v
		}
	}
	if v, ok := text(FieldAgencySlug, 0, 80, false); ok {
		switch {
		case v == nil:
			d.AgencySlug, *marketCode, *agencyName = nil, "", ""
		case !slugRe.MatchString(*v):
			fields[FieldAgencySlug] = "is not a verified agency; see GET /v1/directory/agencies"
		default:
			agency, err := s.agencies.VerifiedAgency(ctx, *v)
			if err != nil {
				return nil, err
			}
			if agency == nil {
				fields[FieldAgencySlug] = "is not a verified agency; see GET /v1/directory/agencies"
			} else {
				d.AgencySlug, *marketCode, *agencyName = v, agency.MarketCode, agency.Name
			}
		}
	}
	return fields, nil
}

// applyPlace applies country, city and city_name. The platform is global:
// any enabled country may be chosen. Where the country has cities in the
// reference data the city is one of their codes; elsewhere it is free text
// (city_name). A city code sent without a country names its own country, so
// a client that only knows `city` keeps working. The application's market
// (what a market administrator is scoped by) is its country.
func (s *Service) applyPlace(ctx context.Context, d *Details, marketCode *string, fields map[string]string,
	text func(name string, min, max int, multiline bool) (*string, bool)) error {
	country, hasCountry := text(FieldCountry, 0, 2, false)
	city, hasCity := text(FieldCity, 0, 80, false)
	cityName, hasCityName := text(FieldCityName, 0, maxCityNameRunes, false)
	if !hasCountry && !hasCity && !hasCityName {
		return nil
	}
	cityCountry, err := s.vocab.CityMarkets(ctx)
	if err != nil {
		return err
	}
	hasCities := map[string]bool{}
	for _, c := range cityCountry {
		hasCities[c] = true
	}
	newCountry, newCity, newCityName := d.Country, d.City, d.CityName
	if hasCountry {
		newCountry = nil
		if country != nil {
			code := strings.ToUpper(*country)
			known, err := s.vocab.Countries(ctx)
			if err != nil {
				return err
			}
			if !slices.Contains(known, code) {
				fields[FieldCountry] = "is not a known country; see GET /v1/countries"
				return nil
			}
			newCountry = &code
		}
	}
	if hasCity {
		if newCity = city; city != nil {
			newCityName = nil
		}
	}
	if hasCityName {
		switch {
		case cityName != nil && hasCity && city != nil:
			fields[FieldCityName] = "cannot be combined with city; send one of them"
			return nil
		case cityName != nil && strings.ContainsAny(*cityName, "<>"):
			fields[FieldCityName] = "must be plain text, without HTML"
			return nil
		case cityName != nil:
			newCityName, newCity = cityName, nil
		default:
			newCityName = nil
		}
	}
	if newCity != nil {
		of, known := cityCountry[*newCity]
		switch {
		case !known:
			if hasCity {
				fields[FieldCity] = "is not a known value; see GET /v1/reference/cities"
				return nil
			}
			newCity = nil // the stored city was retired
		case of == "":
		case newCountry == nil || (hasCity && !hasCountry):
			newCountry = &of // a city names its country
		case *newCountry != of && hasCity:
			fields[FieldCity] = "is not a city of that country; see GET /v1/reference/cities?market=" + *newCountry
			return nil
		case *newCountry != of:
			newCity = nil // the country changed: its old city no longer applies
		}
	}
	if newCityName != nil && newCountry != nil && hasCities[*newCountry] {
		if hasCityName {
			fields[FieldCityName] = "this country has a list of cities: send city, a code from GET /v1/reference/cities?market=" + *newCountry
			return nil
		}
		newCityName = nil // the country changed to one whose cities are chosen by code
	}
	d.Country, d.City, d.CityName = newCountry, newCity, newCityName
	*marketCode = deref(newCountry)
	return nil
}

// maxCityNameRunes bounds a free-text city name.
const maxCityNameRunes = 80

// Submit sends the application for review.
func (s *Service) Submit(ctx context.Context, p *auth.Principal, role string) (ApplicationView, error) {
	c, app, err := s.mine(ctx, p, role)
	if err != nil {
		return ApplicationView{}, err
	}
	if !app.Editable() {
		return ApplicationView{}, ErrInvalidState
	}
	fields := map[string]string{}
	m := c.missing(app)
	for _, f := range m.Fields {
		fields["details."+f] = "is required"
	}
	for _, d := range m.Documents {
		fields["documents."+d] = "is required"
	}
	if app.AgencySlug != nil {
		agency, err := s.agencies.VerifiedAgency(ctx, *app.AgencySlug)
		if err != nil {
			return ApplicationView{}, err
		}
		if agency == nil {
			fields["details."+FieldAgencySlug] = "is no longer a verified agency; choose another"
		}
	}
	if len(fields) > 0 {
		return ApplicationView{}, &ValidationError{Fields: fields}
	}
	now := s.clock()
	if err := s.store.Transition(ctx, Transition{
		ApplicationID: app.ID, From: []string{StatusDraft, StatusChangesRequested}, To: StatusSubmitted,
		ActorID: p.UserID, At: now, Event: EventSubmitted,
	}); err != nil {
		return ApplicationView{}, err
	}
	s.audit(ctx, "onboarding.submitted", slog.String("application_id", app.ID.String()), slog.String("role", role),
		slog.String("from", app.Status), slog.String("to", StatusSubmitted), slog.String("actor_user_id", p.UserID.String()))
	return s.reload(ctx, c, app.ID)
}

// Upload validates and stores one document. The content type is decided by
// looking at the bytes: the client's Content-Type header and the file
// extension are ignored. A PNG named "licence.pdf" is therefore accepted as a
// PNG and shown as "licence.png".
func (s *Service) Upload(ctx context.Context, p *auth.Principal, role, typeCode, fileName string, content []byte) (DocumentView, error) {
	c, app, err := s.mine(ctx, p, role)
	if err != nil {
		return DocumentView{}, err
	}
	if !app.Editable() {
		return DocumentView{}, ErrInvalidState
	}
	fields := map[string]string{}
	switch {
	case typeCode == "":
		fields["type"] = "is required"
	case !c.allows(role, typeCode):
		fields["type"] = "is not a document this role can attach; see GET /v1/onboarding/requirements"
	}
	if len(content) == 0 {
		fields["file"] = "is required and must not be empty"
	}
	if len(fields) > 0 {
		return DocumentView{}, &ValidationError{Fields: fields}
	}
	if len(content) > MaxDocumentBytes {
		return DocumentView{}, ErrFileTooLarge
	}
	contentType := http.DetectContentType(content)
	if !slices.Contains(AllowedContentTypes(), contentType) && typeCode != DocumentLogo {
		return DocumentView{}, ErrUnsupportedFile
	}
	if typeCode == DocumentLogo {
		// The one document that becomes public: an image only, checked now
		// with the rules of a profile logo (and again when it is promoted).
		switch _, err := directory.CheckLogo(content); {
		case errors.Is(err, directory.ErrLogoTooLarge):
			return DocumentView{}, ErrLogoTooLarge
		case errors.Is(err, directory.ErrLogoDimensions):
			return DocumentView{}, &ValidationError{Fields: map[string]string{"file": fmt.Sprintf("must be between %[1]dx%[1]d and %[2]dx%[2]d pixels",
				directory.MinLogoPixels, directory.MaxLogoPixels)}}
		case err != nil:
			return DocumentView{}, ErrUnsupportedLogo
		}
	}
	replaces := slices.ContainsFunc(app.Documents, func(d Document) bool { return d.Type == typeCode })
	if !replaces && len(app.Documents) >= MaxDocumentsPerApp {
		return DocumentView{}, ErrTooManyDocuments
	}

	key, err := storage.NewKey(DocumentKeyPrefix)
	if err != nil {
		return DocumentView{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return DocumentView{}, err
	}
	sum := sha256.Sum256(content)
	doc := Document{
		ID: id, ApplicationID: app.ID, Type: typeCode, FileName: SanitiseFileName(fileName, contentType), ContentType: contentType,
		Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), ObjectKey: key, UploadedAt: s.clock(),
	}
	if err := s.files.Put(ctx, key, bytes.NewReader(content), doc.Size, contentType); err != nil {
		return DocumentView{}, fmt.Errorf("store document: %w", err)
	}
	replaced, err := s.store.AddDocument(ctx, doc)
	if err != nil {
		s.remove(ctx, key)
		return DocumentView{}, err
	}
	for _, old := range replaced {
		s.remove(ctx, old)
	}
	s.audit(ctx, "onboarding.document_uploaded", slog.String("application_id", app.ID.String()), slog.String("document_id", id.String()),
		slog.String("document_type", typeCode), slog.String("content_type", contentType), slog.Int64("size_bytes", doc.Size),
		slog.String("actor_user_id", p.UserID.String()))
	return c.documentViews([]Document{doc})[0], nil
}

// remove deletes a stored file. A failure leaves an orphan object behind; it
// is logged (with the random key only) and does not fail the request.
func (s *Service) remove(ctx context.Context, key string) {
	if err := s.files.Delete(context.WithoutCancel(ctx), key); err != nil {
		s.logger.ErrorContext(ctx, "onboarding document object could not be deleted", slog.String("object_key", key), slog.String("error", err.Error()))
	}
}

// DeleteDocument removes one of the caller's documents while the application
// is editable.
func (s *Service) DeleteDocument(ctx context.Context, p *auth.Principal, role string, documentID uuid.UUID) error {
	_, app, err := s.mine(ctx, p, role)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(app.Documents, func(d Document) bool { return d.ID == documentID }) {
		return ErrDocumentNotFound
	}
	if !app.Editable() {
		return ErrInvalidState
	}
	key, err := s.store.DeleteDocument(ctx, app.ID, documentID, s.clock())
	if err != nil {
		return err
	}
	s.remove(ctx, key)
	return nil
}

// File is a stored document ready to be streamed.
type File struct {
	Document Document
	Content  io.ReadCloser
}

func (s *Service) open(ctx context.Context, app Application, documentID uuid.UUID) (File, error) {
	for _, d := range app.Documents {
		if d.ID == documentID {
			rc, err := s.files.Open(ctx, d.ObjectKey)
			if errors.Is(err, storage.ErrNotFound) {
				return File{}, ErrDocumentNotFound
			}
			if err != nil {
				return File{}, fmt.Errorf("open document: %w", err)
			}
			return File{Document: d, Content: rc}, nil
		}
	}
	return File{}, ErrDocumentNotFound
}

// OpenDocument opens one of the caller's own documents.
func (s *Service) OpenDocument(ctx context.Context, p *auth.Principal, role string, documentID uuid.UUID) (File, error) {
	_, app, err := s.mine(ctx, p, role)
	if err != nil {
		return File{}, err
	}
	return s.open(ctx, app, documentID)
}

// ---------------------------------------------------------------------------
// The administrator's side
// ---------------------------------------------------------------------------

// AdminRoles are the platform roles that may review applications.
// MARKET_ADMIN only sees the applications of its own markets.
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformModerator, auth.PlatformMarketAdmin}
}

// scope returns the markets the administrator may review: nil means all.
// ok is false when the caller may not review anything.
func scope(p *auth.Principal) (markets []string, ok bool) {
	if p == nil || p.SessionScope != auth.ScopeAdmin {
		return nil, false
	}
	for _, r := range p.PlatformRoles {
		switch r.Role {
		case auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformModerator:
			return nil, true
		case auth.PlatformMarketAdmin:
			if r.MarketCode != "" && !slices.Contains(markets, r.MarketCode) {
				markets = append(markets, r.MarketCode)
			}
		}
	}
	return markets, len(markets) > 0
}

// ErrForbidden is returned when the caller may not review applications.
var ErrForbidden = errors.New("not allowed to review onboarding applications")

// reviewable loads an application the administrator may see. An application
// outside a market administrator's markets is reported as not found.
func (s *Service) reviewable(ctx context.Context, p *auth.Principal, id uuid.UUID) (Application, error) {
	markets, ok := scope(p)
	if !ok {
		return Application{}, ErrForbidden
	}
	app, err := s.store.Get(ctx, id)
	if err != nil {
		return Application{}, err
	}
	if markets != nil && !slices.Contains(markets, app.MarketCode) {
		return Application{}, ErrNotFound
	}
	return app, nil
}

// ApplicantSummary is the applicant in the admin list.
type ApplicantSummary struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// AgencyRef names the agency an applicant wants to join.
type AgencyRef struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// AdminItem is one row of the admin list.
type AdminItem struct {
	ID            uuid.UUID        `json:"id"`
	Role          string           `json:"role"`
	RoleLabel     string           `json:"role_label"`
	Status        string           `json:"status"`
	StatusLabel   string           `json:"status_label"`
	Applicant     ApplicantSummary `json:"applicant"`
	BusinessName  *string          `json:"business_name"`
	Agency        *AgencyRef       `json:"agency"`
	SubmittedAt   *time.Time       `json:"submitted_at"`
	UpdatedAt     time.Time        `json:"updated_at"`
	DocumentCount int              `json:"document_count"`
}

// AdminPage is one page of the admin list.
type AdminPage struct {
	Items      []AdminItem    `json:"items"`
	Page       int            `json:"page"`
	PageSize   int            `json:"page_size"`
	Total      int            `json:"total"`
	TotalPages int            `json:"total_pages"`
	Counts     map[string]int `json:"counts"`
}

func agencyRef(a Application) *AgencyRef {
	if a.AgencySlug == nil {
		return nil
	}
	name := a.AgencyName
	if name == "" {
		name = *a.AgencySlug
	}
	return &AgencyRef{Slug: *a.AgencySlug, Name: name}
}

// List returns one page of applications for an administrator.
func (s *Service) List(ctx context.Context, p *auth.Principal, status, role, q string, page, size int) (AdminPage, error) {
	markets, ok := scope(p)
	if !ok {
		return AdminPage{}, ErrForbidden
	}
	c, err := s.catalogue(ctx)
	if err != nil {
		return AdminPage{}, err
	}
	fields := map[string]string{}
	if status == "" {
		status = StatusSubmitted
	}
	if !slices.Contains(Statuses(), status) {
		fields["status"] = "must be one of " + strings.Join(Statuses(), ", ")
	}
	if _, known := c.outcomes[role]; role != "" && !known {
		fields["role"] = "is not an onboarding role; see GET /v1/onboarding/requirements"
	}
	if utf8.RuneCountInString(q) > 100 {
		fields["q"] = "must be at most 100 characters"
	}
	if len(fields) > 0 {
		return AdminPage{}, &ValidationError{Fields: fields}
	}
	apps, total, counts, err := s.store.List(ctx, ListFilter{Status: status, Role: role, Q: strings.TrimSpace(q), Markets: markets, Limit: size, Offset: (page - 1) * size})
	if err != nil {
		return AdminPage{}, err
	}
	out := AdminPage{Items: make([]AdminItem, 0, len(apps)), Page: page, PageSize: size, Total: total, TotalPages: (total + size - 1) / size, Counts: map[string]int{}}
	for _, st := range Statuses() {
		out.Counts[st] = counts[st]
	}
	for _, a := range apps {
		out.Items = append(out.Items, AdminItem{
			ID: a.ID, Role: a.Role, RoleLabel: c.roleLabel(a.Role), Status: a.Status, StatusLabel: StatusLabel(a.Status),
			Applicant:    ApplicantSummary{Name: a.Applicant.Name, Email: a.Applicant.Email},
			BusinessName: a.BusinessName, Agency: agencyRef(a), SubmittedAt: utcPtr(a.SubmittedAt), UpdatedAt: a.UpdatedAt.UTC(),
			DocumentCount: len(a.Documents),
		})
	}
	return out, nil
}

// ApplicantView is the applicant in the admin detail.
type ApplicantView struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	EmailVerified bool      `json:"email_verified"`
	CreatedAt     time.Time `json:"created_at"`
}

// ChecklistField and ChecklistDocument say whether each requirement is met.
type ChecklistField struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
	Provided bool   `json:"provided"`
}

type ChecklistDocument struct {
	RequirementDocument
	Provided bool `json:"provided"`
}

// Checklist is the requirement checklist of an application.
type Checklist struct {
	Fields    []ChecklistField    `json:"fields"`
	Documents []ChecklistDocument `json:"documents"`
}

// HistoryView is one transition as the admin detail shows it.
type HistoryView struct {
	From      string    `json:"from"`
	FromLabel string    `json:"from_label"`
	To        string    `json:"to"`
	ToLabel   string    `json:"to_label"`
	ActorName string    `json:"actor_name"`
	Reason    *string   `json:"reason"`
	At        time.Time `json:"at"`
}

// AdminApplication is the full application for an administrator. It
// includes the private details (contact phone, registration number).
type AdminApplication struct {
	ID           uuid.UUID      `json:"id"`
	Role         string         `json:"role"`
	RoleLabel    string         `json:"role_label"`
	Status       string         `json:"status"`
	StatusLabel  string         `json:"status_label"`
	Outcome      string         `json:"outcome"`
	MarketCode   *string        `json:"market_code"`
	Applicant    ApplicantView  `json:"applicant"`
	Details      Details        `json:"details"`
	Agency       *AgencyRef     `json:"agency"`
	Documents    []DocumentView `json:"documents"`
	Requirements Checklist      `json:"requirements"`
	History      []HistoryView  `json:"history"`
	Decision     Decision       `json:"decision"`
	Profile      *ProfileRef    `json:"profile"`
	SubmittedAt  *time.Time     `json:"submitted_at"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

func (s *Service) adminView(ctx context.Context, c *catalogue, a Application) (AdminApplication, error) {
	history, err := s.store.History(ctx, a.ID)
	if err != nil {
		return AdminApplication{}, err
	}
	out := AdminApplication{
		ID: a.ID, Role: a.Role, RoleLabel: c.roleLabel(a.Role), Status: a.Status, StatusLabel: StatusLabel(a.Status), Outcome: c.outcomes[a.Role].Outcome,
		MarketCode: nullable(a.MarketCode),
		Applicant: ApplicantView{ID: a.Applicant.ID, Name: a.Applicant.Name, Email: a.Applicant.Email,
			EmailVerified: a.Applicant.EmailVerified, CreatedAt: a.Applicant.CreatedAt.UTC()},
		Details: normalised(a.Details), Agency: agencyRef(a), Documents: c.documentViews(a.Documents),
		Requirements: Checklist{Fields: []ChecklistField{}, Documents: []ChecklistDocument{}}, History: []HistoryView{},
		Decision: Decision{Reason: nullable(a.DecisionReason), DecidedAt: utcPtr(a.DecidedAt)},
		Profile:  a.Profile, SubmittedAt: utcPtr(a.SubmittedAt), CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(),
	}
	for _, f := range FieldsFor(c.outcomes[a.Role]) {
		provided := a.Details.isSet(f.Name)
		if f.Name == FieldAreas {
			provided = len(a.Areas) > 0
		}
		if f.Name == FieldServices {
			provided = len(a.Services) > 0
		}
		out.Requirements.Fields = append(out.Requirements.Fields, ChecklistField{Name: f.Name, Required: f.Required, Provided: provided})
	}
	for _, d := range c.documentsFor(a.Role) {
		out.Requirements.Documents = append(out.Requirements.Documents, ChecklistDocument{RequirementDocument: d,
			Provided: slices.ContainsFunc(a.Documents, func(doc Document) bool { return doc.Type == d.Code })})
	}
	for _, h := range history {
		out.History = append(out.History, HistoryView{From: h.From, FromLabel: StatusLabel(h.From), To: h.To, ToLabel: StatusLabel(h.To), ActorName: h.ActorName, Reason: nullable(h.Reason), At: h.At.UTC()})
	}
	return out, nil
}

// AdminGet returns one application in full.
func (s *Service) AdminGet(ctx context.Context, p *auth.Principal, id uuid.UUID) (AdminApplication, error) {
	app, err := s.reviewable(ctx, p, id)
	if err != nil {
		return AdminApplication{}, err
	}
	c, err := s.catalogue(ctx)
	if err != nil {
		return AdminApplication{}, err
	}
	return s.adminView(ctx, c, app)
}

// AdminOpenDocument opens a document for an administrator. Every access is
// written to the audit log: who, which document, when. Never the content.
func (s *Service) AdminOpenDocument(ctx context.Context, p *auth.Principal, id, documentID uuid.UUID) (File, error) {
	app, err := s.reviewable(ctx, p, id)
	if err != nil {
		return File{}, err
	}
	f, err := s.open(ctx, app, documentID)
	if err != nil {
		return File{}, err
	}
	s.audit(ctx, "onboarding.document_accessed", slog.String("application_id", id.String()), slog.String("document_id", documentID.String()),
		slog.String("document_type", f.Document.Type), slog.String("actor_user_id", p.UserID.String()), slog.Time("at", s.clock()))
	return f, nil
}

func reason(raw string, required bool) (string, map[string]string) {
	clean, problem := cleanText(raw, 0, maxReasonRunes, true)
	n := utf8.RuneCountInString(clean)
	switch {
	case problem != "":
		return "", map[string]string{"reason": problem}
	case required && n < minReasonRunes:
		return "", map[string]string{"reason": fmt.Sprintf("is required and must be between %d and %d characters", minReasonRunes, maxReasonRunes)}
	}
	return clean, nil
}

// decide rejects an application or asks for changes.
func (s *Service) decide(ctx context.Context, p *auth.Principal, id uuid.UUID, rawReason, to, event, roleStatus string) (AdminApplication, error) {
	app, err := s.reviewable(ctx, p, id)
	if err != nil {
		return AdminApplication{}, err
	}
	text, fields := reason(rawReason, true)
	if fields != nil {
		return AdminApplication{}, &ValidationError{Fields: fields}
	}
	if app.Status != StatusSubmitted {
		return AdminApplication{}, ErrInvalidState
	}
	t := Transition{
		ApplicationID: id, From: []string{StatusSubmitted}, To: to, ActorID: p.UserID, Reason: text, At: s.clock(),
		Event: event, AccountRoleStatus: roleStatus,
	}
	if t.Audit, err = s.joinDecisionAudit(ctx, p, app, to, text, t.At); err != nil {
		return AdminApplication{}, err
	}
	if err := s.store.Transition(ctx, t); err != nil {
		return AdminApplication{}, err
	}
	s.audit(ctx, "onboarding.decided", slog.String("application_id", id.String()), slog.String("role", app.Role),
		slog.String("from", StatusSubmitted), slog.String("to", to), slog.String("actor_user_id", p.UserID.String()),
		slog.String("applicant_user_id", app.UserID.String()))
	return s.AdminGet(ctx, p, id)
}

// Reject rejects an application. The account role becomes REJECTED and
// nothing is created.
func (s *Service) Reject(ctx context.Context, p *auth.Principal, id uuid.UUID, rawReason string) (AdminApplication, error) {
	return s.decide(ctx, p, id, rawReason, StatusRejected, EventRejected, RoleStatusRejected)
}

// RequestChanges sends an application back to the applicant.
func (s *Service) RequestChanges(ctx context.Context, p *auth.Principal, id uuid.UUID, rawReason string) (AdminApplication, error) {
	return s.decide(ctx, p, id, rawReason, StatusChangesRequested, EventChangesRequested, "")
}

// Approved is the result of an approval.
type Approved struct {
	Application AdminApplication `json:"application"`
	Created     Created          `json:"created"`
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Approve approves an application and creates what its role's outcome says.
func (s *Service) Approve(ctx context.Context, p *auth.Principal, id uuid.UUID, rawNote string) (Approved, error) {
	app, err := s.reviewable(ctx, p, id)
	if err != nil {
		return Approved{}, err
	}
	created, err := s.approve(ctx, p, app, rawNote)
	if err != nil {
		return Approved{}, err
	}
	view, err := s.AdminGet(ctx, p, id)
	if err != nil {
		return Approved{}, err
	}
	return Approved{Application: view, Created: created}, nil
}

// approve applies an approval decided by p: a platform reviewer, or (for a
// join request) the OWNER or MANAGER of the agency the applicant asked to
// join. Whoever acts first wins: the store only approves a SUBMITTED
// application, the other gets ErrInvalidState.
func (s *Service) approve(ctx context.Context, p *auth.Principal, app Application, rawNote string) (Created, error) {
	id := app.ID
	note, fields := reason(rawNote, false)
	if fields != nil {
		return Created{}, &ValidationError{Fields: map[string]string{"note": fields["reason"]}}
	}
	if app.Status != StatusSubmitted {
		return Created{}, ErrInvalidState
	}
	c, err := s.catalogue(ctx)
	if err != nil {
		return Created{}, err
	}
	outcome, ok := c.outcomes[app.Role]
	if !ok || auth.IsPlatformRole(app.Role) {
		return Created{}, ErrInvalidState
	}
	if m := c.missing(app); len(m.Fields) > 0 || len(m.Documents) > 0 {
		// Requirements changed since it was submitted: it has to go back first.
		fields := map[string]string{}
		for _, f := range m.Fields {
			fields["details."+f] = "is required; request changes instead"
		}
		for _, d := range m.Documents {
			fields["documents."+d] = "is required; request changes instead"
		}
		return Created{}, &ValidationError{Fields: fields}
	}

	plan := Approval{ApplicationID: id, UserID: app.UserID, Role: app.Role, ActorID: p.UserID, Note: note, At: s.clock()}
	newID := func() (uuid.UUID, error) { return uuid.NewV7() }
	switch outcome.Outcome {
	case OutcomeOrganisation:
		if app.MarketCode == "" {
			return Created{}, &ValidationError{Fields: map[string]string{"details.country": "is required; request changes instead"}}
		}
		orgID, err := newID()
		if err != nil {
			return Created{}, err
		}
		name := deref(app.BusinessName)
		plan.Organisation = &NewOrganisation{ID: orgID, Name: name, Kind: outcome.OrganisationKind, MarketCode: app.MarketCode}
		plan.Membership = &NewMembership{OrganisationID: orgID, MarketCode: app.MarketCode, Role: outcome.MembershipRole}
		if outcome.DirectoryKind != "" {
			profileID, err := newID()
			if err != nil {
				return Created{}, err
			}
			// Public content only: no phone, no registration number, no documents.
			plan.Profile = &NewProfile{
				ID: profileID, Kind: outcome.DirectoryKind, Slug: Slugify(name), Name: name,
				Tagline: deref(app.Tagline), Description: deref(app.Description), CityCode: deref(app.City), CityName: deref(app.CityName), TradeCode: deref(app.Trade),
				Areas: app.Areas, Services: app.Services, OrganisationID: orgID, MarketCode: app.MarketCode,
			}
		}
	case OutcomeAgencyMember:
		agency, err := s.agencies.VerifiedAgency(ctx, deref(app.AgencySlug))
		if err != nil {
			return Created{}, err
		}
		if agency == nil {
			return Created{}, ErrAgencyNotVerified
		}
		plan.AgencySlug = agency.Slug
		plan.Membership = &NewMembership{OrganisationID: agency.OrganisationID, MarketCode: agency.MarketCode, Role: outcome.MembershipRole, JobTitle: deref(app.JobTitle)}
		plan.Audit = joinAudit(p, app, agency.OrganisationID, orgaudit.ActionJoinRequestApproved, StatusApproved, outcome.MembershipRole, note, plan.At)
		if outcome.DirectoryKind != "" {
			profileID, err := newID()
			if err != nil {
				return Created{}, err
			}
			user := app.UserID
			title := deref(app.JobTitle)
			if title == "" {
				title = c.roleLabel(app.Role)
			}
			plan.Profile = &NewProfile{
				ID: profileID, Kind: outcome.DirectoryKind, Slug: Slugify(app.Applicant.Name), Name: app.Applicant.Name,
				RoleTitle: title, Description: deref(app.Bio), CityCode: agency.CityCode, Areas: app.Areas,
				OrganisationID: agency.OrganisationID, AgentUserID: &user, MarketCode: agency.MarketCode,
				Specialism: outcome.Specialism,
			}
		}
	}
	// The applicant's LOGO document becomes the profile's public logo: a copy
	// under its own key, so the document itself stays a private document.
	if plan.Profile != nil {
		plan.Profile.Logo = s.promoteLogo(ctx, app, plan.At)
	}
	created, err := s.store.Approve(ctx, plan)
	if err != nil {
		if plan.Profile != nil && plan.Profile.Logo != nil {
			s.remove(ctx, plan.Profile.Logo.ObjectKey)
		}
		return Created{}, err
	}
	attrs := []slog.Attr{
		slog.String("application_id", id.String()), slog.String("role", app.Role), slog.String("from", StatusSubmitted),
		slog.String("to", StatusApproved), slog.String("actor_user_id", p.UserID.String()), slog.String("applicant_user_id", app.UserID.String()),
	}
	if created.OrganisationID != nil {
		attrs = append(attrs, slog.String("organisation_id", created.OrganisationID.String()))
	}
	if created.Profile != nil {
		attrs = append(attrs, slog.String("profile", created.Profile.Kind+"/"+created.Profile.Slug))
	}
	if created.MembershipRole != nil {
		attrs = append(attrs, slog.String("membership_role", *created.MembershipRole))
	}
	s.audit(ctx, "onboarding.decided", attrs...)
	return created, nil
}

// promoteLogo copies the application's LOGO document, if it has one, to a new
// object for the public profile and returns it; nil when there is none. The
// image is checked again. A logo that cannot be promoted does not stop the
// approval: the profile is created without one (its owner can upload a logo
// afterwards) and the reason is logged.
func (s *Service) promoteLogo(ctx context.Context, app Application, now time.Time) *directory.Logo {
	for _, d := range app.Documents {
		if d.Type != DocumentLogo {
			continue
		}
		logo, err := func() (directory.Logo, error) {
			rc, err := s.files.Open(ctx, d.ObjectKey)
			if err != nil {
				return directory.Logo{}, err
			}
			defer rc.Close()
			content, err := io.ReadAll(io.LimitReader(rc, directory.MaxLogoBytes+1))
			if err != nil {
				return directory.Logo{}, err
			}
			return directory.StoreLogo(ctx, s.files, content, now)
		}()
		if err != nil {
			s.logger.ErrorContext(ctx, "onboarding logo could not be promoted; the profile is created without a logo",
				slog.String("application_id", app.ID.String()), slog.String("error", err.Error()))
			return nil
		}
		return &logo
	}
	return nil
}

// NotificationInfo is what an applicant email needs. It deliberately has no
// document names and no private details.
type NotificationInfo struct {
	UserID    uuid.UUID
	Name      string
	Email     string
	RoleLabel string
	Status    string
	Reason    string
	Profile   *ProfileRef
	// ProfileName is the name of the public profile an approval created.
	ProfileName string
}

// Notification loads the data behind an onboarding event.
func (s *Service) Notification(ctx context.Context, applicationID uuid.UUID) (NotificationInfo, error) {
	app, err := s.store.Get(ctx, applicationID)
	if err != nil {
		return NotificationInfo{}, err
	}
	labels, err := s.vocab.RoleLabels(ctx)
	if err != nil {
		return NotificationInfo{}, err
	}
	label := labels[app.Role]
	if label == "" {
		label = app.Role
	}
	info := NotificationInfo{UserID: app.UserID, Name: app.Applicant.Name, Email: app.Applicant.Email, RoleLabel: label,
		Status: app.Status, Reason: app.DecisionReason, Profile: app.Profile}
	if app.Profile != nil {
		info.ProfileName = app.Applicant.Name
		if app.BusinessName != nil {
			info.ProfileName = *app.BusinessName
		}
	}
	return info, nil
}
