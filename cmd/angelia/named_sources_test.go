package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/detect"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/scheduler"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Named source instances (#205): sources.<name>.kind selects the
// implementation, a watch selects the instance by name, and everything
// downstream of the fetch sees the base kind.

const (
	canaryPublic   = "CANARY-public-npm-token"
	canaryInternal = "CANARY-internal-npm-token"
)

// countingNPM serves fakeVerdaccio(token) and counts the requests it gets.
func countingNPM(token string, hits *atomic.Int32) http.Handler {
	h := fakeVerdaccio(token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h.ServeHTTP(w, r)
	})
}

// twoNPMInstances configures the built-in npm at one httptest server and the
// instance npm-internal at another, each with its own stored token.
func twoNPMInstances(t *testing.T) (e *regEnv, cfg *config.Config, pub, internal *httptest.Server, pubHits, internalHits *atomic.Int32) {
	t.Helper()
	pubHits, internalHits = new(atomic.Int32), new(atomic.Int32)
	pub = httptest.NewServer(countingNPM(canaryPublic, pubHits))
	internal = httptest.NewServer(countingNPM(canaryInternal, internalHits))
	t.Cleanup(pub.Close)
	t.Cleanup(internal.Close)
	e = newRegEnv(t)
	e.put(t, "public-token", canaryPublic, hostOf(t, pub.URL))
	e.put(t, "internal-token", canaryInternal, hostOf(t, internal.URL))
	cfg = config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: pub.URL, Auth: config.SourceAuth{TokenRef: "public-token"}}
	cfg.Sources["npm-internal"] = config.Source{Kind: "npm", BaseURL: internal.URL, Auth: config.SourceAuth{TokenRef: "internal-token"}}
	cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
	return e, cfg, pub, internal, pubHits, internalHits
}

func (e *regEnv) registry(t *testing.T, cfg *config.Config) *source.Registry {
	t.Helper()
	client, err := newNetClient(cfg, e.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtinSources(cfg, client.HTTPClient(), e.log, nil, e.res)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// AC2: a watch with source: <name> uses that instance's base_url and auth,
// tested with two npm instances on httptest servers.
func TestNamedNPMInstancesUseTheirOwnBaseURLAndAuth(t *testing.T) {
	e, cfg, _, _, pubHits, internalHits := twoNPMInstances(t)
	reg := e.registry(t, cfg)
	tgt := npmTarget() // the base kind: a source never sees the instance name
	for _, name := range []string{"npm", "npm-internal"} {
		src, err := reg.Get(name)
		if err != nil {
			t.Fatal(err)
		}
		if src.Kind() != "npm" {
			t.Errorf("%s: Kind() = %q, want npm", name, src.Kind())
		}
		res, err := src.List(t.Context(), tgt, source.FetchState{})
		if err != nil || len(res.Versions) != 2 {
			t.Fatalf("%s: %d versions, %v", name, len(res.Versions), err)
		}
	}
	if pubHits.Load() != 1 || internalHits.Load() != 1 {
		t.Errorf("requests: npm %d, npm-internal %d, want one each", pubHits.Load(), internalHits.Load())
	}

	// Give npm the internal token (bound to the public host too): the
	// public registry refuses it, so the credential is not shared by kind.
	e.put(t, "internal-token-2", canaryInternal, hostOf(t, cfg.Sources["npm"].BaseURL))
	cfg.Sources["npm"] = config.Source{BaseURL: cfg.Sources["npm"].BaseURL, Auth: config.SourceAuth{TokenRef: "internal-token-2"}}
	src, _ := e.registry(t, cfg).Get("npm")
	if _, err := src.List(t.Context(), tgt, source.FetchState{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("npm with the internal token on the public registry: %v, want 401", err)
	}
}

// AC1: events keep the base kind (purl type, data.source, subject), while
// the repository_url of the purl is the instance's base_url.
func TestNamedInstanceEventsKeepTheBaseKind(t *testing.T) {
	e, cfg, _, internal, _, _ := twoNPMInstances(t)
	// The internal registry first has 1.0.0 (the baseline), then 1.1.0.
	var second atomic.Bool
	internal.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+canaryInternal {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		vs, ts := `"1.0.0":{"dist":{"tarball":"http://x/1.tgz"}}`, `"1.0.0":"2026-01-01T00:00:00.000Z"`
		if second.Load() {
			vs += `,"1.1.0":{"dist":{"tarball":"http://x/2.tgz"}}`
			ts += `,"1.1.0":"2026-02-01T00:00:00.000Z"`
		}
		_, _ = w.Write([]byte(`{"name":"@acme/widget","dist-tags":{"latest":"1.0.0"},"versions":{` + vs + `},"time":{` + ts + `}}`))
	})
	reg := e.registry(t, cfg)
	file := writeFile(t, "w.yaml", `version: 1
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://hook.example.org/x', secret_env: HOOK_SECRET}}
subscriptions:
  - {name: all, sink: hook}
watches:
  - name: widget-internal
    source: npm-internal
    coordinates: {name: '@acme/widget'}
`)
	f, err := parseWatchFile(cfg, file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(t.Context(), e.st, f, watchfile.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var tgt store.Target
	err = e.st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		tgt, err = tx.FindTarget(t.Context(), "npm-internal", map[string]string{"name": "@acme/widget"})
		return err
	})
	if err != nil {
		t.Fatalf("the target is stored under the instance name: %v", err)
	}
	chk := &detect.Checker{
		Store: e.st, Sources: reg, Instance: "test", RegistryURL: registryURL(cfg), Matcher: event.NewMatcherCache(),
		Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
	}
	if _, err := chk.Check(t.Context(), tgt); err != nil {
		t.Fatal(err)
	}
	second.Store(true)
	if _, err := chk.Check(t.Context(), tgt); err != nil {
		t.Fatal(err)
	}
	evs, err := e.st.ListEventsAfter(context.Background(), 0, 10)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %d, %v", len(evs), err)
	}
	for _, ev := range evs {
		var p struct {
			Subject string `json:"subject"`
			Data    struct {
				Source  string `json:"source"`
				Package string `json:"package"`
				Scheme  string `json:"scheme"`
				PURL    string `json:"purl"`
			} `json:"data"`
		}
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Data.Source != "npm" || !strings.HasPrefix(p.Subject, "npm:@acme/widget@") || p.Data.Package != "@acme/widget" {
			t.Errorf("source %q subject %q package %q: want the base kind npm", p.Data.Source, p.Subject, p.Data.Package)
		}
		if !strings.HasPrefix(p.Data.PURL, "pkg:npm/%40acme/widget@") || !strings.Contains(p.Data.PURL, "repository_url=") ||
			!strings.Contains(p.Data.PURL, strings.TrimPrefix(internal.URL, "http://")) {
			t.Errorf("purl %q: want type npm and the instance's repository_url", p.Data.PURL)
		}
		if p.Data.Scheme != "semver" {
			t.Errorf("scheme %q, want the default of npm", p.Data.Scheme)
		}
	}
}

// AC3: an unknown instance name is refused by the watch file (and so by
// `config check --watches`, and by the API, which parse with the same
// registry).
func TestUnknownInstanceRefusedByWatchFileAndConfigCheck(t *testing.T) {
	_, cfg, _, _, _, _ := twoNPMInstances(t)
	const watch = "version: 1\nwatches:\n  - {name: a, source: %s, coordinates: {name: x}}\n"
	known := writeFile(t, "known.yaml", fmt.Sprintf(watch, "npm-internal"))
	unknown := writeFile(t, "unknown.yaml", fmt.Sprintf(watch, "npm-elsewhere"))
	if _, err := parseWatchFile(cfg, known); err != nil {
		t.Fatalf("known instance: %v", err)
	}
	if _, err := parseWatchFile(cfg, unknown); err == nil || !strings.Contains(err.Error(), "watches[0].source: must be one of") {
		t.Fatalf("unknown instance: %v", err)
	}
	// The same configuration without the instance does not know the name.
	delete(cfg.Sources, "npm-internal")
	if _, err := parseWatchFile(cfg, known); err == nil {
		t.Fatal("a name no longer configured is accepted")
	}
}

func TestBuiltinSourcesRefuseABadInstance(t *testing.T) {
	for name, tc := range map[string]struct {
		src  map[string]config.Source
		want string
	}{
		"unknown kind":        {map[string]config.Source{"mine": {Kind: "oci"}}, `unknown source kind "oci"`},
		"built-in name other": {map[string]config.Source{"npm": {Kind: "pypi"}}, "npm is a built-in kind"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.Sources = tc.src
			if _, err := builtinSources(cfg, &http.Client{}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	cfg := config.Default()
	cfg.Sources = map[string]config.Source{"npm": {Kind: "npm"}}
	if _, err := builtinSources(cfg, &http.Client{}, nil, nil, nil); err != nil {
		t.Fatalf("a built-in naming its own kind: %v", err)
	}
}

// The instance's secrets are checked against its own base_url host, and the
// event's repository_url follows the instance, not the kind.
func TestNamedInstanceSecretsAndRegistryURL(t *testing.T) {
	cfg := config.Default()
	cfg.Sources["npm-internal"] = config.Source{Kind: "npm", BaseURL: "https://nexus.example.com/repository/npm-group/", Auth: config.SourceAuth{TokenRef: "nexus-npm-token"}} //nolint:gosec // G101: a secret name, not a value
	refs := sourceRefs(cfg)
	if len(refs) != 1 || refs[0].Key != "sources.npm-internal.auth.token_ref" || refs[0].URL.Host != "nexus.example.com" {
		t.Fatalf("refs = %+v", refs)
	}
	err := checkSourceSecrets(cfg, map[string][]string{"nexus-npm-token": {"other.example.com"}})
	if err == nil || !strings.Contains(err.Error(), "sources.npm-internal.auth.token_ref") {
		t.Fatalf("err = %v, want the binding refused for the instance", err)
	}
	if err := checkSourceSecrets(cfg, map[string][]string{"nexus-npm-token": {"nexus.example.com"}}); err != nil {
		t.Fatal(err)
	}
	r := registryURL(cfg)
	if got := r("npm-internal"); got != "https://nexus.example.com/repository/npm-group" {
		t.Errorf("registryURL(npm-internal) = %q", got)
	}
	if got := r("npm"); got != "" {
		t.Errorf("registryURL(npm) = %q, want none for the public registry", got)
	}
	cfg.Sources["npm-public-copy"] = config.Source{Kind: "npm"}
	if got := r("npm-public-copy"); got != "" {
		t.Errorf("an instance at the public registry: %q", got)
	}
}

const widgetWatch = `version: 1
watches:
  - {name: widget, source: npm-internal, coordinates: {name: widget}}
`

// applyWidget applies the watch file widgetWatch under cfg and returns the
// stored target of the instance.
func applyWidget(t *testing.T, e *regEnv, cfg *config.Config) (store.Target, error) {
	t.Helper()
	f, err := parseWatchFile(cfg, writeFile(t, "w.yaml", widgetWatch))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(t.Context(), e.st, f, watchfile.ApplyOptions{}); err != nil {
		return store.Target{}, err
	}
	var tgt store.Target
	err = e.st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		tgt, err = tx.FindTarget(t.Context(), "npm-internal", map[string]string{"name": "widget"})
		return err
	})
	return tgt, err
}

// B1: a target keeps the kind it was created for. An instance that is later
// configured as another kind is not checked (no fetch, no events), and apply
// refuses to point a watch at it.
func TestInstanceThatChangedKindIsNotCheckedOrApplied(t *testing.T) {
	e, cfg, _, internal, _, internalHits := twoNPMInstances(t)
	tgt, err := applyWidget(t, e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.SourceKind != "npm" || tgt.Kind != "npm-internal" {
		t.Fatalf("target kind %q source kind %q", tgt.Kind, tgt.SourceKind)
	}

	cfg.Sources["npm-internal"] = config.Source{Kind: "pypi", BaseURL: internal.URL}
	reg := e.registry(t, cfg)
	chk := &detect.Checker{Store: e.st, Sources: reg, Instance: "test", RegistryURL: registryURL(cfg), Matcher: event.NewMatcherCache()}
	_, err = chk.Check(t.Context(), tgt)
	if !errors.Is(err, scheduler.ErrPermanent) || !strings.Contains(err.Error(), "created for kind") {
		t.Fatalf("Check: %v, want a permanent error naming the stored kind", err)
	}
	if internalHits.Load() != 0 {
		t.Errorf("the registry was fetched %d times", internalHits.Load())
	}
	if evs, _ := e.st.ListEventsAfter(t.Context(), 0, 10); len(evs) != 0 {
		t.Errorf("%d events", len(evs))
	}
	if _, err := applyWidget(t, e, cfg); !errors.Is(err, watchfile.ErrSourceKindChanged) {
		t.Fatalf("apply: %v, want ErrSourceKindChanged", err)
	}
	// Configuring the old kind again fixes both.
	cfg.Sources["npm-internal"] = config.Source{Kind: "npm", BaseURL: internal.URL}
	if _, err := applyWidget(t, e, cfg); err != nil {
		t.Fatalf("apply with the old kind: %v", err)
	}
}

// B1: a target stored before instances (no source kind) means "the source of
// kind <name>". A name that later becomes an instance of another kind, as
// when a future built-in kind takes the name of an instance, does not
// reinterpret it.
func TestStoredTargetIsRefusedWhenItsNameChangesKind(t *testing.T) {
	e, cfg, _, _, _, hits := twoNPMInstances(t)
	// Written as an older version would have: kind "acme", no source kind.
	old := store.Target{ID: "t-acme", Kind: "acme", Coordinates: map[string]string{"name": "widget"}, NextCheckAt: time.Now()}
	if err := e.st.CreateTarget(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	cfg.Sources["acme"] = config.Source{Kind: "npm", BaseURL: cfg.Sources["npm-internal"].BaseURL}
	chk := &detect.Checker{Store: e.st, Sources: e.registry(t, cfg), Instance: "test"}
	if _, err := chk.Check(t.Context(), old); !errors.Is(err, scheduler.ErrPermanent) {
		t.Fatalf("Check: %v, want a permanent error", err)
	}
	if hits.Load() != 0 {
		t.Error("fetched")
	}
	// The config check says so too.
	reg := e.registry(t, cfg)
	warns := storedSourceWarnings(reg, []store.TargetSource{{Name: "acme", Targets: 2}, {Name: "npm-internal", Kind: "npm", Targets: 1}, {Name: "gone", Kind: "npm", Targets: 1}, {Name: "wasm-kind", Targets: 1}}, []string{"wasm-kind"})
	if len(warns) != 2 || !strings.Contains(warns[0], `2 stored targets of the source "acme" were created for kind acme, but it is now of kind npm`) ||
		!strings.Contains(warns[1], `1 stored target use the source "gone", which is not configured`) {
		t.Fatalf("warnings = %q", warns)
	}
}

// Q2: an instance cannot take the kind of an installed plugin. S2: a sources
// key that nothing reads is warned about.
func TestInstanceNameChecksAgainstPlugins(t *testing.T) {
	cfg := config.Default()
	cfg.Sources = map[string]config.Source{
		"clash":     {Kind: "npm"},
		"fine":      {Kind: "npm"},
		"npm":       {BaseURL: "https://npm.example.org"},
		"typo-npm":  {BaseURL: "https://x.example.org"},
		"my-plugin": {BaseURL: "https://y.example.org"},
	}
	plugins := []config.InstalledPlugin{{Kind: "clash"}, {Kind: "my-plugin"}}
	err := checkInstancePluginClash(cfg, plugins)
	if err == nil || !strings.Contains(err.Error(), "sources.clash: a plugin of kind clash is installed") || strings.Contains(err.Error(), "fine") {
		t.Fatalf("err = %v", err)
	}
	if err := checkInstancePluginClash(cfg, nil); err != nil {
		t.Fatalf("no plugins: %v", err)
	}
	warns := sourceKeyWarnings(cfg, plugins)
	if len(warns) != 1 || !strings.Contains(warns[0], "sources.typo-npm: not a built-in source kind or an installed plugin") {
		t.Fatalf("warnings = %q", warns)
	}
}

// Q4 end to end on a database file: the read-only check lists the sources of
// the stored targets, and `config check` warns about a missing one.
func TestStoredTargetWarningsReadTheDatabase(t *testing.T) {
	e, cfg, _, _, _, _ := twoNPMInstances(t)
	if _, err := applyWidget(t, e, cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "s.db")
	st, err := sqlite.Open(t.Context(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f, err := parseWatchFile(cfg, writeFile(t, "w.yaml", widgetWatch))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(t.Context(), st, f, watchfile.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	cfg.Database.DSN = "sqlite://" + path
	if w := storedTargetWarnings(t.Context(), cfg, nil); len(w) != 0 {
		t.Fatalf("configured instance: %q", w)
	}
	delete(cfg.Sources, "npm-internal")
	w := storedTargetWarnings(t.Context(), cfg, nil)
	if len(w) != 1 || !strings.Contains(w[0], `"npm-internal", which is not configured`) {
		t.Fatalf("instance removed: %q", w)
	}
	cfg.Database.DSN = "sqlite://" + filepath.Join(t.TempDir(), "none.db")
	if w := storedTargetWarnings(t.Context(), cfg, nil); len(w) != 0 {
		t.Fatalf("no database: %q", w)
	}
}

// capture serves 404 and records every request header it gets.
type capture struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []http.Header
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.seen = append(c.seen, r.Header.Clone())
		c.mu.Unlock()
		http.NotFound(w, r)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// carries reports whether a request had at least one header holding v.
func (c *capture) carries(v string) (requests int, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, h := range c.seen {
		for _, vs := range h {
			for _, x := range vs {
				if strings.Contains(x, v) {
					found = true
				}
			}
		}
	}
	return len(c.seen), found
}

func envURLs(m map[string]*capture) []string {
	var out []string
	for name, c := range m {
		out = append(out, "ANGELIA_SOURCES_"+strings.ToUpper(strings.ReplaceAll(name, "-", "_"))+"_BASE_URL="+c.srv.URL)
	}
	return out
}

func loadYAML(t *testing.T, yaml string, extraEnv ...string) *config.Config {
	t.Helper()
	cfg, err := config.Load(t.Context(), config.Options{Path: writeFile(t, "c.yaml", yaml), Environ: extraEnv})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// S4: token_env belongs to the source it is set on, never to the other
// sources of the same kind, in both directions, for the kinds that read it
// themselves (github-release, github-tag, gitlab-release).
func TestTokenEnvIsIsolatedBetweenInstancesAndKinds(t *testing.T) {
	const httpOpts = "http: {allow_http_hosts: [127.0.0.1], allow_networks: [127.0.0.0/8]}\n"
	coords := map[string]map[string]string{
		"github-release": {"repo": "o/r"}, "github-tag": {"repo": "o/r"}, "gitlab-release": {"project": "g/p"},
	}
	// "tok" variants: the instance has the token; "base" variants: the base
	// kind has it.
	for variant, yaml := range map[string]string{
		"instance-has-token": httpOpts + `sources:
  github-release: {}
  gh-tok: {kind: github-release, token_env: TOK_ENV}
  github-tag: {}
  tag-tok: {kind: github-tag, token_env: TOK_ENV}
  gitlab-release: {}
  gl-tok: {kind: gitlab-release, token_env: TOK_ENV}
`,
		"base-has-token": httpOpts + `sources:
  github-release: {token_env: TOK_ENV}
  gh-tok: {kind: github-release}
  github-tag: {token_env: TOK_ENV}
  tag-tok: {kind: github-tag}
  gitlab-release: {token_env: TOK_ENV}
  gl-tok: {kind: gitlab-release}
`,
	} {
		t.Run(variant, func(t *testing.T) {
			caps := map[string]*capture{}
			for _, n := range []string{"github-release", "gh-tok", "github-tag", "tag-tok", "gitlab-release", "gl-tok"} {
				caps[n] = newCapture(t)
			}
			cfg := loadYAML(t, yaml, append(envURLs(caps), "TOK_ENV=CANARY-token-env-0123")...)
			reg := newRegEnv(t).registry(t, cfg)
			withToken := map[bool][]string{
				true:  {"gh-tok", "tag-tok", "gl-tok"},
				false: {"github-release", "github-tag", "gitlab-release"},
			}[variant == "instance-has-token"]
			for name, c := range caps {
				src, err := reg.Get(name)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = src.List(t.Context(), source.Target{Kind: src.Kind(), Coordinates: coords[src.Kind()]}, source.FetchState{})
				n, found := c.carries("CANARY-token-env-0123")
				if n == 0 {
					t.Fatalf("%s: no request", name)
				}
				if want := slices.Contains(withToken, name); found != want {
					t.Errorf("%s: token sent = %v, want %v", name, found, want)
				}
			}
		})
	}
}

// S4: two npm instances each send their own token_env, to their own registry.
func TestTokenEnvOfTwoNPMInstances(t *testing.T) {
	var hitsA, hitsB atomic.Int32
	a := httptest.NewServer(countingNPM("CANARY-token-a", &hitsA))
	b := httptest.NewServer(countingNPM("CANARY-token-b", &hitsB))
	defer a.Close()
	defer b.Close()
	cfg := loadYAML(t, `http: {allow_http_hosts: [127.0.0.1], allow_networks: [127.0.0.0/8]}
sources:
  npm-a: {kind: npm, token_env: TOKEN_A}
  npm-b: {kind: npm, token_env: TOKEN_B}
`, "ANGELIA_SOURCES_NPM_A_BASE_URL="+a.URL, "ANGELIA_SOURCES_NPM_B_BASE_URL="+b.URL, "TOKEN_A=CANARY-token-a", "TOKEN_B=CANARY-token-b")
	reg := newRegEnv(t).registry(t, cfg)
	for _, name := range []string{"npm-a", "npm-b"} {
		src, _ := reg.Get(name)
		if res, err := src.List(t.Context(), npmTarget(), source.FetchState{}); err != nil || len(res.Versions) != 2 {
			t.Errorf("%s: %d versions, %v", name, len(res.Versions), err)
		}
	}
}

// N2: metrics wrap each source and label it by its kind, so instances of one
// kind share a series.
func TestMetricsSeeTheKindNotTheInstanceName(t *testing.T) {
	cfg := config.Default()
	cfg.Sources["npm-internal"] = config.Source{Kind: "npm"}
	var kinds []string
	_, err := builtinSources(cfg, &http.Client{}, nil, func(s source.Source) source.Source {
		kinds = append(kinds, s.Kind())
		return s
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := slices.DeleteFunc(slices.Clone(kinds), func(k string) bool { return k != "npm" }); len(n) != 2 {
		t.Fatalf("wrapped kinds = %v, want npm twice and no instance name", kinds)
	}
	if slices.Contains(kinds, "npm-internal") {
		t.Fatalf("wrapped kinds = %v", kinds)
	}
}
