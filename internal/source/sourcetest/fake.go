package sourcetest

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
)

// Fake is an in-memory source.Source for tests of the core and of the
// contract suite itself. It serves one version list per package name
// (coordinate "name") and supports conditional requests through a
// per-package ETag. It is safe for concurrent use; List returns deep copies.
type Fake struct {
	kind string

	mu       sync.RWMutex
	versions map[string][]source.Version
	revs     map[string]int
}

// NewFake returns a Fake of the given kind with no packages.
func NewFake(kind string) *Fake {
	return &Fake{kind: kind, versions: make(map[string][]source.Version), revs: make(map[string]int)}
}

// Set replaces the versions served for name and bumps that package's ETag.
func (f *Fake) Set(name string, versions ...source.Version) {
	cp := make([]source.Version, len(versions))
	for i, v := range versions {
		cp[i] = cloneVersion(v)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.versions[name] = cp
	f.revs[name]++
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
	name := t.Coordinates["name"]
	f.mu.RLock()
	defer f.mu.RUnlock()
	vs, ok := f.versions[name]
	if !ok {
		return source.Result{}, fmt.Errorf("list %s: package %q not found", f.kind, name)
	}
	state := source.FetchState{ETag: `"` + strconv.Itoa(f.revs[name]) + `"`}
	if s.ETag == state.ETag {
		return source.Result{State: state, NotModified: true}, nil
	}
	out := make([]source.Version, len(vs))
	for i, v := range vs {
		out[i] = cloneVersion(v)
	}
	return source.Result{Versions: out, State: state}, nil
}

func cloneVersion(v source.Version) source.Version {
	if v.Prerelease != nil {
		p := *v.Prerelease
		v.Prerelease = &p
	}
	v.Metadata = cloneMap(v.Metadata)
	return v
}

func cloneMap(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneAny(v)
	}
	return out
}

func cloneAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneAny(e)
		}
		return out
	default:
		return v
	}
}

// Ptr returns a pointer to v, for Version.Prerelease literals.
func Ptr[T any](v T) *T { return &v }

// At returns a fixed UTC time n days after 2024-01-01, for fixtures.
func At(n int) time.Time {
	return time.Date(2024, 1, 1+n, 0, 0, 0, 0, time.UTC)
}
