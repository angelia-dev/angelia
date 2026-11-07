package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
)

// The crates, github-tag and gitlab-release sources are registered (#207): the
// watch file, the API and serve accept them, with their token and base_url.

// wantSourceKinds are the built-in source kinds. A new kind is added to
// builtinSources and here.
var wantSourceKinds = []string{"crates", "github-release", "github-tag", "gitlab-release", "npm", "pypi"}

// coordsOf are valid watch-file coordinates for each kind.
var coordsOf = map[string]map[string]string{
	"crates":         {"name": "angelia-test-widget"},
	"github-release": {"repo": "example/tool"},
	"github-tag":     {"repo": "example/tool"},
	"gitlab-release": {"project": "example-group/sub/tool"},
	"npm":            {"name": "left-pad"},
	"pypi":           {"name": "requests"},
}

// AC1: serve and the watch file register the same kinds, all of them.
func TestBuiltinSourcesKinds(t *testing.T) {
	for name, build := range map[string]func() (*source.Registry, error){
		"watch file": func() (*source.Registry, error) { return watchSources(&config.Config{}) },
		"serve": func() (*source.Registry, error) {
			return builtinSources(&config.Config{}, &http.Client{}, nil, nil, nil)
		},
	} {
		reg, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := slices.Sorted(slices.Values(reg.Kinds())); !slices.Equal(got, wantSourceKinds) {
			t.Errorf("%s: kinds %v, want %v", name, got, wantSourceKinds)
		}
	}
}

// AC1: the watch file accepts a watch of each kind, and the API lists each as
// a builtin source plugin.
func TestWatchFileAndAPIAcceptNewSourceKinds(t *testing.T) {
	var b strings.Builder
	b.WriteString("version: 1\nwatches:\n")
	for _, k := range wantSourceKinds {
		b.WriteString("  - name: w-" + k + "\n    source: " + k + "\n    coordinates:\n")
		for ck, cv := range coordsOf[k] {
			b.WriteString("      " + ck + ": " + cv + "\n")
		}
	}
	p := filepath.Join(t.TempDir(), "watches.yaml")
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := parseWatchFile(config.Default(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Watches) != len(wantSourceKinds) {
		t.Fatalf("watches = %d, want %d", len(f.Watches), len(wantSourceKinds))
	}
	reg, _ := watchSources(config.Default())
	var listed []string
	for _, p := range builtinPlugins(reg.Kinds()) {
		if p.Interface == "source" && p.Runtime == "builtin" {
			listed = append(listed, p.Kind)
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(listed)), wantSourceKinds) {
		t.Errorf("API source kinds = %v", listed)
	}
}

// recorder is an upstream that records the credential headers it receives and
// answers with a body.
type recorder struct {
	mu      sync.Mutex
	auth    []string
	private []string
	paths   []string
	body    func(path string) string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	r.private = append(r.private, req.Header.Get("Private-Token"))
	r.paths = append(r.paths, req.URL.EscapedPath())
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(r.body(req.URL.EscapedPath())))
}

func (r *recorder) only(t *testing.T) (auth, private string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.auth) == 0 {
		t.Fatal("no request reached the upstream")
	}
	for i := range r.auth {
		if r.auth[i] != r.auth[0] || r.private[i] != r.private[0] {
			t.Fatalf("requests carry different credentials: %q %q", r.auth, r.private)
		}
	}
	return r.auth[0], r.private[0]
}

const gitlabReleases = `[{"tag_name":"v1.0.0","name":"1.0.0","description":"","created_at":"2026-10-10T09:00:00.000Z","released_at":"2026-10-10T09:05:00.000Z","upcoming_release":false,"commit":{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1"},"_links":{"self":"http://x/-/releases/v1.0.0"}}]`

const githubTags = `[{"name":"v1.0.0","commit":{"sha":"c0000000000000000000000000000000000000a1","url":"http://x/c"}}]`

func gitlabTarget() source.Target {
	return source.Target{Kind: "gitlab-release", Coordinates: coordsOf["gitlab-release"]}
}

// AC2: gitlab-release with token_env sends GitLab's PRIVATE-TOKEN header, to
// the self-managed instance named by base_url.
func TestGitLabTokenEnvAndBaseURL(t *testing.T) {
	up := &recorder{body: func(string) string { return gitlabReleases }}
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := config.Default()
	cfg.Sources["gitlab-release"] = config.Source{BaseURL: srv.URL, TokenEnv: "GL_TEST_TOKEN"} //nolint:gosec // G101: an environment variable name
	cfg.Tokens["gitlab-release"] = secrets.NewValueString(regCanary)
	e := newRegEnv(t)
	res, err := e.list(t, cfg, gitlabTarget())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(res.Versions))
	}
	auth, private := up.only(t)
	if private != regCanary || auth != "" {
		t.Errorf("headers: Authorization %q, PRIVATE-TOKEN %q; want PRIVATE-TOKEN only", auth, private)
	}
	if !strings.HasPrefix(up.paths[0], "/api/v4/projects/example-group%2Fsub%2Ftool/releases") {
		t.Errorf("path = %q", up.paths[0])
	}
	noRegLeak(t, e.logs.String())
}

// AC2: gitlab-release with auth.token_ref sends the stored secret as a bearer
// token (GitLab accepts it for access tokens and OAuth tokens).
func TestGitLabStoredTokenIsBearer(t *testing.T) {
	up := &recorder{body: func(string) string { return gitlabReleases }}
	srv := httptest.NewServer(up)
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "gl-token", regCanary, hostOf(t, srv.URL))
	cfg := config.Default()
	cfg.Sources["gitlab-release"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: "gl-token"}}
	if _, err := e.list(t, cfg, gitlabTarget()); err != nil {
		t.Fatal(err)
	}
	auth, private := up.only(t)
	if auth != "Bearer "+regCanary || private != "" {
		t.Errorf("headers: Authorization %q, PRIVATE-TOKEN %q; want a bearer only", auth, private)
	}
	noRegLeak(t, e.logs.String())
}

func githubTagBody(path string) string {
	if strings.Contains(path, "/git/commits/") {
		return `{"sha":"c0000000000000000000000000000000000000a1","committer":{"date":"2026-06-01T09:00:00Z"},"author":{"date":"2026-06-01T09:00:00Z"}}`
	}
	return githubTags
}

// AC2: github-tag sends token_env and auth.token_ref as bearer tokens, like
// github-release, to the base_url (GitHub Enterprise Server).
func TestGitHubTagTokens(t *testing.T) {
	tgt := source.Target{Kind: "github-tag", Coordinates: coordsOf["github-tag"]}
	t.Run("token_env", func(t *testing.T) {
		up := &recorder{body: githubTagBody}
		srv := httptest.NewServer(up)
		defer srv.Close()
		cfg := config.Default()
		cfg.Sources["github-tag"] = config.Source{BaseURL: srv.URL + "/api/v3", TokenEnv: "GH_TEST_TOKEN"} //nolint:gosec // G101: an environment variable name
		cfg.Tokens["github-tag"] = secrets.NewValueString(regCanary)
		e := newRegEnv(t)
		if _, err := e.list(t, cfg, tgt); err != nil {
			t.Fatal(err)
		}
		if auth, _ := up.only(t); auth != "Bearer "+regCanary {
			t.Errorf("Authorization = %q", auth)
		}
		if !strings.HasPrefix(up.paths[0], "/api/v3/repos/example/tool/tags") {
			t.Errorf("path = %q", up.paths[0])
		}
		noRegLeak(t, e.logs.String())
	})
	t.Run("auth", func(t *testing.T) {
		up := &recorder{body: githubTagBody}
		srv := httptest.NewServer(up)
		defer srv.Close()
		e := newRegEnv(t)
		e.put(t, "ghes-token", regCanary, hostOf(t, srv.URL))
		cfg := config.Default()
		cfg.Sources["github-tag"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: "ghes-token"}}
		if _, err := e.list(t, cfg, tgt); err != nil {
			t.Fatal(err)
		}
		if auth, _ := up.only(t); auth != "Bearer "+regCanary {
			t.Errorf("Authorization = %q", auth)
		}
	})
}

const cratesIndex = `{"name":"angelia-test-widget","vers":"0.1.0","deps":[],"cksum":"1111111111111111111111111111111111111111111111111111111111111111","features":{},"yanked":false}` + "\n"

// AC1: crates reads the sparse index at base_url (a mirror), and, as no
// credential is defined for it, never sends token_env.
func TestCratesBaseURLAndNoCredential(t *testing.T) {
	up := &recorder{body: func(string) string { return cratesIndex }}
	srv := httptest.NewServer(up)
	defer srv.Close()
	cfg := config.Default()
	cfg.Sources["crates"] = config.Source{BaseURL: srv.URL, TokenEnv: "CRATES_TEST_TOKEN"} //nolint:gosec // G101: an environment variable name
	cfg.Tokens["crates"] = secrets.NewValueString(regCanary)
	e := newRegEnv(t)
	res, err := e.list(t, cfg, source.Target{Kind: "crates", Coordinates: coordsOf["crates"]})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(res.Versions))
	}
	if auth, private := up.only(t); auth != "" || private != "" {
		t.Errorf("crates sent a credential: %q %q", auth, private)
	}
	if up.paths[0] != "/an/ge/angelia-test-widget" {
		t.Errorf("index path = %q", up.paths[0])
	}
}

// AC2: config check accepts a config that names the new kinds, with no
// "ignored" warning, and still warns for a kind that sends no credential.
func TestConfigCheckKnowsCredentialKinds(t *testing.T) {
	t.Setenv("GL_TEST_TOKEN", "glpat-not-a-real-one")
	cfg, err := config.Load(t.Context(), config.Options{
		Path:    writeCfg(t, "sources:\n  gitlab-release: {base_url: 'https://gitlab.example.com', token_env: GL_TEST_TOKEN}\n  github-tag: {token_env: GL_TEST_TOKEN}\n  crates: {token_env: GL_TEST_TOKEN}\n"),
		Environ: os.Environ(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := strings.Join(cfg.Warnings(), "\n")
	if strings.Contains(w, "sources.gitlab-release") || strings.Contains(w, "sources.github-tag") {
		t.Errorf("warnings = %q", w)
	}
	if !strings.Contains(w, "sources.crates") {
		t.Errorf("no warning for crates: %q", w)
	}
}

// A plugin whose kind is a built-in kind (the crates example plugin once was)
// is a config check warning: the built-in is used.
func TestConfigCheckWarnsAboutPluginKindOfABuiltin(t *testing.T) {
	for _, kind := range []string{"crates", "webhook"} {
		dir := t.TempDir()
		writePlugin(t, dir, kind, `{"kind":"`+kind+`","allowed_hosts":["crates.io"]}`, "")
		t.Setenv("ANGELIA_CONFIG", "")
		t.Setenv("ANGELIA_PLUGINS_DIR", dir)
		out, errb, err := runCheck(t)
		want := "warning: plugin kind " + kind + " is a built-in kind; the built-in is used"
		if err != nil || !strings.Contains(out, "configuration OK") || !strings.Contains(errb, want) {
			t.Errorf("%s: out=%q stderr=%q err=%v", kind, out, errb, err)
		}
	}
}

// The start-up secret check covers the new kinds: a secret bound to another
// host fails, one bound to the base_url host passes, and crates (no
// credential) produces no reference.
func TestCheckSourceSecretsNewKinds(t *testing.T) {
	for _, kind := range []string{"github-tag", "gitlab-release"} {
		cfg := config.Default()
		cfg.Sources[kind] = config.Source{BaseURL: "https://git.example.com", Auth: config.SourceAuth{TokenRef: "tok"}}
		err := checkSourceSecrets(cfg, map[string][]string{"tok": {"other.example.com"}})
		if err == nil || !strings.Contains(err.Error(), "sources."+kind+".auth.token_ref") {
			t.Errorf("%s wrong host: %v", kind, err)
		}
		if err := checkSourceSecrets(cfg, map[string][]string{"tok": {"git.example.com"}}); err != nil {
			t.Errorf("%s bound host: %v", kind, err)
		}
	}
	cfg := config.Default()
	cfg.Sources["crates"] = config.Source{Auth: config.SourceAuth{TokenRef: "tok"}}
	if refs := sourceRefs(cfg); len(refs) != 0 {
		t.Errorf("crates refs = %v", refs)
	}
}

// The purl repository_url qualifier: the default registry gives none, a
// mirror gives its base_url as written (API path included).
func TestRegistryURLNewKinds(t *testing.T) {
	cases := []struct{ kind, def, mirror string }{
		{"crates", "https://index.crates.io", "https://mirror.example.com/index"},
		{"github-tag", "https://api.github.com", "https://ghes.example.com/api/v3"},
		{"gitlab-release", "https://gitlab.com", "https://gitlab.example.com/api/v4"},
	}
	for _, c := range cases {
		cfg := &config.Config{Sources: map[string]config.Source{}}
		if got := registryURL(cfg)(c.kind); got != "" {
			t.Errorf("%s unset: %q", c.kind, got)
		}
		cfg.Sources[c.kind] = config.Source{BaseURL: c.def + "/"}
		if got := registryURL(cfg)(c.kind); got != "" {
			t.Errorf("%s default: %q", c.kind, got)
		}
		cfg.Sources[c.kind] = config.Source{BaseURL: c.mirror + "/"}
		if got := registryURL(cfg)(c.kind); got != c.mirror {
			t.Errorf("%s mirror: %q, want %q", c.kind, got, c.mirror)
		}
	}
}
