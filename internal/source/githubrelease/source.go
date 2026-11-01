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

// WithToken sets a fixed API token.
func WithToken(token string) Option {
	return func(s *Source) { s.token = func() secret { return secret(strings.TrimSpace(token)) } }
}

// WithTokenEnv reads the token from the environment variable name on every
// List call (so a rotated token is picked up). An unset or empty variable
// means unauthenticated requests.
func WithTokenEnv(name string) Option {
	return func(s *Source) { s.token = func() secret { return secret(strings.TrimSpace(os.Getenv(name))) } }
}

// WithPageSize overrides the page size (default 100, GitHub's maximum).
func WithPageSize(n int) Option {
	return func(s *Source) {
		if n > 0 && n <= defaultPageSize {
			s.pageSize = n
		}
	}
}

// Source implements source.Source for GitHub releases. It is safe for
// concurrent use.
type Source struct {
	base     string
	hc       *http.Client
	token    func() secret
	pageSize int
}

var _ source.Source = (*Source)(nil)

// New returns a Source that sends requests to baseURL (DefaultBaseURL if
// empty) with hc, normally the netguard client. hc must not be nil.
func New(baseURL string, hc *http.Client, opts ...Option) *Source {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	s := &Source{
		base:     strings.TrimRight(baseURL, "/"),
		hc:       hc,
		token:    func() secret { return "" },
		pageSize: defaultPageSize,
	}
	for _, o := range opts {
		o(s)
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

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	owner, name, err := parseRepo(t)
	if err != nil {
		return source.Result{}, err
	}
	var known int64
	if st.Cursor != "" {
		// A malformed cursor only costs a full fetch.
		known, _ = strconv.ParseInt(st.Cursor, 10, 64)
	}
	prev := netguard.Validators{ETag: st.ETag, LastModified: st.LastModified}
	repoRef := owner + "/" + name

	var (
		out   []source.Version
		seen  = map[string]bool{}
		maxID = known
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
	if maxID > 0 {
		state.Cursor = strconv.FormatInt(maxID, 10)
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
	if tok := s.token(); tok != "" {
		if !tokenSafe(req.URL) {
			return nil, errors.New("refusing to send the token over plain http to a non-loopback host")
		}
		req.Header.Set("Authorization", "Bearer "+string(tok))
	}
	res, err := netguard.Fetch(ctx, s.hc, req, v)
	if err != nil {
		var se *netguard.StatusError
		if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("repository not found (or not visible to the token): %w", err)
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
	return u.Hostname() == "localhost"
}
