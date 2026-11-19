package devseed

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/secretbox"
	"rightproperty365.com/backend/internal/platform/totp"
)

// The administrator enrolled in MFA (API 0.40.0), so the web's three-step
// administrator sign-in can be tried. Every other sample administrator is
// NOT enrolled (ADMIN_MFA_REQUIRED is false in development and staging by
// default).
//
// DEVELOPMENT AND STAGING SEED ONLY. The TOTP secret and recovery codes
// below are FIXED and PUBLIC (docs/sample-accounts.md): they protect nothing
// and exist only on databases seeded with sample data, which cmd/seed refuses
// to write in production (CheckEnvironment).
const (
	MFAAdminEmail = "admin.mfa@example.test"
	// DevMFASecretBase32 is the fixed development-only TOTP secret of
	// MFAAdminEmail (add it to an authenticator app by hand).
	DevMFASecretBase32 = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
)

// MFAAdminID is the fixed user ID of MFAAdminEmail.
var MFAAdminID = id("d317")

// DevMFARecoveryCodes are the fixed development-only recovery codes of
// MFAAdminEmail. Each works once until the account is seeded again.
func DevMFARecoveryCodes() []string {
	return []string{
		"devrc-aaaa2", "devrc-bbbb3", "devrc-cccc4", "devrc-dddd5", "devrc-eeee6",
		"devrc-ffff7", "devrc-gggg8", "devrc-hhhh9", "devrc-jjjj2", "devrc-kkkk3",
	}
}

func sampleAdminMFA(box *secretbox.Box, hasher identity.Hasher) ([]byte, []string, error) {
	secret, err := totp.DecodeSecret(DevMFASecretBase32)
	if err != nil {
		return nil, nil, err
	}
	sealed, err := identity.SealTOTPSecret(box, MFAAdminID, secret)
	if err != nil {
		return nil, nil, err
	}
	var hashes []string
	for _, c := range DevMFARecoveryCodes() {
		// Stored as the sign-in normalises them: no dash, lower case.
		h, err := hasher.Hash(c[:5] + c[6:])
		if err != nil {
			return nil, nil, err
		}
		hashes = append(hashes, h)
	}
	return sealed, hashes, nil
}

// SeedMemoryAdminMFA enrols MFAAdminEmail in the in-memory identity store
// (after SeedMemoryUsers) with the fixed development secret, sealed by box.
func SeedMemoryAdminMFA(store *identity.MemoryStore, box *secretbox.Box, hasher identity.Hasher, now time.Time) error {
	sealed, hashes, err := sampleAdminMFA(box, hasher)
	if err != nil {
		return fmt.Errorf("seed sample administrator mfa: %w", err)
	}
	store.PutAdminMFA(MFAAdminID, sealed, hashes, now.UTC().Truncate(time.Microsecond))
	return nil
}

// SeedPostgresAdminMFA enrols MFAAdminEmail on Postgres (needs migration
// 0042; after SeedPostgres). Re-running replaces the authenticator and
// restores all ten recovery codes. box must seal with the API's
// MFA_ENCRYPTION_KEY (or the development key when none is set).
func SeedPostgresAdminMFA(ctx context.Context, pool *pgxpool.Pool, box *secretbox.Box, hasher identity.Hasher, now time.Time) error {
	sealed, hashes, err := sampleAdminMFA(box, hasher)
	if err != nil {
		return fmt.Errorf("seed sample administrator mfa: %w", err)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return identity.PutConfirmedAdminMFATx(ctx, tx, MFAAdminID, sealed, hashes, now.UTC().Truncate(time.Microsecond))
	})
}
