package maven_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/maven"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

// prefix is the repository path of the fake repository, like a Nexus
// ".../repository/maven-releases" base URL.
const prefix = "/repository/maven-releases"

const metaPath = prefix + "/com/example/test/widget/maven-metadata.xml"

// repo is a fake Maven repository: metadata documents by path, and the
// Last-Modified (or status) of each pom.
type repo struct {
	mu       sync.Mutex
	docs     map[string][]byte
	poms     map[string]string // path -> Last-Modified
	pomCodes map[string]int    // path -> status, overrides poms
	requests []string          // "METHOD path"
}

func newRepo(t testing.TB) *repo {
	b, err := os.ReadFile("testdata/widget.xml")
	if err != nil {
		t.Fatal(err)
	}
	return &repo{docs: map[string][]byte{metaPath: b}, poms: map[string]string{}, pomCodes: map[string]int{}}
}

func (r *repo) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req.Method+" "+req.URL.EscapedPath())
	p := req.URL.EscapedPath()
	if strings.HasSuffix(p, ".pom") {
		if req.Method != http.MethodHead {
			http.Error(w, "HEAD only", http.StatusMethodNotAllowed)
			return
		}
		if code, ok := r.pomCodes[p]; ok {
			w.WriteHeader(code)
			return
		}
		lm, ok := r.poms[p]
		if !ok {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Last-Modified", lm)
		return
	}
	body, ok := r.docs[p]
	if !ok {
		http.NotFound(w, req)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	w.Header().Set("ETag", etag)
	if req.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	_, _ = w.Write(body)
}

func (r *repo) setMeta(versions ...string) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><metadata><groupId>com.example.test</groupId><artifactId>widget</artifactId><versioning><versions>`)
	for _, v := range versions {
		b.WriteString("<version>" + v + "</version>")
	}
	b.WriteString(`</versions></versioning></metadata>`)
	r.setDoc(metaPath, b.String())
}

func (r *repo) setDoc(path, body string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.docs[path] = []byte(body)
}

func (r *repo) pom(v, lastModified string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.poms[pomPath(v)] = lastModified
}

func (r *repo) pomStatus(v string, code int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pomCodes[pomPath(v)] = code
}

func (r *repo) clearStatus(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.pomCodes, pomPath(v))
}

func pomPath(v string) string {
	return prefix + "/com/example/test/widget/" + v + "/widget-" + v + ".pom"
}

func (r *repo) heads() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, q := range r.requests {
		if strings.HasPrefix(q, "HEAD ") {
			out = append(out, strings.TrimPrefix(q, "HEAD "))
		}
	}
	return out
}

func (r *repo) reqs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

func (r *repo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

func (r *repo) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = nil
}

func target(pkg string) source.Target {
	return source.Target{Kind: maven.Kind, Coordinates: map[string]string{"package": pkg}}
}

var widget = target("com.example.test:widget")

func TestContract(t *testing.T) {
	r := newRepo(t)
	missing := target("com.example.test:nope")
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: r,
		New: func(baseURL string, c *http.Client) source.Source {
			return maven.New(baseURL+prefix+"/", c, maven.WithLogger(slog.New(slog.DiscardHandler)))
		},
		Valid: widget,
		Invalid: []source.Target{
			{Kind: maven.Kind},
			target(""),
			target("com.example.test"),                   // no ":"
			target("com.example:test:widget"),            // two ":"
			target(":widget"),                            // empty group
			target("com.example.test:"),                  // empty artifact
			target("a..b:c"),                             // empty group segment
			target(".a:b"),                               // leading dot
			target("a.:b"),                               // trailing dot
			target("a:.."),                               // artifact ..
			target("a:."),                                // artifact .
			target("a/b:c"),                              // slash in group
			target("a:b/c"),                              // slash in artifact
			target("a:" + strings.Repeat("x", 256)),      // 256-character artifact
			target(strings.Repeat("g", 256) + ":widget"), // 256-character group
			target(" com.example.test:widget"),           // white space
			target("com.example.test:wid get"),           // white space
			{Kind: maven.Kind, Coordinates: map[string]string{"package": "a:b", "name": "x"}}, // extra key
			{Kind: maven.Kind, Coordinates: map[string]string{"group": "a", "artifact": "b"}},
			{Kind: "npm", Coordinates: map[string]string{"package": "a:b"}},
		},
		Missing:     &missing,
		WantRaw:     []string{"1.2.0", "1.2.0-rc1", "1.1.0", "1.1.0-beta-1", "1.0.1", "1.0.0"},
		Conditional: true,
		Change: func() {
			r.setMeta("1.0.0", "1.1.0-beta-1", "1.1.0", "1.2.0-rc1", "1.2.0", "1.0.1", "1.3.0")
		},
	})
}

func TestValidateAcceptsRealCoordinates(t *testing.T) {
	s := maven.New("", &http.Client{})
	for _, pkg := range []string{
		"org.slf4j:slf4j-api", "com.google.guava:guava", "junit:junit", "org.apache.maven.plugins:maven-compiler-plugin",
		"io.netty:netty-tcnative-boringssl-static", "com.example_test:Widget.Core", "a:" + strings.Repeat("x", 255),
	} {
		if err := s.Validate(target(pkg)); err != nil {
			t.Errorf("Validate(%s): %v", pkg, err)
		}
	}
}

type env struct {
	r   *repo
	src *maven.Source
	log *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	r := newRepo(t)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	logs := &bytes.Buffer{}
	src := maven.New(srv.URL+prefix, sourcetest.Client(t, srv),
		maven.WithLogger(slog.New(slog.NewTextHandler(&syncWriter{w: logs}, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	return &env{r: r, src: src, log: logs}
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (e *env) list(t *testing.T, st source.FetchState) source.Result {
	t.Helper()
	res, err := e.src.List(t.Context(), widget, st)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// noValidators keeps only the cursor, so the next call fetches the metadata.
func noValidators(st source.FetchState) source.FetchState {
	return source.FetchState{Cursor: st.Cursor}
}

func raws(vs []source.Version) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Raw
	}
	return out
}

// The metadata is read under a base_url with a path, and the date lookup is a
// HEAD of the new version's pom.
func TestRequestPathsAndLookup(t *testing.T) {
	e := newEnv(t)
	first := e.list(t, source.FetchState{})
	if got := e.r.reqs(); !slices.Equal(got, []string{"GET " + metaPath}) {
		t.Fatalf("first scan requests = %v", got)
	}
	for _, v := range first.Versions {
		if !v.PublishedAt.IsZero() || v.Yanked || v.Prerelease != nil || v.URL != "" {
			t.Errorf("first scan version %+v: want undated, not yanked, no flag, no URL", v)
		}
	}
	e.r.setMeta("1.0.0", "1.1.0-beta-1", "1.1.0", "1.2.0-rc1", "1.2.0", "1.0.1", "1.2.1-SNAPSHOT", "1.3.0")
	e.r.pom("1.3.0", "Tue, 22 Sep 2026 14:50:55 GMT")
	e.r.reset()
	second := e.list(t, first.State)
	if got := e.r.heads(); !slices.Equal(got, []string{pomPath("1.3.0"), pomPath("1.2.1-SNAPSHOT")}) {
		t.Fatalf("lookups = %v", got)
	}
	if !slices.Equal(raws(second.Versions)[:2], []string{"1.3.0", "1.2.1-SNAPSHOT"}) {
		t.Fatalf("versions = %v", raws(second.Versions))
	}
	want := time.Date(2026, 9, 22, 14, 50, 55, 0, time.UTC)
	if got := second.Versions[0].PublishedAt; !got.Equal(want) || got.Location() != time.UTC {
		t.Errorf("PublishedAt = %v, want %v", got, want)
	}
	if !second.Versions[1].PublishedAt.IsZero() {
		t.Error("a 404 pom must leave the version undated")
	}
	if !strings.Contains(e.log.String(), "level=WARN") || !strings.Contains(e.log.String(), "1.2.1-SNAPSHOT") {
		t.Errorf("no WARN for the failed lookup: %s", e.log.String())
	}
	for _, v := range second.Versions[2:] {
		if !v.PublishedAt.IsZero() {
			t.Errorf("%s was looked up again", v.Raw)
		}
	}
	// Nothing new: no lookup.
	e.r.reset()
	e.list(t, noValidators(second.State))
	if h := e.r.heads(); len(h) != 0 {
		t.Errorf("lookups without new versions: %v", h)
	}
}

func TestLookupsAreCapped(t *testing.T) {
	e := newEnv(t)
	first := e.list(t, source.FetchState{})
	vs := []string{"1.0.0"}
	for i := range 60 {
		vs = append(vs, fmt.Sprintf("2.%d", i))
		e.r.pom(fmt.Sprintf("2.%d", i), "Tue, 22 Sep 2026 14:50:55 GMT")
	}
	e.r.setMeta(vs...)
	e.r.reset()
	res := e.list(t, noValidators(first.State))
	if n := len(e.r.heads()); n != 50 {
		t.Fatalf("lookups = %d, want 50", n)
	}
	dated := 0
	for _, v := range res.Versions {
		if !v.PublishedAt.IsZero() {
			dated++
		}
	}
	if dated != 50 || res.Versions[0].Raw != "2.59" || res.Versions[0].PublishedAt.IsZero() {
		t.Errorf("dated = %d, newest %+v: the newest versions are dated first", dated, res.Versions[0])
	}
}

func TestLookupFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  int
		fatal bool
	}{
		{"server error", http.StatusBadGateway, false},
		{"forbidden", http.StatusForbidden, false},
		{"too many requests", http.StatusTooManyRequests, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			first := e.list(t, source.FetchState{})
			e.r.setMeta("1.0.0", "1.3.0")
			e.r.pomStatus("1.3.0", tc.code)
			res, err := e.src.List(t.Context(), widget, noValidators(first.State))
			if tc.fatal {
				var se *netguard.StatusError
				if !errors.As(err, &se) || se.StatusCode != tc.code {
					t.Fatalf("err = %v, want the %d", err, tc.code)
				}
				// The cursor did not advance: the next call looks 1.3.0 up again.
				e.r.pom("1.3.0", "Tue, 22 Sep 2026 14:50:55 GMT")
				e.r.clearStatus("1.3.0")
				res = e.list(t, noValidators(first.State))
				if res.Versions[0].Raw != "1.3.0" || res.Versions[0].PublishedAt.IsZero() {
					t.Errorf("retry = %+v", res.Versions[0])
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if res.Versions[0].Raw != "1.3.0" || !res.Versions[0].PublishedAt.IsZero() {
				t.Errorf("version = %+v, want undated", res.Versions[0])
			}
		})
	}
}

func TestLookupCancelled(t *testing.T) {
	r := newRepo(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodHead {
			cancel()
			<-req.Context().Done()
			return
		}
		r.ServeHTTP(w, req)
	}))
	defer srv.Close()
	src := maven.New(srv.URL+prefix, sourcetest.Client(t, srv), maven.WithLogger(slog.New(slog.DiscardHandler)))
	first, err := src.List(t.Context(), widget, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	r.setMeta("1.0.0", "1.3.0")
	if _, err := src.List(ctx, widget, noValidators(first.State)); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestUnreadableCursorIsAFirstScan(t *testing.T) {
	e := newEnv(t)
	for _, c := range []string{"", "not json", `{"versions":`} {
		e.r.reset()
		res := e.list(t, source.FetchState{Cursor: c})
		if h := e.r.heads(); len(h) != 0 {
			t.Errorf("cursor %q: lookups %v", c, h)
		}
		if !strings.Contains(res.State.Cursor, `"1.2.0-rc1"`) {
			t.Errorf("cursor %q: next cursor %q", c, res.State.Cursor)
		}
	}
}

func TestNotModifiedKeepsCursor(t *testing.T) {
	e := newEnv(t)
	first := e.list(t, source.FetchState{})
	second := e.list(t, first.State)
	if !second.NotModified || second.State != first.State {
		t.Fatalf("second = %+v", second)
	}
}

func xmlDoc(decl, body string) string {
	return decl + `<metadata><groupId>com.example.test</groupId><artifactId>widget</artifactId><versioning><versions>` + body + `</versions></versioning></metadata>`
}

func TestXML(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want []string // nil: List fails
	}{
		{"iso-8859-1", xmlDoc(`<?xml version="1.0" encoding="ISO-8859-1"?><!-- caf`+"\xe9"+` -->`, `<version>1.0</version><version>2.0</version>`), []string{"2.0", "1.0"}},
		{"utf8 without hyphen", xmlDoc(`<?xml version="1.0" encoding="Utf8"?>`, `<version>1.0</version>`), []string{"1.0"}},
		{"UTF8", xmlDoc(`<?xml version="1.0" encoding="UTF8"?>`, `<version>1.0</version>`), []string{"1.0"}},
		{"us-ascii", xmlDoc(`<?xml version="1.0" encoding="US-ASCII"?>`, `<version>1.0</version>`), []string{"1.0"}},
		{"no declaration", xmlDoc(``, `<version>1.0</version>`), []string{"1.0"}},
		{"utf-16 refused", xmlDoc(`<?xml version="1.0" encoding="UTF-16"?>`, `<version>1.0</version>`), nil},
		{"windows-1252 refused", xmlDoc(`<?xml version="1.0" encoding="windows-1252"?>`, `<version>1.0</version>`), nil},
		{"artifactId mismatch", strings.Replace(xmlDoc(``, `<version>1.0</version>`), "<artifactId>widget<", "<artifactId>gadget<", 1), nil},
		{"groupId mismatch", strings.Replace(xmlDoc(``, `<version>1.0</version>`), "<groupId>com.example.test<", "<groupId>com.example.other<", 1), nil},
		{"no groupId or artifactId", `<metadata><versioning><versions><version>1.0</version></versions></versioning></metadata>`, []string{"1.0"}},
		{"not metadata", `<project><version>1.0</version></project>`, nil},
		{"not XML", `{"versions":["1.0"]}`, nil},
		{
			"internal entity not expanded",
			`<?xml version="1.0"?><!DOCTYPE metadata [<!ENTITY v "6.6.6">]>` + xmlDoc(``, `<version>&v;</version>`), nil,
		},
		{"unused entity declaration", `<?xml version="1.0"?><!DOCTYPE metadata [<!ENTITY v "6.6.6">]>` + xmlDoc(``, `<version>1.0</version>`), []string{"1.0"}},
		{"no versions", `<metadata><groupId>com.example.test</groupId><artifactId>widget</artifactId></metadata>`, []string{}},
		{
			"invalid and duplicate versions",
			xmlDoc(``, `<version> 1.0 </version><version>1.0</version><version></version><version>a/b</version><version>a\b</version>`+
				`<version>1 0</version><version>..</version><version>.</version><version>`+strings.Repeat("9", 257)+`</version>`+
				`<version>1&#x9;2</version><version>1&#x9f;2</version><version>2.0-SNAPSHOT</version><version>`+strings.Repeat("9", 256)+`</version>`),
			[]string{strings.Repeat("9", 256), "2.0-SNAPSHOT", "1.0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.r.setDoc(metaPath, tc.doc)
			res, err := e.src.List(t.Context(), widget, source.FetchState{})
			if tc.want == nil {
				if err == nil {
					t.Fatalf("List succeeded with %v", raws(res.Versions))
				}
				if strings.Contains(err.Error(), "6.6.6") {
					t.Errorf("entity expanded: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := raws(res.Versions); !slices.Equal(got, tc.want) {
				t.Errorf("versions = %v, want %v", got, tc.want)
			}
		})
	}
}

// An external entity is never fetched: the only request is the metadata.
func TestXMLExternalEntityNotFetched(t *testing.T) {
	e := newEnv(t)
	doc := `<?xml version="1.0"?><!DOCTYPE metadata [<!ENTITY ext SYSTEM "` + strings.TrimSuffix(metaPath, "maven-metadata.xml") + `secret">]>` +
		xmlDoc(``, `<version>&ext;</version>`)
	e.r.setDoc(metaPath, doc)
	if _, err := e.src.List(t.Context(), widget, source.FetchState{}); err == nil {
		t.Fatal("a document using an external entity was accepted")
	}
	if n := e.r.count(); n != 1 {
		t.Errorf("requests = %d (%v), want only the metadata", n, e.r.reqs())
	}
	// Declared but unused, it is not fetched either.
	e.r.reset()
	e.r.setDoc(metaPath, `<?xml version="1.0"?><!DOCTYPE metadata [<!ENTITY ext SYSTEM "http://192.0.2.1/x">]>`+xmlDoc(``, `<version>1.0</version>`))
	if _, err := e.src.List(t.Context(), widget, source.FetchState{}); err != nil {
		t.Fatal(err)
	}
	if n := e.r.count(); n != 1 {
		t.Errorf("requests = %d, want only the metadata", n)
	}
}

func TestSkippedVersionsLogOneDebugRecord(t *testing.T) {
	e := newEnv(t)
	e.r.setMeta("1.0", "a/b", "..", "x y")
	e.list(t, source.FetchState{})
	if n := strings.Count(e.log.String(), "level=DEBUG"); n != 1 || !strings.Contains(e.log.String(), "count=3") {
		t.Errorf("log = %q, want one DEBUG record counting 3", e.log.String())
	}
}

// More than 10,000 versions: the last 10,000 in metadata order are kept,
// with a WARN.
func TestVersionCap(t *testing.T) {
	e := newEnv(t)
	vs := make([]string, 0, 10005)
	for i := range 10005 {
		vs = append(vs, fmt.Sprintf("1.%d", i))
	}
	e.r.setMeta(vs...)
	res := e.list(t, source.FetchState{})
	if len(res.Versions) != 10000 || res.Versions[0].Raw != "1.10004" || res.Versions[9999].Raw != "1.5" {
		t.Fatalf("versions = %d, newest %s, oldest %s", len(res.Versions), res.Versions[0].Raw, res.Versions[len(res.Versions)-1].Raw)
	}
	if !strings.Contains(e.log.String(), "level=WARN") || !strings.Contains(e.log.String(), "versions=10005") {
		t.Errorf("no WARN: %s", e.log.String())
	}
}

// roundTrip serves requests in-process, so a Central base URL can be tested
// without the network.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func inProcess(h http.Handler, hosts *[]string) *http.Client {
	var mu sync.Mutex
	return &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		*hosts = append(*hosts, r.URL.Host+r.URL.Path)
		mu.Unlock()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Result(), nil
	})}
}

func TestURL(t *testing.T) {
	doc := xmlDoc(``, `<version>1.0</version>`)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/com/example/test/widget/maven-metadata.xml") {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(doc))
	})
	for base, want := range map[string]string{
		"":                            "https://central.sonatype.com/artifact/com.example.test/widget/1.0",
		maven.DefaultBaseURL:          "https://central.sonatype.com/artifact/com.example.test/widget/1.0",
		maven.CentralAltBaseURL + "/": "https://central.sonatype.com/artifact/com.example.test/widget/1.0",
		"https://nexus.example.test/repository/maven-releases": "",
	} {
		var hosts []string
		res, err := maven.New(base, inProcess(h, &hosts)).List(t.Context(), widget, source.FetchState{})
		if err != nil {
			t.Fatalf("%q: %v", base, err)
		}
		if res.Versions[0].URL != want || res.Versions[0].Yanked || res.Versions[0].Prerelease != nil {
			t.Errorf("%q: version %+v, want URL %q", base, res.Versions[0], want)
		}
		wantPath := strings.TrimPrefix(strings.TrimPrefix(strings.TrimRight(base, "/"), "https://"), "") + "/com/example/test/widget/maven-metadata.xml"
		if base == "" {
			wantPath = "repo.maven.apache.org/maven2/com/example/test/widget/maven-metadata.xml"
		}
		if len(hosts) != 1 || hosts[0] != wantPath {
			t.Errorf("%q: requests %v, want %s", base, hosts, wantPath)
		}
	}
	if !maven.IsCentral("HTTPS://REPO1.MAVEN.ORG/maven2/") || maven.IsCentral("https://repo.example.test/maven2") {
		t.Error("IsCentral")
	}
}

func TestNoClient(t *testing.T) {
	if _, err := maven.New("", nil).List(t.Context(), widget, source.FetchState{}); err == nil {
		t.Error("List without a client succeeded")
	}
	if _, err := maven.New("", &http.Client{}).List(t.Context(), target("bad"), source.FetchState{}); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("invalid target: %v", err)
	}
}
