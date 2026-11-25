package generic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"tool", "my-tool_2", "ünïcode", strings.Repeat("a", MaxNameBytes), "a@b/c"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("a", MaxNameBytes+1), "a b", "a\tb", "a\x00b", "a b", "\xff", "a​b"} {
		if err := ValidateName(bad); !errors.Is(err, source.ErrInvalidTarget) {
			t.Errorf("ValidateName(%q) = %v, want ErrInvalidTarget", bad, err)
		}
	}
}

func TestOnlyKeys(t *testing.T) {
	tg := source.Target{Coordinates: map[string]string{"name": "x", "url": "y"}}
	if err := OnlyKeys(tg, "name", "url"); err != nil {
		t.Error(err)
	}
	if err := OnlyKeys(tg, "name"); !errors.Is(err, source.ErrInvalidTarget) || !strings.Contains(err.Error(), `"url"`) {
		t.Errorf("OnlyKeys = %v", err)
	}
}

// Each D52 URL rule fails without its check.
func TestValidateURL(t *testing.T) {
	allow := []string{"intranet.example", "127.0.0.1"}
	for _, ok := range []string{
		"https://example.org/x.json",
		"HTTPS://Example.ORG:443",
		"https://example.org/a?b=c&d=%2F",
		"http://intranet.example/p",
		"http://INTRANET.example:8080/p", // allow list is case-insensitive
		"http://127.0.0.1:9/x",
		"https://[2001:db8::1]/x",
		"https://example.org/" + strings.Repeat("a", MaxURLBytes-len("https://example.org/")),
	} {
		if _, err := ValidateURL(ok, allow); err != nil {
			t.Errorf("ValidateURL(%q) = %v", ok, err)
		}
	}
	cases := map[string]string{
		"": "needs a url",
		"https://example.org/" + strings.Repeat("a", MaxURLBytes): "longer than",
		"http://example.org/":          "must be https",
		"ftp://example.org/":           "scheme must be https",
		"file:///etc/passwd":           "absolute",
		"https:example.org":            "absolute",
		"/relative/path":               "absolute",
		"https://user:pw@example.org/": "user information",
		"https://token@example.org/":   "user information",
		"https://example.org/#frag":    "fragment",
		"https://example.org/#":        "fragment",
		"https://2130706433/":          "non-canonical",
		"https://0x7f000001/":          "non-canonical",
		"http://127.1/":                "non-canonical",
		"https://example.org/a b":      "white space",
		"https://example.org/\n":       "white space",
		"https://example.org/%zz":      "not a URL",
		"https://exa mple.org/":        "white space",
		"https:///nohost":              "absolute",
		"https://example.org/\xff":     "UTF-8",
	}
	for raw, want := range cases {
		_, err := ValidateURL(raw, allow)
		if !errors.Is(err, source.ErrInvalidTarget) || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateURL(%q) = %v, want ErrInvalidTarget containing %q", raw, err, want)
		}
	}
}

func TestCanonicalURL(t *testing.T) {
	cases := map[string]string{
		"HTTPS://Example.ORG:443":                "https://example.org/",
		"https://example.org/":                   "https://example.org/",
		"https://example.org":                    "https://example.org/",
		"https://Example.org:8443/A/b?Q=1&a=%2F": "https://example.org:8443/A/b?Q=1&a=%2F",
		"http://127.0.0.1:80/x":                  "http://127.0.0.1/x",
		"http://127.0.0.1:8080/x?":               "http://127.0.0.1:8080/x?",
		"https://[2001:DB8::1]:443/x":            "https://[2001:db8::1]/x",
		"https://[2001:db8::1]:8443/x":           "https://[2001:db8::1]:8443/x",
		"https://example.org/a%2Fb":              "https://example.org/a%2Fb",
	}
	for in, want := range cases {
		u, err := ValidateURL(in, []string{"127.0.0.1"})
		if err != nil {
			t.Fatalf("ValidateURL(%q): %v", in, err)
		}
		got := CanonicalURL(u)
		if got != want {
			t.Errorf("CanonicalURL(%q) = %q, want %q", in, got, want)
		}
		u2, err := ValidateURL(got, []string{"127.0.0.1"})
		if err != nil {
			t.Fatalf("canonical %q does not validate: %v", got, err)
		}
		if again := CanonicalURL(u2); again != got {
			t.Errorf("not idempotent: %q then %q", got, again)
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestFetchConditionalAndAccept(t *testing.T) {
	var accept atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept.Store(r.Header.Get("Accept"))
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte("body"))
	}))
	defer srv.Close()
	f := NewFetcher(srv.Client(), nil)
	res, err := f.Fetch(context.Background(), mustURL(t, srv.URL), "text/plain", source.FetchState{})
	if err != nil || string(res.Body) != "body" || res.Validators.ETag != `"v1"` {
		t.Fatalf("first fetch = %+v, %v", res, err)
	}
	if accept.Load() != "text/plain" {
		t.Errorf("Accept = %v", accept.Load())
	}
	res, err = f.Fetch(context.Background(), mustURL(t, srv.URL), "", source.FetchState{ETag: `"v1"`})
	if err != nil || !res.NotModified {
		t.Fatalf("second fetch = %+v, %v", res, err)
	}
	if r := NotModified(res.Validators, source.FetchState{ETag: `"v1"`, Cursor: "c"}); !r.NotModified || r.State.Cursor != "c" || r.State.ETag != `"v1"` {
		t.Errorf("NotModified = %+v", r)
	}
}

func TestFetchStatusAndNoClient(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := NewFetcher(srv.Client(), nil).Fetch(context.Background(), mustURL(t, srv.URL), "", source.FetchState{})
	var se *netguard.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Errorf("404: %v", err)
	}
	if _, err := NewFetcher(nil, nil).Fetch(context.Background(), mustURL(t, srv.URL), "", source.FetchState{}); err == nil {
		t.Error("nil client accepted")
	}
}

func TestFetchBodyCap(t *testing.T) {
	for _, n := range []int{MaxBodyBytes, MaxBodyBytes + 1} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", n)))
		}))
		res, err := NewFetcher(srv.Client(), nil).Fetch(context.Background(), mustURL(t, srv.URL), "", source.FetchState{})
		srv.Close()
		if n == MaxBodyBytes {
			if err != nil || len(res.Body) != n {
				t.Errorf("%d bytes: %v", n, err)
			}
			continue
		}
		if !errors.Is(err, netguard.ErrBodyTooLarge) {
			t.Errorf("%d bytes: err = %v, want ErrBodyTooLarge", n, err)
		}
	}
}

// redirector redirects /r/<n> to /r/<n-1> and serves /r/0.
func redirector(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n int
		if _, err := fmt.Sscanf(r.URL.Path, "/r/%d", &n); err != nil {
			http.NotFound(w, r)
			return
		}
		if n == 0 {
			_, _ = w.Write([]byte("end"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/r/%d", n-1), http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchRedirectHops(t *testing.T) {
	srv := redirector(t)
	f := NewFetcher(srv.Client(), []string{"127.0.0.1"})
	if res, err := f.Fetch(context.Background(), mustURL(t, srv.URL+"/r/5"), "", source.FetchState{}); err != nil || string(res.Body) != "end" {
		t.Errorf("5 hops: %v", err)
	}
	if _, err := f.Fetch(context.Background(), mustURL(t, srv.URL+"/r/6"), "", source.FetchState{}); err == nil || !strings.Contains(err.Error(), "stopped after 5 redirects") {
		t.Errorf("6 hops: %v", err)
	}
}

func TestFetchRedirectCallsOriginal(t *testing.T) {
	srv := redirector(t)
	hc := srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("original refuses") }
	if _, err := NewFetcher(hc, []string{"127.0.0.1"}).Fetch(context.Background(), mustURL(t, srv.URL+"/r/1"), "", source.FetchState{}); err == nil || !strings.Contains(err.Error(), "original refuses") {
		t.Errorf("err = %v", err)
	}
	if hc.Transport == nil || hc.CheckRedirect == nil {
		t.Error("the caller's client was modified")
	}
}

func TestFetchRedirectDowngrade(t *testing.T) {
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("plain")) }))
	defer plain.Close()
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		target := plain.URL + "/x"
		if r.URL.Path == "/userinfo" {
			target = strings.Replace(plain.URL, "http://", "http://u:p@", 1)
		}
		http.Redirect(w, r, target, http.StatusFound)
	}))
	defer tls.Close()
	ctx := context.Background()
	if _, err := NewFetcher(tls.Client(), nil).Fetch(ctx, mustURL(t, tls.URL), "", source.FetchState{}); err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Errorf("downgrade: %v", err)
	}
	res, err := NewFetcher(tls.Client(), []string{"127.0.0.1"}).Fetch(ctx, mustURL(t, tls.URL), "", source.FetchState{})
	if err != nil || string(res.Body) != "plain" {
		t.Errorf("allowed downgrade: %v", err)
	}
	if _, err := NewFetcher(tls.Client(), []string{"127.0.0.1"}).Fetch(ctx, mustURL(t, tls.URL+"/userinfo"), "", source.FetchState{}); err == nil || !strings.Contains(err.Error(), "user information") {
		t.Errorf("userinfo hop: %v", err)
	}
}

func TestVersions(t *testing.T) {
	vs, skipped, err := Versions([]string{"2.0", "", "1.0", "2.0", strings.Repeat("9", MaxVersionBytes+1), "\xff", "a\nb", "a\x7fb", "1.5", strings.Repeat("8", MaxVersionBytes)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range vs {
		got = append(got, v.Raw)
	}
	if want := "2.0,1.0,1.5," + strings.Repeat("8", MaxVersionBytes); strings.Join(got, ",") != want || skipped != 5 {
		t.Errorf("Versions = %v (skipped %d)", got, skipped)
	}
	if _, _, err := Versions(nil); !errors.Is(err, ErrNoVersions) {
		t.Errorf("empty: %v", err)
	}
	if _, n, err := Versions([]string{"", "\x01"}); !errors.Is(err, ErrNoVersions) || n != 2 || !strings.Contains(err.Error(), "2 value(s) skipped") {
		t.Errorf("all skipped: %v", err)
	}
	many := make([]string, 0, MaxVersions+1)
	for i := range MaxVersions {
		many = append(many, fmt.Sprint(i), fmt.Sprint(i)) // duplicates do not count
	}
	if vs, _, err := Versions(many); err != nil || len(vs) != MaxVersions {
		t.Errorf("%d distinct: %d, %v", MaxVersions, len(vs), err)
	}
	if _, _, err := Versions(append(many, "x")); !errors.Is(err, ErrTooManyVersions) {
		t.Errorf("%d distinct: %v", MaxVersions+1, err)
	}
}

func TestCursorAndResult(t *testing.T) {
	a := []source.Version{{Raw: "1"}, {Raw: "2"}}
	b := []source.Version{{Raw: "1"}, {Raw: "3"}}
	if Cursor(a) == Cursor(b) || !strings.HasPrefix(Cursor(a), "sha256:") || len(Cursor(a)) != len("sha256:")+64 {
		t.Errorf("Cursor: %s %s", Cursor(a), Cursor(b))
	}
	if Cursor(a) == Cursor([]source.Version{{Raw: "2"}, {Raw: "1"}}) {
		t.Error("order is not part of the cursor")
	}
	first := Result(a, netguard.Validators{}, source.FetchState{})
	if first.NotModified || len(first.Versions) != 2 || first.State.Cursor != Cursor(a) {
		t.Fatalf("first = %+v", first)
	}
	same := Result(a, netguard.Validators{ETag: "e"}, first.State)
	if !same.NotModified || len(same.Versions) != 0 || same.State.ETag != "e" || same.State.Cursor != first.State.Cursor {
		t.Errorf("same = %+v", same)
	}
	if changed := Result(b, netguard.Validators{}, first.State); changed.NotModified || len(changed.Versions) != 2 {
		t.Errorf("changed = %+v", changed)
	}
}
