package secrets

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeKey(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "secrets.key")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

var keyB64 = base64.StdEncoding.EncodeToString(master(7))

func TestLoadKeyringFromFile(t *testing.T) {
	want := ring(t, 7).PrimaryID()
	for _, content := range []string{keyB64, keyB64 + "\n", keyB64 + "\r\n"} {
		k, warns, err := LoadKeyring(KeyConfig{KeyFile: writeKey(t, content, 0o600)})
		if err != nil || len(warns) != 0 || k.PrimaryID() != want {
			t.Fatalf("%q: %v %v", content, warns, err)
		}
	}
	// A symbolic link is followed (Kubernetes secret volumes).
	target := writeKey(t, keyB64, 0o400)
	link := filepath.Join(t.TempDir(), "link.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if k, _, err := LoadKeyring(KeyConfig{KeyFile: link}); err != nil || k.PrimaryID() != want {
		t.Fatalf("symlink: %v", err)
	}
}

func TestLoadKeyringRejectsBadContent(t *testing.T) {
	secretish := base64.StdEncoding.EncodeToString([]byte(canary))
	for _, content := range []string{
		"", "\n", keyB64 + "\n\n", " " + keyB64, keyB64 + " ", keyB64[:43],
		base64.StdEncoding.EncodeToString(master(7)[:31]), base64.RawStdEncoding.EncodeToString(master(7)),
		strings.ReplaceAll(keyB64, "B", "-"), secretish, canary,
		base64.URLEncoding.EncodeToString(append(master(7)[:30], 0xfb, 0xff)),
	} {
		_, _, err := LoadKeyring(KeyConfig{KeyFile: writeKey(t, content, 0o600)})
		if err == nil {
			t.Fatalf("%q was accepted", content)
		}
		noLeak(t, "key file error", err.Error())
		if strings.TrimSpace(content) != "" && strings.Contains(err.Error(), strings.TrimSpace(content)) {
			t.Fatalf("error quotes the file: %v", err)
		}
		if !strings.Contains(err.Error(), "angelia secrets keygen") {
			t.Fatalf("error does not say how to make a key: %v", err)
		}
	}
	if _, _, err := LoadKeyring(KeyConfig{KeyFile: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}
	if _, _, err := LoadKeyring(KeyConfig{KeyFile: filepath.Join(t.TempDir(), "none")}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
}

func TestLoadKeyringModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are not checked on Windows")
	}
	for mode, want := range map[os.FileMode]string{
		0o600: "", 0o400: "", 0o440: "warn", 0o640: "warn",
		0o644: "chmod 600", 0o604: "chmod 600", 0o444: "chmod 600", 0o660: "chmod 600", 0o602: "chmod 600", 0o620: "chmod 600",
	} {
		k, warns, err := LoadKeyring(KeyConfig{KeyFile: writeKey(t, keyB64, mode)})
		switch want {
		case "":
			if err != nil || len(warns) != 0 || k == nil {
				t.Errorf("mode %04o: %v %v", mode, warns, err)
			}
		case "warn":
			if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "group") {
				t.Errorf("mode %04o: %v %v", mode, warns, err)
			}
		default:
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("mode %04o: %v", mode, err)
			}
		}
	}
}

func TestLoadKeyringSources(t *testing.T) {
	if k, w, err := LoadKeyring(KeyConfig{}); k != nil || w != nil || err != nil {
		t.Fatal("no key configured must give a nil keyring")
	}
	file := writeKey(t, keyB64, 0o600)
	if _, _, err := LoadKeyring(KeyConfig{KeyFile: file, Key: NewValueString(keyB64)}); err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("both set: %v", err)
	}
	if _, _, err := LoadKeyring(KeyConfig{PreviousKeyFiles: []string{file}}); err == nil {
		t.Fatal("previous keys without a primary were accepted")
	}
	k, _, err := LoadKeyring(KeyConfig{Key: NewValueString(keyB64 + "\n")})
	if err != nil || k.PrimaryID() != ring(t, 7).PrimaryID() {
		t.Fatalf("env key: %v", err)
	}
	_, _, err = LoadKeyring(KeyConfig{Key: NewValueString(canary)})
	if err == nil || !strings.Contains(err.Error(), "ANGELIA_SECRETS_KEY") {
		t.Fatalf("bad env key: %v", err)
	}
	noLeak(t, "env key error", err.Error())

	prev := writeKey(t, base64.StdEncoding.EncodeToString(master(8))+"\n", 0o600)
	k, _, err = LoadKeyring(KeyConfig{KeyFile: file, PreviousKeyFiles: []string{prev}})
	if err != nil || len(k.IDs()) != 2 || k.IDs()[1] != ring(t, 8).PrimaryID() {
		t.Fatalf("previous: %v", err)
	}
	if _, _, err := LoadKeyring(KeyConfig{KeyFile: file, PreviousKeyFiles: []string{writeKey(t, "x", 0o600)}}); err == nil ||
		!strings.Contains(err.Error(), "previous_key_files[0]") {
		t.Fatalf("bad previous: %v", err)
	}
}

func TestWriteKeyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new.key")
	if err := WriteKeyFile(p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %04o", info.Mode().Perm())
	}
	if k, warns, err := LoadKeyring(KeyConfig{KeyFile: p}); err != nil || len(warns) != 0 || k == nil {
		t.Fatalf("generated key: %v %v", warns, err)
	}
	if err := WriteKeyFile(p); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite: %v", err)
	}
	a, _ := GenerateKey()
	b, _ := GenerateKey()
	if a.Len() != 45 || a.Equal(b.Reveal()) || !strings.HasSuffix(string(a.Reveal()), "\n") {
		t.Fatal("GenerateKey")
	}
}
