package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// gzLines gzips one JSON value per line.
func gzLines(t *testing.T, lines ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, l := range lines {
		var b []byte
		if s, ok := l.(string); ok {
			b = []byte(s)
		} else {
			var err error
			if b, err = json.Marshal(l); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := gz.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeSnapshot writes a synthetic snapshot for registry and returns its path.
func writeSnapshot(t *testing.T, registry string, pkgs ...Package) string {
	t.Helper()
	lines := []any{map[string]Meta{"meta": {Registry: registry, Oracle: "test", Generated: "2026-11-01", Packages: len(pkgs)}}}
	for _, p := range pkgs {
		lines = append(lines, p)
	}
	path := filepath.Join(t.TempDir(), registry+".jsonl.gz")
	if err := os.WriteFile(path, gzLines(t, lines...), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeJSON writes v as JSON to a temporary file and returns its path.
func writeJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// groups builds a Package order where every version is its own group.
func groups(vs ...string) [][]string {
	out := make([][]string, len(vs))
	for i, v := range vs {
		out[i] = []string{v}
	}
	return out
}
