package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink/webhook"
)

// ADR 0023 decided question 6: `angelia webhook secret new` prints a fresh
// secret that the sender accepts.
func TestWebhookSecretNew(t *testing.T) {
	seen := map[string]bool{}
	for range 3 {
		var out, errOut bytes.Buffer
		root := newRootCmd()
		root.SetOut(&out)
		root.SetErr(&errOut)
		root.SetArgs([]string{"webhook", "secret", "new"})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		s := strings.TrimSuffix(out.String(), "\n")
		if strings.ContainsAny(s, " \n") || !strings.HasPrefix(s, "whsec_") || errOut.Len() != 0 {
			t.Fatalf("output %q, stderr %q", out.String(), errOut.String())
		}
		keys, err := webhook.ParseSecrets(s)
		if err != nil || len(keys) != 1 || len(keys[0]) != 32 {
			t.Fatalf("generated secret is not accepted: %v", err)
		}
		if seen[s] {
			t.Fatal("the same secret twice")
		}
		seen[s] = true
	}
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"webhook", "secret", "new", "extra"})
	if err := root.Execute(); err == nil {
		t.Fatal("extra argument accepted")
	}
}

// ADR 0023 decided questions 3, 4 and 5: `config check` accepts http:// for
// a host of http.allow_http_hosts, and warns about an unsigned sink and a
// timeout above the dispatcher's deliver timeout.
func TestConfigCheckWebhookWarnings(t *testing.T) {
	cfg := writeCfg(t, "plugins_dir: "+t.TempDir()+"\nhttp:\n  allow_http_hosts: [hooks.lan]\n")
	watches := writeCfg(t, "version: 1\nsinks:\n"+
		"  - {name: lan, kind: webhook, config: {url: 'http://hooks.lan/x'}}\n"+
		"  - {name: slow, kind: webhook, config: {url: 'https://hooks.example.org/x', secret_env: PATH, timeout: 1m}}\n")
	out, errb, err := runCheck(t, "--config", cfg, "--watches", watches)
	if err != nil || !strings.Contains(out, "watch file OK: 2 sinks") {
		t.Fatalf("out=%q err=%q %v", out, errb, err)
	}
	if !strings.Contains(errb, "the webhook is delivered unsigned") || !strings.Contains(errb, "above the dispatcher's deliver timeout (15s)") {
		t.Fatalf("stderr %q, want both warnings", errb)
	}
	// Without the allowlist, http:// is refused.
	if _, _, err := runCheck(t, "--config", writeCfg(t, "plugins_dir: "+t.TempDir()+"\n"), "--watches", watches); err == nil {
		t.Fatal("http:// accepted without http.allow_http_hosts")
	}
}
