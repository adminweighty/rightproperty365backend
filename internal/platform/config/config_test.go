package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func lookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestFromLookupDefaults(t *testing.T) {
	cfg, err := FromLookup(lookup(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Env != EnvDevelopment || cfg.HTTPAddr != ":8080" || cfg.DBMaxConns != 8 || cfg.LogLevel != slog.LevelInfo {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.CORSAllowedOrigins, []string{"http://localhost:3000"}) {
		t.Fatalf("unexpected CORS default: %v", cfg.CORSAllowedOrigins)
	}
	if !cfg.UseMemoryStore() {
		t.Fatal("development without DATABASE_URL should use the memory store")
	}
	if cfg.PublicWebURL != "http://localhost:3000" || cfg.DevSeedPassword != "" {
		t.Fatalf("unexpected identity defaults: web=%q seedSet=%v", cfg.PublicWebURL, cfg.DevSeedPassword != "")
	}
}

func TestFromLookupAdminSessions(t *testing.T) {
	cfg, err := FromLookup(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminSessionTTL != 12*time.Hour || cfg.AdminSessionIdleTTL != 30*time.Minute {
		t.Fatalf("defaults: ttl=%s idle=%s", cfg.AdminSessionTTL, cfg.AdminSessionIdleTTL)
	}

	cfg, err = FromLookup(lookup(map[string]string{"ADMIN_SESSION_TTL": "8h", "ADMIN_SESSION_IDLE_TTL": "15m"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AdminSessionTTL != 8*time.Hour || cfg.AdminSessionIdleTTL != 15*time.Minute {
		t.Fatalf("overrides: ttl=%s idle=%s", cfg.AdminSessionTTL, cfg.AdminSessionIdleTTL)
	}

	for name, env := range map[string]map[string]string{
		"not a duration": {"ADMIN_SESSION_TTL": "12"},
		"too long":       {"ADMIN_SESSION_TTL": "720h"},
		"too short":      {"ADMIN_SESSION_IDLE_TTL": "5s"},
		"negative":       {"ADMIN_SESSION_IDLE_TTL": "-30m"},
		"idle above ttl": {"ADMIN_SESSION_TTL": "1h", "ADMIN_SESSION_IDLE_TTL": "2h"},
	} {
		if _, err := FromLookup(lookup(env)); err == nil || !strings.Contains(err.Error(), "ADMIN_SESSION") {
			t.Fatalf("%s: expected an ADMIN_SESSION error, got %v", name, err)
		}
	}
}

func TestDatabasePoolMode(t *testing.T) {
	const db = "postgres://user:pw@db.example.internal/db?sslmode=require"

	cfg, err := FromLookup(lookup(nil))
	if err != nil || cfg.DBPoolMode != PoolModeSession {
		t.Fatalf("default pool mode = %q, %v; want session", cfg.DBPoolMode, err)
	}
	for raw, want := range map[string]string{"session": PoolModeSession, "transaction": PoolModeTransaction, "Transaction": PoolModeTransaction, " session ": PoolModeSession} {
		cfg, err := FromLookup(lookup(map[string]string{"DB_POOL_MODE": raw}))
		if err != nil || cfg.DBPoolMode != want {
			t.Fatalf("DB_POOL_MODE=%q -> %q, %v; want %q", raw, cfg.DBPoolMode, err, want)
		}
	}
	for _, raw := range []string{"pgbouncer", "statement", "true", "session,transaction"} {
		if _, err := FromLookup(lookup(map[string]string{"DB_POOL_MODE": raw})); err == nil || !strings.Contains(err.Error(), "DB_POOL_MODE") {
			t.Fatalf("DB_POOL_MODE=%q: expected an error, got %v", raw, err)
		}
	}

	// DATABASE_URL alone is enough outside development.
	prod, err := FromLookup(lookup(map[string]string{"APP_ENV": "production", "DATABASE_URL": db, "RESEND_API_KEY": "test-key-not-real"}))
	if err != nil || prod.DatabaseDirectURL != "" || prod.DBPoolMode != PoolModeSession {
		t.Fatalf("production with only DATABASE_URL: %v (direct set=%v, mode=%s)", err, prod.DatabaseDirectURL != "", prod.DBPoolMode)
	}
}

func TestToolDatabase(t *testing.T) {
	const runtime, direct = "postgres://u:p@db.example.internal/app", "postgres://u:p@db.example.internal:5433/app"
	tests := []struct {
		name     string
		env      map[string]string
		wantURL  string
		wantMode string
		wantErr  bool
	}{
		{"only DATABASE_URL", map[string]string{"DATABASE_URL": runtime}, runtime, PoolModeSession, false},
		{"DATABASE_URL behind a transaction pooler", map[string]string{"DATABASE_URL": runtime, "DB_POOL_MODE": "transaction"}, runtime, PoolModeTransaction, false},
		{"direct override wins and is a session connection", map[string]string{"DATABASE_URL": runtime, "DATABASE_DIRECT_URL": direct, "DB_POOL_MODE": "transaction"}, direct, PoolModeSession, false},
		{"nothing set", nil, "", PoolModeSession, false},
		{"bad pool mode", map[string]string{"DATABASE_URL": runtime, "DB_POOL_MODE": "nope"}, "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url, mode, err := ToolDatabase(lookup(tt.env))
			if (err != nil) != tt.wantErr || url != tt.wantURL || mode != tt.wantMode {
				t.Fatalf("ToolDatabase = (set=%v, %q, %v), want (%q, %v)", url != "", mode, err, tt.wantMode, tt.wantErr)
			}
		})
	}
}

func TestDatabaseTLSWarning(t *testing.T) {
	tests := []struct {
		name, url string
		wantWarn  bool
	}{
		{"remote with require", "postgres://u:secret@db.example.internal/app?sslmode=require", false},
		{"remote with verify-full", "postgres://u:secret@db.example.internal/app?sslmode=verify-full", false},
		{"remote with verify-ca", "postgres://u:secret@10.0.0.5:5432/app?sslmode=verify-ca", false},
		{"remote without sslmode", "postgres://u:secret@db.example.internal/app", true},
		{"remote with prefer", "postgres://u:secret@db.example.internal/app?sslmode=prefer", true},
		{"remote with disable", "postgres://u:secret@10.0.0.5/app?sslmode=disable", true},
		{"localhost with disable", "postgres://u:secret@localhost:5432/app?sslmode=disable", false},
		{"loopback without sslmode", "postgres://u:secret@127.0.0.1/app", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DatabaseTLSWarning(tt.url)
			if (got != "") != tt.wantWarn {
				t.Fatalf("warning = %q, want a warning: %v", got, tt.wantWarn)
			}
			if strings.Contains(got, "secret") || strings.Contains(got, "example.internal") || strings.Contains(got, "10.0.0.5") {
				t.Fatalf("the warning leaks the connection string: %q", got)
			}
		})
	}
}

func TestFromLookupIdentity(t *testing.T) {
	const db = "postgres://user:pw@db.example.internal/db"

	dev, err := FromLookup(lookup(map[string]string{"WEB_BASE_URL": "http://localhost:4000/", "DEV_SEED_PASSWORD": "seed-value"}))
	if err != nil {
		t.Fatal(err)
	}
	if dev.PublicWebURL != "http://localhost:4000" || dev.DevSeedPassword != "seed-value" {
		t.Fatalf("development: web=%q seedSet=%v", dev.PublicWebURL, dev.DevSeedPassword != "")
	}

	prod, err := FromLookup(lookup(map[string]string{"APP_ENV": "production", "DATABASE_URL": db, "DEV_SEED_PASSWORD": "seed-value", "RESEND_API_KEY": "test-key-not-real"}))
	if err != nil {
		t.Fatal(err)
	}
	if prod.DevSeedPassword != "" {
		t.Fatal("DEV_SEED_PASSWORD must be ignored outside development")
	}

	if _, err := FromLookup(lookup(map[string]string{"WEB_BASE_URL": "localhost:3000"})); err == nil || !strings.Contains(err.Error(), "PUBLIC_WEB_URL") {
		t.Fatalf("expected a PUBLIC_WEB_URL error, got %v", err)
	}

	// PUBLIC_WEB_URL is the setting; WEB_BASE_URL is a deprecated alias that loses.
	both, err := FromLookup(lookup(map[string]string{"PUBLIC_WEB_URL": "https://rightproperty365.com/", "WEB_BASE_URL": "http://localhost:4000"}))
	if err != nil || both.PublicWebURL != "https://rightproperty365.com" {
		t.Fatalf("PUBLIC_WEB_URL must win: %q %v", both.PublicWebURL, err)
	}
}

func TestFromLookup(t *testing.T) {
	const db = "postgres://user:pw@db.example.internal/db"
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
		check   func(t *testing.T, cfg Config)
	}{
		{
			name: "production fully configured",
			env: map[string]string{
				"APP_ENV": "production", "DATABASE_URL": db, "DATABASE_DIRECT_URL": db, "RESEND_API_KEY": "test-key-not-real",
				"DB_MAX_CONNS": "4", "LOG_LEVEL": "warn", "HTTP_ADDR": ":9000",
				"CORS_ALLOWED_ORIGINS": "https://rightproperty365.com/, https://www.rightproperty365.com",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.UseMemoryStore() || cfg.DBMaxConns != 4 || cfg.LogLevel != slog.LevelWarn || cfg.HTTPAddr != ":9000" {
					t.Fatalf("unexpected config: %+v", cfg)
				}
				want := []string{"https://rightproperty365.com", "https://www.rightproperty365.com"}
				if !reflect.DeepEqual(cfg.CORSAllowedOrigins, want) {
					t.Fatalf("origins = %v, want %v", cfg.CORSAllowedOrigins, want)
				}
			},
		},
		{name: "production requires DATABASE_URL", env: map[string]string{"APP_ENV": "production"}, wantErr: "DATABASE_URL is required"},
		{name: "staging requires DATABASE_URL", env: map[string]string{"APP_ENV": "staging"}, wantErr: "DATABASE_URL is required"},
		{name: "unknown environment", env: map[string]string{"APP_ENV": "qa"}, wantErr: "APP_ENV must be one of"},
		{name: "non numeric pool size", env: map[string]string{"DB_MAX_CONNS": "lots"}, wantErr: "DB_MAX_CONNS"},
		{name: "zero pool size", env: map[string]string{"DB_MAX_CONNS": "0"}, wantErr: "DB_MAX_CONNS"},
		{name: "oversized pool", env: map[string]string{"DB_MAX_CONNS": "500"}, wantErr: "DB_MAX_CONNS"},
		{name: "wildcard CORS origin", env: map[string]string{"CORS_ALLOWED_ORIGINS": "*"}, wantErr: "CORS_ALLOWED_ORIGINS"},
		{name: "CORS origin with path", env: map[string]string{"CORS_ALLOWED_ORIGINS": "https://a.example/app"}, wantErr: "CORS_ALLOWED_ORIGINS"},
		{name: "bad log level", env: map[string]string{"LOG_LEVEL": "loud"}, wantErr: "LOG_LEVEL"},
		{name: "bad pool mode", env: map[string]string{"DB_POOL_MODE": "pgbouncer"}, wantErr: "DB_POOL_MODE"},
		{name: "bad database scheme", env: map[string]string{"DATABASE_URL": "mysql://secret-value"}, wantErr: "DATABASE_URL must start with"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := FromLookup(lookup(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "secret-value") {
					t.Fatalf("error leaks the connection string: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

const fakeKey = "test-key-not-real-0123456789"

func TestMailDriverSelection(t *testing.T) {
	const db = "postgres://user:pw@db.example.internal/db"
	tests := []struct {
		name       string
		env        map[string]string
		wantDriver string
		wantErr    string
	}{
		{"development, no key: log", nil, MailDriverLog, ""},
		{"development, key set: resend", map[string]string{"RESEND_API_KEY": fakeKey}, MailDriverResend, ""},
		{"development, key set, log forced", map[string]string{"RESEND_API_KEY": fakeKey, "MAIL_DRIVER": "log"}, MailDriverLog, ""},
		{"development, key set, driver upper case", map[string]string{"RESEND_API_KEY": fakeKey, "MAIL_DRIVER": "LOG"}, MailDriverLog, ""},
		{"development, resend forced without a key", map[string]string{"MAIL_DRIVER": "resend"}, "", "requires RESEND_API_KEY"},
		{"development, blank key counts as unset", map[string]string{"RESEND_API_KEY": "   "}, MailDriverLog, ""},
		{"empty MAIL_DRIVER means automatic", map[string]string{"RESEND_API_KEY": fakeKey, "MAIL_DRIVER": ""}, MailDriverResend, ""},
		{"unknown driver", map[string]string{"MAIL_DRIVER": "smtp"}, "", "MAIL_DRIVER must be"},
		{"production, key set: resend", map[string]string{"APP_ENV": "production", "DATABASE_URL": db, "RESEND_API_KEY": fakeKey}, MailDriverResend, ""},
		{"production without a key fails startup", map[string]string{"APP_ENV": "production", "DATABASE_URL": db}, "", "RESEND_API_KEY is required"},
		{"staging without a key fails startup", map[string]string{"APP_ENV": "staging", "DATABASE_URL": db}, "", "RESEND_API_KEY is required"},
		{"production refuses the log driver", map[string]string{"APP_ENV": "production", "DATABASE_URL": db, "RESEND_API_KEY": fakeKey, "MAIL_DRIVER": "log"}, "", "only allowed when APP_ENV=development"},
		{"staging refuses the log driver", map[string]string{"APP_ENV": "staging", "DATABASE_URL": db, "MAIL_DRIVER": "log"}, "", "only allowed when APP_ENV=development"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := FromLookup(lookup(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				if strings.Contains(err.Error(), fakeKey) {
					t.Fatal("the configuration error contains the API key")
				}
				return
			}
			if err != nil || cfg.MailDriver != tt.wantDriver {
				t.Fatalf("driver = %q, %v; want %q", cfg.MailDriver, err, tt.wantDriver)
			}
		})
	}
}

func TestEmailSettings(t *testing.T) {
	cfg, err := FromLookup(lookup(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.EmailFrom != "RightProperty365 <no-reply@rightproperty365.com>" || cfg.EmailReplyTo != "" {
		t.Fatalf("defaults: from=%q replyTo=%q", cfg.EmailFrom, cfg.EmailReplyTo)
	}
	cfg, err = FromLookup(lookup(map[string]string{"EMAIL_FROM": "Ops <ops@example.com>", "EMAIL_REPLY_TO": "help@example.com"}))
	if err != nil || cfg.EmailFrom != "Ops <ops@example.com>" || cfg.EmailReplyTo != "help@example.com" {
		t.Fatalf("overrides: %+v %v", cfg, err)
	}
	for key, bad := range map[string]string{"EMAIL_FROM": "not an address", "EMAIL_REPLY_TO": "nope"} {
		if _, err := FromLookup(lookup(map[string]string{key: bad})); err == nil || !strings.Contains(err.Error(), key) {
			t.Fatalf("%s=%q: expected an error, got %v", key, bad, err)
		}
	}
}

// The API key must not leak through any way a Config can be printed.
func TestSecretIsNeverPrinted(t *testing.T) {
	cfg, err := FromLookup(lookup(map[string]string{"RESEND_API_KEY": fakeKey}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ResendAPIKey.Reveal() != fakeKey || !cfg.ResendAPIKey.IsSet() {
		t.Fatal("the key was not loaded")
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	logger.Debug("config", slog.Any("config", cfg), slog.Any("key", cfg.ResendAPIKey))
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string]string{
		"%v":   fmt.Sprintf("%v", cfg),
		"%+v":  fmt.Sprintf("%+v", cfg),
		"%#v":  fmt.Sprintf("%#v", cfg),
		"%s":   fmt.Sprintf("%s", cfg.ResendAPIKey),
		"%q":   fmt.Sprintf("%q", cfg.ResendAPIKey),
		"slog": logs.String(),
		"json": string(encoded),
	} {
		if strings.Contains(out, fakeKey) {
			t.Fatalf("%s leaks the API key: %s", name, out)
		}
	}
	if Secret("").String() != "" || Secret("").IsSet() {
		t.Fatal("an empty secret must stay empty")
	}
}

// Real environment variables win over .env, including when they are set to
// an empty string. That is how a local run is kept away from a real key.
func TestLoadDotEnvPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "RP_TEST_FROM_FILE=file-value\nRP_TEST_OVERRIDDEN=file-value\nRP_TEST_EMPTY_IN_ENV=file-value\n" +
		"# a comment\nexport RP_TEST_EXPORTED=\"quoted value\"\nRP_TEST_EMPTY_IN_FILE=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RP_TEST_OVERRIDDEN", "env-value")
	t.Setenv("RP_TEST_EMPTY_IN_ENV", "") // present but empty
	for _, k := range []string{"RP_TEST_FROM_FILE", "RP_TEST_EXPORTED", "RP_TEST_EMPTY_IN_FILE"} {
		t.Setenv(k, "x") // registers cleanup
		os.Unsetenv(k)
	}

	if err := LoadDotEnv(path); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"RP_TEST_FROM_FILE":     "file-value",   // only in the file: loaded
		"RP_TEST_OVERRIDDEN":    "env-value",    // the environment wins
		"RP_TEST_EMPTY_IN_ENV":  "",             // set-but-empty in the environment still wins
		"RP_TEST_EXPORTED":      "quoted value", // export prefix and quotes are handled
		"RP_TEST_EMPTY_IN_FILE": "",
	} {
		if got := os.Getenv(key); got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	if _, present := os.LookupEnv("RP_TEST_EMPTY_IN_ENV"); !present {
		t.Fatal("the empty variable must remain present")
	}
	if err := LoadDotEnv(filepath.Join(t.TempDir(), "missing.env")); err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}

	// The practical consequence for email: a key in .env is ignored when the
	// process environment sets RESEND_API_KEY to empty, and MAIL_DRIVER=log in
	// the environment beats MAIL_DRIVER=resend in .env.
	mailEnv := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(mailEnv, []byte("RESEND_API_KEY="+fakeKey+"\nMAIL_DRIVER=resend\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("MAIL_DRIVER", "log")
	t.Setenv("APP_ENV", "development")
	t.Setenv("DATABASE_URL", "")
	if err := LoadDotEnv(mailEnv); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MailDriver != MailDriverLog || cfg.ResendAPIKey.IsSet() {
		t.Fatalf("driver=%s keySet=%v; want the log driver and no key", cfg.MailDriver, cfg.ResendAPIKey.IsSet())
	}

	// Forcing the log driver also works while the key IS loaded from .env.
	os.Unsetenv("RESEND_API_KEY")
	if err := LoadDotEnv(mailEnv); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MailDriver != MailDriverLog || !cfg.ResendAPIKey.IsSet() {
		t.Fatalf("driver=%s keySet=%v; want the log driver with the key present", cfg.MailDriver, cfg.ResendAPIKey.IsSet())
	}
}

func TestSearchDriverSelection(t *testing.T) {
	const db, es = "postgres://user:pw@db.example.internal/db", "https://search.example.internal:9200"
	prod := func(extra map[string]string) map[string]string {
		env := map[string]string{"APP_ENV": "production", "DATABASE_URL": db, "RESEND_API_KEY": fakeKey}
		for k, v := range extra {
			env[k] = v
		}
		return env
	}
	tests := []struct {
		name       string
		env        map[string]string
		wantDriver string
		wantErr    string
	}{
		{"development, nothing set: database", nil, SearchDriverDatabase, ""},
		{"development, URL set: elasticsearch", map[string]string{"ELASTICSEARCH_URL": es}, SearchDriverElasticsearch, ""},
		{"development, URL set but database forced", map[string]string{"ELASTICSEARCH_URL": es, "SEARCH_DRIVER": "database"}, SearchDriverDatabase, ""},
		{"development, elasticsearch forced without a URL", map[string]string{"SEARCH_DRIVER": "elasticsearch"}, "", "requires ELASTICSEARCH_URL"},
		{"production, nothing set: database", prod(nil), SearchDriverDatabase, ""},
		{"production, elasticsearch without a URL fails startup", prod(map[string]string{"SEARCH_DRIVER": "elasticsearch"}), "", "requires ELASTICSEARCH_URL"},
		{"production, elasticsearch configured", prod(map[string]string{"SEARCH_DRIVER": "ELASTICSEARCH", "ELASTICSEARCH_URL": es + "/", "ELASTICSEARCH_API_KEY": fakeKey}), SearchDriverElasticsearch, ""},
		{"staging, URL set: elasticsearch", map[string]string{"APP_ENV": "staging", "DATABASE_URL": db, "RESEND_API_KEY": fakeKey, "ELASTICSEARCH_URL": es}, SearchDriverElasticsearch, ""},
		{"unknown driver", map[string]string{"SEARCH_DRIVER": "solr"}, "", "SEARCH_DRIVER must be"},
		{"URL without a scheme", map[string]string{"ELASTICSEARCH_URL": "es.private.example:9200"}, "", "ELASTICSEARCH_URL"},
		{"URL with credentials", map[string]string{"ELASTICSEARCH_URL": "https://elastic:hunter2@es.private.example:9200"}, "", "ELASTICSEARCH_URL"},
		{"bad prefix", map[string]string{"SEARCH_INDEX_PREFIX": "Bad Prefix!"}, "", "SEARCH_INDEX_PREFIX"},
		{"bad timeout", map[string]string{"SEARCH_TIMEOUT": "30s"}, "", "SEARCH_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := FromLookup(lookup(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
				}
				for _, secret := range []string{fakeKey, "hunter2", "es.private.example"} {
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("the configuration error echoes %q", secret)
					}
				}
				return
			}
			if err != nil || cfg.SearchDriver != tt.wantDriver {
				t.Fatalf("driver = %q, %v; want %q", cfg.SearchDriver, err, tt.wantDriver)
			}
		})
	}

	cfg, err := FromLookup(lookup(map[string]string{"ELASTICSEARCH_URL": es + "/", "ELASTICSEARCH_API_KEY": fakeKey, "SEARCH_TIMEOUT": "500ms", "SEARCH_INDEX_PREFIX": "RP365-QA"}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ElasticsearchURL != es || cfg.SearchTimeout != 500*time.Millisecond || cfg.SearchIndexPrefix != "rp365-qa" || cfg.ElasticsearchAPIKey.Reveal() != fakeKey {
		t.Fatalf("unexpected search config: %+v", cfg)
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v", cfg, cfg, cfg), fakeKey) {
		t.Fatal("the Elasticsearch API key is printed with the config")
	}
	def, _ := FromLookup(lookup(nil))
	if def.SearchIndexPrefix != "rp365-development" || def.SearchTimeout != 800*time.Millisecond || def.ElasticsearchAPIKey.IsSet() {
		t.Fatalf("search defaults: %+v", def)
	}
}
