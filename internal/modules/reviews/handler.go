package reviews

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
	"rightproperty365.com/backend/internal/platform/logging"
)

// Error codes.
const (
	CodeBookNotFound      = "BOOK_NOT_FOUND"
	CodeNotFound          = "REVIEW_NOT_FOUND"
	CodeProfileNotFound   = "PROFILE_NOT_FOUND"
	CodeNotEligible       = "REVIEW_NOT_ELIGIBLE"
	CodeExists            = "REVIEW_EXISTS"
	CodeLocked            = "REVIEW_LOCKED"
	CodeInvalidTransition = "REVIEW_INVALID_TRANSITION"
	CodeAlreadyReported   = "REVIEW_ALREADY_REPORTED"
	CodeReplyExists       = "REVIEW_REPLY_EXISTS"
	CodeReplyLocked       = "REVIEW_REPLY_LOCKED"
	CodeReplyNotFound     = "REVIEW_REPLY_NOT_FOUND"
	CodeChanged           = "REVIEW_CHANGED"

	DefaultPageSize, MaxPageSize             = 25, 100
	DefaultPublicPageSize, MaxPublicPageSize = 10, 50
	// PublicCacheMaxAge / PublicCacheStale: the public list is cacheable
	// (moderation shows within a minute).
	PublicCacheMaxAge = 60 * time.Second
	PublicCacheStale  = 5 * time.Minute
)

// Handler serves /v1/me/reviews, /v1/agent/organisations/{id}/reviews,
// /v1/directory/{kind}/{slug}/reviews, /v1/reviews/{id}/report and
// /v1/admin/reviews.
type Handler struct {
	svc    *Service
	logger *slog.Logger
}

// NewHandler builds the handler.
func NewHandler(svc *Service, logger *slog.Logger) *Handler {
	return &Handler{svc: svc, logger: logger}
}

type detailBody struct {
	Error struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Details map[string]any `json:"details"`
	} `json:"error"`
	TraceID string `json:"trace_id"`
}

func writeDetails(w http.ResponseWriter, r *http.Request, status int, code, message string, details map[string]any) {
	var b detailBody
	b.Error.Code, b.Error.Message, b.Error.Details, b.TraceID = code, message, details, logging.RequestID(r.Context())
	httpx.WriteJSON(w, status, b)
}

var reasonMessages = map[string]string{
	ReasonBasisNotFound: "This job or enquiry does not exist or is not yours.",
	ReasonWrongProvider: "This job or enquiry was not with this provider.",
	ReasonNotCompleted:  "The job must be resolved or closed before it can be reviewed.",
	ReasonTooRecent:     "An enquiry can be reviewed 14 days after it was sent.",
	ReasonNotReplied:    "The provider has not replied to this enquiry.",
}

func private(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
}

func (h *Handler) ok(w http.ResponseWriter, status int, v any) {
	private(w)
	httpx.WriteJSON(w, status, v)
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	private(w)
	var verr *ValidationError
	var inel *IneligibleError
	var state *StateError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &inel):
		details := map[string]any{"reason": inel.Reason, "eligible_from": nil}
		if inel.EligibleFrom != nil {
			details["eligible_from"] = inel.EligibleFrom.UTC()
		}
		writeDetails(w, r, http.StatusConflict, CodeNotEligible, reasonMessages[inel.Reason], details)
	case errors.As(err, &state):
		details := map[string]any{"status": state.Status}
		msg := "This review cannot change now."
		switch state.Code {
		case CodeLocked:
			msg = "A review can only be edited until it is moderated."
		case CodeInvalidTransition:
			msg = "This action is not possible for a review that is " + strings.ToLower(StatusLabel(state.Status)) + "."
		case CodeReplyLocked:
			msg = "The reply can no longer be changed."
			if state.Until != nil {
				details["editable_until"] = state.Until.UTC()
			}
		}
		writeDetails(w, r, http.StatusConflict, state.Code, msg, details)
	case errors.Is(err, ErrUnauthenticated):
		httpx.WriteError(w, r, http.StatusUnauthorized, httpx.CodeUnauthenticated, "You need to sign in to do this.", nil)
	case errors.Is(err, ErrBookNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeBookNotFound, "These books do not exist or are not yours.", nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeNotFound, "This review does not exist or is not yours.", nil)
	case errors.Is(err, ErrProfileNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeProfileNotFound, "This profile does not exist.", nil)
	case errors.Is(err, ErrForbidden):
		httpx.WriteError(w, r, http.StatusForbidden, httpx.CodeForbidden, "Your role does not allow this.", nil)
	case errors.Is(err, ErrBasisReviewed):
		httpx.WriteError(w, r, http.StatusConflict, CodeExists, "This job or enquiry has already been reviewed.", nil)
	case errors.Is(err, ErrAlreadyReported):
		httpx.WriteError(w, r, http.StatusConflict, CodeAlreadyReported, "You have already reported this review.", nil)
	case errors.Is(err, ErrReplyExists):
		httpx.WriteError(w, r, http.StatusConflict, CodeReplyExists, "This review already has your reply: edit it instead.", nil)
	case errors.Is(err, ErrNoReply):
		httpx.WriteError(w, r, http.StatusNotFound, CodeReplyNotFound, "This review has no reply.", nil)
	case errors.Is(err, ErrConflict):
		httpx.WriteError(w, r, http.StatusConflict, CodeChanged, "The review changed meanwhile. Reload it and try again.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

func paging(q url.Values, def, maxSize int, f map[string]string) (int, int) {
	page, size := 1, def
	if raw := q.Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			f["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := q.Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > maxSize {
			f["page_size"] = "must be a whole number from 1 to " + strconv.Itoa(maxSize)
		} else {
			size = n
		}
	}
	return page, size
}

func (h *Handler) principal(w http.ResponseWriter, r *http.Request) (*auth.Principal, bool) {
	p := auth.FromContext(r.Context())
	if p == nil {
		h.fail(w, r, ErrUnauthenticated)
		return nil, false
	}
	return p, true
}

func (h *Handler) reviewID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		h.fail(w, r, ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// book resolves the caller and the book of a book's route (404 first).
func (h *Handler) book(w http.ResponseWriter, r *http.Request) (*auth.Principal, uuid.UUID, bool) {
	p, ok := h.principal(w, r)
	if !ok {
		return nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err == nil {
		err = h.svc.CheckBook(p, id)
	} else {
		err = ErrBookNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return nil, uuid.Nil, false
	}
	return p, id, true
}

// provider resolves the caller and the provider organisation (404 first).
func (h *Handler) provider(w http.ResponseWriter, r *http.Request) (*auth.Principal, uuid.UUID, bool) {
	p, ok := h.principal(w, r)
	if !ok {
		return nil, uuid.Nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err == nil {
		err = h.svc.CheckProvider(p, id)
	} else {
		err = ErrBookNotFound
	}
	if err != nil {
		h.fail(w, r, err)
		return nil, uuid.Nil, false
	}
	return p, id, true
}

func lookupOf(q url.Values) (ProfileLookup, error) {
	var l ProfileLookup
	if raw := strings.TrimSpace(q.Get("directory_profile_id")); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			return l, &ValidationError{Fields: map[string]string{"directory_profile_id": "must be a profile id"}}
		}
		l.ID = &id
		return l, nil
	}
	l.Kind, l.Slug = strings.TrimSpace(q.Get("kind")), strings.TrimSpace(q.Get("slug"))
	if l.Kind == "" || l.Slug == "" {
		return l, &ValidationError{Fields: map[string]string{"directory_profile_id": "send directory_profile_id, or kind and slug"}}
	}
	return l, nil
}

type itemsResponse[T any] struct {
	Items []T `json:"items"`
}

type reviewResponse[T any] struct {
	Review T `json:"review"`
}

func myViews(list []Review, write bool) []MyReview {
	out := make([]MyReview, 0, len(list))
	for _, rv := range list {
		out = append(out, MyView(rv, write))
	}
	return out
}

// MyRoutes mounts /v1/me/reviews.
func (h *Handler) MyRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.Mine)
	r.With(limit).Post("/", h.CreateMine)
	r.Get("/eligibility", h.MyEligibility)
	r.Get("/{reviewId}", h.MyReview)
	r.With(limit).Patch("/{reviewId}", h.EditMine)
	r.With(limit).Delete("/{reviewId}", h.DeleteMine)
}

// BookRoutes mounts /v1/agent/organisations/{id}/reviews: the reviews the
// book wrote, and (under /received and /{reviewId}/reply) the reviews of
// the organisation's own profiles.
func (h *Handler) BookRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.BookList)
	r.With(limit).Post("/", h.BookCreate)
	r.Get("/eligibility", h.BookEligibility)
	r.Get("/received", h.Received)
	r.Get("/{reviewId}", h.BookGet)
	r.With(limit).Patch("/{reviewId}", h.BookEdit)
	r.With(limit).Delete("/{reviewId}", h.BookDelete)
	r.With(limit).Post("/{reviewId}/reply", h.Reply)
	r.With(limit).Patch("/{reviewId}/reply", h.EditReply)
}

// AdminRoutes mounts /v1/admin/reviews (the router checks the roles).
func (h *Handler) AdminRoutes(r chi.Router, limit func(http.Handler) http.Handler) {
	r.Get("/", h.AdminList)
	r.Get("/{reviewId}", h.AdminGet)
	r.With(limit).Post("/{reviewId}/publish", h.moderate(ActionPublish))
	r.With(limit).Post("/{reviewId}/hide", h.moderate(ActionHide))
	r.With(limit).Post("/{reviewId}/reject", h.moderate(ActionReject))
	r.With(limit).Post("/{reviewId}/remove-reply", h.moderate(ActionRemoveReply))
}

// Mine handles GET /v1/me/reviews.
func (h *Handler) Mine(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	list, err := h.svc.Mine(r.Context(), p)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, itemsResponse[MyReview]{Items: myViews(list, true)})
}

// CreateMine handles POST /v1/me/reviews.
func (h *Handler) CreateMine(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	rv, err := h.svc.CreateMine(r.Context(), p, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, reviewResponse[MyReview]{Review: MyView(rv, true)})
}

// MyEligibility handles GET /v1/me/reviews/eligibility.
func (h *Handler) MyEligibility(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	l, err := lookupOf(r.URL.Query())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.MyEligibility(r.Context(), p, l)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// MyReview handles GET /v1/me/reviews/{reviewId}.
func (h *Handler) MyReview(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	rv, err := h.svc.MyReview(r.Context(), p, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, reviewResponse[MyReview]{Review: MyView(rv, true)})
}

// EditMine handles PATCH /v1/me/reviews/{reviewId}.
func (h *Handler) EditMine(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	var in Patch
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	rv, err := h.svc.EditMine(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, reviewResponse[MyReview]{Review: MyView(rv, true)})
}

// DeleteMine handles DELETE /v1/me/reviews/{reviewId}.
func (h *Handler) DeleteMine(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	if err := h.svc.DeleteMine(r.Context(), p, id); err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	w.WriteHeader(http.StatusNoContent)
}

type bookListResponse struct {
	Items       []MyReview `json:"items"`
	Permissions struct {
		Write bool `json:"write"`
	} `json:"permissions"`
}

// BookList handles GET /v1/agent/organisations/{id}/reviews.
func (h *Handler) BookList(w http.ResponseWriter, r *http.Request) {
	p, book, ok := h.book(w, r)
	if !ok {
		return
	}
	list, write, err := h.svc.BookReviews(r.Context(), p, book)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := bookListResponse{Items: myViews(list, write)}
	out.Permissions.Write = write
	h.ok(w, http.StatusOK, out)
}

// BookCreate handles POST /v1/agent/organisations/{id}/reviews.
func (h *Handler) BookCreate(w http.ResponseWriter, r *http.Request) {
	p, book, ok := h.book(w, r)
	if !ok {
		return
	}
	var in Input
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	rv, err := h.svc.CreateForBook(r.Context(), p, book, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusCreated, reviewResponse[MyReview]{Review: MyView(rv, true)})
}

// BookEligibility handles GET /v1/agent/organisations/{id}/reviews/eligibility.
func (h *Handler) BookEligibility(w http.ResponseWriter, r *http.Request) {
	p, book, ok := h.book(w, r)
	if !ok {
		return
	}
	l, err := lookupOf(r.URL.Query())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, err := h.svc.BookEligibility(r.Context(), p, book, l)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, out)
}

// BookGet handles GET /v1/agent/organisations/{id}/reviews/{reviewId}.
func (h *Handler) BookGet(w http.ResponseWriter, r *http.Request) {
	p, book, ok := h.book(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	rv, write, err := h.svc.BookReview(r.Context(), p, book, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, reviewResponse[MyReview]{Review: MyView(rv, write)})
}

// BookEdit handles PATCH /v1/agent/organisations/{id}/reviews/{reviewId}.
func (h *Handler) BookEdit(w http.ResponseWriter, r *http.Request) {
	p, book, ok := h.book(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	var in Patch
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	rv, err := h.svc.EditForBook(r.Context(), p, book, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, reviewResponse[MyReview]{Review: MyView(rv, true)})
}

// BookDelete handles DELETE /v1/agent/organisations/{id}/reviews/{reviewId}.
func (h *Handler) BookDelete(w http.ResponseWriter, r *http.Request) {
	p, book, ok := h.book(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	if err := h.svc.DeleteForBook(r.Context(), p, book, id); err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	w.WriteHeader(http.StatusNoContent)
}

type receivedResponse struct {
	Items       []ReceivedReview `json:"items"`
	Total       int              `json:"total"`
	Page        int              `json:"page"`
	PageSize    int              `json:"page_size"`
	Permissions struct {
		Reply bool `json:"reply"`
	} `json:"permissions"`
}

// Received handles GET /v1/agent/organisations/{id}/reviews/received.
func (h *Handler) Received(w http.ResponseWriter, r *http.Request) {
	p, org, ok := h.provider(w, r)
	if !ok {
		return
	}
	f := map[string]string{}
	page, size := paging(r.URL.Query(), DefaultPageSize, MaxPageSize, f)
	if len(f) > 0 {
		h.fail(w, r, &ValidationError{Fields: f})
		return
	}
	list, total, mayReply, err := h.svc.Received(r.Context(), p, org, page, size)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	now := h.svc.clock()
	out := receivedResponse{Items: make([]ReceivedReview, 0, len(list)), Total: total, Page: page, PageSize: size}
	for _, rv := range list {
		out.Items = append(out.Items, ReceivedView(rv, mayReply, now))
	}
	out.Permissions.Reply = mayReply
	h.ok(w, http.StatusOK, out)
}

type replyInput struct {
	Text string `json:"text"`
}

func (h *Handler) reply(w http.ResponseWriter, r *http.Request, edit bool) {
	p, org, ok := h.provider(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	var in replyInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	var rv Review
	var err error
	status := http.StatusCreated
	if edit {
		rv, err = h.svc.EditReply(r.Context(), p, org, id, in.Text)
		status = http.StatusOK
	} else {
		rv, err = h.svc.Reply(r.Context(), p, org, id, in.Text)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, status, reviewResponse[ReceivedReview]{Review: ReceivedView(rv, true, h.svc.clock())})
}

// Reply handles POST /v1/agent/organisations/{id}/reviews/{reviewId}/reply.
func (h *Handler) Reply(w http.ResponseWriter, r *http.Request) { h.reply(w, r, false) }

// EditReply handles PATCH /v1/agent/organisations/{id}/reviews/{reviewId}/reply.
func (h *Handler) EditReply(w http.ResponseWriter, r *http.Request) { h.reply(w, r, true) }

type publicResponse struct {
	Profile struct {
		Kind string `json:"kind"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	} `json:"profile"`
	Rating   RatingView     `json:"rating"`
	Items    []PublicReview `json:"items"`
	Total    int            `json:"total"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
}

// Public handles GET /v1/directory/{kind}/{slug}/reviews (public, cacheable).
func (h *Handler) Public(w http.ResponseWriter, r *http.Request) {
	f := map[string]string{}
	page, size := paging(r.URL.Query(), DefaultPublicPageSize, MaxPublicPageSize, f)
	if len(f) > 0 {
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", f)
		return
	}
	prof, list, total, rating, err := h.svc.Public(r.Context(), chi.URLParam(r, "kind"), chi.URLParam(r, "slug"), page, size)
	if errors.Is(err, ErrProfileNotFound) {
		httpx.WriteError(w, r, http.StatusNotFound, CodeProfileNotFound, "This profile does not exist.", nil)
		return
	}
	if err != nil {
		httpx.WriteInternalError(w, r, h.logger, err)
		return
	}
	out := publicResponse{Rating: RatingOf(rating), Items: make([]PublicReview, 0, len(list)), Total: total, Page: page, PageSize: size}
	out.Profile.Kind, out.Profile.Slug, out.Profile.Name = prof.Kind, prof.Slug, prof.Name
	for _, rv := range list {
		out.Items = append(out.Items, PublicView(rv))
	}
	httpx.WriteCacheableJSON(w, r, out, PublicCacheMaxAge, PublicCacheStale)
}

type reportResponse struct {
	Report struct {
		ID        uuid.UUID `json:"id"`
		Status    string    `json:"status"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"report"`
}

// Report handles POST /v1/reviews/{id}/report.
func (h *Handler) Report(w http.ResponseWriter, r *http.Request) {
	p, ok := h.principal(w, r)
	if !ok {
		return
	}
	id, ok := h.reviewID(w, r, "id")
	if !ok {
		return
	}
	var in ReportInput
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	rp, err := h.svc.ReportReview(r.Context(), p, id, in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	var out reportResponse
	out.Report.ID, out.Report.Status, out.Report.CreatedAt = rp.ID, rp.Status, rp.CreatedAt
	h.ok(w, http.StatusCreated, out)
}

type adminListResponse struct {
	Items    []AdminReview `json:"items"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Counts   QueueCounts   `json:"counts"`
}

// AdminList handles GET /v1/admin/reviews.
func (h *Handler) AdminList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fields := map[string]string{}
	var af AdminFilter
	af.Page, af.PageSize = paging(q, DefaultPageSize, MaxPageSize, fields)
	switch af.View = q.Get("view"); af.View {
	case "", "queue", "all":
	default:
		fields["view"] = "must be queue or all"
	}
	if af.Status = strings.ToUpper(q.Get("status")); af.Status != "" && StatusLabel(af.Status) == af.Status {
		fields["status"] = "must be PENDING, PUBLISHED, HIDDEN or REJECTED"
	}
	switch raw := q.Get("reported"); raw {
	case "", "false":
	case "true":
		af.Reported = true
	default:
		fields["reported"] = "must be true or false"
	}
	if af.BasisKind = strings.ToUpper(q.Get("basis_kind")); af.BasisKind != "" && af.BasisKind != BasisMaintenance && af.BasisKind != BasisEnquiry {
		fields["basis_kind"] = "must be MAINTENANCE or ENQUIRY"
	}
	if raw := q.Get("rating"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < MinRating || n > MaxRating {
			fields["rating"] = "must be a whole number from 1 to 5"
		} else {
			af.Rating = n
		}
	}
	if raw := q.Get("profile_id"); raw != "" {
		if id, err := uuid.Parse(raw); err != nil {
			fields["profile_id"] = "must be a profile id"
		} else {
			af.ProfileID = &id
		}
	}
	if af.Q = strings.TrimSpace(q.Get("q")); len([]rune(af.Q)) > 100 {
		fields["q"] = "must be at most 100 characters"
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	list, total, counts, err := h.svc.AdminList(r.Context(), af)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := adminListResponse{Items: make([]AdminReview, 0, len(list)), Total: total, Page: af.Page, PageSize: af.PageSize, Counts: counts}
	for _, rv := range list {
		out.Items = append(out.Items, AdminView(rv, nil))
	}
	h.ok(w, http.StatusOK, out)
}

// AdminGet handles GET /v1/admin/reviews/{reviewId}.
func (h *Handler) AdminGet(w http.ResponseWriter, r *http.Request) {
	id, ok := h.reviewID(w, r, "reviewId")
	if !ok {
		return
	}
	rv, reports, err := h.svc.AdminGet(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.ok(w, http.StatusOK, reviewResponse[AdminReview]{Review: AdminView(rv, reports)})
}

// moderate handles POST /v1/admin/reviews/{reviewId}/publish, /hide,
// /reject and /remove-reply.
func (h *Handler) moderate(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := h.principal(w, r)
		if !ok {
			return
		}
		id, ok := h.reviewID(w, r, "reviewId")
		if !ok {
			return
		}
		var in ModerationInput
		if !httpx.DecodeJSON(w, r, &in) {
			return
		}
		rv, reports, err := h.svc.Moderate(r.Context(), p, id, action, in)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.ok(w, http.StatusOK, reviewResponse[AdminReview]{Review: AdminView(rv, reports)})
	}
}
