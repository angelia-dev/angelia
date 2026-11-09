package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGitHub serves search pages per query and a release listing per
// repository: releases[repo] releases (0: an empty list; -1: 404).
func fakeGitHub(t *testing.T, search map[string][]string, releases map[string]int, limited *bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-for-test" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		if limited != nil && *limited {
			w.Header().Set("X-RateLimit-Remaining", "0")
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/search/repositories" {
			q := r.URL.Query()
			if q.Get("sort") != "stars" || q.Get("per_page") != "100" {
				t.Errorf("search query %v", q)
			}
			var page int
			_, _ = fmt.Sscan(q.Get("page"), &page)
			all := search[q.Get("q")]
			type item struct {
				FullName string `json:"full_name"`
				Fork     bool   `json:"fork"`
			}
			var items []item
			for i := (page - 1) * 100; i < min(page*100, len(all)); i++ {
				items = append(items, item{FullName: all[i], Fork: strings.HasPrefix(all[i], "fork/")})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
			return
		}
		repo := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/"), "/releases")
		n, ok := releases[repo]
		switch {
		case !ok || n < 0:
			http.NotFound(w, r)
		case n == 0:
			_, _ = w.Write([]byte("[]"))
		default:
			if n > 1 {
				w.Header().Set("Link", fmt.Sprintf(`<https://x/repos/%s/releases?per_page=1&page=2>; rel="next", <https://x/repos/%s/releases?per_page=1&page=%d>; rel="last"`, repo, repo, n))
			}
			_, _ = w.Write([]byte(`[{"id":1}]`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testGen(api string) *gen {
	return &gen{api: api, token: "tok-for-test", hc: http.DefaultClient, log: slog.New(slog.DiscardHandler)}
}

func TestGenerateQuotasAndCarry(t *testing.T) {
	bs := []bucket{{"a", 50, []string{"qa", "qa2"}}, {"b", 25, []string{"qb"}}, {"c", 25, []string{"qc"}}}
	search := map[string][]string{
		"qa":  {"o/a0", "o/a1", "o/a2"},
		"qa2": {"o/a3", "o/a4", "o/a5"},
		"qb":  {"fork/x", "o/a1", "o/b0", "o/b1", "bad name"},
		"qc":  {"o/c0", "o/c1", "o/c2", "o/c3", "o/c4"},
	}
	rel := map[string]int{"o/a0": 1, "o/a1": 0, "o/a2": 250, "o/a3": 1, "o/a4": 5, "o/b0": -1, "o/b1": 3, "o/c0": 2, "o/c1": 0, "o/c2": 1500, "o/c3": 7, "o/c4": 1}
	l, err := testGen(fakeGitHub(t, search, rel, nil).URL).generate(context.Background(), 8, bs)
	if err != nil {
		t.Fatal(err)
	}
	// a: 4 wanted, a0 a2 a3 a4. b: 2 wanted, only b1 (a1 already seen, a fork and a bad name skipped).
	// c: the rest, 3: c0 c2 c3.
	if got := strings.Join(l.repos, " "); got != "o/a0 o/a2 o/a3 o/a4 o/b1 o/c0 o/c2 o/c3" {
		t.Errorf("repos = %s", got)
	}
	if l.counts["a"] != 4 || l.counts["b"] != 1 || l.counts["c"] != 3 {
		t.Errorf("counts = %v", l.counts)
	}
	// Releases 1+250+1+5+3+2+1500+7; pages 1+3+1+1+1+1+10+1.
	if l.releases != 1769 || l.pages != 19 {
		t.Errorf("releases %d, pages %d", l.releases, l.pages)
	}
	out := filepath.Join(t.TempDir(), "sub", "repos.txt")
	if err := writeList(out, l, time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out) //nolint:gosec // test temp dir
	for _, want := range []string{"# Generated 2026-11-09", "#   b           1  qb\n", "#   a           4  qa | qa2", "8 repositories, 1769 releases", "\no/a0\n", "\no/c3\n"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("file lacks %q:\n%s", want, b)
		}
	}
	if _, err := testGen(fakeGitHub(t, search, rel, nil).URL).generate(context.Background(), 20, bs); err == nil || !strings.Contains(err.Error(), "widen") {
		t.Errorf("too few: %v", err)
	}
}

func TestGenerateStopsWhenRateLimited(t *testing.T) {
	limited := true
	srv := fakeGitHub(t, map[string][]string{"q": {"o/a"}}, nil, &limited)
	if _, err := testGen(srv.URL).generate(context.Background(), 1, []bucket{{"a", 100, []string{"q"}}}); err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err = %v", err)
	}
}

func TestBuckets(t *testing.T) {
	bs := buckets(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC))
	total := 0
	for _, b := range bs {
		total += b.share
		for _, q := range b.queries {
			if !strings.Contains(q, "fork:false") {
				t.Errorf("%s: forks not excluded in %q", b.name, q)
			}
		}
	}
	if total != 100 || !strings.Contains(bs[1].queries[1], "pushed:>=2026-08-11") || !strings.Contains(bs[2].queries[0], "pushed:<2025-11-09") {
		t.Errorf("buckets = %+v", bs)
	}
}

func TestRunNeedsToken(t *testing.T) {
	t.Setenv(tokenEnv, "")
	var stderr bytes.Buffer
	if code := run(context.Background(), nil, &stderr); code != 2 || !strings.Contains(stderr.String(), tokenEnv) {
		t.Errorf("exit %d, %s", code, stderr.String())
	}
	if code := run(context.Background(), []string{"-n", "0"}, &stderr); code != 2 {
		t.Errorf("-n 0: exit %d", code)
	}
}
