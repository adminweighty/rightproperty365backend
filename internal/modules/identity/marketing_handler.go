package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// marketingRecordView is one marketing consent record in the API.
type marketingRecordView struct {
	MarketingOptIn     bool      `json:"marketing_opt_in"`
	ChangedAt          time.Time `json:"changed_at"`
	Source             string    `json:"source"`
	Context            string    `json:"context"`
	ConsentTextVersion *string   `json:"consent_text_version"`
}

// marketingPreferencesView is the body of GET and PUT
// /v1/me/marketing-preferences (API 0.37.1). changed_at, source, context and
// consent_text_version come from the newest record and are all null when
// the account never chose.
type marketingPreferencesView struct {
	MarketingOptIn            bool       `json:"marketing_opt_in"`
	ChangedAt                 *time.Time `json:"changed_at"`
	Source                    *string    `json:"source"`
	Context                   *string    `json:"context"`
	ConsentTextVersion        *string    `json:"consent_text_version"`
	CurrentConsentTextVersion string     `json:"current_consent_text_version"`
	// ConsentText is the current wording (new in 0.37.1 with the wording
	// table), for clients to show instead of a copy of their own.
	ConsentText consentTextView       `json:"consent_text"`
	History     []marketingRecordView `json:"history"`
}

// consentTextView is one wording version in the API.
type consentTextView struct {
	Version string `json:"version"`
	Text    string `json:"text"`
	Status  string `json:"status"`
}

func viewConsentText(t MarketingConsentText) consentTextView {
	return consentTextView{Version: t.Version, Text: t.Text, Status: t.Status}
}

func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func marketingView(m MarketingState, text MarketingConsentText) marketingPreferencesView {
	v := marketingPreferencesView{
		MarketingOptIn:            m.OptIn,
		CurrentConsentTextVersion: CurrentMarketingConsentVersion,
		ConsentText:               viewConsentText(text),
		History:                   make([]marketingRecordView, 0, len(m.History)),
	}
	if latest, ok := m.Latest(); ok {
		at, source, kind := latest.ChangedAt.UTC(), latest.Source, latest.Context
		v.ChangedAt, v.Source, v.Context = &at, &source, &kind
		v.ConsentTextVersion = optionalString(latest.ConsentTextVersion)
	}
	for _, e := range m.History {
		v.History = append(v.History, marketingRecordView{
			MarketingOptIn: e.OptIn, ChangedAt: e.ChangedAt.UTC(), Source: e.Source, Context: e.Context,
			ConsentTextVersion: optionalString(e.ConsentTextVersion),
		})
	}
	return v
}

// writeMarketing answers with the preferences. The route also sets
// Cache-Control: private, no-store on every answer (httpx.PrivateNoStore).
func (h *Handler) writeMarketing(w http.ResponseWriter, r *http.Request, m MarketingState) {
	text, err := h.svc.CurrentMarketingConsentText(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, marketingView(m, text))
}

// CodeConsentTextNotFound answers an unknown wording version.
const CodeConsentTextNotFound = "MARKETING_CONSENT_TEXT_NOT_FOUND"

// MarketingConsentText handles GET /v1/marketing-consent-texts/{version}
// (public): one wording version, so a client can show what an older record
// agreed to. Rows never change, so the answer is cacheable.
func (h *Handler) MarketingConsentText(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.MarketingConsentText(r.Context(), chi.URLParam(r, "version"))
	if errors.Is(err, ErrConsentTextNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, CodeConsentTextNotFound, "There is no consent wording with this version.", nil)
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, viewConsentText(t), time.Hour, 24*time.Hour)
}

// signedIn returns the caller, or answers 401.
func (h *Handler) signedIn(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

// MarketingPreferences handles GET /v1/me/marketing-preferences.
func (h *Handler) MarketingPreferences(w http.ResponseWriter, r *http.Request) {
	p := h.signedIn(w, r)
	if p == nil {
		return
	}
	m, err := h.svc.MarketingPreferences(r.Context(), p.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeMarketing(w, r, m)
}

// SetMarketingPreferences handles PUT /v1/me/marketing-preferences:
// {"marketing_opt_in": bool, "consent_text_version": string?}. The X-Client
// header is recorded as the change's source.
func (h *Handler) SetMarketingPreferences(w http.ResponseWriter, r *http.Request) {
	p := h.signedIn(w, r)
	if p == nil {
		return
	}
	// Raw, so that a missing or non-boolean value is a field error rather
	// than INVALID_JSON.
	var body struct {
		MarketingOptIn     json.RawMessage `json:"marketing_opt_in"`
		ConsentTextVersion json.RawMessage `json:"consent_text_version"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	var optIn bool
	if len(body.MarketingOptIn) == 0 || string(body.MarketingOptIn) == "null" || json.Unmarshal(body.MarketingOptIn, &optIn) != nil {
		h.fail(w, r, &ValidationError{Fields: map[string]string{"marketing_opt_in": msgMarketingOptInRequired}})
		return
	}
	in := MarketingPreferenceInput{OptIn: optIn, Source: httpx.ClientSource(r)}
	// The version matters on opt-in only; on opt-out it is ignored.
	if optIn && len(body.ConsentTextVersion) > 0 && string(body.ConsentTextVersion) != "null" {
		var version string
		if json.Unmarshal(body.ConsentTextVersion, &version) != nil {
			h.fail(w, r, &ValidationError{Fields: map[string]string{"consent_text_version": msgConsentVersionInvalid}})
			return
		}
		in.ConsentTextVersion = &version
	}
	m, err := h.svc.SetMarketingPreferences(r.Context(), p.UserID, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.writeMarketing(w, r, m)
}
