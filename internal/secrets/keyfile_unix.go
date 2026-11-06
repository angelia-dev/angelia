//go:build !windows

package secrets

import (
	"fmt"
	"os"
)

// checkKeyFileMode refuses a key file other users can read, or group or
// others can write, and warns when the group can read it (ADR 0024 §4).
// Ownership is not checked.
func checkKeyFileMode(path, what string, info os.FileInfo) (string, error) {
	perm := info.Mode().Perm()
	switch {
	case perm&0o004 != 0:
		return "", fmt.Errorf("secrets: %s %s is readable by other users (mode %04o); run: chmod 600 %s", what, path, perm, path)
	case perm&0o022 != 0:
		return "", fmt.Errorf("secrets: %s %s is writable by group or others (mode %04o); run: chmod 600 %s", what, path, perm, path)
	case perm&0o040 != 0:
		return fmt.Sprintf("secrets: %s %s is readable by its group (mode %04o); chmod 600 it unless the group is the service's own", what, path, perm), nil
	}
	return "", nil
}
