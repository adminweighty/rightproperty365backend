package adminctl

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
)

const goodPassword = "a-long-enough-password"

type harness struct {
	store  *identity.MemoryStore
	opened int
	out    bytes.Buffer
}

func (h *harness) run(t *testing.T, stdin string, envVars map[string]string, args ...string) error {
	t.Helper()
	if h.store == nil {
		h.store = identity.NewMemoryStore()
	}
	return Run(context.Background(), Env{
		Args:   args,
		Getenv: func(k string) string { return envVars[k] },
		Stdin:  strings.NewReader(stdin),
		Stdout: &h.out,
		OpenStore: func(context.Context) (Store, func(), error) {
			h.opened++
			return h.store, func() {}, nil
		},
		Hasher: identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1},
		Now:    func() time.Time { return time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC) },
	})
}

func TestCreate(t *testing.T) {
	ctx := context.Background()

	t.Run("password from stdin", func(t *testing.T) {
		h := &harness{}
		if err := h.run(t, goodPassword+"\r\n", nil, "create", "--email", " Root@Example.com ", "--name", "Root Admin", "--role", "SUPER_ADMIN"); err != nil {
			t.Fatal(err)
		}
		u, err := h.store.UserByEmail(ctx, "root@example.com")
		if err != nil {
			t.Fatal(err)
		}
		if !u.Verified() || u.Name != "Root Admin" || len(u.PlatformRoles) != 1 || u.PlatformRoles[0] != (auth.PlatformRole{Role: auth.PlatformSuperAdmin}) {
			t.Fatalf("unexpected user: %+v", u)
		}
		if len(u.Memberships) != 0 || len(u.AccountRoles) != 0 {
			t.Fatalf("an administrator must have no memberships or account roles: %+v", u)
		}
		if !strings.HasPrefix(u.PasswordHash, "$argon2id$") {
			t.Fatalf("password not stored as Argon2id: %q", u.PasswordHash)
		}
		if ok, err := identity.VerifyPassword(u.PasswordHash, goodPassword); err != nil || !ok {
			t.Fatalf("stored hash does not verify: %v %v", ok, err)
		}
		if strings.Contains(h.out.String(), goodPassword) {
			t.Fatal("the password was printed")
		}
	})

	t.Run("password from the environment wins and stdin is not needed", func(t *testing.T) {
		h := &harness{}
		err := h.run(t, "", map[string]string{PasswordEnv: goodPassword}, "create", "--email", "m@example.com", "--name", "M", "--role", "MARKET_ADMIN", "--market", "ZW")
		if err != nil {
			t.Fatal(err)
		}
		u, _ := h.store.UserByEmail(ctx, "m@example.com")
		if len(u.PlatformRoles) != 1 || u.PlatformRoles[0] != (auth.PlatformRole{Role: auth.PlatformMarketAdmin, MarketCode: "ZW"}) {
			t.Fatalf("unexpected roles: %+v", u.PlatformRoles)
		}
	})

	t.Run("existing email is refused, not promoted", func(t *testing.T) {
		h := &harness{}
		args := []string{"create", "--email", "dup@example.com", "--name", "Dup", "--role", "SUPPORT"}
		if err := h.run(t, goodPassword, nil, args...); err != nil {
			t.Fatal(err)
		}
		err := h.run(t, goodPassword, nil, args...)
		if err == nil || !strings.Contains(err.Error(), "already exists") || IsUsageError(err) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCreateRejectsBadInput(t *testing.T) {
	base := func(role string, extra ...string) []string {
		return append([]string{"create", "--email", "a@example.com", "--name", "A", "--role", role}, extra...)
	}
	tests := []struct {
		name     string
		args     []string
		stdin    string
		wantText string
	}{
		{"no command", nil, goodPassword, "command is required"},
		{"unknown command", []string{"delete"}, goodPassword, "unknown command"},
		{"missing email", []string{"create", "--name", "A", "--role", "SUPPORT"}, goodPassword, "--email is required"},
		{"bad email", []string{"create", "--email", "nope", "--name", "A", "--role", "SUPPORT"}, goodPassword, "not a valid email"},
		{"missing name", []string{"create", "--email", "a@example.com", "--role", "SUPPORT"}, goodPassword, "--name is required"},
		{"missing role", []string{"create", "--email", "a@example.com", "--name", "A"}, goodPassword, "--role is required"},
		{"membership role", base("OWNER"), goodPassword, "not a platform role"},
		{"account role", base("AGENCY_OWNER"), goodPassword, "not a platform role"},
		{"consumer", base("CONSUMER"), goodPassword, "not a platform role"},
		{"lower-case role", base("super_admin"), goodPassword, "not a platform role"},
		{"market admin without market", base("MARKET_ADMIN"), goodPassword, "--market is required"},
		{"market on a global role", base("SUPER_ADMIN", "--market", "ZW"), goodPassword, "only allowed with --role MARKET_ADMIN"},
		{"malformed market", base("MARKET_ADMIN", "--market", "zimbabwe"), goodPassword, "two-letter"},
		{"password as a flag", base("SUPPORT", "--password", goodPassword), "", "flag provided but not defined"},
		{"stray argument", base("SUPPORT", "extra"), goodPassword, "unexpected argument"},
		{"no password", base("SUPPORT"), "", "no password supplied"},
		{"short password", base("SUPPORT"), "123456789\n", "at least 10"},
		{"long password", base("SUPPORT"), strings.Repeat("x", 129) + "\n", "at most 128"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &harness{}
			err := h.run(t, tt.stdin, nil, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantText)
			}
			if !IsUsageError(err) {
				t.Fatalf("expected a usage error, got %T", err)
			}
			if h.opened != 0 {
				t.Fatal("the database must not be opened for invalid input")
			}
			if h.store.UserCount() != 0 {
				t.Fatal("invalid input created an account")
			}
			if tt.stdin != "" && len(tt.stdin) > 5 && strings.Contains(err.Error(), strings.TrimSpace(tt.stdin)) {
				t.Fatal("the error message contains the password")
			}
		})
	}

	// Every platform role is accepted; nothing else is.
	for _, role := range auth.AllPlatformRoles() {
		args := base(role)
		if role == auth.PlatformMarketAdmin {
			args = append(args, "--market", "ZW")
		}
		if err := (&harness{}).run(t, goodPassword, nil, args...); err != nil {
			t.Fatalf("role %s: %v", role, err)
		}
	}
}

func TestSetPassword(t *testing.T) {
	ctx := context.Background()
	h := &harness{}
	if err := h.run(t, goodPassword, nil, "create", "--email", "root@example.com", "--name", "Root", "--role", "SUPER_ADMIN"); err != nil {
		t.Fatal(err)
	}
	// An ordinary customer account.
	if err := h.store.CreateUser(ctx, identity.NewUser{ID: identity.SeedConsumerUserID, Name: "C", Email: "customer@example.com", PasswordHash: "x"}); err != nil {
		t.Fatal(err)
	}

	const changed = "another-long-password"
	if err := h.run(t, changed+"\n", nil, "set-password", "--email", "ROOT@example.com"); err != nil {
		t.Fatal(err)
	}
	u, _ := h.store.UserByEmail(ctx, "root@example.com")
	if ok, _ := identity.VerifyPassword(u.PasswordHash, changed); !ok {
		t.Fatal("new password does not verify")
	}
	if ok, _ := identity.VerifyPassword(u.PasswordHash, goodPassword); ok {
		t.Fatal("old password still verifies")
	}

	for name, tt := range map[string]struct {
		email, stdin, wantText string
		usage                  bool
	}{
		"unknown account":  {"nobody@example.com", changed, "no account", false},
		"customer account": {"customer@example.com", changed, "not a platform administrator", false},
		"weak password":    {"root@example.com", "short", "at least 10", true},
		"missing email":    {"", changed, "--email is required", true},
	} {
		args := []string{"set-password"}
		if tt.email != "" {
			args = append(args, "--email", tt.email)
		}
		err := h.run(t, tt.stdin, nil, args...)
		if err == nil || !strings.Contains(err.Error(), tt.wantText) || IsUsageError(err) != tt.usage {
			t.Fatalf("%s: err = %v (usage=%v)", name, err, IsUsageError(err))
		}
	}
	c, _ := h.store.UserByEmail(ctx, "customer@example.com")
	if c.PasswordHash != "x" {
		t.Fatal("a customer's password was changed by adminctl")
	}
}

func TestStoreIsNotOpenedWhenItFails(t *testing.T) {
	err := Run(context.Background(), Env{
		Args:      []string{"set-password", "--email", "root@example.com"},
		Getenv:    func(string) string { return "" },
		Stdin:     strings.NewReader(goodPassword),
		Stdout:    &bytes.Buffer{},
		OpenStore: func(context.Context) (Store, func(), error) { return nil, nil, errors.New("DATABASE_URL is not set") },
		Hasher:    identity.Hasher{MemoryKiB: 64, Time: 1, Threads: 1},
		Now:       time.Now,
	})
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL is not set") {
		t.Fatalf("err = %v", err)
	}
}
