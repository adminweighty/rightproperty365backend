package accounts

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/adminaudit"
	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/orgaudit"
)

// ReadRoles may list and open accounts: SUPER_ADMIN, PLATFORM_ADMIN and
// SUPPORT (the roles that see the accounts section of the overview; an
// account has no market, so MARKET_ADMIN does not).
func ReadRoles() []string {
	return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin, auth.PlatformSupport}
}

// ManageRoles may suspend, reactivate and delete accounts: SUPER_ADMIN and
// PLATFORM_ADMIN. An account that holds a platform role can only be acted on
// by a SUPER_ADMIN, and nobody acts on their own account.
func ManageRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin} }

// AuditRoles may read the administrators' audit trail (GET /v1/admin/audit).
func AuditRoles() []string { return []string{auth.PlatformSuperAdmin, auth.PlatformAdmin} }

// SubscriptionInfo is one subscription of an account, as its detail shows it.
type SubscriptionInfo struct {
	ID               uuid.UUID  `json:"id"`
	InvoiceNumber    string     `json:"invoice_number"`
	PlanName         string     `json:"plan_name"`
	Period           string     `json:"period"`
	Status           string     `json:"status"`
	SubscriberKind   string     `json:"subscriber_kind"`
	SubscriberName   string     `json:"subscriber_name"`
	Amount           string     `json:"amount"`
	CurrencyCode     string     `json:"currency_code"`
	StartsAt         *time.Time `json:"starts_at"`
	EndsAt           *time.Time `json:"ends_at"`
	OrganisationID   *uuid.UUID `json:"organisation_id"`
	OrganisationName string     `json:"organisation_name,omitempty"`
}

// Deps are the service's collaborators.
type Deps struct {
	Store Store
	// Audit reads the administrators' audit trail.
	Audit adminaudit.Log
	// Mailer sends the deletion notice (nil: none is sent).
	Mailer Mailer
	// Subscriptions returns the subscriptions of a person and of the
	// organisations they own (nil: none).
	Subscriptions func(ctx context.Context, userID uuid.UUID, organisationIDs []uuid.UUID) ([]SubscriptionInfo, error)
	Logger        *slog.Logger
	Now           func() time.Time
}

// Service is account administration.
type Service struct{ d Deps }

// NewService builds the service.
func NewService(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	return &Service{d: d}
}

func (s *Service) now() time.Time { return s.d.Now().UTC().Truncate(time.Microsecond) }

func isSuperAdmin(p *auth.Principal) bool {
	if p.SessionScope != auth.ScopeAdmin {
		return false
	}
	for _, r := range p.PlatformRoles {
		if r.Role == auth.PlatformSuperAdmin {
			return true
		}
	}
	return false
}

// List returns one page of accounts.
func (s *Service) List(ctx context.Context, f Filter) ([]Summary, int, error) {
	fields := map[string]string{}
	if f.Status != "" && !slices.Contains([]string{StatusActive, StatusSuspended, StatusDeleted}, f.Status) {
		fields["status"] = "must be ACTIVE, SUSPENDED or DELETED"
	}
	if f.Market != "" {
		f.Market = strings.ToUpper(f.Market)
		if len(f.Market) != 2 {
			fields["market"] = "must be a two-letter country code"
		}
	}
	f.Q = strings.TrimSpace(f.Q)
	if utf8.RuneCountInString(f.Q) > 200 {
		fields["q"] = "must be at most 200 characters"
	}
	f.Role = strings.ToUpper(strings.TrimSpace(f.Role))
	if len(fields) > 0 {
		return nil, 0, &ValidationError{Fields: fields}
	}
	return s.d.Store.Search(ctx, f)
}

// Detail is a user's detail with their subscriptions and audit records.
type Detail struct {
	Account       Account
	Subscriptions []SubscriptionInfo
	Audit         []adminaudit.Event
}

// Get returns a user's detail.
func (s *Service) Get(ctx context.Context, id uuid.UUID) (Detail, error) {
	a, err := s.d.Store.Account(ctx, id)
	if err != nil {
		return Detail{}, err
	}
	d := Detail{Account: a, Subscriptions: []SubscriptionInfo{}, Audit: []adminaudit.Event{}}
	if s.d.Subscriptions != nil {
		var owned []uuid.UUID
		for _, m := range a.Memberships {
			if m.Role == auth.RoleOwner && m.Status == identity.MembershipActive {
				owned = append(owned, m.OrganisationID)
			}
		}
		subs, err := s.d.Subscriptions(ctx, id, owned)
		if err != nil {
			return Detail{}, err
		}
		if subs != nil {
			d.Subscriptions = subs
		}
	}
	if s.d.Audit != nil {
		events, _, err := s.d.Audit.List(ctx, adminaudit.Filter{TargetType: adminaudit.TargetUser, TargetID: id.String(), Limit: 20})
		if err != nil {
			return Detail{}, err
		}
		d.Audit = events
	}
	return d, nil
}

func reasonOf(field, raw string, required bool) (string, error) {
	reason := strings.TrimSpace(raw)
	n := utf8.RuneCountInString(reason)
	switch {
	case n == 0 && !required:
		return "", nil
	case n < MinReasonRunes:
		return "", &ValidationError{Fields: map[string]string{field: "is required (at least 3 characters)"}}
	case n > MaxReasonRunes:
		return "", &ValidationError{Fields: map[string]string{field: "must be at most 1000 characters"}}
	}
	return reason, nil
}

// target loads the account an administrator acts on and applies the rules:
// never their own, and an administrator account only by a SUPER_ADMIN.
func (s *Service) target(ctx context.Context, p *auth.Principal, id uuid.UUID) (Account, error) {
	if p.UserID == id {
		return Account{}, ErrSelf
	}
	a, err := s.d.Store.Account(ctx, id)
	if err != nil {
		return Account{}, err
	}
	if a.Administrator() && !isSuperAdmin(p) {
		return Account{}, ErrAdminProtected
	}
	return a, nil
}

func statusState(status string) map[string]any { return map[string]any{"status": status} }

// Suspend suspends an ACTIVE account with a reason.
func (s *Service) Suspend(ctx context.Context, p *auth.Principal, id uuid.UUID, rawReason string) (Detail, error) {
	reason, err := reasonOf("reason", rawReason, true)
	if err != nil {
		return Detail{}, err
	}
	a, err := s.target(ctx, p, id)
	if err != nil {
		return Detail{}, err
	}
	if a.Status != StatusActive {
		return Detail{}, ErrState
	}
	now := s.now()
	audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionUserSuspended, adminaudit.TargetUser, id.String(), a.Name, reason,
		statusState(a.Status), statusState(StatusSuspended), now)
	if _, err := s.d.Store.Suspend(ctx, Suspension{UserID: id, Reason: reason, Actor: Actor{UserID: p.UserID, Name: p.Name}, At: now, Audit: audit}); err != nil {
		return Detail{}, err
	}
	s.d.Logger.InfoContext(ctx, "audit", slog.Bool("audit", true), slog.String("event", "account.suspended"),
		slog.String("actor_user_id", p.UserID.String()), slog.String("user_id", id.String()))
	return s.Get(ctx, id)
}

// Reactivate makes a SUSPENDED account ACTIVE again.
func (s *Service) Reactivate(ctx context.Context, p *auth.Principal, id uuid.UUID, rawNote string) (Detail, error) {
	note, err := reasonOf("note", rawNote, false)
	if err != nil {
		return Detail{}, err
	}
	a, err := s.target(ctx, p, id)
	if err != nil {
		return Detail{}, err
	}
	if a.Status != StatusSuspended {
		return Detail{}, ErrState
	}
	now := s.now()
	audit := adminaudit.New(p.UserID, p.Name, adminaudit.ActionUserReactivated, adminaudit.TargetUser, id.String(), a.Name, note,
		statusState(a.Status), statusState(StatusActive), now)
	if _, err := s.d.Store.Reactivate(ctx, Reactivation{UserID: id, Actor: Actor{UserID: p.UserID, Name: p.Name}, At: now, Audit: audit}); err != nil {
		return Detail{}, err
	}
	s.d.Logger.InfoContext(ctx, "audit", slog.Bool("audit", true), slog.String("event", "account.reactivated"),
		slog.String("actor_user_id", p.UserID.String()), slog.String("user_id", id.String()))
	return s.Get(ctx, id)
}

func (s *Service) deletion(a Account, actor Actor, self bool, reason string, now time.Time) Deletion {
	action := adminaudit.ActionUserDeleted
	if self {
		action = adminaudit.ActionUserSelfDeleted
	}
	audit := adminaudit.New(actor.UserID, actor.Name, action, adminaudit.TargetUser, a.ID.String(), DeletedName, reason,
		statusState(a.Status), statusState(StatusDeleted), now)
	return Deletion{UserID: a.ID, Actor: actor, SelfService: self, Reason: reason, At: now, Audit: audit,
		TeamAudit: func(organisationID uuid.UUID, released map[string]any) orgaudit.Event {
			after := map[string]any{"status": "REMOVED", "reason": "ACCOUNT_DELETED"}
			for k, v := range released {
				after[k] = v
			}
			return orgaudit.New(organisationID, actor.UserID, actor.Name, orgaudit.ActionMemberRemoved, orgaudit.TargetMember, a.ID, DeletedName,
				map[string]any{"status": "ACTIVE"}, after, now)
		}}
}

// notifyDeleted emails the address the account had, once it is gone.
func (s *Service) notifyDeleted(ctx context.Context, a Account) {
	if s.d.Mailer == nil || a.Email == "" || strings.HasSuffix(a.Email, "@"+DeletedEmailDomain) {
		return
	}
	if err := s.d.Mailer.SendAccountDeleted(ctx, a.Email, a.Name); err != nil {
		s.d.Logger.ErrorContext(ctx, "account deletion email failed", slog.String("user_id", a.ID.String()), slog.String("error", err.Error()))
	}
}

// Delete deletes (anonymises) an account on an administrator's decision.
func (s *Service) Delete(ctx context.Context, p *auth.Principal, id uuid.UUID, rawReason string) (Deleted, error) {
	reason, err := reasonOf("reason", rawReason, true)
	if err != nil {
		return Deleted{}, err
	}
	a, err := s.target(ctx, p, id)
	if err != nil {
		return Deleted{}, err
	}
	if a.Status == StatusDeleted {
		return Deleted{}, ErrState
	}
	out, err := s.d.Store.Delete(ctx, s.deletion(a, Actor{UserID: p.UserID, Name: p.Name}, false, reason, s.now()))
	if err != nil {
		return Deleted{}, err
	}
	s.d.Logger.InfoContext(ctx, "audit", slog.Bool("audit", true), slog.String("event", "account.deleted"),
		slog.String("actor_user_id", p.UserID.String()), slog.String("user_id", id.String()))
	s.notifyDeleted(ctx, a)
	return out, nil
}

// DeleteSelf deletes the signed-in person's own account after they typed
// their password again. An administrator account cannot delete itself (its
// password is changed with cmd/adminctl, and so is its removal).
func (s *Service) DeleteSelf(ctx context.Context, p *auth.Principal, password string) (Deleted, error) {
	if password == "" {
		return Deleted{}, &ValidationError{Fields: map[string]string{"password": "is required"}}
	}
	a, err := s.d.Store.Account(ctx, p.UserID)
	if err != nil {
		return Deleted{}, err
	}
	if a.Administrator() {
		return Deleted{}, ErrAdminSelfDelete
	}
	if a.Status != StatusActive {
		return Deleted{}, ErrState
	}
	if a.PasswordHash == "" {
		return Deleted{}, ErrInvalidPassword
	}
	ok, err := identity.VerifyPassword(a.PasswordHash, password)
	if err != nil {
		return Deleted{}, err
	}
	if !ok {
		return Deleted{}, ErrInvalidPassword
	}
	out, err := s.d.Store.Delete(ctx, s.deletion(a, Actor{UserID: p.UserID, Name: a.Name}, true, "Deleted by the account holder.", s.now()))
	if err != nil {
		return Deleted{}, err
	}
	s.d.Logger.InfoContext(ctx, "audit", slog.Bool("audit", true), slog.String("event", "account.self_deleted"), slog.String("user_id", p.UserID.String()))
	s.notifyDeleted(ctx, a)
	return out, nil
}

// Audit returns one page of the administrators' audit trail.
func (s *Service) Audit(ctx context.Context, f adminaudit.Filter) ([]adminaudit.Event, int, error) {
	if s.d.Audit == nil {
		return []adminaudit.Event{}, 0, nil
	}
	return s.d.Audit.List(ctx, f)
}

// IsNotFound reports whether err means the account does not exist.
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }
