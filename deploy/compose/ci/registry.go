package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/bench/fakereg"
)

// The fake registry has one package, pkg-00000, that releases once an hour on
// a fixed phase. History versions exist at start. Each POST /_ci/publish
// moves the registry's clock forward by one hour, so exactly one more version
// appears, and answers with it.
const (
	registryHistory = 2
	publishStep     = time.Hour
)

// registry is the fake PyPI plus the publish control.
type registry struct {
	reg http.Handler
	mux *http.ServeMux

	mu      sync.Mutex
	offset  time.Duration // added to the wall clock
	version int           // index of the newest version
}

func newRegistry() (http.Handler, error) {
	r := &registry{mux: http.NewServeMux(), version: registryHistory - 1}
	cfg := fakereg.DefaultConfig()
	b := fakereg.DefaultBehaviour()
	b.Packages, b.History = 1, registryHistory
	b.ReleasesPerHour = float64(time.Hour / publishStep)
	b.Latency, b.LatencyJitter = 0, 0
	for _, e := range fakereg.Ecosystems() {
		cfg.Ecosystems[e] = b
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r.reg = fakereg.New(cfg, fakereg.WithClock(r.now))
	r.mux.HandleFunc("POST /_ci/publish", r.publish)
	r.mux.Handle("/", r.reg)
	return r.mux, nil
}

func (r *registry) now() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Add(r.offset)
}

func (r *registry) publish(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	r.offset += publishStep
	r.version++
	v := fakereg.Version(r.version)
	r.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"package": fakereg.PackageName(0), "version": v})
}
