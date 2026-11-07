//go:build live

package shoutrrr

import (
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

// #120 criterion 3, live: a message to a local ntfy container through the
// netguard client.
//
//	docker run --rm -p 8080:80 binwiederhier/ntfy serve
//	ANGELIA_LIVE_NTFY=localhost:8080 go test -tags live -run LiveNtfy ./internal/sink/shoutrrr/
func TestLiveNtfy(t *testing.T) {
	host := os.Getenv("ANGELIA_LIVE_NTFY")
	if host == "" {
		t.Skip("ANGELIA_LIVE_NTFY (host:port of a local ntfy server) is not set")
	}
	topic := "angelia-live-" + time.Now().Format("150405")
	raw := "ntfy://" + host + "/" + topic + "?disabletls=yes&title=Angelia"
	nc, err := netguard.New(netguard.Config{Allow: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}})
	if err != nil {
		t.Fatal(err)
	}
	s := New(nc.HTTPClient(), Options{})
	origins, err := s.SecretDestinations(URLEnvPointer, secrets.NewValueString(raw))
	if err != nil {
		t.Fatal(err)
	}
	hp, _ := secrets.Destination(origins[0])
	req := sink.Request{
		Config: sink.Config(`{"url_env":"NTFY_URL"}`), Secrets: map[string]secrets.Value{URLEnvPointer: secrets.NewValueString(raw)},
		Destinations: []string{hp}, Event: sampleEvent(t, "dev.angelia.release.published.v1"),
	}
	if _, err := s.Deliver(t.Context(), req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	resp, err := http.Get((&url.URL{Scheme: "http", Host: host, Path: "/" + topic + "/json", RawQuery: "poll=1"}).String()) //nolint:noctx // a live test
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "released") {
		t.Errorf("ntfy has no message on %s: %s", topic, b)
	}
}
