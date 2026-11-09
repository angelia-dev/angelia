// Command genrepos builds the watch list of Phase 0 benchmark 3
// (bench/github/repos.txt, story angelia-dev/angelia#64): public GitHub
// repositories that publish releases, a mix of very popular, active, quiet
// and smaller ones.
//
//	ANGELIA_BENCH_GITHUB_TOKEN=… make bench-github-repos
//
// It searches repositories (GET /search/repositories, sorted by stars,
// forks excluded) in four buckets and keeps a candidate only if
// GET /repos/{repo}/releases?per_page=1 lists a release. A bucket that runs
// short passes its shortfall to the next. It sends about 40 search requests
// and one request per candidate (about 3,000), paced under GitHub's
// secondary limits; that uses most of an hour of the token's 5,000, so run
// it at least an hour before the benchmark (bench/load waits for a fresh
// window anyway).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const tokenEnv = "ANGELIA_BENCH_GITHUB_TOKEN"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stderr)
	stop()
	os.Exit(code)
}

// bucket is a kind of repository and the share of the list it fills. Its
// searches run in order; each returns at most 1,000 results (GitHub's cap),
// so a bucket splits its star range over several.
type bucket struct {
	name    string
	share   int // percent of n
	queries []string
}

// buckets are the searches, relative to today.
func buckets(today time.Time) []bucket {
	d := func(days int) string { return today.AddDate(0, 0, -days).Format("2006-01-02") }
	return []bucket{
		{"popular", 35, []string{"stars:>=50000 fork:false archived:false", "stars:20000..49999 fork:false archived:false"}},
		{"active", 30, []string{"stars:5000..19999 fork:false archived:false pushed:>=" + d(90), "stars:2000..4999 fork:false archived:false pushed:>=" + d(90)}},
		{"quiet", 20, []string{"stars:5000..19999 fork:false pushed:<" + d(365), "stars:2000..4999 fork:false pushed:<" + d(365)}},
		{"smaller", 15, []string{"stars:800..1999 fork:false archived:false pushed:>=" + d(180), "stars:300..799 fork:false archived:false pushed:>=" + d(180)}},
	}
}

type gen struct {
	api, token string
	hc         *http.Client
	searchGap  time.Duration // GitHub allows 30 searches a minute
	checkGap   time.Duration // between release checks, under the secondary limits
	log        *slog.Logger
}

func run(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("genrepos", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "bench/github/repos.txt", "file to write")
	api := fs.String("api", "https://api.github.com", "GitHub API base URL")
	n := fs.Int("n", 2000, "repositories in the list")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *n < 1 {
		_, _ = fmt.Fprintln(stderr, "genrepos: -n must be positive; no arguments")
		return 2
	}
	tok := strings.TrimSpace(os.Getenv(tokenEnv))
	if tok == "" {
		_, _ = fmt.Fprintf(stderr, "genrepos: set %s to a token with public read access\n", tokenEnv)
		return 2
	}
	g := &gen{
		api: strings.TrimSuffix(*api, "/"), token: tok, hc: &http.Client{Timeout: 30 * time.Second},
		searchGap: 2100 * time.Millisecond, checkGap: 250 * time.Millisecond, log: slog.New(slog.NewTextHandler(stderr, nil)),
	}
	today := time.Now().UTC()
	list, err := g.generate(ctx, *n, buckets(today))
	if err != nil {
		g.log.Error("generate", "err", err)
		return 1
	}
	if err := writeList(*out, list, today); err != nil {
		g.log.Error("write", "err", err)
		return 1
	}
	g.log.Info("wrote the watch list", "file", *out, "repositories", len(list.repos), "first_scan_pages", list.pages)
	return 0
}

// list is the result: repositories in bucket order and what was found.
type list struct {
	repos    []string
	counts   map[string]int // per bucket
	buckets  []bucket
	releases int64 // total releases (one page of 1 per release)
	pages    int64 // requests of a first full scan: ceil(releases/100), 1 to 10 per repository
	checked  int   // candidates checked
}

func (g *gen) generate(ctx context.Context, n int, bs []bucket) (*list, error) {
	l := &list{counts: map[string]int{}, buckets: bs}
	seen := map[string]bool{}
	carry := 0
	for i, b := range bs {
		want := n*b.share/100 + carry
		if i == len(bs)-1 {
			want = n - len(l.repos)
		}
		got := 0
		for _, q := range b.queries {
			for page := 1; page <= 10 && got < want; page++ {
				items, more, err := g.search(ctx, q, page)
				if err != nil {
					return nil, fmt.Errorf("bucket %s: %w", b.name, err)
				}
				added, err := g.keep(ctx, l, seen, items, want-got)
				if err != nil {
					return nil, fmt.Errorf("bucket %s: %w", b.name, err)
				}
				if got += added; !more {
					break
				}
			}
		}
		l.counts[b.name] = got
		carry = want - got
		g.log.Info("bucket done", "bucket", b.name, "repositories", got, "wanted", want)
	}
	if len(l.repos) < n {
		return nil, fmt.Errorf("found %d repositories with releases, want %d: widen the buckets", len(l.repos), n)
	}
	return l, nil
}

// keep checks the candidates in order and adds up to want of those with a
// release to l; it returns how many it added.
func (g *gen) keep(ctx context.Context, l *list, seen map[string]bool, items []string, want int) (int, error) {
	got := 0
	for _, repo := range items {
		if got >= want {
			break
		}
		key := strings.ToLower(repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		count, err := g.releases(ctx, repo)
		l.checked++
		if err != nil {
			return got, err
		}
		if count == 0 {
			continue
		}
		l.repos = append(l.repos, repo)
		l.releases += count
		l.pages += min(10, (count+99)/100)
		got++
	}
	return got, nil
}

var errRateLimited = errors.New("rate limited by GitHub: wait for the window to reset and run again")

func (g *gen) get(ctx context.Context, u string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+g.token)
	resp, err := g.hc.Do(req) //nolint:gosec // GitHub's API (or a test server)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || (resp.StatusCode == http.StatusForbidden && (resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0")) {
		_ = resp.Body.Close()
		return nil, errRateLimited
	}
	return resp, nil
}

func (g *gen) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// search returns one page of full names and whether more pages exist.
func (g *gen) search(ctx context.Context, q string, page int) ([]string, bool, error) {
	if err := g.sleep(ctx, g.searchGap); err != nil {
		return nil, false, err
	}
	v := url.Values{"q": {q}, "sort": {"stars"}, "order": {"desc"}, "per_page": {"100"}, "page": {strconv.Itoa(page)}}
	resp, err := g.get(ctx, g.api+"/search/repositories?"+v.Encode())
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("search %q page %d: %s", q, page, resp.Status)
	}
	var body struct {
		Items []struct {
			FullName string `json:"full_name"`
			Fork     bool   `json:"fork"`
			Private  bool   `json:"private"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&body); err != nil {
		return nil, false, fmt.Errorf("search %q page %d: %w", q, page, err)
	}
	var out []string
	for _, it := range body.Items {
		if !it.Fork && !it.Private && repoRe.MatchString(it.FullName) {
			out = append(out, it.FullName)
		}
	}
	return out, len(body.Items) == 100, nil
}

// repoRe is the owner/name form the github-release source accepts.
var repoRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

var lastPageRe = regexp.MustCompile(`[?&]page=(\d+)[^>]*>;\s*rel="last"`)

// releases returns how many releases repo has (0 when none, or when the
// repository cannot be read), from one request of one release per page.
func (g *gen) releases(ctx context.Context, repo string) (int64, error) {
	if err := g.sleep(ctx, g.checkGap); err != nil {
		return 0, err
	}
	resp, err := g.get(ctx, g.api+"/repos/"+repo+"/releases?per_page=1")
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		g.log.Info("skipped", "repo", repo, "status", resp.Status)
		return 0, nil
	}
	var rels []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&rels); err != nil || len(rels) == 0 {
		return 0, nil //nolint:nilerr // an unreadable listing counts as no release
	}
	if m := lastPageRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
			return n, nil
		}
	}
	return 1, nil
}

func writeList(path string, l *list, today time.Time) error {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	p("# Watch list of Phase 0 benchmark 3 (angelia-dev/angelia#64, bench/README.md):")
	p("# public GitHub repositories with at least one release, one owner/name per line.")
	p("# Generated %s by `make bench-github-repos` (bench/github/genrepos): GitHub search,", today.Format("2006-01-02"))
	p("# sorted by stars, forks excluded, each candidate kept if GET /repos/{repo}/releases?per_page=1")
	p("# lists a release (%d candidates checked). Buckets, in file order:", l.checked)
	for _, bk := range l.buckets {
		p("#   %-8s %4d  %s", bk.name, l.counts[bk.name], strings.Join(bk.queries, " | "))
	}
	p("# %d repositories, %d releases; a first (full) scan of all of them reads %d pages of 100.", len(l.repos), l.releases, l.pages)
	for _, r := range l.repos {
		p("%s", r)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}
