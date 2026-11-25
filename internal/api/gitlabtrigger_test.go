package api

import (
	"net/http"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
)

func withGitlabTrigger(o *Options) {
	o.Plugins = append(o.Plugins, PluginInfo{Kind: "gitlab-pipeline-trigger", Interface: "sink", Runtime: "builtin", Version: "0.1.0"})
	o.API = config.API{AllowedEnv: []string{"GITLAB_TRIGGER_TOKEN"}}
}

// 90-C: a gitlab-pipeline-trigger sink is written through the API with the
// rules of the kind (spec decision 56): the token is a reference, a token_env
// must be on api.allowed_env, no token value is accepted.
func TestPutGitlabTriggerSink(t *testing.T) {
	e := newEnv(t, withGitlabTrigger)
	body := map[string]any{"kind": "gitlab-pipeline-trigger", "config": map[string]any{
		"project": "platform/deploys/app", "ref": "main", "token_env": "GITLAB_TRIGGER_TOKEN", "base_url": "https://gitlab.example.com",
		"inputs": map[string]any{"version": "{{.data.version}}"},
	}}
	r := e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/gitlab-ci", tokAdmin, body))
	cfg := r.json(t)["config"].(map[string]any)
	for k, want := range map[string]string{"project": "platform/deploys/app", "ref": "main", "token_env": "GITLAB_TRIGGER_TOKEN", "base_url": "https://gitlab.example.com"} {
		if cfg[k] != want {
			t.Errorf("config.%s = %v, want %q", k, cfg[k], want)
		}
	}

	bad := map[string]any{"kind": "gitlab-pipeline-trigger", "config": map[string]any{"project": "g/p", "ref": "main", "token_env": "OTHER_TOKEN"}}
	if r := e.do("PUT", "/v1/sinks/other", tokAdmin, bad); r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugEnvNotAllowed {
		t.Errorf("unlisted token_env: %d %s", r.Status, r.Body)
	}
	for name, cfg := range map[string]map[string]any{
		"no token":       {"project": "g/p", "ref": "main"},
		"both tokens":    {"project": "g/p", "ref": "main", "token_env": "GITLAB_TRIGGER_TOKEN", "token_ref": "gl"},
		"inline token":   {"project": "g/p", "ref": "main", "token": "glptt-x"},
		"ftp base":       {"project": "g/p", "ref": "main", "token_env": "GITLAB_TRIGGER_TOKEN", "base_url": "ftp://gitlab.example.com"},
		"no ref":         {"project": "g/p", "token_env": "GITLAB_TRIGGER_TOKEN"},
		"variable _ref":  {"project": "g/p", "ref": "main", "token_env": "GITLAB_TRIGGER_TOKEN", "variables": map[string]any{"DEPLOY_ref": "x"}},
		"input template": {"project": "g/p", "ref": "main", "token_env": "GITLAB_TRIGGER_TOKEN", "inputs": map[string]any{"a": "{{.data.pakage}}"}},
	} {
		if r := e.do("PUT", "/v1/sinks/bad", tokAdmin, map[string]any{"kind": "gitlab-pipeline-trigger", "config": cfg}); r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	ref := map[string]any{"kind": "gitlab-pipeline-trigger", "config": map[string]any{"project": "4711", "ref": "main", "token_ref": "gl-trigger"}}
	e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/by-ref", tokAdmin, ref))
}

// 90-C: the kind is listed as a built-in sink, and its config_schema is the
// one of the vendored spec (spec 0.11.0): the server reads it from
// SinkConfig.x-angelia-kind-config for write validation, and the destination
// key is base_url. (As for github-dispatch, GET /v1/plugins/... reports
// config_schema null for a built-in kind.)
func TestGitlabTriggerKindAndSchema(t *testing.T) {
	e := newEnv(t, withGitlabTrigger)
	r := e.must(http.StatusOK, e.do("GET", "/v1/plugins/sink/gitlab-pipeline-trigger", tokRead, nil))
	if j := r.json(t); j["kind"] != "gitlab-pipeline-trigger" || j["interface"] != "sink" || j["runtime"] != "builtin" {
		t.Errorf("plugin: %s", r.Body)
	}
	spec, err := loadSpec()
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{spec: spec, opts: Options{Plugins: []PluginInfo{{Kind: "gitlab-pipeline-trigger", Interface: "sink", Runtime: "builtin"}}}}
	sch, _ := s.configSchema("gitlab-pipeline-trigger")
	props, _ := sch["properties"].(map[string]any)
	for _, k := range []string{"project", "ref", "base_url", "token_env", "token_ref", "variables", "inputs", "timeout"} {
		if props[k] == nil {
			t.Errorf("config_schema has no property %s", k)
		}
	}
	if props["token"] != nil || sch["additionalProperties"] != false {
		t.Errorf("config_schema accepts a token or unknown keys: %v", sch["additionalProperties"])
	}
	got, err := SinkDestinations([]PluginInfo{{Kind: "gitlab-pipeline-trigger", Interface: "sink", Runtime: "builtin"}})
	if err != nil || got["gitlab-pipeline-trigger"] != "/base_url" {
		t.Errorf("SinkDestinations = %v, %v", got, err)
	}
}
