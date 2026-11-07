package main

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// ADR 0026 decided questions 4 and 7: config check warns about a url_ref
// whose secret has no hosts and about a plain-http origin (and whether
// allow_http_hosts lets it through), and about a URL deliveries would
// refuse. It never prints the URL.
func TestShoutrrrConfigCheckWarnings(t *testing.T) {
	const pass = "NTFYSECRETPASS0123"
	cfgPath, db := instanceFiles(t, "secrets:\n  key_file: "+writeKeyFile(t, 1)+"\nhttp:\n  allow_http_hosts: [lan.example]\n")
	st, err := sqlite.Open(t.Context(), db, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	keys := keyring(t, 1)
	at := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	for i, s := range []struct {
		name, value string
		hosts       []string
	}{
		{"bound", "ntfy://u:" + pass + "@ntfy.example/releases", []string{"ntfy.example"}},
		{"unbound", "ntfy://u:" + pass + "@ntfy.example/releases", nil},
		{"lan", "ntfy://u:" + pass + "@lan.example/releases?disabletls=yes", []string{"lan.example:80"}},
		{"cleartext", "gotify://gotify.example/AGOTIFYSECRET01?disabletls=yes", []string{"gotify.example:80"}},
		{"smtp", "smtp://u:" + pass + "@mail.example/?to=a@b.example", []string{"mail.example"}},
	} {
		id := "01SHOUT" + strings.Repeat("0", 18) + string(rune('A'+i))
		env, kid, err := keys.Seal(id, s.name, []byte(s.value))
		if err != nil {
			t.Fatal(err)
		}
		row := store.Secret{ID: id, Name: s.name, Hosts: s.hosts, KeyID: kid, Ciphertext: env, CreatedAt: at, UpdatedAt: at}
		if err := st.InSecretTx(t.Context(), func(tx store.SecretTx) error { return tx.CreateSecret(t.Context(), row) }); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	cfg, err := config.Load(t.Context(), config.Options{Path: cfgPath})
	if err != nil {
		t.Fatal(err)
	}
	var sinks []watchfile.Sink
	for _, c := range []string{"bound", "unbound", "lan", "cleartext", "smtp", "missing"} {
		sinks = append(sinks, watchfile.Sink{Name: c, Kind: "shoutrrr", Config: []byte(`{"url_ref":"` + c + `"}`)})
	}
	sinks = append(sinks,
		watchfile.Sink{Name: "env", Kind: "shoutrrr", Config: []byte(`{"url_env":"CHAT_URL"}`)},
		watchfile.Sink{Name: "unset", Kind: "shoutrrr", Config: []byte(`{"url_env":"UNSET_URL"}`)},
		watchfile.Sink{Name: "hook", Kind: "webhook", Config: []byte(`{"url":"https://h.example/"}`)})
	env := map[string]string{"CHAT_URL": "slack://hook:AAAAAAAAA-BBBBBBBBB-123456789123456789123456@webhook?skiptlsverify=yes"}
	got := strings.Join(shoutrrrWarnings(t.Context(), cfg, sinks, func(k string) (string, bool) { v, ok := env[k]; return v, ok }), "\n")
	for _, want := range []string{
		"sinks unbound: url_ref unbound: the stored secret has no hosts",
		"sinks lan: url_ref lan: http://lan.example is plain http (disabletls): the service's credentials travel in clear text",
		"sinks cleartext: url_ref cleartext: http://gotify.example is plain http (disabletls) and its host is not in http.allow_http_hosts: deliveries fail",
		`sinks smtp: url_ref smtp: deliveries fail until the URL is fixed: shoutrrr: service "smtp" is not allowed`,
		"sinks missing: url_ref missing: deliveries fail until this is fixed: secrets: no such secret",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("warnings lack %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"sinks bound:", "sinks hook:", "sinks unset:", pass, "AGOTIFYSECRET01", "123456789123456789123456"} {
		if strings.Contains(got, not) {
			t.Errorf("warnings hold %q:\n%s", not, got)
		}
	}
	// slack has no skiptlsverify option: the library refuses the key.
	if !strings.Contains(got, "sinks env: url_env CHAT_URL: deliveries fail until the URL is fixed: shoutrrr: slack: the service URL is invalid") {
		t.Errorf("env sink not checked:\n%s", got)
	}
}
