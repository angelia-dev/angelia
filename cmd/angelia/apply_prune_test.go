package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestApplyFlagErrors(t *testing.T) {
	isolate(t)
	ok := writeTemp(t, "ok.yaml", "version: 1\n")
	conn := []string{"--server", "http://127.0.0.1:1", "--token", e2eAdmin}
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"-f", ok, "--prune"}, "--prune needs a label scope"},
		{[]string{"-f", ok, "-l", "a=b"}, "-l scopes --prune"},
		{[]string{"-f", ok, "--prune-allow-empty"}, "--prune-allow-empty only makes sense with --prune"},
		{[]string{"-f", ok, "--prune", "-l", "ab"}, `-l "ab": want key=value`},
		{[]string{"-f", writeTemp(t, "two.yaml", "version: 1\n---\nversion: 1\n")}, "several YAML documents"},
	}
	for _, c := range cases {
		_, _, err := runCLI(t, append(append([]string{"apply"}, c.args...), conn...)...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("apply %v: err = %v, want %q", c.args, err, c.want)
		}
	}
}

// S1: a real prune with no watch of the scope in the files is refused.
func TestApplyPruneRefusesEmptyScope(t *testing.T) {
	a := newApplyEnv(t)
	a.must("apply", "-f", writeTemp(t, "base.yaml", applyBase))
	empty := writeTemp(t, "empty.yaml", "version: 1\nwatches:\n  - {name: web-thing, source: npm, coordinates: {name: vue}, labels: {team: web}}\n")
	_, _, err := a.run("apply", "-f", empty, "--prune", "-l", "team=platform")
	if err == nil || !strings.Contains(err.Error(), "refusing to prune: the files hold no watch with the labels team=platform") {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(a.names("watch"), ","); got != "npm-react,pypi-old,pypi-requests,web-thing" {
		t.Errorf("watches after the refused prune: %s", got)
	}
	out, errOut, err := a.run("apply", "-f", empty, "--prune", "-l", "team=platform", "--dry-run")
	if err != nil || !strings.Contains(errOut, "warning: the files hold no watch") || !strings.Contains(out, "3 deleted") {
		t.Errorf("dry run: err %v\nstdout %s\nstderr %s", err, out, errOut)
	}
	out = a.must("apply", "-f", empty, "--prune", "-l", "team=platform", "--prune-allow-empty")
	if !strings.Contains(out, "3 deleted") || strings.Join(a.names("watch"), ",") != "web-thing" {
		t.Errorf("--prune-allow-empty:\n%s", out)
	}
}

// S4: a real run whose PUT fails does not prune.
func TestApplyFailedPutDoesNotPrune(t *testing.T) {
	a := newApplyEnv(t)
	a.must("apply", "-f", writeTemp(t, "base.yaml", applyBase))
	bad := writeTemp(t, "bad.yaml", "version: 1\nwatches:\n  - {name: pypi-requests, source: pypi, coordinates: {name: requests}, interval: 1m, labels: {team: platform}}\n")
	out, _, err := a.run("apply", "-f", bad, "--prune", "-l", "team=platform")
	if err == nil || !strings.Contains(err.Error(), `apply stopped at watch "pypi-requests"`) || !strings.Contains(err.Error(), "422") {
		t.Fatalf("err = %v (the cause must be in the error)", err)
	}
	if strings.Contains(out, " deleted") && !strings.Contains(out, "0 deleted") {
		t.Errorf("pruned after a failed PUT:\n%s", out)
	}
	if got := strings.Join(a.names("watch"), ","); got != "npm-react,pypi-old,pypi-requests,web-thing" {
		t.Errorf("watches: %s", got)
	}
}

// pruneFake serves a fixed state for the prune paths: gone-0 vanishes before
// the re-read, gone-a before the delete, gone-b leaves the scope, the delete
// of gone-c fails, so gone-d is never deleted.
type pruneFake struct {
	mu   sync.Mutex
	seen []string
}

func (f *pruneFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	watch := func(name, team string) string {
		return `{"name":"` + name + `","managed_by":"api","labels":{"team":"` + team + `"}}`
	}
	problem := func(status int, slug string) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"type":"https://github.com/angelia-dev/spec/blob/main/docs/api.md#problem-%s","title":"%s","status":%d}`, slug, slug, status)
	}
	name := strings.TrimPrefix(r.URL.Path, "/v1/watches/")
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/watches":
		var items []string
		for _, n := range []string{"keep", "gone-0", "gone-a", "gone-b", "gone-c", "gone-d"} {
			items = append(items, watch(n, "p"))
		}
		_, _ = io.WriteString(w, `{"items":[`+strings.Join(items, ",")+`],"next_cursor":null}`)
	case r.Method == http.MethodGet && (r.URL.Path == "/v1/sinks" || r.URL.Path == "/v1/subscriptions"):
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	case r.Method == http.MethodPut:
		w.Header().Set("Angelia-Change", "unchanged")
		_, _ = io.WriteString(w, watch(name, "p")) //nolint:gosec // test fake, JSON to a test client
	case r.Method == http.MethodGet && name == "gone-0":
		problem(http.StatusNotFound, "not-found")
	case r.Method == http.MethodGet && name == "gone-b":
		_, _ = io.WriteString(w, watch(name, "q")) //nolint:gosec // test fake, JSON to a test client
	case r.Method == http.MethodGet:
		_, _ = io.WriteString(w, watch(name, "p")) //nolint:gosec // test fake, JSON to a test client
	case r.Method == http.MethodDelete && name == "gone-a":
		problem(http.StatusNotFound, "not-found")
	case r.Method == http.MethodDelete && name == "gone-c":
		problem(http.StatusInternalServerError, "internal-error")
	case r.Method == http.MethodDelete:
		w.WriteHeader(http.StatusNoContent)
	default:
		problem(http.StatusBadRequest, "bad-request")
	}
}

// S2, S3, N3: the re-read before each delete, the 404 paths and the stop path.
func TestApplyPrunePaths(t *testing.T) {
	isolate(t)
	fake := &pruneFake{}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	f := writeTemp(t, "f.yaml", "version: 1\nwatches:\n  - {name: keep, source: pypi, coordinates: {name: x}, labels: {team: p}}\n")
	out, _, err := runCLI(t, "apply", "-f", f, "--prune", "-l", "team=p", "--server", srv.URL, "--token", testToken)
	if err == nil || !strings.Contains(err.Error(), `prune stopped at watch "gone-c", not deleted: gone-c, gone-d: 500`) {
		t.Fatalf("err = %v", err)
	}
	golden(t, "apply-prune-paths.table", out)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, s := range fake.seen {
		if s == "DELETE /v1/watches/gone-b" || s == "DELETE /v1/watches/gone-d" || s == "DELETE /v1/watches/gone-0" {
			t.Errorf("sent %s", s)
		}
	}
}

// N5: a cancelled run sends no further request.
func TestApplyStopsWhenCancelled(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var mu sync.Mutex
	puts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			mu.Lock()
			puts++
			mu.Unlock()
			cancel()
		}
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	t.Cleanup(srv.Close)
	f := writeTemp(t, "f.yaml", "version: 1\nwatches:\n  - {name: a, source: pypi}\n  - {name: b, source: pypi}\n  - {name: c, source: pypi}\n")
	root := newRootCmd()
	var o, e strings.Builder
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetArgs([]string{"apply", "-f", f, "--dry-run", "--server", srv.URL, "--token", testToken})
	err := root.ExecuteContext(ctx)
	if err == nil || !strings.Contains(err.Error(), "apply interrupted before watch") {
		t.Errorf("err = %v\n%s", err, o.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 1 {
		t.Errorf("%d PUTs, want 1: the run must stop after the cancel", puts)
	}
}
