package applications

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/saved"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/storage"
)

// fakeListings is the catalogue of the tests.
type fakeListings struct{ m map[uuid.UUID]ListingInfo }

func (f fakeListings) Listings(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]ListingInfo, error) {
	out := map[uuid.UUID]ListingInfo{}
	for _, id := range ids {
		if l, ok := f.m[id]; ok {
			out[id] = l
		}
	}
	return out, nil
}

func (f fakeListings) BySlug(_ context.Context, slug string) (ListingInfo, error) {
	for _, l := range f.m {
		if l.Slug == slug && l.Public {
			return l, nil
		}
	}
	return ListingInfo{}, ErrListingNotFound
}

func (f fakeListings) AgentListingIDs(_ context.Context, org, agent uuid.UUID) ([]uuid.UUID, error) {
	var out []uuid.UUID
	for id, l := range f.m {
		if l.OrganisationID != nil && *l.OrganisationID == org && l.AgentUserID != nil && *l.AgentUserID == agent {
			out = append(out, id)
		}
	}
	return out, nil
}

type world struct {
	t                                    *testing.T
	svc                                  *Service
	store                                *MemoryStore
	files                                *storage.Local
	audit                                *orgaudit.MemoryLog
	now                                  time.Time
	org, other                           uuid.UUID
	owner, manager, agent, agent2, staff *auth.Principal
	otherOwner, landlord                 *auth.Principal
	tenant, tenant2, admin               *auth.Principal
	listing, listing2, shortStay, ownL   uuid.UUID
	cat                                  fakeListings
}

func member(org uuid.UUID, role, name string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Name: name, SessionScope: auth.ScopeCustomer,
		Memberships: []auth.Membership{{OrganisationID: org, OrganisationKind: auth.KindAgency, Role: role, MarketCode: "ZW"}}}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, store: NewMemoryStore(), audit: &orgaudit.MemoryLog{}, now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
		org: uuid.New(), other: uuid.New()}
	var err error
	if w.files, err = storage.NewLocal(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	w.store.SetAuditSink(w.audit.Append)
	w.owner, w.manager = member(w.org, auth.RoleOwner, "Rudo Owner"), member(w.org, auth.RoleManager, "Mona Manager")
	w.agent, w.agent2, w.staff = member(w.org, auth.RoleAgent, "Tendai Agent"), member(w.org, auth.RoleAgent, "Chipo Agent"), member(w.org, auth.RoleStaff, "Sam Staff")
	w.otherOwner = member(w.other, auth.RoleOwner, "Blake Other")
	w.landlord = &auth.Principal{UserID: uuid.New(), Name: "Lindiwe Landlord", SessionScope: auth.ScopeCustomer,
		AccountRoles: []auth.AccountRole{{Role: auth.AccountRoleLandlord, Status: auth.AccountRoleActive}}}
	w.tenant = &auth.Principal{UserID: uuid.New(), Name: "Tariro Tenant", SessionScope: auth.ScopeCustomer}
	w.tenant2 = &auth.Principal{UserID: uuid.New(), Name: "Other Tenant", SessionScope: auth.ScopeCustomer}
	w.admin = &auth.Principal{UserID: uuid.New(), Name: "Admin", SessionScope: auth.ScopeAdmin}
	w.listing, w.listing2, w.shortStay, w.ownL = uuid.New(), uuid.New(), uuid.New(), uuid.New()
	agentID := w.agent.UserID
	org := w.org
	landlord := w.landlord.UserID
	card := func(title string) saved.Snapshot { return saved.Snapshot{Title: title, Reference: "RP-" + title} }
	w.cat = fakeListings{m: map[uuid.UUID]ListingInfo{
		w.listing:   {ID: w.listing, Slug: "flat-one", Reference: "RP-1", Title: "Flat one", Category: "residential", MarketCode: "ZW", OrganisationID: &org, AgentUserID: &agentID, Public: true, Card: card("Flat one")},
		w.listing2:  {ID: w.listing2, Slug: "flat-two", Reference: "RP-2", Title: "Flat two", Category: "residential", MarketCode: "ZW", OrganisationID: &org, Public: true, Card: card("Flat two")},
		w.shortStay: {ID: w.shortStay, Slug: "lodge", Reference: "RP-3", Title: "Lodge", Category: ShortStay, MarketCode: "ZW", OrganisationID: &org, Public: true},
		w.ownL:      {ID: w.ownL, Slug: "cottage", Reference: "RP-4", Title: "Cottage", Category: "residential", MarketCode: "ZW", SellerUserID: &landlord, Public: true, Card: card("Cottage")},
	}}
	people := func(_ context.Context, id uuid.UUID) (string, string, bool, error) {
		for _, p := range []*auth.Principal{w.tenant, w.tenant2} {
			if p.UserID == id {
				return p.Name, strings.ToLower(strings.Fields(p.Name)[0]) + "@example.com", true, nil
			}
		}
		return "", "", false, nil
	}
	w.svc = NewService(Deps{Store: w.store, Files: w.files, Listings: w.cat, People: people, Now: func() time.Time { return w.now }})
	return w
}

func s(v string) *string   { return &v }
func f(v float64) *float64 { return &v }
func n(v int) *int         { return &v }

func rentalSectionsV1() []Section {
	yesNo := []Option{{Value: "yes", Label: "Yes"}, {Value: "no", Label: "No"}}
	return []Section{
		{Key: "about", Title: "About you", Fields: []Field{
			{Key: "occupants", Type: TypeNumber, Label: "How many people?", Required: true, Min: f(1), Max: f(8)},
			{Key: "move_in", Type: TypeDate, Label: "Move-in date", Required: true},
		}},
		{Key: "household", Title: "Household", Fields: []Field{
			{Key: "has_pets", Type: TypeSingleChoice, Label: "Do you have pets?", Required: true, Options: yesNo},
			{Key: "pets_details", Type: TypeLongText, Label: "Describe them", Required: true, VisibleIf: &VisibleIf{Field: "has_pets", Equals: s("yes")}},
		}},
		{Key: "documents", Title: "Documents", Fields: []Field{
			{Key: "payslip", Type: TypeFile, Label: "Payslip", Required: true, Accept: []string{FilePDF}, MaxFiles: n(2)},
		}},
	}
}

func (w *world) formInput(sections []Section) FormInput {
	return FormInput{Name: s("Rental application"), MarketCode: s("zw"), Purpose: s("To check affordability."), PrivacyNotice: s("We keep this for a year."),
		RetentionDays: n(365), Sections: &sections}
}

// publishedForm creates and publishes the rental form of the organisation
// and opens intake on listing.
func (w *world) publishedForm() FormView {
	w.t.Helper()
	ctx := context.Background()
	f, err := w.svc.CreateForm(ctx, w.owner, w.org, w.formInput(rentalSectionsV1()))
	if err != nil {
		w.t.Fatal(err)
	}
	if f, err = w.svc.PublishForm(ctx, w.owner, w.org, f.ID); err != nil {
		w.t.Fatal(err)
	}
	id := f.ID.String()
	if _, err := w.svc.PutIntake(ctx, w.owner, w.listing, IntakeInput{FormID: &id, Status: IntakeOpen}); err != nil {
		w.t.Fatal(err)
	}
	return f
}

var pdf = []byte("%PDF-1.4\n1 0 obj<<>>endobj\ntrailer<<>>\n%%EOF\n")

func raw(m map[string]any) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	for k, v := range m {
		b, _ := json.Marshal(v)
		out[k] = b
	}
	return out
}

// submitted starts, answers and submits an application of the tenant on listing.
func (w *world) submitted(p *auth.Principal) ApplicantView {
	w.t.Helper()
	ctx := context.Background()
	v, _, err := w.svc.Start(ctx, p, w.listing)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.svc.Autosave(ctx, p, v.ID, raw(map[string]any{"occupants": 2, "move_in": "2026-12-01", "has_pets": "no"})); err != nil {
		w.t.Fatal(err)
	}
	if _, _, err := w.svc.Upload(ctx, p, v.ID, "payslip", "payslip.pdf", pdf); err != nil {
		w.t.Fatal(err)
	}
	out, err := w.svc.Submit(ctx, p, v.ID)
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func wantVErr(t *testing.T, err error, path string) {
	t.Helper()
	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("err = %v, want a validation error on %s", err, path)
	}
	if _, ok := v.Fields[path]; !ok {
		t.Fatalf("fields = %v, want %s", v.Fields, path)
	}
}

// FR-017: the definition rules, including visible_if pointing only back.
func TestValidateSectionsTable(t *testing.T) {
	yesNo := []Option{{Value: "yes", Label: "Yes"}, {Value: "no", Label: "No"}}
	choice := Field{Key: "pets", Type: TypeSingleChoice, Label: "Pets?", Options: yesNo}
	text := func(key string) Field { return Field{Key: key, Type: TypeShortText, Label: "Text"} }
	sec := func(fields ...Field) []Section { return []Section{{Key: "main", Title: "Main", Fields: fields}} }
	cases := []struct {
		name     string
		sections []Section
		complete bool
		path     string // "" = valid
	}{
		{"valid", sec(choice, Field{Key: "details", Type: TypeLongText, Label: "Details", VisibleIf: &VisibleIf{Field: "pets", Equals: s("yes")}}), true, ""},
		{"visible_if referencing a later field", sec(Field{Key: "details", Type: TypeLongText, Label: "Details", VisibleIf: &VisibleIf{Field: "pets", Equals: s("yes")}}, choice), false, "sections[0].fields[0].visible_if.field"},
		{"visible_if referencing itself", sec(Field{Key: "pets", Type: TypeSingleChoice, Label: "P", Options: yesNo, VisibleIf: &VisibleIf{Field: "pets", Equals: s("yes")}}), false, "sections[0].fields[0].visible_if.field"},
		{"visible_if on a text field", sec(text("name"), Field{Key: "x", Type: TypeShortText, Label: "X", VisibleIf: &VisibleIf{Field: "name", Equals: s("a")}}), false, "sections[0].fields[1].visible_if.field"},
		{"visible_if value not an option", sec(choice, Field{Key: "x", Type: TypeShortText, Label: "X", VisibleIf: &VisibleIf{Field: "pets", Equals: s("maybe")}}), false, "sections[0].fields[1].visible_if"},
		{"visible_if with equals and in", sec(choice, Field{Key: "x", Type: TypeShortText, Label: "X", VisibleIf: &VisibleIf{Field: "pets", Equals: s("yes"), In: []string{"no"}}}), false, "sections[0].fields[1].visible_if"},
		{"visible_if unknown field", sec(Field{Key: "x", Type: TypeShortText, Label: "X", VisibleIf: &VisibleIf{Field: "nope", Equals: s("yes")}}), false, "sections[0].fields[0].visible_if.field"},
		{"duplicate field keys", sec(text("a"), text("a")), false, "sections[0].fields[1].key"},
		{"field key equals a section key", sec(text("main")), false, "sections[0].fields[0].key"},
		{"bad key", sec(text("Bad Key")), false, "sections[0].fields[0].key"},
		{"unknown type", sec(Field{Key: "a", Type: "colour", Label: "A"}), false, "sections[0].fields[0].type"},
		{"choice with one option", sec(Field{Key: "a", Type: TypeSingleChoice, Label: "A", Options: yesNo[:1]}), false, "sections[0].fields[0].options"},
		{"duplicate option values", sec(Field{Key: "a", Type: TypeMultipleChoice, Label: "A", Options: []Option{{"x", "X"}, {"x", "Y"}}}), false, "sections[0].fields[0].options[1].value"},
		{"options on a text field", sec(Field{Key: "a", Type: TypeShortText, Label: "A", Options: yesNo}), false, "sections[0].fields[0].options"},
		{"accept outside the allowlist", sec(Field{Key: "a", Type: TypeFile, Label: "A", Accept: []string{"pdf", "exe"}}), false, "sections[0].fields[0].accept[1]"},
		{"too many files", sec(Field{Key: "a", Type: TypeFile, Label: "A", MaxFiles: n(6)}), false, "sections[0].fields[0].max_files"},
		{"min above max", sec(Field{Key: "a", Type: TypeNumber, Label: "A", Min: f(5), Max: f(1)}), false, "sections[0].fields[0].max"},
		{"min on a date", sec(Field{Key: "a", Type: TypeDate, Label: "A", Min: f(1)}), false, "sections[0].fields[0].min"},
		{"multiple choice min above options", sec(Field{Key: "a", Type: TypeMultipleChoice, Label: "A", Options: yesNo, Min: f(3)}), false, "sections[0].fields[0].min"},
		{"max_length above the cap", sec(Field{Key: "a", Type: TypeShortText, Label: "A", MaxLength: n(501)}), false, "sections[0].fields[0].max_length"},
		{"bad min_date", sec(Field{Key: "a", Type: TypeDate, Label: "A", MinDate: s("tomorrow")}), false, "sections[0].fields[0].min_date"},
		{"missing label", sec(Field{Key: "a", Type: TypeShortText}), false, "sections[0].fields[0].label"},
		{"empty section on publish", []Section{{Key: "main", Title: "Main"}}, true, "sections[0].fields"},
		{"no sections on publish", []Section{}, true, "sections"},
		{"no sections in a draft", []Section{}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateSections(tc.sections, tc.complete)
			if tc.path == "" {
				if err != nil {
					t.Fatalf("unexpected error %v", err)
				}
				return
			}
			wantVErr(t, err, tc.path)
		})
	}
}

// FR-018: government IDs and similar get a warning (never a block).
func TestSensitiveFieldWarnings(t *testing.T) {
	cases := map[string]string{
		"passport_number": "", "id_number": "", "national_id": "", "ssn": "", "bank_account": "", "bvn": "", "tax_number": "",
	}
	for key := range cases {
		w := Warnings([]Section{{Key: "s", Title: "S", Fields: []Field{{Key: key, Type: TypeShortText, Label: "Your " + strings.ReplaceAll(key, "_", " ")}}}})
		if len(w) != 1 || w[0].Code != WarningSensitiveField || w[0].Field != key || !strings.Contains(w[0].Message, "necessary") {
			t.Fatalf("%s: warnings %+v", key, w)
		}
	}
	// By label too.
	if w := Warnings([]Section{{Key: "s", Title: "S", Fields: []Field{{Key: "doc", Type: TypeFile, Label: "Copy of your Social Security card"}}}}); len(w) != 1 {
		t.Fatalf("label warnings %+v", w)
	}
	for _, label := range []string{"Do you have pets?", "Monthly income", "Move-in date", "Previous landlord", "Identity of your employer"} {
		if w := Warnings([]Section{{Key: "s", Title: "S", Fields: []Field{{Key: "x", Type: TypeShortText, Label: label}}}}); len(w) != 0 {
			t.Fatalf("%q warned: %+v", label, w)
		}
	}
	// The warning is on the form view, and publishing is not blocked.
	wd := newWorld(t)
	sections := rentalSectionsV1()
	sections[0].Fields = append(sections[0].Fields, Field{Key: "passport", Type: TypeShortText, Label: "Passport number"})
	v, err := wd.svc.CreateForm(context.Background(), wd.owner, wd.org, wd.formInput(sections))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Warnings) != 1 || v.Warnings[0].Field != "passport" {
		t.Fatalf("warnings %+v", v.Warnings)
	}
	if p, err := wd.svc.PublishForm(context.Background(), wd.owner, wd.org, v.ID); err != nil || p.Status != FormPublished || len(p.Warnings) != 1 {
		t.Fatalf("publish %+v %v", p, err)
	}
}

// FR-017 acceptance: an application keeps showing the exact questions of
// its submission version after version 2 is published.
func TestApplicationKeepsItsFormVersion(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	v1 := w.publishedForm()
	app := w.submitted(w.tenant)
	if app.Status != StatusSubmitted || app.Reference == nil || !strings.HasPrefix(*app.Reference, "RPA-APP-2026-") {
		t.Fatalf("submitted %+v", app)
	}
	// A version with submissions cannot change in place (the store guard).
	locked, _ := w.store.Form(ctx, v1.ID)
	locked.Name = "Changed"
	if err := w.store.UpdateDraft(ctx, locked); !errors.Is(err, ErrFormLocked) {
		t.Fatalf("update of a submitted version: %v", err)
	}
	// Editing the published version makes version 2 (a draft).
	sections := rentalSectionsV1()
	sections[0].Fields[0].Label = "How many adults and children?"
	sections[1].Fields = append(sections[1].Fields, Field{Key: "smoker", Type: TypeSingleChoice, Label: "Do you smoke?", Required: true,
		Options: []Option{{"yes", "Yes"}, {"no", "No"}}})
	v2, created, err := w.svc.EditForm(ctx, w.owner, w.org, v1.ID, FormInput{Sections: &sections})
	if err != nil || !created || v2.Version != 2 || v2.Status != FormDraft || v2.FamilyID != v1.FamilyID {
		t.Fatalf("new version %+v %v %v", v2, created, err)
	}
	if _, _, err := w.svc.EditForm(ctx, w.owner, w.org, v1.ID, FormInput{Name: s("Again")}); !errors.Is(err, ErrDraftExists) {
		t.Fatalf("second draft: %v", err)
	}
	if _, err := w.svc.PublishForm(ctx, w.owner, w.org, v2.ID); err != nil {
		t.Fatal(err)
	}
	old, _, err := w.svc.GetForm(ctx, w.owner, w.org, v1.ID)
	if err != nil || old.Status != FormRetired || !old.Locked || old.SubmissionCount != 1 {
		t.Fatalf("v1 after publishing v2: %+v %v", old, err)
	}
	if _, _, err := w.svc.EditForm(ctx, w.owner, w.org, v1.ID, FormInput{Name: s("x")}); !errors.Is(err, ErrFormNotEditable) {
		t.Fatalf("edit retired: %v", err)
	}
	// The intake moved to version 2.
	in, err := w.svc.GetIntake(ctx, w.owner, w.listing)
	if err != nil || *in.FormVersion != 2 || in.Status != IntakeOpen {
		t.Fatalf("intake %+v %v", in, err)
	}
	// The applicant and the agency still see version 1's questions.
	mine, err := w.svc.MineOne(ctx, w.tenant, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	pf := mine.Form.(PublicForm)
	if pf.Version != 1 || pf.Sections[0].Fields[0].Label != "How many people?" || len(pf.Sections[1].Fields) != 2 {
		t.Fatalf("applicant sees %+v", pf)
	}
	got, err := w.svc.ReceivedOne(ctx, w.owner, w.org, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	rendered := got.Rendered.([]RenderedSection)
	if got.Form.(PublicForm).Version != 1 || rendered[0].Fields[0].Label != "How many people?" || len(rendered[1].Fields) != 2 ||
		*rendered[1].Fields[0].Display != "No" || rendered[1].Fields[1].Visible {
		t.Fatalf("agency sees %+v", rendered)
	}
	// A new applicant starts on version 2.
	other, created, err := w.svc.Start(ctx, w.tenant2, w.listing)
	if err != nil || !created || other.Form.(PublicForm).Version != 2 {
		t.Fatalf("new applicant %+v %v", other.Form, err)
	}
}

// FR-019: intake OPEN / PAUSED / CLOSED, and a submission after close is
// 409 with the reason.
func TestIntakeAndSubmitAfterClose(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	form := w.publishedForm()
	id := form.ID.String()
	put := func(p *auth.Principal, listing uuid.UUID, status string, closes *time.Time) (IntakeView, error) {
		return w.svc.PutIntake(ctx, p, listing, IntakeInput{FormID: &id, Status: status, ClosesAt: closes})
	}
	closedReason := func(err error) string {
		var c *ClosedError
		if !errors.As(err, &c) {
			t.Fatalf("err = %v, want APPLICATIONS_CLOSED", err)
		}
		return c.Reason
	}
	// Short stays take no applications.
	if _, err := put(w.owner, w.shortStay, IntakeOpen, nil); !errors.Is(err, ErrListingNotEligible) {
		t.Fatalf("short stay: %v", err)
	}
	// Only the listing's editors: the unassigned agent reads (403), staff and
	// another organisation do not see it (404).
	if _, err := put(w.agent2, w.listing, IntakeOpen, nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("unassigned agent: %v", err)
	}
	if _, err := put(w.agent, w.listing, IntakeOpen, nil); err != nil {
		t.Fatalf("assigned agent: %v", err)
	}
	for _, p := range []*auth.Principal{w.staff, w.otherOwner, w.tenant} {
		if _, err := put(p, w.listing, IntakeOpen, nil); !errors.Is(err, ErrListingNotFound) {
			t.Fatalf("%s: %v", p.Name, err)
		}
	}
	// Another organisation's form, a draft and a wrong market are refused.
	theirs, err := w.svc.CreateForm(ctx, w.otherOwner, w.other, w.formInput(rentalSectionsV1()))
	if err != nil {
		t.Fatal(err)
	}
	theirs, _ = w.svc.PublishForm(ctx, w.otherOwner, w.other, theirs.ID)
	tid := theirs.ID.String()
	if _, err := w.svc.PutIntake(ctx, w.owner, w.listing2, IntakeInput{FormID: &tid, Status: IntakeOpen}); err == nil {
		t.Fatal("attached another organisation's form")
	} else {
		wantVErr(t, err, "form_id")
	}
	draft, _ := w.svc.CreateForm(ctx, w.owner, w.org, w.formInput(rentalSectionsV1()))
	did := draft.ID.String()
	_, err = w.svc.PutIntake(ctx, w.owner, w.listing2, IntakeInput{FormID: &did, Status: IntakeOpen})
	wantVErr(t, err, "form_id")

	// PAUSED and CLOSED stop new applications, with the reason.
	if _, err := put(w.owner, w.listing, IntakePaused, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.svc.Start(ctx, w.tenant, w.listing); closedReason(err) != ReasonPaused {
		t.Fatal("paused")
	}
	if _, err := put(w.owner, w.listing, IntakeClosed, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.svc.Start(ctx, w.tenant, w.listing); closedReason(err) != ReasonClosed {
		t.Fatal("closed")
	}
	// No form at all.
	if _, _, err := w.svc.Start(ctx, w.tenant, w.listing2); closedReason(err) != ReasonNone {
		t.Fatal("none")
	}
	// OPEN with a closing time; once it passes the intake reads CLOSED.
	closes := w.now.Add(48 * time.Hour)
	if _, err := put(w.owner, w.listing, IntakeOpen, &closes); err != nil {
		t.Fatal(err)
	}
	app, created, err := w.svc.Start(ctx, w.tenant, w.listing)
	if err != nil || !created || app.Status != StatusDraft {
		t.Fatalf("start %+v %v", app, err)
	}
	again, created, err := w.svc.Start(ctx, w.tenant, w.listing)
	if err != nil || created || again.ID != app.ID {
		t.Fatal("start is not idempotent")
	}
	if _, err := w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"occupants": 2, "move_in": "2026-12-01", "has_pets": "no"})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "p.pdf", pdf); err != nil {
		t.Fatal(err)
	}
	w.now = closes.Add(time.Minute)
	in, _ := w.svc.GetIntake(ctx, w.owner, w.listing)
	if in.Status != IntakeClosed || in.StoredStatus != IntakeOpen || *in.ClosedReason != ReasonClosesAtPassed {
		t.Fatalf("intake after closes_at %+v", in)
	}
	if _, err := w.svc.Submit(ctx, w.tenant, app.ID); closedReason(err) != ReasonClosesAtPassed {
		t.Fatal("submit after close")
	}
	if ReasonMessage(ReasonClosesAtPassed) == "" || !strings.Contains(ReasonMessage(ReasonPaused), "paused") {
		t.Fatal("reasons need messages")
	}
	// The summary on the public listing.
	sums, _ := w.svc.Summaries(ctx, []uuid.UUID{w.listing, w.listing2, w.shortStay}, map[uuid.UUID]bool{w.shortStay: true})
	if len(sums) != 1 || sums[w.listing].Open || *sums[w.listing].FormName != "Rental application" {
		t.Fatalf("summaries %+v", sums)
	}
	// Detaching.
	if v, err := w.svc.PutIntake(ctx, w.owner, w.listing, IntakeInput{Status: IntakeNone}); err != nil || v.Status != IntakeNone || v.FormID != nil {
		t.Fatalf("detach %+v %v", v, err)
	}
	if _, _, err := w.svc.PublicForm(ctx, "flat-one"); !errors.Is(err, ErrFormUnavailable) {
		t.Fatalf("public form after detach: %v", err)
	}
}

// visible_if: a hidden field is not required and its answer is dropped.
func TestConditionalRequiredFields(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app, _, err := w.svc.Start(ctx, w.tenant, w.listing)
	if err != nil {
		t.Fatal(err)
	}
	// Autosave validates each answer but allows missing ones.
	_, err = w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"occupants": 20}))
	wantVErr(t, err, "answers.occupants")
	_, err = w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"has_pets": "maybe"}))
	wantVErr(t, err, "answers.has_pets")
	_, err = w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"nope": "x"}))
	wantVErr(t, err, "answers.nope")
	_, err = w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"move_in": "1 December"}))
	wantVErr(t, err, "answers.move_in")
	v, err := w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"occupants": 2, "move_in": "2026-12-01", "has_pets": "yes"}))
	if err != nil {
		t.Fatal(err)
	}
	if missing := v.MissingRequired.([]string); !slices.Equal(missing, []string{"pets_details", "payslip"}) {
		t.Fatalf("missing %v", missing)
	}
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "p.pdf", pdf); err != nil {
		t.Fatal(err)
	}
	// Pets: yes makes the description required.
	_, err = w.svc.Submit(ctx, w.tenant, app.ID)
	wantVErr(t, err, "answers.pets_details")
	// Pets: no hides it: not required, and an answer given earlier is dropped.
	if _, err := w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"pets_details": "A cat", "has_pets": "no"})); err != nil {
		t.Fatal(err)
	}
	done, err := w.svc.Submit(ctx, w.tenant, app.ID)
	if err != nil {
		t.Fatal(err)
	}
	answers := done.Answers.(Answers)
	if _, ok := answers["pets_details"]; ok || answers["has_pets"] != "no" {
		t.Fatalf("answers %+v", answers)
	}
	// Submitted: no more autosave.
	if _, err := w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"occupants": 3})); !errors.Is(err, ErrNotEditable) {
		t.Fatalf("autosave after submit: %v", err)
	}
}

func docx(t *testing.T, macro bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	ct := `<Types><Override ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`
	if macro {
		ct = `<Types><Override ContentType="application/vnd.ms-word.document.macroEnabled.main+xml"/></Types>`
	}
	for name, body := range map[string]string{"[Content_Types].xml": ct, "word/document.xml": "<w:document/>"} {
		fw, err := z.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fw.Write([]byte(body))
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// FR-018 acceptance: disallowed file types are rejected (by content), and
// the size cap and the per-field file count hold.
func TestFileTypesAndSizeCap(t *testing.T) {
	heic := append([]byte{0, 0, 0, 24}, []byte("ftypheic\x00\x00\x00\x00mif1heic")...)
	webp := append([]byte("RIFF\x10\x00\x00\x00WEBPVP8 "), make([]byte, 16)...)
	jpg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 16)...)
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 16)...)
	sniffs := map[string][]byte{FilePDF: pdf, FileJPG: jpg, FilePNG: png, FileWEBP: webp, FileHEIC: heic, FileDOCX: docx(t, false),
		"": []byte("MZ\x90\x00 an executable")}
	for want, content := range sniffs {
		if got := Sniff(content); got != want {
			t.Fatalf("Sniff = %q, want %q", got, want)
		}
	}
	if Sniff(docx(t, true)) != "" || Sniff([]byte("<html><script>")) != "" || Sniff([]byte("plain text")) != "" {
		t.Fatal("a macro document, HTML or text was accepted")
	}

	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app, _, _ := w.svc.Start(ctx, w.tenant, w.listing)
	var unsupported *UnsupportedFileError
	// The field accepts PDF only: a PNG named .pdf is still a PNG.
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "payslip.pdf", png); !errors.As(err, &unsupported) || !slices.Equal(unsupported.Accepted, []string{FilePDF}) {
		t.Fatalf("png on a pdf field: %v", err)
	}
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "virus.pdf", sniffs[""]); !errors.Is(err, ErrUnsupportedFile) {
		t.Fatalf("executable: %v", err)
	}
	big := append(append([]byte{}, pdf...), make([]byte, MaxFileBytes)...)
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "big.pdf", big); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("over 10 MB: %v", err)
	}
	_, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "occupants", "x.pdf", pdf)
	wantVErr(t, err, "field")
	fv, view, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "../../etc/My payslip.PDF", pdf)
	if err != nil || fv.FileName != "My payslip.pdf" || fv.ContentType != "application/pdf" {
		t.Fatalf("upload %+v %v", fv, err)
	}
	if ids := view.Answers.(Answers)["payslip"].([]string); len(ids) != 1 || ids[0] != fv.ID.String() {
		t.Fatalf("answers after upload %+v", view.Answers)
	}
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "two.pdf", pdf); err != nil {
		t.Fatal(err)
	}
	var tooMany *TooManyFilesError
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "three.pdf", pdf); !errors.As(err, &tooMany) || tooMany.Max != 2 {
		t.Fatalf("third file: %v", err)
	}
	// The applicant reads their own file; deleting one frees a slot and
	// removes the stored object.
	fc, err := w.svc.OpenFile(ctx, w.tenant, app.ID, fv.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(fc.Content)
	_ = fc.Content.Close()
	if !bytes.Equal(body, pdf) {
		t.Fatal("file content")
	}
	stored, _ := w.store.Files(ctx, app.ID)
	if _, err := w.svc.DeleteFile(ctx, w.tenant, app.ID, fv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.files.Open(ctx, stored[0].ObjectKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("object after delete: %v", err)
	}
	if len(w.store.PendingPurgeKeys()) != 0 {
		t.Fatal("purge queue not drained")
	}
}

// Every read of answers and every file download is in the organisation audit.
func TestViewsAreAudited(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app := w.submitted(w.tenant)
	// The list carries no answers and records nothing.
	page, err := w.svc.Received(ctx, w.agent, w.org, AppFilter{}, 1, 20)
	if err != nil || page.Total != 1 || page.Items[0].Answers != nil || page.StatusCounts[StatusSubmitted] != 1 {
		t.Fatalf("list %+v %v", page, err)
	}
	if len(w.audit.All()) != 0 {
		t.Fatal("a list was audited")
	}
	if _, err := w.svc.ReceivedOne(ctx, w.agent, w.org, app.ID); err != nil {
		t.Fatal(err)
	}
	files := app.Files.([]FileView)
	fc, err := w.svc.ReceivedFile(ctx, w.owner, w.org, app.ID, files[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	_ = fc.Content.Close()
	events := w.audit.All()
	if len(events) != 2 || events[0].Action != orgaudit.ActionApplicationViewed || events[0].ActorUserID == nil || *events[0].ActorUserID != w.agent.UserID ||
		events[0].TargetType != orgaudit.TargetApplication || events[0].TargetID != app.ID || events[0].TargetLabel != *app.Reference ||
		events[1].Action != orgaudit.ActionApplicationFileViewed || events[1].After["file_id"] != files[0].ID.String() || events[0].OrganisationID != w.org {
		t.Fatalf("audit %+v", events)
	}
}

func TestStateMachineTable(t *testing.T) {
	allowed := map[string][]string{
		StatusSubmitted:   {StatusUnderReview, StatusMoreInfo, StatusShortlisted, StatusAccepted, StatusRejected},
		StatusUnderReview: {StatusMoreInfo, StatusShortlisted, StatusAccepted, StatusRejected},
		StatusShortlisted: {StatusAccepted, StatusRejected},
	}
	for _, from := range Statuses() {
		for _, to := range Statuses() {
			if got, want := CanTransition(from, to), slices.Contains(allowed[from], to); got != want {
				t.Fatalf("%s -> %s: %v, want %v", from, to, got, want)
			}
		}
	}
	for _, st := range Statuses() {
		if Final(st) != (st == StatusAccepted || st == StatusRejected || st == StatusWithdrawn) {
			t.Fatalf("final %s", st)
		}
	}
	// Through the service: an invalid move is a TransitionError.
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app := w.submitted(w.tenant)
	_, err := w.svc.SetStatus(ctx, w.owner, w.org, app.ID, StatusInput{Status: StatusMoreInfo})
	wantVErr(t, err, "status")
	if _, err := w.svc.SetStatus(ctx, w.owner, w.org, app.ID, StatusInput{Status: StatusShortlisted, Note: s("Strong references.")}); err != nil {
		t.Fatal(err)
	}
	var te *TransitionError
	if _, err := w.svc.SetStatus(ctx, w.owner, w.org, app.ID, StatusInput{Status: StatusUnderReview}); !errors.As(err, &te) ||
		te.From != StatusShortlisted || !slices.Equal(te.Allowed, []string{StatusAccepted, StatusRejected}) {
		t.Fatalf("shortlisted -> under review: %v", err)
	}
	got, err := w.svc.SetStatus(ctx, w.owner, w.org, app.ID, StatusInput{Status: StatusAccepted})
	if err != nil || got.Status != StatusAccepted || got.DecidedAt == nil || len(got.AllowedTransitions) != 0 {
		t.Fatalf("accept %+v %v", got, err)
	}
	if notes := got.Notes.([]NoteView); len(notes) != 1 || notes[0].Text != "Strong references." {
		t.Fatalf("notes %+v", got.Notes)
	}
	// Notes never reach the applicant.
	mine, _ := w.svc.MineOne(ctx, w.tenant, app.ID)
	raw, _ := json.Marshal(mine)
	if strings.Contains(string(raw), "Strong references") || strings.Contains(string(raw), "actor_name") {
		t.Fatalf("applicant view leaks: %s", raw)
	}
	if _, err := w.svc.SetStatus(ctx, w.owner, w.org, app.ID, StatusInput{Status: StatusRejected}); !errors.As(err, &te) {
		t.Fatalf("final moved: %v", err)
	}
}

func TestWithdraw(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app := w.submitted(w.tenant)
	got, err := w.svc.Withdraw(ctx, w.tenant, app.ID)
	if err != nil || got.Status != StatusWithdrawn || got.CanWithdraw || got.CanEdit {
		t.Fatalf("withdraw %+v %v", got, err)
	}
	var te *TransitionError
	if _, err := w.svc.Withdraw(ctx, w.tenant, app.ID); !errors.As(err, &te) || te.From != StatusWithdrawn {
		t.Fatalf("withdraw twice: %v", err)
	}
	if _, err := w.svc.SetStatus(ctx, w.owner, w.org, app.ID, StatusInput{Status: StatusAccepted}); !errors.As(err, &te) {
		t.Fatalf("accept withdrawn: %v", err)
	}
	// A draft can be withdrawn too (no event for the organisation).
	draft, _, _ := w.svc.Start(ctx, w.tenant2, w.listing)
	before := len(w.store.OutboxEvents())
	if v, err := w.svc.Withdraw(ctx, w.tenant2, draft.ID); err != nil || v.Status != StatusWithdrawn {
		t.Fatalf("withdraw draft %+v %v", v, err)
	}
	if len(w.store.OutboxEvents()) != before {
		t.Fatal("a draft's withdrawal was announced")
	}
}

// FR-019: request more information, the applicant edits and resubmits.
func TestRequestInfoRoundTrip(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app := w.submitted(w.tenant)
	_, err := w.svc.RequestInfo(ctx, w.agent, w.org, app.ID, "  ")
	wantVErr(t, err, "message")
	got, err := w.svc.RequestInfo(ctx, w.agent, w.org, app.ID, "Please add a second payslip.")
	if err != nil || got.Status != StatusMoreInfo {
		t.Fatalf("request info %+v %v", got, err)
	}
	mine, _ := w.svc.MineOne(ctx, w.tenant, app.ID)
	if !mine.CanEdit || mine.InfoRequest == nil || mine.InfoRequest.Message != "Please add a second payslip." {
		t.Fatalf("applicant %+v", mine)
	}
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "second.pdf", pdf); err != nil {
		t.Fatal(err)
	}
	// Even with intake closed, an answer to a request for information goes through.
	id := mustForm(t, w).String()
	if _, err := w.svc.PutIntake(ctx, w.owner, w.listing, IntakeInput{FormID: &id, Status: IntakeClosed}); err != nil {
		t.Fatal(err)
	}
	again, err := w.svc.Submit(ctx, w.tenant, app.ID)
	if err != nil || again.Status != StatusSubmitted || *again.Reference != *app.Reference || len(again.Files.([]FileView)) != 2 {
		t.Fatalf("resubmit %+v %v", again, err)
	}
	if again.Timeline[0].Kind != EventResubmitted || again.Timeline[1].Kind != EventInfoRequested || *again.Timeline[1].Message != "Please add a second payslip." {
		t.Fatalf("timeline %+v", again.Timeline)
	}
}

func mustForm(t *testing.T, w *world) uuid.UUID {
	t.Helper()
	in, err := w.svc.GetIntake(context.Background(), w.owner, w.listing)
	if err != nil || in.FormID == nil {
		t.Fatal(err)
	}
	return *in.FormID
}

// The outbox events behind the emails, the CRM link and analytics.
func TestNotificationsEnqueued(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app := w.submitted(w.tenant)
	if _, err := w.svc.RequestInfo(ctx, w.owner, w.org, app.ID, "More please."); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Submit(ctx, w.tenant, app.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.SetStatus(ctx, w.owner, w.org, app.ID, StatusInput{Status: StatusRejected}); err != nil {
		t.Fatal(err)
	}
	var types []string
	var resub []bool
	for _, ev := range w.store.OutboxEvents() {
		types = append(types, ev.Type)
		var p EventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil || p.ApplicationID != app.ID || p.ListingID != w.listing || ev.AggregateID != app.ID {
			t.Fatalf("payload %s %v", ev.Payload, err)
		}
		if ev.Type == EventTypeSubmitted {
			resub = append(resub, p.Resubmission)
		}
	}
	if !slices.Equal(types, []string{EventTypeSubmitted, EventTypeInfoRequested, EventTypeSubmitted, EventTypeStatusChanged}) || !slices.Equal(resub, []bool{false, true}) {
		t.Fatalf("outbox %v %v", types, resub)
	}
	// The notice the emails are built from.
	n, err := w.svc.Notice(ctx, app.ID)
	if err != nil || n.Reference != *app.Reference || n.ApplicantEmail != "tariro@example.com" || n.AgentUserID == nil || *n.AgentUserID != w.agent.UserID ||
		n.InfoMessage != "More please." || n.ListingTitle != "Flat one" {
		t.Fatalf("notice %+v %v", n, err)
	}
}

// The retention job erases final applications past the form's retention
// and keeps a tombstone; the files' objects are removed.
func TestRetentionJob(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	old := w.submitted(w.tenant)
	if _, err := w.svc.SetStatus(ctx, w.owner, w.org, old.ID, StatusInput{Status: StatusRejected}); err != nil {
		t.Fatal(err)
	}
	open := w.submitted(w.tenant2)
	files, _ := w.store.Files(ctx, old.ID)
	w.now = w.now.AddDate(0, 0, 364)
	if n, _, err := w.svc.RunRetention(ctx); err != nil || n != 0 {
		t.Fatalf("before the end of retention: %d %v", n, err)
	}
	w.now = w.now.AddDate(0, 0, 2)
	erased, purged, err := w.svc.RunRetention(ctx)
	if err != nil || erased != 1 || purged != 1 {
		t.Fatalf("retention %d %d %v", erased, purged, err)
	}
	if _, err := w.files.Open(ctx, files[0].ObjectKey); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("object kept: %v", err)
	}
	tomb, err := w.svc.MineOne(ctx, w.tenant, old.ID)
	if err != nil || !tomb.Erased || len(tomb.Answers.(Answers)) != 0 || len(tomb.Files.([]FileView)) != 0 || *tomb.Reference != *old.Reference ||
		tomb.Status != StatusRejected || tomb.Timeline[0].Kind != EventErased {
		t.Fatalf("tombstone %+v %v", tomb, err)
	}
	// The open one is untouched; running again erases nothing.
	if still, _ := w.svc.MineOne(ctx, w.tenant2, open.ID); still.Erased || len(still.Answers.(Answers)) == 0 {
		t.Fatal("an open application was erased")
	}
	if n, _, _ := w.svc.RunRetention(ctx); n != 0 {
		t.Fatal("erased twice")
	}
}

// Account deletion: drafts deleted, the others withdrawn, erased and unlinked.
func TestAccountDeletion(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	sub := w.submitted(w.tenant)
	id := mustForm(t, w).String()
	if _, err := w.svc.PutIntake(ctx, w.owner, w.listing2, IntakeInput{FormID: &id, Status: IntakeOpen}); err != nil {
		t.Fatal(err)
	}
	draft, _, _ := w.svc.Start(ctx, w.tenant, w.listing2)
	deleted, anonymised := w.store.EraseApplicant(w.tenant.UserID, w.now)
	if deleted != 1 || anonymised != 1 {
		t.Fatalf("deleted %d anonymised %d", deleted, anonymised)
	}
	if _, err := w.store.Application(ctx, draft.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("draft kept")
	}
	if _, err := w.svc.MineOne(ctx, w.tenant, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("the deleted account still sees it")
	}
	got, err := w.svc.ReceivedOne(ctx, w.owner, w.org, sub.ID)
	if err != nil || got.Status != StatusWithdrawn || !got.Erased || got.Applicant.UserID != nil || got.Applicant.Name != DeletedApplicantName ||
		got.Applicant.Email != nil || len(got.Answers.(Answers)) != 0 {
		t.Fatalf("anonymised %+v %v", got, err)
	}
	if _, err := w.svc.Purge(ctx); err != nil || len(w.store.PendingPurgeKeys()) != 0 {
		t.Fatalf("purge %v", err)
	}
}

// Every agent-side endpoint: another organisation (and anyone who may not
// see the application) gets 404.
func TestWrongOrganisationIsNotFound(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	form := w.publishedForm()
	app := w.submitted(w.tenant)
	fileID := app.Files.([]FileView)[0].ID
	// Forms.
	for _, p := range []*auth.Principal{w.otherOwner, w.tenant, w.admin} {
		if _, err := w.svc.ListForms(ctx, p, w.org, "", "", nil); !errors.Is(err, ErrFormNotFound) {
			t.Fatalf("%s list forms: %v", p.Name, err)
		}
		if _, err := w.svc.CreateForm(ctx, p, w.org, w.formInput(nil)); !errors.Is(err, ErrFormNotFound) {
			t.Fatalf("%s create form: %v", p.Name, err)
		}
	}
	// The other organisation's owner, in their own organisation, cannot reach our form.
	for name, call := range map[string]func() error{
		"get": func() error { _, _, err := w.svc.GetForm(ctx, w.otherOwner, w.other, form.ID); return err },
		"patch": func() error {
			_, _, err := w.svc.EditForm(ctx, w.otherOwner, w.other, form.ID, FormInput{})
			return err
		},
		"publish": func() error { _, err := w.svc.PublishForm(ctx, w.otherOwner, w.other, form.ID); return err },
		"retire":  func() error { _, err := w.svc.RetireForm(ctx, w.otherOwner, w.other, form.ID); return err },
		"get in our org": func() error {
			_, _, err := w.svc.GetForm(ctx, w.otherOwner, w.org, form.ID)
			return err
		},
	} {
		if err := call(); !errors.Is(err, ErrFormNotFound) {
			t.Fatalf("form %s: %v", name, err)
		}
	}
	// STAFF may not read forms (403); AGENT reads but may not change them.
	if _, err := w.svc.ListForms(ctx, w.staff, w.org, "", "", nil); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff: %v", err)
	}
	if list, err := w.svc.ListForms(ctx, w.agent, w.org, "", "", nil); err != nil || len(list) != 1 || list[0].CanEdit {
		t.Fatalf("agent reads: %+v %v", list, err)
	}
	if _, _, err := w.svc.EditForm(ctx, w.agent, w.org, form.ID, FormInput{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent edits: %v", err)
	}
	// Applications.
	for _, p := range []*auth.Principal{w.otherOwner, w.staff, w.tenant, w.admin} {
		if _, err := w.svc.Received(ctx, p, w.org, AppFilter{}, 1, 20); !errors.Is(err, ErrOrganisationUnknown) {
			t.Fatalf("%s list: %v", p.Name, err)
		}
	}
	// agent2 is a member but not this listing's agent: the list is empty.
	if page, err := w.svc.Received(ctx, w.agent2, w.org, AppFilter{}, 1, 20); err != nil || page.Total != 0 {
		t.Fatalf("unassigned agent list %+v %v", page, err)
	}
	for _, c := range []struct {
		p    *auth.Principal
		book uuid.UUID
	}{{w.otherOwner, w.other}, {w.otherOwner, w.org}, {w.staff, w.org}, {w.agent2, w.org}, {w.tenant, w.org}, {w.landlord, w.landlord.UserID}} {
		calls := map[string]func() error{
			"get":  func() error { _, err := w.svc.ReceivedOne(ctx, c.p, c.book, app.ID); return err },
			"file": func() error { _, err := w.svc.ReceivedFile(ctx, c.p, c.book, app.ID, fileID); return err },
			"status": func() error {
				_, err := w.svc.SetStatus(ctx, c.p, c.book, app.ID, StatusInput{Status: StatusUnderReview})
				return err
			},
			"request-info": func() error { _, err := w.svc.RequestInfo(ctx, c.p, c.book, app.ID, "More"); return err },
			"notes":        func() error { _, err := w.svc.AddNote(ctx, c.p, c.book, app.ID, "x"); return err },
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s in %s %s: %v", c.p.Name, c.book, name, err)
			}
		}
	}
	if len(w.audit.All()) != 0 {
		t.Fatal("a refused read was audited")
	}
	// A draft is never visible to the receiving side.
	draft, _, _ := w.svc.Start(ctx, w.tenant2, w.listing)
	if _, err := w.svc.ReceivedOne(ctx, w.owner, w.org, draft.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("draft visible: %v", err)
	}
}

// The applicant endpoints: another applicant's application is 404.
func TestOtherApplicantIsNotFound(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.publishedForm()
	app, _, _ := w.svc.Start(ctx, w.tenant, w.listing)
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "p.pdf", pdf); err != nil {
		t.Fatal(err)
	}
	files, _ := w.store.Files(ctx, app.ID)
	for _, p := range []*auth.Principal{w.tenant2, w.owner, w.agent} {
		calls := map[string]func() error{
			"get": func() error { _, err := w.svc.MineOne(ctx, p, app.ID); return err },
			"patch": func() error {
				_, err := w.svc.Autosave(ctx, p, app.ID, raw(map[string]any{"occupants": 1}))
				return err
			},
			"upload":   func() error { _, _, err := w.svc.Upload(ctx, p, app.ID, "payslip", "p.pdf", pdf); return err },
			"file":     func() error { _, err := w.svc.OpenFile(ctx, p, app.ID, files[0].ID); return err },
			"delete":   func() error { _, err := w.svc.DeleteFile(ctx, p, app.ID, files[0].ID); return err },
			"submit":   func() error { _, err := w.svc.Submit(ctx, p, app.ID); return err },
			"withdraw": func() error { _, err := w.svc.Withdraw(ctx, p, app.ID); return err },
		}
		for name, call := range calls {
			if err := call(); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s %s: %v", p.Name, name, err)
			}
		}
	}
	if page, err := w.svc.Mine(ctx, w.tenant2, "", 1, 20); err != nil || page.Total != 0 {
		t.Fatalf("other applicant's list %+v %v", page, err)
	}
	// An administrator session may not apply.
	if _, _, err := w.svc.Start(ctx, w.admin, w.listing); !errors.Is(err, ErrForbidden) {
		t.Fatalf("admin applies: %v", err)
	}
}

// A direct owner builds their own form and receives the applications to
// their own listing; the organisation audit does not apply.
func TestDirectOwnerBook(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	book := w.landlord.UserID
	f, err := w.svc.CreateForm(ctx, w.landlord, book, w.formInput(rentalSectionsV1()))
	if err != nil || f.OwnerUserID == nil || f.OrganisationID != nil {
		t.Fatalf("create %+v %v", f, err)
	}
	if f, err = w.svc.PublishForm(ctx, w.landlord, book, f.ID); err != nil {
		t.Fatal(err)
	}
	id := f.ID.String()
	if _, err := w.svc.PutIntake(ctx, w.landlord, w.ownL, IntakeInput{FormID: &id, Status: IntakeOpen}); err != nil {
		t.Fatal(err)
	}
	app, _, err := w.svc.Start(ctx, w.tenant, w.ownL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Autosave(ctx, w.tenant, app.ID, raw(map[string]any{"occupants": 1, "move_in": "2027-01-01", "has_pets": "no"})); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.svc.Upload(ctx, w.tenant, app.ID, "payslip", "p.pdf", pdf); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Submit(ctx, w.tenant, app.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := w.svc.ReceivedOne(ctx, w.landlord, book, app.ID); err != nil || got.Status != StatusSubmitted {
		t.Fatalf("direct owner reads %+v %v", got, err)
	}
	if _, err := w.svc.ReceivedOne(ctx, w.owner, w.org, app.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the agency sees the landlord's application: %v", err)
	}
	if len(w.audit.All()) != 0 {
		t.Fatal("a direct owner's read went to an organisation audit")
	}
}
