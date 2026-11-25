// Package httpregex is the http-regex Source (ADR 0038): it GETs the URL of
// the watch and runs an RE2 regular expression over the body; each match, or
// its capture group, is a raw version.
//
// # Coordinates
//
//   - name: the package name in events (data.package), 1 to 128 bytes,
//     printable, no white space.
//   - url: https://, or http:// for a host in http.allow_http_hosts; no user
//     information or fragment; at most 2048 bytes.
//   - regex: Go regexp syntax (RE2, linear time), at most 1024 bytes, no
//     repetition count above 100 and at most 2,000 compiled instructions. With no
//     capture group the whole match is the version; with one group, that
//     group; with several, the group named "version".
//
// # Fetching
//
// The request goes through the injected client (netguard in production) with
// at most 5 redirects, none to plain http outside the allow list, and a 2 MiB
// body cap. ETag and Last-Modified are sent back; the cursor is a hash of
// the extracted list, so a page without validators that yields the same
// versions is NotModified. Versions are in document order, de-duplicated;
// more than 1,000 distinct versions, or none, is an error. PublishedAt,
// Prerelease and URL are left empty.
package httpregex

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"regexp/syntax"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/internal/generic"
)

const (
	// Kind is the registry key of this source.
	Kind = "http-regex"
	// MaxRegexBytes is the longest regex coordinate.
	MaxRegexBytes = 1024
	// MaxRepeat is the largest count of a {n,m} repetition.
	MaxRepeat = 100
	// MaxProgInst is the most instructions the compiled program may have.
	// RE2 runs in time linear in the input, but the factor is the program
	// size, and a match over a 2 MiB body cannot be cancelled: a pattern
	// such as (a|aa|aaa){1,999}b compiles to thousands of instructions and
	// takes seconds per 64 KiB.
	MaxProgInst = 2000
	// accept prefers the formats a release page is served in.
	accept = "text/html, text/plain;q=0.9, */*;q=0.8"
)

// Option configures a Source.
type Option func(*Source)

// WithAllowHTTPHosts sets http.allow_http_hosts: the hosts a url coordinate,
// or a redirect, may reach over plain http.
func WithAllowHTTPHosts(hosts []string) Option {
	return func(s *Source) { s.allowHTTP = hosts }
}

// WithLogger sets the logger of the debug line about skipped values.
func WithLogger(l *slog.Logger) Option { return func(s *Source) { s.log = l } }

// Source implements source.Source for http-regex. It is safe for concurrent
// use.
type Source struct {
	hc        *http.Client
	allowHTTP []string
	log       *slog.Logger
	fetch     *generic.Fetcher
}

var (
	_ source.Source        = (*Source)(nil)
	_ source.Canonicalizer = (*Source)(nil)
)

// New returns an http-regex Source that sends its requests with hc, normally
// the HTTPClient of the netguard client.
func New(hc *http.Client, opts ...Option) *Source {
	s := &Source{hc: hc}
	for _, o := range opts {
		o(s)
	}
	s.fetch = generic.NewFetcher(hc, s.allowHTTP)
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

type target struct {
	name  string
	re    *regexp.Regexp
	group int // the submatch that is the version; 0 is the whole match
}

func (s *Source) parse(t source.Target) (target, *url.URL, error) {
	if t.Kind != Kind {
		return target{}, nil, fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	if err := generic.OnlyKeys(t, "name", "url", "regex"); err != nil {
		return target{}, nil, err
	}
	if err := generic.ValidateName(t.Coordinates["name"]); err != nil {
		return target{}, nil, err
	}
	u, err := generic.ValidateURL(t.Coordinates["url"], s.allowHTTP)
	if err != nil {
		return target{}, nil, err
	}
	re, group, err := Compile(t.Coordinates["regex"])
	if err != nil {
		return target{}, nil, err
	}
	return target{name: t.Coordinates["name"], re: re, group: group}, u, nil
}

// Compile checks a regex coordinate and returns it compiled with the index
// of the submatch that is the version (D53). Errors wrap
// source.ErrInvalidTarget.
func Compile(pattern string) (*regexp.Regexp, int, error) {
	switch {
	case pattern == "":
		return nil, 0, fmt.Errorf("%w: needs a regex coordinate", source.ErrInvalidTarget)
	case len(pattern) > MaxRegexBytes:
		return nil, 0, fmt.Errorf("%w: regex longer than %d bytes", source.ErrInvalidTarget, MaxRegexBytes)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: regex: %w", source.ErrInvalidTarget, err)
	}
	if err := checkCost(pattern); err != nil {
		return nil, 0, fmt.Errorf("%w: regex: %w", source.ErrInvalidTarget, err)
	}
	switch n := re.NumSubexp(); n {
	case 0:
		return re, 0, nil
	case 1:
		return re, 1, nil
	default:
		i := re.SubexpIndex("version")
		if i < 0 {
			return nil, 0, fmt.Errorf("%w: regex has %d capture groups; name the one that is the version (?P<version>...) or make the others non-capturing (?:...)", source.ErrInvalidTarget, n)
		}
		return re, i, nil
	}
}

// checkCost bounds the matching cost of pattern (ADR 0038 §4): no
// repetition count above MaxRepeat, and at most MaxProgInst instructions
// once simplified and compiled, the steps regexp.Compile itself takes.
func checkCost(pattern string) error {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return err
	}
	if n := maxRepeat(re); n > MaxRepeat {
		return fmt.Errorf("repetition count %d is above %d; use + or * instead", n, MaxRepeat)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return err
	}
	if n := len(prog.Inst); n > MaxProgInst {
		return fmt.Errorf("the expression is too complex (%d instructions compiled, at most %d); simplify it", n, MaxProgInst)
	}
	return nil
}

// maxRepeat returns the largest count of a {n,m} repetition in re.
func maxRepeat(re *syntax.Regexp) int {
	n := 0
	if re.Op == syntax.OpRepeat {
		n = max(re.Min, re.Max)
	}
	for _, sub := range re.Sub {
		n = max(n, maxRepeat(sub))
	}
	return n
}

// Validate implements source.Source. It does no I/O.
func (s *Source) Validate(t source.Target) error {
	_, _, err := s.parse(t)
	return err
}

// Canonical implements source.Canonicalizer: the url is written in its
// canonical form (generic.CanonicalURL), so "HTTPS://Example.ORG:443" and
// "https://example.org/" share one target. name and regex are kept as
// written.
func (s *Source) Canonical(t source.Target) (source.Target, error) {
	_, u, err := s.parse(t)
	if err != nil {
		return source.Target{}, err
	}
	c := maps.Clone(t.Coordinates)
	c["url"] = generic.CanonicalURL(u)
	return source.Target{Kind: t.Kind, Coordinates: c}, nil
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	tg, u, err := s.parse(t)
	if err != nil {
		return source.Result{}, err
	}
	res, err := s.fetch.Fetch(ctx, u, accept, st)
	if err != nil {
		return source.Result{}, fmt.Errorf("http-regex %s: %w", tg.name, err)
	}
	if res.NotModified {
		return generic.NotModified(res.Validators, st), nil
	}
	raw, err := Extract(tg.re, tg.group, res.Body)
	if err != nil {
		return source.Result{}, fmt.Errorf("http-regex %s: %s: %w", tg.name, u.Host, err)
	}
	vs, skipped, err := generic.Versions(raw)
	if err != nil {
		return source.Result{}, fmt.Errorf("http-regex %s: %s: %w", tg.name, u.Host, err)
	}
	if skipped > 0 && s.log != nil {
		s.log.LogAttrs(ctx, slog.LevelDebug, "http-regex: skipped unusable matches", slog.String("name", tg.name), slog.Int("skipped", skipped))
	}
	return generic.Result(vs, res.Validators, st), nil
}

// Extract returns the submatch group of every match of re in body, in
// document order. A group that did not take part in a match yields "",
// which generic.Versions skips. More than generic.MaxMatches matches is
// generic.ErrTooManyMatches.
func Extract(re *regexp.Regexp, group int, body []byte) ([]string, error) {
	ms := re.FindAllSubmatchIndex(body, generic.MaxMatches+1)
	if len(ms) > generic.MaxMatches {
		return nil, generic.ErrTooManyMatches
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		a, b := m[2*group], m[2*group+1]
		if a < 0 {
			out = append(out, "")
			continue
		}
		out = append(out, string(body[a:b]))
	}
	return out, nil
}
