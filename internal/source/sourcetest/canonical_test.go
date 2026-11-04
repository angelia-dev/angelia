package sourcetest

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
)

// canon is a Fake with a configurable Canonical.
type canon struct {
	*Fake
	canonical func(source.Target) (source.Target, error)
}

func (c canon) Canonical(t source.Target) (source.Target, error) { return c.canonical(t) }

// lower lower-cases the name, the well-behaved canonicalizer.
func lower(t source.Target) (source.Target, error) {
	coords := maps.Clone(t.Coordinates)
	coords["name"] = strings.ToLower(coords["name"])
	return source.Target{Kind: t.Kind, Coordinates: coords}, nil
}

func aliasFixtures() Fixtures {
	fx := fixtures()
	fx.Aliases = []source.Target{{Kind: "fake", Coordinates: map[string]string{"name": "PKG"}}}
	return fx
}

func TestCanonicalizerPassesContract(t *testing.T) {
	f := newFake()
	fx := aliasFixtures()
	Run(t, canon{Fake: f, canonical: lower}, fx)
}

func TestCanonicalSkippedWithoutCanonicalizer(t *testing.T) {
	if err := checkCanonical(context.Background(), newFake(), fixtures(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalDetectsViolations(t *testing.T) {
	f := newFake()
	tests := []struct {
		name string
		src  source.Source
		fx   func(*Fixtures)
	}{
		{"aliases without canonicalizer", f, nil},
		{"canonicalizer without aliases", canon{Fake: f, canonical: lower}, func(fx *Fixtures) { fx.Aliases = nil }},
		{"canonical error", canon{Fake: f, canonical: func(source.Target) (source.Target, error) { return source.Target{}, errBoom }}, nil},
		{"changes kind", canon{Fake: f, canonical: func(t source.Target) (source.Target, error) {
			t.Kind = "other"
			return t, nil
		}}, nil},
		{"result fails validate", canon{Fake: f, canonical: func(t source.Target) (source.Target, error) {
			return source.Target{Kind: t.Kind}, nil
		}}, nil},
		{"not idempotent", canon{Fake: f, canonical: func(t source.Target) (source.Target, error) {
			coords := maps.Clone(t.Coordinates)
			coords["name"] += "x"
			return source.Target{Kind: t.Kind, Coordinates: coords}, nil
		}}, nil},
		{"second pass error", canon{Fake: f, canonical: func(t source.Target) (source.Target, error) {
			if t.Coordinates["name"] == "done" {
				return source.Target{}, errBoom
			}
			return source.Target{Kind: t.Kind, Coordinates: map[string]string{"name": "done"}}, nil
		}}, nil},
		{"mutates argument", canon{Fake: f, canonical: func(t source.Target) (source.Target, error) {
			t.Coordinates["name"] = strings.ToLower(t.Coordinates["name"])
			return t, nil
		}}, nil},
		{"alias not coalesced", canon{Fake: f, canonical: func(t source.Target) (source.Target, error) { return t, nil }}, nil},
		{"alias rejected by validate", canon{Fake: f, canonical: lower}, func(fx *Fixtures) {
			fx.Aliases = []source.Target{{Kind: "fake", Coordinates: map[string]string{"name": "a b"}}}
		}},
		{"alias canonical error", canon{Fake: f, canonical: func(t source.Target) (source.Target, error) {
			if t.Coordinates["name"] == "PKG" {
				return source.Target{}, errBoom
			}
			return t, nil
		}}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := aliasFixtures()
			if tc.fx != nil {
				tc.fx(&fx)
			}
			if err := checkCanonical(context.Background(), tc.src, fx, nil); err == nil {
				t.Fatal("check passed, want a violation")
			}
		})
	}
}

func TestCanonicalRequestsDetected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	c := Client(t, srv)
	src := canon{Fake: newFake(), canonical: func(t source.Target) (source.Target, error) {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
		if err != nil {
			return t, err
		}
		res, err := c.Do(req)
		if err != nil {
			return t, err
		}
		_ = res.Body.Close()
		return lower(t)
	}}
	err := checkCanonical(context.Background(), src, aliasFixtures(), c)
	if err == nil || !strings.Contains(err.Error(), "no I/O") {
		t.Fatalf("err = %v, want a no-I/O violation", err)
	}
}
