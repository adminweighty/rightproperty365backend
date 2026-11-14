package appointments

import (
	"context"
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

// Error codes specific to this module.
const (
	CodeAppointmentNotFound  = "APPOINTMENT_NOT_FOUND"
	CodeInvalidState         = "INVALID_STATE"
	CodeSlotTaken            = "SLOT_TAKEN"
	CodeIdempotencyConflict  = "IDEMPOTENCY_CONFLICT"
	CodeListingNotFound      = "LISTING_NOT_FOUND"
	CodeListingUnavailable   = "LISTING_UNAVAILABLE"
	CodeRecipientNotFound    = "RECIPIENT_NOT_FOUND"
	CodeRecipientUnavailable = "RECIPIENT_UNAVAILABLE"
)

// HeaderIdempotencyKey may carry the idempotency key instead of the body.
const HeaderIdempotencyKey = "Idempotency-Key"

// InboxCounts supplies the other two numbers of GET /v1/inbox/summary.
type InboxCounts interface {
	NewPropertyRequests(ctx context.Context, p *auth.Principal) (int, error)
	NewBusinessEnquiries(ctx context.Context, p *auth.Principal) (int, error)
}

// Handler serves the appointment endpoints.
type Handler struct {
	svc    *Service
	inbox  InboxCounts
	logger *slog.Logger
}

// NewHandler builds the handler. inbox may be nil (the summary then reports
// zero requests and enquiries).
func NewHandler(svc *Service, inbox InboxCounts, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, inbox: inbox, logger: logger}
}

// ScopeOf builds the professional scope of a caller. An organisation counts
// only when auth.MayManageAppointments allows it; OWNER and MANAGER
// (auth.MayAssignWork) see all of it, the others their own share. The
// direct-owner branch needs an APPROVED landlord or property seller role.
func ScopeOf(p *auth.Principal) Scope {
	s := Scope{UserID: p.UserID, DirectOwner: p.IsDirectOwner()}
	for _, m := range p.Memberships {
		org := m.OrganisationID
		if !p.MayManageAppointments(org) || slices.Contains(s.ManagedOrganisationIDs, org) || slices.Contains(s.MemberOrganisationIDs, org) {
			continue
		}
		if p.MayAssignWork(org) {
			s.ManagedOrganisationIDs = append(s.ManagedOrganisationIDs, org)
		} else {
			s.MemberOrganisationIDs = append(s.MemberOrganisationIDs, org)
		}
	}
	return s
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

type listingView struct {
	Slug      string `json:"slug"`
	Title     string `json:"title"`
	Reference string `json:"reference"`
}

type withView struct {
	Kind string `json:"kind"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

type timelineView struct {
	Event    string     `json:"event"`
	From     *string    `json:"from"`
	To       string     `json:"to"`
	ToLabel  string     `json:"to_label"`
	Actor    string     `json:"actor"`
	Note     *string    `json:"note"`
	StartUTC *time.Time `json:"start_utc"`
	At       time.Time  `json:"at"`
}

// baseView is what both parties see.
type baseView struct {
	ID                       uuid.UUID       `json:"id"`
	Reference                string          `json:"reference"`
	Type                     string          `json:"type"`
	Status                   string          `json:"status"`
	StatusLabel              string          `json:"status_label"`
	StartUTC                 time.Time       `json:"start_utc"`
	EndUTC                   time.Time       `json:"end_utc"`
	Timezone                 string          `json:"timezone"`
	DurationMinutes          int             `json:"duration_minutes"`
	ProposedStartUTC         *time.Time      `json:"proposed_start_utc"`
	ProposedBy               *string         `json:"proposed_by"`
	Listing                  *listingView    `json:"listing"`
	With                     *withView       `json:"with"`
	LocationNote             *string         `json:"location_note"`
	Note                     *string         `json:"note"`
	PropertyRequestReference *string         `json:"property_request_reference"`
	BusinessEnquiryReference *string         `json:"business_enquiry_reference"`
	CancelledBy              *string         `json:"cancelled_by"`
	CancelReason             *string         `json:"cancel_reason"`
	Timeline                 *[]timelineView `json:"timeline,omitempty"`
	CreatedAt                time.Time       `json:"created_at"`
	UpdatedAt                time.Time       `json:"updated_at"`
}

// customerView is an appointment as its customer sees it. It never carries
// the professional's phone number or email address.
type customerView struct {
	baseView
	CanCancel     bool `json:"can_cancel"`
	CanReschedule bool `json:"can_reschedule"`
	CanAccept     bool `json:"can_accept"`
}

type contactView struct {
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Phone *string `json:"phone"`
}

type assigneeView struct {
	UserID uuid.UUID `json:"user_id"`
	Name   string    `json:"name"`
}

// professionalView is an appointment as the receiving professional sees it:
// the only place the customer's contact details are returned.
type professionalView struct {
	baseView
	ListingID              *uuid.UUID    `json:"listing_id"`
	OrganisationID         *uuid.UUID    `json:"organisation_id"`
	AssignedTo             *assigneeView `json:"assigned_to"`
	Contact                contactView   `json:"contact"`
	PreferredContactMethod string        `json:"preferred_contact_method"`
	CanConfirm             bool          `json:"can_confirm"`
	CanReschedule          bool          `json:"can_reschedule"`
	CanCancel              bool          `json:"can_cancel"`
	CanComplete            bool          `json:"can_complete"`
	CanNoShow              bool          `json:"can_no_show"`
	CanAssign              bool          `json:"can_assign"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func base(a Appointment) baseView {
	v := baseView{
		ID: a.ID, Reference: a.Reference, Type: a.Type, Status: a.Status, StatusLabel: StatusLabel(a.Status),
		StartUTC: a.StartUTC.UTC(), EndUTC: a.EndUTC().UTC(), Timezone: a.Timezone, DurationMinutes: a.DurationMinutes,
		ProposedStartUTC: a.ProposedStartUTC, ProposedBy: optional(a.ProposedBy),
		LocationNote: optional(a.LocationNote), Note: optional(a.CustomerNote),
		CancelledBy: optional(a.CancelledBy), CancelReason: optional(a.CancelReason),
		CreatedAt: a.CreatedAt.UTC(), UpdatedAt: a.UpdatedAt.UTC(),
	}
	if a.ListingSlug != "" {
		v.Listing = &listingView{Slug: a.ListingSlug, Title: a.ListingTitle, Reference: a.ListingReference}
	}
	if a.WithSlug != "" {
		v.With = &withView{Kind: a.WithKind, Slug: a.WithSlug, Name: a.WithName}
	}
	switch {
	case a.PropertyRequestID != nil:
		v.PropertyRequestReference = optional(a.LinkedReference)
	case a.BusinessEnquiryID != nil:
		v.BusinessEnquiryReference = optional(a.LinkedReference)
	}
	return v
}

// timelineOf renders entries; a customer does not see internal assignments.
func timelineOf(entries []TimelineEntry, forCustomer bool) *[]timelineView {
	out := []timelineView{}
	for _, e := range entries {
		if forCustomer && e.Event == TimelineAssigned {
			continue
		}
		out = append(out, timelineView{
			Event: e.Event, From: optional(e.FromStatus), To: e.ToStatus, ToLabel: StatusLabel(e.ToStatus),
			Actor: e.ActorType, Note: optional(e.Note), StartUTC: e.StartUTC, At: e.At.UTC(),
		})
	}
	return &out
}

func proposedAhead(a Appointment, by string, now time.Time) bool {
	return a.Status == StatusRescheduleRequested && a.ProposedBy == by && a.ProposedStartUTC != nil && a.ProposedStartUTC.After(now)
}

func forCustomer(a Appointment, now time.Time) customerView {
	return customerView{
		baseView:  base(a),
		CanCancel: a.Open(),
		CanReschedule: a.Status == StatusRequested || a.Status == StatusConfirmed ||
			(a.Status == StatusRescheduleRequested && a.ProposedBy == PartyProfessional),
		CanAccept: proposedAhead(a, PartyProfessional, now),
	}
}

func (h *Handler) forProfessional(ctx context.Context, a Appointment, scope Scope, now time.Time, names map[uuid.UUID]string) (professionalView, error) {
	v := professionalView{
		baseView: base(a), ListingID: a.ListingID, OrganisationID: a.OrganisationID,
		Contact:                contactView{Name: a.CustomerName, Email: a.CustomerEmail, Phone: optional(a.CustomerPhone)},
		PreferredContactMethod: a.PreferredContactMethod,
		CanConfirm:             (a.Status == StatusRequested && a.StartUTC.After(now)) || proposedAhead(a, PartyCustomer, now),
		CanReschedule: a.Status == StatusRequested || a.Status == StatusConfirmed ||
			(a.Status == StatusRescheduleRequested && a.ProposedBy == PartyCustomer),
		CanCancel:   a.Open(),
		CanComplete: a.Status == StatusConfirmed && !now.Before(a.StartUTC),
		CanAssign:   a.Open() && scope.Manages(a),
	}
	v.CanNoShow = v.CanComplete
	if a.AssignedUserID != nil {
		name, ok := names[*a.AssignedUserID]
		if !ok {
			var err error
			if name, err = h.svc.AssigneeName(ctx, *a.AssignedUserID); err != nil {
				return professionalView{}, err
			}
			names[*a.AssignedUserID] = name
		}
		v.AssignedTo = &assigneeView{UserID: *a.AssignedUserID, Name: name}
	}
	return v, nil
}

type pageResponse struct {
	Items      any `json:"items"`
	Page       int `json:"page"`
	PageSize   int `json:"page_size"`
	Total      int `json:"total"`
	TotalPages int `json:"total_pages"`
}

// ---------------------------------------------------------------------------
// Plumbing
// ---------------------------------------------------------------------------

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeAppointmentNotFound, "This appointment does not exist.", nil)
	case errors.Is(err, errStartPassed):
		httpx.WriteError(w, r, http.StatusConflict, CodeInvalidState, "That time has already passed. Propose a new time.", nil)
	case errors.Is(err, ErrInvalidState):
		httpx.WriteError(w, r, http.StatusConflict, CodeInvalidState, "This appointment cannot be changed in that way in its current status.", nil)
	case errors.Is(err, ErrSlotTaken):
		httpx.WriteError(w, r, http.StatusConflict, CodeSlotTaken, "There is already a confirmed appointment at that time.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
	case errors.Is(err, ErrNotAMember):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
			map[string]string{"user_id": "must be an active member of the appointment's organisation"})
	case errors.Is(err, ErrListingNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
	case errors.Is(err, ErrListingUnavailable):
		httpx.WriteError(w, r, http.StatusGone, CodeListingUnavailable, "This listing is no longer accepting appointments.", nil)
	case errors.Is(err, ErrRecipientNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeRecipientNotFound, "This profile does not exist.", nil)
	case errors.Is(err, ErrRecipientUnavailable):
		httpx.WriteError(w, r, http.StatusGone, CodeRecipientUnavailable, "This profile is not accepting appointments.", nil)
	case errors.Is(err, ErrIdempotencyConflict):
		httpx.WriteError(w, r, http.StatusConflict, CodeIdempotencyConflict, "This idempotency key was already used for a different appointment.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// decodeOptional decodes a JSON body when there is one; an absent body is fine.
func decodeOptional(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil || r.Body == http.NoBody || r.ContentLength == 0 {
		return true
	}
	return httpx.DecodeJSON(w, r, dst)
}

func pathID(r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	return id, err == nil
}

// customer returns the signed-in caller (the routes sit behind auth.RequireAuth).
func (h *Handler) customer(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
		return nil, false
	}
	return p, true
}

// professional returns the caller's scope: 401 when anonymous, 403 when the
// caller manages appointments for no organisation and is not an approved
// direct owner.
func (h *Handler) professional(w http.ResponseWriter, r *http.Request) (Scope, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
		return Scope{}, false
	}
	scope := ScopeOf(p)
	if scope.Empty() {
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
		return Scope{}, false
	}
	return scope, true
}

// listQuery reads the query parameters shared by both lists.
func listQuery(r *http.Request, f *ListFilter) map[string]string {
	q, fields := r.URL.Query(), map[string]string{}
	f.Page, f.PageSize = 1, DefaultPageSize
	if raw := q.Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		}
		f.Page = n
	}
	if raw := q.Get("page_size"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		}
		f.PageSize = n
	}
	for _, raw := range q["status"] {
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s == "" {
				continue
			}
			if !slices.Contains(Statuses(), s) {
				fields["status"] = "must be one of " + strings.Join(Statuses(), ", ")
			}
			f.Statuses = append(f.Statuses, s)
		}
	}
	return fields
}

func (h *Handler) writePage(w http.ResponseWriter, items any, f ListFilter, total int) {
	pages := 0
	if total > 0 {
		pages = (total + f.PageSize - 1) / f.PageSize
	}
	httpx.WriteJSON(w, http.StatusOK, pageResponse{Items: items, Page: f.Page, PageSize: f.PageSize, Total: total, TotalPages: pages})
}

// ---------------------------------------------------------------------------
// Customer endpoints
// ---------------------------------------------------------------------------

// Create serves POST /v1/appointments: 201 after the appointment and its
// outbox event are committed, 200 for an idempotent replay.
func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.customer(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	// The customer's identity comes from the session only, never from the body.
	in.Customer = Customer{UserID: p.UserID, Name: p.Name, Email: p.Email}
	a, replayed, err := h.svc.Create(r.Context(), in, r.Header.Get(HeaderIdempotencyKey))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	// Identifiers only; notes and contact details are personal data.
	h.logger.InfoContext(r.Context(), "appointment requested",
		slog.String("appointment_id", a.ID.String()), slog.String("reference", a.Reference), slog.Bool("replayed", replayed))
	h.writeMine(w, r, status, a)
}

func (h *Handler) writeMine(w http.ResponseWriter, r *http.Request, status int, a Appointment) {
	entries, err := h.svc.Timeline(r.Context(), a.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v := forCustomer(a, h.svc.now())
	v.Timeline = timelineOf(entries, true)
	httpx.WriteJSON(w, status, map[string]any{"appointment": v})
}

// ListMine serves GET /v1/me/appointments.
func (h *Handler) ListMine(w http.ResponseWriter, r *http.Request) {
	p, ok := h.customer(w, r)
	if !ok {
		return
	}
	f := ListFilter{CustomerUserID: &p.UserID, When: WhenUpcoming}
	fields := listQuery(r, &f)
	if raw := r.URL.Query().Get("when"); raw != "" {
		if !slices.Contains([]string{WhenUpcoming, WhenPast, WhenAll}, raw) {
			fields["when"] = "must be one of upcoming, past, all"
		}
		f.When = raw
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	// Upcoming: soonest first. Past and all: newest first.
	f.Ascending = f.When == WhenUpcoming
	list, total, err := h.svc.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.svc.now()
	items := make([]customerView, 0, len(list))
	for _, a := range list {
		items = append(items, forCustomer(a, now))
	}
	h.writePage(w, items, f, total)
}

// GetMine serves GET /v1/me/appointments/{id}.
func (h *Handler) GetMine(w http.ResponseWriter, r *http.Request) {
	p, ok := h.customer(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	a, err := h.svc.Mine(r.Context(), p.UserID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeMine(w, r, http.StatusOK, a)
}

// mineAction runs one customer transition and writes the appointment.
func (h *Handler) mineAction(w http.ResponseWriter, r *http.Request, body any, act func(userID, id uuid.UUID) (Appointment, error)) {
	p, ok := h.customer(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	if body != nil && !decodeOptional(w, r, body) {
		return
	}
	a, err := act(p.UserID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "appointment changed by the customer",
		slog.String("appointment_id", a.ID.String()), slog.String("status", a.Status))
	h.writeMine(w, r, http.StatusOK, a)
}

// CancelMine serves POST /v1/me/appointments/{id}/cancel.
func (h *Handler) CancelMine(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	h.mineAction(w, r, &in, func(userID, id uuid.UUID) (Appointment, error) {
		return h.svc.CustomerCancel(r.Context(), userID, id, in.Reason)
	})
}

// RescheduleMine serves POST /v1/me/appointments/{id}/reschedule.
func (h *Handler) RescheduleMine(w http.ResponseWriter, r *http.Request) {
	var in RescheduleInput
	h.mineAction(w, r, &in, func(userID, id uuid.UUID) (Appointment, error) {
		return h.svc.CustomerReschedule(r.Context(), userID, id, in)
	})
}

// AcceptMine serves POST /v1/me/appointments/{id}/accept.
func (h *Handler) AcceptMine(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	h.mineAction(w, r, &in, func(userID, id uuid.UUID) (Appointment, error) {
		return h.svc.CustomerAccept(r.Context(), userID, id)
	})
}

// ---------------------------------------------------------------------------
// Professional endpoints
// ---------------------------------------------------------------------------

func (h *Handler) writeReceived(w http.ResponseWriter, r *http.Request, scope Scope, a Appointment) {
	entries, err := h.svc.Timeline(r.Context(), a.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.forProfessional(r.Context(), a, scope, h.svc.now(), map[uuid.UUID]string{})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v.Timeline = timelineOf(entries, false)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"appointment": v})
}

// Inbox serves GET /v1/inbox/appointments, soonest first.
func (h *Handler) Inbox(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	f := ListFilter{Scope: &scope, When: WhenAll, Ascending: true}
	fields := listQuery(r, &f)
	q := r.URL.Query()
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
	if raw := q.Get("type"); raw != "" {
		if !slices.Contains(Types(), raw) {
			fields["type"] = "must be one of " + strings.Join(Types(), ", ")
		}
		f.Type = raw
	}
	if raw := q.Get("listing_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fields["listing_id"] = "must be a UUID"
		}
		f.ListingID = &id
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	list, total, err := h.svc.List(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now, names := h.svc.now(), map[uuid.UUID]string{}
	items := make([]professionalView, 0, len(list))
	for _, a := range list {
		v, err := h.forProfessional(r.Context(), a, scope, now, names)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		items = append(items, v)
	}
	h.writePage(w, items, f, total)
}

// Received serves GET /v1/inbox/appointments/{id}.
func (h *Handler) Received(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	a, err := h.svc.Received(r.Context(), scope, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeReceived(w, r, scope, a)
}

// inboxAction runs one professional transition and writes the appointment.
func (h *Handler) inboxAction(w http.ResponseWriter, r *http.Request, body any, act func(scope Scope, id uuid.UUID) (Appointment, error)) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	id, ok := pathID(r)
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	if body != nil && !decodeOptional(w, r, body) {
		return
	}
	a, err := act(scope, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "appointment changed by the professional",
		slog.String("appointment_id", a.ID.String()), slog.String("status", a.Status))
	h.writeReceived(w, r, scope, a)
}

// Confirm serves POST /v1/inbox/appointments/{id}/confirm.
func (h *Handler) Confirm(w http.ResponseWriter, r *http.Request) {
	var in ConfirmInput
	h.inboxAction(w, r, &in, func(scope Scope, id uuid.UUID) (Appointment, error) {
		return h.svc.Confirm(r.Context(), scope, id, in)
	})
}

// Reschedule serves POST /v1/inbox/appointments/{id}/reschedule.
func (h *Handler) Reschedule(w http.ResponseWriter, r *http.Request) {
	var in RescheduleInput
	h.inboxAction(w, r, &in, func(scope Scope, id uuid.UUID) (Appointment, error) {
		return h.svc.Reschedule(r.Context(), scope, id, in)
	})
}

// Cancel serves POST /v1/inbox/appointments/{id}/cancel.
func (h *Handler) Cancel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Reason string `json:"reason"`
	}
	h.inboxAction(w, r, &in, func(scope Scope, id uuid.UUID) (Appointment, error) {
		return h.svc.Cancel(r.Context(), scope, id, in.Reason)
	})
}

// Complete serves POST /v1/inbox/appointments/{id}/complete.
func (h *Handler) Complete(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	h.inboxAction(w, r, &in, func(scope Scope, id uuid.UUID) (Appointment, error) {
		return h.svc.Complete(r.Context(), scope, id)
	})
}

// NoShow serves POST /v1/inbox/appointments/{id}/no-show.
func (h *Handler) NoShow(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	h.inboxAction(w, r, &in, func(scope Scope, id uuid.UUID) (Appointment, error) {
		return h.svc.NoShow(r.Context(), scope, id)
	})
}

// Assign serves POST /v1/inbox/appointments/{id}/assign (OWNER and MANAGER only).
func (h *Handler) Assign(w http.ResponseWriter, r *http.Request) {
	var in struct {
		UserID string `json:"user_id"`
	}
	h.inboxAction(w, r, &in, func(scope Scope, id uuid.UUID) (Appointment, error) {
		return h.svc.Assign(r.Context(), scope, id, in.UserID)
	})
}

// NewCount is the number of new items in an inbox.
type NewCount struct {
	New int `json:"new"`
}

// Summary is the body of GET /v1/inbox/summary.
type Summary struct {
	PropertyRequests  NewCount `json:"property_requests"`
	BusinessEnquiries NewCount `json:"business_enquiries"`
	Appointments      Counts   `json:"appointments"`
}

// SummaryFor returns the numbers behind the app's badges for a caller, each
// scoped exactly as its own inbox. GET /v1/me/dashboard uses the same numbers.
func (h *Handler) SummaryFor(ctx context.Context, p *auth.Principal) (Summary, error) {
	var out Summary
	var err error
	if out.Appointments, err = h.svc.Count(ctx, ScopeOf(p)); err != nil {
		return Summary{}, err
	}
	if h.inbox != nil {
		if out.PropertyRequests.New, err = h.inbox.NewPropertyRequests(ctx, p); err != nil {
			return Summary{}, err
		}
		if out.BusinessEnquiries.New, err = h.inbox.NewBusinessEnquiries(ctx, p); err != nil {
			return Summary{}, err
		}
	}
	return out, nil
}

// Summary serves GET /v1/inbox/summary. Any signed-in caller may ask; someone
// who receives nothing gets zeros.
func (h *Handler) Summary(w http.ResponseWriter, r *http.Request) {
	p, ok := h.customer(w, r)
	if !ok {
		return
	}
	out, err := h.SummaryFor(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// Brief is an appointment in a dashboard's short list.
type Brief struct {
	ID          uuid.UUID    `json:"id"`
	Reference   string       `json:"reference"`
	Type        string       `json:"type"`
	Status      string       `json:"status"`
	StatusLabel string       `json:"status_label"`
	StartUTC    time.Time    `json:"start_utc"`
	Timezone    string       `json:"timezone"`
	Listing     *listingView `json:"listing"`
	// Side says whose list it is from: CUSTOMER (the caller asked for it) or
	// PROFESSIONAL (the caller receives it).
	Side string `json:"side"`
}

func briefs(list []Appointment, side string) []Brief {
	out := make([]Brief, 0, len(list))
	for _, a := range list {
		v := base(a)
		out = append(out, Brief{
			ID: v.ID, Reference: v.Reference, Type: v.Type, Status: v.Status, StatusLabel: v.StatusLabel,
			StartUTC: v.StartUTC, Timezone: v.Timezone, Listing: v.Listing, Side: side,
		})
	}
	return out
}

// NextReceived returns the caller's next upcoming appointments as a
// professional, soonest first, scoped as GET /v1/inbox/appointments.
func (h *Handler) NextReceived(ctx context.Context, p *auth.Principal, limit int) ([]Brief, error) {
	scope := ScopeOf(p)
	if scope.Empty() {
		return []Brief{}, nil
	}
	list, _, err := h.svc.List(ctx, ListFilter{Scope: &scope, When: WhenUpcoming, Ascending: true, Page: 1, PageSize: limit})
	if err != nil {
		return nil, err
	}
	return briefs(list, PartyProfessional), nil
}

// MineSummary is what a customer's dashboard shows of their appointments.
type MineSummary struct {
	// Upcoming is the number of appointments still ahead.
	Upcoming int
	// NeedsReply is how many of them wait for the customer to accept or
	// decline a new time the professional proposed.
	NeedsReply int
	Next       []Brief
}

// MineSummaryFor returns the caller's own upcoming appointments, soonest
// first, scoped as GET /v1/me/appointments.
func (h *Handler) MineSummaryFor(ctx context.Context, p *auth.Principal, limit int) (MineSummary, error) {
	list, total, err := h.svc.List(ctx, ListFilter{CustomerUserID: &p.UserID, When: WhenUpcoming, Ascending: true, Page: 1, PageSize: MaxPageSize})
	if err != nil {
		return MineSummary{}, err
	}
	out := MineSummary{Upcoming: total}
	now := h.svc.now()
	for _, a := range list {
		if proposedAhead(a, PartyProfessional, now) {
			out.NeedsReply++
		}
	}
	out.Next = briefs(list[:min(limit, len(list))], PartyCustomer)
	return out, nil
}
