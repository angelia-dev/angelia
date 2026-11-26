package oci

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
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
)

// Kind is the registry key of this source.
const Kind = "oci"

const (
	pageSize = 1000
	// maxPages bounds a listing (D48): 50,000 tags. A lexically sorted list
	// cannot be truncated safely, so more pages fail the call.
	maxPages = 50
	// maxBody bounds a response the source reads (netguard caps it too).
	maxBody = 16 << 20
	// maxLookups bounds the manifest HEADs of one List call (D46). Tags past
	// the bound are returned without a digest and are not looked up again.
	maxLookups = 50
	// manifestAccept lists the manifest media types a registry may answer
	// with: OCI index and manifest, Docker manifest list and manifest v2.
	manifestAccept = "application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, " +
		"application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// referrersTagRE matches the tags cosign and the referrers tag schema create
// for signatures, attestations and SBOMs: other artifacts, not versions.
var referrersTagRE = regexp.MustCompile(`^sha256-[0-9a-f]{64}(?:\.(?:sig|att|sbom))?$`)

// Option configures a Source.
type Option func(*Source)

// WithEndpoint maps a registry host (as in the image coordinate) to the base
// URL of its API. It is meant for tests with an httptest server.
func WithEndpoint(host, baseURL string) Option {
	return func(s *Source) { s.endpoints[strings.ToLower(host)] = strings.TrimRight(baseURL, "/") }
}

// WithAllowHTTPHosts lists the hosts a token realm may be fetched from over
// plain http (http.allow_http_hosts).
func WithAllowHTTPHosts(hosts ...string) Option {
	return func(s *Source) {
		for _, h := range hosts {
			s.httpHosts[strings.ToLower(h)] = true
		}
	}
}

// Source implements source.Source for OCI registries. It is safe for
// concurrent use.
type Source struct {
	hc        *http.Client
	pinned    string // origin of base_url, "" for the unpinned built-in
	pinnedRef string // image host that origin serves ("docker.io" for Docker Hub)
	endpoints map[string]string
	httpHosts map[string]bool
}

var (
	_ source.Source        = (*Source)(nil)
	_ source.Canonicalizer = (*Source)(nil)
)

// New returns a Source sending requests with hc, normally the netguard
// client; hc must not be nil. An empty baseURL is the built-in source: it
// fetches from the registry of each image. A non-empty baseURL pins the
// source to that origin: it accepts only images served there (D41).
func New(baseURL string, hc *http.Client, opts ...Option) *Source {
	s := &Source{
		hc:        apiclient.GuardRedirects(Kind, hc),
		endpoints: map[string]string{},
		httpHosts: map[string]bool{},
	}
	if baseURL != "" {
		s.pinned = originOf(baseURL)
		if s.pinned == "" {
			s.pinned = "invalid:" + baseURL
		} else if u, err := url.Parse(baseURL); err == nil {
			s.pinnedRef = strings.ToLower(u.Host)
			if dockerHubAliases[s.pinnedRef] {
				s.pinnedRef = DockerHub
			}
		}
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Kind implements source.Source.
func (*Source) Kind() string { return Kind }

// Validate implements source.Source. It does no I/O.
func (s *Source) Validate(t source.Target) error {
	r, err := parseRef(t)
	if err != nil {
		return err
	}
	if s.pinned != "" && r.host != s.pinnedRef {
		return invalid("image %q is not served by this source's registry (%s)", r.String(), strings.TrimPrefix(s.pinned, "invalid:"))
	}
	return nil
}

// Canonical implements source.Canonicalizer (D42).
func (s *Source) Canonical(t source.Target) (source.Target, error) {
	r, err := parseRef(t)
	if err != nil {
		return source.Target{}, err
	}
	return source.Target{Kind: Kind, Coordinates: map[string]string{"image": r.String()}}, nil
}

type reply struct {
	status int
	header http.Header
	body   []byte
}

// do sends one request with hc. A non-2xx status is not an error here.
func (s *Source) do(ctx context.Context, method, u string, tok apiclient.Secret, accept string) (*reply, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+string(tok))
	}
	resp, err := s.hc.Do(req) //nolint:gosec // G704: hc is the injected netguard client.
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read body from %s: %w", req.URL.Host, err)
	}
	return &reply{status: resp.StatusCode, header: resp.Header, body: b}, nil
}

// statusError converts a non-2xx reply to the netguard error type, so that
// callers can read RetryAt.
func statusError(r *reply) error {
	se := &netguard.StatusError{StatusCode: r.status, Status: fmt.Sprintf("%d %s", r.status, http.StatusText(r.status))}
	if d := netguard.ParseRetryAfter(r.header.Get("Retry-After"), time.Now()); d > 0 {
		se.RetryAt = time.Now().Add(d)
	}
	return se
}

// session is the state of one List call: the registry token lives here only.
type session struct {
	name     string
	endpoint string
	tok      apiclient.Secret
	authed   bool // a token was fetched (a later 401 is final)
}

// get requests u on the registry, answering one 401 challenge.
func (s *Source) get(ctx context.Context, ss *session, u string) (*reply, error) {
	r, err := s.do(ctx, http.MethodGet, u, ss.tok, "application/json")
	if err != nil {
		return nil, err
	}
	if r.status == http.StatusUnauthorized {
		if ss.authed {
			return nil, errors.New("not found or access denied (HTTP 401 after a registry token)")
		}
		c, ok := pickChallenge(parseChallenges(r.header))
		switch {
		case !ok:
			return nil, errors.New("the registry answered 401 without a WWW-Authenticate challenge")
		case c.scheme == "bearer":
			tok, err := s.token(ctx, c, ss.name)
			if err != nil {
				return nil, err
			}
			ss.tok, ss.authed = tok, true
			if r, err = s.do(ctx, http.MethodGet, u, ss.tok, "application/json"); err != nil {
				return nil, err
			}
			if r.status == http.StatusUnauthorized {
				return nil, errors.New("not found or access denied (HTTP 401 after a registry token)")
			}
		case c.scheme == "basic":
			return nil, errors.New("the registry requires credentials (Basic challenge); configure sources.<name>.auth on a source with base_url")
		default:
			return nil, fmt.Errorf("the registry asks for unsupported authentication %q", c.scheme)
		}
	}
	switch {
	case r.status == http.StatusNotFound:
		return nil, errors.New("not found (HTTP 404)")
	case r.status < 200 || r.status > 299:
		return nil, statusError(r)
	}
	return r, nil
}

// List implements source.Source.
func (s *Source) List(ctx context.Context, t source.Target, st source.FetchState) (source.Result, error) {
	r, err := parseRef(t)
	if err != nil {
		return source.Result{}, err
	}
	if err := s.Validate(t); err != nil {
		return source.Result{}, err
	}
	ss := &session{name: r.path, endpoint: s.endpoint(r)}
	tags, err := s.tags(ctx, ss)
	if err != nil {
		return source.Result{}, fmt.Errorf("oci %s: %w", r.String(), err)
	}
	// A first scan (no readable cursor) is a baseline (ADR 0010 section 5):
	// no digest lookups.
	known, haveCursor := parseCursor(st.Cursor)
	out := make([]source.Version, 0, len(tags))
	lookups := 0
	for _, tg := range tags {
		v := source.Version{Raw: tg, URL: hubTagURL(r, tg)}
		if haveCursor && !known[tagHash(tg)] && lookups < maxLookups {
			lookups++
			d, err := s.digest(ctx, ss, tg)
			switch {
			case err == nil:
				if d != "" {
					v.Metadata = map[string]any{"digest": d}
				}
			case ctx.Err() != nil:
				return source.Result{}, fmt.Errorf("oci %s: digest of %s: %w", r.String(), tg, context.Cause(ctx))
			case fatalLookup(err):
				// State is not advanced: the next call retries this tag.
				return source.Result{}, fmt.Errorf("oci %s: digest of %s: %w", r.String(), tg, err)
			default:
				// The tag stays without a digest; the text carries no URL.
				slog.WarnContext(ctx, "oci: manifest digest lookup failed, tag left without a digest",
					"image", r.String(), "tag", tg, "error", err.Error())
			}
		}
		out = append(out, v)
	}
	return source.Result{Versions: out, State: source.FetchState{Cursor: encodeCursor(tags)}}, nil
}

// fatalLookup reports whether a digest lookup error must fail List: a context
// error, or a rate limit (the server named a retry time or answered 429).
func fatalLookup(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var se *netguard.StatusError
	return errors.As(err, &se) && (se.StatusCode == http.StatusTooManyRequests || !se.RetryAt.IsZero())
}

// digest reads Docker-Content-Digest of the manifest of tag with a HEAD. An
// invalid or missing header gives "" and no error.
func (s *Source) digest(ctx context.Context, ss *session, tag string) (string, error) {
	u := ss.endpoint + "/v2/" + ss.name + "/manifests/" + url.PathEscape(tag)
	r, err := s.do(ctx, http.MethodHead, u, ss.tok, manifestAccept)
	if err != nil {
		return "", err
	}
	if r.status < 200 || r.status > 299 {
		return "", statusError(r)
	}
	if d := r.header.Get("Docker-Content-Digest"); digestRE.MatchString(d) {
		return d, nil
	}
	return "", nil
}

// hubTagURL is the Docker Hub tag page of a docker.io image, "" elsewhere (D49).
func hubTagURL(r ref, tag string) string {
	if r.host != DockerHub {
		return ""
	}
	q := "/tags?name=" + url.QueryEscape(tag)
	if rest, ok := strings.CutPrefix(r.path, "library/"); ok && !strings.Contains(rest, "/") {
		return "https://hub.docker.com/_/" + rest + q
	}
	return "https://hub.docker.com/r/" + r.path + q
}

// tagHash is the 32-bit FNV-1a hash of a tag name. A collision only costs a
// digest lookup.
func tagHash(tag string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(tag))
	return h.Sum32()
}

// encodeCursor is the sorted hashes of the tags, big-endian, base64 (4 bytes
// per tag: about 50 KB for library/node).
func encodeCursor(tags []string) string {
	hs := make([]uint32, len(tags))
	for i, t := range tags {
		hs[i] = tagHash(t)
	}
	slices.Sort(hs)
	b := make([]byte, 0, 4*len(hs))
	for _, h := range hs {
		b = binary.BigEndian.AppendUint32(b, h)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// parseCursor decodes c. ok is false for an empty or unreadable cursor, which
// marks a first scan.
func parseCursor(c string) (known map[uint32]bool, ok bool) {
	if c == "" {
		return nil, false
	}
	b, err := base64.StdEncoding.DecodeString(c)
	if err != nil || len(b)%4 != 0 {
		return nil, false
	}
	known = make(map[uint32]bool, len(b)/4)
	for i := 0; i < len(b); i += 4 {
		known[binary.BigEndian.Uint32(b[i:])] = true
	}
	return known, true
}

// tags lists the version tags of the session's repository, sorted.
func (s *Source) tags(ctx context.Context, ss *session) ([]string, error) {
	endpointOrigin := originOf(ss.endpoint)
	u := ss.endpoint + "/v2/" + ss.name + "/tags/list?n=" + fmt.Sprint(pageSize)
	seen := map[string]bool{}
	var tags []string
	for page := 1; ; page++ {
		res, err := s.get(ctx, ss, u)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		if err := json.Unmarshal(res.body, &body); err != nil {
			return nil, fmt.Errorf("page %d: decode: %w", page, err)
		}
		for _, tg := range body.Tags {
			if tg == "" || seen[tg] || referrersTagRE.MatchString(tg) {
				continue
			}
			seen[tg] = true
			tags = append(tags, tg)
		}
		next := nextLink(res.header)
		if next == "" {
			break
		}
		base, _ := url.Parse(u)
		nu, err := base.Parse(next)
		if err != nil || originOf(nu.String()) != endpointOrigin {
			return nil, fmt.Errorf("page %d: the next link points to another origin; refusing to follow it", page)
		}
		if page >= maxPages {
			return nil, fmt.Errorf("more than %d pages (%d tags) of tags; refusing to list a truncated set", maxPages, maxPages*pageSize)
		}
		u = nu.String()
	}
	slices.Sort(tags)
	return tags, nil
}
