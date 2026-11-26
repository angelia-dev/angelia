package npm_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

// packument is a one-version full packument whose top-level repository is
// repo ("" leaves the field out).
func packument(repo string) []byte {
	r := ""
	if repo != "" {
		r = `"repository": ` + repo + `,`
	}
	return []byte(`{"name":"angelia-test-linked",` + r + `"dist-tags":{"latest":"1.0.0"},
"versions":{"1.0.0":{"name":"angelia-test-linked","version":"1.0.0","dist":{"tarball":"https://registry.example/x.tgz"}},
"0.9.0":{"name":"angelia-test-linked","version":"0.9.0","dist":{"tarball":"https://registry.example/y.tgz"}}},
"time":{"created":"2020-01-01T00:00:00.000Z","modified":"2020-02-01T00:00:00.000Z","0.9.0":"2020-01-01T00:00:00.000Z","1.0.0":"2020-02-01T00:00:00.000Z","0.8.0":"2019-12-01T00:00:00.000Z"}}`)
}

// Repository discovery costs no request (issue #84, D80).
func TestRepositoryLink(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		repo    string
		want    string
		wantDir string
	}{
		{"object with directory", `{"type":"git","url":"git+https://github.com/babel/babel.git","directory":"packages/babel-core"}`, "https://github.com/babel/babel", "packages/babel-core"},
		{"github shorthand", `"github:owner/repo"`, "https://github.com/owner/repo", ""},
		{"bare shorthand", `"owner/repo"`, "https://github.com/owner/repo", ""},
		{"git+ssh", `{"type":"git","url":"git+ssh://git@github.com/owner/repo.git"}`, "https://github.com/owner/repo", ""},
		{"gitlab", `{"type":"git","url":"https://gitlab.com/group/sub/proj.git"}`, "https://gitlab.com/group/sub/proj", ""},
		{"non-forge URL", `{"type":"git","url":"https://git.example.org/owner/repo.git"}`, "", ""},
		{"absent", ``, "", ""},
		{"null", `null`, "", ""},
		{"wrong type", `42`, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			up := &upstream{docs: map[string][]byte{"/angelia-test-linked": packument(c.repo)}}
			srv := httptest.NewServer(up)
			t.Cleanup(srv.Close)
			res, err := npm.New(srv.URL, sourcetest.Client(t, srv)).List(context.Background(), target("angelia-test-linked"), source.FetchState{})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Versions) != 3 { // 1.0.0, 0.9.0 and the unpublished 0.8.0
				t.Fatalf("got %d versions, want 3", len(res.Versions))
			}
			for _, v := range res.Versions {
				got, ok := v.Metadata["repository_url"]
				if c.want == "" {
					if ok {
						t.Errorf("%s: repository_url = %v, want none", v.Raw, got)
					}
				} else if got != c.want {
					t.Errorf("%s: repository_url = %v, want %q", v.Raw, got, c.want)
				}
				gotDir, hasDir := v.Metadata["repository_directory"]
				if (c.wantDir != "") != hasDir || (hasDir && gotDir != c.wantDir) {
					t.Errorf("%s: repository_directory = %v (%v), want %q", v.Raw, gotDir, hasDir, c.wantDir)
				}
				if v.NotesURL != "" {
					t.Errorf("%s: NotesURL = %q, npm declares no changelog", v.Raw, v.NotesURL)
				}
			}
			if n := len(up.accepts); n != 1 {
				t.Errorf("%d requests, want exactly one", n)
			}
		})
	}
}

// The abbreviated packument has no repository: nothing changes there.
func TestAbbreviatedHasNoRepository(t *testing.T) {
	t.Parallel()
	res, _ := listOne(t, "angelia-test-abbr", npm.WithAbbreviated())
	for _, v := range res.Versions {
		if _, ok := v.Metadata["repository_url"]; ok {
			t.Errorf("%s: repository_url in abbreviated mode", v.Raw)
		}
	}
}
