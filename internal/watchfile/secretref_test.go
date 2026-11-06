package watchfile

import (
	"strings"
	"testing"
)

// ADR 0024 §7: the watch file names stored secrets with *_ref keys; values
// stay refused.
func TestWebhookSecretRef(t *testing.T) {
	f := mustParse(t, base+"sinks:\n  - {name: s, kind: webhook, config: {url: 'https://h.example.org/x', secret_ref: ci-webhook-signing}}\n")
	if got := string(f.Sinks[0].Config); got != `{"url":"https://h.example.org/x","secret_ref":"ci-webhook-signing","timeout":"10s"}` {
		t.Fatalf("config: %s", got)
	}
	if len(f.Warnings) != 0 {
		t.Fatalf("a signed webhook warns: %q", f.Warnings)
	}
	for doc, want := range map[string]string{ //nolint:gosec // synthetic values and names
		"secret_ref: 'Not a name'":              "secret_ref: must be the name of a stored secret",
		"secret_ref: whsec_abcdefabcdef":        "looks like a credential",
		"secret_ref: a, secret_env: CI_WEBHOOK": "set at most one of secret_env and secret_ref",
		"secret_ref: 12":                        "must be a string",
	} {
		_, err := Parse([]byte(base+"sinks:\n  - {name: s, kind: webhook, config: {url: 'https://h.example.org/x', "+doc+"}}\n"), "w.yaml", opts(t))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", doc, err)
			continue
		}
		if strings.Contains(err.Error(), "whsec_") {
			t.Errorf("%s: error echoes the value: %v", doc, err)
		}
	}
}
