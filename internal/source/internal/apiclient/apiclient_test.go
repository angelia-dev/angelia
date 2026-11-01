package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
)

func TestSecretRedactsEverywhere(t *testing.T) {
	const raw = "tok_SyntheticSecretValue" //nolint:gosec // G101: synthetic test value.
	tok := Secret(raw)
	var buf bytes.Buffer
	for _, h := range []slog.Handler{slog.NewJSONHandler(&buf, nil), slog.NewTextHandler(&buf, nil)} {
		slog.New(h).Info("x", "token", tok, slog.Any("t2", tok))
	}
	j, _ := json.Marshal(map[string]any{"t": tok})
	out := buf.String() + string(j) + fmt.Sprintf("%v %+v %#v %s", tok, tok, tok, tok)
	if strings.Contains(out, raw) {
		t.Errorf("secret leaked: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("no redaction marker in %s", out)
	}
}

func TestTokens(t *testing.T) {
	if tok, err := (Token{}).Get(); tok != "" || err != nil || (Token{}).Set() {
		t.Errorf("zero Token = %q, %v; want empty, nil, unset", tok, err)
	}
	if tok, err := FixedToken("  abc \n").Get(); tok != "abc" || err != nil || !FixedToken("x").Set() {
		t.Errorf("FixedToken = %q, %v; want trimmed value", tok, err)
	}
	if _, err := FixedToken("   ").Get(); err == nil {
		t.Error("blank FixedToken: want error")
	}
	t.Setenv("APICLIENT_TEST_TOKEN", " from-env ")
	if tok, err := EnvToken("APICLIENT_TEST_TOKEN").Get(); tok != "from-env" || err != nil {
		t.Errorf("EnvToken = %q, %v", tok, err)
	}
	t.Setenv("APICLIENT_TEST_TOKEN", "rotated")
	tk := EnvToken("APICLIENT_TEST_TOKEN")
	t.Setenv("APICLIENT_TEST_TOKEN", "rotated2")
	if tok, _ := tk.Get(); tok != "rotated2" {
		t.Errorf("EnvToken must read the variable on every Get, got %q", tok)
	}
	t.Setenv("APICLIENT_TEST_TOKEN", "")
	if _, err := tk.Get(); err == nil || !strings.Contains(err.Error(), `"APICLIENT_TEST_TOKEN"`) {
		t.Errorf("empty env: err = %v", err)
	}
}

func TestTokenSafe(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://example.com":   true,
		"http://example.com":    false,
		"http://127.0.0.1:8080": true,
		"http://[::1]:8080":     true,
		"http://localhost":      true,
		"http://LOCALHOST:1":    true,
		"http://10.0.0.1":       false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := TokenSafe(u); got != want {
			t.Errorf("TokenSafe(%s) = %v, want %v", raw, got, want)
		}
	}
}

// bearer reports credentials from the request about to be sent.
func bearer(req *http.Request, _ []*http.Request) bool { return req.Header.Get("Authorization") != "" }

// fetch sends a GET with an optional bearer token through hc.
func fetch(t *testing.T, hc *http.Client, u, tok string) (*netguard.Result, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return Fetch(context.Background(), hc, req, netguard.Validators{}, "thing not found")
}

func TestGuardRedirectsDoesNotMutateCallerClient(t *testing.T) {
	hc := &http.Client{}
	GuardRedirects("k", hc, bearer)
	if hc.CheckRedirect != nil {
		t.Error("GuardRedirects modified the caller's client")
	}
}

func TestFetchStatusHints(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			http.NotFound(w, r)
		case "/401":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	var se *netguard.StatusError

	_, err := fetch(t, srv.Client(), srv.URL+"/404", "")
	if !errors.As(err, &se) || se.StatusCode != 404 || !strings.HasPrefix(err.Error(), "thing not found (or not visible to the token): ") {
		t.Errorf("404: %v", err)
	}
	_, err = fetch(t, srv.Client(), srv.URL+"/401", "")
	if !errors.As(err, &se) || se.StatusCode != 401 || !strings.HasPrefix(err.Error(), "the token is invalid or expired: ") {
		t.Errorf("401: %v", err)
	}
	_, err = fetch(t, srv.Client(), srv.URL+"/500", "")
	if !errors.As(err, &se) || se.StatusCode != 500 || strings.Contains(err.Error(), "token") {
		t.Errorf("500: %v", err)
	}
}

func TestRedirectGuard(t *testing.T) {
	const secretTok = "tok-must-not-travel" //nolint:gosec // G101: synthetic test value.
	var otherHits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherHits++
		if r.Header.Get("Authorization") != "" {
			t.Error("token reached the other host")
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer other.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("/off", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	})
	mux.HandleFunc("/same", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ok", http.StatusFound)
	})
	mux.HandleFunc("/loop", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusFound)
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretTok {
			t.Errorf("same-host redirect lost the token: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte("{}"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	hc := GuardRedirects("my-kind", srv.Client(), bearer)
	_, err := fetch(t, hc, srv.URL+"/off", secretTok)
	if err == nil || !strings.Contains(err.Error(), "my-kind: refusing to follow a redirect to ") {
		t.Errorf("cross-host redirect with token: err = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), secretTok) {
		t.Errorf("error leaks the token: %v", err)
	}
	if otherHits != 0 {
		t.Errorf("other host was contacted %d times", otherHits)
	}
	if _, err := fetch(t, hc, srv.URL+"/same", secretTok); err != nil {
		t.Errorf("same-host redirect: %v", err)
	}
	// Without a token the redirect is followed anywhere.
	if _, err := fetch(t, hc, srv.URL+"/off", ""); err != nil || otherHits != 1 {
		t.Errorf("anonymous cross-host redirect: err %v, hits %d", err, otherHits)
	}
	// Redirect cap when the caller's client has no CheckRedirect.
	if _, err := fetch(t, hc, srv.URL+"/loop", ""); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("redirect loop: err = %v", err)
	}
}

// routed returns a client whose transport dials addr whatever host is asked.
func routed(base *http.Client, addr string) *http.Client {
	tr := base.Transport.(*http.Transport).Clone()
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr}
}

func TestRedirectGuardRefusesSubdomain(t *testing.T) {
	// https on both sides, so TokenSafe passes and only the host comparison
	// can refuse; a suffix match would let the redirect through.
	var subHits, subAuth int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Host, "sub.") {
			subHits++
			if r.Header.Get("Authorization") != "" {
				subAuth++
			}
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.Redirect(w, r, "https://sub."+r.Host+"/x", http.StatusFound) //nolint:gosec // G710: test server, fixed target.
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	hc := GuardRedirects("k", routed(srv.Client(), srv.Listener.Addr().String()), bearer)
	host := "example.com:" + port
	_, err := fetch(t, hc, "https://"+host+"/start", "tok")
	if err == nil || !strings.Contains(err.Error(), "refusing to follow a redirect to sub.example.com:") {
		t.Errorf("subdomain redirect with token: err = %v", err)
	}
	if subHits != 0 || subAuth != 0 {
		t.Errorf("subdomain was contacted (%d hits, %d with a token)", subHits, subAuth)
	}
}

func TestRedirectGuardRefusesHTTPSDowngrade(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Same host and port, plain scheme: only the scheme check can refuse it.
		http.Redirect(w, r, "http://"+r.Host+"/x", http.StatusFound) //nolint:gosec // G710: test server, fixed target.
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	hc := GuardRedirects("k", routed(srv.Client(), srv.Listener.Addr().String()), bearer)
	host := "example.com:" + port // the httptest certificate is valid for example.com
	_, err := fetch(t, hc, "https://"+host+"/start", "tok")
	if err == nil || !strings.Contains(err.Error(), "refusing to follow a redirect to "+host) {
		t.Errorf("https to http redirect with token: err = %v", err)
	}
}

func TestRedirectGuardChainsCallerCheckRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/a" {
			http.Redirect(w, r, "/b", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	errStop := errors.New("caller says stop")
	hc := srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errStop }
	if _, err := fetch(t, GuardRedirects("k", hc, bearer), srv.URL+"/a", ""); !errors.Is(err, errStop) {
		t.Errorf("caller CheckRedirect not honoured: %v", err)
	}
}

func TestRedirectGuardUsesFirstRequestCredentials(t *testing.T) {
	// A header that net/http keeps across hosts (PRIVATE-TOKEN style) is
	// judged on the first request of the chain, not on the redirected one.
	var otherHits int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherHits++
		_, _ = w.Write([]byte("{}"))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()
	first := func(_ *http.Request, via []*http.Request) bool { return via[0].Header.Get("PRIVATE-TOKEN") != "" }
	hc := GuardRedirects("k", srv.Client(), first)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	req.Header.Set("PRIVATE-TOKEN", "tok")
	if _, err := Fetch(context.Background(), hc, req, netguard.Validators{}, "x"); err == nil || !strings.Contains(err.Error(), "refusing to follow") {
		t.Errorf("err = %v", err)
	}
	if otherHits != 0 {
		t.Errorf("other host was contacted %d times", otherHits)
	}
}
