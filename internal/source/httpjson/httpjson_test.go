package httpjson_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/httpjson"
	"github.com/angelia-dev/angelia/internal/source/internal/generic"
	"github.com/angelia-dev/angelia/internal/source/sourcetest"
)

const ph = "{{server}}"

func tgt(kv ...string) source.Target {
	c := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		c[kv[i]] = kv[i+1]
	}
	return source.Target{Kind: httpjson.Kind, Coordinates: c}
}

// upstream serves testdata/<f> at /?f=<f> (latest.json at / without f)
// with a Last-Modified that changes with rev; each Change adds a release to
// both documents.
func upstream(t *testing.T, rev *atomic.Int64) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f := r.URL.Query().Get("f")
		if f == "" {
			f = "latest.json"
		}
		if r.URL.Path != "/" || strings.ContainsAny(f, "/\\") {
			http.NotFound(w, r)
			return
		}
		b, err := os.ReadFile("testdata/" + f)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		n := rev.Load()
		mod := time.Date(2026, 11, 1, 0, 0, int(n), 0, time.UTC).Format(http.TimeFormat)
		if r.Header.Get("If-Modified-Since") == mod {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		body := string(b)
		if n > 0 {
			body = strings.Replace(body, `"current_version": "2.1.0"`, fmt.Sprintf(`"current_version": "3.%d.0"`, n), 1)
			body = strings.Replace(body, `"releases": [`, fmt.Sprintf(`"releases": [{"name": "3.%d.0"},`, n), 1)
		}
		w.Header().Set("Last-Modified", mod)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func invalid() []source.Target {
	ok := []string{"name", "tool", "url", ph, "jsonpath", "$.current_version"}
	with := func(k, v string) source.Target {
		t := tgt(ok...)
		t.Coordinates[k] = v
		return t
	}
	without := func(k string) source.Target {
		t := tgt(ok...)
		delete(t.Coordinates, k)
		return t
	}
	wrong := tgt(ok...)
	wrong.Kind = "http-regex"
	return []source.Target{
		{Kind: httpjson.Kind}, wrong,
		without("name"), with("name", "two words"),
		without("url"), with("url", "http://example.org/x.json"), with("url", "https://u:p@example.org/"),
		without("jsonpath"), with("jsonpath", ""), with("jsonpath", "$..version"), with("jsonpath", "$[?@.v]"),
		with("extract", "both"), with("extract", ""), with("regex", "x"),
	}
}

func TestContractSingleField(t *testing.T) {
	t.Parallel()
	var rev atomic.Int64
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: upstream(t, &rev),
		New: func(_ string, c *http.Client) source.Source {
			return httpjson.New(c, httpjson.WithAllowHTTPHosts([]string{"127.0.0.1"}))
		},
		ServerURLPlaceholder: ph,
		Valid:                tgt("name", "tool", "url", ph, "jsonpath", "$.current_version"),
		Invalid:              invalid(),
		Missing:              &source.Target{Kind: httpjson.Kind, Coordinates: map[string]string{"name": "tool", "url": ph + "/?f=missing.json", "jsonpath": "$.v"}},
		WantRaw:              []string{"2.1.0"},
		Conditional:          true,
		Change:               func() { rev.Add(1) },
		Aliases: []source.Target{
			tgt("name", "tool", "url", ph+"/", "jsonpath", "$.current_version"), // canonical: an empty path is "/"
		},
	})
}

func TestContractListOfObjects(t *testing.T) {
	t.Parallel()
	var rev atomic.Int64
	sourcetest.Run(t, nil, sourcetest.Fixtures{
		Handler: upstream(t, &rev),
		New: func(_ string, c *http.Client) source.Source {
			return httpjson.New(c, httpjson.WithAllowHTTPHosts([]string{"127.0.0.1"}))
		},
		ServerURLPlaceholder: ph,
		Valid:                tgt("name", "tool", "url", ph+"?f=releases.json", "jsonpath", "$.releases[*].name", "extract", "values"),
		Invalid:              invalid(),
		Missing:              &source.Target{Kind: httpjson.Kind, Coordinates: map[string]string{"name": "tool", "url": ph + "?f=gone.json", "jsonpath": "$.v"}},
		// null is skipped, 1.10 keeps its literal text, the duplicate goes.
		WantRaw:     []string{"2.1.0", "2.0.0", "2.0.0-rc.1", "1.10"},
		Conditional: true,
		Change:      func() { rev.Add(1) },
		Aliases: []source.Target{
			tgt("name", "tool", "url", ph+"/?f=releases.json", "jsonpath", "$.releases[*].name", "extract", "values"),
		},
	})
}

func serve(t *testing.T, body string) (*httptest.Server, *httpjson.Source) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return srv, httpjson.New(srv.Client(), httpjson.WithAllowHTTPHosts([]string{"127.0.0.1"}))
}

func raws(vs []source.Version) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Raw
	}
	return strings.Join(out, ",")
}

func TestExtractKeys(t *testing.T) {
	b, err := os.ReadFile("testdata/releases.json")
	if err != nil {
		t.Fatal(err)
	}
	srv, s := serve(t, string(b))
	res, err := s.List(context.Background(), tgt("name", "tool", "url", srv.URL, "jsonpath", "$.by_version", "extract", "keys"), source.FetchState{})
	if err != nil || raws(res.Versions) != "1.9.3,2.0.0" {
		t.Errorf("keys: %v %v", res.Versions, err)
	}
	_, err = s.List(context.Background(), tgt("name", "tool", "url", srv.URL, "jsonpath", "$.releases", "extract", "keys"), source.FetchState{})
	if err == nil || !strings.Contains(err.Error(), "keys needs objects, but node 0 is array") {
		t.Errorf("keys of an array: %v", err)
	}
}

func TestValuesSkipOtherTypes(t *testing.T) {
	var buf strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`["1.0", 2, 1e3, true, null, {"v":"x"}, ["y"], "1.0"]`))
	}))
	defer srv.Close()
	s := httpjson.New(srv.Client(), httpjson.WithAllowHTTPHosts([]string{"127.0.0.1"}),
		httpjson.WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	res, err := s.List(context.Background(), tgt("name", "tool", "url", srv.URL, "jsonpath", "$[*]"), source.FetchState{})
	if err != nil || raws(res.Versions) != "1.0,2,1e3" || !strings.Contains(buf.String(), "skipped=4") {
		t.Errorf("%v %v %s", res.Versions, err, buf.String())
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	for body, want := range map[string]error{
		`{"v": true}`: generic.ErrNoVersions,
		`{}`:          generic.ErrNoVersions,
	} {
		srv, s := serve(t, body)
		if _, err := s.List(ctx, tgt("name", "tool", "url", srv.URL, "jsonpath", "$.v"), source.FetchState{}); !errors.Is(err, want) {
			t.Errorf("%s: %v", body, err)
		}
	}
	for _, body := range []string{`<html>`, `{"v":"1"} trailing`, `{"v":"1"}{}`, ``} {
		srv, s := serve(t, body)
		_, err := s.List(ctx, tgt("name", "tool", "url", srv.URL, "jsonpath", "$.v"), source.FetchState{})
		if err == nil || !strings.Contains(err.Error(), "the body from 127.0.0.1") || !strings.Contains(err.Error(), "is not JSON") {
			t.Errorf("%q: %v", body, err)
		}
	}
	var many strings.Builder
	many.WriteString("[")
	for i := range generic.MaxVersions + 1 {
		fmt.Fprintf(&many, `"%d",`, i)
	}
	srv, s := serve(t, strings.TrimSuffix(many.String(), ",")+"]")
	if _, err := s.List(ctx, tgt("name", "tool", "url", srv.URL, "jsonpath", "$[*]"), source.FetchState{}); !errors.Is(err, generic.ErrTooManyVersions) {
		t.Errorf("too many: %v", err)
	}
	srv, s = serve(t, "["+strings.TrimSuffix(strings.Repeat(`"1",`, generic.MaxMatches+1), ",")+"]")
	if _, err := s.List(ctx, tgt("name", "tool", "url", srv.URL, "jsonpath", "$[*]"), source.FetchState{}); !errors.Is(err, generic.ErrTooManyMatches) {
		t.Errorf("too many matches: %v", err)
	}
	srv, s = serve(t, `{"v":"1","pad":"`+strings.Repeat("x", generic.MaxBodyBytes)+`"}`)
	if _, err := s.List(ctx, tgt("name", "tool", "url", srv.URL, "jsonpath", "$.v"), source.FetchState{}); !errors.Is(err, netguard.ErrBodyTooLarge) {
		t.Errorf("body cap: %v", err)
	}
	srv404 := httptest.NewServer(http.NotFoundHandler())
	defer srv404.Close()
	var se *netguard.StatusError
	if _, err := httpjson.New(srv404.Client(), httpjson.WithAllowHTTPHosts([]string{"127.0.0.1"})).List(ctx, tgt("name", "tool", "url", srv404.URL, "jsonpath", "$"), source.FetchState{}); !errors.As(err, &se) {
		t.Errorf("404: %v", err)
	}
	if _, err := s.List(ctx, tgt("name", "tool"), source.FetchState{}); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("invalid target: %v", err)
	}
}

func TestSameVersionsWithoutValidatorsIsNotModified(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"served": %d, "version": "1.0"}`, n.Add(1))
	}))
	defer srv.Close()
	s := httpjson.New(srv.Client(), httpjson.WithAllowHTTPHosts([]string{"127.0.0.1"}))
	tg := tgt("name", "tool", "url", srv.URL, "jsonpath", "$.version")
	first, err := s.List(context.Background(), tg, source.FetchState{})
	if err != nil || first.NotModified {
		t.Fatalf("%+v %v", first, err)
	}
	second, err := s.List(context.Background(), tg, first.State)
	if err != nil || !second.NotModified || len(second.Versions) != 0 {
		t.Errorf("%+v %v", second, err)
	}
}

func TestCanonical(t *testing.T) {
	s := httpjson.New(nil)
	in := tgt("name", "Tool", "url", "HTTPS://Example.ORG:443?q=A", "jsonpath", "$ .v", "extract", "values")
	got, err := s.Canonical(in)
	if err != nil || got.Coordinates["url"] != "https://example.org/?q=A" || got.Coordinates["jsonpath"] != "$ .v" ||
		got.Coordinates["name"] != "Tool" || got.Coordinates["extract"] != "values" || in.Coordinates["url"] != "HTTPS://Example.ORG:443?q=A" {
		t.Errorf("%v %v", got, err)
	}
	if _, err := s.Canonical(tgt("name", "tool")); !errors.Is(err, source.ErrInvalidTarget) {
		t.Errorf("invalid: %v", err)
	}
	if _, err := s.List(context.Background(), tgt("name", "tool", "url", "https://example.org", "jsonpath", "$"), source.FetchState{}); err == nil {
		t.Error("nil client accepted")
	}
}
