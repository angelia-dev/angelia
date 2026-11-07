package source_test

import (
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

func TestRegistry(t *testing.T) {
	r := source.NewRegistry()
	if got := r.Kinds(); len(got) != 0 {
		t.Fatalf("new registry kinds = %v, want none", got)
	}
	b, a := sourcetest.NewFake("b"), sourcetest.NewFake("a")
	for _, s := range []source.Source{b, a} {
		if err := r.Register(s); err != nil {
			t.Fatalf("Register(%s): %v", s.Kind(), err)
		}
	}
	got, err := r.Get("a")
	if err != nil || got != source.Source(a) {
		t.Fatalf("Get(a) = %v, %v", got, err)
	}
	if want := []string{"a", "b"}; !slices.Equal(r.Kinds(), want) {
		t.Fatalf("Kinds() = %v, want %v", r.Kinds(), want)
	}
}

func TestRegistryErrors(t *testing.T) {
	r := source.NewRegistry()
	if err := r.Register(sourcetest.NewFake("x")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(sourcetest.NewFake("x")); !errors.Is(err, source.ErrDuplicateKind) {
		t.Errorf("duplicate: err = %v, want ErrDuplicateKind", err)
	}
	if _, err := r.Get("nope"); !errors.Is(err, source.ErrUnknownKind) {
		t.Errorf("unknown: err = %v, want ErrUnknownKind", err)
	}
	if err := r.Register(nil); err == nil {
		t.Error("nil source accepted")
	}
	if err := r.Register(sourcetest.NewFake("")); err == nil {
		t.Error("empty kind accepted")
	}
}

func TestRegistryTypedNilAndZeroValue(t *testing.T) {
	var r source.Registry // zero value must work
	var typedNil *sourcetest.Fake
	if err := r.Register(typedNil); err == nil {
		t.Error("typed-nil source accepted")
	}
	if _, err := r.Get("x"); !errors.Is(err, source.ErrUnknownKind) {
		t.Errorf("Get on zero registry: %v", err)
	}
	if err := r.Register(sourcetest.NewFake("x")); err != nil {
		t.Fatalf("Register on zero registry: %v", err)
	}
	if !slices.Equal(r.Kinds(), []string{"x"}) {
		t.Errorf("Kinds() = %v", r.Kinds())
	}
}

func TestRegistryConcurrent(t *testing.T) {
	r := source.NewRegistry()
	var wg sync.WaitGroup
	for _, k := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = r.Register(sourcetest.NewFake(k))
			_, _ = r.Get(k)
			_ = r.Kinds()
		}()
	}
	wg.Wait()
	if len(r.Kinds()) != 4 {
		t.Fatalf("Kinds() = %v", r.Kinds())
	}
}

func TestFetchStateIsZero(t *testing.T) {
	if !(source.FetchState{}).IsZero() {
		t.Error("zero state not zero")
	}
	if (source.FetchState{ETag: "x"}).IsZero() {
		t.Error("non-zero state reported zero")
	}
}

func TestRegistryInstances(t *testing.T) {
	r := source.NewRegistry()
	pub, internal := sourcetest.NewFake("npm"), sourcetest.NewFake("npm")
	if err := r.Register(pub); err != nil {
		t.Fatal(err)
	}
	if err := r.RegisterAs("npm-internal", internal); err != nil {
		t.Fatal(err)
	}
	if got, err := r.Get("npm-internal"); err != nil || got != source.Source(internal) {
		t.Fatalf("Get(npm-internal) = %v, %v", got, err)
	}
	if want := []string{"npm", "npm-internal"}; !slices.Equal(r.Names(), want) {
		t.Errorf("Names() = %v, want %v", r.Names(), want)
	}
	if want := []string{"npm"}; !slices.Equal(r.Kinds(), want) {
		t.Errorf("Kinds() = %v, want the one base kind %v", r.Kinds(), want)
	}
	if got := r.KindOf("npm-internal"); got != "npm" {
		t.Errorf("KindOf(npm-internal) = %q, want npm", got)
	}
	if got := r.KindOf("unknown"); got != "unknown" {
		t.Errorf("KindOf(unknown) = %q, want the name back", got)
	}
	if err := r.RegisterAs("npm-internal", sourcetest.NewFake("npm")); !errors.Is(err, source.ErrDuplicateKind) {
		t.Errorf("duplicate instance: err = %v, want ErrDuplicateKind", err)
	}
	if err := r.RegisterAs("", sourcetest.NewFake("npm")); err == nil {
		t.Error("empty name accepted")
	}
	if err := r.RegisterAs("x", nil); err == nil {
		t.Error("nil source accepted")
	}
}
