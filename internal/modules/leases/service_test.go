package leases

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/storage"
)

type templateHarness struct {
	svc                          *Service
	store                        *MemoryStore
	org, otherOrg                uuid.UUID
	owner, manager, agent, staff *auth.Principal
	outsider, admin              *auth.Principal
	sample                       Template
}

func member(org uuid.UUID, role string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Name: role + " person", Email: strings.ToLower(role) + "@example.test", SessionScope: auth.ScopeCustomer,
		Memberships: []auth.Membership{{OrganisationID: org, OrganisationName: "Org " + role, OrganisationKind: auth.KindAgency, Role: role}}}
}

func newTemplateHarness(t *testing.T) *templateHarness {
	t.Helper()
	files, err := storage.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	h := &templateHarness{store: NewMemoryStore(), org: uuid.New(), otherOrg: uuid.New()}
	h.svc = NewService(Deps{Store: h.store, Files: files, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }})
	h.owner, h.manager = member(h.org, auth.RoleOwner), member(h.org, auth.RoleManager)
	h.agent, h.staff = member(h.org, auth.RoleAgent), member(h.org, auth.RoleStaff)
	h.outsider = member(h.otherOrg, auth.RoleOwner)
	h.admin = &auth.Principal{UserID: uuid.New(), Name: "Content Admin"}
	id := uuid.New()
	h.sample = Template{ID: id, FamilyID: id, Version: 1, MarketCode: "ZW", Name: "Sample lease", LeaseKind: KindResidential,
		Sections: []Section{{Heading: "Rent", Text: "Rent {{rent_amount}}."}}, Clauses: []Clause{}, Status: TemplateApproved,
		ApprovedAt: &now, CreatedAt: now, UpdatedAt: now}
	h.store.PutTemplate(h.sample)
	return h
}

func orgInput() OrgTemplateInput {
	market, name, kind := "ZW", "Our lease", KindResidential
	sections := []Section{{Heading: "Rent", Text: "Rent {{rent_currency}} {{rent_amount}}."}}
	clauses := []Clause{{Code: "NO_PETS", Title: "No pets", Text: "No pets."}}
	return OrgTemplateInput{TemplateInput: TemplateInput{MarketCode: &market, Name: &name, LeaseKind: &kind, Sections: &sections, Clauses: &clauses}}
}

func leaseFor(templateID uuid.UUID) LeaseInput {
	id, address := templateID.String(), "7 Example Lane, Harare"
	landlords := []Party{{Name: "Lloyd Landlord", Email: "landlord@example.test"}}
	tenants := []Party{{Name: "Tariro Tenant", Email: "tenant@example.test"}}
	start, end, rent, currency, period := "2026-12-01", "2027-11-30", "950.00", "USD", PeriodMonthly
	day, notice := 1, 30
	return LeaseInput{TemplateID: &id, PropertyAddress: &address, Landlords: &landlords, Tenants: &tenants, StartDate: &start, EndDate: &end,
		RentAmount: &rent, RentCurrency: &currency, RentPeriod: &period, PaymentDay: &day, NoticePeriodDays: &notice}
}

// Who may manage and read an organisation's templates.
func TestOrganisationTemplatePermissions(t *testing.T) {
	h := newTemplateHarness(t)
	ctx := context.Background()
	if _, err := h.svc.CreateOrgTemplate(ctx, h.agent, h.org, orgInput()); !errors.Is(err, ErrTemplateForbidden) {
		t.Fatalf("an agent may not create: %v", err)
	}
	if _, err := h.svc.CreateOrgTemplate(ctx, h.outsider, h.org, orgInput()); !errors.Is(err, ErrOrganisationNotFound) {
		t.Fatalf("another organisation's owner gets 404: %v", err)
	}
	created, err := h.svc.CreateOrgTemplate(ctx, h.manager, h.org, orgInput())
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != TemplateDraft || created.Source != SourceOrganisation || created.OrganisationID == nil || *created.OrganisationID != h.org ||
		len(created.OptionalClauses) != 1 || !created.CanEdit {
		t.Fatalf("created %+v", created)
	}
	// A plain member reads only.
	got, err := h.svc.OrgTemplate(ctx, h.agent, h.org, created.ID)
	if err != nil || got.CanEdit {
		t.Fatalf("an agent reads without editing: %+v %v", got, err)
	}
	name := "Changed"
	if _, _, err := h.svc.EditOrgTemplate(ctx, h.agent, h.org, created.ID, TemplateInput{Name: &name}); !errors.Is(err, ErrTemplateForbidden) {
		t.Fatalf("an agent may not edit: %v", err)
	}
	if _, err := h.svc.PublishOrgTemplate(ctx, h.agent, h.org, created.ID); !errors.Is(err, ErrTemplateForbidden) {
		t.Fatalf("an agent may not publish: %v", err)
	}
	if _, err := h.svc.OrgTemplates(ctx, h.staff, h.org, TemplateFilter{}); !errors.Is(err, ErrTemplateForbidden) {
		t.Fatalf("staff may not read: %v", err)
	}
	if _, err := h.svc.OrgTemplate(ctx, h.outsider, h.org, created.ID); !errors.Is(err, ErrOrganisationNotFound) {
		t.Fatalf("another organisation: %v", err)
	}
	// The organisation's routes never reach a sample or another
	// organisation's template, nor the admin routes an organisation's.
	if _, err := h.svc.OrgTemplate(ctx, h.owner, h.org, h.sample.ID); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("a sample through the organisation: %v", err)
	}
	if _, _, err := h.svc.AdminTemplate(ctx, created.ID); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("an organisation template through the sample library: %v", err)
	}
	if _, err := h.svc.ApproveTemplate(ctx, h.admin, created.ID, nil); !errors.Is(err, ErrTemplateNotFound) {
		t.Fatalf("administrators do not approve organisation templates: %v", err)
	}
	// Publishing needs no review, and retires the family's earlier version.
	published, err := h.svc.PublishOrgTemplate(ctx, h.owner, h.org, created.ID)
	if err != nil || published.Status != TemplateApproved || published.LegalReview != nil {
		t.Fatalf("published %+v %v", published, err)
	}
	next, isNew, err := h.svc.EditOrgTemplate(ctx, h.owner, h.org, created.ID, TemplateInput{Name: &name})
	if err != nil || !isNew || next.Version != 2 {
		t.Fatalf("new version %+v %v %v", next, isNew, err)
	}
	if _, err := h.svc.PublishOrgTemplate(ctx, h.manager, h.org, next.ID); err != nil {
		t.Fatal(err)
	}
	if old, _ := h.store.Template(ctx, created.ID); old.Status != TemplateRetired {
		t.Fatalf("publishing v2 retires v1, got %s", old.Status)
	}
	// A draft can be retired.
	draft, err := h.svc.CreateOrgTemplate(ctx, h.owner, h.org, orgInput())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := h.svc.RetireOrgTemplate(ctx, h.owner, h.org, draft.ID); err != nil || v.Status != TemplateRetired {
		t.Fatalf("retire a draft: %+v %v", v, err)
	}
}

// A copy of a sample (or of the organisation's own template); not of
// another organisation's.
func TestOrganisationTemplateCopies(t *testing.T) {
	h := newTemplateHarness(t)
	ctx := context.Background()
	from := h.sample.ID.String()
	cp, err := h.svc.CreateOrgTemplate(ctx, h.owner, h.org, OrgTemplateInput{CopyFromTemplateID: &from})
	if err != nil {
		t.Fatal(err)
	}
	if cp.Name != "Copy of Sample lease" || cp.Status != TemplateDraft || cp.FamilyID == h.sample.FamilyID || cp.MarketCode != "ZW" ||
		len(cp.Sections) != 1 || cp.Source != SourceOrganisation {
		t.Fatalf("copy %+v", cp)
	}
	named := "Our own"
	if cp, err := h.svc.CreateOrgTemplate(ctx, h.owner, h.org, OrgTemplateInput{CopyFromTemplateID: &from, TemplateInput: TemplateInput{Name: &named}}); err != nil || cp.Name != named {
		t.Fatalf("named copy %+v %v", cp, err)
	}
	theirs, err := h.svc.CreateOrgTemplate(ctx, h.outsider, h.otherOrg, orgInput())
	if err != nil {
		t.Fatal(err)
	}
	other := theirs.ID.String()
	var verr *ValidationError
	if _, err := h.svc.CreateOrgTemplate(ctx, h.owner, h.org, OrgTemplateInput{CopyFromTemplateID: &other}); !errors.As(err, &verr) || verr.Fields["copy_from_template_id"] == "" {
		t.Fatalf("another organisation's template cannot be copied: %v", err)
	}
}

// Offered templates and lease creation: samples and the lease's own
// organisation's published templates only.
func TestLeaseTemplateMustBeSampleOrOwn(t *testing.T) {
	h := newTemplateHarness(t)
	ctx := context.Background()
	ours, err := h.svc.CreateOrgTemplate(ctx, h.owner, h.org, orgInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.PublishOrgTemplate(ctx, h.owner, h.org, ours.ID); err != nil {
		t.Fatal(err)
	}
	theirs, err := h.svc.CreateOrgTemplate(ctx, h.outsider, h.otherOrg, orgInput())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.PublishOrgTemplate(ctx, h.outsider, h.otherOrg, theirs.ID); err != nil {
		t.Fatal(err)
	}
	offered, err := h.svc.OfferedTemplates(ctx, h.agent, "zw", nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[uuid.UUID]bool{}
	for _, v := range offered {
		ids[v.ID] = true
	}
	if !ids[h.sample.ID] || !ids[ours.ID] || ids[theirs.ID] || len(offered) != 2 {
		t.Fatalf("offered %v", ids)
	}
	var verr *ValidationError
	if _, err := h.svc.OfferedTemplates(ctx, h.agent, "ZW", &h.otherOrg); !errors.As(err, &verr) {
		t.Fatalf("another organisation's filter: %v", err)
	}

	if _, err := h.svc.Create(ctx, h.agent, leaseFor(h.sample.ID)); err != nil {
		t.Fatalf("a sample: %v", err)
	}
	if _, err := h.svc.Create(ctx, h.agent, leaseFor(ours.ID)); err != nil {
		t.Fatalf("the organisation's own template: %v", err)
	}
	_, err = h.svc.Create(ctx, h.agent, leaseFor(theirs.ID))
	if !errors.As(err, &verr) || verr.Fields["template_id"] == "" {
		t.Fatalf("another organisation's template: %v", err)
	}
	// Changing a draft lease to another organisation's template is refused too.
	l, err := h.svc.Create(ctx, h.agent, leaseFor(h.sample.ID))
	if err != nil {
		t.Fatal(err)
	}
	other := theirs.ID.String()
	if _, err := h.svc.Patch(ctx, h.agent, l.ID, LeaseInput{TemplateID: &other}); !errors.As(err, &verr) || verr.Fields["template_id"] == "" {
		t.Fatalf("patch to another organisation's template: %v", err)
	}
}

// A sample may be approved with or without a legal review.
func TestSampleApprovalReviewOptional(t *testing.T) {
	h := newTemplateHarness(t)
	ctx := context.Background()
	in := orgInput().TemplateInput
	draft, err := h.svc.CreateTemplate(ctx, h.admin, in)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Source != SourceSample || draft.OrganisationID != nil {
		t.Fatalf("a sample %+v", draft)
	}
	approved, err := h.svc.ApproveTemplate(ctx, h.admin, draft.ID, nil)
	if err != nil || approved.Status != TemplateApproved || approved.LegalReview != nil {
		t.Fatalf("approved without a review %+v %v", approved, err)
	}
	draft, _ = h.svc.CreateTemplate(ctx, h.admin, in)
	if _, err := h.svc.ApproveTemplate(ctx, h.admin, draft.ID, &LegalReview{ReviewerName: "A", Organisation: "B", ReviewedOn: "2099-01-01"}); err == nil {
		t.Fatal("a review that is sent is checked")
	}
}
