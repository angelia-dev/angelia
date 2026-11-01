package policy

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/diff"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/pep440"
	"github.com/angelia-dev/angelia/internal/version/schemes"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

var (
	t0  = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	sem = schemes.WithFallback(semver.Scheme{}, nil)
)

// newChanges runs the diff engine on known and fetched with scheme s, as a
// check at now would, and returns its changes.
func newChanges(s version.Scheme, known []string, fetched []source.Version, now time.Time) []diff.Change {
	ks := make([]store.Version, len(known))
	for i, raw := range known {
		ks[i] = store.Version{TargetID: "t", Raw: raw, FirstSeenAt: t0.Add(-time.Hour)}
	}
	return diff.Compute("t", ks, fetched, s, now).Changes
}

func fetched(raws ...string) []source.Version {
	out := make([]source.Version, len(raws))
	for i, raw := range raws {
		out[i] = source.Version{Raw: raw}
	}
	return out
}

func compile(t *testing.T, p Policy, s version.Scheme) *Engine {
	t.Helper()
	if p.Prerelease == "" {
		p.Prerelease = PrereleaseNone
	}
	e, err := Compile(p, s)
	if err != nil {
		t.Fatalf("Compile(%+v): %v", p, err)
	}
	return e
}

func raws(rs []Release) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Raw
	}
	return out
}

// The candidate set for the filter tables, after 1.0.0 is known:
//
//	1.0.1 patch, 1.1.0 minor, 2.0.0 major, 2.1.0-rc.1 pre-release (bump minor),
//	v2.0.0 alias of 2.0.0, 1.0.2 yanked, latest (lexical fallback, no bump).
func candidates() []source.Version {
	vs := fetched("2.1.0-rc.1", "2.0.0", "v2.0.0", "1.1.0", "1.0.2", "1.0.1", "latest", "1.0.0")
	vs[4].Yanked = true
	return vs
}

func TestFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Policy
		want []string
	}{
		// Defaults: pre-releases, aliases and yanked versions are dropped.
		{"default", Policy{}, []string{"latest", "1.0.1", "1.1.0", "2.0.0"}},

		{"include one", Policy{Include: []string{`^1\.`}}, []string{"1.0.1", "1.1.0"}},
		{"include any of", Policy{Include: []string{`^1\.0`, `^2`}}, []string{"1.0.1", "2.0.0"}},
		{"include unanchored", Policy{Include: []string{`1\.1`}}, []string{"1.1.0"}},
		{"include none matches", Policy{Include: []string{`^9`}}, nil},
		{"exclude", Policy{Exclude: []string{`\.0$`}}, []string{"latest", "1.0.1"}},
		{"exclude several", Policy{Exclude: []string{`^1`, `latest`}}, []string{"2.0.0"}},
		{"exclude wins over include", Policy{Include: []string{`^1`}, Exclude: []string{`^1\.1`}}, []string{"1.0.1"}},

		{"prerelease none", Policy{Prerelease: PrereleaseNone}, []string{"latest", "1.0.1", "1.1.0", "2.0.0"}},
		{"prerelease only", Policy{Prerelease: PrereleaseOnly}, []string{"2.1.0-rc.1"}},
		{"prerelease include", Policy{Prerelease: PrereleaseInclude}, []string{"latest", "1.0.1", "1.1.0", "2.0.0", "2.1.0-rc.1"}},

		{"constraint range", Policy{Constraint: ">=1.1 <2"}, []string{"1.1.0"}},
		{"constraint drops fallbacks", Policy{Constraint: ">=0"}, []string{"1.0.1", "1.1.0", "2.0.0"}},
		{"constraint or", Policy{Constraint: "<1.1 || >=2"}, []string{"1.0.1", "2.0.0"}},
		{"constraint not equal", Policy{Constraint: "!=1.1.0 >=1"}, []string{"1.0.1", "2.0.0"}},

		{"bump major", Policy{Bump: []version.Bump{version.BumpMajor}}, []string{"2.0.0"}},
		{"bump minor patch", Policy{Bump: []version.Bump{version.BumpMinor, version.BumpPatch}}, []string{"1.0.1", "1.1.0"}},
		{"bump patch", Policy{Bump: []version.Bump{version.BumpPatch}}, []string{"1.0.1"}},

		// Combinations.
		{"prerelease include and bump minor", Policy{Prerelease: PrereleaseInclude, Bump: []version.Bump{version.BumpMinor}}, []string{"1.1.0", "2.1.0-rc.1"}},
		{"prerelease include and constraint", Policy{Prerelease: PrereleaseInclude, Constraint: ">=2"}, []string{"2.0.0", "2.1.0-rc.1"}},
		{"include, exclude and prerelease", Policy{Prerelease: PrereleaseInclude, Include: []string{`^v?2`}, Exclude: []string{`rc`}}, []string{"2.0.0"}},
		{"prerelease only and constraint", Policy{Prerelease: PrereleaseOnly, Constraint: "<2"}, nil},
		{"constraint, bump and exclude", Policy{Constraint: ">=1 <2", Bump: []version.Bump{version.BumpPatch}, Exclude: []string{`1\.0\.1`}}, nil},
		{"every key", Policy{
			Prerelease: PrereleaseInclude, Include: []string{`^[0-9]`}, Exclude: []string{`^1\.0`},
			Constraint: ">=1 <3", Bump: []version.Bump{version.BumpMinor, version.BumpMajor},
		}, []string{"1.1.0", "2.0.0", "2.1.0-rc.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := compile(t, tc.p, sem)
			d := e.Evaluate(newChanges(sem, []string{"1.0.0"}, candidates(), t0), nil, t0)
			if got := raws(d.Emit); !slices.Equal(got, tc.want) {
				t.Fatalf("emitted %q, want %q (dropped %+v)", got, tc.want, d.Dropped)
			}
			if len(d.Hold) != 0 || len(d.Unhold) != 0 {
				t.Fatalf("hold %v unhold %v without cooldown", d.Hold, d.Unhold)
			}
			if n := len(d.Emit) + len(d.Dropped); n != 7 {
				t.Fatalf("%d emitted + dropped, want all 7 new versions", n)
			}
		})
	}
}

func TestDropReasons(t *testing.T) {
	e := compile(t, Policy{Include: []string{`^[0-9]`}, Exclude: []string{`^1\.0\.1$`}, Constraint: "<2.0.0", Bump: []version.Bump{version.BumpPatch}}, sem)
	d := e.Evaluate(newChanges(sem, []string{"1.0.0"}, candidates(), t0), nil, t0)
	got := map[string]Reason{}
	for _, dr := range d.Dropped {
		if dr.Held {
			t.Errorf("%s dropped as held", dr.Raw)
		}
		got[dr.Raw] = dr.Reason
	}
	want := map[string]Reason{
		"1.0.2": ReasonYanked, "v2.0.0": ReasonAlias, "latest": ReasonInclude, "1.0.1": ReasonExclude,
		"2.1.0-rc.1": ReasonPrerelease, "2.0.0": ReasonConstraint, "1.1.0": ReasonBump,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drop reasons = %v\nwant %v", got, want)
	}
}

func TestAliases(t *testing.T) {
	pep := schemes.WithFallback(pep440.Scheme{}, nil)
	for _, tc := range []struct {
		name    string
		s       version.Scheme
		known   []string
		fetched []string
		want    []string
	}{
		{"v prefix of a known version", sem, []string{"1.2.0"}, []string{"v1.2.0"}, nil},
		{"build metadata", sem, []string{"1.2.0+a"}, []string{"1.2.0+b"}, nil},
		{"same fetch: first entry wins", sem, []string{"1.0.0"}, []string{"1.1.0", "v1.1.0"}, []string{"1.1.0"}},
		{"pep440 zero padding", pep, []string{"1.0"}, []string{"1.0.0"}, nil},
		{"backport is not an alias", sem, []string{"1.0.0", "2.0.0"}, []string{"1.0.1"}, []string{"1.0.1"}},
		{"first version is not an alias", sem, nil, []string{"1.0.0"}, []string{"1.0.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := compile(t, Policy{}, tc.s)
			d := e.Evaluate(newChanges(tc.s, tc.known, fetched(append(tc.fetched, tc.known...)...), t0), nil, t0)
			if got := raws(d.Emit); !slices.Equal(got, tc.want) {
				t.Fatalf("emitted %q, want %q (dropped %+v)", got, tc.want, d.Dropped)
			}
		})
	}
}

func TestEvaluateIgnoresOtherKinds(t *testing.T) {
	e := compile(t, Policy{}, sem)
	cs := []diff.Change{{Kind: diff.KindYanked, Raw: "1.0.0"}, {Kind: diff.KindUnyanked, Raw: "1.0.1"}, {Kind: diff.KindRemoved, Raw: "0.9.0"}}
	if d := e.Evaluate(cs, nil, t0); !reflect.DeepEqual(d, Decision{}) {
		t.Fatalf("Evaluate = %+v", d)
	}
}

func TestReleaseSnapshot(t *testing.T) {
	pre := true
	f := []source.Version{{
		Raw: "v2.0.0", PublishedAt: t0.Add(-time.Hour).In(time.FixedZone("x", 3600)), Prerelease: &pre,
		URL: "https://example.org/r", NotesURL: "https://example.org/n", Metadata: map[string]any{"digest": "sha256:x"},
	}, {Raw: "1.0.0"}}
	e := compile(t, Policy{Prerelease: PrereleaseInclude}, sem)
	d := e.Evaluate(newChanges(sem, []string{"1.0.0"}, f, t0), nil, t0)
	want := Release{
		Raw: "v2.0.0", Normalized: "2.0.0", Scheme: "semver", Previous: "1.0.0", Bump: version.BumpMajor,
		Prerelease: true, PublishedAt: t0.Add(-time.Hour), DetectedAt: t0,
		URL: "https://example.org/r", NotesURL: "https://example.org/n", Metadata: map[string]any{"digest": "sha256:x"},
	}
	if len(d.Emit) != 1 || !reflect.DeepEqual(d.Emit[0], want) {
		t.Fatalf("Emit = %+v\nwant %+v", d.Emit, want)
	}
	if d.Emit[0].PublishedAt.Location() != time.UTC {
		t.Error("PublishedAt not UTC")
	}
	// A backport keeps its flag.
	d = e.Evaluate(newChanges(sem, []string{"1.0.0", "2.0.0"}, fetched("1.0.1", "2.0.0", "1.0.0"), t0), nil, t0)
	if len(d.Emit) != 1 || !d.Emit[0].Backport || d.Emit[0].Bump != version.BumpPatch {
		t.Fatalf("backport = %+v", d.Emit)
	}
}

func TestCooldownFrom(t *testing.T) {
	for _, tc := range []struct {
		name      string
		published time.Time
		want      time.Time
	}{
		{"published known", t0.Add(-5 * time.Hour), t0.Add(-5 * time.Hour)},
		{"published unknown", time.Time{}, t0},
		{"published in the future", t0.Add(time.Hour), t0},
		{"published at detection", t0, t0},
	} {
		if got := (Release{PublishedAt: tc.published, DetectedAt: t0}).CooldownFrom(); !got.Equal(tc.want) {
			t.Errorf("%s: CooldownFrom = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCooldownNewVersions(t *testing.T) {
	const cool = 72 * time.Hour
	e := compile(t, Policy{Cooldown: Duration(cool)}, sem)
	f := []source.Version{
		{Raw: "1.3.0"}, // no date: from detection
		{Raw: "1.2.0", PublishedAt: t0.Add(-100 * time.Hour)}, // already old enough
		{Raw: "1.1.1", PublishedAt: t0.Add(-cool)},            // exactly old enough
		{Raw: "1.1.0", PublishedAt: t0.Add(-time.Hour)},       // too young
		{Raw: "1.0.1", PublishedAt: t0.Add(time.Hour)},        // future date: from detection
		{Raw: "1.0.0"},
	}
	d := e.Evaluate(newChanges(sem, []string{"1.0.0"}, f, t0), nil, t0)
	if got := raws(d.Emit); !slices.Equal(got, []string{"1.1.1", "1.2.0"}) {
		t.Fatalf("emitted %q", got)
	}
	if got := d.Emit[0].CooldownSatisfiedAt; !got.Equal(t0) {
		t.Errorf("1.1.1 cooldown_satisfied_at = %v, want %v", got, t0)
	}
	if got := d.Emit[1].CooldownSatisfiedAt; !got.Equal(t0.Add(-28 * time.Hour)) {
		t.Errorf("1.2.0 cooldown_satisfied_at = %v", got)
	}
	if got := raws(d.Hold); !slices.Equal(got, []string{"1.0.1", "1.1.0", "1.3.0"}) {
		t.Fatalf("held %q", got)
	}
	for _, r := range d.Hold {
		if !r.CooldownSatisfiedAt.IsZero() {
			t.Errorf("held %s has cooldown_satisfied_at", r.Raw)
		}
	}
	// Filters run before the cooldown: a dropped version is not held.
	e = compile(t, Policy{Cooldown: Duration(cool), Bump: []version.Bump{version.BumpMajor}}, sem)
	if d := e.Evaluate(newChanges(sem, []string{"1.0.0"}, f, t0), nil, t0); len(d.Hold)+len(d.Emit) != 0 {
		t.Fatalf("filtered versions held or emitted: %+v", d)
	}
	// Yanked at detection: dropped, not held.
	f = []source.Version{{Raw: "1.4.0", Yanked: true}, {Raw: "1.0.0"}}
	d = compile(t, Policy{Cooldown: Duration(cool)}, sem).Evaluate(newChanges(sem, []string{"1.0.0"}, f, t0), nil, t0)
	if len(d.Hold) != 0 || len(d.Dropped) != 1 || d.Dropped[0].Reason != ReasonYanked {
		t.Fatalf("yanked new version: %+v", d)
	}
}

func held(t *testing.T, r Release, mut func(*store.PendingRelease)) store.PendingRelease {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	p := store.PendingRelease{WatchID: "w", TargetID: "t", Raw: r.Raw, HeldAt: r.DetectedAt, Release: b}
	if mut != nil {
		mut(&p)
	}
	return p
}

func TestCooldownHeldVersions(t *testing.T) {
	const cool = 72 * time.Hour
	rel := func(raw string, detected time.Time) Release {
		return Release{Raw: raw, Normalized: raw, Scheme: "semver", Previous: "1.0.0", Bump: version.BumpMinor, DetectedAt: detected}
	}
	young := rel("1.3.0", t0.Add(-time.Hour))
	old := rel("1.2.0", t0.Add(-80*time.Hour))
	older := rel("1.1.0", t0.Add(-90*time.Hour))
	sameTimeLower := rel("1.0.5", t0.Add(-80*time.Hour))
	sameTimeLower.Bump = version.BumpPatch

	for _, tc := range []struct {
		name    string
		p       Policy
		held    []store.PendingRelease
		emit    []string
		unhold  []string
		dropped []Drop
	}{
		{"cooling down", Policy{Cooldown: Duration(cool)}, []store.PendingRelease{held(t, young, nil)}, nil, nil, nil},
		{
			"cooldown over, in detection then version order",
			Policy{Cooldown: Duration(cool)},
			[]store.PendingRelease{held(t, young, nil), held(t, old, nil), held(t, older, nil), held(t, sameTimeLower, nil)},
			[]string{"1.1.0", "1.0.5", "1.2.0"},
			[]string{"1.2.0", "1.1.0", "1.0.5"},
			nil,
		},
		{
			"yanked while held",
			Policy{Cooldown: Duration(cool)},
			[]store.PendingRelease{held(t, old, func(p *store.PendingRelease) { p.Yanked = true }), held(t, young, func(p *store.PendingRelease) { p.Yanked = true })},
			nil,
			[]string{"1.2.0", "1.3.0"},
			[]Drop{{Raw: "1.2.0", Reason: ReasonYanked, Held: true}, {Raw: "1.3.0", Reason: ReasonYanked, Held: true}},
		},
		{
			"removed while held is kept",
			Policy{Cooldown: Duration(cool)},
			[]store.PendingRelease{held(t, old, func(p *store.PendingRelease) { p.Removed = true })},
			nil, nil, nil,
		},
		{
			"removed and yanked is dropped",
			Policy{Cooldown: Duration(cool)},
			[]store.PendingRelease{held(t, old, func(p *store.PendingRelease) { p.Removed, p.Yanked = true, true })},
			nil,
			[]string{"1.2.0"},
			[]Drop{{Raw: "1.2.0", Reason: ReasonYanked, Held: true}},
		},
		{
			"policy changed: filtered out",
			Policy{Cooldown: Duration(cool), Bump: []version.Bump{version.BumpPatch}},
			[]store.PendingRelease{held(t, young, nil), held(t, sameTimeLower, nil)},
			[]string{"1.0.5"},
			[]string{"1.3.0", "1.0.5"},
			[]Drop{{Raw: "1.3.0", Reason: ReasonBump, Held: true}},
		},
		{
			"policy changed: constraint",
			Policy{Cooldown: Duration(cool), Constraint: "<1.2"},
			[]store.PendingRelease{held(t, old, nil), held(t, older, nil)},
			[]string{"1.1.0"},
			[]string{"1.2.0", "1.1.0"},
			[]Drop{{Raw: "1.2.0", Reason: ReasonConstraint, Held: true}},
		},
		{
			"cooldown shortened",
			Policy{Cooldown: Duration(time.Hour)},
			[]store.PendingRelease{held(t, young, nil)},
			[]string{"1.3.0"},
			[]string{"1.3.0"},
			nil,
		},
		{
			"cooldown removed",
			Policy{},
			[]store.PendingRelease{held(t, young, nil)},
			[]string{"1.3.0"},
			[]string{"1.3.0"},
			nil,
		},
		{"corrupt row", Policy{Cooldown: Duration(cool)}, []store.PendingRelease{
			held(t, old, func(p *store.PendingRelease) { p.Release = json.RawMessage(`{"raw":`) }),
			held(t, old, func(p *store.PendingRelease) { p.Raw = "other" }),
		}, nil, []string{"1.2.0", "other"}, []Drop{{Raw: "1.2.0", Reason: ReasonCorrupt, Held: true}, {Raw: "other", Reason: ReasonCorrupt, Held: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := compile(t, tc.p, sem).Evaluate(nil, tc.held, t0)
			if got := raws(d.Emit); !slices.Equal(got, tc.emit) {
				t.Errorf("emitted %q, want %q", got, tc.emit)
			}
			if !slices.Equal(d.Unhold, tc.unhold) {
				t.Errorf("unhold %q, want %q", d.Unhold, tc.unhold)
			}
			if !slices.Equal(d.Dropped, tc.dropped) {
				t.Errorf("dropped %+v, want %+v", d.Dropped, tc.dropped)
			}
			for _, r := range d.Emit {
				want := r.CooldownFrom().Add(time.Duration(tc.p.Cooldown))
				if tc.p.Cooldown == 0 {
					want = time.Time{}
				}
				if !r.CooldownSatisfiedAt.Equal(want) {
					t.Errorf("%s cooldown_satisfied_at = %v, want %v", r.Raw, r.CooldownSatisfiedAt, want)
				}
			}
		})
	}
}

func TestHeldThenNewOrder(t *testing.T) {
	e := compile(t, Policy{Cooldown: Duration(time.Hour)}, sem)
	old := Release{Raw: "1.1.0", Scheme: "semver", DetectedAt: t0.Add(-2 * time.Hour)}
	f := []source.Version{{Raw: "1.2.0", PublishedAt: t0.Add(-3 * time.Hour)}, {Raw: "1.1.0"}, {Raw: "1.0.0"}}
	d := e.Evaluate(newChanges(sem, []string{"1.0.0", "1.1.0"}, f, t0), []store.PendingRelease{held(t, old, nil)}, t0)
	if got := raws(d.Emit); !slices.Equal(got, []string{"1.1.0", "1.2.0"}) {
		t.Fatalf("emitted %q", got)
	}
}

func TestCompileErrors(t *testing.T) {
	if _, err := Compile(Policy{Prerelease: "bad"}, sem); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid policy: %v", err)
	}
	if _, err := Compile(Policy{Prerelease: PrereleaseNone, Constraint: ">=latest"}, sem); !errors.Is(err, ErrInvalid) {
		t.Errorf("operand the scheme rejects: %v", err)
	}
	e, err := Compile(Default(), sem)
	if err != nil || !reflect.DeepEqual(e.Policy(), Default()) {
		t.Errorf("Compile(Default()) = %v, %v", e, err)
	}
}
