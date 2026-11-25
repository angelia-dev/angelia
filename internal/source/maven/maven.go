// Package maven is the Source for Maven repositories (Maven Central, Nexus,
// Artifactory, GitLab and GitHub Packages Maven endpoints). It lists the
// versions of an artifact from its metadata file,
// GET {base}/{groupId with "/" for "."}/{artifactId}/maven-metadata.xml.
//
// # Coordinates
//
// One key, "package", in the groupId:artifactId form ("org.slf4j:slf4j-api"),
// which is also data.package. Repository paths are case-sensitive, so there
// is no canonical form. The validation rules keep the request path inside
// the artifact's directory.
//
// # Conditional requests and dates
//
// The metadata request sends the stored ETag and Last-Modified (Maven Central
// answers If-None-Match with 304). The metadata has no per-version date: for
// each version new since the previous call (none on the first scan, which is
// a baseline, ADR 0010 section 5), at most maxLookups per call, List sends
// HEAD {base}/{g}/{a}/{v}/{a}-{v}.pom and uses its Last-Modified as
// PublishedAt. A 404 or another failed lookup leaves the version undated
// (one WARN); a 429 or a cancelled context fails the call, so the cursor does
// not advance and the next call retries. The cursor is JSON with the version
// names already reported.
//
// # What a Version carries
//
// Every listed version is returned, -SNAPSHOT included (the maven scheme
// classifies it as a pre-release). <latest>, <release> and <lastUpdated> are
// ignored: <release> is the last deployed version, which can be a
// pre-release. Yanked is always false (Maven repositories have no yank) and
// Prerelease is nil. URL is the central.sonatype.com page for Maven Central
// and empty for any other repository. Versions are sorted newest first by
// the maven version scheme, ties by Raw.
//
// At most maxVersions (10,000) distinct versions are kept per call, the
// last ones in metadata (deploy) order, with a WARN.
//
// # XML
//
// The document is parsed with encoding/xml in strict mode. It resolves no
// external entity and expands no DTD entity (an undeclared entity reference
// is an error), so XXE and entity expansion do not apply; the HTTP client's
// body cap bounds the document. Declared encodings other than UTF-8,
// US-ASCII and ISO-8859-1 are refused.
package maven

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/version"
	mversion "github.com/angelia-dev/angelia/internal/version/maven"
)

const (
	// Kind is the registry key of this source.
	Kind = "maven"
	// DefaultBaseURL is Maven Central, as Maven's super-POM names it.
	DefaultBaseURL = "https://repo.maven.apache.org/maven2"
	// CentralAltBaseURL is the other URL of Maven Central; it counts as
	// Central for URL.
	CentralAltBaseURL = "https://repo1.maven.org/maven2"
	// CentralArtifactURL is the prefix of artifact pages on Maven Central.
	CentralArtifactURL = "https://central.sonatype.com/artifact"

	maxPartLen    = 255 // groupId and artifactId
	maxVersionLen = mversion.MaxLen
	maxLookups    = 50
	maxVersions   = 10000 // versions kept per call; the newest by metadata (deploy) order
	maxLogged     = 3     // skipped versions named in the DEBUG record
)

var (
	groupRE    = regexp.MustCompile(`^[A-Za-z0-9_-]+(?:\.[A-Za-z0-9_-]+)*$`)
	artifactRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
)

// Option configures a Source.
type Option func(*Source)

// WithLogger sets the logger for skipped versions (DEBUG) and failed date
// lookups (WARN). The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Source) {
		if l != nil {
			s.log = l
		}
	}
}

// Source implements source.Source for Maven repositories. It is safe for
// concurrent use.
type Source struct {
	base     string
	hc       *http.Client
	log      *slog.Logger
	pageBase string
}

var _ source.Source = (*Source)(nil)

// New returns a Source reading from baseURL (DefaultBaseURL if empty; a path
// such as a Nexus ".../repository/maven-releases" is kept) with hc, normally
// the HTTP client of a netguard client.
func New(baseURL string, hc *http.Client, opts ...Option) *Source {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	s := &Source{base: strings.TrimRight(baseURL, "/"), hc: hc, log: slog.Default()}
	if IsCentral(s.base) {
		s.pageBase = CentralArtifactURL
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// IsCentral reports whether base is one of Maven Central's two base URLs.
func IsCentral(base string) bool {
	b := strings.TrimRight(base, "/")
	return strings.EqualFold(b, DefaultBaseURL) || strings.EqualFold(b, CentralAltBaseURL)
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// Validate implements source.Source. The only coordinate is "package",
// groupId:artifactId. It does no I/O.
func (*Source) Validate(t source.Target) error {
	if t.Kind != Kind {
		return fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	_, _, err := coordinates(t)
	return err
}

func coordinates(t source.Target) (group, artifact string, err error) {
	for k := range t.Coordinates {
		if k != "package" {
			return "", "", fmt.Errorf("%w: maven coordinate %q is not supported (want package: groupId:artifactId)", source.ErrInvalidTarget, version.Truncate(k, 32))
		}
	}
	pkg := t.Coordinates["package"]
	if pkg == "" {
		return "", "", fmt.Errorf("%w: maven target needs a package (groupId:artifactId)", source.ErrInvalidTarget)
	}
	group, artifact, ok := strings.Cut(pkg, ":")
	switch {
	case !ok || strings.Contains(artifact, ":"):
		return "", "", fmt.Errorf("%w: maven package must be groupId:artifactId with one colon", source.ErrInvalidTarget)
	case len(group) > maxPartLen || len(artifact) > maxPartLen:
		return "", "", fmt.Errorf("%w: maven groupId and artifactId are at most %d characters", source.ErrInvalidTarget, maxPartLen)
	case !groupRE.MatchString(group):
		return "", "", fmt.Errorf("%w: invalid maven groupId %q", source.ErrInvalidTarget, version.Truncate(group, 64))
	case !artifactRE.MatchString(artifact):
		return "", "", fmt.Errorf("%w: invalid maven artifactId %q", source.ErrInvalidTarget, version.Truncate(artifact, 64))
	}
	return group, artifact, nil
}

// metadata is the part of maven-metadata.xml the source reads.
type metadata struct {
	XMLName    xml.Name `xml:"metadata"`
	GroupID    *string  `xml:"groupId"`
	ArtifactID *string  `xml:"artifactId"`
	Versions   []string `xml:"versioning>versions>version"`
}

// cursor is the JSON kept in FetchState.Cursor: the versions already
// reported, so only new ones cost a date lookup.
type cursor struct {
	Versions []string `json:"versions"`
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	group, artifact, err := coordinates(t)
	if err != nil {
		return source.Result{}, err
	}
	if s.hc == nil {
		return source.Result{}, errors.New("maven: source has no HTTP client")
	}
	pkg := group + ":" + artifact
	dir := s.base + "/" + strings.ReplaceAll(group, ".", "/") + "/" + artifact
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, dir+"/maven-metadata.xml", nil)
	if err != nil {
		return source.Result{}, fmt.Errorf("maven: build request for %s: %w", pkg, err)
	}
	req.Header.Set("Accept", "application/xml, text/xml;q=0.9, */*;q=0.1")
	res, err := netguard.Fetch(ctx, s.hc, req, netguard.Validators{ETag: st.ETag, LastModified: st.LastModified})
	if err != nil {
		return source.Result{}, fmt.Errorf("maven: fetch metadata of %s: %w", pkg, err)
	}
	state := source.FetchState{ETag: res.Validators.ETag, LastModified: res.Validators.LastModified, Cursor: st.Cursor}
	if res.NotModified {
		return source.Result{State: state, NotModified: true}, nil
	}
	raws, err := s.parse(ctx, pkg, group, artifact, res.Body)
	if err != nil {
		return source.Result{}, err
	}

	var cur cursor
	haveCursor := st.Cursor != "" && json.Unmarshal([]byte(st.Cursor), &cur) == nil
	known := make(map[string]bool, len(cur.Versions))
	for _, v := range cur.Versions {
		known[v] = true
	}
	out := make([]source.Version, 0, len(raws))
	lookups := 0
	for _, raw := range raws {
		v := source.Version{Raw: raw}
		if s.pageBase != "" {
			v.URL = s.pageBase + "/" + group + "/" + artifact + "/" + url.PathEscape(raw)
		}
		// A first scan (no readable cursor) is a baseline: its dates are
		// not needed and would cost one request per version.
		if haveCursor && !known[raw] && lookups < maxLookups {
			lookups++
			d, err := s.published(ctx, dir, artifact, raw)
			if err != nil {
				if ctx.Err() != nil {
					return source.Result{}, fmt.Errorf("maven: date of %s %s: %w", pkg, raw, context.Cause(ctx))
				}
				if fatalLookup(err) {
					// State is not advanced: the next call retries this version.
					return source.Result{}, fmt.Errorf("maven: date of %s %s: %w", pkg, raw, err)
				}
				s.log.WarnContext(ctx, "maven: pom date lookup failed, version left undated",
					"package", pkg, "version", version.Truncate(raw, 64), "error", err.Error())
			}
			v.PublishedAt = d
		}
		out = append(out, v)
	}
	names := slices.Clone(raws)
	slices.Sort(names)
	if b, err := json.Marshal(cursor{Versions: names}); err == nil {
		state.Cursor = string(b)
	}
	return source.Result{Versions: out, State: state}, nil
}

// parse decodes the metadata and returns its valid versions, deduplicated,
// sorted newest first by the maven scheme, ties by raw string.
func (s *Source) parse(ctx context.Context, pkg, group, artifact string, body []byte) ([]string, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = true
	d.CharsetReader = charsetReader
	var m metadata
	if err := d.Decode(&m); err != nil {
		return nil, fmt.Errorf("maven: decode metadata of %s: %w", pkg, err)
	}
	if m.GroupID != nil && strings.TrimSpace(*m.GroupID) != group {
		return nil, fmt.Errorf("maven: metadata of %s names groupId %q", pkg, version.Truncate(strings.TrimSpace(*m.GroupID), 64))
	}
	if m.ArtifactID != nil && strings.TrimSpace(*m.ArtifactID) != artifact {
		return nil, fmt.Errorf("maven: metadata of %s names artifactId %q", pkg, version.Truncate(strings.TrimSpace(*m.ArtifactID), 64))
	}
	seen := make(map[string]bool, len(m.Versions))
	parsed := make([]mversion.Version, 0, len(m.Versions))
	var skipped []string
	nSkipped := 0
	for _, raw := range m.Versions {
		raw = strings.TrimSpace(raw)
		if !validVersion(raw) {
			nSkipped++
			if len(skipped) < maxLogged {
				skipped = append(skipped, version.Truncate(raw, 64))
			}
			continue
		}
		if seen[raw] {
			continue
		}
		v, err := mversion.Parse(raw)
		if err != nil { // validVersion is stricter; kept for safety
			nSkipped++
			continue
		}
		seen[raw] = true
		parsed = append(parsed, v)
	}
	if nSkipped > 0 {
		s.log.DebugContext(ctx, "maven: skipping invalid version strings", "package", pkg, "count", nSkipped, "examples", skipped)
	}
	if n := len(parsed); n > maxVersions {
		// The metadata lists versions in deploy order: keep the last ones.
		s.log.WarnContext(ctx, "maven: too many versions, keeping the newest by metadata order",
			"package", pkg, "versions", n, "kept", maxVersions)
		parsed = parsed[n-maxVersions:]
	}
	slices.SortFunc(parsed, func(a, b mversion.Version) int {
		if c := mversion.Compare(b, a); c != 0 {
			return c // newest first
		}
		return strings.Compare(a.String(), b.String())
	})
	out := make([]string, len(parsed))
	for i, v := range parsed {
		out[i] = v.String()
	}
	return out, nil
}

// validVersion accepts a version string that is safe as a path segment of
// the pom lookup: not empty, "." or "..", at most maxVersionLen bytes, valid
// UTF-8, and without "/", "\", white space or control characters.
func validVersion(raw string) bool {
	if raw == "" || raw == "." || raw == ".." || len(raw) > maxVersionLen || !utf8.ValidString(raw) {
		return false
	}
	for _, r := range raw {
		if r == '/' || r == '\\' || unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// charsetReader lets encoding/xml read the encodings a Maven repository
// may declare besides UTF-8.
func charsetReader(label string, input io.Reader) (io.Reader, error) {
	switch strings.ToLower(label) {
	case "utf8":
		return input, nil // "UTF-8" itself never reaches the reader
	case "us-ascii", "ascii":
		return input, nil // a subset of UTF-8
	case "iso-8859-1", "iso8859-1", "latin1":
		b, err := io.ReadAll(input)
		if err != nil {
			return nil, err
		}
		var out strings.Builder
		out.Grow(len(b))
		for _, c := range b {
			out.WriteRune(rune(c)) // ISO-8859-1 maps each byte to the same code point
		}
		return strings.NewReader(out.String()), nil
	}
	return nil, fmt.Errorf("unsupported XML encoding %q", version.Truncate(label, 32))
}

// published returns the Last-Modified time of the version's POM (zero when
// the response has none).
func (s *Source) published(ctx context.Context, dir, artifact, raw string) (time.Time, error) {
	v := url.PathEscape(raw)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, dir+"/"+v+"/"+artifact+"-"+v+".pom", nil)
	if err != nil {
		return time.Time{}, err
	}
	res, err := netguard.Fetch(ctx, s.hc, req, netguard.Validators{})
	if err != nil {
		return time.Time{}, err
	}
	t, err := http.ParseTime(res.Header.Get("Last-Modified"))
	if err != nil {
		return time.Time{}, nil
	}
	return t.UTC(), nil
}

// fatalLookup reports whether a date lookup error must fail List: a context
// error or a 429 (the repository asks the client to slow down).
func fatalLookup(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var se *netguard.StatusError
	return errors.As(err, &se) && se.StatusCode == http.StatusTooManyRequests
}
