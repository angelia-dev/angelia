package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// Known is an explained mismatch: the maintainer has decided that our
// scheme keeps following the specification here, or the fix is tracked.
// Every entry needs an explanation and an issue reference.
type Known struct {
	Registry    string   `json:"registry"`
	Package     string   `json:"package"`
	Kind        Kind     `json:"kind"`
	Versions    []string `json:"versions"`
	Explanation string   `json:"explanation"`
	Issue       string   `json:"issue"`
}

func (k Known) key() string {
	return k.Registry + "\x00" + k.Package + "\x00" + string(k.Kind) + "\x00" + strings.Join(k.Versions, "\x00")
}

func (m Mismatch) key() string {
	return Known{Registry: m.Registry, Package: m.Package, Kind: m.Kind, Versions: m.Versions}.key()
}

// LoadKnown reads the known-mismatch file. A missing path means no entries.
func LoadKnown(path string) ([]Known, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied flag
	if err != nil {
		return nil, fmt.Errorf("known mismatches: %w", err)
	}
	var ks []Known
	if err := json.Unmarshal(b, &ks); err != nil {
		return nil, fmt.Errorf("known mismatches %s: %w", path, err)
	}
	var errs []error
	seen := map[string]bool{}
	for i, k := range ks {
		switch {
		case k.Registry == "" || k.Package == "" || k.Kind == "" || len(k.Versions) == 0:
			errs = append(errs, fmt.Errorf("entry %d: registry, package, kind and versions are required", i))
		case strings.TrimSpace(k.Explanation) == "" || strings.TrimSpace(k.Issue) == "":
			errs = append(errs, fmt.Errorf("entry %d (%s %s): explanation and issue are required", i, k.Registry, k.Package))
		case seen[k.key()]:
			errs = append(errs, fmt.Errorf("entry %d (%s %s): duplicate", i, k.Registry, k.Package))
		}
		seen[k.key()] = true
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("known mismatches %s: %w", path, err)
	}
	return ks, nil
}

// ApplyKnown marks the mismatches listed in known and returns the entries
// of known (for registry) that matched nothing: stale entries, which must
// be removed so that the file stays an accurate list.
func ApplyKnown(registry string, results []PackageResult, known []Known) []Known {
	byKey := map[string]*Known{}
	for i := range known {
		if known[i].Registry == registry {
			byKey[known[i].key()] = &known[i]
		}
	}
	used := map[string]bool{}
	for i := range results {
		ms := results[i].Mismatches
		for j := range ms {
			m := &ms[j]
			if k, ok := byKey[m.key()]; ok {
				m.Known = k
				used[m.key()] = true
			}
		}
	}
	var stale []Known
	for _, k := range known {
		if k.Registry == registry && !used[k.key()] {
			stale = append(stale, k)
		}
	}
	slices.SortFunc(stale, func(a, b Known) int { return strings.Compare(a.key(), b.key()) })
	return stale
}
