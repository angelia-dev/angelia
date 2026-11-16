package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/apitoken"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// tokenAPI runs the real REST API with the real token authenticator on an
// empty SQLite store, with an admin and a read token.
type tokenAPI struct {
	srv         *httptest.Server
	st          *sqlite.Store
	logs        *bytes.Buffer
	admin, read string
	adminID     string
}

func newTokenAPI(t *testing.T) *tokenAPI {
	t.Helper()
	isolate(t)
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "api.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ta := &tokenAPI{st: st, logs: &logs}
	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	seed := func(name, scope string) (string, string) {
		id, tok := apitoken.Generate(now)
		if err := st.CreateAPIToken(t.Context(), store.APIToken{
			ID: id, Name: name, Scope: scope, Hash: apitoken.Hash(tok), CreatedAt: now, CreatedBy: "local",
		}); err != nil {
			t.Fatal(err)
		}
		return id, tok
	}
	ta.adminID, ta.admin = seed("admin", store.TokenScopeAdmin)
	_, ta.read = seed("reader", store.TokenScopeRead)
	sources, err := watchSources(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.New(api.Options{
		Store: st, Auth: apitoken.NewAuthenticator(st, log), Sources: sources,
		Plugins: builtinPlugins(sources.Kinds()), Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	ta.srv = httptest.NewServer(h)
	t.Cleanup(ta.srv.Close)
	return ta
}

// run runs the CLI against the server as token.
func (ta *tokenAPI) run(t *testing.T, token string, args ...string) (string, string, error) {
	t.Helper()
	return runCLI(t, append(append([]string{}, args...), "--server", ta.srv.URL, "--token", token)...)
}

func TestTokenCreateListRevokeAgainstAPI(t *testing.T) {
	ta := newTokenAPI(t)

	// create: the token alone on stdout, the details on stderr.
	out, errOut, err := ta.run(t, ta.admin, "token", "create", "--name", "ci", "--scope", "write", "--expires-in", "90d")
	if err != nil {
		t.Fatalf("create: %v\n%s", err, errOut)
	}
	tok, ok := strings.CutSuffix(out, "\n")
	if !ok || strings.Contains(tok, "\n") {
		t.Fatalf("stdout is not the token alone: %q", out)
	}
	id, err := apitoken.Parse(tok)
	if err != nil {
		t.Fatalf("stdout is not a token: %q", out)
	}
	for _, want := range []string{id, "name ci", "scope write", "shown once"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q: %q", want, errOut)
		}
	}
	if strings.Contains(errOut, tok) || strings.Contains(errOut, "expires never") {
		t.Errorf("stderr: %q", errOut)
	}
	row, err := ta.st.APITokenByID(t.Context(), id)
	if err != nil || row.Name != "ci" || row.Scope != "write" || row.CreatedBy != ta.adminID ||
		row.ExpiresAt == nil || time.Until(*row.ExpiresAt) < 89*24*time.Hour || !bytes.Equal(row.Hash, apitoken.Hash(tok)) {
		t.Fatalf("row %+v %v", row, err)
	}
	// The new token works.
	if _, errOut, err := ta.run(t, tok, "watch", "list"); err != nil {
		t.Fatalf("the new token: %v %s", err, errOut)
	}

	// The default scope is read.
	out, _, err = ta.run(t, ta.admin, "token", "create", "--name", "plain")
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := apitoken.Parse(strings.TrimSpace(out))
	if row, err := ta.st.APITokenByID(t.Context(), pid); err != nil || row.Scope != "read" || row.ExpiresAt != nil {
		t.Fatalf("default row %+v %v", row, err)
	}

	// -o json prints the 201 body, secret included.
	out, _, err = ta.run(t, ta.admin, "token", "create", "--name", "jq", "-o", "json", "--expires-at", "2099-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID, Name, Scope, Token string
		ExpiresAt              string `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("-o json: %v\n%s", err, out)
	}
	if _, err := apitoken.Parse(created.Token); err != nil || created.Name != "jq" || created.Scope != "read" || created.ExpiresAt != "2099-01-01T00:00:00Z" {
		t.Fatalf("-o json body: %+v", created)
	}

	// list: a table without any token, then json.
	out, _, err = ta.run(t, ta.admin, "token", "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "ID") || !strings.Contains(out, "LAST USED") || strings.Contains(out, "REVOKED") {
		t.Errorf("list header:\n%s", out)
	}
	for _, want := range []string{id, "ci", "write", "plain", "reader", ta.adminID} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, tok) || strings.Contains(out, "ang_") {
		t.Errorf("list shows a token:\n%s", out)
	}

	// revoke by id, then by name; revoked tokens leave the list.
	_, errOut, err = ta.run(t, ta.admin, "token", "revoke", id)
	if err != nil || !strings.Contains(errOut, "revoked API token "+id) {
		t.Fatalf("revoke by id: %v %q", err, errOut)
	}
	if _, _, err := ta.run(t, tok, "watch", "list"); err == nil {
		t.Fatal("the revoked token still works")
	}
	if _, errOut, err = ta.run(t, ta.admin, "token", "revoke", "plain"); err != nil || !strings.Contains(errOut, pid) {
		t.Fatalf("revoke by name: %v %q", err, errOut)
	}
	if _, _, err = ta.run(t, ta.admin, "token", "revoke", id); err != nil {
		t.Fatalf("revoking again: %v", err)
	}
	out, _, _ = ta.run(t, ta.admin, "token", "list")
	if strings.Contains(out, id) || strings.Contains(out, pid) {
		t.Errorf("revoked tokens listed:\n%s", out)
	}
	out, _, err = ta.run(t, ta.admin, "token", "list", "--include-revoked")
	if err != nil || !strings.Contains(out, "REVOKED") || !strings.Contains(out, id) || !strings.Contains(out, pid) {
		t.Errorf("--include-revoked: %v\n%s", err, out)
	}
	out, _, err = ta.run(t, ta.admin, "token", "list", "--include-revoked", "-o", "json", "--limit", "1", "--all")
	var page struct {
		Items []struct{ ID, RevokedAt string } `json:"items"`
	}
	if err != nil || json.Unmarshal([]byte(out), &page) != nil || len(page.Items) < 5 {
		t.Fatalf("list json: %v\n%s", err, out)
	}

	// Errors: unknown id, unknown name, a name taken, a token as the argument.
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"token", "revoke", "01JCZ8W3T6Q9N2H4K7M5R0V8XA"}, "404"},
		{[]string{"token", "revoke", "nobody"}, `no active token is named "nobody"`},
		{[]string{"token", "revoke", "Not A Name"}, "token id (26 characters)"},
		{[]string{"token", "revoke", tok}, "looks like a token"},
		{[]string{"token", "create", "--name", "admin"}, "422"},
		{[]string{"token", "create", "--name", "x", "--scope", "root"}, "--scope"},
	} {
		out, errOut, err := ta.run(t, ta.admin, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) || out != "" {
			t.Errorf("%v: %v %q (want %q)", c.args, err, out, c.want)
		}
		if strings.Contains(err.Error()+errOut, tok) {
			t.Errorf("%v: the token is in the error", c.args)
		}
	}
}

// A read token may not manage tokens, and a bad one is refused; neither
// answer nor log holds a token.
func TestTokenCommandsRefusedAndNeverLeak(t *testing.T) {
	ta := newTokenAPI(t)
	_, unknown := apitoken.Generate(time.Now())
	for _, c := range []struct{ token, want string }{{ta.read, "403"}, {unknown, "401"}} {
		for _, args := range [][]string{{"token", "list"}, {"token", "create", "--name", "x"}, {"token", "revoke", "nobody"}} {
			out, errOut, err := ta.run(t, c.token, args...)
			if err == nil || !strings.Contains(err.Error(), c.want) || out != "" {
				t.Errorf("%v as %s: %v %q", args, c.want, err, out)
			}
			for _, tok := range []string{ta.read, unknown} {
				if strings.Contains(errOut+err.Error(), tok) {
					t.Errorf("%v: a token is in the error output", args)
				}
			}
		}
	}
	// A successful create: the new token is on stdout only, not in the logs.
	out, errOut, err := ta.run(t, ta.admin, "token", "create", "--name", "leak")
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(out)
	if strings.Contains(errOut, tok) {
		t.Error("the new token is on stderr")
	}
	for _, s := range []string{ta.admin, ta.read, unknown, tok} {
		if strings.Contains(ta.logs.String(), s) {
			t.Error("a token reached the server log")
		}
	}
}

// --local never talks to the API, so a connection flag next to it is a usage
// error, whatever the order, and -o does not apply.
func TestTokenCreateLocalExcludesConnectionFlags(t *testing.T) {
	cfg := tokenInstance(t, "sqlite://"+filepath.Join(t.TempDir(), "missing.db"))
	for _, extra := range [][]string{
		{"--server", "http://localhost:1"}, {"--token", "x"}, {"--insecure-http"},
	} {
		args := append([]string{"token", "create", "--local", "--config", cfg, "--name", "me"}, extra...)
		out, _, err := runCLI(t, args...)
		if err == nil || !strings.Contains(err.Error(), "none of the others can be") || out != "" {
			t.Errorf("%v: %v %q", extra, err, out)
		}
	}
	_, _, err := runCLI(t, "token", "create", "--local", "--config", cfg, "--name", "me", "-o", "json")
	if err == nil || !strings.Contains(err.Error(), "-o does not apply") {
		t.Errorf("-o with --local: %v", err)
	}
}

func TestAPIDrainWait(t *testing.T) {
	for _, c := range []struct{ in, want time.Duration }{
		{0, 0},
		{3 * time.Second, 3 * time.Second},
		{10 * time.Second, 10 * time.Second},
		{25 * time.Second, 20 * time.Second},
		{2 * time.Minute, 2*time.Minute - tokenFlushTimeout},
	} {
		if got := apiDrainWait(c.in); got != c.want {
			t.Errorf("apiDrainWait(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// serve hands --drain-timeout to the api role.
func TestAPIOptionsDrainWait(t *testing.T) {
	sources, err := watchSources(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "a.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	o, err := apiOptions(&config.Config{}, st, sources, nil, nil, 45*time.Second, slog.New(slog.DiscardHandler))
	if err != nil || o.DrainWait != 40*time.Second {
		t.Errorf("DrainWait = %v", o.DrainWait)
	}
	o, err = apiOptions(&config.Config{}, st, sources, nil, nil, 0, slog.New(slog.DiscardHandler))
	if err != nil || o.DrainWait != 0 { // api.New reads 0 as api.DefaultDrainWait
		t.Errorf("DrainWait = %v", o.DrainWait)
	}
}

// Revoking the last active admin token needs --yes (ADR 0030 T6); it is not
// asked when another active admin token exists, and expired or revoked admin
// tokens do not count.
func TestTokenRevokeLastAdmin(t *testing.T) {
	ta := newTokenAPI(t)
	// Two admin tokens: the seeded one and a new one. Revoking either is fine.
	out, _, err := ta.run(t, ta.admin, "token", "create", "--name", "second", "--scope", "admin")
	if err != nil {
		t.Fatal(err)
	}
	second := strings.TrimSpace(out)
	if _, _, err := ta.run(t, ta.admin, "token", "revoke", "second"); err != nil {
		t.Fatalf("not the last admin: %v", err)
	}
	if _, _, err := ta.run(t, second, "watch", "list"); err == nil {
		t.Fatal("second still works")
	}
	// Now the seeded token is the only active admin.
	out, errOut, err := ta.run(t, ta.admin, "token", "revoke", "admin")
	if err == nil || !strings.Contains(err.Error(), "last active admin token; pass --yes") || out != "" || strings.Contains(errOut, "revoked") {
		t.Fatalf("last admin: %v %q %q", err, out, errOut)
	}
	if _, errOut, err := ta.run(t, ta.admin, "watch", "list"); err != nil {
		t.Fatalf("the refused revoke changed something: %v %s", err, errOut)
	}
	// A non-admin target is never asked about.
	if _, _, err := ta.run(t, ta.admin, "token", "revoke", "reader"); err != nil {
		t.Fatalf("revoking a read token: %v", err)
	}
	if _, errOut, err := ta.run(t, ta.admin, "token", "revoke", ta.adminID, "--yes"); err != nil || !strings.Contains(errOut, "revoked API token "+ta.adminID) {
		t.Fatalf("--yes: %v %q", err, errOut)
	}
	if _, _, err := ta.run(t, ta.admin, "watch", "list"); err == nil {
		t.Fatal("the revoked admin token still works")
	}
}

func TestTokenCreateConfigNeedsLocal(t *testing.T) {
	ta := newTokenAPI(t)
	_, _, err := ta.run(t, ta.admin, "token", "create", "--name", "x", "--config", "/nonexistent.yaml")
	if err == nil || !strings.Contains(err.Error(), "--config only applies with --local") {
		t.Fatalf("err = %v", err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("closed pipe") }

// When stdout cannot take the token, stderr says which token to revoke, and
// does not hold the token.
func TestTokenCreateStdoutFailureNamesTheToken(t *testing.T) {
	ta := newTokenAPI(t)
	root := newRootCmd()
	var e bytes.Buffer
	root.SetOut(failWriter{})
	root.SetErr(&e)
	root.SetArgs([]string{"token", "create", "--name", "lost", "--server", ta.srv.URL, "--token", ta.admin})
	if err := root.ExecuteContext(t.Context()); err == nil {
		t.Fatal("no error")
	}
	rows, err := ta.st.ListAPITokens(t.Context(), "", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Name == "lost" && strings.Contains(e.String(), "created token "+r.ID+" but could not print it; revoke it with `angelia token revoke "+r.ID+"`") {
			return
		}
	}
	t.Fatalf("stderr: %q", e.String())
}
