package githubtag

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/angelia-dev/angelia/internal/source/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/source/internal/ghapi"
)

// Kind is the registry key of this source.
const Kind = "github-tag"

// DefaultBaseURL is the GitHub.com API. For GitHub Enterprise use
// "https://HOST/api/v3".
const DefaultBaseURL = ghapi.DefaultBaseURL

const (
	defaultPageSize = 100
	// maxPages bounds a scan of a repository with a huge number of tags.
	maxPages = 10
	// maxLookups bounds the commit-date requests of one List call (many new
	// tags at once). Tags past the bound are returned without a date and are
	// not looked up again.
	maxLookups = 50
	// fullRescanEvery is how often List ignores the validators of page 1.
	fullRescanEvery = 24 * time.Hour
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$|^[0-9a-f]{64}$`)

// secret hides a token from fmt, slog and JSON. The alias is kept for
// secret_test.go.
type secret = apiclient.Secret

// Option configures a Source.
type Option func(*Source)

// WithToken sets a fixed API token. A blank token makes List fail.
func WithToken(token string) Option {
	return func(s *Source) { s.tok = apiclient.FixedToken(token) }
}

// WithTokenEnv reads the token from the environment variable name on every
// List call (so a rotated token is picked up). If the variable is unset,
// empty or blank, List fails rather than silently falling back to the 60
// requests per hour anonymous limit.
func WithTokenEnv(name string) Option {
	return func(s *Source) { s.tok = apiclient.EnvToken(name) }
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

// Source implements source.Source for GitHub tags. It is safe for concurrent
// use.
type Source struct {
	api      *ghapi.Client
	tok      apiclient.Token
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

type apiTag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
	TarballURL string `json:"tarball_url"`
}

type apiCommit struct {
	Committer struct {
		Date time.Time `json:"date"`
	} `json:"committer"`
	Author struct {
		Date time.Time `json:"date"`
	} `json:"author"`
}

// cursor is the JSON kept in FetchState.Cursor: the names of the tags already
// reported (so only new ones cost a date lookup) and the time of the last
// scan that ignored the validators.
type cursor struct {
	Tags   []string `json:"tags"`
	FullAt int64    `json:"full_at"`
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
	if s.api.Err() != nil {
		return source.Result{}, fmt.Errorf("github-tag: %w", s.api.Err())
	}
	owner, name, err := parseRepo(t)
	if err != nil {
		return source.Result{}, err
	}
	repoRef := owner + "/" + name
	cur, haveCursor := parseCursor(st.Cursor)
	tok, err := s.tok.Get()
	if err != nil {
		return source.Result{}, fmt.Errorf("github-tag %s: %w", repoRef, err)
	}
	now := s.now()
	// The tags endpoint has no creation order, so a new tag can sit on a
	// later page than the one the ETag covers. Once a day, ignore the
	// validators.
	full := now.Unix()-cur.FullAt >= int64(fullRescanEvery/time.Second) || cur.FullAt > now.Unix()
	prev := netguard.Validators{ETag: st.ETag, LastModified: st.LastModified}
	if full {
		prev = netguard.Validators{}
	}

	var (
		tags  []apiTag
		seen  = map[string]bool{}
		state source.FetchState
	)
	for page := 1; page <= maxPages; page++ {
		validators := netguard.Validators{}
		if page == 1 {
			validators = prev
		}
		res, err := s.get(ctx, s.tagsURL(owner, name, page), validators, tok)
		if err != nil {
			return source.Result{}, fmt.Errorf("github-tag %s: page %d: %w", repoRef, page, err)
		}
		if page == 1 {
			if res.NotModified {
				state = st
				state.ETag, state.LastModified = res.Validators.ETag, res.Validators.LastModified
				return source.Result{NotModified: true, State: state}, nil
			}
			state.ETag, state.LastModified = res.Validators.ETag, res.Validators.LastModified
		}
		var batch []apiTag
		if err := json.Unmarshal(res.Body, &batch); err != nil {
			return source.Result{}, fmt.Errorf("github-tag %s: page %d: decode: %w", repoRef, page, err)
		}
		for _, tg := range batch {
			if tg.Name == "" || seen[tg.Name] {
				continue
			}
			seen[tg.Name] = true
			tags = append(tags, tg)
		}
		if len(batch) < s.pageSize {
			break
		}
	}

	known := make(map[string]bool, len(cur.Tags))
	for _, n := range cur.Tags {
		known[n] = true
	}
	web := webBase(s.api.Base())
	out := make([]source.Version, 0, len(tags))
	names := make([]string, 0, len(tags))
	lookups := 0
	for _, tg := range tags {
		names = append(names, tg.Name)
		v := toVersion(web, owner, name, tg)
		// A first scan (no readable cursor) is a baseline (ADR 0010 section
		// 5): its dates are arbitrary and would burn the anonymous rate limit.
		if haveCursor && !known[tg.Name] && lookups < maxLookups && shaRe.MatchString(tg.Commit.SHA) {
			lookups++
			d, err := s.commitDate(ctx, owner, name, tg.Commit.SHA, tok)
			if err != nil {
				if ctx.Err() != nil {
					// The call was cancelled: report the cancellation, not the
					// half-read body it may have caused.
					return source.Result{}, fmt.Errorf("github-tag %s: date of %s: %w", repoRef, tg.Name, context.Cause(ctx))
				}
				if fatalLookup(err) {
					// State is not advanced: the next call retries this tag.
					return source.Result{}, fmt.Errorf("github-tag %s: date of %s: %w", repoRef, tg.Name, err)
				}
				// The tag stays undated; the error text carries no URL.
				slog.WarnContext(ctx, "github-tag: commit date lookup failed, tag left undated",
					"repo", repoRef, "tag", tg.Name)
			}
			v.PublishedAt = d
		}
		out = append(out, v)
	}
	slices.Sort(names)
	next := cursor{Tags: names, FullAt: cur.FullAt}
	if full {
		next.FullAt = now.Unix()
	}
	if b, err := json.Marshal(next); err == nil {
		state.Cursor = string(b)
	}
	return source.Result{Versions: out, State: state}, nil
}

// fatalLookup reports whether a date lookup error must fail List: a context
// error, or a real rate limit (the server named a retry time).
func fatalLookup(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var se *netguard.StatusError
	return errors.As(err, &se) && !se.RetryAt.IsZero()
}

// tagPath escapes each segment of a tag name but keeps "/" (github.com
// accepts it unescaped in /releases/tag/<name>).
func tagPath(name string) string {
	parts := strings.Split(name, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func toVersion(web, owner, repo string, tg apiTag) source.Version {
	md := map[string]any{"sha": tg.Commit.SHA}
	if tg.TarballURL != "" {
		md["tarball_url"] = tg.TarballURL
	}
	return source.Version{
		Raw: tg.Name,
		URL: web + "/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/releases/tag/" + tagPath(tg.Name),
		// Prerelease is nil: a tag has no upstream flag; policy decides.
		Metadata: md,
	}
}

// webBase derives the web UI origin from the API base URL: api.github.com maps
// to github.com, and a GitHub Enterprise "https://HOST/api/v3" to
// "https://HOST".
func webBase(api string) string {
	u, err := url.Parse(api)
	if err != nil {
		return ""
	}
	if strings.EqualFold(u.Host, "api.github.com") {
		return "https://github.com"
	}
	p := strings.TrimSuffix(strings.TrimRight(u.Path, "/"), "/api/v3")
	return u.Scheme + "://" + u.Host + p
}

func (s *Source) tagsURL(owner, name string, page int) string {
	return fmt.Sprintf("%s/repos/%s/%s/tags?per_page=%d&page=%d",
		s.api.Base(), url.PathEscape(owner), url.PathEscape(name), s.pageSize, page)
}

// commitDate returns the committer date of sha (the date the tag points at).
// A commit GitHub cannot serve (404, 409, 422) yields a zero time, not an error.
func (s *Source) commitDate(ctx context.Context, owner, name, sha string, tok secret) (time.Time, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/git/commits/%s", s.api.Base(), url.PathEscape(owner), url.PathEscape(name), sha)
	res, err := s.get(ctx, u, netguard.Validators{}, tok)
	if err != nil {
		var se *netguard.StatusError
		if errors.As(err, &se) && (se.StatusCode == http.StatusNotFound || se.StatusCode == http.StatusConflict || se.StatusCode == http.StatusUnprocessableEntity) {
			return time.Time{}, nil
		}
		return time.Time{}, err
	}
	var c apiCommit
	if err := json.Unmarshal(res.Body, &c); err != nil {
		return time.Time{}, fmt.Errorf("decode commit: %w", err)
	}
	if !c.Committer.Date.IsZero() {
		return c.Committer.Date, nil
	}
	return c.Author.Date, nil
}

func (s *Source) get(ctx context.Context, u string, v netguard.Validators, tok secret) (*netguard.Result, error) {
	return s.api.Get(ctx, u, v, tok, "not found")
}
