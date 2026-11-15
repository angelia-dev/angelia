package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apitoken"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/open"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// `angelia token …`: API tokens (story #72, ADR 0030 T7). One group for
// both paths: `generate` is offline and `create --local` writes to the
// instance's database directly; create, list and revoke through the API
// (72-F) join the same group, added to newTokenCmd.

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage API tokens",
		Long: `Manage the bearer tokens of the REST API. A token is shown once, when it is
created; the instance stores only its hash. See docs/configuration.md, "API tokens".`,
	}
	cmd.AddCommand(newTokenGenerateCmd(), newTokenCreateCmd())
	return cmd
}

func newTokenGenerateCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Print a new API token for api.bootstrap_token_file (offline)",
		Long: `Print a new API token and a newline, for api.bootstrap_token_file. Nothing is
stored: the api role stores the token as the admin token "bootstrap" when it
starts with an empty token table. With --out the token is written to a new file
with mode 0600 (an existing file is never overwritten). The token is an admin
credential until it is revoked: keep the file as secret as the token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			id, tok := apitoken.Generate(time.Now())
			if out == "" {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), tok)
				return err
			}
			if err := writeNewFile(out, tok+"\n"); err != nil {
				return fmt.Errorf("token generate: %w", err)
			}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote a new API token (id %s) to %s (mode 0600)\n", id, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the token to this new file (mode 0600) instead of stdout")
	return cmd
}

// writeNewFile creates path with mode 0600 and writes content; it refuses to
// overwrite a file, and removes the file it created when the write fails, so
// no partial token is left behind.
func writeNewFile(path, content string) error {
	return writeNewFileWith(path, content, func(f *os.File, c string) error {
		_, err := f.WriteString(c)
		return err //nolint:wrapcheck // the caller prefixes it
	})
}

func writeNewFileWith(path, content string, write func(*os.File, string) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's path.
	if err != nil {
		return err //nolint:wrapcheck // the caller prefixes it
	}
	err = write(f, content)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return err //nolint:wrapcheck // the caller prefixes it
	}
	return nil
}

// tokenCreateFlags are the flags of `token create`, shared by both paths.
type tokenCreateFlags struct {
	local     bool
	name      string
	scope     string
	expiresAt string
	expiresIn string
}

func newTokenCreateCmd() *cobra.Command {
	var f tokenCreateFlags
	cmd := &cobra.Command{
		Use:   "create --local --name NAME [--scope read|write|admin] [--expires-in D | --expires-at T]",
		Short: "Create an API token and print it once",
		Long: `Create an API token. The token is printed alone on stdout, once, so that
$(angelia token create …) works; its id, name, scope and expiry go to stderr.

--local writes to the instance's database directly, without the API: it reads
database.dsn from the instance configuration (--config or ANGELIA_CONFIG), needs
no secrets key and never creates or migrates the database. It is how the first
admin token is made, and the recovery path when no admin token is left. The
token's creator is recorded as "local". With --local, --scope defaults to admin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !f.local {
				return errors.New("token create: only --local is available in this version (creating a token through the API comes with the token API)")
			}
			return runTokenCreateLocal(cmd, f, time.Now())
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.local, "local", false, "write to the instance's database directly (instance-side, no API)")
	fl.StringVar(&f.name, "name", "", "the token's name, unique among active tokens: ^[a-z0-9][a-z0-9._-]{0,62}$")
	fl.StringVar(&f.scope, "scope", "", "read, write or admin (default admin with --local)")
	fl.StringVar(&f.expiresAt, "expires-at", "", "expiry as an RFC 3339 time, such as 2027-01-31T00:00:00Z (default: never)")
	fl.StringVar(&f.expiresIn, "expires-in", "", "expiry from now, such as 90d or 720h (default: never)")
	cmd.MarkFlagsMutuallyExclusive("expires-at", "expires-in")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.RegisterFlagCompletionFunc("scope", func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
		return []string{store.TokenScopeRead, store.TokenScopeWrite, store.TokenScopeAdmin}, cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

// validate checks the flags and returns the expiry (nil: never), before
// anything is opened.
func (f tokenCreateFlags) validate(now time.Time) (*time.Time, error) {
	if !watchfile.ValidName(f.name) {
		return nil, fmt.Errorf("--name %q: must match %s", f.name, watchfile.NamePattern)
	}
	switch f.scope {
	case store.TokenScopeRead, store.TokenScopeWrite, store.TokenScopeAdmin:
	default:
		return nil, fmt.Errorf("--scope %q: want read, write or admin", f.scope)
	}
	return tokenExpiry(f.expiresAt, f.expiresIn, now)
}

// tokenExpiry parses --expires-at (RFC 3339) or --expires-in (a Go duration or
// a number of days, such as 90d) into a time after now, to the millisecond
// the store keeps. Neither: nil, no expiry.
func tokenExpiry(at, in string, now time.Time) (*time.Time, error) {
	var t time.Time
	switch {
	case at != "":
		v, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, fmt.Errorf("--expires-at %q: want an RFC 3339 time such as 2027-01-31T00:00:00Z", at)
		}
		t = v
	case in != "":
		var d time.Duration
		if days, ok := strings.CutSuffix(in, "d"); ok {
			n, err := strconv.Atoi(days)
			if err != nil || n <= 0 || n > 36500 {
				return nil, fmt.Errorf("--expires-in %q: want a number of days from 1 to 36500, such as 90d", in)
			}
			d = time.Duration(n) * 24 * time.Hour
		} else {
			v, err := time.ParseDuration(in)
			if err != nil {
				return nil, fmt.Errorf("--expires-in %q: want a duration such as 90d or 720h", in)
			}
			d = v
		}
		t = now.Add(d)
	default:
		return nil, nil //nolint:nilnil // no expiry
	}
	t = t.UTC().Truncate(time.Millisecond)
	if !t.After(now) {
		return nil, fmt.Errorf("the expiry %s is not in the future", t.Format(time.RFC3339))
	}
	return &t, nil
}

// runTokenCreateLocal is `token create --local`: it opens the store through
// open.TokenWriter (no keyring, no migration, schema 00021 or newer).
func runTokenCreateLocal(cmd *cobra.Command, f tokenCreateFlags, now time.Time) error {
	if f.scope == "" {
		f.scope = store.TokenScopeAdmin
	}
	expires, err := f.validate(now)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(cmd)
	if err != nil {
		return err
	}
	st, err := open.TokenWriter(cmd.Context(), cfg.Database.DSN)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	id, tok := apitoken.Generate(now)
	err = st.CreateAPIToken(cmd.Context(), store.APIToken{
		ID: id, Name: f.name, Scope: f.scope, Hash: apitoken.Hash(tok),
		CreatedAt: now.UTC().Truncate(time.Millisecond), CreatedBy: apitoken.CreatedByLocal, ExpiresAt: expires,
	})
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("token create: an active token is already named %q; pick another name or revoke that token", f.name)
	}
	if err != nil {
		return fmt.Errorf("token create: %w", err)
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), tok); err != nil {
		return err
	}
	exp := "never"
	if expires != nil {
		exp = expires.Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "created API token %s (name %s, scope %s, expires %s)\nthe token is shown once: store it now\n", id, f.name, f.scope, exp)
	return nil
}

// bootstrapAPITokens runs at api role start (ADR 0030 T4): it stores the
// token of api.bootstrap_token_file if the token table has never held a
// row, then warns while no admin token is active. Neither logs a token.
func bootstrapAPITokens(ctx context.Context, cfg *config.Config, st store.TokenStore, log *slog.Logger) error {
	now := time.Now()
	if err := apitoken.Bootstrap(ctx, st, cfg.API.BootstrapTokenFile, now, log); err != nil {
		return err //nolint:wrapcheck // its messages name api.bootstrap_token_file
	}
	return apitoken.WarnIfNoAdmin(ctx, st, now, log) //nolint:wrapcheck // prefixed
}
