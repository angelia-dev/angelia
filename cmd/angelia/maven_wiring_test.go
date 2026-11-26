package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// The maven source (#96): registration, credentials and events.

const mavenRepoPrefix = "/repository/maven-releases"

func mavenTarget() source.Target {
	return source.Target{Kind: "maven", Coordinates: map[string]string{"package": "com.example.test:widget"}}
}

// fakeNexus mimics a Nexus Maven repository with basic auth (user ci,
// password regCanary unless check is set) serving the versions of
// com.example.test:widget under mavenRepoPrefix.
type fakeNexus struct {
	mu       sync.Mutex
	versions []string
	check    func(*http.Request) bool
}

func (f *fakeNexus) set(vs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions = vs
}

func (f *fakeNexus) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ok := f.check != nil && f.check(r)
	if f.check == nil {
		u, p, basic := r.BasicAuth()
		ok = basic && u == "ci" && p == regCanary
	}
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="Sonatype Nexus Repository Manager"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.URL.Path != mavenRepoPrefix+"/com/example/test/widget/maven-metadata.xml" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	vs := f.versions
	f.mu.Unlock()
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><metadata><groupId>com.example.test</groupId><artifactId>widget</artifactId><versioning><versions>`)
	for _, v := range vs {
		b.WriteString("<version>" + v + "</version>")
	}
	b.WriteString(`</versions></versioning></metadata>`)
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write([]byte(b.String()))
}

// auth: {type: basic} reaches the repository when the secret is bound to its
// host, and is refused without a request when it is bound elsewhere.
func TestMavenBasicAuth(t *testing.T) {
	var hits atomic.Int32
	nexus := &fakeNexus{versions: []string{"1.0.0", "1.1.0"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		nexus.ServeHTTP(w, r)
	}))
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "nexus-password", regCanary, hostOf(t, srv.URL))
	e.put(t, "elsewhere", regCanary, "nexus.example.test")
	for _, tc := range []struct {
		ref     string
		wantErr error
	}{{"nexus-password", nil}, {"elsewhere", secrets.ErrHostMismatch}} {
		hits.Store(0)
		cfg := config.Default()
		cfg.Sources["nexus"] = config.Source{
			Kind: "maven", BaseURL: srv.URL + mavenRepoPrefix,
			Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci", PasswordRef: tc.ref},
		}
		cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
		cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
		src, err := e.registry(t, cfg).Get("nexus")
		if err != nil {
			t.Fatal(err)
		}
		res, err := src.List(t.Context(), mavenTarget(), source.FetchState{})
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) || hits.Load() != 0 {
				t.Errorf("%s: err = %v, %d requests; want %v and none", tc.ref, err, hits.Load(), tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Versions) != 2 || res.Versions[0].Raw != "1.1.0" || res.Versions[0].URL != "" {
			t.Fatalf("versions = %+v", res.Versions)
		}
		out, _ := json.Marshal(res)
		noRegLeak(t, string(out)+e.logs.String())
	}
}

// token_env sends a bearer token to the repository.
func TestMavenTokenEnvIsBearer(t *testing.T) {
	nexus := &fakeNexus{versions: []string{"1.0.0"}, check: func(r *http.Request) bool {
		return r.Header.Get("Authorization") == "Bearer "+regCanary
	}}
	srv := httptest.NewServer(nexus)
	defer srv.Close()
	cfg := config.Default()
	cfg.Sources["maven"] = config.Source{BaseURL: srv.URL + mavenRepoPrefix, TokenEnv: "MVN_TEST_TOKEN"} //nolint:gosec // G101: an environment variable name
	cfg.Tokens["maven"] = secrets.NewValueString(regCanary)
	e := newRegEnv(t)
	res, err := e.list(t, cfg, mavenTarget())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 1 {
		t.Fatalf("versions = %d", len(res.Versions))
	}
	noRegLeak(t, e.logs.String())
}

// A repository that redirects to a storage host the secret is not bound to
// fails with ErrHostMismatch, and the storage host receives no credential.
func TestMavenRedirectToUnboundHost(t *testing.T) {
	var storageAuth []string
	var mu sync.Mutex
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		storageAuth = append(storageAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		_, _ = w.Write([]byte(`<metadata><versioning><versions><version>1.0</version></versions></versioning></metadata>`))
	}))
	defer storage.Close()
	repo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+r.URL.Path, http.StatusFound) //nolint:gosec // G710: the target is the test's own storage server.
	}))
	defer repo.Close()
	e := newRegEnv(t)
	e.put(t, "repo-password", regCanary, hostOf(t, repo.URL))
	cfg := config.Default()
	cfg.Sources["maven"] = config.Source{BaseURL: repo.URL, Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci", PasswordRef: "repo-password"}}
	_, err := e.list(t, cfg, mavenTarget())
	if !errors.Is(err, secrets.ErrHostMismatch) {
		t.Fatalf("err = %v, want ErrHostMismatch", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, a := range storageAuth {
		if a != "" {
			t.Errorf("the storage host received a credential: %q", a)
		}
	}
	noRegLeak(t, err.Error()+e.logs.String())
}

// The start-up secret check covers maven instances, and config check does not
// warn that maven ignores its credential.
func TestMavenCredentialChecks(t *testing.T) {
	cfg := config.Default()
	cfg.Sources["nexus"] = config.Source{
		Kind: "maven", BaseURL: "https://nexus.example.test/repository/maven-releases",
		Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci", PasswordRef: "pw"},
	}
	err := checkSourceSecrets(cfg, map[string][]string{"pw": {"other.example.test"}})
	if err == nil || !strings.Contains(err.Error(), "sources.nexus.auth.password_ref") {
		t.Errorf("wrong host: %v", err)
	}
	if err := checkSourceSecrets(cfg, map[string][]string{"pw": {"nexus.example.test"}}); err != nil {
		t.Errorf("bound host: %v", err)
	}
	if !config.SendsCredential("maven") {
		t.Error("maven does not send credentials")
	}
}

func TestRegistryURLMaven(t *testing.T) {
	for base, want := range map[string]string{
		"":                                     "",
		"https://repo.maven.apache.org/maven2": "",
		"https://repo1.maven.org/maven2/":      "",
		"https://nexus.example.test/repository/maven-releases/": "https://nexus.example.test/repository/maven-releases",
	} {
		cfg := &config.Config{Sources: map[string]config.Source{"maven": {BaseURL: base}, "nexus": {Kind: "maven", BaseURL: base}}}
		for _, name := range []string{"maven", "nexus"} {
			if got := registryURL(cfg)(name); got != want {
				t.Errorf("%s %q: %q, want %q", name, base, got, want)
			}
		}
	}
}

// A maven watch from the watch file produces events with data.source maven,
// data.package g:a, data.scheme maven and a maven purl; a private instance
// adds repository_url.
func TestMavenWatchEvents(t *testing.T) {
	nexus := &fakeNexus{versions: []string{"1.0.0"}}
	srv := httptest.NewServer(nexus)
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "nexus-password", regCanary, hostOf(t, srv.URL))
	cfg := config.Default()
	cfg.Sources["nexus"] = config.Source{
		Kind: "maven", BaseURL: srv.URL + mavenRepoPrefix,
		Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci", PasswordRef: "nexus-password"},
	}
	cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
	reg := e.registry(t, cfg)
	file := writeFile(t, "w.yaml", `version: 1
sinks:
  - {name: hook, kind: webhook, config: {url: 'https://hook.example.org/x', secret_env: HOOK_SECRET}}
subscriptions:
  - {name: all, sink: hook}
watches:
  - name: widget
    source: nexus
    coordinates: {package: 'com.example.test:widget'}
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
		tgt, err = tx.FindTarget(t.Context(), "nexus", map[string]string{"package": "com.example.test:widget"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	chk := &detect.Checker{
		Store: e.st, Sources: reg, Instance: "test", RegistryURL: registryURL(cfg), Matcher: event.NewMatcherCache(),
		Now: func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) },
	}
	if _, err := chk.Check(t.Context(), tgt); err != nil {
		t.Fatal(err)
	}
	nexus.set("1.0.0", "1.2.0-SNAPSHOT", "1.2.0")
	if _, err := chk.Check(t.Context(), tgt); err != nil {
		t.Fatal(err)
	}
	evs, err := e.st.ListEventsAfter(t.Context(), 0, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("events = %d, %v; want one (1.2.0; the snapshot is a pre-release)", len(evs), err)
	}
	var p struct {
		Data struct {
			Source  string `json:"source"`
			Package string `json:"package"`
			Version string `json:"version"`
			Scheme  string `json:"scheme"`
			PURL    string `json:"purl"`
		} `json:"data"`
	}
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	d := p.Data
	if d.Source != "maven" || d.Package != "com.example.test:widget" || d.Version != "1.2.0" || d.Scheme != "maven" {
		t.Errorf("data = %+v", d)
	}
	if !strings.HasPrefix(d.PURL, "pkg:maven/com.example.test/widget@1.2.0?repository_url=") ||
		!strings.Contains(d.PURL, strings.TrimPrefix(srv.URL, "http://")) {
		t.Errorf("purl = %q, want the maven purl with the instance's repository_url", d.PURL)
	}
	noRegLeak(t, string(evs[0].Payload)+e.logs.String())
}
