package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/angelia-dev/angelia/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/apitoken"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// `angelia token create|list|revoke` through the API (72-F, ADR 0030 T7).
// None of these prints or logs a token except `create`, which prints the new
// one on stdout; an error message never contains a token.

// ulidRE is the id of a token: 26 characters of upper-case Crockford base32.
var ulidRE = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

// runTokenCreateAPI is `token create` without --local: POST /v1/tokens with
// the caller's admin token.
func runTokenCreateAPI(cmd *cobra.Command, f tokenCreateFlags, now time.Time) error {
	if f.scope == "" {
		f.scope = store.TokenScopeRead // least privilege; --local defaults to admin
	}
	expires, err := f.validate(now)
	if err != nil {
		return err
	}
	e, err := newCLIEnv(cmd)
	if err != nil {
		return err
	}
	body := apiclient.TokenCreate{Name: f.name, Scope: apiclient.TokenScope(f.scope), ExpiresAt: expires}
	var out apiclient.TokenCreated
	resp, err := e.API.CreateToken(cmd.Context(), body) //nolint:bodyclose // Decode closes it
	if _, err := apiclient.Decode(resp, err, &out); err != nil {
		return fmt.Errorf("token create: %w", err)
	}
	if out.Token == nil || *out.Token == "" {
		return errors.New("token create: the server's answer holds no token")
	}
	var werr error
	if e.Format == formatJSON {
		// The 201 body, secret included: as sensitive as the token itself.
		werr = writeJSON(e.Out, out)
	} else {
		_, werr = fmt.Fprintln(e.Out, *out.Token)
	}
	if werr != nil {
		reportTokenLost(e.Err, out.Id)
		return werr
	}
	reportTokenCreated(e.Err, out.Id, out.Name, string(out.Scope), out.ExpiresAt)
	return nil
}

// reportTokenCreated is the stderr half of `create`, the same for both paths.
func reportTokenCreated(w io.Writer, id, name, scope string, expires *time.Time) {
	exp := "never"
	if expires != nil {
		exp = expires.UTC().Format(time.RFC3339)
	}
	_, _ = fmt.Fprintf(w, "created API token %s (name %s, scope %s, expires %s)\nthe token is shown once: store it now\n", id, name, scope, exp)
}

// reportTokenLost tells the operator that a token exists whose secret never
// reached stdout, so that it can be revoked.
func reportTokenLost(w io.Writer, id string) {
	_, _ = fmt.Fprintf(w, "created token %s but could not print it; revoke it with `angelia token revoke %s`\n", id, id)
}

func newTokenListCmd() *cobra.Command {
	var lf listFlags
	var includeRevoked bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List API tokens (metadata only; needs an admin token)",
		Long: `List the instance's API tokens through the API: id, name, scope, creation,
expiry and last use. Revoked tokens are left out unless --include-revoked is
given; expired tokens are always listed. A token or its hash is never shown.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			fetch := tokenFetcher(e.API, lf.limitPtr(), includeRevoked)
			p, err := collect(cmd.Context(), fetch, lf.cursor, lf.all)
			if err != nil {
				return err
			}
			return e.printList(p, p.NextCursor, tokenTable(p.Items, includeRevoked))
		},
	}
	addClientFlags(cmd)
	lf.add(cmd)
	cmd.Flags().BoolVar(&includeRevoked, "include-revoked", false, "list revoked tokens too")
	return cmd
}

func tokenFetcher(a *apiclient.API, limit *int, includeRevoked bool) fetchPage[apiclient.Token] {
	return func(ctx context.Context, cursor *string) ([]apiclient.Token, *string, error) {
		p := &apiclient.ListTokensParams{Limit: limit, Cursor: cursor}
		if includeRevoked {
			p.IncludeRevoked = &includeRevoked
		}
		var out apiclient.TokenPage
		resp, err := a.ListTokens(ctx, p) //nolint:bodyclose // Decode closes it
		_, err = apiclient.Decode(resp, err, &out)
		return out.Items, out.NextCursor, err
	}
}

func tokenTable(items []apiclient.Token, revoked bool) table {
	t := table{Header: []string{"ID", "NAME", "SCOPE", "CREATED", "EXPIRES", "LAST USED"}}
	if revoked {
		t.Header = append(t.Header, "REVOKED")
	}
	for _, k := range items {
		row := []string{k.Id, k.Name, string(k.Scope), fmtTime(k.CreatedAt), timeCell(k.ExpiresAt), timeCell(k.LastUsedAt)}
		if revoked {
			row = append(row, timeCell(k.RevokedAt))
		}
		t.Rows = append(t.Rows, row)
	}
	return t
}

func newTokenRevokeCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "revoke ID|NAME",
		Short: "Revoke an API token (needs an admin token)",
		Long: `Revoke an API token by its id, or by the name of the active token. The token
stops working at once for new requests, and open event streams notice within a
minute. Revoking a revoked token changes nothing. Pass the id (the 26
characters after "ang_" in the token), never the token itself.

Revoking the last active admin token is refused unless --yes is given: without
an admin token the API cannot manage tokens (recover with
"angelia token create --local").`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := newCLIEnv(cmd)
			if err != nil {
				return err
			}
			id, err := resolveTokenID(cmd.Context(), e.API, args[0])
			if err != nil {
				return err
			}
			if err := checkLastAdmin(cmd.Context(), e.API, id, yes); err != nil {
				return err
			}
			resp, err := e.API.RevokeToken(cmd.Context(), id) //nolint:bodyclose // Decode closes it
			if _, err := apiclient.Decode(resp, err, nil); err != nil {
				return fmt.Errorf("token revoke: %w", err)
			}
			_, _ = fmt.Fprintf(e.Err, "revoked API token %s\n", id)
			return nil
		},
	}
	addClientFlags(cmd)
	cmd.Flags().BoolVar(&yes, "yes", false, "revoke the last active admin token too")
	return cmd
}

// checkLastAdmin refuses (without yes) to revoke the only active admin token
// (ADR 0030 T6): it lists the active tokens and counts the admin ones that
// have not expired. A target that is not an active admin token needs no check.
func checkLastAdmin(ctx context.Context, a *apiclient.API, id string, yes bool) error {
	if yes {
		return nil
	}
	items, err := collect(ctx, tokenFetcher(a, nil, false), "", true)
	if err != nil {
		return fmt.Errorf("token revoke: %w", err)
	}
	now := time.Now()
	active := func(k apiclient.Token) bool {
		return k.Scope == apiclient.TokenScopeAdmin && k.RevokedAt == nil && (k.ExpiresAt == nil || k.ExpiresAt.After(now))
	}
	target, others := false, 0
	for _, k := range items.Items {
		switch {
		case !active(k):
		case k.Id == id:
			target = true
		default:
			others++
		}
	}
	if target && others == 0 {
		return errors.New("token revoke: this is the last active admin token; pass --yes to revoke it (recover with `angelia token create --local`)")
	}
	return nil
}

// resolveTokenID turns the argument of `revoke` into a token id: an id as is,
// a name through the list of active tokens. The argument is echoed only when
// it is a valid id or name, so that a token pasted by mistake never reaches
// an error message or the terminal history of the output.
func resolveTokenID(ctx context.Context, a *apiclient.API, arg string) (string, error) {
	if config.LooksLikeToken(arg) || strings.HasPrefix(arg, apitoken.Prefix) {
		return "", errors.New("token revoke: that looks like a token, not an id or a name; pass the token's id (see `angelia token list`)")
	}
	if ulidRE.MatchString(arg) {
		return arg, nil
	}
	if !watchfile.ValidName(arg) {
		return "", errors.New("token revoke: want a token id (26 characters) or the name of an active token")
	}
	items, err := collect(ctx, tokenFetcher(a, nil, false), "", true)
	if err != nil {
		return "", fmt.Errorf("token revoke: %w", err)
	}
	for _, k := range items.Items {
		if k.Name == arg && k.RevokedAt == nil {
			return k.Id, nil
		}
	}
	return "", fmt.Errorf("token revoke: no active token is named %q", arg)
}
