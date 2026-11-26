package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
)

// The notes step of PyPI and npm (ADR 0045) is on for the public GitHub API
// and off, with one Info log, for any other sources.github-release.base_url:
// a GitHub Enterprise token is never sent to api.github.com.
func TestNewNotesFillerBaseURLRule(t *testing.T) {
	for name, tc := range map[string]struct {
		cfg *config.Config
		on  bool
	}{
		"no config":        {&config.Config{}, true},
		"explicit default": {&config.Config{Sources: map[string]config.Source{"github-release": {BaseURL: "https://api.github.com/"}}}, true},
		"upper case":       {&config.Config{Sources: map[string]config.Source{"github-release": {BaseURL: "https://API.github.com"}}}, true},
		"enterprise":       {&config.Config{Sources: map[string]config.Source{"github-release": {BaseURL: "https://ghe.example.org/api/v3"}}}, false},
		"mirror":           {&config.Config{Sources: map[string]config.Source{"github-release": {BaseURL: "https://github-mirror.example.org"}}}, false},
		"other source":     {&config.Config{Sources: map[string]config.Source{"github-tag": {BaseURL: "https://ghe.example.org/api/v3"}}}, true},
	} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			f := newNotesFiller(tc.cfg, &http.Client{}, nil, slog.New(slog.NewTextHandler(&logs, nil)), nil)
			if (f != nil) != tc.on {
				t.Fatalf("filler = %v, want on = %v", f, tc.on)
			}
			if tc.on && logs.Len() != 0 {
				t.Errorf("unexpected log: %s", logs.String())
			}
			if !tc.on && (strings.Count(logs.String(), "level=INFO") != 1 || !strings.Contains(logs.String(), "release notes for pypi and npm are off")) {
				t.Errorf("want one Info log saying why:\n%s", logs.String())
			}
		})
	}
}
