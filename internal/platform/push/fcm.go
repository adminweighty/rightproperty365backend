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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultFCMEndpoint is the FCM HTTP v1 API.
	DefaultFCMEndpoint = "https://fcm.googleapis.com"
	// DefaultTokenURI is Google's OAuth 2.0 token endpoint.
	DefaultTokenURI = "https://oauth2.googleapis.com/token"
	// fcmScope is the OAuth scope of FCM sends.
	fcmScope = "https://www.googleapis.com/auth/firebase.messaging"
	// maxBodyBytes bounds what is read of a provider response.
	maxBodyBytes = 64 << 10
)

// ServiceAccount is the part of a Google service-account JSON key the
// adapter uses. The private key is never logged and never part of an error.
type ServiceAccount struct {
	ClientEmail  string
	PrivateKeyID string
	TokenURI     string
	key          *rsa.PrivateKey
}

// ParseServiceAccount reads a service-account JSON key. Its errors name the
// problem, never a value from the key.
func ParseServiceAccount(raw []byte) (ServiceAccount, error) {
	var f struct {
		Type         string `json:"type"`
		ClientEmail  string `json:"client_email"`
		PrivateKeyID string `json:"private_key_id"`
		PrivateKey   string `json:"private_key"`
		TokenURI     string `json:"token_uri"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return ServiceAccount{}, errors.New("not valid JSON")
	}
	if f.Type != "" && f.Type != "service_account" {
		return ServiceAccount{}, errors.New(`"type" is not service_account`)
	}
	if f.ClientEmail == "" || f.PrivateKey == "" {
		return ServiceAccount{}, errors.New("client_email or private_key missing")
	}
	block, _ := pem.Decode([]byte(f.PrivateKey))
	if block == nil {
		return ServiceAccount{}, errors.New("private_key is not PEM")
	}
	var key *rsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return ServiceAccount{}, errors.New("private_key is not an RSA key")
		}
		key = rk
	} else if rk, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		key = rk
	} else {
		return ServiceAccount{}, errors.New("private_key could not be parsed")
	}
	if f.TokenURI == "" {
		f.TokenURI = DefaultTokenURI
	}
	if err := checkURL(f.TokenURI); err != nil {
		return ServiceAccount{}, errors.New("token_uri " + err.Error())
	}
	return ServiceAccount{ClientEmail: f.ClientEmail, PrivateKeyID: f.PrivateKeyID, TokenURI: f.TokenURI, key: key}, nil
}

// checkURL accepts https, and plain http to a loopback host only (tests).
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("is not a URL")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errors.New("must be https")
}

// FCMConfig configures the FCM adapter.
type FCMConfig struct {
	ProjectID string
	Account   ServiceAccount
	// Endpoint overrides DefaultFCMEndpoint (tests).
	Endpoint string
	// Client is the HTTP client (nil: a client with a 10 s timeout).
	Client *http.Client
	// Now is the clock (nil: time.Now).
	Now func() time.Time
}

// FCM sends through FCM HTTP v1 with an OAuth 2.0 access token obtained from
// the service account (a signed RS256 JWT exchanged at the token endpoint;
// no SDK). The token is cached until a minute before it expires.
type FCM struct {
	cfg FCMConfig

	mu      sync.Mutex
	token   string
	expires time.Time
}

// NewFCM builds the adapter.
func NewFCM(cfg FCMConfig) (*FCM, error) {
	if cfg.ProjectID == "" {
		return nil, errors.New("no project id")
	}
	if cfg.Account.key == nil {
		return nil, errors.New("no service account key")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = DefaultFCMEndpoint
	}
	if err := checkURL(cfg.Endpoint); err != nil {
		return nil, errors.New("endpoint " + err.Error())
	}
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &FCM{cfg: cfg}, nil
}

// Name implements Pusher.
func (f *FCM) Name() string { return ProviderFCM }

// Send implements Pusher.
func (f *FCM) Send(ctx context.Context, m Message) (Result, error) {
	token, err := f.accessToken(ctx)
	if err != nil {
		return Result{}, err
	}
	body, err := json.Marshal(map[string]any{"message": fcmMessage(m)})
	if err != nil {
		return Result{}, &Error{Kind: Permanent, Msg: "message could not be encoded"}
	}
	endpoint := f.cfg.Endpoint + "/v1/projects/" + url.PathEscape(f.cfg.ProjectID) + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return Result{}, &Error{Kind: Permanent, Msg: "request could not be built"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := f.cfg.Client.Do(req)
	if err != nil {
		return Result{}, &Error{Kind: Transient, Msg: "FCM could not be reached"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if resp.StatusCode == http.StatusOK {
		var ok struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &ok) != nil || ok.Name == "" {
			return Result{}, &Error{Kind: Transient, Status: resp.StatusCode, Msg: "FCM answered without a message name"}
		}
		return Result{MessageID: ok.Name}, nil
	}
	e := mapFCMError(resp.StatusCode, raw)
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if s, err := strconv.Atoi(ra); err == nil && s > 0 {
			e.RetryAfter = time.Duration(s) * time.Second
		}
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// The cached token was refused: fetch a new one on the next try.
		f.mu.Lock()
		f.token, f.expires = "", time.Time{}
		f.mu.Unlock()
	}
	return Result{}, e
}

// fcmMessage builds the HTTP v1 message: the safe text as the notification,
// the type, link and notification id as data, and the badge for APNs.
func fcmMessage(m Message) map[string]any {
	data := map[string]string{}
	for k, v := range m.Data {
		data[k] = v
	}
	android := map[string]any{"priority": "HIGH"}
	if m.CollapseKey != "" {
		android["collapse_key"] = m.CollapseKey
	}
	aps := map[string]any{"sound": "default"}
	if m.Badge != nil {
		aps["badge"] = *m.Badge
		android["notification"] = map[string]any{"notification_count": *m.Badge}
	}
	apns := map[string]any{"headers": map[string]string{"apns-priority": "10"}, "payload": map[string]any{"aps": aps}}
	if m.CollapseKey != "" {
		apns["headers"].(map[string]string)["apns-collapse-id"] = m.CollapseKey
	}
	return map[string]any{
		"token":        m.Token,
		"notification": map[string]string{"title": m.Title, "body": m.Body},
		"data":         data,
		"android":      android,
		"apns":         apns,
	}
}

// mapFCMError classifies an FCM error response:
//
//   - UNREGISTERED (404), SENDER_ID_MISMATCH (403), and INVALID_ARGUMENT
//     about message.token: the token is invalid (remove the device);
//   - other INVALID_ARGUMENT and 400 / 403 / 404: permanent;
//   - 401 (the access token), 429 QUOTA_EXCEEDED, 5xx UNAVAILABLE /
//     INTERNAL and anything else: transient.
func mapFCMError(status int, raw []byte) *Error {
	var body struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
			Details []struct {
				Type            string `json:"@type"`
				ErrorCode       string `json:"errorCode"`
				FieldViolations []struct {
					Field string `json:"field"`
				} `json:"fieldViolations"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &body)
	code := body.Error.Status
	aboutToken := false
	for _, d := range body.Error.Details {
		if d.ErrorCode != "" {
			code = d.ErrorCode
		}
		for _, v := range d.FieldViolations {
			if v.Field == "message.token" {
				aboutToken = true
			}
		}
	}
	if code == "INVALID_ARGUMENT" && strings.Contains(strings.ToLower(body.Error.Message), "registration token") {
		aboutToken = true
	}
	e := &Error{Status: status, Code: code}
	switch {
	case code == "UNREGISTERED":
		e.Kind, e.Msg = InvalidToken, "the device token is no longer registered"
	case code == "SENDER_ID_MISMATCH":
		e.Kind, e.Msg = InvalidToken, "the device token belongs to another sender"
	case code == "INVALID_ARGUMENT" && aboutToken:
		e.Kind, e.Msg = InvalidToken, "the device token is not valid"
	case status == http.StatusUnauthorized:
		e.Kind, e.Msg = Transient, "FCM refused the access token"
	case status == http.StatusTooManyRequests || status >= 500:
		e.Kind, e.Msg = Transient, "FCM is unavailable or rate limiting"
	case status == http.StatusBadRequest || status == http.StatusForbidden || status == http.StatusNotFound:
		e.Kind, e.Msg = Permanent, "FCM rejected the message"
	default:
		e.Kind, e.Msg = Transient, "FCM answered with an unexpected status"
	}
	return e
}

// accessToken returns a cached OAuth token or obtains a new one.
func (f *FCM) accessToken(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.cfg.Now()
	if f.token != "" && now.Before(f.expires.Add(-time.Minute)) {
		return f.token, nil
	}
	assertion, err := f.signedJWT(now)
	if err != nil {
		return "", &Error{Kind: Permanent, Msg: "the service-account assertion could not be signed"}
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.cfg.Account.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", &Error{Kind: Permanent, Msg: "token request could not be built"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := f.cfg.Client.Do(req)
	if err != nil {
		return "", &Error{Kind: Transient, Msg: "the OAuth token endpoint could not be reached"}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(raw, &tok)
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		kind := Transient
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			// invalid_grant and friends: the key is revoked or wrong.
			kind = Permanent
		}
		return "", &Error{Kind: kind, Status: resp.StatusCode, Code: tok.Error, Msg: "the OAuth token endpoint refused the service account"}
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	f.token, f.expires = tok.AccessToken, now.Add(time.Duration(tok.ExpiresIn)*time.Second)
	return f.token, nil
}

// signedJWT builds the RS256 assertion of the JWT bearer grant (RFC 7523).
func (f *FCM) signedJWT(now time.Time) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	if f.cfg.Account.PrivateKeyID != "" {
		header["kid"] = f.cfg.Account.PrivateKeyID
	}
	claims := map[string]any{
		"iss": f.cfg.Account.ClientEmail, "scope": fcmScope, "aud": f.cfg.Account.TokenURI,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	unsigned := enc.EncodeToString(h) + "." + enc.EncodeToString(c)
	sum := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.cfg.Account.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	return unsigned + "." + enc.EncodeToString(sig), nil
}
