package main

import (
	"hash/fnv"
	"math/rand/v2"
	"slices"

	"github.com/angelia-dev/angelia/internal/version"
)

// Kind is the kind of a mismatch between our scheme and the registry.
type Kind string

// Mismatch kinds.
const (
	// KindRejected: the registry accepts the version, our scheme does not
	// (it falls back to lexical and ranks below every valid version).
	KindRejected Kind = "rejected"
	// KindAccepted: the registry rejects the version, our scheme parses it.
	KindAccepted Kind = "accepted"
	// KindOrder: two versions that are adjacent in the registry's order
	// compare differently with our scheme.
	KindOrder Kind = "order"
)

// Mismatch is one disagreement between our scheme and the registry.
type Mismatch struct {
	Registry string `json:"registry"`
	Package  string `json:"package"`
	Kind     Kind   `json:"kind"`
	// Versions is the version (rejected, accepted) or the pair A, B with
	// A before B in the registry's order (order).
	Versions []string `json:"versions"`
	// Expected and Actual are the relation of A to B ("<", "=", ">") in the
	// registry's order and with our scheme. Empty unless Kind is order.
	Expected string `json:"expected,omitempty"`
	Actual   string `json:"actual,omitempty"`
	// Known is set when the mismatch is listed in the known-mismatch file.
	Known *Known `json:"known,omitempty"`
}

// PackageResult is the outcome for one package.
type PackageResult struct {
	Package    string     `json:"package"`
	Versions   int        `json:"versions"`
	Mismatches []Mismatch `json:"mismatches,omitempty"`
	// Expected and Actual are the whole orders (lowest first, one string
	// per version, groups of equal precedence joined by " = ") of the
	// versions both sides accept. Set only when they differ.
	Expected []string `json:"expected,omitempty"`
	Actual   []string `json:"actual,omitempty"`
}

// ComparePackage checks scheme against the registry's view of one package.
// The versions are shuffled deterministically (seeded by the package name)
// before sorting, so the result never depends on the snapshot already
// being in order.
func ComparePackage(registry string, scheme version.Scheme, p Package) PackageResult {
	res := PackageResult{Package: p.Name}
	mis := func(k Kind, vs ...string) Mismatch {
		return Mismatch{Registry: registry, Package: p.Name, Kind: k, Versions: vs}
	}

	for _, raw := range p.Invalid {
		res.Versions++
		if v, err := scheme.Parse(raw); err == nil && v.Scheme() == scheme.Name() {
			res.Mismatches = append(res.Mismatches, mis(KindAccepted, raw))
		}
	}

	// Parse the registry's valid versions; keep the ones our scheme accepts,
	// with the index of their registry group.
	type item struct {
		raw   string
		v     version.Version
		group int
	}
	var items []item
	var expected [][]string
	for _, g := range p.Order {
		var kept []string
		for _, raw := range g {
			res.Versions++
			v, err := scheme.Parse(raw)
			if err != nil || v.Scheme() != scheme.Name() {
				res.Mismatches = append(res.Mismatches, mis(KindRejected, raw))
				continue
			}
			items = append(items, item{raw: raw, v: v, group: len(expected)})
			kept = append(kept, raw)
		}
		if len(kept) > 0 {
			expected = append(expected, kept)
		}
	}

	// Adjacent pairs of the registry's order: inside a group every version
	// must compare equal to the group's first; between groups the last of
	// one must compare below the first of the next.
	for i := 1; i < len(items); i++ {
		a, b := items[i-1], items[i]
		want := 0
		if a.group != b.group {
			want = -1
		}
		if got := scheme.Compare(a.v, b.v); got != want {
			m := mis(KindOrder, a.raw, b.raw)
			m.Expected, m.Actual = relation(want), relation(got)
			res.Mismatches = append(res.Mismatches, m)
		}
	}

	// Full order: shuffle, sort with our scheme, regroup.
	shuffled := slices.Clone(items)
	h := fnv.New64a()
	_, _ = h.Write([]byte(registry + "/" + p.Name))
	r := rand.New(rand.NewPCG(h.Sum64(), 55)) //nolint:gosec // deterministic shuffle, not security
	r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	slices.SortStableFunc(shuffled, func(a, b item) int { return scheme.Compare(a.v, b.v) })
	var actual [][]string
	for i, it := range shuffled {
		if i > 0 && scheme.Compare(shuffled[i-1].v, it.v) == 0 {
			actual[len(actual)-1] = append(actual[len(actual)-1], it.raw)
			continue
		}
		actual = append(actual, []string{it.raw})
	}
	exp, act := render(expected), render(actual)
	if !slices.Equal(exp, act) {
		res.Expected, res.Actual = exp, act
		if !hasKind(res.Mismatches, KindOrder) {
			// Every adjacent pair agrees but the sort disagrees: our
			// Compare is not transitive on this set. Report the first
			// position where the orders differ.
			i := firstDiff(exp, act)
			m := mis(KindOrder, exp[i], act[i])
			m.Expected, m.Actual = "<", ">"
			res.Mismatches = append(res.Mismatches, m)
		}
	}
	return res
}

// render turns groups into one string per group, each group's versions
// sorted bytewise and joined by " = ".
func render(groups [][]string) []string {
	out := make([]string, len(groups))
	for i, g := range groups {
		g = slices.Clone(g)
		slices.Sort(g)
		s := g[0]
		for _, v := range g[1:] {
			s += " = " + v
		}
		out[i] = s
	}
	return out
}

func firstDiff(a, b []string) int {
	for i, x := range a {
		if i >= len(b) || x != b[i] {
			return min(i, len(b)-1)
		}
	}
	return len(a) - 1
}

func hasKind(ms []Mismatch, k Kind) bool {
	return slices.ContainsFunc(ms, func(m Mismatch) bool { return m.Kind == k })
}

func relation(c int) string {
	switch {
	case c < 0:
		return "<"
	case c > 0:
		return ">"
	}
	return "="
}
