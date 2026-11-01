package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommandPrintsBuildInfo(t *testing.T) {
	var out bytes.Buffer
	root := newRootCmd()
	root.SetOut(&out)
	root.SetArgs([]string{"version"})

	if err := root.Execute(); err != nil {
		t.Fatalf("version failed: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "angelia ") || !strings.Contains(got, "(commit ") || !strings.Contains(got, ", built ") {
		t.Fatalf("unexpected output %q", got)
	}
}

func TestVersionRejectsArgs(t *testing.T) {
	root := newRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	root.SetArgs([]string{"version", "extra"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected an error for extra arguments")
	}
}
