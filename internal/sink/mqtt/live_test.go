//go:build live

package mqtt

import (
	"crypto/x509"
	"net/netip"
	"os"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
)

// #93 (93-D), live: publish with QoS 0 and 1 to a real broker through the
// netguard client. Not run in CI.
//
//	docker run --rm -p 1883:1883 eclipse-mosquitto:2 mosquitto -c /mosquitto-no-auth.conf
//	ANGELIA_LIVE_MQTT_URL=mqtt://localhost:1883 go test -tags live -run Live ./internal/sink/mqtt/
//
// ANGELIA_LIVE_MQTT_URL may list several brokers, comma-separated (one
// plain and one mqtts://, for example). ANGELIA_LIVE_MQTT_USERNAME and
// ANGELIA_LIVE_MQTT_PASSWORD add a password; ANGELIA_LIVE_MQTT_CA_FILE adds
// a private CA for mqtts://. Loopback and private networks are allowed, and
// plain mqtt:// is allowed for the listed hosts.
func TestLivePublish(t *testing.T) {
	raw := os.Getenv("ANGELIA_LIVE_MQTT_URL")
	if raw == "" {
		t.Skip("ANGELIA_LIVE_MQTT_URL (a broker such as mqtt://localhost:1883) is not set")
	}
	var allow []netip.Prefix
	for _, p := range []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"} {
		allow = append(allow, netip.MustParsePrefix(p))
	}
	nc, err := netguard.New(netguard.Config{Allow: allow})
	if err != nil {
		t.Fatal(err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if f := os.Getenv("ANGELIA_LIVE_MQTT_CA_FILE"); f != "" {
		pem, err := os.ReadFile(f) //nolint:gosec // G304: the tester names the file.
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			t.Fatalf("CA file %s: %v", f, err)
		}
	}
	urls := strings.Split(raw, ",")
	var hosts []string
	for _, u := range urls {
		if parsed, err := CheckURL(strings.TrimSpace(u)); err == nil {
			hosts = append(hosts, parsed.Hostname())
		}
	}
	s := New(nc, Options{RootCAs: roots, AllowPlaintextHosts: hosts})
	user, pass := os.Getenv("ANGELIA_LIVE_MQTT_USERNAME"), os.Getenv("ANGELIA_LIVE_MQTT_PASSWORD")
	for _, u := range urls {
		for _, qos := range []int{0, 1} {
			cfg := map[string]any{"url": strings.TrimSpace(u), "topic": "angelia/live/{{.data.source}}/{{.data.package}}", "qos": qos}
			var sec map[string]secrets.Value
			if user != "" {
				cfg["username"] = user
			}
			if pass != "" {
				cfg["password_env"] = "ANGELIA_LIVE_MQTT_PASSWORD"
				sec = map[string]secrets.Value{PasswordPointer: secrets.NewValueString(pass)}
			}
			if err := deliver(t, s, request(t, cfg, sec)); err != nil {
				t.Errorf("%s qos %d: %v", u, qos, err)
			}
		}
	}
}
