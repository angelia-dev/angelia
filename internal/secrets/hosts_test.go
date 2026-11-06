package secrets

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestValidHostAndName(t *testing.T) {
	for s, want := range map[string]bool{
		"ci.example.com": true, "ci.example.com:8443": true, "xn--bcher-kva.example": true, "10.0.0.1": true,
		"10.0.0.1:8080": true, "::1": true, "[::1]:8443": true, "localhost": true,
		"": false, "CI.example.com": false, "ci.example.com.": false, "*.example.com": false, "ci.example.com:0": false,
		"ci.example.com:080": false, "ci.example.com:65536": false, "https://ci.example.com": false, "bücher.example": false,
		"[::1]": false, "0:0::1": false, "fe80::1%eth0": false, "a..b": false, "-a.b": false,
	} {
		if got := ValidHost(s); got != want {
			t.Errorf("ValidHost(%q) = %v", s, got)
		}
	}
	for s, want := range map[string]bool{
		"ci-webhook": true, "a": true, "a.b_c-d": true, "": false, "-a": false, "A": false, "a/b": false,
		strings.Repeat("a", 63): true, strings.Repeat("a", 64): false,
	} {
		if got := ValidName(s); got != want {
			t.Errorf("ValidName(%q) = %v", s, got)
		}
	}
}

func TestDestination(t *testing.T) {
	for raw, want := range map[string]string{
		"https://CI.Example.com/x": "ci.example.com:443", "http://ci.example.com": "ci.example.com:80",
		"https://ci.example.com:8443/": "ci.example.com:8443", "https://[::1]/": "[::1]:443", "HTTPS://h": "h:443",
	} {
		u, _ := url.Parse(raw)
		if got, err := Destination(u); err != nil || got != want {
			t.Errorf("Destination(%s) = %q %v", raw, got, err)
		}
	}
	for _, raw := range []string{"ftp://h/", "https:///x", "https://bücher.example/"} {
		u, _ := url.Parse(raw)
		if _, err := Destination(u); err == nil {
			t.Errorf("Destination(%s) accepted", raw)
		}
	}
	if _, err := Destination(nil); err == nil {
		t.Error("nil URL accepted")
	}
}

func TestBoundAllows(t *testing.T) {
	b := Bound{Name: "registry", Value: NewValueString(canary), Hosts: []string{"registry.example.com", "mirror.example.com:8443"}}
	for raw, ok := range map[string]bool{
		"https://registry.example.com/simple/": true, "https://registry.example.com:443/": true,
		"https://REGISTRY.example.com/": true, "https://mirror.example.com:8443/x": true,
		"https://mirror.example.com/x": false, "http://registry.example.com/": false, "https://registry.example.com:8443/": false,
		"https://evil.example.com/": false, "https://registry.example.com.evil.example/": false,
	} {
		u, _ := url.Parse(raw)
		err := b.Allows(u)
		if (err == nil) != ok {
			t.Errorf("Allows(%s) = %v", raw, err)
		}
		if err != nil {
			if !errors.Is(err, ErrHostMismatch) || !strings.Contains(err.Error(), "registry") {
				t.Errorf("error %v", err)
			}
			noLeak(t, "Allows", err.Error())
		}
	}
	if err := (Bound{Name: "x"}).Allows(nil); err != nil {
		t.Fatalf("unbound: %v", err)
	}
	if err := b.Allows(nil); !errors.Is(err, ErrHostMismatch) {
		t.Fatalf("bound with no destination: %v", err)
	}
}

// A bound secret is refused on a redirect hop to another host (ADR 0024 §7).
func TestBoundCheckRedirect(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("the redirect to another host was followed, Authorization %q", r.Header.Get("Authorization"))
	}))
	defer other.Close()
	var sameHits int
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			sameHits++
			_, _ = w.Write([]byte("ok"))
		case "/to-same":
			http.Redirect(w, r, "/same", http.StatusFound)
		default:
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		}
	}))
	defer first.Close()
	fu, _ := url.Parse(first.URL)
	b := Bound{Name: "registry", Value: NewValueString(canary), Hosts: []string{fu.Host}}
	c := &http.Client{CheckRedirect: b.CheckRedirect(nil)}

	resp, err := c.Get(first.URL + "/to-same")
	if err != nil {
		t.Fatalf("redirect within the host: %v", err)
	}
	_ = resp.Body.Close()
	if sameHits != 1 {
		t.Fatalf("same-host hop not followed")
	}
	resp, err = c.Get(first.URL + "/away")
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("redirect to another host followed")
	}
	if !errors.Is(err, ErrHostMismatch) {
		t.Fatalf("error %v", err)
	}
	noLeak(t, "redirect error", err.Error())

	calls := 0
	c = &http.Client{CheckRedirect: b.CheckRedirect(func(*http.Request, []*http.Request) error { calls++; return nil })}
	resp, err = c.Get(first.URL + "/to-same")
	if err != nil || calls != 1 {
		t.Fatalf("next not called: %v %d", err, calls)
	}
	_ = resp.Body.Close()
	stop := b.CheckRedirect(nil)
	req := httptest.NewRequest("GET", first.URL+"/same", nil)
	if err := stop(req, make([]*http.Request, 10)); err == nil {
		t.Fatal("more than 10 redirects allowed")
	}
}
