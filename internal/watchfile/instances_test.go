package watchfile

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/source/pypi"
)

// Named source instances (#205): a watch selects a configured instance by its
// name, which is stored as the target's kind (two instances never share a
// target); its scheme and canonical coordinates come from the base kind; a
// name that is neither a kind nor an instance is refused.
func instanceOpts(t *testing.T) Options {
	t.Helper()
	o := opts(t)
	if err := o.Sources.RegisterAs("pypi-internal", pypi.New("https://pypi.internal.example.org", &http.Client{})); err != nil {
		t.Fatal(err)
	}
	return o
}

const instanceDoc = `version: 1
watches:
  - {name: public, source: pypi, coordinates: {name: Requests}}
  - {name: internal, source: pypi-internal, coordinates: {name: Requests}}
`

func TestNamedSourceInstanceInWatchFile(t *testing.T) {
	f, err := Parse([]byte(instanceDoc), "w.yaml", instanceOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	w := f.Watches[1]
	if w.Source != "pypi-internal" || w.Kind != "pypi" || w.SourceKind() != "pypi" {
		t.Errorf("Source %q Kind %q, want pypi-internal on pypi", w.Source, w.Kind)
	}
	if w.Coordinates["name"] != "requests" {
		t.Errorf("coordinates = %v, want the base kind's canonical form", w.Coordinates)
	}
	if !strings.Contains(string(w.PolicyJSON()), `"scheme":"pep440"`) {
		t.Errorf("policy %s: want the scheme of pypi", w.PolicyJSON())
	}

	st := openStore(t)
	if _, err := Apply(t.Context(), st, f, ApplyOptions{Now: func() time.Time { return clock }}); err != nil {
		t.Fatal(err)
	}
	coords := map[string]string{"name": "requests"}
	pub, err := target(t, st, "pypi", coords)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := target(t, st, "pypi-internal", coords)
	if err != nil {
		t.Fatal(err)
	}
	if pub.ID == inst.ID || inst.Kind != "pypi-internal" {
		t.Errorf("the public registry (%s) and the instance (%s, kind %q) must be two targets", pub.ID, inst.ID, inst.Kind)
	}
}

func TestUnknownSourceInstanceIsRefused(t *testing.T) {
	_, err := Parse([]byte("version: 1\nwatches:\n  - {name: a, source: pypi-elsewhere, coordinates: {name: x}}\n"), "w.yaml", instanceOpts(t))
	if err == nil || !strings.Contains(err.Error(), "watches[0].source: must be one of") || !strings.Contains(err.Error(), "pypi-internal") {
		t.Fatalf("err = %v, want a refusal listing the known names", err)
	}
}

// A subscription filter names the kind events carry, not an instance.
func TestSubscriptionFilterTakesKinds(t *testing.T) {
	doc := func(src string) string {
		return "version: 1\nsinks:\n  - {name: s, kind: webhook, config: {url: 'https://hook.example.org/x', secret_env: S}}\nsubscriptions:\n  - {name: a, sink: s, filter: {source: [" + src + "]}}\n"
	}
	if _, err := Parse([]byte(doc("pypi")), "w.yaml", instanceOpts(t)); err != nil {
		t.Fatalf("kind: %v", err)
	}
	if _, err := Parse([]byte(doc("pypi-internal")), "w.yaml", instanceOpts(t)); err == nil {
		t.Fatal("an instance name is accepted in a filter: events carry the kind")
	}
}
