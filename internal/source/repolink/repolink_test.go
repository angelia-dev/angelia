package repolink_test

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/source/repolink"
)

func TestParse(t *testing.T) {
	t.Parallel()
	gh := func(o, n string) repolink.Repo { return repolink.Repo{Host: "github.com", Owner: o, Name: n} }
	gl := func(o, n string) repolink.Repo { return repolink.Repo{Host: "gitlab.com", Owner: o, Name: n} }
	ok := []struct {
		in   string
		want repolink.Repo
	}{
		{"https://github.com/psf/requests", gh("psf", "requests")},
		{"http://github.com/psf/requests", gh("psf", "requests")},
		{"git://github.com/psf/requests.git", gh("psf", "requests")},
		{"git+https://github.com/psf/requests.git", gh("psf", "requests")},
		{"git+ssh://git@github.com/psf/requests.git", gh("psf", "requests")},
		{"ssh://git@github.com/psf/requests.git", gh("psf", "requests")},
		{"git+ssh://git@github.com:psf/requests.git", gh("psf", "requests")},
		{"git@github.com:psf/requests.git", gh("psf", "requests")},
		{"git@github.com:psf/requests", gh("psf", "requests")},
		{"github:psf/requests", gh("psf", "requests")},
		{"gitlab:group/proj", gl("group", "proj")},
		{"https://github.com/psf/requests.git", gh("psf", "requests")},
		{"https://github.com/psf/requests/", gh("psf", "requests")},
		{"https://github.com/psf/requests/tree/main/packages/x", gh("psf", "requests")},
		{"https://github.com/psf/requests/issues", gh("psf", "requests")},
		{"https://github.com/psf/requests#readme", gh("psf", "requests")},
		{"https://github.com/psf/requests?tab=readme", gh("psf", "requests")},
		{"https://github.com/psf/requests.git#main", gh("psf", "requests")},
		{"https://www.github.com/psf/requests", gh("psf", "requests")},
		{"HTTPS://GitHub.COM/Psf/Requests", gh("Psf", "Requests")},
		{"  https://github.com/psf/requests \n", gh("psf", "requests")},
		{"https://github.com/pydantic/pydantic-core", gh("pydantic", "pydantic-core")},
		{"https://github.com/zopefoundation/zope.interface", gh("zopefoundation", "zope.interface")},
		{"https://gitlab.com/group/proj", gl("group", "proj")},
		{"https://gitlab.com/group/sub/proj.git", gl("group/sub", "proj")},
		{"https://gitlab.com/group/proj/-/tree/main", gl("group", "proj")},
		{"https://gitlab.com/group/sub/proj/-/issues/1", gl("group/sub", "proj")},
		{"git@gitlab.com:group/proj.git", gl("group", "proj")},
		{"https://www.gitlab.com/group/proj", gl("group", "proj")},
	}
	for _, c := range ok {
		got, valid := repolink.Parse(c.in)
		if !valid || got != c.want {
			t.Errorf("Parse(%q) = %+v, %v; want %+v", c.in, got, valid, c.want)
		}
	}
	bad := []string{
		"", "   ", "requests", "psf/requests", "not a url",
		"https://user@github.com/psf/requests",
		"https://user:pw@github.com/psf/requests",
		"https://github.com:443/psf/requests",
		"https://github.com:8080/psf/requests",
		"ssh://git@github.com:22/psf/requests",
		"git+ssh://other@github.com/psf/requests",
		"git+ssh://git:pw@github.com/psf/requests",
		"https://requests.readthedocs.io/",
		"https://bitbucket.org/psf/requests",
		"https://git.example.com/psf/requests",
		"https://github.com.evil.example/psf/requests",
		"https://evilgithub.com/psf/requests",
		"https://github.com/",
		"https://github.com/psf",
		"https://github.com/psf/",
		"https://gitlab.com/group",
		"https://gitlab.com/-/projects",
		"https://github.com/psf/..",
		"https://github.com/../requests",
		"https://github.com/psf/.",
		"https://github.com/psf/%2e%2e",
		"https://github.com/-bad/requests",
		"https://github.com/psf/re quests",
		"https://github.com/psf/requests\nx",
		"https://github.com/psf/a%0ab",
		"https://github.com/psf/.git",
		"https://github.com/psf/a.git.git",
		"https://github.com//requests",
		"https://github.com/" + strings.Repeat("a", 40) + "/requests",
		"ftp://github.com/psf/requests",
		"file:///github.com/psf/requests",
		"javascript://github.com/psf/requests",
		"github:psf",
		"github://psf/requests",
		"bitbucket:psf/requests",
		"gist:abc",
		"git@evil.example:psf/requests.git",
		"https://[::1]/psf/requests",
	}
	for _, in := range bad {
		if got, valid := repolink.Parse(in); valid {
			t.Errorf("Parse(%q) = %+v, want rejected", in, got)
		}
	}
}

func TestParseNPMBareShorthand(t *testing.T) {
	t.Parallel()
	got, ok := repolink.ParseNPM("facebook/react")
	if !ok || got.URL() != "https://github.com/facebook/react" {
		t.Fatalf("ParseNPM = %+v, %v", got, ok)
	}
	if _, ok := repolink.Parse("facebook/react"); ok {
		t.Error("Parse accepted a bare owner/repo, which only npm defines")
	}
	for _, in := range []string{"react", "bitbucket:a/b", "gist:123", "./local/path", "a/b/c d"} {
		if g, ok := repolink.ParseNPM(in); ok {
			t.Errorf("ParseNPM(%q) = %+v, want rejected", in, g)
		}
	}
}

func TestRepoURL(t *testing.T) {
	t.Parallel()
	r := repolink.Repo{Host: "gitlab.com", Owner: "g/s", Name: "p"}
	if got := r.URL(); got != "https://gitlab.com/g/s/p" {
		t.Errorf("URL = %q", got)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{
		"https://github.com/psf/requests", "git+ssh://git@github.com:psf/requests.git", "git@gitlab.com:g/s/p.git",
		"github:a/b", "https://gitlab.com/g/p/-/tree/x", "a/b", "https://github.com/a/b.git.git", "https://u:p@github.com/a/b",
		"https://github.com:80/a/b", "ssh://git@github.com:22/a/b", "https://github.com/%2e%2e/b", "", "://", "git@:",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, parse := range []func(string) (repolink.Repo, bool){repolink.Parse, repolink.ParseNPM} {
			r, ok := parse(in)
			if !ok {
				continue
			}
			u, err := url.Parse(r.URL())
			if err != nil {
				t.Fatalf("URL %q of %q does not parse: %v", r.URL(), in, err)
			}
			if u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" || strings.ContainsAny(r.URL(), "?#@ ") {
				t.Fatalf("URL %q of %q carries userinfo, port, query or fragment", r.URL(), in)
			}
			back, ok := repolink.Parse(r.URL())
			if !ok || back != r {
				t.Fatalf("Parse(%q) = %+v, but Parse(URL()=%q) = %+v, %v", in, r, r.URL(), back, ok)
			}
		}
	})
}

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func TestPyPILabels(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		urls     string
		home     string
		wantRepo string
		wantLog  string
	}{
		{"source", `{"Source":"https://github.com/a/b"}`, `null`, "https://github.com/a/b", ""},
		{"lower source", `{"source":"https://github.com/a/b"}`, `null`, "https://github.com/a/b", ""},
		{"Source Code", `{"Source Code":"https://github.com/a/b"}`, `null`, "https://github.com/a/b", ""},
		{"source-code", `{"source-code":"https://github.com/a/b"}`, `null`, "https://github.com/a/b", ""},
		{"Repository", `{"Repository":"https://github.com/a/b"}`, `null`, "https://github.com/a/b", ""},
		{"source over homepage", `{"Homepage":"https://github.com/x/home","Source":"https://github.com/a/b"}`, `null`, "https://github.com/a/b", ""},
		{"non-forge homepage skipped for a later link", `{"Homepage":"https://example.org/","Bug Tracker":"https://github.com/a/b/issues"}`, `null`, "https://github.com/a/b", ""},
		{"unknown label used last, in label order", `{"Zed":"https://github.com/z/z","Alpha":"https://github.com/a/a","Source":"https://example.org/x"}`, `null`, "https://github.com/a/a", ""},
		{"home_page last resort", `{"Docs":"https://docs.example.org/"}`, `"https://github.com/h/p"`, "https://github.com/h/p", ""},
		{"project_urls over home_page", `{"Homepage":"https://github.com/a/b"}`, `"https://github.com/h/p"`, "https://github.com/a/b", ""},
		{"no link", `{"Homepage":"https://example.org/"}`, `"https://example.org/"`, "", ""},
		{"null project_urls", `null`, `null`, "", ""},
		{"project_urls of the wrong type", `["https://github.com/a/b"]`, `7`, "", ""},
		{"null and number values skipped", `{"Source":null,"Repository":5,"Code":"https://github.com/a/b"}`, `null`, "https://github.com/a/b", ""},
		{"gitlab", `{"Source":"https://gitlab.com/g/p"}`, `null`, "https://gitlab.com/g/p", ""},
		{"changelog", `{"Changelog":"https://example.org/CHANGES"}`, `null`, "", "https://example.org/CHANGES"},
		{"Release notes", `{"Release notes":"https://example.org/rn"}`, `null`, "", "https://example.org/rn"},
		{"release notes lower", `{"release notes":"https://example.org/rn"}`, `null`, "", "https://example.org/rn"},
		{"Changes", `{"Changes":"https://example.org/c"}`, `null`, "", "https://example.org/c"},
		{"changelog precedence", `{"History":"https://example.org/h","Changelog":"https://example.org/c"}`, `null`, "", "https://example.org/c"},
		{"non-https ignored", `{"Changelog":"http://example.org/c","News":"https://example.org/n"}`, `null`, "", "https://example.org/n"},
		{"not a URL ignored", `{"Changelog":"see README","Changes":"ftp://example.org/c","History":"https://"}`, `null`, "", ""},
		{"changelog with userinfo ignored", `{"Changelog":"https://u:p@example.org/c"}`, `null`, "", ""},
		{"both", `{"Source":"https://github.com/a/b","Changelog":"https://github.com/a/b/blob/main/CHANGES.md"}`, `null`, "https://github.com/a/b", "https://github.com/a/b/blob/main/CHANGES.md"},
	}
	for _, c := range cases {
		l := repolink.PyPI(raw(c.urls), raw(c.home))
		got := ""
		if l.HasRepo {
			got = l.Repo.URL()
		}
		if got != c.wantRepo || l.Changelog != c.wantLog {
			t.Errorf("%s: repo %q changelog %q; want %q, %q", c.name, got, l.Changelog, c.wantRepo, c.wantLog)
		}
	}
	if l := repolink.PyPI(nil, nil); l.HasRepo || l.Changelog != "" {
		t.Errorf("nil input gave %+v", l)
	}
}

func TestNPM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		in      string
		want    string
		wantDir string
	}{
		{"object with directory", `{"type":"git","url":"git+https://github.com/babel/babel.git","directory":"packages/babel-core"}`, "https://github.com/babel/babel", "packages/babel-core"},
		{"github shorthand", `"github:owner/repo"`, "https://github.com/owner/repo", ""},
		{"bare shorthand", `"owner/repo"`, "https://github.com/owner/repo", ""},
		{"git+ssh", `{"url":"git+ssh://git@github.com/owner/repo.git"}`, "https://github.com/owner/repo", ""},
		{"git+ssh string", `"git+ssh://git@github.com/owner/repo.git"`, "https://github.com/owner/repo", ""},
		{"directory slashes trimmed", `{"url":"https://github.com/o/r","directory":"/pkg/a/"}`, "https://github.com/o/r", "pkg/a"},
		{"non-forge URL", `{"url":"https://example.org/owner/repo.git"}`, "", ""},
		{"non-forge shorthand", `"bitbucket:owner/repo"`, "", ""},
		{"null", `null`, "", ""},
		{"number", `7`, "", ""},
		{"array", `["github:a/b"]`, "", ""},
		{"empty object", `{}`, "", ""},
	}
	for _, c := range cases {
		r, dir, ok := repolink.NPM(raw(c.in))
		got := ""
		if ok {
			got = r.URL()
		}
		if got != c.want || dir != c.wantDir {
			t.Errorf("%s: %q dir %q; want %q dir %q", c.name, got, dir, c.want, c.wantDir)
		}
	}
	if _, _, ok := repolink.NPM(nil); ok {
		t.Error("nil gave a repository")
	}
}
