package source

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
)

// Registry maps a kind to its Source. It is an explicit value, not a package
// global: the composition root builds one and passes it down.
type Registry struct {
	mu      sync.RWMutex
	sources map[string]Source
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{sources: make(map[string]Source)}
}

// Register adds src under src.Kind(). It fails with ErrDuplicateKind if the
// kind is taken, and rejects a nil source or an empty kind.
func (r *Registry) Register(src Source) error {
	if isNil(src) {
		return errors.New("register source: nil source")
	}
	kind := src.Kind()
	if kind == "" {
		return errors.New("register source: empty kind")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[kind]; ok {
		return fmt.Errorf("register source %q: %w", kind, ErrDuplicateKind)
	}
	if r.sources == nil { // zero-value Registry
		r.sources = make(map[string]Source)
	}
	r.sources[kind] = src
	return nil
}

// isNil reports whether src is nil or an interface holding a nil pointer,
// map, slice, func or chan.
func isNil(src Source) bool {
	if src == nil {
		return true
	}
	v := reflect.ValueOf(src)
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// Get returns the Source for kind, or an error wrapping ErrUnknownKind.
func (r *Registry) Get(kind string) (Source, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src, ok := r.sources[kind]
	if !ok {
		return nil, fmt.Errorf("get source %q: %w", kind, ErrUnknownKind)
	}
	return src, nil
}

// Kinds returns the registered kinds, sorted.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	kinds := make([]string, 0, len(r.sources))
	for k := range r.sources {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	return kinds
}
