package webauth

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/angelia-dev/angelia/internal/config"
)

// An email claim that is not a plain address (control characters, longer
// than an address can be) is dropped, never trimmed into another address
// that could match an entry.
func TestOIDCEmailClaimNotRewritten(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	p := is.provider(t, "kc")
	long := strings.Repeat("a", 64) + "@" + strings.Repeat("b", 185) + ".com" // 254
	for name, email := range map[string]string{
		"control character": "alice\x01@example.com",
		"newline":           "alice@example.com\n",
		"too long":          long + "x.evil.example",
		"two at signs":      "alice@example.com@evil.example",
	} {
		t.Run(name, func(t *testing.T) {
			is.set(func(is *testIssuer) { is.claims = func(c map[string]any) { c["email"] = email } })
			id, err := is.signIn(t, p)
			if err != nil {
				t.Fatal(err)
			}
			if id.verifiedEmail() != "" || id.Email != "" {
				t.Fatalf("email %q kept as %q", email, id.Email)
			}
		})
	}
	is.set(func(is *testIssuer) { is.claims = func(c map[string]any) { c["email"] = long } })
	if id, err := is.signIn(t, p); err != nil || id.verifiedEmail() != long {
		t.Fatal(err, id.Email)
	}
}

// Email entries fold ASCII case only: a Unicode character that folds to an
// ASCII letter (the Kelvin sign K, the long s ſ) does not match, and does
// not take the pin of the address it folds to.
func TestOIDCEmailASCIIFold(t *testing.T) {
	ui := oidcUI()
	ui.Access = []config.UIAccess{{Provider: "idp", Email: "kim@example.com", Role: config.UIRoleAdmin}}
	s, st, _, _ := newTestSessions(t, ui)
	for _, email := range []string{"Kim@example.com", "kim@example.ℯom", "kim@examplе.com"} {
		m, err := s.access.role(t.Context(), "idp", "attacker", "x", email)
		if err != nil || m.role != "" || m.mismatch {
			t.Fatalf("%q: %+v %v", email, m, err)
		}
	}
	if _, err := st.UIIdentityPin(t.Context(), "idp", "email:kim@example.com"); err == nil {
		t.Fatal("a look-alike address pinned the entry")
	}
	m, err := s.access.role(t.Context(), "idp", "kim-sub", "x", "KIM@Example.COM")
	if err != nil || m.role != config.UIRoleAdmin {
		t.Fatal(m, err)
	}
	if got, err := st.UIIdentityPin(t.Context(), "idp", "email:kim@example.com"); err != nil || got != "kim-sub" {
		t.Fatal(got, err)
	}
	if emailPinKey("Kim@x") == emailPinKey("kim@x") {
		t.Fatal("the pin key folds non-ASCII")
	}
}

// Two providers with one name would share routes, metrics and principal
// ids: the handler refuses them.
func TestNewHandlerDuplicateProvider(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	s, _, _, _ := newTestSessions(t, oidcUI())
	_, err := NewHandler(HandlerOptions{
		Providers: []IdentityProvider{is.provider(t, "idp"), is.provider(t, "idp")}, Sessions: s, UI: oidcUI(),
	})
	if err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatal(err)
	}
}

// While the issuer is down, sign-ins fail fast for discoveryRetry instead
// of each starting a discovery request; afterwards discovery is tried again.
func TestOIDCDiscoveryBackoff(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	is.discovery = func(d map[string]any) { d["status"] = 500 }
	p := is.provider(t, "kc")
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	p.cfg.Now = func() time.Time { return time.Unix(0, now.Load()) }
	for range 5 {
		if _, err := p.AuthCodeURL(t.Context(), "s", "c", "n"); err == nil {
			t.Fatal("no error")
		}
	}
	if is.discoveryHits != 1 {
		t.Fatalf("%d discovery requests", is.discoveryHits)
	}
	is.set(func(is *testIssuer) { is.discovery = nil })
	now.Add(int64(discoveryRetry + time.Second))
	if _, err := p.AuthCodeURL(t.Context(), "s", "c", "n"); err != nil || is.discoveryHits != 2 {
		t.Fatal(err, is.discoveryHits)
	}
}

// A sign-in waiting for another's discovery gives up with its own context.
func TestOIDCDiscoveryWaitCancelled(t *testing.T) {
	is := newTestIssuer(t, jose.RS256)
	p := is.provider(t, "kc")
	p.sem <- struct{}{} // a discovery in flight
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.AuthCodeURL(ctx, "s", "c", "n"); err == nil || ctx.Err() == nil {
		t.Fatal(err)
	}
	<-p.sem
	// Concurrent sign-ins share one discovery.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if _, err := p.AuthCodeURL(t.Context(), "s", "c", "n"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if is.discoveryHits != 1 {
		t.Fatal(is.discoveryHits)
	}
}
