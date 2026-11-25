package gitlabtrigger

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/sink"
)

var (
	fenceRE = regexp.MustCompile("(?s)```yaml\n(.*?)```")
	envRE   = regexp.MustCompile(`ANGELIA_[A-Z_]+`)
)

// 90-C: docs/gitlab-pipeline-trigger.md is consistent with the sink. Every
// yaml block parses; every sink configuration in it validates; the example
// pipelines read only variables the sink sends, and the table lists exactly
// the variables it sends; the example listens for the "trigger" source.
func TestDocsMatchTheSink(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/gitlab-pipeline-trigger.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	s := New(guardedNoServer(t), Options{})

	var configs, pipelines int
	for _, m := range fenceRE.FindAllStringSubmatch(doc, -1) {
		dec := yaml.NewDecoder(strings.NewReader(m[1]))
		for {
			var v map[string]any
			if err := dec.Decode(&v); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("a yaml block of the docs does not parse: %v\n%s", err, m[1])
			}
			var cfg any
			switch {
			case v["sinks"] != nil:
				cfg = v["sinks"].([]any)[0].(map[string]any)["config"]
			case v["config"] != nil:
				cfg = v["config"]
			case v["react"] != nil || v["workflow"] != nil:
				pipelines++
				if !strings.Contains(m[1], `$CI_PIPELINE_SOURCE == "trigger"`) {
					t.Errorf("a pipeline example does not select trigger pipelines:\n%s", m[1])
				}
				continue
			default:
				continue
			}
			b, err := json.Marshal(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Validate(sink.Config(b)); err != nil {
				t.Errorf("a configuration of the docs is invalid: %v\n%s", err, b)
			}
			configs++
		}
	}
	if configs < 2 || pipelines < 2 {
		t.Fatalf("found %d configurations and %d pipelines in the docs", configs, pipelines)
	}

	// The variables the sink sends, with every optional one cut.
	srv, got := gitlab(t, nil)
	env := testEnvelope()
	env.Data.Version = strings.Repeat("v", 3000)
	if _, err := newSink(t, srv).Deliver(t.Context(), request(t, cfgOf(srv.URL, ""), env)); err != nil {
		t.Fatal(err)
	}
	_, body := got.last()
	sent, _ := decode(t, body)
	for _, name := range envRE.FindAllString(doc, -1) {
		if _, ok := sent.Variables[name]; !ok {
			t.Errorf("the docs mention %s, which the sink does not send", name)
		}
	}
	for k := range sent.Variables {
		if !bytes.Contains(raw, []byte("| `"+k+"` |")) {
			t.Errorf("%s is not in the docs table", k)
		}
	}
}
