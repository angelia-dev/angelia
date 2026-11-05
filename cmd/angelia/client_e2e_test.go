package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

const (
	e2eAdmin = "e2e-admin-token-00000000000000"
	e2eRead  = "e2e-read-token-000000000000000"
)

type e2eAuth struct{}

func (e2eAuth) Authenticate(_ context.Context, token string) (api.Principal, error) {
	switch {
	case subtle.ConstantTimeCompare([]byte(token), []byte(e2eAdmin)) == 1:
		return api.Principal{ID: "admin", Scope: api.ScopeAdmin}, nil
	case subtle.ConstantTimeCompare([]byte(token), []byte(e2eRead)) == 1:
		return api.Principal{ID: "read", Scope: api.ScopeRead}, nil
	}
	return api.Principal{}, fmt.Errorf("token: %w", api.ErrUnauthenticated)
}

// newE2EServer runs the real REST API handler on an empty SQLite store.
func newE2EServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newE2EServerEnv(t, "CI_WEBHOOK_SECRET")
}

// newE2EServerEnv is newE2EServer with these api.allowed_env names.
func newE2EServerEnv(t *testing.T, allowedEnv ...string) *httptest.Server {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "api.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sources, err := watchSources(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	h, err := api.New(api.Options{
		Store: st, Auth: e2eAuth{}, Sources: sources, Plugins: builtinPlugins(sources.Kinds()),
		API: config.API{AllowedEnv: allowedEnv}, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCRUDAgainstAPI drives every resource command against the real handler.
func TestCRUDAgainstAPI(t *testing.T) {
	isolate(t)
	srv := newE2EServer(t)
	conn := []string{"--server", srv.URL, "--token", e2eAdmin}
	run := func(args ...string) (string, string, error) {
		return runCLI(t, append(append([]string{}, args...), conn...)...)
	}
	must := func(args ...string) (string, string) {
		t.Helper()
		out, errOut, err := run(args...)
		if err != nil {
			t.Fatalf("angelia %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, errOut)
		}
		return out, errOut
	}

	sink := writeTemp(t, "sink.yaml", "kind: webhook\nconfig:\n  url: https://ci.example.com/hooks/angelia\n  secret_env: CI_WEBHOOK_SECRET\n")
	watch := writeTemp(t, "watch.json", `{"source":"pypi","coordinates":{"name":"requests"},"interval":"30m","labels":{"team":"platform"}}`)
	sub := writeTemp(t, "sub.yaml", "sink: ci-webhook\nfilter:\n  type: [release.published]\n")

	// put: created, then unchanged (idempotent), with the change on stderr.
	for _, c := range []struct{ kind, name, file string }{{"sink", "ci-webhook", sink}, {"watch", "requests", watch}, {"sub", "platform-ci", sub}} {
		_, errOut := must(c.kind, "put", c.name, "-f", c.file)
		if !strings.Contains(errOut, "created") {
			t.Errorf("%s put: stderr %q lacks created", c.kind, errOut)
		}
		_, errOut = must(c.kind, "put", c.name, "-f", c.file)
		if !strings.Contains(errOut, "unchanged") {
			t.Errorf("%s put again: stderr %q lacks unchanged", c.kind, errOut)
		}
	}

	// put from stdin, with a dry run that writes nothing.
	root := newRootCmd()
	var o, e strings.Builder
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetIn(strings.NewReader(`{"source":"pypi","coordinates":{"name":"flask"}}`))
	root.SetArgs(append([]string{"watch", "put", "flask", "-f", "-", "--dry-run"}, conn...))
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.String(), "created (dry run)") {
		t.Errorf("dry run: stderr %q", e.String())
	}
	if _, _, err := run("watch", "get", "flask"); err == nil {
		t.Error("the dry run wrote the watch")
	}

	// list and get
	out, _ := must("watch", "list")
	if !strings.Contains(out, "requests") || strings.Contains(out, "flask") {
		t.Errorf("watch list:\n%s", out)
	}
	out, _ = must("watch", "list", "--source", "npm")
	if strings.Contains(out, "requests") {
		t.Errorf("--source npm kept requests:\n%s", out)
	}
	out, _ = must("watch", "list", "--label", "team=platform", "-o", "json")
	if !strings.Contains(out, `"team": "platform"`) {
		t.Errorf("watch list --label:\n%s", out)
	}
	out, _ = must("sink", "get", "ci-webhook")
	if !strings.Contains(out, "config.secret_env") {
		t.Errorf("sink get:\n%s", out)
	}
	out, _ = must("sub", "list", "--sink", "ci-webhook")
	if !strings.Contains(out, "platform-ci") {
		t.Errorf("sub list:\n%s", out)
	}
	out, _ = must("watch", "versions", "requests")
	if !strings.HasPrefix(out, "VERSION") {
		t.Errorf("watch versions:\n%s", out)
	}
	out, _ = must("event", "list")
	if !strings.HasPrefix(out, "SEQ") {
		t.Errorf("event list:\n%s", out)
	}
	out, _ = must("delivery", "list", "--status", "dead")
	if !strings.HasPrefix(out, "ID") {
		t.Errorf("delivery list:\n%s", out)
	}

	// paging
	for _, n := range []string{"a1", "a2"} {
		must("watch", "put", n, "-f", watch)
	}
	out, errOut := must("watch", "list", "--limit", "2")
	if !strings.Contains(errOut, "more results: --cursor ") {
		t.Errorf("no cursor hint on stderr: %q", errOut)
	}
	if strings.Contains(out, "more results") {
		t.Errorf("cursor hint on stdout:\n%s", out)
	}
	cursor := strings.Fields(strings.SplitN(errOut, "--cursor ", 2)[1])[0]
	out, _ = must("watch", "list", "--limit", "2", "--cursor", cursor)
	if !strings.Contains(out, "requests") {
		t.Errorf("second page:\n%s", out)
	}
	out, _ = must("watch", "list", "--limit", "1", "--all")
	for _, n := range []string{"a1", "a2", "requests"} {
		if !strings.Contains(out, n) {
			t.Errorf("--all lacks %s:\n%s", n, out)
		}
	}

	// delete
	if _, errOut := must("sub", "delete", "platform-ci", "--dry-run"); !strings.Contains(errOut, "would be deleted") {
		t.Errorf("delete dry run: %q", errOut)
	}
	must("sub", "get", "platform-ci")
	if _, errOut := must("sub", "delete", "platform-ci"); !strings.Contains(errOut, "deleted") {
		t.Errorf("delete: %q", errOut)
	}
	_, _, err := run("sub", "get", "platform-ci")
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("get after delete: %v", err)
	}
}

func TestAPIErrorsAreReadable(t *testing.T) {
	isolate(t)
	srv := newE2EServer(t)
	body := writeTemp(t, "w.json", `{"source":"pypi","coordinates":{"name":"requests"},"interval":"1s"}`)

	t.Run("validation lists every error", func(t *testing.T) {
		_, _, err := runCLI(t, "watch", "put", "requests", "-f", body, "--server", srv.URL, "--token", e2eAdmin)
		if err == nil || !strings.Contains(err.Error(), "422") || !strings.Contains(err.Error(), "/interval") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a bad token is a 401 that does not echo it", func(t *testing.T) {
		out, errOut, err := runCLI(t, "watch", "list", "--server", srv.URL, "--token", "wrong-token-value")
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error()+out+errOut, "wrong-token-value") {
			t.Error("the token is in the output")
		}
	})
	t.Run("no token is a 401", func(t *testing.T) {
		_, _, err := runCLI(t, "watch", "list", "--server", srv.URL)
		if err == nil || !strings.Contains(err.Error(), "401") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a read token cannot write", func(t *testing.T) {
		_, _, err := runCLI(t, "watch", "put", "requests", "-f", body, "--server", srv.URL, "--token", e2eRead)
		if err == nil || !strings.Contains(err.Error(), "403") {
			t.Errorf("err = %v", err)
		}
		if _, _, err := runCLI(t, "watch", "list", "--server", srv.URL, "--token", e2eRead); err != nil {
			t.Errorf("a read token lists: %v", err)
		}
	})
	t.Run("an unreachable server", func(t *testing.T) {
		_, _, err := runCLI(t, "watch", "list", "--server", "http://127.0.0.1:1", "--token", "tok-must-not-show")
		if err == nil || strings.Contains(err.Error(), "tok-must-not-show") {
			t.Errorf("err = %v", err)
		}
	})
}

func TestPutRejectsBadFiles(t *testing.T) {
	isolate(t)
	srv := newFixtureServer(t, nil)
	for name, content := range map[string]string{"list": "- a\n- b\n", "scalar": "hello\n", "invalid": "{unclosed"} {
		t.Run(name, func(t *testing.T) {
			p := writeTemp(t, "r.yaml", content)
			_, _, err := runCLI(t, "watch", "put", "x", "-f", p, "--server", srv.URL)
			if err == nil {
				t.Fatal("accepted")
			}
			if len(srv.reqs) != 0 {
				t.Error("sent a request")
			}
		})
	}
	if _, _, err := runCLI(t, "watch", "put", "x", "-f", filepath.Join(t.TempDir(), "missing.json"), "--server", srv.URL); err == nil {
		t.Error("missing file accepted")
	}
	if _, _, err := runCLI(t, "watch", "put", "x", "--server", srv.URL); err == nil {
		t.Error("put without -f accepted")
	}
}
