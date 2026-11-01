//go:build live

package npm_test

import (
	"context"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/npm"
)

// TestLiveReact fetches the real react packument. Run with:
//
//	go test -tags live -run TestLiveReact ./internal/source/npm
func TestLiveReact(t *testing.T) {
	// The full react packument is 7.1 MB today, under netguard's default
	// 10 MiB cap, but other packages are not (see the package documentation);
	// use the recommended 64 MiB cap for npm.
	c, err := netguard.New(netguard.Config{MaxBodyBytes: 64 << 20, Timeout: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	s := npm.New("", c.HTTPClient())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	tgt := source.Target{Kind: npm.Kind, Coordinates: map[string]string{"name": "react"}}
	res, err := s.List(ctx, tgt, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range res.Versions {
		if v.Raw == "18.2.0" {
			found = true
			if v.PublishedAt.IsZero() || v.URL == "" {
				t.Errorf("react 18.2.0 incomplete: %+v", v)
			}
		}
	}
	if len(res.Versions) < 100 || !found {
		t.Fatalf("got %d versions, 18.2.0 found: %v", len(res.Versions), found)
	}
	again, err := s.List(ctx, tgt, res.State)
	if err != nil {
		t.Fatal(err)
	}
	// Only logged, not asserted: whether the registry's CDN honours the ETag
	// is its business and can vary, so it must not fail the test.
	t.Logf("react: %d versions, second call NotModified=%v", len(res.Versions), again.NotModified)
}
