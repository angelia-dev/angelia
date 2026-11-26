package dispatch

import (
	"net/url"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink/gitlabtrigger"
)

// D73 (#90): a stored token bound to the GitLab host is resolved for the
// sink's base_url (the destination pointer /base_url) and refused for any
// other host, before anything is sent.
func TestGitlabTriggerHostBoundToken(t *testing.T) {
	ss := newSecretStore(t)
	ss.put(t, "gl-trigger", "glptt-bound-token", "gitlab.example.com")
	cfg := func(base string) []byte {
		return []byte(`{"project":"g/p","ref":"main","base_url":"` + base + `","token_ref":"gl-trigger"}`)
	}
	src := func(c []byte) SecretSources {
		return SecretSources{Stored: ss.resolver(), Destination: destination(c, "/base_url")}
	}

	c := cfg("https://gitlab.example.com")
	got, err := ResolveSecrets(t.Context(), c, src(c))
	if err != nil || !got[gitlabtrigger.TokenRefPointer].Equal([]byte("glptt-bound-token")) {
		t.Fatalf("bound host: %v, %v", got, err)
	}
	c = cfg("https://gitlab.example.com/gitlab")
	if _, err := ResolveSecrets(t.Context(), c, src(c)); err != nil {
		t.Fatalf("bound host with a relative root: %v", err)
	}

	c = cfg("https://evil.example.org")
	_, err = ResolveSecrets(t.Context(), c, src(c))
	if err == nil || !strings.Contains(err.Error(), "host not allowed") || strings.Contains(err.Error(), "glptt") {
		t.Fatalf("other host: err = %v", err)
	}
	// Another port is another host.
	c = cfg("https://gitlab.example.com:8443")
	if _, err = ResolveSecrets(t.Context(), c, src(c)); err == nil {
		t.Fatal("another port accepted")
	}
	// A default config has the destination filled in by the parser; without
	// one the bound token is refused.
	c = []byte(`{"project":"g/p","ref":"main","token_ref":"gl-trigger"}`)
	if d := destination(c, "/base_url"); d != (*url.URL)(nil) {
		t.Fatalf("destination = %v", d)
	}
	if _, err = ResolveSecrets(t.Context(), c, src(c)); err == nil {
		t.Fatal("a bound token was resolved without a destination")
	}
}
