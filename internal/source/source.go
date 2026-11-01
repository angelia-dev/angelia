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
	"time"
)

// Source detects releases on one kind of upstream.
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
	Yanked      bool           // yanked, deprecated or withdrawn upstream
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
