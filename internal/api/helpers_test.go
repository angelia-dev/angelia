package api

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// Test tokens, one per scope. They are synthetic.
const (
	tokRead  = "test-read-token-0000000000000001"
	tokWrite = "test-write-token-000000000000002"
	tokAdmin = "test-admin-token-000000000000003"
)

// fakeAuth accepts a fixed set of tokens, compared in constant time.
// Revoke removes one.
type fakeAuth struct {
	mu     sync.Mutex
	tokens map[string]Principal
	calls  int
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{tokens: map[string]Principal{
		tokRead:  {ID: "tok-read", Scope: ScopeRead},
		tokWrite: {ID: "tok-write", Scope: ScopeWrite},
		tokAdmin: {ID: "tok-admin", Scope: ScopeAdmin},
	}}
}

func (a *fakeAuth) Authenticate(_ context.Context, token string) (Principal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	var found Principal
	ok := false
	for t, p := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			found, ok = p, true
		}
	}
	if !ok {
		return Principal{}, fmt.Errorf("token: %w", ErrUnauthenticated)
	}
	return found, nil
}

func (a *fakeAuth) revoke(token string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.tokens, token)
}

// syncBuffer is a log sink safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type env struct {
	t    *testing.T
	st   *sqlite.Store
	srv  *Server
	auth *fakeAuth
	log  *syncBuffer
	now  time.Time
}

func testSources(t *testing.T) *source.Registry {
	t.Helper()
	reg := source.NewRegistry()
	hc := &http.Client{}
	for _, s := range []source.Source{pypi.New(pypi.DefaultBaseURL, hc), githubrelease.New(githubrelease.DefaultBaseURL, hc)} {
		if err := reg.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	return reg
}

func newEnv(t *testing.T, mod ...func(*Options)) *env {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e := &env{t: t, st: st, auth: newFakeAuth(), log: &syncBuffer{}, now: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)}
	opts := Options{
		Store: st, Auth: e.auth, Sources: testSources(t),
		Plugins: []PluginInfo{
			{Kind: "pypi", Interface: "source", Runtime: "builtin", Version: "0.1.0"},
			{Kind: "github-release", Interface: "source", Runtime: "builtin", Version: "0.1.0"},
			{Kind: "webhook", Interface: "sink", Runtime: "builtin", Version: "0.1.0"},
		},
		API:    config.API{AllowedEnv: []string{"CI_WEBHOOK_SECRET"}},
		Logger: slog.New(slog.NewJSONHandler(e.log, nil)),
		Now:    func() time.Time { return e.now },
		NewID:  func() string { return ulid.New(time.Now()) },
	}
	for _, m := range mod {
		m(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	return e
}

type result struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r result) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		t.Fatalf("body is not a JSON object: %v: %s", err, r.Body)
	}
	return m
}

// problemType returns the slug of a problem response, checking its shape.
func (r result) problemType(t *testing.T) string {
	t.Helper()
	if ct := r.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Fatalf("Content-Type = %q, want application/problem+json (status %d, body %s)", ct, r.Status, r.Body)
	}
	m := r.json(t)
	typ, _ := m["type"].(string)
	if !strings.HasPrefix(typ, ProblemTypeBase) {
		t.Fatalf("problem type %q", typ)
	}
	if int(m["status"].(float64)) != r.Status {
		t.Fatalf("problem status %v != %d", m["status"], r.Status)
	}
	return strings.TrimPrefix(typ, ProblemTypeBase)
}

func (e *env) do(method, path, token string, body any, hdr ...string) result {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)
	return result{Status: rec.Code, Header: rec.Header(), Body: rec.Body.Bytes()}
}

func (e *env) must(want int, r result) result {
	e.t.Helper()
	if r.Status != want {
		e.t.Fatalf("status %d, want %d: %s", r.Status, want, r.Body)
	}
	return r
}

// Spec example bodies.
var (
	sinkPut = map[string]any{"kind": "webhook", "config": map[string]any{ //nolint:gosec // secret_env names a variable, not a secret
		"url": "https://ci.example.com/hooks/angelia", "secret_env": "CI_WEBHOOK_SECRET",
	}}
	watchPut = map[string]any{
		"source": "pypi", "coordinates": map[string]any{"name": "requests"}, "interval": "30m",
		"labels": map[string]any{"team": "platform", "repo": "api"},
		"policy": map[string]any{"bump": []any{"major", "minor"}, "cooldown": "72h"},
	}
	subscriptionPut = map[string]any{
		"sink": "ci-webhook",
		"filter": map[string]any{
			"labels": map[string]any{"team": "platform"}, "bump": []any{"major", "minor"},
			"type": []any{"release.published", "release.yanked"},
		},
		"policy": map[string]any{"cooldown": "24h"},
	}
)

// seedConfig creates the spec's example sink, watch and subscription.
func (e *env) seedConfig() {
	e.t.Helper()
	e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/ci-webhook", tokAdmin, sinkPut))
	e.must(http.StatusCreated, e.do("PUT", "/v1/watches/requests", tokWrite, watchPut))
	e.must(http.StatusCreated, e.do("PUT", "/v1/subscriptions/platform-ci", tokWrite, subscriptionPut))
}

func (e *env) watchID(name string) string {
	e.t.Helper()
	w, err := e.st.WatchByName(context.Background(), name)
	if err != nil {
		e.t.Fatalf("watch %s: %v", name, err)
	}
	return w.ID
}

func (e *env) subscriptionID(name string) string {
	e.t.Helper()
	sub, err := e.st.SubscriptionByName(context.Background(), name)
	if err != nil {
		e.t.Fatalf("subscription %s: %v", name, err)
	}
	return sub.ID
}

// envelope returns a release event envelope like the spec's example.
func envelope(id, typ, watchID, source, version string, at time.Time) []byte {
	m := map[string]any{
		"specversion": "1.0", "type": typ, "source": "angelia://angelia/watches/" + watchID, "id": id,
		"subject": source + ":requests@" + version, "time": at.UTC().Format(timestampLayout),
		"datacontenttype": "application/json", "dataschema": "https://github.com/angelia-dev/spec/schemas/release/v1.json",
		"data": map[string]any{
			"source": source, "package": "requests", "version": version, "normalized_version": version,
			"previous_version": nil, "scheme": "pep440", "bump": "minor", "backport": false, "prerelease": false,
			"yanked": false, "published_at": nil, "detected_at": at.UTC().Format(timestampLayout),
			"cooldown_satisfied_at": nil, "urls": map[string]any{"release": nil, "changelog": nil, "artifact": nil},
			"notes": nil, "watch": map[string]any{"id": watchID, "name": "requests", "labels": map[string]any{}},
			"purl": nil,
		},
	}
	b, _ := json.Marshal(m)
	return b
}

// appendEvent stores an event with optional deliveries and returns it.
func (e *env) appendEvent(watchID, typ, source string, at time.Time, ds ...store.Delivery) store.Event {
	e.t.Helper()
	id := ulid.New(at)
	ev := store.Event{ID: id, WatchID: watchID, Type: typ, Payload: envelope(id, typ, watchID, source, "2.33.0", at), CreatedAt: at}
	for i := range ds {
		ds[i].EventID = id
		if ds[i].ID == "" {
			ds[i].ID = ulid.New(at)
		}
	}
	if err := e.st.AppendEvent(context.Background(), ev, ds); err != nil {
		e.t.Fatal(err)
	}
	got, err := e.st.GetEvent(context.Background(), id)
	if err != nil {
		e.t.Fatal(err)
	}
	return got
}
