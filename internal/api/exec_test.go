package api

import (
	"net/http"
	"strings"
	"testing"
)

func withExec(o *Options) {
	sch, err := SpecKindConfig("exec")
	if err != nil {
		panic(err)
	}
	o.Plugins = append(o.Plugins, PluginInfo{Kind: "exec", Interface: "sink", Runtime: "builtin", Version: "0.1.0", ConfigSchema: sch})
	o.ExecCommands = []string{"deploy-hook"}
}

// ADR 0042 §1: an exec sink written through the API only names a command
// of exec.commands; the kind publishes ExecSinkConfig.
func TestPutExecSink(t *testing.T) {
	e := newEnv(t, withExec)
	r := e.must(http.StatusOK, e.do("GET", "/v1/plugins/sink/exec", tokRead, nil))
	sch, _ := r.json(t)["config_schema"].(map[string]any)
	props, _ := sch["properties"].(map[string]any)
	if _, ok := props["command"]; !ok || sch["additionalProperties"] != false {
		t.Fatalf("config_schema %v", sch)
	}
	r = e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/deploy", tokAdmin, map[string]any{"kind": "exec", "config": map[string]any{"command": "deploy-hook"}}))
	if c := r.json(t)["config"].(map[string]any); c["command"] != "deploy-hook" {
		t.Fatalf("config %v", c)
	}
	for name, cfg := range map[string]map[string]any{
		"unknown command": {"command": "other"},
		"no command":      {},
		"argv":            {"command": "deploy-hook", "argv": []any{"/bin/sh"}},
	} {
		if r := e.do("PUT", "/v1/sinks/bad", tokAdmin, map[string]any{"kind": "exec", "config": cfg}); r.Status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
}

func TestSpecKindConfigUnknown(t *testing.T) {
	if _, err := SpecKindConfig("nope"); err == nil {
		t.Fatal("accepted")
	}
}

// Without exec.commands the kind is unknown: refused like any other.
func TestPutExecSinkWithoutCommands(t *testing.T) {
	e := newEnv(t)
	r := e.do("PUT", "/v1/sinks/deploy", tokAdmin, map[string]any{"kind": "exec", "config": map[string]any{"command": "deploy-hook"}})
	if r.Status != http.StatusUnprocessableEntity || !strings.Contains(string(r.Body), "exec.commands") {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	e.must(http.StatusNotFound, e.do("GET", "/v1/plugins/sink/exec", tokRead, nil))
}
