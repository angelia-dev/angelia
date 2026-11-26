//go:build live

package goproxy_test

import (
	"slices"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/goproxy"
)

func liveList(t *testing.T, mod string) []source.Version {
	t.Helper()
	c, err := netguard.New(netguard.Config{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	res, err := goproxy.New("", c.HTTPClient()).List(t.Context(), target(mod), source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Versions
}

func hasRaw(vs []source.Version, raw string) bool {
	return slices.ContainsFunc(vs, func(v source.Version) bool { return v.Raw == raw })
}

// TestLiveGorillaMux lists a module with lower-case path elements. Run with:
//
//	go test -tags live -run TestLive ./internal/source/goproxy
func TestLiveGorillaMux(t *testing.T) {
	vs := liveList(t, "github.com/gorilla/mux")
	if !hasRaw(vs, "v1.8.1") {
		t.Errorf("v1.8.1 not listed: %d versions", len(vs))
	}
	if vs[0].URL == "" {
		t.Errorf("no pkg.go.dev URL: %+v", vs[0])
	}
}

// TestLiveBurntSushiToml checks the case encoding ("!burnt!sushi") against
// the real proxy.
func TestLiveBurntSushiToml(t *testing.T) {
	if vs := liveList(t, "github.com/BurntSushi/toml"); !hasRaw(vs, "v1.4.0") {
		t.Errorf("v1.4.0 not listed: %d versions", len(vs))
	}
}

// TestLiveSQLiteRetractions: the accidental v2.0.x+incompatible tags of
// go-sqlite3 are retracted, so they are reported as yanked.
func TestLiveSQLiteRetractions(t *testing.T) {
	vs := liveList(t, "github.com/mattn/go-sqlite3")
	for _, v := range vs {
		if v.Raw == "v2.0.0+incompatible" {
			if !v.Yanked {
				t.Errorf("v2.0.0+incompatible is not yanked: %+v", v)
			}
			return
		}
	}
	t.Error("v2.0.0+incompatible not listed")
}
