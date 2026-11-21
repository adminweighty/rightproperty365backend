package appointments

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/venues"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Availability error codes (API 0.51.0).
const (
	CodeServiceNotFound = "SERVICE_NOT_FOUND"
	CodeTooManyServices = "TOO_MANY_SERVICES"
)

type settingsView struct {
	Enabled       bool                  `json:"enabled"`
	Timezone      string                `json:"timezone"`
	WorkingHours  []venues.OpeningRange `json:"working_hours"`
	Exceptions    []venues.Exception    `json:"exceptions"`
	SlotMinutes   int                   `json:"slot_minutes"`
	BufferMinutes int                   `json:"buffer_minutes"`
	LeadTimeHours int                   `json:"lead_time_hours"`
	HorizonDays   int                   `json:"horizon_days"`
	MaxPerDay     int                   `json:"max_per_day"`
	UpdatedAt     *time.Time            `json:"updated_at"`
}

func viewSettings(s Settings) settingsView {
	v := settingsView{Enabled: s.Enabled, Timezone: s.Timezone, WorkingHours: s.WorkingHours, Exceptions: s.Exceptions,
		SlotMinutes: s.SlotMinutes, BufferMinutes: s.BufferMinutes, LeadTimeHours: s.LeadTimeHours, HorizonDays: s.HorizonDays,
		MaxPerDay: s.MaxPerDay, UpdatedAt: s.UpdatedAt}
	if v.WorkingHours == nil {
		v.WorkingHours = []venues.OpeningRange{}
	}
	if v.Exceptions == nil {
		v.Exceptions = []venues.Exception{}
	}
	return v
}

type serviceView struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	DurationMinutes int       `json:"duration_minutes"`
	Channel         string    `json:"channel"`
	Location        *string   `json:"location"`
	Notes           *string   `json:"notes"`
	Active          bool      `json:"active"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func viewService(c ConsultationService) serviceView {
	return serviceView{ID: c.ID, Name: c.Name, DurationMinutes: c.DurationMinutes, Channel: c.Channel,
		Location: optional(c.Location), Notes: optional(c.Notes), Active: c.Active, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
}

// publicServiceView is a service as customers see it on the slots page.
type publicServiceView struct {
	ID              uuid.UUID `json:"id"`
	Name            string    `json:"name"`
	DurationMinutes int       `json:"duration_minutes"`
	Channel         string    `json:"channel"`
	Location        *string   `json:"location"`
	Notes           *string   `json:"notes"`
}

// MySettings serves GET /v1/me/appointment-settings.
func (h *Handler) MySettings(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	s, err := h.svc.MySettings(r.Context(), scope.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewSettings(s))
}

// SaveSettings serves PUT /v1/me/appointment-settings.
func (h *Handler) SaveSettings(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	var in SettingsInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	s, err := h.svc.SaveSettings(r.Context(), scope.UserID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, viewSettings(s))
}

// MyServices serves GET /v1/me/consultation-services.
func (h *Handler) MyServices(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	list, err := h.svc.MyServices(r.Context(), scope.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]serviceView, 0, len(list))
	for _, c := range list {
		items = append(items, viewService(c))
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// CreateService serves POST /v1/me/consultation-services.
func (h *Handler) CreateService(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	var in ServiceInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	c, err := h.svc.CreateService(r.Context(), scope.UserID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"service": viewService(c)})
}

func serviceID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpx.WriteError(w, r, http.StatusNotFound, CodeServiceNotFound, "This consultation service does not exist.", nil)
		return uuid.Nil, false
	}
	return id, true
}

// UpdateService serves PATCH /v1/me/consultation-services/{id}.
func (h *Handler) UpdateService(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	id, ok := serviceID(w, r)
	if !ok {
		return
	}
	var in ServiceInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	c, err := h.svc.UpdateService(r.Context(), scope.UserID, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"service": viewService(c)})
}

// ArchiveService serves DELETE /v1/me/consultation-services/{id}.
func (h *Handler) ArchiveService(w http.ResponseWriter, r *http.Request) {
	scope, ok := h.professional(w, r)
	if !ok {
		return
	}
	id, ok := serviceID(w, r)
	if !ok {
		return
	}
	if err := h.svc.ArchiveService(r.Context(), scope.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type slotView struct {
	StartUTC time.Time `json:"start_utc"`
	EndUTC   time.Time `json:"end_utc"`
}

type slotDayView struct {
	Date  string     `json:"date"`
	Slots []slotView `json:"slots"`
}

// Slots serves GET /v1/appointments/slots (public). It never names the
// professional.
func (h *Handler) Slots(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	in := SlotQuery{
		Type: strings.TrimSpace(q.Get("type")), ListingID: q.Get("listing_id"),
		Kind: strings.TrimSpace(q.Get("recipient_kind")), Slug: strings.TrimSpace(q.Get("recipient_slug")),
		ServiceID: strings.TrimSpace(q.Get("service_id")), From: strings.TrimSpace(q.Get("from")),
	}
	if raw := q.Get("days"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.",
				map[string]string{"days": "must be a whole number"})
			return
		}
		in.Days = n
	}
	ans, err := h.svc.Slots(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	services := make([]publicServiceView, 0, len(ans.Services))
	for _, c := range ans.Services {
		services = append(services, publicServiceView{ID: c.ID, Name: c.Name, DurationMinutes: c.DurationMinutes, Channel: c.Channel,
			Location: optional(c.Location), Notes: optional(c.Notes)})
	}
	days := make([]slotDayView, 0, len(ans.Days))
	for _, d := range ans.Days {
		v := slotDayView{Date: d.Date, Slots: make([]slotView, 0, len(d.Slots))}
		for _, s := range d.Slots {
			v.Slots = append(v.Slots, slotView{StartUTC: s.Start, EndUTC: s.End})
		}
		days = append(days, v)
	}
	var slotMinutes *int
	if ans.Configured {
		slotMinutes = &ans.Length
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"configured": ans.Configured, "timezone": ans.Timezone, "slot_minutes": slotMinutes, "services": services, "days": days,
	})
}
