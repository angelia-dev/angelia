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
	"sync/atomic"
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
	g := &gen{api: api, token: "tok-for-test", hc: http.DefaultClient, log: slog.New(slog.DiscardHandler), now: time.Now}
	g.pause = func(context.Context, time.Duration) error { return nil }
	return g
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
	for _, want := range []string{"\no/a0\no/b1\no/c0\no/a2\n", "# Generated 2026-11-09", "#   b           1  qb\n", "#   a           4  qa | qa2", "8 repositories, 1769 releases", "\no/a0\n", "\no/c3\n"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("file lacks %q:\n%s", want, b)
		}
	}
	if _, err := testGen(fakeGitHub(t, search, rel, nil).URL).generate(context.Background(), 20, bs); err == nil || !strings.Contains(err.Error(), "widen") {
		t.Errorf("too few: %v", err)
	}
}

// A rate limit is waited out (Retry-After, else X-RateLimit-Reset) and the
// request sent again; one that never lifts stops the run, and the list found
// so far is written next to the real one.
func TestRateLimitIsWaitedOut(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	answers := []func(w http.ResponseWriter){
		func(w http.ResponseWriter) { // primary limit used up
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", fmt.Sprint(now.Add(10*time.Minute).Unix()))
			w.WriteHeader(http.StatusForbidden)
		},
		func(w http.ResponseWriter) { // secondary limit
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
		},
		func(w http.ResponseWriter) { // a plain 403 is not a rate limit: skipped
			w.WriteHeader(http.StatusForbidden)
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := int(calls.Add(1)) - 1; k < len(answers) && strings.Contains(r.URL.Path, "/releases") {
			answers[k](w)
			return
		}
		if r.URL.Path == "/search/repositories" {
			_, _ = w.Write([]byte(`{"items":[{"full_name":"o/a"},{"full_name":"o/b"}]}`))
			return
		}
		_, _ = w.Write([]byte(`[{"id":1}]`))
	}))
	defer srv.Close()
	g := testGen(srv.URL)
	g.now = func() time.Time { return now }
	var waits []time.Duration
	g.pause = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	calls.Store(-1) // the search comes first
	l, err := g.generate(context.Background(), 1, []bucket{{"a", 100, []string{"q"}}})
	if err != nil {
		t.Fatal(err)
	}
	// o/a: 403 limit (wait 10m5s), 429 (wait 7s), plain 403 (skipped); o/b kept.
	if len(waits) != 2 || waits[0] != 10*time.Minute+5*time.Second || waits[1] != 7*time.Second || strings.Join(l.repos, " ") != "o/b" {
		t.Errorf("waits %v, repos %v", waits, l.repos)
	}
}

func TestRateLimitThatNeverLifts(t *testing.T) {
	limited := true
	srv := fakeGitHub(t, map[string][]string{"q": {"o/a"}}, nil, &limited)
	g := testGen(srv.URL)
	waits := 0
	g.pause = func(context.Context, time.Duration) error { waits++; return nil }
	if _, err := g.generate(context.Background(), 1, []bucket{{"a", 100, []string{"q"}}}); err == nil || !strings.Contains(err.Error(), "rate limited") || waits != maxRateWaits {
		t.Errorf("err = %v after %d waits", err, waits)
	}
}

// A fatal error keeps the list found so far in out.partial; out is untouched.
func TestPartialListOnError(t *testing.T) {
	search := map[string][]string{"qa": {"o/a0", "o/a1"}, "qb": {"o/b0"}}
	rel := map[string]int{"o/a0": 1, "o/a1": 2, "o/b0": 1}
	srv := fakeGitHub(t, search, rel, nil)
	bs := []bucket{{"a", 50, []string{"qa"}}, {"b", 50, []string{"qb"}}}
	out := filepath.Join(t.TempDir(), "repos.txt")
	if err := os.WriteFile(out, []byte("good/list\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := testGen(srv.URL)
	if code := g.write(context.Background(), 4, bs, out, time.Now()); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(out); string(b) != "good/list\n" { //nolint:gosec // test temp dir
		t.Errorf("the list was replaced: %s", b)
	}
	b, err := os.ReadFile(out + ".partial") //nolint:gosec // test temp dir
	if err != nil || !bytes.Contains(b, []byte("# INCOMPLETE: 3 of 4")) || !bytes.Contains(b, []byte("\no/a1\n")) {
		t.Errorf("partial: %v\n%s", err, b)
	}
	// Success writes the list itself, readable by everyone (public data).
	if code := g.write(context.Background(), 2, bs, out, time.Now()); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm()&0o044 == 0 {
		t.Errorf("mode: %v %v", fi, err)
	}
}

// Lines take the buckets in turn, so the first N are a mix.
func TestInterleave(t *testing.T) {
	l := &list{
		repos:    []string{"a1", "a2", "a3", "b1", "c1", "c2"},
		buckets:  []bucket{{name: "a"}, {name: "b"}, {name: "c"}},
		byBucket: map[string][]string{"a": {"a1", "a2", "a3"}, "b": {"b1"}, "c": {"c1", "c2"}},
	}
	if got := strings.Join(interleave(l), " "); got != "a1 b1 c1 a2 c2 a3" {
		t.Errorf("interleave = %s", got)
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
