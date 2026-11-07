package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/sink/webhook"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Synthetic webhook secrets (32 bytes each, not one repeated byte).
var (
	envSecret = "whsec_" + base64.StdEncoding.EncodeToString(seq(1)) //nolint:gosec // G101: synthetic test value
	refSecret = "whsec_" + base64.StdEncoding.EncodeToString(seq(2)) //nolint:gosec // G101: synthetic test value
)

func seq(start byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = start + byte(i)
	}
	return b
}

// fakePyPI serves GET /pypi/demo/json from a version list the test changes.
type fakePyPI struct {
	mu       sync.Mutex
	versions []string
	hits     int
}

func (f *fakePyPI) set(vs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions = vs
}

func (f *fakePyPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *fakePyPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits++
	if r.URL.Path != "/pypi/demo/json" {
		http.NotFound(w, r)
		return
	}
	rel := map[string][]map[string]any{}
	at := time.Now().Add(-time.Hour)
	for i, v := range f.versions {
		rel[v] = []map[string]any{{"upload_time_iso_8601": at.Add(-time.Duration(i) * 24 * time.Hour).Format(time.RFC3339), "yanked": false}}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"info": map[string]any{"name": "demo"}, "releases": rel})
}

// receiver records the webhook requests it gets.
type receiver struct {
	mu  sync.Mutex
	got []received
}

type received struct {
	path   string
	header http.Header
	body   []byte
}

func newReceiver() *receiver { return &receiver{} }

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.got = append(rc.got, received{path: r.URL.Path, header: r.Header.Clone(), body: b})
	rc.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (rc *receiver) all() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.got)
}

// listenOn starts an httptest server on addr ("127.0.0.2:0").
func listenOn(t *testing.T, addr string, h http.Handler) *httptest.Server {
	t.Helper()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	srv := httptest.NewUnstartedServer(h)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// startServe runs `serve` until the test ends and returns its log.
func startServe(t *testing.T, args ...string) *syncBuf {
	t.Helper()
	var logs syncBuf
	root := newRootCmd()
	root.SetOut(&logs)
	root.SetErr(&logs)
	root.SetArgs(append([]string{"serve", "--drain-timeout=5s"}, args...))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v\n%s", err, logs.String())
			}
		case <-time.After(15 * time.Second):
			t.Error("serve did not exit")
		}
	})
	return &logs
}

// openDB opens the instance database for reads next to the running serve.
func openDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// waitFor polls cond until it holds or 20 s pass.
func waitFor(t *testing.T, what string, logs *syncBuf, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s\n%s", what, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func queryInt(t *testing.T, db *sql.DB, q string) int64 {
	t.Helper()
	var n sql.NullInt64
	if err := db.QueryRowContext(t.Context(), q).Scan(&n); err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("%s: %v", q, err)
	}
	return n.Int64
}

// e2eInstance writes the instance config and returns its path and database.
// The secrets key is created and a webhook secret bound to bindHost stored
// under the name "hook-ref".
func e2eInstance(t *testing.T, registryURL, allowNetworks, bindHost string) (cfgPath, dbPath, listen string) {
	t.Helper()
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_WATCHES", "")
	t.Setenv(config.KeyEnv, "")
	t.Setenv("HOOK_SECRET", envSecret)
	keyFile := writeKeyFile(t, 7)
	listen = freeAddr(t)
	cfgPath, dbPath = instanceFiles(t, fmt.Sprintf(`listen: %q
instance:
  name: e2e
http:
  allow_networks: [%s]
  allow_http_hosts: [127.0.0.1, 127.0.0.2]
sources:
  pypi:
    base_url: %s
secrets:
  key_file: %s
`, listen, allowNetworks, registryURL, keyFile))
	st, err := sqlite.Open(t.Context(), dbPath, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	keys := keyring(t, 7)
	const id = "01JBSECRET0000000000000000"
	env, kid, err := keys.Seal(id, "hook-ref", []byte(refSecret))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	s := store.Secret{ID: id, Name: "hook-ref", Hosts: []string{bindHost}, KeyID: kid, Ciphertext: env, CreatedAt: now, UpdatedAt: now}
	if err := st.InSecretTx(t.Context(), func(tx store.SecretTx) error { return tx.CreateSecret(t.Context(), s) }); err != nil {
		t.Fatal(err)
	}
	return cfgPath, dbPath, listen
}

func watchFile(t *testing.T, hookURL string) string {
	t.Helper()
	return writeFile(t, "w.yaml", fmt.Sprintf(`version: 1
sinks:
  - {name: hook-env, kind: webhook, config: {url: '%[1]s/env', secret_env: HOOK_SECRET}}
  - {name: hook-ref, kind: webhook, config: {url: '%[1]s/ref', secret_ref: hook-ref}}
watches:
  - {name: demo, source: pypi, coordinates: {name: demo}}
subscriptions:
  - {name: to-env, sink: hook-env}
  - {name: to-ref, sink: hook-ref}
`, hookURL))
}

// recheckSoon makes the target due again: the scheduler's interval is an
// hour, and the first check of a watch is its baseline.
func recheckSoon(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), `UPDATE target SET next_check_at = 0`); err != nil {
		t.Fatal(err)
	}
}

// AC 1: `angelia serve` with a watch and webhook subscriptions delivers a
// signed event to a local receiver. The fake registry and the receiver are
// on loopback, reachable only because http.allow_networks lists it; one
// sink signs with secret_env, the other with a stored secret_ref bound to
// the receiver's host and port.
func TestServeDeliversSignedWebhook(t *testing.T) {
	reg := &fakePyPI{}
	reg.set("1.0.0", "0.9.0")
	registry := listenOn(t, "127.0.0.1:0", reg)
	rc := newReceiver()
	hook := listenOn(t, "127.0.0.1:0", rc)
	cfgPath, dbPath, listen := e2eInstance(t, registry.URL, "127.0.0.1/32", strings.TrimPrefix(hook.URL, "http://"))
	logs := startServe(t, "--config", cfgPath, "--watches", watchFile(t, hook.URL), "--roles=all")
	db := openDB(t, dbPath)

	waitFor(t, "the baseline check", logs, func() bool {
		return queryInt(t, db, `SELECT last_checked_at FROM target`) > 0 &&
			queryInt(t, db, `SELECT COUNT(*) FROM watch WHERE baseline_done = 1`) == 1
	})
	if n := queryInt(t, db, `SELECT COUNT(*) FROM event`); n != 0 {
		t.Fatalf("the baseline emitted %d events", n)
	}
	reg.set("1.1.0", "1.0.0", "0.9.0")
	recheckSoon(t, db)
	waitFor(t, "two deliveries", logs, func() bool { return len(rc.all()) >= 2 })

	secretFor := map[string]string{"/env": envSecret, "/ref": refSecret}
	for _, r := range rc.all() {
		if err := webhook.Verify([]string{secretFor[r.path]}, r.header, r.body, time.Now(), 5*time.Minute); err != nil {
			t.Errorf("%s: signature does not verify: %v", r.path, err)
		}
		other := envSecret
		if r.path == "/env" {
			other = refSecret
		}
		if err := webhook.Verify([]string{other}, r.header, r.body, time.Now(), 5*time.Minute); err == nil {
			t.Errorf("%s: verifies with the other sink's secret", r.path)
		}
		var ev struct {
			Type   string `json:"type"`
			Source string `json:"source"`
			Data   struct {
				Version string `json:"version"`
				Package string `json:"package"`
			} `json:"data"`
		}
		if err := json.Unmarshal(r.body, &ev); err != nil {
			t.Fatal(err)
		}
		if ev.Type != "dev.angelia.release.published.v1" || ev.Data.Version != "1.1.0" || ev.Data.Package != "demo" ||
			!strings.HasPrefix(ev.Source, "angelia://e2e/watches/") {
			t.Errorf("%s: event = %+v", r.path, ev)
		}
	}
	waitFor(t, "both deliveries recorded", logs, func() bool {
		return queryInt(t, db, `SELECT COUNT(*) FROM delivery WHERE status = 'delivered'`) == 2
	})
	for _, leak := range []string{envSecret, refSecret} {
		if strings.Contains(logs.String(), leak) {
			t.Fatal("a webhook secret reached the log")
		}
	}
	// The running roles report to /metrics (ADR 0019).
	resp, err := http.Get("http://" + listen + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, want := range []string{
		`angelia_source_fetches_total{result="ok",source="pypi"}`,
		`angelia_delivery_attempts_total{outcome="delivered"} 2`,
		`angelia_deliveries{status="delivered"} 2`,
		"angelia_netguard_rate_limit_wait_seconds_count",
		"angelia_scheduler_due_targets",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

// AC 2: http.allow_networks changes what the running process may dial. The
// registry is on 127.0.0.1 and the receiver on 127.0.0.2; only 127.0.0.1/32
// is allowed, so the check runs but the delivery is refused by the guard
// before any request, and is dead (a blocked destination is permanent).
func TestServeAllowNetworksGuardsDelivery(t *testing.T) {
	reg := &fakePyPI{}
	reg.set("1.0.0", "0.9.0")
	registry := listenOn(t, "127.0.0.1:0", reg)
	rc := newReceiver()
	hook := listenOn(t, "127.0.0.2:0", rc)
	cfgPath, dbPath, _ := e2eInstance(t, registry.URL, "127.0.0.1/32", strings.TrimPrefix(hook.URL, "http://"))
	logs := startServe(t, "--config", cfgPath, "--watches", watchFile(t, hook.URL), "--roles=all")
	db := openDB(t, dbPath)
	// last_checked_at is written with next_check_at, after the commit that
	// sets baseline_done: waiting for both keeps recheckSoon from being
	// overwritten by the scheduler's own write.
	waitFor(t, "the baseline check", logs, func() bool {
		return queryInt(t, db, `SELECT last_checked_at FROM target`) > 0 &&
			queryInt(t, db, `SELECT COUNT(*) FROM watch WHERE baseline_done = 1`) == 1
	})
	reg.set("1.1.0", "1.0.0", "0.9.0")
	recheckSoon(t, db)
	waitFor(t, "two dead deliveries", logs, func() bool {
		return queryInt(t, db, `SELECT COUNT(*) FROM delivery WHERE status = 'dead' AND last_error LIKE '%destination not allowed%'`) == 2
	})
	if n := len(rc.all()); n != 0 {
		t.Fatalf("the receiver got %d requests", n)
	}
}

// AC 2, source side: without http.allow_networks the scheduler may not dial
// the loopback registry; the check fails and the target backs off.
func TestServeWithoutAllowNetworksRefusesLoopbackSource(t *testing.T) {
	reg := &fakePyPI{}
	reg.set("1.0.0")
	registry := listenOn(t, "127.0.0.1:0", reg)
	hook := listenOn(t, "127.0.0.1:0", newReceiver())
	cfgPath, dbPath, _ := e2eInstance(t, registry.URL, "", strings.TrimPrefix(hook.URL, "http://"))
	logs := startServe(t, "--config", cfgPath, "--watches", watchFile(t, hook.URL), "--roles=scheduler,api")
	db := openDB(t, dbPath)
	waitFor(t, "a failed check", logs, func() bool {
		return queryInt(t, db, `SELECT error_count FROM target`) > 0
	})
	if reg.count() != 0 {
		t.Fatalf("the registry got %d requests", reg.count())
	}
	if !strings.Contains(logs.String(), "check failed") {
		t.Errorf("no check failure logged:\n%s", logs.String())
	}
}

// The netguard client serve builds takes http.allow_networks: a loopback
// server is refused without it and reached with it.
func TestNetClientAllowNetworks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	get := func(allow ...string) error {
		c, err := newNetClient(&config.Config{HTTP: config.HTTP{AllowNetworks: allow}}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		resp, err := c.HTTPClient().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := get(); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("without allow_networks: %v, want ErrBlocked", err)
	}
	if err := get("10.0.0.0/8"); !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("with another prefix: %v, want ErrBlocked", err)
	}
	if err := get("127.0.0.0/8"); err != nil {
		t.Fatalf("with 127.0.0.0/8: %v", err)
	}
}

// Every sink kind the watch file accepts is registered for delivery, so a
// new built-in kind plugs in through builtinSinks.
func TestBuiltinSinksMatchWatchFileKinds(t *testing.T) {
	var kinds []string
	for _, s := range builtinSinks(&config.Config{}, &http.Client{}, nil) {
		kinds = append(kinds, s.Kind())
	}
	slices.Sort(kinds)
	want := slices.Sorted(slices.Values(watchfile.SinkKinds))
	if !slices.Equal(kinds, want) {
		t.Fatalf("built-in sinks %v, watch file sink kinds %v", kinds, want)
	}
}

func TestRegistryURL(t *testing.T) {
	cfg := &config.Config{Sources: map[string]config.Source{
		"pypi": {BaseURL: "https://pypi.org/"},
		"npm":  {BaseURL: "https://npm.example.org/"},
	}}
	r := registryURL(cfg)
	for kind, want := range map[string]string{"pypi": "", "npm": "https://npm.example.org", "github-release": ""} {
		if got := r(kind); got != want {
			t.Errorf("%s: %q, want %q", kind, got, want)
		}
	}
}

// The stored-sinks check runs for every role that opens the store, the
// scheduler and dispatcher included (ADR 0024 amendment): openStore refuses
// a sink whose kind marks a key x-angelia-secret and holds a literal.
func TestOpenStoreRunsStoredSinksCheck(t *testing.T) {
	cfgPath, db := instanceFiles(t, "")
	st, err := sqlite.Open(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSink(t.Context(), store.Sink{ID: "01K", Kind: "chat", Name: "old", ConfigEncrypted: []byte(`{"token":"literal"}`)}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	plugins := []api.PluginInfo{{
		Kind: "chat", Interface: "sink", Runtime: "wasm", Version: "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","x-angelia-secret":true}}}`),
	}}
	cfg := mustLoad(t, cfgPath)
	if _, err := openStore(t.Context(), cfg, plugins, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "sink old /config/token") {
		t.Fatalf("openStore: %v", err)
	}
	st, err = openStore(t.Context(), cfg, builtinPlugins(nil), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("built-in kinds mark nothing x-angelia-secret: %v", err)
	}
	_ = st.Close()
	if _, err := openStore(t.Context(), &config.Config{Database: config.Database{DSN: "postgres://db.example.org/a"}}, nil, nil); err == nil {
		t.Fatal("postgres DSN")
	}
}

// newPipelineRoles builds only the selected roles, and the built-in kinds
// yield the webhook's destination pointer. That the dispatcher really
// resolves secret_ref through the store is shown end to end by
// TestServeDeliversSignedWebhook.
func TestPipelineRolesWiring(t *testing.T) {
	cfgPath, db := instanceFiles(t, "")
	cfg := mustLoad(t, cfgPath)
	st, err := sqlite.Open(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	d := pipelineDeps{cfg: cfg, store: st, keys: keyring(t, 1), plugins: builtinPlugins(nil), log: slog.New(slog.DiscardHandler)}
	p, err := newPipelineRoles(d, false, false)
	if err != nil || p.scheduler != nil || p.dispatcher != nil {
		t.Fatalf("no role selected: %+v, %v", p, err)
	}
	p, err = newPipelineRoles(d, true, true)
	if err != nil || p.scheduler == nil || p.dispatcher == nil {
		t.Fatalf("both roles: %+v, %v", p, err)
	}
	dest, err := api.SinkDestinations(d.plugins)
	if err != nil || dest[webhook.Kind] != "/url" {
		t.Fatalf("webhook destination = %v, %v", dest, err)
	}
}

func TestWarnDrainBudget(t *testing.T) {
	for _, tc := range []struct {
		drain time.Duration
		warn  bool
	}{{25 * time.Second, false}, {23 * time.Second, true}, {10 * time.Second, true}} {
		var b syncBuf
		warnDrainBudget(slog.New(slog.NewTextHandler(&b, nil)), tc.drain)
		if got := strings.Contains(b.String(), "drain timeout too short"); got != tc.warn {
			t.Errorf("drain %v: warned = %v, want %v (%s)", tc.drain, got, tc.warn, b.String())
		}
	}
}
