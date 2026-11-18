package applications

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// Organisation audit actions and target type (FR-018).
const (
	ActionApplicationViewed     = orgaudit.ActionApplicationViewed
	ActionApplicationFileViewed = orgaudit.ActionApplicationFileViewed
)

// received is who is reading a book's applications.
type received struct {
	p    *auth.Principal
	book Book
	// all: OWNER, MANAGER or the direct owner; otherwise an AGENT, who sees
	// the applications of the listings they are the assigned agent of.
	all bool
}

// receivedAccess resolves the book: ErrOrganisationUnknown for somebody who
// may see none of its applications (not a member, STAFF, another kind).
func receivedAccess(p *auth.Principal, bookID uuid.UUID) (received, error) {
	switch {
	case p == nil:
		return received{}, ErrUnauthenticated
	case bookID == p.UserID && p.IsDirectOwner():
		return received{p: p, book: PersonalBook(bookID), all: true}, nil
	case p.MaySeeAllApplications(bookID):
		return received{p: p, book: OrganisationBook(bookID), all: true}, nil
	case p.MembershipRole(bookID) == auth.RoleAgent && p.MayReadForms(bookID):
		return received{p: p, book: OrganisationBook(bookID)}, nil
	}
	return received{}, ErrOrganisationUnknown
}

// one returns an application the caller may see (ErrNotFound otherwise:
// another organisation's, a DRAFT, another agent's listing).
func (s *Service) one(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (received, Application, ListingInfo, error) {
	r, err := receivedAccess(p, bookID)
	if errors.Is(err, ErrOrganisationUnknown) {
		return received{}, Application{}, ListingInfo{}, ErrNotFound
	}
	if err != nil {
		return received{}, Application{}, ListingInfo{}, err
	}
	a, err := s.store.Application(ctx, id)
	if err != nil {
		return received{}, Application{}, ListingInfo{}, err
	}
	if !a.Book().Same(r.book) || a.Status == StatusDraft {
		return received{}, Application{}, ListingInfo{}, ErrNotFound
	}
	known, err := s.listings.Listings(ctx, []uuid.UUID{a.ListingID})
	if err != nil {
		return received{}, Application{}, ListingInfo{}, err
	}
	l := known[a.ListingID]
	if !r.all && !p.MayReadApplication(bookID, l.AgentUserID) {
		return received{}, Application{}, ListingInfo{}, ErrNotFound
	}
	return r, a, l, nil
}

// Received lists the applications a book received (never DRAFTs).
func (s *Service) Received(ctx context.Context, p *auth.Principal, bookID uuid.UUID, f AppFilter, page, size int) (ReceivedPage, error) {
	r, err := receivedAccess(p, bookID)
	if err != nil {
		return ReceivedPage{}, err
	}
	f.Book, f.Limit, f.Offset = r.book, size, (page-1)*size
	if !r.all {
		ids, err := s.listings.AgentListingIDs(ctx, bookID, p.UserID)
		if err != nil {
			return ReceivedPage{}, err
		}
		if ids == nil {
			ids = []uuid.UUID{}
		}
		f.ListingIDs = ids
	}
	list, total, counts, err := s.store.Received(ctx, f)
	if err != nil {
		return ReceivedPage{}, err
	}
	for _, st := range Statuses() {
		if _, ok := counts[st]; !ok && st != StatusDraft {
			counts[st] = 0
		}
	}
	out := ReceivedPage{Items: make([]AgentView, 0, len(list)), Page: page, PageSize: size, Total: total, TotalPages: totalPages(total, size), StatusCounts: counts}
	for _, a := range list {
		v, err := s.agentView(ctx, a, false)
		if err != nil {
			return ReceivedPage{}, err
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

// ReceivedOne returns one received application with its answers rendered
// against its submission version, and records APPLICATION_VIEWED.
func (s *Service) ReceivedOne(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID) (AgentView, error) {
	r, a, _, err := s.one(ctx, p, bookID, id)
	if err != nil {
		return AgentView{}, err
	}
	v, err := s.agentView(ctx, a, true)
	if err != nil {
		return AgentView{}, err
	}
	if err := s.recordView(ctx, r, a, orgaudit.ActionApplicationViewed, nil); err != nil {
		return AgentView{}, err
	}
	return v, nil
}

// ReceivedFile opens one file of a received application and records
// APPLICATION_FILE_VIEWED.
func (s *Service) ReceivedFile(ctx context.Context, p *auth.Principal, bookID, id, fileID uuid.UUID) (FileContent, error) {
	r, a, _, err := s.one(ctx, p, bookID, id)
	if err != nil {
		return FileContent{}, err
	}
	fc, err := s.open(ctx, a.ID, fileID)
	if err != nil {
		return FileContent{}, err
	}
	if err := s.recordView(ctx, r, a, orgaudit.ActionApplicationFileViewed, map[string]any{"file_id": fileID.String(), "field": fc.File.FieldKey}); err != nil {
		_ = fc.Content.Close()
		return FileContent{}, err
	}
	return fc, nil
}

// recordView writes the organisation audit record of a read (FR-018). A
// direct owner has no organisation audit trail: the read is logged.
func (s *Service) recordView(ctx context.Context, r received, a Application, action string, after map[string]any) error {
	attrs := []slog.Attr{slog.String("application_id", a.ID.String()), slog.String("actor_user_id", r.p.UserID.String()), slog.String("action", action)}
	if f, ok := after["file_id"].(string); ok {
		attrs = append(attrs, slog.String("file_id", f))
	}
	s.audit(ctx, "application.viewed", attrs...)
	if r.book.OrganisationID == nil {
		return nil
	}
	label := a.Reference
	e := orgaudit.New(*r.book.OrganisationID, r.p.UserID, r.p.Name, action, orgaudit.TargetApplication, a.ID, label, nil, after, s.clock())
	return s.store.RecordAudit(ctx, []orgaudit.Event{e})
}

// StatusInput is the body of POST .../applications/{id}/status.
type StatusInput struct {
	Status string  `json:"status"`
	Note   *string `json:"note"`
}

// SetStatus moves a received application (UNDER_REVIEW, SHORTLISTED,
// ACCEPTED, REJECTED) along the allowed transitions; *TransitionError
// otherwise. A note is kept as an internal note.
func (s *Service) SetStatus(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, in StatusInput) (AgentView, error) {
	_, a, _, err := s.one(ctx, p, bookID, id)
	if err != nil {
		return AgentView{}, err
	}
	targets := []string{StatusUnderReview, StatusShortlisted, StatusAccepted, StatusRejected}
	errs := fieldErrors{}
	switch {
	case in.Status == StatusMoreInfo:
		errs.add("status", "use POST .../request-info to ask for more information")
	case !slices.Contains(targets, in.Status):
		errs.add("status", "must be one of "+strings.Join(targets, ", "))
	}
	note := ""
	if in.Note != nil {
		note = strings.TrimSpace(*in.Note)
		if runes(note) > MaxNoteLength {
			errs.add("note", fmt.Sprintf("must be at most %d characters", MaxNoteLength))
		}
	}
	if err := errs.err(); err != nil {
		return AgentView{}, err
	}
	if !CanTransition(a.Status, in.Status) {
		return AgentView{}, &TransitionError{From: a.Status, To: in.Status, Allowed: StaffTransitions(a.Status)}
	}
	now := s.clock()
	actor := p.UserID
	expect, from := a.UpdatedAt, a.Status
	a.Status, a.UpdatedAt = in.Status, now
	if Final(a.Status) {
		a.DecidedAt = &now
	}
	ch := Change{Application: &a, Expect: expect,
		Events: []Event{{ID: newID(), ApplicationID: a.ID, Kind: EventStatusChanged, FromStatus: from, ToStatus: a.Status, ActorType: ActorStaff,
			ActorUserID: &actor, ActorName: p.Name, At: now}},
		Outbox: []Outbox{{ID: newID(), Type: EventTypeStatusChanged, Payload: EventPayload{ApplicationID: a.ID, ListingID: a.ListingID, From: from,
			To: a.Status, ActorUserID: &actor, OccurredAt: now}}}}
	if note != "" {
		ch.Notes = []Note{{ID: newID(), ApplicationID: a.ID, Text: note, AuthorUserID: &actor, AuthorName: p.Name, At: now}}
	}
	stored, err := s.store.Apply(ctx, ch)
	if err != nil {
		return AgentView{}, err
	}
	s.audit(ctx, "application.status_changed", slog.String("application_id", a.ID.String()), slog.String("from", from), slog.String("to", a.Status),
		slog.String("actor_user_id", p.UserID.String()))
	return s.agentView(ctx, stored, true)
}

// RequestInfo moves a received application to MORE_INFO with a message to
// the applicant, who may then edit and resubmit it.
func (s *Service) RequestInfo(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, message string) (AgentView, error) {
	_, a, _, err := s.one(ctx, p, bookID, id)
	if err != nil {
		return AgentView{}, err
	}
	message = strings.TrimSpace(message)
	switch {
	case message == "":
		return AgentView{}, &ValidationError{Fields: map[string]string{"message": "is required: say what the applicant should add"}}
	case runes(message) > MaxMessageLength:
		return AgentView{}, &ValidationError{Fields: map[string]string{"message": fmt.Sprintf("must be at most %d characters", MaxMessageLength)}}
	}
	if !CanTransition(a.Status, StatusMoreInfo) {
		return AgentView{}, &TransitionError{From: a.Status, To: StatusMoreInfo, Allowed: StaffTransitions(a.Status)}
	}
	now := s.clock()
	actor := p.UserID
	expect, from := a.UpdatedAt, a.Status
	a.Status, a.UpdatedAt = StatusMoreInfo, now
	stored, err := s.store.Apply(ctx, Change{Application: &a, Expect: expect,
		Events: []Event{{ID: newID(), ApplicationID: a.ID, Kind: EventInfoRequested, FromStatus: from, ToStatus: StatusMoreInfo, Message: message,
			ActorType: ActorStaff, ActorUserID: &actor, ActorName: p.Name, At: now}},
		Outbox: []Outbox{{ID: newID(), Type: EventTypeInfoRequested, Payload: EventPayload{ApplicationID: a.ID, ListingID: a.ListingID, From: from,
			To: StatusMoreInfo, ActorUserID: &actor, OccurredAt: now}}}})
	if err != nil {
		return AgentView{}, err
	}
	s.audit(ctx, "application.info_requested", slog.String("application_id", a.ID.String()), slog.String("actor_user_id", p.UserID.String()))
	return s.agentView(ctx, stored, true)
}

// AddNote adds an internal note (never shown to the applicant).
func (s *Service) AddNote(ctx context.Context, p *auth.Principal, bookID, id uuid.UUID, text string) (NoteView, error) {
	_, a, _, err := s.one(ctx, p, bookID, id)
	if err != nil {
		return NoteView{}, err
	}
	text = strings.TrimSpace(text)
	switch {
	case text == "":
		return NoteView{}, &ValidationError{Fields: map[string]string{"text": "is required"}}
	case runes(text) > MaxNoteLength:
		return NoteView{}, &ValidationError{Fields: map[string]string{"text": fmt.Sprintf("must be at most %d characters", MaxNoteLength)}}
	}
	actor := p.UserID
	n := Note{ID: newID(), ApplicationID: a.ID, Text: text, AuthorUserID: &actor, AuthorName: p.Name, At: s.clock()}
	if _, err := s.store.Apply(ctx, Change{Notes: []Note{n}}); err != nil {
		return NoteView{}, err
	}
	return noteView(n), nil
}

func noteView(n Note) NoteView {
	return NoteView{ID: n.ID, Text: n.Text, AuthorUserID: n.AuthorUserID, AuthorName: n.AuthorName, CreatedAt: n.At.UTC()}
}

// agentView builds the receiving side's view; full adds the form, the
// answers rendered against the submission version, the files and the notes.
func (s *Service) agentView(ctx context.Context, a Application, full bool) (AgentView, error) {
	base, f, files, l, err := s.base(ctx, a, full, true)
	if err != nil {
		return AgentView{}, err
	}
	v := AgentView{ApplicantView: base, AllowedTransitions: StaffTransitions(a.Status), AssignedAgentUserID: l.AgentUserID}
	v.CanEdit, v.CanSubmit, v.CanWithdraw = false, false, false
	v.Applicant = Applicant{Name: a.ApplicantName}
	if a.ApplicantUserID == nil {
		v.Applicant.Name = DeletedApplicantName
	} else {
		id := *a.ApplicantUserID
		v.Applicant.UserID = &id
		if s.people != nil {
			name, email, ok, err := s.people(ctx, id)
			if err != nil {
				return AgentView{}, err
			}
			if ok {
				if name != "" {
					v.Applicant.Name = name
				}
				if email != "" {
					v.Applicant.Email = &email
				}
			}
		}
	}
	if !full {
		return v, nil
	}
	v.Rendered = render(f, a.Answers, files)
	notes, err := s.store.Notes(ctx, a.ID)
	if err != nil {
		return AgentView{}, err
	}
	nv := make([]NoteView, 0, len(notes))
	for i := len(notes) - 1; i >= 0; i-- {
		nv = append(nv, noteView(notes[i]))
	}
	v.Notes = nv
	return v, nil
}

// render lays the answers out as the form version asked them.
func render(f Form, answers Answers, files []File) []RenderedSection {
	visible := Visibility(f, answers)
	out := make([]RenderedSection, 0, len(f.Sections))
	for _, sec := range f.Sections {
		rs := RenderedSection{Key: sec.Key, Title: sec.Title, Description: sec.Description, Fields: make([]RenderedField, 0, len(sec.Fields))}
		for _, fld := range sec.Fields {
			rf := RenderedField{Key: fld.Key, Label: fld.Label, Type: fld.Type, Visible: visible[fld.Key], Files: []FileView{}}
			if v, ok := answers[fld.Key]; ok {
				rf.Answer, rf.Display = v, display(fld, v)
			}
			if fld.Type == TypeFile {
				for _, x := range files {
					if x.FieldKey == fld.Key {
						rf.Files = append(rf.Files, fileView(x))
					}
				}
			}
			rs.Fields = append(rs.Fields, rf)
		}
		out = append(out, rs)
	}
	return out
}
