package main

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// ui.enabled decides whether the api role serves the admin UI.
func TestAPIOptionsUI(t *testing.T) {
	sources, err := watchSources(&config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "a.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.DiscardHandler)
	if o := apiOptions(&config.Config{}, st, sources, nil, nil, 0, log); o.UI != nil {
		t.Error("UI set with ui.enabled false")
	}
	cfg := &config.Config{UI: config.UI{Enabled: true}}
	if o := apiOptions(cfg, st, sources, nil, nil, 0, log); o.UI == nil {
		t.Error("UI not set with ui.enabled true")
	}
}
