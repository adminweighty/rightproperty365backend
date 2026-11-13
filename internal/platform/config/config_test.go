package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
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

func TestFromLookupIdentity(t *testing.T) {
	const db = "postgres://user:pw@example.neon.tech/db"

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
	const db = "postgres://user:pw@example.neon.tech/db"
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
