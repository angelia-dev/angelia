package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/metrics"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/webauth"
)

// uiYAML is an enabled ui section whose client secret comes from secret
// (a client_secret_env or client_secret_file line).
func uiYAML(secret string) string {
	return "ui:\n  enabled: true\n  public_url: https://angelia.example.com\n" +
		"  github:\n    client_id: Ov23liTESTCLIENT\n    " + secret + "\n" +
		"  access:\n    - github_user: alice\n      role: admin\n"
}

func writeSecret(t *testing.T, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(p, []byte("test-client-secret-0001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// config check reads the client secret file: unreadable or writable by
// others is an error, readable by others a warning; the secret is never
// printed.
func TestConfigCheckUIClientSecretFile(t *testing.T) {
	ok := writeSecret(t, 0o600)
	out, errb, err := runCheck(t, "--config", writeCfg(t, uiYAML("client_secret_file: "+ok)))
	if err != nil || !strings.Contains(out, "configuration OK") || strings.Contains(errb, "client_secret_file") {
		t.Fatalf("out=%q err=%q %v", out, errb, err)
	}
	readable := writeSecret(t, 0o644)
	out, errb, err = runCheck(t, "--config", writeCfg(t, uiYAML("client_secret_file: "+readable)))
	if err != nil || !strings.Contains(out, "configuration OK") || !strings.Contains(errb, "warning:") {
		t.Fatalf("out=%q err=%q %v", out, errb, err)
	}
	for _, p := range []string{writeSecret(t, 0o666), filepath.Join(t.TempDir(), "missing")} {
		_, errb, err := runCheck(t, "--config", writeCfg(t, uiYAML("client_secret_file: "+p)))
		if err == nil || !strings.Contains(errb, "client_secret_file") {
			t.Fatalf("%s: err=%q %v", p, errb, err)
		}
	}
	for _, s := range []string{out, errb} {
		if strings.Contains(s, "test-client-secret-0001") {
			t.Fatal("the secret is printed")
		}
	}
}

func loadUIConfig(t *testing.T, secret string, env ...string) *config.Config {
	t.Helper()
	cfg, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, uiYAML(secret)), Environ: env})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// With ui.enabled the api role authenticates session cookies, mounts the
// sign-in routes and runs both last-use flushes.
func TestAPIOptionsUI(t *testing.T) {
	sources, err := watchSources(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "a.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.DiscardHandler)

	cfg := loadUIConfig(t, "client_secret_env: AGL_TEST_UI_SECRET", "AGL_TEST_UI_SECRET=test-client-secret-0001")
	o, err := apiOptions(cfg, st, sources, nil, metrics.New(nil), 0, log)
	if err != nil {
		t.Fatal(err)
	}
	if o.Sessions == nil || o.UIAuth == nil || o.SessionCookie != webauth.SessionCookie || o.PublicOrigin != "https://angelia.example.com" {
		t.Fatalf("%+v", o)
	}
	w := httptest.NewRecorder()
	o.UIAuth.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/auth/login/github", nil))
	loc := w.Header().Get("Location")
	if w.Code != http.StatusFound || !strings.HasPrefix(loc, "https://github.com/login/oauth/authorize?") ||
		!strings.Contains(loc, "redirect_uri=https%3A%2F%2Fangelia.example.com%2Fui%2Fauth%2Fcallback%2Fgithub") {
		t.Fatal(w.Code, loc)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- o.AuthLoop(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("AuthLoop did not return")
	}
	// The api server builds.
	if _, err := newAPIServer(cfg, st, sources, nil, nil, 0, log); err != nil {
		t.Fatal(err)
	}

	// No secret: the api role does not start.
	cfg = loadUIConfig(t, "client_secret_env: AGL_TEST_UI_SECRET")
	if _, err := apiOptions(cfg, st, sources, nil, nil, 0, log); err == nil || !strings.Contains(err.Error(), "AGL_TEST_UI_SECRET") {
		t.Fatal(err)
	}
	if _, err := newAPIServer(cfg, st, sources, nil, nil, 0, log); err == nil {
		t.Fatal("api role started without a secret")
	}
	// A loopback http public_url uses the cookie without __Host-.
	cfg = loadUIConfig(t, "client_secret_file: "+writeSecret(t, 0o600))
	cfg.UI.PublicURL = "http://127.0.0.1:8080"
	if o, err := apiOptions(cfg, st, sources, nil, nil, 0, log); err != nil || o.SessionCookie != webauth.SessionCookieLocal {
		t.Fatal(o.SessionCookie, err)
	}
}
