package identity

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"rightproperty365.com/backend/internal/platform/auth"
	"rightproperty365.com/backend/internal/platform/httpx"
)

// Account security (API 0.50.0, Handover section 14): change the password
// while signed in, see and sign out signed-in devices, change the email
// address with a link to the new address and notices to the old one.
// Platform accounts are refused, as for the password reset: their
// credentials are changed with cmd/adminctl.

// EmailChangeTokenTTL is how long the confirmation link of a new address works.
const EmailChangeTokenTTL = 24 * time.Hour

// Errors and codes of account security.
var (
	ErrSessionNotFound = errors.New("session not found")
	ErrPlatformAccount = errors.New("platform accounts change their credentials with adminctl")
)

const (
	CodeSessionNotFound  = "SESSION_NOT_FOUND"
	CodePlatformAccount  = "PLATFORM_ACCOUNT"
	CodeEmailChangeTaken = "EMAIL_TAKEN"
)

// SecurityStore is the part of Store behind this file.
type SecurityStore interface {
	// ChangePassword sets a new password hash, revokes every session of the
	// user but keep, and writes PASSWORD_CHANGED, together.
	ChangePassword(ctx context.Context, userID uuid.UUID, passwordHash string, keep uuid.UUID, now time.Time) error
	// ActiveSessions returns the user's unrevoked, unexpired customer
	// sessions, newest first.
	ActiveSessions(ctx context.Context, userID uuid.UUID, now time.Time) ([]Session, error)
	// RevokeUserSession revokes one of the user's sessions; ErrSessionNotFound
	// for a session that is not theirs or not active.
	RevokeUserSession(ctx context.Context, userID, sessionID uuid.UUID, now time.Time) error
	// RevokeOtherSessions revokes every active session of the user but keep.
	RevokeOtherSessions(ctx context.Context, userID, keep uuid.UUID, now time.Time) (int, error)
	// CreateEmailChange stores a pending change (replacing an older one).
	CreateEmailChange(ctx context.Context, userID uuid.UUID, newEmail string, tokenHash []byte, expiresAt, now time.Time) error
	// ConfirmEmailChange consumes the token and sets the address (verified):
	// ErrInvalidToken, ErrEmailTaken. It returns the user and the old address.
	ConfirmEmailChange(ctx context.Context, tokenHash []byte, now time.Time) (userID uuid.UUID, oldEmail string, err error)
}

// SecurityMailer sends the email-change emails (notifications.AccountMailer).
type SecurityMailer interface {
	// SendEmailChangeConfirm sends the confirmation link to the new address.
	SendEmailChangeConfirm(ctx context.Context, to, name, token string) error
	// SendEmailChangeNotice tells the current address that a change was
	// asked for (done false) or made (done true), naming the new address
	// masked.
	SendEmailChangeNotice(ctx context.Context, to, name, maskedNew string, done bool) error
}

// ClientLabel is the short label of a User-Agent shown in the list of
// signed-in devices: a browser and a system, the app, or "Unknown device".
func ClientLabel(ua string) string {
	l := strings.ToLower(ua)
	if l == "" {
		return "Unknown device"
	}
	system := ""
	switch {
	case strings.Contains(l, "android"):
		system = "Android"
	case strings.Contains(l, "iphone"), strings.Contains(l, "ipad"), strings.Contains(l, "ios"):
		system = "iOS"
	case strings.Contains(l, "windows"):
		system = "Windows"
	case strings.Contains(l, "mac os"), strings.Contains(l, "macintosh"):
		system = "macOS"
	case strings.Contains(l, "linux"):
		system = "Linux"
	}
	app := ""
	switch {
	case strings.Contains(l, "rightproperty365"):
		app = "RightProperty365 app"
	case strings.Contains(l, "edg/"):
		app = "Edge"
	case strings.Contains(l, "firefox/"):
		app = "Firefox"
	case strings.Contains(l, "chrome/"):
		app = "Chrome"
	case strings.Contains(l, "safari/"):
		app = "Safari"
	case strings.Contains(l, "dart/"), strings.Contains(l, "okhttp"):
		app = "App"
	}
	switch {
	case app != "" && system != "":
		return app + " on " + system
	case app != "":
		return app
	case system != "":
		return "Browser on " + system
	}
	return "Unknown device"
}

type clientKey struct{}

// WithClient puts the caller's User-Agent label in ctx for the session a
// sign-in starts.
func WithClient(ctx context.Context, userAgent string) context.Context {
	return context.WithValue(ctx, clientKey{}, ClientLabel(userAgent))
}

func clientOf(ctx context.Context) string {
	if v, ok := ctx.Value(clientKey{}).(string); ok {
		return v
	}
	return ""
}

// ---------------------------------------------------------------------------
// Service
// ---------------------------------------------------------------------------

// SetSecurityMailer connects the email-change emails (nil: none are sent).
func (s *Service) SetSecurityMailer(m SecurityMailer) { s.securityMailer = m }

func (s *Service) customerAccount(ctx context.Context, userID uuid.UUID) (User, error) {
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if len(u.PlatformRoles) > 0 {
		return User{}, ErrPlatformAccount
	}
	return u, nil
}

func (s *Service) checkCurrent(u User, password string) error {
	if password == "" {
		return &ValidationError{Fields: map[string]string{"current_password": "is required"}}
	}
	ok, err := VerifyPassword(u.PasswordHash, password)
	if err != nil {
		return fmt.Errorf("verify password: %w", err)
	}
	if !ok {
		return &ValidationError{Fields: map[string]string{"current_password": "is not correct"}}
	}
	return nil
}

// ChangePassword replaces the password of a signed-in account, keeping the
// current session and revoking every other one.
func (s *Service) ChangePassword(ctx context.Context, userID, sessionID uuid.UUID, current, next string) error {
	u, err := s.customerAccount(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.checkCurrent(u, current); err != nil {
		return err
	}
	if msg := passwordProblem(next); msg != "" {
		return &ValidationError{Fields: map[string]string{"new_password": msg}}
	}
	if current == next {
		return &ValidationError{Fields: map[string]string{"new_password": "must be different from the current password"}}
	}
	hash, err := s.hasher.Hash(next)
	if err != nil {
		return err
	}
	if err := s.store.ChangePassword(ctx, userID, hash, sessionID, s.clock()); err != nil {
		return err
	}
	s.audit(ctx, slog.LevelInfo, "password_changed", "success", slog.String("user_id", userID.String()))
	return nil
}

// Sessions returns the signed-in devices of an account.
func (s *Service) Sessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	return s.store.ActiveSessions(ctx, userID, s.clock())
}

// RevokeSession signs one of the account's devices out.
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	return s.store.RevokeUserSession(ctx, userID, sessionID, s.clock())
}

// RevokeOtherSessions signs every other device out.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID, keep uuid.UUID) (int, error) {
	return s.store.RevokeOtherSessions(ctx, userID, keep, s.clock())
}

// RequestEmailChange emails a confirmation link to the new address and a
// notice to the current one. A taken address gets the "account exists"
// email instead and the caller the same answer (no probing).
func (s *Service) RequestEmailChange(ctx context.Context, userID uuid.UUID, newEmail, password string) error {
	u, err := s.customerAccount(ctx, userID)
	if err != nil {
		return err
	}
	if err := s.checkCurrent(u, password); err != nil {
		return err
	}
	newEmail = NormalizeEmail(newEmail)
	switch {
	case !validEmail(newEmail):
		return &ValidationError{Fields: map[string]string{"new_email": "must be a valid email address"}}
	case newEmail == NormalizeEmail(u.Email):
		return &ValidationError{Fields: map[string]string{"new_email": "is already your email address"}}
	}
	if other, err := s.store.UserByEmail(ctx, newEmail); err == nil {
		if err := s.mailer.SendAccountExists(ctx, newEmail, other.Name); err != nil {
			s.logger.ErrorContext(ctx, "account exists email failed", slog.String("error", err.Error()))
		}
		return nil
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	now := s.clock()
	token, hash, err := newToken()
	if err != nil {
		return err
	}
	if err := s.store.CreateEmailChange(ctx, userID, newEmail, hash, now.Add(EmailChangeTokenTTL), now); err != nil {
		return err
	}
	if s.securityMailer != nil {
		if err := s.securityMailer.SendEmailChangeConfirm(ctx, newEmail, u.Name, token); err != nil {
			s.logger.ErrorContext(ctx, "email change confirmation failed", slog.String("error", err.Error()))
		}
		if err := s.securityMailer.SendEmailChangeNotice(ctx, u.Email, u.Name, MaskEmail(newEmail), false); err != nil {
			s.logger.ErrorContext(ctx, "email change notice failed", slog.String("error", err.Error()))
		}
	}
	s.audit(ctx, slog.LevelInfo, "email_change_requested", "success", slog.String("user_id", userID.String()))
	return nil
}

// ConfirmEmailChange sets the new address from a confirmation token and
// tells the old address.
func (s *Service) ConfirmEmailChange(ctx context.Context, token string) (User, error) {
	token = strings.TrimSpace(token)
	if !plausibleToken(token) {
		return User{}, ErrInvalidToken
	}
	userID, oldEmail, err := s.store.ConfirmEmailChange(ctx, hashToken(token), s.clock())
	if err != nil {
		return User{}, err
	}
	u, err := s.store.UserByID(ctx, userID)
	if err != nil {
		return User{}, err
	}
	if s.securityMailer != nil {
		if err := s.securityMailer.SendEmailChangeNotice(ctx, oldEmail, u.Name, MaskEmail(u.Email), true); err != nil {
			s.logger.ErrorContext(ctx, "email changed notice failed", slog.String("error", err.Error()))
		}
	}
	s.audit(ctx, slog.LevelInfo, "email_changed", "success", slog.String("user_id", userID.String()))
	return u, nil
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

type memoryEmailChange struct {
	UserID    uuid.UUID
	NewEmail  string
	ExpiresAt time.Time
	UsedAt    *time.Time
}

func (s *MemoryStore) revokeLocked(userID, keep uuid.UUID, now time.Time) int {
	n := 0
	for _, sess := range s.sessions {
		if sess.UserID == userID && sess.ID != keep && sess.RevokedAt == nil {
			t := now
			sess.RevokedAt = &t
			n++
		}
	}
	return n
}

// ChangePassword implements Store.
func (s *MemoryStore) ChangePassword(_ context.Context, userID uuid.UUID, passwordHash string, keep uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return ErrNotFound
	}
	u.PasswordHash = passwordHash
	s.revokeLocked(userID, keep, now)
	s.emit(OutboxEvent{Type: EventPasswordChanged, UserID: userID, OccurredAt: now})
	return nil
}

// ActiveSessions implements Store.
func (s *MemoryStore) ActiveSessions(_ context.Context, userID uuid.UUID, now time.Time) ([]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Session{}
	for _, sess := range s.sessions {
		if sess.UserID == userID && sess.RevokedAt == nil && sess.ExpiresAt.After(now) && sess.Scope != auth.ScopeAdmin {
			out = append(out, *sess)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// RevokeUserSession implements Store.
func (s *MemoryStore) RevokeUserSession(_ context.Context, userID, sessionID uuid.UUID, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, sess := range s.sessions {
		if sess.ID == sessionID && sess.UserID == userID && sess.RevokedAt == nil {
			t := now
			sess.RevokedAt = &t
			return nil
		}
	}
	return ErrSessionNotFound
}

// RevokeOtherSessions implements Store.
func (s *MemoryStore) RevokeOtherSessions(_ context.Context, userID, keep uuid.UUID, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokeLocked(userID, keep, now), nil
}

// CreateEmailChange implements Store.
func (s *MemoryStore) CreateEmailChange(_ context.Context, userID uuid.UUID, newEmail string, tokenHash []byte, expiresAt, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.emailChanges == nil {
		s.emailChanges = map[string]*memoryEmailChange{}
	}
	for _, c := range s.emailChanges {
		if c.UserID == userID && c.UsedAt == nil {
			t := now
			c.UsedAt = &t
		}
	}
	s.emailChanges[key(tokenHash)] = &memoryEmailChange{UserID: userID, NewEmail: newEmail, ExpiresAt: expiresAt}
	return nil
}

// ConfirmEmailChange implements Store.
func (s *MemoryStore) ConfirmEmailChange(_ context.Context, tokenHash []byte, now time.Time) (uuid.UUID, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.emailChanges[key(tokenHash)]
	if !ok || c.UsedAt != nil || !c.ExpiresAt.After(now) {
		return uuid.Nil, "", ErrInvalidToken
	}
	if _, taken := s.byEmail[c.NewEmail]; taken {
		return uuid.Nil, "", ErrEmailTaken
	}
	u, ok := s.users[c.UserID]
	if !ok {
		return uuid.Nil, "", ErrInvalidToken
	}
	t := now
	c.UsedAt = &t
	old := u.Email
	delete(s.byEmail, NormalizeEmail(old))
	u.Email = c.NewEmail
	u.EmailVerifiedAt = &t
	s.byEmail[c.NewEmail] = u.ID
	return u.ID, old, nil
}

// ---------------------------------------------------------------------------
// Postgres
// ---------------------------------------------------------------------------

// ChangePassword implements Store.
func (s *PostgresStore) ChangePassword(ctx context.Context, userID uuid.UUID, passwordHash string, keep uuid.UUID, now time.Time) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = $3 WHERE id = $1`, userID, passwordHash, now)
		if err != nil {
			return fmt.Errorf("set password: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, userID, keep, now); err != nil {
			return fmt.Errorf("revoke sessions: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE password_reset_tokens SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`, userID, now); err != nil {
			return fmt.Errorf("invalidate reset tokens: %w", err)
		}
		return insertOutbox(ctx, tx, OutboxEvent{Type: EventPasswordChanged, UserID: userID, OccurredAt: now})
	})
}

// ActiveSessions implements Store.
func (s *PostgresStore) ActiveSessions(ctx context.Context, userID uuid.UUID, now time.Time) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, scope, delivery, created_at, last_seen_at, expires_at, COALESCE(client, '')
		FROM sessions WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > $2 AND scope <> 'admin'
		ORDER BY created_at DESC, id DESC`, userID, now)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
		var sess Session
		err := row.Scan(&sess.ID, &sess.UserID, &sess.Scope, &sess.Delivery, &sess.CreatedAt, &sess.LastSeenAt, &sess.ExpiresAt, &sess.Client)
		sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = sess.CreatedAt.UTC(), sess.LastSeenAt.UTC(), sess.ExpiresAt.UTC()
		return sess, err
	})
	if out == nil {
		out = []Session{}
	}
	return out, err
}

// RevokeUserSession implements Store.
func (s *PostgresStore) RevokeUserSession(ctx context.Context, userID, sessionID uuid.UUID, now time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL`, sessionID, userID, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// RevokeOtherSessions implements Store.
func (s *PostgresStore) RevokeOtherSessions(ctx context.Context, userID, keep uuid.UUID, now time.Time) (int, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE sessions SET revoked_at = $3 WHERE user_id = $1 AND id <> $2 AND revoked_at IS NULL`, userID, keep, now)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// CreateEmailChange implements Store.
func (s *PostgresStore) CreateEmailChange(ctx context.Context, userID uuid.UUID, newEmail string, tokenHash []byte, expiresAt, now time.Time) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE email_change_tokens SET used_at = $2 WHERE user_id = $1 AND used_at IS NULL`, userID, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO email_change_tokens (id, user_id, new_email, token_hash, expires_at, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.New(), userID, newEmail, tokenHash, expiresAt, now)
		return err
	})
}

// ConfirmEmailChange implements Store.
func (s *PostgresStore) ConfirmEmailChange(ctx context.Context, tokenHash []byte, now time.Time) (uuid.UUID, string, error) {
	var userID uuid.UUID
	var oldEmail string
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var newEmail string
		err := tx.QueryRow(ctx, `
			UPDATE email_change_tokens SET used_at = $2
			WHERE token_hash = $1 AND used_at IS NULL AND expires_at > $2
			RETURNING user_id, new_email`, tokenHash, now).Scan(&userID, &newEmail)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidToken
		}
		if err != nil {
			return err
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = lower($1) AND id <> $2)`, newEmail, userID).Scan(&taken); err != nil {
			return err
		}
		if taken {
			return ErrEmailTaken
		}
		if err := tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&oldEmail); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE users SET email = $2, email_verified_at = $3, updated_at = $3 WHERE id = $1`, userID, newEmail, now)
		return err
	})
	if err != nil {
		return uuid.Nil, "", err
	}
	return userID, oldEmail, nil
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (h *Handler) securityFail(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationError
	switch {
	case errors.As(err, &verr):
		httpx.WriteError(w, r, http.StatusBadRequest, httpx.CodeValidationFailed, "Some fields need your attention.", verr.Fields)
	case errors.Is(err, ErrSessionNotFound):
		httpx.WriteError(w, r, http.StatusNotFound, CodeSessionNotFound, "This device is not signed in.", nil)
	case errors.Is(err, ErrPlatformAccount):
		httpx.WriteError(w, r, http.StatusForbidden, CodePlatformAccount, "Administrator accounts change their password and email with the operator tool.", nil)
	case errors.Is(err, ErrInvalidToken):
		httpx.WriteError(w, r, http.StatusBadRequest, CodeInvalidToken, "This link is not valid any more. Ask for a new one from your account.", nil)
	case errors.Is(err, ErrEmailTaken):
		httpx.WriteError(w, r, http.StatusConflict, CodeEmailChangeTaken, "This email address is used by another account.", nil)
	default:
		httpx.WriteInternalError(w, r, h.logger, err)
	}
}

// ChangePassword serves POST /v1/me/password.
func (h *Handler) ChangePassword(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var in struct {
		Current string `json:"current_password"`
		New     string `json:"new_password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.ChangePassword(r.Context(), p.UserID, p.SessionID, in.Current, in.New); err != nil {
		h.securityFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type deviceView struct {
	ID         uuid.UUID `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Client     string    `json:"client"`
	Current    bool      `json:"current"`
}

// Sessions serves GET /v1/me/sessions.
func (h *Handler) Sessions(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	list, err := h.svc.Sessions(r.Context(), p.UserID)
	if err != nil {
		h.securityFail(w, r, err)
		return
	}
	out := make([]deviceView, 0, len(list))
	for _, s := range list {
		client := s.Client
		if client == "" {
			client = "Unknown device"
		}
		out = append(out, deviceView{ID: s.ID, CreatedAt: s.CreatedAt, LastSeenAt: s.LastSeenAt, ExpiresAt: s.ExpiresAt, Client: client, Current: s.ID == p.SessionID})
	}
	w.Header().Set("Cache-Control", "private, no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"items": out})
}

// RevokeSession serves DELETE /v1/me/sessions/{id}.
func (h *Handler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		h.securityFail(w, r, ErrSessionNotFound)
		return
	}
	if err := h.svc.RevokeSession(r.Context(), p.UserID, id); err != nil {
		h.securityFail(w, r, err)
		return
	}
	if id == p.SessionID {
		h.clearSessionCookie(w)
	}
	w.WriteHeader(http.StatusNoContent)
}

// RevokeOtherSessions serves POST /v1/me/sessions/revoke-others.
func (h *Handler) RevokeOtherSessions(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	n, err := h.svc.RevokeOtherSessions(r.Context(), p.UserID, p.SessionID)
	if err != nil {
		h.securityFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"revoked": n})
}

// RequestEmailChange serves POST /v1/me/email.
func (h *Handler) RequestEmailChange(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	var in struct {
		NewEmail string `json:"new_email"`
		Password string `json:"current_password"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	if err := h.svc.RequestEmailChange(r.Context(), p.UserID, in.NewEmail, in.Password); err != nil {
		h.securityFail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// ConfirmEmailChange serves POST /v1/auth/email-change/confirm.
func (h *Handler) ConfirmEmailChange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"token"`
	}
	if !httpx.DecodeJSON(w, r, &in) {
		return
	}
	u, err := h.svc.ConfirmEmailChange(r.Context(), in.Token)
	if err != nil {
		h.securityFail(w, r, err)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"user": map[string]any{"id": u.ID, "email": u.Email, "email_verified": true}})
}
