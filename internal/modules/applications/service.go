package applications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// ListingInfo is a listing as this module needs it, whatever its status.
type ListingInfo struct {
	ID             uuid.UUID
	Slug           string
	Reference      string
	Title          string
	Category       string
	MarketCode     string
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID
	// Public: published, unexpired, from a verified advertiser (now).
	Public bool
	// Card is the listing's public card (its saved-listings snapshot).
	Card saved.Snapshot
}

// ShortStay is the category of the listings that take no applications.
const ShortStay = "short-stay"

// Eligible reports whether a listing takes applications: sale, rent and
// commercial listings do; short stays do not.
func (l ListingInfo) Eligible() bool { return l.Category != ShortStay }

// Book returns who receives the listing's applications.
func (l ListingInfo) Book() (Book, bool) {
	switch {
	case l.OrganisationID != nil:
		return OrganisationBook(*l.OrganisationID), true
	case l.SellerUserID != nil:
		return PersonalBook(*l.SellerUserID), true
	}
	return Book{}, false
}

// Listings reads listings.
type Listings interface {
	// Listings returns the listings among ids that exist, whatever their status.
	Listings(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]ListingInfo, error)
	// BySlug returns a PUBLIC listing (ErrListingNotFound otherwise).
	BySlug(ctx context.Context, slug string) (ListingInfo, error)
	// AgentListingIDs returns the IDs of the organisation's listings the
	// member is the assigned agent of.
	AgentListingIDs(ctx context.Context, organisationID, agentUserID uuid.UUID) ([]uuid.UUID, error)
}

// People reads an account's current name and email address.
type People func(ctx context.Context, id uuid.UUID) (name, email string, ok bool, err error)

// Deps are the service's collaborators.
type Deps struct {
	Store    Store
	Files    storage.Storage
	Listings Listings
	People   People
	Logger   *slog.Logger
	Now      func() time.Time
}

// Service implements forms, intake and applications.
type Service struct {
	store    Store
	files    storage.Storage
	listings Listings
	people   People
	logger   *slog.Logger
	now      func() time.Time
}

// NewService builds the Service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, files: d.Files, listings: d.Listings, people: d.People, logger: d.Logger, now: d.Now}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func (s *Service) audit(ctx context.Context, event string, attrs ...slog.Attr) {
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx))}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

func (s *Service) listing(ctx context.Context, id uuid.UUID) (ListingInfo, error) {
	got, err := s.listings.Listings(ctx, []uuid.UUID{id})
	if err != nil {
		return ListingInfo{}, err
	}
	l, ok := got[id]
	if !ok {
		return ListingInfo{}, ErrListingNotFound
	}
	return l, nil
}

// ---------------------------------------------------------------------------
// Forms
// ---------------------------------------------------------------------------

// formAccess resolves the book addressed by bookID and whether the caller
// may manage it: ErrFormNotFound for somebody outside it, ErrForbidden for
// a member who may not read forms (STAFF).
func formAccess(p *auth.Principal, bookID uuid.UUID) (Book, bool, error) {
	switch {
	case p == nil:
		return Book{}, false, ErrUnauthenticated
	case bookID == p.UserID && p.IsDirectOwner():
		return PersonalBook(bookID), true, nil
	case p.MembershipRole(bookID) == "":
		return Book{}, false, ErrFormNotFound
	case !p.MayReadForms(bookID):
		return Book{}, false, ErrForbidden
	}
	return OrganisationBook(bookID), p.MayManageForms(bookID), nil
}

// FormInput is the body of POST and PATCH .../forms. Every field is
// optional on PATCH.
type FormInput struct {
	Name          *string    `json:"name"`
	MarketCode    *string    `json:"market_code"`
	Purpose       *string    `json:"purpose"`
	PrivacyNotice *string    `json:"privacy_notice"`
	RetentionDays *int       `json:"retention_days"`
	ExpiresAt     *time.Time `json:"expires_at"`
	// ClearExpiry is set by the handler when expires_at was sent as null.
	ClearExpiry bool       `json:"-"`
	Sections    *[]Section `json:"sections"`
}

// apply copies the input onto f and validates the result. create requires
// the mandatory fields.
func (in FormInput) apply(f *Form, create bool) error {
	errs := fieldErrors{}
	text := func(path string, v *string, dst *string, max int, required bool) {
		if v == nil {
			if create && required {
				errs.add(path, "is required")
			}
			return
		}
		t := strings.TrimSpace(*v)
		switch {
		case required && t == "":
			errs.add(path, "is required")
		case runes(t) > max:
			errs.add(path, fmt.Sprintf("must be at most %d characters", max))
		}
		*dst = t
	}
	text("name", in.Name, &f.Name, MaxNameLength, true)
	text("purpose", in.Purpose, &f.Purpose, MaxPurposeLength, true)
	text("privacy_notice", in.PrivacyNotice, &f.PrivacyNotice, MaxPrivacyNoticeLength, true)
	if in.MarketCode != nil {
		code := strings.ToUpper(strings.TrimSpace(*in.MarketCode))
		if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
			errs.add("market_code", "must be a two-letter country code")
		}
		f.MarketCode = code
	} else if create {
		errs.add("market_code", "is required")
	}
	if in.RetentionDays != nil {
		if *in.RetentionDays < MinRetentionDays || *in.RetentionDays > MaxRetentionDays {
			errs.add("retention_days", fmt.Sprintf("must be between %d and %d days", MinRetentionDays, MaxRetentionDays))
		}
		f.RetentionDays = *in.RetentionDays
	} else if create {
		errs.add("retention_days", "is required")
	}
	if in.ClearExpiry {
		f.ExpiresAt = nil
	} else if in.ExpiresAt != nil {
		t := in.ExpiresAt.UTC()
		f.ExpiresAt = &t
	}
	if in.Sections != nil {
		f.Sections = append([]Section{}, (*in.Sections)...)
	}
	if f.Sections == nil {
		f.Sections = []Section{}
	}
	validateSections(errs, f.Sections, false)
	return errs.err()
}

func (s *Service) ownedForm(ctx context.Context, book Book, id uuid.UUID) (Form, error) {
	f, err := s.store.Form(ctx, id)
	if err != nil {
		return Form{}, err
	}
	if !f.Book().Same(book) {
		return Form{}, ErrFormNotFound
	}
	return f, nil
}

// ListForms lists a book's forms (every version), newest first.
func (s *Service) ListForms(ctx context.Context, p *auth.Principal, bookID uuid.UUID, status, market string, familyID *uuid.UUID) ([]FormView, error) {
	book, manage, err := formAccess(p, bookID)
	if err != nil {
		return nil, err
	}
	list, err := s.store.Forms(ctx, book, status, market, familyID)
	if err != nil {
		return nil, err
	}
	out := make([]FormView, 0, len(list))
	for _, f := range list {
		out = append(out, formView(f, manage))
	}
	return out, nil
}

// GetForm returns one form and its family's versions.
func (s *Service) GetForm(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (FormView, []VersionRef, error) {
	book, manage, err := formAccess(p, bookID)
	if err != nil {
		return FormView{}, nil, err
	}
	f, err := s.ownedForm(ctx, book, id)
	if err != nil {
		return FormView{}, nil, err
	}
	family, err := s.store.Forms(ctx, book, "", "", &f.FamilyID)
	if err != nil {
		return FormView{}, nil, err
	}
	versions := make([]VersionRef, 0, len(family))
	for _, v := range family {
		versions = append(versions, VersionRef{ID: v.ID, Version: v.Version, Status: v.Status, PublishedAt: utcPtr(v.PublishedAt), SubmissionCount: v.Submissions})
	}
	return formView(f, manage), versions, nil
}

// CreateForm creates version 1 of a new family, as a DRAFT.
func (s *Service) CreateForm(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in FormInput) (FormView, error) {
	book, manage, err := formAccess(p, bookID)
	if err != nil {
		return FormView{}, err
	}
	if !manage {
		return FormView{}, ErrForbidden
	}
	now := s.clock()
	f := Form{ID: newID(), Version: 1, Status: FormDraft, OrganisationID: book.OrganisationID, OwnerUserID: book.UserID,
		CreatedBy: &p.UserID, CreatedByName: p.Name, CreatedAt: now, UpdatedAt: now}
	f.FamilyID = f.ID
	if err := in.apply(&f, true); err != nil {
		return FormView{}, err
	}
	if err := s.store.CreateForm(ctx, f); err != nil {
		return FormView{}, err
	}
	s.audit(ctx, "application_form.created", slog.String("form_id", f.ID.String()), slog.String("book_id", book.ID().String()),
		slog.String("actor_user_id", p.UserID.String()))
	return formView(f, true), nil
}

// EditForm changes a DRAFT in place, or creates the family's next DRAFT
// from a PUBLISHED version (created=true). A RETIRED version cannot be
// edited, nor can a version with submissions (always guaranteed: only a
// DRAFT changes, and a draft has none).
func (s *Service) EditForm(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in FormInput) (FormView, bool, error) {
	book, manage, err := formAccess(p, bookID)
	if err != nil {
		return FormView{}, false, err
	}
	if !manage {
		return FormView{}, false, ErrForbidden
	}
	current, err := s.ownedForm(ctx, book, id)
	if err != nil {
		return FormView{}, false, err
	}
	now := s.clock()
	switch current.Status {
	case FormRetired:
		return FormView{}, false, ErrFormNotEditable
	case FormPublished:
		next := current
		next.ID, next.Status, next.Submissions = newID(), FormDraft, 0
		next.PublishedAt, next.RetiredAt = nil, nil
		next.CreatedBy, next.CreatedByName, next.CreatedAt, next.UpdatedAt = &p.UserID, p.Name, now, now
		next.Sections = append([]Section{}, current.Sections...)
		if err := in.apply(&next, false); err != nil {
			return FormView{}, false, err
		}
		created, err := s.store.CreateVersion(ctx, next)
		if err != nil {
			return FormView{}, false, err
		}
		s.audit(ctx, "application_form.new_version", slog.String("form_id", created.ID.String()), slog.Int("version", created.Version),
			slog.String("from_form_id", current.ID.String()))
		return formView(created, true), true, nil
	}
	if current.Submissions > 0 {
		return FormView{}, false, ErrFormNotEditable
	}
	if err := in.apply(&current, false); err != nil {
		return FormView{}, false, err
	}
	current.UpdatedAt = now
	if err := s.store.UpdateDraft(ctx, current); err != nil {
		if errors.Is(err, ErrFormLocked) {
			return FormView{}, false, ErrFormNotEditable
		}
		return FormView{}, false, err
	}
	return formView(current, true), false, nil
}

// PublishForm publishes a complete DRAFT: the family's PUBLISHED version
// is retired and the intakes that pointed at it move to this one.
func (s *Service) PublishForm(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (FormView, error) {
	book, manage, err := formAccess(p, bookID)
	if err != nil {
		return FormView{}, err
	}
	if !manage {
		return FormView{}, ErrForbidden
	}
	f, err := s.ownedForm(ctx, book, id)
	if err != nil {
		return FormView{}, err
	}
	if f.Status != FormDraft {
		return FormView{}, ErrFormState
	}
	errs := fieldErrors{}
	if f.Purpose == "" {
		errs.add("purpose", "is required")
	}
	if f.PrivacyNotice == "" {
		errs.add("privacy_notice", "is required")
	}
	if f.ExpiresAt != nil && !f.ExpiresAt.After(s.clock()) {
		errs.add("expires_at", "must be in the future to publish")
	}
	validateSections(errs, f.Sections, true)
	if err := errs.err(); err != nil {
		return FormView{}, err
	}
	out, err := s.store.Publish(ctx, id, s.clock())
	if err != nil {
		return FormView{}, err
	}
	s.audit(ctx, "application_form.published", slog.String("form_id", id.String()), slog.Int("version", out.Version),
		slog.Int("sensitive_warnings", len(Warnings(out.Sections))), slog.String("actor_user_id", p.UserID.String()))
	return formView(out, true), nil
}

// RetireForm retires a PUBLISHED or DRAFT version.
func (s *Service) RetireForm(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (FormView, error) {
	book, manage, err := formAccess(p, bookID)
	if err != nil {
		return FormView{}, err
	}
	if !manage {
		return FormView{}, ErrForbidden
	}
	f, err := s.ownedForm(ctx, book, id)
	if err != nil {
		return FormView{}, err
	}
	if f.Status == FormRetired {
		return FormView{}, ErrFormState
	}
	out, err := s.store.Retire(ctx, id, s.clock())
	if err != nil {
		return FormView{}, err
	}
	s.audit(ctx, "application_form.retired", slog.String("form_id", id.String()), slog.String("actor_user_id", p.UserID.String()))
	return formView(out, true), nil
}

// ---------------------------------------------------------------------------
// Intake (/v1/me/listings/{id}/applications-intake)
// ---------------------------------------------------------------------------

// IntakeInput is the body of PUT .../applications-intake.
type IntakeInput struct {
	FormID   *string    `json:"form_id"`
	Status   string     `json:"status"`
	ClosesAt *time.Time `json:"closes_at"`
}

func (s *Service) intakeView(ctx context.Context, l ListingInfo) (IntakeView, error) {
	v := IntakeView{ListingID: l.ID, Status: IntakeNone, StoredStatus: IntakeNone, Eligible: l.Eligible()}
	in, ok, err := s.store.Intake(ctx, l.ID)
	if err != nil || !ok {
		return v, err
	}
	f, err := s.store.Form(ctx, in.FormID)
	if err != nil {
		return IntakeView{}, err
	}
	status, reason := Effective(in, f, s.clock())
	formID, version, name, updated := f.ID, f.Version, f.Name, in.UpdatedAt.UTC()
	v.FormID, v.FormVersion, v.FormName, v.UpdatedAt = &formID, &version, &name, &updated
	v.Status, v.StoredStatus, v.ClosesAt, v.ClosedReason = status, in.Status, utcPtr(in.ClosesAt), strPtr(reason)
	return v, nil
}

// GetIntake returns a listing's intake, for whoever may manage the listing.
func (s *Service) GetIntake(ctx context.Context, p *auth.Principal, listingID uuid.UUID) (IntakeView, error) {
	if p == nil {
		return IntakeView{}, ErrUnauthenticated
	}
	l, err := s.listing(ctx, listingID)
	if err != nil {
		return IntakeView{}, err
	}
	if !p.MayManageListing(l.OrganisationID, l.SellerUserID) {
		return IntakeView{}, ErrListingNotFound
	}
	return s.intakeView(ctx, l)
}

// PutIntake attaches a PUBLISHED form of the listing's organisation (or
// direct owner) and market and sets the intake status; status NONE with no
// form detaches. Whoever may edit the listing may change it.
func (s *Service) PutIntake(ctx context.Context, p *auth.Principal, listingID uuid.UUID, in IntakeInput) (IntakeView, error) {
	if p == nil {
		return IntakeView{}, ErrUnauthenticated
	}
	l, err := s.listing(ctx, listingID)
	if err != nil {
		return IntakeView{}, err
	}
	if !p.MayEditListing(l.OrganisationID, l.AgentUserID, l.SellerUserID) {
		if p.MayManageListing(l.OrganisationID, l.SellerUserID) {
			return IntakeView{}, ErrForbidden
		}
		return IntakeView{}, ErrListingNotFound
	}
	if !l.Eligible() {
		return IntakeView{}, ErrListingNotEligible
	}
	now := s.clock()
	errs := fieldErrors{}
	if in.Status == IntakeNone {
		if in.FormID != nil {
			errs.add("form_id", "must be null to detach the form (status NONE)")
			return IntakeView{}, errs.err()
		}
		if err := s.store.DeleteIntake(ctx, l.ID); err != nil {
			return IntakeView{}, err
		}
		s.audit(ctx, "application_intake.detached", slog.String("listing_id", l.ID.String()), slog.String("actor_user_id", p.UserID.String()))
		return s.intakeView(ctx, l)
	}
	if !slices.Contains([]string{IntakeOpen, IntakePaused, IntakeClosed}, in.Status) {
		errs.add("status", "must be OPEN, PAUSED, CLOSED or NONE")
	}
	var formID uuid.UUID
	if in.FormID == nil {
		errs.add("form_id", "is required")
	} else if id, err := uuid.Parse(*in.FormID); err != nil {
		errs.add("form_id", "must be a form ID")
	} else {
		formID = id
	}
	if err := errs.err(); err != nil {
		return IntakeView{}, err
	}
	book, _ := l.Book()
	f, err := s.store.Form(ctx, formID)
	switch {
	case errors.Is(err, ErrFormNotFound) || (err == nil && !f.Book().Same(book)):
		errs.add("form_id", "must be a published form of this listing's organisation")
	case err != nil:
		return IntakeView{}, err
	case f.Status != FormPublished:
		errs.add("form_id", "must be a PUBLISHED form (publish it first)")
	case f.MarketCode != l.MarketCode:
		errs.add("form_id", fmt.Sprintf("is a form for %s; this listing is in %s", f.MarketCode, l.MarketCode))
	case f.Expired(now):
		errs.add("form_id", "has expired")
	}
	if in.ClosesAt != nil && !in.ClosesAt.After(now) && in.Status == IntakeOpen {
		errs.add("closes_at", "must be in the future")
	}
	if err := errs.err(); err != nil {
		return IntakeView{}, err
	}
	rec := Intake{ListingID: l.ID, FormID: f.ID, Status: in.Status, ClosesAt: utcPtr(in.ClosesAt), UpdatedBy: &p.UserID, UpdatedAt: now}
	if err := s.store.PutIntake(ctx, rec); err != nil {
		return IntakeView{}, err
	}
	s.audit(ctx, "application_intake.changed", slog.String("listing_id", l.ID.String()), slog.String("form_id", f.ID.String()),
		slog.String("status", in.Status), slog.String("actor_user_id", p.UserID.String()))
	return s.intakeView(ctx, l)
}

// PublicForm returns the published form attached to a public listing and
// its intake (a PAUSED or CLOSED intake still returns the form).
func (s *Service) PublicForm(ctx context.Context, slug string) (PublicForm, IntakeState, error) {
	l, err := s.listings.BySlug(ctx, slug)
	if err != nil {
		return PublicForm{}, IntakeState{}, err
	}
	in, ok, err := s.store.Intake(ctx, l.ID)
	if err != nil {
		return PublicForm{}, IntakeState{}, err
	}
	if !ok || !l.Eligible() {
		return PublicForm{}, IntakeState{}, ErrFormUnavailable
	}
	f, err := s.store.Form(ctx, in.FormID)
	if err != nil {
		return PublicForm{}, IntakeState{}, err
	}
	if f.Status != FormPublished {
		return PublicForm{}, IntakeState{}, ErrFormUnavailable
	}
	status, reason := Effective(in, f, s.clock())
	return publicForm(f), IntakeState{Status: status, ClosesAt: utcPtr(in.ClosesAt), ClosedReason: strPtr(reason)}, nil
}

// Summaries returns the `applications` object of each listing among ids
// that has a form attached (short stays have none).
func (s *Service) Summaries(ctx context.Context, ids []uuid.UUID, shortStay map[uuid.UUID]bool) (map[uuid.UUID]Summary, error) {
	out := map[uuid.UUID]Summary{}
	if len(ids) == 0 {
		return out, nil
	}
	intakes, err := s.store.Intakes(ctx, ids)
	if err != nil {
		return nil, err
	}
	forms := map[uuid.UUID]Form{}
	now := s.clock()
	for id, in := range intakes {
		if shortStay[id] {
			continue
		}
		f, ok := forms[in.FormID]
		if !ok {
			if f, err = s.store.Form(ctx, in.FormID); err != nil {
				return nil, err
			}
			forms[in.FormID] = f
		}
		status, _ := Effective(in, f, now)
		name := f.Name
		out[id] = Summary{Open: status == IntakeOpen, ClosesAt: utcPtr(in.ClosesAt), FormName: &name}
	}
	return out, nil
}
