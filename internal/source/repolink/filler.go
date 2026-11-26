package repolink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/source/internal/ghapi"
)

// Results of one enrichment, the values of the result label of
// angelia_notes_enrichments_total (ADR 0045).
const (
	ResultMatched      = "matched"       // at least one new version matched a release tag
	ResultNoMatch      = "no_match"      // the releases request worked, no tag matched
	ResultNoRepository = "no_repository" // no declared repository, or GitHub does not know it
	ResultNotGitHub    = "not_github"    // the declared repository is not on github.com
	ResultPrivate      = "private"       // the repository is private: never read with a token
	ResultRateLimited  = "rate_limited"  // the host's rate limit would pass the step's deadline
	ResultError        = "error"         // a failed request, a bad response, a timeout
)

// Results lists every result, for metrics that start each pair at 0.
var Results = []string{ResultMatched, ResultNoMatch, ResultNoRepository, ResultNotGitHub, ResultPrivate, ResultRateLimited, ResultError}

// Kinds lists the source kinds the Filler enriches.
var Kinds = []string{"pypi", "npm"}

// Recorder counts enrichments by source kind and result.
type Recorder interface {
	NotesEnrichment(kind, result string)
}

const (
	// ReleasesPerRequest is the page size of the one releases request of an
	// enrichment (D81, D82: only the newest releases are searched).
	ReleasesPerRequest = 30
	// DefaultTimeout bounds a whole enrichment, so that the rate limiter
	// refuses a request instead of making the check wait (D81).
	DefaultTimeout = 15 * time.Second
	// PublicCheckTTL is how long the answer to "is this repository public?"
	// is remembered (D84).
	PublicCheckTTL = 24 * time.Hour
	// PublicCheckEntries bounds that memory (D84).
	PublicCheckEntries = 4096
)

// Option configures a Filler.
type Option func(*Filler)

// WithToken sets the API token sent to the GitHub API, which also turns on the
// public-repository check. An empty token means anonymous.
func WithToken(token string) Option {
	return func(f *Filler) {
		if token != "" {
			f.tok = apiclient.FixedToken(token)
			f.authed = true
		}
	}
}

// WithAuthenticated says that the HTTP client adds a credential of its own
// (a stored secret, sources.github-release.auth), so the public-repository
// check applies although the Filler holds no token.
func WithAuthenticated(b bool) Option {
	return func(f *Filler) { f.authed = f.authed || b }
}

// WithLogger sets the logger; the default discards.
func WithLogger(l *slog.Logger) Option {
	return func(f *Filler) {
		if l != nil {
			f.log = l
		}
	}
}

// WithRecorder counts every enrichment.
func WithRecorder(r Recorder) Option {
	return func(f *Filler) { f.rec = r }
}

// WithClock replaces time.Now, for the public-check cache.
func WithClock(now func() time.Time) Option {
	return func(f *Filler) {
		if now != nil {
			f.now = now
		}
	}
}

// WithTimeout replaces DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(f *Filler) {
		if d > 0 {
			f.timeout = d
		}
	}
}

// Filler fills the release notes and the release page of new PyPI and npm
// versions from the GitHub release of the repository the package declares
// (ADR 0045). It sends one releases request per call, and with a token one
// more to check that the repository is public. Every failure leaves the
// versions as they were. It is safe for concurrent use.
type Filler struct {
	api     *ghapi.Client
	tok     apiclient.Token
	authed  bool
	log     *slog.Logger
	rec     Recorder
	now     func() time.Time
	timeout time.Duration

	mu     sync.Mutex
	public map[string]publicEntry
}

type publicEntry struct {
	public bool
	at     time.Time
}

// NewFiller returns a Filler that sends its requests to the GitHub API at
// baseURL (ghapi.DefaultBaseURL when empty) with hc, normally the netguard
// client. The owner and repository of every request come from Parse, never
// from a URL of package metadata.
func NewFiller(baseURL string, hc *http.Client, opts ...Option) *Filler {
	f := &Filler{log: slog.New(slog.DiscardHandler), now: time.Now, timeout: DefaultTimeout, public: map[string]publicEntry{}}
	for _, o := range opts {
		o(f)
	}
	f.api = ghapi.New("notes", baseURL, hc, f.tok)
	return f
}

// Wants reports whether Fill enriches versions of a source kind (pypi and
// npm; the Checker passes the base kind of a named instance).
func (f *Filler) Wants(kind string) bool { return slices.Contains(Kinds, kind) }

// Fill returns versions with Notes and NotesURL set from the matching GitHub
// release, for the versions of t's kind that have none yet. Other kinds, and
// versions that already carry Notes, are returned unchanged. The versions
// should be the new ones of a check, newest first. It never fails.
func (f *Filler) Fill(ctx context.Context, t source.Target, versions []source.Version) []source.Version {
	if !f.Wants(t.Kind) {
		return versions
	}
	var want []int
	for i, v := range versions {
		if v.Notes == nil && !v.Yanked {
			want = append(want, i)
		}
	}
	if len(want) == 0 {
		return versions
	}
	out := slices.Clone(versions)
	result := f.fill(ctx, t, out, want)
	if f.rec != nil {
		f.rec.NotesEnrichment(t.Kind, result)
	}
	return out
}

func (f *Filler) fill(ctx context.Context, t source.Target, out []source.Version, want []int) string {
	log := f.log.With("kind", t.Kind)
	if err := f.api.Err(); err != nil {
		log.WarnContext(ctx, "release notes: GitHub client not usable", "error", err)
		return ResultError
	}
	raw, _ := out[want[0]].Metadata["repository_url"].(string)
	repo, ok := Parse(raw)
	if !ok {
		return ResultNoRepository
	}
	if repo.Host != GitHub {
		return ResultNotGitHub
	}
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	log = log.With("repository", repo.Owner+"/"+repo.Name)

	if f.authed {
		public, err := f.isPublic(ctx, repo)
		if err != nil {
			return f.failure(ctx, log, "repository check", err)
		}
		if !public {
			log.DebugContext(ctx, "release notes: repository is private, not read")
			return ResultPrivate
		}
	}
	releases, err := f.releases(ctx, repo)
	if err != nil {
		return f.failure(ctx, log, "releases request", err)
	}
	byTag := make(map[string]release, len(releases))
	for _, r := range releases {
		if r.Draft || r.TagName == "" {
			continue
		}
		if _, dup := byTag[r.TagName]; !dup {
			byTag[r.TagName] = r
		}
	}
	names := packageNames(t, out[want[0]])
	matched := false
	for _, i := range want {
		for _, tag := range TagCandidates(out[i].Raw, names) {
			r, ok := byTag[tag]
			if !ok {
				continue
			}
			matched = true
			out[i].Notes = source.NewNotes("markdown", r.Body)
			if out[i].Notes != nil && httpsURL(r.HTMLURL) {
				out[i].NotesURL = r.HTMLURL // the full text is on the release page
			}
			break
		}
	}
	if matched {
		return ResultMatched
	}
	return ResultNoMatch
}

// failure classifies err, logs it (it never holds the token: the token is
// only in a request header) and returns the result.
func (f *Filler) failure(ctx context.Context, log *slog.Logger, what string, err error) string {
	var se *netguard.StatusError
	switch {
	case errors.As(err, &se) && se.StatusCode == http.StatusNotFound:
		log.DebugContext(ctx, "release notes: repository not found or not visible", "step", what)
		return ResultNoRepository
	case errors.As(err, &se) && (se.StatusCode == http.StatusTooManyRequests || (se.StatusCode == http.StatusForbidden && !se.RetryAt.IsZero())):
		log.InfoContext(ctx, "release notes: GitHub rate limit", "step", what, "retry_at", se.RetryAt)
		return ResultRateLimited
	case errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		// The step's deadline has not passed, so the limiter refused the
		// request because its slot would (netguard.limiter.wait).
		log.InfoContext(ctx, "release notes: skipped, the api.github.com rate limit would delay the check", "step", what)
		return ResultRateLimited
	}
	log.WarnContext(ctx, "release notes: not filled", "step", what, "error", err)
	return ResultError
}

type release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
	Body    string `json:"body"`
}

// UnmarshalJSON accepts a null body.
func (r *release) UnmarshalJSON(b []byte) error {
	var raw struct {
		TagName string  `json:"tag_name"`
		HTMLURL string  `json:"html_url"`
		Draft   bool    `json:"draft"`
		Body    *string `json:"body"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	r.TagName, r.HTMLURL, r.Draft = raw.TagName, raw.HTMLURL, raw.Draft
	if raw.Body != nil {
		r.Body = *raw.Body
	}
	return nil
}

func (f *Filler) token() (apiclient.Secret, error) {
	if !f.tok.Set() {
		return "", nil
	}
	return f.tok.Get()
}

func (f *Filler) releases(ctx context.Context, r Repo) ([]release, error) {
	tok, err := f.token()
	if err != nil {
		return nil, err
	}
	u := fmt.Sprintf("%s/repos/%s/%s/releases?per_page=%d", f.api.Base(), url.PathEscape(r.Owner), url.PathEscape(r.Name), ReleasesPerRequest)
	res, err := f.api.Get(ctx, u, netguard.Validators{}, tok, "repository not found")
	if err != nil {
		return nil, err
	}
	var rs []release
	if err := json.Unmarshal(res.Body, &rs); err != nil {
		return nil, fmt.Errorf("decode releases: %w", err)
	}
	return rs, nil
}

// isPublic answers "is the repository public?" from memory or one request
// (GET /repos/{owner}/{repo}), and remembers the answer for PublicCheckTTL.
func (f *Filler) isPublic(ctx context.Context, r Repo) (bool, error) {
	key := strings.ToLower(r.Owner + "/" + r.Name)
	now := f.now()
	f.mu.Lock()
	e, ok := f.public[key]
	f.mu.Unlock()
	if ok && now.Sub(e.at) < PublicCheckTTL {
		return e.public, nil
	}
	tok, err := f.token()
	if err != nil {
		return false, err
	}
	u := fmt.Sprintf("%s/repos/%s/%s", f.api.Base(), url.PathEscape(r.Owner), url.PathEscape(r.Name))
	res, err := f.api.Get(ctx, u, netguard.Validators{}, tok, "repository not found")
	if err != nil {
		return false, err
	}
	var doc struct {
		Private *bool `json:"private"`
	}
	if err := json.Unmarshal(res.Body, &doc); err != nil || doc.Private == nil {
		return false, errors.New("decode repository: no \"private\" field")
	}
	public := !*doc.Private
	f.mu.Lock()
	f.remember(key, publicEntry{public: public, at: now}, now)
	f.mu.Unlock()
	return public, nil
}

// remember stores e under key, evicting expired entries first and then the
// oldest while the cache is over PublicCheckEntries. f.mu is held.
func (f *Filler) remember(key string, e publicEntry, now time.Time) {
	if _, ok := f.public[key]; !ok && len(f.public) >= PublicCheckEntries {
		for k, v := range f.public {
			if now.Sub(v.at) >= PublicCheckTTL {
				delete(f.public, k)
			}
		}
		for len(f.public) >= PublicCheckEntries {
			oldest, first := "", true
			var at time.Time
			for k, v := range f.public {
				if first || v.at.Before(at) {
					oldest, at, first = k, v.at, false
				}
			}
			delete(f.public, oldest)
		}
	}
	f.public[key] = e
}

// packageNames are the names a monorepo's tags may carry (D82): for npm the
// scoped and the bare name, for PyPI the name as the project reports it and
// its PEP 503 form.
func packageNames(t source.Target, v source.Version) []string {
	var names []string
	add := func(s string) {
		if s != "" && !slices.Contains(names, s) {
			names = append(names, s)
		}
	}
	switch t.Kind {
	case "npm":
		n := t.Coordinates["name"]
		add(n)
		if _, bare, ok := strings.Cut(n, "/"); ok && strings.HasPrefix(n, "@") {
			add(bare)
		}
	case "pypi":
		add(t.Coordinates["name"]) // the PEP 503 name
		if s, ok := v.Metadata["project_name"].(string); ok {
			add(s)
		}
		if s, ok := v.Metadata["project"].(string); ok {
			add(s)
		}
	}
	return names
}

// TagCandidates lists the tags that name version in a GitHub repository, in
// the order D82 tries them: "{v}", "v{v}", then for every package name n
// "n@{v}", "n-v{v}", "n-{v}" and "n_v{v}". A tag is matched exactly, so
// "1.2.30" never matches "1.2.3" and another package's tag never matches.
func TagCandidates(version string, names []string) []string {
	if version == "" {
		return nil
	}
	c := []string{version, "v" + version}
	for _, n := range names {
		c = append(c, n+"@"+version, n+"-v"+version, n+"-"+version, n+"_v"+version)
	}
	return c
}
