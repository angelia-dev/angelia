package api

import (
	"net/http"
	"testing"

	"github.com/angelia-dev/angelia/internal/source/pypi"
)

// Named source instances (#205): a watch selects an instance by its name; the
// API stores that name as the target's kind and answers with it, takes the
// version scheme from the instance's base kind, and refuses a name that is
// neither a kind nor a configured instance.
func TestWatchOnNamedSourceInstance(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		if err := o.Sources.RegisterAs("pypi-internal", pypi.New("https://pypi.internal.example.org", &http.Client{})); err != nil {
			t.Fatal(err)
		}
	})
	w := clone(watchPut)
	w["source"] = "pypi-internal"
	r := e.must(http.StatusCreated, e.do("PUT", "/v1/watches/requests", tokWrite, w))
	got := r.json(t)
	if got["source"] != "pypi-internal" {
		t.Errorf("source = %v, want the instance name", got["source"])
	}
	if got["scheme"] != "pep440" {
		t.Errorf("scheme = %v, want the default of the base kind pypi (pep440)", got["scheme"])
	}
	again := e.must(http.StatusOK, e.do("GET", "/v1/watches/requests", tokRead, nil)).json(t)
	if again["source"] != "pypi-internal" {
		t.Errorf("GET source = %v", again["source"])
	}
	e.must(http.StatusOK, e.do("GET", "/v1/watches/requests/versions", tokRead, nil))

	w["source"] = "pypi-elsewhere"
	if r := e.do("PUT", "/v1/watches/other", tokWrite, w); r.Status != http.StatusUnprocessableEntity {
		t.Fatalf("unknown instance: status %d: %s", r.Status, r.Body)
	}
}
