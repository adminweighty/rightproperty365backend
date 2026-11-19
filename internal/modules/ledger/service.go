package ledger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/money"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Links looks up what an entry may be linked to. ok is false when the item
// does not exist or does not belong to the book.
type Links interface {
	Contact(ctx context.Context, book Book, id uuid.UUID) (ContactRef, bool, error)
	Listing(ctx context.Context, book Book, id uuid.UUID) (ListingRef, bool, error)
	Lease(ctx context.Context, book Book, id uuid.UUID) (LeaseRef, bool, error)
}

// Deps is what the service needs.
type Deps struct {
	Store Store
	// Files is the private object storage of the attachments.
	Files storage.Storage
	Links Links
	// Currencies returns the ISO 4217 codes a book may use (reference data).
	Currencies func(ctx context.Context) ([]string, error)
	// RentArrears returns the overdue rent dues of a book as of a date (the
	// tenancies module): the receivables report includes them. nil: none.
	RentArrears func(ctx context.Context, book Book, asOf string) ([]RentArrear, error)
	Logger      *slog.Logger
	Now         func() time.Time
}

// Service is the agent ledger.
type Service struct {
	store      Store
	files      storage.Storage
	links      Links
	currencies func(ctx context.Context) ([]string, error)
	arrears    func(ctx context.Context, book Book, asOf string) ([]RentArrear, error)
	logger     *slog.Logger
	now        func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, files: d.Files, links: d.Links, currencies: d.Currencies, arrears: d.RentArrears, logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func (s *Service) today() string { return s.clock().Format(time.DateOnly) }

// access is what the caller may do in one book.
type access struct {
	p        *auth.Principal
	book     Book
	write    bool
	manage   bool
	personal bool
}

// access resolves the caller's rights in the book bookID (an organisation
// ID, or the caller's own user ID for a direct owner's personal book). A
// book that is not theirs is ErrBookNotFound.
func (s *Service) access(p *auth.Principal, bookID uuid.UUID) (access, error) {
	if p == nil {
		return access{}, ErrUnauthenticated
	}
	if p.MayReadLedger(bookID) {
		return access{p: p, book: OrganisationBook(bookID), write: p.MayWriteLedger(bookID), manage: p.MayManageLedger(bookID)}, nil
	}
	if p.OwnsLedgerBook(bookID) {
		return access{p: p, book: PersonalBook(bookID), write: true, manage: true, personal: true}, nil
	}
	return access{}, ErrBookNotFound
}

// Check answers ErrBookNotFound (or ErrUnauthenticated) when the caller has
// no access to the book: handlers ask first, so another organisation never
// learns more than 404.
func (s *Service) Check(p *auth.Principal, bookID uuid.UUID) error {
	_, err := s.access(p, bookID)
	return err
}

func (a access) mine(e Entry) bool { return e.AuthorUserID != nil && *e.AuthorUserID == a.p.UserID }

// mayCorrect: a manager any entry; a writer their own unreconciled one.
func (a access) mayCorrect(e Entry) bool {
	if a.manage {
		return true
	}
	return a.write && a.mine(e) && e.Status != StatusReconciled
}

// mayAnnotate: notes and attachments, a manager on any entry, a writer on their own.
func (a access) mayAnnotate(e Entry) bool { return a.manage || (a.write && a.mine(e)) }

func (a access) can(e Entry) Can {
	reversible := e.Type != TypeReversal && e.ReversedBy == nil
	return Can{
		Reverse: reversible && a.mayCorrect(e), Correct: reversible && a.mayCorrect(e) && e.TenancyID == nil, Reconcile: a.manage,
		Note: a.mayAnnotate(e), Attach: a.mayAnnotate(e),
	}
}

func (s *Service) entry(ctx context.Context, a access, id uuid.UUID) (Entry, error) {
	e, err := s.store.Entry(ctx, id)
	if err != nil {
		return Entry{}, err
	}
	if !e.Book.Same(a.book) {
		return Entry{}, ErrEntryNotFound
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

// Label is the audit label of an entry.
func Label(e Entry) string {
	return fmt.Sprintf("%s %s %s %s", e.Type, e.Amount, e.Currency, e.EffectiveDate)
}

// event builds the audit record of an action on the book (nil for a personal
// book: there is no organisation audit trail; the action is logged).
func (s *Service) event(ctx context.Context, a access, action, targetType string, targetID uuid.UUID, label string, before, after map[string]any) []orgaudit.Event {
	attrs := []slog.Attr{slog.Bool("audit", true), slog.String("event", "ledger."+strings.ToLower(action)),
		slog.String("trace_id", logging.RequestID(ctx)), slog.String("book_id", a.book.ID().String()),
		slog.String("actor_user_id", a.p.UserID.String()), slog.String("target_id", targetID.String())}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", attrs...)
	if a.book.OrganisationID == nil {
		return nil
	}
	return []orgaudit.Event{orgaudit.New(*a.book.OrganisationID, a.p.UserID, a.p.Name, action, targetType, targetID, label, before, after, s.clock())}
}

func entryAudit(e Entry) map[string]any {
	m := map[string]any{"type": e.Type, "direction": e.Direction, "amount": e.Amount, "currency": e.Currency,
		"effective_date": e.EffectiveDate, "status": e.Status}
	if e.Method != "" {
		m["method"] = e.Method
	}
	if e.Category != "" {
		m["category"] = e.Category
	}
	if e.Branch != "" {
		m["branch"] = e.Branch
	}
	if e.TenancyID != nil {
		m["tenancy_id"] = e.TenancyID.String()
	}
	if e.PropertyID != nil {
		m["property_id"] = e.PropertyID.String()
	}
	return m
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

type viewer struct {
	s          *Service
	a          access
	categories map[string]string
	contacts   map[uuid.UUID]*ContactRef
	listings   map[uuid.UUID]*ListingRef
	leases     map[uuid.UUID]*LeaseRef
}

func (s *Service) viewer(ctx context.Context, a access) (*viewer, error) {
	v := &viewer{s: s, a: a, categories: map[string]string{}, contacts: map[uuid.UUID]*ContactRef{}, listings: map[uuid.UUID]*ListingRef{},
		leases: map[uuid.UUID]*LeaseRef{}}
	cats, err := s.categories(ctx, a.book)
	if err != nil {
		return nil, err
	}
	for _, c := range cats {
		v.categories[c.Code] = c.Label
	}
	return v, nil
}

func (v *viewer) contact(ctx context.Context, id *uuid.UUID) (*ContactRef, error) {
	if id == nil {
		return nil, nil
	}
	if r, ok := v.contacts[*id]; ok {
		return r, nil
	}
	ref := &ContactRef{ID: *id}
	if v.s.links != nil {
		got, ok, err := v.s.links.Contact(ctx, v.a.book, *id)
		if err != nil {
			return nil, err
		}
		if ok {
			ref = &got
		}
	}
	v.contacts[*id] = ref
	return ref, nil
}

func (v *viewer) listing(ctx context.Context, id *uuid.UUID) (*ListingRef, error) {
	if id == nil {
		return nil, nil
	}
	if r, ok := v.listings[*id]; ok {
		return r, nil
	}
	ref := &ListingRef{ID: *id}
	if v.s.links != nil {
		got, ok, err := v.s.links.Listing(ctx, v.a.book, *id)
		if err != nil {
			return nil, err
		}
		if ok {
			ref = &got
		}
	}
	v.listings[*id] = ref
	return ref, nil
}

func (v *viewer) lease(ctx context.Context, id *uuid.UUID) (*LeaseRef, error) {
	if id == nil {
		return nil, nil
	}
	if r, ok := v.leases[*id]; ok {
		return r, nil
	}
	ref := &LeaseRef{ID: *id}
	if v.s.links != nil {
		got, ok, err := v.s.links.Lease(ctx, v.a.book, *id)
		if err != nil {
			return nil, err
		}
		if ok {
			ref = &got
		}
	}
	v.leases[*id] = ref
	return ref, nil
}

func (v *viewer) view(ctx context.Context, e Entry) (EntryView, error) {
	info, _ := typeInfo(e.Type)
	signed := money.Rat(e.Amount)
	if e.Direction == Out {
		signed.Neg(signed)
	}
	out := EntryView{
		ID: e.ID, Type: e.Type, TypeLabel: info.Label, Direction: e.Direction, Amount: e.Amount,
		SignedAmount: money.Format(signed, e.Currency), Currency: e.Currency, MinorUnit: money.MinorUnit(e.Currency),
		EffectiveDate: e.EffectiveDate, RecordedAt: e.RecordedAt, Author: PersonRef{UserID: e.AuthorUserID, Name: e.AuthorName},
		Status: e.Status, StatusLabel: StatusLabel(e.Status), Description: e.Description, Reference: e.Reference,
		Branch: strPtr(e.Branch), ReversalOf: e.ReversalOf, ReversedBy: e.ReversedBy, CorrectionOf: e.CorrectionOf,
		CorrectedBy: e.CorrectedBy, TransferPair: e.TransferPair, Settles: e.Settles, Reason: strPtr(e.Reason), TenancyID: e.TenancyID,
		PropertyID: e.PropertyID, IsReversed: e.ReversedBy != nil, Cash: e.Cash(), NoteCount: e.NoteCount, AttachmentCount: e.AttachmentCount, Can: v.a.can(e),
	}
	if e.Category != "" {
		code := e.Category
		lbl, ok := v.categories[code]
		if !ok {
			lbl = code
		}
		out.Category, out.CategoryLabel = &code, &lbl
	}
	if e.Method != "" {
		m, lbl := e.Method, MethodLabel(e.Method)
		out.Method, out.MethodLabel = &m, &lbl
	}
	if e.Status == StatusReconciled && e.ReconciledAt != nil {
		out.Reconciliation = &ReconciliationView{At: *e.ReconciledAt, By: PersonRef{UserID: e.ReconciledBy, Name: e.ReconciledName},
			Note: strPtr(e.ReconcileNote)}
	}
	var err error
	if out.Contact, err = v.contact(ctx, e.ContactID); err != nil {
		return EntryView{}, err
	}
	if out.Listing, err = v.listing(ctx, e.ListingID); err != nil {
		return EntryView{}, err
	}
	if out.Lease, err = v.lease(ctx, e.LeaseID); err != nil {
		return EntryView{}, err
	}
	return out, nil
}

func (v *viewer) views(ctx context.Context, list []Entry) ([]EntryView, error) {
	out := make([]EntryView, 0, len(list))
	for _, e := range list {
		ev, err := v.view(ctx, e)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

func noteView(n Note) NoteView {
	return NoteView{ID: n.ID, Text: n.Text, Author: PersonRef{UserID: n.AuthorUserID, Name: n.AuthorName}, CreatedAt: n.CreatedAt}
}

func attachmentView(book Book, a Attachment) AttachmentView {
	return AttachmentView{ID: a.ID, FileName: a.FileName, ContentType: a.ContentType, Size: a.Size,
		UploadedBy: PersonRef{UserID: a.UploadedBy, Name: a.UploadedByName}, UploadedAt: a.UploadedAt, ScanStatus: a.ScanStatus,
		DownloadPath: fmt.Sprintf("/v1/agent/organisations/%s/ledger/entries/%s/attachments/%s", book.ID(), a.EntryID, a.ID)}
}

// ---------------------------------------------------------------------------
// Categories
// ---------------------------------------------------------------------------

func (s *Service) categories(ctx context.Context, book Book) ([]Category, error) {
	list, err := s.store.Categories(ctx, book)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		if err := s.store.EnsureCategories(ctx, book, withClock(DefaultCategories(), s.clock())); err != nil {
			return nil, err
		}
		return s.store.Categories(ctx, book)
	}
	return list, nil
}

func withClock(list []Category, now time.Time) []Category {
	for i := range list {
		list[i].CreatedAt, list[i].UpdatedAt = now, now
	}
	return list
}

func categoryView(c Category) CategoryView {
	return CategoryView{Code: c.Code, Label: c.Label, Kind: c.Kind, Active: c.Active, Default: c.Default, Used: c.Used, SortOrder: c.SortOrder}
}

// Categories serves GET .../categories (active nil: all).
func (s *Service) Categories(ctx context.Context, p *auth.Principal, bookID uuid.UUID, active *bool) ([]CategoryView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return nil, err
	}
	list, err := s.categories(ctx, a.book)
	if err != nil {
		return nil, err
	}
	out := []CategoryView{}
	for _, c := range list {
		if active == nil || c.Active == *active {
			out = append(out, categoryView(c))
		}
	}
	return out, nil
}

// CategoryInput is the body of POST and PATCH .../categories.
type CategoryInput struct {
	Code   *string `json:"code"`
	Label  *string `json:"label"`
	Kind   *string `json:"kind"`
	Active *bool   `json:"active"`
}

var validKinds = []string{KindIn, KindOut, KindAny}

// slug makes a category code from a label.
func slug(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if len(out) > 40 {
		out = strings.TrimSuffix(out[:40], "-")
	}
	return out
}

func validCode(c string) bool {
	if c == "" || len(c) > 40 {
		return false
	}
	for i, r := range c {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (r == '-' && i > 0)
		if !ok {
			return false
		}
	}
	return true
}

// CreateCategory serves POST .../categories (OWNER and MANAGER).
func (s *Service) CreateCategory(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in CategoryInput) (CategoryView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return CategoryView{}, err
	}
	if !a.manage {
		return CategoryView{}, ErrForbidden
	}
	list, err := s.categories(ctx, a.book)
	if err != nil {
		return CategoryView{}, err
	}
	f := map[string]string{}
	label := ""
	if in.Label != nil {
		label = strings.TrimSpace(*in.Label)
	}
	if label == "" || utf8.RuneCountInString(label) > MaxLabel {
		f["label"] = fmt.Sprintf("is required (at most %d characters)", MaxLabel)
	}
	code := slug(label)
	if in.Code != nil {
		code = strings.TrimSpace(*in.Code)
	}
	if !validCode(code) {
		f["code"] = "must be 1 to 40 lower-case letters, digits and dashes, starting with a letter or digit"
	}
	kind := ""
	if in.Kind != nil {
		kind = *in.Kind
	}
	if !slices.Contains(validKinds, kind) {
		f["kind"] = "must be IN, OUT or ANY"
	}
	if len(list) >= MaxCategories {
		f["code"] = fmt.Sprintf("a book has at most %d categories", MaxCategories)
	}
	if len(f) > 0 {
		return CategoryView{}, &ValidationError{Fields: f}
	}
	order := 0
	for _, c := range list {
		order = max(order, c.SortOrder)
	}
	now := s.clock()
	active := in.Active == nil || *in.Active
	by := p.UserID
	c := Category{ID: newID(), Book: a.book, Code: code, Label: label, Kind: kind, Active: active, SortOrder: order + 10, CreatedBy: &by,
		CreatedAt: now, UpdatedAt: now}
	if err := s.store.CreateCategory(ctx, c); err != nil {
		return CategoryView{}, err
	}
	return categoryView(c), nil
}

// UpdateCategory serves PATCH .../categories/{code} (OWNER and MANAGER).
func (s *Service) UpdateCategory(ctx context.Context, p *auth.Principal, bookID uuid.UUID, code string, in CategoryInput) (CategoryView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return CategoryView{}, err
	}
	list, err := s.categories(ctx, a.book)
	if err != nil {
		return CategoryView{}, err
	}
	i := slices.IndexFunc(list, func(c Category) bool { return c.Code == code })
	if i < 0 {
		return CategoryView{}, ErrCategoryNotFound
	}
	if !a.manage {
		return CategoryView{}, ErrForbidden
	}
	c := list[i]
	f := map[string]string{}
	if in.Code != nil && *in.Code != c.Code {
		f["code"] = "cannot be changed"
	}
	if in.Label != nil {
		label := strings.TrimSpace(*in.Label)
		if label == "" || utf8.RuneCountInString(label) > MaxLabel {
			f["label"] = fmt.Sprintf("must be 1 to %d characters", MaxLabel)
		}
		c.Label = label
	}
	if in.Kind != nil && *in.Kind != c.Kind {
		switch {
		case !slices.Contains(validKinds, *in.Kind):
			f["kind"] = "must be IN, OUT or ANY"
		case c.Used:
			f["kind"] = "cannot change once entries use the category"
		}
		c.Kind = *in.Kind
	}
	if in.Active != nil {
		c.Active = *in.Active
	}
	if len(f) > 0 {
		return CategoryView{}, &ValidationError{Fields: f}
	}
	c.UpdatedAt = s.clock()
	if err := s.store.UpdateCategory(ctx, c); err != nil {
		return CategoryView{}, err
	}
	return categoryView(c), nil
}

// ---------------------------------------------------------------------------
// Options
// ---------------------------------------------------------------------------

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
		if money.ValidCurrencyCode(c) && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Options serves GET .../options.
func (s *Service) Options(ctx context.Context, p *auth.Principal, bookID uuid.UUID) (Options, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Options{}, err
	}
	out := Options{Methods: []CodeLabel{}, Categories: []CategoryView{}, Currencies: []CurrencyOption{},
		Permissions: Permissions{Write: a.write, Manage: a.manage, Export: a.manage}}
	for _, t := range types {
		out.Types = append(out.Types, TypeOption{Code: t.Code, Label: t.Label, Direction: strPtr(t.Direction), Cash: t.Cash, Category: strPtr(t.Category)})
	}
	for _, m := range Methods() {
		out.Methods = append(out.Methods, CodeLabel{Code: m, Label: MethodLabel(m)})
	}
	for _, st := range Statuses() {
		out.Statuses = append(out.Statuses, StatusOption{Code: st, Label: StatusLabel(st), Settable: false})
	}
	curs, err := s.allowedCurrencies(ctx)
	if err != nil {
		return Options{}, err
	}
	for _, c := range curs {
		out.Currencies = append(out.Currencies, CurrencyOption{Code: c, MinorUnit: money.MinorUnit(c)})
	}
	cats, err := s.categories(ctx, a.book)
	if err != nil {
		return Options{}, err
	}
	for _, c := range cats {
		if c.Active {
			out.Categories = append(out.Categories, categoryView(c))
		}
	}
	if out.Branches, err = s.store.Branches(ctx, a.book); err != nil {
		return Options{}, err
	}
	if out.Branches == nil {
		out.Branches = []string{}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Reading entries
// ---------------------------------------------------------------------------

// List serves GET .../entries.
func (s *Service) List(ctx context.Context, p *auth.Principal, bookID uuid.UUID, f Filter, page, size int) (Page, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Page{}, err
	}
	f.Limit, f.Offset = size, (page-1)*size
	list, total, err := s.store.Entries(ctx, a.book, f)
	if err != nil {
		return Page{}, err
	}
	v, err := s.viewer(ctx, a)
	if err != nil {
		return Page{}, err
	}
	items, err := v.views(ctx, list)
	if err != nil {
		return Page{}, err
	}
	pages := 0
	if size > 0 {
		pages = (total + size - 1) / size
	}
	return Page{Items: items, Page: page, PageSize: size, Total: total, TotalPages: pages}, nil
}

// Get serves GET .../entries/{id}.
func (s *Service) Get(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (EntryDetail, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return EntryDetail{}, err
	}
	e, err := s.entry(ctx, a, id)
	if err != nil {
		return EntryDetail{}, err
	}
	return s.detail(ctx, a, e)
}

func (s *Service) detail(ctx context.Context, a access, e Entry) (EntryDetail, error) {
	v, err := s.viewer(ctx, a)
	if err != nil {
		return EntryDetail{}, err
	}
	out := EntryDetail{Notes: []NoteView{}, Attachments: []AttachmentView{}, Related: []EntryView{}}
	if out.Entry, err = v.view(ctx, e); err != nil {
		return EntryDetail{}, err
	}
	notes, err := s.store.Notes(ctx, e.ID)
	if err != nil {
		return EntryDetail{}, err
	}
	for _, n := range notes {
		out.Notes = append(out.Notes, noteView(n))
	}
	files, err := s.store.Attachments(ctx, e.ID)
	if err != nil {
		return EntryDetail{}, err
	}
	if s.files != nil && len(files) > 0 {
		keys := make([]string, len(files))
		for i, f := range files {
			keys[i] = f.ObjectKey
		}
		statuses := storage.ScanStatuses(ctx, s.files, keys...)
		for i := range files {
			files[i].ScanStatus = statuses[files[i].ObjectKey]
		}
	}
	for _, f := range files {
		out.Attachments = append(out.Attachments, attachmentView(a.book, f))
	}
	related, err := s.store.Related(ctx, a.book, e.ID)
	if err != nil {
		return EntryDetail{}, err
	}
	seen := map[uuid.UUID]bool{e.ID: true}
	var rel []Entry
	for _, r := range related {
		if !seen[r.ID] {
			seen[r.ID] = true
			rel = append(rel, r)
		}
	}
	for _, id := range []*uuid.UUID{e.ReversalOf, e.ReversedBy, e.CorrectionOf, e.CorrectedBy, e.TransferPair, e.Settles} {
		if id == nil || seen[*id] {
			continue
		}
		seen[*id] = true
		r, err := s.store.Entry(ctx, *id)
		if errors.Is(err, ErrEntryNotFound) {
			continue
		}
		if err != nil {
			return EntryDetail{}, err
		}
		if r.Book.Same(a.book) {
			rel = append(rel, r)
		}
	}
	sortEntries(rel, true)
	if out.Related, err = v.views(ctx, rel); err != nil {
		return EntryDetail{}, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Posting
// ---------------------------------------------------------------------------

// TransferTarget is where a transfer goes: another method, another branch, or both.
type TransferTarget struct {
	Method *string `json:"method"`
	Branch *string `json:"branch"`
}

// EntryInput is the body of POST .../entries (and of a correction).
type EntryInput struct {
	Type          string          `json:"type"`
	Direction     *string         `json:"direction"`
	Category      *string         `json:"category"`
	Method        *string         `json:"method"`
	Amount        *string         `json:"amount"`
	Currency      string          `json:"currency"`
	EffectiveDate string          `json:"effective_date"`
	Description   *string         `json:"description"`
	Reference     *string         `json:"reference"`
	ContactID     *uuid.UUID      `json:"contact_id"`
	ListingID     *uuid.UUID      `json:"listing_id"`
	LeaseID       *uuid.UUID      `json:"lease_id"`
	TenancyID     *uuid.UUID      `json:"tenancy_id"`
	Branch        *string         `json:"branch"`
	Notes         *string         `json:"notes"`
	TransferTo    *TransferTarget `json:"transfer_to"`
	Settles       *uuid.UUID      `json:"settles"`
	// Status and GatewayReference are read only to be refused: manual
	// entries are always USER_RECORDED_UNVERIFIED (FR-028).
	Status           *string `json:"status"`
	GatewayReference *string `json:"gateway_reference"`
}

// ValidationError is 400 VALIDATION_FAILED with per-field messages.
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// OpeningExistsError is 409 LEDGER_OPENING_BALANCE_EXISTS.
type OpeningExistsError struct {
	Existing                 uuid.UUID
	Currency, Method, Branch string
}

func (e *OpeningExistsError) Error() string { return ErrOpeningBalanceExists.Error() }

// Unwrap lets errors.Is find ErrOpeningBalanceExists.
func (e *OpeningExistsError) Unwrap() error { return ErrOpeningBalanceExists }

// ReversedError is 409 LEDGER_ENTRY_ALREADY_REVERSED.
type ReversedError struct{ ReversedBy uuid.UUID }

func (e *ReversedError) Error() string { return ErrAlreadyReversed.Error() }

// Unwrap lets errors.Is find ErrAlreadyReversed.
func (e *ReversedError) Unwrap() error { return ErrAlreadyReversed }

func validDate(s string) bool {
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Year() >= 1900 && t.Year() <= 2999 && t.Format(time.DateOnly) == s
}

func trimmed(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

// draft is a validated body: one entry (two for a transfer) and the first note.
type draft struct {
	entries []Entry
	note    string
}

// build validates a body into the entries it posts (not yet stored).
func (s *Service) build(ctx context.Context, a access, in EntryInput, f map[string]string, reversing []uuid.UUID) (draft, error) {
	info, ok := typeInfo(in.Type)
	if !ok || in.Type == TypeReversal {
		f["type"] = "must be one of " + strings.Join(typeCodes(), ", ")
	}
	if in.Status != nil {
		f["status"] = "is set by the platform: manual entries are always " + StatusUnverified + "; mark one reconciled with POST .../reconcile"
	}
	if in.GatewayReference != nil {
		f["gateway_reference"] = "cannot be set: no payment gateway confirms ledger entries"
	}
	// Currency and amount.
	cur := strings.ToUpper(strings.TrimSpace(in.Currency))
	curs, err := s.allowedCurrencies(ctx)
	if err != nil {
		return draft{}, err
	}
	var amount *big.Rat
	switch {
	case cur == "":
		f["currency"] = "is required"
	case !money.ValidCurrencyCode(cur) || !slices.Contains(curs, cur):
		f["currency"] = "must be an ISO 4217 currency code from reference data, such as USD"
	}
	if in.Amount == nil || strings.TrimSpace(*in.Amount) == "" {
		f["amount"] = "is required: a positive decimal string such as \"123.45\""
	} else if _, bad := f["currency"]; !bad {
		r, problem := money.ParseAmount(*in.Amount, cur)
		switch problem {
		case "":
			amount = r
		case money.ProblemDecimals:
			if d := money.MinorUnit(cur); d == 0 {
				f["amount"] = "must be a whole number for " + cur + " (it has no minor unit)"
			} else {
				f["amount"] = fmt.Sprintf("must have at most %d decimals for %s", d, cur)
			}
		case money.ProblemZero:
			f["amount"] = "must be more than zero; the sign comes from direction"
		default:
			f["amount"] = "must be a positive decimal string such as \"123.45\" (no sign, exponent or thousands separator)"
		}
	}
	if !validDate(in.EffectiveDate) {
		f["effective_date"] = "must be a date (YYYY-MM-DD)"
	}
	// Direction.
	direction := ""
	if in.Direction != nil {
		direction = *in.Direction
	}
	switch {
	case !ok:
	case in.Type == TypeTransfer:
		if direction != "" && direction != Out {
			f["direction"] = "a transfer is recorded as an OUT / IN pair: omit direction (or send OUT)"
		}
		direction = Out
	case info.Direction != "":
		if direction != "" && direction != info.Direction {
			f["direction"] = fmt.Sprintf("is %s for %s", info.Direction, in.Type)
		}
		direction = info.Direction
	default:
		if direction != In && direction != Out {
			f["direction"] = "must be IN or OUT"
		}
	}
	// Method.
	method := trimmed(in.Method)
	memo := ok && !info.Cash
	switch {
	case memo && method != "":
		f["method"] = "must be empty for a receivable or payable (nothing has been paid yet)"
	case !memo && method == "":
		f["method"] = "is required: CASH, BANK, MOBILE_MONEY or OTHER"
	case method != "" && !slices.Contains(Methods(), method):
		f["method"] = "must be CASH, BANK, MOBILE_MONEY or OTHER"
	}
	// Category.
	category := trimmed(in.Category)
	if category == "" && ok {
		category = info.Category
	}
	if category != "" {
		cats, err := s.categories(ctx, a.book)
		if err != nil {
			return draft{}, err
		}
		i := slices.IndexFunc(cats, func(c Category) bool { return c.Code == category })
		switch {
		case i < 0:
			f["category"] = "is not a category of this book"
		case !cats[i].Active:
			f["category"] = "is deactivated"
		case direction != "" && cats[i].Kind != KindAny && cats[i].Kind != direction:
			f["category"] = fmt.Sprintf("is a %s category; this entry is %s", cats[i].Kind, direction)
		}
	} else if ok && (in.Type == TypeReceivable || in.Type == TypePayable) {
		f["category"] = "is required for " + in.Type
	}
	// Text.
	desc, ref, branch, note := trimmed(in.Description), trimmed(in.Reference), trimmed(in.Branch), trimmed(in.Notes)
	if utf8.RuneCountInString(desc) > MaxDescription {
		f["description"] = fmt.Sprintf("must be at most %d characters", MaxDescription)
	}
	if utf8.RuneCountInString(ref) > MaxReference {
		f["reference"] = fmt.Sprintf("must be at most %d characters", MaxReference)
	}
	if utf8.RuneCountInString(branch) > MaxBranch {
		f["branch"] = fmt.Sprintf("must be at most %d characters", MaxBranch)
	}
	if utf8.RuneCountInString(note) > MaxNote {
		f["notes"] = fmt.Sprintf("must be at most %d characters", MaxNote)
	}
	// Links: the same book only.
	if in.TenancyID != nil {
		f["tenancy_id"] = "is set by POST .../tenancies/{id}/receipts: record rent there"
	}
	if in.ContactID != nil && s.links != nil {
		if _, found, err := s.links.Contact(ctx, a.book, *in.ContactID); err != nil {
			return draft{}, err
		} else if !found {
			f["contact_id"] = "is not a contact of this book"
		}
	}
	if in.ListingID != nil && s.links != nil {
		if _, found, err := s.links.Listing(ctx, a.book, *in.ListingID); err != nil {
			return draft{}, err
		} else if !found {
			f["listing_id"] = "is not a listing of this book"
		}
	}
	if in.LeaseID != nil && s.links != nil {
		if _, found, err := s.links.Lease(ctx, a.book, *in.LeaseID); err != nil {
			return draft{}, err
		} else if !found {
			f["lease_id"] = "is not a lease of this book"
		}
	}
	// Transfer.
	toMethod, toBranch := method, branch
	if in.Type == TypeTransfer {
		if in.TransferTo == nil {
			f["transfer_to"] = "is required for a transfer: {method, branch}"
		} else {
			if in.TransferTo.Method != nil {
				toMethod = strings.TrimSpace(*in.TransferTo.Method)
			}
			if in.TransferTo.Branch != nil {
				toBranch = strings.TrimSpace(*in.TransferTo.Branch)
			}
			switch {
			case !slices.Contains(Methods(), toMethod):
				f["transfer_to.method"] = "must be CASH, BANK, MOBILE_MONEY or OTHER"
			case utf8.RuneCountInString(toBranch) > MaxBranch:
				f["transfer_to.branch"] = fmt.Sprintf("must be at most %d characters", MaxBranch)
			case toMethod == method && strings.EqualFold(toBranch, branch):
				f["transfer_to"] = "must name another method or another branch"
			}
		}
		if in.Settles != nil {
			f["settles"] = "a transfer settles nothing"
		}
	} else if in.TransferTo != nil {
		f["transfer_to"] = "is only for a transfer"
	}
	// Settlement of a receivable or payable.
	if in.Settles != nil && in.Type != TypeTransfer {
		target, err := s.store.Entry(ctx, *in.Settles)
		switch {
		case errors.Is(err, ErrEntryNotFound) || (err == nil && !target.Book.Same(a.book)):
			f["settles"] = "is not an entry of this book"
		case err != nil:
			return draft{}, err
		case target.Type != TypeReceivable && target.Type != TypePayable:
			f["settles"] = "must be a receivable or a payable"
		case target.ReversedBy != nil:
			f["settles"] = "is reversed"
		case memo || in.Type == TypeOpeningBalance:
			f["settles"] = "only a cash entry settles a receivable or payable"
		case target.Type == TypeReceivable && direction != In:
			f["settles"] = "a receivable is settled by money coming IN"
		case target.Type == TypePayable && direction != Out:
			f["settles"] = "a payable is settled by money going OUT"
		case target.Currency != cur:
			f["settles"] = "is in " + target.Currency + ": a settlement is in the same currency"
		}
	}
	if len(f) > 0 {
		return draft{}, &ValidationError{Fields: f}
	}
	now := s.clock()
	author := a.p.UserID
	base := Entry{
		ID: newID(), Book: a.book, Type: in.Type, Direction: direction, Category: category, Method: method,
		Amount: money.Format(amount, cur), Currency: cur, EffectiveDate: in.EffectiveDate, RecordedAt: now,
		AuthorUserID: &author, AuthorName: a.p.Name, Status: StatusUnverified, ContactID: in.ContactID, ListingID: in.ListingID,
		LeaseID: in.LeaseID, Branch: branch, Description: desc, Reference: ref, Settles: in.Settles,
	}
	out := draft{entries: []Entry{base}, note: note}
	if in.Type == TypeTransfer {
		leg := base
		leg.ID, leg.Direction, leg.Method, leg.Branch = newID(), In, toMethod, toBranch
		pair := base.ID
		leg.TransferPair = &pair
		out.entries = append(out.entries, leg)
	}
	// One live opening balance per currency, method and branch.
	if in.Type == TypeOpeningBalance {
		if existing, found, err := s.store.OpeningBalance(ctx, a.book, cur, method, branch); err != nil {
			return draft{}, err
		} else if found && !slices.Contains(reversing, existing.ID) {
			return draft{}, &OpeningExistsError{Existing: existing.ID, Currency: cur, Method: method, Branch: branch}
		}
	}
	return out, nil
}

func typeCodes() []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		out = append(out, t.Code)
	}
	return out
}

// post finishes a draft into a Change: the transfer link and the first note.
func (d draft) post(ch *Change, a access) {
	ch.Entries = append(ch.Entries, d.entries...)
	if len(d.entries) == 2 {
		pair := d.entries[1].ID
		ch.PostLinks = append(ch.PostLinks, LinkSet{EntryID: d.entries[0].ID, TransferPair: &pair})
		d.entries[0].TransferPair = &pair
	}
	if d.note != "" {
		author := a.p.UserID
		ch.Notes = append(ch.Notes, Note{ID: newID(), EntryID: d.entries[0].ID, Text: d.note, AuthorUserID: &author, AuthorName: a.p.Name,
			CreatedAt: d.entries[0].RecordedAt})
	}
}

// storeErr maps a store failure of a draft's opening balance to the 409 answer.
func storeErr(err error, d draft) error {
	if errors.Is(err, ErrOpeningBalanceExists) && len(d.entries) > 0 {
		e := d.entries[0]
		return &OpeningExistsError{Currency: e.Currency, Method: e.Method, Branch: e.Branch}
	}
	if errors.Is(err, ErrOverSettled) {
		return &ValidationError{Fields: map[string]string{"amount": "is more than the outstanding amount of the entry it settles"}}
	}
	return err
}

// Create serves POST .../entries: the entry is posted immediately.
func (s *Service) Create(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in EntryInput) (Posted, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Posted{}, err
	}
	if !a.write {
		return Posted{}, ErrForbidden
	}
	d, err := s.build(ctx, a, in, map[string]string{}, nil)
	if err != nil {
		return Posted{}, err
	}
	var ch Change
	d.post(&ch, a)
	if err := s.settleCheck(ctx, a, d, nil); err != nil {
		return Posted{}, err
	}
	for _, e := range d.entries {
		ch.Audit = append(ch.Audit, s.event(ctx, a, orgaudit.ActionLedgerEntryCreated, orgaudit.TargetLedgerEntry, e.ID, Label(e), nil, entryAudit(e))...)
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return Posted{}, storeErr(err, d)
	}
	created, err := s.reload(ctx, a, ids(d.entries))
	if err != nil {
		return Posted{}, err
	}
	return Posted{Entry: created[0], Created: created}, nil
}

// settleCheck refuses a settlement larger than the outstanding amount (the
// stores check again inside their transaction).
func (s *Service) settleCheck(ctx context.Context, a access, d draft, reversing []uuid.UUID) error {
	e := d.entries[0]
	if e.Settles == nil {
		return nil
	}
	target, err := s.store.Entry(ctx, *e.Settles)
	if err != nil {
		return err
	}
	settled, err := s.store.Settled(ctx, a.book, []uuid.UUID{target.ID})
	if err != nil {
		return err
	}
	left := money.Rat(target.Amount)
	for _, p := range settled[target.ID] {
		if slices.Contains(reversing, p.ID) {
			continue
		}
		left.Sub(left, money.Rat(p.Amount))
	}
	if money.Rat(e.Amount).Cmp(left) > 0 {
		return &ValidationError{Fields: map[string]string{"amount": fmt.Sprintf("is more than the %s %s still outstanding on the entry it settles",
			money.Format(left, target.Currency), target.Currency)}}
	}
	return nil
}

func ids(list []Entry) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(list))
	for _, e := range list {
		out = append(out, e.ID)
	}
	return out
}

func (s *Service) reload(ctx context.Context, a access, idList []uuid.UUID) ([]EntryView, error) {
	v, err := s.viewer(ctx, a)
	if err != nil {
		return nil, err
	}
	out := make([]EntryView, 0, len(idList))
	for _, id := range idList {
		e, err := s.store.Entry(ctx, id)
		if err != nil {
			return nil, err
		}
		ev, err := v.view(ctx, e)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// ReverseInput is the body of POST .../entries/{id}/reverse.
type ReverseInput struct {
	Reason        string  `json:"reason"`
	EffectiveDate *string `json:"effective_date"`
}

// targets returns the entries a reversal or correction of e acts on (both
// legs of a transfer), after the checks shared by both.
func (s *Service) targets(ctx context.Context, a access, e Entry) ([]Entry, error) {
	if e.Type == TypeReversal {
		return nil, ErrNotReversible
	}
	if !a.mayCorrect(e) {
		return nil, ErrForbidden
	}
	list := []Entry{e}
	if e.TransferPair != nil {
		pair, err := s.entry(ctx, a, *e.TransferPair)
		if err != nil {
			return nil, err
		}
		if !a.mayCorrect(pair) {
			return nil, ErrForbidden
		}
		list = append(list, pair)
	}
	for _, t := range list {
		if t.ReversedBy != nil {
			return nil, &ReversedError{ReversedBy: *t.ReversedBy}
		}
	}
	return list, nil
}

// reversalOf builds the REVERSAL entry of e.
func reversalOf(a access, e Entry, reason, date string, now time.Time) Entry {
	author := a.p.UserID
	dir := In
	if e.Direction == In {
		dir = Out
	}
	orig := e.ID
	desc := "Reversal of " + Label(e)
	if utf8.RuneCountInString(desc) > MaxDescription {
		desc = string([]rune(desc)[:MaxDescription])
	}
	return Entry{
		ID: newID(), Book: e.Book, Type: TypeReversal, Direction: dir, Category: e.Category, Method: e.Method, Amount: e.Amount,
		Currency: e.Currency, EffectiveDate: date, RecordedAt: now, AuthorUserID: &author, AuthorName: a.p.Name, Status: StatusUnverified,
		ReversalOf: &orig, Reason: reason, ContactID: e.ContactID, ListingID: e.ListingID, LeaseID: e.LeaseID, TenancyID: e.TenancyID, Branch: e.Branch,
		Description: desc, Reference: e.Reference, PropertyID: e.PropertyID,
	}
}

func checkReason(reason string, f map[string]string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || utf8.RuneCountInString(reason) > MaxReason {
		f["reason"] = fmt.Sprintf("is required (at most %d characters)", MaxReason)
	}
	return reason
}

// Reverse serves POST .../entries/{id}/reverse: a REVERSAL entry (both legs
// of a transfer) is posted and the original is marked reversed_by it.
func (s *Service) Reverse(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in ReverseInput) (Posted, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Posted{}, err
	}
	e, err := s.entry(ctx, a, id)
	if err != nil {
		return Posted{}, err
	}
	if !a.write {
		return Posted{}, ErrForbidden
	}
	list, err := s.targets(ctx, a, e)
	if err != nil {
		return Posted{}, err
	}
	f := map[string]string{}
	reason := checkReason(in.Reason, f)
	date := e.EffectiveDate
	if in.EffectiveDate != nil {
		date = strings.TrimSpace(*in.EffectiveDate)
		switch {
		case !validDate(date):
			f["effective_date"] = "must be a date (YYYY-MM-DD)"
		case date < e.EffectiveDate:
			f["effective_date"] = "may not be before the original's effective date " + e.EffectiveDate
		}
	}
	if len(f) > 0 {
		return Posted{}, &ValidationError{Fields: f}
	}
	now := s.clock()
	var ch Change
	var revs []Entry
	for _, t := range list {
		r := reversalOf(a, t, reason, max(date, t.EffectiveDate), now)
		revs = append(revs, r)
		ch.NotReversed = append(ch.NotReversed, t.ID)
		ch.Reversals = append(ch.Reversals, r)
		rid := r.ID
		ch.Links = append(ch.Links, LinkSet{EntryID: t.ID, ReversedBy: &rid})
		ch.Audit = append(ch.Audit, s.event(ctx, a, orgaudit.ActionLedgerEntryReversed, orgaudit.TargetLedgerEntry, t.ID, Label(t),
			entryAudit(t), map[string]any{"reversal_id": rid.String(), "reason": reason, "effective_date": r.EffectiveDate})...)
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return Posted{}, s.reversedErr(ctx, err, e.ID)
	}
	created, err := s.reload(ctx, a, ids(revs))
	if err != nil {
		return Posted{}, err
	}
	updated, err := s.reload(ctx, a, ids(list))
	if err != nil {
		return Posted{}, err
	}
	return Posted{Entry: created[0], Created: created, Updated: updated}, nil
}

// reversedErr turns the store's ErrAlreadyReversed (a concurrent reversal) into the 409 with its details.
func (s *Service) reversedErr(ctx context.Context, err error, id uuid.UUID) error {
	if !errors.Is(err, ErrAlreadyReversed) {
		return err
	}
	if e, gerr := s.store.Entry(ctx, id); gerr == nil && e.ReversedBy != nil {
		return &ReversedError{ReversedBy: *e.ReversedBy}
	}
	return err
}

// CorrectInput is the body of POST .../entries/{id}/correct: a new entry and the reason.
type CorrectInput struct {
	EntryInput
	Reason string `json:"reason"`
}

// Correct serves POST .../entries/{id}/correct: in one transaction the
// original is reversed and the corrected entry is posted (correction_of the
// original, which gets corrected_by). FR-027: the predecessor stays.
func (s *Service) Correct(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in CorrectInput) (Posted, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return Posted{}, err
	}
	e, err := s.entry(ctx, a, id)
	if err != nil {
		return Posted{}, err
	}
	if !a.write {
		return Posted{}, ErrForbidden
	}
	if e.TenancyID != nil {
		return Posted{}, ErrRentReceipt
	}
	list, err := s.targets(ctx, a, e)
	if err != nil {
		return Posted{}, err
	}
	f := map[string]string{}
	reason := checkReason(in.Reason, f)
	// The entries being reversed no longer count (their opening balance,
	// their settlement).
	d, err := s.build(ctx, a, in.EntryInput, f, ids(list))
	if err != nil {
		return Posted{}, err
	}
	now := s.clock()
	var ch Change
	var revs []Entry
	for _, t := range list {
		r := reversalOf(a, t, reason, t.EffectiveDate, now)
		revs = append(revs, r)
		ch.NotReversed = append(ch.NotReversed, t.ID)
		ch.Reversals = append(ch.Reversals, r)
		rid := r.ID
		ch.Links = append(ch.Links, LinkSet{EntryID: t.ID, ReversedBy: &rid})
	}
	orig := e.ID
	for i := range d.entries {
		d.entries[i].CorrectionOf = &orig
		d.entries[i].Reason = reason
	}
	d.post(&ch, a)
	first := d.entries[0].ID
	for _, t := range list {
		ch.PostLinks = append(ch.PostLinks, LinkSet{EntryID: t.ID, CorrectedBy: &first})
	}
	if err := s.settleCheck(ctx, a, d, ids(list)); err != nil {
		return Posted{}, err
	}
	ch.Audit = append(ch.Audit, s.event(ctx, a, orgaudit.ActionLedgerEntryCorrected, orgaudit.TargetLedgerEntry, e.ID, Label(e),
		entryAudit(e), map[string]any{"reversal_id": revs[0].ID.String(), "corrected_by": first.String(), "reason": reason,
			"new": entryAudit(d.entries[0])})...)
	for _, n := range d.entries {
		ch.Audit = append(ch.Audit, s.event(ctx, a, orgaudit.ActionLedgerEntryCreated, orgaudit.TargetLedgerEntry, n.ID, Label(n), nil,
			entryAudit(n))...)
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return Posted{}, storeErr(s.reversedErr(ctx, err, e.ID), d)
	}
	created, err := s.reload(ctx, a, append(ids(revs), ids(d.entries)...))
	if err != nil {
		return Posted{}, err
	}
	updated, err := s.reload(ctx, a, ids(list))
	if err != nil {
		return Posted{}, err
	}
	return Posted{Entry: created[len(revs)], Created: created, Updated: updated}, nil
}

// ReconcileInput is the body of POST .../entries/{id}/reconcile.
type ReconcileInput struct {
	Reconciled *bool   `json:"reconciled"`
	Note       *string `json:"note"`
}

// Reconcile serves POST .../entries/{id}/reconcile (OWNER and MANAGER): the
// organisation marks an entry reconciled against its own statement (never a
// bank verification), or takes the mark back.
func (s *Service) Reconcile(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in ReconcileInput) (EntryView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return EntryView{}, err
	}
	e, err := s.entry(ctx, a, id)
	if err != nil {
		return EntryView{}, err
	}
	if !a.manage {
		return EntryView{}, ErrForbidden
	}
	f := map[string]string{}
	if in.Reconciled == nil {
		f["reconciled"] = "is required (true or false)"
	}
	note := trimmed(in.Note)
	if utf8.RuneCountInString(note) > MaxReconcile {
		f["note"] = fmt.Sprintf("must be at most %d characters", MaxReconcile)
	}
	if len(f) > 0 {
		return EntryView{}, &ValidationError{Fields: f}
	}
	status, action := StatusUnverified, orgaudit.ActionLedgerEntryUnreconciled
	if *in.Reconciled {
		status, action = StatusReconciled, orgaudit.ActionLedgerEntryReconciled
	}
	if status != e.Status || (status == StatusReconciled && note != e.ReconcileNote) {
		by := p.UserID
		after := map[string]any{"status": status}
		if note != "" {
			after["note"] = note
		}
		ch := Change{Reconcile: &Reconciliation{EntryID: e.ID, Status: status, At: s.clock(), By: &by, ByName: p.Name, Note: note},
			Audit: s.event(ctx, a, action, orgaudit.TargetLedgerEntry, e.ID, Label(e), map[string]any{"status": e.Status}, after)}
		if err := s.store.Apply(ctx, ch); err != nil {
			return EntryView{}, err
		}
	}
	got, err := s.reload(ctx, a, []uuid.UUID{e.ID})
	if err != nil {
		return EntryView{}, err
	}
	return got[0], nil
}

// AddNote serves POST .../entries/{id}/notes (append-only).
func (s *Service) AddNote(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, text string) (NoteView, error) {
	a, err := s.access(p, bookID)
	if err != nil {
		return NoteView{}, err
	}
	e, err := s.entry(ctx, a, id)
	if err != nil {
		return NoteView{}, err
	}
	if !a.mayAnnotate(e) {
		return NoteView{}, ErrForbidden
	}
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > MaxNote {
		return NoteView{}, &ValidationError{Fields: map[string]string{"text": fmt.Sprintf("is required (at most %d characters)", MaxNote)}}
	}
	author := p.UserID
	n := Note{ID: newID(), EntryID: e.ID, Text: text, AuthorUserID: &author, AuthorName: p.Name, CreatedAt: s.clock()}
	if err := s.store.Apply(ctx, Change{Notes: []Note{n}}); err != nil {
		return NoteView{}, err
	}
	return noteView(n), nil
}
