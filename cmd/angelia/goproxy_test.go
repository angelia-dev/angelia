package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/detect"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// The go-proxy source (#95): registered as a built-in kind, sends token_env
// and auth like npm and pypi, and its events keep the module path's case.

// fakeGoProxy is a GOPROXY server under prefix for example.test/Team/Lib. It
// serves v1.1.0, and v1.2.0 too once more is set. A non-empty user makes it
// require basic auth.
type fakeGoProxy struct {
	prefix, user, password string
	more                   atomic.Bool
	hits                   atomic.Int32
}

func (f *fakeGoProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.hits.Add(1)
	if f.user != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.password {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	const base = "/example.test/!team/!lib/@v/"
	switch r.URL.Path {
	case f.prefix + base + "list":
		list := "v1.1.0\n"
		if f.more.Load() {
			list += "v1.2.0\n"
		}
		_, _ = w.Write([]byte(list))
	case f.prefix + base + "v1.1.0.info":
		_, _ = w.Write([]byte(`{"Version":"v1.1.0","Time":"2026-01-01T00:00:00Z"}`))
	case f.prefix + base + "v1.2.0.info":
		_, _ = w.Write([]byte(`{"Version":"v1.2.0","Time":"2026-02-01T00:00:00Z"}`))
	default:
		http.NotFound(w, r)
	}
}

func goProxyTarget() source.Target {
	return source.Target{Kind: "go-proxy", Coordinates: map[string]string{"module": "example.test/Team/Lib"}}
}

// A named go-proxy instance with a base_url path and basic auth reaches the
// proxy under that path with the credential.
func TestGoProxyInstanceWithPathAndBasicAuth(t *testing.T) {
	const prefix = "/artifactory/api/go/go-virtual"
	fp := &fakeGoProxy{prefix: prefix, user: "ci-bot", password: regCanary}
	srv := httptest.NewServer(fp)
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "go-password", regCanary, hostOf(t, srv.URL))
	cfg := config.Default()
	cfg.Sources["goproxy-corp"] = config.Source{Kind: "go-proxy", BaseURL: srv.URL + prefix, Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci-bot", PasswordRef: "go-password"}}
	cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")

	reg := e.registry(t, cfg)
	src, err := reg.Get("goproxy-corp")
	if err != nil {
		t.Fatal(err)
	}
	res, err := src.List(t.Context(), goProxyTarget(), source.FetchState{})
	if err != nil || len(res.Versions) != 1 || res.Versions[0].Raw != "v1.1.0" {
		t.Fatalf("versions = %+v, %v", res.Versions, err)
	}
	out, _ := json.Marshal(res)
	noRegLeak(t, string(out)+e.logs.String())
}

// token_env is sent as a Bearer credential to a go-proxy instance.
func TestGoProxyTokenEnvIsBearer(t *testing.T) {
	var got atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte("v1.0.0\n"))
	}))
	defer srv.Close()
	t.Setenv("GOPROXY_TEST_TOKEN", regCanary)
	cfg, err := config.Load(t.Context(), config.Options{Environ: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sources["goproxy-corp"] = config.Source{Kind: "go-proxy", BaseURL: srv.URL, TokenEnv: "GOPROXY_TEST_TOKEN"} //nolint:gosec // G101: an environment variable name
	cfg.Tokens["goproxy-corp"] = secrets.NewValueString(regCanary)
	cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
	e := newRegEnv(t)
	src, err := e.registry(t, cfg).Get("goproxy-corp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.List(t.Context(), goProxyTarget(), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if g, _ := got.Load().(string); g != "Bearer "+regCanary {
		t.Errorf("Authorization = %q, want the Bearer token", g)
	}
	noRegLeak(t, e.logs.String())
}

// A secret bound to another host is not sent and no request is made.
func TestGoProxySecretBoundToAnotherHostIsRefused(t *testing.T) {
	fp := &fakeGoProxy{}
	srv := httptest.NewServer(fp)
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "go-token", regCanary, "other.example.com")
	cfg := config.Default()
	cfg.Sources["go-proxy"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: "go-token"}}
	_, err := e.list(t, cfg, goProxyTarget())
	if !errors.Is(err, secrets.ErrHostMismatch) {
		t.Fatalf("err = %v, want ErrHostMismatch", err)
	}
	if fp.hits.Load() != 0 {
		t.Errorf("%d requests reached the proxy", fp.hits.Load())
	}
}

// The start-up secret check covers go-proxy, and the purl of the public
// proxy has no repository_url.
func TestGoProxySecretRefsAndRegistryURL(t *testing.T) {
	cfg := config.Default()
	cfg.Sources["goproxy-corp"] = config.Source{Kind: "go-proxy", BaseURL: "https://athens.example.test/", Auth: config.SourceAuth{TokenRef: "athens-token"}} //nolint:gosec // G101: a secret name
	refs := sourceRefs(cfg)
	if len(refs) != 1 || refs[0].Key != "sources.goproxy-corp.auth.token_ref" || refs[0].URL.Host != "athens.example.test" {
		t.Fatalf("refs = %+v", refs)
	}
	if err := checkSourceSecrets(cfg, map[string][]string{"athens-token": {"other.example.com"}}); err == nil {
		t.Error("a secret bound to another host passed the start-up check")
	}
	if err := checkSourceSecrets(cfg, map[string][]string{"athens-token": {"athens.example.test"}}); err != nil {
		t.Error(err)
	}
	r := registryURL(cfg)
	if got := r("go-proxy"); got != "" {
		t.Errorf("registryURL(go-proxy) = %q, want none for proxy.golang.org", got)
	}
	if got := r("goproxy-corp"); got != "https://athens.example.test" {
		t.Errorf("registryURL(goproxy-corp) = %q", got)
	}
}

// config check no longer says "no built-in source of this kind sends a
// credential" for go-proxy.
func TestGoProxyConfigHasNoCredentialWarning(t *testing.T) {
	t.Setenv("GOPROXY_TEST_TOKEN", "not-a-real-token")
	cfg, err := config.Load(t.Context(), config.Options{
		Path:    writeCfg(t, "sources:\n  go-proxy: {base_url: 'https://athens.example.test', token_env: GOPROXY_TEST_TOKEN}\n  goproxy-corp: {kind: go-proxy, base_url: 'https://nexus.example.test/repository/go-group', token_env: GOPROXY_TEST_TOKEN}\n"),
		Environ: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(cfg.Warnings(), "\n"); strings.Contains(w, "sources.go-proxy") || strings.Contains(w, "sources.goproxy-corp") {
		t.Errorf("warnings = %q", w)
	}
}

// A watch file applies a go-proxy watch and its events carry the module path
// with its case, the base kind and the SemVer scheme.
func TestGoProxyWatchFileEvents(t *testing.T) {
	for _, private := range []bool{false, true} {
		fp := &fakeGoProxy{}
		srv := httptest.NewServer(fp)
		e := newRegEnv(t)
		cfg := config.Default()
		cfg.Sources["goproxy-corp"] = config.Source{Kind: "go-proxy", BaseURL: srv.URL}
		cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
		cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
		reg := e.registry(t, cfg)
		f, err := parseWatchFile(cfg, writeFile(t, "w.yaml", `version: 1
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://hook.example.org/x', secret_env: HOOK_SECRET}}
subscriptions:
  - {name: all, sink: hook}
watches:
  - name: lib
    source: goproxy-corp
    coordinates: {module: example.test/Team/Lib}
`))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := watchfile.Apply(t.Context(), e.st, f, watchfile.ApplyOptions{}); err != nil {
			t.Fatal(err)
		}
		var tgt store.Target
		err = e.st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
			tgt, err = tx.FindTarget(t.Context(), "goproxy-corp", map[string]string{"module": "example.test/Team/Lib"})
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		regURL := registryURL(cfg)
		if !private {
			regURL = func(string) string { return "" } // as for the public proxy
		}
		chk := &detect.Checker{
			Store: e.st, Sources: reg, Instance: "test", RegistryURL: regURL, Matcher: event.NewMatcherCache(),
			Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
		}
		if _, err := chk.Check(t.Context(), tgt); err != nil { // baseline: v1.1.0
			t.Fatal(err)
		}
		fp.more.Store(true)
		if _, err := chk.Check(t.Context(), tgt); err != nil {
			t.Fatal(err)
		}
		evs, err := e.st.ListEventsAfter(t.Context(), 0, 10)
		if err != nil || len(evs) == 0 {
			t.Fatalf("events = %d, %v", len(evs), err)
		}
		for _, ev := range evs {
			var p struct {
				Data struct {
					Source  string `json:"source"`
					Package string `json:"package"`
					Scheme  string `json:"scheme"`
					PURL    string `json:"purl"`
				} `json:"data"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			d := p.Data
			if d.Source != "go-proxy" || d.Package != "example.test/Team/Lib" || d.Scheme != "semver" {
				t.Errorf("source %q package %q scheme %q", d.Source, d.Package, d.Scheme)
			}
			if !private && d.PURL != "pkg:golang/example.test/Team/Lib@v1.2.0" {
				t.Errorf("purl %q", d.PURL)
			}
			if private && (!strings.HasPrefix(d.PURL, "pkg:golang/example.test/Team/Lib@v1.2.0?") || !strings.Contains(d.PURL, "repository_url=")) {
				t.Errorf("purl %q, want a repository_url", d.PURL)
			}
		}
		srv.Close()
	}
}

// A retraction in a later go.mod turns a known, emitted version yanked and
// the check pipeline emits release.yanked for it (95-B).
func TestGoProxyRetractionEmitsYanked(t *testing.T) {
	var step atomic.Int32 // 0: v1.0.0; 1: and v1.1.0; 2: and v1.2.0, which retracts v1.1.0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const base = "/example.test/!team/!lib/"
		n := step.Load()
		latest := []string{"v1.0.0", "v1.1.0", "v1.2.0"}[n]
		switch p := strings.TrimPrefix(r.URL.Path, base); {
		case p == "@v/list":
			_, _ = w.Write([]byte(strings.Join([]string{"v1.0.0", "v1.1.0", "v1.2.0"}[:n+1], "\n") + "\n"))
		case p == "@latest":
			_, _ = w.Write([]byte(`{"Version":"` + latest + `","Time":"2026-03-01T00:00:00Z"}`))
		case p == "@v/"+latest+".mod":
			mod := "module example.test/Team/Lib\n"
			if n == 2 {
				mod += "\nretract v1.1.0 // broken release\n"
			}
			_, _ = w.Write([]byte(mod))
		case strings.HasSuffix(p, ".info"):
			_, _ = w.Write([]byte(`{"Time":"2026-03-01T00:00:00Z"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := newRegEnv(t)
	cfg := config.Default()
	cfg.Sources["goproxy-corp"] = config.Source{Kind: "go-proxy", BaseURL: srv.URL}
	cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
	reg := e.registry(t, cfg)
	f, err := parseWatchFile(cfg, writeFile(t, "w.yaml", `version: 1
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://hook.example.org/x', secret_env: HOOK_SECRET}}
subscriptions:
  - {name: all, sink: hook}
watches:
  - name: lib
    source: goproxy-corp
    coordinates: {module: example.test/Team/Lib}
`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Apply(t.Context(), e.st, f, watchfile.ApplyOptions{}); err != nil {
		t.Fatal(err)
	}
	var tgt store.Target
	err = e.st.InConfigTx(t.Context(), func(tx store.ConfigTx) (err error) {
		tgt, err = tx.FindTarget(t.Context(), "goproxy-corp", map[string]string{"module": "example.test/Team/Lib"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	chk := &detect.Checker{
		Store: e.st, Sources: reg, Instance: "test", RegistryURL: registryURL(cfg), Matcher: event.NewMatcherCache(),
		Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
	}
	for i := range int32(3) {
		step.Store(i)
		if _, err := chk.Check(t.Context(), tgt); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
	}
	evs, err := e.st.ListEventsAfter(t.Context(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var yankedVersion string
	for _, ev := range evs {
		types = append(types, ev.Type)
		if ev.Type == "dev.angelia.release.yanked.v1" {
			var p struct {
				Data struct {
					Version string `json:"version"`
				} `json:"data"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatal(err)
			}
			yankedVersion = p.Data.Version
		}
	}
	// v1.1.0 published, then v1.2.0 published and v1.1.0 yanked. v1.0.0 is
	// the baseline: no event.
	if yankedVersion != "v1.1.0" {
		t.Fatalf("event types %v, yanked version %q, want a release.yanked for v1.1.0", types, yankedVersion)
	}
	if n := strings.Count(strings.Join(types, " "), "release.yanked"); n != 1 {
		t.Errorf("%d yanked events: %v", n, types)
	}
}
