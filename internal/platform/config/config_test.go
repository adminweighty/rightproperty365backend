package config

import (
	"log/slog"
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
	if cfg.WebBaseURL != "http://localhost:3000" || cfg.DevSeedPassword != "" {
		t.Fatalf("unexpected identity defaults: web=%q seedSet=%v", cfg.WebBaseURL, cfg.DevSeedPassword != "")
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
	prod, err := FromLookup(lookup(map[string]string{"APP_ENV": "production", "DATABASE_URL": db}))
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
	if dev.WebBaseURL != "http://localhost:4000" || dev.DevSeedPassword != "seed-value" {
		t.Fatalf("development: web=%q seedSet=%v", dev.WebBaseURL, dev.DevSeedPassword != "")
	}

	prod, err := FromLookup(lookup(map[string]string{"APP_ENV": "production", "DATABASE_URL": db, "DEV_SEED_PASSWORD": "seed-value"}))
	if err != nil {
		t.Fatal(err)
	}
	if prod.DevSeedPassword != "" {
		t.Fatal("DEV_SEED_PASSWORD must be ignored outside development")
	}

	if _, err := FromLookup(lookup(map[string]string{"WEB_BASE_URL": "localhost:3000"})); err == nil || !strings.Contains(err.Error(), "WEB_BASE_URL") {
		t.Fatalf("expected a WEB_BASE_URL error, got %v", err)
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
				"APP_ENV": "production", "DATABASE_URL": db, "DATABASE_DIRECT_URL": db,
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
