// Package config loads and validates process configuration from the
// environment. Nothing here reads secrets from disk in production; the optional
// .env loader exists purely for local development convenience.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net/mail"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	EnvDevelopment = "development"
	EnvStaging     = "staging"
	EnvProduction  = "production"

	// Pool modes (DB_POOL_MODE). "session" is a normal connection to
	// PostgreSQL. "transaction" is for a deployment that puts PgBouncer in
	// transaction-pooling mode in front of it.
	PoolModeSession     = "session"
	PoolModeTransaction = "transaction"

	defaultHTTPAddr     = ":8080"
	defaultDBMaxConns   = 8
	maxDBMaxConns       = 100
	defaultCORSOrigins  = "http://localhost:3000"
	defaultPublicWebURL = "http://localhost:3000"
	// DefaultEmailFrom is the confirmed sender. Its domain must be verified
	// in the Resend account before Resend will send from it.
	DefaultEmailFrom = "RightProperty365 <no-reply@rightproperty365.com>"

	// Search drivers (SEARCH_DRIVER).
	SearchDriverDatabase      = "database"
	SearchDriverElasticsearch = "elasticsearch"

	// The search target is a p95 of one second (unmeasured). A request to
	// the search engine that takes longer than this is abandoned and
	// answered from the database instead.
	defaultSearchTimeout = 800 * time.Millisecond
	maxSearchTimeout     = 5 * time.Second

	// Mail drivers (MAIL_DRIVER).
	MailDriverLog    = "log"
	MailDriverResend = "resend"

	defaultAdminSessionTTL     = 12 * time.Hour
	defaultAdminSessionIdleTTL = 30 * time.Minute
	// Hard ceiling so a typo cannot create week-long administrator sessions.
	maxAdminSessionTTL = 24 * time.Hour
)

// Config is the validated runtime configuration shared by the API and worker.
type Config struct {
	Env      string
	HTTPAddr string
	// DatabaseURL is the PostgreSQL connection string. It is the one required
	// database setting outside development.
	DatabaseURL string
	// DatabaseDirectURL is an OPTIONAL override used by migrations, adminctl
	// and seed. It is only needed when DatabaseURL goes through a transaction
	// pooler; otherwise leave it empty and DatabaseURL is used.
	DatabaseDirectURL string
	// DBPoolMode is PoolModeSession (default) or PoolModeTransaction and
	// describes what DatabaseURL connects to.
	DBPoolMode         string
	DBMaxConns         int32
	CORSAllowedOrigins []string
	LogLevel           slog.Level
	// PublicWebURL is the public origin of the web app (PUBLIC_WEB_URL; the
	// older WEB_BASE_URL is still read as a deprecated alias). Every link and
	// image URL in an email is built from it. No trailing slash.
	PublicWebURL string
	// MailDriver is MailDriverResend or MailDriverLog, already resolved from
	// MAIL_DRIVER, RESEND_API_KEY and the environment.
	MailDriver string
	// ResendAPIKey is the Resend API key. It formats as "[redacted]"; call
	// Reveal only where the Authorization header is built.
	ResendAPIKey Secret
	// SearchDriver is SearchDriverDatabase or SearchDriverElasticsearch,
	// already resolved from SEARCH_DRIVER and ELASTICSEARCH_URL.
	SearchDriver string
	// ElasticsearchURL is the cluster address; ElasticsearchAPIKey formats as
	// "[redacted]".
	ElasticsearchURL    string
	ElasticsearchAPIKey Secret
	// SearchIndexPrefix names this environment's indices: <prefix>-listings-*.
	SearchIndexPrefix string
	// SearchTimeout bounds one request to the search engine.
	SearchTimeout time.Duration
	// EmailFrom is the sender of every email; EmailReplyTo is optional.
	EmailFrom    string
	EmailReplyTo string
	// DevSeedPassword is the shared password of the development seed users.
	// It is only ever populated when Env is development and must never be logged.
	DevSeedPassword string
	// AdminSessionTTL and AdminSessionIdleTTL bound administrator sessions
	// (absolute lifetime and inactivity timeout).
	AdminSessionTTL     time.Duration
	AdminSessionIdleTTL time.Duration
}

// ToolDatabase returns the connection string and pool mode that one-off
// administrative commands (migrate, adminctl, seed) should use: the direct URL
// when one is configured (always a plain session connection), otherwise the
// runtime URL with its configured pool mode. url is "" when neither is set.
func ToolDatabase(get func(string) string) (url, poolMode string, err error) {
	if direct := strings.TrimSpace(get("DATABASE_DIRECT_URL")); direct != "" {
		return direct, PoolModeSession, nil
	}
	poolMode, err = parsePoolMode(strings.TrimSpace(get("DB_POOL_MODE")))
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(get("DATABASE_URL")), poolMode, nil
}

func parsePoolMode(raw string) (string, error) {
	switch strings.ToLower(raw) {
	case "", PoolModeSession:
		return PoolModeSession, nil
	case PoolModeTransaction:
		return PoolModeTransaction, nil
	}
	return "", fmt.Errorf("DB_POOL_MODE must be %q or %q", PoolModeSession, PoolModeTransaction)
}

// DatabaseTLSWarning returns a human-readable warning when databaseURL would
// let the connection run without TLS to a host that is not this machine, or
// "" when there is nothing to say. It never includes the URL, the host or any
// credential, and it changes nothing about how the connection is made.
//
// PostgreSQL clients default to sslmode=prefer, which silently falls back to
// an unencrypted connection; only require, verify-ca and verify-full insist
// on TLS.
func DatabaseTLSWarning(databaseURL string) string {
	u, err := url.Parse(databaseURL)
	if err != nil || databaseURL == "" {
		return ""
	}
	switch u.Hostname() {
	case "", "localhost", "127.0.0.1", "::1":
		return "" // same machine (or a Unix socket): nothing travels over a network
	}
	switch mode := strings.ToLower(u.Query().Get("sslmode")); mode {
	case "require", "verify-ca", "verify-full":
		return ""
	case "":
		return "the database is on another host and its connection string has no sslmode, so it defaults to \"prefer\" and can fall back to an UNENCRYPTED connection; add sslmode=require (or verify-full)"
	default:
		return "the database is on another host and its connection string uses sslmode=" + mode + ", which allows an UNENCRYPTED connection; use sslmode=require (or verify-full)"
	}
}

var searchPrefixRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// Secret is a configuration value that must never be printed. Every
// formatting path (%s, %v, %+v, %#v, slog) yields "[redacted]".
type Secret string

// Reveal returns the secret. Use it only at the point of use.
func (s Secret) Reveal() string { return string(s) }

// IsSet reports whether a value is present.
func (s Secret) IsSet() bool { return s != "" }

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// GoString implements fmt.GoStringer (%#v).
func (s Secret) GoString() string { return s.String() }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

// MarshalText keeps the value out of JSON and other encoders.
func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// resolveSearchDriver decides which search engine answers GET /v1/listings.
//
//	SEARCH_DRIVER=elasticsearch  Elasticsearch; ELASTICSEARCH_URL is required
//	                             (startup fails without it, like DATABASE_URL).
//	SEARCH_DRIVER=database       the database adapter, even when
//	                             ELASTICSEARCH_URL is set.
//	SEARCH_DRIVER unset          elasticsearch when ELASTICSEARCH_URL is set,
//	(or empty)                   otherwise database.
func resolveSearchDriver(raw string, urlSet bool) (string, error) {
	switch strings.ToLower(raw) {
	case SearchDriverElasticsearch:
		if !urlSet {
			return "", errors.New("SEARCH_DRIVER=elasticsearch requires ELASTICSEARCH_URL")
		}
		return SearchDriverElasticsearch, nil
	case SearchDriverDatabase:
		return SearchDriverDatabase, nil
	case "":
		if urlSet {
			return SearchDriverElasticsearch, nil
		}
		return SearchDriverDatabase, nil
	}
	return "", fmt.Errorf("SEARCH_DRIVER must be %q or %q", SearchDriverDatabase, SearchDriverElasticsearch)
}

// resolveMailDriver decides which mailer runs.
//
//	MAIL_DRIVER=resend  Resend; RESEND_API_KEY is required.
//	MAIL_DRIVER=log     the development log mailer, even when a key is set.
//	                    Refused outside development: it prints links that
//	                    are credentials and delivers nothing.
//	MAIL_DRIVER unset   resend when RESEND_API_KEY is set; otherwise log in
//	(or empty)          development and an error elsewhere.
func resolveMailDriver(raw, env string, keySet bool) (string, error) {
	switch strings.ToLower(raw) {
	case MailDriverResend:
		if !keySet {
			return "", errors.New("MAIL_DRIVER=resend requires RESEND_API_KEY")
		}
		return MailDriverResend, nil
	case MailDriverLog:
		if env != EnvDevelopment {
			return "", fmt.Errorf("MAIL_DRIVER=log is only allowed when APP_ENV=development (got %s): it sends nothing and logs sign-in links", env)
		}
		return MailDriverLog, nil
	case "":
		if keySet {
			return MailDriverResend, nil
		}
		if env == EnvDevelopment {
			return MailDriverLog, nil
		}
		return "", fmt.Errorf("RESEND_API_KEY is required when APP_ENV=%s: without it no email (verification, password reset) can be sent", env)
	}
	return "", fmt.Errorf("MAIL_DRIVER must be %q or %q", MailDriverLog, MailDriverResend)
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

	poolMode, err := parsePoolMode(val("DB_POOL_MODE", ""))
	if err != nil {
		errs = append(errs, err)
	}
	cfg.DBPoolMode = poolMode

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

	// PUBLIC_WEB_URL is the setting; WEB_BASE_URL is its deprecated alias and
	// is only read when PUBLIC_WEB_URL is not set.
	cfg.PublicWebURL = strings.TrimRight(val("PUBLIC_WEB_URL", val("WEB_BASE_URL", defaultPublicWebURL)), "/")
	if u, err := url.Parse(cfg.PublicWebURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		errs = append(errs, errors.New("PUBLIC_WEB_URL must be an http(s) URL such as https://app.example.com"))
	}

	// Search. The address is validated without being echoed; the key never is.
	cfg.ElasticsearchURL = strings.TrimRight(val("ELASTICSEARCH_URL", ""), "/")
	if cfg.ElasticsearchURL != "" {
		if u, err := url.Parse(cfg.ElasticsearchURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			errs = append(errs, errors.New("ELASTICSEARCH_URL must be an http(s) URL without credentials, such as https://search.internal:9200"))
		}
	}
	cfg.ElasticsearchAPIKey = Secret(val("ELASTICSEARCH_API_KEY", ""))
	searchDriver, err := resolveSearchDriver(val("SEARCH_DRIVER", ""), cfg.ElasticsearchURL != "")
	if err != nil {
		errs = append(errs, err)
	}
	cfg.SearchDriver = searchDriver
	cfg.SearchIndexPrefix = strings.ToLower(val("SEARCH_INDEX_PREFIX", "rp365-"+cfg.Env))
	if !searchPrefixRe.MatchString(cfg.SearchIndexPrefix) {
		errs = append(errs, errors.New("SEARCH_INDEX_PREFIX must be 1-40 lower-case letters, digits or dashes, starting with a letter"))
	}
	cfg.SearchTimeout = defaultSearchTimeout
	if raw := val("SEARCH_TIMEOUT", ""); raw != "" {
		v, err := time.ParseDuration(raw)
		if err != nil || v < 50*time.Millisecond || v > maxSearchTimeout {
			errs = append(errs, fmt.Errorf("SEARCH_TIMEOUT must be a Go duration between 50ms and %s, for example 800ms", maxSearchTimeout))
		} else {
			cfg.SearchTimeout = v
		}
	}

	// Email. The API key is never echoed, not even in an error.
	cfg.ResendAPIKey = Secret(val("RESEND_API_KEY", ""))
	driver, err := resolveMailDriver(val("MAIL_DRIVER", ""), cfg.Env, cfg.ResendAPIKey.IsSet())
	if err != nil {
		errs = append(errs, err)
	}
	cfg.MailDriver = driver
	cfg.EmailFrom = val("EMAIL_FROM", DefaultEmailFrom)
	if _, err := mail.ParseAddress(cfg.EmailFrom); err != nil {
		errs = append(errs, errors.New("EMAIL_FROM must be an address such as \"RightProperty365 <no-reply@rightproperty365.com>\""))
	}
	cfg.EmailReplyTo = val("EMAIL_REPLY_TO", "")
	if cfg.EmailReplyTo != "" {
		if _, err := mail.ParseAddress(cfg.EmailReplyTo); err != nil {
			errs = append(errs, errors.New("EMAIL_REPLY_TO must be an email address"))
		}
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
// A missing file is not an error. Deployed environments should inject real
// env vars instead.
//
// Precedence: a variable that is PRESENT in the process environment always
// wins, even when its value is empty. So `RESEND_API_KEY= go run ./cmd/api`
// runs without a key although .env has one, and `MAIL_DRIVER=log` in the
// environment overrides whatever .env says.
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
