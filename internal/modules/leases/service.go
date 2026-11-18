package leases

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// ListingInfo is what a lease needs to know about a listing.
type ListingInfo struct {
	ID             uuid.UUID
	Reference      string
	Title          string
	MarketCode     string
	OrganisationID *uuid.UUID
	AgentUserID    *uuid.UUID
	SellerUserID   *uuid.UUID
	// Address is the listing's place as the public sees it (suburb, city,
	// region, country): listings have no street address.
	Address string
}

// Listings looks a listing up (whatever its status).
type Listings interface {
	LeaseListing(ctx context.Context, id uuid.UUID) (ListingInfo, error)
}

// Deps is what the service needs.
type Deps struct {
	Store Store
	Files storage.Storage
	// Agreements sends a lease for signing; nil: leases cannot be sent.
	Agreements Agreements
	// Listings: nil means a lease cannot name a listing.
	Listings Listings
	// CountryName returns a country's name ("" and false for an unknown code).
	CountryName func(ctx context.Context, code string) (string, bool, error)
	// Person returns an account's current name and email address.
	Person   func(ctx context.Context, id uuid.UUID) (name, email string, ok bool, err error)
	Logger   *slog.Logger
	Now      func() time.Time
	DemoCode string
}

// Service implements the lease use cases.
type Service struct {
	store      Store
	files      storage.Storage
	agreements Agreements
	listings   Listings
	country    func(ctx context.Context, code string) (string, bool, error)
	person     func(ctx context.Context, id uuid.UUID) (string, string, bool, error)
	logger     *slog.Logger
	now        func() time.Time
	demoCode   string
}

// NewService builds a Service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, files: d.Files, agreements: d.Agreements, listings: d.Listings, country: d.CountryName,
		person: d.Person, logger: d.Logger, now: d.Now, demoCode: d.DemoCode}
	if s.now == nil {
		s.now = time.Now
	}
	if s.country == nil {
		s.country = func(_ context.Context, code string) (string, bool, error) { return code, true, nil }
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

func (s *Service) audit(ctx context.Context, event string, attrs ...slog.Attr) {
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx))}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }

func userPtr(id uuid.UUID) *uuid.UUID { return &id }

// ---------------------------------------------------------------------------
// Who may
// ---------------------------------------------------------------------------

// TemplateAdminRoles manage the template library (SUPER_ADMIN always passes).
func TemplateAdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformContentAdmin}
}

// LeaseAdminRoles read every lease (read-only).
func LeaseAdminRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformSupport} }

// MayCreateLeases reports whether the caller may draft leases: whoever may
// manage listings (auth.ManagesListings, the dashboard's can_manage_listings),
// on a customer session. STAFF, seekers and pending professionals may not.
func MayCreateLeases(p *auth.Principal) bool {
	return p != nil && p.SessionScope == auth.ScopeCustomer && p.ManagesListings()
}

// mayCreateIn reports whether the caller may create leases for the organisation.
func mayCreateIn(p *auth.Principal, organisationID uuid.UUID) bool {
	return p.MayManageListings(organisationID)
}

// orgWide reports whether the caller sees and acts on every lease of the
// organisation: its OWNER and MANAGER (of a kind that manages listings).
func orgWide(p *auth.Principal, organisationID uuid.UUID) bool {
	return p.MayManageListings(organisationID) && p.MayAssignWork(organisationID)
}

// maySee reports whether the caller sees (and may act on) a lease: the
// organisation's OWNER and MANAGER every lease of it; its other members who
// may manage listings (AGENT) the leases they created; a direct owner the
// leases they created without an organisation.
func maySee(p *auth.Principal, l Lease) bool {
	if p == nil || p.SessionScope != auth.ScopeCustomer {
		return false
	}
	if l.OrganisationID != nil {
		return orgWide(p, *l.OrganisationID) || (l.CreatedBy == p.UserID && p.MayManageListings(*l.OrganisationID))
	}
	return l.CreatedBy == p.UserID && p.IsDirectOwner()
}

func scopeOf(p *auth.Principal) LeaseFilter {
	f := LeaseFilter{Creator: userPtr(p.UserID), CreatorDirect: p.IsDirectOwner()}
	for _, id := range p.ListingOrganisationIDs() {
		if orgWide(p, id) {
			f.Organisations = append(f.Organisations, id)
		} else {
			f.CreatorOrganisations = append(f.CreatorOrganisations, id)
		}
	}
	return f
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// TemplateView is a template as the API returns it.
type TemplateView struct {
	ID              uuid.UUID    `json:"id"`
	FamilyID        uuid.UUID    `json:"family_id"`
	Version         int          `json:"version"`
	OrganisationID  *uuid.UUID   `json:"organisation_id"`
	Source          string       `json:"source"`
	CanEdit         bool         `json:"can_edit"`
	MarketCode      string       `json:"market_code"`
	Name            string       `json:"name"`
	LeaseKind       string       `json:"lease_kind"`
	Description     *string      `json:"description"`
	Sections        []Section    `json:"sections"`
	OptionalClauses []Clause     `json:"optional_clauses"`
	Status          string       `json:"status"`
	LegalReview     *LegalReview `json:"legal_review"`
	CreatedByName   string       `json:"created_by_name"`
	ApprovedAt      *time.Time   `json:"approved_at"`
	ApprovedByName  *string      `json:"approved_by_name"`
	RetiredAt       *time.Time   `json:"retired_at"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// templateView is a template as the API returns it; canEdit says whether the
// caller may manage it.
func templateView(t Template, canEdit bool) TemplateView {
	v := TemplateView{
		ID: t.ID, FamilyID: t.FamilyID, Version: t.Version, OrganisationID: t.OrganisationID, Source: SourceSample, CanEdit: canEdit,
		MarketCode: t.MarketCode, Name: t.Name, LeaseKind: t.LeaseKind,
		Description: optional(t.Description), Sections: t.Sections, OptionalClauses: t.Clauses, Status: t.Status,
		LegalReview: t.LegalReview, CreatedByName: t.CreatedByName, ApprovedAt: t.ApprovedAt, ApprovedByName: optional(t.ApprovedByName),
		RetiredAt: t.RetiredAt, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
	if v.Sections == nil {
		v.Sections = []Section{}
	}
	if v.OptionalClauses == nil {
		v.OptionalClauses = []Clause{}
	}
	if t.OrganisationID != nil {
		v.Source = SourceOrganisation
	}
	return v
}

// TemplateEventView is one record of a template's audit trail.
type TemplateEventView struct {
	ID     uuid.UUID      `json:"id"`
	Action string         `json:"action"`
	Actor  ActorView      `json:"actor"`
	Detail map[string]any `json:"detail"`
	At     time.Time      `json:"at"`
}

// ActorView is who did something.
type ActorView struct {
	Type   string     `json:"type"`
	UserID *uuid.UUID `json:"user_id"`
	Name   string     `json:"name"`
}

// PartyView is a landlord or tenant of a lease.
type PartyView struct {
	Role  string  `json:"role"`
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Phone *string `json:"phone"`
}

// DocumentView describes a stored PDF (download it from .../pdf).
type DocumentView struct {
	Kind            string    `json:"kind"`
	SHA256          string    `json:"sha256"`
	SizeBytes       int64     `json:"size_bytes"`
	TemplateVersion int       `json:"template_version"`
	GeneratedAt     time.Time `json:"generated_at"`
}

// LeaseView is a lease as its author (and their organisation) sees it.
type LeaseView struct {
	ID        uuid.UUID `json:"id"`
	Reference string    `json:"reference"`
	Status    string    `json:"status"`
	Template  struct {
		ID         uuid.UUID `json:"id"`
		Name       string    `json:"name"`
		Version    int       `json:"version"`
		MarketCode string    `json:"market_code"`
		LeaseKind  string    `json:"lease_kind"`
	} `json:"template"`
	Organisation *struct {
		ID   uuid.UUID `json:"id"`
		Name string    `json:"name"`
	} `json:"organisation"`
	Listing *struct {
		ID        uuid.UUID `json:"id"`
		Reference string    `json:"reference"`
		Title     string    `json:"title"`
	} `json:"listing"`
	CreatedBy struct {
		UserID uuid.UUID `json:"user_id"`
		Name   string    `json:"name"`
	} `json:"created_by"`
	PropertyAddress   string        `json:"property_address"`
	Landlords         []PartyView   `json:"landlords"`
	Tenants           []PartyView   `json:"tenants"`
	StartDate         string        `json:"start_date"`
	EndDate           *string       `json:"end_date"`
	Periodic          bool          `json:"periodic"`
	RentAmount        string        `json:"rent_amount"`
	RentCurrency      string        `json:"rent_currency"`
	RentPeriod        string        `json:"rent_period"`
	PaymentDay        int           `json:"payment_day"`
	DepositAmount     *string       `json:"deposit_amount"`
	NoticePeriodDays  int           `json:"notice_period_days"`
	OptionalClauses   []string      `json:"optional_clauses"`
	SpecialConditions *string       `json:"special_conditions"`
	LandlordStamp     bool          `json:"landlord_stamp"`
	Document          *DocumentView `json:"document"`
	SigningCopy       *DocumentView `json:"signing_copy"`
	// AgreementID is the agreement the lease was sent for signing with
	// (GET /v1/me/agreements/{id} for its recipients and progress).
	AgreementID  *uuid.UUID `json:"agreement_id"`
	CanEdit      bool       `json:"can_edit"`
	CanSend      bool       `json:"can_send"`
	CanCancel    bool       `json:"can_cancel"`
	CanDelete    bool       `json:"can_delete"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
	SentAt       *time.Time `json:"sent_at"`
	CompletedAt  *time.Time `json:"completed_at"`
	DeclinedAt   *time.Time `json:"declined_at"`
	CancelledAt  *time.Time `json:"cancelled_at"`
	CancelReason *string    `json:"cancel_reason"`
}

func documentView(d Document, ok bool) *DocumentView {
	if !ok {
		return nil
	}
	return &DocumentView{Kind: d.Kind, SHA256: d.SHA256, SizeBytes: d.Size, TemplateVersion: d.TemplateVersion, GeneratedAt: d.GeneratedAt}
}

// currentDocument is the PDF that GET .../pdf returns for the lease's status.
func currentDocument(l Lease) (Document, bool) {
	order := []string{DocumentDraft}
	switch l.Status {
	case StatusSigned:
		order = []string{DocumentFinal, DocumentSigning}
	case StatusSentForSigning, StatusDeclined, StatusExpired:
		order = []string{DocumentSigning}
	case StatusCancelled:
		order = []string{DocumentSigning, DocumentDraft}
	}
	for _, kind := range order {
		if d, ok := l.Documents[kind]; ok {
			return d, true
		}
	}
	return Document{}, false
}

func leaseView(l Lease) LeaseView {
	v := LeaseView{ID: l.ID, Reference: l.Reference, Status: l.Status, PropertyAddress: l.Input.PropertyAddress,
		StartDate: l.Input.StartDate, Periodic: l.Input.Periodic, RentAmount: l.Input.RentAmount, RentCurrency: l.Input.RentCurrency,
		RentPeriod: l.Input.RentPeriod, PaymentDay: l.Input.PaymentDay, DepositAmount: optional(l.Input.DepositAmount),
		NoticePeriodDays: l.Input.NoticePeriodDays, OptionalClauses: append([]string{}, l.Input.OptionalClauses...),
		SpecialConditions: optional(l.Input.SpecialConditions), EndDate: optional(l.Input.EndDate), LandlordStamp: l.Input.LandlordStamp,
		CreatedAt: l.CreatedAt, UpdatedAt: l.UpdatedAt, SentAt: l.SentAt, CompletedAt: l.CompletedAt, DeclinedAt: l.DeclinedAt,
		CancelledAt: l.CancelledAt, CancelReason: optional(l.CancelReason), Landlords: []PartyView{}, Tenants: []PartyView{},
	}
	v.Template.ID, v.Template.Name, v.Template.Version, v.Template.MarketCode, v.Template.LeaseKind = l.TemplateID, l.TemplateName, l.TemplateVersion, l.MarketCode, l.LeaseKind
	if l.OrganisationID != nil {
		v.Organisation = &struct {
			ID   uuid.UUID `json:"id"`
			Name string    `json:"name"`
		}{*l.OrganisationID, l.OrganisationName}
	}
	if l.ListingID != nil {
		v.Listing = &struct {
			ID        uuid.UUID `json:"id"`
			Reference string    `json:"reference"`
			Title     string    `json:"title"`
		}{*l.ListingID, l.ListingReference, l.ListingTitle}
	}
	v.CreatedBy.UserID, v.CreatedBy.Name = l.CreatedBy, l.CreatedByName
	for _, p := range l.Input.Landlords {
		v.Landlords = append(v.Landlords, PartyView{Role: RoleLandlord, Name: p.Name, Email: p.Email, Phone: optional(p.Phone)})
	}
	for _, p := range l.Input.Tenants {
		v.Tenants = append(v.Tenants, PartyView{Role: RoleTenant, Name: p.Name, Email: p.Email, Phone: optional(p.Phone)})
	}
	v.AgreementID = l.AgreementID
	doc, ok := currentDocument(l)
	v.Document = documentView(doc, ok)
	sc, ok := l.Documents[DocumentSigning]
	v.SigningCopy = documentView(sc, ok)
	v.CanEdit = l.Status == StatusDraft
	v.CanSend = l.Status == StatusDraft
	v.CanDelete = l.Status == StatusDraft
	v.CanCancel = l.Status == StatusDraft || l.Status == StatusSentForSigning
	return v
}

// EventView is one record of a lease's audit trail.
type EventView struct {
	ID     uuid.UUID      `json:"id"`
	Action string         `json:"action"`
	Actor  ActorView      `json:"actor"`
	Detail map[string]any `json:"detail"`
	At     time.Time      `json:"at"`
}

// Page is one page of leases.
type Page struct {
	Items      []LeaseView `json:"items"`
	Page       int         `json:"page"`
	PageSize   int         `json:"page_size"`
	Total      int         `json:"total"`
	TotalPages int         `json:"total_pages"`
}

// ---------------------------------------------------------------------------
// Templates
// ---------------------------------------------------------------------------

func (s *Service) knownMarket(ctx context.Context) func(string) bool {
	return func(code string) bool {
		_, ok, err := s.country(ctx, code)
		return err == nil && ok
	}
}

func templateEvent(t Template, action string, p *auth.Principal, at time.Time, detail map[string]any) TemplateEvent {
	return TemplateEvent{ID: newID(), TemplateID: t.ID, Action: action, ActorUserID: userPtr(p.UserID), ActorName: p.Name, Detail: detail, At: at}
}

// sameOwner reports whether the template belongs to owner (nil: a sample).
func sameOwner(t Template, owner *uuid.UUID) bool {
	if owner == nil || t.OrganisationID == nil {
		return owner == nil && t.OrganisationID == nil
	}
	return *owner == *t.OrganisationID
}

// ownedTemplate loads a template of owner (nil: the sample library);
// ErrTemplateNotFound for any other template.
func (s *Service) ownedTemplate(ctx context.Context, id uuid.UUID, owner *uuid.UUID) (Template, error) {
	t, err := s.store.Template(ctx, id)
	if err != nil {
		return Template{}, err
	}
	if !sameOwner(t, owner) {
		return Template{}, ErrTemplateNotFound
	}
	return t, nil
}

func ownerAttr(owner *uuid.UUID) slog.Attr {
	if owner == nil {
		return slog.String("source", SourceSample)
	}
	return slog.String("organisation_id", owner.String())
}

// createTemplate stores t (filled in by the caller) as version 1 of a new
// family, as a DRAFT of owner.
func (s *Service) createTemplate(ctx context.Context, p *auth.Principal, t Template, owner *uuid.UUID, detail map[string]any) (Template, error) {
	now := s.clock()
	t.ID = newID()
	t.FamilyID, t.Version, t.Status, t.OrganisationID = t.ID, 1, TemplateDraft, owner
	t.LegalReview, t.ApprovedAt, t.ApprovedBy, t.ApprovedByName, t.RetiredAt = nil, nil, nil, "", nil
	t.CreatedBy, t.CreatedByName, t.CreatedAt, t.UpdatedAt = userPtr(p.UserID), p.Name, now, now
	if t.Clauses == nil {
		t.Clauses = []Clause{}
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["version"], detail["market_code"] = 1, t.MarketCode
	if err := s.store.CreateTemplate(ctx, t, templateEvent(t, TemplateEventCreated, p, now, detail)); err != nil {
		return Template{}, err
	}
	s.audit(ctx, "lease_template.created", slog.String("template_id", t.ID.String()), slog.String("actor_user_id", p.UserID.String()), ownerAttr(owner))
	return t, nil
}

// editTemplate changes a DRAFT of owner in place, or creates the family's
// next DRAFT version from an APPROVED one (created=true).
func (s *Service) editTemplate(ctx context.Context, p *auth.Principal, id uuid.UUID, in TemplateInput, owner *uuid.UUID) (Template, bool, error) {
	current, err := s.ownedTemplate(ctx, id, owner)
	if err != nil {
		return Template{}, false, err
	}
	now := s.clock()
	switch current.Status {
	case TemplateRetired:
		return Template{}, false, ErrTemplateNotEditable
	case TemplateApproved:
		next := current
		next.ID, next.Status, next.LegalReview = newID(), TemplateDraft, nil
		next.ApprovedAt, next.ApprovedBy, next.ApprovedByName, next.RetiredAt = nil, nil, "", nil
		next.CreatedBy, next.CreatedByName, next.CreatedAt, next.UpdatedAt = userPtr(p.UserID), p.Name, now, now
		next.Sections = append([]Section{}, current.Sections...)
		next.Clauses = append([]Clause{}, current.Clauses...)
		if err := applyTemplateInput(&next, in, false, s.knownMarket(ctx)); err != nil {
			return Template{}, false, err
		}
		created, err := s.store.CreateTemplateVersion(ctx, next, templateEvent(next, TemplateEventNewVersion, p, now, map[string]any{"from_template_id": current.ID.String(), "from_version": current.Version}))
		if err != nil {
			return Template{}, false, err
		}
		s.audit(ctx, "lease_template.new_version", slog.String("template_id", created.ID.String()), slog.Int("version", created.Version), ownerAttr(owner))
		return created, true, nil
	}
	t, err := s.store.ChangeTemplate(ctx, id, func(t Template) (Template, []TemplateEvent, error) {
		if t.Status != TemplateDraft {
			return Template{}, nil, ErrTemplateState
		}
		if err := applyTemplateInput(&t, in, false, s.knownMarket(ctx)); err != nil {
			return Template{}, nil, err
		}
		t.UpdatedAt = now
		return t, []TemplateEvent{templateEvent(t, TemplateEventEdited, p, now, nil)}, nil
	})
	if err != nil {
		return Template{}, false, err
	}
	return t, false, nil
}

// approveTemplate approves (publishes) a DRAFT of owner, with an optional
// legal review; the family's earlier APPROVED version is RETIRED at the same
// time (its leases keep it).
func (s *Service) approveTemplate(ctx context.Context, p *auth.Principal, id uuid.UUID, review *LegalReview, owner *uuid.UUID) (Template, error) {
	if _, err := s.ownedTemplate(ctx, id, owner); err != nil {
		return Template{}, err
	}
	now := s.clock()
	var clean *LegalReview
	if review != nil {
		c, err := checkLegalReview(*review, now)
		if err != nil {
			return Template{}, err
		}
		clean = &c
	}
	t, err := s.store.ApproveTemplate(ctx, id, clean, userPtr(p.UserID), p.Name, now, func(t Template, retired []Template) []TemplateEvent {
		detail := map[string]any{"version": t.Version}
		if clean != nil {
			detail["reviewer_name"], detail["organisation"], detail["reviewed_on"] = clean.ReviewerName, clean.Organisation, clean.ReviewedOn
		}
		events := []TemplateEvent{templateEvent(t, TemplateEventApproved, p, now, detail)}
		for _, r := range retired {
			events = append(events, templateEvent(r, TemplateEventRetired, p, now, map[string]any{"replaced_by_version": t.Version}))
		}
		return events
	})
	if err != nil {
		return Template{}, err
	}
	s.audit(ctx, "lease_template.approved", slog.String("template_id", id.String()), slog.Int("version", t.Version),
		slog.String("actor_user_id", p.UserID.String()), ownerAttr(owner))
	return t, nil
}

// retireTemplate retires an APPROVED template of owner (a DRAFT too when
// draftsToo): it is no longer offered or accepted for new leases; leases
// made from it keep it.
func (s *Service) retireTemplate(ctx context.Context, p *auth.Principal, id uuid.UUID, owner *uuid.UUID, draftsToo bool) (Template, error) {
	if _, err := s.ownedTemplate(ctx, id, owner); err != nil {
		return Template{}, err
	}
	now := s.clock()
	t, err := s.store.ChangeTemplate(ctx, id, func(t Template) (Template, []TemplateEvent, error) {
		if t.Status != TemplateApproved && !(draftsToo && t.Status == TemplateDraft) {
			return Template{}, nil, ErrTemplateState
		}
		t.Status, t.RetiredAt, t.UpdatedAt = TemplateRetired, &now, now
		return t, []TemplateEvent{templateEvent(t, TemplateEventRetired, p, now, nil)}, nil
	})
	if err != nil {
		return Template{}, err
	}
	s.audit(ctx, "lease_template.retired", slog.String("template_id", id.String()), slog.String("actor_user_id", p.UserID.String()), ownerAttr(owner))
	return t, nil
}

func (s *Service) templateEvents(ctx context.Context, id uuid.UUID) ([]TemplateEventView, error) {
	events, err := s.store.TemplateEvents(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]TemplateEventView, 0, len(events))
	for _, e := range events {
		detail := e.Detail
		if detail == nil {
			detail = map[string]any{}
		}
		out = append(out, TemplateEventView{ID: e.ID, Action: e.Action, Actor: ActorView{Type: ActorUser, UserID: e.ActorUserID, Name: e.ActorName}, Detail: detail, At: e.At})
	}
	return out, nil
}

func views(list []Template, canEdit func(Template) bool) []TemplateView {
	out := make([]TemplateView, 0, len(list))
	for _, t := range list {
		out = append(out, templateView(t, canEdit(t)))
	}
	return out
}

// ---------------------------------------------------------------------------
// Sample templates (content administrators)
// ---------------------------------------------------------------------------

func always(Template) bool { return true }

// CreateTemplate creates version 1 of a new sample template family, as a DRAFT.
func (s *Service) CreateTemplate(ctx context.Context, p *auth.Principal, in TemplateInput) (TemplateView, error) {
	t := Template{Clauses: []Clause{}}
	if err := applyTemplateInput(&t, in, true, s.knownMarket(ctx)); err != nil {
		return TemplateView{}, err
	}
	created, err := s.createTemplate(ctx, p, t, nil, nil)
	if err != nil {
		return TemplateView{}, err
	}
	return templateView(created, true), nil
}

// AdminTemplates lists the sample templates (every version), newest first.
func (s *Service) AdminTemplates(ctx context.Context, f TemplateFilter) ([]TemplateView, error) {
	f.Scoped, f.Samples, f.Organisations = true, true, nil
	list, err := s.store.Templates(ctx, f)
	if err != nil {
		return nil, err
	}
	return views(list, always), nil
}

// AdminTemplate returns one sample template with its audit trail.
func (s *Service) AdminTemplate(ctx context.Context, id uuid.UUID) (TemplateView, []TemplateEventView, error) {
	t, err := s.ownedTemplate(ctx, id, nil)
	if err != nil {
		return TemplateView{}, nil, err
	}
	events, err := s.templateEvents(ctx, id)
	if err != nil {
		return TemplateView{}, nil, err
	}
	return templateView(t, true), events, nil
}

// EditTemplate changes a sample DRAFT in place. Editing an APPROVED sample
// creates the family's next DRAFT version from it (created=true);
// ErrTemplateDraftExists when the family already has one. A RETIRED
// template cannot be edited.
func (s *Service) EditTemplate(ctx context.Context, p *auth.Principal, id uuid.UUID, in TemplateInput) (TemplateView, bool, error) {
	t, created, err := s.editTemplate(ctx, p, id, in, nil)
	if err != nil {
		return TemplateView{}, false, err
	}
	return templateView(t, true), created, nil
}

// ApproveTemplate approves a sample DRAFT; the legal review is optional.
func (s *Service) ApproveTemplate(ctx context.Context, p *auth.Principal, id uuid.UUID, review *LegalReview) (TemplateView, error) {
	t, err := s.approveTemplate(ctx, p, id, review, nil)
	if err != nil {
		return TemplateView{}, err
	}
	return templateView(t, true), nil
}

// RetireTemplate retires an APPROVED sample.
func (s *Service) RetireTemplate(ctx context.Context, p *auth.Principal, id uuid.UUID) (TemplateView, error) {
	t, err := s.retireTemplate(ctx, p, id, nil, false)
	if err != nil {
		return TemplateView{}, err
	}
	return templateView(t, true), nil
}

// ---------------------------------------------------------------------------
// Organisation templates (/v1/me/organisations/{orgId}/lease-templates)
// ---------------------------------------------------------------------------

// orgTemplates checks that the caller may read (manage=false) or manage
// the organisation's templates: ErrOrganisationNotFound for somebody who is
// not a member, ErrTemplateForbidden for a member without the right.
// Reading: any member who may create leases there (OWNER, MANAGER, AGENT of
// a kind that manages listings). Managing: its OWNER and MANAGER (orgWide).
func orgTemplates(p *auth.Principal, orgID uuid.UUID, manage bool) error {
	if p == nil || p.MembershipRole(orgID) == "" {
		return ErrOrganisationNotFound
	}
	if !MayCreateLeases(p) || !mayCreateIn(p, orgID) || (manage && !orgWide(p, orgID)) {
		return ErrTemplateForbidden
	}
	return nil
}

// MayManageTemplates reports whether the caller may create, edit, publish
// and retire the organisation's templates.
func MayManageTemplates(p *auth.Principal, orgID uuid.UUID) bool {
	return orgTemplates(p, orgID, true) == nil
}

// OrgTemplateInput is the body of POST /v1/me/organisations/{orgId}/lease-templates:
// a new template, or a copy of one the caller may use (copy_from_template_id).
type OrgTemplateInput struct {
	TemplateInput
	CopyFromTemplateID *string `json:"copy_from_template_id"`
}

// OrgTemplates lists the organisation's templates (every version).
func (s *Service) OrgTemplates(ctx context.Context, p *auth.Principal, orgID uuid.UUID, f TemplateFilter) ([]TemplateView, error) {
	if err := orgTemplates(p, orgID, false); err != nil {
		return nil, err
	}
	f.Scoped, f.Samples, f.Organisations = true, false, []uuid.UUID{orgID}
	list, err := s.store.Templates(ctx, f)
	if err != nil {
		return nil, err
	}
	manage := orgWide(p, orgID)
	return views(list, func(Template) bool { return manage }), nil
}

// OrgTemplate returns one of the organisation's templates.
func (s *Service) OrgTemplate(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID) (TemplateView, error) {
	if err := orgTemplates(p, orgID, false); err != nil {
		return TemplateView{}, err
	}
	t, err := s.ownedTemplate(ctx, id, &orgID)
	if err != nil {
		return TemplateView{}, err
	}
	return templateView(t, orgWide(p, orgID)), nil
}

// OrgTemplateEvents returns the audit trail of one of the organisation's templates.
func (s *Service) OrgTemplateEvents(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID) ([]TemplateEventView, error) {
	if err := orgTemplates(p, orgID, false); err != nil {
		return nil, err
	}
	if _, err := s.ownedTemplate(ctx, id, &orgID); err != nil {
		return nil, err
	}
	return s.templateEvents(ctx, id)
}

// CreateOrgTemplate creates a DRAFT (version 1 of a new family) of the
// organisation: from the body, or as a copy of a template the caller may use
// (an approved sample, or any of this organisation's), named "Copy of
// <name>" unless a name is sent.
func (s *Service) CreateOrgTemplate(ctx context.Context, p *auth.Principal, orgID uuid.UUID, in OrgTemplateInput) (TemplateView, error) {
	if err := orgTemplates(p, orgID, true); err != nil {
		return TemplateView{}, err
	}
	fields := map[string]string{}
	sourceID := parseID(fields, "copy_from_template_id", in.CopyFromTemplateID)
	if len(fields) > 0 {
		return TemplateView{}, &ValidationError{Fields: fields}
	}
	if sourceID == nil {
		t := Template{Clauses: []Clause{}}
		if err := applyTemplateInput(&t, in.TemplateInput, true, s.knownMarket(ctx)); err != nil {
			return TemplateView{}, err
		}
		created, err := s.createTemplate(ctx, p, t, &orgID, nil)
		if err != nil {
			return TemplateView{}, err
		}
		return templateView(created, true), nil
	}
	source, err := s.store.Template(ctx, *sourceID)
	if errors.Is(err, ErrTemplateNotFound) || (err == nil && !(source.IsSample() && source.Status == TemplateApproved) && !sameOwner(source, &orgID)) {
		return TemplateView{}, &ValidationError{Fields: map[string]string{"copy_from_template_id": "must be a published sample template or one of this organisation's templates"}}
	}
	if err != nil {
		return TemplateView{}, err
	}
	t := Template{MarketCode: source.MarketCode, Name: source.Name, LeaseKind: source.LeaseKind, Description: source.Description,
		Sections: append([]Section{}, source.Sections...), Clauses: append([]Clause{}, source.Clauses...)}
	// A sample's description is about the sample ("not reviewed by a
	// lawyer..."), not about the organisation's own template.
	if source.OrganisationID == nil {
		t.Description = ""
	}
	if in.Name == nil {
		name := "Copy of " + source.Name
		if r := []rune(name); len(r) > MaxTemplateName {
			name = string(r[:MaxTemplateName])
		}
		t.Name = name
	}
	if err := applyTemplateInput(&t, in.TemplateInput, false, s.knownMarket(ctx)); err != nil {
		return TemplateView{}, err
	}
	created, err := s.createTemplate(ctx, p, t, &orgID, map[string]any{"copied_from_template_id": source.ID.String(),
		"copied_from_version": source.Version, "copied_from_source": templateView(source, false).Source})
	if err != nil {
		return TemplateView{}, err
	}
	return templateView(created, true), nil
}

// EditOrgTemplate is EditTemplate for one of the organisation's templates.
func (s *Service) EditOrgTemplate(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID, in TemplateInput) (TemplateView, bool, error) {
	if err := orgTemplates(p, orgID, true); err != nil {
		return TemplateView{}, false, err
	}
	t, created, err := s.editTemplate(ctx, p, id, in, &orgID)
	if err != nil {
		return TemplateView{}, false, err
	}
	return templateView(t, true), created, nil
}

// PublishOrgTemplate makes one of the organisation's DRAFTs APPROVED
// (published) and retires the family's other APPROVED version. No review.
func (s *Service) PublishOrgTemplate(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID) (TemplateView, error) {
	if err := orgTemplates(p, orgID, true); err != nil {
		return TemplateView{}, err
	}
	t, err := s.approveTemplate(ctx, p, id, nil, &orgID)
	if err != nil {
		return TemplateView{}, err
	}
	return templateView(t, true), nil
}

// RetireOrgTemplate retires one of the organisation's APPROVED or DRAFT templates.
func (s *Service) RetireOrgTemplate(ctx context.Context, p *auth.Principal, orgID, id uuid.UUID) (TemplateView, error) {
	if err := orgTemplates(p, orgID, true); err != nil {
		return TemplateView{}, err
	}
	t, err := s.retireTemplate(ctx, p, id, &orgID, true)
	if err != nil {
		return TemplateView{}, err
	}
	return templateView(t, true), nil
}

// ---------------------------------------------------------------------------
// Templates on offer
// ---------------------------------------------------------------------------

// OfferedTemplates are the APPROVED templates the caller may make a lease
// from, of a country (every country when market is ""): every sample, and
// the published templates of the organisations the caller may create leases
// for (only orgID's when given).
func (s *Service) OfferedTemplates(ctx context.Context, p *auth.Principal, market string, orgID *uuid.UUID) ([]TemplateView, error) {
	if !MayCreateLeases(p) {
		return nil, ErrForbidden
	}
	orgs := p.ListingOrganisationIDs()
	if orgID != nil {
		if !mayCreateIn(p, *orgID) {
			return nil, &ValidationError{Fields: map[string]string{"organisation_id": "must be an organisation whose listings you manage"}}
		}
		orgs = []uuid.UUID{*orgID}
	}
	list, err := s.store.Templates(ctx, TemplateFilter{MarketCode: strings.ToUpper(market), Status: TemplateApproved, Scoped: true, Samples: true, Organisations: orgs})
	if err != nil {
		return nil, err
	}
	return views(list, func(t Template) bool { return t.OrganisationID != nil && orgWide(p, *t.OrganisationID) }), nil
}

// mayUse reports whether a lease of organisation (nil: no organisation) may
// use the template: a sample, or a template of that organisation.
func mayUse(t Template, organisation *uuid.UUID) bool {
	return t.IsSample() || (organisation != nil && *organisation == *t.OrganisationID)
}

func templateNotOfOrganisation() error {
	return &ValidationError{Fields: map[string]string{"template_id": "must be a sample template or a published template of the lease's organisation"}}
}

// ---------------------------------------------------------------------------
// Drafting leases
// ---------------------------------------------------------------------------

func parseID(fields map[string]string, key string, raw *string) *uuid.UUID {
	if raw == nil || strings.TrimSpace(*raw) == "" {
		return nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*raw))
	if err != nil {
		fields[key] = "must be an ID"
		return nil
	}
	return &id
}

// usableTemplate loads a template a new lease (or a changed one) may use.
func (s *Service) usableTemplate(ctx context.Context, id uuid.UUID) (Template, error) {
	t, err := s.store.Template(ctx, id)
	if errors.Is(err, ErrTemplateNotFound) {
		return Template{}, ErrTemplateUnavailable
	}
	if err != nil {
		return Template{}, err
	}
	if t.Status != TemplateApproved {
		return Template{}, ErrTemplateUnavailable
	}
	return t, nil
}

func snapshotTemplate(t Template) SnapshotTemplate {
	return SnapshotTemplate{ID: t.ID.String(), Version: t.Version, Name: t.Name, LeaseKind: t.LeaseKind, MarketCode: t.MarketCode,
		Sections: t.Sections, Clauses: t.Clauses}
}

func (s *Service) snapshot(ctx context.Context, l Lease, t Template, at time.Time) (Snapshot, error) {
	country, _, err := s.country(ctx, l.MarketCode)
	if err != nil {
		return Snapshot{}, err
	}
	if country == "" {
		country = l.MarketCode
	}
	return Snapshot{Reference: l.Reference, AgreementDate: at.Format(time.DateOnly), GeneratedAt: at, CountryName: country,
		OrganisationName: l.OrganisationName, PreparedBy: l.CreatedByName, Template: snapshotTemplate(t), Input: l.Input}, nil
}

// storeDocument renders snap in mode and stores it privately.
func (s *Service) storeDocument(ctx context.Context, snap Snapshot, mode, kind string) (Document, Layout, error) {
	content, layout, err := RenderPDF(snap, mode)
	if err != nil {
		return Document{}, Layout{}, err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return Document{}, Layout{}, err
	}
	key, err := storage.NewKey(ObjectKeyPrefix)
	if err != nil {
		return Document{}, Layout{}, err
	}
	if err := s.files.Put(ctx, key, bytes.NewReader(content), int64(len(content)), "application/pdf"); err != nil {
		return Document{}, Layout{}, fmt.Errorf("store lease document: %w", err)
	}
	tid, _ := uuid.Parse(snap.Template.ID)
	return Document{Kind: kind, TemplateID: tid, TemplateVersion: snap.Template.Version, Snapshot: raw, ObjectKey: key, SHA256: Checksum(content),
		Size: int64(len(content)), GeneratedAt: snap.GeneratedAt}, layout, nil
}

func (s *Service) dropObject(ctx context.Context, key string) {
	if key == "" {
		return
	}
	if err := s.files.Delete(context.WithoutCancel(ctx), key); err != nil {
		s.logger.WarnContext(ctx, "lease object not deleted", slog.String("object_key", key), slog.String("error", err.Error()))
	}
}

func userEvent(l Lease, action string, p *auth.Principal, at time.Time, detail map[string]any) Event {
	return Event{ID: newID(), LeaseID: l.ID, Action: action, ActorType: ActorUser, ActorUserID: userPtr(p.UserID), ActorName: p.Name, Detail: detail, At: at}
}

func docEvent(l Lease, d Document, actorType string, p *auth.Principal, at time.Time) Event {
	e := Event{ID: newID(), LeaseID: l.ID, Action: EventPDFGenerated, ActorType: actorType, Detail: map[string]any{
		"kind": d.Kind, "sha256": d.SHA256, "template_version": d.TemplateVersion, "size_bytes": d.Size}, At: at}
	if p != nil {
		e.ActorUserID, e.ActorName = userPtr(p.UserID), p.Name
	}
	return e
}

// Create drafts a lease from an APPROVED template, renders its draft PDF
// and returns it. listing_id (one the caller may edit) gives the
// organisation, the country (which must be the template's) and the
// address unless one is sent.
func (s *Service) Create(ctx context.Context, p *auth.Principal, in LeaseInput) (LeaseView, error) {
	if !MayCreateLeases(p) {
		return LeaseView{}, ErrForbidden
	}
	fields := map[string]string{}
	templateID := parseID(fields, "template_id", in.TemplateID)
	if templateID == nil && fields["template_id"] == "" {
		fields["template_id"] = "is required"
	}
	listingID := parseID(fields, "listing_id", in.ListingID)
	orgID := parseID(fields, "organisation_id", in.OrganisationID)
	if len(fields) > 0 {
		return LeaseView{}, &ValidationError{Fields: fields}
	}
	t, err := s.usableTemplate(ctx, *templateID)
	if err != nil {
		return LeaseView{}, err
	}
	now := s.clock()
	l := Lease{ID: newID(), Status: StatusDraft, TemplateID: t.ID, TemplateVersion: t.Version, TemplateName: t.Name,
		MarketCode: t.MarketCode, LeaseKind: t.LeaseKind, CreatedBy: p.UserID, CreatedByName: p.Name, CreatedAt: now, UpdatedAt: now,
		Documents: map[string]Document{}}
	input := in.merge(Input{})
	input.TemplateID = t.ID

	if listingID != nil {
		if s.listings == nil {
			return LeaseView{}, ErrListingNotFound
		}
		info, err := s.listings.LeaseListing(ctx, *listingID)
		if errors.Is(err, ErrListingNotFound) {
			return LeaseView{}, ErrListingNotFound
		}
		if err != nil {
			return LeaseView{}, err
		}
		if !p.MayEditListing(info.OrganisationID, info.AgentUserID, info.SellerUserID) {
			return LeaseView{}, ErrListingNotFound
		}
		if info.MarketCode != t.MarketCode {
			return LeaseView{}, ErrMarketMismatch
		}
		l.ListingID, l.ListingReference, l.ListingTitle, l.OrganisationID = &info.ID, info.Reference, info.Title, info.OrganisationID
		input.ListingID = &info.ID
		if strings.TrimSpace(input.PropertyAddress) == "" {
			input.PropertyAddress = info.Address
		}
	} else {
		switch {
		case orgID != nil:
			if !mayCreateIn(p, *orgID) {
				return LeaseView{}, &ValidationError{Fields: map[string]string{"organisation_id": "must be an organisation whose listings you manage"}}
			}
			l.OrganisationID = orgID
		default:
			orgs := p.ListingOrganisationIDs()
			switch {
			case len(orgs) == 1:
				l.OrganisationID = &orgs[0]
			case len(orgs) > 1:
				return LeaseView{}, &ValidationError{Fields: map[string]string{"organisation_id": "is required: you manage listings for more than one organisation"}}
			}
		}
	}
	if l.OrganisationID != nil {
		for _, m := range p.Memberships {
			if m.OrganisationID == *l.OrganisationID {
				l.OrganisationName = m.OrganisationName
			}
		}
		input.OrganisationID = l.OrganisationID
	}
	if !mayUse(t, l.OrganisationID) {
		return LeaseView{}, templateNotOfOrganisation()
	}
	clean, problems := checkInput(input, t)
	if len(problems) > 0 {
		return LeaseView{}, &ValidationError{Fields: problems}
	}
	l.Input = clean

	var ref string
	for range maxReferenceAttempt {
		if ref, err = s.store.NextReference(ctx, now.Year()); err != nil {
			return LeaseView{}, err
		}
		l.Reference = ref
		snap, err := s.snapshot(ctx, l, t, now)
		if err != nil {
			return LeaseView{}, err
		}
		doc, _, err := s.storeDocument(ctx, snap, ModeDraft, DocumentDraft)
		if err != nil {
			return LeaseView{}, err
		}
		l.Documents = map[string]Document{DocumentDraft: doc}
		err = s.store.CreateLease(ctx, l, []Event{
			userEvent(l, EventCreated, p, now, map[string]any{"template_id": t.ID.String(), "template_version": t.Version}),
			docEvent(l, doc, ActorUser, p, now),
		})
		if errors.Is(err, ErrReferenceTaken) {
			s.dropObject(ctx, doc.ObjectKey)
			continue
		}
		if err != nil {
			s.dropObject(ctx, doc.ObjectKey)
			return LeaseView{}, err
		}
		s.audit(ctx, "lease.created", slog.String("lease_id", l.ID.String()), slog.String("reference", l.Reference),
			slog.String("actor_user_id", p.UserID.String()))
		return leaseView(l), nil
	}
	return LeaseView{}, ErrReferenceTaken
}

// visible loads a lease the caller may see (ErrLeaseNotFound otherwise).
func (s *Service) visible(ctx context.Context, p *auth.Principal, id uuid.UUID) (Lease, error) {
	if !MayCreateLeases(p) {
		return Lease{}, ErrForbidden
	}
	l, err := s.store.Lease(ctx, id)
	if err != nil {
		return Lease{}, err
	}
	if !maySee(p, l) {
		return Lease{}, ErrLeaseNotFound
	}
	return l, nil
}

// Get returns one lease.
func (s *Service) Get(ctx context.Context, p *auth.Principal, id uuid.UUID) (LeaseView, error) {
	l, err := s.visible(ctx, p, id)
	if err != nil {
		return LeaseView{}, err
	}
	return leaseView(l), nil
}

// List returns one page of the leases the caller sees, newest first.
func (s *Service) List(ctx context.Context, p *auth.Principal, status string, page, size int) (Page, error) {
	if !MayCreateLeases(p) {
		return Page{}, ErrForbidden
	}
	f := scopeOf(p)
	f.Status, f.Limit, f.Offset = status, size, (page-1)*size
	return s.page(ctx, f, page, size)
}

// AdminList returns one page of every lease (SUPER_ADMIN, SUPPORT).
func (s *Service) AdminList(ctx context.Context, status string, page, size int) (Page, error) {
	return s.page(ctx, LeaseFilter{All: true, Status: status, Limit: size, Offset: (page - 1) * size}, page, size)
}

func (s *Service) page(ctx context.Context, f LeaseFilter, page, size int) (Page, error) {
	list, total, err := s.store.Leases(ctx, f)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: make([]LeaseView, 0, len(list)), Page: page, PageSize: size, Total: total}
	if total > 0 {
		out.TotalPages = (total + size - 1) / size
	}
	for _, l := range list {
		out.Items = append(out.Items, leaseView(l))
	}
	return out, nil
}

// Counts are the lease numbers of a dashboard.
type Counts struct {
	Drafts             int
	AwaitingSignatures int
}

// DashboardCounts counts the caller's leases.
func (s *Service) DashboardCounts(ctx context.Context, p *auth.Principal) (Counts, error) {
	var c Counts
	if !MayCreateLeases(p) {
		return c, nil
	}
	list, _, err := s.store.Leases(ctx, scopeOf(p))
	if err != nil {
		return Counts{}, err
	}
	for _, l := range list {
		switch l.Status {
		case StatusDraft:
			c.Drafts++
		case StatusSentForSigning:
			c.AwaitingSignatures++
		}
	}
	return c, nil
}

// Patch changes a DRAFT lease: the keys sent replace the saved values (the
// listing and the organisation cannot change). The draft PDF is rendered
// again; only the newest draft PDF is kept.
func (s *Service) Patch(ctx context.Context, p *auth.Principal, id uuid.UUID, in LeaseInput) (LeaseView, error) {
	l, err := s.visible(ctx, p, id)
	if err != nil {
		return LeaseView{}, err
	}
	if l.Status != StatusDraft {
		return LeaseView{}, ErrLeaseNotEditable
	}
	fields := map[string]string{}
	if in.ListingID != nil || in.OrganisationID != nil {
		fields["listing_id"] = "cannot change after the lease was created; delete the draft and start again"
	}
	templateID := parseID(fields, "template_id", in.TemplateID)
	if len(fields) > 0 {
		return LeaseView{}, &ValidationError{Fields: fields}
	}
	var t Template
	if templateID != nil && *templateID != l.TemplateID {
		if t, err = s.usableTemplate(ctx, *templateID); err != nil {
			return LeaseView{}, err
		}
		if !mayUse(t, l.OrganisationID) {
			return LeaseView{}, templateNotOfOrganisation()
		}
		if t.MarketCode != l.MarketCode {
			return LeaseView{}, ErrMarketMismatch
		}
	} else if t, err = s.store.Template(ctx, l.TemplateID); err != nil {
		return LeaseView{}, err
	}
	input := in.merge(l.Input)
	input.TemplateID = t.ID
	clean, problems := checkInput(input, t)
	if len(problems) > 0 {
		return LeaseView{}, &ValidationError{Fields: problems}
	}
	now := s.clock()
	next := l
	next.Input, next.TemplateID, next.TemplateVersion, next.TemplateName, next.LeaseKind = clean, t.ID, t.Version, t.Name, t.LeaseKind
	snap, err := s.snapshot(ctx, next, t, now)
	if err != nil {
		return LeaseView{}, err
	}
	doc, _, err := s.storeDocument(ctx, snap, ModeDraft, DocumentDraft)
	if err != nil {
		return LeaseView{}, err
	}
	var old string
	saved, err := s.store.ChangeLease(ctx, id, func(cur *Lease) ([]Event, error) {
		if cur.Status != StatusDraft {
			return nil, ErrLeaseNotEditable
		}
		if !cur.UpdatedAt.Equal(l.UpdatedAt) {
			return nil, ErrLeaseState
		}
		old = cur.Documents[DocumentDraft].ObjectKey
		cur.Input, cur.TemplateID, cur.TemplateVersion, cur.TemplateName, cur.LeaseKind = next.Input, next.TemplateID, next.TemplateVersion, next.TemplateName, next.LeaseKind
		cur.Documents[DocumentDraft] = doc
		cur.UpdatedAt = now
		return []Event{userEvent(*cur, EventEdited, p, now, map[string]any{"fields": changedKeys(in)}), docEvent(*cur, doc, ActorUser, p, now)}, nil
	})
	if err != nil {
		s.dropObject(ctx, doc.ObjectKey)
		return LeaseView{}, err
	}
	if old != doc.ObjectKey {
		s.dropObject(ctx, old)
	}
	return leaseView(saved), nil
}

func changedKeys(in LeaseInput) []string {
	var keys []string
	add := func(set bool, key string) {
		if set {
			keys = append(keys, key)
		}
	}
	add(in.TemplateID != nil, "template_id")
	add(in.PropertyAddress != nil, "property_address")
	add(in.Landlords != nil, "landlords")
	add(in.Tenants != nil, "tenants")
	add(in.StartDate != nil, "start_date")
	add(in.EndDate != nil, "end_date")
	add(in.Periodic != nil, "periodic")
	add(in.RentAmount != nil, "rent_amount")
	add(in.RentCurrency != nil, "rent_currency")
	add(in.RentPeriod != nil, "rent_period")
	add(in.PaymentDay != nil, "payment_day")
	add(in.DepositAmount != nil, "deposit_amount")
	add(in.NoticePeriodDays != nil, "notice_period_days")
	add(in.OptionalClauses != nil, "optional_clauses")
	add(in.SpecialConditions != nil, "special_conditions")
	add(in.LandlordStamp != nil, "landlord_stamp")
	if keys == nil {
		keys = []string{}
	}
	return keys
}

// Delete removes a DRAFT lease, its audit trail and its draft PDF.
func (s *Service) Delete(ctx context.Context, p *auth.Principal, id uuid.UUID) error {
	if _, err := s.visible(ctx, p, id); err != nil {
		return err
	}
	l, err := s.store.DeleteLease(ctx, id)
	if err != nil {
		return err
	}
	for _, d := range l.Documents {
		s.dropObject(ctx, d.ObjectKey)
	}
	s.audit(ctx, "lease.deleted", slog.String("lease_id", id.String()), slog.String("actor_user_id", p.UserID.String()))
	return nil
}

// File is a PDF to stream.
type File struct {
	Content  io.ReadCloser
	Size     int64
	SHA256   string
	FileName string
}

func (s *Service) open(ctx context.Context, l Lease, d Document) (File, error) {
	rc, err := s.files.Open(ctx, d.ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return File{}, ErrDocumentUnavailable
	}
	if err != nil {
		return File{}, err
	}
	name := l.Reference
	switch d.Kind {
	case DocumentDraft:
		name += "-draft"
	case DocumentFinal:
		name += "-signed"
	}
	return File{Content: rc, Size: d.Size, SHA256: d.SHA256, FileName: name + ".pdf"}, nil
}

// PDF opens the lease's current PDF: the draft, the signing copy, or the
// signed document once everybody signed.
func (s *Service) PDF(ctx context.Context, p *auth.Principal, id uuid.UUID) (File, error) {
	l, err := s.visible(ctx, p, id)
	if err != nil {
		return File{}, err
	}
	d, ok := currentDocument(l)
	if !ok {
		return File{}, ErrDocumentUnavailable
	}
	return s.open(ctx, l, d)
}

// Events returns the lease's audit trail, oldest first.
func (s *Service) Events(ctx context.Context, p *auth.Principal, id uuid.UUID) ([]EventView, error) {
	l, err := s.visible(ctx, p, id)
	if err != nil {
		return nil, err
	}
	events, err := s.store.Events(ctx, id)
	if err != nil {
		return nil, err
	}
	// The signing steps (emailed, viewed, code sent, signed, declined,
	// completed) are the agreement's audit trail.
	if l.AgreementID != nil && s.agreements != nil {
		more, err := s.agreements.Events(ctx, *l.AgreementID)
		if err != nil {
			return nil, err
		}
		events = append(events, more...)
		sort.SliceStable(events, func(i, j int) bool { return events[i].At.Before(events[j].At) })
	}
	out := make([]EventView, 0, len(events))
	for _, e := range events {
		detail := e.Detail
		if detail == nil {
			detail = map[string]any{}
		}
		out = append(out, EventView{ID: e.ID, Action: e.Action, Actor: ActorView{Type: e.ActorType, UserID: e.ActorUserID, Name: e.ActorName},
			Detail: detail, At: e.At})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Sending for signing (through an agreement) and following it
// ---------------------------------------------------------------------------

// AgreementParty is a landlord or tenant who signs.
type AgreementParty struct {
	Role     string
	Position int
	Name     string
	Email    string
}

// LeaseAgreement is what the agreements module needs to send a lease.
type LeaseAgreement struct {
	LeaseID           uuid.UUID
	Reference         string
	Name              string
	Message           string
	ExpiresAt         *time.Time
	ReminderFrequency string
	OrganisationID    *uuid.UUID
	OrganisationName  string
	Document          Document
	Layout            Layout
	Parties           []AgreementParty
}

// Agreements is the signing workflow, as the lease module uses it.
type Agreements interface {
	// SendLease creates the lease's agreement, places the parties' fields
	// and sends it; it returns the agreement's ID.
	SendLease(ctx context.Context, p *auth.Principal, a LeaseAgreement) (uuid.UUID, error)
	// CancelLease cancels the lease's agreement (which then calls Follow).
	CancelLease(ctx context.Context, p *auth.Principal, agreementID uuid.UUID, reason string) error
	// Events returns the agreement's audit trail as lease events.
	Events(ctx context.Context, agreementID uuid.UUID) ([]Event, error)
}

// SendInput is the optional body of POST /v1/me/leases/{id}/send.
type SendInput struct {
	Message           string     `json:"message"`
	ExpiresAt         *time.Time `json:"expires_at"`
	ReminderFrequency string     `json:"reminder_frequency"`
}

// Send freezes a DRAFT: the signing copy is rendered (no watermark, with
// the initials and signature places) and stored with its SHA-256, and an
// agreement with every landlord and tenant as a signer (in any order) is
// created from it and sent. The lease is then SENT_FOR_SIGNING and follows
// its agreement. A lease whose template was retired since it was drafted
// can still be sent: it keeps its template version.
func (s *Service) Send(ctx context.Context, p *auth.Principal, id uuid.UUID, in SendInput) (LeaseView, error) {
	l, err := s.visible(ctx, p, id)
	if err != nil {
		return LeaseView{}, err
	}
	if l.Status != StatusDraft || s.agreements == nil {
		return LeaseView{}, ErrLeaseState
	}
	t, err := s.store.Template(ctx, l.TemplateID)
	if err != nil {
		return LeaseView{}, err
	}
	now := s.clock()
	snap, err := s.snapshot(ctx, l, t, now)
	if err != nil {
		return LeaseView{}, err
	}
	doc, layout, err := s.storeDocument(ctx, snap, ModeSigning, DocumentSigning)
	if err != nil {
		return LeaseView{}, err
	}
	name := "Lease " + l.Reference + ": " + l.Input.PropertyAddress
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	a := LeaseAgreement{LeaseID: l.ID, Reference: l.Reference, Name: name, Message: in.Message, ExpiresAt: in.ExpiresAt,
		ReminderFrequency: in.ReminderFrequency, OrganisationID: l.OrganisationID, OrganisationName: l.OrganisationName,
		Document: doc, Layout: layout}
	for i, pt := range l.Input.Landlords {
		a.Parties = append(a.Parties, AgreementParty{Role: RoleLandlord, Position: i + 1, Name: pt.Name, Email: pt.Email})
	}
	for i, pt := range l.Input.Tenants {
		a.Parties = append(a.Parties, AgreementParty{Role: RoleTenant, Position: i + 1, Name: pt.Name, Email: pt.Email})
	}
	agreementID, err := s.agreements.SendLease(ctx, p, a)
	if err != nil {
		s.dropObject(ctx, doc.ObjectKey)
		return LeaseView{}, err
	}
	saved, err := s.store.ChangeLease(ctx, id, func(cur *Lease) ([]Event, error) {
		if cur.Status != StatusDraft || !cur.UpdatedAt.Equal(l.UpdatedAt) {
			return nil, ErrLeaseState
		}
		cur.Status, cur.SentAt, cur.SentBy, cur.UpdatedAt, cur.AgreementID = StatusSentForSigning, &now, userPtr(p.UserID), now, &agreementID
		cur.Documents[DocumentSigning] = doc
		return []Event{docEvent(*cur, doc, ActorUser, p, now),
			userEvent(*cur, EventSent, p, now, map[string]any{"document_sha256": doc.SHA256, "agreement_id": agreementID.String()})}, nil
	})
	if err != nil {
		_ = s.agreements.CancelLease(context.WithoutCancel(ctx), p, agreementID, "The lease changed while it was being sent.")
		return LeaseView{}, err
	}
	s.audit(ctx, "lease.sent", slog.String("lease_id", id.String()), slog.String("agreement_id", agreementID.String()),
		slog.String("document_sha256", doc.SHA256), slog.String("actor_user_id", p.UserID.String()))
	return leaseView(saved), nil
}

// Cancel cancels a DRAFT, or a lease waiting for signatures (its agreement
// is cancelled: every signing link stops working).
func (s *Service) Cancel(ctx context.Context, p *auth.Principal, id uuid.UUID, reason string) (LeaseView, error) {
	l, err := s.visible(ctx, p, id)
	if err != nil {
		return LeaseView{}, err
	}
	clean, problem := cleanText(reason, MaxReasonRunes, true)
	if problem != "" {
		return LeaseView{}, &ValidationError{Fields: map[string]string{"reason": problem}}
	}
	if l.Status == StatusSentForSigning && l.AgreementID != nil && s.agreements != nil {
		if err := s.agreements.CancelLease(ctx, p, *l.AgreementID, clean); err != nil {
			return LeaseView{}, err
		}
		if l, err = s.store.Lease(ctx, id); err != nil {
			return LeaseView{}, err
		}
		return leaseView(l), nil
	}
	now := s.clock()
	saved, err := s.store.ChangeLease(ctx, id, func(cur *Lease) ([]Event, error) {
		if cur.Status != StatusDraft {
			return nil, ErrLeaseState
		}
		cur.Status, cur.CancelledAt, cur.CancelledBy, cur.CancelReason, cur.UpdatedAt = StatusCancelled, &now, userPtr(p.UserID), clean, now
		return []Event{userEvent(*cur, EventCancelled, p, now, map[string]any{"reason": clean})}, nil
	})
	if err != nil {
		return LeaseView{}, err
	}
	s.audit(ctx, "lease.cancelled", slog.String("lease_id", id.String()), slog.String("actor_user_id", p.UserID.String()))
	return leaseView(saved), nil
}

// Agreement statuses the lease follows (the agreements module's).
const (
	AgreementCompleted = "COMPLETED"
	AgreementDeclined  = "DECLINED"
	AgreementCancelled = "CANCELLED"
	AgreementExpired   = "EXPIRED"
)

// Follow brings a lease to its agreement's end: COMPLETED -> SIGNED (final
// is the agreement's signed PDF), DECLINED, CANCELLED, EXPIRED. Called by
// the agreements module; a lease no longer SENT_FOR_SIGNING is left alone.
func (s *Service) Follow(ctx context.Context, leaseID uuid.UUID, agreementStatus string, final *Document, at time.Time, reason string) error {
	status := map[string]string{AgreementCompleted: StatusSigned, AgreementDeclined: StatusDeclined,
		AgreementCancelled: StatusCancelled, AgreementExpired: StatusExpired}[agreementStatus]
	action := map[string]string{AgreementCompleted: EventSigned, AgreementDeclined: EventDeclined,
		AgreementCancelled: EventCancelled, AgreementExpired: EventExpired}[agreementStatus]
	if status == "" {
		return nil
	}
	at = at.UTC()
	_, err := s.store.ChangeLease(ctx, leaseID, func(cur *Lease) ([]Event, error) {
		if cur.Status != StatusSentForSigning {
			return nil, nil
		}
		cur.Status, cur.UpdatedAt = status, at
		detail := map[string]any{}
		switch status {
		case StatusSigned:
			cur.CompletedAt = &at
			if final != nil {
				d := *final
				d.Kind, d.TemplateID, d.TemplateVersion = DocumentFinal, cur.TemplateID, cur.TemplateVersion
				d.Snapshot = cur.Documents[DocumentSigning].Snapshot
				if len(d.Snapshot) == 0 {
					d.Snapshot = []byte(`{}`)
				}
				cur.Documents[DocumentFinal] = d
				detail["signed_document_sha256"] = d.SHA256
			}
		case StatusDeclined:
			cur.DeclinedAt = &at
			detail["reason"] = reason
		case StatusCancelled:
			cur.CancelledAt, cur.CancelReason = &at, reason
			detail["reason"] = reason
		}
		return []Event{{ID: newID(), LeaseID: cur.ID, Action: action, ActorType: ActorSystem, ActorName: "RightProperty365", Detail: detail, At: at}}, nil
	})
	return err
}
