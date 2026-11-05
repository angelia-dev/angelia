package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

func newConfigCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect the instance configuration",
	}
	cmd.AddCommand(newConfigCheckCmd())
	return cmd
}

func newConfigCheckCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate the instance configuration and exit non-zero if invalid",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := cmd.Flags().GetString("config")
			if err != nil {
				return err
			}
			if path == "" {
				path = os.Getenv("ANGELIA_CONFIG")
			}
			cfg, err := config.Load(cmd.Context(), config.Options{Path: path, Environ: os.Environ()})
			if err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			for _, w := range cfg.Warnings() {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			secretsLine, err := checkSecretsConfig(cmd.Context(), cfg, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			plugins, warns, err := config.LoadPlugins(cmd.Context(), cfg.PluginsDir, os.Environ())
			for _, w := range warns {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			unreadable := false
			if errors.Is(err, config.ErrPluginsDirUnreadable) && cfg.PluginsDirIsDefault() {
				// The default folder may belong to the service user; only a
				// configured plugins_dir must be readable (#184).
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v; plugins not checked\n", err)
				err, unreadable = nil, true
			}
			if err != nil {
				return fmt.Errorf("invalid plugins: %w", err)
			}
			show := func(w io.Writer) error {
				if unreadable {
					if _, err := fmt.Fprintf(w, "plugins in %s: not readable, not checked\n", cfg.PluginsDir); err != nil {
						return err
					}
				} else if err := printPlugins(w, cfg.PluginsDir, plugins); err != nil {
					return err
				}
				_, err := fmt.Fprintln(w, secretsLine)
				return err
			}
			watches, err := watchesPath(cmd)
			if err != nil {
				return err
			}
			if watches == "" {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), "configuration OK"); err != nil {
					return err
				}
				return show(cmd.OutOrStdout())
			}
			f, err := parseWatchFile(cfg, watches)
			if err != nil {
				return err
			}
			for _, w := range f.Warnings {
				_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "configuration OK; watch file OK: %d sinks, %d watches, %d subscriptions\n", len(f.Sinks), len(f.Watches), len(f.Subscriptions)); err != nil {
				return err
			}
			return show(cmd.OutOrStdout())
		},
	}
	cmd.Flags().String("watches", "", watchesFlagHelp)
	return cmd
}

// printPlugins shows, for each installed plugin, the hosts it may reach and
// the hosts that get a credential, with the header and scheme (#184 Q4).
// It names the variable that holds a token, never the token.
func printPlugins(w io.Writer, dir string, plugins []config.InstalledPlugin) error {
	var b strings.Builder
	if len(plugins) == 0 {
		fmt.Fprintf(&b, "plugins in %s: none\n", dir)
	} else {
		fmt.Fprintf(&b, "plugins in %s: %d\n", dir, len(plugins))
	}
	for _, p := range plugins {
		fmt.Fprintf(&b, "  %s\n", p.Kind)
		var hosts []string
		from := map[string][]string{}
		add := func(h, origin string) {
			if _, seen := from[h]; !seen {
				hosts = append(hosts, h)
			}
			from[h] = append(from[h], origin)
		}
		for _, h := range p.ManifestHosts {
			add(printable(strings.TrimSuffix(strings.ToLower(h), ".")), "manifest")
		}
		for _, h := range p.Grants.AllowedHosts {
			add(h, "grants")
		}
		if len(hosts) == 0 {
			b.WriteString("    allowed hosts: none\n")
		} else {
			parts := make([]string, len(hosts))
			for i, h := range hosts {
				parts[i] = fmt.Sprintf("%s (%s)", h, strings.Join(from[h], ", "))
			}
			fmt.Fprintf(&b, "    allowed hosts: %s\n", strings.Join(parts, ", "))
		}
		keys := make([]string, 0, len(p.Grants.Credentials))
		for k := range p.Grants.Credentials {
			keys = append(keys, k)
		}
		if len(keys) == 0 {
			b.WriteString("    credentials: none\n")
			continue
		}
		slices.Sort(keys)
		b.WriteString("    credentials:\n")
		for _, k := range keys {
			c := p.Grants.Credentials[k]
			hostPort := k
			if _, _, err := net.SplitHostPort(k); err != nil {
				hostPort = net.JoinHostPort(k, "443")
			}
			header := c.Header
			if header == "" {
				header = "Authorization"
			}
			scheme := ""
			if c.Scheme != "" {
				scheme = c.Scheme + " "
			}
			unset := ""
			if p.Token(k) == "" {
				unset = ", not set"
			}
			fmt.Fprintf(&b, "      %s  %s: %s<token from $%s%s>\n", hostPort, header, scheme, c.TokenEnv, unset)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// printable quotes a manifest value that is not plain printable ASCII, so
// that a plugin's manifest cannot write control characters to a terminal.
func printable(s string) string {
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			return strconv.Quote(s)
		}
	}
	return s
}

// checkSecretsConfig reads the secrets keys, as serve does (a bad or loose
// key file fails the check), and counts the stored secrets read-only,
// without creating or migrating the database. It returns the summary line
// ("secrets: no key configured (0 stored)") and fails, like serve would,
// when stored secrets have no configured key.
func checkSecretsConfig(ctx context.Context, cfg *config.Config, warn io.Writer) (string, error) {
	keys, warns, err := secrets.LoadKeyring(cfg.Secrets.KeyConfig())
	if err != nil {
		return "", fmt.Errorf("invalid secrets key: %w", err)
	}
	for _, w := range warns {
		_, _ = fmt.Fprintln(warn, "warning:", w)
	}
	counts := map[string]int64{}
	dbPath, err := cfg.Database.SQLitePath()
	if err != nil {
		// TODO(postgres, #85): count the Postgres store too.
		return keysLine(keys) + "; stored secrets not checked (postgres)", nil //nolint:nilerr // only sqlite stores are counted in this phase
	}
	c, err := sqlite.SecretKeyCounts(ctx, dbPath)
	switch {
	case errors.Is(err, sqlite.ErrNoDatabase):
	case err != nil:
		_, _ = fmt.Fprintf(warn, "warning: secrets: stored secrets not checked: %v\n", err)
	default:
		counts = c
	}
	if err := secrets.CheckCounts(counts, keys); err != nil {
		return "", err
	}
	return secrets.Summary(counts, keys), nil
}

// keysLine names the configured keys, for a store whose secrets are not
// counted.
func keysLine(keys *secrets.Keyring) string {
	if keys == nil {
		return "secrets: no key configured"
	}
	s := "secrets: key " + keys.PrimaryID()
	if prev := len(keys.IDs()) - 1; prev > 0 {
		s += fmt.Sprintf(" and %d previous key%s", prev, plural(prev))
	}
	return s
}
