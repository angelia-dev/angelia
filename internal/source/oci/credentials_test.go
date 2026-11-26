package oci_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/oci"
	"github.com/angelia-dev/angelia/internal/source/registryauth"
)

// loginCanary is a synthetic registry password: it must reach the realm (or a
// registry that asks for Basic) only, and never a log, an error or the state.
const loginCanary = "CANARY-oci-password-0123456789"

const robot = "robot"

var wantBasic = "Basic " + base64.StdEncoding.EncodeToString([]byte(robot+":"+loginCanary))

// resolver resolves one stored secret, checking its host binding as
// secrets.Resolver does.
type resolver struct{ hosts []string }

func (r resolver) Resolve(_ context.Context, name string, dest *url.URL) (secrets.Bound, error) {
	if name != "registry-pw" {
		return secrets.Bound{}, fmt.Errorf("%w: secret %s does not exist", secrets.ErrNotFound, name)
	}
	b := secrets.Bound{Name: name, Value: secrets.NewValueString(loginCanary), Hosts: r.hosts}
	if err := b.Allows(dest); err != nil {
		return secrets.Bound{}, err
	}
	return b, nil
}

func dest(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	d, err := secrets.Destination(u)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// privateReg is a private registry and its token realm on two hosts (two
// httptest servers), as Docker Hub has registry-1.docker.io and
// auth.docker.io.
type privateReg struct {
	mu        sync.Mutex
	basic     bool   // the registry asks for Basic instead of a Bearer token
	realmCode int    // status the realm answers when the credential is right (0: 200)
	denyAll   bool   // the registry answers 401 to every request
	realmTo   string // the realm redirects (302) there
	page2To   string // the registry redirects (302) its second page there
	realmAuth []string
	regAuth   []string
	realmURL  string

	reg, realm *httptest.Server
}

func newPrivateReg(t *testing.T) *privateReg {
	t.Helper()
	p := &privateReg{}
	p.realm = httptest.NewServer(http.HandlerFunc(p.serveRealm))
	p.reg = httptest.NewServer(http.HandlerFunc(p.serveRegistry))
	t.Cleanup(p.realm.Close)
	t.Cleanup(p.reg.Close)
	p.realmURL = p.realm.URL + "/token"
	return p
}

func (p *privateReg) serveRealm(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.realmAuth = append(p.realmAuth, r.Header.Get("Authorization"))
	if p.realmTo != "" && r.URL.Path == "/token" {
		http.Redirect(w, r, p.realmTo, http.StatusFound)
		return
	}
	if r.Header.Get("Authorization") != wantBasic {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if p.realmCode != 0 {
		w.WriteHeader(p.realmCode)
		return
	}
	_, _ = fmt.Fprintf(w, `{"token":%q,"expires_in":300}`, canary)
}

func (p *privateReg) serveRegistry(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	got := r.Header.Get("Authorization")
	p.regAuth = append(p.regAuth, got)
	if r.URL.Path == "/steal" {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	ok := got == "Bearer "+canary
	ch := fmt.Sprintf(`Bearer realm=%q,service="registry.test",scope="repository:team/app:pull"`, p.realmURL)
	if p.basic {
		ok, ch = got == wantBasic, `Basic realm="registry"`
	}
	if !ok || p.denyAll {
		w.Header().Set("WWW-Authenticate", ch)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodHead {
		w.Header().Set("Docker-Content-Digest", dig("c"))
		return
	}
	if p.page2To != "" && r.URL.Query().Get("last") != "" {
		http.Redirect(w, r, p.page2To, http.StatusFound)
		return
	}
	if r.URL.Query().Get("last") == "" {
		w.Header().Set("Link", `</v2/team/app/tags/list?last=1.0.0&n=1000>; rel="next"`)
		_, _ = w.Write([]byte(`{"name":"team/app","tags":["1.0.0"]}`))
		return
	}
	_, _ = w.Write([]byte(`{"name":"team/app","tags":["1.1.0"]}`))
}

func (p *privateReg) snapshot() (realm, reg []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.realmAuth...), append([]string(nil), p.regAuth...)
}

// image is the coordinate of the test repository on the pinned registry.
func (p *privateReg) image() source.Target {
	return img(strings.TrimPrefix(p.reg.URL, "http://") + "/team/app")
}

// pinned builds a source pinned to the registry with a basic credential whose
// secret is bound to hosts, through registryauth as the process wires it.
func (p *privateReg) pinned(t *testing.T, base string, hosts ...string) *oci.Source {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{Proxy: nil}}
	u, _ := url.Parse(p.reg.URL)
	allow := []string{u.Hostname()}
	cred := registryauth.Wrap(hc, registryauth.Config{
		PasswordRef: "registry-pw", Username: robot, Resolver: resolver{hosts: hosts},
		BaseURL: u, Proxy: func(*http.Request) (*url.URL, error) { return nil, nil }, AllowHTTPHosts: allow,
	})
	return oci.New(base, hc, oci.WithCredentials(cred), oci.WithEndpoint(u.Host, p.reg.URL), oci.WithAllowHTTPHosts(allow...))
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &logs
}

// noLeak fails when the password or the registry token shows in any of the
// texts (AC 4 of 94-C).
func noLeak(t *testing.T, texts map[string]string) {
	t.Helper()
	enc := base64.StdEncoding.EncodeToString([]byte(robot + ":" + loginCanary))
	for name, s := range texts {
		for _, c := range []string{loginCanary, enc, canary} {
			if strings.Contains(s, c) {
				t.Errorf("secret %q leaked into %s", c, name)
			}
		}
	}
}

// AC 94-C (1), (3), (4): a secret bound to the registry and the realm hosts
// reaches the realm only; the registry gets the registry token, never the
// credential.
func TestCredentialGoesToTheRealmOnly(t *testing.T) {
	logs := captureLogs(t)
	p := newPrivateReg(t)
	s := p.pinned(t, p.reg.URL, dest(t, p.reg.URL), dest(t, p.realm.URL))
	first, err := s.List(t.Context(), p.image(), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.List(t.Context(), p.image(), source.FetchState{Cursor: "AAAAAA=="}) // a cursor: digests are looked up
	if err != nil {
		t.Fatal(err)
	}
	realm, reg := p.snapshot()
	if len(realm) != 2 || realm[0] != wantBasic || realm[1] != wantBasic {
		t.Errorf("realm Authorization = %q, want the Basic credential once per List", realm)
	}
	for i, a := range reg {
		if a != "" && a != "Bearer "+canary {
			t.Errorf("registry request %d carried %q: only the registry token may go there", i, a)
		}
	}
	if len(res.Versions) != 2 || res.Versions[0].Metadata["digest"] != dig("c") {
		t.Errorf("versions = %+v", res.Versions)
	}
	noLeak(t, map[string]string{
		"logs": logs.String(), "state": fmt.Sprint(first.State, res.State), "versions": fmt.Sprint(res.Versions),
	})
}

// AC 94-C (2), (4): a secret not bound to the realm's host fails the call
// with ErrHostMismatch, and the realm receives nothing.
func TestCredentialRefusedForAnUnboundRealm(t *testing.T) {
	logs := captureLogs(t)
	p := newPrivateReg(t)
	s := p.pinned(t, p.reg.URL, dest(t, p.reg.URL))
	_, err := s.List(t.Context(), p.image(), source.FetchState{})
	if !errors.Is(err, secrets.ErrHostMismatch) {
		t.Fatalf("err = %v, want ErrHostMismatch", err)
	}
	realm, reg := p.snapshot()
	if len(realm) != 0 {
		t.Errorf("the realm received %d requests", len(realm))
	}
	if len(reg) != 1 || reg[0] != "" {
		t.Errorf("registry Authorization = %q, want one anonymous request", reg)
	}
	noLeak(t, map[string]string{"logs": logs.String(), "error": err.Error()})
}

// A registry that answers with a Basic challenge receives the credential,
// on every later request of the call (pages and digest HEADs), when the
// secret is bound to its host; there is no realm.
func TestBasicChallengeSendsTheCredentialToTheRegistry(t *testing.T) {
	logs := captureLogs(t)
	p := newPrivateReg(t)
	p.basic = true
	s := p.pinned(t, p.reg.URL, dest(t, p.reg.URL))
	res, err := s.List(t.Context(), p.image(), source.FetchState{Cursor: "AAAAAA=="})
	if err != nil {
		t.Fatal(err)
	}
	realm, reg := p.snapshot()
	// 401, retry, page 2, two HEADs.
	want := []string{"", wantBasic, wantBasic, wantBasic, wantBasic}
	if fmt.Sprint(reg) != fmt.Sprint(want) {
		t.Errorf("registry Authorization = %q, want %q", reg, want)
	}
	if len(realm) != 0 {
		t.Errorf("realm requests = %d", len(realm))
	}
	noLeak(t, map[string]string{"logs": logs.String(), "state": fmt.Sprint(res.State), "versions": fmt.Sprint(res.Versions)})
}

// A Basic challenge from a registry the secret is not bound to fails with
// ErrHostMismatch and the credential is not sent.
func TestBasicChallengeRefusedForAnUnboundRegistry(t *testing.T) {
	p := newPrivateReg(t)
	p.basic = true
	s := p.pinned(t, p.reg.URL, dest(t, p.realm.URL))
	_, err := s.List(t.Context(), p.image(), source.FetchState{})
	if !errors.Is(err, secrets.ErrHostMismatch) {
		t.Fatalf("err = %v, want ErrHostMismatch", err)
	}
	if _, reg := p.snapshot(); len(reg) != 1 || reg[0] != "" {
		t.Errorf("registry Authorization = %q", reg)
	}
	noLeak(t, map[string]string{"error": err.Error()})
}

func TestCredentialRefusals(t *testing.T) {
	t.Run("realm refuses the credential", func(t *testing.T) {
		p := newPrivateReg(t)
		p.realmCode = http.StatusUnauthorized
		s := p.pinned(t, p.reg.URL, dest(t, p.reg.URL), dest(t, p.realm.URL))
		_, err := s.List(t.Context(), p.image(), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "refused the configured credential (sources.<name>.auth)") {
			t.Fatalf("err = %v", err)
		}
		noLeak(t, map[string]string{"error": err.Error()})
	})
	t.Run("401 after Basic", func(t *testing.T) {
		p := newPrivateReg(t)
		p.basic, p.denyAll = true, true
		s := p.pinned(t, p.reg.URL, dest(t, p.reg.URL))
		_, err := s.List(t.Context(), p.image(), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "not found or access denied (HTTP 401 with the configured credential)") {
			t.Fatalf("err = %v", err)
		}
		if _, reg := p.snapshot(); len(reg) != 2 {
			t.Errorf("registry requests = %d, want the 401 and one retry", len(reg))
		}
		noLeak(t, map[string]string{"error": err.Error()})
	})
}

// The built-in (unpinned) oci is anonymous only (D41): a credential client
// given to it is ignored, so neither the realm nor a Basic registry gets it.
func TestUnpinnedSourceIgnoresCredentials(t *testing.T) {
	p := newPrivateReg(t)
	s := p.pinned(t, "", dest(t, p.reg.URL), dest(t, p.realm.URL))
	_, err := s.List(t.Context(), p.image(), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "token request") {
		t.Fatalf("err = %v, want the anonymous token request refused", err)
	}
	if realm, _ := p.snapshot(); len(realm) != 1 || realm[0] != "" {
		t.Errorf("realm Authorization = %q, want one anonymous request", realm)
	}

	p = newPrivateReg(t)
	p.basic = true
	s = p.pinned(t, "", dest(t, p.reg.URL))
	_, err = s.List(t.Context(), p.image(), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "sources.<name>.auth") {
		t.Fatalf("err = %v", err)
	}
	if _, reg := p.snapshot(); len(reg) != 1 || reg[0] != "" {
		t.Errorf("registry Authorization = %q", reg)
	}
}

// Review of 94-C: a redirect off the request's origin is not followed with
// the credential, even when the secret is bound to the target host
// (registryauth sets the header inside the transport, out of sight of
// GuardRedirects).
func TestCredentialIsNotFollowedAcrossOrigins(t *testing.T) {
	t.Run("realm redirects to the registry", func(t *testing.T) {
		p := newPrivateReg(t)
		p.realmTo = p.reg.URL + "/steal"
		s := p.pinned(t, p.reg.URL, dest(t, p.reg.URL), dest(t, p.realm.URL))
		_, err := s.List(t.Context(), p.image(), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "refusing to follow a redirect") {
			t.Fatalf("err = %v", err)
		}
		if _, reg := p.snapshot(); len(reg) != 1 || reg[0] != "" {
			t.Errorf("registry Authorization = %q, want only the first anonymous request", reg)
		}
		noLeak(t, map[string]string{"error": err.Error()})
	})
	t.Run("Basic registry redirects to another origin", func(t *testing.T) {
		p := newPrivateReg(t)
		p.basic = true
		p.page2To = p.realm.URL + "/elsewhere"
		s := p.pinned(t, p.reg.URL, dest(t, p.reg.URL), dest(t, p.realm.URL))
		_, err := s.List(t.Context(), p.image(), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "refusing to follow a redirect") {
			t.Fatalf("err = %v", err)
		}
		if realm, _ := p.snapshot(); len(realm) != 0 {
			t.Errorf("the other origin received %q", realm)
		}
		noLeak(t, map[string]string{"error": err.Error()})
	})
}
