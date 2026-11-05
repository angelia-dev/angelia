//go:build windows

package config

import "os"

// checkTrust is a no-op on Windows, where POSIX modes and owners do not
// apply (as for the client config check of #74).
func checkTrust(string, os.FileInfo) error { return nil }

func openNoFollow(path string) (*os.File, error) {
	return os.Open(path) //nolint:gosec // G304: the operator's plugins_dir.
}
