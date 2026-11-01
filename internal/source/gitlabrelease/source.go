package gitlabrelease

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
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
)

// Kind is the registry key of this source.
const Kind = "gitlab-release"

// DefaultBaseURL is gitlab.com. For a self-managed instance use its root URL,
// "https://HOST" (or "https://HOST/prefix" if GitLab is served under a
// relative URL root). A trailing "/api/v4" (any case) is accepted and ignored.
// A base URL with a query or a fragment is invalid.
const DefaultBaseURL = "https://gitlab.com"

const (
	defaultPageSize = 100
	// maxPages bounds a scan of a project with a huge release history.
	maxPages = 10
	// fullRescanEvery is how often List ignores the validators of page 1.
	fullRescanEvery = 24 * time.Hour
	// upcomingGrace delays the rescan that reports an upcoming release, so
	// that GitLab has flipped upcoming_release by the time it is made.
	upcomingGrace = time.Minute
	apiSuffix     = "/api/v4"
)

var (
	// segmentRe is one path segment of a project path (a group, a subgroup or
	// the project itself).
	segmentRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,254}$`)
	idRe      = regexp.MustCompile(`^[0-9]{1,18}$`)
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
// empty or blank, List fails rather than silently falling back to anonymous
// access, which cannot see private projects.
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

// WithBearerAuth sends the token as "Authorization: Bearer" (OAuth 2 access
// tokens) instead of the default "PRIVATE-TOKEN" header (personal, project and
// group access tokens). GitLab accepts both forms for access tokens.
func WithBearerAuth() Option {
	return func(s *Source) { s.bearer = true }
}

// WithPageSize overrides the page size (default 100, GitLab's maximum).
func WithPageSize(n int) Option {
	return func(s *Source) {
		if n > 0 && n <= defaultPageSize {
			s.pageSize = n
		}
	}
}

// WithClock overrides the clock used for the periodic full rescan and for the
// wake-up of upcoming releases.
func WithClock(now func() time.Time) Option {
	return func(s *Source) { s.now = now }
}

// Source implements source.Source for GitLab releases. It is safe for
// concurrent use.
type Source struct {
	base     string // instance root, without "/api/v4"
	hc       *http.Client
	token    func() (secret, error)
	tokenSet bool
	bearer   bool
	pageSize int
	now      func() time.Time
	cfgErr   error // invalid configuration, reported by List
}

var _ source.Source = (*Source)(nil)

var errBasePlain = errors.New("refusing to send the token over plain http to a non-loopback host")

// New returns a Source that sends requests to the GitLab instance at baseURL
// (DefaultBaseURL if empty) with hc, normally the netguard client. hc must not
// be nil. An invalid baseURL, or a token with a plain-http non-loopback base
// URL, does not panic: List returns the error.
//
// New works on a shallow copy of hc whose CheckRedirect refuses to follow a
// redirect that would carry the token to another host or to plain http
// (net/http only compares host names, and forwards a custom header such as
// PRIVATE-TOKEN to any host, so it would keep the token across ports and
// subdomains).
func New(baseURL string, hc *http.Client, opts ...Option) *Source {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	s := &Source{
		base:     trimAPISuffix(strings.TrimRight(baseURL, "/")),
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
		// The credentials are those of the original request: net/http drops
		// Authorization on a cross-domain hop but keeps PRIVATE-TOKEN, so
		// req.Header says nothing about what the first request carried.
		if h := via[0].Header; h.Get("PRIVATE-TOKEN") != "" || h.Get("Authorization") != "" {
			if !tokenSafe(req.URL) || !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
				return fmt.Errorf("gitlab-release: refusing to follow a redirect to %s with credentials", req.URL.Host)
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
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(baseURL, "?#"):
		s.cfgErr = errors.New("invalid base URL: want an absolute http(s) URL without credentials, query or fragment")
	case s.tokenSet && !tokenSafe(u):
		s.cfgErr = errBasePlain
	}
	return s
}

// trimAPISuffix removes a trailing "/api/v4", whatever its case.
func trimAPISuffix(base string) string {
	if n := len(base) - len(apiSuffix); n >= 0 && strings.EqualFold(base[n:], apiSuffix) {
		return base[:n]
	}
	return base
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// parseProject checks the "project" coordinate: a full path such as
// "group/subgroup/project", or a numeric project id.
func parseProject(t source.Target) (string, error) {
	if t.Kind != Kind {
		return "", fmt.Errorf("%w: kind %q, want %q", source.ErrInvalidTarget, t.Kind, Kind)
	}
	p := t.Coordinates["project"]
	bad := fmt.Errorf("%w: coordinate project must be a path like group/project (subgroups allowed) or a numeric id, got %q", source.ErrInvalidTarget, p)
	if idRe.MatchString(p) {
		return p, nil
	}
	segs := strings.Split(p, "/")
	if len(segs) < 2 || len(segs) > 20 || len(p) > 1024 {
		return "", bad
	}
	for _, seg := range segs {
		if !segmentRe.MatchString(seg) || seg == ".." {
			return "", bad
		}
	}
	return p, nil
}

// Validate implements source.Source. It does no I/O.
func (*Source) Validate(t source.Target) error {
	_, err := parseProject(t)
	return err
}

type apiRelease struct {
	TagName         string     `json:"tag_name"`
	Name            string     `json:"name"`
	CreatedAt       time.Time  `json:"created_at"`
	ReleasedAt      *time.Time `json:"released_at"`
	UpcomingRelease bool       `json:"upcoming_release"`
	Commit          struct {
		ID string `json:"id"`
	} `json:"commit"`
	Links struct {
		Self string `json:"self"`
	} `json:"_links"`
}

// cursor is the JSON kept in FetchState.Cursor.
type cursor struct {
	FullAt int64 `json:"full_at"` // unix seconds of the last validator-free scan
	// MaxCreatedMs is the newest created_at (unix milliseconds) of a released
	// (not upcoming) release seen so far. Pages are ordered by created_at, so a
	// page whose entries are all at or below it holds nothing new.
	MaxCreatedMs int64 `json:"max_created_at,omitempty"`
	// UpcomingAt is the earliest released_at (unix seconds) of an upcoming
	// release seen by the last scan, or 0. Once it has passed, the release is
	// no longer upcoming but the ETag may not have changed, so List rescans.
	UpcomingAt int64 `json:"upcoming_at,omitempty"`
}

func parseCursor(c string) cursor {
	var cur cursor
	if c == "" || json.Unmarshal([]byte(c), &cur) != nil {
		return cursor{}
	}
	return cur
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	if s.cfgErr != nil {
		return source.Result{}, fmt.Errorf("gitlab-release: %w", s.cfgErr)
	}
	project, err := parseProject(t)
	if err != nil {
		return source.Result{}, err
	}
	tok, err := s.token()
	if err != nil {
		return source.Result{}, fmt.Errorf("gitlab-release %s: %w", project, err)
	}
	cur := parseCursor(st.Cursor)
	now := s.now()
	// GitLab does not say when an upcoming release turns into a released one,
	// and the ETag does not change with the clock. Once a day, and as soon as
	// the earliest upcoming release is due, ignore the validators.
	full := now.Unix()-cur.FullAt >= int64(fullRescanEvery/time.Second) || cur.FullAt > now.Unix() ||
		(cur.UpcomingAt > 0 && now.Unix() >= cur.UpcomingAt+int64(upcomingGrace/time.Second))
	prev := netguard.Validators{ETag: st.ETag, LastModified: st.LastModified}
	if full {
		prev = netguard.Validators{}
	}

	var (
		out      []source.Version
		seen     = map[string]bool{}
		state    source.FetchState
		upcoming int64
		maxMs    = cur.MaxCreatedMs
	)
	known := cur.MaxCreatedMs
	if full {
		known = -1 // a full scan walks every page
	}
	if !full {
		// Still pending: the scan may stop before the page that lists it.
		upcoming = cur.UpcomingAt
	}
	for page := 1; page <= maxPages; page++ {
		validators := netguard.Validators{}
		if page == 1 {
			validators = prev
		}
		res, err := s.get(ctx, s.releasesURL(project, page), validators, tok)
		if err != nil {
			return source.Result{}, fmt.Errorf("gitlab-release %s: page %d: %w", project, page, err)
		}
		if page == 1 {
			if res.NotModified {
				state = st
				state.ETag, state.LastModified = res.Validators.ETag, res.Validators.LastModified
				return source.Result{NotModified: true, State: state}, nil
			}
			state.ETag, state.LastModified = res.Validators.ETag, res.Validators.LastModified
		}
		var rels []apiRelease
		if err := json.Unmarshal(res.Body, &rels); err != nil {
			return source.Result{}, fmt.Errorf("gitlab-release %s: page %d: decode: %w", project, page, err)
		}
		anyKnown := false
		for _, r := range rels {
			if !r.UpcomingRelease {
				ms := r.CreatedAt.UnixMilli()
				if ms <= known {
					anyKnown = true // the order is created_at desc: all that follows is known
				}
				maxMs = max(maxMs, ms)
			}
			if r.TagName == "" || seen[r.TagName] {
				continue
			}
			seen[r.TagName] = true
			if r.UpcomingRelease {
				// Not released yet: skipped until released_at passes.
				if r.ReleasedAt != nil && (upcoming == 0 || r.ReleasedAt.Unix() < upcoming) {
					upcoming = r.ReleasedAt.Unix()
				}
				continue
			}
			out = append(out, s.toVersion(project, r))
		}
		// X-Next-Page is empty on the last page. Without the header (stripped by
		// a proxy) fall back to a short page. An early stop needs a released
		// entry already seen; upcoming releases say nothing about order.
		if np, ok := res.Header["X-Next-Page"]; (ok && (len(np) == 0 || np[0] == "")) || (!ok && len(rels) < s.pageSize) ||
			anyKnown {
			break
		}
	}
	next := cursor{FullAt: cur.FullAt, UpcomingAt: upcoming, MaxCreatedMs: maxMs}
	if full {
		next.FullAt = now.Unix()
	}
	if b, err := json.Marshal(next); err == nil {
		state.Cursor = string(b)
	}
	return source.Result{Versions: out, State: state}, nil
}

func (s *Source) toVersion(project string, r apiRelease) source.Version {
	published := r.CreatedAt
	if r.ReleasedAt != nil && !r.ReleasedAt.IsZero() {
		published = *r.ReleasedAt
	}
	// released_at is set by the releaser (it may be in the past or the future):
	// created_at is when GitLab recorded the release, so it is kept too.
	md := map[string]any{}
	if !r.CreatedAt.IsZero() {
		md["created_at"] = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	if r.Name != "" {
		md["name"] = r.Name
	}
	if r.Commit.ID != "" {
		md["sha"] = r.Commit.ID
	}
	return source.Version{
		Raw:         r.TagName,
		PublishedAt: published,
		// Prerelease is nil: GitLab has no prerelease flag; policy decides.
		URL:      s.releaseURL(project, r),
		NotesURL: s.releaseURL(project, r), // release notes are rendered on the release page
		Metadata: md,
	}
}

// releaseURL is the web page of a release: the "self" link when it is an
// http(s) URL, else built from the base URL (possible for a path project).
func (s *Source) releaseURL(project string, r apiRelease) string {
	if u, err := url.Parse(r.Links.Self); err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
		return r.Links.Self
	}
	if idRe.MatchString(project) {
		return ""
	}
	segs := strings.Split(project, "/")
	for i, p := range segs {
		segs[i] = url.PathEscape(p)
	}
	return s.base + "/" + strings.Join(segs, "/") + "/-/releases/" + url.PathEscape(r.TagName)
}

// releasesURL URL-encodes the project path as one segment ("a/b" becomes
// "a%2Fb"), as the GitLab API requires.
func (s *Source) releasesURL(project string, page int) string {
	return fmt.Sprintf("%s%s/projects/%s/releases?order_by=created_at&sort=desc&per_page=%d&page=%d",
		s.base, apiSuffix, url.PathEscape(project), s.pageSize, page)
}

func (s *Source) get(ctx context.Context, u string, v netguard.Validators, tok secret) (*netguard.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if tok != "" {
		if !tokenSafe(req.URL) {
			return nil, errBasePlain
		}
		if s.bearer {
			req.Header.Set("Authorization", "Bearer "+string(tok))
		} else {
			req.Header.Set("PRIVATE-TOKEN", string(tok))
		}
	}
	res, err := netguard.Fetch(ctx, s.hc, req, v)
	if err != nil {
		var se *netguard.StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("project not found (or not visible to the token): %w", err)
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
