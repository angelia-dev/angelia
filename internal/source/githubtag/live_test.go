//go:build live

package githubtag_test

import (
	"os"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/githubtag"
)

// TestLiveGitTags fetches tags of git/git (which publishes tags, not
// releases) from api.github.com. Run with
// `go test -tags live ./internal/source/githubtag`; GITHUB_TOKEN must be set
// (the test is skipped otherwise: the anonymous limit is 60 requests per hour).
func TestLiveGitTags(t *testing.T) {
	if os.Getenv("GITHUB_TOKEN") == "" {
		t.Skip("GITHUB_TOKEN is unset; the anonymous rate limit (60 requests per hour) is too low")
	}
	c, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := githubtag.New("", c.HTTPClient(), githubtag.WithTokenEnv("GITHUB_TOKEN"))
	target := source.Target{Kind: githubtag.Kind, Coordinates: map[string]string{"repo": "git/git"}}

	res, err := s.List(t.Context(), target, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) == 0 || res.State.ETag == "" {
		t.Fatalf("got %d versions, state %+v", len(res.Versions), res.State)
	}
	t.Logf("git/git: %d tags, first %s", len(res.Versions), res.Versions[0].Raw)

	again, err := s.List(t.Context(), target, res.State)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified {
		t.Error("second fetch with the ETag was not a 304")
	}
}
