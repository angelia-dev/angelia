package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/netguard"
)

// The files of a plugin folder, <plugins_dir>/<kind>/ (ADR 0025).
const (
	// ManifestFile is the plugin author's manifest (ADR 0011).
	ManifestFile = "manifest.json"
	// WasmFile is the plugin's module. Loading it is angelia-dev/angelia#88.
	WasmFile = "plugin.wasm"
	// GrantsFile is the operator's grants: extra allowed hosts and
	// credentials. Optional.
	GrantsFile = "grants.yaml"
)

// maxPluginFile caps the manifest and grants files read.
const maxPluginFile = 1 << 20

// PluginGrants is the content of <plugins_dir>/<kind>/grants.yaml: what the
// operator grants a plugin beyond its manifest (ADR 0011 amendments, ADR
// 0025). It holds no token: credentials name an environment variable.
type PluginGrants struct {
	// AllowedHosts extends the manifest's allowed_hosts, for example with
	// a registry mirror. Hostnames (lower case, no trailing dot) or IP
	// literals; no scheme, port, path or wildcard.
	AllowedHosts []string `yaml:"allowed_hosts"`
	// Credentials, keyed by "host" (port 443) or "host:port", are injected
	// by the plugin host into the https requests to that host and port.
	Credentials map[string]PluginCredential `yaml:"credentials"`
}

// PluginCredential binds a token to one host of a plugin. The token is read
// from the environment variable named by TokenEnv. A secret-store
// reference is a later, additive key.
type PluginCredential struct {
	TokenEnv string `yaml:"token_env"`
	// Scheme, if set, is written before the token ("Bearer"). Only with
	// the Authorization header.
	Scheme string `yaml:"scheme"`
	// Header carries the token. Empty means Authorization; otherwise a
	// header netguard counts as a credential (netguard.IsCredentialHeader),
	// such as PRIVATE-TOKEN.
	Header string `yaml:"header"`
}

// InstalledPlugin is one plugin folder of plugins_dir, with its grants.
type InstalledPlugin struct {
	// Kind is the folder name, equal to the manifest's kind.
	Kind string
	// Dir is the plugin folder.
	Dir string
	// ManifestHosts is the manifest's allowed_hosts, as written.
	ManifestHosts []string
	// Grants is the content of grants.yaml; zero when there is none.
	Grants PluginGrants
	// Tokens holds the values of the credentials' token_env, keyed like
	// Grants.Credentials. They print as [REDACTED]; use Token. (Exported
	// so that fmt calls their Format method.)
	Tokens map[string]secret
}

// Token returns the token of the credential keyed by host ("host" or
// "host:port", as in grants.yaml), read from its token_env variable when
// the plugins were loaded. It is empty when none is configured or set.
// Hand it to the plugin host as a credential, never to the plugin.
func (p InstalledPlugin) Token(host string) string { return reveal(p.Tokens[host]) }

var (
	// pluginKindRE is the manifest's kind rule (ADR 0011).
	pluginKindRE      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	pluginHostLabelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	authSchemeRE      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,31}$`)
	headerNameRE      = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// ErrPluginsDirUnreadable is wrapped when plugins_dir exists but cannot be
// listed (not a directory, no permission).
var ErrPluginsDirUnreadable = errors.New("plugins_dir cannot be read")

// LoadPlugins reads every plugin folder of dir (the instance's plugins_dir):
// each subdirectory is a plugin whose name is its kind, with a
// manifest.json and an optional grants.yaml. It validates the grants and
// reads the credentials' variables from environ ("KEY=value" pairs). A
// missing dir means no plugins. Hidden entries are skipped; symbolic links
// and other files are ignored with a warning. Except on Windows, dir, each
// plugin folder and grants.yaml must be owned by this user or root and not
// writable by group or others (a sticky directory may be), and grants.yaml
// must not be a symbolic link (ADR 0025). Problems are all reported at
// once, and never quote a value that may be a token. Loading the plugins
// themselves is angelia-dev/angelia#88.
func LoadPlugins(ctx context.Context, dir string, environ []string) ([]InstalledPlugin, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, fmt.Errorf("load plugins: %w", err)
	}
	info, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err == nil && !info.IsDir() {
		err = errors.New("not a directory")
	}
	var entries []fs.DirEntry
	if err == nil {
		entries, err = os.ReadDir(dir)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %w", ErrPluginsDirUnreadable, dir, err)
	}
	if err := checkTrust(dir, info); err != nil {
		return nil, nil, fmt.Errorf("plugins_dir: %w", err)
	}
	env := environMap(environ)
	var (
		out   []InstalledPlugin
		warns []string
		errs  []error
	)
	for _, e := range entries { // sorted by name
		name := e.Name()
		path := filepath.Join(dir, name)
		switch {
		// The name comes from the file system: it is quoted until it
		// passed the kind rule, so it cannot write control characters.
		case strings.HasPrefix(name, "."):
			continue
		case e.Type()&fs.ModeSymlink != 0:
			warns = append(warns, fmt.Sprintf("%s: %q: symbolic link, ignored (a plugin folder must be a directory)", dir, name))
			continue
		case !e.IsDir():
			warns = append(warns, fmt.Sprintf("%s: %q: not a directory, ignored", dir, name))
			continue
		case !pluginKindRE.MatchString(name):
			errs = append(errs, fmt.Errorf("%s: %q is not a valid plugin kind (want ^[a-z][a-z0-9-]{0,62}$)", dir, name))
			continue
		}
		info, err := e.Info()
		if err == nil {
			err = checkTrust(path, info)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		p, w, err := loadPlugin(path, name, env)
		warns = append(warns, w...)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
	}
	if len(errs) > 0 {
		return nil, warns, errors.Join(errs...)
	}
	return out, warns, nil
}

func loadPlugin(dir, kind string, env map[string]string) (InstalledPlugin, []string, error) {
	p := InstalledPlugin{Kind: kind, Dir: dir}
	mpath := filepath.Join(dir, ManifestFile)
	data, err := readCapped(mpath)
	if err != nil {
		return p, nil, fmt.Errorf("%s: %w", mpath, err)
	}
	// Only the fields this check needs; the plugin host validates the
	// whole manifest when it loads the plugin.
	var m struct {
		Kind         string   `json:"kind"`
		AllowedHosts []string `json:"allowed_hosts"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return p, nil, fmt.Errorf("%s: %w", mpath, err)
	}
	if m.Kind != kind {
		return p, nil, fmt.Errorf("%s: declares kind %q, but the folder is %q", mpath, m.Kind, kind)
	}
	p.ManifestHosts = m.AllowedHosts

	gpath := filepath.Join(dir, GrantsFile)
	data, err = readGrantsFile(gpath)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil, nil
	}
	if err != nil {
		return p, nil, err
	}
	if err := decodeStrict(data, gpath, &p.Grants); err != nil {
		return p, nil, err
	}
	allowed := map[string]bool{}
	for _, h := range m.AllowedHosts {
		allowed[strings.TrimSuffix(strings.ToLower(h), ".")] = true
	}
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(gpath+": "+format, a...)) }
	for i, h := range p.Grants.AllowedHosts {
		if !validPluginHost(h) {
			bad("allowed_hosts[%d]: must be a lower-case hostname or an IP literal (no scheme, port, path or wildcard)", i)
			continue
		}
		allowed[h] = true
	}
	for _, k := range sortedKeys(p.Grants.Credentials) {
		validateCredential(k, p.Grants.Credentials[k], allowed, bad)
	}
	if len(errs) > 0 {
		return p, nil, errors.Join(errs...)
	}
	var warns []string
	p.Tokens = map[string]secret{}
	for _, k := range sortedKeys(p.Grants.Credentials) {
		name := p.Grants.Credentials[k].TokenEnv
		if env[name] == "" {
			warns = append(warns, fmt.Sprintf("%s: credentials.%s: token_env: environment variable %s is not set or empty", gpath, k, name))
			continue
		}
		p.Tokens[k] = newSecret(env[name])
	}
	return p, warns, nil
}

// validateCredential checks one credentials entry; values that may be
// tokens are never echoed.
func validateCredential(k string, cr PluginCredential, allowed map[string]bool, bad func(string, ...any)) {
	host, ok := credentialHost(k)
	if !ok {
		bad("credentials: key must be host or host:port, the host a lower-case hostname or an IP literal ([addr]:port for IPv6 with a port)")
		return
	}
	if !allowed[host] {
		bad("credentials.%s: host %s is not in the manifest's or this file's allowed_hosts", k, host)
	}
	switch {
	case cr.TokenEnv == "":
		bad("credentials.%s.token_env: required", k)
	case hasTokenPrefix(cr.TokenEnv):
		bad("credentials.%s.token_env: looks like a token, not an environment variable name", k)
	case !envNameRE.MatchString(cr.TokenEnv):
		bad("credentials.%s.token_env: must be an environment variable name, not a token", k)
	case ReservedEnvName(cr.TokenEnv):
		bad("credentials.%s.token_env: names starting with %s are always refused: they hold the instance's own settings", k, envPrefix)
	}
	if cr.Scheme != "" && !authSchemeRE.MatchString(cr.Scheme) {
		bad("credentials.%s.scheme: must be one word such as Bearer", k)
	}
	if cr.Header != "" {
		switch {
		case !headerNameRE.MatchString(cr.Header) || !netguard.IsCredentialHeader(cr.Header):
			bad("credentials.%s.header: must be a credential header: Authorization or PRIVATE-TOKEN", k)
		case cr.Scheme != "" && !strings.EqualFold(cr.Header, "Authorization"):
			bad("credentials.%s.scheme: scheme applies to the Authorization header only", k)
		}
	}
}

// readGrantsFile reads grants.yaml. The file grants network access, so
// one that someone else could change is refused: tampering, not reading,
// is the risk (it holds variable names, no secret). A symbolic link is
// refused, and the owner and mode are checked on the opened file.
func readGrantsFile(path string) ([]byte, error) {
	f, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if err := checkTrust(path, info); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPluginFile+1))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(data) > maxPluginFile {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, maxPluginFile)
	}
	return data, nil
}

func readCapped(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the operator's plugins_dir.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxPluginFile+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPluginFile {
		return nil, fmt.Errorf("larger than %d bytes", maxPluginFile)
	}
	return data, nil
}

// decodeStrict decodes one YAML document, rejecting unknown keys.
func decodeStrict(data []byte, path string, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return yamlError(path, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s: multiple YAML documents are not supported", path)
	}
	return nil
}

// validPluginHost reports whether h is in the canonical form of an
// allowed_hosts entry: a lower-case hostname without a trailing dot, or a
// canonical IP literal.
func validPluginHost(h string) bool {
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Zone() == "" && a.String() == h
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for l := range strings.SplitSeq(h, ".") {
		if !pluginHostLabelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// credentialHost returns the host of a credential key, host or host:port,
// with host in canonical form and port 1-65535 without leading zeros. An
// IPv6 literal with a port is written [addr]:port.
func credentialHost(k string) (string, bool) {
	if validPluginHost(k) {
		return k, true
	}
	h, port, err := net.SplitHostPort(k)
	if err != nil {
		return "", false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port || !validPluginHost(h) {
		return "", false
	}
	return h, true
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
