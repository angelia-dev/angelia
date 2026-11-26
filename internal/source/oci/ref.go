package oci

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/angelia-dev/angelia/internal/source"
)

const (
	// DockerHub is the canonical registry host of Docker Hub images.
	DockerHub = "docker.io"
	// dockerHubEndpoint is where the registry API of Docker Hub lives.
	dockerHubEndpoint = "https://registry-1.docker.io"
	// maxNameLen is the longest repository path the distribution spec allows.
	maxNameLen = 255
)

var (
	// componentRE is one path component of a repository name (distribution
	// reference grammar).
	componentRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)
	// hostRE is a registry host with an optional port (no IPv6 literal).
	hostRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?::[0-9]{1,5})?$`)
)

// ref is a parsed image reference: a lower-case registry host and a
// repository path, in canonical form.
type ref struct {
	host string // "docker.io" or the registry host[:port]
	path string // "library/node", "org/app"
}

// String is the canonical image coordinate.
func (r ref) String() string { return r.host + "/" + r.path }

// dockerHubAliases are the other names of Docker Hub's registry.
var dockerHubAliases = map[string]bool{
	"docker.io":            true,
	"index.docker.io":      true,
	"registry-1.docker.io": true,
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("oci: %w: %s", source.ErrInvalidTarget, fmt.Sprintf(format, args...))
}

// parseRef checks and canonicalises the "image" coordinate (D42): the host is
// lower-cased, a missing host is Docker Hub, the Docker Hub aliases become
// docker.io, and a one-segment Docker Hub path gets "library/". A tag, a
// digest, a scheme, upper case in the path or anything outside the
// distribution reference grammar is refused.
func parseRef(t source.Target) (ref, error) {
	if t.Kind != Kind {
		return ref{}, invalid("kind %q, want %q", t.Kind, Kind)
	}
	for k := range t.Coordinates {
		if k != "image" {
			return ref{}, invalid("unknown coordinate %q (only image)", k)
		}
	}
	s := t.Coordinates["image"]
	switch {
	case s == "":
		return ref{}, invalid(`coordinate "image" is required, for example "ghcr.io/org/app" or "node"`)
	case strings.TrimSpace(s) != s || strings.ContainsAny(s, " \t\r\n\\?#"):
		return ref{}, invalid("image %q contains spaces or URL punctuation", s)
	case strings.Contains(s, "://"):
		return ref{}, invalid("image %q must not have a scheme; write the registry host and the path only", s)
	case strings.Contains(s, "@"):
		return ref{}, invalid("image %q must not carry a digest; the source lists the tags", s)
	}
	host, path := DockerHub, s
	if first, rest, ok := strings.Cut(s, "/"); ok && (strings.ContainsAny(first, ".:") || strings.EqualFold(first, "localhost")) {
		host, path = strings.ToLower(first), rest
	}
	if path == "" {
		return ref{}, invalid("image %q has no repository path", s)
	}
	if host != DockerHub && !hostRE.MatchString(host) {
		return ref{}, invalid("image %q: %q is not a registry host", s, host)
	}
	if dockerHubAliases[host] {
		host = DockerHub
	}
	if strings.Contains(path, ":") {
		return ref{}, invalid("image %q must not carry a tag; the source lists the tags", s)
	}
	if len(path) > maxNameLen {
		return ref{}, invalid("image path is %d characters, the limit is %d", len(path), maxNameLen)
	}
	comps := strings.Split(path, "/")
	for _, c := range comps {
		if !componentRE.MatchString(c) {
			return ref{}, invalid("image %q: %q is not a valid repository path component (lower-case letters, digits, '.', '_' and '-')", s, c)
		}
	}
	if host == DockerHub && len(comps) == 1 {
		path = "library/" + path
	}
	return ref{host: host, path: path}, nil
}

// endpoint returns the registry API origin of the reference's host.
func (s *Source) endpoint(r ref) string {
	if s.pinned != "" {
		return s.pinned
	}
	if e, ok := s.endpoints[r.host]; ok {
		return e
	}
	if r.host == DockerHub {
		return dockerHubEndpoint
	}
	return "https://" + r.host
}

// origin returns "scheme://host" of a URL in lower case, or "" if u is not
// an absolute http(s) URL.
func origin(u *url.URL) string {
	if u == nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return origin(u)
}
