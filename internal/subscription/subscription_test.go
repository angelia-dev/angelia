package subscription_test

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/subscription"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

var base = subscription.Input{
	Source: "npm", Package: "@angular/core", Bump: "minor", Type: "release.published",
	Labels: map[string]string{"team": "web", "tier": "critical"},
}

func TestFilterMatch(t *testing.T) {
	tests := []struct {
		name string
		f    subscription.Filter
		mod  func(*subscription.Input)
		want bool
	}{
		{"empty filter matches everything", subscription.Filter{}, nil, true},
		{"empty filter matches zero input", subscription.Filter{}, func(i *subscription.Input) { *i = subscription.Input{} }, true},

		{"source hit", subscription.Filter{Source: []string{"pypi", "npm"}}, nil, true},
		{"source miss", subscription.Filter{Source: []string{"pypi"}}, nil, false},

		{"type hit", subscription.Filter{Type: []string{"release.published"}}, nil, true},
		{"type miss", subscription.Filter{Type: []string{"release.yanked"}}, nil, false},

		{"bump hit", subscription.Filter{Bump: []string{"major", "minor"}}, nil, true},
		{"bump miss", subscription.Filter{Bump: []string{"major"}}, nil, false},
		{"bump filter vs unclassified bump", subscription.Filter{Bump: []string{"major", "minor", "patch"}}, func(i *subscription.Input) { i.Bump = "" }, false},

		{"labels subset hit", subscription.Filter{Labels: map[string]string{"team": "web"}}, nil, true},
		{"labels all hit", subscription.Filter{Labels: map[string]string{"team": "web", "tier": "critical"}}, nil, true},
		{"labels value differs", subscription.Filter{Labels: map[string]string{"team": "platform"}}, nil, false},
		{"labels key missing", subscription.Filter{Labels: map[string]string{"owner": "x"}}, nil, false},
		{"labels partial miss", subscription.Filter{Labels: map[string]string{"team": "web", "tier": "low"}}, nil, false},
		{"labels on input without labels", subscription.Filter{Labels: map[string]string{"team": "web"}}, func(i *subscription.Input) { i.Labels = nil }, false},
		{"labels value is exact, not prefix", subscription.Filter{Labels: map[string]string{"team": "we"}}, nil, false},

		{"glob scope hit", subscription.Filter{Package: "@angular/*"}, nil, true},
		{"glob scope miss", subscription.Filter{Package: "@angular/*"}, func(i *subscription.Input) { i.Package = "@types/node" }, false},
		{"glob star does not cross slash", subscription.Filter{Package: "@angular/*"}, func(i *subscription.Input) { i.Package = "@angular/core/testing" }, false},
		{"glob exact", subscription.Filter{Package: "react"}, func(i *subscription.Input) { i.Package = "react" }, true},
		{"glob prefix star", subscription.Filter{Package: "re*"}, func(i *subscription.Input) { i.Package = "requests" }, true},
		{"glob question mark", subscription.Filter{Package: "re?"}, func(i *subscription.Input) { i.Package = "red" }, true},
		{"glob class", subscription.Filter{Package: "[a-c]*"}, func(i *subscription.Input) { i.Package = "django" }, false},
		{"glob case sensitive", subscription.Filter{Package: "Django"}, func(i *subscription.Input) { i.Package = "django" }, false},
		{"glob malformed matches nothing", subscription.Filter{Package: "[a"}, nil, false},

		{"combination all hit", subscription.Filter{
			Source: []string{"npm"}, Labels: map[string]string{"team": "web"}, Bump: []string{"minor"},
			Package: "@angular/*", Type: []string{"release.published"},
		}, nil, true},
		{"combination one key misses (bump)", subscription.Filter{
			Source: []string{"npm"}, Labels: map[string]string{"team": "web"}, Bump: []string{"major"},
			Package: "@angular/*", Type: []string{"release.published"},
		}, nil, false},
		{"combination one key misses (package)", subscription.Filter{
			Source: []string{"npm"}, Labels: map[string]string{"team": "web"}, Bump: []string{"minor"},
			Package: "@types/*", Type: []string{"release.published"},
		}, nil, false},
		{"combination source and labels", subscription.Filter{
			Source: []string{"pypi"}, Labels: map[string]string{"team": "web"},
		}, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			if tc.mod != nil {
				tc.mod(&in)
			}
			if got := tc.f.Match(in); got != tc.want {
				t.Fatalf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFromJSON(t *testing.T) {
	for _, empty := range []string{"", "  ", "null", "{}"} {
		f, err := subscription.FromJSON([]byte(empty))
		if err != nil || !f.Match(base) {
			t.Fatalf("FromJSON(%q) = %+v, %v; want match-all", empty, f, err)
		}
	}
	// The exact form the watch-file loader stores.
	f, err := subscription.FromJSON([]byte(`{"source":["npm"],"labels":{"team":"web"},"bump":["minor"],"package":"@angular/*","type":["release.published"]}`))
	if err != nil || !f.Match(base) {
		t.Fatalf("stored filter: %+v, %v", f, err)
	}
	for _, bad := range []string{`{"ecosystem":["npm"]}`, `{"package":"[a"}`, `{"bump":"major"}`, `[`} {
		if _, err := subscription.FromJSON([]byte(bad)); err == nil {
			t.Errorf("FromJSON(%s): want error", bad)
		}
	}
}

// TestFromWatchFile loads a real watch file and matches against it.
func TestFromWatchFile(t *testing.T) {
	const src = `version: 1
sinks:
  - {name: s, kind: webhook, config: {url: "https://example.org/h"}}
subscriptions:
  - name: all
    sink: s
  - name: web
    sink: s
    filter:
      source: [npm]
      labels: {team: web}
      bump: [minor]
      package: "@angular/*"
`
	reg := source.NewRegistry()
	if err := reg.Register(npm.New("", &http.Client{})); err != nil {
		t.Fatal(err)
	}
	file, err := watchfile.Parse([]byte(src), "w.yaml", watchfile.Options{Sources: reg})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var subs []subscription.Subscription
	for _, s := range file.Subscriptions {
		subs = append(subs, subscription.Subscription{ID: s.Name, Filter: subscription.FromWatchFile(s.Filter)})
	}
	m := subscription.NewMatcher(subs)
	// "all" has no filter in the file; the loader defaults type to release.published.
	if got := m.Match(base, nil); fmt.Sprint(got) != "[all web]" {
		t.Fatalf("matches = %v", got)
	}
	yanked := base
	yanked.Type = "release.yanked"
	if got := m.Match(yanked, nil); len(got) != 0 {
		t.Fatalf("yanked matches = %v, want none (default type is release.published)", got)
	}
}

func TestMatcherOrderAndAppend(t *testing.T) {
	m := subscription.NewMatcher([]subscription.Subscription{
		{ID: "a"}, {ID: "b", Filter: subscription.Filter{Source: []string{"pypi"}}}, {ID: "c"},
	})
	if m.Len() != 3 {
		t.Fatal("Len")
	}
	got := m.Match(base, []string{"x"})
	if fmt.Sprint(got) != "[x a c]" {
		t.Fatalf("got %v", got)
	}
}

// BenchmarkMatcher10k matches one event against 10,000 subscriptions with
// mixed filters.
func BenchmarkMatcher10k(b *testing.B) {
	subs := make([]subscription.Subscription, 10000)
	for i := range subs {
		var f subscription.Filter
		switch i % 5 {
		case 0: // match-all
		case 1:
			f = subscription.Filter{Source: []string{"npm", "pypi"}, Bump: []string{"major", "minor"}}
		case 2:
			f = subscription.Filter{Labels: map[string]string{"team": fmt.Sprintf("team-%d", i%50)}}
		case 3:
			f = subscription.Filter{Package: fmt.Sprintf("@scope%d/*", i%100), Type: []string{"release.published"}}
		case 4:
			f = subscription.Filter{Source: []string{"npm"}, Labels: map[string]string{"team": "web"}, Package: "@angular/*"}
		}
		subs[i] = subscription.Subscription{ID: fmt.Sprint(i), Filter: f}
	}
	m := subscription.NewMatcher(subs)
	dst := make([]string, 0, len(subs))
	b.ReportAllocs()
	for b.Loop() {
		dst = m.Match(base, dst[:0])
	}
	if len(dst) == 0 {
		b.Fatal("no matches")
	}
}
