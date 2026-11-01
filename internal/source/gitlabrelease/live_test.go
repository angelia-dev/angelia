//go:build live

package gitlabrelease_test

import (
	"os"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/gitlabrelease"
)

// TestLiveGitLabRunner fetches gitlab-org/gitlab-runner from gitlab.com. Run
// with `go test -tags live ./internal/source/gitlabrelease`. Anonymous access
// works for this public project; set GITLAB_TOKEN to use a token. The test
// makes a handful of requests, well within gitlab.com's anonymous limits.
func TestLiveGitLabRunner(t *testing.T) {
	c, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var opts []gitlabrelease.Option
	if os.Getenv("GITLAB_TOKEN") != "" {
		opts = append(opts, gitlabrelease.WithTokenEnv("GITLAB_TOKEN"))
	}
	s := gitlabrelease.New("", c.HTTPClient(), opts...)
	target := source.Target{Kind: gitlabrelease.Kind, Coordinates: map[string]string{"project": "gitlab-org/gitlab-runner"}}

	res, err := s.List(t.Context(), target, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) == 0 {
		t.Fatalf("got no versions, state %+v", res.State)
	}
	t.Logf("gitlab-runner: %d releases, newest %s", len(res.Versions), res.Versions[0].Raw)

	// Check the conditional request only when gitlab.com sent an ETag for
	// this endpoint (it may not for every project).
	if res.State.ETag != "" {
		again, err := s.List(t.Context(), target, res.State)
		if err != nil {
			t.Fatal(err)
		}
		if !again.NotModified {
			t.Error("second fetch with the ETag was not a 304")
		}
	}
}
