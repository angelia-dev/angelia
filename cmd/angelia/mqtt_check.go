package main

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/sink/mqtt"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// mqttWarnings warns about an mqtt sink whose broker host is an address
// literal the SSRF guard refuses under http.allow_networks: every delivery
// to it would fail (ADR 0039). A host name is not resolved here; a private
// one shows as a permanent delivery error naming the host.
func mqttWarnings(cfg *config.Config, sinks []watchfile.Sink) []string {
	guard := netguard.NewGuard(cfg.HTTP.AllowPrefixes(), nil)
	var out []string
	for _, s := range sinks {
		if s.Kind != mqtt.Kind {
			continue
		}
		var c mqtt.Config
		if json.Unmarshal(s.Config, &c) != nil {
			continue // the watch file parser reported it
		}
		u, err := url.Parse(c.URL)
		if err != nil {
			continue
		}
		addr, err := netip.ParseAddr(u.Hostname())
		if err != nil {
			continue
		}
		if guard.CheckAddr(addr.String(), addr) != nil {
			out = append(out, fmt.Sprintf("sink %q: the broker %s is a private or reserved address not covered by http.allow_networks; deliveries will be refused by the SSRF guard (docs/mqtt.md)", s.Name, addr))
		}
	}
	return out
}
