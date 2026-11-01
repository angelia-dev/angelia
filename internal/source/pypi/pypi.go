// Package pypi is the Source for PyPI releases (architecture.md section 3.3).
// It reads the JSON API, GET {base}/pypi/{name}/json, and lists one Version
// per entry of "releases" that has at least one file. The base URL is
// configurable so mirrors work.
//
// PyPI's Simple API and release notes are out of scope.
package pypi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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

// Source implements source.Source for PyPI. It is safe for concurrent use.
type Source struct {
	base   string
	client *http.Client
	log    *slog.Logger
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

// New returns a Source that queries baseURL (for example DefaultBaseURL or a
// mirror) with c. c is injected, normally the netguard client; it must not
// be nil.
func New(baseURL string, c *http.Client, opts ...Option) *Source {
	s := &Source{base: strings.TrimRight(baseURL, "/"), client: c, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// Normalize returns the PEP 503 normalized form of a project name: lower
// case, with each run of "-", "_" and "." replaced by a single "-".
func Normalize(name string) string {
	return strings.ToLower(sepRE.ReplaceAllString(name, "-"))
}

// Validate implements source.Source. The only coordinate is "name".
func (*Source) Validate(t source.Target) error {
	_, err := nameOf(t)
	return err
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
	name, err := nameOf(t)
	if err != nil {
		return source.Result{}, err
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
		versions = append(versions, s.version(name, raw, files))
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

func (s *Source) version(name, raw string, files []file) source.Version {
	v := source.Version{
		Raw:    raw,
		URL:    s.base + "/project/" + url.PathEscape(name) + "/" + url.PathEscape(raw) + "/",
		Yanked: true,
		Metadata: map[string]any{
			"project":     name,
			"project_url": s.base + "/project/" + url.PathEscape(name) + "/",
			"files":       len(files),
		},
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
			s.log.Debug("pypi: ignoring file upload time", "project", name, "version", raw, "value", f.UploadTime)
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
