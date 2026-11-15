package listings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes of listing management.
const (
	CodeListingIncomplete = "LISTING_INCOMPLETE"
	CodeListingChanged    = "LISTING_CHANGED"
	CodePhotoLimitReached = "PHOTO_LIMIT_REACHED"
	CodePhotoNotFound     = "PHOTO_NOT_FOUND"
	CodePhotoRequired     = "PHOTO_REQUIRED"
	CodeFileTooLarge      = "FILE_TOO_LARGE"
	CodeUnsupportedFile   = "UNSUPPORTED_FILE_TYPE"
	// CodeSelfReviewForbidden: a reviewer tried to decide a listing of their
	// own organisation, or one they own, are the agent of or submitted.
	CodeSelfReviewForbidden = "SELF_REVIEW_FORBIDDEN"
)

// PhotoLimitMessage is the message of PHOTO_LIMIT_REACHED (the owner's words).
const PhotoLimitMessage = "A property can have up to 20 photos."

// Cache policy of a public photo: a year when the URL names its version
// (that URL never serves another image), minutes otherwise.
const (
	PhotoCacheVersioned = 365 * 24 * time.Hour
	PhotoCacheMaxAge    = 5 * time.Minute
	PhotoCacheStale     = 24 * time.Hour
)

// ManageHandler serves /v1/me/listings (the advertiser's side),
// /v1/admin/listings (the reviewers') and /v1/listing-photos.
type ManageHandler struct {
	mg     *Manager
	logger *slog.Logger
}

// NewManageHandler builds the handler.
func NewManageHandler(mg *Manager, logger *slog.Logger) *ManageHandler {
	return &ManageHandler{mg: mg, logger: logger}
}

func (h *ManageHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var incomplete *IncompleteError
	var state *StateError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &incomplete):
		fields := map[string]string{}
		for _, m := range incomplete.Missing {
			if _, ok := fields[m.Field]; !ok {
				fields[m.Field] = m.Message
			}
		}
		httpx.WriteError(w, r, http.StatusUnprocessableEntity, CodeListingIncomplete, "The listing needs more information before it can be submitted.", fields)
	case errors.As(err, &state):
		httpx.WriteError(w, r, http.StatusConflict, state.Code, state.Message, nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
	case errors.Is(err, ErrSelfReview):
		httpx.WriteError(w, r, http.StatusForbidden, CodeSelfReviewForbidden, "You cannot review a listing of your own organisation, or one you own, are the agent of or submitted. Another reviewer must decide it.", nil)
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, CodeListingChanged, "The listing was changed by someone else. Reload it and try again.", nil)
	case errors.Is(err, ErrPhotoLimit):
		httpx.WriteError(w, r, http.StatusConflict, CodePhotoLimitReached, PhotoLimitMessage, nil)
	case errors.Is(err, ErrPhotoNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodePhotoNotFound, "This photo does not exist.", nil)
	case errors.Is(err, ErrPhotoRequired):
		httpx.WriteError(w, r, http.StatusConflict, CodePhotoRequired, "A published listing must keep at least one photo.", nil)
	case errors.Is(err, ErrPhotoTooLarge), errors.As(err, &tooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, CodeFileTooLarge, "A photo must be at most 15 MB.", nil)
	case errors.Is(err, ErrPhotoUnsupported):
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, CodeUnsupportedFile, "A photo must be a JPEG, PNG or WebP image.", nil)
	case errors.Is(err, ErrPhotoDimensions):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", map[string]string{
			"file": fmt.Sprintf("must be at least %d pixels on its longer side, and at most %dx%d pixels (%d megapixels)",
				MinPhotoLongSide, MaxPhotoSide, MaxPhotoSide, MaxPhotoPixels/1_000_000)})
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func principalOf(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

type managedResponse struct {
	Listing ManagedView `json:"listing"`
}

type adminResponse struct {
	Listing AdminView `json:"listing"`
}

func (h *ManageHandler) writeManaged(w http.ResponseWriter, r *http.Request, status int, v ManagedView, err error) {
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, status, managedResponse{Listing: v})
}

// listingID reads {id}; a malformed one is the same 404 as an unknown one.
func listingID(r *http.Request, param string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	return id, nil
}

// withListing runs fn for the signed-in caller and the listing in the path.
func (h *ManageHandler) withListing(w http.ResponseWriter, r *http.Request, fn func(p *auth.Principal, id uuid.UUID) (ManagedView, error)) {
	p := principalOf(w, r)
	if p == nil {
		return
	}
	id, err := listingID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := fn(p, id)
	h.writeManaged(w, r, http.StatusOK, v, err)
}

// Create handles POST /v1/me/listings.
func (h *ManageHandler) Create(w http.ResponseWriter, r *http.Request) {
	p := principalOf(w, r)
	if p == nil {
		return
	}
	var body map[string]json.RawMessage
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	in := CreateInput{Fields: body}
	fields := map[string]string{}
	if raw, ok := body["organisation_id"]; ok {
		delete(body, "organisation_id")
		if !isNull(raw) {
			var id uuid.UUID
			if json.Unmarshal(raw, &id) != nil {
				fields["organisation_id"] = "must be an organisation ID"
			} else {
				in.OrganisationID = &id
			}
		}
	}
	if raw, ok := body["as_direct_owner"]; ok {
		delete(body, "as_direct_owner")
		if json.Unmarshal(raw, &in.AsDirectOwner) != nil {
			fields["as_direct_owner"] = "must be true or false"
		}
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	v, err := h.mg.Create(r.Context(), p, in)
	h.writeManaged(w, r, http.StatusCreated, v, err)
}

// Get handles GET /v1/me/listings/{id}.
func (h *ManageHandler) Get(w http.ResponseWriter, r *http.Request) {
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) { return h.mg.Get(r.Context(), p, id) })
}

// Patch handles PATCH /v1/me/listings/{id}.
func (h *ManageHandler) Patch(w http.ResponseWriter, r *http.Request) {
	var body map[string]json.RawMessage
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) {
		return h.mg.Update(r.Context(), p, id, body)
	})
}

// Submit handles POST /v1/me/listings/{id}/submit.
func (h *ManageHandler) Submit(w http.ResponseWriter, r *http.Request) {
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) { return h.mg.Submit(r.Context(), p, id) })
}

// Archive handles POST /v1/me/listings/{id}/archive.
func (h *ManageHandler) Archive(w http.ResponseWriter, r *http.Request) {
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) { return h.mg.Archive(r.Context(), p, id) })
}

// Relist handles POST /v1/me/listings/{id}/relist.
func (h *ManageHandler) Relist(w http.ResponseWriter, r *http.Request) {
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) { return h.mg.Relist(r.Context(), p, id) })
}

// Delete handles DELETE /v1/me/listings/{id}.
func (h *ManageHandler) Delete(w http.ResponseWriter, r *http.Request) {
	p := principalOf(w, r)
	if p == nil {
		return
	}
	id, err := listingID(r, "id")
	if err == nil {
		err = h.mg.Delete(r.Context(), p, id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// readPhoto reads the multipart form: exactly one "file" part. At most
// MaxPhotoBytes+1 bytes of it are read, so an oversized file is detected
// without being kept.
func readPhoto(r *http.Request) ([]byte, error) {
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
			return nil, ErrPhotoTooLarge
		}
		if err != nil {
			return nil, problem("the multipart body could not be read")
		}
		if parts >= 3 || (part.FormName() == "file" && seen) {
			return nil, problem("send exactly one file")
		}
		if part.FormName() == "file" {
			seen = true
			content, err = io.ReadAll(io.LimitReader(part, MaxPhotoBytes+1))
			if errors.As(err, &tooLarge) {
				return nil, ErrPhotoTooLarge
			}
			if err != nil {
				return nil, problem("the file could not be read")
			}
		}
		_ = part.Close()
	}
	if len(content) > MaxPhotoBytes {
		return nil, ErrPhotoTooLarge
	}
	return content, nil
}

// AddPhoto handles POST /v1/me/listings/{id}/photos (multipart, field file).
func (h *ManageHandler) AddPhoto(w http.ResponseWriter, r *http.Request) {
	p := principalOf(w, r)
	if p == nil {
		return
	}
	id, err := listingID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	content, err := readPhoto(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.mg.AddPhoto(r.Context(), p, id, content)
	h.writeManaged(w, r, http.StatusCreated, v, err)
}

// PatchPhoto handles PATCH /v1/me/listings/{id}/photos/{photoId}: {"alt": "..."}.
func (h *ManageHandler) PatchPhoto(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Alt *string `json:"alt"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) {
		photoID, err := uuid.Parse(chi.URLParam(r, "photoId"))
		if err != nil {
			return ManagedView{}, ErrPhotoNotFound
		}
		if body.Alt == nil {
			return ManagedView{}, &ValidationError{Fields: map[string]string{"alt": "is required"}}
		}
		return h.mg.SetPhotoAlt(r.Context(), p, id, photoID, *body.Alt)
	})
}

// OrderPhotos handles PUT /v1/me/listings/{id}/photos/order: {"photo_ids": [...]}.
func (h *ManageHandler) OrderPhotos(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PhotoIDs []uuid.UUID `json:"photo_ids"`
	}
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) {
		return h.mg.ReorderPhotos(r.Context(), p, id, body.PhotoIDs)
	})
}

// DeletePhoto handles DELETE /v1/me/listings/{id}/photos/{photoId}.
func (h *ManageHandler) DeletePhoto(w http.ResponseWriter, r *http.Request) {
	h.withListing(w, r, func(p *auth.Principal, id uuid.UUID) (ManagedView, error) {
		photoID, err := uuid.Parse(chi.URLParam(r, "photoId"))
		if err != nil {
			return ManagedView{}, ErrPhotoNotFound
		}
		return h.mg.DeletePhoto(r.Context(), p, id, photoID)
	})
}

// Photo handles GET /v1/listing-photos/{photoId}/{size}: one stored size of
// an uploaded photo. While the listing and the photo are public it is the
// same for everyone and cacheable (a year with the current ?v=); otherwise
// it is served only to the listing's managers and reviewers, never cached by
// anything shared, and everyone else gets 404.
func (h *ManageHandler) Photo(w http.ResponseWriter, r *http.Request) {
	photoID, err := uuid.Parse(chi.URLParam(r, "photoId"))
	size := chi.URLParam(r, "size")
	if err != nil || !slices.Contains(PhotoSizes(), size) {
		h.fail(w, r, ErrPhotoNotFound)
		return
	}
	file, err := h.mg.OpenPhoto(r.Context(), auth.FromContext(r.Context()), photoID, size, r.URL.Query().Get("v"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer file.Content.Close()
	head := w.Header()
	cache := "private, no-store"
	switch {
	case file.Public && file.Versioned:
		cache = fmt.Sprintf("public, max-age=%d, immutable", int(PhotoCacheVersioned.Seconds()))
	case file.Public:
		cache = fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d", int(PhotoCacheMaxAge.Seconds()), int(PhotoCacheStale.Seconds()))
	}
	head.Set("Cache-Control", cache)
	head.Set("ETag", file.ETag)
	if match := r.Header.Get("If-None-Match"); match != "" && noneMatch(match, file.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	head.Set("Content-Type", photoTypeJPEG)
	head.Set("Content-Length", strconv.FormatInt(file.Size, 10))
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	head.Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, file.Content); err != nil {
		h.logger.WarnContext(r.Context(), "photo download interrupted", slog.String("photo_id", photoID.String()))
	}
}

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

// ---------------------------------------------------------------------------
// Reviewers
// ---------------------------------------------------------------------------

// AdminList handles GET /v1/admin/listings.
func (h *ManageHandler) AdminList(w http.ResponseWriter, r *http.Request) {
	p := principalOf(w, r)
	if p == nil {
		return
	}
	v := r.URL.Query()
	fields := map[string]string{}
	f := ReviewFilter{Status: strings.ToUpper(strings.TrimSpace(v.Get("status"))), Kind: strings.ToUpper(strings.TrimSpace(v.Get("kind"))),
		Country: strings.ToUpper(strings.TrimSpace(v.Get("country"))), Q: strings.TrimSpace(v.Get("q"))}
	if f.Status == "" {
		f.Status = StatusPendingReview
	}
	if !slices.Contains(Statuses(), f.Status) && f.Status != RevisionEditing {
		fields["status"] = "must be one of " + strings.Join(Statuses(), ", ")
	}
	if f.Kind != "" && f.Kind != ReviewKindNew && f.Kind != ReviewKindEdit {
		fields["kind"] = "must be NEW or EDIT"
	}
	if f.Country != "" && !countryRe.MatchString(f.Country) {
		fields["country"] = "must be a two-letter country code such as ZW"
	}
	if len([]rune(f.Q)) > maxQueryLength {
		fields["q"] = "must be at most 100 characters"
	}
	f.Page, f.PageSize = ParsePaging(v, fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	page, err := h.mg.Queue(r.Context(), p, f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, page)
}

func (h *ManageHandler) writeAdmin(w http.ResponseWriter, r *http.Request, v AdminView, err error) {
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, adminResponse{Listing: v})
}

// AdminGet handles GET /v1/admin/listings/{id}.
func (h *ManageHandler) AdminGet(w http.ResponseWriter, r *http.Request) {
	p := principalOf(w, r)
	if p == nil {
		return
	}
	id, err := listingID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.mg.AdminGet(r.Context(), p, id)
	h.writeAdmin(w, r, v, err)
}

func (h *ManageHandler) decide(w http.ResponseWriter, r *http.Request, decision string) {
	p := principalOf(w, r)
	if p == nil {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 || decision != DecisionApprove {
		if !httpx.DecodeJSON(w, r, &body) {
			return
		}
	}
	id, err := listingID(r, "id")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	v, err := h.mg.Decide(r.Context(), p, id, decision, body.Reason)
	h.writeAdmin(w, r, v, err)
}

// Approve handles POST /v1/admin/listings/{id}/approve.
func (h *ManageHandler) Approve(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, DecisionApprove)
}

// RequestChanges handles POST /v1/admin/listings/{id}/request-changes {reason}.
func (h *ManageHandler) RequestChanges(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, DecisionRequestChanges)
}

// Reject handles POST /v1/admin/listings/{id}/reject {reason}.
func (h *ManageHandler) Reject(w http.ResponseWriter, r *http.Request) {
	h.decide(w, r, DecisionReject)
}
