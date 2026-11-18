package crm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/markets"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Deps is what the service needs.
type Deps struct {
	Store      Store
	Items      Items
	Listings   Listings
	People     People
	Vocabulary Vocabulary
	// Members lists an organisation's active members (GET .../assignees).
	Members Members
	Logger  *slog.Logger
	Now     func() time.Time
}

// Service is the agent CRM.
type Service struct {
	store    Store
	items    Items
	listings Listings
	people   People
	vocab    Vocabulary
	members  Members
	logger   *slog.Logger
	now      func() time.Time
}

// NewService builds the service.
func NewService(d Deps) *Service {
	s := &Service{store: d.Store, items: d.Items, listings: d.Listings, people: d.People, vocab: d.Vocabulary, members: d.Members, logger: d.Logger, now: d.Now}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logger == nil {
		s.logger = slog.Default()
	}
	return s
}

func (s *Service) clock() time.Time { return s.now().UTC().Truncate(time.Microsecond) }

// access is what the caller may do in one book.
type access struct {
	p        *auth.Principal
	book     Book
	write    bool
	manage   bool
	role     string
	market   string
	personal bool
}

// access resolves the caller's rights in the book bookID (an organisation ID,
// or the caller's own user ID for a direct owner's personal book). A book
// that is not theirs is ErrNotFound.
func (s *Service) access(ctx context.Context, p *auth.Principal, bookID uuid.UUID) (access, error) {
	if p == nil {
		return access{}, ErrUnauthenticated
	}
	if p.MayReadContacts(bookID) {
		return access{p: p, book: OrganisationBook(bookID), write: p.MayWriteContacts(bookID), manage: p.MayManageContacts(bookID),
			role: p.MembershipRole(bookID), market: p.MembershipMarket(bookID)}, nil
	}
	if p.OwnsContactBook(bookID) {
		market := ""
		if s.listings != nil {
			m, err := s.listings.OwnerMarket(ctx, p.UserID)
			if err != nil {
				return access{}, err
			}
			market = m
		}
		return access{p: p, book: PersonalBook(bookID), write: true, personal: true, market: market}, nil
	}
	return access{}, ErrNotFound
}

func (a access) actor() (*uuid.UUID, string) {
	id := a.p.UserID
	return &id, a.p.Name
}

// contact returns a contact of the book that is not deleted (a tombstone
// included), or ErrNotFound.
func (s *Service) contact(ctx context.Context, a access, id uuid.UUID) (Contact, error) {
	c, err := s.store.Get(ctx, id)
	if err != nil {
		return Contact{}, err
	}
	if !c.Book.Same(a.book) || c.DeletedAt != nil {
		return Contact{}, ErrNotFound
	}
	return c, nil
}

// live returns a contact that may be changed: ErrMerged for a tombstone.
func (s *Service) live(ctx context.Context, a access, id uuid.UUID) (Contact, error) {
	c, err := s.contact(ctx, a, id)
	if err != nil {
		return Contact{}, err
	}
	if c.MergedInto != nil {
		return Contact{}, ErrMerged
	}
	return c, nil
}

// retry runs fn again when a concurrent change made it fail (up to 3 times).
func retry[T any](fn func() (T, error)) (T, error) {
	var out T
	var err error
	for range 3 {
		out, err = fn()
		if !errors.Is(err, ErrConflict) {
			return out, err
		}
	}
	return out, err
}

// ---------------------------------------------------------------------------
// Labels and views
// ---------------------------------------------------------------------------

type labels struct {
	types, sources map[string]string
	names          map[uuid.UUID]string
}

func (s *Service) labels(ctx context.Context) (*labels, error) {
	l := &labels{names: map[uuid.UUID]string{}}
	var err error
	if l.types, err = s.vocab.Labels(ctx, SetContactTypes); err != nil {
		return nil, err
	}
	if l.sources, err = s.vocab.Labels(ctx, SetContactSources); err != nil {
		return nil, err
	}
	return l, nil
}

func (s *Service) person(ctx context.Context, l *labels, id *uuid.UUID) (*PersonRef, error) {
	if id == nil {
		return nil, nil
	}
	name, ok := l.names[*id]
	if !ok && s.people != nil {
		n, err := s.people.Name(ctx, *id)
		if err != nil {
			return nil, err
		}
		name, l.names[*id] = n, n
	}
	return &PersonRef{UserID: *id, Name: name}, nil
}

func label(m map[string]string, code string) string {
	if l, ok := m[code]; ok {
		return l
	}
	return code
}

func (s *Service) view(ctx context.Context, l *labels, c Contact, refs map[uuid.UUID]ListingRef) (ContactView, error) {
	v := ContactView{
		ID: c.ID, Name: c.Name, Type: c.Type, TypeLabel: label(l.types, c.Type), Stage: c.Stage, StageLabel: StageLabel(c.Stage),
		Emails: nonNil(c.Emails), Phones: nonNil(c.Phones), Tags: nonNil(c.Tags), OwnerUserID: c.OwnerUserID,
		LastContactAt: c.LastContactAt, RemindersDue: c.DueReminders, OpenReminders: c.OpenReminders, NextReminderAt: c.NextReminderAt,
		MergedInto: c.MergedInto, CreatedBy: c.CreatedBy, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt,
		LinkedListings: []ListingView{},
	}
	owner, err := s.person(ctx, l, c.OwnerUserID)
	if err != nil {
		return ContactView{}, err
	}
	if owner != nil {
		v.OwnerName = &owner.Name
	}
	if c.Source != "" {
		src, lbl := c.Source, label(l.sources, c.Source)
		v.Source, v.SourceLabel = &src, &lbl
	}
	if m := c.Marketing; m != nil {
		mv := &MarketingView{Granted: m.Granted, GrantedAt: m.GrantedAt, Source: m.Source, RecordedBy: m.RecordedBy}
		by, err := s.person(ctx, l, m.RecordedBy)
		if err != nil {
			return ContactView{}, err
		}
		if by != nil {
			mv.RecordedByName = &by.Name
		}
		v.MarketingPermission = mv
	}
	for _, id := range c.ListingIDs {
		r := refs[id]
		lv := ListingView{ID: id, Title: r.Title, Reference: r.Reference}
		if r.Slug != "" {
			slug := r.Slug
			lv.Slug = &slug
		}
		v.LinkedListings = append(v.LinkedListings, lv)
	}
	return v, nil
}

// listingRefs looks up the linked listings of contacts in one call.
func (s *Service) listingRefs(ctx context.Context, contacts ...Contact) (map[uuid.UUID]ListingRef, error) {
	out := map[uuid.UUID]ListingRef{}
	var ids []uuid.UUID
	for _, c := range contacts {
		for _, id := range c.ListingIDs {
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 || s.listings == nil {
		return out, nil
	}
	refs, err := s.listings.Listings(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, r := range refs {
		out[r.ID] = r
	}
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Service) noteView(_ context.Context, _ *labels, n Note) (NoteView, error) {
	v := NoteView{ID: n.ID, ContactID: n.ContactID, Text: n.Text, CreatedAt: n.CreatedAt, MergedFrom: n.MergedFrom}
	if n.AuthorUserID != nil {
		v.Author = &PersonRef{UserID: *n.AuthorUserID, Name: n.AuthorName}
	}
	return v, nil
}

func (s *Service) reminderView(r Reminder, now time.Time) ReminderView {
	v := ReminderView{ID: r.ID, ContactID: r.ContactID, DueAt: r.DueAt, Text: r.Text, Done: r.Done, DoneAt: r.DoneAt,
		CreatedAt: r.CreatedAt, Overdue: !r.Done && r.DueAt.Before(now)}
	if r.CreatedBy != nil {
		v.CreatedBy = &PersonRef{UserID: *r.CreatedBy, Name: r.CreatedByName}
	}
	return v
}

// detail builds the answer of every single-contact endpoint from the
// stored contact (re-read, so that it shows what was written).
func (s *Service) detail(ctx context.Context, written Contact) (ContactBody, error) {
	c, err := s.store.Get(ctx, written.ID)
	if err != nil {
		return ContactBody{}, err
	}
	l, err := s.labels(ctx)
	if err != nil {
		return ContactBody{}, err
	}
	rems, err := s.store.Reminders(ctx, c.ID)
	if err != nil {
		return ContactBody{}, err
	}
	now := s.clock()
	out := ContactBody{LinkedItems: []LinkedItemView{}, Reminders: []ReminderView{}}
	for _, r := range rems {
		out.Reminders = append(out.Reminders, s.reminderView(r, now))
		if !r.Done && !r.DueAt.After(now) {
			c.DueReminders++
		}
	}
	refs, err := s.listingRefs(ctx, c)
	if err != nil {
		return ContactBody{}, err
	}
	if out.Contact, err = s.view(ctx, l, c, refs); err != nil {
		return ContactBody{}, err
	}
	links, err := s.store.Links(ctx, c.ID)
	if err != nil {
		return ContactBody{}, err
	}
	for _, k := range links {
		if k.Kind == KindListing {
			continue
		}
		it, err := s.items.Item(ctx, k.Kind, k.ItemID)
		if errors.Is(err, ErrItemNotFound) {
			continue
		}
		if err != nil {
			return ContactBody{}, err
		}
		out.LinkedItems = append(out.LinkedItems, LinkedItemView{Kind: it.Kind, ID: it.ID, Reference: it.Reference, Type: it.Type, Status: it.Status,
			Title: it.Title, ListingID: it.ListingID, CreatedAt: it.CreatedAt, Auto: k.Auto, LinkedAt: k.LinkedAt})
	}
	sort.SliceStable(out.LinkedItems, func(i, j int) bool { return out.LinkedItems[i].CreatedAt.After(out.LinkedItems[j].CreatedAt) })
	return out, nil
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

type fields map[string]string

func (f fields) err() error {
	if len(f) == 0 {
		return nil
	}
	return &ValidationError{Fields: f}
}

func cleanName(raw string, f fields, key string) string {
	name := strings.Join(strings.Fields(raw), " ")
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		f[key] = "is required"
	case n > MaxNameLength:
		f[key] = fmt.Sprintf("must be at most %d characters", MaxNameLength)
	}
	return name
}

func cleanEmails(raw []string, f fields, key string) []string {
	var out []string
	for i, e := range raw {
		if strings.TrimSpace(e) == "" {
			continue
		}
		n, err := NormaliseEmail(e)
		if err != nil {
			f[fmt.Sprintf("%s[%d]", key, i)] = err.Error()
			continue
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) > MaxEmails {
		f[key] = fmt.Sprintf("may hold at most %d addresses", MaxEmails)
	}
	return out
}

func cleanPhones(raw []string, region string, f fields, key string) []string {
	var out []string
	for i, p := range raw {
		if strings.TrimSpace(p) == "" {
			continue
		}
		n, err := NormalisePhone(p, region)
		if err != nil {
			f[fmt.Sprintf("%s[%d]", key, i)] = err.Error()
			continue
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) > MaxPhones {
		f[key] = fmt.Sprintf("may hold at most %d numbers", MaxPhones)
	}
	return out
}

func cleanTags(raw []string, f fields, key string) []string {
	var out []string
	for _, t := range raw {
		n := normaliseTag(t)
		if n == "" {
			continue
		}
		if utf8.RuneCountInString(n) > MaxTagLength {
			f[key] = fmt.Sprintf("each tag must be at most %d characters", MaxTagLength)
			continue
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	if len(out) > MaxTags {
		f[key] = fmt.Sprintf("may hold at most %d tags", MaxTags)
	}
	return out
}

func (s *Service) checkCode(ctx context.Context, set, code string, f fields, key string) error {
	codes, err := s.vocab.Labels(ctx, set)
	if err != nil {
		return err
	}
	if _, ok := codes[code]; !ok {
		f[key] = "must be one of the codes in GET /v1/reference/" + set
	}
	return nil
}

func cleanMarketing(in *MarketingInput, a access, now time.Time, f fields) *Marketing {
	if in == nil {
		return nil
	}
	if in.Granted == nil {
		f["marketing_permission.granted"] = "is required"
		return nil
	}
	src := strings.TrimSpace(in.Source)
	if utf8.RuneCountInString(src) > MaxMarketingSource {
		f["marketing_permission.source"] = fmt.Sprintf("must be at most %d characters", MaxMarketingSource)
	}
	by, _ := a.actor()
	return &Marketing{Granted: *in.Granted, GrantedAt: now, Source: src, RecordedBy: by}
}

// ownerFor checks the relationship owner a caller asks for.
func (s *Service) ownerFor(ctx context.Context, a access, want *uuid.UUID, f fields, key string) (*uuid.UUID, error) {
	if want == nil {
		return nil, nil
	}
	if a.personal {
		if *want != a.p.UserID {
			f[key] = "must be you: a personal contact book has no other members"
			return nil, nil
		}
		return want, nil
	}
	if !a.manage && *want != a.p.UserID {
		return nil, ErrForbidden
	}
	_, ok, err := s.people.Assignable(ctx, a.book.ID(), *want)
	if err != nil {
		return nil, err
	}
	if !ok {
		f[key] = "must be an active OWNER, MANAGER or AGENT of the organisation"
		return nil, nil
	}
	o := *want
	return &o, nil
}

// ---------------------------------------------------------------------------
// Duplicates and links
// ---------------------------------------------------------------------------

func (s *Service) duplicates(ctx context.Context, book Book, emails, phones []string, except uuid.UUID) ([]DuplicateRef, error) {
	if len(emails) == 0 && len(phones) == 0 {
		return nil, nil
	}
	found, err := s.store.Matching(ctx, book, emails, phones)
	if err != nil {
		return nil, err
	}
	var out []DuplicateRef
	for _, c := range found {
		if c.ID != except && !slices.ContainsFunc(out, func(d DuplicateRef) bool { return d.ID == c.ID }) {
			out = append(out, DuplicateRef{ID: c.ID, Name: c.Name})
		}
	}
	return out, nil
}

// identifiers returns an item's normalised emails and phones.
func identifiers(it Item, fallbackRegion string) (emails, phones []string) {
	for _, e := range []string{it.Email, it.AccountEmail} {
		if n, err := NormaliseEmail(e); err == nil && !slices.Contains(emails, n) {
			emails = append(emails, n)
		}
	}
	region := it.MarketCode
	if region == "" {
		region = fallbackRegion
	}
	if it.Phone != "" {
		if n, err := NormalisePhone(it.Phone, region); err == nil {
			phones = append(phones, n)
		}
	}
	return emails, phones
}

func shares(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// matchItems returns the items of the book that share an email or a phone
// with the identifiers (exact match after normalisation).
func (s *Service) matchItems(ctx context.Context, book Book, region string, emails, phones []string) ([]Item, error) {
	if s.items == nil || (len(emails) == 0 && len(phones) == 0) {
		return nil, nil
	}
	tails := make([]string, 0, len(phones))
	for _, p := range phones {
		tails = append(tails, phoneTail(p))
	}
	cands, err := s.items.Candidates(ctx, book, emails, tails)
	if err != nil {
		return nil, err
	}
	var out []Item
	for _, it := range cands {
		ib, ok := it.Book()
		if !ok || !ib.Same(book) {
			continue
		}
		ie, ip := identifiers(it, region)
		if shares(ie, emails) || shares(ip, phones) {
			out = append(out, it)
		}
	}
	return out, nil
}

// linkChange adds the links (and their ITEM_LINKED entries) of items to a
// contact, leaving out the ones it already has.
func linkChange(ch *Change, contactID uuid.UUID, items []Item, existing []Link, actor *uuid.UUID, actorName string, now time.Time) {
	for _, it := range items {
		if slices.ContainsFunc(existing, func(l Link) bool { return l.Kind == it.Kind && l.ItemID == it.ID }) ||
			slices.ContainsFunc(ch.Links, func(l Link) bool { return l.ContactID == contactID && l.Kind == it.Kind && l.ItemID == it.ID }) {
			continue
		}
		ch.Links = append(ch.Links, Link{ContactID: contactID, Kind: it.Kind, ItemID: it.ID, Auto: true, LinkedAt: now})
		ch.Events = append(ch.Events, Event{ID: newID(), ContactID: contactID, Kind: EventItemLinked, ActorUserID: actor, ActorName: actorName,
			Data: map[string]any{"kind": it.Kind, "id": it.ID.String(), "reference": it.Reference}, At: now})
	}
}

// ---------------------------------------------------------------------------
// Contacts
// ---------------------------------------------------------------------------

// List serves GET .../contacts.
func (s *Service) List(ctx context.Context, p *auth.Principal, bookID uuid.UUID, f ListFilter, page, size int) (Page, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return Page{}, err
	}
	f.Limit, f.Offset, f.Now = size, (page-1)*size, s.clock()
	rows, total, err := s.store.List(ctx, a.book, f)
	if err != nil {
		return Page{}, err
	}
	counts, err := s.store.StageCounts(ctx, a.book, f)
	if err != nil {
		return Page{}, err
	}
	l, err := s.labels(ctx)
	if err != nil {
		return Page{}, err
	}
	refs, err := s.listingRefs(ctx, rows...)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: make([]ContactView, 0, len(rows)), Page: page, PageSize: size, Total: total, TotalPages: (total + size - 1) / size,
		StageCounts: counts}
	for _, c := range rows {
		v, err := s.view(ctx, l, c, refs)
		if err != nil {
			return Page{}, err
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

// ResolveOwnerFilter turns ?owner= (a user ID, "me" or "none") into the filter.
func ResolveOwnerFilter(p *auth.Principal, raw string, f *ListFilter) bool {
	switch raw {
	case "":
		return true
	case "me":
		if p == nil {
			return false
		}
		id := p.UserID
		f.Owner = &id
		return true
	case "none":
		f.Unowned = true
		return true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return false
	}
	f.Owner = &id
	return true
}

// Get serves GET .../contacts/{id}.
func (s *Service) Get(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (ContactBody, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return ContactBody{}, err
	}
	c, err := s.contact(ctx, a, id)
	if err != nil {
		return ContactBody{}, err
	}
	return s.detail(ctx, c)
}

// Create serves POST .../contacts.
func (s *Service) Create(ctx context.Context, p *auth.Principal, bookID uuid.UUID, in CreateInput, allowDuplicate bool) (ContactBody, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return ContactBody{}, err
	}
	if !a.write {
		return ContactBody{}, ErrForbidden
	}
	now := s.clock()
	f := fields{}
	c := Contact{ID: newID(), Book: a.book, Stage: StageNew, CreatedAt: now, UpdatedAt: now}
	c.CreatedBy, _ = a.actor()
	c.Name = cleanName(in.Name, f, "name")
	c.Emails = cleanEmails(in.Emails, f, "emails")
	c.Phones = cleanPhones(in.Phones, a.market, f, "phones")
	c.Tags = cleanTags(in.Tags, f, "tags")
	c.Type = strings.TrimSpace(in.Type)
	if c.Type == "" {
		f["type"] = "is required"
	} else if err := s.checkCode(ctx, SetContactTypes, c.Type, f, "type"); err != nil {
		return ContactBody{}, err
	}
	if in.Source != nil && strings.TrimSpace(*in.Source) != "" {
		c.Source = strings.TrimSpace(*in.Source)
		if err := s.checkCode(ctx, SetContactSources, c.Source, f, "source"); err != nil {
			return ContactBody{}, err
		}
	}
	if in.Stage != nil {
		if !validStage(*in.Stage) {
			f["stage"] = "must be one of " + strings.Join(Stages(), ", ")
		} else {
			c.Stage = *in.Stage
		}
	}
	c.Marketing = cleanMarketing(in.MarketingPermission, a, now, f)
	var note string
	if in.Notes != nil {
		note = strings.TrimSpace(*in.Notes)
		if utf8.RuneCountInString(note) > MaxNoteLength {
			f["notes"] = fmt.Sprintf("must be at most %d characters", MaxNoteLength)
		}
	}
	owner, err := s.ownerFor(ctx, a, in.OwnerUserID, f, "owner_user_id")
	if err != nil {
		return ContactBody{}, err
	}
	if err := f.err(); err != nil {
		return ContactBody{}, err
	}
	if owner == nil && in.OwnerUserID == nil && (a.personal || auth.RoleHoldsLeads(a.role)) {
		owner = c.CreatedBy // the creator holds the relationship by default
	}
	c.OwnerUserID = owner
	if !allowDuplicate {
		ids, err := s.duplicates(ctx, a.book, c.Emails, c.Phones, uuid.Nil)
		if err != nil {
			return ContactBody{}, err
		}
		if len(ids) > 0 {
			return ContactBody{}, &DuplicateError{Contacts: ids}
		}
	}
	actor, name := a.actor()
	ch := Change{Contacts: []Contact{c}, Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventCreated, ActorUserID: actor, ActorName: name,
		Data: map[string]any{"stage": c.Stage}, At: now}}}
	if note != "" {
		ch.Notes = []Note{{ID: newID(), ContactID: c.ID, Text: note, AuthorUserID: actor, AuthorName: name, CreatedAt: now}}
	}
	items, err := s.matchItems(ctx, a.book, a.market, c.Emails, c.Phones)
	if err != nil {
		return ContactBody{}, err
	}
	linkChange(&ch, c.ID, items, nil, nil, "", now)
	if err := s.store.Apply(ctx, ch); err != nil {
		return ContactBody{}, err
	}
	return s.detail(ctx, c)
}

// Patch serves PATCH .../contacts/{id}.
func (s *Service) Patch(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in PatchInput, allowDuplicate bool) (ContactBody, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return ContactBody{}, err
	}
	if !a.write {
		if _, err := s.contact(ctx, a, id); err != nil {
			return ContactBody{}, err
		}
		return ContactBody{}, ErrForbidden
	}
	return retry(func() (ContactBody, error) {
		cur, err := s.live(ctx, a, id)
		if err != nil {
			return ContactBody{}, err
		}
		now := s.clock()
		f := fields{}
		c := cur
		var changed []string
		if in.Name != nil {
			if c.Name = cleanName(*in.Name, f, "name"); c.Name != cur.Name {
				changed = append(changed, "name")
			}
		}
		if in.Emails != nil {
			if c.Emails = cleanEmails(*in.Emails, f, "emails"); !slices.Equal(c.Emails, cur.Emails) {
				changed = append(changed, "emails")
			}
		}
		if in.Phones != nil {
			if c.Phones = cleanPhones(*in.Phones, a.market, f, "phones"); !slices.Equal(c.Phones, cur.Phones) {
				changed = append(changed, "phones")
			}
		}
		if in.Tags != nil {
			if c.Tags = cleanTags(*in.Tags, f, "tags"); !slices.Equal(c.Tags, cur.Tags) {
				changed = append(changed, "tags")
			}
		}
		if in.Type != nil {
			c.Type = strings.TrimSpace(*in.Type)
			if err := s.checkCode(ctx, SetContactTypes, c.Type, f, "type"); err != nil {
				return ContactBody{}, err
			}
			if c.Type != cur.Type {
				changed = append(changed, "type")
			}
		}
		if in.Source.Set {
			c.Source = ""
			if in.Source.Value != nil && strings.TrimSpace(*in.Source.Value) != "" {
				c.Source = strings.TrimSpace(*in.Source.Value)
				if err := s.checkCode(ctx, SetContactSources, c.Source, f, "source"); err != nil {
					return ContactBody{}, err
				}
			}
			if c.Source != cur.Source {
				changed = append(changed, "source")
			}
		}
		if in.MarketingPermission.Set {
			c.Marketing = cleanMarketing(in.MarketingPermission.Value, a, now, f)
			changed = append(changed, "marketing_permission")
		}
		if in.LastContactAt.Set {
			c.LastContactAt = nil
			if v := in.LastContactAt.Value; v != nil {
				t := v.UTC().Truncate(time.Microsecond)
				if t.After(now.Add(5 * time.Minute)) {
					f["last_contact_at"] = "cannot be in the future"
				}
				c.LastContactAt = &t
			}
			changed = append(changed, "last_contact_at")
		}
		if err := f.err(); err != nil {
			return ContactBody{}, err
		}
		if len(changed) == 0 {
			return s.detail(ctx, cur)
		}
		var added []string
		for _, e := range c.Emails {
			if !slices.Contains(cur.Emails, e) {
				added = append(added, e)
			}
		}
		var addedPhones []string
		for _, ph := range c.Phones {
			if !slices.Contains(cur.Phones, ph) {
				addedPhones = append(addedPhones, ph)
			}
		}
		if !allowDuplicate {
			ids, err := s.duplicates(ctx, a.book, added, addedPhones, c.ID)
			if err != nil {
				return ContactBody{}, err
			}
			if len(ids) > 0 {
				return ContactBody{}, &DuplicateError{Contacts: ids}
			}
		}
		c.UpdatedAt = now
		actor, name := a.actor()
		ch := Change{Expect: map[uuid.UUID]time.Time{c.ID: cur.UpdatedAt}, Contacts: []Contact{c},
			Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventEdited, ActorUserID: actor, ActorName: name,
				Data: map[string]any{"fields": changed}, At: now}}}
		if len(added) > 0 || len(addedPhones) > 0 {
			items, err := s.matchItems(ctx, a.book, a.market, added, addedPhones)
			if err != nil {
				return ContactBody{}, err
			}
			existing, err := s.store.Links(ctx, c.ID)
			if err != nil {
				return ContactBody{}, err
			}
			linkChange(&ch, c.ID, items, existing, nil, "", now)
		}
		if err := s.store.Apply(ctx, ch); err != nil {
			return ContactBody{}, err
		}
		return s.detail(ctx, c)
	})
}

// Delete serves DELETE .../contacts/{id}: a soft delete.
func (s *Service) Delete(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) error {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return err
	}
	_, err = retry(func() (struct{}, error) {
		cur, err := s.contact(ctx, a, id)
		if err != nil {
			return struct{}{}, err
		}
		if !a.write {
			return struct{}{}, ErrForbidden
		}
		now := s.clock()
		c := cur
		c.DeletedAt, c.UpdatedAt = &now, now
		actor, name := a.actor()
		return struct{}{}, s.store.Apply(ctx, Change{Expect: map[uuid.UUID]time.Time{c.ID: cur.UpdatedAt}, Contacts: []Contact{c},
			Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventDeleted, ActorUserID: actor, ActorName: name, At: now}}})
	})
	return err
}

// writable loads a live contact for a change, answering 404 before 403.
func (s *Service) writable(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (access, Contact, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return access{}, Contact{}, err
	}
	c, err := s.contact(ctx, a, id)
	if err != nil {
		return access{}, Contact{}, err
	}
	if !a.write {
		return access{}, Contact{}, ErrForbidden
	}
	if c.MergedInto != nil {
		return access{}, Contact{}, ErrMerged
	}
	return a, c, nil
}

// AddNote serves POST .../contacts/{id}/notes. Notes are append-only.
func (s *Service) AddNote(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, text string) (NoteView, error) {
	a, c, err := s.writable(ctx, p, bookID, id)
	if err != nil {
		return NoteView{}, err
	}
	text = strings.TrimSpace(text)
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return NoteView{}, &ValidationError{Fields: map[string]string{"text": "is required"}}
	case n > MaxNoteLength:
		return NoteView{}, &ValidationError{Fields: map[string]string{"text": fmt.Sprintf("must be at most %d characters", MaxNoteLength)}}
	}
	actor, name := a.actor()
	n := Note{ID: newID(), ContactID: c.ID, Text: text, AuthorUserID: actor, AuthorName: name, CreatedAt: s.clock()}
	if err := s.store.Apply(ctx, Change{Notes: []Note{n}}); err != nil {
		return NoteView{}, err
	}
	return s.noteView(ctx, nil, n)
}

// Notes returns a contact's notes, newest first.
func (s *Service) Notes(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) ([]NoteView, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return nil, err
	}
	c, err := s.contact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	notes, err := s.store.Notes(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	out := make([]NoteView, 0, len(notes))
	for i := len(notes) - 1; i >= 0; i-- {
		v, _ := s.noteView(ctx, nil, notes[i])
		out = append(out, v)
	}
	return out, nil
}

// AddReminder serves POST .../contacts/{id}/reminders.
func (s *Service) AddReminder(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, dueAt *time.Time, text string) (ReminderView, error) {
	a, c, err := s.writable(ctx, p, bookID, id)
	if err != nil {
		return ReminderView{}, err
	}
	f := fields{}
	if dueAt == nil || dueAt.IsZero() {
		f["due_at"] = "is required"
	}
	text = strings.TrimSpace(text)
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		f["text"] = "is required"
	case n > MaxReminderLength:
		f["text"] = fmt.Sprintf("must be at most %d characters", MaxReminderLength)
	}
	if err := f.err(); err != nil {
		return ReminderView{}, err
	}
	now := s.clock()
	actor, name := a.actor()
	r := Reminder{ID: newID(), ContactID: c.ID, DueAt: dueAt.UTC().Truncate(time.Microsecond), Text: text, CreatedBy: actor, CreatedByName: name, CreatedAt: now}
	ev := Event{ID: newID(), ContactID: c.ID, Kind: EventReminderAdded, ActorUserID: actor, ActorName: name,
		Data: map[string]any{"reminder_id": r.ID.String(), "due_at": r.DueAt, "text": r.Text}, At: now}
	if err := s.store.Apply(ctx, Change{Reminders: []Reminder{r}, Events: []Event{ev}}); err != nil {
		return ReminderView{}, err
	}
	return s.reminderView(r, now), nil
}

// PatchReminder serves PATCH .../contacts/{id}/reminders/{rid} {done}.
func (s *Service) PatchReminder(ctx context.Context, p *auth.Principal, bookID, id, reminderID uuid.UUID, done *bool) (ReminderView, error) {
	a, c, err := s.writable(ctx, p, bookID, id)
	if err != nil {
		return ReminderView{}, err
	}
	rems, err := s.store.Reminders(ctx, c.ID)
	if err != nil {
		return ReminderView{}, err
	}
	i := slices.IndexFunc(rems, func(r Reminder) bool { return r.ID == reminderID })
	if i < 0 {
		return ReminderView{}, ErrReminderNotFound
	}
	if done == nil {
		return ReminderView{}, &ValidationError{Fields: map[string]string{"done": "is required"}}
	}
	r, now := rems[i], s.clock()
	if r.Done == *done {
		return s.reminderView(r, now), nil
	}
	actor, name := a.actor()
	kind := EventReminderReopen
	r.Done, r.DoneAt, r.DoneBy = *done, nil, nil
	if *done {
		kind, r.DoneAt, r.DoneBy = EventReminderDone, &now, actor
	}
	ev := Event{ID: newID(), ContactID: c.ID, Kind: kind, ActorUserID: actor, ActorName: name, Data: map[string]any{"reminder_id": r.ID.String(), "text": r.Text}, At: now}
	if err := s.store.Apply(ctx, Change{Reminders: []Reminder{r}, Events: []Event{ev}}); err != nil {
		return ReminderView{}, err
	}
	return s.reminderView(r, now), nil
}

// ChangeStage serves POST .../contacts/{id}/stage {stage, note?}.
func (s *Service) ChangeStage(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, stage string, note string) (ContactBody, error) {
	if _, _, err := s.writable(ctx, p, bookID, id); err != nil {
		return ContactBody{}, err
	}
	f := fields{}
	if !validStage(stage) {
		f["stage"] = "must be one of " + strings.Join(Stages(), ", ")
	}
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(note) > MaxNoteLength {
		f["note"] = fmt.Sprintf("must be at most %d characters", MaxNoteLength)
	}
	if err := f.err(); err != nil {
		return ContactBody{}, err
	}
	return retry(func() (ContactBody, error) {
		a, cur, err := s.writable(ctx, p, bookID, id)
		if err != nil {
			return ContactBody{}, err
		}
		if cur.Stage == stage {
			return s.detail(ctx, cur)
		}
		now := s.clock()
		c := cur
		c.Stage, c.UpdatedAt = stage, now
		actor, name := a.actor()
		data := map[string]any{"from": cur.Stage, "to": stage}
		if note != "" {
			data["note"] = note
		}
		if err := s.store.Apply(ctx, Change{Expect: map[uuid.UUID]time.Time{c.ID: cur.UpdatedAt}, Contacts: []Contact{c},
			Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventStageChanged, ActorUserID: actor, ActorName: name, Data: data, At: now}}}); err != nil {
			return ContactBody{}, err
		}
		return s.detail(ctx, c)
	})
}

// ChangeOwner serves POST .../contacts/{id}/owner {user_id}: an OWNER or
// MANAGER, or the current owner, gives the relationship to an active member
// who holds leads (or to nobody with null). Logged in the activity log and
// the organisation's audit trail.
func (s *Service) ChangeOwner(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, userID *uuid.UUID) (ContactBody, error) {
	return retry(func() (ContactBody, error) {
		a, cur, err := s.writable(ctx, p, bookID, id)
		if err != nil {
			return ContactBody{}, err
		}
		if a.personal {
			return ContactBody{}, ErrForbidden
		}
		mine := cur.OwnerUserID != nil && *cur.OwnerUserID == p.UserID
		if !a.manage && !mine {
			return ContactBody{}, ErrForbidden
		}
		var toName string
		if userID != nil {
			n, ok, err := s.people.Assignable(ctx, a.book.ID(), *userID)
			if err != nil {
				return ContactBody{}, err
			}
			if !ok {
				return ContactBody{}, &ValidationError{Fields: map[string]string{"user_id": "must be an active OWNER, MANAGER or AGENT of the organisation"}}
			}
			toName = n
		}
		if ptrEq(cur.OwnerUserID, userID) {
			return s.detail(ctx, cur)
		}
		var fromName string
		if cur.OwnerUserID != nil {
			if fromName, err = s.people.Name(ctx, *cur.OwnerUserID); err != nil {
				return ContactBody{}, err
			}
		}
		now := s.clock()
		c := cur
		c.OwnerUserID, c.UpdatedAt = userID, now
		actor, name := a.actor()
		before := map[string]any{"owner_user_id": idOrNil(cur.OwnerUserID), "owner_name": fromName}
		after := map[string]any{"owner_user_id": idOrNil(userID), "owner_name": toName}
		ch := Change{Expect: map[uuid.UUID]time.Time{c.ID: cur.UpdatedAt}, Contacts: []Contact{c},
			Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventOwnerChanged, ActorUserID: actor, ActorName: name,
				Data: map[string]any{"from": before, "to": after}, At: now}},
			Audit: []orgaudit.Event{orgaudit.New(a.book.ID(), p.UserID, p.Name, orgaudit.ActionContactOwnerChanged, orgaudit.TargetContact,
				c.ID, c.Name, before, after, now)}}
		if err := s.store.Apply(ctx, ch); err != nil {
			return ContactBody{}, err
		}
		return s.detail(ctx, c)
	})
}

func idOrNil(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// LinkListing serves POST .../contacts/{id}/listings {listing_id}: a listing
// of the same book only (404 LISTING_NOT_FOUND otherwise).
func (s *Service) LinkListing(ctx context.Context, p *auth.Principal, bookID, id, listingID uuid.UUID) (ContactBody, error) {
	a, c, err := s.writable(ctx, p, bookID, id)
	if err != nil {
		return ContactBody{}, err
	}
	refs, err := s.listings.Listings(ctx, []uuid.UUID{listingID})
	if err != nil {
		return ContactBody{}, err
	}
	if len(refs) == 0 || !listingInBook(refs[0], a.book) {
		return ContactBody{}, ErrListingNotFound
	}
	links, err := s.store.Links(ctx, c.ID)
	if err != nil {
		return ContactBody{}, err
	}
	if !slices.ContainsFunc(links, func(l Link) bool { return l.Kind == KindListing && l.ItemID == listingID }) {
		now := s.clock()
		actor, name := a.actor()
		ch := Change{Links: []Link{{ContactID: c.ID, Kind: KindListing, ItemID: listingID, LinkedBy: actor, LinkedAt: now}},
			Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventListingLinked, ActorUserID: actor, ActorName: name,
				Data: map[string]any{"listing_id": listingID.String(), "title": refs[0].Title, "reference": refs[0].Reference}, At: now}}}
		if err := s.store.Apply(ctx, ch); err != nil {
			return ContactBody{}, err
		}
	}
	return s.detail(ctx, c)
}

func listingInBook(l ListingRef, b Book) bool {
	if b.OrganisationID != nil {
		return l.OrganisationID != nil && *l.OrganisationID == *b.OrganisationID
	}
	return b.UserID != nil && l.OrganisationID == nil && l.SellerUserID != nil && *l.SellerUserID == *b.UserID
}

// UnlinkListing serves DELETE .../contacts/{id}/listings/{listingId}.
func (s *Service) UnlinkListing(ctx context.Context, p *auth.Principal, bookID, id, listingID uuid.UUID) error {
	a, c, err := s.writable(ctx, p, bookID, id)
	if err != nil {
		return err
	}
	links, err := s.store.Links(ctx, c.ID)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(links, func(l Link) bool { return l.Kind == KindListing && l.ItemID == listingID }) {
		return ErrListingNotFound
	}
	now := s.clock()
	actor, name := a.actor()
	return s.store.Apply(ctx, Change{Unlinks: []Link{{ContactID: c.ID, Kind: KindListing, ItemID: listingID}},
		Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventListingUnlinked, ActorUserID: actor, ActorName: name,
			Data: map[string]any{"listing_id": listingID.String()}, At: now}}})
}

// Timeline serves GET .../contacts/{id}/timeline: notes, the activity log
// (creation, edits, stage and owner changes, reminders, listing links,
// merges) and the linked property requests, business enquiries and
// appointments, newest first.
func (s *Service) Timeline(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) ([]TimelineEntry, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return nil, err
	}
	c, err := s.contact(ctx, a, id)
	if err != nil {
		return nil, err
	}
	var out []TimelineEntry
	actor := func(e *TimelineEntry, id *uuid.UUID, name string) {
		if id != nil {
			e.Actor = &PersonRef{UserID: *id, Name: name}
		}
		if name != "" {
			n := name
			e.ActorName = &n
		}
	}
	str := func(v any) string {
		s, _ := v.(string)
		return s
	}
	textOf := func(v any) *string {
		if s := str(v); s != "" {
			return &s
		}
		return nil
	}
	ref := func(kind string, raw any, reference string) *TimelineRef {
		id, err := uuid.Parse(str(raw))
		if err != nil {
			return nil
		}
		r := &TimelineRef{Kind: kind, ID: id}
		if reference != "" {
			r.Reference = &reference
		}
		return r
	}
	notes, err := s.store.Notes(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	for _, n := range notes {
		e := TimelineEntry{ID: n.ID, Kind: TimelineNoteAdded, At: n.CreatedAt, Label: "Note added", Text: textOf(n.Text),
			Data: map[string]any{"text": n.Text, "merged_from": idOrNil(n.MergedFrom)}}
		actor(&e, n.AuthorUserID, n.AuthorName)
		out = append(out, e)
	}
	events, err := s.store.Events(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		data := ev.Data
		if data == nil {
			data = map[string]any{}
		}
		e := TimelineEntry{ID: ev.ID, Kind: ev.Kind, At: ev.At, Data: data}
		actor(&e, ev.ActorUserID, ev.ActorName)
		switch ev.Kind {
		case EventItemLinked, EventDeleted:
			continue // the item itself is an entry; a deleted contact has no timeline
		case EventCreated:
			e.Label = "Contact created"
		case EventImported:
			e.Kind, e.Label = EventCreated, "Contact imported"
		case EventEdited:
			e.Label = "Details changed"
		case EventStageChanged:
			e.Label = fmt.Sprintf("Stage changed from %s to %s", StageLabel(str(data["from"])), StageLabel(str(data["to"])))
			e.Text = textOf(data["note"])
		case EventOwnerChanged:
			name := func(side any) string {
				m, _ := side.(map[string]any)
				if n := str(m["owner_name"]); n != "" {
					return n
				}
				return "nobody"
			}
			e.Label = fmt.Sprintf("Reassigned from %s to %s", name(data["from"]), name(data["to"]))
		case EventReminderAdded:
			e.Label, e.Text = "Reminder added", textOf(data["text"])
		case EventReminderDone:
			e.Label, e.Text = "Reminder done", textOf(data["text"])
		case EventReminderReopen:
			e.Label, e.Text = "Reminder reopened", textOf(data["text"])
		case EventListingLinked:
			e.Label = "Listing linked: " + str(data["title"])
			e.Ref = ref(KindListing, data["listing_id"], str(data["reference"]))
		case EventListingUnlinked:
			e.Label = "Listing unlinked"
			e.Ref = ref(KindListing, data["listing_id"], "")
		case EventMerged:
			names, _ := data["merged_names"].([]any)
			parts := make([]string, 0, len(names))
			for _, n := range names {
				parts = append(parts, str(n))
			}
			e.Label = "Merged " + strings.Join(parts, ", ") + " into this contact"
			if ids, _ := data["merged_ids"].([]any); len(ids) > 0 {
				e.Ref = ref(TimelineRefContact, ids[0], "")
			}
		case EventMergedInto:
			e.Label = "Merged into " + str(data["into_name"])
			e.Ref = ref(TimelineRefContact, data["into"], "")
		}
		out = append(out, e)
	}
	links, err := s.store.Links(ctx, c.ID)
	if err != nil {
		return nil, err
	}
	for _, k := range links {
		if k.Kind == KindListing {
			continue // LISTING_LINKED is in the activity log
		}
		it, err := s.items.Item(ctx, k.Kind, k.ItemID)
		if errors.Is(err, ErrItemNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		reference := it.Reference
		e := TimelineEntry{ID: it.ID, Kind: it.Kind, At: it.CreatedAt, Label: itemLabel(it), Ref: &TimelineRef{Kind: it.Kind, ID: it.ID, Reference: &reference},
			Data: map[string]any{"id": it.ID.String(), "reference": it.Reference, "type": it.Type, "status": it.Status, "title": it.Title,
				"listing_id": idOrNil(it.ListingID), "auto": k.Auto}}
		out = append(out, e)
	}
	SortTimeline(out)
	if out == nil {
		out = []TimelineEntry{}
	}
	return out, nil
}

// Timeline kinds that are not activity log kinds.
const (
	TimelineNoteAdded  = "NOTE_ADDED"
	TimelineRefContact = "CONTACT"
)

func itemLabel(it Item) string {
	what := map[string]string{KindPropertyRequest: "Property request", KindBusinessEnquiry: "Business enquiry", KindAppointment: "Appointment",
		KindApplication: "Application"}[it.Kind]
	label := what + " " + it.Reference
	if it.Title != "" {
		label += ": " + it.Title
	}
	return label
}

// SortTimeline orders entries newest first (ties: the larger ID first).
func SortTimeline(out []TimelineEntry) {
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.After(out[j].At)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
}

// Merge serves POST .../contacts/merge {keep_id, merge_ids}: the merged
// contacts' emails, phones, tags, notes, reminders, links and activity are
// moved onto keep; they stay as tombstones pointing at it.
func (s *Service) Merge(ctx context.Context, p *auth.Principal, bookID, keepID uuid.UUID, mergeIDs []uuid.UUID) (ContactBody, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return ContactBody{}, err
	}
	f := fields{}
	if keepID == uuid.Nil {
		f["keep_id"] = "is required"
	}
	var from []uuid.UUID
	for _, id := range mergeIDs {
		if id == keepID {
			f["merge_ids"] = "must not contain keep_id"
		}
		if !slices.Contains(from, id) {
			from = append(from, id)
		}
	}
	switch {
	case len(from) == 0:
		f["merge_ids"] = "must name at least one contact"
	case len(from) > MaxMergeIDs:
		f["merge_ids"] = fmt.Sprintf("may name at most %d contacts", MaxMergeIDs)
	}
	if err := f.err(); err != nil {
		return ContactBody{}, err
	}
	return retry(func() (ContactBody, error) {
		keep, err := s.contact(ctx, a, keepID)
		if err != nil {
			return ContactBody{}, err
		}
		others := make([]Contact, 0, len(from))
		for _, id := range from {
			c, err := s.contact(ctx, a, id)
			if err != nil {
				return ContactBody{}, err
			}
			others = append(others, c)
		}
		if !a.write {
			return ContactBody{}, ErrForbidden
		}
		if keep.MergedInto != nil || slices.ContainsFunc(others, func(c Contact) bool { return c.MergedInto != nil }) {
			return ContactBody{}, ErrMerged
		}
		now := s.clock()
		actor, name := a.actor()
		merged := keep
		expect := map[uuid.UUID]time.Time{keep.ID: keep.UpdatedAt}
		ch := Change{Merge: &Merge{Keep: keep.ID, From: from}}
		names := make([]string, 0, len(others))
		ids := make([]string, 0, len(others))
		var latestMarketing *Marketing
		for _, o := range others {
			expect[o.ID] = o.UpdatedAt
			names, ids = append(names, o.Name), append(ids, o.ID.String())
			for _, e := range o.Emails {
				if !slices.Contains(merged.Emails, e) {
					merged.Emails = append(merged.Emails, e)
				}
			}
			for _, ph := range o.Phones {
				if !slices.Contains(merged.Phones, ph) {
					merged.Phones = append(merged.Phones, ph)
				}
			}
			for _, t := range o.Tags {
				if !slices.Contains(merged.Tags, t) {
					merged.Tags = append(merged.Tags, t)
				}
			}
			if merged.OwnerUserID == nil {
				merged.OwnerUserID = o.OwnerUserID
			}
			if merged.Source == "" {
				merged.Source = o.Source
			}
			if o.Marketing != nil && (latestMarketing == nil || o.Marketing.GrantedAt.After(latestMarketing.GrantedAt)) {
				latestMarketing = o.Marketing
			}
			if o.LastContactAt != nil && (merged.LastContactAt == nil || o.LastContactAt.After(*merged.LastContactAt)) {
				merged.LastContactAt = o.LastContactAt
			}
			t := o
			into := keep.ID
			t.MergedInto, t.UpdatedAt = &into, now
			ch.Contacts = append(ch.Contacts, t)
			ch.Events = append(ch.Events, Event{ID: newID(), ContactID: o.ID, Kind: EventMergedInto, ActorUserID: actor, ActorName: name,
				Data: map[string]any{"into": keep.ID.String(), "into_name": keep.Name}, At: now})
		}
		if merged.Marketing == nil {
			merged.Marketing = latestMarketing
		}
		merged.UpdatedAt = now
		ch.Expect = expect
		ch.Contacts = append([]Contact{merged}, ch.Contacts...)
		ch.Events = append(ch.Events, Event{ID: newID(), ContactID: keep.ID, Kind: EventMerged, ActorUserID: actor, ActorName: name,
			Data: map[string]any{"merged_ids": ids, "merged_names": names}, At: now})
		if a.book.OrganisationID != nil {
			ch.Audit = []orgaudit.Event{orgaudit.New(a.book.ID(), p.UserID, p.Name, orgaudit.ActionContactsMerged, orgaudit.TargetContact,
				keep.ID, keep.Name, map[string]any{"merged_ids": ids, "merged_names": names}, map[string]any{"kept_id": keep.ID.String()}, now)}
		}
		if err := s.store.Apply(ctx, ch); err != nil {
			return ContactBody{}, err
		}
		got, err := s.store.Get(ctx, keep.ID)
		if err != nil {
			return ContactBody{}, err
		}
		return s.detail(ctx, got)
	})
}

// FromItem serves POST .../contacts/from-request/{requestId}: a contact is
// created from a property request, business enquiry or appointment the
// caller can see, or the item is linked to the existing contact that shares
// its email or phone. kind may be "" (every kind is tried).
func (s *Service) FromItem(ctx context.Context, p *auth.Principal, bookID, itemID uuid.UUID, kind, contactType string) (ContactBody, bool, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return ContactBody{}, false, err
	}
	kinds := ItemKinds()
	if kind != "" {
		if !slices.Contains(kinds, kind) {
			return ContactBody{}, false, &ValidationError{Fields: map[string]string{"kind": "must be one of " + strings.Join(kinds, ", ")}}
		}
		kinds = []string{kind}
	}
	var it Item
	found := false
	for _, k := range kinds {
		got, err := s.items.Item(ctx, k, itemID)
		if errors.Is(err, ErrItemNotFound) {
			continue
		}
		if err != nil {
			return ContactBody{}, false, err
		}
		if visible(a, got) {
			it, found = got, true
			break
		}
	}
	if !found {
		return ContactBody{}, false, ErrItemNotFound
	}
	if !a.write {
		return ContactBody{}, false, ErrForbidden
	}
	if contactType == "" {
		contactType = TypeOther
	}
	f := fields{}
	if err := s.checkCode(ctx, SetContactTypes, contactType, f, "type"); err != nil {
		return ContactBody{}, false, err
	}
	if err := f.err(); err != nil {
		return ContactBody{}, false, err
	}
	// Matching uses what the customer entered and their verified account
	// email; a new contact gets what they entered (the account email when
	// they entered none).
	all, phones := identifiers(it, a.market)
	existing, err := s.store.Matching(ctx, a.book, all, phones)
	if err != nil {
		return ContactBody{}, false, err
	}
	if len(existing) > 0 {
		return s.linkExisting(ctx, a, existing, it)
	}
	emails, _ := identifiers(Item{Email: it.Email}, "")
	if len(emails) == 0 {
		emails = all
	}
	now := s.clock()
	actor, name := a.actor()
	c := Contact{ID: newID(), Book: a.book, Name: strings.TrimSpace(it.Name), Type: contactType, Stage: StageNew, Source: SourceEnquiry,
		Emails: emails, Phones: phones, CreatedBy: actor, CreatedAt: now, UpdatedAt: now, LastContactAt: nil}
	if c.Name == "" {
		c.Name = "Unnamed contact"
	}
	if utf8.RuneCountInString(c.Name) > MaxNameLength {
		c.Name = string([]rune(c.Name)[:MaxNameLength])
	}
	if a.personal || auth.RoleHoldsLeads(a.role) {
		c.OwnerUserID = actor
	}
	if valid, _ := s.vocab.Labels(ctx, SetContactSources); valid != nil {
		if _, ok := valid[SourceEnquiry]; !ok {
			c.Source = ""
		}
	}
	ch := Change{Contacts: []Contact{c}, Events: []Event{{ID: newID(), ContactID: c.ID, Kind: EventCreated, ActorUserID: actor, ActorName: name,
		Data: map[string]any{"stage": c.Stage, "from": map[string]any{"kind": it.Kind, "id": it.ID.String(), "reference": it.Reference}}, At: now}}}
	items, err := s.matchItems(ctx, a.book, a.market, all, phones)
	if err != nil {
		return ContactBody{}, false, err
	}
	if !slices.ContainsFunc(items, func(x Item) bool { return x.Kind == it.Kind && x.ID == it.ID }) {
		items = append([]Item{it}, items...)
	}
	linkChange(&ch, c.ID, items, nil, actor, name, now)
	for i := range ch.Links {
		if ch.Links[i].Kind == it.Kind && ch.Links[i].ItemID == it.ID {
			ch.Links[i].Auto, ch.Links[i].LinkedBy = false, actor
		}
	}
	if err := s.store.Apply(ctx, ch); err != nil {
		return ContactBody{}, false, err
	}
	d, err := s.detail(ctx, c)
	return d, true, err
}

func (s *Service) linkExisting(ctx context.Context, a access, existing []Contact, it Item) (ContactBody, bool, error) {
	sort.SliceStable(existing, func(i, j int) bool { return existing[i].CreatedAt.Before(existing[j].CreatedAt) })
	c := existing[0]
	links, err := s.store.Links(ctx, c.ID)
	if err != nil {
		return ContactBody{}, false, err
	}
	actor, name := a.actor()
	ch := Change{}
	linkChange(&ch, c.ID, []Item{it}, links, actor, name, s.clock())
	for i := range ch.Links {
		ch.Links[i].Auto, ch.Links[i].LinkedBy = false, actor
	}
	if len(ch.Links) > 0 {
		if err := s.store.Apply(ctx, ch); err != nil {
			return ContactBody{}, false, err
		}
	}
	d, err := s.detail(ctx, c)
	return d, false, err
}

// visible reports whether the caller can see an item in their inbox: an
// OWNER or MANAGER every item of the organisation, another member the
// organisation inbox's and their own, a direct owner their own.
func visible(a access, it Item) bool {
	if a.personal {
		return it.OrganisationID == nil && it.DirectOwnerUserID != nil && *it.DirectOwnerUserID == a.p.UserID
	}
	if it.OrganisationID == nil || *it.OrganisationID != a.book.ID() {
		return false
	}
	if a.manage {
		return true
	}
	// The enquiry inbox shows members their own only; so does the
	// applications inbox (the assigned agent's listings).
	if it.Kind == KindBusinessEnquiry || it.Kind == KindApplication {
		return it.AssignedUserID != nil && *it.AssignedUserID == a.p.UserID
	}
	return it.AssignedUserID == nil || *it.AssignedUserID == a.p.UserID
}

// Due periods of GET .../contacts/reminders.
const (
	DueOverdue = "overdue"
	DueToday   = "today"
	DueWeek    = "week"
)

var (
	zonesOnce sync.Once
	zones     map[string]string
)

// zoneOf returns the time zone of a market (UTC when unknown).
func zoneOf(code string) *time.Location {
	zonesOnce.Do(func() {
		zones = map[string]string{}
		for _, m := range markets.Seed() {
			zones[m.Code] = m.Timezone
		}
	})
	if name, ok := zones[code]; ok {
		if loc, err := time.LoadLocation(name); err == nil {
			return loc
		}
	}
	return time.UTC
}

// DueBefore returns the end of a due period: now for overdue, the end of
// the market-local day for today, the end of the sixth day after it for week.
func DueBefore(due string, now time.Time, market string) (time.Time, bool) {
	local := now.In(zoneOf(market))
	endOfDay := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).AddDate(0, 0, 1)
	switch due {
	case DueOverdue:
		return now, true
	case DueToday, "":
		return endOfDay.UTC(), true
	case DueWeek:
		return endOfDay.AddDate(0, 0, 6).UTC(), true
	}
	return time.Time{}, false
}

// DueReminders serves GET .../contacts/reminders?due=today|overdue|week:
// the open reminders of the book due before the end of the period (overdue
// ones included), soonest first. mine keeps the reminders the caller created
// or whose contact they own.
func (s *Service) DueReminders(ctx context.Context, p *auth.Principal, bookID uuid.UUID, due string, mine bool) ([]DueReminderView, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return nil, err
	}
	now := s.clock()
	before, ok := DueBefore(due, now, a.market)
	if !ok {
		return nil, &ValidationError{Fields: map[string]string{"due": "must be today, overdue or week"}}
	}
	rows, err := s.store.DueReminders(ctx, a.book, before)
	if err != nil {
		return nil, err
	}
	l, err := s.labels(ctx)
	if err != nil {
		return nil, err
	}
	out := []DueReminderView{}
	for _, r := range rows {
		if mine && !(ptrEq(r.CreatedBy, &p.UserID) || ptrEq(r.OwnerUserID, &p.UserID)) {
			continue
		}
		owner, err := s.person(ctx, l, r.OwnerUserID)
		if err != nil {
			return nil, err
		}
		out = append(out, DueReminderView{ReminderView: s.reminderView(r.Reminder, now), Contact: ContactRef{ID: r.ContactID, Name: r.ContactName, Owner: owner}})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Worker
// ---------------------------------------------------------------------------

// LinkItem links one property request, business enquiry or appointment to
// the live contacts of its book that share its email or phone. Idempotent.
func (s *Service) LinkItem(ctx context.Context, kind string, id uuid.UUID) (int, error) {
	it, err := s.items.Item(ctx, kind, id)
	if errors.Is(err, ErrItemNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	book, ok := it.Book()
	if !ok {
		return 0, nil // a routing exception reaches no book
	}
	emails, phones := identifiers(it, "")
	if len(emails) == 0 && len(phones) == 0 {
		return 0, nil
	}
	contacts, err := s.store.Matching(ctx, book, emails, phones)
	if err != nil {
		return 0, err
	}
	now := s.clock()
	ch := Change{}
	for _, c := range contacts {
		links, err := s.store.Links(ctx, c.ID)
		if err != nil {
			return 0, err
		}
		linkChange(&ch, c.ID, []Item{it}, links, nil, "", now)
	}
	if len(ch.Links) == 0 {
		return 0, nil
	}
	return len(ch.Links), s.store.Apply(ctx, ch)
}

// SweepReminders claims the reminders that fell due and writes their
// CONTACT_REMINDER_DUE outbox events. It returns how many.
func (s *Service) SweepReminders(ctx context.Context) (int, error) {
	total := 0
	for {
		got, err := s.store.ClaimDueReminders(ctx, s.clock(), 200)
		total += len(got)
		if err != nil || len(got) < 200 {
			return total, err
		}
	}
}

// Tags serves GET .../contacts/tags: the book's tags with their counts.
func (s *Service) Tags(ctx context.Context, p *auth.Principal, bookID uuid.UUID) ([]TagCount, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return nil, err
	}
	return s.store.Tags(ctx, a.book)
}

// Assignees serves GET .../contacts/assignees: the members a contact may be
// given to (active OWNER, MANAGER and AGENT), for anyone who may change the
// book (an AGENT reassigning a contact they own included). A personal book
// lists its owner.
func (s *Service) Assignees(ctx context.Context, p *auth.Principal, bookID uuid.UUID) ([]Assignee, error) {
	a, err := s.access(ctx, p, bookID)
	if err != nil {
		return nil, err
	}
	if !a.write {
		return nil, ErrForbidden
	}
	if a.personal {
		return []Assignee{{UserID: p.UserID, Name: p.Name}}, nil
	}
	if s.members == nil {
		return []Assignee{}, nil
	}
	all, err := s.members(ctx, a.book.ID())
	if err != nil {
		return nil, err
	}
	out := []Assignee{}
	for _, m := range all {
		if auth.RoleHoldsLeads(m.Role) {
			out = append(out, m)
		}
	}
	return out, nil
}
