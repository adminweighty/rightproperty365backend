// Package config loads and validates process configuration from the
// environment. Nothing here reads secrets from disk in production; the optional
// .env loader exists purely for local development convenience.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"

	defaultHTTPAddr    = ":8080"
	defaultDBMaxConns  = 8
	maxDBMaxConns      = 100
	defaultCORSOrigins = "http://localhost:3000"
	defaultWebBaseURL  = "http://localhost:3000"

	defaultAdminSessionTTL     = 12 * time.Hour
	defaultAdminSessionIdleTTL = 30 * time.Minute
	// Hard ceiling so a typo cannot create week-long administrator sessions.
	maxAdminSessionTTL = 24 * time.Hour
)

// Config is the validated runtime configuration shared by the API and worker.
type Config struct {
	Env      string
	HTTPAddr string
	// DatabaseURL is the pooled (PgBouncer, transaction mode) runtime URL.
	DatabaseURL string
	// DatabaseDirectURL is the direct connection used only by migrations.
	DatabaseDirectURL  string
	DBMaxConns         int32
	CORSAllowedOrigins []string
	LogLevel           slog.Level
	// WebBaseURL is the frontend origin used to build links placed in emails
	// (verify-email, reset-password). No trailing slash.
	WebBaseURL string
	// DevSeedPassword is the shared password of the development seed users.
	// It is only ever populated when Env is development and must never be logged.
	DevSeedPassword string
	// AdminSessionTTL and AdminSessionIdleTTL bound administrator sessions
	// (absolute lifetime and inactivity timeout).
	AdminSessionTTL     time.Duration
	AdminSessionIdleTTL time.Duration
}

// UseMemoryStore reports whether the process should run on the in-memory
// development stores. Load guarantees this is only ever true in development.
func (c Config) UseMemoryStore() bool { return c.DatabaseURL == "" }

// Load reads configuration from the process environment.
func Load() (Config, error) { return FromLookup(os.Getenv) }

// FromLookup builds a Config using get to resolve variables (injectable for tests).
func FromLookup(get func(string) string) (Config, error) {
	var errs []error
	val := func(key, def string) string {
		if v := strings.TrimSpace(get(key)); v != "" {
			return v
		}
		return def
	}

	cfg := Config{
		Env:               strings.ToLower(val("APP_ENV", EnvDevelopment)),
		HTTPAddr:          val("HTTP_ADDR", defaultHTTPAddr),
		DatabaseURL:       val("DATABASE_URL", ""),
		DatabaseDirectURL: val("DATABASE_DIRECT_URL", ""),
		DBMaxConns:        defaultDBMaxConns,
	}

	switch cfg.Env {
	case EnvDevelopment, EnvStaging, EnvProduction:
	default:
		errs = append(errs, fmt.Errorf("APP_ENV must be one of development, staging, production (got %q)", cfg.Env))
	}

	if raw := val("DB_MAX_CONNS", ""); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxDBMaxConns {
			errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be an integer between 1 and %d", maxDBMaxConns))
		} else {
			cfg.DBMaxConns = int32(n)
		}
	}

	// Connection strings carry credentials: validate them without ever echoing the value.
	for _, kv := range []struct{ key, value string }{
		{"DATABASE_URL", cfg.DatabaseURL},
		{"DATABASE_DIRECT_URL", cfg.DatabaseDirectURL},
	} {
		if kv.value != "" && !strings.HasPrefix(kv.value, "postgres://") && !strings.HasPrefix(kv.value, "postgresql://") {
			errs = append(errs, fmt.Errorf("%s must start with postgres:// or postgresql://", kv.key))
		}
	}
	if cfg.DatabaseURL == "" && cfg.Env != EnvDevelopment {
		errs = append(errs, fmt.Errorf("DATABASE_URL is required when APP_ENV=%s (in-memory stores are development only)", cfg.Env))
	}

	origins, err := parseOrigins(val("CORS_ALLOWED_ORIGINS", defaultCORSOrigins))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.CORSAllowedOrigins = origins

	cfg.WebBaseURL = strings.TrimRight(val("WEB_BASE_URL", defaultWebBaseURL), "/")
	if u, err := url.Parse(cfg.WebBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		errs = append(errs, errors.New("WEB_BASE_URL must be an http(s) URL such as https://app.example.com"))
	}

	// Seed users exist only in development; elsewhere the variable is ignored
	// so a stray value can never create known-password accounts.
	if cfg.Env == EnvDevelopment {
		cfg.DevSeedPassword = get("DEV_SEED_PASSWORD")
	}

	cfg.AdminSessionTTL, cfg.AdminSessionIdleTTL = defaultAdminSessionTTL, defaultAdminSessionIdleTTL
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{
		{"ADMIN_SESSION_TTL", &cfg.AdminSessionTTL},
		{"ADMIN_SESSION_IDLE_TTL", &cfg.AdminSessionIdleTTL},
	} {
		raw := val(d.key, "")
		if raw == "" {
			continue
		}
		v, err := time.ParseDuration(raw)
		if err != nil || v < time.Minute || v > maxAdminSessionTTL {
			errs = append(errs, fmt.Errorf("%s must be a Go duration between 1m and %s, for example 12h or 30m", d.key, maxAdminSessionTTL))
			continue
		}
		*d.dst = v
	}
	if cfg.AdminSessionIdleTTL > cfg.AdminSessionTTL {
		errs = append(errs, errors.New("ADMIN_SESSION_IDLE_TTL must not be longer than ADMIN_SESSION_TTL"))
	}

	level, err := parseLevel(val("LOG_LEVEL", "info"))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.LogLevel = level

	if len(errs) > 0 {
		return Config{}, fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return cfg, nil
}

func parseOrigins(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		o := strings.TrimRight(strings.TrimSpace(part), "/")
		if o == "" {
			continue
		}
		u, err := url.Parse(o)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q must be an exact origin such as https://app.example.com (wildcards are not allowed)", o)
		}
		if strings.Contains(u.Host, "*") {
			return nil, fmt.Errorf("CORS_ALLOWED_ORIGINS entry %q must not contain wildcards", o)
		}
		out = append(out, o)
	}
	if len(out) == 0 {
		return nil, errors.New("CORS_ALLOWED_ORIGINS must contain at least one origin")
	}
	return out, nil
}

func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("LOG_LEVEL must be one of debug, info, warn, error (got %q)", raw)
}

// LoadDotEnv sets variables from a KEY=VALUE file for local development.
// Variables already present in the environment always win and a missing file
// is not an error. Deployed environments should inject real env vars instead.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return sc.Err()
}
