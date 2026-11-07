package registryauth_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source/registryauth"
)

// canary is a synthetic credential: it must never reach a log or an error.
const canary = "CANARY-registry-credential-0123456789"

type fakeResolver struct {
	byName map[string]secrets.Bound
	calls  int
}

func (f *fakeResolver) Resolve(_ context.Context, name string, dest *url.URL) (secrets.Bound, error) {
	f.calls++
	b, ok := f.byName[name]
	if !ok {
		return secrets.Bound{}, fmt.Errorf("%w: secret %s does not exist", secrets.ErrNotFound, name)
	}
	if err := b.Allows(dest); err != nil {
		return secrets.Bound{}, err
	}
	return b, nil
}

func bound(name, value string, hosts ...string) secrets.Bound {
	return secrets.Bound{Name: name, Value: secrets.NewValueString(value), Hosts: hosts}
}

func host(t *testing.T, u string) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	h, err := secrets.Destination(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// allow is http.allow_http_hosts for the loopback test servers.
var allow = []string{"127.0.0.1"}

func noProxy(*http.Request) (*url.URL, error) { return nil, nil }

// get returns the response body, the body closed.
func get(t *testing.T, c *http.Client, u string) (string, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

// echoAuth answers with the Authorization header it received.
func echoAuth() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get("Authorization"))) //nolint:gosec // G705: a test server echoing a header
	})
}

func TestBearerAndNPMAuthTokenSendTheSameHeader(t *testing.T) {
	srv := httptest.NewServer(echoAuth())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	res := &fakeResolver{byName: map[string]secrets.Bound{"npm-token": bound("npm-token", canary, host(t, srv.URL))}}
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "npm-token", Resolver: res, BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	got, err := get(t, hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Bearer "+canary {
		t.Errorf("Authorization = %q", got)
	}
}

func TestBasic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		_, _ = fmt.Fprintf(w, "%v %s %s", ok, u, p) //nolint:gosec // G705: a test server echoing a header
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	res := &fakeResolver{byName: map[string]secrets.Bound{"pw": bound("pw", canary, host(t, srv.URL))}}
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{Username: "ci-bot", PasswordRef: "pw", Resolver: res, BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	got, err := get(t, hc, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if got != "true ci-bot "+canary {
		t.Errorf("basic auth = %q", got)
	}
}

func TestEnvTokenIsSentToTheBaseHostOnly(t *testing.T) {
	other := httptest.NewServer(echoAuth())
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/away" {
			http.Redirect(w, r, other.URL, http.StatusFound)
			return
		}
		echoAuth().ServeHTTP(w, r)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{Token: secrets.NewValueString(canary), BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	if got, err := get(t, hc, srv.URL); err != nil || got != "Bearer "+canary {
		t.Fatalf("base host: %q, %v", got, err)
	}
	got, err := get(t, hc, srv.URL+"/away")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("a redirect to another host carried Authorization %q", got)
	}
}

// A stored secret is re-checked at every redirect hop, and the request to a
// host outside its binding is not sent.
func TestBoundSecretRefusedOnRedirectToAnotherHost(t *testing.T) {
	var hit atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit.Store(true) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	res := &fakeResolver{byName: map[string]secrets.Bound{"tok": bound("tok", canary, host(t, srv.URL))}}
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	_, err := get(t, hc, srv.URL)
	if !errors.Is(err, secrets.ErrHostMismatch) {
		t.Fatalf("err = %v, want ErrHostMismatch", err)
	}
	if hit.Load() {
		t.Error("the redirect target received a request")
	}
	if strings.Contains(err.Error(), canary) {
		t.Error("the error leaks the secret")
	}
}

func TestUnboundSecretIsRefused(t *testing.T) {
	srv := httptest.NewServer(echoAuth())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	res := &fakeResolver{byName: map[string]secrets.Bound{"tok": bound("tok", canary)}}
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	_, err := get(t, hc, srv.URL)
	if !errors.Is(err, registryauth.ErrUnbound) {
		t.Fatalf("err = %v, want ErrUnbound", err)
	}
}

func TestMissingSecretAndMissingResolver(t *testing.T) {
	srv := httptest.NewServer(echoAuth())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "gone", Resolver: &fakeResolver{}, BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	if _, err := get(t, hc, srv.URL); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("missing secret: err = %v", err)
	}
	hc = registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "gone", BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	if _, err := get(t, hc, srv.URL); err == nil || !strings.Contains(err.Error(), "no secrets resolver") {
		t.Errorf("no resolver: err = %v", err)
	}
}

// The value is resolved on every request, not cached.
func TestSecretIsResolvedPerRequest(t *testing.T) {
	srv := httptest.NewServer(echoAuth())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	res := &fakeResolver{byName: map[string]secrets.Bound{"tok": bound("tok", canary, host(t, srv.URL))}}
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: noProxy, AllowHTTPHosts: allow})
	for range 3 {
		if _, err := get(t, hc, srv.URL); err != nil {
			t.Fatal(err)
		}
	}
	if res.calls != 3 {
		t.Errorf("resolved %d times, want 3", res.calls)
	}
}

// Through a proxy the credential goes only inside an https CONNECT tunnel.
func TestCredentialNeverGoesThroughAProxyAsPlainHTTP(t *testing.T) {
	var reached atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	proxy := func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy.invalid:3128") }
	res := &fakeResolver{byName: map[string]secrets.Bound{"tok": bound("tok", canary, host(t, srv.URL))}}
	for name, cfg := range map[string]registryauth.Config{
		"stored": {TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: proxy, AllowHTTPHosts: allow},
		"env":    {Token: secrets.NewValueString(canary), BaseURL: u, Proxy: proxy, AllowHTTPHosts: allow},
	} {
		hc := registryauth.Wrap(srv.Client(), cfg)
		_, err := get(t, hc, srv.URL)
		if !errors.Is(err, registryauth.ErrClearProxy) {
			t.Errorf("%s: err = %v, want ErrClearProxy", name, err)
		} else if strings.Contains(err.Error(), canary) {
			t.Errorf("%s: the error leaks the credential", name)
		}
	}
	if reached.Load() {
		t.Error("a request was sent")
	}
}

func TestNoCredentialReturnsTheClient(t *testing.T) {
	hc := &http.Client{}
	if registryauth.Wrap(hc, registryauth.Config{}) != hc {
		t.Error("Wrap without a credential must return the client unchanged")
	}
}

func TestCredentialNeverLogged(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	b := bound("tok", canary, "registry.example.com")
	log.Info("resolved", "secret", b, "value", b.Value, "cfg", registryauth.Config{Token: b.Value})
	if out := logs.String() + fmt.Sprintf("%v %+v %#v", b, registryauth.Config{Token: b.Value}, b); strings.Contains(out, canary) {
		t.Errorf("credential in output: %s", out)
	}
}

// recorder is a base RoundTripper that records the requests it receives, so
// the proxy and scheme rules are tested without a network.
type recorder struct{ got []http.Header }

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.got = append(r.got, req.Header.Clone())
	return &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}, Request: req}, nil
}

func recorded(t *testing.T, cfg registryauth.Config, rawURL string) (*recorder, error) {
	t.Helper()
	rec := &recorder{}
	if cfg.BaseURL == nil {
		cfg.BaseURL, _ = url.Parse(rawURL)
	}
	_, err := get(t, registryauth.Wrap(&http.Client{Transport: rec}, cfg), rawURL)
	return rec, err
}

func envCfg(proxy func(*http.Request) (*url.URL, error)) registryauth.Config {
	return registryauth.Config{Token: secrets.NewValueString(canary), Proxy: proxy}
}

func TestHTTPSThroughAProxyIsAllowed(t *testing.T) {
	proxy := func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy.internal:3128") }
	rec, err := recorded(t, envCfg(proxy), "https://registry.example.com/pkg")
	if err != nil || len(rec.got) != 1 || rec.got[0].Get("Authorization") != "Bearer "+canary {
		t.Fatalf("https through a proxy: %v %v", err, rec.got)
	}
}

// NO_PROXY: the proxy function answers nil for the host, so a registry on
// the allowed plain-http list is reached directly with its credential.
func TestDirectPlainHTTPToAnAllowedHost(t *testing.T) {
	cfg := envCfg(noProxy)
	cfg.AllowHTTPHosts = []string{"verdaccio.internal"}
	rec, err := recorded(t, cfg, "http://verdaccio.internal:4873/pkg")
	if err != nil || len(rec.got) != 1 || rec.got[0].Get("Authorization") == "" {
		t.Fatalf("direct http to an allowed host: %v %v", err, rec.got)
	}
}

func TestPlainHTTPOutsideTheAllowListIsRefused(t *testing.T) {
	cfg := envCfg(noProxy)
	cfg.AllowHTTPHosts = []string{"other.internal"}
	rec, err := recorded(t, cfg, "http://verdaccio.internal:4873/pkg")
	if !errors.Is(err, registryauth.ErrClearText) || len(rec.got) != 0 {
		t.Fatalf("err = %v, sent %d", err, len(rec.got))
	}
	// A stored secret is refused before it is read.
	res := &fakeResolver{byName: map[string]secrets.Bound{"tok": bound("tok", canary, "verdaccio.internal:4873")}}
	scfg := registryauth.Config{TokenRef: "tok", Resolver: res, Proxy: noProxy}
	if _, err := recorded(t, scfg, "http://verdaccio.internal:4873/pkg"); !errors.Is(err, registryauth.ErrClearText) || res.calls != 0 {
		t.Fatalf("stored secret: err = %v, resolved %d times", err, res.calls)
	}
}

func TestProxyErrorRefusesTheRequest(t *testing.T) {
	proxy := func(*http.Request) (*url.URL, error) { return nil, errors.New("bad proxy settings") }
	rec, err := recorded(t, envCfg(proxy), "https://registry.example.com/pkg")
	if !errors.Is(err, registryauth.ErrClearProxy) || len(rec.got) != 0 {
		t.Fatalf("err = %v, sent %d", err, len(rec.got))
	}
}

// An https registry that redirects to http:// must not receive the token.
func TestEnvTokenNotSentOnDowngrade(t *testing.T) {
	cfg := envCfg(noProxy)
	cfg.AllowHTTPHosts = []string{"registry.example.com"}
	cfg.BaseURL, _ = url.Parse("https://registry.example.com")
	rec, err := recorded(t, cfg, "http://registry.example.com:443/pkg")
	if err != nil || len(rec.got) != 1 || rec.got[0].Get("Authorization") != "" {
		t.Fatalf("downgrade hop: %v %v", err, rec.got)
	}
}

func TestCheckRefs(t *testing.T) {
	u, _ := url.Parse("https://nexus.example.com/repository/npm")
	refs := []registryauth.Ref{
		{Key: "sources.npm.auth.token_ref", Name: "ok", URL: u},
		{Key: "sources.npm.auth.password_ref", Name: "gone", URL: u},
		{Key: "sources.pypi.auth.token_ref", Name: "loose", URL: u},
		{Key: "sources.pypi.auth.password_ref", Name: "wrong", URL: u},
	}
	errs := registryauth.CheckRefs(refs, map[string][]string{
		"ok": {"nexus.example.com"}, "loose": {}, "wrong": {"other.example.com"},
	})
	if len(errs) != 3 {
		t.Fatalf("errors = %v, want 3", errs)
	}
	for i, want := range []string{"does not exist", "no hosts", "other.example.com"} {
		if !strings.Contains(errs[i].Error(), want) {
			t.Errorf("error %d = %v, want %q", i, errs[i], want)
		}
	}
}

// A credentialed request that netguard retries (a timeout, a 503)
// logs the attempt and its error at debug level. Neither the log nor the
// returned error may hold the credential or its basic-auth encoding: a
// url.Error carries the method and URL, never a header.
func TestRetryLogsAndErrorsNeverHoldTheCredential(t *testing.T) {
	var n atomic.Int32
	// Only timeouts and 5xx are retried, and only a retried error is logged:
	// attempts 1 and 4 hang past netguard's per-attempt timeout, 2 and 3 are 503.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) % 4 {
		case 2, 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	prefix := netip.MustParsePrefix("127.0.0.0/8")

	for name, cfg := range map[string]registryauth.Config{
		"basic":  {Username: "ci-bot", PasswordRef: "pw"},
		"bearer": {TokenRef: "pw"},
	} {
		t.Run(name, func(t *testing.T) {
			n.Store(0)
			var logs bytes.Buffer
			log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
			nc, err := netguard.New(netguard.Config{
				Allow: []netip.Prefix{prefix}, Logger: log, Timeout: 50 * time.Millisecond, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg.Resolver = &fakeResolver{byName: map[string]secrets.Bound{"pw": bound("pw", canary, host(t, srv.URL))}}
			cfg.BaseURL, cfg.Proxy, cfg.AllowHTTPHosts = u, noProxy, allow
			_, err = get(t, registryauth.Wrap(nc.HTTPClient(), cfg), srv.URL)
			if err == nil {
				t.Fatal("want the final attempt to fail")
			}
			if !strings.Contains(logs.String(), "retrying request") {
				t.Fatalf("no retry was logged:\n%s", logs.String())
			}
			basic := base64.StdEncoding.EncodeToString([]byte("ci-bot:" + canary))
			for _, bad := range []string{canary, "CANARY", basic} {
				if strings.Contains(logs.String(), bad) || strings.Contains(err.Error(), bad) {
					t.Errorf("%q leaks:\nlog: %s\nerr: %v", bad, logs.String(), err)
				}
			}
		})
	}
}
