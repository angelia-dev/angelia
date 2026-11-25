// Package httpjson is the http-json Source (ADR 0038): it GETs the URL of
// the watch, parses the body as JSON and selects version strings with a
// JSONPath expression of the D50 subset of RFC 9535 (see jsonpath.go).
//
// # Coordinates
//
//   - name: the package name in events (data.package), 1 to 128 bytes,
//     printable, no white space.
//   - url: https://, or http:// for a host in http.allow_http_hosts; no user
//     information or fragment; at most 2048 bytes.
//   - jsonpath: $ followed by .name, ['name'], [n], [-n], [*] or .*
//     segments; at most 512 bytes and 32 segments.
//   - extract (optional): values (the default) takes the selected strings
//     and numbers; keys takes the member names of the selected objects, for
//     documents keyed by version ({"releases": {"1.0": {...}}}).
//
// # Values
//
// The body is decoded with json.Decoder.UseNumber, so a number keeps its
// literal text (1.10 stays "1.10"). Strings and numbers are versions;
// booleans, null, objects and arrays are skipped. Fetching, caps and the
// cursor are those of http-regex (internal/source/internal/generic):
// at most 5 redirects, no downgrade, 2 MiB, 1,000 distinct versions, none is
// an error. PublishedAt, Prerelease and URL are left empty.
package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/internal/generic"
)

const (
	// Kind is the registry key of this source.
	Kind = "http-json"
	// ExtractValues takes the selected strings and numbers (the default).
	ExtractValues = "values"
	// ExtractKeys takes the member names of the selected objects.
	ExtractKeys = "keys"
	accept      = "application/json"
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

// Source implements source.Source for http-json. It is safe for concurrent
// use.
type Source struct {
	allowHTTP []string
	log       *slog.Logger
	fetch     *generic.Fetcher
}

var (
	_ source.Source        = (*Source)(nil)
	_ source.Canonicalizer = (*Source)(nil)
)

// New returns an http-json Source that sends its requests with hc, normally
// the HTTPClient of the netguard client.
func New(hc *http.Client, opts ...Option) *Source {
	s := &Source{}
	for _, o := range opts {
		o(s)
	}
	s.fetch = generic.NewFetcher(hc, s.allowHTTP)
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

type target struct {
	name string
	path *Path
	keys bool
}

func (s *Source) parse(t source.Target) (target, *url.URL, error) {
	if t.Kind != Kind {
		return target{}, nil, fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	if err := generic.OnlyKeys(t, "name", "url", "jsonpath", "extract"); err != nil {
		return target{}, nil, err
	}
	if err := generic.ValidateName(t.Coordinates["name"]); err != nil {
		return target{}, nil, err
	}
	u, err := generic.ValidateURL(t.Coordinates["url"], s.allowHTTP)
	if err != nil {
		return target{}, nil, err
	}
	expr, ok := t.Coordinates["jsonpath"]
	if !ok {
		return target{}, nil, fmt.Errorf("%w: needs a jsonpath coordinate", source.ErrInvalidTarget)
	}
	p, err := ParsePath(expr)
	if err != nil {
		return target{}, nil, fmt.Errorf("%w: %w", source.ErrInvalidTarget, err)
	}
	tg := target{name: t.Coordinates["name"], path: p}
	switch ex, ok := t.Coordinates["extract"]; {
	case !ok || ex == ExtractValues:
	case ex == ExtractKeys:
		tg.keys = true
	default:
		return target{}, nil, fmt.Errorf("%w: extract must be %s or %s", source.ErrInvalidTarget, ExtractValues, ExtractKeys)
	}
	return tg, u, nil
}

// Validate implements source.Source. It does no I/O.
func (s *Source) Validate(t source.Target) error {
	_, _, err := s.parse(t)
	return err
}

// Canonical implements source.Canonicalizer: the url is written in its
// canonical form (generic.CanonicalURL). The other coordinates are kept as
// written: an explicit "extract: values" and none are two spellings that
// are not folded, since folding would remove a key the user wrote.
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
		return source.Result{}, fmt.Errorf("http-json %s: %w", tg.name, err)
	}
	if res.NotModified {
		return generic.NotModified(res.Validators, st), nil
	}
	doc, err := decode(res.Body)
	if err != nil {
		return source.Result{}, fmt.Errorf("http-json %s: the body from %s is not JSON: %w", tg.name, u.Host, err)
	}
	raw, skipped, err := Extract(tg.path, tg.keys, doc)
	if err != nil {
		return source.Result{}, fmt.Errorf("http-json %s: %s: %w", tg.name, u.Host, err)
	}
	vs, n, err := generic.Versions(raw)
	skipped += n
	if err != nil {
		return source.Result{}, fmt.Errorf("http-json %s: %s: %w", tg.name, u.Host, err)
	}
	if skipped > 0 && s.log != nil {
		s.log.LogAttrs(ctx, slog.LevelDebug, "http-json: skipped unusable values", slog.String("name", tg.name), slog.Int("skipped", skipped))
	}
	return generic.Result(vs, res.Validators, st), nil
}

// decode reads exactly one JSON value, numbers as json.Number.
func decode(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the JSON value")
	}
	return v, nil
}

// Extract returns the raw values path selects in doc: strings as they are,
// numbers as their literal text, other types skipped (counted). With keys,
// every selected node must be an object, and its member names, sorted, are
// the values. More than generic.MaxMatches values is
// generic.ErrTooManyMatches.
func Extract(path *Path, keys bool, doc any) (raw []string, skipped int, err error) {
	for i, n := range path.Select(doc) {
		if keys {
			m, ok := n.(map[string]any)
			if !ok {
				return nil, 0, fmt.Errorf("extract: keys needs objects, but node %d is %s", i, typeName(n))
			}
			raw = append(raw, slices.Sorted(maps.Keys(m))...)
		} else {
			switch v := n.(type) {
			case string:
				raw = append(raw, v)
			case json.Number:
				raw = append(raw, v.String())
			default:
				skipped++
			}
		}
		if len(raw) > generic.MaxMatches {
			return nil, 0, generic.ErrTooManyMatches
		}
	}
	return raw, skipped, nil
}
