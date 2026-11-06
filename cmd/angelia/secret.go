package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/secrets"
)

// `angelia secret …`: stored secrets through the API (ADR 0024 §7). The
// value is read from a file or stdin, never from an argument, and is never
// printed: the commands show metadata only.

func newSecretCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage stored secrets through the API (values are write-only)",
	}
	addClientFlags(cmd)
	cmd.AddCommand(
		newSecretSetCmd(),
		newListCmd("secret", func(*cobra.Command) func(*apiclient.API, *int) fetchPage[apiclient.Secret] {
			return func(a *apiclient.API, limit *int) fetchPage[apiclient.Secret] {
				return func(ctx context.Context, cursor *string) ([]apiclient.Secret, *string, error) {
					var out apiclient.SecretPage
					resp, err := a.ListSecrets(ctx, &apiclient.ListSecretsParams{Limit: limit, Cursor: cursor}) //nolint:bodyclose // Decode closes it
					_, err = apiclient.Decode(resp, err, &out)
					return out.Items, out.NextCursor, err
				}
			}
		}, secretTable),
		newGetCmd("secret", "NAME", func(ctx context.Context, a *apiclient.API, n string) (*http.Response, error) {
			return a.GetSecret(ctx, n)
		}, secretDetail),
		newDeleteCmd("secret", func(ctx context.Context, a *apiclient.API, n string, dry *bool) (*http.Response, error) {
			return a.DeleteSecret(ctx, n, &apiclient.DeleteSecretParams{DryRun: dry})
		}),
	)
	return cmd
}

func newSecretSetCmd() *cobra.Command {
	var from string
	var hosts []string
	var dry bool
	cmd := &cobra.Command{
		Use:   "set NAME --from-file PATH|-",
		Short: "Store a secret's value, read from a file or stdin",
		Long: `Store a secret's value, encrypted at rest by the instance, under NAME.

The value is read from --from-file (a path, or - for stdin), never from an
argument, and is never printed. Exactly one trailing newline (\n or \r\n) is
stripped, because editors end files with one; anything else, including other
whitespace, is stored as is. The value is at most 64 KiB, and any bytes are
allowed. --host binds the secret to the hosts it may be sent to (repeatable).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !secrets.ValidName(args[0]) {
				return errors.New("NAME must match ^[a-z0-9][a-z0-9._-]{0,62}$")
			}
			for _, h := range hosts {
				if !secrets.ValidHost(h) {
					return fmt.Errorf("--host %s: must be host or host:port, the host a lower-case hostname (punycode) or an IP literal, with no wildcard", h)
				}
			}
			value, err := readSecretValue(cmd.InOrStdin(), from)
			if err != nil {
				return err
			}
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			body, err := secretWriteBody(value, hosts)
			if err != nil {
				return err
			}
			var v apiclient.Secret
			resp, err := e.API.PutSecretWithBody(cmd.Context(), args[0], &apiclient.PutSecretParams{DryRun: dryRunPtr(dry)}, jsonContentType, bytes.NewReader(body)) //nolint:bodyclose // Decode closes it
			res, err := apiclient.Decode(resp, err, &v)
			if err != nil {
				return err
			}
			if err := render(e.Out, e.Format, v, secretDetail(v)); err != nil {
				return err
			}
			e.reportChange("secret", args[0], res, dry)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from-file", "", "file holding the value, or - for stdin (one trailing newline is stripped)")
	cmd.Flags().StringArrayVar(&hosts, "host", nil, "host or host:port the secret may be sent to (repeatable)")
	cmd.Flags().BoolVar(&dry, "dry-run", false, "validate without storing")
	_ = cmd.MarkFlagRequired("from-file")
	return cmd
}

// readSecretValue reads the value from a file or stdin ("-"), strips exactly
// one trailing newline (\n or \r\n), and bounds its size. Errors never
// quote the value.
func readSecretValue(in io.Reader, path string) (secrets.Value, error) {
	r := in
	what := "stdin"
	if path != "-" {
		f, err := os.Open(path) //nolint:gosec // the file the user named
		if err != nil {
			return secrets.Value{}, fmt.Errorf("read the secret: %w", err)
		}
		defer func() { _ = f.Close() }()
		r, what = f, path
	}
	raw, err := io.ReadAll(io.LimitReader(r, secrets.MaxValueBytes+3))
	if err != nil {
		return secrets.Value{}, fmt.Errorf("read the secret from %s: %w", what, err)
	}
	defer clear(raw)
	raw = stripOneNewline(raw)
	switch {
	case len(raw) == 0:
		return secrets.Value{}, fmt.Errorf("the secret read from %s is empty", what)
	case len(raw) > secrets.MaxValueBytes:
		return secrets.Value{}, fmt.Errorf("the secret read from %s is larger than %d bytes", what, secrets.MaxValueBytes)
	}
	return secrets.NewValue(raw), nil
}

// stripOneNewline removes exactly one trailing "\n" or "\r\n".
func stripOneNewline(b []byte) []byte {
	if n := len(b); n > 0 && b[n-1] == '\n' {
		b = b[:n-1]
		if n := len(b); n > 0 && b[n-1] == '\r' {
			b = b[:n-1]
		}
	}
	return b
}

// secretWriteBody is the PUT body: the value as value_base64, so that any
// bytes survive, and the hosts.
func secretWriteBody(v secrets.Value, hosts []string) ([]byte, error) {
	if hosts == nil {
		hosts = []string{}
	}
	plain := v.Reveal()
	defer clear(plain)
	return json.Marshal(struct {
		ValueBase64 string   `json:"value_base64"`
		Hosts       []string `json:"hosts"`
	}{base64.StdEncoding.EncodeToString(plain), hosts})
}

func secretTable(ss []apiclient.Secret) table {
	t := table{Header: []string{"NAME", "HOSTS", "KEY ID", "REFERENCED BY", "UPDATED"}}
	for _, s := range ss {
		t.Rows = append(t.Rows, []string{s.Name, hostsCell(s.Hosts), s.KeyId, referrersCell(s.ReferencedBy), fmtTime(s.UpdatedAt)})
	}
	return t
}

func secretDetail(s apiclient.Secret) table {
	return detail("name", s.Name, "hosts", hostsCell(s.Hosts), "key id", s.KeyId,
		"referenced by", referrersCell(s.ReferencedBy), "created", fmtTime(s.CreatedAt), "updated", fmtTime(s.UpdatedAt))
}

func hostsCell(h []string) string {
	if len(h) == 0 {
		return "(any)"
	}
	return strings.Join(h, ",")
}

func referrersCell(rs []apiclient.SecretReferrer) string {
	if len(rs) == 0 {
		return "-"
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("%s/%s%s", r.Resource, r.Name, r.Pointer)
	}
	return strings.Join(parts, ",")
}
