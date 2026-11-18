package applications

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/onboarding"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/storage"
)

// customer returns the caller's user ID: any customer-scope session may
// apply; an admin-scope session may not.
func customer(p *auth.Principal) (uuid.UUID, error) {
	switch {
	case p == nil:
		return uuid.Nil, ErrUnauthenticated
	case p.SessionScope != auth.ScopeCustomer:
		return uuid.Nil, ErrForbidden
	}
	return p.UserID, nil
}

// mine returns one of the caller's applications (ErrNotFound for anybody
// else's: other applicants stay private).
func (s *Service) mine(ctx context.Context, p *auth.Principal, id uuid.UUID) (Application, error) {
	me, err := customer(p)
	if err != nil {
		return Application{}, err
	}
	a, err := s.store.Application(ctx, id)
	if err != nil {
		return Application{}, err
	}
	if a.ApplicantUserID == nil || *a.ApplicantUserID != me {
		return Application{}, ErrNotFound
	}
	return a, nil
}

// intakeOf returns why a listing takes no application now ("" when OPEN).
func (s *Service) intakeOf(ctx context.Context, l ListingInfo) (Intake, Form, string, error) {
	if !l.Eligible() {
		return Intake{}, Form{}, ReasonNone, nil
	}
	in, ok, err := s.store.Intake(ctx, l.ID)
	if err != nil || !ok {
		return Intake{}, Form{}, ReasonNone, err
	}
	f, err := s.store.Form(ctx, in.FormID)
	if err != nil {
		return Intake{}, Form{}, "", err
	}
	_, reason := Effective(in, f, s.clock())
	return in, f, reason, nil
}

// Start creates a DRAFT on the listing's current form version, or returns
// the caller's existing application for that listing and version
// (created=false). A listing whose intake is not OPEN answers
// *ClosedError (409 APPLICATIONS_CLOSED).
func (s *Service) Start(ctx context.Context, p *auth.Principal, listingID uuid.UUID) (ApplicantView, bool, error) {
	me, err := customer(p)
	if err != nil {
		return ApplicantView{}, false, err
	}
	l, err := s.listing(ctx, listingID)
	if err != nil {
		return ApplicantView{}, false, err
	}
	if !l.Public {
		return ApplicantView{}, false, ErrListingNotFound
	}
	in, f, reason, err := s.intakeOf(ctx, l)
	if err != nil {
		return ApplicantView{}, false, err
	}
	if f.ID != uuid.Nil {
		existing, ok, err := s.store.Existing(ctx, me, l.ID, f.ID)
		if err != nil {
			return ApplicantView{}, false, err
		}
		if ok {
			v, err := s.applicantView(ctx, existing, true)
			return v, false, err
		}
	}
	if reason != "" {
		return ApplicantView{}, false, &ClosedError{Reason: reason, ClosesAt: utcPtr(in.ClosesAt)}
	}
	book, ok := l.Book()
	if !ok {
		return ApplicantView{}, false, ErrListingNotFound
	}
	now := s.clock()
	a := Application{ID: newID(), ListingID: l.ID, FormID: f.ID, FormVersion: f.Version, ApplicantUserID: &me, ApplicantName: p.Name,
		OrganisationID: book.OrganisationID, OwnerUserID: book.UserID, Status: StatusDraft, Answers: Answers{}, Listing: l.Card,
		CreatedAt: now, UpdatedAt: now}
	stored, err := s.store.Apply(ctx, Change{Application: &a, Insert: true,
		Events: []Event{{ID: newID(), ApplicationID: a.ID, Kind: EventCreated, ToStatus: StatusDraft, ActorType: ActorApplicant, ActorUserID: &me, ActorName: p.Name, At: now}}})
	if errors.Is(err, ErrConflict) {
		// Started concurrently by another request of the same customer.
		existing, ok, lerr := s.store.Existing(ctx, me, l.ID, f.ID)
		if lerr != nil || !ok {
			return ApplicantView{}, false, err
		}
		v, verr := s.applicantView(ctx, existing, true)
		return v, false, verr
	}
	if err != nil {
		return ApplicantView{}, false, err
	}
	v, err := s.applicantView(ctx, stored, true)
	return v, true, err
}

// MineOne returns one of the caller's applications.
func (s *Service) MineOne(ctx context.Context, p *auth.Principal, id uuid.UUID) (ApplicantView, error) {
	a, err := s.mine(ctx, p, id)
	if err != nil {
		return ApplicantView{}, err
	}
	return s.applicantView(ctx, a, true)
}

// Mine lists the caller's applications, newest first.
func (s *Service) Mine(ctx context.Context, p *auth.Principal, status string, page, size int) (MinePage, error) {
	me, err := customer(p)
	if err != nil {
		return MinePage{}, err
	}
	list, total, err := s.store.Mine(ctx, me, status, size, (page-1)*size)
	if err != nil {
		return MinePage{}, err
	}
	out := MinePage{Items: make([]ApplicantView, 0, len(list)), Page: page, PageSize: size, Total: total, TotalPages: totalPages(total, size)}
	for _, a := range list {
		v, err := s.applicantView(ctx, a, false)
		if err != nil {
			return MinePage{}, err
		}
		out.Items = append(out.Items, v)
	}
	return out, nil
}

func filesByField(files []File) map[string][]string {
	out := map[string][]string{}
	for _, f := range files {
		out[f.FieldKey] = append(out[f.FieldKey], f.ID.String())
	}
	return out
}

// Autosave merges answers into a DRAFT or MORE_INFO application. Each
// answer is validated; a missing required answer is allowed until submit.
func (s *Service) Autosave(ctx context.Context, p *auth.Principal, id uuid.UUID, patch map[string]json.RawMessage) (ApplicantView, error) {
	a, err := s.mine(ctx, p, id)
	if err != nil {
		return ApplicantView{}, err
	}
	if !a.Editable() {
		return ApplicantView{}, ErrNotEditable
	}
	f, err := s.store.Form(ctx, a.FormID)
	if err != nil {
		return ApplicantView{}, err
	}
	files, err := s.store.Files(ctx, a.ID)
	if err != nil {
		return ApplicantView{}, err
	}
	merged, err := mergeAnswers(f, a.Answers, patch, filesByField(files))
	if err != nil {
		return ApplicantView{}, err
	}
	expect := a.UpdatedAt
	a.Answers, a.UpdatedAt = merged, s.clock()
	stored, err := s.store.Apply(ctx, Change{Application: &a, Expect: expect})
	if err != nil {
		return ApplicantView{}, err
	}
	return s.applicantView(ctx, stored, true)
}

// Upload stores a private file for a file field of an editable
// application, checked as onboarding documents are: by its content (never
// its name), against the field's accepted types, and at most 10 MB.
func (s *Service) Upload(ctx context.Context, p *auth.Principal, id uuid.UUID, fieldKey, fileName string, content []byte) (FileView, ApplicantView, error) {
	a, err := s.mine(ctx, p, id)
	if err != nil {
		return FileView{}, ApplicantView{}, err
	}
	if !a.Editable() {
		return FileView{}, ApplicantView{}, ErrNotEditable
	}
	f, err := s.store.Form(ctx, a.FormID)
	if err != nil {
		return FileView{}, ApplicantView{}, err
	}
	field, ok := f.field(fieldKey)
	switch {
	case fieldKey == "":
		return FileView{}, ApplicantView{}, &ValidationError{Fields: map[string]string{"field": "is required (?field=<key>)"}}
	case !ok || field.Type != TypeFile:
		return FileView{}, ApplicantView{}, &ValidationError{Fields: map[string]string{"field": "is not a file question of this form"}}
	case len(content) == 0:
		return FileView{}, ApplicantView{}, &ValidationError{Fields: map[string]string{"file": "is required and must not be empty"}}
	case len(content) > MaxFileBytes:
		return FileView{}, ApplicantView{}, ErrFileTooLarge
	}
	kind := Sniff(content)
	if kind == "" || !slices.Contains(field.accepts(), kind) {
		return FileView{}, ApplicantView{}, &UnsupportedFileError{Accepted: field.accepts()}
	}
	files, err := s.store.Files(ctx, a.ID)
	if err != nil {
		return FileView{}, ApplicantView{}, err
	}
	if len(filesByField(files)[fieldKey]) >= field.maxFiles() {
		return FileView{}, ApplicantView{}, &TooManyFilesError{Max: field.maxFiles()}
	}
	key, err := storage.NewKey(DocumentKeyPrefix)
	if err != nil {
		return FileView{}, ApplicantView{}, err
	}
	contentType := kindTypes[kind]
	sum := sha256.Sum256(content)
	now := s.clock()
	rec := File{ID: newID(), ApplicationID: a.ID, FieldKey: fieldKey, FileName: fileNameFor(fileName, kind), ContentType: contentType,
		Size: int64(len(content)), SHA256: hex.EncodeToString(sum[:]), ObjectKey: key, UploadedAt: now}
	if err := s.files.Put(ctx, key, bytes.NewReader(content), rec.Size, contentType); err != nil {
		return FileView{}, ApplicantView{}, fmt.Errorf("store application file: %w", err)
	}
	ids, _ := a.Answers[fieldKey].([]string)
	a.Answers = a.Answers.clone()
	a.Answers[fieldKey] = append(append([]string{}, ids...), rec.ID.String())
	expect := a.UpdatedAt
	a.UpdatedAt = now
	stored, err := s.store.Apply(ctx, Change{Application: &a, Expect: expect, AddFiles: []File{rec}})
	if err != nil {
		if derr := s.files.Delete(context.WithoutCancel(ctx), key); derr != nil {
			s.logger.WarnContext(ctx, "could not remove an application file after a failed upload", slog.String("error", derr.Error()))
		}
		return FileView{}, ApplicantView{}, err
	}
	s.audit(ctx, "application.file_uploaded", slog.String("application_id", a.ID.String()), slog.String("file_id", rec.ID.String()),
		slog.String("type", kind), slog.Int64("size", rec.Size))
	v, err := s.applicantView(ctx, stored, true)
	return fileView(rec), v, err
}

// fileNameFor keeps the name readable and gives it the extension of what
// the file really is (onboarding's sanitising).
func fileNameFor(name, kind string) string {
	clean := onboarding.SanitiseFileName(name, "")
	if i := strings.LastIndex(clean, "."); i > 0 {
		clean = clean[:i]
	}
	if clean == "" || clean == "document" && name == "" {
		clean = "file"
	}
	return clean + kindExtensions[kind]
}

// DeleteFile removes one of the files of an editable application.
func (s *Service) DeleteFile(ctx context.Context, p *auth.Principal, id, fileID uuid.UUID) (ApplicantView, error) {
	a, err := s.mine(ctx, p, id)
	if err != nil {
		return ApplicantView{}, err
	}
	if !a.Editable() {
		return ApplicantView{}, ErrNotEditable
	}
	files, err := s.store.Files(ctx, a.ID)
	if err != nil {
		return ApplicantView{}, err
	}
	i := slices.IndexFunc(files, func(f File) bool { return f.ID == fileID })
	if i < 0 {
		return ApplicantView{}, ErrFileNotFound
	}
	field := files[i].FieldKey
	a.Answers = a.Answers.clone()
	if ids, ok := a.Answers[field].([]string); ok {
		ids = slices.DeleteFunc(append([]string{}, ids...), func(x string) bool { return x == fileID.String() })
		if len(ids) == 0 {
			delete(a.Answers, field)
		} else {
			a.Answers[field] = ids
		}
	}
	expect := a.UpdatedAt
	a.UpdatedAt = s.clock()
	stored, err := s.store.Apply(ctx, Change{Application: &a, Expect: expect, RemoveFiles: []uuid.UUID{fileID}})
	if err != nil {
		return ApplicantView{}, err
	}
	s.purgeSoon(ctx)
	return s.applicantView(ctx, stored, true)
}

// FileContent is an open private file.
type FileContent struct {
	File    File
	Content io.ReadCloser
}

func (s *Service) open(ctx context.Context, applicationID, fileID uuid.UUID) (FileContent, error) {
	files, err := s.store.Files(ctx, applicationID)
	if err != nil {
		return FileContent{}, err
	}
	i := slices.IndexFunc(files, func(f File) bool { return f.ID == fileID })
	if i < 0 {
		return FileContent{}, ErrFileNotFound
	}
	rc, err := s.files.Open(ctx, files[i].ObjectKey)
	if errors.Is(err, storage.ErrNotFound) {
		return FileContent{}, ErrFileNotFound
	}
	if err != nil {
		return FileContent{}, err
	}
	return FileContent{File: files[i], Content: rc}, nil
}

// OpenFile opens one of the caller's own files.
func (s *Service) OpenFile(ctx context.Context, p *auth.Principal, id, fileID uuid.UUID) (FileContent, error) {
	a, err := s.mine(ctx, p, id)
	if err != nil {
		return FileContent{}, err
	}
	return s.open(ctx, a.ID, fileID)
}

// Submit checks the answers against the application's form version
// (required fields, honouring visible_if: a hidden field is not required
// and its answer is dropped) and moves the application to SUBMITTED with a
// reference. A first submission needs the listing's intake to be OPEN
// (*ClosedError otherwise); answering a request for more information
// (MORE_INFO) does not.
func (s *Service) Submit(ctx context.Context, p *auth.Principal, id uuid.UUID) (ApplicantView, error) {
	a, err := s.mine(ctx, p, id)
	if err != nil {
		return ApplicantView{}, err
	}
	if !a.Editable() {
		return ApplicantView{}, ErrNotEditable
	}
	f, err := s.store.Form(ctx, a.FormID)
	if err != nil {
		return ApplicantView{}, err
	}
	l, err := s.listing(ctx, a.ListingID)
	if err != nil && !errors.Is(err, ErrListingNotFound) {
		return ApplicantView{}, err
	}
	resubmission := a.Status == StatusMoreInfo
	if !resubmission {
		if err != nil {
			return ApplicantView{}, &ClosedError{Reason: ReasonClosed}
		}
		in, _, reason, err := s.intakeOf(ctx, l)
		if err != nil {
			return ApplicantView{}, err
		}
		if reason == "" && f.Expired(s.clock()) {
			reason = ReasonFormExpired
		}
		if reason == "" && !l.Public {
			reason = ReasonClosed
		}
		if reason != "" {
			return ApplicantView{}, &ClosedError{Reason: reason, ClosesAt: utcPtr(in.ClosesAt)}
		}
	}
	files, err := s.store.Files(ctx, a.ID)
	if err != nil {
		return ApplicantView{}, err
	}
	final, err := finalAnswers(f, a.Answers, filesByField(files))
	if err != nil {
		return ApplicantView{}, err
	}
	// Files no longer answered (a hidden question, a replaced upload) go.
	kept := map[string]bool{}
	for _, v := range final {
		if ids, ok := v.([]string); ok {
			for _, x := range ids {
				kept[x] = true
			}
		}
	}
	var remove []uuid.UUID
	for _, file := range files {
		if !kept[file.ID.String()] {
			remove = append(remove, file.ID)
		}
	}
	now := s.clock()
	me := p.UserID
	expect, from := a.UpdatedAt, a.Status
	a.Answers, a.Status, a.UpdatedAt, a.LastSubmittedAt = final, StatusSubmitted, now, &now
	if l.ID != uuid.Nil && l.Public {
		a.Listing = l.Card
	}
	kind := EventSubmitted
	ch := Change{Application: &a, Expect: expect, RemoveFiles: remove}
	if resubmission {
		kind = EventResubmitted
	} else if a.SubmittedAt == nil {
		a.SubmittedAt = &now
	}
	ch.NextReference = a.Reference == ""
	ch.Events = []Event{{ID: newID(), ApplicationID: a.ID, Kind: kind, FromStatus: from, ToStatus: StatusSubmitted, ActorType: ActorApplicant,
		ActorUserID: &me, ActorName: p.Name, At: now}}
	ch.Outbox = []Outbox{{ID: newID(), Type: EventTypeSubmitted, Payload: EventPayload{ApplicationID: a.ID, ListingID: a.ListingID,
		From: from, To: StatusSubmitted, Resubmission: resubmission, ActorUserID: &me, OccurredAt: now}}}
	stored, err := s.store.Apply(ctx, ch)
	if err != nil {
		return ApplicantView{}, err
	}
	if len(remove) > 0 {
		s.purgeSoon(ctx)
	}
	s.audit(ctx, "application.submitted", slog.String("application_id", a.ID.String()), slog.String("reference", stored.Reference),
		slog.String("form_id", a.FormID.String()), slog.Int("form_version", a.FormVersion), slog.Bool("resubmission", resubmission))
	return s.applicantView(ctx, stored, true)
}

// Withdraw withdraws an application that is not final.
func (s *Service) Withdraw(ctx context.Context, p *auth.Principal, id uuid.UUID) (ApplicantView, error) {
	a, err := s.mine(ctx, p, id)
	if err != nil {
		return ApplicantView{}, err
	}
	if Final(a.Status) {
		return ApplicantView{}, &TransitionError{From: a.Status, To: StatusWithdrawn, Allowed: []string{}}
	}
	now := s.clock()
	me := p.UserID
	expect, from := a.UpdatedAt, a.Status
	a.Status, a.UpdatedAt, a.DecidedAt = StatusWithdrawn, now, &now
	ch := Change{Application: &a, Expect: expect,
		Events: []Event{{ID: newID(), ApplicationID: a.ID, Kind: EventWithdrawn, FromStatus: from, ToStatus: StatusWithdrawn, ActorType: ActorApplicant,
			ActorUserID: &me, ActorName: p.Name, At: now}}}
	if a.SubmittedAt != nil {
		ch.Outbox = []Outbox{{ID: newID(), Type: EventTypeWithdrawn, Payload: EventPayload{ApplicationID: a.ID, ListingID: a.ListingID,
			From: from, To: StatusWithdrawn, ActorUserID: &me, OccurredAt: now}}}
	}
	stored, err := s.store.Apply(ctx, ch)
	if err != nil {
		return ApplicantView{}, err
	}
	s.audit(ctx, "application.withdrawn", slog.String("application_id", a.ID.String()), slog.String("from", from))
	return s.applicantView(ctx, stored, true)
}

// UnsupportedFileError is 400 UNSUPPORTED_FILE_TYPE.
type UnsupportedFileError struct{ Accepted []string }

func (e *UnsupportedFileError) Error() string { return ErrUnsupportedFile.Error() }

// Unwrap lets errors.Is match ErrUnsupportedFile.
func (e *UnsupportedFileError) Unwrap() error { return ErrUnsupportedFile }

// TooManyFilesError is 409 TOO_MANY_FILES.
type TooManyFilesError struct{ Max int }

func (e *TooManyFilesError) Error() string { return ErrTooManyFiles.Error() }

// Unwrap lets errors.Is match ErrTooManyFiles.
func (e *TooManyFilesError) Unwrap() error { return ErrTooManyFiles }

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

var eventLabels = map[string]string{
	EventCreated: "Application started", EventSubmitted: "Application submitted", EventResubmitted: "Application resubmitted with the information asked for",
	EventInfoRequested: "More information requested", EventWithdrawn: "Application withdrawn",
}

func eventLabel(e Event) string {
	switch e.Kind {
	case EventStatusChanged:
		return "Status changed to " + StatusLabel(e.ToStatus)
	case EventErased:
		if e.Message == ErasedByAccountDeletion {
			return "Answers and files deleted with the applicant's account"
		}
		return "Answers and files deleted at the end of the retention period"
	}
	return eventLabels[e.Kind]
}

func timeline(events []Event, withActors bool) []TimelineEntry {
	out := make([]TimelineEntry, 0, len(events))
	for i := len(events) - 1; i >= 0; i-- { // newest first
		e := events[i]
		t := TimelineEntry{ID: e.ID, Kind: e.Kind, Status: strPtr(e.ToStatus), Label: eventLabel(e), At: e.At.UTC()}
		if e.Kind == EventInfoRequested {
			t.Message = strPtr(e.Message)
		}
		if withActors {
			name := e.ActorName
			if e.ActorType == ActorSystem {
				name = "RightProperty365"
			}
			t.ActorName = &name
		}
		out = append(out, t)
	}
	return out
}

func infoRequest(a Application, events []Event) *InfoRequest {
	if a.Status != StatusMoreInfo {
		return nil
	}
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == EventInfoRequested {
			return &InfoRequest{Message: events[i].Message, RequestedAt: events[i].At.UTC()}
		}
	}
	return nil
}

// base builds the part of a view both sides share.
func (s *Service) base(ctx context.Context, a Application, full bool, withActors bool) (ApplicantView, Form, []File, ListingInfo, error) {
	f, err := s.store.Form(ctx, a.FormID)
	if err != nil {
		return ApplicantView{}, Form{}, nil, ListingInfo{}, err
	}
	events, err := s.store.Events(ctx, a.ID)
	if err != nil {
		return ApplicantView{}, Form{}, nil, ListingInfo{}, err
	}
	known, err := s.listings.Listings(ctx, []uuid.UUID{a.ListingID})
	if err != nil {
		return ApplicantView{}, Form{}, nil, ListingInfo{}, err
	}
	l := known[a.ListingID]
	v := ApplicantView{ID: a.ID, Reference: strPtr(a.Reference), Status: a.Status, StatusLabel: StatusLabel(a.Status), ListingID: a.ListingID,
		Listing: a.Listing, InfoRequest: infoRequest(a, events), Timeline: timeline(events, withActors), CanEdit: a.Editable(),
		CanSubmit: a.Editable(), CanWithdraw: !Final(a.Status), Erased: a.ErasedAt != nil, CreatedAt: a.CreatedAt.UTC(),
		UpdatedAt: a.UpdatedAt.UTC(), SubmittedAt: utcPtr(a.LastSubmittedAt), DecidedAt: utcPtr(a.DecidedAt)}
	if l.Public {
		v.Listing, v.ListingAvailable = l.Card, true
	} else {
		v.Listing.Slug = nil
	}
	var files []File
	if full {
		if files, err = s.store.Files(ctx, a.ID); err != nil {
			return ApplicantView{}, Form{}, nil, ListingInfo{}, err
		}
		v.Form = publicForm(f)
		answers := a.Answers
		if answers == nil {
			answers = Answers{}
		}
		v.Answers = answers
		fv := make([]FileView, 0, len(files))
		for _, x := range files {
			fv = append(fv, fileView(x))
		}
		v.Files = fv
		missing := []string{}
		if a.Editable() {
			missing = MissingRequired(f, a.Answers)
		}
		v.MissingRequired = missing
	} else {
		v.Form = FormRef{ID: f.ID, FamilyID: f.FamilyID, Version: f.Version, Name: f.Name}
	}
	return v, f, files, l, nil
}

func (s *Service) applicantView(ctx context.Context, a Application, full bool) (ApplicantView, error) {
	v, _, _, _, err := s.base(ctx, a, full, false)
	return v, err
}

// purgeSoon removes the objects of removed files now (best effort; the
// retention job retries whatever is left).
func (s *Service) purgeSoon(ctx context.Context) {
	if _, err := s.Purge(context.WithoutCancel(ctx)); err != nil {
		s.logger.WarnContext(ctx, "application file purge failed; the worker retries", slog.String("error", err.Error()))
	}
}
