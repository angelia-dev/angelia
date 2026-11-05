//go:build !windows

package config

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// checkTrust refuses a plugin folder or grants file that someone other
// than this process's user or root could change (ADR 0025): one owned by
// another user, or writable by group or others. A directory with the
// sticky bit may be group- or world-writable (others cannot replace
// entries they do not own).
func checkTrust(path string, info os.FileInfo) error {
	perm := info.Mode().Perm()
	if perm&0o022 != 0 && (!info.IsDir() || info.Mode()&os.ModeSticky == 0) {
		return fmt.Errorf("%s grants network access but is writable by group or others (mode %04o); run: chmod go-w %s", path, perm, path)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: cannot read its owner", path)
	}
	if euid := os.Geteuid(); euid >= 0 && !ownerTrusted(int64(st.Uid), int64(euid)) {
		return fmt.Errorf("%s grants network access but is owned by uid %d, not by this user (uid %d) or root", path, st.Uid, euid)
	}
	return nil
}

// ownerTrusted reports whether a file owned by uid may grant network
// access to a process running as euid.
func ownerTrusted(uid, euid int64) bool { return uid == euid || uid == 0 }

// openNoFollow opens path for reading and refuses a symbolic link, so the
// checks apply to the file itself, not to the target of a link.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // G304: the operator's plugins_dir.
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%s: is a symbolic link; grants.yaml must be a regular file", path)
	}
	return f, err
}
