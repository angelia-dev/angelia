package fakereg

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// StatsPath serves the Stats of the running server as JSON.
const StatsPath = "/_fakereg/stats"

// Server is the fake registry. It is an http.Handler; create it with New.
type Server struct {
	now   func() time.Time
	start time.Time
	ecos  map[string]*eco
	mux   *http.ServeMux
}

// Option configures New.
type Option func(*Server)

// WithClock replaces time.Now, for tests. Latency still uses real timers.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// New returns a Server for c. The release schedule starts now: every
// package has its configured history, and new releases follow.
func New(c *Config, opts ...Option) *Server {
	s := &Server{now: time.Now, ecos: map[string]*eco{}, mux: http.NewServeMux()}
	for _, o := range opts {
		o(s)
	}
	s.start = s.now()
	for _, name := range Ecosystems() {
		b := c.Ecosystems[name]
		e := &eco{name: name, b: b, start: s.start, served: make([]int, b.Packages), phases: make([]time.Duration, b.Packages)}
		if b.ReleasesPerHour > 0 {
			e.interval = time.Duration(float64(b.Packages) / b.ReleasesPerHour * float64(time.Hour))
		}
		for i := range b.Packages {
			e.served[i] = b.History
			if e.interval > 0 {
				e.phases[i] = time.Duration(phaseHash(c.Seed, name, i) % uint64(e.interval)) //nolint:gosec // interval > 0, result < interval
			}
		}
		s.ecos[name] = e
	}
	s.mux.HandleFunc("GET /pypi/pypi/{name}/json", s.handle("pypi", renderPyPI))
	s.mux.HandleFunc("GET /npm/{name}", s.handle("npm", renderNPM))
	s.mux.HandleFunc("GET /crates/api/v1/crates/{name}/versions", s.handle("crates", renderCrates))
	s.mux.HandleFunc("GET /crates/{a}/{b}/{name}", s.cratesIndex())
	s.mux.HandleFunc("GET /github/repos/{owner}/{name}/releases", s.handle("github", renderGitHub))
	s.mux.HandleFunc("GET /github/rate_limit", s.rateLimit)
	s.mux.HandleFunc("GET /oci/v2/{owner}/{name}/tags/list", s.handle("oci", renderOCI))
	s.mux.HandleFunc("GET /oci/v2/{$}", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]any{}) })
	s.mux.HandleFunc("GET "+StatsPath, func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Stats()) })
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// PackageName is the name of package i: "pkg-00042". GitHub repositories
// and OCI repositories are "bench/<name>".
func PackageName(i int) string { return fmt.Sprintf("pkg-%05d", i) }

// Version is the version string of the k-th version (0-based) of every
// package: "1.<k>.0". GitHub tags carry a "v" prefix.
func Version(k int) string { return "1." + strconv.Itoa(k) + ".0" }

func parseName(name string, packages int) (int, bool) {
	digits, ok := strings.CutPrefix(name, "pkg-")
	if !ok || len(digits) != 5 {
		return 0, false
	}
	i, err := strconv.Atoi(digits)
	if err != nil || i < 0 || i >= packages {
		return 0, false
	}
	return i, true
}

func phaseHash(seed uint64, eco string, i int) uint64 {
	h := fnv.New64a()
	var buf [16]byte
	binary.LittleEndian.PutUint64(buf[:8], seed)
	binary.LittleEndian.PutUint64(buf[8:], uint64(i)) //nolint:gosec // i is a package index, never negative
	_, _ = h.Write(buf[:])
	_, _ = h.Write([]byte(eco))
	return h.Sum64()
}

// eco is the state of one ecosystem.
type eco struct {
	name     string
	b        Behaviour
	start    time.Time
	interval time.Duration // between two releases of one package; 0: none
	phases   []time.Duration

	requests, ok, notModified, throttled, forbidden, notFound, cancelled atomic.Int64

	mu     sync.Mutex
	served []int // per package: highest version count sent in a 200
	seen   []seenRelease
	tokens float64
	last   time.Time
	pStart time.Time // start of the current primary-limit window
	pUsed  int
}

// ts formats t at the ecosystem's fixed precision, as the real registry
// does, so body sizes do not depend on the clock.
func (e *eco) ts(t time.Time) string {
	layout := time.RFC3339 // GitHub, crates.io: whole seconds
	switch e.name {
	case "pypi":
		layout = "2006-01-02T15:04:05.000000Z07:00" // microseconds
	case "npm":
		layout = "2006-01-02T15:04:05.000Z07:00" // milliseconds
	}
	return t.UTC().Format(layout)
}

// primaryTake reserves one request of the primary limit. It reports false
// when the window is used up.
func (e *eco) primaryTake(now time.Time) bool {
	pl := e.b.PrimaryLimit
	if pl.Requests <= 0 {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pStart.IsZero() || !now.Before(e.pStart.Add(pl.Window)) {
		e.pStart, e.pUsed = now, 0
	}
	if e.pUsed >= pl.Requests {
		return false
	}
	e.pUsed++
	return true
}

// primaryRefund gives back a request that ended in 304.
func (e *eco) primaryRefund() {
	if e.b.PrimaryLimit.Requests <= 0 {
		return
	}
	e.mu.Lock()
	e.pUsed = max(e.pUsed-1, 0)
	e.mu.Unlock()
}

// primaryHeaders sets GitHub's x-ratelimit-* headers.
func (e *eco) primaryHeaders(h http.Header) {
	pl := e.b.PrimaryLimit
	if pl.Requests <= 0 {
		return
	}
	e.mu.Lock()
	used, reset := e.pUsed, e.pStart.Add(pl.Window)
	e.mu.Unlock()
	h.Set("X-RateLimit-Limit", strconv.Itoa(pl.Requests))
	h.Set("X-RateLimit-Remaining", strconv.Itoa(pl.Requests-used))
	h.Set("X-RateLimit-Used", strconv.Itoa(used))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
	h.Set("X-RateLimit-Resource", "core")
}

// rateLimit answers GET /github/rate_limit like GitHub (core and the
// deprecated rate, the same object) when primary_limit is set, else 404.
// Like on GitHub it does not count against the limit, and it is not a
// request in the statistics.
func (s *Server) rateLimit(w http.ResponseWriter, _ *http.Request) {
	e := s.ecos["github"]
	pl := e.b.PrimaryLimit
	if pl.Requests <= 0 {
		http.NotFound(w, nil)
		return
	}
	now := s.now()
	e.mu.Lock()
	used, reset := e.pUsed, e.pStart.Add(pl.Window)
	if e.pStart.IsZero() || !now.Before(reset) {
		used, reset = 0, now.Add(pl.Window)
	}
	e.mu.Unlock()
	c := map[string]int64{"limit": int64(pl.Requests), "remaining": int64(pl.Requests - used), "used": int64(used), "reset": reset.Unix()}
	writeJSON(w, http.StatusOK, map[string]any{"resources": map[string]any{"core": c}, "rate": c})
}

// count is the number of versions package i has at t.
func (e *eco) count(i int, t time.Time) int {
	n := e.b.History
	if e.interval > 0 {
		if since := t.Sub(e.start) - e.phases[i]; since >= 0 {
			n += int(since/e.interval) + 1
		}
	}
	return n
}

// publishedAt is when version k of package i was published. The initial
// history is spaced one day apart before the start.
func (e *eco) publishedAt(i, k int) time.Time {
	if k < e.b.History {
		return e.start.Add(-time.Duration(e.b.History-k) * 24 * time.Hour)
	}
	return e.start.Add(e.phases[i] + time.Duration(k-e.b.History)*e.interval)
}

func (e *eco) allow(now time.Time) bool {
	rl := e.b.RateLimit
	if rl.RPS <= 0 {
		return true
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last.IsZero() {
		e.tokens = float64(rl.Burst)
	} else {
		e.tokens = math.Min(float64(rl.Burst), e.tokens+now.Sub(e.last).Seconds()*rl.RPS)
	}
	e.last = now
	if e.tokens < 1 {
		return false
	}
	e.tokens--
	return true
}

// recordServed notes that a 200 with n versions of package i was sent at
// now, and records the lag of every release it carries for the first time.
func (e *eco) recordServed(i, n int, now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k := e.served[i]; k < n; k++ {
		pub := e.publishedAt(i, k)
		e.seen = append(e.seen, seenRelease{pub: pub, lag: now.Sub(pub)})
	}
	e.served[i] = max(e.served[i], n)
}

// page is what a renderer gets: package index and name, version count,
// the request and the server's base URL for this ecosystem.
type page struct {
	e     *eco
	i, n  int
	name  string
	owner string
	r     *http.Request
	base  string
	pad   string
}

// renderer returns the response body, extra headers, and whether the body
// holds the newest version (only then does it count for check lag).
type renderer func(p page, h http.Header) (body any, newest bool)

func (s *Server) handle(name string, render renderer) http.HandlerFunc {
	e := s.ecos[name]
	return func(w http.ResponseWriter, r *http.Request) {
		e.requests.Add(1)
		b := e.b
		// A random 429 is decided first, so it does not take a token.
		//nolint:gosec // simulated faults and latency, not security
		if (b.Random429 > 0 && rand.Float64() < b.Random429) || !e.allow(s.now()) {
			e.throttled.Add(1)
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(b.RateLimit.RetryAfter.Seconds()))))
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"message": "rate limited"})
			return
		}
		if !e.primaryTake(s.now()) {
			e.forbidden.Add(1)
			e.primaryHeaders(w.Header())
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "API rate limit exceeded"})
			return
		}
		d := b.Latency
		if b.LatencyJitter > 0 {
			d += rand.N(b.LatencyJitter) //nolint:gosec // simulated latency
		}
		if d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				e.cancelled.Add(1)
				return
			}
		}
		i, ok := parseName(r.PathValue("name"), b.Packages)
		if !ok {
			e.notFound.Add(1)
			e.primaryHeaders(w.Header())
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		now := s.now()
		n := e.count(i, now)
		etag := fmt.Sprintf("%016x", phaseHash(uint64(n), name+"\x00"+r.URL.RawQuery, i)) //nolint:gosec // n > 0
		etag = `"` + etag + `"`
		if b.ETag == ETagWeak {
			etag = "W/" + etag
		}
		lastMod := e.publishedAt(i, n-1).UTC().Truncate(time.Second)
		if b.ETag != ETagNone {
			w.Header().Set("ETag", etag)
		}
		if b.LastModified {
			w.Header().Set("Last-Modified", lastMod.Format(http.TimeFormat))
		}
		if notModified(r, b, etag, lastMod) {
			e.notModified.Add(1)
			e.primaryRefund() // a 304 does not count against GitHub's limit
			e.primaryHeaders(w.Header())
			w.WriteHeader(http.StatusNotModified)
			return
		}
		p := page{
			e: e, i: i, n: n, name: PackageName(i), owner: r.PathValue("owner"), r: r,
			base: "http://" + r.Host + "/" + name, pad: strings.Repeat("x", b.BodyPadding),
		}
		body, newest := render(p, w.Header())
		e.primaryHeaders(w.Header())
		if raw, ok := body.(rawBody); ok {
			w.Header().Set("Content-Type", raw.contentType)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(raw.data)
		} else {
			writeJSON(w, http.StatusOK, body)
		}
		e.ok.Add(1)
		if newest {
			e.recordServed(i, n, now)
		}
	}
}

func notModified(r *http.Request, b Behaviour, etag string, lastMod time.Time) bool {
	if b.ETag == ETagIgnore {
		return false
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if b.ETag == ETagNone {
			return false
		}
		for t := range strings.SplitSeq(inm, ",") {
			t = strings.TrimSpace(t)
			if t == "*" || strings.TrimPrefix(t, "W/") == strings.TrimPrefix(etag, "W/") {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); b.LastModified && ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !lastMod.After(t) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func renderPyPI(p page, _ http.Header) (any, bool) {
	releases := make(map[string]any, p.n)
	for k := range p.n {
		v := Version(k)
		releases[v] = []any{map[string]any{
			"filename":             p.name + "-" + v + ".tar.gz",
			"upload_time_iso_8601": p.e.ts(p.e.publishedAt(p.i, k)),
			"yanked":               false,
			"yanked_reason":        nil,
		}}
	}
	return map[string]any{
		"info":     map[string]any{"name": p.name, "version": Version(p.n - 1), "description": p.pad},
		"releases": releases,
	}, true
}

func renderNPM(p page, _ http.Header) (any, bool) {
	versions := make(map[string]any, p.n)
	times := map[string]string{"created": p.e.ts(p.e.publishedAt(p.i, 0)), "modified": p.e.ts(p.e.publishedAt(p.i, p.n-1))}
	for k := range p.n {
		v := Version(k)
		versions[v] = map[string]any{
			"name": p.name, "version": v,
			"dist": map[string]string{"tarball": p.base + "/" + p.name + "/-/" + p.name + "-" + v + ".tgz"},
		}
		times[v] = p.e.ts(p.e.publishedAt(p.i, k))
	}
	return map[string]any{
		"name": p.name, "dist-tags": map[string]string{"latest": Version(p.n - 1)},
		"versions": versions, "time": times, "readme": p.pad,
	}, true
}

func renderCrates(p page, _ http.Header) (any, bool) {
	versions := make([]any, 0, p.n)
	for k := p.n - 1; k >= 0; k-- {
		v := Version(k)
		versions = append(versions, map[string]any{
			"num": v, "created_at": p.e.ts(p.e.publishedAt(p.i, k)), "yanked": false,
			"dl_path": "/api/v1/crates/" + p.name + "/" + v + "/download",
		})
	}
	return map[string]any{
		"versions": versions, "meta": map[string]any{"total": p.n, "next_page": nil}, "_padding": p.pad,
	}, true
}

// cratesIndex serves the sparse index after checking the path prefix.
func (s *Server) cratesIndex() http.HandlerFunc {
	h, e := s.handle("crates", renderCratesIndex), s.ecos["crates"]
	return func(w http.ResponseWriter, r *http.Request) {
		if n := r.PathValue("name"); len(n) < 4 || r.PathValue("a") != n[:2] || r.PathValue("b") != n[2:4] {
			e.requests.Add(1)
			e.notFound.Add(1)
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		h(w, r)
	}
}

// rawBody is a renderer result that is not JSON.
type rawBody struct {
	contentType string
	data        []byte
}

// renderCratesIndex serves the sparse index file of a crate
// (https://doc.rust-lang.org/cargo/reference/registry-index.html): one JSON
// line per version, oldest first. Package names have more than three
// characters, so the path is {name[0:2]}/{name[2:4]}/{name}; cratesIndex
// answers any other prefix with 404.
func renderCratesIndex(p page, _ http.Header) (any, bool) {
	var b strings.Builder
	for k := range p.n {
		line := map[string]any{
			"name": p.name, "vers": Version(k), "deps": []any{}, "features": map[string]any{},
			"cksum": fmt.Sprintf("%064x", phaseHash(uint64(k), "crates", p.i)), "yanked": false, //nolint:gosec // k >= 0
		}
		if k == 0 && p.pad != "" {
			line["_padding"] = p.pad
		}
		data, _ := json.Marshal(line)
		b.Write(data)
		b.WriteByte('\n')
	}
	return rawBody{contentType: "text/plain; charset=utf-8", data: []byte(b.String())}, true
}

func renderGitHub(p page, _ http.Header) (any, bool) {
	q := p.r.URL.Query()
	perPage := queryInt(q, "per_page", 30, 1, 100)
	pageNo := queryInt(q, "page", 1, 1, math.MaxInt32)
	rels := []any{}
	first := (pageNo - 1) * perPage // offset in newest-first order
	for j := first; j < first+perPage && j < p.n; j++ {
		k := p.n - 1 - j
		tag := "v" + Version(k)
		at := p.e.ts(p.e.publishedAt(p.i, k))
		rel := map[string]any{
			"id": k + 1, "tag_name": tag, "name": tag, "draft": false, "prerelease": false,
			"html_url":   p.base + "/" + p.owner + "/" + p.name + "/releases/tag/" + tag,
			"created_at": at, "published_at": at,
		}
		if j == first {
			rel["body"] = p.pad
		}
		rels = append(rels, rel)
	}
	return rels, first == 0
}

func renderOCI(p page, h http.Header) (any, bool) {
	tags := make([]string, p.n)
	for k := range p.n {
		tags[k] = Version(k)
	}
	slices.Sort(tags) // the distribution spec lists tags in lexical order
	q := p.r.URL.Query()
	if last := q.Get("last"); last != "" {
		at, _ := slices.BinarySearch(tags, last)
		for at < len(tags) && tags[at] <= last {
			at++
		}
		tags = tags[at:]
	}
	if q.Has("n") {
		if n := queryInt(q, "n", len(tags), 0, len(tags)); n < len(tags) {
			tags = tags[:n]
			next := url.Values{"n": {strconv.Itoa(n)}, "last": {tags[len(tags)-1]}}
			h.Set("Link", fmt.Sprintf("</oci/v2/%s/%s/tags/list?%s>; rel=\"next\"", p.owner, p.name, next.Encode()))
		}
	}
	return map[string]any{"name": p.owner + "/" + p.name, "tags": tags, "_padding": p.pad},
		slices.Contains(tags, Version(p.n-1))
}

func queryInt(q url.Values, key string, def, lo, hi int) int {
	v, err := strconv.Atoi(q.Get(key))
	if err != nil {
		return def
	}
	return min(max(v, lo), hi)
}
