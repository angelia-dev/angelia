package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// applyBase is the state the plan tests start from: in the scope team=platform
// are the watches pypi-requests, npm-react, pypi-old and the subscriptions
// platform-to-ci, old-sub; out of it the watch web-thing and web-sub.
const applyBase = `version: 1
sinks:
  - {name: platform-ci, kind: webhook, config: {url: "https://ci.example.org/hooks/angelia", secret_env: CI_WEBHOOK_SECRET}}
  - {name: old-sink, kind: webhook, config: {url: "https://old.example.org/hook", secret_env: CI_WEBHOOK_SECRET}}
  - {name: idle-sink, kind: webhook, config: {url: "https://idle.example.org/hook"}}
watches:
  - {name: pypi-requests, source: pypi, coordinates: {name: requests}, interval: 1h, labels: {team: platform}, policy: {prerelease: none, cooldown: 72h}}
  - {name: npm-react, source: npm, coordinates: {name: react}, interval: 2h, labels: {team: platform}, policy: {bump: [major, minor]}}
  - {name: pypi-old, source: pypi, coordinates: {name: six}, labels: {team: platform}}
  - {name: web-thing, source: npm, coordinates: {name: vue}, labels: {team: web}}
subscriptions:
  - {name: platform-to-ci, sink: platform-ci, filter: {labels: {team: platform}}}
  - {name: old-sub, sink: old-sink, filter: {labels: {team: platform}}}
  - {name: web-sub, sink: platform-ci, filter: {labels: {team: web}}}
`

// applyNext changes pypi-requests, adds pypi-flask, a sink and a subscription
// on it, and leaves out pypi-old, old-sub, the two other sinks and the web
// objects.
const applyNext = `version: 1
sinks:
  - name: platform-ci
    kind: webhook
    config: {url: "https://ci.example.org/hooks/angelia", secret_env: CI_WEBHOOK_SECRET}
  - name: chat-new
    kind: webhook
    config: {url: "https://chat.example.org/hook", secret_env: CI_WEBHOOK_SECRET}
watches:
  - name: pypi-requests
    source: pypi
    coordinates: {name: requests}
    interval: 30m
    labels: {team: platform, tier: critical}
    policy: {prerelease: none}
  - {name: npm-react, source: npm, coordinates: {name: react}, interval: 2h, labels: {team: platform}, policy: {bump: [major, minor]}}
  - {name: pypi-flask, source: pypi, coordinates: {name: flask}, labels: {team: platform}}
subscriptions:
  - {name: platform-to-ci, sink: platform-ci, filter: {labels: {team: platform}}}
  - {name: new-sub, sink: chat-new, filter: {labels: {team: platform}, bump: [major]}}
`

type applyEnv struct {
	t    *testing.T
	conn []string
}

func newApplyEnv(t *testing.T, allowedEnv ...string) *applyEnv {
	t.Helper()
	isolate(t)
	if len(allowedEnv) == 0 {
		allowedEnv = []string{"CI_WEBHOOK_SECRET"}
	}
	srv := newE2EServerEnv(t, allowedEnv...)
	return &applyEnv{t: t, conn: []string{"--server", srv.URL, "--token", e2eAdmin}}
}

func (a *applyEnv) run(args ...string) (string, string, error) {
	a.t.Helper()
	return runCLI(a.t, append(append([]string{}, args...), a.conn...)...)
}

func (a *applyEnv) must(args ...string) string {
	a.t.Helper()
	out, errOut, err := a.run(args...)
	if err != nil {
		a.t.Fatalf("angelia %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, out, errOut)
	}
	return out
}

// names lists the names of a kind on the server.
func (a *applyEnv) names(kind string) []string {
	a.t.Helper()
	var p struct {
		Items []struct {
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(a.must(kind, "list", "--all", "-o", "json")), &p); err != nil {
		a.t.Fatal(err)
	}
	var out []string
	for _, it := range p.Items {
		out = append(out, it.Name)
	}
	return out
}

func summary(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1]
}

// Criterion: apply twice = no changes.
func TestApplyTwiceChangesNothing(t *testing.T) {
	a := newApplyEnv(t)
	out := a.must("apply", "-f", filepath.Join("..", "..", "examples", "apply.yaml"))
	if got, want := summary(out), "4 created, 0 updated, 0 unchanged, 0 deleted"; got != want {
		t.Fatalf("first apply: %q, want %q\n%s", got, want, out)
	}
	out = a.must("apply", "-f", filepath.Join("..", "..", "examples", "apply.yaml"), "--prune", "-l", "team=platform")
	if got, want := summary(out), "0 created, 0 updated, 4 unchanged, 0 deleted"; got != want {
		t.Fatalf("second apply: %q, want %q\n%s", got, want, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n")[:4] {
		if !strings.HasSuffix(line, " unchanged") {
			t.Errorf("second apply line %q", line)
		}
	}
}

// Criterion: labels on watches are read from the file.
func TestApplyWatchLabels(t *testing.T) {
	a := newApplyEnv(t)
	a.must("apply", "-f", writeTemp(t, "w.yaml", applyNext))
	out := a.must("watch", "list", "--label", "tier=critical")
	if !strings.Contains(out, "pypi-requests") || strings.Contains(out, "npm-react") {
		t.Errorf("watch list --label tier=critical:\n%s", out)
	}
	out = a.must("watch", "get", "pypi-requests")
	if !strings.Contains(out, "team=platform,tier=critical") {
		t.Errorf("watch get:\n%s", out)
	}
}

// Criteria: the dry run shows a diff and writes nothing; --prune removes the
// objects not in the file, scoped by a label.
func TestApplyDryRunAndPrune(t *testing.T) {
	a := newApplyEnv(t)
	a.must("apply", "-f", writeTemp(t, "base.yaml", applyBase))
	next := writeTemp(t, "next.yaml", applyNext)
	before := map[string][]string{"watch": a.names("watch"), "sink": a.names("sink"), "sub": a.names("sub")}

	for _, format := range []string{formatTable, formatJSON} {
		out := a.must("apply", "-f", next, "--dry-run", "--prune", "-l", "team=platform", "-o", format)
		golden(t, "apply-dry-run."+format, out)
	}
	for kind, names := range before {
		if got := a.names(kind); strings.Join(got, ",") != strings.Join(names, ",") {
			t.Errorf("the dry run changed the %ss: %v, was %v", kind, got, names)
		}
	}

	out := a.must("apply", "-f", next, "--prune", "-l", "team=platform")
	golden(t, "apply-prune.table", out)
	want := map[string]string{
		"watch": "npm-react,pypi-flask,pypi-requests,web-thing",
		"sink":  "chat-new,idle-sink,platform-ci",
		"sub":   "new-sub,platform-to-ci,web-sub",
	}
	for kind, w := range want {
		if got := strings.Join(a.names(kind), ","); got != w {
			t.Errorf("%ss after prune: %s, want %s", kind, got, w)
		}
	}
	out = a.must("apply", "-f", next, "--prune", "-l", "team=platform", "--dry-run")
	if got, want := summary(out), "0 created, 0 updated, 7 unchanged, 0 deleted (dry run: nothing was written)"; got != want {
		t.Errorf("apply after prune: %q, want %q\n%s", got, want, out)
	}
}

// Criterion: the Phase 0 watch file is accepted as it is.
func TestApplyPhase0WatchFile(t *testing.T) {
	a := newApplyEnv(t, "CI_WEBHOOK_SECRET", "CHAT_WEBHOOK_SECRET")
	path := filepath.Join("..", "..", "examples", "watches.yaml")
	out, errOut, err := a.run("apply", "-f", path)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, errOut)
	}
	if got, want := summary(out), "9 created, 0 updated, 0 unchanged, 0 deleted"; got != want {
		t.Errorf("summary %q, want %q\n%s", got, want, out)
	}
	if strings.Contains(errOut, "deprecat") {
		t.Errorf("the Phase 0 format is the apply format, not deprecated: %s", errOut)
	}
	if got := summary(a.must("apply", "-f", path)); got != "0 created, 0 updated, 9 unchanged, 0 deleted" {
		t.Errorf("second apply: %q", got)
	}
}

func TestApplyDirectoryAndStdin(t *testing.T) {
	a := newApplyEnv(t)
	dir := t.TempDir()
	files := map[string]string{
		"10-sinks.yaml": "version: 1\nsinks:\n  - {name: s, kind: webhook, config: {url: \"https://h.example.org/x\"}}\n",
		"20-subs.yml":   "version: 1\nsubscriptions:\n  - {name: sub, sink: s}\n",
		"30-watch.json": `{"version": 1, "watches": [{"name": "w", "source": "pypi", "coordinates": {"name": "requests"}}]}`,
		"README.md":     "not read",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out := a.must("apply", "-f", dir)
	if !strings.Contains(out, "sink/s created\nwatch/w created\nsubscription/sub created\n") {
		t.Errorf("dependency order across files:\n%s", out)
	}

	root := newRootCmd()
	var o, e strings.Builder
	root.SetOut(&o)
	root.SetErr(&e)
	root.SetIn(strings.NewReader(files["10-sinks.yaml"] + "---\n" + files["20-subs.yml"]))
	root.SetArgs(append([]string{"apply", "-f", "-"}, a.conn...))
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatalf("%v: %s", err, e.String())
	}
	if summary(o.String()) != "0 created, 0 updated, 2 unchanged, 0 deleted" {
		t.Errorf("stdin:\n%s", o.String())
	}
}

// A real run stops at the first failure; a dry run reports every failure.
func TestApplyFailure(t *testing.T) {
	a := newApplyEnv(t)
	bad := writeTemp(t, "bad.yaml", `version: 1
sinks:
  - {name: s, kind: webhook, config: {url: "https://h.example.org/x"}}
watches:
  - {name: w1, source: pypi, coordinates: {name: requests}, interval: 1m}
  - {name: w2, source: pypi, coordinates: {name: flask}}
subscriptions:
  - {name: sub, sink: s}
`)
	out, _, err := a.run("apply", "-f", bad, "--dry-run")
	if err == nil || !strings.Contains(err.Error(), "1 of 4 objects failed") {
		t.Fatalf("dry run err = %v\n%s", err, out)
	}
	if !strings.Contains(out, "watch/w1 failed\n    422") || !strings.Contains(out, "watch/w2 created") {
		t.Errorf("dry run output:\n%s", out)
	}
	if !strings.Contains(out, `subscription/sub created (sink "s" is created by this apply; not validated by the server)`) {
		t.Errorf("dry run output:\n%s", out)
	}
	out, _, err = a.run("apply", "-f", bad)
	if err == nil || !strings.Contains(err.Error(), `apply stopped at watch "w1"`) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "sink/s created\nwatch/w1 failed") || strings.Contains(out, "w2") {
		t.Errorf("output:\n%s", out)
	}
	if got := a.names("watch"); len(got) != 0 {
		t.Errorf("watches after the failed apply: %v", got)
	}
	if got := a.names("sink"); strings.Join(got, ",") != "s" {
		t.Errorf("sinks after the failed apply: %v (the objects before the failure stay)", got)
	}
}

func TestApplyInputErrors(t *testing.T) {
	isolate(t)
	conn := []string{"--server", "http://127.0.0.1:1", "--token", e2eAdmin}
	empty := t.TempDir()
	dup := t.TempDir()
	for _, n := range []string{"a.yaml", "b.yaml"} {
		if err := os.WriteFile(filepath.Join(dup, n), []byte("version: 1\nwatches:\n  - {name: w, source: pypi}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"prune without scope", []string{"--prune"}, []string{"--prune needs a label scope"}},
		{"scope without prune", []string{"-l", "a=b"}, []string{"-l scopes --prune"}},
		{"bad selector", []string{"--prune", "-l", "ab"}, []string{`-l "ab": want key=value`}},
		{"selector twice", []string{"--prune", "-l", "a=b", "-l", "a=c"}, []string{`label "a" given twice`}},
		{"no version", []string{"-f", writeTemp(t, "f.yaml", "watches: []\n")}, []string{"version: required"}},
		{"wrong version", []string{"-f", writeTemp(t, "f.yaml", "version: 2\n")}, []string{"version: must be 1"}},
		{"empty file", []string{"-f", writeTemp(t, "f.yaml", "")}, []string{"empty: a watch file needs"}},
		{"not a mapping", []string{"-f", writeTemp(t, "f.yaml", "- a\n")}, []string{"must be a mapping"}},
		{"unknown key and bad items", []string{"-f", writeTemp(t, "f.yaml", "version: 1\nkind: Watch\nsinks: {}\nwatches:\n  - x\n  - {name: Bad}\n  - {source: pypi}\n")}, []string{
			"kind: unknown key", "sinks: must be a list", "watches[0]: must be an object", "watches[1]: name: required", "watches[2]: name: required",
		}},
		{"duplicate across files", []string{"-f", dup}, []string{`b.yaml: watches[0]: name: duplicate watch "w" (also at ` + filepath.Join(dup, "a.yaml") + ": watches[0])"}},
		{"empty directory", []string{"-f", empty}, []string{"no .yaml, .yml or .json file"}},
		{"missing file", []string{"-f", filepath.Join(empty, "nope.yaml")}, []string{"no such file"}},
		{"yaml error", []string{"-f", writeTemp(t, "f.yaml", "a: [")}, []string{"parse resource file"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := append([]string{"apply"}, c.args...)
			if !strings.Contains(strings.Join(c.args, " "), "-f") {
				args = append(args, "-f", writeTemp(t, "ok.yaml", "version: 1\n"))
			}
			_, _, err := runCLI(t, append(args, conn...)...)
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

func TestApplyWarnsOutOfScope(t *testing.T) {
	a := newApplyEnv(t)
	_, errOut, err := a.run("apply", "-f", writeTemp(t, "f.yaml", applyBase), "--prune", "-l", "team=platform", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`watch "web-thing": its labels lack team=platform`, `subscription "web-sub": its filter.labels lack team=platform`} {
		if !strings.Contains(errOut, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errOut)
		}
	}
	if strings.Contains(errOut, `"pypi-requests"`) {
		t.Errorf("warned about an object in scope:\n%s", errOut)
	}
}

func TestPruneSetScope(t *testing.T) {
	api, file := "api", "file"
	watch := func(owner string, labels map[string]any) map[string]any {
		return map[string]any{"managed_by": owner, "labels": labels}
	}
	sub := func(owner, sink string, labels map[string]any) map[string]any {
		return map[string]any{"managed_by": owner, "sink": sink, "filter": map[string]any{"labels": labels}}
	}
	sink := func(owner string) map[string]any { return map[string]any{"managed_by": owner} }
	p := map[string]any{"team": "p"}
	r := &applyRun{sel: map[string]string{"team": "p"}}
	r.current[kWatch] = map[string]map[string]any{
		"in-file": watch(api, p), "gone": watch(api, p), "other-team": watch(api, map[string]any{"team": "q"}),
		"by-file": watch(file, p), "more-labels": watch(api, map[string]any{"team": "p", "x": "y"}),
	}
	r.current[kSub] = map[string]map[string]any{
		"kept": sub(api, "moved-from", p), "gone": sub(api, "only-gone", p), "other": sub(api, "shared", map[string]any{"team": "q"}),
		"gone2": sub(api, "shared", p), "by-file": sub(file, "file-sink", p),
	}
	r.current[kSink] = map[string]map[string]any{
		"moved-from": sink(api), "only-gone": sink(api), "shared": sink(api), "unused": sink(api), "file-sink": sink(file),
		"in-file": sink(api),
	}
	set := &applySet{}
	for k := range set.origins {
		set.origins[k] = map[string]string{}
	}
	_ = set.addObject(kWatch, map[string]any{"name": "in-file"}, "f")
	_ = set.addObject(kSub, map[string]any{"name": "kept", "sink": "in-file"}, "f")
	_ = set.addObject(kSink, map[string]any{"name": "in-file"}, "f")
	got := r.pruneSet(set)
	want := [numKinds]string{
		kSink:  "moved-from,only-gone",
		kWatch: "gone,more-labels",
		kSub:   "gone,gone2",
	}
	for k := range want {
		if g := strings.Join(got[k], ","); g != want[k] {
			t.Errorf("%s: pruned %q, want %q", applyKinds[k].name, g, want[k])
		}
	}
}

func TestDiffWriteKeys(t *testing.T) {
	old := map[string]any{"interval": "1h", "labels": map[string]any{"a": "1", "b": "2"}, "id": "X", "policy": map[string]any{"bump": []any{"major"}}}
	cur := map[string]any{"interval": "30m", "labels": map[string]any{"a": "1", "c": "3"}, "id": "Y", "policy": map[string]any{"bump": []any{"major"}}}
	got := diffWriteKeys(old, cur, []string{"interval", "labels", "policy"})
	b, _ := json.Marshal(got)
	want := `[{"path":"interval","old":"1h","new":"30m"},{"path":"labels.b","old":"2","new":null},{"path":"labels.c","old":null,"new":"3"}]`
	if string(b) != want {
		t.Errorf("diff = %s\nwant   %s", b, want)
	}
}

// A server that does not send Angelia-Change still gets a report.
func TestApplyWithoutChangeHeader(t *testing.T) {
	isolate(t)
	srv := newFixtureServer(t, map[string]string{
		"GET /v1/sinks":            "sinks",
		"GET /v1/watches":          "watches",
		"GET /v1/subscriptions":    "subs",
		"PUT /v1/watches/requests": "watch",
		"PUT /v1/watches/new":      "watch",
	})
	f := writeTemp(t, "f.yaml", "version: 1\nwatches:\n  - {name: requests, source: pypi, coordinates: {name: requests}}\n  - {name: new, source: pypi, coordinates: {name: x}}\n")
	out, _, err := runCLI(t, "apply", "-f", f, "--server", srv.URL, "--token", testToken)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "watch/requests updated (no difference in the fields the API shows)\nwatch/new created\n") {
		t.Errorf("output:\n%s", out)
	}
	if r := srv.last(); r.Method != http.MethodPut || r.ContentType != jsonContentType || !strings.Contains(r.Body, `"name":"new"`) {
		t.Errorf("last request %+v", r)
	}
}
