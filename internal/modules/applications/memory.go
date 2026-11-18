package applications

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/orgaudit"
	"rightproperty365.com/backend/internal/platform/outbox"
)

// MemoryStore is the in-memory Store (development and tests).
type MemoryStore struct {
	mu      sync.Mutex
	forms   map[uuid.UUID]Form
	intakes map[uuid.UUID]Intake
	apps    map[uuid.UUID]Application
	files   map[uuid.UUID]File
	events  map[uuid.UUID][]Event
	notes   map[uuid.UUID][]Note
	purges  []string
	refs    map[int]int
	// titles names the listings for ?q= (nil: no title search).
	titles func(ids []uuid.UUID) map[uuid.UUID]string
	sink   outbox.Sink
	audit  orgaudit.Sink
	// outboxLog keeps every event written (tests).
	outboxLog []outbox.Event
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{forms: map[uuid.UUID]Form{}, intakes: map[uuid.UUID]Intake{}, apps: map[uuid.UUID]Application{},
		files: map[uuid.UUID]File{}, events: map[uuid.UUID][]Event{}, notes: map[uuid.UUID][]Note{}, refs: map[int]int{}}
}

// SetOutboxSink receives every outbox event written (development: the API
// handles them in-process).
func (s *MemoryStore) SetOutboxSink(sink outbox.Sink) { s.sink = sink }

// SetAuditSink receives the organisation audit events.
func (s *MemoryStore) SetAuditSink(sink orgaudit.Sink) { s.audit = sink }

// SetListingTitles lets ?q= match listing titles.
func (s *MemoryStore) SetListingTitles(titles func(ids []uuid.UUID) map[uuid.UUID]string) {
	s.titles = titles
}

// OutboxEvents returns every outbox event written (test helper).
func (s *MemoryStore) OutboxEvents() []outbox.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]outbox.Event{}, s.outboxLog...)
}

// AllApplications returns every application (CRM items, tests).
func (s *MemoryStore) AllApplications() []Application {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Application, 0, len(s.apps))
	for _, a := range s.apps {
		out = append(out, copyApp(a))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// PendingPurgeKeys returns the queued object keys (test helper).
func (s *MemoryStore) PendingPurgeKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.purges...)
}

func copyForm(f Form) Form {
	f.Sections = cloneSections(f.Sections)
	return f
}

func cloneSections(in []Section) []Section {
	if in == nil {
		return nil
	}
	out := make([]Section, len(in))
	for i, s := range in {
		s.Fields = append([]Field{}, s.Fields...)
		out[i] = s
	}
	return out
}

func copyApp(a Application) Application {
	a.Answers = a.Answers.clone()
	return a
}

func (s *MemoryStore) submissions(formID uuid.UUID) int {
	n := 0
	for _, a := range s.apps {
		if a.FormID == formID && a.SubmittedAt != nil {
			n++
		}
	}
	return n
}

func (s *MemoryStore) withCount(f Form) Form {
	f = copyForm(f)
	f.Submissions = s.submissions(f.ID)
	return f
}

// CreateForm implements Store.
func (s *MemoryStore) CreateForm(_ context.Context, f Form) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.forms[f.ID]; ok {
		return fmt.Errorf("form %s exists", f.ID)
	}
	s.forms[f.ID] = copyForm(f)
	return nil
}

// PutForm stores a form as it is (seed).
func (s *MemoryStore) PutForm(f Form) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forms[f.ID] = copyForm(f)
}

// Form implements Store.
func (s *MemoryStore) Form(_ context.Context, id uuid.UUID) (Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[id]
	if !ok {
		return Form{}, ErrFormNotFound
	}
	return s.withCount(f), nil
}

// Forms implements Store.
func (s *MemoryStore) Forms(_ context.Context, book Book, status, market string, familyID *uuid.UUID) ([]Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Form{}
	for _, f := range s.forms {
		if !f.Book().Same(book) || (status != "" && f.Status != status) || (market != "" && f.MarketCode != market) ||
			(familyID != nil && f.FamilyID != *familyID) {
			continue
		}
		out = append(out, s.withCount(f))
	}
	sortForms(out)
	return out, nil
}

func sortForms(out []Form) {
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Version > out[j].Version
	})
}

// UpdateDraft implements Store. Like the database trigger, it refuses to
// change a version that has a submission.
func (s *MemoryStore) UpdateDraft(_ context.Context, f Form) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.forms[f.ID]
	switch {
	case !ok:
		return ErrFormNotFound
	case s.submissions(f.ID) > 0:
		return ErrFormLocked
	case cur.Status != FormDraft:
		return ErrFormState
	}
	f.Status, f.Version, f.FamilyID = cur.Status, cur.Version, cur.FamilyID
	s.forms[f.ID] = copyForm(f)
	return nil
}

// CreateVersion implements Store.
func (s *MemoryStore) CreateVersion(_ context.Context, f Form) (Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	top := 0
	for _, o := range s.forms {
		if o.FamilyID != f.FamilyID {
			continue
		}
		if o.Status == FormDraft {
			return Form{}, ErrDraftExists
		}
		top = max(top, o.Version)
	}
	f.Version = top + 1
	s.forms[f.ID] = copyForm(f)
	return copyForm(f), nil
}

// Publish implements Store.
func (s *MemoryStore) Publish(_ context.Context, id uuid.UUID, at time.Time) (Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[id]
	if !ok {
		return Form{}, ErrFormNotFound
	}
	if f.Status != FormDraft {
		return Form{}, ErrFormState
	}
	for oid, o := range s.forms {
		if o.FamilyID == f.FamilyID && o.Status == FormPublished {
			o.Status, o.RetiredAt, o.UpdatedAt = FormRetired, &at, at
			s.forms[oid] = o
			for lid, in := range s.intakes {
				if in.FormID == oid {
					in.FormID = id
					s.intakes[lid] = in
				}
			}
		}
	}
	f.Status, f.PublishedAt, f.UpdatedAt = FormPublished, &at, at
	s.forms[id] = f
	return s.withCount(f), nil
}

// Retire implements Store.
func (s *MemoryStore) Retire(_ context.Context, id uuid.UUID, at time.Time) (Form, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.forms[id]
	if !ok {
		return Form{}, ErrFormNotFound
	}
	if f.Status == FormRetired {
		return Form{}, ErrFormState
	}
	f.Status, f.RetiredAt, f.UpdatedAt = FormRetired, &at, at
	s.forms[id] = f
	return s.withCount(f), nil
}

// Intake implements Store.
func (s *MemoryStore) Intake(_ context.Context, listingID uuid.UUID) (Intake, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	in, ok := s.intakes[listingID]
	return in, ok, nil
}

// Intakes implements Store.
func (s *MemoryStore) Intakes(_ context.Context, ids []uuid.UUID) (map[uuid.UUID]Intake, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[uuid.UUID]Intake{}
	for _, id := range ids {
		if in, ok := s.intakes[id]; ok {
			out[id] = in
		}
	}
	return out, nil
}

// PutIntake implements Store.
func (s *MemoryStore) PutIntake(_ context.Context, in Intake) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.forms[in.FormID]; !ok {
		return ErrFormNotFound
	}
	s.intakes[in.ListingID] = in
	return nil
}

// DeleteIntake implements Store.
func (s *MemoryStore) DeleteIntake(_ context.Context, listingID uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.intakes, listingID)
	return nil
}

// Application implements Store.
func (s *MemoryStore) Application(_ context.Context, id uuid.UUID) (Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok {
		return Application{}, ErrNotFound
	}
	return copyApp(a), nil
}

// Existing implements Store.
func (s *MemoryStore) Existing(_ context.Context, applicant, listingID, formID uuid.UUID) (Application, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.apps {
		if a.ApplicantUserID != nil && *a.ApplicantUserID == applicant && a.ListingID == listingID && a.FormID == formID {
			return copyApp(a), true, nil
		}
	}
	return Application{}, false, nil
}

func sortApps(out []Application) {
	sort.SliceStable(out, func(i, j int) bool {
		ti, tj := out[i].UpdatedAt, out[j].UpdatedAt
		if out[i].LastSubmittedAt != nil {
			ti = *out[i].LastSubmittedAt
		}
		if out[j].LastSubmittedAt != nil {
			tj = *out[j].LastSubmittedAt
		}
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		return out[i].ID.String() > out[j].ID.String()
	})
}

func page(list []Application, limit, offset int) []Application {
	start := min(max(offset, 0), len(list))
	end := len(list)
	if limit > 0 {
		end = min(start+limit, len(list))
	}
	return list[start:end]
}

// Mine implements Store.
func (s *MemoryStore) Mine(_ context.Context, applicant uuid.UUID, status string, limit, offset int) ([]Application, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Application
	for _, a := range s.apps {
		if a.ApplicantUserID != nil && *a.ApplicantUserID == applicant && (status == "" || a.Status == status) {
			out = append(out, copyApp(a))
		}
	}
	sortApps(out)
	return page(out, limit, offset), len(out), nil
}

// Received implements Store.
func (s *MemoryStore) Received(_ context.Context, f AppFilter) ([]Application, int, map[string]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []Application
	for _, a := range s.apps {
		if a.Status == StatusDraft || !a.Book().Same(f.Book) {
			continue
		}
		if f.ListingIDs != nil && !slices.Contains(f.ListingIDs, a.ListingID) {
			continue
		}
		if f.ListingID != nil && a.ListingID != *f.ListingID {
			continue
		}
		candidates = append(candidates, copyApp(a))
	}
	if q := strings.ToLower(strings.TrimSpace(f.Q)); q != "" {
		var titles map[uuid.UUID]string
		if s.titles != nil {
			ids := make([]uuid.UUID, 0, len(candidates))
			for _, a := range candidates {
				ids = append(ids, a.ListingID)
			}
			s.mu.Unlock()
			titles = s.titles(ids)
			s.mu.Lock()
		}
		kept := candidates[:0]
		for _, a := range candidates {
			if strings.Contains(strings.ToLower(a.Reference), q) || strings.Contains(strings.ToLower(a.ApplicantName), q) ||
				strings.Contains(strings.ToLower(titles[a.ListingID]), q) || strings.Contains(strings.ToLower(a.Listing.Title), q) {
				kept = append(kept, a)
			}
		}
		candidates = kept
	}
	counts := map[string]int{}
	var out []Application
	for _, a := range candidates {
		counts[a.Status]++
		if f.Status == "" || a.Status == f.Status {
			out = append(out, a)
		}
	}
	sortApps(out)
	return page(out, f.Limit, f.Offset), len(out), counts, nil
}

// Apply implements Store.
func (s *MemoryStore) Apply(_ context.Context, ch Change) (Application, error) {
	s.mu.Lock()
	var out Application
	if a := ch.Application; a != nil {
		cur, exists := s.apps[a.ID]
		switch {
		case ch.Insert && exists:
			s.mu.Unlock()
			return Application{}, ErrConflict
		case ch.Insert:
			for _, o := range s.apps {
				if o.ApplicantUserID != nil && a.ApplicantUserID != nil && *o.ApplicantUserID == *a.ApplicantUserID &&
					o.ListingID == a.ListingID && o.FormID == a.FormID {
					s.mu.Unlock()
					return Application{}, ErrConflict
				}
			}
		case !exists:
			s.mu.Unlock()
			return Application{}, ErrNotFound
		case !ch.Expect.IsZero() && !cur.UpdatedAt.Equal(ch.Expect):
			s.mu.Unlock()
			return Application{}, ErrConflict
		}
		next := copyApp(*a)
		if ch.NextReference && next.Reference == "" {
			year := next.CreatedAt.Year()
			if next.LastSubmittedAt != nil {
				year = next.LastSubmittedAt.Year()
			}
			s.refs[year]++
			next.Reference = fmt.Sprintf("%s-%d-%06d", ReferencePrefix, year, s.refs[year])
		}
		s.apps[next.ID] = next
		out = copyApp(next)
	}
	for _, f := range ch.AddFiles {
		s.files[f.ID] = f
	}
	for _, id := range ch.RemoveFiles {
		if f, ok := s.files[id]; ok {
			s.purges = append(s.purges, f.ObjectKey)
			delete(s.files, id)
		}
	}
	for _, e := range ch.Events {
		s.events[e.ApplicationID] = append(s.events[e.ApplicationID], e)
	}
	for _, n := range ch.Notes {
		s.notes[n.ApplicationID] = append(s.notes[n.ApplicationID], n)
	}
	var written []outbox.Event
	for _, o := range ch.Outbox {
		ev := outbox.Event{ID: o.ID, Type: o.Type, AggregateType: AggregateApplication, AggregateID: o.Payload.ApplicationID,
			Payload: o.PayloadJSON(), CreatedAt: o.Payload.OccurredAt}
		s.outboxLog = append(s.outboxLog, ev)
		written = append(written, ev)
	}
	sink, audit := s.sink, s.audit
	s.mu.Unlock()
	if audit != nil {
		for _, e := range ch.Audit {
			audit(e)
		}
	}
	if sink != nil {
		for _, ev := range written {
			sink(ev)
		}
	}
	return out, nil
}

// PutApplication stores an application as it is (seed), with its events and files.
func (s *MemoryStore) PutApplication(a Application, events []Event, files []File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.apps[a.ID] = copyApp(a)
	s.events[a.ID] = append([]Event{}, events...)
	for _, f := range files {
		s.files[f.ID] = f
	}
}

// PutIntakeRecord stores an intake as it is (seed).
func (s *MemoryStore) PutIntakeRecord(in Intake) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intakes[in.ListingID] = in
}

// Files implements Store.
func (s *MemoryStore) Files(_ context.Context, applicationID uuid.UUID) ([]File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []File{}
	for _, f := range s.files {
		if f.ApplicationID == applicationID {
			out = append(out, f)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].UploadedAt.Equal(out[j].UploadedAt) {
			return out[i].UploadedAt.Before(out[j].UploadedAt)
		}
		return out[i].ID.String() < out[j].ID.String()
	})
	return out, nil
}

// Events implements Store (oldest first).
func (s *MemoryStore) Events(_ context.Context, applicationID uuid.UUID) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Event{}, s.events[applicationID]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Notes implements Store (oldest first).
func (s *MemoryStore) Notes(_ context.Context, applicationID uuid.UUID) ([]Note, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Note{}, s.notes[applicationID]...), nil
}

// RecordAudit implements Store.
func (s *MemoryStore) RecordAudit(_ context.Context, events []orgaudit.Event) error {
	if s.audit != nil {
		for _, e := range events {
			s.audit(e)
		}
	}
	return nil
}

// DueForErasure implements Store.
func (s *MemoryStore) DueForErasure(_ context.Context, now time.Time, limit int) ([]Application, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Application
	for _, a := range s.apps {
		f, ok := s.forms[a.FormID]
		if !ok || a.ErasedAt != nil || !Final(a.Status) || a.DecidedAt == nil {
			continue
		}
		if !a.DecidedAt.AddDate(0, 0, f.RetentionDays).After(now) {
			out = append(out, copyApp(a))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DecidedAt.Before(*out[j].DecidedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// eraseLocked removes the answers and files of one application.
func (s *MemoryStore) eraseLocked(a Application, reason string, at time.Time) Application {
	for id, f := range s.files {
		if f.ApplicationID == a.ID {
			s.purges = append(s.purges, f.ObjectKey)
			delete(s.files, id)
		}
	}
	a.Answers, a.ErasedAt, a.UpdatedAt = Answers{}, &at, at
	s.apps[a.ID] = a
	s.events[a.ID] = append(s.events[a.ID], Event{ID: newID(), ApplicationID: a.ID, Kind: EventErased, Message: reason, ActorType: ActorSystem, At: at})
	return a
}

// Erase implements Store.
func (s *MemoryStore) Erase(_ context.Context, id uuid.UUID, reason string, at time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.apps[id]
	if !ok || a.ErasedAt != nil {
		return false, nil
	}
	s.eraseLocked(a, reason, at)
	return true, nil
}

// EraseApplicant is the account deletion of an applicant: their DRAFTs are
// deleted; every other application is withdrawn when not final, its answers
// and files erased and the applicant unlinked. It returns how many
// applications were deleted and anonymised.
func (s *MemoryStore) EraseApplicant(userID uuid.UUID, at time.Time) (deleted, anonymised int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, a := range s.apps {
		if a.ApplicantUserID == nil || *a.ApplicantUserID != userID {
			continue
		}
		if a.Status == StatusDraft {
			for fid, f := range s.files {
				if f.ApplicationID == id {
					s.purges = append(s.purges, f.ObjectKey)
					delete(s.files, fid)
				}
			}
			delete(s.apps, id)
			delete(s.events, id)
			delete(s.notes, id)
			deleted++
			continue
		}
		if !Final(a.Status) {
			s.events[id] = append(s.events[id], Event{ID: newID(), ApplicationID: id, Kind: EventWithdrawn, FromStatus: a.Status, ToStatus: StatusWithdrawn,
				ActorType: ActorSystem, At: at})
			a.Status, a.DecidedAt = StatusWithdrawn, &at
		}
		if a.ErasedAt == nil {
			a = s.eraseLocked(a, ErasedByAccountDeletion, at)
		}
		a.ApplicantUserID, a.ApplicantName = nil, DeletedApplicantName
		s.apps[id] = a
		anonymised++
	}
	return deleted, anonymised
}

// PendingPurges implements Store.
func (s *MemoryStore) PendingPurges(_ context.Context, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]string{}, s.purges...)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// DonePurges implements Store.
func (s *MemoryStore) DonePurges(_ context.Context, keys []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purges = slices.DeleteFunc(s.purges, func(k string) bool { return slices.Contains(keys, k) })
	return nil
}
