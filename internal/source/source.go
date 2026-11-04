// Package source defines the Source contract (architecture.md section 3.3)
// and a Registry of Source implementations by kind. Implementations live in
// subpackages, one per kind (internal/source/pypi, ...).
//
// Sources are dumb: they list the raw upstream versions and an opaque fetch
// state. Diffing, version parsing and policy belong to the core.
package source

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"
)

// Source detects releases on one kind of upstream.
//
// Implementations that do HTTP receive their *http.Client through their
// constructor (injection) and never use http.DefaultClient, so the netguard
// client or a test guard can be substituted. List wraps context errors with
// %w. See ADR 0006.
type Source interface {
	// Kind is the registry key: "pypi", "npm", "github-release", "oci", ...
	Kind() string
	// Validate checks the target's coordinates at create time. It does no
	// network I/O and returns an error wrapping ErrInvalidTarget.
	Validate(target Target) error
	// List fetches the upstream versions. When s lets the source prove that
	// nothing changed (ETag, Last-Modified, cursor) it returns a Result with
	// NotModified set and no Versions.
	List(ctx context.Context, t Target, s FetchState) (Result, error)
}

// Canonicalizer is an optional interface a Source implements when one
// upstream object can be spelled several ways in its coordinates ("Requests"
// and "requests" on PyPI). Canonical returns the one spelling the core stores
// and uses as the coalescing key, so that every spelling shares one target
// and one fetch. See ADR 0021, which amends ADR 0006.
//
// Canonical is called only on a target that Validate accepted. It does no
// I/O, keeps the Kind, is idempotent (Canonical of its result returns the
// result unchanged) and its result passes Validate. It must not mutate t. A
// Source whose coordinates are already canonical as written does not
// implement it.
type Canonicalizer interface {
	Canonical(t Target) (Target, error)
}

// Canonical returns the canonical form of t for src: src.Canonical(t) when
// src implements Canonicalizer, otherwise a copy of t. The result never
// shares its Coordinates map with t. It rejects a Canonicalizer that changes
// the Kind.
func Canonical(src Source, t Target) (Target, error) {
	c, ok := src.(Canonicalizer)
	if !ok {
		return Target{Kind: t.Kind, Coordinates: maps.Clone(t.Coordinates)}, nil
	}
	out, err := c.Canonical(Target{Kind: t.Kind, Coordinates: maps.Clone(t.Coordinates)})
	if err != nil {
		return Target{}, fmt.Errorf("canonical target for %q: %w", t.Kind, err)
	}
	if out.Kind != t.Kind {
		return Target{}, fmt.Errorf("canonical target for %q: source changed the kind to %q", t.Kind, out.Kind)
	}
	out.Coordinates = maps.Clone(out.Coordinates)
	return out, nil
}

// Target identifies one upstream object (target.kind and
// target.coordinates_json in architecture.md section 5). Coordinates are
// source specific, for example {"name": "requests"} for pypi.
type Target struct {
	Kind        string            `json:"kind"`
	Coordinates map[string]string `json:"coordinates"`
}

// FetchState is the conditional-request state kept per target
// (target.fetch_state_json). Its content is opaque to the core: only the
// source that produced it interprets it. The zero value means "no state".
type FetchState struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
}

// IsZero reports whether s carries no state.
func (s FetchState) IsZero() bool { return s == FetchState{} }

// Result is the outcome of a List call.
type Result struct {
	// Versions are the raw upstream versions, newest first if known.
	Versions []Version
	// State is the state to pass to the next List call for the same target.
	State FetchState
	// NotModified is true when the upstream confirmed nothing changed.
	NotModified bool
}

// Version is one raw upstream version.
type Version struct {
	Raw         string         // "v2.3.0-rc.1"
	PublishedAt time.Time      // zero if upstream does not say
	Prerelease  *bool          // upstream flag if available; nil if unknown
	Yanked      bool           // yanked, unpublished or withdrawn upstream; deprecation goes in Metadata
	URL         string         // release page
	NotesURL    string         // release notes
	Metadata    map[string]any // digest, tarball url, dist-tags, ...
}

var (
	// ErrInvalidTarget is wrapped by Validate errors.
	ErrInvalidTarget = errors.New("source: invalid target")
	// ErrUnknownKind is returned when no source is registered for a kind.
	ErrUnknownKind = errors.New("source: unknown kind")
	// ErrDuplicateKind is returned when a kind is registered twice.
	ErrDuplicateKind = errors.New("source: kind already registered")
)
