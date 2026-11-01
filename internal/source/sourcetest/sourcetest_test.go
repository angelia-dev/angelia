package sourcetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
)

var errBoom = errors.New("boom")

func fixtures() Fixtures {
	return Fixtures{
		Valid: source.Target{Kind: "fake", Coordinates: map[string]string{"name": "pkg"}},
		Invalid: []source.Target{
			{Kind: "fake"},
			{Kind: "fake", Coordinates: map[string]string{"name": "a/b"}},
			{Kind: "other", Coordinates: map[string]string{"name": "pkg"}},
		},
		WantRaw:        []string{"v2.0.0", "v1.0.0"},
		Conditional:    true,
		HasPublishedAt: true,
	}
}

func newFake() *Fake {
	f := NewFake("fake")
	f.Set("pkg",
		source.Version{Raw: "v2.0.0", PublishedAt: At(2), Prerelease: Ptr(false)},
		source.Version{Raw: "v1.0.0", PublishedAt: At(1), Metadata: map[string]any{"digest": "sha256:x"}},
	)
	return f
}

// TestFakePassesContract is the acceptance test: the in-memory fake passes
// the full suite.
func TestFakePassesContract(t *testing.T) {
	f := newFake()
	fx := fixtures()
	missing := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "missing"}}
	fx.Missing = &missing
	fx.Change = func() { f.Set("pkg", source.Version{Raw: "v3.0.0", PublishedAt: At(3)}) }
	Run(t, f, fx)
}

// httpSource is a minimal HTTP source: GET <base>/<name> returns one raw
// version per line, with ETag support.
type httpSource struct {
	base       string
	c          *http.Client
	leakyCheck bool // Validate sends a request (violation)
}

func (h httpSource) Kind() string { return "fake" }

func (h httpSource) Validate(t source.Target) error {
	if h.leakyCheck {
		if resp, err := h.c.Get(h.base + "/pkg"); err == nil {
			_ = resp.Body.Close()
		}
	}
	if t.Kind != "fake" || t.Coordinates["name"] == "" || strings.Contains(t.Coordinates["name"], "/") {
		return fmt.Errorf("bad target: %w", source.ErrInvalidTarget)
	}
	return nil
}

func (h httpSource) List(ctx context.Context, t source.Target, s source.FetchState) (source.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.base+"/"+t.Coordinates["name"], http.NoBody)
	if err != nil {
		return source.Result{}, fmt.Errorf("build request: %w", err)
	}
	if s.ETag != "" {
		req.Header.Set("If-None-Match", s.ETag)
	}
	resp, err := h.c.Do(req)
	if err != nil {
		return source.Result{}, fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	state := source.FetchState{ETag: resp.Header.Get("ETag")}
	switch resp.StatusCode {
	case http.StatusNotModified:
		return source.Result{State: s, NotModified: true}, nil // keep the state
	case http.StatusOK:
	default:
		return source.Result{}, fmt.Errorf("fetch: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return source.Result{}, fmt.Errorf("read: %w", err)
	}
	var vs []source.Version
	for _, line := range strings.Fields(string(body)) {
		vs = append(vs, source.Version{Raw: line, PublishedAt: At(1)})
	}
	return source.Result{Versions: vs, State: state}, nil
}

func upstream(rev *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pkg" {
			http.NotFound(w, r)
			return
		}
		etag := `"` + strconv.FormatInt(rev.Load(), 10) + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_, _ = io.WriteString(w, "v2.0.0\nv1.0.0\n")
	})
}

func httpFixtures(rev *atomic.Int64, leaky bool) Fixtures {
	fx := fixtures()
	missing := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "missing"}}
	fx.Missing = &missing
	fx.Handler = upstream(rev)
	fx.Change = func() { rev.Add(1) }
	fx.New = func(base string, c *http.Client) source.Source {
		return httpSource{base: base, c: c, leakyCheck: leaky}
	}
	return fx
}

// TestHTTPSourcePassesContract runs the suite with an owned server and the
// guarded client, in parallel with other tests (no global state).
func TestHTTPSourcePassesContract(t *testing.T) {
	t.Parallel()
	var rev atomic.Int64
	Run(t, nil, httpFixtures(&rev, false))
}

func TestValidateRequestsDetected(t *testing.T) {
	var rev atomic.Int64
	srv := httptest.NewServer(upstream(&rev))
	defer srv.Close()
	c := Client(t, srv)
	src := httpSource{base: srv.URL, c: c, leakyCheck: true}
	if err := checkNoRequests(context.Background(), src, fixtures(), c); err == nil {
		t.Error("Validate request not detected")
	}
	if err := checkNoRequests(context.Background(), src, fixtures(), nil); err != nil {
		t.Errorf("nil client must skip: %v", err)
	}
}

// recorder captures Errorf so tests can assert a guard violation.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func TestClientGuard(t *testing.T) {
	var rev atomic.Int64
	srv := httptest.NewServer(upstream(&rev))
	defer srv.Close()
	rec := &recorder{TB: t}
	c := Client(rec, srv)

	get := func(url string) error {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := c.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := get(srv.URL + "/pkg"); err != nil || len(rec.errs) != 0 || Requests(c) != 1 {
		t.Fatalf("allowed request: err=%v errs=%v n=%d", err, rec.errs, Requests(c))
	}
	if err := get("http://example.com/"); err == nil {
		t.Error("foreign host allowed")
	}
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "example.com") {
		t.Errorf("guard did not fail the test clearly: %v", rec.errs)
	}
	if Requests(c) != 1 {
		t.Errorf("blocked request was counted: %d", Requests(c))
	}
	if Requests(http.DefaultClient) != -1 {
		t.Error("Requests on a foreign client must be -1")
	}
}

func TestDialControl(t *testing.T) {
	ctl := dialControl("127.0.0.1:1")
	if err := ctl("tcp", "127.0.0.1:1", nil); err != nil {
		t.Errorf("allowed address rejected: %v", err)
	}
	if err := ctl("tcp", "93.184.216.34:80", nil); err == nil {
		t.Error("foreign address accepted")
	}
}

type listFunc = func(context.Context, source.Target, source.FetchState) (source.Result, error)

// broken wraps a Fake and breaks one behaviour per test.
type broken struct {
	*Fake
	validate func(source.Target) error
	list     listFunc
}

func (b broken) Validate(t source.Target) error {
	if b.validate != nil {
		return b.validate(t)
	}
	return b.Fake.Validate(t)
}

func (b broken) List(ctx context.Context, t source.Target, s source.FetchState) (source.Result, error) {
	if b.list != nil {
		return b.list(ctx, t, s)
	}
	return b.Fake.List(ctx, t, s)
}

func fixedList(r source.Result, err error) listFunc {
	return func(context.Context, source.Target, source.FetchState) (source.Result, error) {
		return r, err
	}
}

func TestChecksDetectViolations(t *testing.T) {
	f := newFake()
	one := []source.Version{{Raw: "1"}}
	etag := source.FetchState{ETag: "x"}
	n := 0
	var calls atomic.Int64
	tests := []struct {
		name  string
		check func(context.Context, source.Source, Fixtures) error
		src   source.Source
		fx    func(*Fixtures)
	}{
		{"empty kind", checkKind, NewFake(""), nil},
		{"fixture kind mismatch", checkKind, NewFake("zzz"), nil},
		{"validate rejects valid", checkValidateAccepts, broken{Fake: f, validate: func(source.Target) error { return source.ErrInvalidTarget }}, nil},
		{"validate accepts invalid", checkValidateRejects, broken{Fake: f, validate: func(source.Target) error { return nil }}, nil},
		{"validate unwrapped error", checkValidateRejects, broken{Fake: f, validate: func(source.Target) error { return errBoom }}, nil},
		{"no invalid fixtures", checkValidateRejects, f, func(fx *Fixtures) { fx.Invalid = nil }},
		{"list error", checkDeterministic, broken{Fake: f, list: fixedList(source.Result{}, errBoom)}, nil},
		{"list empty", checkDeterministic, broken{Fake: f, list: fixedList(source.Result{}, nil)}, nil},
		{"list empty raw", checkDeterministic, broken{Fake: f, list: fixedList(source.Result{Versions: []source.Version{{}}}, nil)}, nil},
		{"list not modified on empty state", checkDeterministic, broken{Fake: f, list: fixedList(source.Result{NotModified: true}, nil)}, nil},
		{"list nondeterministic", checkDeterministic, broken{Fake: f, list: func(context.Context, source.Target, source.FetchState) (source.Result, error) {
			n++
			return source.Result{Versions: []source.Version{{Raw: strconv.Itoa(n)}}}, nil
		}}, nil},
		{"wrong raw", checkWantRaw, f, func(fx *Fixtures) { fx.WantRaw = []string{"nope"} }},
		{"want raw list error", checkWantRaw, broken{Fake: f, list: fixedList(source.Result{}, errBoom)}, nil},
		{"ignores context", checkContext, broken{Fake: f, list: fixedList(source.Result{}, nil)}, nil},
		{"context error not wrapped", checkContext, broken{Fake: f, list: fixedList(source.Result{}, errBoom)}, nil},
		{"zero state", checkNotModified, broken{Fake: f, list: fixedList(source.Result{Versions: one}, nil)}, nil},
		{"not modified list error", checkNotModified, broken{Fake: f, list: fixedList(source.Result{}, errBoom)}, nil},
		{"not modified missing", checkNotModified, broken{Fake: f, list: fixedList(source.Result{Versions: one, State: etag}, nil)}, nil},
		{"not modified with versions", checkNotModified, broken{Fake: f, list: func(_ context.Context, _ source.Target, s source.FetchState) (source.Result, error) {
			return source.Result{Versions: one, State: etag, NotModified: !s.IsZero()}, nil
		}}, nil},
		{"not modified drops state", checkNotModified, broken{Fake: f, list: func(_ context.Context, _ source.Target, s source.FetchState) (source.Result, error) {
			if s.IsZero() {
				return source.Result{Versions: one, State: etag}, nil
			}
			return source.Result{NotModified: true}, nil
		}}, nil},
		{"missing target returns empty", checkMissing, broken{Fake: f, list: fixedList(source.Result{}, nil)}, func(fx *Fixtures) {
			m := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "x"}}
			fx.Missing = &m
		}},
		{"concurrent list error", checkConcurrent, broken{Fake: f, list: fixedList(source.Result{}, errBoom)}, nil},
		{"concurrent nondeterministic", checkConcurrent, broken{Fake: f, list: func(context.Context, source.Target, source.FetchState) (source.Result, error) {
			return source.Result{Versions: []source.Version{{Raw: strconv.FormatInt(calls.Add(1), 10)}}}, nil
		}}, nil},
		{"always not modified", checkChange, broken{Fake: f, list: func(_ context.Context, _ source.Target, s source.FetchState) (source.Result, error) {
			if s.IsZero() {
				return source.Result{Versions: one, State: etag}, nil
			}
			return source.Result{NotModified: true, State: etag}, nil
		}}, func(fx *Fixtures) { fx.Change = func() {} }},
		{"change returns nothing", checkChange, broken{Fake: f, list: func(_ context.Context, _ source.Target, s source.FetchState) (source.Result, error) {
			if s.IsZero() {
				return source.Result{Versions: one, State: etag}, nil
			}
			return source.Result{State: etag}, nil
		}}, func(fx *Fixtures) { fx.Change = func() {} }},
		{"change list error", checkChange, broken{Fake: f, list: fixedList(source.Result{}, errBoom)}, func(fx *Fixtures) { fx.Change = func() {} }},
		{"missing published at", checkPublishedAt, broken{Fake: f, list: fixedList(source.Result{Versions: one}, nil)}, nil},
		{"published at list error", checkPublishedAt, broken{Fake: f, list: fixedList(source.Result{}, errBoom)}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := fixtures()
			if tc.fx != nil {
				tc.fx(&fx)
			}
			if err := tc.check(context.Background(), tc.src, fx); err == nil {
				t.Fatal("check passed, want a violation")
			}
		})
	}
}

func TestOptionalChecksSkipped(t *testing.T) {
	fx := fixtures()
	fx.Conditional, fx.HasPublishedAt, fx.WantRaw = false, false, nil
	ctx := context.Background()
	for _, c := range []func(context.Context, source.Source, Fixtures) error{checkNotModified, checkPublishedAt, checkWantRaw, checkMissing, checkChange} {
		if err := c(ctx, NewFake("fake"), fx); err != nil {
			t.Errorf("optional check failed: %v", err)
		}
	}
}

func TestFakeListErrors(t *testing.T) {
	f := newFake()
	ctx := context.Background()
	if _, err := f.List(ctx, source.Target{Kind: "fake", Coordinates: map[string]string{"name": "missing"}}, source.FetchState{}); err == nil {
		t.Error("unknown package listed without error")
	}
	if _, err := f.List(ctx, source.Target{Kind: "fake"}, source.FetchState{}); err == nil {
		t.Error("invalid target listed without error")
	}
}

func TestFakeIsolation(t *testing.T) {
	f := NewFake("fake")
	meta := map[string]any{"tags": []any{"a"}}
	pre := true
	f.Set("pkg", source.Version{Raw: "1", Prerelease: &pre, Metadata: meta})
	meta["tags"].([]any)[0] = "mutated" // caller mutates its own input
	pre = false
	tgt := source.Target{Kind: "fake", Coordinates: map[string]string{"name": "pkg"}}
	res, err := f.List(context.Background(), tgt, source.FetchState{})
	if err != nil {
		t.Fatal(err)
	}
	v := res.Versions[0]
	if !*v.Prerelease || v.Metadata["tags"].([]any)[0] != "a" {
		t.Fatalf("Set did not copy its input: %+v", v)
	}
	*v.Prerelease = false
	v.Metadata["tags"].([]any)[0] = "x" // caller mutates the result
	again, _ := f.List(context.Background(), tgt, source.FetchState{})
	if !*again.Versions[0].Prerelease || again.Versions[0].Metadata["tags"].([]any)[0] != "a" {
		t.Fatal("List result aliases internal state")
	}
	// per-package revisions: bumping another package keeps this ETag.
	f.Set("other", source.Version{Raw: "1"})
	res2, _ := f.List(context.Background(), tgt, res.State)
	if !res2.NotModified {
		t.Fatal("unrelated package bumped this package's ETag")
	}
}
