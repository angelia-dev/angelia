package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// Shared parts of the PUT and DELETE operations.

// errDryRun rolls back the transaction of a dry run: the request runs every
// write, computes its response from the database, and nothing is kept.
var errDryRun = errors.New("api: dry run")

// inTx runs fn in one API transaction; with dry true, it rolls back.
func (s *Server) inTx(ctx context.Context, dry bool, fn func(tx store.APITx) error) error {
	err := s.opts.Store.InAPITx(ctx, func(tx store.APITx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if dry {
			return errDryRun
		}
		return nil
	})
	if dry && errors.Is(err, errDryRun) {
		return nil
	}
	return err
}

// now returns the request time, cut to milliseconds like the store.
func (s *Server) now() time.Time { return s.opts.Now().UTC().Truncate(time.Millisecond) }

// owned is a row with a name and an owner.
type owned interface {
	store.Watch | store.Sink | store.Subscription
}

func ownerOf[T owned](row T) string {
	var m string
	switch r := any(row).(type) {
	case store.Watch:
		m = r.ManagedBy
	case store.Sink:
		m = r.ManagedBy
	case store.Subscription:
		m = r.ManagedBy
	}
	if m == "" {
		return store.ManagedByAPI
	}
	return m
}

// readByName turns a lookup by name into the row of a read: 404 when there
// is none.
func readByName[T any](row T, err error, what, name string) (T, error) {
	if isNotFound(err) {
		return row, problem(SlugNotFound, "no %s named %s", what, name)
	}
	return row, err
}

// writeByName turns a lookup by name into the API row a write changes, nil
// when there is none. A row of the watch file with this name makes the write
// a 409 managed-by-file: names are unique per resource type whoever owns them
// (spec docs/api.md decision 2).
func writeByName[T owned](row T, err error, what, name string) (*T, error) {
	switch {
	case isNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	case ownerOf(row) != store.ManagedByAPI:
		return nil, problem(SlugManagedByFile, "%s %s is managed by the watch file; change the file instead", what, name)
	}
	return &row, nil
}

// bodyNameErrors enforces "name, when given, equals the name in the path".
func bodyNameErrors(raw []byte, name string) []fieldError {
	if v, ok := rawObject(raw)["name"]; ok && v != name {
		return []fieldError{{Pointer: "/name", Detail: "must equal the name in the path"}}
	}
	return nil
}

// parseErrors turns the watch file parser's errors into JSON Pointers. free
// lists the paths whose keys are free-form (labels, coordinates): a dot after
// them belongs to the key.
func parseErrors(errs watchfile.Errors, free ...string) []fieldError {
	out := make([]fieldError, 0, len(errs))
	for _, e := range errs {
		out = append(out, fieldError{Pointer: pathToPointer(e.Path, free), Detail: e.Msg})
	}
	return out
}

func pathToPointer(path string, free []string) string {
	path = strings.TrimPrefix(path, ".")
	if path == "" {
		return ""
	}
	for _, f := range free {
		if rest, ok := strings.CutPrefix(path, f+"."); ok {
			return pathToPointer(f, nil) + pointerJoin("", rest)
		}
	}
	var ptr string
	for _, part := range strings.Split(path, ".") {
		name, idx, _ := strings.Cut(part, "[")
		if name != "" {
			ptr = pointerJoin(ptr, name)
		}
		for idx != "" {
			var n string
			n, idx, _ = strings.Cut(idx, "]")
			ptr = pointerJoin(ptr, n)
			idx = strings.TrimPrefix(idx, "[")
		}
	}
	return ptr
}

// truncateDurations cuts the fraction of a second from the duration strings at
// the given key paths of a JSON object (spec docs/api.md decision 30: whole
// seconds are kept). Values that are not valid durations are left for the
// parser to report, and so are negative ones.
func truncateDurations(raw []byte, paths ...[]string) []byte {
	v, err := decodeJSON(raw)
	if err != nil {
		return raw
	}
	changed := false
	for _, p := range paths {
		m, ok := v.(map[string]any)
		for _, k := range p[:len(p)-1] {
			if !ok {
				break
			}
			m, ok = m[k].(map[string]any)
		}
		if !ok {
			continue
		}
		k := p[len(p)-1]
		s, ok := m[k].(string)
		if !ok {
			continue
		}
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 || d%time.Second == 0 {
			continue
		}
		m[k] = d.Truncate(time.Second).String()
		changed = true
	}
	if !changed {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return b
}

// jsonEqual compares two JSON documents by value.
func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// rawObject decodes a JSON object body.
func rawObject(b []byte) map[string]any {
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	_ = dec.Decode(&m) // validated against the spec before
	return m
}

// location is the Location of a created resource.
func location(collection, name string) string { return "/v1/" + collection + "/" + name }

// storeProblem maps a store error to a problem when it has an API meaning.
func storeProblem(err error, what, id string) error {
	if errors.Is(err, store.ErrNotFound) {
		return problem(SlugNotFound, "no %s %s", what, id)
	}
	return fmt.Errorf("%s %s: %w", what, id, err)
}

func isNotFound(err error) bool { return errors.Is(err, store.ErrNotFound) }

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v) // decoded JSON values always encode
	return b
}
