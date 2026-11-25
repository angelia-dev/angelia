//go:build live

package oci_test

import (
	"regexp"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/oci"
)

var semverTag = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// TestLive lists tags on Docker Hub and GHCR anonymously. Run with
// `go test -tags live ./internal/source/oci`.
func TestLive(t *testing.T) {
	c, err := netguard.New(netguard.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s := oci.New("", c.HTTPClient())
	for _, image := range []string{"library/alpine", "ghcr.io/angelia-dev/angelia"} {
		res, err := s.List(t.Context(), img(image), source.FetchState{})
		if err != nil {
			t.Errorf("%s: %v", image, err)
			continue
		}
		n := 0
		for _, v := range res.Versions {
			if semverTag.MatchString(v.Raw) {
				n++
			}
		}
		if n == 0 {
			t.Errorf("%s: %d tags, none SemVer", image, len(res.Versions))
		}
		t.Logf("%s: %d tags, %d SemVer", image, len(res.Versions), n)
	}
}
