package event

import (
	"strings"
)

// PURL returns data.purl, the Package URL of version raw of package pkg (as
// Package returns it), built per source kind as the spec's purl table says,
// or nil for kinds without a purl type (http-json, http-regex, plugins) and
// when the identity cannot be built. md is the release's source metadata;
// registryURL, when not empty, is the repository_url qualifier of a
// non-default registry. It is written by hand: package-url/packageurl-go is
// not in architecture.md §6, and the rules here deviate from it (go-proxy
// case, pypi name from the registry).
func PURL(kind, pkg, raw string, md map[string]any, registryURL string) *string {
	var typ string
	var path []string // namespace segments then the name
	quals := map[string]string{}
	version := raw
	switch kind {
	case "pypi":
		// The name as PyPI reports it, lower case, "_" → "-"; "." is kept
		// (purl-spec pypi type, not PEP 503). Without it no purl is built:
		// guessing from the PEP 503 form would name zope-interface for
		// zope.interface.
		name, _ := md["project_name"].(string)
		if name == "" {
			return nil
		}
		typ, path = "pypi", []string{strings.ReplaceAll(strings.ToLower(name), "_", "-")}
	case "npm":
		typ, path = "npm", strings.SplitN(pkg, "/", 2) // "@scope/name": the scope is the namespace
		if len(path) == 2 && !strings.HasPrefix(path[0], "@") {
			return nil
		}
	case "crates":
		typ, path = "cargo", []string{pkg}
	case "github-release", "github-tag":
		typ, path = "github", strings.Split(strings.ToLower(pkg), "/")
		if len(path) != 2 {
			return nil
		}
	case "gitlab-release":
		// A numeric project id has no path to name.
		typ, path = "gitlab", strings.Split(strings.ToLower(pkg), "/")
		if len(path) < 2 {
			return nil
		}
	case "go-proxy":
		typ, path = "golang", strings.Split(pkg, "/") // case kept: module paths are case-sensitive
	case "maven":
		g, a, ok := strings.Cut(pkg, ":")
		if !ok || g == "" || a == "" {
			return nil
		}
		typ, path = "maven", []string{g, a}
	case "oci":
		// The version is the manifest digest; until a source reports one
		// the purl is versionless, with the tag as a qualifier.
		segs := strings.Split(pkg, "/")
		typ, path, version = "oci", []string{strings.ToLower(segs[len(segs)-1])}, ""
		quals["repository_url"], quals["tag"] = pkg, raw
		registryURL = ""
	default:
		return nil
	}
	for _, seg := range path {
		if seg == "" {
			return nil
		}
	}
	if registryURL != "" {
		quals["repository_url"] = registryURL
	}
	var b strings.Builder
	b.WriteString("pkg:" + typ)
	for _, seg := range path {
		b.WriteString("/" + escape(seg, false))
	}
	if version != "" {
		b.WriteString("@" + escape(version, false))
	}
	sep := "?"
	for _, k := range []string{"repository_url", "tag"} { // keys in lexical order
		if v, ok := quals[k]; ok {
			b.WriteString(sep + k + "=" + escape(v, true))
			sep = "&"
		}
	}
	s := b.String()
	return &s
}

// escape percent-encodes every byte but the unreserved characters
// A-Z a-z 0-9 . - _ ~, so "@" becomes %40, "+" %2B and ":" %3A. A qualifier
// value also keeps "/" and ":" as the purl examples write them
// (repository_url=docker.io/library/node).
func escape(s string, qualifier bool) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '.', c == '-', c == '_', c == '~',
			qualifier && (c == '/' || c == ':'):
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&15])
		}
	}
	return b.String()
}
