package manifest

import (
	"fmt"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
)

func reqFS() fs.FS { return os.DirFS("testdata/requirements") }

func summarize(deps []Dependency) []string {
	var out []string
	for _, d := range deps {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d", d.Name, d.Type, d.Pin, d.Origin.File, d.Origin.Line))
	}
	return out
}

func equal(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func warnings(r *Result) []string {
	var out []string
	for _, w := range r.Warnings {
		out = append(out, fmt.Sprintf("%s skip=%v", w, w.Skip))
	}
	return out
}

// Criterion: comments, continuations, extras, markers, specifiers.
func TestRequirementsGrammar(t *testing.T) {
	r, err := ParseRequirements(reqFS(), "basic.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, summarize(r.Deps), []string{
		"requests|runtime|2.31.0|basic.txt|2",
		"Flask|runtime||basic.txt|3",
		"Django|runtime||basic.txt|4",
		"PyYAML|runtime||basic.txt|5",
		"zope.interface|runtime||basic.txt|6",
		"my_Package.Name|runtime|1.0.0|basic.txt|7",
		"numpy|runtime|1.26.4|basic.txt|8",
		"pandas|runtime||basic.txt|9",
		"black|runtime|24.1.0|basic.txt|12",
		"httpx|runtime||basic.txt|14",
		"wildcard|runtime||basic.txt|16",
	})
	if len(r.Warnings) != 0 {
		t.Errorf("warnings: %v", warnings(r))
	}
}

// Criterion: every skip and ignore case, with its warning text.
func TestRequirementsSkipsAndIgnores(t *testing.T) {
	r, err := ParseRequirements(reqFS(), "skips.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, summarize(r.Deps), []string{
		"requests|runtime|2.31.0|skips.txt|1",
		"good-pkg|runtime|1.0|skips.txt|24",
	})
	equal(t, warnings(r), []string{
		"skips.txt:2: skipped -c constraints.txt: constraint files pin versions, they do not declare dependencies skip=true",
		"skips.txt:3: skipped --constraint=other-constraints.txt: constraint files pin versions, they do not declare dependencies skip=true",
		"skips.txt:4: skipped -e .: editable requirements are not registry dependencies skip=true",
		"skips.txt:5: skipped --editable git+https://example.com/org/repo.git#egg=repo: editable requirements are not registry dependencies skip=true",
		`skips.txt:6: skipped "./local/package": a local path is not a registry dependency skip=true`,
		`skips.txt:7: skipped "../sibling/pkg": a local path is not a registry dependency skip=true`,
		`skips.txt:8: skipped "/abs/path/pkg.whl": a local path is not a registry dependency skip=true`,
		`skips.txt:9: skipped "git+https://example.com/org/repo.git#egg=gitpkg": URL and VCS requirements are not registry dependencies skip=true`,
		`skips.txt:10: skipped "gitpkg2 @ git+https://example.com/org/repo2.git": URL and VCS requirements are not registry dependencies skip=true`,
		`skips.txt:11: skipped "urlpkg @ https://example.com/urlpkg-1.0.tar.gz": URL and VCS requirements are not registry dependencies skip=true`,
		`skips.txt:12: skipped "https://example.com/bare-1.0.whl": URL and VCS requirements are not registry dependencies skip=true`,
		`skips.txt:13: skipped "${PRIVATE_PKG}==1.0": the name holds a variable, which angelia import does not expand skip=true`,
		`skips.txt:14: skipped "pkg${SUFFIX}>=2": the name holds a variable, which angelia import does not expand skip=true`,
		"skips.txt:15: --index-url ignored (and the other index options of this file): angelia import reads no index settings; map the PyPI watches to a named source with --source pypi=NAME skip=false",
		"skips.txt:23: skipped unknown option --frobnicate skip=true",
		`skips.txt:25: skipped "bad token here": cannot read the version specifier skip=true`,
	})
}

// The index warning is once per file, not once per line.
func TestRequirementsIndexWarnsOncePerFile(t *testing.T) {
	r, err := ParseRequirements(reqFS(), "two-index-files.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Warnings) != 1 || r.Warnings[0].Skip || !strings.HasPrefix(r.Warnings[0].String(), "two-index-files.txt:1: -i ignored") {
		t.Errorf("warnings: %v", warnings(r))
	}
	if len(r.Deps) != 1 {
		t.Errorf("deps: %v", summarize(r.Deps))
	}
}

// Criterion: -r relative includes (nested).
func TestRequirementsIncludes(t *testing.T) {
	r, err := ParseRequirements(reqFS(), "requirements.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	equal(t, summarize(r.Deps), []string{
		"base-pkg|runtime|1.0|include/base.txt|1",
		"base-pkg|runtime|1.0|include/base.txt|1",
		"leaf-pkg|runtime||include/sub/leaf.txt|1",
		"mid-pkg|runtime||include/sub/mid.txt|3",
		"top|runtime|1.0|requirements.txt|3",
	})
}

// --dev: the file and what it includes are dev dependencies.
func TestRequirementsDevFileMarksIncludes(t *testing.T) {
	r, err := ParseRequirements(reqFS(), "requirements-dev.txt", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range r.Deps {
		if d.Type != TypeDev {
			t.Errorf("%s is %s, want dev", d.Name, d.Type)
		}
	}
	if len(r.Deps) != 3 {
		t.Errorf("deps: %v", summarize(r.Deps))
	}
}

// Criterion: depth 9 fails; depth 8 works.
func TestRequirementsIncludeDepth(t *testing.T) {
	chain := func(n int) fstest.MapFS {
		m := fstest.MapFS{}
		for i := 0; i <= n; i++ {
			body := fmt.Sprintf("pkg%d\n", i)
			if i < n {
				body += fmt.Sprintf("-r f%d.txt\n", i+1)
			}
			m[fmt.Sprintf("f%d.txt", i)] = &fstest.MapFile{Data: []byte(body)}
		}
		return m
	}
	r, err := ParseRequirements(chain(8), "f0.txt", false)
	if err != nil {
		t.Fatalf("depth 8: %v", err)
	}
	if len(r.Deps) != 9 {
		t.Errorf("deps = %d, want 9", len(r.Deps))
	}
	_, err = ParseRequirements(chain(9), "f0.txt", false)
	if err == nil || !strings.Contains(err.Error(), "f8.txt:2: f9.txt: -r includes nest deeper than 8 files") {
		t.Errorf("depth 9: %v", err)
	}
}

// Criterion: a cycle fails with both files named.
func TestRequirementsIncludeCycle(t *testing.T) {
	_, err := ParseRequirements(reqFS(), "cycle-a.txt", false)
	want := "cycle-b.txt:1: include cycle: cycle-a.txt -> cycle-b.txt -> cycle-a.txt"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

// Criterion: -r https://... fails; a missing include names the line.
func TestRequirementsIncludeErrors(t *testing.T) {
	_, err := ParseRequirements(reqFS(), "url-include.txt", false)
	if err == nil || !strings.Contains(err.Error(), "url-include.txt:1: -r https://example.com/requirements.txt: includes are read from the file system only") {
		t.Errorf("url: %v", err)
	}
	m := fstest.MapFS{"a.txt": {Data: []byte("x\n-r missing.txt\n")}}
	_, err = ParseRequirements(m, "a.txt", false)
	if err == nil || !strings.HasPrefix(err.Error(), "a.txt:2: read missing.txt:") {
		t.Errorf("missing: %v", err)
	}
	m = fstest.MapFS{"a.txt": {Data: []byte("-r\n")}}
	if _, err = ParseRequirements(m, "a.txt", false); err == nil {
		t.Error("-r without a file: no error")
	}
}

// Criterion: names normalised with pypi.Normalize; each coordinate passes the
// pypi source's Validate.
func TestRequirementsNamesNormalizeAndValidate(t *testing.T) {
	r, err := ParseRequirements(reqFS(), "mixed-case.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	src := pypi.New(pypi.DefaultBaseURL, nil)
	for _, d := range r.Deps {
		canon := kinds[d.Kind].canonical(d.Name)
		if canon != pypi.Normalize(d.Name) {
			t.Errorf("%s: canonical %q, want %q", d.Name, canon, pypi.Normalize(d.Name))
		}
		if err := src.Validate(source.Target{Kind: "pypi", Coordinates: map[string]string{"name": canon}}); err != nil {
			t.Errorf("%s: %v", d.Name, err)
		}
	}
	out, err := Generate(r.Deps, Options{Project: "api"})
	if err != nil {
		t.Fatal(err)
	}
	// Three spellings of requests, three of typing-extensions: two watches.
	if len(out.Entries) != 2 || out.Entries[0].Watch != "api-pypi-requests" || out.Entries[1].Watch != "api-pypi-typing-extensions" {
		t.Errorf("entries: %+v", out.Entries)
	}
}
