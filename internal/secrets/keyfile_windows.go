//go:build windows

package secrets

import "os"

// checkKeyFileMode checks nothing on Windows, where the mode bits do not
// describe who can read a file.
func checkKeyFileMode(string, string, os.FileInfo) (string, error) { return "", nil }
