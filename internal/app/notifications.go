package app

import (
	"context"
	"log/slog"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/inapp"
	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/modules/reference"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// The /v1/me settings write bucket: registering and removing devices,
// marking notifications read, changing notification preferences (API
// 0.35.0) and changing marketing preferences (API 0.37.1) share it: bursts
// of 60, refilled at 60 per minute, per caller IP (httpx.RateLimiter). The
// unread count is read-only and polled every 60 s by the website, so it is
// not limited here.
const (
	notificationChangesPerMinute = 60
	notificationChangesBurst     = 60
)

// newSettingsLimiter is the shared /v1/me settings write bucket.
func newSettingsLimiter() *httpx.RateLimiter {
	return httpx.NewRateLimiter(notificationChangesPerMinute, notificationChangesBurst)
}

// notificationRoutes mounts the notification centre (API 0.35.0): every
// route needs a signed-in session (either scope); another user's device or
// notification is 404. Writes use the shared settings bucket.
func notificationRoutes(r chi.Router, h *inapp.Handler, limiter *httpx.RateLimiter) {
	r.Route("/me/devices", func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Get("/", h.Devices)
		r.With(limiter.Middleware).Post("/", h.RegisterDevice)
		r.With(limiter.Middleware).Delete("/", h.DeleteDeviceByToken)
		r.With(limiter.Middleware).Delete("/{deviceId}", h.DeleteDevice)
	})
	r.Route("/me/notifications", func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Get("/", h.List)
		r.Get("/unread-count", h.UnreadCount)
		r.With(limiter.Middleware).Post("/read-all", h.MarkAllRead)
		r.With(limiter.Middleware).Post("/{id}/read", h.MarkRead)
	})
	r.Route("/me/notification-preferences", func(r chi.Router) {
		r.Use(auth.RequireAuth)
		r.Get("/", h.Preferences)
		r.With(limiter.Middleware).Put("/", h.SetPreferences)
	})
}

// ListingPlace names a listing's place for the in-app and push text: its
// suburb's label, else its city's, else its free-text city. "" when the
// listing or the labels cannot be read (the text then has no place).
func ListingPlace(store listings.Store, ref reference.Store, logger *slog.Logger) func(ctx context.Context, id uuid.UUID) string {
	labels := reference.NewService(ref, nil)
	return func(ctx context.Context, id uuid.UUID) string {
		records, err := store.ByIDs(ctx, []uuid.UUID{id})
		if err != nil || len(records) == 0 {
			return ""
		}
		r := records[0]
		for _, c := range []struct{ set, code string }{{listings.SetSuburbs, r.Suburb}, {listings.SetCities, r.City}} {
			if c.code == "" {
				continue
			}
			names, err := labels.Labels(ctx, c.set)
			if err != nil {
				logger.WarnContext(ctx, "could not load place labels", slog.String("error", err.Error()))
				return ""
			}
			if name, ok := names[c.code]; ok {
				return name
			}
		}
		return r.CityName
	}
}
