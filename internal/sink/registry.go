package sink

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
)

// Registry maps a kind to its Sink. It is an explicit value, not a package
// global: the composition root builds one and passes it down.
type Registry struct {
	mu    sync.RWMutex
	sinks map[string]Sink
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{sinks: make(map[string]Sink)}
}

// Register adds s under s.Kind(). It fails with ErrDuplicateKind if the
// kind is taken, and rejects a nil sink or an empty kind.
func (r *Registry) Register(s Sink) error {
	if s == nil || isNilValue(reflect.ValueOf(s)) {
		return errors.New("register sink: nil sink")
	}
	kind := s.Kind()
	if kind == "" {
		return errors.New("register sink: empty kind")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.sinks[kind]; ok {
		return fmt.Errorf("register sink %q: %w", kind, ErrDuplicateKind)
	}
	if r.sinks == nil { // zero-value Registry
		r.sinks = make(map[string]Sink)
	}
	r.sinks[kind] = s
	return nil
}

func isNilValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return v.IsNil()
	default:
		return false
	}
}

// Get returns the Sink for kind, or an error wrapping ErrUnknownKind.
func (r *Registry) Get(kind string) (Sink, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.sinks[kind]
	if !ok {
		return nil, fmt.Errorf("get sink %q: %w", kind, ErrUnknownKind)
	}
	return s, nil
}
