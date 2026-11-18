package inapp

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/push"
)

// Error codes.
const (
	CodeDeviceNotFound       = "DEVICE_NOT_FOUND"
	CodeNotificationNotFound = "NOTIFICATION_NOT_FOUND"
)

// Handler serves /v1/me/devices, /v1/me/notifications,
// /v1/me/notification-preferences and /v1/admin/notifications/health.
// Every route needs a signed-in session (auth.RequireAuth in the router);
// another user's device or notification is 404.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var v *ValidationError
	switch {
	case errors.As(err, &v):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields are not valid.", v.Fields)
	case errors.Is(err, ErrDeviceNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeDeviceNotFound, "This device is not registered to your account.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotificationNotFound, "This notification does not exist.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// ---------------------------------------------------------------------------
// Devices
// ---------------------------------------------------------------------------

type deviceView struct {
	ID          uuid.UUID `json:"id"`
	Platform    string    `json:"platform"`
	AppVersion  string    `json:"app_version"`
	Locale      *string   `json:"locale"`
	PushConsent bool      `json:"push_consent"`
	TokenHint   string    `json:"token_hint"`
	CreatedAt   time.Time `json:"created_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

func viewDevice(d Device) deviceView {
	v := deviceView{ID: d.ID, Platform: d.Platform, AppVersion: d.AppVersion, PushConsent: d.PushConsent,
		TokenHint: push.TokenHint(d.Token), CreatedAt: d.CreatedAt, LastSeenAt: d.LastSeenAt}
	if d.Locale != "" {
		l := d.Locale
		v.Locale = &l
	}
	return v
}

// RegisterDevice serves POST /v1/me/devices.
func (h *Handler) RegisterDevice(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	var in struct {
		Token       string  `json:"token"`
		Platform    string  `json:"platform"`
		AppVersion  string  `json:"app_version"`
		Locale      *string `json:"locale"`
		PushConsent *bool   `json:"push_consent"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	var session *uuid.UUID
	if p.SessionID != uuid.Nil {
		s := p.SessionID
		session = &s
	}
	d, created, err := h.svc.RegisterDevice(r.Context(), p.UserID, session, DeviceInput{Token: in.Token, Platform: in.Platform,
		AppVersion: in.AppVersion, Locale: in.Locale, PushConsent: in.PushConsent})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	private(w)
	httpx.WriteJSON(w, status, map[string]uuid.UUID{"device_id": d.ID})
}

// Devices serves GET /v1/me/devices.
func (h *Handler) Devices(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	devices, err := h.svc.Devices(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]deviceView, 0, len(devices))
	for _, d := range devices {
		items = append(items, viewDevice(d))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// DeleteDevice serves DELETE /v1/me/devices/{deviceId}.
func (h *Handler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "deviceId"))
	if err != nil {
		h.fail(w, r, ErrDeviceNotFound)
		return
	}
	if err := h.svc.DeleteDevice(r.Context(), p.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeleteDeviceByToken serves DELETE /v1/me/devices?token=.
func (h *Handler) DeleteDeviceByToken(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	token := r.URL.Query().Get("token")
	if token == "" {
		h.fail(w, r, &ValidationError{Fields: map[string]string{"token": "is required"}})
		return
	}
	if err := h.svc.DeleteDeviceByToken(r.Context(), p.UserID, token); err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Notifications
// ---------------------------------------------------------------------------

type notificationView struct {
	ID        uuid.UUID  `json:"id"`
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Link      string     `json:"link"`
	Reference *string    `json:"reference"`
	CreatedAt time.Time  `json:"created_at"`
	ReadAt    *time.Time `json:"read_at"`
}

func viewNotification(n Notification) notificationView {
	v := notificationView{ID: n.ID, Type: n.Type, Title: n.Title, Body: n.Body, Link: n.Link, CreatedAt: n.CreatedAt, ReadAt: n.ReadAt}
	if n.Reference != "" {
		ref := n.Reference
		v.Reference = &ref
	}
	return v
}

// List serves GET /v1/me/notifications?unread=true&page=.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	q := r.URL.Query()
	page := 1
	if raw := q.Get("page"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 100000 {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"page": "must be an integer of at least 1"}})
			return
		}
		page = n
	}
	unread := false
	if raw := q.Get("unread"); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"unread": "must be true or false"}})
			return
		}
		unread = b
	}
	res, err := h.svc.List(r.Context(), p.UserID, unread, page)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]notificationView, 0, len(res.Items))
	for _, n := range res.Items {
		items = append(items, viewNotification(n))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "unread_count": res.UnreadCount, "page": res.Page,
		"page_size": PageSize, "total": res.Total})
}

// UnreadCount serves GET /v1/me/notifications/unread-count.
func (h *Handler) UnreadCount(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	n, err := h.svc.UnreadCount(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"unread_count": n})
}

// MarkRead serves POST /v1/me/notifications/{id}/read.
func (h *Handler) MarkRead(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrNotFound)
		return
	}
	n, unread, err := h.svc.MarkRead(r.Context(), p.UserID, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"notification": viewNotification(n), "unread_count": unread})
}

// MarkAllRead serves POST /v1/me/notifications/read-all.
func (h *Handler) MarkAllRead(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	n, err := h.svc.MarkAllRead(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]int{"marked": n, "unread_count": 0})
}

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

type preferenceView struct {
	Type      string `json:"type"`
	Label     string `json:"label"`
	Essential bool   `json:"essential"`
	// EssentialReason is set for an essential type only.
	EssentialReason *string    `json:"essential_reason"`
	Email           bool       `json:"email"`
	Push            bool       `json:"push"`
	InApp           bool       `json:"in_app"`
	ChangedAt       *time.Time `json:"changed_at"`
	Source          *string    `json:"source"`
}

func writePreferences(w http.ResponseWriter, views []PreferenceView) {
	items := make([]preferenceView, 0, len(views))
	for _, v := range views {
		pv := preferenceView{Type: v.Type, Label: v.Label, Essential: v.Essential, Email: v.Email, Push: v.Push, InApp: true, ChangedAt: v.ChangedAt}
		if v.Source != "" {
			s := v.Source
			pv.Source = &s
		}
		if v.Essential {
			reason := v.EssentialReason
			pv.EssentialReason = &reason
		}
		items = append(items, pv)
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

// Preferences serves GET /v1/me/notification-preferences.
func (h *Handler) Preferences(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	views, err := h.svc.Preferences(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writePreferences(w, views)
}

// SetPreferences serves PUT /v1/me/notification-preferences. The X-Client
// header is recorded as the change's source.
func (h *Handler) SetPreferences(w http.ResponseWriter, r *http.Request) {
	p := h.principal(w, r)
	if p == nil {
		return
	}
	var in struct {
		Items []struct {
			Type  string `json:"type"`
			Email *bool  `json:"email"`
			Push  *bool  `json:"push"`
			InApp *bool  `json:"in_app"`
		} `json:"items"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	items := make([]PreferenceItem, 0, len(in.Items))
	for _, it := range in.Items {
		items = append(items, PreferenceItem{Type: it.Type, Email: it.Email, Push: it.Push, InApp: it.InApp})
	}
	views, err := h.svc.SetPreferences(r.Context(), p.UserID, items, SourceFromClient(r.Header.Get("X-Client")))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writePreferences(w, views)
}

// ---------------------------------------------------------------------------
// Admin
// ---------------------------------------------------------------------------

// AdminRoles may read the delivery health.
func AdminRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformSupport}
}

// Health serves GET /v1/admin/notifications/health.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	health, now, err := h.svc.Health(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var age *int64
	if health.OldestUnattemptedAt != nil {
		s := int64(now.Sub(*health.OldestUnattemptedAt).Seconds())
		age = &s
	}
	provider := h.svc.PushProvider()
	private(w)
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"push":                           map[string]any{"enabled": provider != push.ProviderDisabled, "provider": provider},
		"deliveries":                     health.Counts,
		"dead_letter_backlog":            health.DeadLetterBacklog,
		"oldest_unattempted_at":          health.OldestUnattemptedAt,
		"oldest_unattempted_age_seconds": age,
		"notifications_last_24h":         health.NotificationsLast24h,
		"unread_total":                   health.UnreadTotal,
		"generated_at":                   now,
	})
}
