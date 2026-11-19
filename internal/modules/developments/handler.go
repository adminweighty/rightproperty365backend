package developments

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

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/listings"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Error codes.
const (
	CodeNotFound          = "DEVELOPMENT_NOT_FOUND"
	CodeSlugTaken         = "DEVELOPMENT_SLUG_TAKEN"
	CodeLocked            = "DEVELOPMENT_LOCKED"
	CodeStateConflict     = "DEVELOPMENT_STATE_CONFLICT"
	CodeChanged           = "DEVELOPMENT_CHANGED"
	CodeIncomplete        = "DEVELOPMENT_INCOMPLETE"
	CodeNoProfile         = "NO_DEVELOPER_PROFILE"
	CodePhaseNotFound     = "PHASE_NOT_FOUND"
	CodeUnitTypeNotFound  = "UNIT_TYPE_NOT_FOUND"
	CodeListingNotFound   = "LISTING_NOT_FOUND"
	CodeListingLinked     = "LISTING_ALREADY_LINKED"
	CodeSelfReview        = "SELF_REVIEW_FORBIDDEN"
	maxPublicQueryRunes   = 100
	maxJSONBody           = 64 << 10
	organisationURLParam  = "id"
	developmentURLParam   = "developmentId"
	developmentMarketSlug = 80
)

// MaxPhotoBodyBytes is the body limit of a photo upload (the listing one).
const MaxPhotoBodyBytes = listings.MaxPhotoBodyBytes

// Handler serves the showcase's routes.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var incomplete *IncompleteError
	var state *StateError
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &incomplete):
		httpx.WriteError(w, r, http.StatusUnprocessableEntity, CodeIncomplete, "The development needs more information before it can be reviewed.", incomplete.Fields)
	case errors.As(err, &state):
		httpx.WriteError(w, r, http.StatusConflict, CodeStateConflict, state.Message, nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This development does not exist.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "You do not have permission to do this.", nil)
	case errors.Is(err, ErrSlugTaken):
		httpx.WriteError(w, r, http.StatusConflict, CodeSlugTaken, "Another development in this country already has this address.", map[string]string{"slug": "is taken in this country"})
	case errors.Is(err, ErrLocked):
		httpx.WriteError(w, r, http.StatusConflict, CodeLocked, "The development is waiting for review or published: withdraw it to change its details or photos.", nil)
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, CodeChanged, "The development was changed by someone else. Reload it and try again.", nil)
	case errors.Is(err, ErrNoProfile):
		httpx.WriteError(w, r, http.StatusConflict, CodeNoProfile, "Your organisation needs a developer profile in the directory before it can show developments.", nil)
	case errors.Is(err, ErrPhaseNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodePhaseNotFound, "This phase does not exist.", nil)
	case errors.Is(err, ErrUnitTypeNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeUnitTypeNotFound, "This unit type does not exist.", nil)
	case errors.Is(err, ErrListingNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeListingNotFound, "This listing does not exist.", nil)
	case errors.Is(err, ErrListingLinked):
		httpx.WriteError(w, r, http.StatusConflict, CodeListingLinked, "This listing is already shown in another development.", nil)
	case errors.Is(err, ErrSelfReview):
		httpx.WriteError(w, r, http.StatusForbidden, CodeSelfReview, "You cannot review a development of your own organisation, or one you created or submitted. Another reviewer must decide it.", nil)
	case errors.Is(err, listings.ErrPhotoLimit):
		httpx.WriteError(w, r, http.StatusConflict, listings.CodePhotoLimitReached, fmt.Sprintf("A development can have up to %d photos.", MaxPhotos), nil)
	case errors.Is(err, listings.ErrPhotoNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, listings.CodePhotoNotFound, "This photo does not exist.", nil)
	case errors.Is(err, listings.ErrPhotoTooLarge), errors.As(err, &tooLarge):
		httpx.WriteError(w, r, http.StatusRequestEntityTooLarge, listings.CodeFileTooLarge, "A photo must be at most 15 MB.", nil)
	case errors.Is(err, listings.ErrPhotoUnsupported):
		httpx.WriteError(w, r, http.StatusUnsupportedMediaType, listings.CodeUnsupportedFile, "A photo must be a JPEG, PNG or WebP image.", nil)
	case errors.Is(err, listings.ErrPhotoDimensions):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", map[string]string{
			"file": fmt.Sprintf("must be at least %d pixels on its longer side, and at most %dx%d pixels (%d megapixels)",
				listings.MinPhotoLongSide, listings.MaxPhotoSide, listings.MaxPhotoSide, listings.MaxPhotoPixels/1_000_000)})
	default:
		// The malware scanner's answers (FILE_INFECTED, SCANNER_UNAVAILABLE)
		// are storage.PublicError: WriteInternalError answers them as such.
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func principal(w http.ResponseWriter, r *http.Request) *auth.Principal {
	p := auth.FromContext(r.Context())
	if p == nil {
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	}
	return p
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

type oneResponse struct {
	Development Managed `json:"development"`
}

type listResponse struct {
	Items []Managed `json:"items"`
}

// ids reads {id} (the organisation) and, when want, {developmentId}. A
// malformed one is the same 404 as an unknown one.
func ids(r *http.Request, want bool) (uuid.UUID, uuid.UUID, error) {
	org, err := uuid.Parse(chi.URLParam(r, organisationURLParam))
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrNotFound
	}
	if !want {
		return org, uuid.Nil, nil
	}
	dev, err := uuid.Parse(chi.URLParam(r, developmentURLParam))
	if err != nil {
		return uuid.Nil, uuid.Nil, ErrNotFound
	}
	return org, dev, nil
}

func readBody(r *http.Request) ([]byte, error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, &ValidationError{Fields: map[string]string{"body": "must be application/json"}}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxJSONBody {
		return nil, &ValidationError{Fields: map[string]string{"body": "is too large"}}
	}
	return body, nil
}

// one runs a use case on one project and answers it.
func (h *Handler) one(w http.ResponseWriter, r *http.Request, status int, withBody bool,
	run func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error)) {
	p := principal(w, r)
	if p == nil {
		return
	}
	org, dev, err := ids(r, chi.URLParam(r, developmentURLParam) != "")
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// Whoever may not see the project learns nothing, whatever the body.
	if dev == uuid.Nil {
		err = member(p, org, false)
	} else {
		_, err = h.svc.load(r.Context(), p, org, dev, false)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var body []byte
	if withBody {
		if body, err = readBody(r); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	m, err := run(p, org, dev, body)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, status, oneResponse{Development: m})
}

func childID(r *http.Request, param string, notFound error) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r, param))
	if err != nil {
		return uuid.Nil, notFound
	}
	return id, nil
}

// AgentRoutes mounts /v1/agent/organisations/{id}/developments. change is
// the rate limit of the changes, upload the one of the photo uploads.
func (h *Handler) AgentRoutes(r chi.Router, change, upload func(http.Handler) http.Handler) {
	r.Get("/", h.list)
	r.With(change).Post("/", func(w http.ResponseWriter, r *http.Request) {
		h.one(w, r, http.StatusCreated, true, func(p *auth.Principal, org, _ uuid.UUID, body []byte) (Managed, error) {
			return h.svc.Create(r.Context(), p, org, body)
		})
	})
	r.Route("/{developmentId}", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, false, func(p *auth.Principal, org, dev uuid.UUID, _ []byte) (Managed, error) {
				return h.svc.Get(r.Context(), p, org, dev)
			})
		})
		r.With(change).Patch("/", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				return h.svc.Update(r.Context(), p, org, dev, body)
			})
		})
		r.With(change).Delete("/", h.delete)
		r.With(change).Post("/submit", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, false, func(p *auth.Principal, org, dev uuid.UUID, _ []byte) (Managed, error) {
				return h.svc.Submit(r.Context(), p, org, dev)
			})
		})
		r.With(change).Post("/withdraw", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, false, func(p *auth.Principal, org, dev uuid.UUID, _ []byte) (Managed, error) {
				return h.svc.Withdraw(r.Context(), p, org, dev)
			})
		})
		r.With(change).Post("/phases", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusCreated, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				return h.svc.AddPhase(r.Context(), p, org, dev, body)
			})
		})
		r.With(change).Patch("/phases/{phaseId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				phase, err := childID(r, "phaseId", ErrPhaseNotFound)
				if err != nil {
					return Managed{}, err
				}
				return h.svc.UpdatePhase(r.Context(), p, org, dev, phase, body)
			})
		})
		r.With(change).Delete("/phases/{phaseId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, false, func(p *auth.Principal, org, dev uuid.UUID, _ []byte) (Managed, error) {
				phase, err := childID(r, "phaseId", ErrPhaseNotFound)
				if err != nil {
					return Managed{}, err
				}
				return h.svc.DeletePhase(r.Context(), p, org, dev, phase)
			})
		})
		r.With(change).Post("/phases/{phaseId}/unit-types", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusCreated, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				phase, err := childID(r, "phaseId", ErrPhaseNotFound)
				if err != nil {
					return Managed{}, err
				}
				return h.svc.AddUnitType(r.Context(), p, org, dev, phase, body)
			})
		})
		r.With(change).Patch("/unit-types/{unitTypeId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				unit, err := childID(r, "unitTypeId", ErrUnitTypeNotFound)
				if err != nil {
					return Managed{}, err
				}
				return h.svc.UpdateUnitType(r.Context(), p, org, dev, unit, body)
			})
		})
		r.With(change).Delete("/unit-types/{unitTypeId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, false, func(p *auth.Principal, org, dev uuid.UUID, _ []byte) (Managed, error) {
				unit, err := childID(r, "unitTypeId", ErrUnitTypeNotFound)
				if err != nil {
					return Managed{}, err
				}
				return h.svc.DeleteUnitType(r.Context(), p, org, dev, unit)
			})
		})
		r.With(upload).Post("/photos", h.addPhoto)
		r.With(change).Put("/photos/order", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				in, err := decode(body)
				if err != nil {
					return Managed{}, err
				}
				var list []uuid.UUID
				if raw, ok := in["photo_ids"]; !ok || json.Unmarshal(raw, &list) != nil {
					return Managed{}, &ValidationError{Fields: map[string]string{"photo_ids": "must list every photo of the development exactly once"}}
				}
				return h.svc.ReorderPhotos(r.Context(), p, org, dev, list)
			})
		})
		r.With(change).Patch("/photos/{photoId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				photo, err := childID(r, "photoId", listings.ErrPhotoNotFound)
				if err != nil {
					return Managed{}, err
				}
				in, err := decode(body)
				if err != nil {
					return Managed{}, err
				}
				fields := map[string]string{}
				alt, ok := in.str("alt", fields)
				if !ok {
					fields["alt"] = "is required"
				}
				if len(fields) > 0 {
					return Managed{}, &ValidationError{Fields: fields}
				}
				return h.svc.SetPhotoAlt(r.Context(), p, org, dev, photo, alt)
			})
		})
		r.With(change).Delete("/photos/{photoId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, false, func(p *auth.Principal, org, dev uuid.UUID, _ []byte) (Managed, error) {
				photo, err := childID(r, "photoId", listings.ErrPhotoNotFound)
				if err != nil {
					return Managed{}, err
				}
				return h.svc.DeletePhoto(r.Context(), p, org, dev, photo)
			})
		})
		r.With(change).Put("/listings/{listingId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, true, func(p *auth.Principal, org, dev uuid.UUID, body []byte) (Managed, error) {
				listing, err := childID(r, "listingId", ErrListingNotFound)
				if err != nil {
					return Managed{}, err
				}
				in, err := decode(body)
				if err != nil {
					return Managed{}, err
				}
				fields := map[string]string{}
				in.keys([]string{"unit_type_id"}, fields)
				var unit *uuid.UUID
				if v, ok := in.str("unit_type_id", fields); ok && v != "" {
					id, err := uuid.Parse(v)
					if err != nil {
						return Managed{}, ErrUnitTypeNotFound
					}
					unit = &id
				}
				if len(fields) > 0 {
					return Managed{}, &ValidationError{Fields: fields}
				}
				return h.svc.LinkListing(r.Context(), p, org, dev, listing, unit)
			})
		})
		r.With(change).Delete("/listings/{listingId}", func(w http.ResponseWriter, r *http.Request) {
			h.one(w, r, http.StatusOK, false, func(p *auth.Principal, org, dev uuid.UUID, _ []byte) (Managed, error) {
				listing, err := childID(r, "listingId", ErrListingNotFound)
				if err != nil {
					return Managed{}, err
				}
				return h.svc.UnlinkListing(r.Context(), p, org, dev, listing)
			})
		})
		r.Get("/enquiries", h.enquiries)
	})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	org, _, err := ids(r, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.svc.List(r.Context(), p, org)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, listResponse{Items: items})
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	org, dev, err := ids(r, true)
	if err == nil {
		err = h.svc.Delete(r.Context(), p, org, dev)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type enquiriesResponse struct {
	Items any `json:"items"`
}

func (h *Handler) enquiries(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	org, dev, err := ids(r, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items, err := h.svc.Enquiries(r.Context(), p, org, dev)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, enquiriesResponse{Items: items})
}

// readPhoto reads the one "file" part of a multipart upload.
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
			return nil, listings.ErrPhotoTooLarge
		}
		if err != nil {
			return nil, problem("the multipart body could not be read")
		}
		if parts >= 3 || (part.FormName() == "file" && seen) {
			return nil, problem("send exactly one file")
		}
		if part.FormName() == "file" {
			seen = true
			content, err = io.ReadAll(io.LimitReader(part, listings.MaxPhotoBytes+1))
			if errors.As(err, &tooLarge) || len(content) > listings.MaxPhotoBytes {
				return nil, listings.ErrPhotoTooLarge
			}
			if err != nil {
				return nil, problem("the file could not be read")
			}
		}
	}
	if !seen {
		return nil, problem("is required")
	}
	return content, nil
}

func (h *Handler) addPhoto(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	org, dev, err := ids(r, true)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	// The caller may change the project before the upload is read.
	if _, err := h.svc.load(r.Context(), p, org, dev, true); err != nil {
		h.fail(w, r, err)
		return
	}
	content, err := readPhoto(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	m, err := h.svc.AddPhoto(r.Context(), p, org, dev, content)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusCreated, oneResponse{Development: m})
}

// ---------------------------------------------------------------------------
// Public
// ---------------------------------------------------------------------------

// List handles GET /v1/developments.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	fields := map[string]string{}
	page, size := listings.ParsePaging(v, fields)
	q := Query{Market: strings.ToUpper(strings.TrimSpace(v.Get("market"))), City: strings.TrimSpace(v.Get("city")),
		Status: strings.ToUpper(strings.TrimSpace(v.Get("status"))), Q: strings.TrimSpace(v.Get("q")), Page: page, Size: size}
	if len([]rune(q.Q)) > maxPublicQueryRunes {
		fields["q"] = "must be at most 100 characters"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.PublicList(r.Context(), q)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, out, listings.CacheMaxAge, listings.CacheStale)
}

type detailResponse struct {
	Development Detail `json:"development"`
}

// Get handles GET /v1/developments/{market}/{slug}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	market, slug := chi.URLParam(r, "market"), chi.URLParam(r, "slug")
	if len(market) != 2 || len(slug) > developmentMarketSlug || !slugRe.MatchString(slug) {
		h.fail(w, r, ErrNotFound)
		return
	}
	out, err := h.svc.PublicGet(r.Context(), market, slug)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	httpx.WriteCacheableJSON(w, r, detailResponse{Development: out}, listings.CacheMaxAge, listings.CacheStale)
}

// Photo handles GET /v1/development-photos/{photoId}/{size}.
func (h *Handler) Photo(w http.ResponseWriter, r *http.Request) {
	photoID, err := uuid.Parse(chi.URLParam(r, "photoId"))
	size := chi.URLParam(r, "size")
	if err != nil || !slices.Contains(listings.PhotoSizes(), size) {
		h.fail(w, r, listings.ErrPhotoNotFound)
		return
	}
	file, err := h.svc.OpenPhoto(r.Context(), auth.FromContext(r.Context()), photoID, size, r.URL.Query().Get("v"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	defer file.Content.Close()
	head := w.Header()
	cache := "private, no-store"
	switch {
	case file.Public && file.Versioned:
		cache = fmt.Sprintf("public, max-age=%d, immutable", int(listings.PhotoCacheVersioned.Seconds()))
	case file.Public:
		cache = fmt.Sprintf("public, max-age=%d, stale-while-revalidate=%d", int(listings.PhotoCacheMaxAge.Seconds()), int(listings.PhotoCacheStale.Seconds()))
	}
	head.Set("Cache-Control", cache)
	head.Set("ETag", file.ETag)
	if match := r.Header.Get("If-None-Match"); match != "" && noneMatch(match, file.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	head.Set("Content-Type", "image/jpeg")
	head.Set("Content-Length", strconv.FormatInt(file.Size, 10))
	head.Set("X-Content-Type-Options", "nosniff")
	head.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	head.Set("Cross-Origin-Resource-Policy", "cross-origin")
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, file.Content); err != nil {
		h.logger.WarnContext(r.Context(), "photo download interrupted", slog.String("photo_id", photoID.String()))
	}
}

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
// Administrators
// ---------------------------------------------------------------------------

// AdminRoutes mounts /v1/admin/developments (behind the review roles).
func (h *Handler) AdminRoutes(r chi.Router, change func(http.Handler) http.Handler) {
	r.Get("/", h.queue)
	r.Get("/{developmentId}", func(w http.ResponseWriter, r *http.Request) {
		h.decide(w, r, func(p *auth.Principal, id uuid.UUID) (Managed, error) { return h.svc.AdminGet(r.Context(), p, id) })
	})
	r.With(change).Post("/{developmentId}/approve", func(w http.ResponseWriter, r *http.Request) {
		h.decide(w, r, func(p *auth.Principal, id uuid.UUID) (Managed, error) {
			return h.svc.Decide(r.Context(), p, id, DecisionApprove, "")
		})
	})
	r.With(change).Post("/{developmentId}/reject", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Reason string `json:"reason"`
		}
		if !httpx.DecodeJSON(w, r, &body) {
			return
		}
		h.decide(w, r, func(p *auth.Principal, id uuid.UUID) (Managed, error) {
			return h.svc.Decide(r.Context(), p, id, DecisionReject, body.Reason)
		})
	})
}

func (h *Handler) decide(w http.ResponseWriter, r *http.Request, run func(p *auth.Principal, id uuid.UUID) (Managed, error)) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, developmentURLParam))
	if err != nil {
		h.fail(w, r, ErrNotFound)
		return
	}
	m, err := run(p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, oneResponse{Development: m})
}

func (h *Handler) queue(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	v := r.URL.Query()
	fields := map[string]string{}
	page, size := listings.ParsePaging(v, fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	out, err := h.svc.Queue(r.Context(), p, strings.ToUpper(strings.TrimSpace(v.Get("status"))), strings.TrimSpace(v.Get("market")), page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}
