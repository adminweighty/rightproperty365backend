// Package adminctl holds the logic of the cmd/adminctl command: creating the
// first platform administrators and changing their passwords directly in the
// database. It exists because platform roles can never be obtained through
// the public API.
//
// The password is read from the ADMINCTL_PASSWORD environment variable or from
// standard input. It is never accepted as a command-line flag (flags end up in
// shell history and process listings), never echoed and never logged.
package adminctl

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"rightproperty365.com/backend/internal/modules/identity"
	"rightproperty365.com/backend/internal/platform/auth"
)

// PasswordEnv is the environment variable that may carry the password.
const PasswordEnv = "ADMINCTL_PASSWORD"

const usage = `usage:
  adminctl create --email EMAIL --name NAME --role ROLE [--market CC]
  adminctl set-password --email EMAIL

ROLE is one of SUPER_ADMIN, PLATFORM_ADMIN, MARKET_ADMIN, FINANCE_ADMIN,
CONTENT_ADMIN, MODERATOR, SUPPORT. MARKET_ADMIN requires --market (for example ZW).

The password is read from the ADMINCTL_PASSWORD environment variable, or from
standard input when that is not set. It is never accepted as a flag.
The database is DATABASE_URL (or DATABASE_DIRECT_URL when that override is set).`

var (
	emailRe  = regexp.MustCompile(`^[^\s@<>"]+@[^\s@<>"]+\.[^\s@<>".]+$`)
	marketRe = regexp.MustCompile(`^[A-Z]{2}$`)
)

// Store is the slice of the identity store the command needs.
type Store interface {
	CreatePlatformAdmin(ctx context.Context, a identity.NewPlatformAdmin) error
	SetPlatformAdminPassword(ctx context.Context, email, passwordHash string, now time.Time) error
}

// Env is everything the command touches, injectable for tests.
type Env struct {
	Args   []string // without the program name
	Getenv func(string) string
	// ReadPassword returns the password when ADMINCTL_PASSWORD is not set. It
	// must not echo. Nil means: read the first line of Stdin.
	ReadPassword func() (string, error)
	Stdin        io.Reader
	Stdout       io.Writer
	// OpenStore connects to the database. It is called only after every
	// argument and the password have been validated.
	OpenStore func(ctx context.Context) (Store, func(), error)
	Hasher    identity.Hasher
	Now       func() time.Time
}

// usageError marks mistakes in how the command was invoked.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// IsUsageError reports whether err is an invocation mistake (exit status 2).
func IsUsageError(err error) bool {
	var u *usageError
	return errors.As(err, &u)
}

// Usage returns the help text.
func Usage() string { return usage }

// createArgs is a validated `create` invocation.
type createArgs struct {
	email, name, role, market string
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // errors are reported by the caller
	return fs
}

func parseCreate(args []string) (createArgs, error) {
	fs := newFlagSet("create")
	var a createArgs
	fs.StringVar(&a.email, "email", "", "")
	fs.StringVar(&a.name, "name", "", "")
	fs.StringVar(&a.role, "role", "", "")
	fs.StringVar(&a.market, "market", "", "")
	if err := fs.Parse(args); err != nil {
		return createArgs{}, usagef("%v", err)
	}
	if fs.NArg() > 0 {
		return createArgs{}, usagef("unexpected argument %q", fs.Arg(0))
	}

	var err error
	if a.email, err = validEmail(a.email); err != nil {
		return createArgs{}, err
	}
	a.name = strings.TrimSpace(a.name)
	switch n := utf8.RuneCountInString(a.name); {
	case n == 0:
		return createArgs{}, usagef("--name is required")
	case n > 120:
		return createArgs{}, usagef("--name must be at most 120 characters")
	}

	a.role = strings.TrimSpace(a.role)
	switch {
	case a.role == "":
		return createArgs{}, usagef("--role is required")
	case !auth.IsPlatformRole(a.role):
		return createArgs{}, usagef("--role %q is not a platform role (allowed: %s)", a.role, strings.Join(auth.AllPlatformRoles(), ", "))
	}

	a.market = strings.TrimSpace(a.market)
	switch {
	case a.role == auth.PlatformMarketAdmin && a.market == "":
		return createArgs{}, usagef("--market is required for MARKET_ADMIN (for example --market ZW)")
	case a.role != auth.PlatformMarketAdmin && a.market != "":
		return createArgs{}, usagef("--market is only allowed with --role MARKET_ADMIN")
	case a.market != "" && !marketRe.MatchString(a.market):
		return createArgs{}, usagef("--market must be a two-letter upper-case country code such as ZW")
	}
	return a, nil
}

func parseSetPassword(args []string) (string, error) {
	fs := newFlagSet("set-password")
	var email string
	fs.StringVar(&email, "email", "", "")
	if err := fs.Parse(args); err != nil {
		return "", usagef("%v", err)
	}
	if fs.NArg() > 0 {
		return "", usagef("unexpected argument %q", fs.Arg(0))
	}
	return validEmail(email)
}

func validEmail(raw string) (string, error) {
	email := identity.NormalizeEmail(raw)
	switch {
	case email == "":
		return "", usagef("--email is required")
	case len(email) > 254 || !emailRe.MatchString(email):
		return "", usagef("--email is not a valid email address")
	}
	return email, nil
}

// readPassword obtains and validates the password. The value is never
// included in an error message.
func readPassword(env Env) (string, error) {
	password := env.Getenv(PasswordEnv)
	if password == "" {
		var err error
		if env.ReadPassword != nil {
			password, err = env.ReadPassword()
		} else {
			password, err = FirstLine(env.Stdin)
		}
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
	}
	switch n := utf8.RuneCountInString(password); {
	case password == "":
		return "", usagef("no password supplied: set %s or provide it on standard input", PasswordEnv)
	case !utf8.ValidString(password):
		return "", usagef("the password contains invalid characters")
	case n < identity.MinPasswordLength:
		return "", usagef("the password must be at least %d characters", identity.MinPasswordLength)
	case n > identity.MaxPasswordLength:
		return "", usagef("the password must be at most %d characters", identity.MaxPasswordLength)
	}
	return password, nil
}

// FirstLine reads one line (without its line ending) from r.
func FirstLine(r io.Reader) (string, error) {
	if r == nil {
		return "", nil
	}
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// Run executes the command. Everything is validated before the database is
// opened, and the password is hashed with Argon2id before it goes anywhere.
func Run(ctx context.Context, env Env) error {
	if len(env.Args) == 0 {
		return usagef("a command is required")
	}
	switch env.Args[0] {
	case "create":
		a, err := parseCreate(env.Args[1:])
		if err != nil {
			return err
		}
		hash, err := passwordHash(env)
		if err != nil {
			return err
		}
		id, err := uuid.NewV7()
		if err != nil {
			return err
		}
		store, closeStore, err := env.OpenStore(ctx)
		if err != nil {
			return err
		}
		defer closeStore()
		err = store.CreatePlatformAdmin(ctx, identity.NewPlatformAdmin{
			ID: id, Name: a.name, Email: a.email, PasswordHash: hash,
			Role: a.role, MarketCode: a.market, CreatedAt: env.Now().UTC().Truncate(time.Microsecond),
		})
		switch {
		case errors.Is(err, identity.ErrEmailTaken):
			return fmt.Errorf("an account with email %s already exists; adminctl does not promote existing accounts", a.email)
		case errors.Is(err, identity.ErrMarketNotFound):
			return fmt.Errorf("market %s does not exist", a.market)
		case err != nil:
			return fmt.Errorf("create administrator: %w", err)
		}
		scope := ""
		if a.market != "" {
			scope = " for market " + a.market
		}
		fmt.Fprintf(env.Stdout, "created administrator %s (%s) with role %s%s; email marked verified\n", a.email, id, a.role, scope)
		return nil

	case "set-password":
		email, err := parseSetPassword(env.Args[1:])
		if err != nil {
			return err
		}
		hash, err := passwordHash(env)
		if err != nil {
			return err
		}
		store, closeStore, err := env.OpenStore(ctx)
		if err != nil {
			return err
		}
		defer closeStore()
		err = store.SetPlatformAdminPassword(ctx, email, hash, env.Now().UTC().Truncate(time.Microsecond))
		switch {
		case errors.Is(err, identity.ErrNotFound):
			return fmt.Errorf("no account with email %s", email)
		case errors.Is(err, identity.ErrNotPlatformAdmin):
			return fmt.Errorf("%s is not a platform administrator; adminctl only changes administrator passwords", email)
		case err != nil:
			return fmt.Errorf("set password: %w", err)
		}
		fmt.Fprintf(env.Stdout, "password changed for %s; all of their sessions were revoked\n", email)
		return nil

	case "help", "-h", "--help":
		fmt.Fprintln(env.Stdout, usage)
		return nil
	}
	return usagef("unknown command %q", env.Args[0])
}

func passwordHash(env Env) (string, error) {
	password, err := readPassword(env)
	if err != nil {
		return "", err
	}
	hash, err := env.Hasher.Hash(password)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	return hash, nil
}
