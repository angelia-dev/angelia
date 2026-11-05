//go:build !windows

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOwnerTrusted(t *testing.T) {
	for _, c := range []struct {
		uid, euid int64
		want      bool
	}{{1000, 1000, true}, {0, 1000, true}, {1001, 1000, false}, {0, 0, true}, {1000, 0, false}} {
		if got := ownerTrusted(c.uid, c.euid); got != c.want {
			t.Errorf("ownerTrusted(%d, %d) = %v", c.uid, c.euid, got)
		}
	}
}

// Review S1 (#184): a symlinked grants.yaml is refused, so the checks
// cannot be pointed at another file.
func TestPluginGrantsSymlinkRefused(t *testing.T) {
	dir := pluginDir(t, "", "crates", manifestOf("crates"), "")
	target := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(target, []byte("allowed_hosts: [evil.example]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "crates", GrantsFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadPlugins(t, dir); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("err = %v, want the symlink refused", err)
	}
	// A dangling link too.
	dir = pluginDir(t, "", "crates", manifestOf("crates"), "")
	if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), filepath.Join(dir, "crates", GrantsFile)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadPlugins(t, dir); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("dangling: err = %v, want the symlink refused", err)
	}
}

// Review S1 (#184): a plugin folder or plugins_dir that others can write
// lets them replace grants.yaml, so it is refused like the file; a sticky
// directory is fine.
func TestPluginFoldersMode(t *testing.T) {
	for _, mode := range []os.FileMode{0o775, 0o757, 0o777} {
		dir := pluginDir(t, "", "crates", manifestOf("crates"), "allowed_hosts: []\n")
		if err := os.Chmod(filepath.Join(dir, "crates"), mode); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadPlugins(t, dir); err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Errorf("<kind> mode %04o: err = %v, want refused", mode, err)
		}
		if err := os.Chmod(filepath.Join(dir, "crates"), 0o755); err != nil { //nolint:gosec // G302: a normal folder mode.
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_, _, err := loadPlugins(t, dir)
		if err == nil || !strings.Contains(err.Error(), "plugins_dir: ") || !strings.Contains(err.Error(), "chmod go-w "+dir) {
			t.Errorf("plugins_dir mode %04o: err = %v, want refused", mode, err)
		}
	}
	dir := pluginDir(t, "", "crates", manifestOf("crates"), "allowed_hosts: []\n")
	for _, d := range []string{dir, filepath.Join(dir, "crates")} {
		if err := os.Chmod(d, 0o777|os.ModeSticky); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := loadPlugins(t, dir); err != nil {
		t.Errorf("sticky directories refused: %v", err)
	}
}

// Review S1 (#184): a grants file or folder owned by another user (not
// root) is refused, since its owner can rewrite it whatever the mode.
// Changing owners needs root.
func TestPluginForeignOwnerRefused(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("chown needs root; ownerTrusted is covered by TestOwnerTrusted")
	}
	const other = 54321
	for _, which := range []string{"grants", "kind", "plugins_dir"} {
		dir := pluginDir(t, "", "crates", manifestOf("crates"), "allowed_hosts: []\n")
		p := map[string]string{
			"grants":      filepath.Join(dir, "crates", GrantsFile),
			"kind":        filepath.Join(dir, "crates"),
			"plugins_dir": dir,
		}[which]
		if err := os.Chown(p, other, other); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadPlugins(t, dir); err == nil || !strings.Contains(err.Error(), "owned by uid 54321") {
			t.Errorf("%s: err = %v, want refused", which, err)
		}
	}
}

// Q3 (#184): a plugins_dir that exists but cannot be listed is an error
// the caller can recognise (config check warns for the default dir).
func TestPluginsDirUnreadable(t *testing.T) {
	f := filepath.Join(t.TempDir(), "plugins")
	if err := os.WriteFile(f, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadPlugins(t, f); !errors.Is(err, ErrPluginsDirUnreadable) {
		t.Fatalf("err = %v, want ErrPluginsDirUnreadable", err)
	}
}

// Review N1/N2 (#184): names from the file system are quoted, and a
// symlinked plugin folder is ignored with its own warning.
func TestPluginFolderNamesQuotedAndSymlinksIgnored(t *testing.T) {
	dir := pluginDir(t, "", "crates", manifestOf("crates"), "")
	if err := os.Mkdir(filepath.Join(dir, "a\x1b[31m"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, _, err := loadPlugins(t, dir)
	if err == nil || strings.Contains(err.Error(), "\x1b") || !strings.Contains(err.Error(), `"a\x1b[31m"`) {
		t.Fatalf("err = %q, want the name quoted", err)
	}
	dir = pluginDir(t, "", "crates", manifestOf("crates"), "")
	if err := os.Symlink(filepath.Join(dir, "crates"), filepath.Join(dir, "linked")); err != nil {
		t.Fatal(err)
	}
	ps, warns, err := loadPlugins(t, dir)
	if err != nil || len(ps) != 1 || !strings.Contains(strings.Join(warns, "\n"), `"linked": symbolic link, ignored`) {
		t.Fatalf("plugins %+v, warnings %q, err %v", ps, warns, err)
	}
}
