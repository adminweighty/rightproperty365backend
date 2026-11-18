package app

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/devseed"
	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/modules/inapp"
)

type deviceList struct {
	Items []struct {
		ID        string `json:"id"`
		TokenHint string `json:"token_hint"`
		Platform  string `json:"platform"`
	} `json:"items"`
}

type notificationPage struct {
	Items []struct {
		ID        string  `json:"id"`
		Type      string  `json:"type"`
		Title     string  `json:"title"`
		Link      string  `json:"link"`
		Reference *string `json:"reference"`
		ReadAt    *string `json:"read_at"`
	} `json:"items"`
	UnreadCount int `json:"unread_count"`
	Page        int `json:"page"`
	PageSize    int `json:"page_size"`
	Total       int `json:"total"`
}

func (e *env) userID(email string) uuid.UUID {
	e.t.Helper()
	u, err := e.identity.UserByEmail(context.Background(), email)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

func (e *env) notifyUser(user uuid.UUID) inapp.Notification {
	e.t.Helper()
	n, _, err := e.inboxSvc.Notify(context.Background(), inapp.Draft{SourceEventID: uuid.New(), UserID: user, Type: inapp.TypeNewLead,
		Title: "New property enquiry", Body: "PR-ZW-10001 · Borrowdale", Link: "/agent/requests/" + uuid.NewString(), Reference: "PR-ZW-10001"})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func device(token string) obj {
	return obj{"token": token, "platform": "ANDROID", "app_version": "1.4.0", "locale": "en-ZW", "push_consent": true}
}

// Devices: register (201, then 200), list with the token masked, and a
// wrong user's delete is 404; logout with device_token removes it.
func TestDeviceRoutes(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	other := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	wantError(t, e.post("/v1/me/devices", device("tok-1")), http.StatusUnauthorized, "UNAUTHENTICATED")

	rec := e.post("/v1/me/devices", device("fcm-token-agent-0001"), agent)
	wantStatus(t, rec, http.StatusCreated)
	id := decode[map[string]string](t, rec)["device_id"]
	wantStatus(t, e.post("/v1/me/devices", device("fcm-token-agent-0001"), agent), http.StatusOK)
	wantError(t, e.post("/v1/me/devices", obj{"token": "", "platform": "PALM", "app_version": ""}, agent), http.StatusBadRequest, "VALIDATION_FAILED")

	list := decode[deviceList](t, e.get("/v1/me/devices", agent))
	if len(list.Items) != 1 || list.Items[0].ID != id || list.Items[0].TokenHint != "…0001" {
		t.Fatalf("devices = %+v", list)
	}
	if body := e.get("/v1/me/devices", agent).Body.String(); strings.Contains(body, "fcm-token-agent") {
		t.Fatalf("the token is returned: %s", body)
	}
	wantError(t, e.del("/v1/me/devices/"+id, nil, other), http.StatusNotFound, inapp.CodeDeviceNotFound)
	wantError(t, e.del("/v1/me/devices?token=fcm-token-agent-0001", nil, other), http.StatusNotFound, inapp.CodeDeviceNotFound)
	wantError(t, e.del("/v1/me/devices/not-a-uuid", nil, agent), http.StatusNotFound, inapp.CodeDeviceNotFound)
	wantError(t, e.del("/v1/me/devices", nil, agent), http.StatusBadRequest, "VALIDATION_FAILED")

	// Logout with the token removes it (FR-045); the empty body still works.
	wantStatus(t, e.post("/v1/auth/logout", obj{"device_token": "fcm-token-agent-0001"}, agent), http.StatusNoContent)
	if devs, _ := e.inboxSvc.Devices(context.Background(), e.userID(identity.SeedAgentEmail)); len(devs) != 0 {
		t.Fatalf("logout kept the device: %v", devs)
	}
	wantStatus(t, e.post("/v1/auth/logout", nil, other), http.StatusNoContent)

	// The session that registered a device removes it on logout, without a body.
	bearer := e.loginBearer(identity.SeedAgentEmail, seedPassword)
	wantStatus(t, e.post("/v1/me/devices", device("fcm-token-app-0002"), withBearer(bearer)), http.StatusCreated)
	wantStatus(t, e.post("/v1/auth/logout", nil, withBearer(bearer)), http.StatusNoContent)
	if devs, _ := e.inboxSvc.Devices(context.Background(), e.userID(identity.SeedAgentEmail)); len(devs) != 0 {
		t.Fatalf("the session's logout kept its device: %v", devs)
	}
}

// The notification centre: list, unread count, read (404 for another
// user's), read-all.
func TestNotificationRoutes(t *testing.T) {
	e := newEnv(t)
	agentID := e.userID(identity.SeedAgentEmail)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	other := browser(e.loginCookie(identity.SeedConsumerEmail, seedPassword))
	n1 := e.notifyUser(agentID)
	e.notifyUser(agentID)

	page := decode[notificationPage](t, e.get("/v1/me/notifications?unread=true&page=1", agent))
	if len(page.Items) != 2 || page.UnreadCount != 2 || page.Page != 1 || page.PageSize != inapp.PageSize || page.Total != 2 ||
		page.Items[0].Reference == nil || *page.Items[0].Reference != "PR-ZW-10001" {
		t.Fatalf("page = %+v", page)
	}
	if got := decode[map[string]int](t, e.get("/v1/me/notifications/unread-count", agent))["unread_count"]; got != 2 {
		t.Fatalf("unread = %d", got)
	}
	wantError(t, e.get("/v1/me/notifications?page=0", agent), http.StatusBadRequest, "VALIDATION_FAILED")
	if page := decode[notificationPage](t, e.get("/v1/me/notifications", other)); len(page.Items) != 0 || page.UnreadCount != 0 {
		t.Fatalf("another user sees %+v", page)
	}
	wantError(t, e.post("/v1/me/notifications/"+n1.ID.String()+"/read", nil, other), http.StatusNotFound, inapp.CodeNotificationNotFound)
	wantError(t, e.post("/v1/me/notifications/nope/read", nil, agent), http.StatusNotFound, inapp.CodeNotificationNotFound)

	rec := e.post("/v1/me/notifications/"+n1.ID.String()+"/read", nil, agent)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[map[string]any](t, rec)["unread_count"]; got != float64(1) {
		t.Fatalf("unread after read = %v", got)
	}
	rec = e.post("/v1/me/notifications/read-all", nil, agent)
	wantStatus(t, rec, http.StatusOK)
	if got := decode[map[string]int](t, rec); got["marked"] != 1 || got["unread_count"] != 0 {
		t.Fatalf("read-all = %v", got)
	}
	if rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

type preferenceList struct {
	Items []struct {
		Type            string  `json:"type"`
		Label           string  `json:"label"`
		Essential       bool    `json:"essential"`
		EssentialReason *string `json:"essential_reason"`
		Email           bool    `json:"email"`
		Push            bool    `json:"push"`
		InApp           bool    `json:"in_app"`
		ChangedAt       *string `json:"changed_at"`
		Source          *string `json:"source"`
	} `json:"items"`
}

// Preferences: every type, essential ones locked on (400 on that item),
// X-Client recorded as the source.
func TestNotificationPreferenceRoutes(t *testing.T) {
	e := newEnv(t)
	agent := browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))
	prefs := decode[preferenceList](t, e.get("/v1/me/notification-preferences", agent))
	if len(prefs.Items) != len(inapp.Catalogue()) {
		t.Fatalf("%d items", len(prefs.Items))
	}
	for _, p := range prefs.Items {
		if !p.Email || !p.Push || !p.InApp || p.ChangedAt != nil || p.Essential != (p.EssentialReason != nil) {
			t.Fatalf("default = %+v", p)
		}
	}
	env := wantError(t, e.do(http.MethodPut, "/v1/me/notification-preferences", obj{"items": []obj{{"type": "NEW_LEAD", "email": false},
		{"type": "ACCOUNT", "push": false}}}, agent), http.StatusBadRequest, "VALIDATION_FAILED")
	if env.Error.Fields["items[1].push"] == "" {
		t.Fatalf("fields = %v", env.Error.Fields)
	}
	rec := e.do(http.MethodPut, "/v1/me/notification-preferences", obj{"items": []obj{{"type": "NEW_LEAD", "email": false}}}, agent,
		withHeader("X-Client", "web"))
	wantStatus(t, rec, http.StatusOK)
	for _, p := range decode[preferenceList](t, rec).Items {
		if p.Type == "NEW_LEAD" && (p.Email || !p.Push || p.Source == nil || *p.Source != "web" || p.ChangedAt == nil) {
			t.Fatalf("NEW_LEAD = %+v", p)
		}
	}
	h := e.inbox.History()
	if len(h) != 1 || h[0].Source != inapp.SourceWeb {
		t.Fatalf("history = %+v", h)
	}
}

// Account deletion removes the user's devices and notifications (the users
// row is anonymised, so nothing cascades).
func TestAccountDeletionRemovesDevices(t *testing.T) {
	e := newEnv(t)
	cookie := browser(e.loginCookie(devseed.SellerEmail, seedPassword))
	seller := e.userID(devseed.SellerEmail)
	wantStatus(t, e.post("/v1/me/devices", device("fcm-token-seller-0003"), cookie), http.StatusCreated)
	e.notifyUser(seller)
	wantStatus(t, e.del("/v1/me", obj{"password": seedPassword}, cookie), http.StatusOK)
	if devs, _ := e.inboxSvc.Devices(context.Background(), seller); len(devs) != 0 {
		t.Fatalf("deletion kept the device: %v", devs)
	}
	if n, _ := e.inboxSvc.UnreadCount(context.Background(), seller); n != 0 {
		t.Fatal("deletion kept the notifications")
	}
}

// The health endpoint: administrators only.
func TestNotificationHealth(t *testing.T) {
	e := newEnv(t)
	wantError(t, e.get("/v1/admin/notifications/health", browser(e.loginCookie(identity.SeedAgentEmail, seedPassword))), http.StatusForbidden, "FORBIDDEN")
	e.notifyUser(e.userID(identity.SeedAgentEmail))
	rec := e.get("/v1/admin/notifications/health", withCookie(e.adminLogin(identity.SeedAdminEmail, seedPassword)))
	wantStatus(t, rec, http.StatusOK)
	body := decode[map[string]any](t, rec)
	pushInfo := body["push"].(map[string]any)
	deliveries := body["deliveries"].(map[string]any)
	if pushInfo["provider"] != "log" || pushInfo["enabled"] != true || deliveries["PUSH"] == nil || body["unread_total"] != float64(1) ||
		body["notifications_last_24h"] == nil {
		t.Fatalf("health = %v", body)
	}
}
