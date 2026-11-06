package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The dispatcher checks a bound secret against the URL a kind marks
// x-angelia-destination (ADR 0024 §2): the spec's webhook url, or the key a
// plugin's config_schema marks, at any depth.
func TestSinkDestinations(t *testing.T) {
	got, err := SinkDestinations([]PluginInfo{
		{Kind: "pypi", Interface: "source", Runtime: "builtin"},
		{Kind: "webhook", Interface: "sink", Runtime: "builtin"},
		{Kind: "chat", Interface: "sink", Runtime: "wasm", ConfigSchema: json.RawMessage(
			`{"type":"object","properties":{"name":{"type":"string"},"target":{"type":"object","properties":{"a/b":{"type":"string","x-angelia-destination":true}}}}}`)},
		{Kind: "open", Interface: "sink", Runtime: "wasm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["webhook"] != "/url" || got["chat"] != "/target/a~1b" {
		t.Errorf("SinkDestinations = %v", got)
	}
	_, err = SinkDestinations([]PluginInfo{{Kind: "two", Interface: "sink", Runtime: "wasm", ConfigSchema: json.RawMessage(
		`{"properties":{"a":{"x-angelia-destination":true},"b":{"x-angelia-destination":true}}}`)}})
	if err == nil || !strings.Contains(err.Error(), "sink kind two marks more than one key x-angelia-destination: /a, /b") {
		t.Errorf("two destinations: %v", err)
	}
}
