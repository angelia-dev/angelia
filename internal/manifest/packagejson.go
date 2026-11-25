package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
)

// The package.json keys that hold dependencies (docs/cli.md, "Import").
var packageSections = map[string]string{
	"dependencies":         TypeRuntime,
	"optionalDependencies": TypeOptional,
	"devDependencies":      TypeDev,
	"peerDependencies":     TypePeer,
}

// Specifiers that do not name a registry package.
var (
	nonRegistryPrefixes = []string{
		"file:", "link:", "portal:", "workspace:", "git:", "git+", "github:", "gitlab:", "bitbucket:", "gist:", "http:", "https:",
	}
	exactVersionRE = regexp.MustCompile(`^[=v]*(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.+-]*)?)$`)
)

// ParsePackageJSON reads the dependency sections of the package.json name:
// dependencies (runtime), optionalDependencies (optional), devDependencies
// (dev) and peerDependencies (peer). bundledDependencies, overrides and
// resolutions are ignored.
func ParsePackageJSON(fsys fs.FS, name string) (*Result, error) {
	raw, err := readFile(fsys, name)
	if err != nil {
		return nil, err
	}
	res := &Result{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil {
		return nil, jsonError(name, raw, err)
	} else if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("%s:%d: the top level of package.json must be an object", name, lineAt(raw, 0))
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, jsonError(name, raw, err)
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, jsonError(name, raw, err)
		}
		typ, ok := packageSections[key]
		if !ok {
			continue
		}
		base := int(dec.InputOffset()) - len(val)
		packageSection(res, name, raw, key, typ, val, base)
	}
	if _, err := dec.Token(); err != nil { // the closing brace
		return nil, jsonError(name, raw, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s:%d: unexpected data after the top-level object", name, lineAt(raw, int(dec.InputOffset())))
	}
	return res, nil
}

// packageSection reads one dependency object. base is the offset of val in raw.
func packageSection(res *Result, file string, raw []byte, key, typ string, val json.RawMessage, base int) {
	warn := func(off int, format string, a ...any) {
		res.Warnings = append(res.Warnings, Warning{Origin: Origin{File: file, Line: lineAt(raw, off)}, Skip: true, Msg: fmt.Sprintf(format, a...)})
	}
	dec := json.NewDecoder(bytes.NewReader(val))
	if tok, _ := dec.Token(); tok != json.Delim('{') {
		if string(val) != "null" {
			warn(base, "skipped %s: must be an object of name: version, the dependencies in it are not imported", key)
		}
		return
	}
	for dec.More() {
		tok, _ := dec.Token()
		dep, _ := tok.(string)
		at := base + int(dec.InputOffset())
		var spec json.RawMessage
		_ = dec.Decode(&spec) // val is valid JSON: Decode cannot fail here
		var s string
		if err := json.Unmarshal(spec, &s); err != nil {
			warn(at, "skipped %s in %s: the version must be a string", dep, key)
			continue
		}
		name, pin, skip := npmSpecifier(dep, s)
		if skip != "" {
			warn(at, "skipped %s in %s: %s", dep, key, skip)
			continue
		}
		res.Deps = append(res.Deps, Dependency{
			Name: name, Kind: KindNPM, Type: typ, Pin: pin, Origin: Origin{File: file, Line: lineAt(raw, at)},
		})
	}
}

// npmSpecifier returns the registry package that dep: spec names, the exact
// version when spec is one, or why it is not a registry dependency.
func npmSpecifier(dep, spec string) (name, pin, skip string) {
	spec = strings.TrimSpace(spec)
	name = dep
	if rest, ok := strings.CutPrefix(spec, "npm:"); ok { // an alias: the real package is imported
		name, spec = rest, ""
		if at := strings.LastIndex(rest, "@"); at > 0 {
			name, spec = rest[:at], rest[at+1:]
		}
		if name == "" {
			return "", "", "an npm: alias without a package name"
		}
	}
	for _, p := range nonRegistryPrefixes {
		if strings.HasPrefix(spec, p) {
			return "", "", fmt.Sprintf("%q is not a registry dependency (%s)", spec, strings.TrimSuffix(strings.TrimSuffix(p, ":"), "+"))
		}
	}
	if strings.Contains(spec, "/") { // user/repo shorthand, or a path
		return "", "", fmt.Sprintf("%q is a repository or path, not a registry dependency", spec)
	}
	if m := exactVersionRE.FindStringSubmatch(spec); m != nil {
		pin = m[1]
	}
	return name, pin, ""
}

// lineAt returns the 1-based line of offset off in raw.
func lineAt(raw []byte, off int) int {
	off = max(0, min(off, len(raw)))
	return 1 + bytes.Count(raw[:off], []byte("\n"))
}

// jsonError turns the offset of a JSON error into a line.
func jsonError(file string, raw []byte, err error) error {
	var se *json.SyntaxError
	switch {
	case errors.As(err, &se):
		return fmt.Errorf("%s:%d: invalid JSON: %s", file, lineAt(raw, int(se.Offset)-1), se)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%s:%d: invalid JSON: unexpected end of file", file, lineAt(raw, len(raw)))
	}
	return fmt.Errorf("%s: invalid JSON: %w", file, err)
}
