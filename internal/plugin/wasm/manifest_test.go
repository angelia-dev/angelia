package wasm

import (
	"errors"
	"slices"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
)

func TestParseManifest(t *testing.T) {
	m, err := ParseManifest([]byte(`{"kind":"crates","allowed_hosts":["Crates.IO.","2001:db8::1","192.0.2.1"],
		"config_schema":{"type":"object","properties":{"token":{"type":"string"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	keys, _ := m.configKeys()
	if m.Kind != "crates" || !keys["token"] || len(keys) != 1 {
		t.Errorf("manifest = %+v, keys %v", m, keys)
	}
	if n, _ := normAllowedHost("Crates.IO."); n != "crates.io" {
		t.Errorf("normAllowedHost = %q", n)
	}

	bad := map[string]string{
		"not json":           `{`,
		"unknown field":      `{"kind":"x","allowed_hosts":[],"allowed_paths":{"/":"/"}}`,
		"empty kind":         `{"kind":"","allowed_hosts":[]}`,
		"upper kind":         `{"kind":"Crates","allowed_hosts":[]}`,
		"scheme":             `{"kind":"x","allowed_hosts":["https://crates.io"]}`,
		"port":               `{"kind":"x","allowed_hosts":["crates.io:443"]}`,
		"path":               `{"kind":"x","allowed_hosts":["crates.io/api"]}`,
		"wildcard":           `{"kind":"x","allowed_hosts":["*.crates.io"]}`,
		"star":               `{"kind":"x","allowed_hosts":["*"]}`,
		"empty host":         `{"kind":"x","allowed_hosts":[""]}`,
		"empty label":        `{"kind":"x","allowed_hosts":["a..b"]}`,
		"userinfo":           `{"kind":"x","allowed_hosts":["u@crates.io"]}`,
		"zoned ip":           `{"kind":"x","allowed_hosts":["fe80::1%eth0"]}`,
		"schema not object":  `{"kind":"x","allowed_hosts":[],"config_schema":[1]}`,
		"schema type string": `{"kind":"x","allowed_hosts":[],"config_schema":{"type":"string"}}`,
	}
	for name, in := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseManifest([]byte(in)); !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("err = %v, want ErrInvalidManifest", err)
			}
		})
	}
}

func TestToResult(t *testing.T) {
	in := source.FetchState{ETag: `"a"`}
	res, err := toResult([]byte(`{"not_modified":true}`), in)
	if err != nil || !res.NotModified || res.State != in {
		t.Errorf("NotModified without state = %+v, %v; want the incoming state kept", res, err)
	}
	res, err = toResult([]byte(`{"versions":[{"raw":"1.0.0","published_at":"2024-01-02T03:04:05.5+02:00","prerelease":true,"yanked":true,"metadata":{"k":1}}],"state":{"cursor":"c"}}`), in)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Versions[0]
	if v.PublishedAt.Location().String() != "UTC" || v.PublishedAt.Hour() != 1 || v.Prerelease == nil || !*v.Prerelease || !v.Yanked || res.State.Cursor != "c" {
		t.Errorf("result = %+v", res)
	}
	for name, out := range map[string]string{
		"not json":                   `nope`,
		"not_modified with versions": `{"not_modified":true,"versions":[{"raw":"1"}]}`,
		"empty raw":                  `{"versions":[{"raw":""}]}`,
		"bad date":                   `{"versions":[{"raw":"1","published_at":"x"}]}`,
	} {
		if _, err := toResult([]byte(out), in); !errors.Is(err, errPluginOutput) {
			t.Errorf("%s: err = %v, want errPluginOutput", name, err)
		}
	}
}

func TestHelpers(t *testing.T) {
	if got := truncate("héllo", 2); got != "h…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("truncate = %q", got)
	}
	if !validHeader("X-Token", "a\tb") || validHeader("", "x") || validHeader("Bad Name", "x") || validHeader("X", "a\rb") || validHeader("X", "a\x7f") {
		t.Error("validHeader")
	}
	if got := sortedKeys(map[string]bool{"b": true, "a": true}); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("sortedKeys = %v", got)
	}
}
