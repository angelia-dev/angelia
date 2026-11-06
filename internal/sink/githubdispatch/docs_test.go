package githubdispatch

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var (
	fenceRE   = regexp.MustCompile("(?s)```yaml\n(.*?)```")
	payloadRE = regexp.MustCompile(`github\.event\.client_payload\.([a-z_]+)`)
)

// AC 3 of #113: the example workflow in docs/github-dispatch.md is valid YAML,
// listens for the default event_type, and reads only keys the sink sends.
func TestDocsExampleWorkflowConsumesThePayload(t *testing.T) {
	doc, err := os.ReadFile("../../../docs/github-dispatch.md")
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	for _, m := range fenceRE.FindAllStringSubmatch(string(doc), -1) {
		var v any
		if err := yaml.Unmarshal([]byte(m[1]), &v); err != nil {
			t.Fatalf("a yaml block of the docs does not parse: %v\n%s", err, m[1])
		}
		if top, ok := v.(map[string]any); ok && workflow == nil {
			if _, isWorkflow := top["jobs"]; isWorkflow {
				workflow = top
			}
		}
	}
	if workflow == nil {
		t.Fatal("no example workflow in docs/github-dispatch.md")
	}
	types := workflow["on"].(map[string]any)["repository_dispatch"].(map[string]any)["types"].([]any)
	if len(types) != 1 || types[0] != DefaultEventType {
		t.Errorf("the workflow listens for %v, the sink sends %q by default", types, DefaultEventType)
	}

	// What the sink sends, with every optional key present.
	srv, got := github(t, nil)
	env := testEnvelope()
	env.Data.Version = strings.Repeat("v", 3000) // so that truncated is present too
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, repoCfg(srv.URL, ""), env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	sent := decode(t, body)["client_payload"].(map[string]any)

	used := map[string]bool{}
	for _, m := range payloadRE.FindAllStringSubmatch(string(doc), -1) {
		used[m[1]] = true
		if _, ok := sent[m[1]]; !ok {
			t.Errorf("the docs read client_payload.%s, which the sink does not send", m[1])
		}
	}
	for _, k := range []string{"package", "version", "source", "previous_version", "bump", "release_url", "event_id", "watch"} {
		if !used[k] {
			t.Errorf("the example workflow does not show client_payload.%s", k)
		}
	}
	// The table of the docs lists exactly the keys sent.
	for k := range sent {
		if !strings.Contains(string(doc), "| `"+k+"` |") {
			t.Errorf("client_payload.%s is not in the docs table", k)
		}
	}
}
