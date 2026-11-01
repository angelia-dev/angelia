package sourcetest

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
)

// Fake is an in-memory source.Source for tests of the core and of the
// contract suite itself. It serves one fixed version list per package name
// (coordinate "name") and supports conditional requests through an ETag.
// It is not safe for concurrent mutation.
type Fake struct {
	kind     string
	versions map[string][]source.Version
	rev      int
}

// NewFake returns a Fake of the given kind with no packages.
func NewFake(kind string) *Fake {
	return &Fake{kind: kind, versions: make(map[string][]source.Version)}
}

// Set replaces the versions served for name and bumps the ETag.
func (f *Fake) Set(name string, versions ...source.Version) {
	f.versions[name] = versions
	f.rev++
}

// Kind implements source.Source.
func (f *Fake) Kind() string { return f.kind }

// Validate implements source.Source: the kind must match and the "name"
// coordinate must be a non-empty string without spaces or slashes.
func (f *Fake) Validate(t source.Target) error {
	if t.Kind != f.kind {
		return fmt.Errorf("kind %q, want %q: %w", t.Kind, f.kind, source.ErrInvalidTarget)
	}
	name := t.Coordinates["name"]
	if name == "" {
		return fmt.Errorf("missing name: %w", source.ErrInvalidTarget)
	}
	for _, r := range name {
		if r == ' ' || r == '/' {
			return fmt.Errorf("bad name %q: %w", name, source.ErrInvalidTarget)
		}
	}
	return nil
}

// List implements source.Source.
func (f *Fake) List(ctx context.Context, t source.Target, s source.FetchState) (source.Result, error) {
	if err := ctx.Err(); err != nil {
		return source.Result{}, fmt.Errorf("list %s: %w", f.kind, err)
	}
	if err := f.Validate(t); err != nil {
		return source.Result{}, fmt.Errorf("list %s: %w", f.kind, err)
	}
	vs, ok := f.versions[t.Coordinates["name"]]
	if !ok {
		return source.Result{}, fmt.Errorf("list %s: package %q not found", f.kind, t.Coordinates["name"])
	}
	state := source.FetchState{ETag: `"` + strconv.Itoa(f.rev) + `"`}
	if s.ETag == state.ETag {
		return source.Result{State: state, NotModified: true}, nil
	}
	return source.Result{Versions: append([]source.Version(nil), vs...), State: state}, nil
}

// Ptr returns a pointer to v, for Version.Prerelease literals.
func Ptr[T any](v T) *T { return &v }

// At returns a fixed UTC time n days after 2024-01-01, for fixtures.
func At(n int) time.Time {
	return time.Date(2024, 1, 1+n, 0, 0, 0, 0, time.UTC)
}
