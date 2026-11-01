package diff

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/lexical"
	"github.com/angelia-dev/angelia/internal/version/pep440"
	"github.com/angelia-dev/angelia/internal/version/schemes"
	"github.com/angelia-dev/angelia/internal/version/semver"
)

var (
	t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
)

// known builds a known set: "1.0.0" is listed, "1.0.0!" is yanked and
// "1.0.0-" was removed at t0.
func known(specs ...string) []store.Version {
	out := make([]store.Version, len(specs))
	for i, s := range specs {
		v := store.Version{TargetID: "t", FirstSeenAt: t0}
		switch {
		case strings.HasSuffix(s, "!"):
			v.Yanked, s = true, strings.TrimSuffix(s, "!")
		case strings.HasSuffix(s, "~"):
			v.RemovedAt, s = t0, strings.TrimSuffix(s, "~")
		}
		v.Raw, v.Normalized = s, s
		out[i] = v
	}
	return out
}

// fetched builds an upstream list with the same "!" suffix for yanked.
func fetched(specs ...string) []source.Version {
	out := make([]source.Version, len(specs))
	for i, s := range specs {
		y := strings.HasSuffix(s, "!")
		out[i] = source.Version{Raw: strings.TrimSuffix(s, "!"), Yanked: y}
	}
	return out
}

// summary renders a change as "kind raw [prev bump backport]".
func summary(c Change) string {
	s := string(c.Kind) + " " + c.Raw
	if c.Kind == KindNew {
		s += fmt.Sprintf(" prev=%s bump=%s", c.Previous, c.Bump)
		if c.Backport {
			s += " backport"
		}
	}
	if c.Yanked {
		s += " yanked"
	}
	return s
}

func summaries(cs []Change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = summary(c)
	}
	return out
}

func TestCompute(t *testing.T) {
	semverFB := schemes.WithFallback(semver.Scheme{}, nil)
	tests := []struct {
		name    string
		scheme  version.Scheme
		known   []store.Version
		fetched []source.Version
		want    []string
		add     []string
		update  []string // raw:yanked:removed
	}{
		{
			name:    "new latest",
			known:   known("1.0.0", "1.1.0"),
			fetched: fetched("1.2.0", "1.1.0", "1.0.0"),
			want:    []string{"new 1.2.0 prev=1.1.0 bump=minor"},
			add:     []string{"1.2.0"},
		},
		{
			name:    "backport after a newer major",
			known:   known("1.9.3", "2.0.0"),
			fetched: fetched("2.0.0", "1.9.4", "1.9.3"),
			want:    []string{"new 1.9.4 prev=1.9.3 bump=patch backport"},
			add:     []string{"1.9.4"},
		},
		{
			name:    "several new at once, in version order",
			known:   known("1.0.0"),
			fetched: fetched("2.0.0", "1.1.0", "1.0.1", "1.0.0"),
			want: []string{
				"new 1.0.1 prev=1.0.0 bump=patch",
				"new 1.1.0 prev=1.0.1 bump=minor",
				"new 2.0.0 prev=1.1.0 bump=major",
			},
			add: []string{"1.0.1", "1.1.0", "2.0.0"},
		},
		{
			name:    "pre-release then release",
			known:   known("1.0.0", "2.0.0-rc.1"),
			fetched: fetched("2.0.0", "2.0.0-rc.1", "1.0.0"),
			want:    []string{"new 2.0.0 prev=2.0.0-rc.1 bump=major"},
			add:     []string{"2.0.0"},
		},
		{
			name:    "first version ever has no previous",
			fetched: fetched("0.1.0"),
			want:    []string{"new 0.1.0 prev= bump="},
			add:     []string{"0.1.0"},
		},
		{
			name:    "new version already yanked",
			known:   known("1.0.0"),
			fetched: fetched("1.0.1!", "1.0.0"),
			want:    []string{"new 1.0.1 prev=1.0.0 bump=patch yanked"},
			add:     []string{"1.0.1"},
		},
		{
			name:    "yank",
			known:   known("1.0.0", "1.0.1"),
			fetched: fetched("1.0.1!", "1.0.0"),
			want:    []string{"yanked 1.0.1 yanked"},
			update:  []string{"1.0.1:true:false"},
		},
		{
			name:    "un-yank",
			known:   known("1.0.0", "1.0.1!"),
			fetched: fetched("1.0.1", "1.0.0"),
			want:    []string{"unyanked 1.0.1"},
			update:  []string{"1.0.1:false:false"},
		},
		{
			name:    "removal",
			known:   known("1.0.0", "1.0.1", "1.0.2"),
			fetched: fetched("1.0.2", "1.0.0"),
			want:    []string{"removed 1.0.1"},
			update:  []string{"1.0.1:false:true"},
		},
		{
			name:    "already removed is not reported again",
			known:   known("1.0.0", "1.0.1~"),
			fetched: fetched("1.0.0"),
		},
		{
			name:    "removed version returns, yanked",
			known:   known("1.0.0", "1.0.1~"),
			fetched: fetched("1.0.1!", "1.0.0"),
			want:    []string{"yanked 1.0.1 yanked", "returned 1.0.1 yanked"},
			update:  []string{"1.0.1:true:false"},
		},
		{
			name:  "empty upstream changes nothing",
			known: known("1.0.0", "1.0.1"),
		},
		{
			name: "empty upstream, empty known set",
		},
		{
			name:    "no change",
			known:   known("1.0.0", "1.0.1!", "0.9.0~"),
			fetched: fetched("1.0.1!", "1.0.0"),
		},
		{
			name:    "mixed kinds are grouped by kind, then by version",
			known:   known("1.0.0", "1.1.0!", "1.2.0", "1.3.0", "1.4.0"),
			fetched: fetched("2.0.0", "1.4.0!", "1.3.0", "1.1.0", "1.0.0", "1.5.0"),
			want: []string{
				"new 1.5.0 prev=1.4.0 bump=minor",
				"new 2.0.0 prev=1.5.0 bump=major",
				"yanked 1.4.0 yanked",
				"unyanked 1.1.0",
				"removed 1.2.0",
			},
			add:    []string{"1.5.0", "2.0.0"},
			update: []string{"1.4.0:true:false", "1.1.0:false:false", "1.2.0:false:true"}, // fetch order, then removals
		},
		{
			name:    "blank, padded and duplicate entries are skipped",
			known:   known("1.0.0"),
			fetched: fetched("1.1.0", " ", "1.1.0!", "", " 1.2.0", "1.3.0\n", "1.0.0"),
			want:    []string{"new 1.1.0 prev=1.0.0 bump=minor"},
			add:     []string{"1.1.0"},
		},
		{
			name:    "a duplicate with another yank flag is a stale copy",
			known:   known("1.0.0"),
			fetched: fetched("1.0.0", "1.0.0!"),
		},
		{
			name:    "new version listed twice, first entry wins",
			fetched: fetched("1.0.0!", "1.0.0"),
			want:    []string{"new 1.0.0 prev= bump= yanked"},
			add:     []string{"1.0.0"},
		},
		{
			name:    "all entries blank: nothing removed",
			known:   known("1.0.0"),
			fetched: fetched(" ", ""),
		},
		{
			name:    "equal precedence with a known version: 1.0 then 1.0.0",
			scheme:  pep440.Scheme{},
			known:   known("0.9", "1.0"),
			fetched: fetched("1.0.0", "1.0", "0.9"),
			want:    []string{"new 1.0.0 prev=1.0 bump=other"},
			add:     []string{"1.0.0"},
		},
		{
			name:    "equal precedence with a known version: v1.2.0 then 1.2.0",
			known:   known("1.1.0", "1.2.0"),
			fetched: fetched("v1.2.0", "1.2.0", "1.1.0"),
			want:    []string{"new v1.2.0 prev=1.2.0 bump=other"},
			add:     []string{"v1.2.0"},
		},
		{
			name:    "equal precedence with build metadata",
			known:   known("1.1.0", "1.2.0+a"),
			fetched: fetched("1.2.0+b", "1.2.0+a", "1.1.0"),
			want:    []string{"new 1.2.0+b prev=1.2.0+a bump=other"},
			add:     []string{"1.2.0+b"},
		},
		{
			name:    "equal precedence within one fetch: the earlier entry is the original",
			known:   known("1.1.0"),
			fetched: fetched("v1.2.0", "1.2.0", "1.1.0"),
			want:    []string{"new 1.2.0 prev=v1.2.0 bump=other", "new v1.2.0 prev=1.1.0 bump=minor"},
			add:     []string{"1.2.0", "v1.2.0"},
		},
		{
			name:    "a removed version is not a backport reason",
			known:   known("2.0.0", "3.0.0~"),
			fetched: fetched("2.0.1", "2.0.0"),
			want:    []string{"new 2.0.1 prev=2.0.0 bump=patch"},
			add:     []string{"2.0.1"},
		},
		{
			name:    "a removed version can be the previous one",
			known:   known("1.0.0~", "0.9.0"),
			fetched: fetched("1.0.1", "0.9.0"),
			want:    []string{"new 1.0.1 prev=1.0.0 bump=patch"},
			add:     []string{"1.0.1"},
		},
		{
			name:    "known pre-release above a backport",
			known:   known("1.9.3", "2.0.0-rc.1"),
			fetched: fetched("1.9.4", "2.0.0-rc.1", "1.9.3"),
			want:    []string{"new 1.9.4 prev=1.9.3 bump=patch backport"},
			add:     []string{"1.9.4"},
		},
		{
			name:    "lexical fallback: no backport, no previous across schemes",
			scheme:  schemes.WithFallback(semver.Scheme{Coerce: true}, nil),
			known:   known("1.0.0"),
			fetched: fetched("latest", "1.0.0"),
			want:    []string{"new latest prev= bump="},
			add:     []string{"latest"},
		},
		{
			name:    "known fallback below a first real version",
			scheme:  schemes.WithFallback(semver.Scheme{Coerce: true}, nil),
			known:   known("nightly"),
			fetched: fetched("1.0.0", "nightly"),
			want:    []string{"new 1.0.0 prev= bump="},
			add:     []string{"1.0.0"},
		},
		{
			name:    "two fallbacks compare among themselves",
			known:   known("1.0.0", "edge-1"),
			fetched: fetched("edge-2", "edge-1", "1.0.0"),
			want:    []string{"new edge-2 prev=edge-1 bump=other"},
			add:     []string{"edge-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := tt.scheme
			if scheme == nil {
				scheme = semverFB
			}
			knownCopy := slices.Clone(tt.known)
			r := Compute("t", tt.known, tt.fetched, scheme, t1)
			if got := summaries(r.Changes); !slices.Equal(got, tt.want) {
				t.Errorf("changes:\n got %q\nwant %q", got, tt.want)
			}
			var add []string
			for _, a := range r.Add {
				add = append(add, a.Raw)
				if a.TargetID != "t" || !a.FirstSeenAt.Equal(t1) || !a.RemovedAt.IsZero() {
					t.Errorf("added row %+v", a)
				}
			}
			if !slices.Equal(add, tt.add) {
				t.Errorf("add = %q, want %q", add, tt.add)
			}
			var upd []string
			for _, u := range r.Update {
				upd = append(upd, fmt.Sprintf("%s:%v:%v", u.Raw, u.Yanked, !u.RemovedAt.IsZero()))
				if !u.RemovedAt.IsZero() && !u.RemovedAt.Equal(t1) {
					t.Errorf("removed_at = %v, want %v", u.RemovedAt, t1)
				}
			}
			if !slices.Equal(upd, tt.update) {
				t.Errorf("update = %q, want %q", upd, tt.update)
			}
			if !slices.EqualFunc(tt.known, knownCopy, func(a, b store.Version) bool { return a == b }) {
				t.Error("Compute modified its input")
			}
			if r.IsZero() != (len(tt.want) == 0) {
				t.Errorf("IsZero = %v", r.IsZero())
			}
		})
	}
}

// applyResult returns the known set after writing r over known, as the
// store would.
func applyResult(known []store.Version, r Result) []store.Version {
	out := slices.Clone(known)
	for _, u := range r.Update {
		for i := range out {
			if out[i].Raw == u.Raw {
				out[i] = u
			}
		}
	}
	return append(out, r.Add...)
}

func TestComputeIdempotent(t *testing.T) {
	inputs := [][]source.Version{
		fetched("1.0.0"),
		fetched("2.0.0", "1.9.4", "1.0.0"),
		fetched("2.0.0!", "1.9.4", "1.0.0"),
		fetched("2.0.0", "1.0.0"),
		fetched("2.0.0", "1.9.4", "1.0.0", "nightly"),
		nil,
	}
	scheme := schemes.WithFallback(semver.Scheme{}, nil)
	var set []store.Version
	for i, in := range inputs {
		r := Compute("t", set, in, scheme, t0.Add(time.Duration(i)*time.Minute))
		set = applyResult(set, r)
		if again := Compute("t", set, in, scheme, t1); !again.IsZero() {
			t.Fatalf("input %d: second run changed %q", i, summaries(again.Changes))
		}
	}
}

func TestComputeDetails(t *testing.T) {
	scheme := schemes.WithFallback(pep440.Scheme{}, nil)
	pre := false
	pub := time.Date(2026, 10, 27, 14, 2, 11, 0, time.FixedZone("x", 3600))
	in := []source.Version{
		{Raw: "2.0rc1", Prerelease: &pre, PublishedAt: pub, URL: "https://pypi.org/project/x/2.0rc1/"},
		{Raw: "1.1b1"},
		{Raw: "1.0-RC1"},
		{Raw: "weird tag"},
	}
	r := Compute("t", nil, in, scheme, t0)
	byRaw := map[string]Change{}
	for _, c := range r.Changes {
		byRaw[c.Raw] = c
	}
	if c := byRaw["2.0rc1"]; c.Prerelease || c.Upstream.URL == "" || c.Previous != "1.1b1" || c.Bump != version.BumpMajor {
		t.Errorf("upstream prerelease flag wins: %+v", c)
	}
	if c := byRaw["1.1b1"]; !c.Prerelease || c.Previous != "1.0-RC1" {
		t.Errorf("scheme prerelease: %+v", c)
	}
	if !slices.Equal(r.Fallbacks, []string{"weird tag"}) {
		t.Errorf("fallbacks = %q", r.Fallbacks)
	}
	if byRaw["weird tag"].Version.Scheme() != lexical.Name {
		t.Error("fallback version not lexical")
	}
	for _, a := range r.Add {
		switch a.Raw {
		case "1.0-RC1":
			if a.Normalized != "1.0rc1" || !a.Prerelease {
				t.Errorf("row = %+v", a)
			}
		case "2.0rc1":
			if !a.PublishedAt.Equal(pub) || a.PublishedAt.Location() != time.UTC || a.Prerelease {
				t.Errorf("row = %+v", a)
			}
		}
	}

	// A lexical primary reports no fallbacks.
	if r := Compute("t", nil, fetched("a", "b"), lexical.Scheme{}, t0); len(r.Fallbacks) != 0 || len(r.Add) != 2 {
		t.Errorf("lexical: %+v", r)
	}
	// A scheme without fallback still orders what it cannot parse lexically.
	if r := Compute("t", known("1.0.0"), fetched("1.0.0", "nope", "1.1.0"), semver.Scheme{}, t0); r.Skipped != 0 || len(r.Add) != 2 ||
		!slices.Equal(r.Fallbacks, []string{"nope"}) || r.Usable != 3 {
		t.Errorf("strict: %+v", r)
	}
}

func TestKindNotifies(t *testing.T) {
	for k, want := range map[Kind]bool{KindNew: true, KindYanked: true, KindUnyanked: true, KindRemoved: false, KindReturned: false} {
		if k.Notifies() != want {
			t.Errorf("%s.Notifies() = %v", k, !want)
		}
	}
}
