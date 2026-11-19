package developments

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Admin audit vocabulary of the review decisions.
const (
	ActionApproved    = "DEVELOPMENT_APPROVED"
	ActionRejected    = "DEVELOPMENT_REJECTED"
	TargetDevelopment = "DEVELOPMENT"
)

// ListingSource reads the listings a project links to.
type ListingSource interface {
	// Record returns a listing whatever its status, or listings.ErrNotFound.
	Record(ctx context.Context, id uuid.UUID) (listings.Record, error)
	// PublicByIDs returns the public cards of the listings among ids that
	// are public right now.
	PublicByIDs(ctx context.Context, ids []uuid.UUID) ([]listings.Listing, error)
}

// Profile is a developer's directory profile, whatever its status.
type Profile struct {
	ID         uuid.UUID
	Kind       string
	Slug       string
	Name       string
	MarketCode string
}

// Profiles reads the developers' directory profiles.
type Profiles interface {
	// DeveloperOf returns the organisation's "developers" profile whatever
	// its status, or ErrNoProfile.
	DeveloperOf(ctx context.Context, organisationID uuid.UUID) (Profile, error)
	// PublicRef returns the public card of a profile, nil when the profile
	// is not public (not ACTIVE or not verified).
	PublicRef(ctx context.Context, profileID uuid.UUID) (*listings.ProfileRef, error)
}

// Labels supplies reference labels.
type Labels interface {
	Labels(ctx context.Context, set string) (map[string]string, error)
}

// Inbox lists business enquiries (the developer's project enquiries).
type Inbox interface {
	Inbox(ctx context.Context, scope businessenquiries.InboxScope) ([]businessenquiries.InboxItem, error)
}

// Deps is what the Service needs.
type Deps struct {
	Store    Store
	Files    storage.Storage
	Listings ListingSource
	Profiles Profiles
	Labels   Labels
	Places   listings.Places
	Inbox    Inbox
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service implements the showcase: the developer's side, the reviewers'
// side and the public catalogue.
type Service struct {
	store    Store
	files    storage.Storage
	listings ListingSource
	profiles Profiles
	labels   Labels
	places   listings.Places
	inbox    Inbox
	logger   *slog.Logger
	now      func() time.Time
}

// NewService builds the Service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, files: d.Files, listings: d.Listings, profiles: d.Profiles, labels: d.Labels, places: d.Places,
		inbox: d.Inbox, logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func (s *Service) audit(ctx context.Context, event string, p *auth.Principal, id uuid.UUID, attrs ...slog.Attr) {
	if s.logger == nil {
		return
	}
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx)),
		slog.String("development_id", id.String()), slog.String("actor_user_id", p.UserID.String())}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

func (s *Service) removeObjects(ctx context.Context, photos []listings.StoredPhoto) {
	if s.files == nil {
		return
	}
	for _, p := range photos {
		for _, key := range p.Keys() {
			if err := s.files.Delete(context.WithoutCancel(ctx), key); err != nil && s.logger != nil {
				s.logger.ErrorContext(ctx, "development photo object could not be deleted", slog.String("object_key", key), slog.String("error", err.Error()))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Access
// ---------------------------------------------------------------------------

// member checks that the caller may see the organisation's projects (any
// member, STAFF included, of a listing-managing organisation) and, for a
// write, change them (OWNER, MANAGER, AGENT). Anyone else learns nothing:
// ErrNotFound.
func member(p *auth.Principal, organisationID uuid.UUID, write bool) error {
	if p == nil || !p.MayReadListingPackage(organisationID) {
		return ErrNotFound
	}
	if write && !p.MayManageListings(organisationID) {
		return ErrForbidden
	}
	return nil
}

// load returns a project of the organisation the caller may see.
func (s *Service) load(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID, write bool) (Development, error) {
	if err := member(p, organisationID, write); err != nil {
		return Development{}, err
	}
	d, err := s.store.Get(ctx, id)
	if err != nil {
		return Development{}, err
	}
	if d.OrganisationID != organisationID {
		return Development{}, ErrNotFound
	}
	return d, nil
}

// save writes a changed project and removes the files of the photos that went.
func (s *Service) save(ctx context.Context, d Development, expect time.Time, ev *adminaudit.Event) error {
	removed, err := s.store.Save(ctx, d, expect, ev)
	if err != nil {
		return err
	}
	s.removeObjects(ctx, removed)
	return nil
}

func unlocked(d Development) error {
	if d.Locked() {
		return ErrLocked
	}
	return nil
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func (s *Service) developerCard(ctx context.Context, d Development) (listings.ProfileRef, error) {
	ref, err := s.profiles.PublicRef(ctx, d.ProfileID)
	if err != nil {
		return listings.ProfileRef{}, err
	}
	if ref != nil {
		return *ref, nil
	}
	// The developer's own view of a profile that is not public: its name,
	// unverified, without a logo.
	prof, err := s.profiles.DeveloperOf(ctx, d.OrganisationID)
	if err != nil && !errors.Is(err, ErrNoProfile) {
		return listings.ProfileRef{}, err
	}
	return listings.ProfileRef{Kind: KindDevelopers, Slug: prof.Slug, Name: prof.Name}, nil
}

func (s *Service) managed(ctx context.Context, p *auth.Principal, d Development, n names, canWrite bool) (Managed, error) {
	developer, err := s.developerCard(ctx, d)
	if err != nil {
		return Managed{}, err
	}
	m := Managed{ID: d.ID, OrganisationID: d.OrganisationID, Developer: developer, Market: n.country(d.MarketCode), Slug: d.Slug, Name: d.Name,
		Summary: d.Summary, Description: d.Description, Status: statusCoded(d.Status), Completion: completionOf(d.Completion),
		ModerationStatus: d.Moderation, ReviewReason: strOrNil(d.ReviewReason), Location: n.location(d, true), CityName: strOrNil(d.CityName),
		Amenities: n.amenityList(d.Amenities), InvestmentHighlights: highlightsOf(d.Highlights),
		Video: listings.VideoOf(d.YouTubeVideoID, d.YouTubeStartSec), Photos: []ManagedPhoto{}, Phases: []ManagedPhase{},
		LinkedListings: []LinkedListing{}, Actions: []string{}, SubmittedAt: utc(d.SubmittedAt), ReviewedAt: utc(d.ReviewedAt),
		PublishedAt: utc(d.PublishedAt), CreatedAt: d.CreatedAt.UTC(), UpdatedAt: d.UpdatedAt.UTC()}
	if d.YouTubeVideoID != "" {
		u := listings.YouTubeWatchURL(d.YouTubeVideoID)
		m.YouTubeURL = &u
	}
	for _, ph := range sortedPhotos(d) {
		m.Photos = append(m.Photos, ManagedPhoto{ID: ph.ID, Approved: ph.ApprovedAt != nil, Position: ph.Position, Photo: listings.PhotoAt(ph, PhotoPath)})
	}
	available, public, err := s.availability(ctx, d)
	if err != nil {
		return Managed{}, err
	}
	for _, ph := range sortedPhases(d) {
		mp := ManagedPhase{ID: ph.ID, Name: ph.Name, Status: statusCoded(ph.Status), Completion: completionOf(ph.Completion), Position: ph.Position,
			UnitTypes: []ManagedUnitType{}}
		for _, u := range ph.UnitTypes {
			mp.UnitTypes = append(mp.UnitTypes, ManagedUnitType{UnitTypeView: unitTypeView(u, available), PhaseID: ph.ID, Position: u.Position})
		}
		m.Phases = append(m.Phases, mp)
	}
	now := s.clock()
	for _, l := range d.Links {
		ll := LinkedListing{ListingID: l.ListingID, UnitTypeID: copyID(l.UnitTypeID), Public: public[l.ListingID]}
		if r, err := s.listings.Record(ctx, l.ListingID); err == nil {
			ll.Title, ll.Slug, ll.Reference, ll.Status = r.Title, r.Slug, r.Reference, r.EffectiveStatus(now)
		} else if !errors.Is(err, listings.ErrNotFound) {
			return Managed{}, err
		}
		m.LinkedListings = append(m.LinkedListings, ll)
	}
	if d.Moderation == ModerationPublished && developer.Verified {
		path := "/v1/developments/" + strings.ToLower(d.MarketCode) + "/" + d.Slug
		m.PublicURLPath = &path
	}
	if canWrite {
		if d.Locked() {
			m.Actions = append(m.Actions, "withdraw")
		} else {
			m.Actions = append(m.Actions, "edit", "submit", "delete")
		}
	}
	return m, nil
}

// availability counts the public linked listings per unit type, and says
// which links are public.
func (s *Service) availability(ctx context.Context, d Development) (map[uuid.UUID]int, map[uuid.UUID]bool, error) {
	counts, public := map[uuid.UUID]int{}, map[uuid.UUID]bool{}
	if len(d.Links) == 0 {
		return counts, public, nil
	}
	ids := make([]uuid.UUID, 0, len(d.Links))
	for _, l := range d.Links {
		ids = append(ids, l.ListingID)
	}
	cards, err := s.listings.PublicByIDs(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	for _, c := range cards {
		public[c.ID] = true
	}
	for _, l := range d.Links {
		if public[l.ListingID] && l.UnitTypeID != nil {
			counts[*l.UnitTypeID]++
		}
	}
	return counts, public, nil
}

func (s *Service) view(ctx context.Context, p *auth.Principal, d Development) (Managed, error) {
	n, err := s.names(ctx)
	if err != nil {
		return Managed{}, err
	}
	return s.managed(ctx, p, d, n, p.MayManageListings(d.OrganisationID))
}

// reload reads a saved project back and answers it.
func (s *Service) reload(ctx context.Context, p *auth.Principal, id uuid.UUID) (Managed, error) {
	d, err := s.store.Get(ctx, id)
	if err != nil {
		return Managed{}, err
	}
	return s.view(ctx, p, d)
}

// ---------------------------------------------------------------------------
// The developer's side
// ---------------------------------------------------------------------------

// List returns the organisation's projects.
func (s *Service) List(ctx context.Context, p *auth.Principal, organisationID uuid.UUID) ([]Managed, error) {
	if err := member(p, organisationID, false); err != nil {
		return nil, err
	}
	rows, err := s.store.ByOrganisation(ctx, organisationID)
	if err != nil {
		return nil, err
	}
	n, err := s.names(ctx)
	if err != nil {
		return nil, err
	}
	out := []Managed{}
	for _, d := range rows {
		m, err := s.managed(ctx, p, d, n, p.MayManageListings(organisationID))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Get returns one project of the organisation.
func (s *Service) Get(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) (Managed, error) {
	d, err := s.load(ctx, p, organisationID, id, false)
	if err != nil {
		return Managed{}, err
	}
	return s.view(ctx, p, d)
}

// decode reads a JSON object body into its keys.
func decode(body []byte) (patch, error) {
	var p patch
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&p); err != nil || p == nil {
		return nil, &ValidationError{Fields: map[string]string{"body": "must be a JSON object"}}
	}
	return p, nil
}

// applyProject writes the project fields of a body onto d.
func (s *Service) applyProject(p patch, d *Development, n names, creating bool, fields map[string]string) {
	p.keys(projectFields, fields)
	if v, ok := p.str("name", fields); ok {
		d.Name = v
	}
	if v, ok := p.str("market", fields); ok && !creating {
		fields["market"] = "cannot change: create a new development in the other country"
	} else if ok && v != "" {
		d.MarketCode = strings.ToUpper(v)
	}
	if v, ok := p.str("slug", fields); ok {
		switch {
		case d.PublishedAt != nil && v != d.Slug:
			fields["slug"] = "cannot change once the development has been published"
		case v == "":
			d.Slug = Slugify(d.Name)
		case len(v) > MaxSlugLength || !slugRe.MatchString(v):
			fields["slug"] = "must be lower-case letters, digits and single hyphens, at most 80 characters"
		default:
			d.Slug = v
		}
	}
	if v, ok := p.str("status", fields); ok {
		d.Status = v
	}
	if v, ok := p.str("completion_estimate", fields); ok {
		d.Completion = v
	}
	if v, ok := p.str("summary", fields); ok {
		d.Summary = v
	}
	if raw, ok := p["description"]; ok {
		var v string
		if !isNull(raw) && json.Unmarshal(raw, &v) != nil {
			fields["description"] = "must be a string or null"
		}
		d.Description = strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
	}
	cityTouched := false
	if v, ok := p.str("city", fields); ok {
		d.CityCode, cityTouched = v, true
		if v != "" {
			d.CityName = ""
		}
	}
	if v, ok := p.str("city_name", fields); ok {
		d.CityName, cityTouched = v, true
		if v != "" {
			d.CityCode = ""
		}
	}
	if v, ok := p.str("suburb", fields); ok {
		d.SuburbCode = v
	} else if cityTouched && d.SuburbCode != "" && n.geo.SuburbCity[d.SuburbCode] != d.CityCode {
		d.SuburbCode = ""
	}
	lat, latOK := p.coordinate("latitude", fields)
	lon, lonOK := p.coordinate("longitude", fields)
	if latOK {
		d.Latitude = lat
	}
	if lonOK {
		d.Longitude = lon
	}
	if v, ok := p.str("location_precision", fields); ok {
		d.LocationPrecision = listings.NormalPrecision(v)
	}
	if raw, ok := p["amenities"]; ok {
		var codes []string
		if !isNull(raw) && json.Unmarshal(raw, &codes) != nil {
			fields["amenities"] = "must be a list of development_amenities codes"
		}
		d.Amenities = []string{}
		for _, c := range codes {
			c = strings.TrimSpace(c)
			if !slices.Contains(d.Amenities, c) {
				d.Amenities = append(d.Amenities, c)
			}
		}
	}
	if raw, ok := p["investment_highlights"]; ok {
		var items []Highlight
		if !isNull(raw) && json.Unmarshal(raw, &items) != nil {
			fields["investment_highlights"] = `must be a list of {"label", "value"}`
		}
		d.Highlights = []Highlight{}
		for _, h := range items {
			d.Highlights = append(d.Highlights, Highlight{Label: strings.TrimSpace(h.Label), Value: strings.TrimSpace(h.Value)})
		}
	}
	if v, ok := p.str("youtube_url", fields); ok {
		if v == "" {
			d.YouTubeVideoID, d.YouTubeStartSec = "", nil
		} else if id, start, err := listings.ParseYouTubeURL(v); err != nil {
			fields["youtube_url"] = listings.MsgNotYouTubeVideo
		} else {
			d.YouTubeVideoID, d.YouTubeStartSec = id, start
		}
	}
	s.checkProject(*d, n, fields)
}

// checkProject validates the whole project (what a body may have broken).
func (s *Service) checkProject(d Development, n names, fields map[string]string) {
	set := func(key, msg string) {
		if _, ok := fields[key]; !ok && msg != "" {
			fields[key] = msg
		}
	}
	if d.Name == "" {
		set("name", "is required")
	} else {
		set("name", checkText(d.Name, false, MaxNameLength))
	}
	set("summary", checkText(d.Summary, false, MaxSummaryLength))
	set("description", checkText(d.Description, true, MaxDescriptionLength))
	if StatusLabel(d.Status) == "" {
		set("status", "must be one of "+strings.Join(Statuses(), ", "))
	}
	if d.Completion != "" {
		if _, ok := ParseCompletion(d.Completion); !ok {
			set("completion_estimate", `must be a quarter such as "2027-Q3", a date such as "2027-06-30", or null`)
		}
	}
	if _, ok := n.geo.Countries[d.MarketCode]; !ok {
		set("market", "is not a known country; see GET /v1/countries")
	}
	switch {
	case d.CityCode != "":
		if _, ok := n.cities[d.CityCode]; !ok {
			set("city", "is not a known value; see GET /v1/reference/cities")
		} else if n.countryOf(d.CityCode) != d.MarketCode {
			set("city", "is not a city of the development's country")
		}
	case d.CityName != "":
		if n.hasCities(d.MarketCode) {
			set("city_name", "is only for a country without reference cities: send city (see GET /v1/reference/cities)")
		} else {
			set("city_name", checkText(d.CityName, false, MaxCityNameLength))
		}
	}
	if d.SuburbCode != "" {
		if _, ok := n.suburbs[d.SuburbCode]; !ok {
			set("suburb", "is not a known value; see GET /v1/reference/suburbs")
		} else if n.geo.SuburbCity[d.SuburbCode] != d.CityCode {
			set("suburb", "is not a suburb of the city")
		}
	}
	if _, bad := fields["latitude"]; !bad {
		if _, bad := fields["longitude"]; !bad {
			listings.CheckCoordinates(d.Latitude, d.Longitude, d.LocationPrecision, fields)
		}
	}
	if len(d.Amenities) > MaxAmenities {
		set("amenities", fmt.Sprintf("must have at most %d amenities", MaxAmenities))
	}
	for _, c := range d.Amenities {
		if _, ok := n.amenities[c]; !ok {
			set("amenities", "must be codes of GET /v1/reference/development_amenities ("+c+" is not one)")
		}
	}
	if len(d.Highlights) > MaxHighlights {
		set("investment_highlights", fmt.Sprintf("must have at most %d items", MaxHighlights))
	}
	for i, h := range d.Highlights {
		key := "investment_highlights[" + strconv.Itoa(i) + "]"
		switch {
		case h.Label == "" || h.Value == "":
			set(key, "needs a label and a value")
		default:
			set(key+".label", checkText(h.Label, false, MaxHighlightLabel))
			set(key+".value", checkText(h.Value, false, MaxHighlightValue))
		}
	}
}

// Create starts a project of the organisation's developer profile.
func (s *Service) Create(ctx context.Context, p *auth.Principal, organisationID uuid.UUID, body []byte) (Managed, error) {
	if err := member(p, organisationID, true); err != nil {
		return Managed{}, err
	}
	prof, err := s.profiles.DeveloperOf(ctx, organisationID)
	if err != nil {
		return Managed{}, err
	}
	in, err := decode(body)
	if err != nil {
		return Managed{}, err
	}
	n, err := s.names(ctx)
	if err != nil {
		return Managed{}, err
	}
	now := s.clock()
	user := p.UserID
	market := prof.MarketCode
	if market == "" {
		market = p.MembershipMarket(organisationID)
	}
	d := Development{ID: uuid.New(), OrganisationID: organisationID, ProfileID: prof.ID, MarketCode: market, Status: StatusPlanned,
		Moderation: ModerationDraft, LocationPrecision: listings.PrecisionExact, Amenities: []string{}, Highlights: []Highlight{},
		CreatedBy: &user, CreatedAt: now, UpdatedAt: now}
	fields := map[string]string{}
	_, explicitSlug := in["slug"]
	s.applyProject(in, &d, n, true, fields)
	if len(fields) > 0 {
		return Managed{}, &ValidationError{Fields: fields}
	}
	if !explicitSlug || d.Slug == "" {
		d.Slug = Slugify(d.Name)
	}
	base := d.Slug
	for attempt := 2; ; attempt++ {
		err = s.store.Create(ctx, d)
		if !errors.Is(err, ErrSlugTaken) || explicitSlug || attempt > 20 {
			break
		}
		d.Slug = base + "-" + strconv.Itoa(attempt)
	}
	if err != nil {
		return Managed{}, err
	}
	s.audit(ctx, "development.created", p, d.ID)
	return s.reload(ctx, p, d.ID)
}

// Update changes a project's own fields (not while locked).
func (s *Service) Update(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID, body []byte) (Managed, error) {
	d, err := s.load(ctx, p, organisationID, id, true)
	if err != nil {
		return Managed{}, err
	}
	if err := unlocked(d); err != nil {
		return Managed{}, err
	}
	in, err := decode(body)
	if err != nil {
		return Managed{}, err
	}
	n, err := s.names(ctx)
	if err != nil {
		return Managed{}, err
	}
	fields := map[string]string{}
	s.applyProject(in, &d, n, false, fields)
	if len(fields) > 0 {
		return Managed{}, &ValidationError{Fields: fields}
	}
	expect := d.UpdatedAt
	d.UpdatedAt = s.clock()
	if err := s.save(ctx, d, expect, nil); err != nil {
		return Managed{}, err
	}
	return s.reload(ctx, p, id)
}

// Delete removes a project that is neither waiting for review nor published.
func (s *Service) Delete(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) error {
	d, err := s.load(ctx, p, organisationID, id, true)
	if err != nil {
		return err
	}
	if d.Locked() {
		return &StateError{Message: "A development that is waiting for review or published cannot be deleted. Withdraw it first."}
	}
	removed, err := s.store.Delete(ctx, id, d.UpdatedAt)
	if err != nil {
		return err
	}
	s.removeObjects(ctx, removed)
	s.audit(ctx, "development.deleted", p, id)
	return nil
}

// completeness lists what a review needs that the project lacks.
func completeness(d Development) map[string]string {
	missing := map[string]string{}
	if d.Summary == "" {
		missing["summary"] = "is required"
	}
	if d.Description == "" {
		missing["description"] = "is required"
	}
	if d.CityCode == "" && d.CityName == "" {
		missing["city"] = "is required"
	}
	if len(d.Photos) == 0 {
		missing["photos"] = "add at least one photo"
	}
	for _, ph := range d.Photos {
		if strings.TrimSpace(ph.Alt) == "" {
			missing["photos"] = "every photo needs a description (alt text)"
		}
	}
	if unitTypeCount(d) == 0 {
		missing["phases"] = "add at least one phase with one unit type"
	}
	return missing
}

// Submit sends a DRAFT or REJECTED project for review.
func (s *Service) Submit(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) (Managed, error) {
	d, err := s.load(ctx, p, organisationID, id, true)
	if err != nil {
		return Managed{}, err
	}
	if d.Locked() {
		return Managed{}, &StateError{Message: "The development is already waiting for review or published."}
	}
	if missing := completeness(d); len(missing) > 0 {
		return Managed{}, &IncompleteError{Fields: missing}
	}
	now, user := s.clock(), p.UserID
	expect := d.UpdatedAt
	d.Moderation, d.SubmittedAt, d.SubmittedBy, d.UpdatedAt = ModerationPending, &now, &user, now
	if err := s.save(ctx, d, expect, nil); err != nil {
		return Managed{}, err
	}
	s.audit(ctx, "development.submitted", p, id)
	return s.reload(ctx, p, id)
}

// Withdraw takes a project waiting for review or published back to DRAFT.
func (s *Service) Withdraw(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) (Managed, error) {
	d, err := s.load(ctx, p, organisationID, id, true)
	if err != nil {
		return Managed{}, err
	}
	if !d.Locked() {
		return Managed{}, &StateError{Message: "Only a development that is waiting for review or published can be withdrawn."}
	}
	expect := d.UpdatedAt
	d.Moderation, d.UpdatedAt = ModerationDraft, s.clock()
	if err := s.save(ctx, d, expect, nil); err != nil {
		return Managed{}, err
	}
	s.audit(ctx, "development.withdrawn", p, id)
	return s.reload(ctx, p, id)
}

// ---------------------------------------------------------------------------
// Phases and unit types (they may change at any moderation status)
// ---------------------------------------------------------------------------

func (s *Service) change(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID, body []byte,
	apply func(d *Development, in patch, fields map[string]string) error) (Managed, error) {
	d, err := s.load(ctx, p, organisationID, id, true)
	if err != nil {
		return Managed{}, err
	}
	var in patch
	if body != nil {
		if in, err = decode(body); err != nil {
			return Managed{}, err
		}
	}
	fields := map[string]string{}
	if err := apply(&d, in, fields); err != nil {
		return Managed{}, err
	}
	if len(fields) > 0 {
		return Managed{}, &ValidationError{Fields: fields}
	}
	expect := d.UpdatedAt
	d.UpdatedAt = s.clock()
	if err := s.save(ctx, d, expect, nil); err != nil {
		return Managed{}, err
	}
	return s.reload(ctx, p, id)
}

func nextPosition[T any](items []T, pos func(T) int) int {
	n := 0
	for _, it := range items {
		n = max(n, pos(it)+1)
	}
	return n
}

// AddPhase adds a phase.
func (s *Service) AddPhase(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID, body []byte) (Managed, error) {
	return s.change(ctx, p, organisationID, id, body, func(d *Development, in patch, fields map[string]string) error {
		if len(d.Phases) >= MaxPhases {
			return &StateError{Message: fmt.Sprintf("A development can have at most %d phases.", MaxPhases)}
		}
		ph := Phase{ID: uuid.New(), Position: nextPosition(d.Phases, func(x Phase) int { return x.Position }), CreatedAt: s.clock(), UnitTypes: []UnitType{}}
		applyPhase(in, &ph, fields)
		d.Phases = append(d.Phases, ph)
		return nil
	})
}

// UpdatePhase changes a phase.
func (s *Service) UpdatePhase(ctx context.Context, p *auth.Principal, organisationID, id, phaseID uuid.UUID, body []byte) (Managed, error) {
	return s.change(ctx, p, organisationID, id, body, func(d *Development, in patch, fields map[string]string) error {
		i := d.PhaseIndex(phaseID)
		if i < 0 {
			return ErrPhaseNotFound
		}
		applyPhase(in, &d.Phases[i], fields)
		return nil
	})
}

// DeletePhase removes a phase and its unit types.
func (s *Service) DeletePhase(ctx context.Context, p *auth.Principal, organisationID, id, phaseID uuid.UUID) (Managed, error) {
	return s.change(ctx, p, organisationID, id, nil, func(d *Development, _ patch, _ map[string]string) error {
		i := d.PhaseIndex(phaseID)
		if i < 0 {
			return ErrPhaseNotFound
		}
		d.Phases = slices.Delete(d.Phases, i, i+1)
		return nil
	})
}

// AddUnitType adds a unit type to a phase.
func (s *Service) AddUnitType(ctx context.Context, p *auth.Principal, organisationID, id, phaseID uuid.UUID, body []byte) (Managed, error) {
	return s.change(ctx, p, organisationID, id, body, func(d *Development, in patch, fields map[string]string) error {
		i := d.PhaseIndex(phaseID)
		if i < 0 {
			return ErrPhaseNotFound
		}
		if len(d.Phases[i].UnitTypes) >= MaxUnitTypesPerPhase {
			return &StateError{Message: fmt.Sprintf("A phase can have at most %d unit types.", MaxUnitTypesPerPhase)}
		}
		if _, ok := in["phase_id"]; ok {
			fields["phase_id"] = "is the phase of the address"
		}
		u := UnitType{ID: uuid.New(), PhaseID: phaseID, CreatedAt: s.clock(),
			Position: nextPosition(d.Phases[i].UnitTypes, func(x UnitType) int { return x.Position })}
		applyUnitType(in, &u, fields)
		d.Phases[i].UnitTypes = append(d.Phases[i].UnitTypes, u)
		return nil
	})
}

// UpdateUnitType changes a unit type, and moves it to another phase of the
// project when phase_id says so.
func (s *Service) UpdateUnitType(ctx context.Context, p *auth.Principal, organisationID, id, unitTypeID uuid.UUID, body []byte) (Managed, error) {
	return s.change(ctx, p, organisationID, id, body, func(d *Development, in patch, fields map[string]string) error {
		u, from, ok := d.UnitType(unitTypeID)
		if !ok {
			return ErrUnitTypeNotFound
		}
		changed := *u
		applyUnitType(in, &changed, fields)
		to := from
		if v, ok := in.str("phase_id", fields); ok {
			target, err := uuid.Parse(v)
			if to = d.PhaseIndex(target); err != nil || to < 0 {
				fields["phase_id"] = "must be one of the development's phases"
				return nil
			}
		}
		if to == from {
			*u = changed
			return nil
		}
		if len(d.Phases[to].UnitTypes) >= MaxUnitTypesPerPhase {
			return &StateError{Message: fmt.Sprintf("A phase can have at most %d unit types.", MaxUnitTypesPerPhase)}
		}
		changed.PhaseID = d.Phases[to].ID
		d.Phases[from].UnitTypes = slices.DeleteFunc(d.Phases[from].UnitTypes, func(x UnitType) bool { return x.ID == unitTypeID })
		d.Phases[to].UnitTypes = append(d.Phases[to].UnitTypes, changed)
		return nil
	})
}

// DeleteUnitType removes a unit type (its listing links stay, without it).
func (s *Service) DeleteUnitType(ctx context.Context, p *auth.Principal, organisationID, id, unitTypeID uuid.UUID) (Managed, error) {
	return s.change(ctx, p, organisationID, id, nil, func(d *Development, _ patch, _ map[string]string) error {
		_, i, ok := d.UnitType(unitTypeID)
		if !ok {
			return ErrUnitTypeNotFound
		}
		d.Phases[i].UnitTypes = slices.DeleteFunc(d.Phases[i].UnitTypes, func(x UnitType) bool { return x.ID == unitTypeID })
		for j := range d.Links {
			if d.Links[j].UnitTypeID != nil && *d.Links[j].UnitTypeID == unitTypeID {
				d.Links[j].UnitTypeID = nil
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Listing links
// ---------------------------------------------------------------------------

// LinkListing shows a listing of the organisation as an available unit of
// the project (of one of its unit types when unitTypeID is set), or changes
// the unit type of a link.
func (s *Service) LinkListing(ctx context.Context, p *auth.Principal, organisationID, id, listingID uuid.UUID, unitTypeID *uuid.UUID) (Managed, error) {
	return s.change(ctx, p, organisationID, id, nil, func(d *Development, _ patch, _ map[string]string) error {
		r, err := s.listings.Record(ctx, listingID)
		if errors.Is(err, listings.ErrNotFound) || (err == nil && (r.OrganisationID == nil || *r.OrganisationID != organisationID)) {
			return ErrListingNotFound
		}
		if err != nil {
			return err
		}
		if unitTypeID != nil {
			if _, _, ok := d.UnitType(*unitTypeID); !ok {
				return ErrUnitTypeNotFound
			}
		}
		for i := range d.Links {
			if d.Links[i].ListingID == listingID {
				d.Links[i].UnitTypeID = copyID(unitTypeID)
				return nil
			}
		}
		d.Links = append(d.Links, Link{ListingID: listingID, UnitTypeID: copyID(unitTypeID), CreatedAt: s.clock()})
		return nil
	})
}

// UnlinkListing removes a listing from the project.
func (s *Service) UnlinkListing(ctx context.Context, p *auth.Principal, organisationID, id, listingID uuid.UUID) (Managed, error) {
	return s.change(ctx, p, organisationID, id, nil, func(d *Development, _ patch, _ map[string]string) error {
		before := len(d.Links)
		d.Links = slices.DeleteFunc(d.Links, func(l Link) bool { return l.ListingID == listingID })
		if len(d.Links) == before {
			return ErrListingNotFound
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Photos (the listing photo processing and malware scanning)
// ---------------------------------------------------------------------------

// AddPhoto stores an uploaded photo in three sizes, after the original was
// scanned for malware (422 FILE_INFECTED, 503 SCANNER_UNAVAILABLE).
func (s *Service) AddPhoto(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID, content []byte) (Managed, error) {
	d, err := s.load(ctx, p, organisationID, id, true)
	if err != nil {
		return Managed{}, err
	}
	if err := unlocked(d); err != nil {
		return Managed{}, err
	}
	if len(d.Photos) >= MaxPhotos {
		return Managed{}, listings.ErrPhotoLimit
	}
	if len(content) == 0 {
		return Managed{}, &ValidationError{Fields: map[string]string{"file": "is required and must not be empty"}}
	}
	if s.files == nil {
		return Managed{}, errors.New("developments: no file storage configured")
	}
	processed, err := listings.ProcessPhoto(content)
	if err != nil {
		return Managed{}, err
	}
	if err := storage.Screen(ctx, s.files, "development_photo", content); err != nil {
		return Managed{}, err
	}
	now, user := s.clock(), p.UserID
	photo := listings.StoredPhoto{ID: uuid.New(), Checksum: processed.Checksum, Width: processed.Width, Height: processed.Height,
		UploadedBy: &user, CreatedAt: now, Position: nextPosition(d.Photos, func(x listings.StoredPhoto) int { return x.Position })}
	for _, v := range processed.Variants {
		key, err := storage.NewKey(PhotoKeyPrefix)
		if err == nil {
			err = s.files.Put(ctx, key, bytes.NewReader(v.Data), int64(len(v.Data)), "image/jpeg")
		}
		if err != nil {
			s.removeObjects(ctx, []listings.StoredPhoto{photo})
			return Managed{}, fmt.Errorf("store photo: %w", err)
		}
		photo.Variants = append(photo.Variants, listings.StoredVariant{Size: v.Size, Key: key, Width: v.Width, Height: v.Height, Bytes: int64(len(v.Data))})
	}
	expect := d.UpdatedAt
	d.Photos = append(d.Photos, photo)
	d.UpdatedAt = now
	if err := s.save(ctx, d, expect, nil); err != nil {
		s.removeObjects(ctx, []listings.StoredPhoto{photo})
		return Managed{}, err
	}
	s.audit(ctx, "development.photo_added", p, id, slog.String("photo_id", photo.ID.String()))
	return s.reload(ctx, p, id)
}

func photoIndex(d Development, photoID uuid.UUID) int {
	return slices.IndexFunc(d.Photos, func(ph listings.StoredPhoto) bool { return ph.ID == photoID })
}

func (s *Service) photoChange(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID,
	apply func(d *Development) error) (Managed, error) {
	return s.change(ctx, p, organisationID, id, nil, func(d *Development, _ patch, _ map[string]string) error {
		if err := unlocked(*d); err != nil {
			return err
		}
		return apply(d)
	})
}

// SetPhotoAlt changes a photo's description.
func (s *Service) SetPhotoAlt(ctx context.Context, p *auth.Principal, organisationID, id, photoID uuid.UUID, alt string) (Managed, error) {
	return s.photoChange(ctx, p, organisationID, id, func(d *Development) error {
		i := photoIndex(*d, photoID)
		if i < 0 {
			return listings.ErrPhotoNotFound
		}
		alt = strings.TrimSpace(alt)
		if problem := checkText(alt, false, listings.MaxPhotoAltLength); problem != "" {
			return &ValidationError{Fields: map[string]string{"alt": problem}}
		}
		d.Photos[i].Alt = alt
		return nil
	})
}

// ReorderPhotos puts the photos in the given order (every photo exactly once).
func (s *Service) ReorderPhotos(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID, ids []uuid.UUID) (Managed, error) {
	return s.photoChange(ctx, p, organisationID, id, func(d *Development) error {
		bad := &ValidationError{Fields: map[string]string{"photo_ids": "must list every photo of the development exactly once"}}
		if len(ids) != len(d.Photos) {
			return bad
		}
		for pos, photoID := range ids {
			i := photoIndex(*d, photoID)
			if i < 0 || slices.Index(ids, photoID) != pos {
				return bad
			}
			d.Photos[i].Position = pos
		}
		return nil
	})
}

// DeletePhoto removes a photo and its stored files.
func (s *Service) DeletePhoto(ctx context.Context, p *auth.Principal, organisationID, id, photoID uuid.UUID) (Managed, error) {
	return s.photoChange(ctx, p, organisationID, id, func(d *Development) error {
		i := photoIndex(*d, photoID)
		if i < 0 {
			return listings.ErrPhotoNotFound
		}
		d.Photos = slices.Delete(d.Photos, i, i+1)
		return nil
	})
}

// OpenPhoto returns one size of a photo: to anyone while the project is
// public and the photo approved; otherwise only to the organisation's
// members and to the reviewers of its country (p may be nil).
func (s *Service) OpenPhoto(ctx context.Context, p *auth.Principal, photoID uuid.UUID, size, version string) (listings.PhotoFile, error) {
	devID, photo, err := s.store.PhotoByID(ctx, photoID)
	if err != nil {
		return listings.PhotoFile{}, err
	}
	variant, ok := photo.Variant(size)
	if !photo.Uploaded() || !ok || s.files == nil {
		return listings.PhotoFile{}, listings.ErrPhotoNotFound
	}
	d, err := s.store.Get(ctx, devID)
	if err != nil {
		return listings.PhotoFile{}, listings.ErrPhotoNotFound
	}
	public := false
	if d.Moderation == ModerationPublished && photo.ApprovedAt != nil {
		if _, err := s.store.PublicBySlug(ctx, d.MarketCode, d.Slug); err == nil {
			public = true
		}
	}
	if !public && (p == nil || !(p.MayReadListingPackage(d.OrganisationID) || p.MayReviewListing(d.MarketCode))) {
		return listings.PhotoFile{}, listings.ErrPhotoNotFound
	}
	rc, err := s.files.Open(ctx, variant.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return listings.PhotoFile{}, listings.ErrPhotoNotFound
	}
	if err != nil {
		return listings.PhotoFile{}, fmt.Errorf("open photo: %w", err)
	}
	return listings.PhotoFile{Content: rc, Size: variant.Bytes, ETag: `"` + photo.Checksum[:min(32, len(photo.Checksum))] + "-" + size + `"`,
		Public: public, Versioned: version != "" && version == listings.LogoVersion(photo.Checksum)}, nil
}

// ---------------------------------------------------------------------------
// Enquiries
// ---------------------------------------------------------------------------

// Enquiries lists the business enquiries about a project: OWNER and MANAGER
// only (the inbox rule: they carry the customers' contact details).
func (s *Service) Enquiries(ctx context.Context, p *auth.Principal, organisationID, id uuid.UUID) ([]businessenquiries.InboxItem, error) {
	if _, err := s.load(ctx, p, organisationID, id, false); err != nil {
		return nil, err
	}
	if !p.MayAssignWork(organisationID) {
		return nil, ErrForbidden
	}
	if s.inbox == nil {
		return []businessenquiries.InboxItem{}, nil
	}
	return s.inbox.Inbox(ctx, businessenquiries.InboxScope{UserID: p.UserID, ManagedOrganisationIDs: []uuid.UUID{organisationID},
		DevelopmentID: &id, Limit: MaxEnquiriesShown})
}

// ---------------------------------------------------------------------------
// The reviewers' side
// ---------------------------------------------------------------------------

// Queue returns one page of the review queue, scoped to the reviewer's countries.
func (s *Service) Queue(ctx context.Context, p *auth.Principal, moderation, market string, page, size int) (QueuePage, error) {
	codes, all, ok := p.ListingReviewMarkets()
	if !ok {
		return QueuePage{}, ErrForbidden
	}
	if moderation == "" {
		moderation = ModerationPending
	}
	if !slices.Contains(ModerationStatuses(), moderation) {
		return QueuePage{}, &ValidationError{Fields: map[string]string{"status": "must be one of " + strings.Join(ModerationStatuses(), ", ")}}
	}
	f := QueueFilter{Moderation: moderation, Market: strings.ToUpper(market), Limit: size, Offset: (page - 1) * size}
	if !all {
		f.Markets = append([]string{}, codes...)
	}
	rows, total, err := s.store.Queue(ctx, f)
	if err != nil {
		return QueuePage{}, err
	}
	n, err := s.names(ctx)
	if err != nil {
		return QueuePage{}, err
	}
	out := QueuePage{Items: []Managed{}, Page: page, PageSize: size, Total: total, TotalPages: (total + size - 1) / size}
	for _, d := range rows {
		m, err := s.managed(ctx, p, d, n, false)
		if err != nil {
			return QueuePage{}, err
		}
		out.Items = append(out.Items, m)
	}
	return out, nil
}

func (s *Service) reviewable(ctx context.Context, p *auth.Principal, id uuid.UUID) (Development, error) {
	d, err := s.store.Get(ctx, id)
	if err != nil {
		return Development{}, err
	}
	if !p.MayReviewListing(d.MarketCode) {
		return Development{}, ErrNotFound
	}
	return d, nil
}

func (s *Service) adminView(ctx context.Context, p *auth.Principal, d Development) (Managed, error) {
	n, err := s.names(ctx)
	if err != nil {
		return Managed{}, err
	}
	return s.managed(ctx, p, d, n, false)
}

// AdminGet returns a project as its reviewer sees it.
func (s *Service) AdminGet(ctx context.Context, p *auth.Principal, id uuid.UUID) (Managed, error) {
	d, err := s.reviewable(ctx, p, id)
	if err != nil {
		return Managed{}, err
	}
	return s.adminView(ctx, p, d)
}

// Decisions.
const (
	DecisionApprove = "approve"
	DecisionReject  = "reject"
)

// Decide approves or rejects a project waiting for review. A rejection
// needs a reason. The decision is written to the administrators' audit
// trail in the same transaction.
func (s *Service) Decide(ctx context.Context, p *auth.Principal, id uuid.UUID, decision, reason string) (Managed, error) {
	d, err := s.reviewable(ctx, p, id)
	if err != nil {
		return Managed{}, err
	}
	selfReview := p.MembershipRole(d.OrganisationID) != "" ||
		(d.CreatedBy != nil && *d.CreatedBy == p.UserID) || (d.SubmittedBy != nil && *d.SubmittedBy == p.UserID)
	if selfReview {
		s.audit(ctx, "development.self_review_refused", p, id, slog.String("decision", decision))
		return Managed{}, ErrSelfReview
	}
	if d.Moderation != ModerationPending {
		return Managed{}, &StateError{Message: "This development is not waiting for review."}
	}
	now, reviewer := s.clock(), p.UserID
	expect := d.UpdatedAt
	before := map[string]any{"moderation_status": d.Moderation}
	var action string
	switch decision {
	case DecisionApprove:
		reason = ""
		d.Moderation, d.PublishedAt, d.ReviewReason = ModerationPublished, &now, ""
		for i := range d.Photos {
			if d.Photos[i].ApprovedAt == nil {
				d.Photos[i].ApprovedAt = &now
			}
		}
		action = ActionApproved
	case DecisionReject:
		reason = strings.TrimSpace(reason)
		switch {
		case reason == "":
			return Managed{}, &ValidationError{Fields: map[string]string{"reason": "is required"}}
		default:
			if problem := listings.PlainText(reason, true, MaxReasonLength); problem != "" {
				return Managed{}, &ValidationError{Fields: map[string]string{"reason": problem}}
			}
		}
		d.Moderation, d.ReviewReason = ModerationRejected, reason
		action = ActionRejected
	default:
		return Managed{}, &ValidationError{Fields: map[string]string{"decision": "must be approve or reject"}}
	}
	d.ReviewedAt, d.ReviewedBy, d.UpdatedAt = &now, &reviewer, now
	ev := adminaudit.New(p.UserID, p.Name, action, TargetDevelopment, d.ID.String(), d.Name+" ("+d.MarketCode+")", reason,
		before, map[string]any{"moderation_status": d.Moderation}, now)
	if err := s.save(ctx, d, expect, &ev); err != nil {
		return Managed{}, err
	}
	s.audit(ctx, "development.review_"+decision, p, id)
	saved, err := s.store.Get(ctx, id)
	if err != nil {
		return Managed{}, err
	}
	return s.adminView(ctx, p, saved)
}

// ---------------------------------------------------------------------------
// The public catalogue
// ---------------------------------------------------------------------------

// Query is a public list request.
type Query struct {
	Market string
	City   string
	Status string
	Q      string
	Page   int
	Size   int
}

// refs caches the public developer cards of one answer.
type refs struct {
	s    *Service
	seen map[uuid.UUID]*listings.ProfileRef
}

func (r *refs) get(ctx context.Context, profileID uuid.UUID) (*listings.ProfileRef, error) {
	if ref, ok := r.seen[profileID]; ok {
		return ref, nil
	}
	ref, err := r.s.profiles.PublicRef(ctx, profileID)
	if err != nil {
		return nil, err
	}
	r.seen[profileID] = ref
	return ref, nil
}

func (s *Service) cards(ctx context.Context, rows []Development, n names) ([]Card, error) {
	r := refs{s: s, seen: map[uuid.UUID]*listings.ProfileRef{}}
	out := []Card{}
	for _, d := range rows {
		ref, err := r.get(ctx, d.ProfileID)
		if err != nil {
			return nil, err
		}
		if ref == nil {
			continue // the profile stopped being public since the query
		}
		out = append(out, n.card(d, *ref))
	}
	return out, nil
}

// PublicList returns one page of public projects.
func (s *Service) PublicList(ctx context.Context, q Query) (Page, error) {
	n, err := s.names(ctx)
	if err != nil {
		return Page{}, err
	}
	fields := map[string]string{}
	if q.Market != "" {
		if _, ok := n.geo.Countries[q.Market]; !ok {
			fields["market"] = "is not a known country; see GET /v1/countries"
		}
	}
	if q.City != "" {
		if _, ok := n.cities[q.City]; !ok {
			fields["city"] = "is not a known value; see GET /v1/reference/cities"
		}
	}
	if q.Status != "" && StatusLabel(q.Status) == "" {
		fields["status"] = "must be one of " + strings.Join(Statuses(), ", ")
	}
	if len(fields) > 0 {
		return Page{}, &ValidationError{Fields: fields}
	}
	f := PublicFilter{Market: q.Market, City: q.City, Status: q.Status, Q: q.Q, Limit: q.Size, Offset: (q.Page - 1) * q.Size}
	if q.Q != "" {
		lower := strings.ToLower(q.Q)
		for _, set := range []map[string]string{n.cities, n.suburbs} {
			for code, label := range set {
				if strings.Contains(strings.ToLower(label), lower) {
					f.QPlaces = append(f.QPlaces, code)
				}
			}
		}
		slices.Sort(f.QPlaces)
	}
	rows, total, err := s.store.Public(ctx, f)
	if err != nil {
		return Page{}, err
	}
	items, err := s.cards(ctx, rows, n)
	if err != nil {
		return Page{}, err
	}
	return Page{Items: items, Page: q.Page, PageSize: q.Size, Total: total, TotalPages: (total + q.Size - 1) / q.Size}, nil
}

// PublicGet returns a public project page.
func (s *Service) PublicGet(ctx context.Context, market, slug string) (Detail, error) {
	d, err := s.store.PublicBySlug(ctx, strings.ToUpper(market), slug)
	if err != nil {
		return Detail{}, err
	}
	ref, err := s.profiles.PublicRef(ctx, d.ProfileID)
	if err != nil {
		return Detail{}, err
	}
	if ref == nil {
		return Detail{}, ErrNotFound
	}
	n, err := s.names(ctx)
	if err != nil {
		return Detail{}, err
	}
	available, _, err := s.availability(ctx, d)
	if err != nil {
		return Detail{}, err
	}
	out := Detail{Card: n.card(d, *ref), Description: d.Description, Amenities: n.amenityList(d.Amenities),
		InvestmentHighlights: highlightsOf(d.Highlights), Photos: publicPhotos(d), Video: listings.VideoOf(d.YouTubeVideoID, d.YouTubeStartSec),
		Phases: []PhaseView{}, AvailableUnits: []AvailableUnit{}}
	for _, ph := range sortedPhases(d) {
		pv := PhaseView{ID: ph.ID, Name: ph.Name, Status: statusCoded(ph.Status), Completion: completionOf(ph.Completion), UnitTypes: []UnitTypeView{}}
		for _, u := range ph.UnitTypes {
			pv.UnitTypes = append(pv.UnitTypes, unitTypeView(u, available))
		}
		out.Phases = append(out.Phases, pv)
	}
	if len(d.Links) > 0 {
		ids := make([]uuid.UUID, 0, len(d.Links))
		for _, l := range d.Links {
			ids = append(ids, l.ListingID)
		}
		cards, err := s.listings.PublicByIDs(ctx, ids)
		if err != nil {
			return Detail{}, err
		}
		byID := map[uuid.UUID]listings.Listing{}
		for _, c := range cards {
			byID[c.ID] = c
		}
		for _, l := range d.Links {
			if c, ok := byID[l.ListingID]; ok {
				out.AvailableUnits = append(out.AvailableUnits, AvailableUnit{UnitTypeID: copyID(l.UnitTypeID), Listing: c})
			}
		}
	}
	return out, nil
}

// ProfileProjects returns the public projects of a developer profile (the
// directory profile's "projects").
func (s *Service) ProfileProjects(ctx context.Context, profileID uuid.UUID) ([]Card, error) {
	rows, err := s.store.PublicByProfile(ctx, profileID)
	if err != nil {
		return nil, err
	}
	n, err := s.names(ctx)
	if err != nil {
		return nil, err
	}
	return s.cards(ctx, rows, n)
}

// SitemapEntries returns every public project's address.
func (s *Service) SitemapEntries(ctx context.Context) ([]Entry, error) {
	return s.store.PublicEntries(ctx)
}

// ResolveDevelopment implements businessenquiries.DevelopmentResolver: the
// public project an enquiry names, its developer and the names it quotes.
func (s *Service) ResolveDevelopment(ctx context.Context, market, slug string, unitTypeID *uuid.UUID) (businessenquiries.DevelopmentTarget, error) {
	d, err := s.store.PublicBySlug(ctx, strings.ToUpper(market), slug)
	if errors.Is(err, ErrNotFound) {
		return businessenquiries.DevelopmentTarget{}, businessenquiries.ErrDevelopmentNotFound
	}
	if err != nil {
		return businessenquiries.DevelopmentTarget{}, err
	}
	ref, err := s.profiles.PublicRef(ctx, d.ProfileID)
	if err != nil {
		return businessenquiries.DevelopmentTarget{}, err
	}
	if ref == nil {
		return businessenquiries.DevelopmentTarget{}, businessenquiries.ErrDevelopmentNotFound
	}
	out := businessenquiries.DevelopmentTarget{ID: d.ID, Market: d.MarketCode, Slug: d.Slug, Name: d.Name, ProfileKind: ref.Kind, ProfileSlug: ref.Slug}
	if unitTypeID != nil {
		u, _, ok := d.UnitType(*unitTypeID)
		if !ok {
			return businessenquiries.DevelopmentTarget{}, businessenquiries.ErrUnitTypeNotFound
		}
		id := u.ID
		out.UnitTypeID, out.UnitTypeName = &id, u.Name
	}
	return out, nil
}
