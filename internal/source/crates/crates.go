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
// changed, and an API is configured, List reads created_at from the paginated
// GET {api}/api/v1/crates/{name}/versions?per_page=100 (https://crates.io),
// following meta.next_page for at most maxPages pages. That endpoint is used
// rather than the crate record because the record of a very large crate is
// itself above netguard's response cap. A 304 on the index costs no API
// request. If an API call fails, List fails and the fetch state is not
// advanced, so the next poll retries both. If the API does not yet list some
// version that the index has (the two are not updated atomically), List
// returns the versions it has, those with a zero PublishedAt, but hands back
// the incoming fetch state unchanged, so the next poll refetches both and
// fills the times in.
//
// The public index uses the public crates.io API. A custom index (a mirror or
// an alternative registry) has no API by default, since a mirror rarely
// serves crates.io's API: its versions have a zero PublishedAt unless
// WithAPIURL names one.
//
// # Size cap
//
// The index file of a very large crate (swc_core: about 16 MiB) is above
// netguard's global 10 MiB response cap. netguard gives index.crates.io a
// built-in 64 MiB cap; a custom index host keeps the global cap unless the
// operator sets HostLimit.MaxBodyBytes for it. A crate over its host's cap
// fails on every poll with an error wrapping netguard.ErrBodyTooLarge, and
// the state is not advanced.
//
// # Names
//
// crates.io treats names case-insensitively but "-" and "_" as different
// characters ("serde_json" is not "serde-json"), so Normalize only lower-cases
// and the coalescing key keeps "-" and "_" apart. Alternative registries may
// allow a leading "_", which is rejected here.
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
// sorted newest first by PublishedAt; versions without one, and versions with
// equal ones, follow in reverse index order (the index is append-only, oldest
// first).
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
	"strconv"
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
	// maxPages bounds the pages read from the versions endpoint (100 each).
	maxPages = 50
	perPage  = 100
)

// nameRE is crates.io's rule for crate names: ASCII letters, digits, "-" and
// "_", starting with a letter.
var nameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)

// Option configures a Source.
type Option func(*Source)

// WithAPIURL sets the base URL of the crates.io API, used for created_at.
// The default is DefaultAPIURL when the index is the public one, and
// otherwise none: no API request is made and PublishedAt stays zero.
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

// Normalize returns the canonical form of a crate name, the one used in the
// index path and in the target coalescing key: lower case. "-" and "_" are
// not folded, they are different characters on crates.io.
func Normalize(name string) string { return strings.ToLower(name) }

func crateName(t source.Target) (string, error) {
	// No trimming: what is validated is what is stored, and the name is
	// normalized so that watches of "Serde" and "serde" coalesce.
	name := t.Coordinates["name"]
	switch {
	case name == "":
		return "", fmt.Errorf("%w: crates target needs a name", source.ErrInvalidTarget)
	case len(name) > maxNameLen:
		return "", fmt.Errorf("%w: crate name longer than %d characters", source.ErrInvalidTarget, maxNameLen)
	case !nameRE.MatchString(name):
		return "", fmt.Errorf("%w: invalid crate name %q", source.ErrInvalidTarget, name)
	}
	return Normalize(name), nil
}

// indexPath is the path of a crate in the sparse index: 1/{n}, 2/{nn},
// 3/{n}/{nnn} and {nn}/{nn}/{name}, on the lower-cased name.
func indexPath(name string) string {
	n := Normalize(name)
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

type apiPage struct {
	Versions []struct {
		Num       string  `json:"num"`
		CreatedAt string  `json:"created_at"`
		License   *string `json:"license"`
	} `json:"versions"`
	Meta struct {
		NextPage *string `json:"next_page"`
	} `json:"meta"`
}

type apiVersion struct {
	created time.Time
	license string
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
		return source.Result{}, indexError(name, err)
	}
	state := source.FetchState{ETag: res.Validators.ETag, LastModified: res.Validators.LastModified, Cursor: st.Cursor}
	if res.NotModified {
		return source.Result{State: state, NotModified: true}, nil
	}
	lines, err := parseIndex(res.Body)
	if err != nil {
		return source.Result{}, fmt.Errorf("crates: decode index of %s: %w", name, err)
	}
	info := map[string]apiVersion{}
	if s.api != "" {
		var complete bool
		if info, complete, err = s.apiInfo(ctx, name); err != nil {
			return source.Result{}, err
		}
		for _, l := range lines {
			if !complete {
				break // truncated listing: do not hold the state back forever
			}
			if info[l.Vers].created.IsZero() {
				// The API lags the index: keep the old validators so that the
				// next poll refetches the index and asks the API again.
				return source.Result{Versions: s.versions(name, lines, info), State: st}, nil
			}
		}
	}
	return source.Result{Versions: s.versions(name, lines, info), State: state}, nil
}

func indexError(name string, err error) error {
	var se *netguard.StatusError
	switch {
	case errors.As(err, &se) && se.StatusCode == http.StatusNotFound:
		return fmt.Errorf("crates: fetch index of %s (crates.io names are exact about '-' versus '_'; check the spelling): %w", name, err)
	case errors.Is(err, netguard.ErrBodyTooLarge):
		return fmt.Errorf("crates: index of %s is above the response size cap (set netguard.HostLimit.MaxBodyBytes for its host): %w", name, err)
	}
	return fmt.Errorf("crates: fetch index of %s: %w", name, err)
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

// apiInfo reads created_at (and the license) of the versions from the
// paginated versions endpoint, following meta.next_page.
func (s *Source) apiInfo(ctx context.Context, name string) (out map[string]apiVersion, complete bool, err error) {
	out = map[string]apiVersion{}
	base := s.api + "/api/v1/crates/" + url.PathEscape(name) + "/versions"
	query := "?per_page=" + strconv.Itoa(perPage)
	for range maxPages {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+query, nil)
		if err != nil {
			return nil, false, fmt.Errorf("crates: build API request for %s: %w", name, err)
		}
		req.Header.Set("Accept", "application/json")
		res, err := netguard.Fetch(ctx, s.hc, req, netguard.Validators{})
		if err != nil {
			return nil, false, fmt.Errorf("crates: fetch API versions of %s: %w", name, err)
		}
		var page apiPage
		if err := json.Unmarshal(res.Body, &page); err != nil {
			return nil, false, fmt.Errorf("crates: decode API versions of %s: %w", name, err)
		}
		for _, v := range page.Versions {
			var av apiVersion
			if ts, err := time.Parse(time.RFC3339, v.CreatedAt); err == nil {
				av.created = ts.UTC()
			}
			if v.License != nil {
				av.license = *v.License
			}
			out[v.Num] = av
		}
		next := page.Meta.NextPage
		if next == nil || *next == "" || !strings.HasPrefix(*next, "?") || len(page.Versions) == 0 {
			return out, true, nil
		}
		query = *next
	}
	return out, false, nil // more than maxPages pages: the oldest versions stay without a time
}

func (s *Source) versions(name string, lines []indexLine, info map[string]apiVersion) []source.Version {
	out := make([]source.Version, 0, len(lines))
	for _, l := range slices.Backward(lines) { // newest first for versions without a time
		v := source.Version{Raw: l.Vers, Yanked: l.Yanked, PublishedAt: info[l.Vers].created, Metadata: map[string]any{}}
		if s.pageBase != "" {
			v.URL = s.pageBase + "/" + name + "/" + url.PathEscape(l.Vers)
		}
		if l.Cksum != "" {
			v.Metadata["checksum"] = l.Cksum
		}
		if l.RustVersion != nil && *l.RustVersion != "" {
			v.Metadata["rust_version"] = *l.RustVersion
		}
		if lic := info[l.Vers].license; lic != "" {
			v.Metadata["license"] = lic
		}
		out = append(out, v)
	}
	slices.SortStableFunc(out, func(a, b source.Version) int { return b.PublishedAt.Compare(a.PublishedAt) })
	return out
}
