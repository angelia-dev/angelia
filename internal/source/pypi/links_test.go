package pypi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/store"
)

// Repository discovery costs no request (issue #84, D80): the repository and
// the changelog come from the response List already fetches.
func TestProjectURLsGiveRepositoryAndChangelog(t *testing.T) {
	t.Parallel()
	up := &upstream{}
	srv := httptest.NewServer(up)
	t.Cleanup(srv.Close)
	res, err := pypi.New(srv.URL, srv.Client()).List(context.Background(), target("angelia-linked"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(res.Versions))
	}
	for _, v := range res.Versions {
		if got := v.Metadata["repository_url"]; got != "https://github.com/example-org/angelia-linked" {
			t.Errorf("%s: repository_url = %v", v.Raw, got)
		}
		want := ""
		if v.Raw == "2.0.0" {
			want = "https://example.org/angelia-linked/releases" // the latest only
		}
		if v.NotesURL != want {
			t.Errorf("%s: NotesURL = %q, want %q", v.Raw, v.NotesURL, want)
		}
	}
	if len(up.paths) != 1 {
		t.Errorf("%d requests %v, want exactly one", len(up.paths), up.paths)
	}
}

func TestNoProjectURLsGiveNoKeys(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"angelia-unlinked", "requests", "django"} {
		for _, v := range list(t, name).Versions {
			if _, ok := v.Metadata["repository_url"]; ok || v.NotesURL != "" {
				t.Fatalf("%s %s: repository_url %v, NotesURL %q; want neither", name, v.Raw, v.Metadata["repository_url"], v.NotesURL)
			}
		}
	}
}

// A wrongly typed info.project_urls or info.home_page must not fail List.
func TestMalformedLinkFieldsAreIgnored(t *testing.T) {
	t.Parallel()
	for _, info := range []string{
		`"project_urls":["https://github.com/a/b"],"home_page":7`,
		`"project_urls":{"Source":{"x":1},"Changelog":3},"home_page":["x"]`,
		`"project_urls":"https://github.com/a/b"`,
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"info":{"name":"x",` + info + `},"releases":{"1.0":[{"upload_time_iso_8601":"2020-05-02T10:00:00Z","yanked":false}]}}`))
		}))
		res, err := pypi.New(srv.URL, srv.Client()).List(context.Background(), target("x"), source.FetchState{})
		srv.Close()
		if err != nil || len(res.Versions) != 1 {
			t.Fatalf("%s: err %v, %d versions", info, err, len(res.Versions))
		}
		if _, ok := res.Versions[0].Metadata["repository_url"]; ok {
			t.Errorf("%s: repository_url set", info)
		}
	}
}

// The Simple API has no project URLs: nothing changes there.
func TestSimpleAPIHasNoLinks(t *testing.T) {
	t.Parallel()
	for _, js := range []bool{true, false} {
		for _, v := range listSimple(t, &simpleServer{json: js}, "example-pkg").Versions {
			if _, ok := v.Metadata["repository_url"]; ok || v.NotesURL != "" {
				t.Fatalf("simple (json %v) %s: repository_url %v, NotesURL %q", js, v.Raw, v.Metadata["repository_url"], v.NotesURL)
			}
		}
	}
}

// An event built from a discovered version carries the changelog.
func TestEventChangelogFromDiscoveredNotesURL(t *testing.T) {
	t.Parallel()
	latest := list(t, "angelia-linked").Versions[0]
	if latest.Raw != "2.0.0" {
		t.Fatalf("newest = %s", latest.Raw)
	}
	env, err := event.Build(event.Input{
		ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Instance: "angelia",
		Target: store.Target{ID: "t1", Kind: "pypi", Coordinates: map[string]string{"name": "angelia-linked"}},
		Watch:  store.Watch{ID: "01JBX1Z3M4N5P6Q7R8S9T0V1W2", Name: "linked"},
		Release: policy.Release{
			Raw: latest.Raw, Normalized: latest.Raw, Scheme: "pep440",
			PublishedAt: latest.PublishedAt, DetectedAt: time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC),
			URL: latest.URL, NotesURL: latest.NotesURL, Metadata: latest.Metadata,
		},
		Time: time.Date(2026, 10, 30, 17, 0, 1, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(env)
	if !strings.Contains(string(b), `"changelog":"https://example.org/angelia-linked/releases"`) {
		t.Errorf("event lacks the changelog:\n%s", b)
	}
	if strings.Contains(string(b), "repository_url") {
		t.Errorf("repository_url leaked into the event:\n%s", b)
	}
}
