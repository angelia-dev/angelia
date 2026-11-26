package detect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/repolink"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// githubHub is a counting fake of api.github.com for example-org/demo.
type githubHub struct {
	mu       sync.Mutex
	paths    []string
	releases string
	status   int
}

func (g *githubHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.paths = append(g.paths, r.URL.RequestURI())
	rel, status := g.releases, g.status
	g.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if r.URL.Path == "/repos/example-org/demo/releases" {
		_, _ = fmt.Fprint(w, rel)
		return
	}
	http.NotFound(w, r)
}

func (g *githubHub) requests() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.paths)
}

func (g *githubHub) set(releases string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.releases = releases
}

func release(tag, body string) string {
	b, _ := json.Marshal(map[string]any{
		"tag_name": tag, "draft": false, "body": body,
		"html_url": "https://github.com/example-org/demo/releases/tag/" + tag,
	})
	return string(b)
}

type enrichCounts struct {
	mu sync.Mutex
	m  map[string]int
}

func (c *enrichCounts) NotesEnrichment(kind, result string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	c.m[kind+"/"+result]++
}

func (c *enrichCounts) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

type notesFixture struct {
	*fixture
	gh     *githubHub
	counts *enrichCounts
	filler *repolink.Filler
	srv    *httptest.Server
}

func newNotesFixture(t *testing.T, policy string, hc func(*httptest.Server) *http.Client) *notesFixture {
	t.Helper()
	nf := &notesFixture{fixture: newFixturePolicy(t, policy), gh: &githubHub{}, counts: &enrichCounts{}}
	nf.srv = httptest.NewServer(nf.gh)
	t.Cleanup(nf.srv.Close)
	client := sourcetest.Client(t, nf.srv)
	if hc != nil {
		client = hc(nf.srv)
	}
	nf.filler = repolink.NewFiller(nf.srv.URL, client, repolink.WithRecorder(nf.counts))
	return nf
}

func (nf *notesFixture) check(t *testing.T, now time.Time) {
	t.Helper()
	c := nf.checker(now)
	c.Notes = nf.filler
	out, err := c.Check(t.Context(), nf.tg)
	if err != nil {
		t.Fatal(err)
	}
	nf.tg.FetchState = out.FetchState
}

func linked(raw string, at time.Time) source.Version {
	v := ver(raw, at)
	v.Metadata["repository_url"] = "https://github.com/example-org/demo"
	return v
}

func (nf *notesFixture) events(t *testing.T) []store.Event {
	t.Helper()
	ev, err := nf.st.ListEventsAfter(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	return ev
}

type stored struct {
	Subject string `json:"subject"`
	Data    struct {
		Notes json.RawMessage `json:"notes"`
		URLs  struct {
			Changelog  *string `json:"changelog"`
			Repository *string `json:"repository"`
		} `json:"urls"`
	} `json:"data"`
}

func decodeEvent(t *testing.T, e store.Event) stored {
	t.Helper()
	var s stored
	if err := json.Unmarshal(e.Payload, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

// The baseline check, a 304 and a 200 with nothing new cost no GitHub request
// (D81); one new version costs exactly one.
func TestNotesStepCost(t *testing.T) {
	nf := newNotesFixture(t, `{}`, nil)
	nf.gh.set("[" + release("1.1.0", "## 1.1.0\n\n- Added things") + "]")
	nf.src.Set("demo", linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t0) // first check of the target: the baseline
	if n := nf.gh.requests(); n != 0 {
		t.Fatalf("baseline: %d GitHub requests", n)
	}
	nf.check(t, t1) // not modified
	if n := nf.gh.requests(); n != 0 {
		t.Fatalf("304: %d GitHub requests", n)
	}
	nf.src.Set("demo", linked("1.0.0", t0.Add(-48*time.Hour))) // new ETag, same versions
	nf.check(t, t1)
	if n := nf.gh.requests(); n != 0 {
		t.Fatalf("200 with nothing new: %d GitHub requests", n)
	}
	nf.src.Set("demo", linked("1.1.0", t1.Add(-time.Minute)), linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t1)
	if n := nf.gh.requests(); n != 1 {
		t.Fatalf("one new version: %d GitHub requests, want 1", n)
	}
	if got := nf.gh.paths[0]; got != "/repos/example-org/demo/releases?per_page=30" {
		t.Errorf("request %q", got)
	}
}

// 8 new versions: still one request; only the 5 newest are looked at.
func TestNotesStepFiveNewestOfEight(t *testing.T) {
	nf := newNotesFixture(t, `{}`, nil)
	var rels, vs []string
	var versions []source.Version
	for i := 8; i >= 1; i-- {
		raw := fmt.Sprintf("1.%d.0", i)
		versions = append(versions, linked(raw, t1.Add(-time.Duration(i)*time.Minute)))
		rels = append(rels, release("v"+raw, "notes of "+raw))
		vs = append(vs, raw)
	}
	nf.gh.set("[" + strings.Join(rels, ",") + "]")
	nf.src.Set("demo", linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t0)
	nf.src.Set("demo", append(versions, linked("1.0.0", t0.Add(-48*time.Hour)))...)
	nf.check(t, t1)
	if n := nf.gh.requests(); n != 1 {
		t.Fatalf("%d GitHub requests, want 1", n)
	}
	withNotes := 0
	for _, e := range nf.events(t) {
		s := decodeEvent(t, e)
		has := string(s.Data.Notes) != "null"
		want := false
		for _, raw := range vs[:5] { // the five newest: 1.8.0 ... 1.4.0
			if s.Subject == "pypi:demo@"+raw {
				want = true
			}
		}
		if has != want {
			t.Errorf("%s: has notes %v, want %v", s.Subject, has, want)
		}
		if has {
			withNotes++
		}
	}
	if withNotes != 5 || len(nf.events(t)) != 8 {
		t.Errorf("%d events, %d with notes; want 8 and 5", len(nf.events(t)), withNotes)
	}
}

// recordingFiller records what the step hands over.
type recordingFiller struct {
	got     [][]source.Version
	nowants bool
}

func (r *recordingFiller) Wants(string) bool { return !r.nowants }

func (r *recordingFiller) Fill(_ context.Context, _ source.Target, vs []source.Version) []source.Version {
	r.got = append(r.got, vs)
	return vs
}

// The step hands over only new, unyanked versions, at most five, newest first.
func TestNotesStepSelectsNewVersions(t *testing.T) {
	f := newFixture(t)
	f.src.Set("demo", ver("1.0.0", t0.Add(-48*time.Hour)), ver("0.9.0", t0.Add(-96*time.Hour)))
	rf := &recordingFiller{}
	c := f.checker(t0)
	c.Notes = rf
	out, err := c.Check(t.Context(), f.tg)
	if err != nil {
		t.Fatal(err)
	}
	f.tg.FetchState = out.FetchState
	if len(rf.got) != 0 {
		t.Fatal("the baseline check reached the notes step")
	}
	yanked := ver("1.5.0", t1.Add(-time.Minute))
	yanked.Yanked = true
	vs := []source.Version{ver("1.7.0", t1.Add(-time.Minute)), yanked}
	for i := 6; i >= 1; i-- {
		vs = append(vs, ver(fmt.Sprintf("1.%d.1", i), t1.Add(-time.Duration(i)*time.Hour)))
	}
	vs = append(vs, ver("1.0.0", t0.Add(-48*time.Hour)), ver("0.9.0", t0.Add(-96*time.Hour)))
	f.src.Set("demo", vs...)
	c = f.checker(t1)
	c.Notes = rf
	if _, err := c.Check(t.Context(), f.tg); err != nil {
		t.Fatal(err)
	}
	if len(rf.got) != 1 {
		t.Fatalf("%d calls", len(rf.got))
	}
	var raws []string
	for _, v := range rf.got[0] {
		raws = append(raws, v.Raw)
	}
	if got, want := strings.Join(raws, " "), "1.7.0 1.6.1 1.5.1 1.4.1 1.3.1"; got != want {
		t.Errorf("handed over %q, want %q", got, want)
	}
}

// A version that already has notes keeps them, and costs no request.
func TestNotesStepKeepsSourceNotes(t *testing.T) {
	nf := newNotesFixture(t, `{}`, nil)
	nf.gh.set("[" + release("1.1.0", "from GitHub") + "]")
	nf.src.Set("demo", linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t0)
	v := linked("1.1.0", t1.Add(-time.Minute))
	v.Notes = source.NewNotes("markdown", "from the source")
	nf.src.Set("demo", v, linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t1)
	if n := nf.gh.requests(); n != 0 {
		t.Errorf("%d GitHub requests, want none", n)
	}
	ev := nf.events(t)
	if len(ev) != 1 || !strings.Contains(string(decodeEvent(t, ev[0]).Data.Notes), "from the source") {
		t.Errorf("events %d, notes %s", len(ev), ev[0].Payload)
	}
}

// Failures never fail the check: the event goes out with notes null.
func TestNotesStepFailuresNeverFailTheCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		releases string
		status   int
		want     string
	}{
		"500":          {status: 500, want: "error"},
		"404":          {status: 404, want: "no_repository"},
		"invalid JSON": {releases: "[{", want: "error"},
		"no match":     {releases: "[" + release("v9.9.9", "other") + "]", want: "no_match"},
	} {
		t.Run(name, func(t *testing.T) {
			nf := newNotesFixture(t, `{}`, nil)
			nf.gh.mu.Lock()
			nf.gh.releases, nf.gh.status = tc.releases, tc.status
			nf.gh.mu.Unlock()
			nf.src.Set("demo", linked("1.0.0", t0.Add(-48*time.Hour)))
			nf.check(t, t0)
			nf.src.Set("demo", linked("1.1.0", t1.Add(-time.Minute)), linked("1.0.0", t0.Add(-48*time.Hour)))
			nf.check(t, t1)
			ev := nf.events(t)
			if len(ev) != 1 {
				t.Fatalf("%d events, want the release committed", len(ev))
			}
			if s := decodeEvent(t, ev[0]); string(s.Data.Notes) != "null" || s.Data.URLs.Changelog != nil {
				t.Errorf("notes %s, changelog %v", s.Data.Notes, s.Data.URLs.Changelog)
			}
			if nf.counts.get("pypi/"+tc.want) != 1 {
				t.Errorf("metric %v, want %s", nf.counts.m, tc.want)
			}
		})
	}
}

// With the anonymous bucket of api.github.com empty, the step is refused at
// once and the event is written without notes.
func TestNotesStepRateLimitedDoesNotDelayTheCheck(t *testing.T) {
	nf := newNotesFixture(t, `{}`, func(*httptest.Server) *http.Client {
		nc, err := netguard.New(netguard.Config{
			Allow:       []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")},
			MaxAttempts: 1,
			HostLimits: map[string]netguard.HostLimit{"127.0.0.1": {
				Anonymous: netguard.Limit{Rate: 1.0 / 3600, Burst: 1}, Authenticated: netguard.Limit{Rate: 1, Burst: 1},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return nc.HTTPClient()
	})
	nf.gh.set("[" + release("1.1.0", "first") + "," + release("1.2.0", "second") + "]")
	nf.src.Set("demo", linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t0)
	nf.src.Set("demo", linked("1.1.0", t1.Add(-time.Minute)), linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t1) // takes the only slot
	start := time.Now()
	nf.src.Set("demo", linked("1.2.0", t1.Add(-time.Second)), linked("1.1.0", t1.Add(-time.Minute)), linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t1)
	if time.Since(start) > 5*time.Second {
		t.Error("the check waited for the rate limit")
	}
	ev := nf.events(t)
	if len(ev) != 2 {
		t.Fatalf("%d events", len(ev))
	}
	if string(decodeEvent(t, ev[0]).Data.Notes) == "null" || string(decodeEvent(t, ev[1]).Data.Notes) != "null" {
		t.Errorf("notes: %s | %s", decodeEvent(t, ev[0]).Data.Notes, decodeEvent(t, ev[1]).Data.Notes)
	}
	if nf.counts.get("pypi/rate_limited") != 1 || nf.counts.get("pypi/matched") != 1 {
		t.Errorf("metric %v", nf.counts.m)
	}
}

func dataSchema(t *testing.T) *jsonschema.Resolved {
	t.Helper()
	b, err := os.ReadFile("../../api/schemas/release/v1.json")
	if err != nil {
		t.Fatal(err)
	}
	// The schema's label pattern uses ECMA 262 \uXXXX escapes, which Go's
	// regexp lacks; \xXX means the same here.
	b = bytes.ReplaceAll(b, []byte(`\\u00`), []byte(`\\x`))
	var s jsonschema.Schema
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func validateData(t *testing.T, r *jsonschema.Resolved, payload []byte) {
	t.Helper()
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(env.Data); err != nil {
		t.Fatalf("data fails the vendored schema: %v\n%s", err, payload)
	}
}

// End to end on SQLite: a PyPI target with a known set and a new version
// whose repository has a matching release.
func TestNotesStepEndToEnd(t *testing.T) {
	schema := dataSchema(t)
	nf := newNotesFixture(t, `{}`, nil)
	nf.gh.set("[" + release("v1.1.0", "## 1.1.0\r\n\r\n- Fixed it\r\n") + "]")
	nf.src.Set("demo", linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t0)
	nv := linked("1.1.0", t1.Add(-time.Minute))
	nv.NotesURL = "https://example.org/declared-changelog"
	nf.src.Set("demo", nv, linked("1.0.0", t0.Add(-48*time.Hour)))
	nf.check(t, t1)
	ev := nf.events(t)
	if len(ev) != 1 {
		t.Fatalf("%d events", len(ev))
	}
	s := decodeEvent(t, ev[0])
	const want = `{"format":"markdown","body":"## 1.1.0\n\n- Fixed it","truncated":false}`
	if string(s.Data.Notes) != want {
		t.Errorf("data.notes = %s, want %s", s.Data.Notes, want)
	}
	if s.Data.URLs.Changelog == nil || *s.Data.URLs.Changelog != "https://github.com/example-org/demo/releases/tag/v1.1.0" {
		t.Errorf("urls.changelog = %v, want the release page", s.Data.URLs.Changelog)
	}
	if s.Data.URLs.Repository == nil || *s.Data.URLs.Repository != "https://github.com/example-org/demo" {
		t.Errorf("urls.repository = %v", s.Data.URLs.Repository)
	}
	validateData(t, schema, ev[0].Payload)
}

// The same with a cooldown: the held version's snapshot carries the notes,
// and so does the event that the check after the cooldown emits.
func TestNotesStepEndToEndWithCooldown(t *testing.T) {
	schema := dataSchema(t)
	nf := newNotesFixture(t, `{"cooldown":"72h"}`, nil)
	nf.gh.set("[" + release("1.1.0", "## 1.1.0\n\n- Held notes") + "]")
	nf.src.Set("demo", linked("1.0.0", t0.Add(-480*time.Hour)))
	nf.check(t, t0)
	nf.src.Set("demo", linked("1.1.0", t1.Add(-time.Minute)), linked("1.0.0", t0.Add(-480*time.Hour)))
	nf.check(t, t1)
	if n := len(nf.events(t)); n != 0 {
		t.Fatalf("%d events before the cooldown ends", n)
	}
	held, err := nf.st.ListPendingReleases(t.Context(), nf.tg.ID)
	if err != nil || len(held) != 1 {
		t.Fatalf("pending releases %v, %v", held, err)
	}
	if !strings.Contains(string(held[0].Release), "Held notes") || !strings.Contains(string(held[0].Release), "releases/tag/1.1.0") {
		t.Errorf("the snapshot lacks the notes: %s", held[0].Release)
	}
	nf.gh.set("[]") // the later check must use the snapshot, not GitHub
	nf.check(t, t1.Add(80*time.Hour))
	ev := nf.events(t)
	if len(ev) != 1 {
		t.Fatalf("%d events after the cooldown", len(ev))
	}
	s := decodeEvent(t, ev[0])
	if !strings.Contains(string(s.Data.Notes), "Held notes") || s.Data.URLs.Changelog == nil || !strings.HasSuffix(*s.Data.URLs.Changelog, "/releases/tag/1.1.0") {
		t.Errorf("event %s", ev[0].Payload)
	}
	validateData(t, schema, ev[0].Payload)
}

// countingStore counts reads of the known set.
type countingStore struct {
	*sqlite.Store
	reads int
}

func (c *countingStore) ListVersions(ctx context.Context, id string) ([]store.Version, error) {
	c.reads++
	return c.Store.ListVersions(ctx, id)
}

// A check of a kind the filler does not want reads no known set and hands
// nothing over; one it wants does read it.
func TestNotesStepAsksTheKindFirst(t *testing.T) {
	for _, wants := range []bool{false, true} {
		f := newFixture(t)
		cs := &countingStore{Store: f.st}
		rf := &recordingFiller{nowants: !wants}
		c := f.checker(t0)
		c.Store, c.Notes = cs, rf
		f.src.Set("demo", ver("1.0.0", t0.Add(-48*time.Hour)))
		out, err := c.Check(t.Context(), f.tg)
		if err != nil {
			t.Fatal(err)
		}
		f.tg.FetchState = out.FetchState
		f.src.Set("demo", ver("1.1.0", t1.Add(-time.Minute)), ver("1.0.0", t0.Add(-48*time.Hour)))
		c = f.checker(t1)
		c.Store, c.Notes = cs, rf
		if _, err := c.Check(t.Context(), f.tg); err != nil {
			t.Fatal(err)
		}
		if wants != (cs.reads > 0) || wants != (len(rf.got) == 1) {
			t.Errorf("wants %v: %d known-set reads, %d Fill calls", wants, cs.reads, len(rf.got))
		}
	}
}
