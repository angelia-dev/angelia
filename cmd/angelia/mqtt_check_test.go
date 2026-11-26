package main

import (
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// AC (#93, 93-D): config check warns about a broker that is a private
// address literal outside http.allow_networks.
func TestMQTTConfigCheckWarnings(t *testing.T) {
	sinks := []watchfile.Sink{
		{Name: "lan", Kind: "mqtt", Config: []byte(`{"url":"mqtts://192.168.1.10:8883","topic":"t"}`)},
		{Name: "allowed", Kind: "mqtt", Config: []byte(`{"url":"mqtt://10.0.0.5","topic":"t"}`)},
		{Name: "v6", Kind: "mqtt", Config: []byte(`{"url":"mqtts://[fd00::1]","topic":"t"}`)},
		{Name: "public", Kind: "mqtt", Config: []byte(`{"url":"mqtts://203.0.113.7","topic":"t"}`)},
		{Name: "named", Kind: "mqtt", Config: []byte(`{"url":"mqtts://mosquitto.lan","topic":"t"}`)},
		{Name: "hook", Kind: "webhook", Config: []byte(`{"url":"https://192.168.1.10/x"}`)},
		{Name: "broken", Kind: "mqtt", Config: []byte(`not json`)},
	}
	cfg := &config.Config{HTTP: config.HTTP{AllowNetworks: []string{"10.0.0.0/8"}}}
	got := mqttWarnings(cfg, sinks)
	if len(got) != 2 || !strings.Contains(got[0], `sink "lan"`) || !strings.Contains(got[1], `sink "v6"`) {
		t.Fatalf("warnings %q", got)
	}
	if !strings.Contains(got[0], "http.allow_networks") {
		t.Fatalf("warning %q does not name http.allow_networks", got[0])
	}
}
