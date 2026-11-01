package main

import (
	"bufio"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// Meta is the first line of a snapshot file.
type Meta struct {
	// Registry is the source kind whose default scheme is checked: "pypi" or "npm".
	Registry  string   `json:"registry"`
	Source    string   `json:"source"`
	Oracle    string   `json:"oracle"`
	Generated string   `json:"generated"`
	Packages  int      `json:"packages"`
	Versions  int      `json:"versions"`
	Missing   []string `json:"missing"`
}

// Package is one package of a snapshot: its versions in the registry's
// order, as computed by the registry's own tooling when the snapshot was made.
type Package struct {
	Name string `json:"name"`
	// Order lists the versions the registry accepts, lowest first. Each
	// group holds versions of equal precedence ("1.0" and "1.0.0" in PEP 440).
	Order [][]string `json:"order"`
	// Invalid lists the versions the registry's tooling rejects.
	Invalid []string `json:"invalid"`
}

// Snapshot is a parsed snapshot file.
type Snapshot struct {
	Meta     Meta
	Packages []Package
}

// maxLine bounds one JSON line; the largest package (thousands of
// versions) is far below it.
const maxLine = 64 << 20

// ReadSnapshotFile reads a gzip'd JSON-lines snapshot written by
// snapshot/pypi.py or snapshot/npm.mjs.
func ReadSnapshotFile(path string) (*Snapshot, error) {
	f, err := os.Open(path) //nolint:gosec // path is an operator-supplied flag
	if err != nil {
		return nil, fmt.Errorf("open snapshot: %w", err)
	}
	defer func() { _ = f.Close() }()
	s, err := ReadSnapshot(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// ReadSnapshot reads a gzip'd JSON-lines snapshot from r.
func ReadSnapshot(r io.Reader) (*Snapshot, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 1<<20), maxLine)

	var s Snapshot
	line := 0
	for sc.Scan() {
		line++
		if line == 1 {
			var head struct {
				Meta *Meta `json:"meta"`
			}
			if err := json.Unmarshal(sc.Bytes(), &head); err != nil {
				return nil, fmt.Errorf("line 1: %w", err)
			}
			if head.Meta == nil || head.Meta.Registry == "" {
				return nil, errors.New("line 1: missing meta.registry")
			}
			s.Meta = *head.Meta
			continue
		}
		var p Package
		if err := json.Unmarshal(sc.Bytes(), &p); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if p.Name == "" {
			return nil, fmt.Errorf("line %d: missing name", line)
		}
		s.Packages = append(s.Packages, p)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	if line == 0 {
		return nil, errors.New("empty snapshot")
	}
	if s.Meta.Packages != len(s.Packages) {
		return nil, fmt.Errorf("meta says %d packages, file has %d", s.Meta.Packages, len(s.Packages))
	}
	if n := s.CountVersions(); s.Meta.Versions != n {
		return nil, fmt.Errorf("meta says %d versions, file has %d", s.Meta.Versions, n)
	}
	return &s, nil
}

// CountVersions counts every version of the snapshot, valid or not.
func (s *Snapshot) CountVersions() int {
	n := 0
	for _, p := range s.Packages {
		n += len(p.Invalid)
		for _, g := range p.Order {
			n += len(g)
		}
	}
	return n
}

// Validate rejects a hollow or incomplete snapshot: fewer packages or
// versions than the floors, or packages of the list that the registry did
// not return (meta.missing), which would silently shrink the benchmark.
func (s *Snapshot) Validate(minPackages, minVersions int) error {
	var errs []error
	if len(s.Packages) < minPackages {
		errs = append(errs, fmt.Errorf("%d packages, want at least %d", len(s.Packages), minPackages))
	}
	if n := s.CountVersions(); n < minVersions {
		errs = append(errs, fmt.Errorf("%d versions, want at least %d", n, minVersions))
	}
	if len(s.Meta.Missing) > 0 {
		errs = append(errs, fmt.Errorf("%d listed packages missing from the registry %q: fix the package list and rebuild",
			len(s.Meta.Missing), s.Meta.Missing))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("%s snapshot: %w", s.Meta.Registry, err)
	}
	return nil
}
