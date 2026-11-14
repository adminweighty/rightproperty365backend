package directory

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// etagOf is the strong ETag of a logo: its stored checksum.
func etagOf(logo Logo) string { return `"` + logo.Checksum + `"` }

// noneMatch implements the weak comparison of If-None-Match (RFC 9110).
func noneMatch(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// logoCacheControl is the cache policy of a logo response. A request whose v
// parameter is exactly the version of the CURRENT logo (the one in the URL the
// API hands out, listings.LogoVersion) may be kept for a year without asking
// again: when the logo changes, so does that URL. Without v, or with the v of
// another image (an old URL still serves the current logo), the answer is
// kept for minutes only, so a wrong image is never pinned.
func logoCacheControl(logo Logo, version string) string {
	if version != "" && version == listings.LogoVersion(logo.Checksum) {
		return fmt.Sprintf("public, max-age=%d, immutable", int(LogoCacheVersioned.Seconds()))
	}
	return fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d", int(LogoCacheMaxAge.Seconds()), int(LogoCacheStale.Seconds()))
}

// Logo handles GET /v1/directory/{kind}/{slug}/logo: the image of a PUBLIC
// profile's logo. It reads no session and sets no cookie; it is the same for
// everyone and cacheable (see logoCacheControl). The stored content type is sent with nosniff, so a
// browser never guesses another type.
func (h *Handler) Logo(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if !slugRe.MatchString(slug) || len(slug) > 80 {
		h.fail(w, r, ErrProfileNotFound)
		return
	}
	logo, err := h.svc.PublicLogo(r.Context(), chi.URLParam(r, "kind"), slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	head := w.Header()
	etag := etagOf(logo)
	cache := logoCacheControl(logo, r.URL.Query().Get("v"))
	if noneMatch(r.Header.Get("If-None-Match"), etag) {
		head.Set("Cache-Control", cache)
		head.Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	content, err := h.svc.OpenLogo(r.Context(), logo)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer content.Close()
	head.Set("Cache-Control", cache)
	head.Set("ETag", etag)
	head.Set("Content-Type", logo.ContentType)
	head.Set("Content-Length", strconv.FormatInt(logo.Size, 10))
	head.Set("X-Content-Type-Options", "nosniff")
	// An image is not a document: nothing in it may run, wherever it is opened.
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// Other origins (the web app) may show it in an <img>.
	head.Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, content); err != nil {
		h.logger.WarnContext(r.Context(), "logo download interrupted", slog.String("profile", chi.URLParam(r, "kind")+"/"+slug))
	}
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

// private marks a response as personal: never cached by anything shared.
func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

type myProfilesResponse struct {
	Profiles []ManagedProfile `json:"profiles"`
}

type managedProfileResponse struct {
	Profile ManagedProfile `json:"profile"`
}

// Mine handles GET /v1/me/profiles.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	profiles, err := h.svc.Mine(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, myProfilesResponse{Profiles: profiles})
}

// readLogo reads the multipart form: exactly one "file" part. At most
// MaxLogoBytes+1 bytes of it are read, so an oversized file is detected
// without being kept.
func readLogo(r *http.Request) ([]byte, error) {
	problem := func(message string) error { return &ValidationError{Fields: map[string]string{"file": message}} }
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, problem("the request must be multipart/form-data with the field file")
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	var content []byte
	seen := false
	for parts := 0; ; parts++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, ErrLogoTooLarge
		}
		if err != nil {
			return nil, problem("the multipart body could not be read")
		}
		if parts >= 3 || (part.FormName() == "file" && seen) {
			return nil, problem("send exactly one file")
		}
		if part.FormName() == "file" {
			seen = true
			content, err = io.ReadAll(io.LimitReader(part, MaxLogoBytes+1))
			if errors.As(err, &tooLarge) {
				return nil, ErrLogoTooLarge
			}
			if err != nil {
				return nil, problem("the file could not be read")
			}
		}
		_ = part.Close()
	}
	return content, nil
}

func profileAddress(r *http.Request) (kind, slug string, err error) {
	kind, slug = chi.URLParam(r, "kind"), chi.URLParam(r, "slug")
	if !slugRe.MatchString(kind) || !slugRe.MatchString(slug) || len(slug) > 80 || len(kind) > 40 {
		return "", "", ErrProfileNotFound
	}
	return kind, slug, nil
}

// PutLogo handles PUT /v1/me/profiles/{kind}/{slug}/logo (multipart).
func (h *Handler) PutLogo(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	kind, slug, err := profileAddress(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	content, err := readLogo(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	profile, err := h.svc.SetLogo(r.Context(), p, kind, slug, content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, managedProfileResponse{Profile: profile})
}

// DeleteLogo handles DELETE /v1/me/profiles/{kind}/{slug}/logo.
func (h *Handler) DeleteLogo(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	kind, slug, err := profileAddress(r)
	if err == nil {
		err = h.svc.RemoveLogo(r.Context(), p, kind, slug)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
