package tenancies

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// fakeLinks knows contacts (with emails) and leases per book.
type fakeLinks struct {
	contacts map[uuid.UUID]fakeContact
	leases   map[uuid.UUID]uuid.UUID // lease -> book ID
}

type fakeContact struct {
	book  uuid.UUID
	name  string
	email string
	dead  bool
	// personal: the book is a direct owner's.
	personal bool
}

func (f fakeLinks) Contact(_ context.Context, book Book, id uuid.UUID) (ContactInfo, bool, error) {
	c, ok := f.contacts[id]
	if !ok || c.dead || c.book != book.ID() {
		return ContactInfo{}, false, nil
	}
	return ContactInfo{ID: id, Book: book, Name: c.name}, true, nil
}

func (f fakeLinks) Listing(context.Context, Book, uuid.UUID) (ListingRef, bool, error) {
	return ListingRef{}, false, nil
}

func (f fakeLinks) Lease(_ context.Context, book Book, id uuid.UUID) (LeaseRef, bool, error) {
	if b, ok := f.leases[id]; ok && b == book.ID() {
		return LeaseRef{ID: id, Reference: "RPL-TEST"}, true, nil
	}
	return LeaseRef{}, false, nil
}

func (f fakeLinks) ContactsWithEmail(_ context.Context, email string) ([]ContactInfo, error) {
	var out []ContactInfo
	for id, c := range f.contacts {
		if !c.dead && c.email == email {
			book := OrganisationBook(c.book)
			if c.personal {
				book = PersonalBook(c.book)
			}
			out = append(out, ContactInfo{ID: id, Book: book, Name: c.name})
		}
	}
	return out, nil
}

type account struct {
	email    string
	verified bool
}

type fixture struct {
	t                                              *testing.T
	ctx                                            context.Context
	svc                                            *Service
	store                                          *MemoryStore
	audit                                          *orgaudit.MemoryLog
	now                                            time.Time
	org, other                                     uuid.UUID
	tenant, tenant2, landlord, foreign             uuid.UUID
	lease, foreignLease                            uuid.UUID
	accounts                                       map[uuid.UUID]account
	links                                          fakeLinks
	owner, agent, staff, outsider, consumer, admin *auth.Principal
}

func member(org uuid.UUID, role, name string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Name: name,
		Memberships: []auth.Membership{{OrganisationID: org, OrganisationName: "Test Estates", OrganisationKind: auth.KindAgency, Role: role}}}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, ctx: context.Background(), store: NewMemoryStore(), audit: &orgaudit.MemoryLog{},
		now: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC), org: uuid.New(), other: uuid.New(), tenant: uuid.New(), tenant2: uuid.New(),
		landlord: uuid.New(), foreign: uuid.New(), lease: uuid.New(), foreignLease: uuid.New(), accounts: map[uuid.UUID]account{}}
	f.store.SetAuditSink(f.audit.Append)
	f.links = fakeLinks{
		contacts: map[uuid.UUID]fakeContact{
			f.tenant:   {book: f.org, name: "Tia Tenant", email: "tia@example.test"},
			f.tenant2:  {book: f.org, name: "Tom Tenant", email: "tom@example.test"},
			f.landlord: {book: f.org, name: "Lars Landlord", email: "lars@example.test"},
			// The same email in another book: never makes the other book's tenancies visible.
			f.foreign: {book: f.other, name: "Tia Elsewhere", email: "tia@example.test"},
		},
		leases: map[uuid.UUID]uuid.UUID{f.lease: f.org, f.foreignLease: f.other},
	}
	f.svc = NewService(Deps{Store: f.store, Links: f.links,
		Currencies: func(context.Context) ([]string, error) { return []string{"USD", "ZAR", "JPY", "USD"}, nil },
		Markets:    func(context.Context) ([]string, error) { return []string{"ZW", "GB"}, nil },
		Account: func(_ context.Context, id uuid.UUID) (string, bool, error) {
			a := f.accounts[id]
			return a.email, a.verified, nil
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return f.now }})
	f.owner, f.agent, f.staff = member(f.org, auth.RoleOwner, "Olive Owner"), member(f.org, auth.RoleAgent, "Ada Agent"), member(f.org, auth.RoleStaff, "Sam Staff")
	f.outsider = member(f.other, auth.RoleOwner, "Other Owner")
	f.consumer = &auth.Principal{UserID: uuid.New(), Name: "Tia"}
	f.admin = &auth.Principal{UserID: uuid.New(), SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformSuperAdmin}}}
	return f
}

// body decodes JSON into an input, as the handler does.
func body[T any](t *testing.T, raw string) T {
	t.Helper()
	var v T
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return v
}

func fields(t *testing.T, err error) map[string]string {
	t.Helper()
	var v *ValidationError
	if !errors.As(err, &v) {
		t.Fatalf("want a validation error, got %v", err)
	}
	return v.Fields
}

func (f *fixture) property(units ...string) PropertyView {
	f.t.Helper()
	list := make([]string, 0, len(units))
	for _, u := range units {
		list = append(list, `{"label":"`+u+`"}`)
	}
	in := body[PropertyInput](f.t, `{"name":"Avondale Court","address":"12 King George Road","market_code":"zw","units":[`+strings.Join(list, ",")+`]}`)
	p, err := f.svc.CreateProperty(f.ctx, f.agent, f.org, in)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) tenancyBody(propertyID uuid.UUID, unitID *uuid.UUID, extra string) TenancyInput {
	unit := "null"
	if unitID != nil {
		unit = `"` + unitID.String() + `"`
	}
	return body[TenancyInput](f.t, `{"property_id":"`+propertyID.String()+`","unit_id":`+unit+`,"tenants":["`+f.tenant.String()+`"],
		"rent":{"amount":"950","currency":"usd","frequency":"MONTHLY","due_day":1},"start_date":"2026-12-01"`+extra+`}`)
}

func TestPropertiesAndUnits(t *testing.T) {
	f := newFixture(t)
	p := f.property("Flat 1", "Flat 2")
	if p.MarketCode != "ZW" || p.UnitCount != 2 || p.LettableUnits != 2 || p.OccupiedUnits != 0 || !p.Can.Delete || p.Units[1].Position != 2 {
		t.Fatalf("created: %+v", p)
	}
	whole := f.property()
	if whole.UnitCount != 0 || whole.LettableUnits != 1 {
		t.Fatalf("a property with no units is one unit: %+v", whole)
	}
	// Validation.
	bad := body[PropertyInput](t, `{"name":"","address":"x","market_code":"XX","units":[{"label":"A"},{"label":"a"}]}`)
	got := fields(t, func() error { _, err := f.svc.CreateProperty(f.ctx, f.agent, f.org, bad); return err }())
	if got["name"] == "" || got["market_code"] == "" || got["units[1].label"] == "" {
		t.Fatalf("fields: %v", got)
	}
	// Units: add, rename (unique, case-insensitive), archive.
	if _, err := f.svc.AddUnit(f.ctx, f.agent, f.org, p.ID, UnitInput{Label: "flat 1"}); fields(t, err)["label"] == "" {
		t.Fatal("duplicate label accepted")
	}
	p, err := f.svc.AddUnit(f.ctx, f.agent, f.org, p.ID, UnitInput{Label: "Flat 3"})
	if err != nil || p.UnitCount != 3 || p.Units[2].Position != 3 {
		t.Fatalf("add: %+v %v", p, err)
	}
	p, err = f.svc.RenameUnit(f.ctx, f.agent, f.org, p.ID, p.Units[2].ID, UnitInput{Label: "Flat 3A"})
	if err != nil || p.Units[2].Label != "Flat 3A" {
		t.Fatalf("rename: %+v %v", p, err)
	}
	p, err = f.svc.DeleteUnit(f.ctx, f.agent, f.org, p.ID, p.Units[2].ID)
	if err != nil || p.UnitCount != 2 {
		t.Fatalf("archive unit: %+v %v", p, err)
	}
	if _, err := f.svc.RenameUnit(f.ctx, f.agent, f.org, p.ID, uuid.New(), UnitInput{Label: "x"}); !errors.Is(err, ErrUnitNotFound) {
		t.Fatalf("unknown unit: %v", err)
	}
	// Patch; units are refused there.
	patched, err := f.svc.UpdateProperty(f.ctx, f.agent, f.org, p.ID, body[PropertyInput](t, `{"name":"Avondale Court East","notes":"Gate code at the office"}`))
	if err != nil || patched.Name != "Avondale Court East" || patched.Notes != "Gate code at the office" || patched.Address != "12 King George Road" {
		t.Fatalf("patch: %+v %v", patched, err)
	}
	if _, err := f.svc.UpdateProperty(f.ctx, f.agent, f.org, p.ID, body[PropertyInput](t, `{"units":[]}`)); fields(t, err)["units"] == "" {
		t.Fatal("units accepted in a PATCH")
	}
	// Lists.
	page, err := f.svc.ListProperties(f.ctx, f.staff, f.org, PropertyFilter{Q: "east"}, 1, 25)
	if err != nil || page.Total != 1 || page.Items[0].ID != p.ID || page.Items[0].Can.Edit {
		t.Fatalf("list: %+v %v", page, err)
	}
	// Rights: STAFF read only; another organisation, a consumer and an administrator see no book.
	if _, err := f.svc.CreateProperty(f.ctx, f.staff, f.org, PropertyInput{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff: %v", err)
	}
	for _, who := range []*auth.Principal{f.outsider, f.consumer, f.admin} {
		if _, err := f.svc.GetProperty(f.ctx, who, f.org, p.ID); !errors.Is(err, ErrBookNotFound) {
			t.Fatalf("outsider: %v", err)
		}
	}
	// The outsider's own book does not reach it.
	if _, err := f.svc.GetProperty(f.ctx, f.outsider, f.other, p.ID); !errors.Is(err, ErrPropertyNotFound) {
		t.Fatalf("other book: %v", err)
	}
	// Archive.
	if err := f.svc.DeleteProperty(f.ctx, f.agent, f.org, whole.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GetProperty(f.ctx, f.agent, f.org, whole.ID); !errors.Is(err, ErrPropertyNotFound) {
		t.Fatalf("archived: %v", err)
	}
}

func TestTenancyLifecycle(t *testing.T) {
	f := newFixture(t)
	p := f.property("Flat 1", "Flat 2")
	flat1 := p.Units[0].ID
	// Validation: unit required, parties of the book, lease of the book, deposit currency, dates.
	in := f.tenancyBody(p.ID, nil, `,"landlords":["`+f.foreign.String()+`"],"lease_id":"`+f.foreignLease.String()+`",
		"deposit":{"amount":"1.234","currency":"ZAR"},"end_date":"2026-11-30","status":"ACTIVE"`)
	got := fields(t, func() error { _, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in); return err }())
	for _, k := range []string{"status"} {
		if got[k] == "" {
			t.Fatalf("%s not refused: %v", k, got)
		}
	}
	in = f.tenancyBody(p.ID, nil, `,"landlords":["`+f.foreign.String()+`"],"lease_id":"`+f.foreignLease.String()+`",
		"deposit":{"amount":"1.234","currency":"ZAR"},"end_date":"2026-11-30"`)
	got = fields(t, func() error { _, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in); return err }())
	for _, k := range []string{"unit_id", "landlords[0]", "lease_id", "deposit.currency", "end_date"} {
		if got[k] == "" {
			t.Fatalf("%s not refused: %v", k, got)
		}
	}
	in = f.tenancyBody(p.ID, &flat1, `,"tenants":[],"rent":{"amount":"-1","currency":"XYZ","frequency":"DAILY","due_day":9}`)
	got = fields(t, func() error { _, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in); return err }())
	for _, k := range []string{"tenants", "rent.currency", "rent.frequency"} {
		if got[k] == "" {
			t.Fatalf("%s not refused: %v", k, got)
		}
	}
	in = f.tenancyBody(p.ID, &flat1, `,"rent":{"amount":"10","currency":"USD","frequency":"WEEKLY","due_day":8},"landlords":["`+f.tenant.String()+`"]`)
	got = fields(t, func() error { _, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in); return err }())
	if got["rent.due_day"] == "" || got["landlords"] == "" {
		t.Fatalf("weekly due day / tenant as landlord: %v", got)
	}
	in = f.tenancyBody(p.ID, &flat1, `,"rent":{"amount":"10.5","currency":"JPY","frequency":"MONTHLY","due_day":31}`)
	if got := fields(t, func() error { _, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in); return err }()); got["rent.amount"] == "" {
		t.Fatalf("JPY decimals: %v", got)
	}

	// Create (DRAFT), audited.
	in = f.tenancyBody(p.ID, &flat1, `,"landlords":["`+f.landlord.String()+`"],"lease_id":"`+f.lease.String()+`",
		"deposit":{"amount":"950.5","currency":"USD"},"end_date":"2027-11-30","notes":"Keys at the office"`)
	ten, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, in)
	if err != nil {
		t.Fatal(err)
	}
	if ten.Status != StatusDraft || ten.Rent.Amount != "950.00" || ten.Rent.Currency != "USD" || ten.Deposit == nil || ten.Deposit.Amount != "950.50" ||
		ten.Unit == nil || ten.Unit.Label != "Flat 1" || len(ten.Tenants) != 1 || !ten.Tenants[0].Live || ten.Tenants[0].Name != "Tia Tenant" ||
		len(ten.Landlords) != 1 || ten.Lease == nil || ten.Lease.Reference != "RPL-TEST" || !ten.Can.Activate || !ten.Can.Delete || ten.Can.End {
		t.Fatalf("created: %+v", ten)
	}
	if n := len(f.audit.All()); n != 1 || f.audit.All()[0].Action != orgaudit.ActionTenancyCreated || f.audit.All()[0].TargetLabel != "Avondale Court Flat 1" {
		t.Fatalf("audit: %+v", f.audit.All())
	}
	// The property cannot be archived nor its unit while a tenancy is open.
	if err := f.svc.DeleteProperty(f.ctx, f.agent, f.org, p.ID); !errors.Is(err, ErrPropertyInUse) {
		t.Fatalf("archive in use: %v", err)
	}
	if _, err := f.svc.DeleteUnit(f.ctx, f.agent, f.org, p.ID, flat1); !errors.Is(err, ErrUnitInUse) {
		t.Fatalf("archive unit in use: %v", err)
	}
	// A draft is fully editable; null clears.
	ten, err = f.svc.UpdateTenancy(f.ctx, f.agent, f.org, ten.ID, body[TenancyInput](t, `{"deposit":null,"rent":{"amount":"1000","currency":"USD","frequency":"QUARTERLY","due_day":15}}`))
	if err != nil || ten.Deposit != nil || ten.Rent.Frequency != FrequencyQuarterly || ten.Rent.Amount != "1000.00" {
		t.Fatalf("draft patch: %+v %v", ten, err)
	}
	if _, err := f.svc.UpdateTenancy(f.ctx, f.agent, f.org, ten.ID, body[TenancyInput](t, `{"status":"ACTIVE"}`)); fields(t, err)["status"] == "" {
		t.Fatal("status patched")
	}

	// Activate (audited); a second tenancy of the same unit cannot be activated.
	ten, err = f.svc.Activate(f.ctx, f.agent, f.org, ten.ID)
	if err != nil || ten.Status != StatusActive || ten.ActivatedAt == nil || ten.ActivatedBy == nil || ten.ActivatedBy.Name != "Ada Agent" || !ten.Can.End {
		t.Fatalf("activate: %+v %v", ten, err)
	}
	if _, err := f.svc.Activate(f.ctx, f.agent, f.org, ten.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("activate twice: %v", err)
	}
	second, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, f.tenancyBody(p.ID, &flat1, ""))
	if err != nil {
		t.Fatal(err)
	}
	var occupied *OccupiedError
	if _, err := f.svc.Activate(f.ctx, f.agent, f.org, second.ID); !errors.As(err, &occupied) || occupied.TenancyID == nil || *occupied.TenancyID != ten.ID {
		t.Fatalf("occupied: %v", err)
	}
	// The store refuses it too (the race the unique index catches).
	raw, _ := f.store.Tenancy(f.ctx, second.ID)
	raw.Status, raw.ActivatedAt = StatusActive, &f.now
	if err := f.store.Apply(f.ctx, Change{Tenancies: []Tenancy{raw}}); !errors.Is(err, ErrUnitOccupied) {
		t.Fatalf("store occupied: %v", err)
	}
	if err := f.svc.DeleteTenancy(f.ctx, f.agent, f.org, second.ID); err != nil {
		t.Fatalf("delete draft: %v", err)
	}
	if _, err := f.svc.GetTenancy(f.ctx, f.agent, f.org, second.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	prop, _ := f.svc.GetProperty(f.ctx, f.agent, f.org, p.ID)
	if prop.OccupiedUnits != 1 || prop.Units[0].ActiveTenancyID == nil || *prop.Units[0].ActiveTenancyID != ten.ID || prop.OpenTenancies != 1 || prop.Can.Delete {
		t.Fatalf("occupancy: %+v", prop)
	}

	// Active: only some fields change.
	got = fields(t, func() error {
		_, err := f.svc.UpdateTenancy(f.ctx, f.agent, f.org, ten.ID, body[TenancyInput](t, `{"start_date":"2026-12-02","rent":{"amount":"1","currency":"USD","frequency":"MONTHLY","due_day":1}}`))
		return err
	}())
	if !strings.Contains(got["start_date"], "fixed") || !strings.Contains(got["rent"], "fixed") {
		t.Fatalf("active fixed fields: %v", got)
	}
	ten, err = f.svc.UpdateTenancy(f.ctx, f.agent, f.org, ten.ID, body[TenancyInput](t, `{"tenants":["`+f.tenant.String()+`","`+f.tenant2.String()+`"],"end_date":"2027-12-31"}`))
	if err != nil || len(ten.Tenants) != 2 || len(ten.Landlords) != 1 || *ten.EndDate != "2027-12-31" {
		t.Fatalf("active patch: %+v %v", ten, err)
	}
	if err := f.svc.DeleteTenancy(f.ctx, f.agent, f.org, ten.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("delete active: %v", err)
	}

	// End before the agreed end date: TERMINATED (audited).
	if _, err := f.svc.End(f.ctx, f.agent, f.org, ten.ID, EndInput{EndDate: "2026-11-01", Reason: "x"}); fields(t, err)["end_date"] == "" {
		t.Fatal("ended before the start")
	}
	if _, err := f.svc.End(f.ctx, f.agent, f.org, ten.ID, EndInput{EndDate: "2027-06-30"}); fields(t, err)["reason"] == "" {
		t.Fatal("no reason accepted")
	}
	ten, err = f.svc.End(f.ctx, f.agent, f.org, ten.ID, EndInput{EndDate: "2027-06-30", Reason: "Tenant relocating"})
	if err != nil || ten.Status != StatusTerminated || *ten.EndedOn != "2027-06-30" || *ten.EndDate != "2027-12-31" || *ten.EndReason != "Tenant relocating" ||
		ten.EndedBy == nil || ten.Can.End || ten.Can.Activate {
		t.Fatalf("end: %+v %v", ten, err)
	}
	if _, err := f.svc.End(f.ctx, f.agent, f.org, ten.ID, EndInput{EndDate: "2027-06-30", Reason: "again"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("end twice: %v", err)
	}
	// Ended: notes only.
	if _, err := f.svc.UpdateTenancy(f.ctx, f.agent, f.org, ten.ID, body[TenancyInput](t, `{"tenants":["`+f.tenant.String()+`"]}`)); fields(t, err)["tenants"] == "" {
		t.Fatal("ended tenancy's tenants changed")
	}
	if ten, err = f.svc.UpdateTenancy(f.ctx, f.agent, f.org, ten.ID, body[TenancyInput](t, `{"notes":"Deposit returned"}`)); err != nil || ten.Notes != "Deposit returned" {
		t.Fatalf("ended notes: %+v %v", ten, err)
	}
	actions := []string{}
	for _, e := range f.audit.All() {
		actions = append(actions, e.Action)
	}
	want := []string{orgaudit.ActionTenancyCreated, orgaudit.ActionTenancyActivated, orgaudit.ActionTenancyCreated, orgaudit.ActionTenancyDeleted, orgaudit.ActionTenancyEnded}
	if strings.Join(actions, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions: %v", actions)
	}
	// Ended without an agreed end date (or on it): ENDED. The unit is free again.
	third, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, f.tenancyBody(p.ID, &flat1, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Activate(f.ctx, f.agent, f.org, third.ID); err != nil {
		t.Fatal(err)
	}
	if third, err = f.svc.End(f.ctx, f.agent, f.org, third.ID, EndInput{EndDate: "2028-01-31", Reason: "Notice given"}); err != nil || third.Status != StatusEnded {
		t.Fatalf("periodic end: %+v %v", third, err)
	}
	// Lists and counts.
	page, err := f.svc.ListTenancies(f.ctx, f.staff, f.org, TenancyFilter{Status: StatusEnded}, 1, 25)
	if err != nil || page.Total != 1 || page.StatusCounts[StatusTerminated] != 1 || page.StatusCounts[StatusEnded] != 1 || page.StatusCounts[StatusDraft] != 0 {
		t.Fatalf("list: %+v %v", page, err)
	}
	if page, _ := f.svc.ListTenancies(f.ctx, f.staff, f.org, TenancyFilter{ContactID: &f.tenant2}, 1, 25); page.Total != 1 {
		t.Fatalf("contact filter: %+v", page)
	}
	// STAFF read only, outsiders nothing.
	if _, err := f.svc.Activate(f.ctx, f.staff, f.org, third.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff activate: %v", err)
	}
	if _, err := f.svc.GetTenancy(f.ctx, f.outsider, f.org, third.ID); !errors.Is(err, ErrBookNotFound) {
		t.Fatalf("outsider: %v", err)
	}
	if _, err := f.svc.GetTenancy(f.ctx, f.outsider, f.other, third.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("other book: %v", err)
	}
}

// A tenant sees their own tenancies only: verified email, a tenant contact
// of the tenancy's own book, never a DRAFT.
func TestTenantView(t *testing.T) {
	f := newFixture(t)
	p := f.property()
	active, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, f.tenancyBody(p.ID, nil, `,"landlords":["`+f.landlord.String()+`"],"notes":"internal"`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Activate(f.ctx, f.agent, f.org, active.ID); err != nil {
		t.Fatal(err)
	}
	q := f.property("Unit A")
	draft, err := f.svc.CreateTenancy(f.ctx, f.agent, f.org, f.tenancyBody(q.ID, &q.Units[0].ID, ""))
	if err != nil {
		t.Fatal(err)
	}
	tia := &auth.Principal{UserID: uuid.New(), Name: "Tia"}
	f.accounts[tia.UserID] = account{email: "Tia@Example.test ", verified: true}
	list, err := f.svc.MyTenancies(f.ctx, tia)
	if err != nil || len(list) != 1 || list[0].ID != active.ID {
		t.Fatalf("mine: %+v %v", list, err)
	}
	mv := list[0]
	if mv.ManagedBy.Kind != ManagerOrganisation || mv.ManagedBy.Name != "Test Estates" || mv.Property.Name != "Avondale Court" || mv.Unit != nil ||
		len(mv.Tenants) != 1 || mv.Tenants[0].Name != "Tia Tenant" || len(mv.Landlords) != 1 || mv.Rent.Amount != "950.00" {
		t.Fatalf("view: %+v", mv)
	}
	raw, _ := json.Marshal(mv)
	for _, leak := range []string{"internal", f.tenant.String(), "lars@example.test", "Ada Agent", `"can"`, `"notes"`} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("tenant view leaks %q: %s", leak, raw)
		}
	}
	if _, err := f.svc.MyTenancy(f.ctx, tia, active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.MyTenancy(f.ctx, tia, draft.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("draft shown: %v", err)
	}
	// Unverified: nothing.
	f.accounts[tia.UserID] = account{email: "tia@example.test", verified: false}
	if list, err := f.svc.MyTenancies(f.ctx, tia); err != nil || len(list) != 0 {
		t.Fatalf("unverified: %+v %v", list, err)
	}
	if _, err := f.svc.MyTenancy(f.ctx, tia, active.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("unverified one: %v", err)
	}
	// Another tenant: 404.
	tom := &auth.Principal{UserID: uuid.New()}
	f.accounts[tom.UserID] = account{email: "tom@example.test", verified: true}
	if _, err := f.svc.MyTenancy(f.ctx, tom, active.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("other tenant: %v", err)
	}
	// A landlord contact with the email is not a tenant.
	lars := &auth.Principal{UserID: uuid.New()}
	f.accounts[lars.UserID] = account{email: "lars@example.test", verified: true}
	if list, _ := f.svc.MyTenancies(f.ctx, lars); len(list) != 0 {
		t.Fatalf("landlord sees: %+v", list)
	}
	// A tenancy of another book whose tenant contact has the same email
	// stays the other book's; the contact of THIS book is what matches.
	other := Tenancy{ID: newID(), Book: OrganisationBook(f.other), PropertyID: p.ID, Status: StatusActive, StartDate: "2026-01-01",
		RentAmount: "1.00", RentCurrency: "USD", RentFrequency: FrequencyMonthly, RentDueDay: 1,
		Parties: []Party{{ContactID: f.tenant, Role: RoleTenant, Name: "Tia"}}}
	f.store.Put(Change{Tenancies: []Tenancy{other}})
	f.accounts[tia.UserID] = account{email: "tia@example.test", verified: true}
	if list, _ := f.svc.MyTenancies(f.ctx, tia); len(list) != 1 {
		t.Fatalf("cross-book match: %+v", list)
	}
	// Signed out.
	if _, err := f.svc.MyTenancies(f.ctx, nil); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("signed out: %v", err)
	}
	// A deleted contact no longer matches.
	c := f.links.contacts[f.tenant]
	c.dead = true
	f.links.contacts[f.tenant] = c
	if list, _ := f.svc.MyTenancies(f.ctx, tia); len(list) != 0 {
		t.Fatalf("deleted contact: %+v", list)
	}
}

// A direct owner's personal book; account deletion handling.
func TestPersonalBookAndForgetUser(t *testing.T) {
	f := newFixture(t)
	owner := &auth.Principal{UserID: uuid.New(), Name: "Lan Lord", AccountRoles: []auth.AccountRole{{Role: auth.AccountRoleLandlord, Status: auth.AccountRoleActive}}}
	f.links.contacts[f.tenant2] = fakeContact{book: owner.UserID, name: "Tom Tenant", email: "tom@example.test", personal: true}
	in := body[PropertyInput](t, `{"name":"Cottage","address":"1 Lane","market_code":"GB"}`)
	p, err := f.svc.CreateProperty(f.ctx, owner, owner.UserID, in)
	if err != nil {
		t.Fatal(err)
	}
	mk := func() TenancyView {
		tv, err := f.svc.CreateTenancy(f.ctx, owner, owner.UserID, body[TenancyInput](t, `{"property_id":"`+p.ID.String()+`","tenants":["`+f.tenant2.String()+`"],
			"rent":{"amount":"1200","currency":"USD","frequency":"MONTHLY","due_day":1},"start_date":"2026-12-01"}`))
		if err != nil {
			t.Fatal(err)
		}
		return tv
	}
	active, draft := mk(), mk()
	if _, err := f.svc.Activate(f.ctx, owner, owner.UserID, active.ID); err != nil {
		t.Fatal(err)
	}
	if len(f.audit.All()) != 0 {
		t.Fatal("a personal book has no organisation audit trail")
	}
	tom := &auth.Principal{UserID: uuid.New()}
	f.accounts[tom.UserID] = account{email: "tom@example.test", verified: true}
	if list, _ := f.svc.MyTenancies(f.ctx, tom); len(list) != 1 || list[0].ManagedBy.Kind != ManagerDirectOwner || list[0].ManagedBy.Name != "Lan Lord" {
		t.Fatalf("personal tenant view: %+v", list)
	}
	f.store.ForgetUser(owner.UserID, "Deleted account")
	if _, err := f.store.Tenancy(f.ctx, draft.ID); !errors.Is(err, ErrTenancyNotFound) {
		t.Fatalf("draft kept: %v", err)
	}
	got, err := f.store.Tenancy(f.ctx, active.ID)
	if err != nil || got.BookName != "Deleted account" || got.CreatedBy != nil || got.ActivatedBy != nil {
		t.Fatalf("forgotten: %+v %v", got, err)
	}
	if prop, _ := f.store.Property(f.ctx, p.ID); prop.CreatedBy != nil {
		t.Fatal("property creator kept")
	}
}
