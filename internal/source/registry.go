package source

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
)

// Registry maps a name to its Source. The name of a built-in is its kind; a
// named instance (RegisterAs) has its own name and a Source of another kind,
// so that one instance can watch the same kind of registry at several URLs.
// It is an explicit value, not a package global: the composition root builds
// one and passes it down.
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
	return r.RegisterAs(src.Kind(), src)
}

// RegisterAs adds src under name, a named instance of src.Kind(): its
// targets are stored with name as their kind, and src still receives and
// reports the base kind. It fails with ErrDuplicateKind if the name is
// taken, and rejects a nil source, an empty name or an empty kind.
func (r *Registry) RegisterAs(name string, src Source) error {
	if isNil(src) {
		return errors.New("register source: nil source")
	}
	if src.Kind() == "" {
		return errors.New("register source: empty kind")
	}
	if name == "" {
		return errors.New("register source: empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sources[name]; ok {
		return fmt.Errorf("register source %q: %w", name, ErrDuplicateKind)
	}
	if r.sources == nil { // zero-value Registry
		r.sources = make(map[string]Source)
	}
	r.sources[name] = src
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

// Get returns the Source registered under name (a kind, or the name of an
// instance), or an error wrapping ErrUnknownKind.
func (r *Registry) Get(name string) (Source, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	src, ok := r.sources[name]
	if !ok {
		return nil, fmt.Errorf("get source %q: %w", name, ErrUnknownKind)
	}
	return src, nil
}

// KindOf returns the base kind of the source registered under name (the
// kind events, purls, schemes and policy use), or name when none is.
func (r *Registry) KindOf(name string) string {
	if src, err := r.Get(name); err == nil {
		return src.Kind()
	}
	return name
}

// Names returns every registered name, kinds and instances, sorted. A watch
// names one of them in its source.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return slices.Sorted(maps.Keys(r.sources))
}

// Kinds returns the distinct kinds of the registered sources, sorted: the
// values of an event's data.source, whatever the instances are called.
func (r *Registry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := make(map[string]struct{}, len(r.sources))
	for _, s := range r.sources {
		set[s.Kind()] = struct{}{}
	}
	return slices.Sorted(maps.Keys(set))
}
