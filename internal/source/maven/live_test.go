//go:build live

package maven_test

import (
	"context"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/maven"
)

// TestLiveSLF4J lists org.slf4j:slf4j-api on Maven Central. Run with:
//
//	go test -tags live -run TestLiveSLF4J ./internal/source/maven
func TestLiveSLF4J(t *testing.T) {
	c, err := netguard.New(netguard.Config{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	s := maven.New("", c.HTTPClient())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tgt := source.Target{Kind: maven.Kind, Coordinates: map[string]string{"package": "org.slf4j:slf4j-api"}}
	res, err := s.List(ctx, tgt, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range res.Versions {
		if v.Raw == "2.0.16" {
			found = true
			if v.URL != "https://central.sonatype.com/artifact/org.slf4j/slf4j-api/2.0.16" {
				t.Errorf("URL = %q", v.URL)
			}
		}
	}
	if len(res.Versions) < 50 || !found {
		t.Fatalf("got %d versions, 2.0.16 found: %v", len(res.Versions), found)
	}
	again, err := s.List(ctx, tgt, res.State)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified {
		t.Errorf("second call with the ETag was not NotModified (state %+v)", res.State)
	}
}
