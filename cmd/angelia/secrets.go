package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// `angelia secrets …`: the instance-side commands of ADR 0024 §5. They read
// the instance configuration and the store directly, like `serve`; none is
// an API call, and none prints a stored value. keygen is the only command
// that ever prints key material.

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Manage the keys of the stored secrets (instance-side)",
	}
	cmd.AddCommand(newSecretsKeygenCmd(), newSecretsRotateCmd(), newSecretsStatusCmd())
	return cmd
}

func newSecretsKeygenCmd() *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "keygen",
		Short: "Create a new secrets key (32 random bytes, base64)",
		Long: `Create a new secrets key: the standard base64 encoding of 32 random bytes and a
newline, the format of secrets.key_file. With --out the key is written to a
new file with mode 0600 (an existing file is never overwritten); without it,
the key is printed to stdout. Back the key up apart from the database: without
it the stored secrets cannot be read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out != "" {
				if err := secrets.WriteKeyFile(out); err != nil {
					return err
				}
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "wrote a new secrets key to %s (mode 0600)\n", out)
				return nil
			}
			k, err := secrets.GenerateKey()
			if err != nil {
				return err
			}
			plain := k.Reveal()
			defer clear(plain)
			_, err = cmd.OutOrStdout().Write(plain)
			return err
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write the key to this new file (mode 0600) instead of stdout")
	return cmd
}

func newSecretsRotateCmd() *cobra.Command {
	var dry bool
	var batch int
	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Re-encrypt every stored secret under the primary key",
		Long: `Re-encrypt every stored secret that another configured key sealed under the
primary key (secrets.key_file). Configure the new key as secrets.key_file and
the old one in secrets.previous_key_files first (docs/configuration.md,
"Secrets"). Each batch is one transaction: a crash leaves every row readable,
and running the command again is safe. Values are never printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := openInstance(cmd)
			if err != nil {
				return err
			}
			defer in.close()
			if in.keys == nil {
				return errors.New("secrets: no key configured; set secrets.key_file to the new key and secrets.previous_key_files to the old ones")
			}
			rep, err := secrets.Rotate(cmd.Context(), in.store, in.keys, secrets.RotateOptions{DryRun: dry, Batch: batch, Logger: in.log})
			w := cmd.OutOrStdout()
			verb := "re-encrypted"
			if dry {
				verb = "would re-encrypt"
			}
			n := 0
			for _, c := range rep.From {
				n += c
			}
			_, _ = fmt.Fprintf(w, "%s %d secret%s under key %s\n", verb, n, plural(n), rep.PrimaryID)
			for _, id := range slices.Sorted(maps.Keys(rep.From)) {
				_, _ = fmt.Fprintf(w, "  from key %s: %d\n", id, rep.From[id])
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&dry, "dry-run", false, "report what would be re-encrypted without writing")
	cmd.Flags().IntVar(&batch, "batch", 0, "rows per transaction (0: all in one)")
	return cmd
}

func newSecretsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show how many stored secrets each key sealed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			in, err := openInstance(cmd)
			if err != nil {
				return err
			}
			defer in.close()
			counts, err := in.store.CountSecretsByKeyID(cmd.Context())
			if err != nil {
				return err
			}
			return printSecretsStatus(cmd.OutOrStdout(), counts, in.keys)
		},
	}
}

// printSecretsStatus prints one line per key id: rows, and whether it is
// configured. It fails when rows are sealed with a key that is not.
func printSecretsStatus(w io.Writer, counts map[string]int64, keys *secrets.Keyring) error {
	var b strings.Builder
	b.WriteString(secrets.Summary(counts, keys) + "\n")
	t := table{Header: []string{"KEY ID", "ROWS", "CONFIGURED"}}
	for _, s := range secrets.Status(counts, keys) {
		role := s.Role
		if role == "" {
			role = "no: add its key file to secrets.previous_key_files"
		}
		t.Rows = append(t.Rows, []string{s.KeyID, fmt.Sprint(s.Rows), role})
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return err
	}
	if len(t.Rows) > 0 {
		if err := writeTable(w, t); err != nil {
			return err
		}
	}
	for _, s := range secrets.Status(counts, keys) {
		if s.Role == "previous" && s.Rows == 0 {
			_, _ = fmt.Fprintf(w, "key %s seals no row: it can leave secrets.previous_key_files (keep the file archived while older backups exist)\n", s.KeyID)
		}
	}
	return secrets.CheckCounts(counts, keys)
}

// instance is what an instance-side command opens.
type instance struct {
	cfg   *config.Config
	keys  *secrets.Keyring
	store *sqlite.Store
	log   *slog.Logger
}

func (i *instance) close() { _ = i.store.Close() }

// openInstance loads the instance configuration and its keys (then unsets
// ANGELIA_SECRETS_KEY) and opens the SQLite store.
func openInstance(cmd *cobra.Command) (*instance, error) {
	cfg, err := loadConfig(cmd)
	if err != nil {
		return nil, err
	}
	log := newLogger(cmd.ErrOrStderr(), cfg.Log)
	keys, err := loadKeys(cfg, log)
	if err != nil {
		return nil, err
	}
	dbPath, err := cfg.Database.SQLitePath()
	if err != nil {
		return nil, fmt.Errorf("only sqlite databases are supported in this phase: %w", err)
	}
	st, err := sqlite.Open(cmd.Context(), dbPath, log)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	return &instance{cfg: cfg, keys: keys, store: st, log: log}, nil
}

// loadConfig reads the instance configuration named by --config or
// ANGELIA_CONFIG.
func loadConfig(cmd *cobra.Command) (*config.Config, error) {
	path, err := cmd.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	if path == "" {
		path = os.Getenv("ANGELIA_CONFIG")
	}
	cfg, err := config.Load(cmd.Context(), config.Options{Path: path, Environ: os.Environ()})
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// loadKeys reads the secrets keys of cfg and logs the warnings. It then
// unsets ANGELIA_SECRETS_KEY, so that no child process started later
// inherits it. That does not scrub /proc/self/environ or the container
// runtime's record of the variable (docs/configuration.md); the key file
// is the documented default.
func loadKeys(cfg *config.Config, log *slog.Logger) (*secrets.Keyring, error) {
	keys, warns, err := secrets.LoadKeyring(cfg.Secrets.KeyConfig())
	_ = os.Unsetenv(config.KeyEnv)
	if err != nil {
		return nil, err
	}
	for _, w := range warns {
		log.Warn(w)
	}
	return keys, nil
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
