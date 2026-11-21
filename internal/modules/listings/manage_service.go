package listings

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/logging"
	"rightproperty365.com/backend/internal/platform/storage"
)

// Errors of the management use cases, mapped to HTTP by the handler.
var (
	// ErrForbidden: the caller may see the listing (or manages listings) but
	// may not do this.
	ErrForbidden = errors.New("not allowed")
	// ErrPhotoRequired: the last public photo of a published listing.
	ErrPhotoRequired = errors.New("a published listing must keep at least one photo")
	// ErrSelfReview: a reviewer may not decide a listing of their own: one of
	// an organisation they are a member of, one they sell or let themselves,
	// are the agent of, created or submitted.
	ErrSelfReview = errors.New("a reviewer may not review their own listing")
)

// selfReview reports whether the reviewer has a stake in the listing.
func selfReview(p *auth.Principal, m ManagedListing) bool {
	if m.OrganisationID != nil && p.MembershipRole(*m.OrganisationID) != "" {
		return true
	}
	for _, id := range []*uuid.UUID{m.SellerUserID, m.AgentUserID, m.CreatedBy, m.SubmittedBy} {
		if id != nil && *id == p.UserID {
			return true
		}
	}
	return false
}

// StateError is an action the listing's status does not allow (409).
type StateError struct {
	Code    string
	Message string
}

func (e *StateError) Error() string { return e.Message }

// Codes of StateError.
const (
	CodeStateConflict = "LISTING_STATE_CONFLICT"
	CodeInReview      = "LISTING_IN_REVIEW"
)

func conflict(message string) error { return &StateError{Code: CodeStateConflict, Message: message} }

var errInReview = &StateError{Code: CodeInReview, Message: "The listing is waiting for review and cannot be changed until the review is done."}

// ValidationError carries per-field messages (400).
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "validation failed" }

// IncompleteError is a submission that lacks required fields (422).
type IncompleteError struct{ Missing []Missing }

func (e *IncompleteError) Error() string { return "listing incomplete" }

// Manager implements listing management: the advertisers' side and the
// reviewers' side. Who may do what is decided in auth/capabilities.go.
type Manager struct {
	svc    *Service
	store  ManageStore
	files  storage.Storage
	logger *slog.Logger
	// quota is the listing package usage of the views (nil: none; see quota.go).
	quota Quota
}

// NewManager builds the Manager. files is the private object storage the
// photos are kept in.
func NewManager(svc *Service, store ManageStore, files storage.Storage, logger *slog.Logger) *Manager {
	return &Manager{svc: svc, store: store, files: files, logger: logger}
}

func (mg *Manager) clock() time.Time { return mg.svc.now().UTC().Truncate(time.Microsecond) }

func (mg *Manager) audit(ctx context.Context, event string, p *auth.Principal, id uuid.UUID, attrs ...slog.Attr) {
	if mg.logger == nil {
		return
	}
	base := []slog.Attr{slog.Bool("audit", true), slog.String("event", event), slog.String("trace_id", logging.RequestID(ctx)),
		slog.String("listing_id", id.String()), slog.String("actor_user_id", p.UserID.String())}
	mg.logger.LogAttrs(ctx, slog.LevelInfo, "audit", append(base, attrs...)...)
}

func (mg *Manager) removeObjects(ctx context.Context, photos []StoredPhoto) {
	if mg.files == nil {
		return
	}
	for _, p := range photos {
		for _, key := range p.Keys() {
			if err := mg.files.Delete(context.WithoutCancel(ctx), key); err != nil && mg.logger != nil {
				mg.logger.ErrorContext(ctx, "listing photo object could not be deleted", slog.String("object_key", key), slog.String("error", err.Error()))
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Access
// ---------------------------------------------------------------------------

// readable loads a listing the caller may see: ErrNotFound when it does not
// exist or belongs to somebody else (the same answer).
func (mg *Manager) readable(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedListing, error) {
	m, err := mg.store.Managed(ctx, id)
	if err != nil {
		return ManagedListing{}, err
	}
	if !p.MayManageListing(m.OrganisationID, m.SellerUserID) && !p.MayEditListing(m.OrganisationID, m.AgentUserID, m.SellerUserID) {
		// A member of the listing's own organisation whose role does not
		// manage listings (STAFF) is told so; anyone else learns nothing.
		if m.OrganisationID != nil && slices.Contains(p.OrganisationIDs(), *m.OrganisationID) {
			return ManagedListing{}, ErrForbidden
		}
		return ManagedListing{}, ErrNotFound
	}
	return m, nil
}

// editable loads a listing the caller may change: ErrForbidden for one they
// may only see.
func (mg *Manager) editable(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedListing, error) {
	m, err := mg.readable(ctx, p, id)
	if err != nil {
		return ManagedListing{}, err
	}
	if !p.MayEditListing(m.OrganisationID, m.AgentUserID, m.SellerUserID) {
		return ManagedListing{}, ErrForbidden
	}
	return m, nil
}

// reviewable loads a listing the reviewer may review (its country):
// ErrNotFound otherwise.
func (mg *Manager) reviewable(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedListing, error) {
	m, err := mg.store.Managed(ctx, id)
	if err != nil {
		return ManagedListing{}, err
	}
	if !p.MayReviewListing(m.MarketCode) {
		return ManagedListing{}, ErrNotFound
	}
	return m, nil
}

// Edit modes.
const (
	editDirect   = "direct"
	editRevision = "revision"
)

// editMode says how the listing's content may change now: on the listing
// itself (not public), through a revision (published), or not at all.
func editMode(m ManagedListing) (string, error) {
	switch {
	case m.Status == StatusExpired && m.Revision != nil:
		// Stored EXPIRED by the sweep with changes pending: they stay a
		// revision (as while it read EXPIRED as PUBLISHED); a relist merges them.
		if m.Revision.Status == RevisionPendingReview {
			return "", errInReview
		}
		return editRevision, nil
	}
	switch m.Status {
	case StatusDraft, StatusChangesRequested, StatusArchived, StatusExpired:
		return editDirect, nil
	case StatusPublished:
		if m.Revision != nil && m.Revision.Status == RevisionPendingReview {
			return "", errInReview
		}
		return editRevision, nil
	case StatusPendingReview:
		return "", errInReview
	case StatusRejected:
		return "", conflict("A rejected listing cannot be changed. You may delete it.")
	}
	return "", conflict(fmt.Sprintf("A listing that is %s cannot be changed here.", strings.ToLower(StatusLabel(m.Status))))
}

// ensureRevision opens (or reopens) the revision of a published listing for editing.
func ensureRevision(m *ManagedListing, now time.Time) *Revision {
	if m.Revision == nil || m.Revision.Status == RevisionRejected {
		c := ContentOf(m.Record)
		m.Revision = &Revision{Status: RevisionEditing, Content: &c}
	}
	if m.Revision.Content == nil {
		c := ContentOf(m.Record)
		m.Revision.Content = &c
	}
	m.Revision.UpdatedAt = now
	return m.Revision
}

// mergeRevision writes a revision onto the listing itself (an archive or a
// relist of a published listing: nothing public changes).
func mergeRevision(m *ManagedListing) {
	if m.Revision == nil {
		return
	}
	if m.Revision.Content != nil {
		m.Revision.Content.ApplyTo(&m.Record)
	}
	for i := range m.AllPhotos {
		if alt, ok := m.Revision.PhotoAlts[m.AllPhotos[i].ID.String()]; ok {
			m.AllPhotos[i].Alt = alt
		}
	}
	m.Revision = nil
}

func (mg *Manager) entry(m ManagedListing, p *auth.Principal, actor, action, from, to, kind, reason string, now time.Time) *TimelineEntry {
	user := p.UserID
	return &TimelineEntry{ID: uuid.New(), ListingID: m.ID, ActorUserID: &user, ActorKind: actor, Action: action,
		FromStatus: from, ToStatus: to, ReviewKind: kind, Reason: reason, CreatedAt: now}
}

// save writes a changed listing based on the version that was read.
func (mg *Manager) save(ctx context.Context, m ManagedListing, expect time.Time, ch Change) error {
	for attempt := 0; ; attempt++ {
		removed, err := mg.store.SaveListing(ctx, m, expect, ch)
		if errors.Is(err, ErrDuplicate) && attempt < 3 && m.PublishedAt.IsZero() {
			// The slug of a draft follows its title; another listing has it.
			m.Slug = slugFor(m.Title+" "+randomDigits(3), m.Reference)
			continue
		}
		if err != nil {
			return err
		}
		mg.removeObjects(ctx, removed)
		return nil
	}
}

func randomDigits(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		d, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteString(d.String())
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

func (mg *Manager) actions(m ManagedListing, canEdit bool, now time.Time) []string {
	out := []string{}
	if !canEdit {
		return out
	}
	status := m.EffectiveStatus(now)
	if _, err := editMode(m); err == nil {
		out = append(out, ActEdit)
	}
	if m.Status == StatusDraft || m.Status == StatusChangesRequested ||
		(m.Status == StatusPublished && m.Revision != nil && (m.Revision.Status == RevisionEditing || m.Revision.Status == RevisionChangesRequested)) {
		out = append(out, ActSubmit)
	}
	switch status {
	case StatusDraft, StatusPendingReview, StatusChangesRequested, StatusPublished, StatusExpired, StatusPaused:
		out = append(out, ActArchive)
	}
	if slices.Contains(relistFrom, status) {
		out = append(out, ActRelist)
	}
	out = append(out, lifecycleActions(m, status)...)
	if m.PublishedAt.IsZero() {
		out = append(out, ActDelete)
	}
	return out
}

func (mg *Manager) view(ctx context.Context, m ManagedListing, canEdit bool, reviewer bool) (ManagedView, error) {
	now := mg.clock()
	g, err := mg.svc.geography(ctx)
	if err != nil {
		return ManagedView{}, err
	}
	labels, err := mg.svc.labelSets(ctx)
	if err != nil {
		return ManagedView{}, err
	}
	timeline, err := mg.store.Timeline(ctx, m.ID)
	if err != nil {
		return ManagedView{}, err
	}
	var alts map[string]string
	if m.Revision != nil {
		alts = m.Revision.PhotoAlts
	}
	status := m.EffectiveStatus(now)
	v := ManagedView{
		ID: m.ID, Slug: m.Slug, Reference: m.Reference, Status: status, StatusLabel: StatusLabel(status), Public: m.Public(now),
		Review:  ReviewState{SubmittedAt: utcTime(m.SubmittedAt), ReviewedAt: utcTime(m.ReviewedAt), Reason: opt(m.ReviewReason)},
		Content: contentView(m.WorkingContent()), Photos: managedPhotos(m.AllPhotos, alts), PhotoLimit: MaxPhotosPerListing,
		Completeness: completeness(m.WorkingContent(), m.AllPhotos, alts, g),
		Owner:        OwnerView{Type: "direct_owner", AgentUserID: m.AgentUserID},
		Advertiser:   presentAdvertiser(m.Record, labels), CanEdit: canEdit, Actions: mg.actions(m, canEdit, now),
		PublishedAt: timePtr(m.PublishedAt), ExpiresAt: timePtr(m.ExpiresAt), Expiry: ExpiryOf(m.Record, now),
		CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(), Timeline: timelineViews(timeline, reviewer),
	}
	if m.OrganisationID != nil {
		v.Owner.Type, v.Owner.OrganisationID = "organisation", m.OrganisationID
	}
	if err := mg.usage(ctx, &v, m, status, reviewer); err != nil {
		return ManagedView{}, err
	}
	if r := m.Revision; r != nil {
		pc := &PendingChanges{Status: r.Status, StatusLabel: StatusLabel(r.Status), Reason: opt(r.Reason), SubmittedAt: utcTime(r.SubmittedAt), UpdatedAt: r.UpdatedAt.UTC()}
		for _, p := range m.AllPhotos {
			if p.ApprovedAt == nil {
				pc.NewPhotos++
			}
		}
		v.PendingChanges = pc
	}
	return v, nil
}

func (mg *Manager) ownerView(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	m, err := mg.store.Managed(ctx, id)
	if err != nil {
		return ManagedView{}, err
	}
	return mg.view(ctx, m, p.MayEditListing(m.OrganisationID, m.AgentUserID, m.SellerUserID), false)
}

// ---------------------------------------------------------------------------
// The advertiser's side
// ---------------------------------------------------------------------------

// CreateInput is POST /v1/me/listings: who the listing is for, and any of
// the editable fields (the same keys as a PATCH).
type CreateInput struct {
	// OrganisationID chooses the organisation; it may be left out when the
	// caller manages listings for exactly one and is not a direct owner too.
	OrganisationID *uuid.UUID
	// AsDirectOwner creates the caller's own listing (landlord, seller).
	AsDirectOwner bool
	Fields        map[string]json.RawMessage
}

// Create creates a DRAFT. The slug and reference are generated here.
func (mg *Manager) Create(ctx context.Context, p *auth.Principal, in CreateInput) (ManagedView, error) {
	if !p.ManagesListings() {
		return ManagedView{}, ErrForbidden
	}
	fields := map[string]string{}
	var m ManagedListing
	orgs := p.ListingOrganisationIDs()
	switch {
	case in.OrganisationID != nil && in.AsDirectOwner:
		fields["organisation_id"] = "cannot be combined with as_direct_owner"
	case in.OrganisationID != nil:
		if !p.MayCreateListing(*in.OrganisationID) {
			return ManagedView{}, ErrForbidden
		}
		org := *in.OrganisationID
		m.OrganisationID = &org
	case in.AsDirectOwner:
		if !p.IsDirectOwner() {
			return ManagedView{}, ErrForbidden
		}
		user := p.UserID
		m.SellerUserID = &user
	case len(orgs) == 1 && !p.IsDirectOwner():
		org := orgs[0]
		m.OrganisationID = &org
	case len(orgs) == 0 && p.IsDirectOwner():
		user := p.UserID
		m.SellerUserID = &user
	default:
		fields["organisation_id"] = "choose the organisation the listing is for, or send as_direct_owner: true"
	}
	if len(fields) > 0 {
		return ManagedView{}, &ValidationError{Fields: fields}
	}
	// An agent's listing is theirs; an owner's or manager's has no agent yet.
	defaultCountry := ""
	for _, ms := range p.Memberships {
		if m.OrganisationID != nil && ms.OrganisationID == *m.OrganisationID {
			defaultCountry = ms.MarketCode
			if ms.Role == auth.RoleAgent {
				user := p.UserID
				m.AgentUserID = &user
			}
		}
	}

	g, err := mg.svc.geography(ctx)
	if err != nil {
		return ManagedView{}, err
	}
	patch := decodePatch(in.Fields, fields)
	c := Content{MarketCode: defaultCountry, Features: []string{}}
	applyPatch(&c, patch, fields)
	if len(fields) > 0 {
		return ManagedView{}, &ValidationError{Fields: fields}
	}
	touched := map[string]bool{"country": true}
	for k := range patch {
		touched[k] = true
	}
	normalize(Content{}, &c, touched, g)
	if c.Currency == "" {
		c.Currency = g.Countries[c.MarketCode].Currency
	}
	if c.MarketCode == "" {
		fields["country"] = "is required"
	}
	if err := mg.svc.validateContent(ctx, c, g, fields); err != nil {
		return ManagedView{}, err
	}
	if len(fields) > 0 {
		return ManagedView{}, &ValidationError{Fields: fields}
	}
	if c.Currency == "" {
		return ManagedView{}, &ValidationError{Fields: map[string]string{"price": "currency is required"}}
	}

	now := mg.clock()
	creator := p.UserID
	m.ID, m.Status, m.UpdatedAt, m.CreatedAt, m.CreatedBy = uuid.New(), StatusDraft, now, now, &creator
	c.ApplyTo(&m.Record)
	for attempt := 0; ; attempt++ {
		m.Reference = "RP-" + c.MarketCode + "-" + randomDigits(6)
		m.Slug = slugFor(m.Title, m.Reference)
		err = mg.store.CreateListing(ctx, m, Change{Entry: mg.entry(m, p, ActorAdvertiser, ActionCreated, "", StatusDraft, "", "", now)})
		if !errors.Is(err, ErrDuplicate) || attempt >= 4 {
			break
		}
	}
	if err != nil {
		return ManagedView{}, err
	}
	mg.audit(ctx, "listing.created", p, m.ID)
	return mg.ownerView(ctx, p, m.ID)
}

// Get returns a listing the caller may see.
func (mg *Manager) Get(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	m, err := mg.readable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	return mg.view(ctx, m, p.MayEditListing(m.OrganisationID, m.AgentUserID, m.SellerUserID), false)
}

// Update applies a partial update (PATCH). A listing that is not public is
// changed at once; a published one through its revision.
func (mg *Manager) Update(ctx context.Context, p *auth.Principal, id uuid.UUID, raw map[string]json.RawMessage) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	mode, err := editMode(m)
	if err != nil {
		return ManagedView{}, err
	}
	fields := map[string]string{}
	patch := decodePatch(raw, fields)
	before := m.WorkingContent()
	c := m.WorkingContent()
	applyPatch(&c, patch, fields)
	if len(fields) > 0 {
		return ManagedView{}, &ValidationError{Fields: fields}
	}
	if len(patch) == 0 {
		return mg.view(ctx, m, true, false)
	}
	g, err := mg.svc.geography(ctx)
	if err != nil {
		return ManagedView{}, err
	}
	touched := map[string]bool{}
	for k := range patch {
		touched[k] = true
	}
	normalize(before, &c, touched, g)
	if c.MarketCode == "" {
		fields["country"] = "is required"
	}
	if mode == editRevision && c.MarketCode != m.MarketCode {
		fields["country"] = "cannot be changed once the listing has been published"
	}
	if err := mg.svc.validateContent(ctx, c, g, fields); err != nil {
		return ManagedView{}, err
	}
	if len(fields) > 0 {
		return ManagedView{}, &ValidationError{Fields: fields}
	}
	now := mg.clock()
	expect := m.UpdatedAt
	if mode == editDirect {
		c.ApplyTo(&m.Record)
		if m.PublishedAt.IsZero() {
			m.Slug = slugFor(m.Title, m.Reference)
		}
	} else {
		rev := ensureRevision(&m, now)
		rev.Content = &c
	}
	m.UpdatedAt = now
	if err := mg.save(ctx, m, expect, Change{}); err != nil {
		return ManagedView{}, err
	}
	mg.audit(ctx, "listing.updated", p, m.ID, slog.String("mode", mode))
	return mg.ownerView(ctx, p, m.ID)
}

// Submit sends a draft, a listing with requested changes or the pending
// changes of a published listing for review.
func (mg *Manager) Submit(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	g, err := mg.svc.geography(ctx)
	if err != nil {
		return ManagedView{}, err
	}
	var alts map[string]string
	if m.Revision != nil {
		alts = m.Revision.PhotoAlts
	}
	now := mg.clock()
	expect := m.UpdatedAt
	user := p.UserID
	var entry *TimelineEntry
	switch {
	case m.Status == StatusDraft || m.Status == StatusChangesRequested:
		if c := completeness(m.WorkingContent(), m.AllPhotos, alts, g); !c.Ready {
			return ManagedView{}, &IncompleteError{Missing: c.Missing}
		}
		entry = mg.entry(m, p, ActorAdvertiser, ActionSubmitted, m.Status, StatusPendingReview, ReviewKindNew, "", now)
		m.Status = StatusPendingReview
	case m.Status == StatusPublished && m.Revision != nil &&
		(m.Revision.Status == RevisionEditing || m.Revision.Status == RevisionChangesRequested):
		if c := completeness(m.WorkingContent(), m.AllPhotos, alts, g); !c.Ready {
			return ManagedView{}, &IncompleteError{Missing: c.Missing}
		}
		entry = mg.entry(m, p, ActorAdvertiser, ActionSubmitted, StatusPublished, StatusPublished, ReviewKindEdit, "", now)
		m.Revision.Status, m.Revision.SubmittedAt, m.Revision.UpdatedAt = RevisionPendingReview, &now, now
	case m.Status == StatusPublished:
		return ManagedView{}, conflict("There are no changes to submit.")
	case m.Status == StatusPendingReview || (m.Revision != nil && m.Revision.Status == RevisionPendingReview):
		return ManagedView{}, errInReview
	default:
		return ManagedView{}, conflict(fmt.Sprintf("A listing that is %s cannot be submitted.", strings.ToLower(StatusLabel(m.EffectiveStatus(now)))))
	}
	m.SubmittedAt, m.SubmittedBy, m.UpdatedAt = &now, &user, now
	if err := mg.save(ctx, m, expect, Change{Entry: entry}); err != nil {
		return ManagedView{}, err
	}
	mg.audit(ctx, "listing.submitted", p, m.ID, slog.String("review_kind", entry.ReviewKind))
	return mg.ownerView(ctx, p, m.ID)
}

// Archive withdraws a listing. A published one leaves the public catalogue
// at once; its pending changes are kept on the listing for a relist.
func (mg *Manager) Archive(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	now := mg.clock()
	from := m.EffectiveStatus(now)
	switch from {
	case StatusDraft, StatusPendingReview, StatusChangesRequested, StatusPublished, StatusExpired, StatusPaused:
	default:
		return ManagedView{}, conflict(fmt.Sprintf("A listing that is %s cannot be archived.", strings.ToLower(StatusLabel(from))))
	}
	expect := m.UpdatedAt
	wasPublic := m.Public(now)
	mergeRevision(&m)
	m.Status, m.UpdatedAt = StatusArchived, now
	ch := Change{Entry: mg.entry(m, p, ActorAdvertiser, ActionArchived, from, StatusArchived, "", "", now)}
	if wasPublic {
		ch.Events = append(ch.Events, indexEvent(EventListingUnpublished, m.ID))
	}
	if err := mg.save(ctx, m, expect, ch); err != nil {
		return ManagedView{}, err
	}
	mg.audit(ctx, "listing.archived", p, m.ID)
	return mg.ownerView(ctx, p, m.ID)
}

// Relist brings back an archived or expired listing: to review when it has
// been published before (a new publication cycle starts on approval), to
// DRAFT when it never was.
func (mg *Manager) Relist(ctx context.Context, p *auth.Principal, id uuid.UUID) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	now := mg.clock()
	from := m.EffectiveStatus(now)
	if !slices.Contains(relistFrom, from) {
		return ManagedView{}, conflict("Only an archived, expired, sold, let or closed listing can be relisted.")
	}
	expect := m.UpdatedAt
	mergeRevision(&m)
	to, kind := StatusDraft, ""
	if !m.PublishedAt.IsZero() {
		g, err := mg.svc.geography(ctx)
		if err != nil {
			return ManagedView{}, err
		}
		if c := completeness(ContentOf(m.Record), m.AllPhotos, nil, g); !c.Ready {
			return ManagedView{}, &IncompleteError{Missing: c.Missing}
		}
		to, kind = StatusPendingReview, ReviewKindNew
		user := p.UserID
		m.SubmittedAt, m.SubmittedBy = &now, &user
	}
	m.Status, m.UpdatedAt, m.ReviewReason = to, now, ""
	ch := Change{Entry: mg.entry(m, p, ActorAdvertiser, ActionRelisted, from, to, kind, "", now),
		Events: []ChangeEvent{indexEvent(EventListingUnpublished, m.ID), relistEvent(m.ID, now)}}
	if err := mg.save(ctx, m, expect, ch); err != nil {
		return ManagedView{}, err
	}
	mg.audit(ctx, "listing.relisted", p, m.ID, slog.String("to", to))
	return mg.ownerView(ctx, p, m.ID)
}

// Delete removes a listing that has never been published, with its photos
// and their stored files.
func (mg *Manager) Delete(ctx context.Context, p *auth.Principal, id uuid.UUID) error {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return err
	}
	if !m.PublishedAt.IsZero() {
		return conflict("A listing that has been published cannot be deleted. Archive it instead.")
	}
	removed, err := mg.store.DeleteListing(ctx, id, m.UpdatedAt)
	if err != nil {
		return err
	}
	mg.removeObjects(ctx, removed)
	mg.audit(ctx, "listing.deleted", p, id, slog.Int("photos", len(removed)))
	return nil
}

// ---------------------------------------------------------------------------
// Photos
// ---------------------------------------------------------------------------

// AddPhoto stores an uploaded photo (three sizes) and adds it at the end.
// The 21st photo is refused with ErrPhotoLimit, before and while storing.
func (mg *Manager) AddPhoto(ctx context.Context, p *auth.Principal, id uuid.UUID, content []byte) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	mode, err := editMode(m)
	if err != nil {
		return ManagedView{}, err
	}
	if len(m.AllPhotos) >= MaxPhotosPerListing {
		return ManagedView{}, ErrPhotoLimit
	}
	if len(content) == 0 {
		return ManagedView{}, &ValidationError{Fields: map[string]string{"file": "is required and must not be empty"}}
	}
	if mg.files == nil {
		return ManagedView{}, errors.New("listings: no file storage configured")
	}
	processed, err := ProcessPhoto(content)
	if err != nil {
		return ManagedView{}, err
	}
	// The ORIGINAL upload is scanned for malware before any size is stored
	// (API 0.41.0): 422 FILE_INFECTED, or 503 SCANNER_UNAVAILABLE.
	if err := storage.Screen(ctx, mg.files, "listing_photo", content); err != nil {
		return ManagedView{}, err
	}
	now := mg.clock()
	user := p.UserID
	photo := StoredPhoto{ID: uuid.New(), Checksum: processed.Checksum, Width: processed.Width, Height: processed.Height, UploadedBy: &user, CreatedAt: now}
	for _, v := range processed.Variants {
		key, err := storage.NewKey(PhotoKeyPrefix)
		if err == nil {
			err = mg.files.Put(ctx, key, bytes.NewReader(v.Data), int64(len(v.Data)), photoTypeJPEG)
		}
		if err != nil {
			mg.removeObjects(ctx, []StoredPhoto{photo})
			return ManagedView{}, fmt.Errorf("store photo: %w", err)
		}
		photo.Variants = append(photo.Variants, StoredVariant{Size: v.Size, Key: key, Width: v.Width, Height: v.Height, Bytes: int64(len(v.Data))})
	}
	expect := m.UpdatedAt
	// A new photo of a published listing is part of its pending changes.
	if mode == editRevision && (m.Revision == nil || m.Revision.Status == RevisionRejected) {
		ensureRevision(&m, now)
		m.UpdatedAt = now
		if err := mg.save(ctx, m, expect, Change{}); err != nil {
			mg.removeObjects(ctx, []StoredPhoto{photo})
			return ManagedView{}, err
		}
		expect = now
	}
	if err := mg.store.AddPhoto(ctx, id, photo, MaxPhotosPerListing, expect, now, Change{}); err != nil {
		mg.removeObjects(ctx, []StoredPhoto{photo})
		return ManagedView{}, err
	}
	mg.audit(ctx, "listing.photo_added", p, id, slog.String("photo_id", photo.ID.String()))
	return mg.ownerView(ctx, p, id)
}

func photoIndex(m ManagedListing, photoID uuid.UUID) int {
	for i := range m.AllPhotos {
		if m.AllPhotos[i].ID == photoID {
			return i
		}
	}
	return -1
}

// SetPhotoAlt changes a photo's alternative text. The text of a public photo
// of a published listing changes with the listing's pending changes.
func (mg *Manager) SetPhotoAlt(ctx context.Context, p *auth.Principal, id, photoID uuid.UUID, alt string) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	mode, err := editMode(m)
	if err != nil {
		return ManagedView{}, err
	}
	i := photoIndex(m, photoID)
	if i < 0 {
		return ManagedView{}, ErrPhotoNotFound
	}
	alt = strings.TrimSpace(alt)
	if problem := plainText(alt, false, MaxPhotoAltLength); problem != "" {
		return ManagedView{}, &ValidationError{Fields: map[string]string{"alt": problem}}
	}
	now := mg.clock()
	expect := m.UpdatedAt
	if mode == editRevision && m.AllPhotos[i].ApprovedAt != nil {
		rev := ensureRevision(&m, now)
		if rev.PhotoAlts == nil {
			rev.PhotoAlts = map[string]string{}
		}
		rev.PhotoAlts[photoID.String()] = alt
		if alt == m.AllPhotos[i].Alt {
			delete(rev.PhotoAlts, photoID.String())
		}
	} else {
		m.AllPhotos[i].Alt = alt
	}
	m.UpdatedAt = now
	if err := mg.save(ctx, m, expect, Change{}); err != nil {
		return ManagedView{}, err
	}
	return mg.ownerView(ctx, p, id)
}

// ReorderPhotos puts the photos in the given order (every photo exactly once).
// The order applies at once, also on a published listing.
func (mg *Manager) ReorderPhotos(ctx context.Context, p *auth.Principal, id uuid.UUID, ids []uuid.UUID) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	if _, err := editMode(m); err != nil {
		return ManagedView{}, err
	}
	if len(ids) != len(m.AllPhotos) {
		return ManagedView{}, &ValidationError{Fields: map[string]string{"photo_ids": "must list every photo of the listing exactly once"}}
	}
	for pos, photoID := range ids {
		i := photoIndex(m, photoID)
		if i < 0 || slices.Index(ids, photoID) != pos {
			return ManagedView{}, &ValidationError{Fields: map[string]string{"photo_ids": "must list every photo of the listing exactly once"}}
		}
		m.AllPhotos[i].Position = pos
	}
	now := mg.clock()
	expect := m.UpdatedAt
	ch := Change{}
	if m.Public(now) {
		ch.Events = append(ch.Events, indexEvent(EventListingUpdated, m.ID))
	}
	m.UpdatedAt = now
	if err := mg.save(ctx, m, expect, ch); err != nil {
		return ManagedView{}, err
	}
	return mg.ownerView(ctx, p, id)
}

// DeletePhoto removes a photo and its stored files. A public photo of a
// published listing disappears at once; the last one cannot be removed.
func (mg *Manager) DeletePhoto(ctx context.Context, p *auth.Principal, id, photoID uuid.UUID) (ManagedView, error) {
	m, err := mg.editable(ctx, p, id)
	if err != nil {
		return ManagedView{}, err
	}
	if _, err := editMode(m); err != nil {
		return ManagedView{}, err
	}
	i := photoIndex(m, photoID)
	if i < 0 {
		return ManagedView{}, ErrPhotoNotFound
	}
	photo := m.AllPhotos[i]
	if photo.ApprovedAt != nil && m.Status == StatusPublished {
		approved := 0
		for _, ph := range m.AllPhotos {
			if ph.ApprovedAt != nil {
				approved++
			}
		}
		if approved <= 1 {
			return ManagedView{}, ErrPhotoRequired
		}
	}
	now := mg.clock()
	expect := m.UpdatedAt
	ch := Change{}
	if photo.ApprovedAt != nil && m.Public(now) {
		ch.Events = append(ch.Events, indexEvent(EventListingUpdated, m.ID))
	}
	m.AllPhotos = slices.Delete(m.AllPhotos, i, i+1)
	if m.Revision != nil {
		delete(m.Revision.PhotoAlts, photoID.String())
	}
	m.UpdatedAt = now
	if err := mg.save(ctx, m, expect, ch); err != nil {
		return ManagedView{}, err
	}
	mg.audit(ctx, "listing.photo_deleted", p, id, slog.String("photo_id", photoID.String()))
	return mg.ownerView(ctx, p, id)
}

// PhotoFile is one stored size of a photo, ready to be streamed.
type PhotoFile struct {
	Content io.ReadCloser
	Size    int64
	ETag    string
	// Public is true when the listing and the photo are public: the answer is
	// the same for everyone and may be cached by anyone.
	Public bool
	// Versioned is true when the request named the photo's current version.
	Versioned bool
}

// OpenPhoto returns one size of a photo: to anyone while the listing and the
// photo are public; otherwise only to the listing's managers and to its
// reviewers (p may be nil). Anything else is ErrPhotoNotFound.
func (mg *Manager) OpenPhoto(ctx context.Context, p *auth.Principal, photoID uuid.UUID, size, version string) (PhotoFile, error) {
	listingID, photo, err := mg.store.PhotoByID(ctx, photoID)
	if err != nil {
		return PhotoFile{}, err
	}
	variant, ok := photo.Variant(size)
	if !photo.Uploaded() || !ok || mg.files == nil {
		return PhotoFile{}, ErrPhotoNotFound
	}
	records, err := mg.svc.store.ByIDs(ctx, []uuid.UUID{listingID})
	if err != nil {
		return PhotoFile{}, err
	}
	if len(records) == 0 {
		return PhotoFile{}, ErrPhotoNotFound
	}
	r := records[0]
	public := r.Public(mg.clock()) && photo.ApprovedAt != nil
	if !public && (p == nil || !(p.MayManageListing(r.OrganisationID, r.SellerUserID) ||
		p.MayEditListing(r.OrganisationID, r.AgentUserID, r.SellerUserID) || p.MayReviewListing(r.MarketCode))) {
		return PhotoFile{}, ErrPhotoNotFound
	}
	rc, err := mg.files.Open(ctx, variant.Key)
	if errors.Is(err, storage.ErrNotFound) {
		return PhotoFile{}, ErrPhotoNotFound
	}
	if err != nil {
		return PhotoFile{}, fmt.Errorf("open photo: %w", err)
	}
	return PhotoFile{Content: rc, Size: variant.Bytes, ETag: `"` + photo.Checksum[:min(32, len(photo.Checksum))] + "-" + size + `"`,
		Public: public, Versioned: version != "" && version == LogoVersion(photo.Checksum)}, nil
}

// ---------------------------------------------------------------------------
// The reviewers' side
// ---------------------------------------------------------------------------

// Review decisions.
const (
	DecisionApprove        = "approve"
	DecisionRequestChanges = "request_changes"
	DecisionReject         = "reject"
)

// Queue returns one page of the review queue, scoped to the reviewer's countries.
func (mg *Manager) Queue(ctx context.Context, p *auth.Principal, f ReviewFilter) (QueuePage, error) {
	codes, all, ok := p.ListingReviewMarkets()
	if !ok {
		return QueuePage{}, ErrForbidden
	}
	if !all {
		f.Markets = codes
		if f.Markets == nil {
			f.Markets = []string{}
		}
	}
	items, total, err := mg.store.ReviewQueue(ctx, f)
	if err != nil {
		return QueuePage{}, err
	}
	labels, err := mg.svc.labelSets(ctx)
	if err != nil {
		return QueuePage{}, err
	}
	page := QueuePage{Items: []QueueItem{}, Page: f.Page, PageSize: f.PageSize, Total: total, TotalPages: (total + f.PageSize - 1) / f.PageSize}
	now := mg.clock()
	for _, m := range items {
		it := QueueItem{ID: m.ID, Reference: m.Reference, Title: m.WorkingContent().Title, Status: m.Status, ReviewKind: opt(m.ReviewKind()),
			Country: Country{Code: m.MarketCode, Name: m.MarketCode}, SubmittedAt: utcTime(m.SubmittedAt), UpdatedAt: m.UpdatedAt.UTC(),
			PhotoCount: len(m.AllPhotos), Expiry: ExpiryOf(m.Record, now)}
		if name := labels[keyCountryNames][m.MarketCode]; name != "" {
			it.Country.Name = name
		}
		if m.Revision != nil {
			it.RevisionStatus = opt(m.Revision.Status)
		}
		for _, ph := range m.AllPhotos {
			if ph.ApprovedAt == nil {
				it.PendingPhotoCount++
			}
		}
		a := presentAdvertiser(m.Record, labels)
		it.Advertiser.Type, it.Advertiser.Name, it.Advertiser.Verified = a.Type, a.Name, a.Verified
		page.Items = append(page.Items, it)
	}
	return page, nil
}

func (mg *Manager) adminView(ctx context.Context, m ManagedListing) (AdminView, error) {
	v, err := mg.view(ctx, m, false, true)
	if err != nil {
		return AdminView{}, err
	}
	out := AdminView{ManagedView: v, Changes: []FieldChange{}, Decisions: []string{}}
	kind := m.ReviewKind()
	out.ReviewKind = opt(kind)
	if kind != "" {
		out.Decisions = []string{DecisionApprove, DecisionRequestChanges, DecisionReject}
	}
	if (m.Status == StatusPublished || m.Status == StatusExpired) && m.Revision != nil {
		live := contentView(ContentOf(m.Record))
		out.Live = &live
		if m.Revision.Content != nil {
			out.Changes = diff(ContentOf(m.Record), *m.Revision.Content)
		}
		for _, ph := range m.AllPhotos {
			if ph.ApprovedAt == nil {
				out.PhotoChanges.Added++
			}
			if alt, ok := m.Revision.PhotoAlts[ph.ID.String()]; ok && ph.ApprovedAt != nil && alt != ph.Alt {
				out.PhotoChanges.AltChanged++
			}
		}
	} else {
		for _, ph := range m.AllPhotos {
			if ph.ApprovedAt == nil {
				out.PhotoChanges.Added++
			}
		}
	}
	return out, nil
}

// AdminGet returns a listing as its reviewer sees it.
func (mg *Manager) AdminGet(ctx context.Context, p *auth.Principal, id uuid.UUID) (AdminView, error) {
	m, err := mg.reviewable(ctx, p, id)
	if err != nil {
		return AdminView{}, err
	}
	return mg.adminView(ctx, m)
}

// checkReason validates a reviewer's reason.
func checkReason(reason string) (string, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "", &ValidationError{Fields: map[string]string{"reason": "is required"}}
	}
	if problem := plainText(reason, true, MaxReasonLength); problem != "" {
		return "", &ValidationError{Fields: map[string]string{"reason": problem}}
	}
	return reason, nil
}

func reviewEvent(eventType string, m ManagedListing, kind, reason string, now time.Time) ChangeEvent {
	raw, _ := json.Marshal(ReviewEventPayload{ListingID: m.ID, ReviewKind: kind, Reason: reason, OccurredAt: now})
	return ChangeEvent{Type: eventType, Payload: raw}
}

// relistApproval is the LISTING_APPROVED event of a relist's approval.
func relistApproval(m ManagedListing, now time.Time) ChangeEvent {
	raw, _ := json.Marshal(ReviewEventPayload{ListingID: m.ID, ReviewKind: ReviewKindNew, Relist: true, OccurredAt: now})
	return ChangeEvent{Type: EventListingApproved, Payload: raw}
}

// Decide records a reviewer's decision on a listing waiting for review. A
// reason is required to request changes or to reject.
func (mg *Manager) Decide(ctx context.Context, p *auth.Principal, id uuid.UUID, decision, reason string) (AdminView, error) {
	m, err := mg.reviewable(ctx, p, id)
	if err != nil {
		return AdminView{}, err
	}
	if selfReview(p, m) {
		mg.audit(ctx, "listing.self_review_refused", p, m.ID, slog.String("decision", decision))
		return AdminView{}, ErrSelfReview
	}
	kind := m.ReviewKind()
	if kind == "" {
		return AdminView{}, conflict("This listing is not waiting for review.")
	}
	if decision != DecisionApprove {
		if reason, err = checkReason(reason); err != nil {
			return AdminView{}, err
		}
	} else {
		reason = ""
	}
	now := mg.clock()
	expect := m.UpdatedAt
	reviewer := p.UserID
	from := m.Status
	ch := Change{}
	switch {
	case decision == DecisionApprove && kind == ReviewKindNew:
		// A listing that was published before is being relisted: the
		// approval starts its next cycle and the advertiser is told it is
		// live again (listing-relisted, API 0.45.0).
		relist := !m.PublishedAt.IsZero()
		approved := reviewEvent(EventListingApproved, m, kind, "", now)
		if relist {
			approved = relistApproval(m, now)
			// A listing published without a cycle row (the sample data)
			// still had a first cycle: this one is the second.
			ch.MinRelistNo = 1
		}
		m.Status, m.PublishedAt, m.ExpiresAt = StatusPublished, now, now.Add(PublicationPeriod)
		m.ReviewedAt, m.ReviewedBy, m.ReviewReason, m.Revision = &now, &reviewer, "", nil
		ch.NewCycle = true
		// The go-live: refused when the advertiser's package is full (FR-008).
		ch.Quota = quotaCheck(m, now)
		ch.Events = []ChangeEvent{indexEvent(EventListingPublished, m.ID), approved}
	case decision == DecisionApprove:
		mergeRevision(&m)
		m.ReviewedAt, m.ReviewedBy = &now, &reviewer
		ch.Events = []ChangeEvent{indexEvent(EventListingUpdated, m.ID), reviewEvent(EventListingApproved, m, kind, "", now)}
	case decision == DecisionRequestChanges && kind == ReviewKindNew:
		m.Status, m.ReviewReason = StatusChangesRequested, reason
		ch.Events = []ChangeEvent{reviewEvent(EventListingChangesRequested, m, kind, reason, now)}
	case decision == DecisionRequestChanges:
		m.Revision.Status, m.Revision.Reason, m.Revision.UpdatedAt = RevisionChangesRequested, reason, now
		ch.Events = []ChangeEvent{reviewEvent(EventListingChangesRequested, m, kind, reason, now)}
	case decision == DecisionReject && kind == ReviewKindNew:
		m.Status, m.ReviewReason = StatusRejected, reason
		ch.Events = []ChangeEvent{reviewEvent(EventListingRejected, m, kind, reason, now)}
	case decision == DecisionReject:
		// The changes are dropped, and the photos that came with them.
		m.Revision = &Revision{Status: RevisionRejected, Reason: reason, UpdatedAt: now}
		m.AllPhotos = slices.DeleteFunc(m.AllPhotos, func(ph StoredPhoto) bool { return ph.ApprovedAt == nil })
		ch.Events = []ChangeEvent{reviewEvent(EventListingRejected, m, kind, reason, now)}
	default:
		return AdminView{}, &ValidationError{Fields: map[string]string{"decision": "must be approve, request_changes or reject"}}
	}
	if decision == DecisionApprove {
		for i := range m.AllPhotos {
			if m.AllPhotos[i].ApprovedAt == nil {
				m.AllPhotos[i].ApprovedAt = &now
			}
		}
	}
	action := map[string]string{DecisionApprove: ActionApproved, DecisionRequestChanges: ActionChangesRequested, DecisionReject: ActionRejected}[decision]
	ch.Entry = mg.entry(m, p, ActorReviewer, action, from, m.Status, kind, reason, now)
	m.UpdatedAt = now
	if err := mg.save(ctx, m, expect, ch); err != nil {
		return AdminView{}, err
	}
	mg.audit(ctx, "listing.review_"+decision, p, m.ID, slog.String("review_kind", kind))
	saved, err := mg.store.Managed(ctx, id)
	if err != nil {
		return AdminView{}, err
	}
	return mg.adminView(ctx, saved)
}
