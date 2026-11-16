package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testHandler() http.Handler {
	return handler(fstest.MapFS{
		"index.html":       {Data: []byte("<html>app</html>")},
		"assets/app-1.js":  {Data: []byte("console.log(1)")},
		"assets/app-1.css": {Data: []byte("body{}")},
		"favicon.svg":      {Data: []byte("<svg/>")},
	})
}

func get(h http.Handler, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestIndexAndFallback(t *testing.T) {
	h := testHandler()
	for _, p := range []string{"/ui/", "/ui", "/ui/watches", "/ui/watches/abc/deliveries", "/ui/assets"} {
		rec := get(h, "GET", p)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "app") {
			t.Errorf("%s: %d %q", p, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q", p, cc)
		}
	}
}

func TestAssetsImmutable(t *testing.T) {
	h := testHandler()
	rec := get(h, "GET", "/ui/assets/app-1.js")
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Errorf("js: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("content type %q", ct)
	}
	if rec := get(h, "GET", "/ui/favicon.svg"); rec.Code != 200 || rec.Header().Get("Cache-Control") != "" {
		t.Errorf("favicon: %d %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

func TestMissingFileWithExtensionIs404(t *testing.T) {
	if rec := get(testHandler(), "GET", "/ui/assets/missing.js"); rec.Code != 404 {
		t.Errorf("status %d", rec.Code)
	}
}

func TestAuthPathsAreNotServed(t *testing.T) {
	for _, p := range []string{"/ui/auth/login/github", "/ui/auth/", "/ui/auth", "/ui/x/../auth/callback"} {
		if rec := get(testHandler(), "GET", p); rec.Code != 404 {
			t.Errorf("%s: status %d", p, rec.Code)
		}
	}
}

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	h := testHandler()
	for _, tc := range []struct{ method, p string }{
		{"GET", "/ui/"},
		{"GET", "/ui/assets/app-1.js"},
		{"GET", "/ui/assets/nope.js"},
		{"GET", "/ui/auth/login/github"},
		{"POST", "/ui/"},
		{"HEAD", "/ui/"},
	} {
		rec := get(h, tc.method, tc.p)
		for k, want := range map[string]string{
			"Content-Security-Policy": CSP,
			"X-Content-Type-Options":  "nosniff",
			"Referrer-Policy":         "same-origin",
			"X-Frame-Options":         "DENY",
		} {
			if got := rec.Header().Get(k); got != want {
				t.Errorf("%s %s: %s = %q", tc.method, tc.p, k, got)
			}
		}
	}
	if !strings.Contains(CSP, "frame-ancestors 'none'") || !strings.Contains(CSP, "https://avatars.githubusercontent.com") {
		t.Errorf("CSP %q", CSP)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	if rec := get(testHandler(), "POST", "/ui/"); rec.Code != 405 {
		t.Errorf("status %d", rec.Code)
	}
}

// The committed placeholder keeps plain `go build` working without Node.
func TestEmbeddedHandlerServesSomething(t *testing.T) {
	rec := get(Handler(), "GET", "/ui/some/route")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if !Built() && !strings.Contains(rec.Body.String(), "make web") {
		t.Errorf("placeholder text: %q", rec.Body.String())
	}
}

// Architecture §6: the binary stays under 40 MB. The embedded UI must stay a
// small part of it; this bounds the whole embedded tree.
func TestEmbeddedSize(t *testing.T) {
	total, err := embeddedSize()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("embedded UI: %d bytes (built: %v)", total, Built())
	if total > 4<<20 {
		t.Errorf("embedded UI is %d bytes, over the 4 MiB budget", total)
	}
}
