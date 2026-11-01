//go:build live

package pypi_test

import (
	"context"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
)

// TestLiveRequests fetches the real "requests" project from pypi.org.
// Run with: go test -tags live ./internal/source/pypi
func TestLiveRequests(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nc, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := pypi.New(pypi.DefaultBaseURL, nc.HTTPClient())
	res, err := s.List(ctx, target("requests"), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Versions) < 100 {
		t.Fatalf("got %d versions, want at least 100", len(res.Versions))
	}
	again, err := s.List(ctx, target("Requests"), res.State)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified {
		t.Log("second request was not a 304 (CDN may not honour validators)")
	}
}
