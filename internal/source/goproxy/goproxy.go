// Package goproxy is the Source for Go modules. It lists the tagged versions
// of one module from proxy.golang.org or any server that speaks the GOPROXY
// protocol (https://go.dev/ref/mod#goproxy-protocol): Athens, Artifactory,
// Nexus, a corporate proxy.
//
// # Coordinates
//
// One key, "module", the module path exactly as written in go.mod
// (github.com/BurntSushi/toml). Module paths are case-sensitive, so the
// source has no canonical form (no source.Canonicalizer). The path is
// case-encoded ("!burnt!sushi") only in request URLs.
//
// # What List does
//
// GET {base}/{escaped module}/@v/list returns the tagged versions, one per
// line, unordered. A line that is not a canonical SemVer version with a "v"
// prefix, or that is a pseudo-version, is skipped (one DEBUG record per List
// call). "+incompatible" versions are kept: it is SemVer build metadata, so
// they order by their number, as in Go. The result is sorted by SemVer
// precedence, newest first, ties by Raw.
//
// For each version that is new since the cursor, GET {base}/{module}/@v/{version}.info
// gives the Time of the version, which becomes PublishedAt. There is none on
// the first scan (a baseline, ADR 0010 section 5), at most maxLookups per
// call, and a failed lookup leaves the version undated. The proxy sends no
// ETag or Last-Modified, so there are no conditional requests and the result
// is never NotModified. The cursor is JSON holding the sorted names of the
// versions already reported.
//
// A module that exists but has no tagged version (an empty 200 body) gives no
// versions and no error. A 404 or 410 is an error: the module does not exist,
// or its path has the wrong case.
//
// # Retractions and deprecation
//
// Each List also requests GET {base}/{module}/@latest. When the latest
// version differs from the one in the cursor, GET {base}/{module}/@v/{latest}.mod
// is parsed with modfile.ParseLax for its "retract" directives and the
// "// Deprecated:" comment of the module line, and both are kept in the
// cursor. A listed version inside a retracted interval is Yanked, with
// Metadata["retracted"] = the rationale (at most 256 bytes). A deprecated
// module sets Metadata["deprecated"] on every version. If @latest is a 404,
// a 410 or a pseudo-version, the latest is the highest release of the list
// that is not "+incompatible" (else the highest version). If @latest or the
// .mod cannot be read, the retractions of the cursor are kept and a WARN is
// logged; a .mod that cannot be parsed is the same, but is not asked for
// again. A 429 or a cancelled context fails the call.
//
// # What a Version carries
//
// Raw, PublishedAt, Yanked, URL (https://pkg.go.dev/{module}@{version}, only for
// the default base URL: pkg.go.dev cannot show a module of a private proxy)
// and Metadata (retracted, deprecated). Prerelease is nil: the version scheme
// decides.
package goproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

const (
	// Kind is the registry key of this source.
	Kind = "go-proxy"
	// DefaultBaseURL is the public Go module proxy.
	DefaultBaseURL = "https://proxy.golang.org"
	// DefaultPageURL is the prefix of module pages for the public proxy.
	DefaultPageURL = "https://pkg.go.dev"

	maxPathLen = 300
	// maxLookups bounds the .info requests of one List call. Versions past
	// the bound are returned undated and are not looked up again.
	maxLookups = 50
	// maxNoteLen bounds a retraction rationale or deprecation message kept
	// in Metadata and in the cursor.
	maxNoteLen = 256
	// maxModBytes bounds a go.mod read from the proxy, and maxRetractIntervals
	// the retract intervals kept from it: both come from an untrusted module.
	maxModBytes         = 1 << 20
	maxRetractIntervals = 1000
)

// Option configures a Source.
type Option func(*Source)

// WithLogger sets the logger for skipped lines and failed date lookups. The
// default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Source) {
		if l != nil {
			s.log = l
		}
	}
}

// Source implements source.Source for the GOPROXY protocol. It is safe for
// concurrent use.
type Source struct {
	base    string
	hc      *http.Client
	pageURL string
	log     *slog.Logger
}

var _ source.Source = (*Source)(nil)

// New returns a Source reading from baseURL (DefaultBaseURL if empty; a path
// is allowed, as in https://host/artifactory/api/go/go-virtual) with hc,
// normally the HTTPClient of a netguard client.
func New(baseURL string, hc *http.Client, opts ...Option) *Source {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	s := &Source{base: strings.TrimRight(baseURL, "/"), hc: hc, log: slog.Default()}
	if s.base == DefaultBaseURL {
		s.pageURL = DefaultPageURL
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// Validate implements source.Source. It does no I/O.
func (*Source) Validate(t source.Target) error {
	_, err := modulePath(t)
	return err
}

// modulePath checks the "module" coordinate. The stored value is not
// trimmed or case-folded: it must equal what is validated.
func modulePath(t source.Target) (string, error) {
	if t.Kind != Kind {
		return "", fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	p := t.Coordinates["module"]
	switch {
	case p == "":
		return "", fmt.Errorf("%w: go-proxy target needs a module", source.ErrInvalidTarget)
	case len(p) > maxPathLen:
		return "", fmt.Errorf("%w: go-proxy module longer than %d characters", source.ErrInvalidTarget, maxPathLen)
	case strings.Contains(p, "://"):
		return "", fmt.Errorf("%w: go-proxy module %q is a URL, want a module path", source.ErrInvalidTarget, p)
	case strings.Contains(p, "@"):
		return "", fmt.Errorf("%w: go-proxy module %q has a version suffix, give the path only", source.ErrInvalidTarget, p)
	case strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/"):
		return "", fmt.Errorf("%w: go-proxy module %q starts or ends with /", source.ErrInvalidTarget, p)
	}
	if err := module.CheckPath(p); err != nil {
		return "", fmt.Errorf("%w: go-proxy module: %v", source.ErrInvalidTarget, err) //nolint:errorlint // CheckPath's text is the message
	}
	return p, nil
}

// cursor is the JSON kept in FetchState.Cursor: the names of the versions
// already reported, sorted.
type cursor struct {
	Versions []string `json:"versions"`
	// Latest is the version whose go.mod Retracted and Deprecated come from.
	Latest     string     `json:"latest,omitempty"`
	Retracted  []interval `json:"retracted,omitempty"`
	Deprecated string     `json:"deprecated,omitempty"`
}

// interval is a retracted range of versions, bounds included; a single
// version has Low == High.
type interval struct {
	Low  string `json:"low"`
	High string `json:"high"`
	Why  string `json:"why,omitempty"`
}

// parseCursor decodes c. ok is false for an empty or unreadable cursor, which
// marks a first scan.
func parseCursor(c string) (cur cursor, ok bool) {
	if c == "" || json.Unmarshal([]byte(c), &cur) != nil {
		return cursor{}, false
	}
	return cur, true
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	mod, err := modulePath(t)
	if err != nil {
		return source.Result{}, err
	}
	if s.hc == nil {
		return source.Result{}, errors.New("go-proxy: source has no HTTP client")
	}
	esc, err := module.EscapePath(mod)
	if err != nil {
		return source.Result{}, fmt.Errorf("%w: go-proxy module: %v", source.ErrInvalidTarget, err) //nolint:errorlint // see modulePath
	}
	cur, haveCursor := parseCursor(st.Cursor)

	body, err := s.get(ctx, s.base+"/"+esc+"/@v/list")
	if err != nil {
		var se *netguard.StatusError
		if errors.As(err, &se) && (se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusGone) {
			return source.Result{}, fmt.Errorf("go-proxy: module %s not found (module paths are case-sensitive): %w", mod, err)
		}
		return source.Result{}, fmt.Errorf("go-proxy: list %s: %w", mod, err)
	}
	names := s.parseList(ctx, mod, body)
	next, err := s.retractions(ctx, mod, esc, names, cur)
	if err != nil {
		return source.Result{}, err
	}

	known := make(map[string]bool, len(cur.Versions))
	for _, n := range cur.Versions {
		known[n] = true
	}
	out := make([]source.Version, 0, len(names))
	lookups := 0
	for _, raw := range names {
		v := source.Version{Raw: raw, URL: s.pageLink(mod, raw)}
		next.mark(&v)
		// A first scan (no readable cursor) is a baseline: no lookups.
		if haveCursor && !known[raw] && lookups < maxLookups {
			lookups++
			ts, err := s.info(ctx, esc, raw)
			if err != nil {
				if ctx.Err() != nil {
					return source.Result{}, fmt.Errorf("go-proxy: date of %s %s: %w", mod, raw, context.Cause(ctx))
				}
				if isFatal(ctx, err) {
					// The cursor does not advance: the next call retries.
					return source.Result{}, fmt.Errorf("go-proxy: date of %s %s: %w", mod, raw, err)
				}
				s.log.WarnContext(ctx, "go-proxy: date lookup failed, version left undated", "module", mod, "version", raw)
			}
			v.PublishedAt = ts
		}
		out = append(out, v)
	}
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	state := source.FetchState{}
	next.Versions = sorted
	if b, err := json.Marshal(next); err == nil {
		state.Cursor = string(b)
	}
	return source.Result{Versions: out, State: state}, nil
}

// mark sets Yanked and the metadata of v from the retractions and deprecation
// of c.
func (c *cursor) mark(v *source.Version) {
	for _, iv := range c.Retracted {
		if semver.Compare(v.Raw, iv.Low) >= 0 && semver.Compare(v.Raw, iv.High) <= 0 {
			v.Yanked = true
			if v.Metadata == nil {
				v.Metadata = map[string]any{}
			}
			if why, ok := v.Metadata["retracted"].(string); !ok || why == "" {
				v.Metadata["retracted"] = iv.Why
			}
		}
	}
	if c.Deprecated != "" {
		if v.Metadata == nil {
			v.Metadata = map[string]any{}
		}
		v.Metadata["deprecated"] = c.Deprecated
	}
}

// retractions returns the cursor to carry forward, with the retractions and
// deprecation up to date: those of cur when the latest version has not
// changed or cannot be read, else those of the go.mod of the new latest. The
// Versions of the result are not set.
func (s *Source) retractions(ctx context.Context, mod, esc string, names []string, cur cursor) (cursor, error) {
	next := cursor{Latest: cur.Latest, Retracted: cur.Retracted, Deprecated: cur.Deprecated}
	latest, err := s.latest(ctx, mod, esc, names)
	if err != nil {
		if isFatal(ctx, err) {
			return cursor{}, fmt.Errorf("go-proxy: latest of %s: %w", mod, err)
		}
		s.log.WarnContext(ctx, "go-proxy: @latest lookup failed, keeping the known retractions", "module", mod)
		return next, nil
	}
	if latest == "" || latest == cur.Latest {
		return next, nil
	}
	escVer, err := module.EscapeVersion(latest)
	if err != nil {
		return next, nil
	}
	body, err := s.get(ctx, s.base+"/"+esc+"/@v/"+escVer+".mod")
	if err != nil {
		if isFatal(ctx, err) {
			return cursor{}, fmt.Errorf("go-proxy: go.mod of %s %s: %w", mod, latest, err)
		}
		// Latest stays as it was: the next call asks again.
		s.log.WarnContext(ctx, "go-proxy: go.mod lookup failed, keeping the known retractions", "module", mod, "version", latest)
		return next, nil
	}
	if len(body) > maxModBytes {
		s.log.WarnContext(ctx, "go-proxy: go.mod is too large, keeping the known retractions", "module", mod, "version", latest, "bytes", len(body))
		return next, nil
	}
	next.Latest = latest
	f, err := modfile.ParseLax("go.mod", body, nil)
	if err != nil {
		s.log.WarnContext(ctx, "go-proxy: go.mod is malformed, keeping the known retractions", "module", mod, "version", latest)
		return next, nil
	}
	next.Retracted, next.Deprecated = nil, ""
	capped := false
	for _, r := range f.Retract {
		if !semver.IsValid(r.Low) || !semver.IsValid(r.High) {
			continue
		}
		if len(next.Retracted) >= maxRetractIntervals {
			capped = true
			break
		}
		next.Retracted = append(next.Retracted, interval{Low: r.Low, High: r.High, Why: truncate(r.Rationale, maxNoteLen)})
	}
	if capped {
		s.log.WarnContext(ctx, "go-proxy: go.mod has too many retract intervals, ignoring the rest", "module", mod, "version", latest, "max", maxRetractIntervals)
	}
	if f.Module != nil {
		next.Deprecated = truncate(f.Module.Deprecated, maxNoteLen)
	}
	return next, nil
}

// latest returns the version whose go.mod holds the current retractions: the
// answer of @latest, or, when the proxy has none (404, 410, a pseudo-version
// or an unreadable answer), the highest release of names that is not
// "+incompatible", else the highest version. It is "" if there is none.
func (s *Source) latest(ctx context.Context, mod, esc string, names []string) (string, error) {
	body, err := s.get(ctx, s.base+"/"+esc+"/@latest")
	if err == nil {
		var info struct{ Version string }
		if json.Unmarshal(body, &info) == nil && validVersion(info.Version) {
			return info.Version, nil
		}
		s.log.DebugContext(ctx, "go-proxy: @latest is not a tagged version, using the list", "module", mod)
	} else {
		var se *netguard.StatusError
		if !errors.As(err, &se) || (se.StatusCode != http.StatusNotFound && se.StatusCode != http.StatusGone) {
			return "", err
		}
	}
	return fallbackLatest(names), nil
}

// fallbackLatest picks, from versions sorted newest first, the highest
// release that is not "+incompatible", else the highest one that is not
// "+incompatible", else the first.
func fallbackLatest(names []string) string {
	var pre string
	for _, n := range names {
		if semver.Build(n) == "+incompatible" {
			continue
		}
		if semver.Prerelease(n) == "" {
			return n
		}
		if pre == "" {
			pre = n
		}
	}
	if pre != "" {
		return pre
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// truncate cuts s to at most n bytes at a rune boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// parseList returns the valid, de-duplicated versions of an @v/list body,
// newest first (SemVer precedence, ties by raw string).
func (s *Source) parseList(ctx context.Context, mod string, body []byte) []string {
	seen := map[string]bool{}
	var out []string
	skipped := 0
	for line := range strings.Lines(string(body)) {
		v := strings.TrimSpace(line)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		if !validVersion(v) {
			skipped++
			continue
		}
		out = append(out, v)
	}
	if skipped > 0 {
		s.log.DebugContext(ctx, "go-proxy: skipped invalid or pseudo versions", "module", mod, "count", skipped)
	}
	slices.SortFunc(out, func(a, b string) int {
		if c := semver.Compare(b, a); c != 0 {
			return c // newest first
		}
		return strings.Compare(a, b)
	})
	return out
}

// validVersion reports whether v is a full SemVer version with a "v" prefix
// ("v1.2.3", "v1.0.0-rc.1", "v2.0.0+incompatible"), not a shorthand like
// "v1" and not a pseudo-version.
func validVersion(v string) bool {
	if !semver.IsValid(v) || module.IsPseudoVersion(v) {
		return false
	}
	// Canonical keeps a prerelease and drops build metadata, which is added back.
	return semver.Canonical(v)+semver.Build(v) == v
}

func (s *Source) pageLink(mod, ver string) string {
	if s.pageURL == "" {
		return ""
	}
	return s.pageURL + "/" + mod + "@" + ver
}

// info returns the Time of version raw (.info). A missing .info (404, 410)
// or one without a time yields a zero time and no error.
func (s *Source) info(ctx context.Context, escMod, raw string) (time.Time, error) {
	escVer, err := module.EscapeVersion(raw)
	if err != nil {
		return time.Time{}, err
	}
	body, err := s.get(ctx, s.base+"/"+escMod+"/@v/"+escVer+".info")
	if err != nil {
		var se *netguard.StatusError
		if errors.As(err, &se) && (se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusGone) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	var info struct {
		Time time.Time
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return time.Time{}, fmt.Errorf("decode .info: %w", err)
	}
	return info.Time.UTC(), nil
}

func (s *Source) get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	res, err := netguard.Fetch(ctx, s.hc, req, netguard.Validators{})
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// isFatal reports whether a lookup error must fail List: a context error or a
// rate limit (429, or a 403 that named a retry time). A 5xx with Retry-After is
// not one: the lookup is skipped with a warning.
func isFatal(ctx context.Context, err error) bool {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var se *netguard.StatusError
	return errors.As(err, &se) && (se.StatusCode == http.StatusTooManyRequests || (se.StatusCode == http.StatusForbidden && !se.RetryAt.IsZero()))
}
