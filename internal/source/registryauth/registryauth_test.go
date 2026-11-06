package registryauth_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

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
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "npm-token", Resolver: res, BaseURL: u, Proxy: noProxy})
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
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{Username: "ci-bot", PasswordRef: "pw", Resolver: res, BaseURL: u, Proxy: noProxy})
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
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{Token: secrets.NewValueString(canary), BaseURL: u, Proxy: noProxy})
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
	var hit bool
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	res := &fakeResolver{byName: map[string]secrets.Bound{"tok": bound("tok", canary, host(t, srv.URL))}}
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: noProxy})
	_, err := get(t, hc, srv.URL)
	if !errors.Is(err, secrets.ErrHostMismatch) {
		t.Fatalf("err = %v, want ErrHostMismatch", err)
	}
	if hit {
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
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: noProxy})
	_, err := get(t, hc, srv.URL)
	if !errors.Is(err, registryauth.ErrUnbound) {
		t.Fatalf("err = %v, want ErrUnbound", err)
	}
}

func TestMissingSecretAndMissingResolver(t *testing.T) {
	srv := httptest.NewServer(echoAuth())
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "gone", Resolver: &fakeResolver{}, BaseURL: u, Proxy: noProxy})
	if _, err := get(t, hc, srv.URL); !errors.Is(err, secrets.ErrNotFound) {
		t.Errorf("missing secret: err = %v", err)
	}
	hc = registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "gone", BaseURL: u, Proxy: noProxy})
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
	hc := registryauth.Wrap(srv.Client(), registryauth.Config{TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: noProxy})
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
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	proxy := func(*http.Request) (*url.URL, error) { return url.Parse("http://proxy.invalid:3128") }
	res := &fakeResolver{byName: map[string]secrets.Bound{"tok": bound("tok", canary, host(t, srv.URL))}}
	for name, cfg := range map[string]registryauth.Config{
		"stored": {TokenRef: "tok", Resolver: res, BaseURL: u, Proxy: proxy},
		"env":    {Token: secrets.NewValueString(canary), BaseURL: u, Proxy: proxy},
	} {
		hc := registryauth.Wrap(srv.Client(), cfg)
		_, err := get(t, hc, srv.URL)
		if !errors.Is(err, registryauth.ErrClearProxy) {
			t.Errorf("%s: err = %v, want ErrClearProxy", name, err)
		} else if strings.Contains(err.Error(), canary) {
			t.Errorf("%s: the error leaks the credential", name)
		}
	}
	if reached {
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
