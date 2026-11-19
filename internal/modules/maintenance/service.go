package maintenance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/businessenquiries"
	"rightproperty365.com/backend/internal/modules/ledger"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Providers looks up the directory's public service providers.
type Providers interface {
	// Provider returns a public profile by ID (ok false when there is none).
	Provider(ctx context.Context, id uuid.UUID) (ProviderOption, bool, error)
	// Search returns public service-provider profiles, sponsored first then by name.
	Search(ctx context.Context, q, trade string, limit int) ([]ProviderOption, error)
}

// Enquiries sends business enquiries (the business-enquiries service).
type Enquiries interface {
	Create(ctx context.Context, in businessenquiries.Input, headerIdempotencyKey string) (businessenquiries.Result, error)
}

// Deps is what the service needs.
type Deps struct {
	Store     Store
	Tenancies *tenancies.Service
	// Ledger posts a cost's EXPENSE entry; LedgerEntry reads an entry back
	// (its status, and whether it was reversed).
	Ledger      *ledger.Service
	LedgerEntry func(ctx context.Context, id uuid.UUID) (ledger.Entry, error)
	// Vocabulary returns the active rows of a reference set.
	Vocabulary func(ctx context.Context, set string) ([]CodeLabel, error)
	// Currencies returns the ISO 4217 codes a cost may use (reference data).
	Currencies func(ctx context.Context) ([]string, error)
	Providers  Providers
	Enquiries  Enquiries
	Files      storage.Storage
	// PrivacyVersion is recorded as the consent of the enquiries the
	// managers send to providers (default DefaultEnquiryPrivacyVer).
	PrivacyVersion string
	Logger         *slog.Logger
	Now            func() time.Time
}

// Service is maintenance requests.
type Service struct {
	store          Store
	tenancies      *tenancies.Service
	ledger         *ledger.Service
	ledgerEntry    func(ctx context.Context, id uuid.UUID) (ledger.Entry, error)
	vocabulary     func(ctx context.Context, set string) ([]CodeLabel, error)
	currencies     func(ctx context.Context) ([]string, error)
	providers      Providers
	enquiries      Enquiries
	files          storage.Storage
	privacyVersion string
	logger         *slog.Logger
	now            func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, tenancies: d.Tenancies, ledger: d.Ledger, ledgerEntry: d.LedgerEntry, vocabulary: d.Vocabulary,
		currencies: d.Currencies, providers: d.Providers, enquiries: d.Enquiries, files: d.Files, privacyVersion: d.PrivacyVersion,
		logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	if s.privacyVersion == "" {
		s.privacyVersion = DefaultEnquiryPrivacyVer
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func (s *Service) today() string { return s.clock().Format(time.DateOnly) }

// ---------------------------------------------------------------------------
// Access
// ---------------------------------------------------------------------------

type access struct {
	p     *auth.Principal
	book  tenancies.Book
	write bool
	name  string
}

func (s *Service) access(p *auth.Principal, bookID uuid.UUID) (access, error) {
	if p == nil {
		return access{}, ErrUnauthenticated
	}
	b, err := s.tenancies.Access(p, bookID)
	switch {
	case errors.Is(err, tenancies.ErrUnauthenticated):
		return access{}, ErrUnauthenticated
	case errors.Is(err, tenancies.ErrBookNotFound):
		return access{}, ErrBookNotFound
	case err != nil:
		return access{}, err
	}
	return access{p: p, book: b.Book, write: b.Write, name: b.Name}, nil
}

func (s *Service) writer(p *auth.Principal, bookID uuid.UUID) (access, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return access{}, err
	}
	if !a.write {
		return access{}, ErrForbidden
	}
	return a, nil
}

// Check answers ErrBookNotFound (or ErrUnauthenticated) when the caller has
// no access to the book: handlers ask first, so another organisation never
// learns more than 404.
func (s *Service) Check(p *auth.Principal, bookID uuid.UUID) error {
	_, err := s.access(p, bookID)
	return err
}

func (s *Service) request(ctx context.Context, a access, id uuid.UUID) (Request, error) {
	r, err := s.store.Request(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if !r.Book.Same(a.book) {
		return Request{}, ErrNotFound
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// Errors with details
// ---------------------------------------------------------------------------

// ValidationError is 400 VALIDATION_FAILED with per-field messages.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

func invalid(f map[string]string) error {
	if len(f) == 0 {
		return nil
	}
	return &ValidationError{Fields: f}
}

// TransitionError is 409 MAINTENANCE_INVALID_TRANSITION.
type TransitionError struct {
	Status  string
	Allowed []string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("maintenance request in status %s cannot move there", e.Status)
}

// ---------------------------------------------------------------------------
// Audit, timeline and outbox
// ---------------------------------------------------------------------------

func (s *Service) audit(ctx context.Context, a access, action string, r Request, after map[string]any) []orgaudit.Event {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", slog.Bool("audit", true), slog.String("event", "maintenance."+strings.ToLower(action)),
		slog.String("trace_id", logging.RequestID(ctx)), slog.String("book_id", a.book.ID().String()),
		slog.String("actor_user_id", a.p.UserID.String()), slog.String("target_id", r.ID.String()))
	if a.book.OrganisationID == nil {
		return nil
	}
	return []orgaudit.Event{orgaudit.New(*a.book.OrganisationID, a.p.UserID, a.p.Name, action, orgaudit.TargetMaintenanceRequest, r.ID,
		r.Reference, nil, after, s.clock())}
}

// actor is who acts in a write.
type actor struct {
	kind   string
	userID uuid.UUID
	name   string
}

func managerActor(p *auth.Principal) actor {
	return actor{kind: ActorManager, userID: p.UserID, name: p.Name}
}
func tenantActor(p *auth.Principal) actor {
	return actor{kind: ActorTenant, userID: p.UserID, name: p.Name}
}

func (x actor) event(requestID uuid.UUID, typ string, at time.Time) Event {
	id := x.userID
	return Event{ID: newID(), RequestID: requestID, Type: typ, ActorKind: x.kind, ActorUserID: &id, ActorName: x.name, At: at}
}

func (x actor) notice(typ string, r Request, from string, at time.Time) Notice {
	id := x.userID
	return Notice{Type: typ, Payload: EventPayload{RequestID: r.ID, Reference: r.Reference, FromStatus: from, ToStatus: r.Status,
		RaisedBy: r.RaisedByKind, ActorKind: x.kind, ActorUserID: &id, OccurredAt: at}}
}

// move sets a request's status and its dates.
func move(r *Request, to string, at time.Time) {
	r.Status, r.StatusChangedAt, r.UpdatedAt = to, at, at
	switch to {
	case StatusResolved:
		r.ResolvedAt = &at
	case StatusInProgress:
		r.ResolvedAt = nil
	case StatusClosed, StatusCancelled:
		r.ClosedAt = &at
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func text(f map[string]string, field, v string, minLen, maxLen int) string {
	v = strings.TrimSpace(strings.ReplaceAll(v, "\r\n", "\n"))
	n := utf8.RuneCountInString(v)
	switch {
	case n < minLen && minLen > 0:
		f[field] = "is required"
	case n > maxLen:
		f[field] = fmt.Sprintf("must be at most %d characters", maxLen)
	case !utf8.ValidString(v) || hasControl(v):
		f[field] = "contains characters that are not allowed"
	}
	return v
}

func hasControl(s string) bool {
	for _, r := range s {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func validDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Year() >= 1900 && t.Year() <= 2999 && t.Format(time.DateOnly) == s
}

// vocab is the active categories and priorities.
type vocab struct {
	categories, priorities []CodeLabel
}

func (s *Service) vocab(ctx context.Context) (vocab, error) {
	if s.vocabulary == nil {
		return vocab{categories: []CodeLabel{{"other", "Other"}}, priorities: []CodeLabel{{"normal", "Normal"}}}, nil
	}
	c, err := s.vocabulary(ctx, reference.SetMaintenanceCategories)
	if err != nil {
		return vocab{}, err
	}
	p, err := s.vocabulary(ctx, reference.SetMaintenancePriorities)
	if err != nil {
		return vocab{}, err
	}
	if c == nil {
		c = []CodeLabel{}
	}
	if p == nil {
		p = []CodeLabel{}
	}
	return vocab{categories: c, priorities: p}, nil
}

func pick(f map[string]string, field, code string, list []CodeLabel, set string) (CodeLabel, bool) {
	code = strings.TrimSpace(code)
	if code == "" {
		f[field] = "is required"
		return CodeLabel{}, false
	}
	for _, c := range list {
		if c.Code == code {
			return c, true
		}
	}
	f[field] = "must be one of the codes in GET /v1/reference/" + set
	return CodeLabel{}, false
}

// label is the current wording of a code (the stored one when the row is gone).
func label(list []CodeLabel, code, stored string) CodeLabel {
	for _, c := range list {
		if c.Code == code {
			return c
		}
	}
	if stored == "" {
		stored = code
	}
	return CodeLabel{Code: code, Label: stored}
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

func photoRules() PhotoRules {
	return PhotoRules{MaxPhotos: MaxPhotos, MaxBytes: MaxPhotoBytes, Types: []string{"image/jpeg", "image/png", "image/webp"}}
}

func (s *Service) allowedCurrencies(ctx context.Context) ([]string, error) {
	if s.currencies == nil {
		return []string{"USD"}, nil
	}
	list, err := s.currencies(ctx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, c := range list {
		c = strings.ToUpper(strings.TrimSpace(c))
		if len(c) == 3 && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Options serves GET .../maintenance/options.
func (s *Service) Options(ctx context.Context, p *auth.Principal, bookID uuid.UUID) (Options, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Options{}, err
	}
	v, err := s.vocab(ctx)
	if err != nil {
		return Options{}, err
	}
	out := Options{Categories: v.categories, Priorities: v.priorities, Statuses: []CodeLabel{}, Transitions: map[string][]string{},
		Currencies: []CurrencyOption{}, Methods: []CodeLabel{}, PhotoRules: photoRules(), Permissions: Permissions{Write: a.write}}
	for _, st := range Statuses() {
		out.Statuses = append(out.Statuses, CodeLabel{Code: st, Label: StatusLabel(st)})
		out.Transitions[st] = Transitions(st)
	}
	curs, err := s.allowedCurrencies(ctx)
	if err != nil {
		return Options{}, err
	}
	for _, c := range curs {
		out.Currencies = append(out.Currencies, CurrencyOption{Code: c, MinorUnit: minorUnit(c)})
	}
	for _, m := range ledger.Methods() {
		out.Methods = append(out.Methods, CodeLabel{Code: m, Label: ledger.MethodLabel(m)})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Requests
// ---------------------------------------------------------------------------

// Input is the body of POST .../maintenance (and, without property, unit
// and tenancy, of the tenant's POST).
type Input struct {
	PropertyID  *uuid.UUID `json:"property_id"`
	UnitID      *uuid.UUID `json:"unit_id"`
	TenancyID   *uuid.UUID `json:"tenancy_id"`
	Category    string     `json:"category"`
	Priority    string     `json:"priority"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
}

// MyInput is the body of POST /v1/me/tenancies/{id}/maintenance.
type MyInput struct {
	Category    string `json:"category"`
	Priority    string `json:"priority"`
	Title       string `json:"title"`
	Description string `json:"description"`
}

// PatchInput is the body of PATCH .../maintenance/{requestId}.
type PatchInput struct {
	Category    *string `json:"category"`
	Priority    *string `json:"priority"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
}

// StatusInput is the body of POST .../status.
type StatusInput struct {
	Status string  `json:"status"`
	Note   *string `json:"note"`
}

// CommentInput is the body of POST .../comments.
type CommentInput struct {
	Text     string `json:"text"`
	Internal bool   `json:"internal"`
}

// fill validates the common fields onto r.
func (s *Service) fill(ctx context.Context, r *Request, category, priority, title, description string, f map[string]string) error {
	v, err := s.vocab(ctx)
	if err != nil {
		return err
	}
	if c, ok := pick(f, "category", category, v.categories, reference.SetMaintenanceCategories); ok {
		r.Category, r.CategoryLabel = c.Code, c.Label
	}
	if c, ok := pick(f, "priority", priority, v.priorities, reference.SetMaintenancePriorities); ok {
		r.Priority, r.PriorityLabel = c.Code, c.Label
	}
	r.Title = text(f, "title", title, 1, MaxTitle)
	r.Description = text(f, "description", description, 0, MaxDescription)
	return nil
}

// create writes a new request, its CREATED item and its outbox event.
func (s *Service) create(ctx context.Context, r Request, x actor) error {
	ref, err := s.store.NextReference(ctx, r.CreatedAt.Year())
	if err != nil {
		return err
	}
	r.Reference = ref
	created := x.event(r.ID, EventCreated, r.CreatedAt)
	created.ToStatus = StatusNew
	return s.store.Apply(ctx, Change{Create: &r, Events: []Event{created},
		Notices: []Notice{x.notice(OutboxRequestCreated, r, "", r.CreatedAt)}})
}

// Create serves POST .../maintenance.
func (s *Service) Create(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in Input) (RequestView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return RequestView{}, err
	}
	now := s.clock()
	actorID := p.UserID
	r := Request{ID: newID(), Book: a.book, Status: StatusNew, RaisedByKind: ActorManager, RaisedBy: &actorID, RaisedByName: p.Name,
		StatusChangedAt: now, CreatedAt: now, UpdatedAt: now}
	f := map[string]string{}
	if err := s.fill(ctx, &r, in.Category, in.Priority, in.Title, in.Description, f); err != nil {
		return RequestView{}, err
	}
	var prop tenancies.Property
	if in.PropertyID == nil {
		f["property_id"] = "is required"
	} else {
		prop, err = s.tenancies.LiveProperty(ctx, a.book, *in.PropertyID)
		switch {
		case errors.Is(err, tenancies.ErrPropertyNotFound):
			f["property_id"] = "is not a property of these books"
		case err != nil:
			return RequestView{}, err
		default:
			r.PropertyID = prop.ID
		}
	}
	if _, bad := f["property_id"]; !bad {
		if in.UnitID != nil {
			u, ok := prop.Unit(*in.UnitID)
			if !ok || u.DeletedAt != nil {
				f["unit_id"] = "is not a unit of this property"
			} else {
				id := u.ID
				r.UnitID = &id
			}
		}
		if in.TenancyID != nil {
			t, err := s.tenancies.BookTenancy(ctx, a.book, *in.TenancyID)
			switch {
			case errors.Is(err, tenancies.ErrTenancyNotFound):
				f["tenancy_id"] = "is not a tenancy of these books"
			case err != nil:
				return RequestView{}, err
			case t.Status == tenancies.StatusDraft:
				f["tenancy_id"] = "is a draft: activate the tenancy first"
			case t.PropertyID != prop.ID:
				f["tenancy_id"] = "is not a tenancy of this property"
			case r.UnitID != nil && (t.UnitID == nil || *t.UnitID != *r.UnitID):
				f["tenancy_id"] = "is not a tenancy of this unit"
			default:
				id := t.ID
				r.TenancyID = &id
				if r.UnitID == nil && t.UnitID != nil {
					u := *t.UnitID
					r.UnitID = &u
				}
			}
		}
	}
	if err := invalid(f); err != nil {
		return RequestView{}, err
	}
	if err := s.create(ctx, r, managerActor(p)); err != nil {
		return RequestView{}, err
	}
	return s.oneView(ctx, a, r.ID)
}

// List serves GET .../maintenance.
func (s *Service) List(ctx context.Context, p *auth.Principal, bookID uuid.UUID, f Filter, page, size int) (Page, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Page{}, err
	}
	f.Limit, f.Offset = size, (page-1)*size
	list, total, counts, err := s.store.Requests(ctx, a.book, f)
	if err != nil {
		return Page{}, err
	}
	v, err := s.viewer(ctx, a, list)
	if err != nil {
		return Page{}, err
	}
	items := make([]Summary, 0, len(list))
	for _, r := range list {
		items = append(items, v.summary(r))
	}
	full := map[string]int{}
	for _, st := range Statuses() {
		full[st] = counts[st]
	}
	return Page{Items: items, Total: total, Page: page, PageSize: size, StatusCounts: full}, nil
}

// Get serves GET .../maintenance/{requestId}.
func (s *Service) Get(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (RequestView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return RequestView{}, err
	}
	return s.oneView(ctx, a, id)
}

// Update serves PATCH .../maintenance/{requestId}.
func (s *Service) Update(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in PatchInput) (RequestView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return RequestView{}, err
	}
	r, err := s.request(ctx, a, id)
	if err != nil {
		return RequestView{}, err
	}
	if !Open(r.Status) {
		return RequestView{}, ErrClosed
	}
	next := r
	f := map[string]string{}
	category, priority, title, description := r.Category, r.Priority, r.Title, r.Description
	if in.Category != nil {
		category = *in.Category
	}
	if in.Priority != nil {
		priority = *in.Priority
	}
	if in.Title != nil {
		title = *in.Title
	}
	if in.Description != nil {
		description = *in.Description
	}
	v, err := s.vocab(ctx)
	if err != nil {
		return RequestView{}, err
	}
	// An unchanged code stays valid even when its row was deactivated since.
	if in.Category != nil && strings.TrimSpace(category) != r.Category {
		if c, ok := pick(f, "category", category, v.categories, reference.SetMaintenanceCategories); ok {
			next.Category, next.CategoryLabel = c.Code, c.Label
		}
	}
	if in.Priority != nil && strings.TrimSpace(priority) != r.Priority {
		if c, ok := pick(f, "priority", priority, v.priorities, reference.SetMaintenancePriorities); ok {
			next.Priority, next.PriorityLabel = c.Code, c.Label
		}
	}
	next.Title = text(f, "title", title, 1, MaxTitle)
	next.Description = text(f, "description", description, 0, MaxDescription)
	if err := invalid(f); err != nil {
		return RequestView{}, err
	}
	var fields []string
	for _, c := range []struct {
		name     string
		old, new string
	}{{"category", r.Category, next.Category}, {"priority", r.Priority, next.Priority}, {"title", r.Title, next.Title},
		{"description", r.Description, next.Description}} {
		if c.old != c.new {
			fields = append(fields, c.name)
		}
	}
	if len(fields) == 0 {
		return s.oneView(ctx, a, id)
	}
	now := s.clock()
	next.UpdatedAt = now
	ev := managerActor(p).event(r.ID, EventUpdated, now)
	ev.Details = map[string]any{"fields": fields}
	if err := s.store.Apply(ctx, Change{Expect: map[uuid.UUID]Expect{r.ID: {Status: r.Status, Assignments: r.Assignments}},
		Requests: []Request{next}, Events: []Event{ev}}); err != nil {
		return RequestView{}, err
	}
	return s.oneView(ctx, a, id)
}

// ChangeStatus serves POST .../maintenance/{requestId}/status.
func (s *Service) ChangeStatus(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in StatusInput) (RequestView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return RequestView{}, err
	}
	r, err := s.request(ctx, a, id)
	if err != nil {
		return RequestView{}, err
	}
	f := map[string]string{}
	to := strings.TrimSpace(in.Status)
	if !slices.Contains(Statuses(), to) {
		f["status"] = "must be one of " + strings.Join(Statuses(), ", ")
	}
	note := ""
	if in.Note != nil {
		note = text(f, "note", *in.Note, 0, MaxNote)
	}
	if to == StatusCancelled && note == "" {
		if _, bad := f["note"]; !bad {
			f["note"] = "is required to cancel: say why"
		}
	}
	if err := invalid(f); err != nil {
		return RequestView{}, err
	}
	if !CanMove(r.Status, to) {
		return RequestView{}, &TransitionError{Status: r.Status, Allowed: Transitions(r.Status)}
	}
	now := s.clock()
	from := r.Status
	next := r
	move(&next, to, now)
	x := managerActor(p)
	ev := x.event(r.ID, EventStatusChanged, now)
	ev.FromStatus, ev.ToStatus, ev.Text = from, to, note
	ch := Change{Expect: map[uuid.UUID]Expect{r.ID: {Status: r.Status, Assignments: r.Assignments}}, Requests: []Request{next},
		Events: []Event{ev}, Notices: []Notice{x.notice(OutboxStatusChanged, next, from, now)}}
	if to == StatusCancelled {
		ch.Audit = s.audit(ctx, a, orgaudit.ActionMaintenanceCancelled, r, map[string]any{"from_status": from, "reason": note})
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return RequestView{}, err
	}
	return s.oneView(ctx, a, id)
}

// Comment serves POST .../maintenance/{requestId}/comments.
func (s *Service) Comment(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in CommentInput) (TimelineItem, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return TimelineItem{}, err
	}
	r, err := s.request(ctx, a, id)
	if err != nil {
		return TimelineItem{}, err
	}
	f := map[string]string{}
	body := text(f, "text", in.Text, 1, MaxComment)
	if err := invalid(f); err != nil {
		return TimelineItem{}, err
	}
	now := s.clock()
	ev := managerActor(p).event(r.ID, EventComment, now)
	ev.Text, ev.Internal = body, in.Internal
	if err := s.store.Apply(ctx, Change{Touch: map[uuid.UUID]time.Time{r.ID: now}, Events: []Event{ev}}); err != nil {
		return TimelineItem{}, err
	}
	return timelineItem(ev), nil
}
