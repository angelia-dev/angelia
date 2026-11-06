package dispatch

import (
	"context"
	"crypto/rand"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// storeWithSink is newStore with the sink's kind and configuration chosen,
// and one event (E0000) with a delivery due at t0.
func storeWithSink(t *testing.T, kind, cfg string) *sqlite.Store {
	t.Helper()
	st := openStore(t, filepath.Join(t.TempDir(), "angelia.db"))
	must(t, st.CreateSink(t.Context(), store.Sink{ID: "k1", Kind: kind, ConfigEncrypted: []byte(cfg)}))
	must(t, st.CreateSubscription(t.Context(), store.Subscription{ID: "s1", SinkID: "k1"}))
	addEvents(t, st, 1)
	return st
}

func envMap(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// secretStore is the store read of secrets.Resolver, over sealed rows held
// in memory, with a real keyring.
type secretStore struct {
	mu     sync.Mutex
	master []byte
	keys   *secrets.Keyring
	rows   map[string]store.Secret
}

func newSecretStore(t *testing.T) *secretStore {
	t.Helper()
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	k, err := secrets.NewKeyring(master)
	if err != nil {
		t.Fatal(err)
	}
	return &secretStore{master: master, keys: k, rows: map[string]store.Secret{}}
}

// put seals value under name, bound to hosts.
func (s *secretStore) put(t *testing.T, name, value string, hosts ...string) {
	t.Helper()
	id := "01JD00000000000000000SEC" + strings.ToUpper(name[:2])
	env, kid, err := s.keys.Seal(id, name, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[name] = store.Secret{ID: id, Name: name, Hosts: hosts, KeyID: kid, Ciphertext: env, Revision: 1}
}

func (s *secretStore) SecretByName(_ context.Context, name string) (store.Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rows[name]
	if !ok {
		return store.Secret{}, store.ErrNotFound
	}
	return r, nil
}

func (s *secretStore) resolver() *secrets.Resolver { return &secrets.Resolver{Store: s, Keys: s.keys} }

func TestResolveSecrets(t *testing.T) {
	env := envMap(map[string]string{"WH": "whsec_value", "TOK": "t0k", "EMPTY": ""})
	ss := newSecretStore(t)
	ss.put(t, "hook", "whsec_stored")
	ss.put(t, "bound", "b0und", "h.example", "other.example:8443")
	dest, _ := url.Parse("https://h.example/hook")
	src := SecretSources{LookupEnv: env, Stored: ss.resolver(), Destination: dest}
	got, err := ResolveSecrets(t.Context(),
		[]byte(`{"url":"https://h/","secret_env":"WH","secret_ref":"hook","auth":{"token_env":"TOK","key_ref":"bound"},"targets":[{"x":1},{"a/b~_env":"TOK"}],"n":1.5}`), src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{ //nolint:gosec // test values, not credentials
		"/secret_env": "whsec_value", "/auth/token_env": "t0k", "/targets/1/a~1b~0_env": "t0k",
		"/secret_ref": "whsec_stored", "/auth/key_ref": "b0und",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d values, want %d", len(got), len(want))
	}
	for k, v := range want {
		if !got[k].Equal([]byte(v)) {
			t.Errorf("%s: wrong value (%d bytes)", k, got[k].Len())
		}
	}
	if got, err := ResolveSecrets(t.Context(), []byte(`{"url":"https://h/"}`), src); got != nil || err != nil {
		t.Errorf("no references: %v, %v", got, err)
	}
	if got, err := ResolveSecrets(t.Context(), []byte(`not json`), src); got != nil || err != nil {
		t.Errorf("invalid JSON is left to the sink: %v, %v", got, err)
	}
	other, _ := url.Parse("https://evil.example/hook")
	for _, tc := range []struct {
		cfg string
		src SecretSources
		msg string
	}{
		{`{"secret_env":"MISSING"}`, src, "dispatch: secret /secret_env: environment variable MISSING is not set or empty"},
		{`{"secret_env":"EMPTY"}`, src, "dispatch: secret /secret_env: environment variable EMPTY is not set or empty"},
		{`{"secret_env":"whsec_pastedSecret"}`, src, "dispatch: secret /secret_env: not an environment variable name"},
		{`{"secret_env":"has space"}`, src, "dispatch: secret /secret_env: not an environment variable name"},
		{`{"secret_env":7}`, src, "dispatch: secret /secret_env: not an environment variable name"},
		{`{"secret_ref":"missing"}`, src, "dispatch: secret /secret_ref: secrets: no such secret: secret missing does not exist"},
		{`{"secret_ref":"whsec_Pasted"}`, src, "dispatch: secret /secret_ref: not a stored secret name"},
		{`{"secret_ref":["hook"]}`, src, "dispatch: secret /secret_ref: not a stored secret name"},
		{`{"a":[{"secret_ref":"hook"}]}`, SecretSources{LookupEnv: env}, "dispatch: secret /a/0/secret_ref: stored secrets are not available to this dispatcher"},
		{
			`{"secret_ref":"bound"}`,
			SecretSources{LookupEnv: env, Stored: ss.resolver(), Destination: other},
			"dispatch: secret /secret_ref: secrets: host not allowed: secret bound is bound to h.example, other.example:8443; refused for evil.example:443",
		},
		{
			`{"secret_ref":"bound"}`,
			SecretSources{LookupEnv: env, Stored: ss.resolver()},
			"dispatch: secret /secret_ref: secrets: host not allowed: secret bound is bound to hosts: secrets: no destination URL",
		},
		{
			`{"secret_ref":"hook"}`,
			SecretSources{LookupEnv: env, Stored: &secrets.Resolver{Store: ss}},
			"dispatch: secret /secret_ref: secrets: no key configured: cannot read secret hook; set secrets.key_file",
		},
	} {
		_, err := ResolveSecrets(t.Context(), []byte(tc.cfg), tc.src)
		if err == nil || err.Error() != tc.msg {
			t.Errorf("%s: %v, want %q", tc.cfg, err, tc.msg)
		}
	}
	// An unbound secret goes to any destination, or none.
	if got, err := ResolveSecrets(t.Context(), []byte(`{"secret_ref":"hook"}`), SecretSources{Stored: ss.resolver()}); err != nil || !got["/secret_ref"].Equal([]byte("whsec_stored")) {
		t.Errorf("unbound without a destination: %v", err)
	}
}

func TestDestination(t *testing.T) {
	cfg := []byte(`{"url":"https://h.example:8443/x","t":[{"u":"http://a/"}],"a/b":{"c~d":"https://c/"},"rel":"/x","n":1}`)
	for ptr, want := range map[string]string{
		"/url": "https://h.example:8443/x", "/t/0/u": "http://a/", "/a~1b/c~0d": "https://c/",
		"": "", "/missing": "", "/rel": "", "/n": "", "/t/1/u": "", "/t/x": "", "/url/deeper": "",
	} {
		got := ""
		if u := destination(cfg, ptr); u != nil {
			got = u.String()
		}
		if got != want {
			t.Errorf("destination(%q) = %q, want %q", ptr, got, want)
		}
	}
	if destination([]byte(`not json`), "/url") != nil {
		t.Error("invalid JSON gave a destination")
	}
}

// An unresolvable reference fails the attempt before Deliver, as a retried
// error; a resolved one reaches the sink in Request.Secrets.
func TestSecretsResolvedBeforeDeliver(t *testing.T) {
	st := storeWithSink(t, sinkKind, `{"url":"https://example.com/hook","secret_env":"WH_TEST"}`)
	ts := &testSink{}
	env := map[string]string{}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}, LookupEnv: envMap(env)})
	pollWait(t, d)
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryPending || !strings.Contains(del.LastError, "environment variable WH_TEST is not set or empty") {
		t.Fatalf("unset variable: %+v", del)
	}
	if n := len(ts.requests()); n != 0 {
		t.Fatalf("sink called %d times", n)
	}
	env["WH_TEST"] = "s3cr3t"
	d.opts.Clock.(*fakeClock).Set(del.NextAttemptAt)
	pollWait(t, d)
	reqs := ts.requests()
	if len(reqs) != 1 || !reqs[0].Secrets["/secret_env"].Equal([]byte("s3cr3t")) {
		t.Fatalf("requests %+v", reqs)
	}
}

// ADR 0024 §6: a secret_ref is resolved through the store, for the URL the
// kind marks x-angelia-destination. A missing secret, then a host the
// secret is not bound to, fail before Deliver and are retried, with a
// last_error naming the reference and never the value; once fixed, the
// value reaches the sink.
func TestSecretRefResolvedBeforeDeliver(t *testing.T) {
	const value = "whsec_c2VjcmV0LXZhbHVlLWZvci10aGUtdGVzdA=="
	st := storeWithSink(t, sinkKind, `{"url":"https://hooks.example.com/hook","secret_ref":"ci-webhook"}`)
	ss := newSecretStore(t)
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{
		Clock: &fakeClock{now: t0}, Secrets: ss.resolver(), Destinations: map[string]string{sinkKind: "/url"},
	})
	retry := func(want string) {
		t.Helper()
		pollWait(t, d)
		del := delivery(t, st, "E0000")
		if del.Status != store.DeliveryPending || !strings.Contains(del.LastError, want) || strings.Contains(del.LastError, value) {
			t.Fatalf("delivery %s %q, want pending with %q", del.Status, del.LastError, want)
		}
		if n := len(ts.requests()); n != 0 {
			t.Fatalf("sink called %d times", n)
		}
		d.opts.Clock.(*fakeClock).Set(del.NextAttemptAt)
	}
	retry("dispatch: secret /secret_ref: secrets: no such secret: secret ci-webhook does not exist")
	ss.put(t, "ci-webhook", value, "other.example.com")
	retry("dispatch: secret /secret_ref: secrets: host not allowed: secret ci-webhook is bound to other.example.com; refused for hooks.example.com:443")
	ss.put(t, "ci-webhook", value, "hooks.example.com")
	pollWait(t, d)
	reqs := ts.requests()
	if len(reqs) != 1 || !reqs[0].Secrets["/secret_ref"].Equal([]byte(value)) {
		t.Fatalf("requests %d", len(reqs))
	}
	if del := delivery(t, st, "E0000"); del.Status != store.DeliveryDelivered {
		t.Errorf("delivery %+v", del)
	}
}

// Without a resolver, a *_ref key at any depth still fails closed: never
// delivered without its secret.
func TestSecretRefWithoutResolverFailsClosed(t *testing.T) {
	st := storeWithSink(t, sinkKind, `{"url":"https://example.com/hook","auth":{"headers":[{"x":1},{"token_ref":"t"}]}}`)
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{Clock: &fakeClock{now: t0}})
	pollWait(t, d)
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryPending || del.LastError != "dispatch: secret /auth/headers/1/token_ref: stored secrets are not available to this dispatcher" {
		t.Errorf("delivery %s %q", del.Status, del.LastError)
	}
	if n := len(ts.requests()); n != 0 {
		t.Errorf("sink called %d times", n)
	}
}
