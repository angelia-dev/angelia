package dispatch

import (
	"path/filepath"
	"strings"
	"testing"

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

func TestResolveSecrets(t *testing.T) {
	env := envMap(map[string]string{"WH": "whsec_value", "TOK": "t0k", "EMPTY": ""})
	got, err := ResolveSecrets([]byte(`{"url":"https://h/","secret_env":"WH","auth":{"token_env":"TOK"},"targets":[{"x":1},{"a/b~_env":"TOK"}],"n":1.5}`), env)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"/secret_env": "whsec_value", "/auth/token_env": "t0k", "/targets/1/a~1b~0_env": "t0k"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if string(got[k]) != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if got, err := ResolveSecrets([]byte(`{"url":"https://h/"}`), env); got != nil || err != nil {
		t.Errorf("no references: %v, %v", got, err)
	}
	if got, err := ResolveSecrets([]byte(`not json`), env); got != nil || err != nil {
		t.Errorf("invalid JSON is left to the sink: %v, %v", got, err)
	}
	for cfg, msg := range map[string]string{
		`{"secret_env":"MISSING"}`:            "dispatch: secret /secret_env: environment variable MISSING is not set or empty",
		`{"secret_env":"EMPTY"}`:              "dispatch: secret /secret_env: environment variable EMPTY is not set or empty",
		`{"secret_env":"whsec_pastedSecret"}`: "dispatch: secret /secret_env: not an environment variable name",
		`{"secret_env":"has space"}`:          "dispatch: secret /secret_env: not an environment variable name",
		`{"secret_env":7}`:                    "dispatch: secret /secret_env: not an environment variable name",
	} {
		_, err := ResolveSecrets([]byte(cfg), env)
		if err == nil || err.Error() != msg {
			t.Errorf("%s: %v, want %q", cfg, err, msg)
		}
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
	if len(reqs) != 1 || string(reqs[0].Secrets["/secret_env"]) != "s3cr3t" {
		t.Fatalf("requests %+v", reqs)
	}
}
