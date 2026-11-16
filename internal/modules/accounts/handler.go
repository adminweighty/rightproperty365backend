package accounts

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Handler serves /v1/admin/users/*, /v1/admin/audit and DELETE /v1/me.
type Handler struct {
	svc          *Service
	logger       *slog.Logger
	secureCookie bool
}

// NewHandler builds the handler. secureCookie is the session cookie's Secure
// flag (cleared after a self-service deletion).
func NewHandler(svc *Service, logger *slog.Logger, secureCookie bool) *Handler {
	return &Handler{svc: svc, logger: logger, secureCookie: secureCookie}
}

func private(w http.ResponseWriter) { w.Header().Set("Cache-Control", "private, no-store") }

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	var sole *SoleOwnerError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.As(err, &sole):
		httpx.WriteError(w, r, http.StatusConflict, CodeSoleOwner,
			fmt.Sprintf("This person is the only owner of %s, which still has other members. Make another member an owner, or remove the members, first.",
				strings.Join(sole.Organisations, ", ")), nil)
	case errors.Is(err, ErrNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeUserNotFound, "This account does not exist.", nil)
	case errors.Is(err, ErrState):
		httpx.WriteError(w, r, http.StatusConflict, CodeUserState, "The account's status does not allow this. Reload it and try again.", nil)
	case errors.Is(err, ErrSelf):
		httpx.WriteError(w, r, http.StatusForbidden, CodeSelfActionForbidden, "You cannot suspend or delete your own account.", nil)
	case errors.Is(err, ErrAdminProtected):
		httpx.WriteError(w, r, http.StatusForbidden, CodeAdminAccountProtected, "Only a super administrator can suspend or delete an administrator account.", nil)
	case errors.Is(err, ErrInvalidPassword):
		httpx.WriteError(w, r, http.StatusUnauthorized, CodeInvalidPassword, "The password is incorrect.", nil)
	case errors.Is(err, ErrAdminSelfDelete):
		httpx.WriteError(w, r, http.StatusForbidden, CodeAdminSelfDelete, "An administrator account cannot be deleted from here.", nil)
	default:
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

func paging(r *http.Request, fields map[string]string) (page, size int) {
	page, size = 1, DefaultPageSize
	if raw := r.URL.Query().Get("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > 100000 {
			fields["page"] = "must be a whole number from 1"
		} else {
			page = n
		}
	}
	if raw := r.URL.Query().Get("page_size"); raw != "" {
		if n, err := strconv.Atoi(raw); err != nil || n < 1 || n > MaxPageSize {
			fields["page_size"] = fmt.Sprintf("must be a whole number between 1 and %d", MaxPageSize)
		} else {
			size = n
		}
	}
	return page, size
}

// ---------------------------------------------------------------------------
// Views
// ---------------------------------------------------------------------------

// SummaryView is one row of GET /v1/admin/users.
type SummaryView struct {
	ID            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	Email         string     `json:"email"`
	Status        string     `json:"status"`
	EmailVerified bool       `json:"email_verified"`
	CreatedAt     time.Time  `json:"created_at"`
	LastSignInAt  *time.Time `json:"last_sign_in_at"`
	AccountRoles  []string   `json:"account_roles"`
	PlatformRoles []string   `json:"platform_roles"`
	MarketCodes   []string   `json:"market_codes"`
}

type pageView struct {
	Items    []SummaryView `json:"items"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
	Total    int           `json:"total"`
}

type platformRoleView struct {
	Role       string  `json:"role"`
	MarketCode *string `json:"market_code"`
}

type accountRoleView struct {
	Role             string  `json:"role"`
	Status           string  `json:"status"`
	OnboardingStatus *string `json:"onboarding_status"`
}

type membershipView struct {
	OrganisationID   uuid.UUID `json:"organisation_id"`
	OrganisationName string    `json:"organisation_name"`
	OrganisationKind string    `json:"organisation_kind"`
	MarketCode       string    `json:"market_code"`
	Role             string    `json:"role"`
	RoleLabel        string    `json:"role_label"`
	Status           string    `json:"status"`
}

// AuditView is one record of the administrators' audit trail.
type AuditView struct {
	ID          uuid.UUID      `json:"id"`
	ActorUserID *uuid.UUID     `json:"actor_user_id"`
	ActorName   string         `json:"actor_name"`
	Action      string         `json:"action"`
	TargetType  string         `json:"target_type"`
	TargetID    string         `json:"target_id"`
	TargetLabel string         `json:"target_label"`
	Reason      string         `json:"reason"`
	Before      map[string]any `json:"before"`
	After       map[string]any `json:"after"`
	At          time.Time      `json:"at"`
}

// AuditViewOf builds the view of an audit record.
func AuditViewOf(e adminaudit.Event) AuditView {
	return AuditView{ID: e.ID, ActorUserID: e.ActorUserID, ActorName: e.ActorName, Action: e.Action, TargetType: e.TargetType,
		TargetID: e.TargetID, TargetLabel: e.TargetLabel, Reason: e.Reason, Before: e.Before, After: e.After, At: e.At.UTC()}
}

type userDetailView struct {
	ID               uuid.UUID          `json:"id"`
	Name             string             `json:"name"`
	Email            string             `json:"email"`
	Status           string             `json:"status"`
	EmailVerified    bool               `json:"email_verified"`
	CreatedAt        time.Time          `json:"created_at"`
	LastSignInAt     *time.Time         `json:"last_sign_in_at"`
	SuspendedAt      *time.Time         `json:"suspended_at"`
	SuspensionReason *string            `json:"suspension_reason"`
	DeletedAt        *time.Time         `json:"deleted_at"`
	PlatformRoles    []platformRoleView `json:"platform_roles"`
	AccountRoles     []accountRoleView  `json:"account_roles"`
	Memberships      []membershipView   `json:"memberships"`
	ListingCounts    map[string]int     `json:"listing_counts"`
	ListingsTotal    int                `json:"listings_total"`
}

type detailResponse struct {
	User          userDetailView     `json:"user"`
	Subscriptions []SubscriptionInfo `json:"subscriptions"`
	Audit         []AuditView        `json:"audit"`
}

func summaryView(x Summary) SummaryView {
	return SummaryView{ID: x.ID, Name: x.Name, Email: x.Email, Status: x.Status, EmailVerified: x.EmailVerified, CreatedAt: x.CreatedAt.UTC(),
		LastSignInAt: x.LastSignInAt, AccountRoles: nonNil(x.AccountRoles), PlatformRoles: nonNil(x.PlatformRoles), MarketCodes: nonNil(x.MarketCodes)}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func detailView(d Detail) detailResponse {
	a := d.Account
	v := userDetailView{ID: a.ID, Name: a.Name, Email: a.Email, Status: a.Status, EmailVerified: a.EmailVerified, CreatedAt: a.CreatedAt.UTC(),
		LastSignInAt: a.LastSignInAt, SuspendedAt: a.SuspendedAt, DeletedAt: a.DeletedAt,
		PlatformRoles: []platformRoleView{}, AccountRoles: []accountRoleView{}, Memberships: []membershipView{}, ListingCounts: a.ListingCounts}
	if a.SuspensionReason != "" {
		reason := a.SuspensionReason
		v.SuspensionReason = &reason
	}
	for _, r := range a.PlatformRoles {
		pr := platformRoleView{Role: r.Role}
		if r.MarketCode != "" {
			code := r.MarketCode
			pr.MarketCode = &code
		}
		v.PlatformRoles = append(v.PlatformRoles, pr)
	}
	for _, r := range a.AccountRoles {
		ar := accountRoleView{Role: r.Role, Status: r.Status}
		if r.OnboardingStatus != "" {
			s := r.OnboardingStatus
			ar.OnboardingStatus = &s
		}
		v.AccountRoles = append(v.AccountRoles, ar)
	}
	for _, m := range a.Memberships {
		v.Memberships = append(v.Memberships, membershipView{OrganisationID: m.OrganisationID, OrganisationName: m.OrganisationName,
			OrganisationKind: m.OrganisationKind, MarketCode: m.MarketCode, Role: m.Role, RoleLabel: auth.MembershipRoleLabel(m.OrganisationKind, m.Role),
			Status: m.Status})
	}
	for _, n := range a.ListingCounts {
		v.ListingsTotal += n
	}
	out := detailResponse{User: v, Subscriptions: d.Subscriptions, Audit: []AuditView{}}
	if out.Subscriptions == nil {
		out.Subscriptions = []SubscriptionInfo{}
	}
	for _, e := range d.Audit {
		out.Audit = append(out.Audit, AuditViewOf(e))
	}
	return out
}

// ---------------------------------------------------------------------------
// Endpoints
// ---------------------------------------------------------------------------

// List handles GET /v1/admin/users.
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	fields := map[string]string{}
	page, size := paging(r, fields)
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	q := r.URL.Query()
	items, total, err := h.svc.List(r.Context(), Filter{Q: q.Get("q"), Role: q.Get("role"), Status: strings.ToUpper(q.Get("status")),
		Market: q.Get("market"), Limit: size, Offset: (page - 1) * size})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := pageView{Items: make([]SummaryView, 0, len(items)), Page: page, PageSize: size, Total: total}
	for _, x := range items {
		out.Items = append(out.Items, summaryView(x))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}

func userID(w http.ResponseWriter, r *http.Request, h *Handler) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.fail(w, r, ErrNotFound)
		return uuid.Nil, false
	}
	return id, true
}

// Get handles GET /v1/admin/users/{id}.
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	id, ok := userID(w, r, h)
	if !ok {
		return
	}
	d, err := h.svc.Get(r.Context(), id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, detailView(d))
}

type reasonBody struct {
	Reason string `json:"reason"`
}

type noteBody struct {
	Note string `json:"note"`
}

// Suspend handles POST /v1/admin/users/{id}/suspend.
func (h *Handler) Suspend(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := userID(w, r, h)
	if !ok {
		return
	}
	var body reasonBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	d, err := h.svc.Suspend(r.Context(), p, id, body.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, detailView(d))
}

// Reactivate handles POST /v1/admin/users/{id}/reactivate.
func (h *Handler) Reactivate(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := userID(w, r, h)
	if !ok {
		return
	}
	var body noteBody
	if r.ContentLength != 0 {
		if !httpx.DecodeJSON(w, r, &body) {
			return
		}
	}
	d, err := h.svc.Reactivate(r.Context(), p, id, body.Note)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, detailView(d))
}

type deletedView struct {
	Status                 string `json:"status"`
	SessionsRevoked        int    `json:"sessions_revoked"`
	OrganisationsLeft      int    `json:"organisations_left"`
	ListingsWithdrawn      int    `json:"listings_withdrawn"`
	ListingsUnassigned     int    `json:"listings_unassigned"`
	SubscriptionsCancelled int    `json:"subscriptions_cancelled"`
}

func deletedOf(d Deleted) deletedView {
	return deletedView{Status: StatusDeleted, SessionsRevoked: d.SessionsRevoked, OrganisationsLeft: d.OrganisationsLeft,
		ListingsWithdrawn: d.ListingsWithdrawn, ListingsUnassigned: d.ListingsUnassigned, SubscriptionsCancelled: d.SubscriptionsCanceled}
}

// Delete handles POST /v1/admin/users/{id}/delete.
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	id, ok := userID(w, r, h)
	if !ok {
		return
	}
	var body reasonBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	out, err := h.svc.Delete(r.Context(), p, id, body.Reason)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, deletedOf(out))
}

type deleteSelfBody struct {
	Password string `json:"password"`
}

// DeleteSelf handles DELETE /v1/me: the signed-in person deletes their own
// account, confirming with their password. The session cookie is cleared.
func (h *Handler) DeleteSelf(w http.ResponseWriter, r *http.Request) {
	p := principal(w, r)
	if p == nil {
		return
	}
	var body deleteSelfBody
	if !httpx.DecodeJSON(w, r, &body) {
		return
	}
	out, err := h.svc.DeleteSelf(r.Context(), p, body.Password)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: auth.SessionCookieName, Value: "", Path: "/", Expires: time.Unix(0, 0), MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookie, SameSite: http.SameSiteLaxMode})
	private(w)
	httpx.WriteJSON(w, http.StatusOK, deletedOf(out))
}

type auditPage struct {
	Items    []AuditView `json:"items"`
	Page     int         `json:"page"`
	PageSize int         `json:"page_size"`
	Total    int         `json:"total"`
}

// Audit handles GET /v1/admin/audit.
func (h *Handler) Audit(w http.ResponseWriter, r *http.Request) {
	if principal(w, r) == nil {
		return
	}
	fields := map[string]string{}
	page, size := paging(r, fields)
	q := r.URL.Query()
	f := adminaudit.Filter{TargetType: strings.ToUpper(q.Get("target_type")), TargetID: q.Get("target_id"), Action: strings.ToUpper(q.Get("action")),
		Limit: size, Offset: (page - 1) * size}
	if raw := q.Get("actor_user_id"); raw != "" {
		id, err := uuid.Parse(raw)
		if err != nil {
			fields["actor_user_id"] = "must be a UUID"
		} else {
			f.ActorID = &id
		}
	}
	if len(fields) > 0 {
		h.fail(w, r, &ValidationError{Fields: fields})
		return
	}
	events, total, err := h.svc.Audit(r.Context(), f)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out := auditPage{Items: make([]AuditView, 0, len(events)), Page: page, PageSize: size, Total: total}
	for _, e := range events {
		out.Items = append(out.Items, AuditViewOf(e))
	}
	private(w)
	httpx.WriteJSON(w, http.StatusOK, out)
}
