package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/httpx"
)

// Personal time zone (API 0.62.0): how the person wants instants shown. It is
// display only: nothing stored is converted and no server computation uses it
// ("today" stays the market's date, 0.61.1).
const (
	// TimeZoneMarket shows times in the viewer's market zone (the default).
	TimeZoneMarket = "MARKET"
	// TimeZoneDevice shows times in the device's own zone; the client reads it.
	TimeZoneDevice = "DEVICE"
	// TimeZoneNamed shows times in one named IANA zone (User.TimeZone).
	TimeZoneNamed = "ZONE"

	// HomeMarketCode is the market of a viewer without a membership: the
	// platform's home market, whose time everything was shown in originally.
	HomeMarketCode = "ZW"

	msgTimeZoneMode = "Choose market time, this device's time zone, or a time zone."
	msgTimeZoneName = "Choose a time zone from the list."
	msgTimeZoneNone = "A time zone is only given with the mode ZONE."
)

// TimeZoneStore stores the setting.
type TimeZoneStore interface {
	// SetTimeZone stores the mode and, for TimeZoneNamed, the zone ("" for the
	// other modes). ErrNotFound when the user does not exist.
	SetTimeZone(ctx context.Context, userID uuid.UUID, mode, zone string) error
}

// TimeZoneView is the setting as the API shows it (in /v1/me, the sign-in
// answers and /v1/me/time-zone).
type TimeZoneView struct {
	Mode string  `json:"mode"`
	Zone *string `json:"zone"`
	// MarketCode and MarketZone are the viewer's market (the first active
	// membership, else HomeMarketCode) and its IANA zone ("UTC" when the
	// market has none configured).
	MarketCode string `json:"market_code"`
	MarketZone string `json:"market_zone"`
}

var zoneNamePattern = regexp.MustCompile(`^[A-Za-z0-9_+\-/]{1,64}$`)

// ValidTimeZone reports whether name is a zone of the tz database that may be
// stored: loadable, not "Local", and of a plain name shape.
func ValidTimeZone(name string) bool {
	if name == "Local" || !zoneNamePattern.MatchString(name) {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// timeZoneView builds the view of a user without the market zone.
func (u User) timeZoneView() TimeZoneView {
	v := TimeZoneView{Mode: u.TimeZoneMode, MarketCode: HomeMarketCode}
	if v.Mode == "" {
		v.Mode = TimeZoneMarket
	}
	if v.Mode == TimeZoneNamed && u.TimeZone != "" {
		zone := u.TimeZone
		v.Zone = &zone
	}
	if len(u.Memberships) > 0 && u.Memberships[0].MarketCode != "" {
		v.MarketCode = u.Memberships[0].MarketCode
	}
	return v
}

// SetTimeZone validates and stores the setting and returns the user.
func (s *Service) SetTimeZone(ctx context.Context, userID uuid.UUID, mode string, zone *string) (User, error) {
	switch mode {
	case TimeZoneMarket, TimeZoneDevice:
		if zone != nil {
			return User{}, &ValidationError{Fields: map[string]string{"zone": msgTimeZoneNone}}
		}
	case TimeZoneNamed:
		if zone == nil || !ValidTimeZone(*zone) {
			return User{}, &ValidationError{Fields: map[string]string{"zone": msgTimeZoneName}}
		}
	default:
		return User{}, &ValidationError{Fields: map[string]string{"mode": msgTimeZoneMode}}
	}
	name := ""
	if mode == TimeZoneNamed {
		name = *zone
	}
	if err := s.store.SetTimeZone(ctx, userID, mode, name); err != nil {
		return User{}, err
	}
	return s.User(ctx, userID)
}

// SetMarketZone sets how the handler finds a market's IANA zone (the
// countries table); without it every market zone is UTC.
func (h *Handler) SetMarketZone(zone func(ctx context.Context, marketCode string) string) {
	h.marketZone = zone
}

// view is the API representation of a user, with the market zone resolved.
func (h *Handler) view(ctx context.Context, u User) UserView {
	v := u.View()
	v.TimeZone.MarketZone = h.zoneOf(ctx, v.TimeZone.MarketCode)
	return v
}

func (h *Handler) zoneOf(ctx context.Context, code string) string {
	if h.marketZone != nil {
		if z := h.marketZone(ctx, code); z != "" && ValidTimeZone(z) {
			return z
		}
	}
	return "UTC"
}

// TimeZone serves GET /v1/me/time-zone.
func (h *Handler) TimeZone(w http.ResponseWriter, r *http.Request) {
	p := h.signedIn(w, r)
	if p == nil {
		return
	}
	user, err := h.svc.User(r.Context(), p.UserID)
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.view(r.Context(), user).TimeZone)
}

// SetTimeZone serves PUT /v1/me/time-zone.
func (h *Handler) SetTimeZone(w http.ResponseWriter, r *http.Request) {
	p := h.signedIn(w, r)
	if p == nil {
		return
	}
	var body struct {
		Mode json.RawMessage `json:"mode"`
		Zone json.RawMessage `json:"zone"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	var mode string
	if len(body.Mode) == 0 || json.Unmarshal(body.Mode, &mode) != nil {
		h.fail(w, r, &ValidationError{Fields: map[string]string{"mode": msgTimeZoneMode}})
		return
	}
	var zone *string
	if len(body.Zone) > 0 && string(body.Zone) != "null" {
		var z string
		if json.Unmarshal(body.Zone, &z) != nil {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"zone": msgTimeZoneName}})
			return
		}
		zone = &z
	}
	user, err := h.svc.SetTimeZone(r.Context(), p.UserID, mode, zone)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, h.view(r.Context(), user).TimeZone)
}
