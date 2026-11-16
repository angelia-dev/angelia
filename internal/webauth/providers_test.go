package webauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/secrets"
)

// GET /ui/auth/providers lists name and label of each provider, in order,
// unauthenticated, and nothing of their configuration.
func TestHandlerProviders(t *testing.T) {
	is := newTestIssuer(t, "RS256")
	s, _, _, _ := newTestSessions(t, oidcUI())
	gh, err := NewGitHub(GitHubConfig{
		ClientID: "gh-client-id-123", ClientSecret: secrets.NewValueString("gh-client-secret-456"),
		RedirectURL: "https://angelia.example.com/ui/auth/callback/github", HTTP: http.DefaultClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	labelled, err := NewOIDC(OIDCConfig{
		Name: "corp", Label: "Company SSO", Issuer: is.srv.URL, ClientID: "corp-client-id", ClientSecret: secrets.NewValueString("corp-secret"),
		RedirectURL: "https://angelia.example.com/ui/auth/callback/corp",
		Scopes:      []string{"openid"}, Algs: []string{"RS256"}, HTTP: is.srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewHandler(HandlerOptions{
		Providers: []IdentityProvider{gh, is.provider(t, "idp"), labelled, &fakeProvider{name: "plain"}},
		Sessions:  s, UI: oidcUI(),
	})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/ui/auth/providers", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	hd := w.Header()
	for k, want := range map[string]string{
		"Content-Type":            "application/json",
		"Cache-Control":           "no-store",
		"Content-Security-Policy": pageCSP,
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "no-referrer",
	} {
		if got := hd.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if len(w.Result().Cookies()) != 0 {
		t.Errorf("cookies set: %v", w.Result().Cookies())
	}
	body := w.Body.String()
	var got []map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err, body)
	}
	want := []map[string]any{
		{"name": "github", "label": "GitHub"},
		{"name": "idp", "label": "idp"},
		{"name": "corp", "label": "Company SSO"},
		{"name": "plain", "label": "plain"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if len(got[i]) != 2 || got[i]["name"] != want[i]["name"] || got[i]["label"] != want[i]["label"] {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	for _, leak := range []string{"gh-client-id-123", "gh-client-secret-456", "corp-client-id", "corp-secret", is.srv.URL, testClientID, testClientSecret, "callback"} {
		if strings.Contains(body, leak) {
			t.Errorf("body leaks %q: %s", leak, body)
		}
	}

	// Only GET.
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/ui/auth/providers", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}
