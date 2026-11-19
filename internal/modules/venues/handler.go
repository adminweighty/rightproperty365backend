package venues

import (
	"errors"
	"fmt"
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
	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes.
const (
	CodeListingNotFound     = "LISTING_NOT_FOUND"
	CodeListingNotEligible  = "LISTING_NOT_ELIGIBLE"
	CodeVenueNotFound       = "VENUE_NOT_FOUND"
	CodeQuoteNotFound       = "QUOTE_NOT_FOUND"
	CodeQuoteExpired        = "QUOTE_EXPIRED"
	CodeQuoteUsed           = "QUOTE_ALREADY_USED"
	CodeBookingNotFound     = "VENUE_BOOKING_NOT_FOUND"
	CodeOrganisationUnknown = "ORGANISATION_NOT_FOUND"
	CodeSlotUnavailable     = "SLOT_UNAVAILABLE"
	CodeSlotTaken           = "SLOT_TAKEN"
	CodeInvalidTransition   = "INVALID_BOOKING_TRANSITION"
)

// Handler serves the hourly rental endpoints.
type Handler struct {
	svc    *Service
	logger *slog.Logger
	// calendar configures the calendar downloads (SetCalendar).
	calendar CalendarConfig
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

type detailBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
	TraceID string `json:"trace_id"`
}

func writeDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	var b detailBody
	b.Error.Code, b.Error.Message, b.Error.Details, b.TraceID = code, message, details, logging.RequestID(r.Context())
	httpx.WriteJSON(w, status, b)
}

// ReasonMessage explains an unavailable slot.
func ReasonMessage(reason string) string {
	switch reason {
	case ReasonClosed:
		return "The venue is not open for the whole of that time."
	case ReasonTaken:
		return "That time is already booked."
	case ReasonBuffer:
		return "That time is too close to another booking: the venue needs its cleaning time between bookings."
	case ReasonLeadTime:
		return "That time is too soon: the venue needs more notice."
	case ReasonOutsideHorizon:
		return "That time is too far ahead to book yet."
	}
	return "That time cannot be booked."
}

var answers = []struct {
	err           error
	status        int
	code, message string
}{
	{ErrUnauthenticated, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this."},
	{ErrForbidden, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this."},
	{ErrListingNotFound, http.StatusNotFound, CodeListingNotFound, "This listing does not exist."},
	{ErrListingNotEligible, http.StatusBadRequest, CodeListingNotEligible, "This property type cannot be booked by the hour. Hourly booking is for showrooms, venues, halls, conference rooms, studios and offices."},
	{ErrVenueNotFound, http.StatusNotFound, CodeVenueNotFound, "This listing cannot be booked by the hour."},
	{ErrQuoteNotFound, http.StatusNotFound, CodeQuoteNotFound, "This quote does not exist. Choose the time again."},
	{ErrQuoteUsed, http.StatusConflict, CodeQuoteUsed, "This quote has already been used. Choose the time again."},
	{ErrNotFound, http.StatusNotFound, CodeBookingNotFound, "This booking does not exist."},
	{ErrOrganisationUnknown, http.StatusNotFound, CodeOrganisationUnknown, "This organisation does not exist or you cannot see its bookings."},
	{ErrSlotTaken, http.StatusConflict, CodeSlotTaken, "Another booking was confirmed for this time first, so this one cannot be confirmed. Decline it or propose another time."},
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var unavailable *UnavailableError
	var expired *QuoteExpiredError
	var trans *TransitionError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
		return
	case errors.As(err, &unavailable):
		writeDetails(w, r, http.StatusConflict, CodeSlotUnavailable, ReasonMessage(unavailable.Reason), map[string]any{"reason": unavailable.Reason})
		return
	case errors.As(err, &expired):
		writeDetails(w, r, http.StatusConflict, CodeQuoteExpired, "This quote has expired. Choose the time again for a new quote.",
			map[string]any{"expired_at": expired.ExpiredAt.UTC()})
		return
	case errors.As(err, &trans):
		allowed := trans.Allowed
		if allowed == nil {
			allowed = []string{}
		}
		writeDetails(w, r, http.StatusConflict, CodeInvalidTransition,
			fmt.Sprintf("A booking that is %s cannot be given the action %q now.", strings.ToLower(StatusLabel(trans.From)), trans.Action),
			map[string]any{"from": trans.From, "action": trans.Action, "allowed": allowed})
		return
	case errors.Is(err, ErrSlotTaken):
		writeDetails(w, r, http.StatusConflict, CodeSlotTaken, answers[len(answers)-1].message, map[string]any{"reason": ReasonTaken})
		return
	}
	for _, a := range answers {
		if errors.Is(err, a.err) {
			httpx.WriteError(w, r, a.status, a.code, a.message, nil)
			return
		}
	}
	httpx.WriteInternalError(w, r, h.logger, err)
}

func pathID(r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	return id, err == nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// GetSettings serves GET /v1/me/listings/{id}/hourly.
func (h *Handler) GetSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrListingNotFound)
		return
	}
	v, err := h.svc.GetSettings(r.Context(), auth.FromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, settingsView(v))
}

// PutSettings serves PUT /v1/me/listings/{id}/hourly.
func (h *Handler) PutSettings(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "id")
	if !ok {
		h.fail(w, r, ErrListingNotFound)
		return
	}
	var in SettingsInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	v, err := h.svc.PutSettings(r.Context(), auth.FromContext(r.Context()), id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, settingsView(v))
}

// ---------------------------------------------------------------------------
// Public
// ---------------------------------------------------------------------------

// Availability serves GET /v1/venues/{listingId}/availability.
func (h *Handler) Availability(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "listingId")
	if !ok {
		h.fail(w, r, ErrVenueNotFound)
		return
	}
	v, err := h.svc.Availability(r.Context(), id, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	httpx.WriteJSON(w, http.StatusOK, availabilityView(v))
}

// Quote serves POST /v1/venues/{listingId}/quotes.
func (h *Handler) Quote(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "listingId")
	if !ok {
		h.fail(w, r, ErrVenueNotFound)
		return
	}
	var in QuoteInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	q, err := h.svc.CreateQuote(r.Context(), id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, quoteView(q))
}

// ---------------------------------------------------------------------------
// Lists
// ---------------------------------------------------------------------------

type pageJSON struct {
	Items      []bookingJSON `json:"items"`
	Page       int           `json:"page"`
	PageSize   int           `json:"page_size"`
	Total      int           `json:"total"`
	TotalPages int           `json:"total_pages"`
}

// filterOf reads ?status=&page=&page_size= (and, for hosts, listing_id,
// from and to).
func filterOf(r *http.Request, host bool) (Filter, error) {
	q := r.URL.Query()
	fields := map[string]string{}
	f := Filter{Page: 1, PageSize: DefaultPageSize}
	if raw := q.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			f.Page = n
		}
	}
	if raw := q.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(MaxPageSize)
		} else {
			f.PageSize = n
		}
	}
	if raw := q.Get("status"); raw != "" {
		for _, st := range strings.Split(raw, ",") {
			st = strings.TrimSpace(st)
			if !slices.Contains(Statuses(), st) {
				fields["status"] = "must be one or more of " + strings.Join(Statuses(), ", ") + ", separated by commas"
				break
			}
			f.Statuses = append(f.Statuses, st)
		}
	}
	if host {
		if raw := q.Get("listing_id"); raw != "" {
			if id, err := uuid.Parse(raw); err != nil {
				fields["listing_id"] = "must be a listing ID"
			} else {
				f.ListingID = &id
			}
		}
		for name, dst := range map[string]*string{"from": &f.FromDay, "to": &f.ToDay} {
			if raw := q.Get(name); raw != "" {
				if _, err := time.Parse(dateLayout, raw); err != nil {
					fields[name] = "must be a date YYYY-MM-DD"
				} else {
					*dst = raw
				}
			}
		}
	}
	if len(fields) > 0 {
		return Filter{}, &ValidationError{Fields: fields}
	}
	return f, nil
}

func (h *Handler) writePage(w http.ResponseWriter, list []Booking, total int, f Filter, party string) {
	now := h.svc.Now()
	out := pageJSON{Items: make([]bookingJSON, 0, len(list)), Page: f.Page, PageSize: f.PageSize, Total: total}
	if f.PageSize > 0 {
		out.TotalPages = (total + f.PageSize - 1) / f.PageSize
	}
	for _, b := range list {
		out.Items = append(out.Items, bookingView(b, party, now, nil))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (h *Handler) writeOne(w http.ResponseWriter, r *http.Request, status int, b Booking, party string) {
	events, err := h.svc.Events(r.Context(), b.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, status, bookingView(b, party, h.svc.Now(), events))
}

// ---------------------------------------------------------------------------
// Customer: /v1/me/venue-bookings
// ---------------------------------------------------------------------------

// Request serves POST /v1/me/venue-bookings.
func (h *Handler) Request(w http.ResponseWriter, r *http.Request) {
	var in RequestInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	b, replayed, err := h.svc.Request(r.Context(), auth.FromContext(r.Context()), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.logger.InfoContext(r.Context(), "venue booking requested", slog.String("booking_id", b.ID.String()), slog.String("reference", b.Reference),
		slog.Bool("replayed", replayed))
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	h.writeOne(w, r, status, b, PartyCustomer)
}

// ListMine serves GET /v1/me/venue-bookings.
func (h *Handler) ListMine(w http.ResponseWriter, r *http.Request) {
	f, err := filterOf(r, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	list, total, err := h.svc.MineList(r.Context(), auth.FromContext(r.Context()), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writePage(w, list, total, f, PartyCustomer)
}

func (h *Handler) mine(w http.ResponseWriter, r *http.Request, act func(p *auth.Principal, id uuid.UUID) (Booking, error)) {
	id, ok := pathID(r, "bookingId")
	if !ok {
		h.fail(w, r, ErrNotFound)
		return
	}
	b, err := act(auth.FromContext(r.Context()), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeOne(w, r, http.StatusOK, b, PartyCustomer)
}

// optionalBody decodes a JSON body when there is one.
func optionalBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.ContentLength == 0 && r.Header.Get("Content-Type") == "" {
		return true
	}
	return httpx.DecodeJSON(w, r, dst)
}

type reasonBody struct {
	Reason string `json:"reason"`
}

// GetMine serves GET /v1/me/venue-bookings/{bookingId}.
func (h *Handler) GetMine(w http.ResponseWriter, r *http.Request) {
	h.mine(w, r, func(p *auth.Principal, id uuid.UUID) (Booking, error) { return h.svc.Mine(r.Context(), p, id) })
}

// CancelMine serves POST /v1/me/venue-bookings/{bookingId}/cancel.
func (h *Handler) CancelMine(w http.ResponseWriter, r *http.Request) {
	var in reasonBody
	if !optionalBody(w, r, &in) {
		return
	}
	h.mine(w, r, func(p *auth.Principal, id uuid.UUID) (Booking, error) {
		return h.svc.CustomerCancel(r.Context(), p, id, in.Reason)
	})
}

// RescheduleMine serves POST /v1/me/venue-bookings/{bookingId}/reschedule.
func (h *Handler) RescheduleMine(w http.ResponseWriter, r *http.Request) {
	var in QuoteRef
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	h.mine(w, r, func(p *auth.Principal, id uuid.UUID) (Booking, error) {
		return h.svc.CustomerReschedule(r.Context(), p, id, in)
	})
}

// AcceptMine serves POST /v1/me/venue-bookings/{bookingId}/accept.
func (h *Handler) AcceptMine(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !optionalBody(w, r, &in) {
		return
	}
	h.mine(w, r, func(p *auth.Principal, id uuid.UUID) (Booking, error) {
		return h.svc.CustomerAccept(r.Context(), p, id)
	})
}

// ---------------------------------------------------------------------------
// Hosts: /v1/agent/organisations/{id}/venue-bookings and /v1/me/hosted-venue-bookings
// ---------------------------------------------------------------------------

// host reads the host of the route: the organisation in {id}, or (orgParam
// "") the caller as the direct owner.
func host(r *http.Request, orgParam string) (Host, bool) {
	if orgParam == "" {
		return Host{}, true
	}
	id, ok := pathID(r, orgParam)
	if !ok {
		return Host{}, false
	}
	return Host{OrganisationID: &id}, true
}

// HostRoutes are one host's handlers (organisation or direct owner).
type HostRoutes struct {
	h        *Handler
	orgParam string
}

// Organisation returns the handlers of /v1/agent/organisations/{param}/venue-bookings.
func (h *Handler) Organisation(param string) HostRoutes { return HostRoutes{h: h, orgParam: param} }

// DirectOwner returns the handlers of /v1/me/hosted-venue-bookings.
func (h *Handler) DirectOwner() HostRoutes { return HostRoutes{h: h} }

// List serves GET .../venue-bookings.
func (x HostRoutes) List(w http.ResponseWriter, r *http.Request) {
	hst, ok := host(r, x.orgParam)
	if !ok {
		x.h.fail(w, r, ErrOrganisationUnknown)
		return
	}
	f, err := filterOf(r, true)
	if err != nil {
		x.h.fail(w, r, err)
		return
	}
	list, total, err := x.h.svc.HostList(r.Context(), auth.FromContext(r.Context()), hst, f)
	if err != nil {
		x.h.fail(w, r, err)
		return
	}
	x.h.writePage(w, list, total, f, PartyHost)
}

func (x HostRoutes) one(w http.ResponseWriter, r *http.Request, act func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error)) {
	hst, ok := host(r, x.orgParam)
	id, okID := pathID(r, "bookingId")
	if !ok || !okID {
		x.h.fail(w, r, ErrNotFound)
		return
	}
	b, err := act(auth.FromContext(r.Context()), hst, id)
	if err != nil {
		x.h.fail(w, r, err)
		return
	}
	x.h.writeOne(w, r, http.StatusOK, b, PartyHost)
}

// Get serves GET .../venue-bookings/{bookingId}.
func (x HostRoutes) Get(w http.ResponseWriter, r *http.Request) {
	x.one(w, r, func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error) {
		return x.h.svc.HostGet(r.Context(), p, hst, id)
	})
}

// Confirm serves POST .../{bookingId}/confirm.
func (x HostRoutes) Confirm(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !optionalBody(w, r, &in) {
		return
	}
	x.one(w, r, func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error) {
		return x.h.svc.Confirm(r.Context(), p, hst, id)
	})
}

// Decline serves POST .../{bookingId}/decline.
func (x HostRoutes) Decline(w http.ResponseWriter, r *http.Request) {
	var in reasonBody
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	x.one(w, r, func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error) {
		return x.h.svc.Decline(r.Context(), p, hst, id, in.Reason)
	})
}

// Cancel serves POST .../{bookingId}/cancel.
func (x HostRoutes) Cancel(w http.ResponseWriter, r *http.Request) {
	var in reasonBody
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	x.one(w, r, func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error) {
		return x.h.svc.HostCancel(r.Context(), p, hst, id, in.Reason)
	})
}

// Propose serves POST .../{bookingId}/propose.
func (x HostRoutes) Propose(w http.ResponseWriter, r *http.Request) {
	var in QuoteRef
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	x.one(w, r, func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error) {
		return x.h.svc.Propose(r.Context(), p, hst, id, in)
	})
}

// Complete serves POST .../{bookingId}/complete.
func (x HostRoutes) Complete(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !optionalBody(w, r, &in) {
		return
	}
	x.one(w, r, func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error) {
		return x.h.svc.Complete(r.Context(), p, hst, id)
	})
}

// NoShow serves POST .../{bookingId}/no-show.
func (x HostRoutes) NoShow(w http.ResponseWriter, r *http.Request) {
	var in struct{}
	if !optionalBody(w, r, &in) {
		return
	}
	x.one(w, r, func(p *auth.Principal, hst Host, id uuid.UUID) (Booking, error) {
		return x.h.svc.NoShow(r.Context(), p, hst, id)
	})
}
