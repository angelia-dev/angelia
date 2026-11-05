package main

import (
	"bytes"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// update rewrites the golden files: go test ./cmd/angelia -run Golden -update
var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

const testToken = "test-cli-token-0000000000000000" //nolint:gosec // synthetic

// fixtureServer serves the recorded responses of testdata/api (public spec
// examples) and keeps the requests it saw.
type fixtureServer struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []seenRequest
}

type seenRequest struct {
	Method, URI, Auth, ContentType, Body string
}

func newFixtureServer(t *testing.T, routes map[string]string) *fixtureServer {
	t.Helper()
	fs := &fixtureServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		fs.mu.Lock()
		fs.reqs = append(fs.reqs, seenRequest{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), body.String()})
		fs.mu.Unlock()
		name, ok := routes[r.Method+" "+r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		status := http.StatusOK
		ct := "application/json"
		if strings.HasPrefix(name, "problem-") {
			ct = "application/problem+json"
			status = map[string]int{"problem-not-found": 404, "problem-validation": 422, "problem-managed": 409}[name]
		}
		data, err := os.ReadFile(filepath.Join("testdata", "api", name+".json")) //nolint:gosec // test fixture
		if err != nil {
			t.Errorf("fixture: %v", err)
			http.Error(w, "no fixture", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		_, _ = w.Write(data)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (f *fixtureServer) last() seenRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

// runCLI runs the root command with args and returns stdout, stderr and the error.
func runCLI(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	root := newRootCmd()
	var o, e bytes.Buffer
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetArgs(args)
	err = root.ExecuteContext(t.Context())
	return o.String(), e.String(), err
}

// isolate keeps the developer's environment and client config out of a test.
func isolate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(envServer, "")
	t.Setenv(envToken, "")
	return home
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil { //nolint:gosec // test data
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // test fixture
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from %s (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, path, got, want)
	}
}

func TestGoldenOutput(t *testing.T) {
	isolate(t)
	routes := map[string]string{
		"GET /v1/watches":                               "watches",
		"GET /v1/watches/requests":                      "watch",
		"GET /v1/watches/requests/versions":             "versions",
		"GET /v1/sinks":                                 "sinks",
		"GET /v1/sinks/ci-webhook":                      "sink",
		"GET /v1/subscriptions":                         "subs",
		"GET /v1/subscriptions/platform-ci":             "sub",
		"GET /v1/events":                                "events",
		"GET /v1/events/01JBX2A7C8D9E0F1G2H3J4K5M6":     "event",
		"GET /v1/deliveries":                            "deliveries",
		"GET /v1/deliveries/01JBX2A9QWERTY0123456789AB": "delivery",
	}
	srv := newFixtureServer(t, routes)
	cases := []struct {
		name string
		args []string
	}{
		{"watch-list", []string{"watch", "list"}},
		{"watch-get", []string{"watch", "get", "requests"}},
		{"watch-versions", []string{"watch", "versions", "requests"}},
		{"sink-list", []string{"sink", "list"}},
		{"sink-get", []string{"sink", "get", "ci-webhook"}},
		{"sub-list", []string{"sub", "list"}},
		{"sub-get", []string{"sub", "get", "platform-ci"}},
		{"event-list", []string{"event", "list"}},
		{"event-get", []string{"event", "get", "01JBX2A7C8D9E0F1G2H3J4K5M6"}},
		{"delivery-list", []string{"delivery", "list", "--status", "dead"}},
		{"delivery-get", []string{"delivery", "get", "01JBX2A9QWERTY0123456789AB"}},
	}
	for _, c := range cases {
		for _, format := range []string{formatTable, formatJSON} {
			t.Run(c.name+"."+format, func(t *testing.T) {
				args := append(append([]string{}, c.args...), "--server", srv.URL, "--token", testToken, "-o", format)
				out, _, err := runCLI(t, args...)
				if err != nil {
					t.Fatal(err)
				}
				golden(t, c.name+"."+format, out)
				if strings.Contains(out, testToken) {
					t.Error("the token is in the output")
				}
			})
		}
	}
}

func TestOutputFormatInvalid(t *testing.T) {
	isolate(t)
	_, _, err := runCLI(t, "watch", "list", "--server", "http://127.0.0.1:1", "-o", "yaml")
	if err == nil || !strings.Contains(err.Error(), `unknown output format "yaml"`) {
		t.Fatalf("err = %v", err)
	}
}
