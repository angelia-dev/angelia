package api

import (
	"net/http"
	"testing"

	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/httpjson"
	"github.com/angelia-dev/angelia/internal/source/httpregex"
)

// withURLSources adds the generic HTTP sources to the test registry.
func withURLSources(t *testing.T) func(*Options) {
	return func(o *Options) {
		reg := testSources(t)
		for _, s := range []source.Source{httpjson.New(&http.Client{}), httpregex.New(&http.Client{})} {
			if err := reg.Register(s); err != nil {
				t.Fatal(err)
			}
		}
		o.Sources = reg
	}
}

func jsonWatch(url string) map[string]any {
	return map[string]any{"source": "http-json", "coordinates": map[string]string{
		"name": "tool", "url": url, "jsonpath": "$.version",
	}}
}

// #97 D52: a write token cannot create or retarget a watch of http-json or
// http-regex; an admin token can; label, policy, scheme and interval
// changes keep the write scope.
func TestURLSourceWatchesNeedAdmin(t *testing.T) {
	e := newEnv(t, withURLSources(t))
	forbidden := func(r result, what string) {
		t.Helper()
		if r.Status != http.StatusForbidden || r.problemType(t) != SlugForbidden || r.json(t)["required_scope"] != "admin" {
			t.Fatalf("%s: %d %s", what, r.Status, r.Body)
		}
	}
	regex := map[string]any{"source": "http-regex", "coordinates": map[string]string{
		"name": "tool", "url": "https://downloads.example.org/", "regex": `tool-([0-9.]+)`,
	}}
	forbidden(e.do("PUT", "/v1/watches/tool", tokWrite, jsonWatch("https://releases.example.org/tool.json")), "write create http-json")
	forbidden(e.do("PUT", "/v1/watches/tool?dry_run=true", tokWrite, jsonWatch("https://releases.example.org/tool.json")), "write dry-run create")
	forbidden(e.do("PUT", "/v1/watches/tool", tokWrite, regex), "write create http-regex")
	if r := e.do("GET", "/v1/watches/tool", tokRead, nil); r.Status != http.StatusNotFound {
		t.Fatalf("a refused create stored the watch: %d", r.Status)
	}

	e.must(http.StatusCreated, e.do("PUT", "/v1/watches/tool", tokAdmin, jsonWatch("https://releases.example.org/tool.json")))

	// Same target spelled differently (canonical URL): not a retarget.
	same := jsonWatch("HTTPS://Releases.Example.ORG:443/tool.json")
	same["labels"] = map[string]string{"team": "platform"}
	same["interval"] = "6h"
	same["scheme"] = "semver"
	same["policy"] = map[string]any{"prerelease": "none"}
	e.must(http.StatusOK, e.do("PUT", "/v1/watches/tool", tokWrite, same))

	// Retargeting: another URL, another expression, another kind.
	forbidden(e.do("PUT", "/v1/watches/tool", tokWrite, jsonWatch("https://releases.example.org/other.json")), "write changes url")
	other := jsonWatch("https://releases.example.org/tool.json")
	other["coordinates"].(map[string]string)["jsonpath"] = "$.other"
	forbidden(e.do("PUT", "/v1/watches/tool", tokWrite, other), "write changes jsonpath")
	forbidden(e.do("PUT", "/v1/watches/tool", tokWrite, regex), "write changes the kind")
	forbidden(e.do("PUT", "/v1/watches/tool", tokWrite, map[string]any{"source": "pypi", "coordinates": map[string]string{"name": "requests"}}), "write moves it to pypi")

	// A pypi watch: write may create it and change it, but not point it at
	// http-json.
	e.must(http.StatusCreated, e.do("PUT", "/v1/watches/req", tokWrite, map[string]any{"source": "pypi", "coordinates": map[string]string{"name": "requests"}}))
	e.must(http.StatusOK, e.do("PUT", "/v1/watches/req", tokWrite, map[string]any{"source": "pypi", "coordinates": map[string]string{"name": "flask"}}))
	forbidden(e.do("PUT", "/v1/watches/req", tokWrite, jsonWatch("https://releases.example.org/tool.json")), "write pypi to http-json")
	e.must(http.StatusOK, e.do("PUT", "/v1/watches/req", tokAdmin, jsonWatch("https://releases.example.org/tool.json")))

	// Deleting needs only write, as for any watch.
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/watches/req", tokWrite, nil))
}

// The URL rules of the source apply to API writes too (no I/O: Validate).
func TestURLSourceWatchValidation(t *testing.T) {
	e := newEnv(t, withURLSources(t))
	for _, u := range []string{"http://releases.example.org/x", "https://u:p@releases.example.org/x", "https://releases.example.org/#x", "https://2130706433/"} {
		r := e.do("PUT", "/v1/watches/tool", tokAdmin, jsonWatch(u))
		if r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", u, r.Status, r.Body)
		}
	}
}
