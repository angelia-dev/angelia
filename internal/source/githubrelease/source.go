package githubrelease

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/internal/ghapi"
)

// Kind is the registry key of this source.
const Kind = "github-release"

// DefaultBaseURL is the GitHub.com API. For GitHub Enterprise use
// "https://HOST/api/v3".
const DefaultBaseURL = ghapi.DefaultBaseURL

const (
	defaultPageSize = 100
	// maxPages bounds a first fetch of a repository with a huge history.
	maxPages = 10
)

// secret hides a token from fmt, slog and JSON. The alias is kept for
// secret_test.go.
type secret = ghapi.Secret

// Option configures a Source.
type Option func(*Source)

// WithToken sets a fixed API token. A blank token makes List fail.
func WithToken(token string) Option {
	return func(s *Source) { s.tok = ghapi.FixedToken(token) }
}

// WithTokenEnv reads the token from the environment variable name on every
// List call (so a rotated token is picked up). If the variable is unset,
// empty or blank, List fails rather than silently falling back to the 60
// requests per hour anonymous limit.
func WithTokenEnv(name string) Option {
	return func(s *Source) { s.tok = ghapi.EnvToken(name) }
}

// WithPageSize overrides the page size (default 100, GitHub's maximum).
func WithPageSize(n int) Option {
	return func(s *Source) {
		if n > 0 && n <= defaultPageSize {
			s.pageSize = n
		}
	}
}

// WithClock overrides the clock used for the periodic full rescan.
func WithClock(now func() time.Time) Option {
	return func(s *Source) { s.now = now }
}

// Source implements source.Source for GitHub releases. It is safe for
// concurrent use.
type Source struct {
	api      *ghapi.Client
	tok      ghapi.Token
	pageSize int
	now      func() time.Time
}

var _ source.Source = (*Source)(nil)

// New returns a Source that sends requests to baseURL (DefaultBaseURL if
// empty) with hc, normally the netguard client. hc must not be nil. An
// invalid baseURL, or a token with a plain-http non-loopback base URL, does
// not panic: List returns the error.
//
// New works on a shallow copy of hc whose CheckRedirect refuses to follow a
// redirect that would carry the token to another host or to plain http
// (net/http only compares host names, so it would keep the header across
// ports and subdomains).
func New(baseURL string, hc *http.Client, opts ...Option) *Source {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	s := &Source{
		pageSize: defaultPageSize,
		now:      time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	s.api = ghapi.New(Kind, baseURL, hc, s.tok)
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// parseRepo splits and checks the "repo" coordinate.
func parseRepo(t source.Target) (owner, name string, err error) {
	return ghapi.ParseRepo(Kind, t)
}

// Validate implements source.Source. It does no I/O.
func (*Source) Validate(t source.Target) error {
	_, _, err := parseRepo(t)
	return err
}

type apiRelease struct {
	ID          int64     `json:"id"`
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	CreatedAt   time.Time `json:"created_at"`
	PublishedAt time.Time `json:"published_at"`
}

// fullRescanEvery is how often List ignores its cursor and validators and
// walks every page (at most maxPages requests), to catch releases GitHub
// lists below a page that looks known.
const fullRescanEvery = 24 * time.Hour

// cursor is the JSON kept in FetchState.Cursor. A bare decimal number (the
// first format) is read as MaxID with no full scan recorded.
type cursor struct {
	MaxID  int64 `json:"max_id"`
	FullAt int64 `json:"full_at"` // unix seconds of the last full scan
}

func parseCursor(c string) cursor {
	var cur cursor
	if c == "" {
		return cur
	}
	if json.Unmarshal([]byte(c), &cur) != nil {
		cur = cursor{}
		if n, err := strconv.ParseInt(c, 10, 64); err == nil {
			cur.MaxID = n
		}
	}
	return cur
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	if s.api.Err() != nil {
		return source.Result{}, fmt.Errorf("github-release: %w", s.api.Err())
	}
	owner, name, err := parseRepo(t)
	if err != nil {
		return source.Result{}, err
	}
	cur := parseCursor(st.Cursor)
	now := s.now()
	// GitHub sorts by created_at, which is the date of the tagged commit, so
	// a release created later for an old tag can sit below a page that looks
	// known. Once a day, ignore the cursor and the validators and walk
	// everything.
	full := now.Unix()-cur.FullAt >= int64(fullRescanEvery/time.Second) || cur.FullAt > now.Unix()
	known := cur.MaxID
	prev := netguard.Validators{ETag: st.ETag, LastModified: st.LastModified}
	if full {
		known, prev = 0, netguard.Validators{}
	}
	repoRef := owner + "/" + name

	var (
		out   []source.Version
		seen  = map[string]bool{}
		maxID = cur.MaxID
		state source.FetchState
	)
	for page := 1; page <= maxPages; page++ {
		validators := netguard.Validators{}
		if page == 1 {
			validators = prev
		}
		res, err := s.fetchPage(ctx, owner, name, page, validators)
		if err != nil {
			return source.Result{}, fmt.Errorf("github-release %s: page %d: %w", repoRef, page, err)
		}
		if page == 1 {
			if res.NotModified {
				state = st
				state.ETag, state.LastModified = res.Validators.ETag, res.Validators.LastModified
				return source.Result{NotModified: true, State: state}, nil
			}
			// Only page 1 is conditional. If it changed, a release was added,
			// edited or deleted, which shifts the later pages too, so page 2
			// is fetched unless page 1 already ends at known releases.
			state.ETag, state.LastModified = res.Validators.ETag, res.Validators.LastModified
		}
		var rels []apiRelease
		if err := json.Unmarshal(res.Body, &rels); err != nil {
			return source.Result{}, fmt.Errorf("github-release %s: page %d: decode: %w", repoRef, page, err)
		}
		allKnown := true
		for _, r := range rels {
			if r.Draft {
				continue
			}
			if r.ID > known {
				allKnown = false
			}
			maxID = max(maxID, r.ID)
			if r.TagName == "" || seen[r.TagName] {
				continue
			}
			seen[r.TagName] = true
			out = append(out, toVersion(r))
		}
		if len(rels) < s.pageSize || (known > 0 && allKnown) {
			break
		}
	}
	next := cursor{MaxID: maxID, FullAt: cur.FullAt}
	if full {
		next.FullAt = now.Unix()
	}
	if b, err := json.Marshal(next); err == nil {
		state.Cursor = string(b)
	}
	return source.Result{Versions: out, State: state}, nil
}

func toVersion(r apiRelease) source.Version {
	pre := r.Prerelease
	published := r.PublishedAt
	if published.IsZero() {
		published = r.CreatedAt
	}
	md := map[string]any{"id": r.ID}
	if r.Name != "" {
		md["name"] = r.Name
	}
	return source.Version{
		Raw:         r.TagName,
		PublishedAt: published,
		Prerelease:  &pre,
		URL:         r.HTMLURL,
		NotesURL:    r.HTMLURL, // release notes are rendered on the release page
		Metadata:    md,
	}
}

func (s *Source) fetchPage(ctx context.Context, owner, name string, page int, v netguard.Validators) (*netguard.Result, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d&page=%d",
		s.api.Base(), url.PathEscape(owner), url.PathEscape(name), s.pageSize, page)
	tok, err := s.tok.Get()
	if err != nil {
		return nil, err
	}
	return s.api.Get(ctx, u, v, tok, "repository not found")
}
