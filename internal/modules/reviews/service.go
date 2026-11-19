package reviews

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/maintenance"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
)

// Profile is the directory profile a review is about (public profiles only).
type Profile struct {
	ID             uuid.UUID
	Kind           string
	Slug           string
	Name           string
	OrganisationID uuid.UUID
}

// Profiles finds public directory profiles (the app wires the directory).
type Profiles interface {
	// ByID returns a public profile, or ErrProfileNotFound.
	ByID(ctx context.Context, id uuid.UUID) (Profile, error)
	// Public returns a public profile by its address, or ErrProfileNotFound.
	Public(ctx context.Context, kind, slug string) (Profile, error)
}

// MaintenanceJob is what a review reads of a maintenance request.
type MaintenanceJob struct {
	ID        uuid.UUID
	Reference string
	Title     string
	Book      tenancies.Book
	Status    string
	// ProviderProfileID is the profile the request is currently assigned
	// to (nil when none).
	ProviderProfileID *uuid.UUID
	// CompletedAt is when it was resolved (else closed); nil while open.
	CompletedAt *time.Time
}

// Bases reads the jobs and enquiries reviews rest on (the app wires the
// maintenance and business-enquiry stores).
type Bases interface {
	// MaintenanceJob returns one request whatever its book, or ErrNotFound.
	MaintenanceJob(ctx context.Context, id uuid.UUID) (MaintenanceJob, error)
	// MaintenanceJobs returns the requests of a book that are assigned to
	// the profile.
	MaintenanceJobs(ctx context.Context, book tenancies.Book, profileID uuid.UUID) ([]MaintenanceJob, error)
	// Enquiry returns one enquiry, or businessenquiries.ErrNotFound.
	Enquiry(ctx context.Context, id uuid.UUID) (businessenquiries.CustomerEnquiry, error)
	// Enquiries returns the enquiries a customer sent to a profile.
	Enquiries(ctx context.Context, userID, profileID uuid.UUID) ([]businessenquiries.CustomerEnquiry, error)
}

// Deps is what the Service is built from.
type Deps struct {
	Store     Store
	Tenancies *tenancies.Service
	Profiles  Profiles
	Bases     Bases
	Logger    *slog.Logger
	Now       func() time.Time
}

// Service implements the reviews use cases.
type Service struct {
	store     Store
	tenancies *tenancies.Service
	profiles  Profiles
	bases     Bases
	logger    *slog.Logger
	now       func() time.Time
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, tenancies: d.Tenancies, profiles: d.Profiles, bases: d.Bases, logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

// AdminRoles are the platform roles that moderate reviews (SUPER_ADMIN
// passes every check).
func AdminRoles() []string { return []string{auth.PlatformContentAdmin, auth.PlatformModerator} }

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// Ratings returns the published reviews' count and sum per profile (the
// directory's `rating`).
func (s *Service) Ratings(ctx context.Context, profileIDs []uuid.UUID) (map[uuid.UUID]Rating, error) {
	return s.store.Ratings(ctx, profileIDs)
}

// ---------------------------------------------------------------------------
// Who is writing
// ---------------------------------------------------------------------------

// reviewer is who writes or owns reviews: a customer (/v1/me/reviews) or a
// book (/v1/agent/organisations/{id}/reviews).
type reviewer struct {
	p *auth.Principal
	// book is set for a book's routes.
	book  *tenancies.Book
	write bool
	// name is the book's name (organisation or direct owner).
	name string
}

func (s *Service) customer(p *auth.Principal) (reviewer, error) {
	if p == nil {
		return reviewer{}, ErrUnauthenticated
	}
	return reviewer{p: p, write: true}, nil
}

func (s *Service) bookOf(p *auth.Principal, bookID uuid.UUID) (reviewer, error) {
	if p == nil {
		return reviewer{}, ErrUnauthenticated
	}
	a, err := s.tenancies.Access(p, bookID)
	switch {
	case errors.Is(err, tenancies.ErrUnauthenticated):
		return reviewer{}, ErrUnauthenticated
	case errors.Is(err, tenancies.ErrBookNotFound):
		return reviewer{}, ErrBookNotFound
	case err != nil:
		return reviewer{}, err
	}
	b := a.Book
	return reviewer{p: p, book: &b, write: a.Write, name: a.Name}, nil
}

// CheckBook answers ErrBookNotFound (or ErrUnauthenticated) before anything
// else on a book's routes.
func (s *Service) CheckBook(p *auth.Principal, bookID uuid.UUID) error {
	_, err := s.bookOf(p, bookID)
	return err
}

// kind is the reviewer kind of what this reviewer writes.
func (r reviewer) kind() string {
	switch {
	case r.book == nil:
		return ReviewerCustomer
	case r.book.OrganisationID != nil:
		return ReviewerOrganisation
	}
	return ReviewerDirectOwner
}

// filter selects this reviewer's own reviews.
func (r reviewer) filter() Filter {
	switch r.kind() {
	case ReviewerOrganisation:
		return Filter{ReviewerOrganisationID: r.book.OrganisationID}
	case ReviewerDirectOwner:
		return Filter{ReviewerKind: ReviewerDirectOwner, ReviewerUserID: r.book.UserID}
	}
	id := r.p.UserID
	return Filter{ReviewerKind: ReviewerCustomer, ReviewerUserID: &id}
}

// owns reports whether a review is this reviewer's.
func (r reviewer) owns(rv Review) bool {
	f := r.filter()
	f.Limit = 0
	return f.matches(rv)
}

// basisKind is the one basis this reviewer may use.
func (r reviewer) basisKind() string {
	if r.book == nil {
		return BasisEnquiry
	}
	return BasisMaintenance
}

// ---------------------------------------------------------------------------
// Eligibility
// ---------------------------------------------------------------------------

// maintenanceReason says why a request is not a basis for the book's
// review of the profile ("" when it is).
func maintenanceReason(job MaintenanceJob, book tenancies.Book, profileID uuid.UUID) string {
	switch {
	case !job.Book.Same(book):
		return ReasonBasisNotFound // another book's request: as if unknown
	case job.ProviderProfileID == nil || *job.ProviderProfileID != profileID:
		return ReasonWrongProvider
	case job.Status != maintenance.StatusResolved && job.Status != maintenance.StatusClosed:
		return ReasonNotCompleted
	}
	return ""
}

// enquiryReason says why an enquiry is not a basis for the customer's
// review of the profile ("" when it is), and when a too recent one will be.
func enquiryReason(e businessenquiries.CustomerEnquiry, userID, profileID uuid.UUID, now time.Time) (string, *time.Time) {
	switch {
	case e.CustomerUserID == nil || *e.CustomerUserID != userID || e.PrivacyVersion == maintenance.DefaultEnquiryPrivacyVer:
		// Another person's enquiry, or one sent by a maintenance assignment
		// (the book reviews that job): as if unknown.
		return ReasonBasisNotFound, nil
	case e.ProfileID != profileID:
		return ReasonWrongProvider, nil
	case e.RepliedAt == nil:
		return ReasonNotReplied, nil
	}
	if from := e.CreatedAt.Add(EnquiryMinAge); now.Before(from) {
		return ReasonTooRecent, &from
	}
	return "", nil
}

// basis is one candidate basis with what the reviewer may know of it.
type basis struct {
	Kind, Reference, Label string
	ID                     uuid.UUID
	Date                   time.Time
	Reason                 string
	EligibleFrom           *time.Time
}

// resolveBasis checks one basis for this reviewer and profile.
func (s *Service) resolveBasis(ctx context.Context, r reviewer, kind string, id, profileID uuid.UUID) (basis, error) {
	now := s.clock()
	switch kind {
	case BasisMaintenance:
		job, err := s.bases.MaintenanceJob(ctx, id)
		if errors.Is(err, ErrNotFound) || errors.Is(err, maintenance.ErrNotFound) {
			return basis{}, &IneligibleError{Reason: ReasonBasisNotFound}
		}
		if err != nil {
			return basis{}, err
		}
		if reason := maintenanceReason(job, *r.book, profileID); reason != "" {
			return basis{}, &IneligibleError{Reason: reason}
		}
		return jobBasis(job), nil
	default:
		e, err := s.bases.Enquiry(ctx, id)
		if errors.Is(err, businessenquiries.ErrNotFound) || errors.Is(err, ErrNotFound) {
			return basis{}, &IneligibleError{Reason: ReasonBasisNotFound}
		}
		if err != nil {
			return basis{}, err
		}
		if reason, from := enquiryReason(e, r.p.UserID, profileID, now); reason != "" {
			return basis{}, &IneligibleError{Reason: reason, EligibleFrom: from}
		}
		return enquiryBasis(e), nil
	}
}

func jobBasis(job MaintenanceJob) basis {
	b := basis{Kind: BasisMaintenance, ID: job.ID, Reference: job.Reference, Label: job.Reference + ": " + job.Title}
	if job.CompletedAt != nil {
		b.Date = job.CompletedAt.UTC()
	}
	return b
}

func enquiryBasis(e businessenquiries.CustomerEnquiry) basis {
	return basis{Kind: BasisEnquiry, ID: e.ID, Reference: e.Reference, Label: "Enquiry " + e.Reference, Date: e.CreatedAt.UTC()}
}

// Eligibility is GET .../reviews/eligibility: what this reviewer may review
// on a profile.
type Eligibility struct {
	Profile    ProfileRef      `json:"profile"`
	Reviewable bool            `json:"reviewable"`
	Eligible   bool            `json:"eligible"`
	Bases      []BasisView     `json:"bases"`
	Upcoming   []UpcomingBasis `json:"upcoming"`
	Reviewed   []ReviewedBasis `json:"reviewed"`
}

// ProfileLookup names the profile of an eligibility question: an ID, or a
// kind and slug.
type ProfileLookup struct {
	ID         *uuid.UUID
	Kind, Slug string
}

func (s *Service) lookup(ctx context.Context, l ProfileLookup) (Profile, error) {
	if l.ID != nil {
		return s.profiles.ByID(ctx, *l.ID)
	}
	return s.profiles.Public(ctx, l.Kind, l.Slug)
}

func (s *Service) eligibility(ctx context.Context, r reviewer, l ProfileLookup) (Eligibility, error) {
	prof, err := s.lookup(ctx, l)
	if err != nil {
		return Eligibility{}, err
	}
	out := Eligibility{Profile: profileRef(prof), Reviewable: prof.Kind == ReviewableKind && !r.memberOf(prof.OrganisationID),
		Bases: []BasisView{}, Upcoming: []UpcomingBasis{}, Reviewed: []ReviewedBasis{}}
	if !out.Reviewable {
		return out, nil
	}
	now := s.clock()
	var candidates []basis
	if r.book != nil {
		jobs, err := s.bases.MaintenanceJobs(ctx, *r.book, prof.ID)
		if err != nil {
			return Eligibility{}, err
		}
		for _, job := range jobs {
			b := jobBasis(job)
			b.Reason = maintenanceReason(job, *r.book, prof.ID)
			if b.Reason != ReasonBasisNotFound && b.Reason != ReasonWrongProvider {
				candidates = append(candidates, b)
			}
		}
	} else {
		list, err := s.bases.Enquiries(ctx, r.p.UserID, prof.ID)
		if err != nil {
			return Eligibility{}, err
		}
		for _, e := range list {
			b := enquiryBasis(e)
			b.Reason, b.EligibleFrom = enquiryReason(e, r.p.UserID, prof.ID, now)
			if b.Reason != ReasonBasisNotFound && b.Reason != ReasonWrongProvider {
				candidates = append(candidates, b)
			}
		}
	}
	ids := make([]uuid.UUID, 0, len(candidates))
	for _, b := range candidates {
		ids = append(ids, b.ID)
	}
	reviewed, err := s.store.ByBasis(ctx, r.basisKind(), ids)
	if err != nil {
		return Eligibility{}, err
	}
	for _, b := range candidates {
		switch rv, done := reviewed[b.ID]; {
		case done:
			ref := ReviewedBasis{Basis: basisView(b), ReviewID: nil, Status: ""}
			if r.owns(rv) {
				id := rv.ID
				ref.ReviewID, ref.Status = &id, rv.Status
			}
			out.Reviewed = append(out.Reviewed, ref)
		case b.Reason == "":
			out.Bases = append(out.Bases, basisView(b))
		default:
			out.Upcoming = append(out.Upcoming, UpcomingBasis{Basis: basisView(b), Reason: b.Reason, EligibleFrom: b.EligibleFrom})
		}
	}
	out.Eligible = len(out.Bases) > 0
	return out, nil
}

// memberOf reports whether the reviewer belongs to the organisation that
// owns a profile (nobody reviews their own business).
func (r reviewer) memberOf(organisationID uuid.UUID) bool {
	if r.book != nil && r.book.OrganisationID != nil && *r.book.OrganisationID == organisationID {
		return true
	}
	return r.p.MembershipRole(organisationID) != ""
}

// MyEligibility is GET /v1/me/reviews/eligibility.
func (s *Service) MyEligibility(ctx context.Context, p *auth.Principal, l ProfileLookup) (Eligibility, error) {
	r, err := s.customer(p)
	if err != nil {
		return Eligibility{}, err
	}
	return s.eligibility(ctx, r, l)
}

// BookEligibility is GET /v1/agent/organisations/{id}/reviews/eligibility.
func (s *Service) BookEligibility(ctx context.Context, p *auth.Principal, bookID uuid.UUID, l ProfileLookup) (Eligibility, error) {
	r, err := s.bookOf(p, bookID)
	if err != nil {
		return Eligibility{}, err
	}
	return s.eligibility(ctx, r, l)
}

// ---------------------------------------------------------------------------
// The reviewer's own reviews
// ---------------------------------------------------------------------------

// BasisInput is the basis of a new review.
type BasisInput struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Input is the body of POST .../reviews.
type Input struct {
	DirectoryProfileID string      `json:"directory_profile_id"`
	Basis              *BasisInput `json:"basis"`
	Rating             *int        `json:"rating"`
	Text               *string     `json:"text"`
}

// Patch is the body of PATCH .../reviews/{reviewId}.
type Patch struct {
	Rating *int    `json:"rating"`
	Text   *string `json:"text"`
}

func checkRating(v *int, required bool, f map[string]string) {
	switch {
	case v == nil && required:
		f["rating"] = "is required"
	case v != nil && (*v < MinRating || *v > MaxRating):
		f["rating"] = "must be a whole number from 1 to 5"
	}
}

func checkText(v *string, required bool, f map[string]string) string {
	if v == nil {
		if required {
			f["text"] = "is required"
		}
		return ""
	}
	t := strings.TrimSpace(*v)
	if n := utf8.RuneCountInString(t); n < MinText || n > MaxText {
		f["text"] = "must be 20 to 2000 characters"
	}
	return t
}

func (s *Service) create(ctx context.Context, r reviewer, in Input) (Review, error) {
	if !r.write {
		return Review{}, ErrForbidden
	}
	f := map[string]string{}
	profileID, err := uuid.Parse(strings.TrimSpace(in.DirectoryProfileID))
	if err != nil {
		f["directory_profile_id"] = "must be the id of a service provider's profile"
	}
	var basisID uuid.UUID
	switch {
	case in.Basis == nil:
		f["basis"] = "is required"
	default:
		if want := r.basisKind(); in.Basis.Kind != want {
			if in.Basis.Kind != BasisMaintenance && in.Basis.Kind != BasisEnquiry {
				f["basis.kind"] = "must be MAINTENANCE or ENQUIRY"
			} else if want == BasisEnquiry {
				f["basis.kind"] = "must be ENQUIRY here: a maintenance job is reviewed by its book at /v1/agent/organisations/{id}/reviews"
			} else {
				f["basis.kind"] = "must be MAINTENANCE here: an enquiry is reviewed by the person who sent it at /v1/me/reviews"
			}
		}
		if basisID, err = uuid.Parse(strings.TrimSpace(in.Basis.ID)); err != nil {
			f["basis.id"] = "must be the id of a maintenance request or business enquiry"
		}
	}
	checkRating(in.Rating, true, f)
	text := checkText(in.Text, true, f)
	var prof Profile
	if _, bad := f["directory_profile_id"]; !bad {
		prof, err = s.profiles.ByID(ctx, profileID)
		switch {
		case errors.Is(err, ErrProfileNotFound):
			f["directory_profile_id"] = "is not a public profile"
		case err != nil:
			return Review{}, err
		case prof.Kind != ReviewableKind:
			f["directory_profile_id"] = "must be a service provider's profile"
		case r.memberOf(prof.OrganisationID):
			f["directory_profile_id"] = "is your own organisation's profile"
		}
	}
	if len(f) > 0 {
		return Review{}, &ValidationError{Fields: f}
	}
	b, err := s.resolveBasis(ctx, r, in.Basis.Kind, basisID, prof.ID)
	if err != nil {
		return Review{}, err
	}
	if existing, err := s.store.ByBasis(ctx, b.Kind, []uuid.UUID{b.ID}); err != nil {
		return Review{}, err
	} else if _, taken := existing[b.ID]; taken {
		return Review{}, ErrBasisReviewed
	}
	now := s.clock()
	user := r.p.UserID
	rv := Review{ID: newID(), ProfileID: prof.ID, ProviderOrganisationID: prof.OrganisationID, ProviderKind: prof.Kind,
		ProviderSlug: prof.Slug, ProviderName: prof.Name, ReviewerKind: r.kind(), ReviewerUserID: &user,
		BasisKind: b.Kind, BasisID: b.ID, BasisReference: b.Reference, Rating: *in.Rating, Text: text, Status: StatusPending,
		Version: 1, CreatedAt: now, UpdatedAt: now}
	switch rv.ReviewerKind {
	case ReviewerOrganisation:
		org := *r.book.OrganisationID
		rv.ReviewerOrganisationID, rv.ReviewerName, rv.AuthorName = &org, r.name, r.p.Name
	default:
		rv.ReviewerName = PublicName(r.p.Name)
	}
	if strings.TrimSpace(rv.ReviewerName) == "" {
		rv.ReviewerName = PublicName("")
	}
	if err := s.store.Apply(ctx, Change{Create: &rv}); err != nil {
		return Review{}, err
	}
	return rv, nil
}

// CreateMine is POST /v1/me/reviews.
func (s *Service) CreateMine(ctx context.Context, p *auth.Principal, in Input) (Review, error) {
	r, err := s.customer(p)
	if err != nil {
		return Review{}, err
	}
	return s.create(ctx, r, in)
}

// CreateForBook is POST /v1/agent/organisations/{id}/reviews.
func (s *Service) CreateForBook(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in Input) (Review, error) {
	r, err := s.bookOf(p, bookID)
	if err != nil {
		return Review{}, err
	}
	return s.create(ctx, r, in)
}

func (s *Service) own(ctx context.Context, r reviewer, id uuid.UUID) (Review, error) {
	rv, err := s.store.Review(ctx, id)
	if err != nil {
		return Review{}, err
	}
	if !r.owns(rv) {
		return Review{}, ErrNotFound
	}
	return rv, nil
}

func (s *Service) list(ctx context.Context, r reviewer) ([]Review, error) {
	f := r.filter()
	f.Limit = MaxOwnReviews
	list, _, err := s.store.Reviews(ctx, f)
	return list, err
}

func (s *Service) edit(ctx context.Context, r reviewer, id uuid.UUID, in Patch) (Review, error) {
	rv, err := s.own(ctx, r, id)
	if err != nil {
		return Review{}, err
	}
	if !r.write {
		return Review{}, ErrForbidden
	}
	if rv.Status != StatusPending {
		return Review{}, &StateError{Code: CodeLocked, Status: rv.Status}
	}
	f := map[string]string{}
	checkRating(in.Rating, false, f)
	text := checkText(in.Text, false, f)
	if len(f) > 0 {
		return Review{}, &ValidationError{Fields: f}
	}
	version := rv.Version
	if in.Rating != nil {
		rv.Rating = *in.Rating
	}
	if in.Text != nil {
		rv.Text = text
	}
	rv.UpdatedAt = s.clock()
	if err := s.store.Apply(ctx, Change{Update: &rv, ExpectVersion: version}); err != nil {
		return Review{}, err
	}
	rv.Version = version + 1
	return rv, nil
}

func (s *Service) remove(ctx context.Context, r reviewer, id uuid.UUID) error {
	rv, err := s.own(ctx, r, id)
	if err != nil {
		return err
	}
	if !r.write {
		return ErrForbidden
	}
	return s.store.Apply(ctx, Change{Delete: &rv.ID})
}

// Mine is GET /v1/me/reviews.
func (s *Service) Mine(ctx context.Context, p *auth.Principal) ([]Review, error) {
	r, err := s.customer(p)
	if err != nil {
		return nil, err
	}
	return s.list(ctx, r)
}

// MyReview is GET /v1/me/reviews/{id}.
func (s *Service) MyReview(ctx context.Context, p *auth.Principal, id uuid.UUID) (Review, error) {
	r, err := s.customer(p)
	if err != nil {
		return Review{}, err
	}
	return s.own(ctx, r, id)
}

// EditMine is PATCH /v1/me/reviews/{id}.
func (s *Service) EditMine(ctx context.Context, p *auth.Principal, id uuid.UUID, in Patch) (Review, error) {
	r, err := s.customer(p)
	if err != nil {
		return Review{}, err
	}
	return s.edit(ctx, r, id, in)
}

// DeleteMine is DELETE /v1/me/reviews/{id}.
func (s *Service) DeleteMine(ctx context.Context, p *auth.Principal, id uuid.UUID) error {
	r, err := s.customer(p)
	if err != nil {
		return err
	}
	return s.remove(ctx, r, id)
}

// BookReviews is GET /v1/agent/organisations/{id}/reviews: the reviews the
// book wrote, and whether the caller may write.
func (s *Service) BookReviews(ctx context.Context, p *auth.Principal, bookID uuid.UUID) ([]Review, bool, error) {
	r, err := s.bookOf(p, bookID)
	if err != nil {
		return nil, false, err
	}
	list, err := s.list(ctx, r)
	return list, r.write, err
}

// BookReview is GET /v1/agent/organisations/{id}/reviews/{reviewId}.
func (s *Service) BookReview(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (Review, bool, error) {
	r, err := s.bookOf(p, bookID)
	if err != nil {
		return Review{}, false, err
	}
	rv, err := s.own(ctx, r, id)
	return rv, r.write, err
}

// EditForBook is PATCH /v1/agent/organisations/{id}/reviews/{reviewId}.
func (s *Service) EditForBook(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in Patch) (Review, error) {
	r, err := s.bookOf(p, bookID)
	if err != nil {
		return Review{}, err
	}
	return s.edit(ctx, r, id, in)
}

// DeleteForBook is DELETE /v1/agent/organisations/{id}/reviews/{reviewId}.
func (s *Service) DeleteForBook(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) error {
	r, err := s.bookOf(p, bookID)
	if err != nil {
		return err
	}
	return s.remove(ctx, r, id)
}

// ---------------------------------------------------------------------------
// The provider
// ---------------------------------------------------------------------------

// provider checks the caller is a member of the provider organisation
// (any role reads; OWNER and MANAGER reply).
func (s *Service) provider(p *auth.Principal, orgID uuid.UUID) (bool, error) {
	if p == nil {
		return false, ErrUnauthenticated
	}
	role := p.MembershipRole(orgID)
	if role == "" {
		return false, ErrBookNotFound
	}
	return role == auth.RoleOwner || role == auth.RoleManager, nil
}

// CheckProvider answers ErrBookNotFound (or ErrUnauthenticated) to anybody
// who is not a member of the organisation.
func (s *Service) CheckProvider(p *auth.Principal, orgID uuid.UUID) error {
	_, err := s.provider(p, orgID)
	return err
}

// Received is GET /v1/agent/organisations/{id}/reviews/received: the
// published reviews of the organisation's profiles, newest first; and
// whether the caller may reply.
func (s *Service) Received(ctx context.Context, p *auth.Principal, orgID uuid.UUID, page, size int) ([]Review, int, bool, error) {
	mayReply, err := s.provider(p, orgID)
	if err != nil {
		return nil, 0, false, err
	}
	list, total, err := s.store.Reviews(ctx, Filter{ProviderOrganisationID: &orgID, Statuses: []string{StatusPublished}, Order: "published",
		Limit: size, Offset: (page - 1) * size})
	return list, total, mayReply, err
}

func (s *Service) received(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID) (Review, bool, error) {
	mayReply, err := s.provider(p, orgID)
	if err != nil {
		return Review{}, false, err
	}
	rv, err := s.store.Review(ctx, id)
	if err != nil {
		return Review{}, false, err
	}
	if rv.ProviderOrganisationID != orgID || rv.Status != StatusPublished {
		return Review{}, false, ErrNotFound
	}
	return rv, mayReply, nil
}

func checkReply(text string) (string, error) {
	t := strings.TrimSpace(text)
	if n := utf8.RuneCountInString(t); n < 1 || n > MaxReply {
		return "", &ValidationError{Fields: map[string]string{"text": "must be 1 to 2000 characters"}}
	}
	return t, nil
}

// ReplyEditableUntil is when a reply stops being editable.
func ReplyEditableUntil(r Reply) time.Time { return r.At.Add(ReplyEditWindow) }

// Reply is POST .../reviews/{reviewId}/reply: the provider's one reply,
// published directly.
func (s *Service) Reply(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID, text string) (Review, error) {
	rv, mayReply, err := s.received(ctx, p, orgID, id)
	if err != nil {
		return Review{}, err
	}
	if !mayReply {
		return Review{}, ErrForbidden
	}
	switch {
	case rv.Reply != nil:
		return Review{}, ErrReplyExists
	case rv.ReplyRemovedAt != nil:
		return Review{}, &StateError{Code: CodeReplyLocked, Status: rv.Status}
	}
	t, err := checkReply(text)
	if err != nil {
		return Review{}, err
	}
	now, user := s.clock(), p.UserID
	version := rv.Version
	rv.Reply = &Reply{Text: t, At: now, UpdatedAt: now, ByUserID: &user, ByName: p.Name}
	rv.UpdatedAt = now
	if err := s.store.Apply(ctx, Change{Update: &rv, ExpectVersion: version}); err != nil {
		return Review{}, err
	}
	rv.Version = version + 1
	return rv, nil
}

// EditReply is PATCH .../reviews/{reviewId}/reply, within 7 days of the reply.
func (s *Service) EditReply(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID, text string) (Review, error) {
	rv, mayReply, err := s.received(ctx, p, orgID, id)
	if err != nil {
		return Review{}, err
	}
	if !mayReply {
		return Review{}, ErrForbidden
	}
	if rv.Reply == nil {
		return Review{}, ErrNoReply
	}
	now := s.clock()
	if until := ReplyEditableUntil(*rv.Reply); !now.Before(until) {
		return Review{}, &StateError{Code: CodeReplyLocked, Status: rv.Status, Until: &until}
	}
	t, err := checkReply(text)
	if err != nil {
		return Review{}, err
	}
	version, user := rv.Version, p.UserID
	rv.Reply.Text, rv.Reply.UpdatedAt, rv.Reply.ByUserID, rv.Reply.ByName = t, now, &user, p.Name
	rv.UpdatedAt = now
	if err := s.store.Apply(ctx, Change{Update: &rv, ExpectVersion: version}); err != nil {
		return Review{}, err
	}
	rv.Version = version + 1
	return rv, nil
}

// ---------------------------------------------------------------------------
// Public and reports
// ---------------------------------------------------------------------------

// Public is GET /v1/directory/{kind}/{slug}/reviews: a public profile's
// published reviews (newest first) and its rating.
func (s *Service) Public(ctx context.Context, kind, slug string, page, size int) (Profile, []Review, int, Rating, error) {
	prof, err := s.profiles.Public(ctx, kind, slug)
	if err != nil {
		return Profile{}, nil, 0, Rating{}, err
	}
	list, total, err := s.store.Reviews(ctx, Filter{ProfileID: &prof.ID, Statuses: []string{StatusPublished}, Order: "published",
		Limit: size, Offset: (page - 1) * size})
	if err != nil {
		return Profile{}, nil, 0, Rating{}, err
	}
	ratings, err := s.store.Ratings(ctx, []uuid.UUID{prof.ID})
	if err != nil {
		return Profile{}, nil, 0, Rating{}, err
	}
	return prof, list, total, ratings[prof.ID], nil
}

// ReportInput is the body of POST /v1/reviews/{id}/report.
type ReportInput struct {
	Reason string `json:"reason"`
}

// ReportReview is POST /v1/reviews/{id}/report: one open report per person
// and review; only published reviews (anything else is 404). The review
// stays visible until a moderator acts.
func (s *Service) ReportReview(ctx context.Context, p *auth.Principal, id uuid.UUID, in ReportInput) (Report, error) {
	if p == nil {
		return Report{}, ErrUnauthenticated
	}
	rv, err := s.store.Review(ctx, id)
	if err != nil {
		return Report{}, err
	}
	if rv.Status != StatusPublished {
		return Report{}, ErrNotFound
	}
	reason := strings.TrimSpace(in.Reason)
	if n := utf8.RuneCountInString(reason); n < 1 || n > MaxReason {
		return Report{}, &ValidationError{Fields: map[string]string{"reason": "must be 1 to 500 characters"}}
	}
	user := p.UserID
	rp := Report{ID: newID(), ReviewID: rv.ID, ReporterUserID: &user, Reason: reason, Status: ReportOpen, CreatedAt: s.clock()}
	if err := s.store.Apply(ctx, Change{Reports: []Report{rp}}); err != nil {
		return Report{}, err
	}
	return rp, nil
}

// ---------------------------------------------------------------------------
// Moderation
// ---------------------------------------------------------------------------

// AdminFilter is the query of GET /v1/admin/reviews.
type AdminFilter struct {
	// View is "queue" (default: PENDING or with an open report) or "all".
	View      string
	Status    string
	Reported  bool
	BasisKind string
	Rating    int
	ProfileID *uuid.UUID
	Q         string
	Page      int
	PageSize  int
}

// QueueCounts are the queue's counters.
type QueueCounts struct {
	Pending  int `json:"pending"`
	Reported int `json:"reported"`
}

// AdminList is GET /v1/admin/reviews.
func (s *Service) AdminList(ctx context.Context, af AdminFilter) ([]Review, int, QueueCounts, error) {
	f := Filter{Queue: af.View != "all", Reported: af.Reported, BasisKind: af.BasisKind, Rating: af.Rating, ProfileID: af.ProfileID,
		Q: af.Q, Limit: af.PageSize, Offset: (af.Page - 1) * af.PageSize}
	if f.Queue {
		f.Order = "queue"
	}
	if af.Status != "" {
		f.Statuses = []string{af.Status}
	}
	list, total, err := s.store.Reviews(ctx, f)
	if err != nil {
		return nil, 0, QueueCounts{}, err
	}
	var c QueueCounts
	c.Pending, c.Reported, err = s.store.QueueCounts(ctx)
	return list, total, c, err
}

// AdminGet is GET /v1/admin/reviews/{id}: the review and its reports.
func (s *Service) AdminGet(ctx context.Context, id uuid.UUID) (Review, []Report, error) {
	rv, err := s.store.Review(ctx, id)
	if err != nil {
		return Review{}, nil, err
	}
	reports, err := s.store.Reports(ctx, id)
	return rv, reports, err
}

// ModerationInput is the body of the moderation actions.
type ModerationInput struct {
	Reason string `json:"reason"`
}

// Allowed returns the moderation actions a review allows now.
func Allowed(rv Review) []string {
	var out []string
	if rv.Status == StatusPending || rv.Status == StatusHidden || (rv.Status == StatusPublished && rv.OpenReports > 0) {
		out = append(out, ActionPublish)
	}
	if rv.Status == StatusPublished {
		out = append(out, ActionHide)
	}
	if rv.Status == StatusPending {
		out = append(out, ActionReject)
	}
	if rv.Reply != nil {
		out = append(out, ActionRemoveReply)
	}
	return out
}

var auditActions = map[string]string{
	ActionPublish: adminaudit.ActionReviewPublished, ActionHide: adminaudit.ActionReviewHidden,
	ActionReject: adminaudit.ActionReviewRejected, ActionRemoveReply: adminaudit.ActionReviewReplyRemoved,
}

// Moderate applies one moderation action (publish, hide, reject, remove
// the reply) with the moderator's reason, audited in the admin audit; the
// reviewer is told of a publish, hide or reject, and the provider of a
// first publication. Every action closes the review's open reports.
func (s *Service) Moderate(ctx context.Context, p *auth.Principal, id uuid.UUID, action string, in ModerationInput) (Review, []Report, error) {
	if p == nil {
		return Review{}, nil, ErrUnauthenticated
	}
	rv, err := s.store.Review(ctx, id)
	if err != nil {
		return Review{}, nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	n := utf8.RuneCountInString(reason)
	switch {
	case n > MaxReason:
		return Review{}, nil, &ValidationError{Fields: map[string]string{"reason": "must be at most 500 characters"}}
	case n == 0 && action != ActionPublish:
		return Review{}, nil, &ValidationError{Fields: map[string]string{"reason": "is required (1 to 500 characters)"}}
	}
	if !slices.Contains(Allowed(rv), action) {
		return Review{}, nil, &StateError{Code: CodeInvalidTransition, Status: rv.Status}
	}
	now, actor := s.clock(), p.UserID
	before := map[string]any{"status": rv.Status, "open_reports": rv.OpenReports, "has_reply": rv.Reply != nil}
	version, from := rv.Version, rv.Status
	notify, first := false, false
	switch action {
	case ActionPublish:
		rv.Status = StatusPublished
		notify = from != StatusPublished
		if rv.PublishedAt == nil {
			first, rv.PublishedAt = true, &now
		}
	case ActionHide:
		rv.Status, notify = StatusHidden, true
	case ActionReject:
		rv.Status, notify = StatusRejected, true
	case ActionRemoveReply:
		rv.Reply, rv.ReplyRemovedAt, rv.ReplyRemovedReason = nil, &now, reason
	}
	if action != ActionRemoveReply {
		rv.ModerationReason, rv.ModeratedAt, rv.ModeratedBy, rv.ModeratedByName = reason, &now, &actor, p.Name
	}
	rv.UpdatedAt = now
	ch := Change{Update: &rv, ExpectVersion: version,
		Resolve: &ReportResolution{ReviewID: rv.ID, At: now, By: actor, Resolution: action},
		Audit: []adminaudit.Event{adminaudit.New(actor, p.Name, auditActions[action], adminaudit.TargetProviderReview, rv.ID.String(),
			"Review of "+rv.ProviderName, reason, before, map[string]any{"status": rv.Status, "has_reply": rv.Reply != nil}, now)}}
	if notify {
		ch.Notices = []EventPayload{{ReviewID: rv.ID, Action: action, Status: rv.Status, FirstPublish: first, ActorUserID: &actor, OccurredAt: now}}
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return Review{}, nil, err
	}
	return s.AdminGet(ctx, rv.ID)
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

// NotificationInfo is what the notifications of a moderation need: safe
// text (the provider's public name and the status) and who to tell.
type NotificationInfo struct {
	ReviewID               uuid.UUID
	Status                 string
	ProviderName           string
	ProviderOrganisationID uuid.UUID
	// ReviewerUserID is nil when the reviewer deleted their account.
	ReviewerUserID *uuid.UUID
	ReviewerKind   string
}

// Notices returns the notifications module's source of review events.
func Notices(store Store) func(ctx context.Context, id uuid.UUID) (NotificationInfo, error) {
	return func(ctx context.Context, id uuid.UUID) (NotificationInfo, error) {
		rv, err := store.Review(ctx, id)
		if err != nil {
			return NotificationInfo{}, err
		}
		return NotificationInfo{ReviewID: rv.ID, Status: rv.Status, ProviderName: rv.ProviderName,
			ProviderOrganisationID: rv.ProviderOrganisationID, ReviewerUserID: rv.ReviewerUserID, ReviewerKind: rv.ReviewerKind}, nil
	}
}
