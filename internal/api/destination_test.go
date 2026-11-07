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
		{Kind: "github-dispatch", Interface: "sink", Runtime: "builtin"},
		{Kind: "chat", Interface: "sink", Runtime: "wasm", ConfigSchema: json.RawMessage(
			`{"type":"object","properties":{"name":{"type":"string"},"target":{"type":"object","properties":{"a/b":{"type":"string","x-angelia-destination":true}}}}}`)},
		{Kind: "open", Interface: "sink", Runtime: "wasm"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got["webhook"] != "/url" || got["github-dispatch"] != "/api_base_url" || got["chat"] != "/target/a~1b" {
		t.Errorf("SinkDestinations = %v", got)
	}
	_, err = SinkDestinations([]PluginInfo{{Kind: "two", Interface: "sink", Runtime: "wasm", ConfigSchema: json.RawMessage(
		`{"properties":{"a":{"x-angelia-destination":true},"b":{"x-angelia-destination":true}}}`)}})
	if err == nil || !strings.Contains(err.Error(), "sink kind two marks more than one key x-angelia-destination: /a, /b") {
		t.Errorf("two destinations: %v", err)
	}
}

// ADR 0026 §1: the shoutrrr kind's url_ref and url_env (from the vendored
// spec) are secret destinations; a plugin can mark its own, only on a
// reference key, and not together with x-angelia-destination.
func TestSinkSecretDestinations(t *testing.T) {
	got, err := SinkSecretDestinations([]PluginInfo{
		{Kind: "webhook", Interface: "sink", Runtime: "builtin"},
		{Kind: "shoutrrr", Interface: "sink", Runtime: "builtin"},
		{Kind: "apprise", Interface: "sink", Runtime: "wasm", ConfigSchema: json.RawMessage(
			`{"properties":{"target":{"properties":{"url_env":{"type":"string","x-angelia-secret-destination":true}}}}}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || strings.Join(got["shoutrrr"], ",") != "/url_env,/url_ref" || strings.Join(got["apprise"], ",") != "/target/url_env" {
		t.Errorf("SinkSecretDestinations = %v", got)
	}
	for name, schema := range map[string]string{
		"not a reference": `{"properties":{"url":{"x-angelia-secret-destination":true}}}`,
		"both":            `{"properties":{"url_ref":{"x-angelia-secret-destination":true},"url":{"x-angelia-destination":true}}}`,
	} {
		_, err := SinkSecretDestinations([]PluginInfo{{Kind: "bad", Interface: "sink", Runtime: "wasm", ConfigSchema: json.RawMessage(schema)}})
		if err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
