package appointments

import (
	"net/http"
	"strconv"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/ics"
)

// CalendarConfig is what the calendar downloads need: the web app's origin
// (the entry's URL) and the platform's sender as the ORGANIZER.
type CalendarConfig struct {
	PublicWebURL string
	Organizer    *ics.Address
}

// SetCalendar configures the calendar downloads (API 0.39.0).
func (h *Handler) SetCalendar(c CalendarConfig) { h.calendar = c }

// CalendarMine serves GET /v1/me/appointments/{id}/calendar.ics: the
// customer's own appointment as a calendar file; 404 for anybody else's.
func (h *Handler) CalendarMine(w http.ResponseWriter, r *http.Request) {
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
	h.writeCalendar(w, r, a, true)
}

// CalendarReceived serves GET /v1/inbox/appointments/{id}/calendar.ics: an
// appointment the professional can see; 404 for anybody else (also a caller
// with no professional scope at all).
func (h *Handler) CalendarReceived(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
		return
	}
	id, ok := pathID(r)
	scope := ScopeOf(p)
	if !ok || scope.Empty() {
		h.fail(w, r, ErrNotFound)
		return
	}
	a, err := h.svc.Received(r.Context(), scope, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeCalendar(w, r, a, false)
}

func (h *Handler) writeCalendar(w http.ResponseWriter, r *http.Request, a Appointment, forCustomer bool) {
	entries, err := h.svc.Timeline(r.Context(), a.ID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	body := CalendarFile(a, CalendarInput{
		Method: ics.MethodPublish, ForCustomer: forCustomer, Stamp: CalendarStamp(entries, a.UpdatedAt),
		Sequence: CalendarSequence(entries, nil), PublicWebURL: h.calendar.PublicWebURL, Organizer: h.calendar.Organizer,
	})
	WriteCalendar(w, CalendarFilename(a), body)
}

// WriteCalendar writes a calendar file download.
func WriteCalendar(w http.ResponseWriter, filename string, body []byte) {
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
