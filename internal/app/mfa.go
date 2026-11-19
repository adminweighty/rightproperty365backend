package app

import (
	"fmt"
	"log/slog"

	"rightproperty365.com/backend/internal/platform/config"
	"rightproperty365.com/backend/internal/platform/secretbox"
)

// AdminMFABox builds the box that seals the administrators' TOTP secrets
// (API 0.40.0) from MFA_ENCRYPTION_KEY:
//   - key set: that key, everywhere;
//   - development without a key: the FIXED, publicly known development key,
//     with a loud warning (a development database and the API agree across
//     restarts; nothing is protected);
//   - staging or production without a key: nil, with a warning: administrator
//     MFA answers 503 MFA_UNAVAILABLE. config.Load already refuses to start
//     when ADMIN_MFA_REQUIRED is true there.
//
// The key is never logged.
func AdminMFABox(cfg config.Config, logger *slog.Logger) (*secretbox.Box, error) {
	if cfg.MFAEncryptionKey.IsSet() {
		key, err := secretbox.ParseKey(cfg.MFAEncryptionKey.Reveal())
		if err != nil {
			return nil, fmt.Errorf("MFA_ENCRYPTION_KEY: %w", err)
		}
		return secretbox.New(key)
	}
	if cfg.Env == config.EnvDevelopment {
		logger.Warn("MFA_ENCRYPTION_KEY is not set: administrator TOTP secrets are sealed with the FIXED, PUBLIC development key. " +
			"Development only; never use this database's authenticators anywhere else.")
		return secretbox.New(secretbox.DevelopmentKey())
	}
	if cfg.AdminMFARequired {
		return nil, fmt.Errorf("MFA_ENCRYPTION_KEY is required when ADMIN_MFA_REQUIRED is true (APP_ENV=%s)", cfg.Env)
	}
	logger.Warn("MFA_ENCRYPTION_KEY is not set: administrator MFA is unavailable (its routes answer 503 MFA_UNAVAILABLE)",
		slog.String("env", cfg.Env))
	return nil, nil
}
