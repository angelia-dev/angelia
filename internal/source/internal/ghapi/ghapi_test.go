package ghapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/internal/apiclient"
)

func TestParseRepo(t *testing.T) {
	ok := source.Target{Kind: "k", Coordinates: map[string]string{"repo": "o-1/n.a_me"}}
	if o, n, err := ParseRepo("k", ok); err != nil || o != "o-1" || n != "n.a_me" {
		t.Errorf("ParseRepo = %q, %q, %v", o, n, err)
	}
	if _, _, err := ParseRepo("other", ok); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("wrong kind: err = %v", err)
	}
	for _, repo := range []string{"", "x", "a/", "/b", "-a/b", "a/b/c", "a/..", "a/.", "a/b c", strings.Repeat("a", 40) + "/b", "a/" + strings.Repeat("b", 101)} {
		tg := source.Target{Kind: "k", Coordinates: map[string]string{"repo": repo}}
		if _, _, err := ParseRepo("k", tg); !errors.Is(err, source.ErrInvalidTarget) {
			t.Errorf("repo %q: err = %v, want ErrInvalidTarget", repo, err)
		}
	}
}

func TestNewConfig(t *testing.T) {
	c := New("k", "", http.DefaultClient, apiclient.Token{})
	if c.Base() != DefaultBaseURL || c.Err() != nil {
		t.Errorf("default: base %q err %v", c.Base(), c.Err())
	}
	if c := New("k", "https://h/api/v3///", http.DefaultClient, apiclient.Token{}); c.Base() != "https://h/api/v3" {
		t.Errorf("trailing slashes kept: %q", c.Base())
	}
	for _, bad := range []string{"::bad", "ftp://h", "https://", "https://u:p@h", "/rel"} {
		if c := New("k", bad, http.DefaultClient, apiclient.Token{}); c.Err() == nil || !strings.Contains(c.Err().Error(), "invalid base URL") {
			t.Errorf("base %q: CfgErr = %v", bad, c.Err())
		}
	}
	if c := New("k", "http://example.com", http.DefaultClient, apiclient.FixedToken("x")); !errors.Is(c.Err(), apiclient.ErrBasePlain) {
		t.Errorf("token over plain http: CfgErr = %v", c.Err())
	}
	if c := New("k", "http://example.com", http.DefaultClient, apiclient.Token{}); c.Err() != nil {
		t.Errorf("plain http without a token is allowed, got %v", c.Err())
	}
}

func TestNewDoesNotMutateCallerClient(t *testing.T) {
	hc := &http.Client{}
	New("k", "", hc, apiclient.Token{})
	if hc.CheckRedirect != nil {
		t.Error("New modified the caller's client")
	}
}

func get(t *testing.T, c *Client, u string, tok apiclient.Secret) (*netguard.Result, error) {
	t.Helper()
	return c.Get(context.Background(), u, netguard.Validators{}, tok, "thing not found")
}

func TestGetSendsHeadersAndToken(t *testing.T) {
	var h http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h = r.Header.Clone()
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := New("k", srv.URL, srv.Client(), apiclient.FixedToken("tok"))
	if _, err := get(t, c, srv.URL+"/x", "tok"); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "Bearer tok" || h.Get("Accept") != "application/vnd.github+json" || h.Get("X-GitHub-Api-Version") != "2022-11-28" {
		t.Errorf("headers = %v", h)
	}
	if _, err := get(t, c, srv.URL+"/x", ""); err != nil {
		t.Fatal(err)
	}
	if h.Get("Authorization") != "" {
		t.Errorf("anonymous request carried Authorization: %v", h)
	}
}

func TestGetRefusesTokenOverPlainHTTP(t *testing.T) {
	c := New("k", "https://example.com", http.DefaultClient, apiclient.Token{})
	if _, err := get(t, c, "http://example.com/x", "tok"); !errors.Is(err, apiclient.ErrBasePlain) {
		t.Errorf("err = %v, want apiclient.ErrBasePlain", err)
	}
}

func TestGetBadURL(t *testing.T) {
	c := New("k", "", http.DefaultClient, apiclient.Token{})
	if _, err := get(t, c, "http://bad\x7f/", ""); err == nil || err.Error() != "build request: invalid base URL" {
		t.Errorf("err = %v", err)
	}
}

func TestGetStatusHints(t *testing.T) {
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
	c := New("k", srv.URL, srv.Client(), apiclient.Token{})
	var se *netguard.StatusError

	_, err := get(t, c, srv.URL+"/404", "")
	if !errors.As(err, &se) || se.StatusCode != 404 || !strings.HasPrefix(err.Error(), "thing not found (or not visible to the token): ") {
		t.Errorf("404: %v", err)
	}
	_, err = get(t, c, srv.URL+"/401", "")
	if !errors.As(err, &se) || se.StatusCode != 401 || !strings.HasPrefix(err.Error(), "the token is invalid or expired: ") {
		t.Errorf("401: %v", err)
	}
	_, err = get(t, c, srv.URL+"/500", "")
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

	c := New("my-kind", srv.URL, srv.Client(), apiclient.FixedToken(secretTok))
	_, err := get(t, c, srv.URL+"/off", secretTok)
	if err == nil || !strings.Contains(err.Error(), "my-kind: refusing to follow a redirect to ") {
		t.Errorf("cross-host redirect with token: err = %v", err)
	}
	if strings.Contains(fmt.Sprint(err), secretTok) {
		t.Errorf("error leaks the token: %v", err)
	}
	if otherHits != 0 {
		t.Errorf("other host was contacted %d times", otherHits)
	}
	if _, err := get(t, c, srv.URL+"/same", secretTok); err != nil {
		t.Errorf("same-host redirect: %v", err)
	}
	// Without a token the redirect is followed anywhere.
	if _, err := get(t, c, srv.URL+"/off", ""); err != nil || otherHits != 1 {
		t.Errorf("anonymous cross-host redirect: err %v, hits %d", err, otherHits)
	}
	// Redirect cap when the caller's client has no CheckRedirect.
	if _, err := get(t, c, srv.URL+"/loop", ""); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
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
	hc := routed(srv.Client(), srv.Listener.Addr().String())
	host := "example.com:" + port
	c := New("k", "https://"+host, hc, apiclient.FixedToken("tok"))
	_, err := get(t, c, "https://"+host+"/start", "tok")
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
	hc := routed(srv.Client(), srv.Listener.Addr().String())
	host := "example.com:" + port // the httptest certificate is valid for example.com
	c := New("k", "https://"+host, hc, apiclient.FixedToken("tok"))
	_, err := get(t, c, "https://"+host+"/start", "tok")
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
	c := New("k", srv.URL, hc, apiclient.Token{})
	if _, err := get(t, c, srv.URL+"/a", ""); !errors.Is(err, errStop) {
		t.Errorf("caller CheckRedirect not honoured: %v", err)
	}
}
