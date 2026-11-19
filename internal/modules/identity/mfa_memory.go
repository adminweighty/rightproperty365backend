package identity

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/platform/adminaudit"
)

// SetAuditLog connects the shared in-memory administrators' audit log (the
// MFA records). Without it they are dropped.
func (s *MemoryStore) SetAuditLog(log *adminaudit.MemoryLog) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminAudit = log
}

func (s *MemoryStore) appendAuditLocked(e adminaudit.Event) {
	if s.adminAudit != nil {
		s.adminAudit.Append(e)
	}
}

// AdminMFA implements MFAStore.
func (s *MemoryStore) AdminMFA(_ context.Context, userID uuid.UUID) (AdminMFA, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.mfaTOTP[userID]
	if !ok {
		return AdminMFA{}, ErrNotFound
	}
	out := *rec
	out.SecretSealed = slices.Clone(rec.SecretSealed)
	return out, nil
}

// PutPendingTOTP implements MFAStore.
func (s *MemoryStore) PutPendingTOTP(_ context.Context, userID uuid.UUID, sealed []byte, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec, ok := s.mfaTOTP[userID]; ok && rec.ConfirmedAt != nil {
		return ErrMFAAlreadyEnrolled
	}
	s.mfaTOTP[userID] = &AdminMFA{UserID: userID, SecretSealed: slices.Clone(sealed), CreatedAt: now, UpdatedAt: now}
	return nil
}

func newRecoveryRows(userID uuid.UUID, hashes []string, now time.Time) []RecoveryCode {
	rows := make([]RecoveryCode, 0, len(hashes))
	for _, h := range hashes {
		rows = append(rows, RecoveryCode{ID: uuid.New(), UserID: userID, Hash: h, CreatedAt: now})
	}
	return rows
}

// ConfirmTOTP implements MFAStore.
func (s *MemoryStore) ConfirmTOTP(_ context.Context, userID uuid.UUID, step int64, hashes []string, notBefore, now time.Time, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.mfaTOTP[userID]
	if !ok || rec.ConfirmedAt != nil || rec.CreatedAt.Before(notBefore) {
		return ErrNotFound
	}
	t := now
	rec.ConfirmedAt, rec.LastUsedStep, rec.UpdatedAt = &t, step, now
	s.mfaRecovery[userID] = newRecoveryRows(userID, hashes, now)
	s.appendAuditLocked(audit)
	return nil
}

// UseTOTPStep implements MFAStore.
func (s *MemoryStore) UseTOTPStep(_ context.Context, userID uuid.UUID, step int64, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.mfaTOTP[userID]
	if !ok || rec.ConfirmedAt == nil {
		return ErrNotFound
	}
	if step <= rec.LastUsedStep {
		return ErrMFACodeReused
	}
	rec.LastUsedStep, rec.UpdatedAt = step, now
	return nil
}

// UnusedRecoveryCodes implements MFAStore.
func (s *MemoryStore) UnusedRecoveryCodes(_ context.Context, userID uuid.UUID) ([]RecoveryCode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []RecoveryCode
	for _, c := range s.mfaRecovery[userID] {
		if c.UsedAt == nil {
			out = append(out, c)
		}
	}
	return out, nil
}

// UseRecoveryCode implements MFAStore.
func (s *MemoryStore) UseRecoveryCode(_ context.Context, userID, codeID uuid.UUID, now time.Time, audit adminaudit.Event, event OutboxEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	codes := s.mfaRecovery[userID]
	for i := range codes {
		if codes[i].ID == codeID && codes[i].UsedAt == nil {
			t := now
			codes[i].UsedAt = &t
			s.appendAuditLocked(audit)
			s.emit(event)
			return nil
		}
	}
	return ErrNotFound
}

// ReplaceRecoveryCodes implements MFAStore.
func (s *MemoryStore) ReplaceRecoveryCodes(_ context.Context, userID uuid.UUID, hashes []string, now time.Time, audit adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.mfaTOTP[userID]
	if !ok || rec.ConfirmedAt == nil {
		return ErrNotFound
	}
	s.mfaRecovery[userID] = newRecoveryRows(userID, hashes, now)
	s.appendAuditLocked(audit)
	return nil
}

// DeleteAdminMFA implements MFAStore.
func (s *MemoryStore) DeleteAdminMFA(_ context.Context, userID uuid.UUID, now time.Time, audit adminaudit.Event, revokeSessions bool, events ...OutboxEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.mfaTOTP[userID]; !ok {
		return ErrNotFound
	}
	s.deleteMFALocked(userID)
	if revokeSessions {
		for _, sess := range s.sessions {
			if sess.UserID == userID && sess.RevokedAt == nil {
				t := now
				sess.RevokedAt = &t
			}
		}
	}
	s.appendAuditLocked(audit)
	for _, e := range events {
		s.emit(e)
	}
	return nil
}

func (s *MemoryStore) deleteMFALocked(userID uuid.UUID) {
	delete(s.mfaTOTP, userID)
	delete(s.mfaRecovery, userID)
}

// RecordAdminAudit implements MFAStore.
func (s *MemoryStore) RecordAdminAudit(_ context.Context, e adminaudit.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appendAuditLocked(e)
	return nil
}

// MarkLoginChallengeCodeVerified implements MFAStore.
func (s *MemoryStore) MarkLoginChallengeCodeVerified(_ context.Context, idHash []byte, now, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[key(idHash)]
	if !ok || !c.usable(now) || c.CodeVerifiedAt != nil {
		return ErrNotFound
	}
	t := now
	c.CodeVerifiedAt, c.ExpiresAt = &t, expiresAt
	return nil
}

// FailLoginChallengeMFA implements MFAStore.
func (s *MemoryStore) FailLoginChallengeMFA(_ context.Context, idHash []byte, _ time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.challenges[key(idHash)]
	if !ok || c.ConsumedAt != nil {
		return 0, ErrNotFound
	}
	c.MFAAttempts++
	return c.MFAAttempts, nil
}

// HasAdminMFA reports whether any MFA row (pending or confirmed, or a
// recovery code) is held for the user (tests).
func (s *MemoryStore) HasAdminMFA(userID uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, totp := s.mfaTOTP[userID]
	return totp || len(s.mfaRecovery[userID]) > 0
}

// PutAdminMFA stores a CONFIRMED authenticator with recovery code hashes
// directly (the development seed).
func (s *MemoryStore) PutAdminMFA(userID uuid.UUID, sealed []byte, hashes []string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := now
	s.mfaTOTP[userID] = &AdminMFA{UserID: userID, SecretSealed: slices.Clone(sealed), ConfirmedAt: &t, CreatedAt: now, UpdatedAt: now}
	s.mfaRecovery[userID] = newRecoveryRows(userID, hashes, now)
}
