package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

	base func() time.Time

	mu     sync.Mutex
	offset time.Duration // added to the base clock
}

func newRegistry() (http.Handler, error) { return newRegistryAt(time.Now) }

// newRegistryAt is newRegistry with an injectable base clock.
func newRegistryAt(base func() time.Time) (http.Handler, error) {
	r := &registry{mux: http.NewServeMux(), base: base}
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
	return r.base().Add(r.offset)
}

// publish moves the clock forward and answers with the newest version the
// registry then serves, read back from the registry itself.
func (r *registry) publish(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	r.offset += publishStep
	r.mu.Unlock()
	rec := httptest.NewRecorder()
	r.reg.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/pypi/pypi/"+fakereg.PackageName(0)+"/json", nil))
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &doc) != nil || doc.Info.Version == "" {
		http.Error(w, "cannot read the published version back", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"package": fakereg.PackageName(0), "version": doc.Info.Version})
}
