package propertyrequests

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes of the endpoints in this file.
const (
	CodeRequestNotFound        = "PROPERTY_REQUEST_NOT_FOUND"
	CodeInvalidStageTransition = "INVALID_STAGE_TRANSITION"
	CodeAlreadyResolved        = "ALREADY_RESOLVED"
)

// MarketIDs resolves country codes to market IDs.
type MarketIDs func(ctx context.Context, codes []string) ([]uuid.UUID, error)

// SetMarkets lets a market administrator see the routing exceptions of their
// own markets.
func (h *Handler) SetMarkets(markets MarketIDs) { h.markets = markets }

// requestTypes are the types the type filter accepts.
func requestTypes() []string {
	return []string{TypeEnquiry, TypePropertyInformation, TypeViewingRequest, TypeConsultationRequest, TypeStayRequest}
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeRequestNotFound, "This request does not exist.", nil)
	case errors.Is(err, ErrBranchForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, "BRANCH_FORBIDDEN", "This request belongs to another branch of your organisation.", nil)
	case errors.Is(err, ErrInvalidTransition):
		httpx.WriteError(w, r, http.StatusConflict, CodeInvalidStageTransition, "This request cannot move to that stage from its current stage.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Only the organisation's owner and managers can assign its requests.", nil)
	case errors.Is(err, ErrAlreadyResolved):
		httpx.WriteError(w, r, http.StatusConflict, CodeAlreadyResolved, "This routing exception was already resolved.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func unauthenticated(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
}

func forbidden(w http.ResponseWriter, r *http.Request) {
	httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
}

// ScopeOf builds the inbox scope of a caller from their user ID and
// memberships only (see InboxScope).
func ScopeOf(p *auth.Principal) InboxScope {
	s := InboxScope{
		UserID:                 p.UserID,
		ManagedOrganisationIDs: p.OrganisationIDs(auth.RoleOwner, auth.RoleManager),
		MemberOrganisationIDs:  p.OrganisationIDs(auth.RoleAgent, auth.RoleStaff),
	}
	for _, m := range p.Memberships {
		if m.BranchID != nil {
			if s.Branches == nil {
				s.Branches = map[uuid.UUID]uuid.UUID{}
			}
			s.Branches[m.OrganisationID] = *m.BranchID
		}
	}
	return s
}

// MayOpenInbox reports whether the caller may use the request inbox at all:
//   - the caller has an organisation membership; or
//   - the caller is the direct seller of at least one listing; or
//   - the caller declared a direct-owner account role (any status).
//
// The third case is the single place an account role affects access, and all
// it does is let a would-be seller open an inbox that is empty until they
// actually sell something. WHAT is returned never depends on account roles.
func (s *Service) MayOpenInbox(ctx context.Context, p *auth.Principal) (bool, error) {
	if p.HasMembership() || p.DeclaresDirectOwner() {
		return true, nil
	}
	return s.IsDirectSeller(ctx, p.UserID)
}

// receiver returns the caller and their scope for the inbox endpoints: 401
// when anonymous, 403 when the caller may not open an inbox.
func (h *Handler) receiver(w http.ResponseWriter, r *http.Request) (*auth.Principal, InboxScope, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		unauthenticated(w, r)
		return nil, InboxScope{}, false
	}
	ok, err := h.svc.MayOpenInbox(r.Context(), p)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return nil, InboxScope{}, false
	}
	if !ok {
		forbidden(w, r)
		return nil, InboxScope{}, false
	}
	return p, ScopeOf(p), true
}

func pathID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}

// paging reads page and page_size.
func paging(r *http.Request, fields map[string]string) (page, size int) {
	q := r.URL.Query()
	page, size = 1, DefaultPageSize
	if raw := q.Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := q.Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			size = n
		}
	}
	return page, size
}

// codes reads a repeatable, comma-separated parameter of known codes.
func codes(r *http.Request, name string, known []string, fields map[string]string) []string {
	var out []string
	for _, raw := range r.URL.Query()[name] {
		for _, c := range strings.Split(raw, ",") {
			if c = strings.TrimSpace(c); c == "" {
				continue
			}
			if !slices.Contains(known, c) {
				fields[name] = "must be one of " + strings.Join(known, ", ")
				continue
			}
			if !slices.Contains(out, c) {
				out = append(out, c)
			}
		}
	}
	return out
}

func totalPages(total, size int) int {
	if total == 0 {
		return 0
	}
	return (total + size - 1) / size
}

// name fills the listing summaries of items, by listing ID.
func (h *Handler) listingNames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]InboxListing, error) {
	if h.names == nil || len(ids) == 0 {
		return map[uuid.UUID]InboxListing{}, nil
	}
	return h.names.ListingNames(ctx, ids)
}

func listingOf(names map[uuid.UUID]InboxListing, id uuid.UUID) *InboxListing {
	if name, ok := names[id]; ok {
		return &name
	}
	return nil
}

func listingIDs(items []StoredRequest) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.ListingID)
	}
	return ids
}

// ---------------------------------------------------------------------------
// The receiving side
// ---------------------------------------------------------------------------

// StageView is a stage with its wording.
type StageView struct {
	Stage string `json:"stage"`
	Label string `json:"label"`
}

// StageCount is the number of requests at a stage.
type StageCount struct {
	Stage string `json:"stage"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// StageCountsOf lists every stage in pipeline order with its count.
func StageCountsOf(counts map[string]int) []StageCount {
	out := make([]StageCount, 0, len(stageLabels))
	for _, stage := range Stages() {
		out = append(out, StageCount{Stage: stage, Label: StageLabel(stage), Count: counts[stage]})
	}
	return out
}

// inboxResponse is the body of GET /v1/agent/property-requests.
type inboxResponse struct {
	Items       []InboxItem  `json:"items"`
	Page        int          `json:"page"`
	PageSize    int          `json:"page_size"`
	Total       int          `json:"total"`
	TotalPages  int          `json:"total_pages"`
	StageCounts []StageCount `json:"stage_counts"`
}

// Items builds the inbox views of stored requests, each with its listing.
func (h *Handler) Items(ctx context.Context, stored []StoredRequest) ([]InboxItem, error) {
	names, err := h.listingNames(ctx, listingIDs(stored))
	if err != nil {
		return nil, err
	}
	assignees, err := h.svc.AssigneeNames(ctx, stored)
	if err != nil {
		return nil, err
	}
	items := make([]InboxItem, 0, len(stored))
	for _, s := range stored {
		item := ItemOf(s)
		item.Listing = listingOf(names, s.ListingID)
		if id := s.AssignedUserID(); id != nil {
			item.AssignedTo = &AssigneeView{UserID: *id, Name: assignees[*id]}
		}
		items = append(items, item)
	}
	return items, nil
}

// Inbox serves GET /v1/agent/property-requests: the requests received by the
// caller (those of their organisations, scoped by role, plus those routed to
// them as the direct seller of a listing), newest first, one page at a time.
// The filters only narrow what the scope allows; nothing a client sends can
// widen it.
func (h *Handler) Inbox(w http.ResponseWriter, r *http.Request) {
	_, scope, ok := h.receiver(w, r)
	if !ok {
		return
	}
	q, fields := r.URL.Query(), map[string]string{}
	f := InboxFilter{Scope: scope}
	page, size := paging(r, fields)
	f.Stages = codes(r, "stage", Stages(), fields)
	if types := codes(r, "type", requestTypes(), fields); len(types) > 1 {
		fields["type"] = "must be a single request type"
	} else if len(types) == 1 {
		f.Type = types[0]
	}
	if raw := q.Get("listing_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fields["listing_id"] = "must be a UUID"
		} else {
			f.ListingID = &id
		}
	}
	for name, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if raw := q.Get(name); raw != "" {
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				fields[name] = "must be an RFC 3339 timestamp"
				continue
			}
			t = t.UTC()
			*dst = &t
		}
	}
	if f.From != nil && f.To != nil && !f.From.Before(*f.To) {
		fields["to"] = "must be after from"
	}
	if f.Q = strings.TrimSpace(q.Get("q")); len([]rune(f.Q)) > MaxSearchLength {
		fields["q"] = "must be at most " + strconv.Itoa(MaxSearchLength) + " characters"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	f.Limit, f.Offset = size, (page-1)*size

	result, err := h.svc.InboxPage(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.Items(r.Context(), result.Items)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, inboxResponse{
		Items: items, Page: page, PageSize: size, Total: result.Total, TotalPages: totalPages(result.Total, size),
		StageCounts: StageCountsOf(result.StageCounts),
	})
}

type timelineView struct {
	Event string `json:"event"`
	Label string `json:"label"`
	// FromStage and ToStage are set for STAGE_CHANGED only.
	FromStage *string `json:"from_stage"`
	ToStage   *string `json:"to_stage"`
	// Actor is CUSTOMER, PROFESSIONAL, ADMIN or SYSTEM.
	Actor     string  `json:"actor"`
	ActorName *string `json:"actor_name"`
	// AssigneeName is who an ASSIGNED entry gave the request to.
	AssigneeName *string   `json:"assignee_name"`
	At           time.Time `json:"at"`
}

type noteView struct {
	ID         uuid.UUID `json:"id"`
	Body       string    `json:"body"`
	AuthorName string    `json:"author_name"`
	// Mine is true for a note the caller wrote.
	Mine      bool      `json:"mine"`
	CreatedAt time.Time `json:"created_at"`
}

// detailView is a request as the receiving side sees it.
type detailView struct {
	InboxItem
	PreferredSlotUTC *time.Time  `json:"preferred_slot_utc"`
	UpdatedAt        time.Time   `json:"updated_at"`
	AllowedStages    []StageView `json:"allowed_stages"`
	// CanAssign: the caller may assign the request (its OWNER or MANAGER).
	CanAssign bool           `json:"can_assign"`
	Timeline  []timelineView `json:"timeline"`
	Notes     []noteView     `json:"notes"`
	// Messages is the conversation with the customer, oldest first (API
	// 0.46.0). CanReply is false once the request is CLOSED.
	Messages []MessageView `json:"messages"`
	CanReply bool          `json:"can_reply"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func professionalTimeline(entries []TimelineEntry) []timelineView {
	out := make([]timelineView, 0, len(entries))
	for _, e := range entries {
		v := timelineView{Event: e.EventType, Actor: e.ActorType, ActorName: optional(e.ActorName), At: e.At}
		if e.ActorType == ActorProfessional {
			v.Actor = "PROFESSIONAL"
		}
		switch e.EventType {
		case TimelineRequestReceived:
			v.Label = "Request received"
		case TimelineStageChanged:
			v.Label = "Moved to " + StageLabel(e.ToStage)
			v.FromStage, v.ToStage = optional(e.FromStage), optional(e.ToStage)
		case TimelineNoteAdded:
			v.Label = "Note added"
		case TimelineAssigned:
			v.Label = "Assigned to " + e.AssigneeName
			if e.AssigneeName == "" {
				v.Label = "Assigned"
			}
			v.AssigneeName = optional(e.AssigneeName)
		case TimelineUnassigned:
			v.Label = "Returned to the organisation inbox"
		case TimelineReplied:
			v.Label = "Replied to the customer"
		case TimelineCustomerReplied:
			v.Label = "The customer replied"
		default:
			continue // nothing else belongs to the receiving side
		}
		out = append(out, v)
	}
	return out
}

func (h *Handler) writeDetail(w http.ResponseWriter, r *http.Request, p *auth.Principal, status int, req StoredRequest) {
	h.writeDetailFor(w, r, p, ScopeOf(p), status, req)
}

func (h *Handler) writeDetailFor(w http.ResponseWriter, r *http.Request, p *auth.Principal, scope InboxScope, status int, req StoredRequest) {
	entries, notes, err := h.svc.History(r.Context(), req.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.Items(r.Context(), []StoredRequest{req})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	msgs, err := h.svc.Messages(r.Context(), req.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := detailView{
		InboxItem: items[0], PreferredSlotUTC: req.PreferredSlot, UpdatedAt: req.UpdatedAt,
		AllowedStages: []StageView{}, Timeline: professionalTimeline(entries), Notes: make([]noteView, 0, len(notes)),
		CanAssign: scope.CanAssign(req), Messages: professionalMessages(msgs, p.UserID), CanReply: req.Stage != StageClosed,
	}
	for _, stage := range NextStages(req.Stage) {
		v.AllowedStages = append(v.AllowedStages, StageView{Stage: stage, Label: StageLabel(stage)})
	}
	for _, n := range notes {
		v.Notes = append(v.Notes, noteView{ID: n.ID, Body: n.Body, AuthorName: n.AuthorName, Mine: n.AuthorUserID == p.UserID, CreatedAt: n.CreatedAt})
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, status, map[string]any{"request": v})
}

// Get serves GET /v1/agent/property-requests/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := h.receiver(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	req, err := h.svc.Received(r.Context(), scope, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Opening the request reads the customer's messages ("awaiting your
	// reply" stays until somebody answers; this only records the visit).
	if err := h.svc.MarkRead(r.Context(), id, AuthorProfessional); err != nil {
		h.logger.WarnContext(r.Context(), "property request read marker not saved",
			slog.String("property_request_id", id.String()), slog.String("error", err.Error()))
	}
	h.writeDetail(w, r, p, http.StatusOK, req)
}

// Patch serves PATCH /v1/agent/property-requests/{id}: move the stage.
func (h *Handler) Patch(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := h.receiver(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in struct {
		Stage string `json:"stage"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	req, err := h.svc.MoveStage(r.Context(), scope, Actor{UserID: p.UserID, Name: p.Name}, id, in.Stage)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Identifiers and stages only: no message, no contact details.
	h.logger.InfoContext(r.Context(), "property request stage changed",
		slog.String("property_request_id", req.ID.String()), slog.String("stage", req.Stage))
	h.writeDetail(w, r, p, http.StatusOK, req)
}

// Assign serves POST /v1/agent/property-requests/{id}/assign: {"user_id":
// "..."} gives the request to a member, {"user_id": null} returns it to the
// organisation inbox. OWNER and MANAGER of the receiving organisation only.
func (h *Handler) Assign(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := h.receiver(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in struct {
		// Raw so that a missing key and null can be told apart.
		UserID json.RawMessage `json:"user_id"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	var to *uuid.UUID
	switch {
	case len(in.UserID) == 0:
		h.fail(w, r, &ValidationError{Fields: map[string]string{"user_id": "is required (null returns the request to the organisation inbox)"}})
		return
	case string(in.UserID) != "null":
		var raw string
		parsed, err := uuid.Nil, json.Unmarshal(in.UserID, &raw)
		if err == nil {
			parsed, err = uuid.Parse(strings.TrimSpace(raw))
		}
		if err != nil {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"user_id": "must be a UUID or null"}})
			return
		}
		to = &parsed
	}
	req, err := h.svc.Assign(r.Context(), scope, Actor{UserID: p.UserID, Name: p.Name}, id, to)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Identifiers only: no message, no contact details.
	attrs := []any{slog.String("property_request_id", req.ID.String()), slog.String("actor_user_id", p.UserID.String())}
	if to != nil {
		attrs = append(attrs, slog.String("assignee_user_id", to.String()))
	}
	h.logger.InfoContext(r.Context(), "property request assigned", attrs...)
	h.writeDetailFor(w, r, p, scope, http.StatusOK, req)
}

// AddNote serves POST /v1/agent/property-requests/{id}/notes.
func (h *Handler) AddNote(w http.ResponseWriter, r *http.Request) {
	p, scope, ok := h.receiver(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in struct {
		Body string `json:"body"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if _, err := h.svc.AddNote(r.Context(), scope, Actor{UserID: p.UserID, Name: p.Name}, id, in.Body); err != nil {
		h.fail(w, r, err)
		return
	}
	req, err := h.svc.Received(r.Context(), scope, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "property request note added", slog.String("property_request_id", id.String()))
	h.writeDetail(w, r, p, http.StatusCreated, req)
}

// ---------------------------------------------------------------------------
// The customer's own requests
// ---------------------------------------------------------------------------

// StatusCount is the number of the customer's requests with a status.
type StatusCount struct {
	Status string `json:"status"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
}

// StatusCountsOf lists every customer status in order with its count.
func StatusCountsOf(counts map[string]int) []StatusCount {
	out := make([]StatusCount, 0, len(customerLabels))
	for _, status := range CustomerStatuses() {
		out = append(out, StatusCount{Status: status, Label: CustomerStatusLabel(status), Count: counts[status]})
	}
	return out
}

type customerTimelineView struct {
	// Event is RECEIVED or STATUS_CHANGED.
	Event       string    `json:"event"`
	Status      string    `json:"status"`
	StatusLabel string    `json:"status_label"`
	At          time.Time `json:"at"`
}

// CustomerView is a request as the customer who sent it sees it. It carries
// what they sent and a status in their own terms: never the lead stage, never
// who handles the request, never an internal note, never whether routing failed.
type CustomerView struct {
	RequestID              uuid.UUID               `json:"request_id"`
	Reference              string                  `json:"reference"`
	ListingID              uuid.UUID               `json:"listing_id"`
	Listing                *InboxListing           `json:"listing"`
	RequestType            string                  `json:"request_type"`
	Status                 string                  `json:"status"`
	StatusLabel            string                  `json:"status_label"`
	Message                string                  `json:"message"`
	PreferredContactMethod string                  `json:"preferred_contact_method"`
	PreferredSlotUTC       *time.Time              `json:"preferred_slot_utc"`
	Stay                   *StayDetails            `json:"stay"`
	CreatedAt              time.Time               `json:"created_at"`
	Timeline               *[]customerTimelineView `json:"timeline,omitempty"`
	// The conversation (API 0.46.0). UnreadReply: the other side wrote last
	// and the customer has not opened the request since. CanReply: the
	// customer may answer now (the other side has written and the request
	// is not closed). Messages is on the single request only.
	MessageCount  int            `json:"message_count"`
	LastMessageAt *time.Time     `json:"last_message_at"`
	UnreadReply   bool           `json:"unread_reply"`
	CanReply      bool           `json:"can_reply"`
	Messages      *[]MessageView `json:"messages,omitempty"`
}

func customerView(r StoredRequest, names map[uuid.UUID]InboxListing) CustomerView {
	status := CustomerStatus(r.Stage)
	return CustomerView{
		RequestID: r.ID, Reference: r.Reference, ListingID: r.ListingID, Listing: listingOf(names, r.ListingID),
		RequestType: r.RequestType, Status: status, StatusLabel: CustomerStatusLabel(status),
		Message: r.Message, PreferredContactMethod: r.PreferredContactMethod, PreferredSlotUTC: r.PreferredSlot,
		Stay: r.Stay, CreatedAt: r.CreatedAt,
		MessageCount: r.MessageCount, LastMessageAt: r.LastMessageAt, UnreadReply: r.UnreadReply(), CanReply: r.CanCustomerReply(),
	}
}

// customerTimeline is the part of a request's history its customer may see:
// that it was received, and each time the status in THEIR terms changed.
// Stage moves that do not change that status, notes, routing and the people
// involved are all left out.
func customerTimeline(r StoredRequest, entries []TimelineEntry) []customerTimelineView {
	out := []customerTimelineView{{Event: "RECEIVED", Status: CustomerReceived, StatusLabel: CustomerStatusLabel(CustomerReceived), At: r.CreatedAt}}
	current := CustomerReceived
	for _, e := range entries {
		if e.EventType != TimelineStageChanged {
			continue
		}
		if next := CustomerStatus(e.ToStage); next != current {
			current = next
			out = append(out, customerTimelineView{Event: "STATUS_CHANGED", Status: next, StatusLabel: CustomerStatusLabel(next), At: e.At})
		}
	}
	return out
}

// CustomerViews builds the customer's view of stored requests.
func (h *Handler) CustomerViews(ctx context.Context, stored []StoredRequest) ([]CustomerView, error) {
	names, err := h.listingNames(ctx, listingIDs(stored))
	if err != nil {
		return nil, err
	}
	out := make([]CustomerView, 0, len(stored))
	for _, s := range stored {
		out = append(out, customerView(s, names))
	}
	return out, nil
}

// Mine serves GET /v1/me/property-requests: the requests this account sent.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		unauthenticated(w, r)
		return
	}
	fields := map[string]string{}
	page, size := paging(r, fields)
	statuses := codes(r, "status", CustomerStatuses(), fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	result, err := h.svc.Mine(r.Context(), p.UserID, statuses, size, (page-1)*size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.CustomerViews(r.Context(), result.Items)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "page": page, "page_size": size, "total": result.Total, "total_pages": totalPages(result.Total, size),
		"status_counts": StatusCountsOf(result.StatusCounts),
	})
}

// MineOne serves GET /v1/me/property-requests/{id}.
func (h *Handler) MineOne(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		unauthenticated(w, r)
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	if _, _, err := h.svc.MineOne(r.Context(), p.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	// Opening the request reads the replies in it.
	if err := h.svc.MarkRead(r.Context(), id, AuthorCustomer); err != nil {
		h.logger.WarnContext(r.Context(), "property request read marker not saved",
			slog.String("property_request_id", id.String()), slog.String("error", err.Error()))
	}
	h.writeCustomerDetail(w, r, p.UserID, id, http.StatusOK)
}

// writeCustomerDetail answers with one request as its customer sees it,
// with the status timeline and the conversation.
func (h *Handler) writeCustomerDetail(w http.ResponseWriter, r *http.Request, userID, id uuid.UUID, status int) {
	req, entries, err := h.svc.MineOne(r.Context(), userID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	views, err := h.CustomerViews(r.Context(), []StoredRequest{req})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	msgs, err := h.svc.Messages(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	timeline := customerTimeline(req, entries)
	messages := customerMessages(req, msgs)
	views[0].Timeline, views[0].Messages = &timeline, &messages
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, status, map[string]any{"request": views[0]})
}

// ---------------------------------------------------------------------------
// Routing exceptions (platform administrators)
// ---------------------------------------------------------------------------

// ExceptionAdminRoles are the platform roles that may see and resolve
// routing exceptions. MARKET_ADMIN only those of its own markets.
func ExceptionAdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformSupport, auth.PlatformMarketAdmin}
}

// AdminMarkets returns the market codes an administrator may see requests
// of: all is true for the global roles; otherwise codes are the markets of
// the caller's MARKET_ADMIN assignments. ok is false when the caller may see
// no requests at all.
func AdminMarkets(p *auth.Principal) (codes []string, all, ok bool) {
	if p == nil || p.SessionScope != auth.ScopeAdmin {
		return nil, false, false
	}
	for _, r := range p.PlatformRoles {
		switch r.Role {
		case auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformSupport:
			return nil, true, true
		case auth.PlatformMarketAdmin:
			if r.MarketCode != "" && !slices.Contains(codes, r.MarketCode) {
				codes = append(codes, r.MarketCode)
			}
		}
	}
	return codes, false, len(codes) > 0
}

// AdminMarketIDs resolves the markets an administrator may see requests of:
// nil means every market, an empty slice none.
func AdminMarketIDs(ctx context.Context, p *auth.Principal, resolve MarketIDs) (ids []uuid.UUID, ok bool, err error) {
	codes, all, ok := AdminMarkets(p)
	if !ok {
		return nil, false, nil
	}
	if all {
		return nil, true, nil
	}
	ids = []uuid.UUID{}
	if resolve != nil {
		if ids, err = resolve(ctx, codes); err != nil {
			return nil, false, err
		}
		if ids == nil {
			ids = []uuid.UUID{}
		}
	}
	return ids, true, nil
}

// admin returns the caller and the markets they may see: 401 when anonymous,
// 403 otherwise unless they hold one of ExceptionAdminRoles.
func (h *Handler) admin(w http.ResponseWriter, r *http.Request) (*auth.Principal, []uuid.UUID, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		unauthenticated(w, r)
		return nil, nil, false
	}
	ids, ok, err := AdminMarketIDs(r.Context(), p, h.markets)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return nil, nil, false
	}
	if !ok {
		forbidden(w, r)
		return nil, nil, false
	}
	return p, ids, true
}

// Exception states.
const (
	ExceptionOpen     = "OPEN"
	ExceptionResolved = "RESOLVED"
	exceptionAll      = "ALL"
)

// exceptionView is a routing exception as an administrator sees it.
type exceptionView struct {
	RequestID              uuid.UUID     `json:"request_id"`
	Reference              string        `json:"reference"`
	ListingID              uuid.UUID     `json:"listing_id"`
	Listing                *InboxListing `json:"listing"`
	RequestType            string        `json:"request_type"`
	Message                string        `json:"message"`
	PreferredContactMethod string        `json:"preferred_contact_method"`
	Contact                InboxContact  `json:"contact"`
	Stay                   *StayDetails  `json:"stay"`
	CreatedAt              time.Time     `json:"created_at"`
	// State is OPEN or RESOLVED.
	State          string     `json:"state"`
	ResolvedAt     *time.Time `json:"resolved_at"`
	ResolutionNote *string    `json:"resolution_note"`
}

func (h *Handler) exceptionViews(ctx context.Context, stored []StoredRequest) ([]exceptionView, error) {
	names, err := h.listingNames(ctx, listingIDs(stored))
	if err != nil {
		return nil, err
	}
	out := make([]exceptionView, 0, len(stored))
	for _, s := range stored {
		item := ItemOf(s)
		v := exceptionView{
			RequestID: s.ID, Reference: s.Reference, ListingID: s.ListingID, Listing: listingOf(names, s.ListingID),
			RequestType: s.RequestType, Message: s.Message, PreferredContactMethod: s.PreferredContactMethod,
			Contact: item.Contact, Stay: s.Stay, CreatedAt: s.CreatedAt,
			State: ExceptionOpen, ResolvedAt: s.ResolvedAt, ResolutionNote: optional(s.ResolutionNote),
		}
		if s.ResolvedAt != nil {
			v.State = ExceptionResolved
		}
		out = append(out, v)
	}
	return out, nil
}

// Exceptions serves GET /v1/admin/routing-exceptions, the longest-waiting first.
func (h *Handler) Exceptions(w http.ResponseWriter, r *http.Request) {
	_, markets, ok := h.admin(w, r)
	if !ok {
		return
	}
	fields := map[string]string{}
	page, size := paging(r, fields)
	f := ExceptionFilter{MarketIDs: markets, Limit: size, Offset: (page - 1) * size}
	open, resolved := false, true
	switch state := r.URL.Query().Get("state"); state {
	case "", ExceptionOpen:
		f.Resolved = &open
	case ExceptionResolved:
		f.Resolved = &resolved
	case exceptionAll:
	default:
		fields["state"] = "must be one of OPEN, RESOLVED, ALL"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	stored, total, err := h.svc.Exceptions(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.exceptionViews(r.Context(), stored)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"items": items, "page": page, "page_size": size, "total": total, "total_pages": totalPages(total, size),
	})
}

// ResolveException serves POST /v1/admin/routing-exceptions/{id}/resolve.
func (h *Handler) ResolveException(w http.ResponseWriter, r *http.Request) {
	p, markets, ok := h.admin(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	if r.ContentLength != 0 && !httpx.DecodeJSON(w, r, &in) {
		return
	}
	req, err := h.svc.ResolveException(r.Context(), markets, Actor{UserID: p.UserID, Name: p.Name}, id, in.Note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Audit line: who resolved which request. No customer data.
	h.logger.InfoContext(r.Context(), "routing exception resolved",
		slog.String("audit", "routing_exception_resolved"), slog.String("property_request_id", req.ID.String()),
		slog.String("actor_user_id", p.UserID.String()))
	views, err := h.exceptionViews(r.Context(), []StoredRequest{req})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"request": views[0]})
}
