//go:build live

package githubrelease_test

import (
	"os"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubrelease"
)

// TestLiveCLICLI fetches cli/cli from api.github.com. Run with
// `go test -tags live ./internal/source/githubrelease`; set GITHUB_TOKEN to
// avoid the 60 requests per hour anonymous limit.
func TestLiveCLICLI(t *testing.T) {
	c, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var opts []githubrelease.Option
	if os.Getenv("GITHUB_TOKEN") != "" {
		opts = append(opts, githubrelease.WithTokenEnv("GITHUB_TOKEN"))
	}
	s := githubrelease.New("", c.HTTPClient(), opts...)
	target := source.Target{Kind: githubrelease.Kind, Coordinates: map[string]string{"repo": "cli/cli"}}

	res, err := s.List(t.Context(), target, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) == 0 || res.State.ETag == "" {
		t.Fatalf("got %d versions, state %+v", len(res.Versions), res.State)
	}
	t.Logf("cli/cli: %d releases, newest %s", len(res.Versions), res.Versions[0].Raw)

	again, err := s.List(t.Context(), target, res.State)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified {
		t.Error("second fetch with the ETag was not a 304")
	}
}
