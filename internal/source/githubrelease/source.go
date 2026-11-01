package githubrelease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

// Kind is the registry key of this source.
const Kind = "github-release"

// DefaultBaseURL is the GitHub.com API. For GitHub Enterprise use
// "https://HOST/api/v3".
const DefaultBaseURL = "https://api.github.com"

const (
	defaultPageSize = 100
	// maxPages bounds a first fetch of a repository with a huge history.
	maxPages = 10
)

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	nameRe  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// secret hides a token from fmt, slog and JSON.
type secret string

func (secret) String() string               { return "[REDACTED]" }
func (secret) GoString() string             { return "[REDACTED]" }
func (secret) LogValue() slog.Value         { return slog.StringValue("[REDACTED]") }
func (secret) MarshalText() ([]byte, error) { return []byte("[REDACTED]"), nil }

// Option configures a Source.
type Option func(*Source)

// WithToken sets a fixed API token. A blank token makes List fail.
func WithToken(token string) Option {
	return func(s *Source) {
		s.tokenSet = true
		s.token = func() (secret, error) {
			t := secret(strings.TrimSpace(token))
			if t == "" {
				return "", errors.New("token is configured but blank")
			}
			return t, nil
		}
	}
}

// WithTokenEnv reads the token from the environment variable name on every
// List call (so a rotated token is picked up). If the variable is unset,
// empty or blank, List fails rather than silently falling back to the 60
// requests per hour anonymous limit.
func WithTokenEnv(name string) Option {
	return func(s *Source) {
		s.tokenSet = true
		s.token = func() (secret, error) {
			t := secret(strings.TrimSpace(os.Getenv(name)))
			if t == "" {
				return "", fmt.Errorf("token_env %q is unset or empty", name)
			}
			return t, nil
		}
	}
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
	base     string
	hc       *http.Client
	token    func() (secret, error)
	tokenSet bool
	pageSize int
	now      func() time.Time
	cfgErr   error // invalid configuration, reported by List
}

var _ source.Source = (*Source)(nil)

var errBasePlain = errors.New("refusing to send the token over plain http to a non-loopback host")

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
		base:     strings.TrimRight(baseURL, "/"),
		token:    func() (secret, error) { return "", nil },
		pageSize: defaultPageSize,
		now:      time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	cp := *hc
	orig := cp.CheckRedirect
	cp.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.Header.Get("Authorization") != "" {
			if !tokenSafe(req.URL) || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return fmt.Errorf("github-release: refusing to follow a redirect to %s with credentials", req.URL.Host)
			}
		}
		if orig != nil {
			return orig(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	s.hc = &cp
	u, err := url.Parse(s.base)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil:
		s.cfgErr = errors.New("invalid base URL: want an absolute http(s) URL without credentials")
	case s.tokenSet && !tokenSafe(u):
		s.cfgErr = errBasePlain
	}
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// parseRepo splits and checks the "repo" coordinate.
func parseRepo(t source.Target) (owner, name string, err error) {
	if t.Kind != Kind {
		return "", "", fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	repo := t.Coordinates["repo"]
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !ownerRe.MatchString(owner) || !nameRe.MatchString(name) || name == "." || name == ".." {
		return "", "", fmt.Errorf("%w: coordinate repo must be owner/name, got %q", source.ErrInvalidTarget, repo)
	}
	return owner, name, nil
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
	if s.cfgErr != nil {
		return source.Result{}, fmt.Errorf("github-release: %w", s.cfgErr)
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
		s.base, url.PathEscape(owner), url.PathEscape(name), s.pageSize, page)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, errors.New("build request: invalid base URL")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	tok, err := s.token()
	if err != nil {
		return nil, err
	}
	if tok != "" {
		if !tokenSafe(req.URL) {
			return nil, errBasePlain
		}
		req.Header.Set("Authorization", "Bearer "+string(tok))
	}
	res, err := netguard.Fetch(ctx, s.hc, req, v)
	if err != nil {
		var se *netguard.StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("repository not found (or not visible to the token): %w", err)
		}
		if errors.As(err, &se) && se.StatusCode == http.StatusUnauthorized {
			return nil, fmt.Errorf("the token is invalid or expired: %w", err)
		}
		return nil, err
	}
	return res, nil
}

// tokenSafe reports whether credentials may be sent to u.
func tokenSafe(u *url.URL) bool {
	if u.Scheme == "https" {
		return true
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(u.Hostname(), "localhost")
}
