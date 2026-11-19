package inspections

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

	"rightproperty365.com/backend/internal/modules/tenancies"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Person is somebody who may inspect.
type Person struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
}

// Deps is what the service needs.
type Deps struct {
	Store     Store
	Tenancies *tenancies.Service
	// Templates returns the active rows of the reference set
	// inspection_checklist_templates, in order.
	Templates func(ctx context.Context) ([]TemplateRow, error)
	// Members returns the active members of an organisation (the people
	// who may be its inspectors). Nil: only the signed-in user.
	Members func(ctx context.Context, organisationID uuid.UUID) ([]Person, error)
	Files   storage.Storage
	Logger  *slog.Logger
	Now     func() time.Time
}

// Service is inspections.
type Service struct {
	store     Store
	tenancies *tenancies.Service
	templates func(ctx context.Context) ([]TemplateRow, error)
	members   func(ctx context.Context, organisationID uuid.UUID) ([]Person, error)
	files     storage.Storage
	logger    *slog.Logger
	now       func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, tenancies: d.Tenancies, templates: d.Templates, members: d.Members, files: d.Files, logger: d.Logger,
		now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

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

func (s *Service) inspection(ctx context.Context, a access, id uuid.UUID) (Inspection, error) {
	in, err := s.store.Inspection(ctx, id)
	if err != nil {
		return Inspection{}, err
	}
	if !in.Book.Same(a.book) {
		return Inspection{}, ErrNotFound
	}
	return in, nil
}

// open loads an inspection for a write: ErrClosed when it is frozen.
func (s *Service) open(ctx context.Context, a access, id uuid.UUID) (Inspection, error) {
	in, err := s.inspection(ctx, a, id)
	if err != nil {
		return Inspection{}, err
	}
	if !Open(in.Status) {
		return Inspection{}, &ClosedError{Status: in.Status}
	}
	return in, nil
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

// TransitionError is 409 INSPECTION_INVALID_TRANSITION.
type TransitionError struct {
	Status  string
	Allowed []string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("inspection in status %s cannot move there", e.Status)
}

// ClosedError is 409 INSPECTION_CLOSED: the inspection is COMPLETED or
// CANCELLED and takes no more writes.
type ClosedError struct{ Status string }

func (e *ClosedError) Error() string { return "inspection is " + strings.ToLower(e.Status) }

// IncompleteError is 409 INSPECTION_INCOMPLETE: items without a status.
type IncompleteError struct {
	Missing int
	ItemIDs []uuid.UUID
}

func (e *IncompleteError) Error() string { return fmt.Sprintf("%d items have no status", e.Missing) }

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *Service) audit(ctx context.Context, a access, action string, in Inspection, after map[string]any) []orgaudit.Event {
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", slog.Bool("audit", true), slog.String("event", "inspection."+strings.ToLower(action)),
		slog.String("trace_id", logging.RequestID(ctx)), slog.String("book_id", a.book.ID().String()),
		slog.String("actor_user_id", a.p.UserID.String()), slog.String("target_id", in.ID.String()))
	if a.book.OrganisationID == nil {
		return nil
	}
	return []orgaudit.Event{orgaudit.New(*a.book.OrganisationID, a.p.UserID, a.p.Name, action, orgaudit.TargetInspection, in.ID,
		in.Reference, nil, after, s.clock())}
}

func notice(typ string, in Inspection, actor uuid.UUID, rescheduled bool, at time.Time) []Notice {
	if in.TenancyID == nil {
		return nil // nobody to tell: tenants are the only recipients
	}
	tid := *in.TenancyID
	return []Notice{{Type: typ, Payload: EventPayload{InspectionID: in.ID, Reference: in.Reference, Type: in.Type, Status: in.Status,
		TenancyID: &tid, Rescheduled: rescheduled, ActorUserID: &actor, OccurredAt: at}}}
}

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

// ValidDate reports whether s is a YYYY-MM-DD date (the list filters).
func ValidDate(s string) bool { return validDate(s) }

func (s *Service) scheduledFor(f map[string]string, raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		f["scheduled_for"] = "is required"
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		f["scheduled_for"] = "must be an RFC 3339 timestamp, for example 2026-12-01T09:00:00Z"
		return time.Time{}
	}
	t = t.UTC().Truncate(time.Second)
	if t.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) || t.After(s.clock().Add(ScheduleHorizon)) {
		f["scheduled_for"] = "must be from 2000-01-01 to two years from now"
	}
	return t
}

func (s *Service) templateList(ctx context.Context) ([]Template, error) {
	if s.templates == nil {
		return []Template{}, nil
	}
	rows, err := s.templates(ctx)
	if err != nil {
		return nil, err
	}
	return BuildTemplates(rows), nil
}

// inspectors are the people who may inspect in the book.
func (s *Service) inspectors(ctx context.Context, a access) ([]Person, error) {
	self := Person{UserID: a.p.UserID, Name: a.p.Name}
	if a.book.OrganisationID == nil || s.members == nil {
		return []Person{self}, nil
	}
	list, err := s.members(ctx, *a.book.OrganisationID)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(list)
	if !slices.ContainsFunc(out, func(p Person) bool { return p.UserID == self.UserID }) {
		out = append(out, self)
	}
	slices.SortStableFunc(out, func(x, y Person) int { return strings.Compare(strings.ToLower(x.Name), strings.ToLower(y.Name)) })
	return out, nil
}

func (s *Service) inspector(ctx context.Context, a access, f map[string]string, id *uuid.UUID) (Person, error) {
	if id == nil || *id == a.p.UserID {
		return Person{UserID: a.p.UserID, Name: a.p.Name}, nil
	}
	list, err := s.inspectors(ctx, a)
	if err != nil {
		return Person{}, err
	}
	for _, p := range list {
		if p.UserID == *id {
			return p, nil
		}
	}
	f["inspector_user_id"] = "is not an active member of these books"
	return Person{}, nil
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

func photoRules() PhotoRules {
	return PhotoRules{MaxPhotosPerItem: MaxPhotosPerItem, MaxPhotos: MaxPhotos, MaxBytes: MaxPhotoBytes,
		Types: []string{"image/jpeg", "image/png", "image/webp"}}
}

// Options serves GET .../inspections/options.
func (s *Service) Options(ctx context.Context, p *auth.Principal, bookID uuid.UUID) (Options, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Options{}, err
	}
	tpls, err := s.templateList(ctx)
	if err != nil {
		return Options{}, err
	}
	people, err := s.inspectors(ctx, a)
	if err != nil {
		return Options{}, err
	}
	out := Options{Types: []CodeLabel{}, Statuses: []CodeLabel{}, FindingStatuses: []CodeLabel{}, Templates: tpls, Inspectors: people,
		Transitions: map[string][]string{}, PhotoRules: photoRules(),
		Limits: Limits{MaxAreas: MaxAreas, MaxItemsPerArea: MaxItemsPerArea, MaxItems: MaxItems, MaxLabel: MaxLabel, MaxNote: MaxNote,
			MaxSummary: MaxSummary, MaxSignOff: MaxSignOff},
		Permissions: Permissions{Write: a.write}}
	for _, t := range Types() {
		out.Types = append(out.Types, CodeLabel{Code: t, Label: TypeLabel(t)})
	}
	for _, st := range Statuses() {
		out.Statuses = append(out.Statuses, CodeLabel{Code: st, Label: StatusLabel(st)})
		out.Transitions[st] = Transitions(st)
	}
	for _, st := range FindingStatuses() {
		out.FindingStatuses = append(out.FindingStatuses, CodeLabel{Code: st, Label: FindingLabel(st)})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Inspections
// ---------------------------------------------------------------------------

// Input is the body of POST .../inspections.
type Input struct {
	PropertyID        *uuid.UUID `json:"property_id"`
	UnitID            *uuid.UUID `json:"unit_id"`
	TenancyID         *uuid.UUID `json:"tenancy_id"`
	Type              string     `json:"type"`
	ScheduledFor      string     `json:"scheduled_for"`
	InspectorUserID   *uuid.UUID `json:"inspector_user_id"`
	ChecklistTemplate string     `json:"checklist_template"`
}

// PatchInput is the body of PATCH .../inspections/{inspectionId}.
type PatchInput struct {
	ScheduledFor    *string    `json:"scheduled_for"`
	InspectorUserID *uuid.UUID `json:"inspector_user_id"`
}

// CompleteInput is the body of POST .../complete.
type CompleteInput struct {
	Summary           *string `json:"summary"`
	TenantSignOffName *string `json:"tenant_sign_off_name"`
	AgentSignOffName  *string `json:"agent_sign_off_name"`
}

// CancelInput is the body of POST .../cancel.
type CancelInput struct {
	Reason string `json:"reason"`
}

// Create serves POST .../inspections.
func (s *Service) Create(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in Input) (InspectionView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	now := s.clock()
	by := p.UserID
	ins := Inspection{ID: newID(), Book: a.book, Status: StatusScheduled, CreatedBy: &by, CreatedByName: p.Name, CreatedAt: now, UpdatedAt: now}
	f := map[string]string{}
	ins.Type = strings.TrimSpace(in.Type)
	if !slices.Contains(Types(), ins.Type) {
		f["type"] = "must be one of " + strings.Join(Types(), ", ")
	}
	ins.ScheduledFor = s.scheduledFor(f, in.ScheduledFor)
	who, err := s.inspector(ctx, a, f, in.InspectorUserID)
	if err != nil {
		return InspectionView{}, err
	}
	if who.UserID != uuid.Nil {
		id := who.UserID
		ins.InspectorID, ins.InspectorName = &id, cut(who.Name, 200)
	}
	tpls, err := s.templateList(ctx)
	if err != nil {
		return InspectionView{}, err
	}
	code := strings.TrimSpace(in.ChecklistTemplate)
	if i := slices.IndexFunc(tpls, func(t Template) bool { return t.Code == code }); i < 0 {
		if code == "" {
			f["checklist_template"] = "is required"
		} else {
			f["checklist_template"] = "must be one of the templates in GET .../inspections/options"
		}
	} else {
		ins.TemplateCode, ins.TemplateLabel, ins.Checklist = tpls[i].Code, cut(tpls[i].Label, 200), Snapshot(tpls[i])
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
			return InspectionView{}, err
		default:
			ins.PropertyID = prop.ID
		}
	}
	if _, bad := f["property_id"]; !bad {
		if in.UnitID != nil {
			u, ok := prop.Unit(*in.UnitID)
			if !ok || u.DeletedAt != nil {
				f["unit_id"] = "is not a unit of this property"
			} else {
				id := u.ID
				ins.UnitID = &id
			}
		}
		if in.TenancyID != nil {
			t, err := s.tenancies.BookTenancy(ctx, a.book, *in.TenancyID)
			switch {
			case errors.Is(err, tenancies.ErrTenancyNotFound):
				f["tenancy_id"] = "is not a tenancy of these books"
			case err != nil:
				return InspectionView{}, err
			case t.PropertyID != prop.ID:
				f["tenancy_id"] = "is not a tenancy of this property"
			case ins.UnitID != nil && (t.UnitID == nil || *t.UnitID != *ins.UnitID):
				f["tenancy_id"] = "is not a tenancy of this unit"
			default:
				id := t.ID
				ins.TenancyID = &id
				if ins.UnitID == nil && t.UnitID != nil {
					u := *t.UnitID
					ins.UnitID = &u
				}
			}
		}
	}
	if err := invalid(f); err != nil {
		return InspectionView{}, err
	}
	ref, err := s.store.NextReference(ctx, now.Year())
	if err != nil {
		return InspectionView{}, err
	}
	ins.Reference = ref
	if err := s.store.Apply(ctx, Change{Create: &ins, Notices: notice(OutboxScheduled, ins, p.UserID, false, now)}); err != nil {
		return InspectionView{}, err
	}
	return s.oneView(ctx, a, ins.ID)
}

// List serves GET .../inspections.
func (s *Service) List(ctx context.Context, p *auth.Principal, bookID uuid.UUID, f Filter, page, size int) (Page, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Page{}, err
	}
	f.Limit, f.Offset = size, (page-1)*size
	list, total, counts, err := s.store.Inspections(ctx, a.book, f)
	if err != nil {
		return Page{}, err
	}
	v, err := s.viewer(ctx, a, list)
	if err != nil {
		return Page{}, err
	}
	items := make([]Summary, 0, len(list))
	for _, in := range list {
		items = append(items, v.summary(in))
	}
	full := map[string]int{}
	for _, st := range Statuses() {
		full[st] = counts[st]
	}
	return Page{Items: items, Total: total, Page: page, PageSize: size, StatusCounts: full}, nil
}

// Get serves GET .../inspections/{inspectionId}.
func (s *Service) Get(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (InspectionView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	return s.oneView(ctx, a, id)
}

// save replaces an inspection decided on cur (its status and version).
func (s *Service) save(ctx context.Context, cur, next Inspection, extra Change) error {
	next.Version = cur.Version + 1
	extra.Expect = map[uuid.UUID]Expect{cur.ID: {Status: cur.Status, Version: cur.Version}}
	extra.Inspections = []Inspection{next}
	return s.store.Apply(ctx, extra)
}

// start moves a SCHEDULED inspection to IN_PROGRESS (on next).
func start(next *Inspection, at time.Time) {
	if next.Status == StatusScheduled {
		next.Status, next.StartedAt = StatusInProgress, &at
	}
}

// Update serves PATCH .../inspections/{inspectionId}.
func (s *Service) Update(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in PatchInput) (InspectionView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return InspectionView{}, err
	}
	next := cur
	f := map[string]string{}
	if in.ScheduledFor != nil {
		next.ScheduledFor = s.scheduledFor(f, *in.ScheduledFor)
	}
	if in.InspectorUserID != nil {
		who, err := s.inspector(ctx, a, f, in.InspectorUserID)
		if err != nil {
			return InspectionView{}, err
		}
		if who.UserID != uuid.Nil {
			uid := who.UserID
			next.InspectorID, next.InspectorName = &uid, cut(who.Name, 200)
		}
	}
	if err := invalid(f); err != nil {
		return InspectionView{}, err
	}
	moved := !next.ScheduledFor.Equal(cur.ScheduledFor)
	sameInspector := (next.InspectorID == nil) == (cur.InspectorID == nil) &&
		(next.InspectorID == nil || *next.InspectorID == *cur.InspectorID)
	if !moved && sameInspector {
		return s.oneView(ctx, a, id)
	}
	now := s.clock()
	next.UpdatedAt = now
	var ch Change
	if moved && next.Status == StatusScheduled {
		ch.Notices = notice(OutboxScheduled, next, p.UserID, true, now)
	}
	if err := s.save(ctx, cur, next, ch); err != nil {
		return InspectionView{}, err
	}
	return s.oneView(ctx, a, id)
}

// PutChecklist serves PUT .../inspections/{inspectionId}/checklist.
func (s *Service) PutChecklist(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in ChecklistInput) (InspectionView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return InspectionView{}, err
	}
	checklist, removed, f := Restructure(cur.Checklist, in)
	if err := invalid(f); err != nil {
		return InspectionView{}, err
	}
	photos, err := s.store.Photos(ctx, cur.ID)
	if err != nil {
		return InspectionView{}, err
	}
	var gone []Photo
	for _, ph := range photos {
		if slices.Contains(removed, ph.ItemID) {
			gone = append(gone, ph)
		}
	}
	now := s.clock()
	next := cur
	next.Checklist, next.UpdatedAt = checklist, now
	ch := Change{}
	for _, ph := range gone {
		ch.DeletePhotos = append(ch.DeletePhotos, ph.ID)
	}
	if err := s.save(ctx, cur, next, ch); err != nil {
		return InspectionView{}, err
	}
	s.removeFiles(ctx, gone)
	return s.oneView(ctx, a, id)
}

// PutFindings serves PUT .../inspections/{inspectionId}/findings.
func (s *Service) PutFindings(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in FindingsInput) (InspectionView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return InspectionView{}, err
	}
	checklist, changed, f := Record(cur.Checklist, in)
	if err := invalid(f); err != nil {
		return InspectionView{}, err
	}
	if !changed && cur.Status == StatusInProgress {
		return s.oneView(ctx, a, id)
	}
	now := s.clock()
	next := cur
	next.Checklist, next.UpdatedAt = checklist, now
	start(&next, now)
	if err := s.save(ctx, cur, next, Change{}); err != nil {
		return InspectionView{}, err
	}
	return s.oneView(ctx, a, id)
}

// Start serves POST .../inspections/{inspectionId}/start.
func (s *Service) Start(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (InspectionView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return InspectionView{}, err
	}
	if !CanMove(cur.Status, StatusInProgress) {
		return InspectionView{}, &TransitionError{Status: cur.Status, Allowed: Transitions(cur.Status)}
	}
	now := s.clock()
	next := cur
	next.UpdatedAt = now
	start(&next, now)
	if err := s.save(ctx, cur, next, Change{}); err != nil {
		return InspectionView{}, err
	}
	return s.oneView(ctx, a, id)
}

// Complete serves POST .../inspections/{inspectionId}/complete: every item
// needs a status; the inspection is frozen afterwards.
func (s *Service) Complete(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in CompleteInput) (InspectionView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return InspectionView{}, err
	}
	f := map[string]string{}
	next := cur
	if in.Summary != nil {
		next.Summary = text(f, "summary", *in.Summary, 0, MaxSummary)
	}
	if in.TenantSignOffName != nil {
		next.TenantSignOff = text(f, "tenant_sign_off_name", strings.ReplaceAll(*in.TenantSignOffName, "\n", " "), 0, MaxSignOff)
	}
	if in.AgentSignOffName != nil {
		next.AgentSignOff = text(f, "agent_sign_off_name", strings.ReplaceAll(*in.AgentSignOffName, "\n", " "), 0, MaxSignOff)
	}
	if err := invalid(f); err != nil {
		return InspectionView{}, err
	}
	if !CanMove(cur.Status, StatusCompleted) {
		return InspectionView{}, &TransitionError{Status: cur.Status, Allowed: Transitions(cur.Status)}
	}
	if missing := cur.Checklist.Missing(); len(missing) > 0 || len(cur.Checklist.ItemIDs()) == 0 {
		ids := missing
		if len(ids) > MaxIncompleteIDs {
			ids = ids[:MaxIncompleteIDs]
		}
		return InspectionView{}, &IncompleteError{Missing: len(missing), ItemIDs: ids}
	}
	now := s.clock()
	by := p.UserID
	next.Status, next.CompletedAt, next.CompletedBy, next.CompletedByName, next.UpdatedAt = StatusCompleted, &now, &by, p.Name, now
	pr := next.Checklist.Progress()
	ch := Change{Audit: s.audit(ctx, a, orgaudit.ActionInspectionCompleted, next, map[string]any{"type": next.Type, "items": pr.Items,
		"attention": pr.Attention, "damaged": pr.Damaged}),
		Notices: notice(OutboxCompleted, next, p.UserID, false, now)}
	if err := s.save(ctx, cur, next, ch); err != nil {
		return InspectionView{}, err
	}
	return s.oneView(ctx, a, id)
}

// Cancel serves POST .../inspections/{inspectionId}/cancel.
func (s *Service) Cancel(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in CancelInput) (InspectionView, error) {
	a, err := s.writer(p, bookID)
	if err != nil {
		return InspectionView{}, err
	}
	cur, err := s.open(ctx, a, id)
	if err != nil {
		return InspectionView{}, err
	}
	f := map[string]string{}
	reason := text(f, "reason", in.Reason, 1, MaxReason)
	if err := invalid(f); err != nil {
		return InspectionView{}, err
	}
	now := s.clock()
	by := p.UserID
	next := cur
	next.Status, next.CancelReason, next.CancelledAt, next.CancelledBy, next.CancelledByName, next.UpdatedAt =
		StatusCancelled, reason, &now, &by, p.Name, now
	ch := Change{Audit: s.audit(ctx, a, orgaudit.ActionInspectionCancelled, next, map[string]any{"from_status": cur.Status, "reason": reason})}
	if err := s.save(ctx, cur, next, ch); err != nil {
		return InspectionView{}, err
	}
	return s.oneView(ctx, a, id)
}

// baseline returns the latest COMPLETED MOVE_IN of a MOVE_OUT's tenancy.
func (s *Service) baseline(ctx context.Context, in Inspection) (*Inspection, error) {
	if in.Type != TypeMoveOut || in.TenancyID == nil {
		return nil, nil
	}
	list, err := s.store.InspectionsOfTenancy(ctx, *in.TenancyID)
	if err != nil {
		return nil, err
	}
	var best *Inspection
	for i := range list {
		c := list[i]
		if c.ID == in.ID || c.Type != TypeMoveIn || c.Status != StatusCompleted || c.CompletedAt == nil || !c.Book.Same(in.Book) {
			continue
		}
		if best == nil || c.CompletedAt.After(*best.CompletedAt) {
			best = &c
		}
	}
	return best, nil
}
