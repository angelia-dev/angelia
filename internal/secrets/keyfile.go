package secrets

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
)

// encodedKeyLen is the length of the standard base64 encoding of a key.
const encodedKeyLen = 44

// maxKeyFile bounds what is read from a key file.
const maxKeyFile = 1024

// KeyConfig says where the master keys come from (ADR 0024 §4).
type KeyConfig struct {
	// KeyFile is secrets.key_file: the primary key, base64 in a file.
	KeyFile string
	// Key is the value of ANGELIA_SECRETS_KEY: the primary key inline.
	// Setting it together with KeyFile is an error.
	Key Value
	// PreviousKeyFiles are keys that only decrypt (rotation, restored
	// backups).
	PreviousKeyFiles []string
}

// Configured reports whether any key is configured.
func (c KeyConfig) Configured() bool {
	return c.KeyFile != "" || !c.Key.IsZero() || len(c.PreviousKeyFiles) > 0
}

// LoadKeyring reads the configured keys. With no key configured it returns
// a nil Keyring and no error: an instance without stored secrets needs no
// key, and CheckStartup refuses to start one that has some. Warnings name a
// key file only group members can read. Errors never quote a file's content.
func LoadKeyring(c KeyConfig) (*Keyring, []string, error) {
	if !c.Configured() {
		return nil, nil, nil
	}
	switch {
	case c.KeyFile != "" && !c.Key.IsZero():
		return nil, nil, errors.New("secrets: both secrets.key_file and ANGELIA_SECRETS_KEY are set; set one")
	case c.KeyFile == "" && c.Key.IsZero():
		return nil, nil, errors.New("secrets: secrets.previous_key_files is set but no primary key: set secrets.key_file")
	}
	var warnings []string
	var primary []byte
	var err error
	if c.KeyFile != "" {
		var w string
		primary, w, err = readKeyFile(c.KeyFile, "secrets.key_file")
		if err != nil {
			return nil, nil, err
		}
		if w != "" {
			warnings = append(warnings, w)
		}
	} else if primary, err = parseKey(c.Key.b, "ANGELIA_SECRETS_KEY"); err != nil {
		return nil, nil, err
	}
	previous := make([][]byte, 0, len(c.PreviousKeyFiles))
	for i, p := range c.PreviousKeyFiles {
		k, w, err := readKeyFile(p, fmt.Sprintf("secrets.previous_key_files[%d]", i))
		if err != nil {
			return nil, nil, err
		}
		if w != "" {
			warnings = append(warnings, w)
		}
		previous = append(previous, k)
	}
	ring, err := NewKeyring(primary, previous...)
	if err != nil {
		return nil, nil, err
	}
	return ring, warnings, nil
}

// readKeyFile opens a key file, following symbolic links (Kubernetes secret
// volumes are links), and checks the mode of the opened file, so there is no
// gap between the check and the read.
func readKeyFile(path, what string) ([]byte, string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's key file.
	if err != nil {
		return nil, "", fmt.Errorf("secrets: %s: %w", what, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("secrets: %s: %w", what, err)
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("secrets: %s %s: not a regular file", what, path)
	}
	warning, err := checkKeyFileMode(path, what, info)
	if err != nil {
		return nil, "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil {
		return nil, "", fmt.Errorf("secrets: %s %s: %w", what, path, err)
	}
	k, err := parseKey(data, what+" "+path)
	return k, warning, err
}

// parseKey decodes one standard base64 encoding of 32 bytes, optionally
// followed by one newline. The error never quotes the input.
func parseKey(data []byte, where string) ([]byte, error) {
	s := string(data)
	switch {
	case len(s) == encodedKeyLen+2 && s[encodedKeyLen:] == "\r\n":
		s = s[:encodedKeyLen]
	case len(s) == encodedKeyLen+1 && s[encodedKeyLen] == '\n':
		s = s[:encodedKeyLen]
	}
	bad := fmt.Errorf("secrets: %s: not a secrets key: want the standard base64 encoding of %d bytes (%d characters) and at most one newline; create one with `angelia secrets keygen`",
		where, KeySize, encodedKeyLen)
	if len(s) != encodedKeyLen {
		return nil, bad
	}
	k, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(k) != KeySize {
		return nil, bad
	}
	return k, nil
}

// GenerateKey returns a new random master key in the key file format: the
// standard base64 encoding of 32 bytes and a newline.
func GenerateKey() (Value, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return Value{}, fmt.Errorf("secrets: generate key: %w", err)
	}
	return NewValueString(base64.StdEncoding.EncodeToString(k) + "\n"), nil
}

// WriteKeyFile creates path with mode 0600 and writes a new key to it. It
// refuses to overwrite an existing file.
func WriteKeyFile(path string) error {
	k, err := GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: the operator's path.
	if err != nil {
		return fmt.Errorf("secrets: create key file: %w", err)
	}
	if _, err := f.Write(k.b); err != nil {
		_ = f.Close()
		return fmt.Errorf("secrets: write key file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("secrets: write key file: %w", err)
	}
	return nil
}
