// Package npm is the Source for the npm registry. It lists the versions of a
// package from its packument, GET {base}/{name}.
//
// # Abbreviated or full metadata
//
// The registry serves two shapes of the packument. The abbreviated one
// (Accept: application/vnd.npm.install-v1+json) is small but has no "time"
// map. The full one has it. A Version needs PublishedAt, and an unpublished
// version is only visible as a key of "time" with no entry in "versions", so
// the default is the full document. WithAbbreviated opts in to the smaller
// shape for callers that can live without both: it leaves PublishedAt zero and
// cannot report unpublished versions. Full packuments of busy packages are
// large (several MiB for react), so the HTTP client's body cap must allow it.
//
// # What a Version carries
//
// Raw is the version key. PublishedAt comes from "time". A version that is in
// "time" but not in "versions" was unpublished and is Yanked. A deprecated
// version is not Yanked; its message is Metadata["deprecated"]. Metadata also
// has "dist-tags" (the tags pointing at the version, sorted), "tarball",
// "integrity" and "shasum" when the registry gives them, and
// "unpublished": true for unpublished versions. Prerelease is left nil: npm
// has no such flag and the core derives it from the version.
package npm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

const (
	// Kind is the registry key of this source.
	Kind = "npm"
	// DefaultBaseURL is the public npm registry.
	DefaultBaseURL = "https://registry.npmjs.org"
	// DefaultPackageURL is the prefix of package pages on the public registry.
	DefaultPackageURL = "https://www.npmjs.com/package"

	acceptFull        = "application/json"
	acceptAbbreviated = "application/vnd.npm.install-v1+json"

	maxNameLen = 214
)

// nameRE accepts "name" and "@scope/name". Upper case is allowed because old
// packages have it.
var nameRE = regexp.MustCompile(`^(?:@[A-Za-z0-9~-][A-Za-z0-9._~-]*/)?[A-Za-z0-9~-][A-Za-z0-9._~-]*$`)

// Option configures a Source.
type Option func(*Source)

// WithAbbreviated requests the abbreviated packument. See the package
// documentation for what is lost.
func WithAbbreviated() Option { return func(s *Source) { s.abbreviated = true } }

// WithPackageURL sets the prefix of the release page URL, to which
// "/{name}/v/{version}" is appended. Without it, URL is set only for the
// public registry.
func WithPackageURL(prefix string) Option {
	return func(s *Source) { s.pageBase = strings.TrimRight(prefix, "/") }
}

// Source implements source.Source for npm.
type Source struct {
	base        string
	hc          *http.Client
	abbreviated bool
	pageBase    string
}

var _ source.Source = (*Source)(nil)

// New returns an npm Source reading from baseURL (DefaultBaseURL if empty)
// with hc, normally the HTTPClient of a netguard client.
func New(baseURL string, hc *http.Client, opts ...Option) *Source {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	s := &Source{base: strings.TrimRight(baseURL, "/"), hc: hc}
	if s.base == DefaultBaseURL {
		s.pageBase = DefaultPackageURL
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// Validate implements source.Source. The only coordinate is "name", the
// package name, scoped as "@scope/name". It does no I/O.
func (*Source) Validate(t source.Target) error {
	if t.Kind != Kind {
		return fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	_, err := packageName(t)
	return err
}

func packageName(t source.Target) (string, error) {
	name := strings.TrimSpace(t.Coordinates["name"])
	switch {
	case name == "":
		return "", fmt.Errorf("%w: npm target needs a name", source.ErrInvalidTarget)
	case len(name) > maxNameLen:
		return "", fmt.Errorf("%w: npm name longer than %d characters", source.ErrInvalidTarget, maxNameLen)
	case !nameRE.MatchString(name):
		return "", fmt.Errorf("%w: invalid npm package name %q", source.ErrInvalidTarget, name)
	}
	if bare := name[strings.IndexByte(name, '/')+1:]; strings.HasPrefix(bare, ".") || strings.HasPrefix(bare, "_") {
		return "", fmt.Errorf("%w: npm name %q starts with . or _", source.ErrInvalidTarget, name)
	}
	return name, nil
}

type packument struct {
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]pkgInfo         `json:"versions"`
	Time     map[string]json.RawMessage `json:"time"`
}

type pkgInfo struct {
	// Deprecated is a string, but old packages have booleans; keep it raw.
	Deprecated json.RawMessage `json:"deprecated"`
	Dist       struct {
		Tarball   string `json:"tarball"`
		Integrity string `json:"integrity"`
		Shasum    string `json:"shasum"`
	} `json:"dist"`
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	name, err := packageName(t)
	if err != nil {
		return source.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.packageURL(name), nil)
	if err != nil {
		return source.Result{}, fmt.Errorf("npm: build request for %s: %w", name, err)
	}
	if s.abbreviated {
		req.Header.Set("Accept", acceptAbbreviated)
	} else {
		req.Header.Set("Accept", acceptFull)
	}
	res, err := netguard.Fetch(ctx, s.hc, req, netguard.Validators{ETag: st.ETag, LastModified: st.LastModified})
	if err != nil {
		return source.Result{}, fmt.Errorf("npm: fetch %s: %w", name, err)
	}
	state := source.FetchState{ETag: res.Validators.ETag, LastModified: res.Validators.LastModified, Cursor: st.Cursor}
	if res.NotModified {
		return source.Result{State: state, NotModified: true}, nil
	}
	var p packument
	if err := json.Unmarshal(res.Body, &p); err != nil {
		return source.Result{}, fmt.Errorf("npm: decode packument of %s: %w", name, err)
	}
	return source.Result{Versions: s.versions(name, &p), State: state}, nil
}

// packageURL builds {base}/{name}; the slash of a scoped name is sent as %2F,
// as npm clients do.
func (s *Source) packageURL(name string) string {
	return s.base + "/" + strings.Replace(url.PathEscape(name), "/", "%2F", 1)
}

func (s *Source) versions(name string, p *packument) []source.Version {
	tagsOf := map[string][]string{}
	for tag, v := range p.DistTags {
		tagsOf[v] = append(tagsOf[v], tag)
	}
	out := make([]source.Version, 0, len(p.Versions))
	for raw, info := range p.Versions {
		out = append(out, s.version(name, raw, timeOf(p.Time, raw), &info, tagsOf[raw]))
	}
	// Keys of "time" that are not versions: "created", "modified" and, for a
	// fully unpublished package, "unpublished". Any other key without an
	// entry in "versions" is an unpublished version.
	for raw := range p.Time {
		if _, ok := p.Versions[raw]; ok || raw == "created" || raw == "modified" || raw == "unpublished" {
			continue
		}
		v := s.version(name, raw, timeOf(p.Time, raw), &pkgInfo{}, nil)
		v.Yanked = true
		v.Metadata["unpublished"] = true
		out = append(out, v)
	}
	slices.SortFunc(out, func(a, b source.Version) int {
		if c := b.PublishedAt.Compare(a.PublishedAt); c != 0 {
			return c // newest first
		}
		return strings.Compare(a.Raw, b.Raw)
	})
	return out
}

func (s *Source) version(name, raw, ts string, info *pkgInfo, tags []string) source.Version {
	v := source.Version{Raw: raw, Metadata: map[string]any{}}
	if t, err := time.Parse(time.RFC3339, ts); err == nil {
		v.PublishedAt = t.UTC()
	}
	if s.pageBase != "" {
		v.URL = s.pageBase + "/" + name + "/v/" + raw
	}
	if msg := deprecation(info.Deprecated); msg != "" {
		v.Metadata["deprecated"] = msg
	}
	if len(tags) > 0 {
		slices.Sort(tags)
		v.Metadata["dist-tags"] = tags
	}
	for k, val := range map[string]string{"tarball": info.Dist.Tarball, "integrity": info.Dist.Integrity, "shasum": info.Dist.Shasum} {
		if val != "" {
			v.Metadata[k] = val
		}
	}
	return v
}

// timeOf returns the "time" entry of raw as a string, "" if it is absent or
// not a string.
func timeOf(m map[string]json.RawMessage, raw string) string {
	var s string
	_ = json.Unmarshal(m[raw], &s)
	return s
}

// deprecation returns the deprecation message, "" if the version is not
// deprecated. An empty string or false means "not deprecated" on npm; true
// (seen in old documents) has no message and yields "deprecated".
func deprecation(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var msg string
	if json.Unmarshal(raw, &msg) == nil {
		return msg
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil && b {
		return "deprecated"
	}
	return ""
}
