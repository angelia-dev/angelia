package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// Private registries and mirrors (#111): base_url and auth reach the source,
// the credential is a stored secret bound to the registry host, and nothing
// that carries it leaks.

// regCanary is a synthetic registry credential.
const regCanary = "CANARY-registry-token-0123456789abcdef"

// fakeVerdaccio mimics Verdaccio's npm API with auth on: a packument needs
// "Authorization: Bearer <token>", else 401 {"error": ...}.
func fakeVerdaccio(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"authorization required"}`))
			return
		}
		if r.URL.EscapedPath() != "/@acme%2Fwidget" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"@acme/widget","dist-tags":{"latest":"1.1.0"},
"versions":{"1.0.0":{"dist":{"tarball":"http://x/1.tgz"}},"1.1.0":{"dist":{"tarball":"http://x/2.tgz"}}},
"time":{"1.0.0":"2026-01-01T00:00:00.000Z","1.1.0":"2026-02-01T00:00:00.000Z"}}`))
	})
}

// fakeIndexMirror mimics a PyPI JSON-API mirror (devpi, Artifactory, Nexus
// proxy) with basic auth, under a repository path.
func fakeIndexMirror(user, password, prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="registry"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != prefix+"/pypi/demo/json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"info":{"name":"demo"},"releases":{"1.0":[{"upload_time_iso_8601":"2026-01-01T00:00:00Z","filename":"demo-1.0.tar.gz"}]}}`))
	})
}

type regEnv struct {
	st   *sqlite.Store
	keys *secrets.Keyring
	res  *secrets.Resolver
	logs *bytes.Buffer
	log  *slog.Logger
}

func newRegEnv(t *testing.T) *regEnv {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "s.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	keys, err := secrets.NewKeyring(bytes.Repeat([]byte{7}, secrets.KeySize))
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	return &regEnv{
		st: st, keys: keys, res: &secrets.Resolver{Store: st, Keys: keys}, logs: logs,
		log: slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
}

func (e *regEnv) put(t *testing.T, name, value string, hosts ...string) {
	t.Helper()
	id := "id-" + name
	env, kid, err := e.keys.Seal(id, name, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := store.Secret{ID: id, Name: name, Hosts: hosts, KeyID: kid, Ciphertext: env, CreatedAt: at, UpdatedAt: at}
	if err := e.st.InSecretTx(t.Context(), func(tx store.SecretTx) error { return tx.CreateSecret(t.Context(), s) }); err != nil {
		t.Fatal(err)
	}
}

// list runs source kind's List through the process's real wiring: a netguard
// client from cfg (loopback allowed) and builtinSources with the resolver.
func (e *regEnv) list(t *testing.T, cfg *config.Config, tgt source.Target) (source.Result, error) {
	t.Helper()
	cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
	cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
	client, err := newNetClient(cfg, e.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtinSources(cfg, client.HTTPClient(), e.log, nil, e.res)
	if err != nil {
		t.Fatal(err)
	}
	src, err := reg.Get(tgt.Kind)
	if err != nil {
		t.Fatal(err)
	}
	return src.List(t.Context(), tgt, source.FetchState{})
}

func hostOf(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	h, err := secrets.Destination(u)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func npmTarget() source.Target {
	return source.Target{Kind: "npm", Coordinates: map[string]string{"name": "@acme/widget"}}
}

// AC1 (default-test stand-in for Verdaccio) and AC2: npm with auth.token_ref.
func TestPrivateNPMRegistryWithStoredToken(t *testing.T) {
	srv := httptest.NewServer(fakeVerdaccio(regCanary))
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "verdaccio-token", regCanary, hostOf(t, srv.URL))
	cfg := config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{Type: config.AuthNPMAuthToken, TokenRef: "verdaccio-token"}}

	res, err := e.list(t, cfg, npmTarget())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(res.Versions))
	}
	// AC2: the credential is in no result, log or event-bound structure.
	out, _ := json.Marshal(res)
	noRegLeak(t, string(out)+e.logs.String()+fmt.Sprintf("%v %+v", cfg, cfg.Sources))
}

func noRegLeak(t *testing.T, out string) {
	t.Helper()
	if strings.Contains(out, regCanary) || strings.Contains(out, "CANARY") {
		t.Fatalf("the credential leaks: %s", out)
	}
}

func TestPrivateNPMRegistryWithoutCredentialIsRefusedByTheRegistry(t *testing.T) {
	srv := httptest.NewServer(fakeVerdaccio(regCanary))
	defer srv.Close()
	cfg := config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: srv.URL}
	_, err := newRegEnv(t).list(t, cfg, npmTarget())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want a 401", err)
	}
}

// fakePypiserver mimics pypiserver: only the Simple API, as PEP 503 HTML (it
// ignores Accept), behind basic auth. Like pypiserver it redirects
// /simple/<name> to the trailing-slash form.
func fakePypiserver(user, password string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, p, ok := r.BasicAuth(); !ok || u != user || p != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="pypiserver"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/simple/example-pkg/" {
			http.NotFound(w, r) // including /pypi/example-pkg/json
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><body>
<a href="../../packages/example_pkg-1.0.0.tar.gz#sha256=00">example_pkg-1.0.0.tar.gz</a><br>
<a href="../../packages/example_pkg-1.1.0-py3-none-any.whl#sha256=01">example_pkg-1.1.0-py3-none-any.whl</a><br>
</body></html>`))
	})
}

// sources.pypi.api: simple (#206) reaches a Simple-API-only server through
// the same client, with the stored credential, and api: json (the default)
// gets the 404 of the absent JSON API.
func TestPypiSimpleAPIWiring(t *testing.T) {
	srv := httptest.NewServer(fakePypiserver("ci", regCanary))
	defer srv.Close()
	tgt := source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "Example_Pkg"}}
	for _, tc := range []struct {
		name, key, kind, api string
		wantVersions         int
	}{
		{"built-in simple", "pypi", "", "simple", 2},
		{"named instance simple", "pypiserver", "pypi", "simple", 2},
		{"json default", "pypi", "", "", 0},
		{"json explicit", "pypi", "", "json", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRegEnv(t)
			e.put(t, "ps-password", regCanary, hostOf(t, srv.URL))
			cfg := config.Default()
			cfg.Sources[tc.key] = config.Source{Kind: tc.kind, API: tc.api, BaseURL: srv.URL, Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci", PasswordRef: "ps-password"}}
			cfg.HTTP.AllowNetworks = append(cfg.HTTP.AllowNetworks, "127.0.0.0/8")
			cfg.HTTP.AllowHTTPHosts = append(cfg.HTTP.AllowHTTPHosts, "127.0.0.1")
			src, err := e.registry(t, cfg).Get(tc.key) // a source never sees the instance name
			if err != nil {
				t.Fatal(err)
			}
			res, err := src.List(t.Context(), tgt, source.FetchState{})
			if tc.wantVersions == 0 {
				if err == nil || !strings.Contains(err.Error(), "404") {
					t.Fatalf("err = %v, want a 404", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Versions) != tc.wantVersions || res.Versions[0].Raw != "1.1.0" {
				t.Fatalf("versions = %+v", res.Versions)
			}
			out, _ := json.Marshal(res)
			noRegLeak(t, string(out)+e.logs.String())
		})
	}
}

func TestPrivatePyPIMirrorWithBasicAuth(t *testing.T) {
	const prefix = "/artifactory/api/pypi/pypi-remote"
	srv := httptest.NewServer(fakeIndexMirror("ci-bot", regCanary, prefix))
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "art-password", regCanary, hostOf(t, srv.URL))
	cfg := config.Default()
	cfg.Sources["pypi"] = config.Source{BaseURL: srv.URL + prefix, Auth: config.SourceAuth{Type: config.AuthBasic, Username: "ci-bot", PasswordRef: "art-password"}}

	res, err := e.list(t, cfg, source.Target{Kind: "pypi", Coordinates: map[string]string{"name": "demo"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(res.Versions))
	}
	out, _ := json.Marshal(res)
	noRegLeak(t, string(out)+e.logs.String())
}

// token_env also authenticates pypi and npm (bearer), not only github-release.
func TestTokenEnvAuthenticatesNPM(t *testing.T) {
	srv := httptest.NewServer(fakeVerdaccio(regCanary))
	defer srv.Close()
	t.Setenv("REG_TEST_TOKEN", regCanary)
	cfg, err := config.Load(t.Context(), config.Options{Environ: os.Environ()})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Sources["npm"] = config.Source{BaseURL: srv.URL, TokenEnv: "REG_TEST_TOKEN"} //nolint:gosec // G101: an environment variable name
	cfg.Tokens["npm"] = secrets.NewValueString(regCanary)
	e := newRegEnv(t)
	if _, err := e.list(t, cfg, npmTarget()); err != nil {
		t.Fatal(err)
	}
	noRegLeak(t, e.logs.String())
}

// A stored secret bound to another host is not sent, and no request is made.
func TestSourceSecretBoundToAnotherHostIsRefused(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "elsewhere", regCanary, "registry.example.com")
	cfg := config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: "elsewhere"}}
	_, err := e.list(t, cfg, npmTarget())
	if !errors.Is(err, secrets.ErrHostMismatch) {
		t.Fatalf("err = %v, want ErrHostMismatch", err)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests reached the registry", n)
	}
	noRegLeak(t, err.Error()+e.logs.String())
}

// A secret with no hosts is refused for a source (ADR 0024 §2), and so is a
// missing one.
func TestSourceSecretMustBeBound(t *testing.T) {
	srv := httptest.NewServer(fakeVerdaccio(regCanary))
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "unbound", regCanary)
	for _, ref := range []string{"unbound", "absent"} {
		cfg := config.Default()
		cfg.Sources["npm"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: ref}}
		_, err := e.list(t, cfg, npmTarget())
		if err == nil || !strings.Contains(err.Error(), ref) {
			t.Errorf("%s: err = %v", ref, err)
		}
		noRegLeak(t, fmt.Sprint(err))
	}
}

// http.ca_file: a registry behind a private CA is reachable through the
// process's client, over https with verification on; without the file it is
// not.
func TestCAFileReachesAPrivateCARegistry(t *testing.T) {
	srv := httptest.NewTLSServer(fakeVerdaccio(regCanary))
	defer srv.Close()
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caPath, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	e := newRegEnv(t)
	e.put(t, "tls-token", regCanary, hostOf(t, srv.URL))

	cfg := config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: "tls-token"}}
	if _, err := e.list(t, cfg, npmTarget()); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without ca_file: err = %v, want a certificate error", err)
	}
	cfg = config.Default()
	cfg.HTTP.CAFile = caPath
	cfg.Sources["npm"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: "tls-token"}}
	if _, err := e.list(t, cfg, npmTarget()); err != nil {
		t.Fatalf("with ca_file: %v", err)
	}
	noRegLeak(t, e.logs.String())
}

func TestNewNetClientFailsOnABadCAFile(t *testing.T) {
	cfg := config.Default()
	cfg.HTTP.CAFile = filepath.Join(t.TempDir(), "missing.pem")
	if _, err := newNetClient(cfg, nil, nil); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Fatalf("err = %v", err)
	}
}

func TestConfigCheckRejectsABadCAFile(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := writeCfg(t, "http:\n  ca_file: "+bad+"\n")
	if _, _, err := runCheck(t, "--config", p); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("err = %v", err)
	}
}

// A credentialed source on http:// needs its host in http.allow_http_hosts,
// as a webhook does (D12): the config is invalid without it, and a hop to
// another http host is refused at fetch.
func TestCredentialedHTTPSourceNeedsAllowHTTPHosts(t *testing.T) {
	yamlText := "sources:\n  npm:\n    base_url: http://verdaccio.internal:4873\n    auth: {token_ref: vt}\n"
	if _, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, yamlText)}); err == nil || !strings.Contains(err.Error(), "allow_http_hosts") {
		t.Fatalf("without the allow list: %v", err)
	}
	if _, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, yamlText+"http:\n  allow_http_hosts: [verdaccio.internal]\n")}); err != nil {
		t.Fatalf("with the allow list: %v", err)
	}
}

func TestSourceCredentialOverHTTPOutsideAllowListIsRefusedAtFetch(t *testing.T) {
	srv := httptest.NewServer(fakeVerdaccio(regCanary))
	defer srv.Close()
	e := newRegEnv(t)
	e.put(t, "vt", regCanary, hostOf(t, srv.URL))
	cfg := config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: srv.URL, Auth: config.SourceAuth{TokenRef: "vt"}}
	cfg.HTTP.AllowNetworks = []string{"127.0.0.0/8"}
	client, err := newNetClient(cfg, e.log, nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := builtinSources(cfg, client.HTTPClient(), e.log, nil, e.res)
	if err != nil {
		t.Fatal(err)
	}
	src, _ := reg.Get("npm")
	if _, err := src.List(t.Context(), npmTarget(), source.FetchState{}); err == nil || !strings.Contains(err.Error(), "allow_http_hosts") {
		t.Fatalf("err = %v", err)
	}
}

// The start-up check: a missing, unbound or wrongly bound secret is named,
// from metadata only.
func TestCheckSourceSecrets(t *testing.T) {
	cfg := config.Default()
	cfg.Sources["npm"] = config.Source{BaseURL: "https://nexus.example.com/repository/npm", Auth: config.SourceAuth{TokenRef: "good"}}
	cfg.Sources["pypi"] = config.Source{Auth: config.SourceAuth{Type: config.AuthBasic, Username: "u", PasswordRef: "wrong"}}
	cfg.Sources["github-release"] = config.Source{Auth: config.SourceAuth{TokenRef: "gone"}}
	err := checkSourceSecrets(cfg, map[string][]string{"good": {"nexus.example.com"}, "wrong": {"nexus.example.com"}})
	if err == nil {
		t.Fatal("want errors")
	}
	for _, want := range []string{"sources.pypi.auth.password_ref", "pypi.org", "sources.github-release.auth.token_ref", "gone"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "sources.npm") {
		t.Errorf("the good secret is reported: %v", err)
	}
	if err := checkSourceSecrets(config.Default(), nil); err != nil {
		t.Errorf("no references: %v", err)
	}
}

// openStore refuses to start when a source's secret is missing, and
// config check reads the database read-only for the same check.
func TestStartAndConfigCheckVerifySourceSecrets(t *testing.T) {
	dir := t.TempDir()
	dsn := "sqlite://" + filepath.ToSlash(filepath.Join(dir, "a.db"))
	cfgText := "database: {dsn: " + dsn + "}\nsources:\n  npm:\n    base_url: https://nexus.example.com/r\n    auth: {token_ref: nexus-npm}\n"
	p := writeCfg(t, cfgText)

	_, errb, err := runCheck(t, "--config", p)
	if err != nil || !strings.Contains(errb, "database does not exist yet") {
		t.Fatalf("no database: %v %q", err, errb)
	}
	cfg, err := config.Load(t.Context(), config.Options{Path: p})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(t.Context(), cfg, nil, slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), "nexus-npm") {
		t.Fatalf("openStore without the secret: %v", err)
	}
	if _, _, err := runCheck(t, "--config", p); err == nil || !strings.Contains(err.Error(), "nexus-npm") {
		t.Fatalf("config check without the secret: %v", err)
	}
	st, err := sqlite.Open(t.Context(), filepath.Join(dir, "a.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := secrets.NewKeyring(bytes.Repeat([]byte{7}, secrets.KeySize))
	e := &regEnv{st: st, keys: keys}
	e.put(t, "nexus-npm", regCanary, "nexus.example.com")
	_ = st.Close()
	st, err = openStore(t.Context(), cfg, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("openStore with the secret: %v", err)
	}
	_ = st.Close()
	// (config check as a whole would now ask for a secrets key.)
	if err := checkSourceSecretsOnDisk(t.Context(), cfg, io.Discard); err != nil {
		t.Fatalf("source check with the secret: %v", err)
	}
}

// auth on a kind no built-in source reads is warned about, not silently kept.
func TestConfigWarnsAboutCredentialOnUnknownKind(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, "sources:\n  nmp:\n    auth: {token_ref: t}\n")})
	if err != nil {
		t.Fatal(err)
	}
	if w := strings.Join(cfg.Warnings(), "\n"); !strings.Contains(w, "sources.nmp") {
		t.Errorf("warnings = %q", w)
	}
}
