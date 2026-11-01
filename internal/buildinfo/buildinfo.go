// Package buildinfo exposes the version, commit and build date of the binary.
//
// The values are overridden at link time, for example:
//
//	go build -ldflags "-X github.com/angelia-dev/angelia/internal/buildinfo.Version=v0.1.0" ./cmd/angelia
package buildinfo

import "fmt"

// Version, Commit and Date are set with -ldflags "-X ..." at build time.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns a one-line description of the build.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version, Commit, Date)
}
