// Package crates is the Source for crates.io. It lists the versions of a
// crate, yanked ones included.
//
// # Two endpoints
//
// The versions and their yank flags come from the sparse index, GET
// {index}/{prefix}/{name} (https://index.crates.io), a newline-delimited JSON
// file with one line per published version. The index is a static, CDN-served
// file with an ETag, so a poll that finds nothing new is one cheap 304. A yank
// or an un-yank rewrites the line of the version, which changes the ETag, so
// yank transitions are detected like new releases.
//
// The index carries no timestamps. When (and only when) the index document has
// changed, List makes one more request, GET {api}/api/v1/crates/{name}
// (https://crates.io), to read each version's created_at. A 304 on the index
// therefore costs no API request, which keeps the load on crates.io at one API
// call per change. If that call fails, List fails and the fetch state is not
// advanced, so the next poll retries both. A version that the index has but
// the API does not know yet (the two are not updated atomically) is returned
// with a zero PublishedAt.
//
// # Crawler policy
//
// crates.io asks crawlers to send a User-Agent that identifies them with a
// way to get in touch, and to make at most one request per second to
// crates.io. netguard's default User-Agent is
// "angelia/<version> (+https://github.com/angelia-dev/angelia)", which
// satisfies the first rule; operators who run Angelia at scale should set
// netguard.Config.UserAgent to something with their own contact. The second
// is netguard.DefaultHostLimits: crates.io (the API) is limited to 1 request
// per second. index.crates.io is the CDN meant for bulk reads and is not
// covered by the crawler policy; it uses netguard's fallback limit.
//
// # Errors and ordering
//
// A crate that does not exist is a 404 from the index, returned as an error
// wrapping *netguard.StatusError, never as an empty result. Versions are
// sorted newest first by PublishedAt; versions without one follow, in
// reverse index order (the index is append-only, oldest first).
//
// # What a Version carries
//
// Raw is the "vers" of the index line. Yanked is its "yanked" flag.
// PublishedAt is the API's created_at. Prerelease is left nil: crates.io has
// no such flag and the core derives it from the version. Metadata has
// "checksum" (the index's cksum), and "rust_version" and "license" when
// known.
package crates

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	Kind = "crates"
	// DefaultIndexURL is the public sparse index.
	DefaultIndexURL = "https://index.crates.io"
	// DefaultAPIURL is the public crates.io API.
	DefaultAPIURL = "https://crates.io"
	// DefaultPageURL is the prefix of crate pages on crates.io.
	DefaultPageURL = "https://crates.io/crates"

	maxNameLen = 64
)

// nameRE is crates.io's rule for crate names: ASCII letters, digits, "-" and
// "_", starting with a letter.
var nameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Option configures a Source.
type Option func(*Source)

// WithAPIURL sets the base URL of the crates.io API, used for created_at.
// The default is DefaultAPIURL when the index is the public one, and
// otherwise the index URL itself, which suits a mirror serving both.
func WithAPIURL(u string) Option { return func(s *Source) { s.api = strings.TrimRight(u, "/") } }

// WithPageURL sets the prefix of the release page URL, to which
// "/{name}/{version}" is appended. Without it, URL is set only for the public
// registry.
func WithPageURL(prefix string) Option {
	return func(s *Source) { s.pageBase = strings.TrimRight(prefix, "/") }
}

// Source implements source.Source for crates.io.
type Source struct {
	index    string
	api      string
	hc       *http.Client
	pageBase string
}

var _ source.Source = (*Source)(nil)

// New returns a crates Source reading the sparse index at indexURL
// (DefaultIndexURL if empty) with hc, normally the HTTPClient of a netguard
// client.
func New(indexURL string, hc *http.Client, opts ...Option) *Source {
	if indexURL == "" {
		indexURL = DefaultIndexURL
	}
	s := &Source{index: strings.TrimRight(indexURL, "/"), hc: hc}
	if s.index == DefaultIndexURL {
		s.api, s.pageBase = DefaultAPIURL, DefaultPageURL
	} else {
		s.api = s.index
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// Validate implements source.Source. The only coordinate is "name", the crate
// name. It does no I/O.
func (*Source) Validate(t source.Target) error {
	if t.Kind != Kind {
		return fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	_, err := crateName(t)
	return err
}

func crateName(t source.Target) (string, error) {
	// No trimming: the stored coordinate must equal what is validated, or
	// watches of the same crate would not coalesce.
	name := t.Coordinates["name"]
	switch {
	case name == "":
		return "", fmt.Errorf("%w: crates target needs a name", source.ErrInvalidTarget)
	case len(name) > maxNameLen:
		return "", fmt.Errorf("%w: crate name longer than %d characters", source.ErrInvalidTarget, maxNameLen)
	case !nameRE.MatchString(name):
		return "", fmt.Errorf("%w: invalid crate name %q", source.ErrInvalidTarget, name)
	}
	return name, nil
}

// indexPath is the path of a crate in the sparse index: 1/{n}, 2/{nn},
// 3/{n}/{nnn} and {nn}/{nn}/{name}, on the lower-cased name.
func indexPath(name string) string {
	n := strings.ToLower(name)
	switch len(n) {
	case 1, 2:
		return fmt.Sprintf("%d/%s", len(n), n)
	case 3:
		return "3/" + n[:1] + "/" + n
	default:
		return n[:2] + "/" + n[2:4] + "/" + n
	}
}

// indexLine is one line of the sparse index. Dependencies and features are
// not read.
type indexLine struct {
	Vers        string  `json:"vers"`
	Cksum       string  `json:"cksum"`
	Yanked      bool    `json:"yanked"`
	RustVersion *string `json:"rust_version"`
}

type apiCrate struct {
	Versions []struct {
		Num       string  `json:"num"`
		CreatedAt string  `json:"created_at"`
		License   *string `json:"license"`
	} `json:"versions"`
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	name, err := crateName(t)
	if err != nil {
		return source.Result{}, err
	}
	if s.hc == nil {
		return source.Result{}, errors.New("crates: source has no HTTP client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.index+"/"+indexPath(name), nil)
	if err != nil {
		return source.Result{}, fmt.Errorf("crates: build request for %s: %w", name, err)
	}
	res, err := netguard.Fetch(ctx, s.hc, req, netguard.Validators{ETag: st.ETag, LastModified: st.LastModified})
	if err != nil {
		return source.Result{}, fmt.Errorf("crates: fetch index of %s: %w", name, err)
	}
	state := source.FetchState{ETag: res.Validators.ETag, LastModified: res.Validators.LastModified, Cursor: st.Cursor}
	if res.NotModified {
		return source.Result{State: state, NotModified: true}, nil
	}
	lines, err := parseIndex(res.Body)
	if err != nil {
		return source.Result{}, fmt.Errorf("crates: decode index of %s: %w", name, err)
	}
	created, licenses, err := s.apiInfo(ctx, name)
	if err != nil {
		return source.Result{}, err
	}
	return source.Result{Versions: s.versions(name, lines, created, licenses), State: state}, nil
}

// parseIndex reads the newline-delimited JSON of the sparse index, skipping
// blank lines.
func parseIndex(body []byte) ([]indexLine, error) {
	var out []indexLine
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(nil, 1<<20)
	for n := 1; sc.Scan(); n++ {
		b := bytes.TrimSpace(sc.Bytes())
		if len(b) == 0 {
			continue
		}
		var l indexLine
		if err := json.Unmarshal(b, &l); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if l.Vers == "" {
			return nil, fmt.Errorf("line %d: no vers", n)
		}
		out = append(out, l)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// apiInfo reads created_at (and the license) of every version from the API.
func (s *Source) apiInfo(ctx context.Context, name string) (created map[string]time.Time, licenses map[string]string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.api+"/api/v1/crates/"+url.PathEscape(name), nil)
	if err != nil {
		return nil, nil, fmt.Errorf("crates: build API request for %s: %w", name, err)
	}
	req.Header.Set("Accept", "application/json")
	res, err := netguard.Fetch(ctx, s.hc, req, netguard.Validators{})
	if err != nil {
		return nil, nil, fmt.Errorf("crates: fetch API record of %s: %w", name, err)
	}
	var doc apiCrate
	if err := json.Unmarshal(res.Body, &doc); err != nil {
		return nil, nil, fmt.Errorf("crates: decode API record of %s: %w", name, err)
	}
	created = make(map[string]time.Time, len(doc.Versions))
	licenses = map[string]string{}
	for _, v := range doc.Versions {
		if ts, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
			created[v.Num] = ts.UTC()
		}
		if v.License != nil && *v.License != "" {
			licenses[v.Num] = *v.License
		}
	}
	return created, licenses, nil
}

func (s *Source) versions(name string, lines []indexLine, created map[string]time.Time, licenses map[string]string) []source.Version {
	out := make([]source.Version, 0, len(lines))
	for _, l := range slices.Backward(lines) { // newest first for versions without a time
		v := source.Version{Raw: l.Vers, Yanked: l.Yanked, PublishedAt: created[l.Vers], Metadata: map[string]any{}}
		if s.pageBase != "" {
			v.URL = s.pageBase + "/" + name + "/" + url.PathEscape(l.Vers)
		}
		if l.Cksum != "" {
			v.Metadata["checksum"] = l.Cksum
		}
		if l.RustVersion != nil && *l.RustVersion != "" {
			v.Metadata["rust_version"] = *l.RustVersion
		}
		if lic := licenses[l.Vers]; lic != "" {
			v.Metadata["license"] = lic
		}
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(a, b source.Version) int { return b.PublishedAt.Compare(a.PublishedAt) })
	return out
}
