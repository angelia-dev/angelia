package dispatch

import (
	"context"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/store"
)

// selfSink derives its destinations from the secret value, which is a list
// of origins separated by spaces ("bad" fails the derivation).
type selfSink struct{ testSink }

func (*selfSink) SecretDestinations(_ string, v secrets.Value) ([]*url.URL, error) {
	var out []*url.URL
	for f := range strings.FieldsSeq(string(v.Reveal())) {
		if f == "bad" {
			return nil, errors.New("chat: unparsable service URL")
		}
		u, err := url.Parse(f)
		if err != nil {
			return nil, errors.New("chat: unparsable origin")
		}
		out = append(out, u)
	}
	return out, nil
}

type guardFunc func(*url.URL) error

func (f guardFunc) CheckURL(_ context.Context, u *url.URL) error { return f(u) }

// ADR 0026 §2-4: for a kind whose config marks x-angelia-secret-destination,
// the dispatcher resolves the reference without a destination, has the sink
// derive the origins, checks each (https or allow_http_hosts, the SSRF
// guard, the secret's hosts) and passes the checked host:port list to
// Deliver. Every failure is retried before Deliver, with a last_error naming
// the pointer, the secret and the origin, never the value or the resolved
// address.
func TestSecretDestinationsCheckedBeforeDeliver(t *testing.T) {
	guard := guardFunc(func(u *url.URL) error {
		if u.Hostname() == "internal.example" {
			return &netguard.BlockedError{Host: "internal.example", Addr: netip.MustParseAddr("10.9.8.7")}
		}
		return nil
	})
	const secretTail = "/TOKEN-path-never-shown" //nolint:gosec // G101: synthetic test value
	for _, tc := range []struct {
		name, value string
		hosts       []string
		env         bool
		guard       URLChecker
		want        string // a last_error fragment; "" means delivered
		dests       []string
	}{
		{name: "unbound", value: "https://chat.example https://chat.example:8443 https://chat.example", dests: []string{"chat.example:443", "chat.example:8443"}},
		{name: "bound", value: "https://chat.example", hosts: []string{"chat.example"}, dests: []string{"chat.example:443"}},
		{name: "env", value: "https://chat.example", env: true, dests: []string{"chat.example:443"}},
		{name: "http allowed", value: "http://lan.example:8080", dests: []string{"lan.example:8080"}},
		{name: "http refused", value: "http://chat.example", want: "dispatch: secret /url_ref (secret chat): http://chat.example: http:// is not allowed for this host"},
		{name: "derivation", value: "bad", want: "dispatch: secret /url_ref (secret chat): chat: unparsable service URL"},
		{name: "none", value: "", want: "no destination derived"},
		{name: "not an origin", value: "https://chat.example" + secretTail, want: "derived a destination that is not an origin"},
		{name: "userinfo", value: "https://u:p@chat.example", want: "not an origin"}, //nolint:gosec // G101: synthetic URL
		{name: "ftp", value: "ftp://chat.example", want: "ftp://chat.example: scheme not allowed"},
		{name: "ssrf", value: "https://internal.example", want: "https://internal.example: destination not allowed by the SSRF guard"},
		{name: "no guard", value: "https://chat.example", guard: URLChecker(nil), want: "no SSRF guard configured"},
		{
			name: "host binding", value: "https://chat.example https://other.example", hosts: []string{"chat.example"},
			want: "dispatch: secret /url_ref (secret chat): https://other.example: secrets: host not allowed: secret chat is bound to chat.example; refused for other.example:443",
		},
		{name: "env no guard", value: "https://chat.example", env: true, guard: URLChecker(nil), want: "(environment variable CHAT_URL): https://chat.example: no SSRF guard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := `{"url_ref":"chat"}`
			env := map[string]string{}
			if tc.env {
				cfg = `{"url_env":"CHAT_URL"}`
				env["CHAT_URL"] = tc.value
			}
			st := storeWithSink(t, sinkKind, cfg)
			ss := newSecretStore(t)
			if !tc.env {
				ss.put(t, "chat", tc.value, tc.hosts...)
			}
			ts := &selfSink{}
			opts := Options{
				Clock: &fakeClock{now: t0}, Secrets: ss.resolver(), LookupEnv: envMap(env),
				SecretDestinations: map[string][]string{sinkKind: {"/url_env", "/url_ref"}},
				URLCheck:           guard, AllowHTTPHosts: []string{"LAN.example"},
			}
			if tc.name == "no guard" || tc.name == "env no guard" {
				opts.URLCheck = nil
			}
			d := newDispatcher(t, st, registry(t, ts), opts)
			pollWait(t, d)
			del := delivery(t, st, "E0000")
			reqs := ts.requests()
			if tc.want == "" {
				if del.Status != store.DeliveryDelivered || len(reqs) != 1 {
					t.Fatalf("delivery %s %q, %d requests", del.Status, del.LastError, len(reqs))
				}
				if got := strings.Join(reqs[0].Destinations, ","); got != strings.Join(tc.dests, ",") {
					t.Errorf("Destinations %q, want %q", got, tc.dests)
				}
				return
			}
			if del.Status != store.DeliveryPending || !strings.Contains(del.LastError, tc.want) || len(reqs) != 0 {
				t.Fatalf("delivery %s %q (%d requests), want pending with %q", del.Status, del.LastError, len(reqs), tc.want)
			}
			for _, leak := range []string{secretTail, "10.9.8.7", "u:p"} {
				if strings.Contains(del.LastError, leak) {
					t.Errorf("last_error %q holds %q", del.LastError, leak)
				}
			}
		})
	}
}

// A kind that marks x-angelia-secret-destination but whose sink cannot
// derive its destinations fails closed, before Deliver.
func TestSecretDestinationsWithoutDerivationFailClosed(t *testing.T) {
	st := storeWithSink(t, sinkKind, `{"url_env":"CHAT_URL"}`)
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{
		Clock: &fakeClock{now: t0}, LookupEnv: envMap(map[string]string{"CHAT_URL": "https://chat.example"}),
		SecretDestinations: map[string][]string{sinkKind: {"/url_env"}}, URLCheck: guardFunc(func(*url.URL) error { return nil }),
	})
	pollWait(t, d)
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryPending || !strings.Contains(del.LastError, "does not derive its destinations") || len(ts.requests()) != 0 {
		t.Fatalf("delivery %s %q", del.Status, del.LastError)
	}
	var _ sink.SecretDestinations = (*selfSink)(nil)
}
