package apitoken

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Creators of tokens that no other token created (ADR 0030 T3,
// api_token.created_by).
const (
	// CreatedByBootstrap marks the token of api.bootstrap_token_file.
	CreatedByBootstrap = "bootstrap"
	// CreatedByLocal marks a token of `angelia token create --local`.
	CreatedByLocal = "local"
)

// BootstrapName is the name of the token of api.bootstrap_token_file.
const BootstrapName = "bootstrap"

// NoAdminWarning is the start-up warning of the api role while no active
// admin token exists (ADR 0030 T4). It holds no secret.
const NoAdminWarning = `no active admin API token; create one with "angelia token create --local --scope admin --name admin" (docs/configuration.md#api-tokens)`

// maxBootstrapFile bounds the read of the bootstrap file: a token, a newline
// and some room, so that a wrong path to a large file is not read whole.
const maxBootstrapFile = 4096

// ReadBootstrapFile reads the bootstrap token file at path (ADR 0030 T4): one
// token in the format of T1, optionally followed by one newline ("\n" or
// "\r\n"). It follows symbolic links, as Kubernetes secret volumes are links,
// and checks the opened file: it must be a regular file, and on Unix group
// or others must not be able to write it. A file that group or others can
// read is accepted with a warning, returned for the caller to log, because
// Kubernetes secret volumes default to mode 0644. No error or warning ever
// holds the file's content.
func ReadBootstrapFile(path string) (id, token, warning string, err error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's bootstrap file.
	if err != nil {
		return "", "", "", fmt.Errorf("api.bootstrap_token_file: %w", err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return "", "", "", fmt.Errorf("api.bootstrap_token_file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", "", "", fmt.Errorf("api.bootstrap_token_file %s: not a regular file", path)
	}
	if runtime.GOOS != "windows" {
		perm := info.Mode().Perm()
		switch {
		case perm&0o022 != 0:
			return "", "", "", fmt.Errorf("api.bootstrap_token_file %s is writable by group or others (mode %04o), so another user could replace the admin token; run: chmod 600 %s", path, perm, path)
		case perm&0o044 != 0:
			warning = fmt.Sprintf("api.bootstrap_token_file %s is readable by group or others (mode %04o); it holds an admin token: chmod 600 it, or revoke the bootstrap token once you have created your own admin token", path, perm)
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxBootstrapFile+1))
	if err != nil {
		return "", "", "", fmt.Errorf("api.bootstrap_token_file %s: %w", path, err)
	}
	s := string(data)
	if t, ok := strings.CutSuffix(s, "\n"); ok {
		s = strings.TrimSuffix(t, "\r")
	}
	id, err = Parse(s)
	if err != nil {
		return "", "", "", fmt.Errorf("api.bootstrap_token_file %s: not an API token: want one token from `angelia token generate` and at most one newline (the content is not shown)", path)
	}
	return id, s, warning, nil
}

// Bootstrap stores the token of the bootstrap file at path as an admin token
// named "bootstrap" when the token table has never held a row (ADR 0030 T4).
// The api role calls it at start; an empty path does nothing.
//
//   - A file that cannot be read, or does not hold a token, is an error: the
//     role does not start. The error never holds the content.
//   - With the table empty, the token is inserted. [store.ErrConflict] means
//     another replica inserted it (or another bootstrap token) first: the
//     bootstrap is done, and the start continues.
//   - With rows in the table, revoked or expired ones included, nothing is
//     inserted, so a revoked bootstrap token never comes back. If the file's
//     token is not among them the file is ignored, which is logged.
//
// It logs the token's id, never the token.
func Bootstrap(ctx context.Context, st store.TokenStore, path string, now time.Time, log *slog.Logger) error {
	if path == "" {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	id, token, warning, err := ReadBootstrapFile(path)
	if err != nil {
		return err
	}
	if warning != "" {
		log.WarnContext(ctx, warning)
	}
	n, err := st.CountAPITokens(ctx)
	if err != nil {
		return fmt.Errorf("api token bootstrap: %w", err)
	}
	if n > 0 {
		_, err := st.APITokenByID(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			log.InfoContext(ctx, "api.bootstrap_token_file ignored: API tokens already exist and its token is not one of them", "id", id)
		case err != nil:
			return fmt.Errorf("api token bootstrap: %w", err)
		}
		return nil
	}
	err = st.CreateAPIToken(ctx, store.APIToken{
		ID: id, Name: BootstrapName, Scope: store.TokenScopeAdmin, Hash: Hash(token),
		CreatedAt: now, CreatedBy: CreatedByBootstrap,
	})
	switch {
	case errors.Is(err, store.ErrConflict):
		log.InfoContext(ctx, "bootstrap admin token already stored by another process", "id", id)
	case err != nil:
		return fmt.Errorf("api token bootstrap: %w", err)
	default:
		log.InfoContext(ctx, "bootstrap admin token stored", "id", id)
	}
	return nil
}

// WarnIfNoAdmin logs [NoAdminWarning] once when no admin token is active at
// now: none, or only revoked or expired ones (ADR 0030 T4).
func WarnIfNoAdmin(ctx context.Context, st store.TokenReader, now time.Time, log *slog.Logger) error {
	n, err := st.CountActiveAdminTokens(ctx, now)
	if err != nil {
		return fmt.Errorf("count admin API tokens: %w", err)
	}
	if n == 0 {
		if log == nil {
			log = slog.Default()
		}
		log.WarnContext(ctx, NoAdminWarning)
	}
	return nil
}
