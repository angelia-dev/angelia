package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultServer is the address a CLI call uses when no flag, environment
// variable or client config file names a server: the default `listen` address
// of a local instance.
const DefaultServer = "http://localhost:8080"

// Environment variables of the CLI client.
const (
	envServer = "ANGELIA_SERVER"
	envToken  = "ANGELIA_TOKEN"
)

// Connection is where and as whom a CLI call talks to an instance. Its String
// method never shows the token.
type Connection struct {
	Server string
	Token  string
}

// String implements fmt.Stringer without the token.
func (c Connection) String() string {
	if c.Token == "" {
		return c.Server + " (no token)"
	}
	return c.Server + " (token set)"
}

// GoString keeps %#v from printing the token.
func (c Connection) GoString() string { return "Connection{" + c.String() + "}" }

// clientFile is the client config file (~/.config/angelia/config.yaml):
//
//	server: https://angelia.example.com
//	token: <api token>
//
// It is not the instance configuration of `angelia serve` (--config).
type clientFile struct {
	Server string `yaml:"server"`
	Token  string `yaml:"token"`
}

// clientConfigPath is the client config file: $XDG_CONFIG_HOME/angelia/config.yaml,
// or ~/.config/angelia/config.yaml when XDG_CONFIG_HOME is unset or relative
// (the XDG specification says to ignore a relative value).
func clientConfigPath(getenv func(string) string) (string, error) {
	if x := getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
		return filepath.Join(x, "angelia", "config.yaml"), nil
	}
	home := getenv("HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the client config file: %w", err)
		}
		home = h
	}
	return filepath.Join(home, ".config", "angelia", "config.yaml"), nil
}

// loadClientFile reads the client config file at path. A missing file is not
// an error. A file that holds a token and is readable by its group or by
// others is refused, as ssh refuses a loose private key: the token would be
// readable by other users of the machine.
func loadClientFile(path string) (clientFile, error) {
	var cf clientFile
	f, err := os.Open(path) //nolint:gosec // the user's own config path
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cf, nil
		}
		return cf, fmt.Errorf("read client config %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return cf, fmt.Errorf("read client config %s: %w", path, err)
	}
	data, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return cf, fmt.Errorf("read client config %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cf); err != nil && !errors.Is(err, io.EOF) {
		return clientFile{}, fmt.Errorf("client config %s: %w", path, err)
	}
	if cf.Token != "" && runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			return clientFile{}, fmt.Errorf("client config %s holds a token but is accessible by other users (mode %04o); run: chmod 600 %s", path, perm, path)
		}
	}
	return cf, nil
}

// resolveConnection picks the server URL and the token: the flags first, then
// ANGELIA_SERVER and ANGELIA_TOKEN, then the client config file, then
// DefaultServer. A value that is set but empty counts as unset.
func resolveConnection(flagServer, flagToken string, getenv func(string) string) (Connection, error) {
	c := Connection{Server: flagServer, Token: flagToken}
	if c.Server == "" {
		c.Server = getenv(envServer)
	}
	if c.Token == "" {
		c.Token = getenv(envToken)
	}
	// The file is read even when both are set elsewhere, so that a loose file
	// with a token is reported instead of silently ignored.
	path, err := clientConfigPath(getenv)
	if err != nil {
		return Connection{}, err
	}
	cf, err := loadClientFile(path)
	if err != nil {
		return Connection{}, err
	}
	if c.Server == "" {
		c.Server = cf.Server
	}
	if c.Token == "" {
		c.Token = cf.Token
	}
	if c.Server == "" {
		c.Server = DefaultServer
	}
	if err := checkServerURL(c.Server); err != nil {
		return Connection{}, err
	}
	return c, nil
}

func checkServerURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("invalid server URL %q: want http://host[:port] or https://host[:port]", redactURL(s))
	}
	if u.User != nil {
		return errors.New("invalid server URL: credentials in the URL are not supported; use --token or ANGELIA_TOKEN")
	}
	return nil
}

// redactURL drops anything after the host so that an error never echoes
// credentials a user pasted into the URL.
func redactURL(s string) string {
	if i := strings.Index(s, "@"); i >= 0 {
		return "..." + s[i+1:]
	}
	return s
}
