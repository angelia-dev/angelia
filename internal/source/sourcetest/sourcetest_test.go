package sourcetest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	Run(t, newFake(), fixtures())
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
	for _, c := range []func(context.Context, source.Source, Fixtures) error{checkNotModified, checkPublishedAt, checkWantRaw} {
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

func TestNetworkGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()
	guardNetwork(t)

	get := func(url string) error {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := get(srv.URL); err != nil {
		t.Errorf("loopback blocked: %v", err)
	}
	if err := get("http://example.com/"); err == nil {
		t.Error("non-loopback request allowed")
	}
}
