// Package buildinfo exposes the version, commit and build date of the binary.
//
// The values are overridden at link time, for example:
//
//	go build -ldflags "-X github.com/angelia-dev/angelia/internal/buildinfo.version=v0.1.0" ./cmd/angelia
//
// Use the Version, Commit and Date functions to read them.
package buildinfo

import "fmt"

// version, commit and date are set with -ldflags "-X ..." at build time.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// Version returns the release version, or "dev" for an untagged build.
func Version() string { return version }

// Commit returns the short git commit the binary was built from, or "none".
func Commit() string { return commit }

// Date returns the build date in UTC (RFC 3339), or "unknown".
func Date() string { return date }

// String returns a one-line description of the build.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s)", Version(), Commit(), Date())
}
