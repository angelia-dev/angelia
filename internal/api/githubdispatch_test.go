package api

import (
	"net/http"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
)

func withGithubDispatch(o *Options) {
	o.Plugins = append(o.Plugins, PluginInfo{Kind: "github-dispatch", Interface: "sink", Runtime: "builtin", Version: "0.1.0"})
	o.API = config.API{AllowedEnv: []string{"GH_TOKEN"}}
}

// A github-dispatch sink is written through the API with the rules of the
// kind (#113, spec decision 38): the token is a reference, a token_env must
// be on api.allowed_env, and the defaults are stored.
func TestPutGithubDispatchSink(t *testing.T) {
	e := newEnv(t, withGithubDispatch)
	body := map[string]any{"kind": "github-dispatch", "config": map[string]any{"repo": "octo-org/octo-repo", "token_env": "GH_TOKEN"}}
	r := e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/ci-dispatch", tokAdmin, body))
	cfg := r.json(t)["config"].(map[string]any)
	for k, want := range map[string]string{
		"mode": "repository_dispatch", "api_base_url": "https://api.github.com", "event_type": "angelia.release.published",
		"token_env": "GH_TOKEN", "timeout": "10s",
	} {
		if cfg[k] != want {
			t.Errorf("config.%s = %v, want %q", k, cfg[k], want)
		}
	}
	// A reader sees the host of the base URL, and the token reference as written.
	r = e.must(http.StatusOK, e.do("GET", "/v1/sinks/ci-dispatch", tokRead, nil))
	if c := r.json(t)["config"].(map[string]any); c["api_base_url"] != "https://api.github.com" || c["token_env"] != "GH_TOKEN" {
		t.Errorf("read config: %v", c)
	}

	bad := map[string]any{"kind": "github-dispatch", "config": map[string]any{"repo": "octo-org/octo-repo", "token_env": "OTHER_TOKEN"}}
	if r := e.do("PUT", "/v1/sinks/other", tokAdmin, bad); r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugEnvNotAllowed {
		t.Errorf("unlisted token_env: %d %s", r.Status, r.Body)
	}
	for name, cfg := range map[string]map[string]any{
		"no token":       {"repo": "o/r"},
		"both tokens":    {"repo": "o/r", "token_env": "GH_TOKEN", "token_ref": "gh"},
		"http base":      {"repo": "o/r", "token_env": "GH_TOKEN", "api_base_url": "http://ghe.example.com"},
		"inline token":   {"repo": "o/r", "token": "ghp_x"},
		"workflow input": {"repo": "o/r", "token_env": "GH_TOKEN", "mode": "workflow_dispatch", "workflow": "a.yml", "ref": "main", "inputs": map[string]any{"deploy_env": "x"}},
	} {
		if r := e.do("PUT", "/v1/sinks/bad", tokAdmin, map[string]any{"kind": "github-dispatch", "config": cfg}); r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	ref := map[string]any{"kind": "github-dispatch", "config": map[string]any{"repo": "o/r", "token_ref": "gh-token"}}
	e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/by-ref", tokAdmin, ref))
}
