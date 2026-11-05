package apply

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFileDirectoryStdin(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "10-sinks.yaml", "version: 1\nsinks:\n  - {name: s, kind: webhook}\n")
	write(t, dir, "20-subs.yml", "version: 1\nsubscriptions:\n  - {name: sub, sink: s, filter: {labels: {team: p}}}\n")
	write(t, dir, "30-watch.json", `{"version": 1, "watches": [{"name": "w", "source": "pypi", "labels": {"team": "p"}}]}`)
	write(t, dir, "README.md", "not read")
	other := write(t, t.TempDir(), "x.yaml", "version: 1\nwatches:\n  - {name: x}\n")
	set, err := Read(strings.NewReader("version: 1\nwatches:\n  - {name: from-stdin}\n"), []string{dir, other, "-"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for k := range set.Objects {
		for _, o := range set.Objects[k] {
			names = append(names, o.Name)
		}
	}
	if got := strings.Join(names, ","); got != "s,w,x,from-stdin,sub" {
		t.Errorf("objects %s", got)
	}
	sub := set.Objects[Subscription][0]
	if sub.Sink != "s" || sub.Labels["team"] != "p" || !strings.HasSuffix(sub.Origin, "20-subs.yml: subscriptions[0]") {
		t.Errorf("subscription %+v", sub)
	}
	w := set.Objects[Watch][0]
	if w.Labels["team"] != "p" || string(w.Body) != `{"labels":{"team":"p"},"name":"w","source":"pypi"}` {
		t.Errorf("watch %+v body %s", w, w.Body)
	}
	if !set.Has(Sink, "s") || set.Has(Watch, "s") || set.Objects[Watch][2].Origin != "stdin: watches[0]" {
		t.Errorf("Has or origin wrong: %+v", set.Objects[Watch])
	}
}

func TestReadErrors(t *testing.T) {
	dir := t.TempDir()
	dup := t.TempDir()
	write(t, dup, "a.yaml", "version: 1\nwatches:\n  - {name: w}\n")
	write(t, dup, "b.yaml", "version: 1\nwatches:\n  - {name: w}\n")
	big := write(t, dir, "big.yaml", "version: 1\n#"+strings.Repeat("x", 4<<20))
	f := func(content string) string { return write(t, t.TempDir(), "f.yaml", content) }
	cases := []struct {
		name  string
		paths []string
		stdin string
		want  []string
	}{
		{"no version", []string{f("watches: []\n")}, "", []string{"version: required"}},
		{"wrong version", []string{f("version: 2\n")}, "", []string{"version: must be 1"}},
		{"empty", []string{f("")}, "", []string{"empty: a watch file needs"}},
		{"null document", []string{f("---\n")}, "", []string{"empty: a watch file needs"}},
		{"not a mapping", []string{f("- a\n")}, "", []string{"must be a mapping"}},
		{"several documents", []string{f("version: 1\n---\nversion: 1\n")}, "", []string{"several YAML documents: a watch file holds one document"}},
		{"several documents on stdin", []string{"-"}, "version: 1\n---\nversion: 1\n", []string{"stdin: several YAML documents"}},
		{"bad second document", []string{f("version: 1\n---\na: [\n")}, "", []string{"parse:"}},
		{"yaml error", []string{f("a: [")}, "", []string{"parse:"}},
		{"structure", []string{f("version: 1\nkind: Watch\nsinks: {}\nwatches:\n  - x\n  - {name: Bad}\n  - {source: pypi}\n")}, "", []string{
			"kind: unknown key", "sinks: must be a list", "watches[0]: must be an object", "watches[1]: name: required", "watches[2]: name: required",
		}},
		{"labels", []string{f("version: 1\nwatches:\n  - {name: a, labels: {tier: 1, team: p}}\n  - {name: b, labels: [x]}\nsubscriptions:\n  - {name: s, sink: k, filter: {labels: {on: true}}}\n")}, "", []string{
			"watches[0].labels.tier: must be a string", "watches[1].labels: must be a mapping", "subscriptions[0].filter.labels.on: must be a string",
		}},
		{"duplicate across files", []string{dup}, "", []string{`b.yaml: watches[0]: name: duplicate watch "w" (also at ` + filepath.Join(dup, "a.yaml") + ": watches[0])"}},
		{"empty directory", []string{t.TempDir()}, "", []string{"no .yaml, .yml or .json file"}},
		{"missing file", []string{filepath.Join(dir, "nope.yaml")}, "", []string{"no such file"}},
		{"too large", []string{big}, "", []string{"larger than the 4 MiB limit"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Read(strings.NewReader(c.stdin), c.paths)
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestSelector(t *testing.T) {
	sel, err := ParseSelector([]string{"team=p", "tier=a=b", "team=p"})
	if err != nil {
		t.Fatal(err)
	}
	if sel.String() != "team=p,tier=a=b" {
		t.Errorf("String() = %q", sel)
	}
	if !sel.Matches(map[string]string{"team": "p", "tier": "a=b", "x": "y"}) || sel.Matches(map[string]string{"team": "p"}) {
		t.Error("Matches")
	}
	for _, bad := range [][]string{{"ab"}, {"=v"}, {"a=b", "a=c"}} {
		if _, err := ParseSelector(bad); err == nil {
			t.Errorf("ParseSelector(%q): no error", bad)
		}
	}
}

func setOf(t *testing.T, doc string) *Set {
	t.Helper()
	set, err := Read(strings.NewReader(doc), []string{"-"})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func TestScopeOfFiles(t *testing.T) {
	set := setOf(t, "version: 1\nwatches:\n  - {name: a, labels: {team: p}}\n  - {name: b, labels: {team: q}}\n  - {name: c}\n")
	sel := Selector{"team": "p"}
	if n := set.WatchesInScope(sel); n != 1 {
		t.Errorf("WatchesInScope = %d", n)
	}
	w := set.OutOfScope(sel)
	if len(w) != 2 || !strings.Contains(w[0], `watch "b": its labels lack team=p`) || !strings.Contains(w[1], `"c"`) {
		t.Errorf("OutOfScope = %q", w)
	}
}

func TestPruneWatchesAndKept(t *testing.T) {
	p := map[string]any{"team": "p"}
	watch := func(owner string, labels map[string]any) Resource {
		return Resource{"managed_by": owner, "labels": labels}
	}
	sub := func(owner, sink string, labels map[string]any) Resource {
		return Resource{"managed_by": owner, "sink": sink, "filter": map[string]any{"labels": labels}}
	}
	sink := func(owner string) Resource { return Resource{"managed_by": owner} }
	var cur Current
	cur[Watch] = map[string]Resource{
		"in-file": watch("api", p), "gone": watch("api", p), "other-team": watch("api", map[string]any{"team": "q"}),
		"by-file": watch("file", p), "more-labels": watch("api", map[string]any{"team": "p", "x": "y"}), "no-labels": watch("api", nil),
	}
	// another team's subscription that listens to team p: never deleted
	cur[Subscription] = map[string]Resource{
		"in-file": sub("api", "moved-from", p), "listens-to-p": sub("api", "web-chat", p), "other": sub("api", "shared", map[string]any{"team": "q"}),
		"by-file": sub("file", "file-sink", p),
	}
	cur[Sink] = map[string]Resource{
		"moved-from": sink("api"), "web-chat": sink("api"), "shared": sink("api"), "unused": sink("api"), "file-sink": sink("file"),
		"unused-by-file": sink("file"), "in-file": sink("api"),
	}
	set := setOf(t, "version: 1\nsinks: [{name: in-file}]\nwatches: [{name: in-file}]\nsubscriptions: [{name: in-file, sink: in-file}]\n")
	sel := Selector{"team": "p"}

	if got := strings.Join(PruneWatches(cur, set, sel), ","); got != "gone,more-labels" {
		t.Errorf("PruneWatches = %s", got)
	}
	var got []string
	for _, c := range Kept(cur, set, sel) {
		if c.Change != ChangeKept || !strings.Contains(c.Note, "angelia "+map[string]string{"sink": "sink", "subscription": "sub"}[c.Kind]+" delete "+c.Name) {
			t.Errorf("kept %+v", c)
		}
		got = append(got, c.Kind+"/"+c.Name)
	}
	// unused is idle but no subscription of the scope used it: another team's
	if s := strings.Join(got, ","); s != "subscription/listens-to-p,sink/moved-from" {
		t.Errorf("Kept = %s", s)
	}
	if !Prunable(cur[Watch]["gone"], sel) || Prunable(cur[Watch]["by-file"], sel) || Prunable(cur[Watch]["other-team"], sel) {
		t.Error("Prunable")
	}
}

func TestDiffWriteKeys(t *testing.T) {
	old := Resource{"interval": "1h", "labels": map[string]any{"a": "1", "b": "2"}, "id": "X", "policy": map[string]any{"bump": []any{"major"}}}
	cur := Resource{"interval": "30m", "labels": map[string]any{"a": "1", "c": "3"}, "id": "Y", "policy": map[string]any{"bump": []any{"major"}}}
	got := DiffWriteKeys(old, cur, []string{"interval", "labels", "policy"})
	b, _ := json.Marshal(got)
	want := `[{"path":"interval","old":"1h","new":"30m"},{"path":"labels.b","old":"2","new":null},{"path":"labels.c","old":null,"new":"3"}]`
	if string(b) != want {
		t.Errorf("diff = %s\nwant   %s", b, want)
	}
	if CompactJSON(nil) != "(none)" || CompactJSON("<a>") != `"<a>"` || CompactJSON(func() {}) == "" {
		t.Error("CompactJSON")
	}
}

func TestReportSummary(t *testing.T) {
	r := Report{Changes: []Change{{Change: ChangeCreated}, {Change: ChangeGone}, {Change: ChangeKept}, {Change: ChangeFailed}}}
	if s := r.Summary(); s != "1 created, 0 updated, 0 unchanged, 0 deleted, 1 gone, 1 kept, 1 failed" {
		t.Errorf("Summary = %q", s)
	}
	r = Report{DryRun: true}
	if s := r.Summary(); s != "0 created, 0 updated, 0 unchanged, 0 deleted (dry run: nothing was written)" {
		t.Errorf("Summary = %q", s)
	}
}

func TestKinds(t *testing.T) {
	k := Kinds()
	if k[Sink].Section != "sinks" || k[Watch].Name != "watch" || k[Subscription].WriteKeys[0] != "sink" {
		t.Errorf("Kinds = %+v", k)
	}
}
