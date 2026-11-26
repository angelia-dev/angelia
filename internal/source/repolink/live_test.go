//go:build live

package repolink_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/npm"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/source/repolink"
)

// The fixed measurement list of ADR 0045 (architecture.md section 10, Q3):
// popular packages, a few of them monorepo members (pydantic-core,
// react-dom, @babel/core, @types/node) and two that declare no repository
// (protobuf, grpcio-status).
var (
	livePyPI = []string{
		"requests", "urllib3", "boto3", "django", "flask", "numpy", "pandas", "pydantic", "pydantic-core", "fastapi",
		"sqlalchemy", "pytest", "click", "rich", "httpx", "jinja2", "cryptography", "black", "protobuf", "grpcio-status",
	}
	liveNPM = []string{
		"react", "react-dom", "lodash", "express", "typescript", "@babel/core", "@types/node", "vue", "next", "webpack",
		"eslint", "axios", "chalk", "commander", "prettier", "jest", "vite", "tailwindcss", "zod", "rxjs",
	}
)

type liveCounts struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *liveCounts) NotesEnrichment(kind, result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	c.m[kind+"/"+result]++
}

// TestLiveMeasurement runs discovery and matching for the packages above
// against the live registries and api.github.com, treating each package's
// three newest versions as the new ones of one check, and prints the counts
// per result. It uses ANGELIA_LIVE_GITHUB_TOKEN when set, otherwise it is
// anonymous (about one request a minute, so the run takes about 40 minutes).
// With ANGELIA_LIVE_DISCOVERY_ONLY=1 it sends no request to api.github.com and
// only reports which packages declare a repository.
//
//	go test -tags live -run TestLiveMeasurement -timeout 120m -v ./internal/source/repolink
func TestLiveMeasurement(t *testing.T) {
	nc, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	hc := nc.HTTPClient()
	counts := &liveCounts{}
	opts := []repolink.Option{repolink.WithRecorder(counts), repolink.WithTimeout(10 * time.Minute)}
	if tok := os.Getenv("ANGELIA_LIVE_GITHUB_TOKEN"); tok != "" {
		opts = append(opts, repolink.WithToken(tok))
	}
	f := repolink.NewFiller("", hc, opts...)

	type row struct {
		kind, pkg, repo, got string
		tried, matched       int
	}
	discovery := os.Getenv("ANGELIA_LIVE_DISCOVERY_ONLY") != ""
	var rows []row
	run := func(kind string, src source.Source, names []string) {
		for _, name := range names {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
			tgt := source.Target{Kind: kind, Coordinates: map[string]string{"name": name}}
			res, err := src.List(ctx, tgt, source.FetchState{})
			if err != nil {
				cancel()
				t.Errorf("%s %s: %v", kind, name, err)
				continue
			}
			var newest []source.Version
			for _, v := range res.Versions {
				if !v.Yanked && len(newest) < 3 {
					newest = append(newest, v)
				}
			}
			if len(newest) == 0 {
				cancel()
				continue
			}
			repo, _ := newest[0].Metadata["repository_url"].(string)
			before := slices.Clone(sortedKeys(counts))
			out := newest
			if !discovery {
				out = f.Fill(ctx, tgt, newest)
			}
			cancel()
			r := row{kind: kind, pkg: name, repo: repo, tried: len(out)}
			for _, v := range out {
				if discovery {
					break // no GitHub request was made: nothing to count
				}
				if v.Notes != nil || (v.NotesURL != "" && strings.Contains(v.NotesURL, "/releases/")) {
					r.matched++
				}
			}
			r.got = diffKeys(before, sortedKeys(counts))
			rows = append(rows, r)
			t.Logf("%-5s %-18s repo=%-48s %-14s %d/%d versions with a release", kind, name, repo, r.got, r.matched, r.tried)
		}
	}
	run("pypi", pypi.New(pypi.DefaultBaseURL, hc), livePyPI)
	run("npm", npm.New(npm.DefaultBaseURL, hc), liveNPM)

	t.Log("results per check (kind/result: packages):")
	counts.mu.Lock()
	keys := make([]string, 0, len(counts.m))
	for k := range counts.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("  %-24s %d", k, counts.m[k])
	}
	counts.mu.Unlock()
	var withRepo, tried, matched int
	for _, r := range rows {
		if r.repo != "" {
			withRepo++
		}
		tried += r.tried
		matched += r.matched
	}
	t.Logf("packages: %d, declaring a repository: %d; versions looked at: %d, with a GitHub release: %d (%.0f%%)",
		len(rows), withRepo, tried, matched, 100*float64(matched)/float64(max(tried, 1)))
}

func sortedKeys(c *liveCounts) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k, n := range c.m {
		out = append(out, fmt.Sprintf("%s=%d", k, n))
	}
	sort.Strings(out)
	return out
}

func diffKeys(before, after []string) string {
	for _, a := range after {
		if !slices.Contains(before, a) {
			_, res, _ := strings.Cut(strings.SplitN(a, "=", 2)[0], "/")
			return res
		}
	}
	return "none"
}
