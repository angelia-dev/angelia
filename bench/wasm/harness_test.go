package wasm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"reflect"
	"testing"

	"github.com/angelia-dev/angelia/bench/plugins"
	"github.com/angelia-dev/angelia/bench/plugins/goplugin"
	"github.com/angelia-dev/angelia/bench/plugins/goplugin/goplugintest"
	"github.com/angelia-dev/angelia/internal/netguard"
	plugin "github.com/angelia-dev/angelia/internal/plugin/wasm"
	"github.com/angelia-dev/angelia/internal/plugin/wasm/wasmtest"
	"github.com/angelia-dev/angelia/internal/source"
)

const benchHost = "crates.bench.test"

// The go-plugin source runs this test binary as its plugin process.
func TestMain(m *testing.M) {
	if goplugintest.ServeIfChild() {
		return
	}
	os.Exit(m.Run())
}

// inMemory serves requests from a handler without sockets, so the
// benchmark measures the plugin runtime, not the loopback stack.
type inMemory struct{ h http.Handler }

func (m inMemory) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body == nil {
		r.Body = http.NoBody
	}
	rec := httptest.NewRecorder()
	m.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

type resolver struct{}

func (resolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{netip.MustParseAddr("203.0.113.20")}, nil
}

// workload is one benchmark scenario.
type workload struct {
	name     string
	versions int // per crate
	pageSize int
	notMod   bool // List with the state of a previous call (304 path)
}

var workloads = []workload{
	{name: "1page-10versions", versions: 10, pageSize: 100},
	{name: "1page-100versions", versions: 100, pageSize: 100},
	{name: "3pages-300versions", versions: 300, pageSize: 100},
	{name: "304-not-modified", versions: 300, pageSize: 100, notMod: true},
}

type impl struct {
	name string
	src  source.Source
}

// impls are the implementations of the same crates source: native first,
// then the plugin runtimes compared against it.
type impls []impl

// env builds every implementation over the same netguard client and fake
// upstream. The go-plugin source sends its HTTP back to the core through
// the Host service, so it reaches the same in-memory upstream.
func env(tb testing.TB, w workload) (srcs impls, st source.FetchState) {
	tb.Helper()
	up := wasmtest.NewCratesUpstream(w.pageSize, map[string]int{"serde": w.versions})
	limit := netguard.Limit{Rate: 1e9, Burst: 1 << 20}
	nc, err := netguard.New(netguard.Config{
		Transport:   inMemory{up},
		Resolver:    resolver{},
		HostLimits:  map[string]netguard.HostLimit{benchHost: {Authenticated: limit, Anonymous: limit}},
		MaxAttempts: 1,
	})
	if err != nil {
		tb.Fatal(err)
	}
	hc := nc.HTTPClient()
	base := "https://" + benchHost

	m, err := plugin.ParseManifest(plugins.CratesManifest())
	if err != nil {
		tb.Fatal(err)
	}
	m.AllowedHosts = []string{benchHost}
	ws, err := plugin.New(context.Background(), m, plugins.CratesWasm(), plugin.Options{
		HTTPClient: hc,
		Config:     map[string]string{"base_url": base},
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ws.Close(context.Background()) })
	gs, err := goplugin.New(context.Background(), goplugin.Options{
		Kind:         "crates",
		Command:      goplugintest.Command,
		AllowedHosts: []string{benchHost},
		HTTPClient:   hc,
		Config:       map[string]string{"base_url": base},
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(gs.Close)
	ns := NewNativeCrates(base, hc)

	if w.notMod {
		r, err := ns.List(context.Background(), target, source.FetchState{})
		if err != nil {
			tb.Fatal(err)
		}
		st = r.State
	}
	return impls{{"native", ns}, {"wasm", ws}, {"goplugin", gs}}, st
}

var target = source.Target{Kind: "crates", Coordinates: map[string]string{"name": "serde"}}

// The implementations must return the same result, or the benchmark
// compares different work.
func TestImplementationsAgree(t *testing.T) {
	for _, w := range workloads {
		t.Run(w.name, func(t *testing.T) {
			srcs, st := env(t, w)
			want, err := srcs[0].src.List(t.Context(), target, st)
			if err != nil {
				t.Fatal(err)
			}
			for _, im := range srcs[1:] {
				got, err := im.src.List(t.Context(), target, st)
				if err != nil {
					t.Fatalf("%s: %v", im.name, err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("native and %s differ:\nnative: %+v\n%s: %+v", im.name, want, im.name, got)
				}
			}
		})
	}
}

func BenchmarkList(b *testing.B) {
	for _, w := range workloads {
		srcs, st := env(b, w)
		for _, im := range srcs {
			b.Run(w.name+"/"+im.name, func(b *testing.B) {
				for b.Loop() {
					if _, err := im.src.List(context.Background(), target, st); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
