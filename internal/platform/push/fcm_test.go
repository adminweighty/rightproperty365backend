package push

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/platform/config"
)

// fakeGoogle is the OAuth token endpoint and the FCM API in one server.
type fakeGoogle struct {
	t      *testing.T
	key    *rsa.PublicKey
	mu     sync.Mutex
	tokens int
	sends  []map[string]any
	auth   []string
	// reply answers a send (status, body); nil is 200.
	reply func(n int) (int, string)
	// tokenStatus answers the token endpoint with an error status when set.
	tokenStatus int
}

func (g *fakeGoogle) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case r.URL.Path == "/token":
		if g.tokenStatus != 0 {
			w.WriteHeader(g.tokenStatus)
			_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"bad"}`)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			g.t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		g.verifyJWT(r.Form.Get("assertion"))
		g.tokens++
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-" + string(rune('A'+g.tokens-1)), "expires_in": 3600, "token_type": "Bearer"})
	case r.URL.Path == "/v1/projects/rightproperty365/messages:send":
		g.auth = append(g.auth, r.Header.Get("Authorization"))
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			g.t.Errorf("send body: %v", err)
		}
		g.sends = append(g.sends, body)
		if g.reply != nil {
			status, b := g.reply(len(g.sends))
			if status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "120")
			}
			w.WriteHeader(status)
			_, _ = io.WriteString(w, b)
			return
		}
		_, _ = io.WriteString(w, `{"name":"projects/rightproperty365/messages/0:123"}`)
	default:
		http.NotFound(w, r)
	}
}

func (g *fakeGoogle) verifyJWT(assertion string) {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		g.t.Errorf("assertion is not a JWT")
		return
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(g.key, crypto.SHA256, sum[:], sig); err != nil {
		g.t.Errorf("JWT signature: %v", err)
	}
	var h map[string]string
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	_ = json.Unmarshal(hb, &h)
	if h["alg"] != "RS256" || h["kid"] != "key-1" {
		g.t.Errorf("JWT header = %v", h)
	}
	var c map[string]any
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(cb, &c)
	if c["iss"] != "push@rightproperty365.iam.gserviceaccount.com" || c["scope"] != fcmScope || !strings.HasSuffix(c["aud"].(string), "/token") {
		g.t.Errorf("JWT claims = %v", c)
	}
}

func newKeyJSON(t *testing.T, tokenURI string) ([]byte, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	raw, _ := json.Marshal(map[string]string{"type": "service_account", "project_id": "rightproperty365", "private_key_id": "key-1",
		"private_key": pemKey, "client_email": "push@rightproperty365.iam.gserviceaccount.com", "token_uri": tokenURI})
	return raw, key
}

func newFake(t *testing.T) (*fakeGoogle, *FCM, []byte) {
	t.Helper()
	g := &fakeGoogle{t: t}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	raw, key := newKeyJSON(t, srv.URL+"/token")
	g.key = &key.PublicKey
	sa, err := ParseServiceAccount(raw)
	if err != nil {
		t.Fatal(err)
	}
	f, err := NewFCM(FCMConfig{ProjectID: "rightproperty365", Account: sa, Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return g, f, raw
}

func badge(n int) *int { return &n }

// The request is FCM HTTP v1: the token, the safe text as the notification,
// type/link/notification_id as data, the APNs badge; the OAuth token is
// obtained once and reused as the bearer.
func TestFCMRequestShapeAndTokenReuse(t *testing.T) {
	g, f, _ := newFake(t)
	msg := Message{Token: "device-token-1", Title: "New property enquiry", Body: "PR-ZW-10001 · Borrowdale",
		Data: map[string]string{"type": "NEW_LEAD", "link": "/agent/requests/x", "notification_id": "n1"}, Badge: badge(3)}
	for range 2 {
		res, err := f.Send(context.Background(), msg)
		if err != nil {
			t.Fatal(err)
		}
		if res.MessageID != "projects/rightproperty365/messages/0:123" {
			t.Fatalf("message id = %q", res.MessageID)
		}
	}
	if g.tokens != 1 {
		t.Fatalf("%d token exchanges, want 1 (cached)", g.tokens)
	}
	for _, a := range g.auth {
		if a != "Bearer access-A" {
			t.Fatalf("Authorization = %q", a)
		}
	}
	m := g.sends[0]["message"].(map[string]any)
	n := m["notification"].(map[string]any)
	data := m["data"].(map[string]any)
	aps := m["apns"].(map[string]any)["payload"].(map[string]any)["aps"].(map[string]any)
	if m["token"] != "device-token-1" || n["title"] != msg.Title || n["body"] != msg.Body || data["type"] != "NEW_LEAD" ||
		data["link"] != "/agent/requests/x" || data["notification_id"] != "n1" || aps["badge"] != float64(3) ||
		m["android"].(map[string]any)["priority"] != "HIGH" {
		t.Fatalf("message = %v", m)
	}
}

// Error mapping: invalid tokens (remove the device), permanent (dead
// letter) and transient (retry) failures; a refused access token is
// fetched again.
func TestFCMErrorMapping(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   Kind
	}{
		{"unregistered", 404, `{"error":{"code":404,"status":"NOT_FOUND","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"UNREGISTERED"}]}}`, InvalidToken},
		{"invalid token", 400, `{"error":{"code":400,"message":"The registration token is not a valid FCM registration token","status":"INVALID_ARGUMENT","details":[{"@type":"type.googleapis.com/google.firebase.fcm.v1.FcmError","errorCode":"INVALID_ARGUMENT"},{"@type":"type.googleapis.com/google.rpc.BadRequest","fieldViolations":[{"field":"message.token","description":"Invalid registration token"}]}]}}`, InvalidToken},
		{"sender mismatch", 403, `{"error":{"code":403,"status":"PERMISSION_DENIED","details":[{"errorCode":"SENDER_ID_MISMATCH"}]}}`, InvalidToken},
		{"invalid message", 400, `{"error":{"code":400,"message":"Invalid JSON payload","status":"INVALID_ARGUMENT"}}`, Permanent},
		{"unavailable", 503, `{"error":{"code":503,"status":"UNAVAILABLE"}}`, Transient},
		{"internal", 500, `{"error":{"code":500,"status":"INTERNAL"}}`, Transient},
		{"quota", 429, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","details":[{"errorCode":"QUOTA_EXCEEDED"}]}}`, Transient},
		{"auth", 401, `{"error":{"code":401,"status":"UNAUTHENTICATED"}}`, Transient},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, f, _ := newFake(t)
			g.reply = func(int) (int, string) { return c.status, c.body }
			_, err := f.Send(context.Background(), Message{Token: "dev-tok-XYZ", Title: "t"})
			if err == nil || KindOf(err) != c.want {
				t.Fatalf("err = %v, kind = %v, want %v", err, KindOf(err), c.want)
			}
			if strings.Contains(err.Error(), "dev-tok-XYZ") {
				t.Fatalf("error echoes the token: %v", err)
			}
			if c.status == 429 {
				var pe *Error
				if !errorsAs(err, &pe) || pe.RetryAfter != 120*time.Second {
					t.Fatalf("Retry-After not kept: %v", err)
				}
			}
			if c.status == 401 {
				g.reply = nil
				if _, err := f.Send(context.Background(), Message{Token: "tok"}); err != nil {
					t.Fatal(err)
				}
				if g.tokens != 2 || g.auth[len(g.auth)-1] != "Bearer access-B" {
					t.Fatalf("after a 401 the token is fetched again: tokens=%d auth=%v", g.tokens, g.auth)
				}
			}
		})
	}
	// The token endpoint refusing the key is permanent; unreachable is transient.
	g, f, _ := newFake(t)
	g.tokenStatus = http.StatusBadRequest
	if _, err := f.Send(context.Background(), Message{Token: "tok"}); KindOf(err) != Permanent {
		t.Fatalf("invalid_grant: %v", err)
	}
	sa := f.cfg.Account
	sa.TokenURI = "http://127.0.0.1:1/token"
	f2, _ := NewFCM(FCMConfig{ProjectID: "rightproperty365", Account: sa, Endpoint: "http://127.0.0.1:1"})
	if _, err := f2.Send(context.Background(), Message{Token: "tok"}); KindOf(err) != Transient {
		t.Fatalf("unreachable: %v", err)
	}
}

func errorsAs(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

// Startup: without credentials push is disabled in staging and production
// (one line, never an error) and logged in development; a broken key never
// fails startup; the key is never logged.
func TestOpenSelection(t *testing.T) {
	raw, _ := newKeyJSON(t, DefaultTokenURI)
	cases := []struct {
		name, env, project, json, file string
		want                           string
		line                           string
	}{
		{"production without project", config.EnvProduction, "", "", "", "", "push disabled: FCM_PROJECT_ID not set"},
		{"staging without key", config.EnvStaging, "rightproperty365", "", "", "", "push disabled: no FCM service account"},
		{"production broken key", config.EnvProduction, "rightproperty365", `{"private_key":"nope","client_email":"x"}`, "", "", "push disabled: the FCM service account is not usable"},
		{"production missing file", config.EnvProduction, "rightproperty365", "", "/does/not/exist.json", "", "push disabled: FCM_SERVICE_ACCOUNT_FILE could not be read"},
		{"development without project", config.EnvDevelopment, "", "", "", ProviderLog, "push: log pusher selected"},
		{"production with key", config.EnvProduction, "rightproperty365", string(raw), "", ProviderFCM, "push provider selected: fcm"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			cfg := config.Config{Env: c.env, FCMProjectID: c.project, FCMServiceAccountJSON: config.Secret(c.json), FCMServiceAccountFile: c.file}
			p := Open(cfg, logger)
			got := ""
			if p != nil {
				got = p.Name()
			}
			if got != c.want {
				t.Fatalf("pusher = %q, want %q", got, c.want)
			}
			if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), c.line) {
				t.Fatalf("want exactly one line with %q, got:\n%s", c.line, logs.String())
			}
			if strings.Contains(logs.String(), "PRIVATE KEY") || strings.Contains(logs.String(), "nope") {
				t.Fatalf("the key leaked into the log: %s", logs.String())
			}
		})
	}
}

// The log pusher logs the safe text and never the token.
func TestLogPusher(t *testing.T) {
	var logs bytes.Buffer
	p := NewLogPusher(slog.New(slog.NewJSONHandler(&logs, nil)))
	if _, err := p.Send(context.Background(), Message{Token: "secret-device-token-abcd", Title: "New property enquiry",
		Data: map[string]string{"link": "/agent/requests/1"}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "secret-device-token") || !strings.Contains(logs.String(), "…abcd") {
		t.Fatalf("log = %s", logs.String())
	}
}

// Plain http is only accepted for a loopback host (tests).
func TestCheckURL(t *testing.T) {
	for raw, ok := range map[string]bool{"https://oauth2.googleapis.com/token": true, "http://127.0.0.1:9/token": true,
		"http://localhost/token": true, "http://example.com/token": false, "ftp://x": false} {
		if got := checkURL(raw) == nil; got != ok {
			t.Fatalf("%s: %v", raw, got)
		}
	}
	_ = url.Values{}
}
