package inapp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ValidationError carries per-field messages (400 VALIDATION_FAILED).
type ValidationError struct{ Fields map[string]string }

func (e *ValidationError) Error() string { return "inapp: invalid input" }

// Service is the notification centre's logic.
type Service struct {
	store  Store
	logger *slog.Logger
	now    func() time.Time
	// pushProvider is the pusher's name ("" when push is disabled: no push
	// delivery is queued at all).
	pushProvider string
}

// NewService builds the service. pushProvider is the pusher's name, or ""
// when push is disabled.
func NewService(store Store, pushProvider string, logger *slog.Logger, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{store: store, logger: logger, now: now, pushProvider: pushProvider}
}

// Store returns the store (wiring).
func (s *Service) Store() Store { return s.store }

// PushProvider is the pusher's name, or "disabled".
func (s *Service) PushProvider() string {
	if s.pushProvider == "" {
		return "disabled"
	}
	return s.pushProvider
}

// ---------------------------------------------------------------------------
// Notifications (the worker)
// ---------------------------------------------------------------------------

// Notify creates the in-app notification of a draft once per (source event,
// recipient) and, when push is on for the type, queues its push deliveries.
// It returns the notification and whether it was created now.
func (s *Service) Notify(ctx context.Context, d Draft) (Notification, bool, error) {
	if _, ok := Lookup(d.Type); !ok {
		return Notification{}, false, fmt.Errorf("inapp: unknown type %q", d.Type)
	}
	if !strings.HasPrefix(d.Link, "/") || strings.HasPrefix(d.Link, "//") {
		return Notification{}, false, fmt.Errorf("inapp: link must be a relative path")
	}
	d.Title, d.Body = clip(d.Title, MaxTitleRunes), clip(d.Body, MaxBodyRunes)
	push := false
	if s.pushProvider != "" {
		on, err := s.channelOn(ctx, d.UserID, d.Type, ChannelPush)
		if err != nil {
			return Notification{}, false, err
		}
		push = on
	}
	return s.store.CreateNotification(ctx, d, push, s.now().UTC())
}

// EmailAllowed reports whether the user lets the type be emailed (always
// for an essential type).
func (s *Service) EmailAllowed(ctx context.Context, userID uuid.UUID, typ string) (bool, error) {
	return s.channelOn(ctx, userID, typ, ChannelEmail)
}

func (s *Service) channelOn(ctx context.Context, userID uuid.UUID, typ, channel string) (bool, error) {
	if IsEssential(typ) {
		return true, nil
	}
	prefs, err := s.store.Preferences(ctx, userID)
	if err != nil {
		return false, err
	}
	p, ok := prefs[typ]
	if !ok {
		return true, nil
	}
	if channel == ChannelEmail {
		return p.Email, nil
	}
	return p.Push, nil
}

// RecordEmail stores the EMAIL delivery record of the notification of
// (source event, user), when there is one.
func (s *Service) RecordEmail(ctx context.Context, sourceEventID, userID uuid.UUID, status string, attempts int, lastError string) error {
	n, ok, err := s.store.NotificationBySource(ctx, sourceEventID, userID)
	if err != nil || !ok {
		return err
	}
	return s.store.RecordEmail(ctx, n.ID, status, attempts, clip(lastError, 500), s.now().UTC())
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// ---------------------------------------------------------------------------
// The recipient's centre
// ---------------------------------------------------------------------------

// Page is one page of a user's notifications.
type Page struct {
	Items       []Notification
	UnreadCount int
	Page        int
	Total       int
}

// List returns a page (1-based) of the user's notifications, newest first.
func (s *Service) List(ctx context.Context, userID uuid.UUID, unreadOnly bool, page int) (Page, error) {
	items, total, err := s.store.List(ctx, userID, unreadOnly, (page-1)*PageSize, PageSize)
	if err != nil {
		return Page{}, err
	}
	unread, err := s.store.UnreadCount(ctx, userID)
	if err != nil {
		return Page{}, err
	}
	return Page{Items: items, UnreadCount: unread, Page: page, Total: total}, nil
}

// UnreadCount is the badge.
func (s *Service) UnreadCount(ctx context.Context, userID uuid.UUID) (int, error) {
	return s.store.UnreadCount(ctx, userID)
}

// MarkRead marks one of the user's notifications read (ErrNotFound for
// anybody else's).
func (s *Service) MarkRead(ctx context.Context, userID, id uuid.UUID) (Notification, int, error) {
	n, err := s.store.MarkRead(ctx, userID, id, s.now().UTC())
	if err != nil {
		return Notification{}, 0, err
	}
	unread, err := s.store.UnreadCount(ctx, userID)
	return n, unread, err
}

// MarkAllRead marks every notification of the user read.
func (s *Service) MarkAllRead(ctx context.Context, userID uuid.UUID) (int, error) {
	return s.store.MarkAllRead(ctx, userID, s.now().UTC())
}

// ---------------------------------------------------------------------------
// Devices
// ---------------------------------------------------------------------------

// DeviceInput is the body of POST /v1/me/devices.
type DeviceInput struct {
	Token       string
	Platform    string
	AppVersion  string
	Locale      *string
	PushConsent *bool
}

// RegisterDevice registers or refreshes a token for the user and session.
func (s *Service) RegisterDevice(ctx context.Context, userID uuid.UUID, sessionID *uuid.UUID, in DeviceInput) (Device, bool, error) {
	fields := map[string]string{}
	in.Token = strings.TrimSpace(in.Token)
	switch {
	case in.Token == "":
		fields["token"] = "is required"
	case len(in.Token) > MaxTokenLength:
		fields["token"] = "must be at most " + strconv.Itoa(MaxTokenLength) + " characters"
	case strings.ContainsAny(in.Token, " \t\r\n"):
		fields["token"] = "must not contain spaces"
	}
	switch in.Platform {
	case PlatformIOS, PlatformAndroid, PlatformWeb:
	default:
		fields["platform"] = "must be IOS, ANDROID or WEB"
	}
	in.AppVersion = strings.TrimSpace(in.AppVersion)
	if in.AppVersion == "" || utf8.RuneCountInString(in.AppVersion) > MaxAppVersionLength {
		fields["app_version"] = "is required, at most " + strconv.Itoa(MaxAppVersionLength) + " characters"
	}
	locale := ""
	if in.Locale != nil {
		locale = strings.TrimSpace(*in.Locale)
		if utf8.RuneCountInString(locale) > MaxLocaleLength {
			fields["locale"] = "must be at most " + strconv.Itoa(MaxLocaleLength) + " characters"
		}
	}
	if in.PushConsent == nil {
		fields["push_consent"] = "is required (true or false)"
	}
	if len(fields) > 0 {
		return Device{}, false, &ValidationError{Fields: fields}
	}
	return s.store.UpsertDevice(ctx, Device{UserID: userID, SessionID: sessionID, Token: in.Token, Platform: in.Platform,
		AppVersion: in.AppVersion, Locale: locale, PushConsent: *in.PushConsent, LastSeenAt: s.now().UTC()})
}

// Devices lists the user's devices.
func (s *Service) Devices(ctx context.Context, userID uuid.UUID) ([]Device, error) {
	return s.store.Devices(ctx, userID)
}

// DeleteDevice removes one of the user's devices.
func (s *Service) DeleteDevice(ctx context.Context, userID, deviceID uuid.UUID) error {
	return s.store.DeleteDevice(ctx, userID, deviceID)
}

// DeleteDeviceByToken removes one of the user's tokens.
func (s *Service) DeleteDeviceByToken(ctx context.Context, userID uuid.UUID, token string) error {
	return s.store.DeleteDeviceByToken(ctx, &userID, strings.TrimSpace(token))
}

// OnLogout removes the devices the ending session registered, and the
// given token when it is the user's (FR-045). Unknown tokens are ignored.
func (s *Service) OnLogout(ctx context.Context, userID, sessionID uuid.UUID, deviceToken string) error {
	if _, err := s.store.DeleteSessionDevices(ctx, userID, sessionID); err != nil {
		return err
	}
	if t := strings.TrimSpace(deviceToken); t != "" {
		if err := s.store.DeleteDeviceByToken(ctx, &userID, t); err != nil && !errors.Is(err, ErrDeviceNotFound) {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Preferences
// ---------------------------------------------------------------------------

// PreferenceView is one row of GET /v1/me/notification-preferences.
type PreferenceView struct {
	TypeInfo
	Email     bool
	Push      bool
	ChangedAt *time.Time
	Source    string
}

// Preferences returns every type with the user's choice (or the default).
func (s *Service) Preferences(ctx context.Context, userID uuid.UUID) ([]PreferenceView, error) {
	prefs, err := s.store.Preferences(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := make([]PreferenceView, 0, len(catalogue))
	for _, c := range catalogue {
		v := PreferenceView{TypeInfo: c, Email: true, Push: true}
		if p, ok := prefs[c.Type]; ok && !c.Essential {
			v.Email, v.Push, v.ChangedAt, v.Source = p.Email, p.Push, p.ChangedAt, p.Source
		}
		out = append(out, v)
	}
	return out, nil
}

// PreferenceItem is one item of a PUT.
type PreferenceItem struct {
	Type  string
	Email *bool
	Push  *bool
	InApp *bool
}

// MaxPreferenceItems bounds a PUT.
const MaxPreferenceItems = 50

// SetPreferences validates every item (nothing is applied when one is
// wrong) and applies the changes from the next send.
func (s *Service) SetPreferences(ctx context.Context, userID uuid.UUID, items []PreferenceItem, source string) ([]PreferenceView, error) {
	fields := map[string]string{}
	if len(items) > MaxPreferenceItems {
		fields["items"] = "at most " + strconv.Itoa(MaxPreferenceItems) + " items"
	}
	seen := map[string]bool{}
	changes := make([]PreferenceChange, 0, len(items))
	for i, it := range items {
		key := "items[" + strconv.Itoa(i) + "]"
		info, ok := Lookup(it.Type)
		switch {
		case !ok:
			fields[key+".type"] = "unknown notification type"
			continue
		case seen[it.Type]:
			fields[key+".type"] = "appears more than once"
			continue
		}
		seen[it.Type] = true
		if it.InApp != nil && !*it.InApp {
			fields[key+".in_app"] = "in-app notifications are always on"
		}
		if info.Essential {
			if it.Email != nil && !*it.Email {
				fields[key+".email"] = "PREFERENCE_ESSENTIAL: this notice is essential and cannot be turned off"
			}
			if it.Push != nil && !*it.Push {
				fields[key+".push"] = "PREFERENCE_ESSENTIAL: this notice is essential and cannot be turned off"
			}
			continue
		}
		if it.Email != nil || it.Push != nil {
			changes = append(changes, PreferenceChange{Type: it.Type, Email: it.Email, Push: it.Push})
		}
	}
	if len(fields) > 0 {
		return nil, &ValidationError{Fields: fields}
	}
	if err := s.store.SetPreferences(ctx, userID, changes, source, s.now().UTC()); err != nil {
		return nil, err
	}
	return s.Preferences(ctx, userID)
}

// Health returns the delivery health (admin).
func (s *Service) Health(ctx context.Context) (Health, time.Time, error) {
	now := s.now().UTC()
	h, err := s.store.Health(ctx, now)
	return h, now, err
}
