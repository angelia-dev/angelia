// Package pypi is the Source for PyPI releases (architecture.md section 3.3).
// By default it reads the JSON API, GET {base}/pypi/{name}/json, and lists one
// Version per entry of "releases" that has at least one file. With
// WithAPI(APISimple) it reads the Simple API instead (PEP 503 and PEP 691),
// for pypiserver and other mirrors that do not serve the JSON API. The base
// URL is configurable so mirrors work.
//
// Release notes are out of scope.
package pypi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/version/pep440"
)

// Kind is the registry key of this source.
const Kind = "pypi"

// DefaultBaseURL is the public PyPI.
const DefaultBaseURL = "https://pypi.org"

var (
	nameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	sepRE  = regexp.MustCompile(`[-_.]+`)
)

// ErrInvalidBase is wrapped by the error List returns when New was given an
// unusable base URL or client.
var ErrInvalidBase = errors.New("pypi: invalid source configuration")

// Source implements source.Source for PyPI. It is safe for concurrent use.
type Source struct {
	base   string // API base for requests, may carry userinfo; no trailing slash
	public string // pypi.org page base for subscriber-facing URLs; "" for mirrors
	client *http.Client
	log    *slog.Logger
	api    API
	err    error // configuration error from New, returned by List
}

// Option configures a Source.
type Option func(*Source)

// WithLogger sets the logger used for skipped releases (debug level). The
// default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Source) {
		if l != nil {
			s.log = l
		}
	}
}

// API selects the pypi HTTP API a Source reads (sources.<name>.api).
type API string

const (
	// APIJSON is the PyPI JSON API, {base}/pypi/{name}/json. The default.
	APIJSON API = "json"
	// APISimple is the Simple API, {base}/simple/{name}/ (PEP 503 HTML or
	// PEP 691 JSON), which pypiserver serves.
	APISimple API = "simple"
)

// WithAPI selects the API the source reads. "" means APIJSON; any other value
// than APIJSON and APISimple makes List return an error wrapping
// ErrInvalidBase.
func WithAPI(a API) Option {
	return func(s *Source) { s.api = a }
}

// New returns a Source that queries baseURL (for example DefaultBaseURL or a
// mirror) with c. c is injected, normally the netguard client.
//
// New never panics and has no error return, so the constructor still fits
// the sourcetest and registry factory shape. An invalid configuration (nil
// client, a base that is not an absolute http or https URL, or one with a
// query or fragment) is remembered and returned by every List call wrapping
// ErrInvalidBase. Validate is target-only and unaffected.
//
// The base may carry userinfo ("https://user:token@mirror/"); it is used
// for requests only and never appears in a Version or its Metadata.
//
// Subscriber-facing URLs: with the default base (https://pypi.org) Version.URL
// is the release page and Metadata["project_url"] the project page. For any
// other base both are left out, because mirrors do not serve Warehouse
// project pages.
//
// Metadata["project"] is the PEP 503 name the source requested, and
// Metadata["project_name"] the project name as PyPI reports it (info.name,
// for example "Django"), from which the event's purl is built. It is left
// out when the response has no info.name.
func New(baseURL string, c *http.Client, opts ...Option) *Source {
	s := &Source{client: c, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	if s.api != "" && s.api != APIJSON && s.api != APISimple {
		s.err = fmt.Errorf("%w: api must be %q or %q", ErrInvalidBase, APIJSON, APISimple)
		return s
	}
	if c == nil {
		s.err = fmt.Errorf("%w: nil http client", ErrInvalidBase)
		return s
	}
	u, err := url.Parse(baseURL)
	switch {
	case err != nil:
		s.err = fmt.Errorf("%w: parse: %s", ErrInvalidBase, redactParseErr(err))
	case u.Scheme != "http" && u.Scheme != "https":
		s.err = fmt.Errorf("%w: scheme must be http or https", ErrInvalidBase)
	case u.Host == "":
		s.err = fmt.Errorf("%w: missing host", ErrInvalidBase)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(baseURL, "#"):
		s.err = fmt.Errorf("%w: query and fragment are not allowed", ErrInvalidBase)
	default:
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = strings.TrimRight(u.RawPath, "/")
		s.base = u.String()
		if u.Scheme == "https" && strings.EqualFold(u.Host, "pypi.org") && u.Path == "" && u.User == nil {
			s.public = "https://pypi.org"
		}
	}
	return s
}

// redactParseErr drops the URL that *url.Error echoes, which may hold
// credentials.
func redactParseErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return "invalid URL"
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// Normalize returns the PEP 503 normalized form of a project name: lower
// case, with each run of "-", "_" and "." replaced by a single "-". It is
// meant for building the target coalescing key, so that "Requests" and
// "requests" count as one target.
func Normalize(name string) string {
	return strings.ToLower(sepRE.ReplaceAllString(name, "-"))
}

// Validate implements source.Source. The only coordinate is "name".
func (*Source) Validate(t source.Target) error {
	_, err := nameOf(t)
	return err
}

// Canonical implements source.Canonicalizer: the "name" coordinate becomes
// its PEP 503 normalized form, so "Requests", "requests" and " requests "
// share one target. PyPI itself treats those spellings as one project
// (https://peps.python.org/pep-0503/#normalized-names). Other coordinates
// are kept as they are. See ADR 0021.
func (*Source) Canonical(t source.Target) (source.Target, error) {
	name, err := nameOf(t)
	if err != nil {
		return source.Target{}, err
	}
	coords := maps.Clone(t.Coordinates)
	coords["name"] = name
	return source.Target{Kind: t.Kind, Coordinates: coords}, nil
}

func nameOf(t source.Target) (string, error) {
	if t.Kind != Kind {
		return "", fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	name := strings.TrimSpace(t.Coordinates["name"])
	if name == "" {
		return "", fmt.Errorf("%w: pypi target needs coordinate %q", source.ErrInvalidTarget, "name")
	}
	if !nameRE.MatchString(name) {
		return "", fmt.Errorf("%w: %q is not a valid PyPI project name", source.ErrInvalidTarget, name)
	}
	return Normalize(name), nil
}

// List implements source.Source. Versions are newest first by PEP 440 order;
// releases whose version PEP 440 cannot parse come last, sorted by Raw.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	if s.err != nil {
		return source.Result{}, s.err
	}
	name, err := nameOf(t)
	if err != nil {
		return source.Result{}, err
	}
	if s.api == APISimple {
		return s.listSimple(ctx, name, st)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/pypi/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return source.Result{}, fmt.Errorf("pypi: build request for %q: %w", name, err)
	}
	req.Header.Set("Accept", "application/json")
	prev := netguard.Validators{ETag: st.ETag, LastModified: st.LastModified}
	res, err := netguard.Fetch(ctx, s.client, req, prev)
	if err != nil {
		return source.Result{}, fmt.Errorf("pypi: fetch %q: %w", name, err)
	}
	state := source.FetchState{ETag: res.Validators.ETag, LastModified: res.Validators.LastModified}
	if res.NotModified {
		return source.Result{State: state, NotModified: true}, nil
	}
	var doc struct {
		Info struct {
			Name string `json:"name"`
		} `json:"info"`
		Releases map[string][]file `json:"releases"`
	}
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return source.Result{}, fmt.Errorf("pypi: decode response for %q: %w", name, err)
	}
	if doc.Releases == nil {
		return source.Result{}, fmt.Errorf("pypi: response for %q has no \"releases\"", name)
	}
	versions := make([]source.Version, 0, len(doc.Releases))
	for raw, files := range doc.Releases {
		if len(files) == 0 {
			s.log.DebugContext(ctx, "pypi: skipping release without files", "project", name, "version", raw)
			continue
		}
		v := s.version(ctx, name, raw, files)
		if doc.Info.Name != "" {
			v.Metadata["project_name"] = doc.Info.Name
		}
		versions = append(versions, v)
	}
	sortNewestFirst(versions)
	return source.Result{Versions: versions, State: state}, nil
}

// file is the part of a release file the source reads.
type file struct {
	UploadTime   string `json:"upload_time_iso_8601"`
	Yanked       bool   `json:"yanked"`
	YankedReason string `json:"yanked_reason"`
}

func (s *Source) version(ctx context.Context, name, raw string, files []file) source.Version {
	v := source.Version{
		Raw:    raw,
		Yanked: true,
		Metadata: map[string]any{
			"project": name,
			"files":   len(files),
		},
	}
	if s.public != "" {
		v.URL = s.public + "/project/" + url.PathEscape(name) + "/" + url.PathEscape(raw) + "/"
		v.Metadata["project_url"] = s.public + "/project/" + url.PathEscape(name) + "/"
	}
	var reason string
	for _, f := range files {
		// PEP 592: yanking is per file; a release is yanked when all are.
		v.Yanked = v.Yanked && f.Yanked
		if f.YankedReason != "" && reason == "" {
			reason = f.YankedReason
		}
		ts, err := time.Parse(time.RFC3339Nano, f.UploadTime)
		if err != nil {
			s.log.DebugContext(ctx, "pypi: ignoring file upload time", "project", name, "version", raw, "value", f.UploadTime)
			continue
		}
		if v.PublishedAt.IsZero() || ts.Before(v.PublishedAt) {
			v.PublishedAt = ts.UTC()
		}
	}
	if v.Yanked && reason != "" {
		v.Metadata["yanked_reason"] = reason
	}
	return v
}

func sortNewestFirst(vs []source.Version) {
	parsed := make(map[string]pep440.Version, len(vs))
	for _, v := range vs {
		if p, err := pep440.Parse(v.Raw); err == nil {
			parsed[v.Raw] = p
		}
	}
	slices.SortFunc(vs, func(a, b source.Version) int {
		pa, okA := parsed[a.Raw]
		pb, okB := parsed[b.Raw]
		switch {
		case okA && okB:
			if c := pep440.Compare(pb, pa); c != 0 {
				return c
			}
		case okA:
			return -1
		case okB:
			return 1
		}
		return strings.Compare(a.Raw, b.Raw)
	})
}

var _ source.Source = (*Source)(nil)
