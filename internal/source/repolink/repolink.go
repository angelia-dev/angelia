// Package repolink finds the source repository a package declares in its own
// registry metadata (ADR 0045, decisions D80 to D84 of issue #84).
//
// Parse normalizes a repository link to https://host/owner/name for github.com
// and gitlab.com; PyPI and NPM pick the link and, for PyPI, a changelog page
// out of the metadata the pypi and npm sources already download. The functions
// are pure and fuzzed. The result is "as declared by the package, not
// verified": a publisher can name any repository.
package repolink

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/angelia-dev/angelia/internal/source/internal/ghapi"
)

// Forge hosts whose links are recognised.
const (
	GitHub = "github.com"
	GitLab = "gitlab.com"
)

// Repo is a repository on a recognised forge. For github.com Owner is the
// account; for gitlab.com it is the group path (one or more segments joined
// by "/") and Name the project.
type Repo struct {
	Host  string
	Owner string
	Name  string
}

// URL is the normalized page of the repository: https://host/owner/name.
func (r Repo) URL() string {
	return "https://" + r.Host + "/" + r.Owner + "/" + r.Name
}

var (
	scpRe     = regexp.MustCompile(`^git@([^:/\s]+):(.+)$`)
	gitlabSeg = regexp.MustCompile(`^[A-Za-z0-9._-]{1,255}$`)
)

// Parse normalizes raw, which may be an https, http, git, git+https,
// git+ssh or ssh URL, an scp-like "git@github.com:owner/repo.git", or the
// shorthands "github:owner/repo" and "gitlab:owner/repo". The host must be
// github.com or gitlab.com (a "www." prefix and upper case are accepted);
// anything else, userinfo other than the ssh user "git", a port, a path with
// fewer than two segments, or text that is not a link gives false. A trailing
// ".git", a trailing slash, any path below the repository, the query and the
// fragment are dropped.
func Parse(raw string) (Repo, bool) {
	return parse(raw, false)
}

// ParseNPM is Parse plus npm's bare "owner/repo" shorthand, which npm reads
// as a GitHub repository.
func ParseNPM(raw string) (Repo, bool) {
	return parse(raw, true)
}

func parse(raw string, bareGitHub bool) (Repo, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) {
		return Repo{}, false
	}
	if host, rest, ok := shorthand(raw); ok {
		return fromPath(host, rest)
	}
	if !strings.Contains(raw, "://") {
		if m := scpRe.FindStringSubmatch(raw); m != nil {
			return fromPath(m[1], m[2])
		}
		if bareGitHub {
			return fromPath(GitHub, raw)
		}
		return Repo{}, false
	}
	scheme, rest, _ := strings.Cut(raw, "://")
	scheme = strings.ToLower(scheme)
	ssh := false
	switch scheme {
	case "https", "http", "git", "git+https", "git+http":
	case "git+ssh", "ssh":
		ssh = true
		// npm writes "git+ssh://git@github.com:owner/repo.git"; that is
		// scp syntax behind a scheme, not a port.
		if m := scpRe.FindStringSubmatch(rest); m != nil && !digitsThenSlash(m[2]) {
			return fromPath(m[1], m[2])
		}
	default:
		return Repo{}, false
	}
	u, err := url.Parse("https://" + rest)
	if err != nil || u.Port() != "" || strings.Contains(u.Host, ":") {
		return Repo{}, false
	}
	if u.User != nil {
		if _, hasPw := u.User.Password(); !ssh || u.User.Username() != "git" || hasPw {
			return Repo{}, false
		}
	}
	return fromPath(u.Hostname(), u.Path)
}

// shorthand splits "github:owner/repo" and "gitlab:owner/repo".
func shorthand(raw string) (host, rest string, ok bool) {
	for prefix, h := range map[string]string{"github:": GitHub, "gitlab:": GitLab} {
		if len(raw) > len(prefix) && strings.EqualFold(raw[:len(prefix)], prefix) && !strings.HasPrefix(raw[len(prefix):], "//") {
			return h, raw[len(prefix):], true
		}
	}
	return "", "", false
}

func digitsThenSlash(s string) bool {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && i < len(s) && s[i] == '/'
}

// fromPath builds the Repo of host and a URL path or scp path.
func fromPath(host, path string) (Repo, bool) {
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if host != GitHub && host != GitLab {
		return Repo{}, false
	}
	// Query and fragment of a non-URL form.
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	segs := strings.Split(strings.Trim(path, "/"), "/")
	if host == GitHub {
		if len(segs) < 2 {
			return Repo{}, false
		}
		segs = segs[:2]
	} else {
		for i, s := range segs {
			if s == "-" {
				segs = segs[:i]
				break
			}
		}
		if len(segs) < 2 {
			return Repo{}, false
		}
	}
	last := len(segs) - 1
	segs[last] = strings.TrimSuffix(segs[last], ".git")
	if strings.HasSuffix(segs[last], ".git") {
		return Repo{}, false // "a.git.git": would not parse back to itself
	}
	for _, s := range segs {
		if s == "" || s == "." || s == ".." {
			return Repo{}, false
		}
	}
	r := Repo{Host: host, Owner: strings.Join(segs[:last], "/"), Name: segs[last]}
	if host == GitHub {
		if !ghapi.ValidRepo(r.Owner, r.Name) {
			return Repo{}, false
		}
		return r, true
	}
	for _, s := range segs {
		if !gitlabSeg.MatchString(s) {
			return Repo{}, false
		}
	}
	return r, true
}
