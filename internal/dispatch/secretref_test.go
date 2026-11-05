package dispatch

import (
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

func TestSecretRef(t *testing.T) {
	for cfg, want := range map[string]string{
		`{"url":"https://example.com"}`:                        "",
		`{"url":"https://example.com","secret_env":"HOOK"}`:    "",
		`{"url":"https://example.com","secret_ref":"hook"}`:    "/secret_ref",
		`{"auth":{"headers":[{"x":1},{"token_ref":"t"}]}}`:     "/auth/headers/1/token_ref",
		`{"a/b":{"c~d_ref":"t"}}`:                              "/a~1b/c~0d_ref",
		`["not an object"]`:                                    "",
		`not json`:                                             "",
		`{"refs":"x","ref":"y","_reference":"z","x":[[null]]}`: "",
	} {
		if got := secretRef([]byte(cfg)); got != want {
			t.Errorf("secretRef(%s) = %q, want %q", cfg, got, want)
		}
	}
}

// SR2 (#75 review): a sink that names a stored secret fails before Deliver,
// retried, until the dispatcher resolves *_ref keys (#108).
func TestSecretRefSinkFailsClosed(t *testing.T) {
	st, _ := newStore(t)
	addEvents(t, st, 1)
	ts := &testSink{}
	d := newDispatcher(t, st, registry(t, ts), Options{
		Clock: &fakeClock{now: t0},
		DecryptConfig: func([]byte) ([]byte, error) {
			return []byte(`{"url":"https://example.com/hook","secret_ref":"ci-webhook"}`), nil
		},
	})
	pollWait(t, d)
	if n := len(ts.requests()); n != 0 {
		t.Fatalf("Deliver was called %d times", n)
	}
	del := delivery(t, st, "E0000")
	if del.Status != store.DeliveryPending || !strings.Contains(del.LastError, "/secret_ref names a stored secret") ||
		!strings.Contains(del.LastError, "not resolved at delivery yet") || strings.Contains(del.LastError, "ci-webhook") {
		t.Errorf("delivery = %+v", del)
	}
}
