package wasm_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/source"
)

const testToken = "s3cret-token-0123456789"

func staticToken(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

// hostAuthLog records the Authorization header of every request per
// host, and echoes it in the body and in a response header, like a
// misbehaving or debugging upstream would.
type hostAuthLog struct {
	mu   sync.Mutex
	seen map[string][]string
}

func (a *hostAuthLog) handler(host string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get("Authorization")
		a.mu.Lock()
		if a.seen == nil {
			a.seen = map[string][]string{}
		}
		a.seen[host] = append(a.seen[host], v)
		a.mu.Unlock()
		if loc := r.URL.Query().Get("to"); loc != "" {
			http.Redirect(w, r, loc, http.StatusFound) //nolint:gosec // G710: test upstream, redirects are the point.
			return
		}
		w.Header().Set("X-Echo", v)
		_, _ = fmt.Fprintf(w, "you sent %q", v) //nolint:gosec // G705: test upstream echoing on purpose.
	})
}

func (a *hostAuthLog) got(host string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.seen[host])
}

// newCredentialed returns the hostile plugin with allowed.test from its
// manifest, other.test added by the operator and a credential bound to
// allowed.test.
func newCredentialed(t *testing.T, opts wasm.Options) (*hostAuthLog, *testNet, *wasm.Source) {
	t.Helper()
	rec := &hostAuthLog{}
	net, hc := newTestNet(t, map[string]http.Handler{
		"allowed.test": rec.handler("allowed.test"),
		"other.test":   rec.handler("other.test"),
	})
	opts.HTTPClient = hc
	opts.AllowedHosts = []string{"other.test"}
	if opts.Credentials == nil {
		opts.Credentials = []wasm.Credential{{Host: "allowed.test", Scheme: "Bearer", Token: staticToken(testToken)}}
	}
	return rec, net, newHostile(t, hostileManifest(t, "allowed.test"), opts)
}

func body(t *testing.T, res source.Result) string {
	t.Helper()
	if len(res.Versions) != 1 {
		t.Fatalf("versions = %+v", res.Versions)
	}
	b, _ := res.Versions[0].Metadata["body"].(string)
	return b
}

// AC: credentials are injected by the host into requests to the host they
// are bound to, and only there.
func TestCredentialInjectedOnBoundHostOnly(t *testing.T) {
	rec, _, s := newCredentialed(t, wasm.Options{})

	if _, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://allowed.test/a"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://other.test/a"), source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if got := rec.got("allowed.test"); !slices.Equal(got, []string{"Bearer " + testToken}) {
		t.Errorf("allowed.test saw Authorization %q, want the injected credential", got)
	}
	if got := rec.got("other.test"); !slices.Equal(got, []string{""}) {
		t.Errorf("other.test saw Authorization %q, want none", got)
	}

	// The plugin's own Authorization on the bound host is replaced.
	_, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://allowed.test/b", "authorization", "plugin-value"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.got("allowed.test"); got[len(got)-1] != "Bearer "+testToken {
		t.Errorf("allowed.test saw %q, want the host's credential over the plugin's", got[len(got)-1])
	}
}

// AC: the plugin cannot read an injected credential, nor send it to
// another allowed host.
func TestPluginCannotReadOrForwardCredential(t *testing.T) {
	logs := &logRecorder{}
	rec, _, s := newCredentialed(t, wasm.Options{Logger: slog.New(logs)})

	t.Run("echo is redacted", func(t *testing.T) {
		res, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://allowed.test/echo"), source.FetchState{})
		if err != nil {
			t.Fatal(err)
		}
		if b := body(t, res); strings.Contains(b, testToken) || !strings.Contains(b, "[REDACTED]") {
			t.Errorf("plugin read body %q, want the token redacted", b)
		}
		if out, _ := json.Marshal(res); strings.Contains(string(out), testToken) {
			t.Errorf("result holds the token: %s", out)
		}
	})

	t.Run("redirect to another allowed host drops it", func(t *testing.T) {
		u := "https://allowed.test/r?to=https://other.test/landing"
		if _, err := s.List(t.Context(), hostileTarget("fetch", "url", u), source.FetchState{}); err != nil {
			t.Fatal(err)
		}
		if got := rec.got("other.test"); slices.ContainsFunc(got, func(v string) bool { return v != "" }) {
			t.Errorf("other.test saw Authorization %q after a redirect, want none", got)
		}
	})

	t.Run("redirect back to the bound host gets it again", func(t *testing.T) {
		before := len(rec.got("allowed.test"))
		u := "https://other.test/r?to=https://allowed.test/back"
		if _, err := s.List(t.Context(), hostileTarget("fetch", "url", u), source.FetchState{}); err != nil {
			t.Fatal(err)
		}
		if got := rec.got("allowed.test")[before:]; !slices.Equal(got, []string{"Bearer " + testToken}) {
			t.Errorf("allowed.test saw %q", got)
		}
	})

	t.Run("not over plain http", func(t *testing.T) {
		before := len(rec.got("allowed.test"))
		_, err := s.List(t.Context(), hostileTarget("fetch", "url", "http://allowed.test/a"), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "only sent over https") {
			t.Fatalf("err = %v, want the credential refused over http", err)
		}
		if strings.Contains(err.Error(), testToken) {
			t.Errorf("error holds the token: %v", err)
		}
		if got := rec.got("allowed.test")[before:]; len(got) != 0 {
			t.Errorf("request reached the network: %q", got)
		}
	})

	for _, r := range logs.all() {
		var b strings.Builder
		b.WriteString(r.Message)
		r.Attrs(func(a slog.Attr) bool { b.WriteString(a.String()); return true })
		if strings.Contains(b.String(), testToken) {
			t.Errorf("log holds the token: %s", b.String())
		}
	}
}

func TestCredentialTokenErrors(t *testing.T) {
	for name, tok := range map[string]func(context.Context) (string, error){
		"resolver fails": func(context.Context) (string, error) { return "", errors.New("environment variable X is not set") },
		"empty":          staticToken(""),
		"header split":   staticToken("abc\r\nX-Evil: 1"),
	} {
		t.Run(name, func(t *testing.T) {
			rec, _, s := newCredentialed(t, wasm.Options{Credentials: []wasm.Credential{{Host: "allowed.test", Token: tok}}})
			_, err := s.List(t.Context(), hostileTarget("fetch", "url", "https://allowed.test/"), source.FetchState{})
			if err == nil || !strings.Contains(err.Error(), "credential") {
				t.Fatalf("err = %v, want a credential error", err)
			}
			if strings.Contains(err.Error(), "X-Evil") {
				t.Errorf("error echoes the token: %v", err)
			}
			if got := rec.got("allowed.test"); len(got) != 0 {
				t.Errorf("request reached the network: %q", got)
			}
		})
	}
}

// AC: the operator's allowed_hosts are merged with the manifest's and
// shown when the plugin is installed.
func TestOperatorAllowedHostsMergedAndShown(t *testing.T) {
	logs := &logRecorder{}
	_, _, s := newCredentialed(t, wasm.Options{Logger: slog.New(logs)})
	want := []wasm.HostGrant{
		{Host: "allowed.test", Manifest: true, Credential: true},
		{Host: "other.test", Operator: true},
	}
	if got := s.Hosts(); !slices.Equal(got, want) {
		t.Errorf("Hosts() = %+v, want %+v", got, want)
	}
	var shown bool
	for _, r := range logs.all() {
		if r.Message == "wasm plugin installed" {
			r.Attrs(func(a slog.Attr) bool {
				shown = shown || (a.Key == "allowed_hosts" && strings.Contains(a.Value.String(), "other.test"))
				return true
			})
		}
	}
	if !shown {
		t.Error("no \"wasm plugin installed\" log with the merged allowed_hosts")
	}
}

func TestInstallErrors(t *testing.T) {
	_, hc := newTestNet(t, nil)
	cases := map[string]wasm.Options{
		"bad operator host":     {AllowedHosts: []string{"*.example.com"}},
		"credential not listed": {Credentials: []wasm.Credential{{Host: "other.test", Token: staticToken("x")}}},
		"credential twice": {Credentials: []wasm.Credential{
			{Host: "allowed.test", Token: staticToken("x")}, {Host: "ALLOWED.test", Token: staticToken("y")},
		}},
		"no token func":  {Credentials: []wasm.Credential{{Host: "allowed.test"}}},
		"bad scheme":     {Credentials: []wasm.Credential{{Host: "allowed.test", Scheme: "Bearer x", Token: staticToken("x")}}},
		"bad credential": {Credentials: []wasm.Credential{{Host: "https://allowed.test", Token: staticToken("x")}}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			opts.HTTPClient = hc
			if s, err := wasm.New(t.Context(), hostileManifest(t, "allowed.test"), nil, opts); err == nil {
				_ = s.Close(context.Background())
				t.Fatal("New succeeded")
			}
		})
	}
}

// An injected credential counts as authenticated for netguard's limiter.
func TestInjectedCredentialUsesAuthenticatedBudget(t *testing.T) {
	rec := &hostAuthLog{}
	n := &testNet{handlers: map[string]http.Handler{"allowed.test": rec.handler("allowed.test")}}
	nc, err := netguard.New(netguard.Config{
		Transport: n,
		Resolver:  fakeResolver{"allowed.test": {netip.MustParseAddr("203.0.113.10")}},
		HostLimits: map[string]netguard.HostLimit{"allowed.test": {
			Authenticated: netguard.Limit{Rate: 1e6, Burst: 1000},
			Anonymous:     netguard.Limit{Rate: 0.001, Burst: 1},
		}},
		MaxAttempts: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := newHostile(t, hostileManifest(t, "allowed.test"), wasm.Options{
		HTTPClient:  nc.HTTPClient(),
		Credentials: []wasm.Credential{{Host: "allowed.test", Token: staticToken(testToken)}},
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for i := range 3 {
		if _, err := s.List(ctx, hostileTarget("fetch", "url", "https://allowed.test/"), source.FetchState{}); err != nil {
			t.Fatalf("request %d: %v (anonymous budget used?)", i, err)
		}
	}
}
