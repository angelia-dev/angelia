package manifest

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/apply"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

// update rewrites the golden files: go test ./internal/manifest -update
var update = flag.Bool("update", false, "rewrite the golden files in testdata/golden")

func dep(name, typ string) Dependency {
	return Dependency{Name: name, Kind: KindPyPI, Type: typ, Origin: Origin{File: "requirements.txt", Line: 1}}
}

func gen(t *testing.T, deps []Dependency, o Options) *Output {
	t.Helper()
	out, err := Generate(deps, o)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func names(out *Output) []string {
	var n []string
	for _, e := range out.Entries {
		if !e.Excluded {
			n = append(n, e.Watch)
		}
	}
	return n
}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".yaml")
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil { //nolint:gosec // test data
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path) //nolint:gosec // test data
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (go test -update rewrites it):\n%s", name, got)
	}
}

// loads checks that both loaders of the watch file accept data: the parser of
// serve --watches (with a subscription's sink added to the file) and the
// reader of apply, from stdin.
func loads(t *testing.T, data []byte) {
	t.Helper()
	doc := string(data)
	if strings.Contains(doc, "subscriptions:") {
		// A --watches file needs the subscription's sink in the same file.
		doc = strings.Replace(doc, "watches:", "sinks:\n  - {name: ci, kind: webhook, config: {url: \"https://example.com/hook\"}}\nwatches:", 1)
	}
	opts := probeOptions()
	if err := opts.Sources.RegisterAs("pypi-internal", pypi.New("https://pypi.example.com", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := watchfile.Parse([]byte(doc), "import.yaml", opts); err != nil {
		t.Errorf("watchfile.Parse: %v\n%s", err, data)
	}
	set, err := apply.Read(bytes.NewReader(data), []string{"-"})
	if err != nil {
		t.Fatalf("apply.Read: %v", err)
	}
	if got, want := len(set.Objects[apply.Watch]), strings.Count(string(data), "\n  - name:")-strings.Count(string(data), "-deps"); got != want {
		t.Errorf("apply.Read found %d watches, the file has %d", got, want)
	}
}

func basicDeps(t *testing.T) []Dependency {
	t.Helper()
	a, err := ParseRequirements(reqFS(), "basic.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParseRequirements(reqFS(), "requirements-dev.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	return append(a.Deps, b.Deps...)
}

// Criterion: golden output, byte-identical on two runs and with the inputs in
// another order; both loaders accept it.
func TestGoldenRequirements(t *testing.T) {
	deps := basicDeps(t)
	o := Options{Project: "api", Inputs: []string{"basic.txt", "requirements-dev.txt"}}
	first := gen(t, deps, o)
	checkGolden(t, "requirements-basic", first.YAML)
	loads(t, first.YAML)

	if again := gen(t, deps, o); !bytes.Equal(first.YAML, again.YAML) {
		t.Error("two runs differ")
	}
	rev := append([]Dependency(nil), deps...)
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	o.Inputs = []string{"requirements-dev.txt", "basic.txt", "basic.txt"}
	if other := gen(t, rev, o); !bytes.Equal(first.YAML, other.YAML) {
		t.Errorf("another input order differs:\n%s", other.YAML)
	}
}

// Criterion: every option at once.
func TestGoldenAllOptions(t *testing.T) {
	policy, err := os.ReadFile("testdata/policy/ok.yaml")
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Project: "api", Labels: map[string]string{"project": "api", "team": "platform"},
		Sources: map[string]string{"pypi": "pypi-internal"}, Interval: "6h", Exclude: []string{"py*", "Django"},
		Policy: policy, PolicyName: "ok.yaml", Subscribe: "ci", Inputs: []string{"basic.txt", "requirements-dev.txt"},
	}
	out := gen(t, basicDeps(t), o)
	checkGolden(t, "requirements-options", out.YAML)
	loads(t, out.YAML)
	var ex []string
	for _, e := range out.Entries {
		if e.Excluded {
			ex = append(ex, e.Dep.Name)
		}
	}
	if strings.Join(ex, ",") != "PyYAML,Django,pytest" && strings.Join(ex, ",") != "Django,PyYAML,pytest" {
		t.Errorf("excluded = %v", ex)
	}
}

// Criterion: naming per D61.
func TestWatchNames(t *testing.T) {
	long := strings.Repeat("a", 80)
	longer := strings.Repeat("a", 79) + "b"
	tests := []struct{ name, want string }{
		{"requests", "api-pypi-requests"},
		{"zope.interface", "api-pypi-zope-interface"},
		{"My_Package", "api-pypi-my-package"},
		{"Flask", "api-pypi-flask"},
		{"a--b__c", "api-pypi-a-b-c"},
	}
	for _, tc := range tests {
		out := gen(t, []Dependency{dep(tc.name, TypeRuntime)}, Options{Project: "api"})
		if got := names(out); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s -> %v, want %s", tc.name, got, tc.want)
		}
	}
	// Over-long: cut to 54, "-", 8 hex digits; stable; distinct per name.
	var cut []string
	for _, n := range []string{long, longer} {
		a := names(gen(t, []Dependency{dep(n, TypeRuntime)}, Options{Project: "api"}))[0]
		b := names(gen(t, []Dependency{dep(n, TypeRuntime)}, Options{Project: "api"}))[0]
		if a != b || len(a) != 63 || !watchfile.ValidName(a) {
			t.Errorf("long name %q (%d), second run %q", a, len(a), b)
		}
		cut = append(cut, a)
	}
	if cut[0][:54] != cut[1][:54] || cut[0] == cut[1] {
		t.Errorf("the two long names should share a prefix and differ in the hash: %v", cut)
	}
	if want := "api-pypi-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-"; !strings.HasPrefix(cut[0], want[:54]) {
		t.Errorf("cut name %q", cut[0])
	}
	// A name that fits is never hashed, even at exactly 63.
	exact := strings.Repeat("b", 63-len("api-pypi-"))
	if got := names(gen(t, []Dependency{dep(exact, TypeRuntime)}, Options{Project: "api"}))[0]; got != "api-pypi-"+exact {
		t.Errorf("exact fit: %q", got)
	}
}

// Criterion: a collision in one run fails and names both dependencies.
func TestWatchNameCollision(t *testing.T) {
	kinds["fake"] = kindSpec{canonical: func(n string) string { return n }}
	t.Cleanup(func() { delete(kinds, "fake") })
	a := Dependency{Name: "A.b", Kind: "fake", Type: TypeRuntime, Origin: Origin{File: "one.txt", Line: 3}}
	b := Dependency{Name: "a.b", Kind: "fake", Type: TypeRuntime, Origin: Origin{File: "two.txt", Line: 9}}
	_, err := Generate([]Dependency{a, b}, Options{Project: "api"})
	if err == nil || !strings.Contains(err.Error(), "fake A.b (one.txt:3) and fake a.b (two.txt:9) both map to the watch name \"api-fake-a.b\"") {
		t.Errorf("err = %v", err)
	}
}

// Criterion: --project with an invalid value fails.
func TestInvalidProject(t *testing.T) {
	for _, p := range []string{"", "Api", "-api", "a_b", strings.Repeat("a", 25), "a b"} {
		if _, err := Generate(nil, Options{Project: p}); err == nil || !strings.Contains(err.Error(), "--project") {
			t.Errorf("project %q: %v", p, err)
		}
	}
	if _, err := Generate(nil, Options{Project: strings.Repeat("a", 24)}); err != nil {
		t.Errorf("24 characters: %v", err)
	}
}

// Criterion: labels per D62.
func TestLabels(t *testing.T) {
	deps := []Dependency{dep("a", TypeRuntime), dep("b", TypeDev), dep("c", TypeOptional), dep("d", TypePeer)}
	out := gen(t, deps, Options{Project: "api", Include: []string{"runtime", "dev", "optional", "peer"}})
	got := string(out.YAML)
	for _, w := range []string{
		"name: api-pypi-a\n    source: pypi\n    coordinates:\n      name: a\n    labels:\n      dependency: runtime\n      project: api\n",
		"dependency: dev\n", "dependency: optional\n", "dependency: peer\n",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %q in\n%s", w, got)
		}
	}
	// -l replaces the default project label.
	out = gen(t, deps[:1], Options{Project: "api", Labels: map[string]string{"team": "platform"}})
	if g := string(out.YAML); !strings.Contains(g, "team: platform") || strings.Contains(g, "project:") {
		t.Errorf("-l: %s", g)
	}
	// Precedence: runtime, optional, dev, peer.
	for _, tc := range []struct {
		types []string
		want  string
	}{
		{[]string{TypePeer, TypeDev, TypeOptional, TypeRuntime}, "runtime"},
		{[]string{TypePeer, TypeDev, TypeOptional}, "optional"},
		{[]string{TypePeer, TypeDev}, "dev"},
		{[]string{TypePeer}, "peer"},
	} {
		var ds []Dependency
		for _, ty := range tc.types {
			ds = append(ds, dep("x", ty))
		}
		g := string(gen(t, ds, Options{Project: "api", Include: Types}).YAML)
		if strings.Count(g, "name: api-pypi-x") != 1 || !strings.Contains(g, "dependency: "+tc.want) {
			t.Errorf("%v: want %s in\n%s", tc.types, tc.want, g)
		}
	}
	// Invalid labels are refused before output, even with no watch.
	for _, l := range []map[string]string{{"bad key": "x"}, {"k": strings.Repeat("v", 254)}, {"k": "a\nb"}, {"dependency": "x"}} {
		if _, err := Generate(deps, Options{Project: "api", Labels: l}); err == nil {
			t.Errorf("labels %v accepted", l)
		}
		if _, err := Generate(nil, Options{Project: "api", Labels: l}); err == nil {
			t.Errorf("labels %v accepted without watches", l)
		}
	}
}

// Criterion: --include filters; the default leaves peer out.
func TestInclude(t *testing.T) {
	deps := []Dependency{dep("a", TypeRuntime), dep("b", TypeDev), dep("c", TypeOptional), dep("d", TypePeer)}
	for _, tc := range []struct {
		include []string
		want    string
	}{
		{nil, "api-pypi-a api-pypi-b api-pypi-c"},
		{[]string{"runtime"}, "api-pypi-a"},
		{[]string{"dev", "peer"}, "api-pypi-b api-pypi-d"},
	} {
		if got := strings.Join(names(gen(t, deps, Options{Project: "api", Include: tc.include})), " "); got != tc.want {
			t.Errorf("include %v: %s, want %s", tc.include, got, tc.want)
		}
	}
	if _, err := Generate(deps, Options{Project: "api", Include: []string{"all"}}); err == nil {
		t.Error("--include all accepted")
	}
}

// Criterion: --source pypi=NAME sets the source, the watch name keeps "pypi".
func TestSourceMapping(t *testing.T) {
	g := string(gen(t, []Dependency{dep("requests", TypeRuntime)}, Options{Project: "api", Sources: map[string]string{"pypi": "pypi-internal"}}).YAML)
	if !strings.Contains(g, "name: api-pypi-requests\n    source: pypi-internal\n") {
		t.Errorf("source: %s", g)
	}
	for _, bad := range [][]string{{"pypi"}, {"pypi="}, {"=x"}, {"cargo=x"}, {"pypi=Bad Name"}, {"pypi=a", "pypi=b"}} {
		if _, err := ParseSourceMappings(bad); err == nil {
			t.Errorf("--source %v accepted", bad)
		}
	}
	m, err := ParseSourceMappings([]string{"pypi=pypi-internal"})
	if err != nil || m["pypi"] != "pypi-internal" {
		t.Errorf("%v %v", m, err)
	}
}

// Criterion: --interval, --exclude, --subscribe per D67 and D68.
func TestIntervalExcludeSubscribe(t *testing.T) {
	deps := []Dependency{dep("requests", TypeRuntime), dep("Flask", TypeRuntime)}
	out := gen(t, deps, Options{Project: "api", Interval: "90m", Exclude: []string{"fl*"}, Subscribe: "ci"})
	g := string(out.YAML)
	if strings.Contains(g, "flask") || !strings.Contains(g, "interval: 90m\n") {
		t.Errorf("interval/exclude:\n%s", g)
	}
	want := "subscriptions:\n  - name: api-deps\n    sink: ci\n    filter:\n      labels:\n        project: api\n"
	if !strings.HasSuffix(g, want) {
		t.Errorf("subscription: want suffix\n%s\ngot\n%s", want, g)
	}
	if g := string(gen(t, deps[:1], Options{Project: "api", Interval: "6h"}).YAML); !strings.Contains(g, "interval: 6h\n") {
		t.Errorf("6h: %s", g)
	}
	for _, bad := range []Options{
		{Project: "api", Interval: "1m"}, {Project: "api", Interval: "soon"}, {Project: "api", Interval: "9999h"},
		{Project: "api", Interval: "5m0.5s"}, {Project: "api", Exclude: []string{"[a"}}, {Project: "api", Subscribe: "Bad Sink"},
	} {
		if _, err := Generate(deps, bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	// Without watches the file is still valid and says so.
	empty := gen(t, nil, Options{Project: "api"})
	if !strings.HasSuffix(string(empty.YAML), "version: 1\nwatches: []\n") {
		t.Errorf("empty: %s", empty.YAML)
	}
}

// Criterion: --policy is validated by the watch file's policy rules, with its line.
func TestPolicyFile(t *testing.T) {
	deps := []Dependency{dep("requests", TypeRuntime)}
	for _, tc := range []struct{ file, want string }{
		{"bad-key.yaml", "bad-key.yaml: line 3: bogus: unknown key"},
		{"bad-constraint.yaml", "bad-constraint.yaml: line 2: constraint:"},
		{"bad-value.yaml", "bad-value.yaml: line 3: prerelease:"},
		{"empty.yaml", "empty policy file"},
	} {
		b, err := os.ReadFile(filepath.Join("testdata", "policy", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		_, err = Generate(deps, Options{Project: "api", Policy: b, PolicyName: tc.file})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.file, err, tc.want)
		}
	}
	if _, err := Generate(deps, Options{Project: "api", Policy: []byte("- a\n- b\n"), PolicyName: "list.yaml"}); err == nil {
		t.Error("a list is not a policy")
	}
}

// Every generated PyPI coordinate is accepted by the pypi source.
func TestGeneratedCoordinatesValidate(t *testing.T) {
	src := pypi.New(pypi.DefaultBaseURL, nil)
	set, err := apply.Read(bytes.NewReader(gen(t, basicDeps(t), Options{Project: "api"}).YAML), []string{"-"})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range set.Objects[apply.Watch] {
		var w struct {
			Coordinates map[string]string `json:"coordinates"`
		}
		if err := jsonUnmarshal(o.Body, &w); err != nil {
			t.Fatal(err)
		}
		if err := src.Validate(source.Target{Kind: "pypi", Coordinates: w.Coordinates}); err != nil {
			t.Errorf("%s: %v", o.Name, err)
		}
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }
