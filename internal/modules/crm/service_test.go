package crm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeItems struct{ items []Item }

func (f *fakeItems) Item(_ context.Context, kind string, id uuid.UUID) (Item, error) {
	for _, it := range f.items {
		if it.Kind == kind && it.ID == id {
			return it, nil
		}
	}
	return Item{}, ErrItemNotFound
}

func (f *fakeItems) Candidates(_ context.Context, book Book, emails, tails []string) ([]Item, error) {
	var out []Item
	for _, it := range f.items {
		b, ok := it.Book()
		if !ok || !b.Same(book) {
			continue
		}
		d := digits(it.Phone)
		if len(d) > 7 {
			d = d[len(d)-7:]
		}
		if slices.Contains(emails, strings.ToLower(strings.TrimSpace(it.Email))) || (it.AccountEmail != "" && slices.Contains(emails, it.AccountEmail)) ||
			(d != "" && slices.Contains(tails, d)) {
			out = append(out, it)
		}
	}
	return out, nil
}

type fakePeople struct {
	members map[uuid.UUID]map[uuid.UUID]string
}

func (f fakePeople) Assignable(_ context.Context, org, user uuid.UUID) (string, bool, error) {
	n, ok := f.members[org][user]
	return n, ok, nil
}

func (f fakePeople) Name(_ context.Context, user uuid.UUID) (string, error) {
	for _, m := range f.members {
		if n, ok := m[user]; ok {
			return n, nil
		}
	}
	return "", nil
}

type fakeVocab struct{}

func (fakeVocab) Labels(_ context.Context, set string) (map[string]string, error) {
	switch set {
	case SetContactTypes:
		return map[string]string{"buyer": "Buyer", "tenant": "Tenant", "landlord": "Landlord", "owner": "Owner", "other": "Other"}, nil
	case SetContactSources:
		return map[string]string{"enquiry": "Enquiry", "walk-in": "Walk-in", "referral": "Referral", "phone": "Phone call", "import": "Import",
			"website": "Website", "other": "Other"}, nil
	}
	return nil, fmt.Errorf("unknown set %s", set)
}

type fakeListings struct{ refs []ListingRef }

func (f fakeListings) Listings(_ context.Context, ids []uuid.UUID) ([]ListingRef, error) {
	var out []ListingRef
	for _, r := range f.refs {
		if slices.Contains(ids, r.ID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f fakeListings) OwnerMarket(_ context.Context, user uuid.UUID) (string, error) {
	for _, r := range f.refs {
		if r.SellerUserID != nil && *r.SellerUserID == user {
			return r.MarketCode, nil
		}
	}
	return "", nil
}

// world is one service with two agencies and their people.
type world struct {
	t                                    *testing.T
	svc                                  *Service
	store                                *MemoryStore
	items                                *fakeItems
	audit                                *orgaudit.MemoryLog
	now                                  time.Time
	org, other                           uuid.UUID
	owner, manager, agent, agent2, staff *auth.Principal
	outsider, consumer, admin, landlord  *auth.Principal
	listing, otherListing                uuid.UUID
}

func member(name string, org uuid.UUID, role string) *auth.Principal {
	return &auth.Principal{UserID: uuid.New(), Name: name, SessionScope: auth.ScopeCustomer,
		Memberships: []auth.Membership{{OrganisationID: org, OrganisationKind: auth.KindAgency, MarketCode: "ZW", Role: role}}}
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, store: NewMemoryStore(), items: &fakeItems{}, audit: &orgaudit.MemoryLog{},
		now: time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC), org: uuid.New(), other: uuid.New()}
	w.store.SetAuditSink(w.audit.Append)
	w.owner, w.manager = member("Rudo", w.org, auth.RoleOwner), member("Ruvimbo", w.org, auth.RoleManager)
	w.agent, w.agent2, w.staff = member("Tendai", w.org, auth.RoleAgent), member("Chipo", w.org, auth.RoleAgent), member("Kuda", w.org, auth.RoleStaff)
	w.outsider = member("Blake", w.other, auth.RoleOwner)
	w.consumer = &auth.Principal{UserID: uuid.New(), Name: "Casey", SessionScope: auth.ScopeCustomer}
	w.admin = &auth.Principal{UserID: uuid.New(), Name: "Sam", SessionScope: auth.ScopeAdmin, PlatformRoles: []auth.PlatformRole{{Role: auth.PlatformSuperAdmin}}}
	w.landlord = &auth.Principal{UserID: uuid.New(), Name: "Lloyd", SessionScope: auth.ScopeCustomer,
		AccountRoles: []auth.AccountRole{{Role: auth.AccountRoleLandlord, Status: auth.AccountRoleActive}}}
	people := fakePeople{members: map[uuid.UUID]map[uuid.UUID]string{
		w.org:   {w.owner.UserID: "Rudo", w.manager.UserID: "Ruvimbo", w.agent.UserID: "Tendai", w.agent2.UserID: "Chipo"},
		w.other: {w.outsider.UserID: "Blake"},
	}}
	w.listing, w.otherListing = uuid.New(), uuid.New()
	org, other := w.org, w.other
	ll := w.landlord.UserID
	listings := fakeListings{refs: []ListingRef{
		{ID: w.listing, Title: "Flat in Avondale", Reference: "RP-ZW-1", Slug: "flat-in-avondale", OrganisationID: &org, MarketCode: "ZW"},
		{ID: w.otherListing, Title: "Second Street house", Reference: "RP-ZW-2", OrganisationID: &other, MarketCode: "ZW"},
		{ID: uuid.New(), Title: "Lloyd's cottage", Reference: "RP-GB-3", SellerUserID: &ll, MarketCode: "GB"},
	}}
	w.svc = NewService(Deps{Store: w.store, Items: w.items, Listings: listings, People: people, Vocabulary: fakeVocab{},
		Now: func() time.Time { return w.now }})
	return w
}

func (w *world) create(p *auth.Principal, in CreateInput) ContactBody {
	w.t.Helper()
	got, err := w.svc.Create(context.Background(), p, w.org, in, false)
	if err != nil {
		w.t.Fatalf("create %s: %v", in.Name, err)
	}
	return got
}

func (w *world) addItem(kind string, org uuid.UUID, email, phone string, at time.Time) Item {
	o := org
	it := Item{Kind: kind, ID: uuid.New(), Reference: fmt.Sprintf("REF-%d", len(w.items.items)+1), Type: "VIEWING", Status: "NEW", Title: "Flat",
		OrganisationID: &o, MarketCode: "ZW", Name: "Customer", Email: email, Phone: phone, CreatedAt: at}
	w.items.items = append(w.items.items, it)
	return it
}

func strp(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// Who may do what, end to end through the service: OWNER, MANAGER, AGENT,
// STAFF of the organisation, a member of another organisation, a consumer
// and a platform administrator.
func TestCapabilityMatrix(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	c := w.create(w.owner, CreateInput{Name: "Seed", Type: "buyer", Emails: []string{"seed@example.com"}})
	type want struct{ read, write, manage error }
	ok := want{}
	cases := []struct {
		name string
		p    *auth.Principal
		want want
	}{
		{"owner", w.owner, ok},
		{"manager", w.manager, ok},
		{"agent", w.agent, want{nil, nil, ErrForbidden}},
		{"staff", w.staff, want{nil, ErrForbidden, ErrForbidden}},
		{"another organisation", w.outsider, want{ErrNotFound, ErrNotFound, ErrNotFound}},
		{"consumer", w.consumer, want{ErrNotFound, ErrNotFound, ErrNotFound}},
		{"administrator", w.admin, want{ErrNotFound, ErrNotFound, ErrNotFound}},
	}
	for i, tc := range cases {
		_, err := w.svc.List(ctx, tc.p, w.org, ListFilter{}, 1, 25)
		if !errors.Is(err, tc.want.read) {
			t.Errorf("%s: list = %v, want %v", tc.name, err, tc.want.read)
		}
		if _, err := w.svc.Get(ctx, tc.p, w.org, c.Contact.ID); !errors.Is(err, tc.want.read) {
			t.Errorf("%s: get = %v", tc.name, err)
		}
		if _, err := w.svc.Timeline(ctx, tc.p, w.org, c.Contact.ID); !errors.Is(err, tc.want.read) {
			t.Errorf("%s: timeline = %v", tc.name, err)
		}
		_, err = w.svc.Create(ctx, tc.p, w.org, CreateInput{Name: fmt.Sprintf("Person %d", i), Type: "buyer"}, false)
		if !errors.Is(err, tc.want.write) {
			t.Errorf("%s: create = %v, want %v", tc.name, err, tc.want.write)
		}
		if _, err := w.svc.AddNote(ctx, tc.p, w.org, c.Contact.ID, "A note"); !errors.Is(err, tc.want.write) {
			t.Errorf("%s: note = %v", tc.name, err)
		}
		if _, err := w.svc.ChangeStage(ctx, tc.p, w.org, c.Contact.ID, StageContacted, ""); !errors.Is(err, tc.want.write) {
			t.Errorf("%s: stage = %v", tc.name, err)
		}
		if _, err := w.svc.Export(ctx, tc.p, w.org); !errors.Is(err, tc.want.manage) {
			t.Errorf("%s: export = %v, want %v", tc.name, err, tc.want.manage)
		}
		if _, err := w.svc.Import(ctx, tc.p, w.org, []byte("name\nX\n"), true); !errors.Is(err, tc.want.manage) {
			t.Errorf("%s: import = %v, want %v", tc.name, err, tc.want.manage)
		}
	}

	// A direct owner's personal book: theirs only, no import or export.
	book := w.landlord.UserID
	got, err := w.svc.Create(ctx, w.landlord, book, CreateInput{Name: "Tenant of the cottage", Type: "tenant", Phones: []string{"07700 900123"}}, false)
	if err != nil || got.Contact.OwnerUserID == nil || *got.Contact.OwnerUserID != book || got.Contact.Phones[0] != "+447700900123" {
		t.Fatalf("personal book: %+v %v", got.Contact, err)
	}
	if _, err := w.svc.Export(ctx, w.landlord, book); !errors.Is(err, ErrForbidden) {
		t.Fatalf("personal export = %v", err)
	}
	for _, p := range []*auth.Principal{w.owner, w.consumer} {
		if _, err := w.svc.List(ctx, p, book, ListFilter{}, 1, 25); !errors.Is(err, ErrNotFound) {
			t.Fatalf("someone else's personal book = %v", err)
		}
	}
	if _, err := w.svc.Get(ctx, w.landlord, book, c.Contact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an organisation's contact through a personal book = %v", err)
	}
}

func TestDuplicatesAndNormalisation(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	first := w.create(w.agent, CreateInput{Name: "Tariro Moyo", Type: "tenant", Emails: []string{" Tariro@Example.com "}, Phones: []string{"077 123 4567"}})
	if !slices.Equal(first.Contact.Emails, []string{"tariro@example.com"}) || !slices.Equal(first.Contact.Phones, []string{"+263771234567"}) {
		t.Fatalf("normalised = %v %v", first.Contact.Emails, first.Contact.Phones)
	}
	if first.Contact.OwnerUserID == nil || *first.Contact.OwnerUserID != w.agent.UserID {
		t.Fatal("the creator owns the contact by default")
	}
	for _, in := range []CreateInput{
		{Name: "Tariro again", Type: "tenant", Emails: []string{"TARIRO@example.COM"}},
		{Name: "Tariro by phone", Type: "tenant", Phones: []string{"+263 77 123 4567"}},
	} {
		_, err := w.svc.Create(ctx, w.agent, w.org, in, false)
		var dup *DuplicateError
		if !errors.As(err, &dup) || len(dup.Contacts) != 1 || dup.Contacts[0].ID != first.Contact.ID || dup.Contacts[0].Name != "Tariro Moyo" {
			t.Fatalf("%s: %v", in.Name, err)
		}
		if got, err := w.svc.Create(ctx, w.agent, w.org, in, true); err != nil || got.Contact.ID == first.Contact.ID {
			t.Fatalf("allow_duplicate: %v", err)
		}
	}
	// Another organisation's book is never a duplicate.
	if _, err := w.svc.Create(ctx, w.outsider, w.other, CreateInput{Name: "Tariro", Type: "tenant", Emails: []string{"tariro@example.com"}}, false); err != nil {
		t.Fatalf("another organisation: %v", err)
	}
	// Editing into a duplicate is refused too.
	second := w.create(w.agent, CreateInput{Name: "Farai", Type: "buyer"})
	emails := []string{"tariro@example.com"}
	var dup *DuplicateError
	if _, err := w.svc.Patch(ctx, w.agent, w.org, second.Contact.ID, PatchInput{Emails: &emails}, false); !errors.As(err, &dup) {
		t.Fatalf("patch into a duplicate: %v", err)
	}
	// Validation.
	_, err := w.svc.Create(ctx, w.agent, w.org, CreateInput{Name: " ", Type: "alien", Emails: []string{"nope"}, Phones: []string{"12"}, Stage: strp("WON")}, false)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("validation: %v", err)
	}
	for _, k := range []string{"name", "type", "emails[0]", "phones[0]", "stage"} {
		if verr.Fields[k] == "" {
			t.Errorf("no error for %s: %v", k, verr.Fields)
		}
	}
	// An agent may not give a new contact to a colleague; a manager may.
	colleague := w.agent2.UserID
	if _, err := w.svc.Create(ctx, w.agent, w.org, CreateInput{Name: "X", Type: "buyer", OwnerUserID: &colleague}, false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("agent assigns a colleague: %v", err)
	}
	if got, err := w.svc.Create(ctx, w.manager, w.org, CreateInput{Name: "Y", Type: "buyer", OwnerUserID: &colleague}, false); err != nil || *got.Contact.OwnerUserID != colleague {
		t.Fatalf("manager assigns: %v", err)
	}
}

func TestMergePreservesEverything(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	appt := w.addItem(KindAppointment, w.org, "t.marufu@example.com", "", w.now.Add(-time.Hour))
	keep := w.create(w.agent, CreateInput{Name: "Tendai Marufu", Type: "buyer", Emails: []string{"t.marufu@example.com"}, Tags: []string{"cash buyer"}})
	w.now = w.now.Add(time.Minute)
	dupe, err := w.svc.Create(ctx, w.owner, w.org, CreateInput{Name: "T. Marufu", Type: "buyer", Emails: []string{"t.marufu@example.com"},
		Phones: []string{"0773555445"}, Tags: []string{"mount pleasant"}, Source: strp("phone"), Notes: strp("Second number"),
		MarketingPermission: &MarketingInput{Granted: new(true), Source: "Phone call"}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dupe.LinkedItems) != 1 || dupe.LinkedItems[0].ID != appt.ID {
		t.Fatalf("the duplicate was linked to %v", dupe.LinkedItems)
	}
	if _, err := w.svc.AddReminder(ctx, w.owner, w.org, dupe.Contact.ID, new(w.now.Add(time.Hour)), "Call back"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.LinkListing(ctx, w.owner, w.org, dupe.Contact.ID, w.listing); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.LinkListing(ctx, w.owner, w.org, dupe.Contact.ID, w.otherListing); !errors.Is(err, ErrListingNotFound) {
		t.Fatalf("another organisation's listing: %v", err)
	}
	w.now = w.now.Add(time.Minute)
	// Another organisation cannot merge, and neither can STAFF.
	if _, err := w.svc.Merge(ctx, w.outsider, w.org, keep.Contact.ID, []uuid.UUID{dupe.Contact.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider merge: %v", err)
	}
	if _, err := w.svc.Merge(ctx, w.staff, w.org, keep.Contact.ID, []uuid.UUID{dupe.Contact.ID}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("staff merge: %v", err)
	}
	merged, err := w.svc.Merge(ctx, w.agent, w.org, keep.Contact.ID, []uuid.UUID{dupe.Contact.ID})
	if err != nil {
		t.Fatal(err)
	}
	m := merged.Contact
	if !slices.Equal(m.Phones, []string{"+263773555445"}) || !slices.Equal(m.Tags, []string{"cash buyer", "mount pleasant"}) ||
		m.Source == nil || *m.Source != "phone" || m.MarketingPermission == nil || !m.MarketingPermission.Granted ||
		len(m.LinkedListings) != 1 || m.LinkedListings[0].ID != w.listing || *m.LinkedListings[0].Slug != "flat-in-avondale" ||
		len(merged.Reminders) != 1 || len(merged.LinkedItems) != 1 || *m.OwnerUserID != w.agent.UserID {
		t.Fatalf("merged = %+v", merged)
	}
	notes, _ := w.svc.Notes(ctx, w.agent, w.org, keep.Contact.ID)
	if len(notes) != 1 || notes[0].Text != "Second number" || notes[0].MergedFrom == nil || *notes[0].MergedFrom != dupe.Contact.ID {
		t.Fatalf("notes = %+v", notes)
	}
	// The tombstone points at keep, is out of the list and refuses changes.
	tomb, err := w.svc.Get(ctx, w.agent, w.org, dupe.Contact.ID)
	if err != nil || tomb.Contact.MergedInto == nil || *tomb.Contact.MergedInto != keep.Contact.ID {
		t.Fatalf("tombstone: %+v %v", tomb.Contact, err)
	}
	if _, err := w.svc.AddNote(ctx, w.agent, w.org, dupe.Contact.ID, "late"); !errors.Is(err, ErrMerged) {
		t.Fatalf("note on a tombstone: %v", err)
	}
	page, _ := w.svc.List(ctx, w.agent, w.org, ListFilter{}, 1, 25)
	if page.Total != 1 || page.Items[0].ID != keep.Contact.ID {
		t.Fatalf("list after merge: %+v", page.Items)
	}
	// History moved too, and the merge is in the timeline and the audit trail.
	tl, _ := w.svc.Timeline(ctx, w.agent, w.org, keep.Contact.ID)
	kinds := map[string]int{}
	for _, e := range tl {
		kinds[e.Kind]++
	}
	if kinds[EventMerged] != 1 || kinds[EventCreated] != 2 || kinds[TimelineNoteAdded] != 1 || kinds[EventReminderAdded] != 1 ||
		kinds[EventListingLinked] != 1 || kinds[KindAppointment] != 1 {
		t.Fatalf("timeline kinds = %v", kinds)
	}
	audit := w.audit.All()
	if len(audit) != 1 || audit[0].Action != orgaudit.ActionContactsMerged || audit[0].TargetID != keep.Contact.ID || audit[0].OrganisationID != w.org {
		t.Fatalf("audit = %+v", audit)
	}
	if _, err := w.svc.Merge(ctx, w.agent, w.org, keep.Contact.ID, []uuid.UUID{keep.Contact.ID}); err == nil {
		t.Fatal("merging a contact into itself")
	}
}

func TestReassignmentIsLogged(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	c := w.create(w.agent, CreateInput{Name: "Nyasha", Type: "buyer"})
	to := w.agent2.UserID
	// Another agent who does not own it may not; the owner agent may.
	if _, err := w.svc.ChangeOwner(ctx, w.agent2, w.org, c.Contact.ID, &to); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-owner agent: %v", err)
	}
	got, err := w.svc.ChangeOwner(ctx, w.agent, w.org, c.Contact.ID, &to)
	if err != nil || *got.Contact.OwnerUserID != to || *got.Contact.OwnerName != "Chipo" {
		t.Fatalf("owner agent: %+v %v", got.Contact, err)
	}
	stranger := w.outsider.UserID
	var verr *ValidationError
	if _, err := w.svc.ChangeOwner(ctx, w.manager, w.org, c.Contact.ID, &stranger); !errors.As(err, &verr) {
		t.Fatalf("to a non-member: %v", err)
	}
	if _, err := w.svc.ChangeOwner(ctx, w.manager, w.org, c.Contact.ID, nil); err != nil {
		t.Fatal(err)
	}
	audit := w.audit.All()
	if len(audit) != 2 || audit[0].Action != orgaudit.ActionContactOwnerChanged || audit[0].TargetType != orgaudit.TargetContact ||
		audit[0].Before["owner_name"] != "Tendai" || audit[0].After["owner_name"] != "Chipo" || audit[1].After["owner_user_id"] != nil {
		t.Fatalf("audit = %+v", audit)
	}
	tl, _ := w.svc.Timeline(ctx, w.manager, w.org, c.Contact.ID)
	if tl[0].Kind != EventOwnerChanged || tl[0].Label != "Reassigned from Chipo to nobody" || *tl[0].ActorName != "Ruvimbo" {
		t.Fatalf("timeline = %+v", tl[0])
	}
}

func TestAutoLinking(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	byPhone := w.addItem(KindPropertyRequest, w.org, "someone@else.example", "077 123 4567", w.now.Add(-3*time.Hour))
	byAccount := w.addItem(KindBusinessEnquiry, w.org, "typo@example.com", "", w.now.Add(-2*time.Hour))
	w.items.items[1].AccountEmail = "tariro@example.com"
	_ = w.addItem(KindAppointment, w.other, "tariro@example.com", "+263771234567", w.now.Add(-time.Hour)) // another organisation
	_ = w.addItem(KindAppointment, w.org, "tariro@example.co", "+263771234568", w.now.Add(-time.Hour))    // close, not equal

	// On contact create.
	c := w.create(w.agent, CreateInput{Name: "Tariro", Type: "tenant", Emails: []string{"tariro@example.com"}, Phones: []string{"+263771234567"}})
	if len(c.LinkedItems) != 2 || c.LinkedItems[0].ID != byAccount.ID || c.LinkedItems[1].ID != byPhone.ID || !c.LinkedItems[0].Auto {
		t.Fatalf("linked on create = %+v", c.LinkedItems)
	}
	// On an item's arrival (the worker's outbox handler), idempotently.
	later := w.addItem(KindAppointment, w.org, "TARIRO@example.com", "", w.now)
	for range 2 {
		if n, err := w.svc.LinkItem(ctx, KindAppointment, later.ID); err != nil || (n != 1 && n != 0) {
			t.Fatalf("link: %d %v", n, err)
		}
	}
	got, _ := w.svc.Get(ctx, w.agent, w.org, c.Contact.ID)
	if len(got.LinkedItems) != 3 {
		t.Fatalf("linked after arrival = %+v", got.LinkedItems)
	}
	tl, _ := w.svc.Timeline(ctx, w.agent, w.org, c.Contact.ID)
	items := 0
	for _, e := range tl {
		if e.Kind == EventItemLinked {
			t.Fatal("ITEM_LINKED is not a timeline entry (the item is)")
		}
		if e.Ref != nil && e.Ref.Kind != KindListing {
			items++
		}
	}
	if items != 3 {
		t.Fatalf("%d item entries", items)
	}
	// On edit: a new phone links what it matches.
	byNew := w.addItem(KindPropertyRequest, w.org, "", "0772 000 111", w.now)
	phones := []string{"+263771234567", "+263772000111"}
	edited, err := w.svc.Patch(ctx, w.agent, w.org, c.Contact.ID, PatchInput{Phones: &phones}, false)
	if err != nil || !slices.ContainsFunc(edited.LinkedItems, func(l LinkedItemView) bool { return l.ID == byNew.ID }) {
		t.Fatalf("linked on edit = %+v %v", edited.LinkedItems, err)
	}
}

func TestFromItem(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	it := w.addItem(KindPropertyRequest, w.org, "new.customer@example.com", "0779 999 000", w.now)
	assigned := w.agent2.UserID
	w.items.items[0].AssignedUserID = &assigned
	// Assigned to Chipo: Tendai (an agent) cannot see it; Chipo and the owner can.
	if _, _, err := w.svc.FromItem(ctx, w.agent, w.org, it.ID, "", ""); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("not visible: %v", err)
	}
	if _, _, err := w.svc.FromItem(ctx, w.outsider, w.other, it.ID, "", ""); !errors.Is(err, ErrItemNotFound) {
		t.Fatalf("another organisation: %v", err)
	}
	c, created, err := w.svc.FromItem(ctx, w.agent2, w.org, it.ID, "", "buyer")
	if err != nil || !created || c.Contact.Type != "buyer" || *c.Contact.Source != SourceEnquiry || c.Contact.Phones[0] != "+263779999000" ||
		len(c.LinkedItems) != 1 || c.LinkedItems[0].Auto {
		t.Fatalf("from item: %+v %v %v", c, created, err)
	}
	again, created, err := w.svc.FromItem(ctx, w.owner, w.org, it.ID, KindPropertyRequest, "")
	if err != nil || created || again.Contact.ID != c.Contact.ID {
		t.Fatalf("second time: %v %v", created, err)
	}
}

func TestImport(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	existing := w.create(w.owner, CreateInput{Name: "Existing", Type: "buyer", Emails: []string{"existing@example.com"}})
	csv := "\xef\xbb\xbfName,Email,Phone,Type,Tags,Stage,Notes,Marketing_Granted,ignored\n" +
		"Alice Moyo,alice@example.com,077 111 2222,buyer,a;b,CONTACTED,Met at the show,yes,x\n" +
		"Existing Again,EXISTING@example.com,,,,,,,\n" +
		"Alice Twin,,0771112222,,,,,,\n" +
		",,,,,,,,\n" +
		"Bad Row,not-an-email,12,alien,,WON,,maybe,\n" +
		"'=Formula,,'+263773000000,,,,,,\n"
	dry, err := w.svc.Import(ctx, w.manager, w.org, []byte(csv), true)
	if err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.TotalRows != 5 || dry.Summary.Create != 2 || dry.Summary.Duplicate != 2 || dry.Summary.Error != 1 || dry.Summary.Created != nil {
		t.Fatalf("dry run = %+v", dry)
	}
	byRow := map[int]ImportRow{}
	for _, r := range dry.Rows {
		byRow[r.Row] = r
	}
	if byRow[2].Status != OutcomeCreate || *byRow[2].Email != "alice@example.com" || *byRow[2].Phone != "+263771112222" {
		t.Fatalf("row 2 = %+v", byRow[2])
	}
	if byRow[3].Status != OutcomeDuplicate || !slices.Equal(byRow[3].DuplicateOf, []uuid.UUID{existing.Contact.ID}) {
		t.Fatalf("row 3 = %+v", byRow[3])
	}
	if byRow[4].Status != OutcomeDuplicate || byRow[4].DuplicateOfRow == nil || *byRow[4].DuplicateOfRow != 2 {
		t.Fatalf("row 4 = %+v", byRow[4])
	}
	if r := byRow[6]; r.Status != OutcomeError || r.Errors["emails[0]"] == "" || r.Errors["phones[0]"] == "" || r.Errors["type"] == "" ||
		r.Errors["stage"] == "" || r.Errors["marketing_granted"] == "" {
		t.Fatalf("row 6 = %+v", r)
	}
	if r := byRow[7]; r.Status != OutcomeCreate || r.Name != "=Formula" {
		t.Fatalf("row 7 = %+v", r)
	}
	if page, _ := w.svc.List(ctx, w.manager, w.org, ListFilter{}, 1, 25); page.Total != 1 || len(w.audit.All()) != 0 {
		t.Fatal("a dry run changed something")
	}

	real, err := w.svc.Import(ctx, w.manager, w.org, []byte(csv), false)
	if err != nil || real.Summary.Created == nil || *real.Summary.Created != 2 {
		t.Fatalf("real run = %+v %v", real, err)
	}
	for _, r := range real.Rows {
		want := map[int]string{2: OutcomeCreated, 3: OutcomeSkipped, 4: OutcomeSkipped, 6: OutcomeError, 7: OutcomeCreated}[r.Row]
		if r.Status != want || (want == OutcomeCreated) != (r.ContactID != nil) {
			t.Fatalf("row %d = %+v", r.Row, r)
		}
	}
	page, _ := w.svc.List(ctx, w.manager, w.org, ListFilter{Sort: SortName}, 1, 25)
	if page.Total != 3 || page.Items[1].Name != "Alice Moyo" || page.Items[1].Stage != StageContacted || *page.Items[1].Source != SourceImport ||
		!page.Items[1].MarketingPermission.Granted {
		t.Fatalf("after import = %+v", page.Items)
	}
	// Never overwritten.
	if got, _ := w.svc.Get(ctx, w.manager, w.org, existing.Contact.ID); got.Contact.Name != "Existing" || !got.Contact.UpdatedAt.Equal(existing.Contact.UpdatedAt) {
		t.Fatal("an existing contact was changed")
	}
	audit := w.audit.All()
	if len(audit) != 1 || audit[0].Action != orgaudit.ActionContactsImported || audit[0].After["created"] != 2 || audit[0].TargetType != orgaudit.TargetContactBook {
		t.Fatalf("audit = %+v", audit)
	}

	// Limits and bad files.
	var big strings.Builder
	big.WriteString("name\n")
	for i := range MaxImportRows + 1 {
		fmt.Fprintf(&big, "P%d\n", i)
	}
	if _, err := w.svc.Import(ctx, w.manager, w.org, []byte(big.String()), true); !errors.Is(err, ErrImportTooManyRows) {
		t.Fatalf("too many rows: %v", err)
	}
	if _, err := w.svc.Import(ctx, w.manager, w.org, make([]byte, MaxImportBytes+1), true); !errors.Is(err, ErrImportTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	var verr *ValidationError
	if _, err := w.svc.Import(ctx, w.manager, w.org, []byte("email\nx@example.com\n"), true); !errors.As(err, &verr) {
		t.Fatalf("no name column: %v", err)
	}
}

func TestExport(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.create(w.owner, CreateInput{Name: "=cmd", Type: "buyer", Emails: []string{"a@example.com"}, Phones: []string{"0771234567"}, Tags: []string{"x", "y"}})
	w.create(w.owner, CreateInput{Name: "Second", Type: "tenant"})
	for _, p := range []*auth.Principal{w.agent, w.staff} {
		if _, err := w.svc.Export(ctx, p, w.org); !errors.Is(err, ErrForbidden) {
			t.Fatalf("export by %s: %v", p.Name, err)
		}
	}
	if len(w.audit.All()) != 0 {
		t.Fatal("a refused export was audited")
	}
	rows, err := w.svc.Export(ctx, w.manager, w.org)
	if err != nil || len(rows) != 2 || len(rows[0]) != len(ExportHeader()) {
		t.Fatalf("export = %v %v", rows, err)
	}
	if rows[1][1] != "=cmd" || rows[1][5] != "+263771234567" || rows[1][6] != "x; y" {
		t.Fatalf("row = %v", rows[1])
	}
	audit := w.audit.All()
	if len(audit) != 1 || audit[0].Action != orgaudit.ActionContactsExported || audit[0].ActorName != "Ruvimbo" || audit[0].After["rows"] != 2 {
		t.Fatalf("audit = %+v", audit)
	}
}

func TestTimelineOrderingAndStages(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	item := w.addItem(KindPropertyRequest, w.org, "nyasha@example.com", "", w.now.Add(-48*time.Hour))
	c := w.create(w.agent, CreateInput{Name: "Nyasha", Type: "buyer", Emails: []string{"nyasha@example.com"}})
	step := func() { w.now = w.now.Add(time.Minute) }
	step()
	if _, err := w.svc.AddNote(ctx, w.agent, w.org, c.Contact.ID, "First call"); err != nil {
		t.Fatal(err)
	}
	step()
	if _, err := w.svc.ChangeStage(ctx, w.agent, w.org, c.Contact.ID, StageNegotiating, "Offer made"); err != nil {
		t.Fatal(err)
	}
	step()
	// Any stage to any other, backwards included.
	if _, err := w.svc.ChangeStage(ctx, w.agent, w.org, c.Contact.ID, StageNew, ""); err != nil {
		t.Fatal(err)
	}
	step()
	r, err := w.svc.AddReminder(ctx, w.agent, w.org, c.Contact.ID, new(w.now.Add(-time.Minute)), "Chase")
	if err != nil {
		t.Fatal(err)
	}
	step()
	if _, err := w.svc.PatchReminder(ctx, w.agent, w.org, c.Contact.ID, r.ID, new(true)); err != nil {
		t.Fatal(err)
	}
	tl, err := w.svc.Timeline(ctx, w.agent, w.org, c.Contact.ID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for i, e := range tl {
		kinds = append(kinds, e.Kind)
		if i > 0 && e.At.After(tl[i-1].At) {
			t.Fatalf("not newest first at %d: %v", i, kinds)
		}
	}
	want := []string{EventReminderDone, EventReminderAdded, EventStageChanged, EventStageChanged, TimelineNoteAdded, EventCreated, KindPropertyRequest}
	if !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	if tl[3].Label != "Stage changed from New to Negotiating" || *tl[3].Text != "Offer made" || *tl[4].Text != "First call" ||
		tl[6].Ref.ID != item.ID || *tl[6].Ref.Reference != item.Reference {
		t.Fatalf("entries = %+v", tl)
	}
	if _, err := w.svc.ChangeStage(ctx, w.agent, w.org, c.Contact.ID, "WON", ""); err == nil {
		t.Fatal("an unknown stage was accepted")
	}
}

func TestListFiltersAndReminders(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	a := w.create(w.agent, CreateInput{Name: "Alpha", Type: "buyer", Tags: []string{"VIP"}, Phones: []string{"0771234567"}})
	w.now = w.now.Add(time.Minute)
	b := w.create(w.owner, CreateInput{Name: "Bravo", Type: "tenant", Stage: strp(StageContacted)})
	if _, err := w.svc.AddReminder(ctx, w.agent, w.org, a.Contact.ID, new(w.now.Add(-time.Hour)), "Overdue"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.AddReminder(ctx, w.owner, w.org, b.Contact.ID, new(w.now.Add(72*time.Hour)), "Later"); err != nil {
		t.Fatal(err)
	}
	list := func(f ListFilter) []string {
		page, err := w.svc.List(ctx, w.staff, w.org, f, 1, 25)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, c := range page.Items {
			names = append(names, c.Name)
		}
		return names
	}
	now := w.now
	for name, tc := range map[string]struct {
		f    ListFilter
		want []string
	}{
		"q name":   {ListFilter{Q: "alp"}, []string{"Alpha"}},
		"q phone":  {ListFilter{Q: "077 123"}, []string{"Alpha"}},
		"type":     {ListFilter{Type: "tenant"}, []string{"Bravo"}},
		"stage":    {ListFilter{Stage: StageContacted}, []string{"Bravo"}},
		"tag":      {ListFilter{Tag: "vip"}, []string{"Alpha"}},
		"owner":    {ListFilter{Owner: &w.agent.UserID}, []string{"Alpha"}},
		"reminder": {ListFilter{ReminderDueBefore: &now}, []string{"Alpha"}},
		"name":     {ListFilter{Sort: SortName}, []string{"Alpha", "Bravo"}},
		"created":  {ListFilter{Sort: SortCreated}, []string{"Bravo", "Alpha"}},
	} {
		if got := list(tc.f); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
	page, _ := w.svc.List(ctx, w.owner, w.org, ListFilter{Stage: StageContacted}, 1, 25)
	if page.StageCounts[StageNew] != 1 || page.StageCounts[StageContacted] != 1 || page.StageCounts[StageClosed] != 0 {
		t.Fatalf("stage counts = %v", page.StageCounts)
	}
	all, _ := w.svc.List(ctx, w.owner, w.org, ListFilter{Sort: SortName}, 1, 25)
	if all.Items[0].RemindersDue != 1 || all.Items[1].RemindersDue != 0 || all.Items[1].OpenReminders != 1 {
		t.Fatalf("reminders due = %+v", all.Items)
	}
	tags, _ := w.svc.Tags(ctx, w.staff, w.org)
	if len(tags) != 1 || tags[0] != (TagCount{Tag: "vip", Count: 1}) {
		t.Fatalf("tags = %v", tags)
	}

	due := func(period string, mine bool) int {
		got, err := w.svc.DueReminders(ctx, w.agent, w.org, period, mine)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if due(DueOverdue, false) != 1 || due(DueToday, false) != 1 || due(DueWeek, false) != 2 || due(DueWeek, true) != 1 {
		t.Fatal("due reminders")
	}
	if _, err := w.svc.DueReminders(ctx, w.agent, w.org, "month", false); err == nil {
		t.Fatal("an unknown period")
	}
	if _, err := w.svc.DueReminders(ctx, w.outsider, w.org, DueToday, false); !errors.Is(err, ErrNotFound) {
		t.Fatal("another organisation's reminders")
	}
	// The worker's sweep: one CONTACT_REMINDER_DUE per due reminder, once.
	for range 2 {
		if _, err := w.svc.SweepReminders(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if ev := w.store.Outbox(); len(ev) != 1 || ev[0].Type != EventReminderDue || ev[0].AggregateID != a.Contact.ID {
		t.Fatalf("outbox = %+v", ev)
	}
	// A deleted contact is gone.
	if err := w.svc.Delete(ctx, w.agent, w.org, b.Contact.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Get(ctx, w.agent, w.org, b.Contact.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("a deleted contact is still there")
	}
}
