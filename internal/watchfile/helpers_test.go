package watchfile

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

func registry(t testing.TB) *source.Registry {
	t.Helper()
	hc := &http.Client{}
	r := source.NewRegistry()
	for _, s := range []source.Source{pypi.New(pypi.DefaultBaseURL, hc), npm.New("", hc), githubrelease.New(githubrelease.DefaultBaseURL, hc)} {
		if err := r.Register(s); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func opts(t testing.TB) Options {
	return Options{Sources: registry(t), LookupEnv: func(string) (string, bool) { return "set", true }}
}

func openStore(t testing.TB) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "a.db"), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seqIDs returns a deterministic id source.
func seqIDs() func() string {
	n := 0
	return func() string { n++; return fmt.Sprintf("id%03d", n) }
}

var clock = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func applyOpts() ApplyOptions {
	return ApplyOptions{Now: func() time.Time { return clock }, NewID: seqIDs()}
}

func mustParse(t testing.TB, doc string) *File {
	t.Helper()
	f, err := Parse([]byte(doc), "w.yaml", opts(t))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f
}

func mustApply(t testing.TB, st store.Store, doc string) *Plan {
	t.Helper()
	p, err := Apply(t.Context(), st, mustParse(t, doc), ApplyOptions{Now: func() time.Time { return clock }})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	return p
}
