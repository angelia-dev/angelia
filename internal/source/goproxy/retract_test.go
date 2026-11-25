package goproxy_test

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

const (
	dbMod    = "example.test/db/sqlite"
	dbPrefix = "/example.test/db/sqlite/@v/"
	dbLatest = "/example.test/db/sqlite/@latest"
)

// dbProxy serves a module modelled on a measured case: accidental
// "v2.0.x+incompatible" tags that are the highest SemVer versions, retracted
// by the go.mod of the real latest, v1.14.53.
func dbProxy(t testing.TB) *proxy {
	return &proxy{
		codes: map[string]int{},
		docs: map[string]string{
			dbPrefix + "list":         string(fixture(t, "sqlite.list")),
			dbLatest:                  `{"Version":"v1.14.53","Time":"2026-05-01T00:00:00Z"}`,
			dbPrefix + "v1.14.53.mod": string(fixture(t, "sqlite-v1.14.53.mod")),
		},
	}
}

func dbTarget() source.Target { return target(dbMod) }

func versionsByRaw(vs []source.Version) map[string]source.Version {
	m := map[string]source.Version{}
	for _, v := range vs {
		m[v.Raw] = v
	}
	return m
}

func TestRetractionsAndDeprecation(t *testing.T) {
	p := dbProxy(t)
	srv := httptest.NewServer(p)
	defer srv.Close()
	res, err := newSource(t, srv, "", nil).List(t.Context(), dbTarget(), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	by := versionsByRaw(res.Versions)
	yanked := map[string]bool{
		"v2.0.0+incompatible": true, "v2.0.1+incompatible": true, "v2.0.2+incompatible": true, "v2.0.3+incompatible": true,
		"v1.14.50": true, "v1.14.51": true, "v1.14.49": true,
	}
	for raw, v := range by {
		if v.Yanked != yanked[raw] {
			t.Errorf("%s: Yanked = %v", raw, v.Yanked)
		}
		if _, ok := v.Metadata["retracted"]; ok != yanked[raw] {
			t.Errorf("%s: Metadata[retracted] present = %v", raw, ok)
		}
		if got, _ := v.Metadata["deprecated"].(string); got != "use example.test/db/sqlite/v3 instead." {
			t.Errorf("%s: deprecated = %q", raw, got)
		}
	}
	for raw, want := range map[string]string{
		"v2.0.2+incompatible": "Accidental tags, never a release.",
		"v1.14.50":            `Broken "build"`,
		"v1.14.51":            "",
		"v1.14.49":            "quoted string",
	} {
		if got := by[raw].Metadata["retracted"]; got != want {
			t.Errorf("%s: retracted = %q, want %q", raw, got, want)
		}
	}
	// The scheme's highest non-yanked version is the v1 one, not v2.0.3.
	sch := semver.Scheme{Coerce: true}
	var best string
	for _, v := range res.Versions {
		if v.Yanked {
			continue
		}
		if best == "" {
			best = v.Raw
			continue
		}
		a, errA := sch.Parse(v.Raw)
		b, errB := sch.Parse(best)
		if errA != nil || errB != nil {
			t.Fatalf("parse %s, %s: %v, %v", v.Raw, best, errA, errB)
		}
		if sch.Compare(a, b) > 0 {
			best = v.Raw
		}
	}
	if best != "v1.14.53" {
		t.Errorf("highest non-yanked version = %q, want v1.14.53", best)
	}
}

// @latest on every check, the go.mod only when the latest version changes.
func TestGoModOnlyWhenLatestChanges(t *testing.T) {
	p := dbProxy(t)
	srv := httptest.NewServer(p)
	defer srv.Close()
	s := newSource(t, srv, "", nil)
	list, mod54 := dbPrefix+"list", dbPrefix+"v1.14.54.mod"

	res, err := s.List(t.Context(), dbTarget(), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{list, dbLatest, dbPrefix + "v1.14.53.mod"}; !slices.Equal(p.requests(), want) {
		t.Fatalf("first call: %v, want %v", p.requests(), want)
	}
	p.reset()
	res2, err := s.List(t.Context(), dbTarget(), res.State)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{list, dbLatest}; !slices.Equal(p.requests(), want) {
		t.Fatalf("unchanged call: %v, want %v", p.requests(), want)
	}
	if !versionsByRaw(res2.Versions)["v2.0.0+incompatible"].Yanked {
		t.Error("the retractions of the cursor were not applied")
	}
	// A new latest version: a new go.mod without the old retractions.
	p.set(list, string(fixture(t, "sqlite.list"))+"v1.14.54\n")
	p.set(dbLatest, `{"Version":"v1.14.54"}`)
	p.set(dbPrefix+"v1.14.54.info", `{"Version":"v1.14.54","Time":"2026-06-01T00:00:00Z"}`)
	p.set(mod54, "module example.test/db/sqlite\n\nretract v1.14.52\n")
	p.reset()
	res3, err := s.List(t.Context(), dbTarget(), res2.State)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{list, dbLatest, mod54, dbPrefix + "v1.14.54.info"}; !slices.Equal(p.requests(), want) {
		t.Fatalf("new latest: %v, want %v", p.requests(), want)
	}
	by := versionsByRaw(res3.Versions)
	if !by["v1.14.52"].Yanked || by["v2.0.0+incompatible"].Yanked || by["v1.14.54"].Yanked {
		t.Errorf("yanked: v1.14.52 %v, v2.0.0+incompatible %v, v1.14.54 %v; want true, false, false",
			by["v1.14.52"].Yanked, by["v2.0.0+incompatible"].Yanked, by["v1.14.54"].Yanked)
	}
	if _, ok := by["v1.14.54"].Metadata["deprecated"]; ok {
		t.Error("the deprecation of the old go.mod is still set")
	}
}

func TestLatestFallbacks(t *testing.T) {
	for name, tc := range map[string]struct {
		latest   func(p *proxy)
		wantMod  string
		wantWarn bool
	}{
		"404": {func(p *proxy) { p.fail(dbLatest, http.StatusNotFound) }, dbPrefix + "v1.14.53.mod", false},
		"410": {func(p *proxy) { p.fail(dbLatest, http.StatusGone) }, dbPrefix + "v1.14.53.mod", false},
		"pseudo-version": {func(p *proxy) {
			p.set(dbLatest, `{"Version":"v0.0.0-20240102030405-abcdef123456"}`)
		}, dbPrefix + "v1.14.53.mod", false},
		"not JSON": {func(p *proxy) { p.set(dbLatest, "<html>") }, dbPrefix + "v1.14.53.mod", false},
		"proxy answers an incompatible version": {func(p *proxy) {
			p.set(dbLatest, `{"Version":"v2.0.3+incompatible"}`)
		}, dbPrefix + "v2.0.3+incompatible.mod", false},
	} {
		t.Run(name, func(t *testing.T) {
			p := dbProxy(t)
			tc.latest(p)
			p.set(dbPrefix+"v2.0.3+incompatible.mod", "module example.test/db/sqlite\n")
			srv := httptest.NewServer(p)
			defer srv.Close()
			var buf bytes.Buffer
			res, err := newSource(t, srv, "", &buf).List(t.Context(), dbTarget(), source.FetchState{})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(p.requests(), tc.wantMod) {
				t.Errorf("requests = %v, want %s", p.requests(), tc.wantMod)
			}
			if strings.Contains(buf.String(), "level=WARN") != tc.wantWarn {
				t.Errorf("log = %q", buf.String())
			}
			if len(res.Versions) != 9 {
				t.Errorf("%d versions", len(res.Versions))
			}
		})
	}
	t.Run("only incompatible versions", func(t *testing.T) {
		p := dbProxy(t)
		p.set(dbPrefix+"list", "v2.0.0+incompatible\nv2.0.1+incompatible\n")
		p.fail(dbLatest, http.StatusNotFound)
		p.set(dbPrefix+"v2.0.1+incompatible.mod", "module example.test/db/sqlite\n")
		srv := httptest.NewServer(p)
		defer srv.Close()
		if _, err := newSource(t, srv, "", nil).List(t.Context(), dbTarget(), source.FetchState{}); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(p.requests(), dbPrefix+"v2.0.1+incompatible.mod") {
			t.Errorf("requests = %v", p.requests())
		}
	})
	t.Run("only prereleases", func(t *testing.T) {
		p := dbProxy(t)
		p.set(dbPrefix+"list", "v0.1.0-rc.2\nv0.1.0-rc.1\nv9.0.0+incompatible\n")
		p.fail(dbLatest, http.StatusNotFound)
		p.set(dbPrefix+"v0.1.0-rc.2.mod", "module example.test/db/sqlite\n")
		srv := httptest.NewServer(p)
		defer srv.Close()
		if _, err := newSource(t, srv, "", nil).List(t.Context(), dbTarget(), source.FetchState{}); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(p.requests(), dbPrefix+"v0.1.0-rc.2.mod") {
			t.Errorf("requests = %v", p.requests())
		}
	})
	t.Run("empty module", func(t *testing.T) {
		p := dbProxy(t)
		p.set(dbPrefix+"list", "")
		p.fail(dbLatest, http.StatusNotFound)
		srv := httptest.NewServer(p)
		defer srv.Close()
		if _, err := newSource(t, srv, "", nil).List(t.Context(), dbTarget(), source.FetchState{}); err != nil {
			t.Fatal(err)
		}
		if got := p.requests(); len(got) != 2 {
			t.Errorf("requests = %v, want the list and @latest", got)
		}
	})
}

// A go.mod that cannot be read keeps the retractions of the cursor, and is
// asked for again; one that cannot be parsed is not.
func TestGoModFailures(t *testing.T) {
	known := func(t *testing.T, p *proxy, srv *httptest.Server) source.FetchState {
		res, err := newSource(t, srv, "", nil).List(t.Context(), dbTarget(), source.FetchState{})
		if err != nil {
			t.Fatal(err)
		}
		return res.State
	}
	newLatest := func(p *proxy) {
		p.set(dbPrefix+"list", string(fixture(t, "sqlite.list"))+"v1.14.54\n")
		p.set(dbLatest, `{"Version":"v1.14.54"}`)
	}
	t.Run("5xx", func(t *testing.T) {
		p := dbProxy(t)
		srv := httptest.NewServer(p)
		defer srv.Close()
		st := known(t, p, srv)
		newLatest(p)
		p.fail(dbPrefix+"v1.14.54.mod", http.StatusBadGateway)
		var buf bytes.Buffer
		s := newSource(t, srv, "", &buf)
		res, err := s.List(t.Context(), dbTarget(), st)
		if err != nil {
			t.Fatal(err)
		}
		if !versionsByRaw(res.Versions)["v2.0.0+incompatible"].Yanked {
			t.Error("the previous retractions were dropped")
		}
		if !strings.Contains(buf.String(), "level=WARN") || strings.Contains(buf.String(), srv.URL) {
			t.Errorf("log = %q", buf.String())
		}
		// Asked for again on the next call, and then taken.
		p.mu.Lock()
		delete(p.codes, dbPrefix+"v1.14.54.mod")
		p.mu.Unlock()
		p.set(dbPrefix+"v1.14.54.mod", "module example.test/db/sqlite\n")
		p.reset()
		res, err = s.List(t.Context(), dbTarget(), res.State)
		if err != nil || !slices.Contains(p.requests(), dbPrefix+"v1.14.54.mod") {
			t.Fatalf("requests %v, err %v", p.requests(), err)
		}
		if versionsByRaw(res.Versions)["v2.0.0+incompatible"].Yanked {
			t.Error("the retractions of the new go.mod (none) were not taken")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		p := dbProxy(t)
		srv := httptest.NewServer(p)
		defer srv.Close()
		st := known(t, p, srv)
		newLatest(p)
		p.set(dbPrefix+"v1.14.54.mod", "module (\nretract [\n")
		var buf bytes.Buffer
		s := newSource(t, srv, "", &buf)
		res, err := s.List(t.Context(), dbTarget(), st)
		if err != nil {
			t.Fatal(err)
		}
		if !versionsByRaw(res.Versions)["v2.0.0+incompatible"].Yanked || !strings.Contains(buf.String(), "level=WARN") {
			t.Errorf("yanked %v, log %q", versionsByRaw(res.Versions)["v2.0.0+incompatible"].Yanked, buf.String())
		}
		p.reset()
		if _, err := s.List(t.Context(), dbTarget(), res.State); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(p.requests(), dbPrefix+"v1.14.54.mod") {
			t.Error("a malformed go.mod was asked for again")
		}
	})
	t.Run("@latest 5xx keeps the retractions", func(t *testing.T) {
		p := dbProxy(t)
		srv := httptest.NewServer(p)
		defer srv.Close()
		st := known(t, p, srv)
		p.fail(dbLatest, http.StatusInternalServerError)
		var buf bytes.Buffer
		res, err := newSource(t, srv, "", &buf).List(t.Context(), dbTarget(), st)
		if err != nil || !versionsByRaw(res.Versions)["v2.0.0+incompatible"].Yanked || !strings.Contains(buf.String(), "level=WARN") {
			t.Fatalf("err %v, log %q", err, buf.String())
		}
	})
	t.Run("429 fails the call", func(t *testing.T) {
		for _, path := range []string{dbLatest, dbPrefix + "v1.14.53.mod"} {
			p := dbProxy(t)
			p.fail(path, http.StatusTooManyRequests)
			srv := httptest.NewServer(p)
			_, err := newSource(t, srv, "", nil).List(t.Context(), dbTarget(), source.FetchState{})
			var se *netguard.StatusError
			if !errors.As(err, &se) || se.StatusCode != http.StatusTooManyRequests {
				t.Errorf("%s: err = %v, want a 429", path, err)
			}
			srv.Close()
		}
	})
}

func TestRationaleIsBounded(t *testing.T) {
	p := dbProxy(t)
	long := strings.Repeat("é", 200) // 400 bytes
	p.set(dbPrefix+"v1.14.53.mod", "module example.test/db/sqlite\n\nretract v1.14.50 // "+long+"\n")
	srv := httptest.NewServer(p)
	defer srv.Close()
	res, err := newSource(t, srv, "", nil).List(t.Context(), dbTarget(), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := versionsByRaw(res.Versions)["v1.14.50"].Metadata["retracted"].(string)
	if len(got) == 0 || len(got) > 256 || !strings.HasPrefix(long, got) {
		t.Errorf("rationale of %d bytes: %q", len(got), got)
	}
}
