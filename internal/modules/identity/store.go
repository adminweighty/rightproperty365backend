package identity

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Store persists users, their roles, sessions and one-time tokens. Token and
// session lookups are always by SHA-256 digest; raw tokens are never stored.
type Store interface {
	// CreateUser atomically creates the account, its registration consent
	// record and its account roles. It returns ErrEmailTaken when the email is
	// already registered.
	CreateUser(ctx context.Context, u NewUser) error

	// UpdatePendingRegistration replaces the name, password, consent and
	// account-role selection of an account whose email is still UNVERIFIED and
	// invalidates its outstanding
	// verification tokens, so only the most recent registration can complete.
	// It returns ErrNotFound if the account is missing or already verified.
	UpdatePendingRegistration(ctx context.Context, u NewUser) error

	// UserByEmail returns the user (with roles) for a lower-cased email, or ErrNotFound.
	UserByEmail(ctx context.Context, email string) (User, error)
	// UserByID returns the user (with roles), or ErrNotFound.
	UserByID(ctx context.Context, id uuid.UUID) (User, error)

	// CreateVerificationToken stores a single-use email verification token.
	CreateVerificationToken(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt, now time.Time) error
	// ConsumeVerificationToken atomically marks the token used and the user's
	// email verified, returning the user ID. When the address was not verified
	// before, it also writes the USER_EMAIL_VERIFIED outbox event. ErrInvalidToken when the token is
	// unknown, expired or already used.
	ConsumeVerificationToken(ctx context.Context, tokenHash []byte, now time.Time) (uuid.UUID, error)

	// CreatePasswordResetToken stores a single-use password reset token.
	CreatePasswordResetToken(ctx context.Context, userID uuid.UUID, tokenHash []byte, expiresAt, now time.Time) error
	// ResetPassword atomically consumes the token, sets the new password hash,
	// marks the email verified (the token proves control of the mailbox),
	// invalidates the user's other reset tokens, revokes ALL the user's
	// sessions and writes the PASSWORD_CHANGED outbox event. ErrInvalidToken when the token is unknown, expired or used.
	ResetPassword(ctx context.Context, tokenHash []byte, passwordHash string, now time.Time) (uuid.UUID, error)

	// CreateSession stores a new session and, atomically with it, any outbox
	// events (the administrator sign-in notice).
	CreateSession(ctx context.Context, s Session, events ...OutboxEvent) error
	// SessionByTokenHash returns the session, or ErrNotFound. Expiry and
	// revocation are judged by the service.
	SessionByTokenHash(ctx context.Context, tokenHash []byte) (Session, error)
	// TouchSession records activity on a session.
	TouchSession(ctx context.Context, id uuid.UUID, now time.Time) error
	// RevokeSession revokes one session (idempotent).
	RevokeSession(ctx context.Context, id uuid.UUID, now time.Time) error

	// CreateCaptcha stores a registration captcha (its id, the hash of its
	// answer and its expiry) and takes the opportunity to delete captchas that
	// expired before c.CreatedAt.
	CreateCaptcha(ctx context.Context, c Captcha) error
	// ConsumeCaptcha atomically marks an unused captcha used and returns it,
	// expired or not (the service judges expiry). ErrNotFound when the id is
	// unknown or the captcha was already used: a captcha is presented once.
	ConsumeCaptcha(ctx context.Context, id string, now time.Time) (Captcha, error)

	// ApprovedDirectOwners returns the number of ACTIVE accounts whose
	// PRIVATE_SELLER or LANDLORD role is approved (GET /v1/trust/summary).
	ApprovedDirectOwners(ctx context.Context) (int, error)
}
