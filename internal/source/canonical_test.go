package source_test

import (
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

// canon is a Fake whose Canonical is configurable.
type canon struct {
	*sourcetest.Fake
	fn func(source.Target) (source.Target, error)
}

func (c canon) Canonical(t source.Target) (source.Target, error) { return c.fn(t) }

func TestCanonicalWithoutCanonicalizerCopies(t *testing.T) {
	in := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "Pkg"}}
	got, err := source.Canonical(sourcetest.NewFake("fake"), in)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v, want %v unchanged", got, in)
	}
	got.Coordinates["name"] = "changed"
	if in.Coordinates["name"] != "Pkg" {
		t.Fatal("result shares its Coordinates map with the input")
	}
}

func TestCanonicalUsesCanonicalizer(t *testing.T) {
	var seen map[string]string
	src := canon{Fake: sourcetest.NewFake("fake"), fn: func(t source.Target) (source.Target, error) {
		seen = t.Coordinates
		t.Coordinates["name"] = strings.ToLower(t.Coordinates["name"]) // mutates its argument: must be a copy
		return t, nil
	}}
	in := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "Pkg", "other": "X"}}
	got, err := source.Canonical(src, in)
	if err != nil {
		t.Fatal(err)
	}
	want := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "pkg", "other": "X"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if in.Coordinates["name"] != "Pkg" {
		t.Fatal("the input was mutated")
	}
	seen["name"] = "later"
	if got.Coordinates["name"] != "pkg" {
		t.Fatal("result shares its Coordinates map with the source")
	}
}

func TestCanonicalErrors(t *testing.T) {
	in := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "pkg"}}
	failing := canon{Fake: sourcetest.NewFake("fake"), fn: func(source.Target) (source.Target, error) {
		return source.Target{}, source.ErrInvalidTarget
	}}
	if _, err := source.Canonical(failing, in); !errors.Is(err, source.ErrInvalidTarget) {
		t.Fatalf("err = %v, want it to wrap ErrInvalidTarget", err)
	}
	rekind := canon{Fake: sourcetest.NewFake("fake"), fn: func(t source.Target) (source.Target, error) {
		return source.Target{Kind: "other", Coordinates: maps.Clone(t.Coordinates)}, nil
	}}
	if _, err := source.Canonical(rekind, in); err == nil || !strings.Contains(err.Error(), "changed the kind") {
		t.Fatalf("err = %v, want a kind change error", err)
	}
}
