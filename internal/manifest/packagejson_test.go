package manifest

import (
	"bytes"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/angelia-dev/angelia/internal/apply"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/npm"
)

func pkgFS() fs.FS { return os.DirFS("testdata/packagejson") }

// Criterion (98-B): the four keys and their types, with the line of each.
func TestPackageJSONSections(t *testing.T) {
	r, err := ParsePackageJSON(pkgFS(), "basic.json")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, summarize(r.Deps), []string{
		"left-pad|runtime|1.3.0|basic.json|6",
		"react|runtime||basic.json|7",
		"@types/node|runtime||basic.json|8",
		"@acme/widget|runtime|2.0.1|basic.json|9",
		"lodash|runtime||basic.json|10",
		"fsevents|optional||basic.json|13",
		"typescript|dev|5.4.2|basic.json|16",
		"eslint|dev||basic.json|17",
		"react-dom|peer||basic.json|20",
	})
	if len(r.Warnings) != 0 {
		t.Errorf("warnings: %v", warnings(r))
	}
}

// Criterion: every skipped specifier form, with its warning.
func TestPackageJSONSkips(t *testing.T) {
	r, err := ParsePackageJSON(pkgFS(), "skips.json")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, summarize(r.Deps), []string{"ok|runtime||skips.json|15"})
	equal(t, warnings(r), []string{
		`skips.json:3: skipped local-a in dependencies: "file:../local-a" is not a registry dependency (file) skip=true`,
		`skips.json:4: skipped local-b in dependencies: "link:../local-b" is not a registry dependency (link) skip=true`,
		`skips.json:5: skipped local-c in dependencies: "portal:../local-c" is not a registry dependency (portal) skip=true`,
		`skips.json:6: skipped ws in dependencies: "workspace:*" is not a registry dependency (workspace) skip=true`,
		`skips.json:7: skipped git-a in dependencies: "git+https://example.com/org/repo.git#v1" is not a registry dependency (git) skip=true`,
		`skips.json:8: skipped git-b in dependencies: "git://example.com/org/repo.git" is not a registry dependency (git) skip=true`,
		`skips.json:9: skipped git-c in dependencies: "github:org/repo" is not a registry dependency (github) skip=true`,
		`skips.json:10: skipped git-d in dependencies: "org/repo" is a repository or path, not a registry dependency skip=true`,
		`skips.json:11: skipped git-e in dependencies: "org/repo#semver:^1.0.0" is a repository or path, not a registry dependency skip=true`,
		`skips.json:12: skipped tarball in dependencies: "https://example.com/pkg-1.0.0.tgz" is not a registry dependency (https) skip=true`,
		`skips.json:13: skipped plain-http in dependencies: "http://example.com/pkg-1.0.0.tgz" is not a registry dependency (http) skip=true`,
		`skips.json:14: skipped number in dependencies: the version must be a string skip=true`,
	})
}

// Criterion: npm: aliases import the real name.
func TestPackageJSONAliases(t *testing.T) {
	r, err := ParsePackageJSON(pkgFS(), "aliases.json")
	if err != nil {
		t.Fatal(err)
	}
	equal(t, summarize(r.Deps), []string{
		"react|runtime||aliases.json|3",
		"left-pad|runtime|1.3.0|aliases.json|4",
		"@acme/real|runtime|2.0.0|aliases.json|5",
		"@acme/other|runtime||aliases.json|6",
	})
	equal(t, warnings(r), []string{"aliases.json:7: skipped empty-alias in dependencies: an npm: alias without a package name skip=true"})
}

// Criterion: a missing key is fine; a non-object key warns.
func TestPackageJSONMissingAndNonObject(t *testing.T) {
	r, err := ParsePackageJSON(pkgFS(), "missing.json")
	if err != nil || len(r.Deps) != 0 || len(r.Warnings) != 0 {
		t.Errorf("missing: %v %v %v", r, err, warnings(r))
	}
	r, err = ParsePackageJSON(pkgFS(), "nonobject.json")
	if err != nil || len(r.Deps) != 0 {
		t.Fatalf("nonobject: %v %v", summarize(r.Deps), err)
	}
	equal(t, warnings(r), []string{
		"nonobject.json:2: skipped dependencies: must be an object of name: version, the dependencies in it are not imported skip=true",
		"nonobject.json:3: skipped devDependencies: must be an object of name: version, the dependencies in it are not imported skip=true",
	})
}

// Criterion: a duplicate name across keys follows the precedence rule.
func TestPackageJSONDuplicatesAcrossKeys(t *testing.T) {
	r, err := ParsePackageJSON(pkgFS(), "duplicates.json")
	if err != nil {
		t.Fatal(err)
	}
	out := gen(t, r.Deps, Options{Project: "web", Include: Types})
	want := map[string]string{"web-npm-shared": "runtime", "web-npm-dev-only": "optional", "web-npm-peer-only": "peer"}
	got := string(out.YAML)
	if len(out.Entries) != len(want) {
		t.Errorf("entries: %+v", out.Entries)
	}
	blocks := strings.Split(got, "\n  - name: ")[1:]
	for name, typ := range want {
		found := false
		for _, b := range blocks {
			if strings.HasPrefix(b, name+"\n") {
				found = true
				if !strings.Contains(b, "dependency: "+typ+"\n") {
					t.Errorf("%s should be %s in\n%s", name, typ, got)
				}
			}
		}
		if !found {
			t.Errorf("no %s in\n%s", name, got)
		}
	}
}

// Criterion: invalid JSON reports the line of the error.
func TestPackageJSONInvalid(t *testing.T) {
	for _, tc := range []struct{ file, want string }{
		{"invalid.json", "invalid.json:5: invalid JSON: invalid character '}' looking for beginning of object key string"},
		{"truncated.json", "truncated.json:4: invalid JSON: unexpected end of file"},
		{"array.json", "array.json:1: the top level of package.json must be an object"},
		{"trailing.json", "trailing.json:2: unexpected data after the top-level object"},
	} {
		_, err := ParsePackageJSON(pkgFS(), tc.file)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s: %v, want %q", tc.file, err, tc.want)
		}
	}
	if _, err := ParsePackageJSON(pkgFS(), "absent.json"); err == nil {
		t.Error("a missing file is not an error")
	}
	if _, err := Parse(FormatPackageJSON, pkgFS(), "basic.json", true); err == nil || !strings.Contains(err.Error(), "--dev applies to requirements files") {
		t.Errorf("--dev on package.json: %v", err)
	}
	if got := FormatOf("web/package.json"); got != FormatPackageJSON {
		t.Errorf("FormatOf = %q", got)
	}
}

// Criterion: each coordinate is accepted by the npm source; scoped names make
// names such as api-npm-types-node.
func TestPackageJSONNamesAndValidate(t *testing.T) {
	r, err := ParsePackageJSON(pkgFS(), "basic.json")
	if err != nil {
		t.Fatal(err)
	}
	out := gen(t, r.Deps, Options{Project: "api"})
	for _, w := range []string{"api-npm-types-node", "api-npm-acme-widget", "api-npm-left-pad", "api-npm-fsevents"} {
		if !strings.Contains(string(out.YAML), "name: "+w+"\n") {
			t.Errorf("no %s in\n%s", w, out.YAML)
		}
	}
	src := npm.New("", nil)
	set, err := apply.Read(bytes.NewReader(out.YAML), []string{"-"})
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
		if err := src.Validate(source.Target{Kind: "npm", Coordinates: w.Coordinates}); err != nil {
			t.Errorf("%s: %v", o.Name, err)
		}
	}
	loads(t, out.YAML)
	// An invalid npm name fails with the dependency and its line.
	bad, _ := ParsePackageJSON(pkgFS(), "badname.json")
	_, err = Generate(bad.Deps, Options{Project: "api"})
	if err == nil || !strings.Contains(err.Error(), "watch api-npm-bad-name- (npm bad name!, badname.json:1)") {
		t.Errorf("bad name: %v", err)
	}
}

// Criterion: --source @acme=NAME maps one scope and wins over npm=NAME; a
// scope without @ is a usage error.
func TestNPMScopeSourceMapping(t *testing.T) {
	m, err := ParseSourceMappings([]string{"npm=npm-mirror", "@acme=npm-internal"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := ParsePackageJSON(pkgFS(), "basic.json")
	if err != nil {
		t.Fatal(err)
	}
	g := string(gen(t, r.Deps, Options{Project: "api", Sources: m}).YAML)
	for name, src := range map[string]string{"api-npm-acme-widget": "npm-internal", "api-npm-types-node": "npm-mirror", "api-npm-react": "npm-mirror"} {
		if !strings.Contains(g, "name: "+name+"\n    source: "+src+"\n") {
			t.Errorf("%s should use %s in\n%s", name, src, g)
		}
	}
	for _, bad := range []string{"acme=npm-internal", "@=x", "@Acme=x", "@acme/x=y"} {
		if _, err := ParseSourceMappings([]string{bad}); err == nil {
			t.Errorf("--source %s accepted", bad)
		}
	}
}

// Criterion: a mixed run gives one sorted file (golden).
func TestGoldenMixed(t *testing.T) {
	a, err := ParseRequirements(reqFS(), "basic.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ParsePackageJSON(pkgFS(), "basic.json")
	if err != nil {
		t.Fatal(err)
	}
	o := Options{Project: "shop", Interval: "12h", Inputs: []string{"basic.txt", "basic.json"}}
	deps := append(append([]Dependency(nil), a.Deps...), b.Deps...)
	first := gen(t, deps, o)
	checkGolden(t, "mixed", first.YAML)
	loads(t, first.YAML)
	swapped := append(append([]Dependency(nil), b.Deps...), a.Deps...)
	o.Inputs = []string{"basic.json", "basic.txt"}
	if other := gen(t, swapped, o); !bytes.Equal(first.YAML, other.YAML) {
		t.Errorf("input order changes the output:\n%s", other.YAML)
	}
	var kindsSeen []string
	for _, line := range strings.Split(string(first.YAML), "\n") {
		if strings.HasPrefix(line, "  - name: shop-") {
			kindsSeen = append(kindsSeen, strings.TrimPrefix(line, "  - name: "))
		}
	}
	for i := 1; i < len(kindsSeen); i++ {
		if kindsSeen[i-1] >= kindsSeen[i] {
			t.Errorf("watches not sorted: %v", kindsSeen)
		}
	}
	if !strings.Contains(string(first.YAML), "shop-npm-react") || !strings.Contains(string(first.YAML), "shop-pypi-flask") {
		t.Errorf("both kinds expected:\n%s", first.YAML)
	}
}

// Two npm names that differ but make one slug (@a/b-c, @a-b/c) fail and name
// both; the mixed case of one name is two different packages to npm.
func TestNPMNameCollision(t *testing.T) {
	d := func(n string, line int) Dependency {
		return Dependency{Name: n, Kind: KindNPM, Type: TypeRuntime, Origin: Origin{File: "package.json", Line: line}}
	}
	_, err := Generate([]Dependency{d("@a/b-c", 3), d("@a-b/c", 4)}, Options{Project: "web"})
	if err == nil || !strings.Contains(err.Error(), "npm @a/b-c (package.json:3) and npm @a-b/c (package.json:4) both map to the watch name \"web-npm-a-b-c\"") {
		t.Errorf("err = %v", err)
	}
	// --exclude resolves it.
	out := gen(t, []Dependency{d("@a/b-c", 3), d("@a-b/c", 4)}, Options{Project: "web", Exclude: []string{"@a-b/*"}})
	if got := names(out); len(got) != 1 || got[0] != "web-npm-a-b-c" {
		t.Errorf("names = %v", got)
	}
}

// A section key given twice: the last wins, as in npm.
func TestPackageJSONDuplicateSectionKeepsLast(t *testing.T) {
	m := fstest.MapFS{"package.json": {Data: []byte(`{"dependencies": {"first": "1.0.0"}, "dependencies": {"second": "2.0.0"}}`)}}
	r, err := ParsePackageJSON(m, "package.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Deps) != 1 || r.Deps[0].Name != "second" {
		t.Errorf("deps: %v", summarize(r.Deps))
	}
}
