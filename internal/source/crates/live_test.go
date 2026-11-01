//go:build live

package crates_test

import (
	"context"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/crates"
)

// TestLiveSerde fetches the real serde index and API record. Run with:
//
//	go test -tags live -run TestLiveSerde ./internal/source/crates
func TestLiveSerde(t *testing.T) {
	c, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := crates.New("", c.HTTPClient())
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	tgt := source.Target{Kind: crates.Kind, Coordinates: map[string]string{"name": "serde"}}
	res, err := s.List(ctx, tgt, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range res.Versions {
		if v.Raw == "1.0.0" {
			found = true
			if v.PublishedAt.IsZero() || v.URL == "" {
				t.Errorf("serde 1.0.0 incomplete: %+v", v)
			}
		}
	}
	if len(res.Versions) < 100 || !found {
		t.Fatalf("got %d versions, 1.0.0 found: %v", len(res.Versions), found)
	}
	again, err := s.List(ctx, tgt, res.State)
	if err != nil {
		t.Fatal(err)
	}
	// Only logged: whether the CDN honours the ETag is its business.
	t.Logf("serde: %d versions, second call NotModified=%v", len(res.Versions), again.NotModified)
}
