package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/watchfile"
)

func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func TestPutChangeHeaderAndDryRun(t *testing.T) {
	e := newEnv(t)
	// A dry run answers like the write and writes nothing.
	r := e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/ci-webhook?dry_run=true", tokAdmin, sinkPut))
	if r.Header.Get("Angelia-Change") != "created" {
		t.Errorf("dry run create: %v", r.Header)
	}
	e.must(http.StatusNotFound, e.do("GET", "/v1/sinks/ci-webhook", tokRead, nil))

	e.seedConfig()
	for _, tc := range []struct {
		path, tok string
		body      any
	}{
		{"/v1/sinks/ci-webhook", tokAdmin, sinkPut},
		{"/v1/watches/requests", tokWrite, watchPut},
		{"/v1/subscriptions/platform-ci", tokWrite, subscriptionPut},
	} {
		r := e.must(http.StatusOK, e.do("PUT", tc.path, tc.tok, tc.body))
		if r.Header.Get("Angelia-Change") != "unchanged" || r.Header.Get("Location") != "" {
			t.Errorf("%s again: %v", tc.path, r.Header)
		}
	}
	w := clone(watchPut)
	w["labels"] = map[string]any{"team": "web"}
	r = e.must(http.StatusOK, e.do("PUT", "/v1/watches/requests?dry_run=true", tokWrite, w))
	if r.Header.Get("Angelia-Change") != "updated" || r.json(t)["labels"].(map[string]any)["team"] != "web" {
		t.Errorf("dry run update: %v %s", r.Header, r.Body)
	}
	if got := e.must(http.StatusOK, e.do("GET", "/v1/watches/requests", tokRead, nil)).json(t)["labels"].(map[string]any)["team"]; got != "platform" {
		t.Errorf("the dry run wrote: team = %v", got)
	}
	r = e.must(http.StatusOK, e.do("PUT", "/v1/watches/requests", tokWrite, w))
	if r.Header.Get("Angelia-Change") != "updated" {
		t.Errorf("update: %v", r.Header)
	}

	// DELETE: dry run, then for real, then 404.
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/subscriptions/platform-ci?dry_run=true", tokWrite, nil))
	e.must(http.StatusOK, e.do("GET", "/v1/subscriptions/platform-ci", tokRead, nil))
	r = e.must(http.StatusNoContent, e.do("DELETE", "/v1/subscriptions/platform-ci", tokWrite, nil))
	if r.Header.Get("Angelia-Change") != "deleted" {
		t.Errorf("delete: %v", r.Header)
	}
	e.must(http.StatusNotFound, e.do("DELETE", "/v1/subscriptions/platform-ci", tokWrite, nil))
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/watches/requests", tokWrite, nil))
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/sinks/ci-webhook", tokAdmin, nil))
}

func TestPutValidation(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	errsOf := func(r result) map[string]string {
		t.Helper()
		if r.Status != http.StatusUnprocessableEntity {
			t.Fatalf("status %d: %s", r.Status, r.Body)
		}
		out := map[string]string{}
		for _, x := range r.json(t)["errors"].([]any) {
			m := x.(map[string]any)
			out[m["pointer"].(string)] = m["detail"].(string)
		}
		return out
	}

	// Schema errors and the watch file's rules are listed together.
	w := clone(watchPut)
	w["interval"] = 5
	w["coordinates"] = map[string]any{"name": "requests", "extra": "x"}
	w["name"] = "other"
	got := errsOf(e.do("PUT", "/v1/watches/requests", tokWrite, w))
	for _, p := range []string{"/interval", "/name"} {
		if _, ok := got[p]; !ok {
			t.Errorf("watch: no error at %s: %v", p, got)
		}
	}
	if r := e.do("PUT", "/v1/watches/requests", tokWrite, w); r.problemType(t) != SlugValidationFailed {
		t.Error("watch: not validation-failed")
	}

	s := clone(subscriptionPut)
	s["sink"] = "nothing"
	if got := errsOf(e.do("PUT", "/v1/subscriptions/x", tokWrite, s)); got["/sink"] == "" {
		t.Errorf("subscription: %v", got)
	}

	// Env names: the allowlist, and ANGELIA_* never.
	k := clone(sinkPut)
	k["config"].(map[string]any)["secret_env"] = "ANGELIA_DATABASE_URL"
	r := e.do("PUT", "/v1/sinks/x", tokAdmin, k)
	if r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugEnvNotAllowed {
		t.Fatalf("ANGELIA_ env: %d %s", r.Status, r.Body)
	}
	k["config"].(map[string]any)["secret_env"] = "OTHER_SECRET"
	if r := e.do("PUT", "/v1/sinks/x", tokAdmin, k); r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugEnvNotAllowed {
		t.Fatalf("unlisted env: %d %s", r.Status, r.Body)
	}
	k["config"].(map[string]any)["url"] = "ftp://x"
	got = errsOf(e.do("PUT", "/v1/sinks/x", tokAdmin, k))
	if got["/config/secret_env"] == "" || len(got) < 2 {
		t.Errorf("env with other errors is listed with them: %v", got)
	}
}

func TestRequestRejects(t *testing.T) {
	e := newEnv(t)
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		hdr    []string
		status int
		slug   string
	}{
		{"malformed JSON", "PUT", "/v1/watches/a", `{"source":`, nil, 400, SlugBadRequest},
		{"trailing data", "PUT", "/v1/watches/a", `{} {}`, nil, 400, SlugBadRequest},
		{"not an object", "PUT", "/v1/watches/a", `[1]`, nil, 422, SlugValidationFailed},
		{"media type", "PUT", "/v1/watches/a", `{}`, []string{"Content-Type", "text/plain"}, 415, SlugUnsupportedMediaType},
		{"too large", "PUT", "/v1/watches/a", `{"source":"` + strings.Repeat("a", MaxBodyBytes) + `"}`, nil, 413, SlugPayloadTooLarge},
		{"unknown query parameter", "GET", "/v1/watches?nope=1", nil, nil, 400, SlugBadRequest},
		{"bad limit", "GET", "/v1/watches?limit=0", nil, nil, 400, SlugBadRequest},
		{"bad cursor", "GET", "/v1/watches?cursor=!!", nil, nil, 400, SlugBadRequest},
		{"bad name in a PUT", "PUT", "/v1/watches/Bad_Name", map[string]any{}, nil, 422, SlugValidationFailed},
		{"bad name in a GET", "GET", "/v1/watches/Bad_Name", nil, nil, 404, SlugNotFound},
		{"bad dry_run", "DELETE", "/v1/watches/a?dry_run=maybe", nil, nil, 400, SlugBadRequest},
	} {
		r := e.do(tc.method, tc.path, tokAdmin, tc.body, tc.hdr...)
		if r.Status != tc.status || r.problemType(t) != tc.slug {
			t.Errorf("%s: %d %s", tc.name, r.Status, r.Body)
		}
	}
}

// applyFile applies a watch file, as `angelia apply` does.
func (e *env) applyFile(yaml string) {
	e.t.Helper()
	f, err := watchfile.Parse([]byte(yaml), "angelia.yaml", watchfile.Options{
		Sources: testSources(e.t), LookupEnv: func(string) (string, bool) { return "", true },
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := watchfile.Apply(context.Background(), e.st, f, watchfile.ApplyOptions{Now: func() time.Time { return e.now }}); err != nil {
		e.t.Fatal(err)
	}
}

const fileConfig = `version: 1
sinks:
  - {name: file-hook, kind: webhook, config: {url: "https://hooks.example.com/a/b?k=v", secret_env: CI_WEBHOOK_SECRET}}
watches:
  - {name: file-watch, source: pypi, coordinates: {name: flask}}
subscriptions:
  - {name: file-sub, sink: file-hook}
`

func TestOwnership(t *testing.T) {
	e := newEnv(t)
	e.applyFile(fileConfig)
	e.seedConfig()
	for _, tc := range []struct {
		method, path string
		body         any
		slug         string
	}{
		{"PUT", "/v1/watches/file-watch", watchPut, SlugManagedByFile},
		{"DELETE", "/v1/watches/file-watch", nil, SlugManagedByFile},
		{"PUT", "/v1/sinks/file-hook", sinkPut, SlugManagedByFile},
		{"DELETE", "/v1/sinks/file-hook", nil, SlugManagedByFile},
		{"PUT", "/v1/subscriptions/file-sub", subscriptionPut, SlugManagedByFile},
		{"DELETE", "/v1/subscriptions/file-sub", nil, SlugManagedByFile},
		{"PUT", "/v1/subscriptions/api-to-file", map[string]any{"sink": "file-hook"}, SlugCrossOwnerReference},
	} {
		r := e.do(tc.method, tc.path, tokAdmin, tc.body)
		if r.Status != http.StatusConflict || r.problemType(t) != tc.slug {
			t.Errorf("%s %s: %d %s", tc.method, tc.path, r.Status, r.Body)
		}
	}
	m := e.must(http.StatusOK, e.do("GET", "/v1/watches/file-watch", tokRead, nil)).json(t)
	if m["managed_by"] != "file" {
		t.Errorf("managed_by = %v", m["managed_by"])
	}
	// The sink is in use by an API subscription: 409 names it.
	r := e.do("DELETE", "/v1/sinks/ci-webhook", tokAdmin, nil)
	if r.Status != http.StatusConflict || r.problemType(t) != SlugSinkInUse || !strings.Contains(string(r.Body), "platform-ci") {
		t.Errorf("sink in use: %d %s", r.Status, r.Body)
	}
}

func TestSinkRedaction(t *testing.T) {
	e := newEnv(t)
	e.applyFile(fileConfig)
	for _, tc := range []struct {
		tok      string
		url      string
		redacted bool
	}{
		{tokRead, "https://hooks.example.com", true},
		{tokWrite, "https://hooks.example.com", true},
		{tokAdmin, "https://hooks.example.com/a/b?k=v", false},
	} {
		m := e.must(http.StatusOK, e.do("GET", "/v1/sinks/file-hook", tc.tok, nil)).json(t)
		cfg := m["config"].(map[string]any)
		if cfg["url"] != tc.url || m["url_redacted"] != tc.redacted || cfg["secret_env"] != "CI_WEBHOOK_SECRET" {
			t.Errorf("%s: %v", tc.tok, m)
		}
		l := e.must(http.StatusOK, e.do("GET", "/v1/sinks", tc.tok, nil)).json(t)
		if got := l["items"].([]any)[0].(map[string]any)["config"].(map[string]any)["url"]; got != tc.url {
			t.Errorf("%s list: %v", tc.tok, got)
		}
	}
	if s := redactURLs("not a url://x"); s != "[redacted]" {
		t.Errorf("redactURLs = %v", s)
	}
}

// A kind's config_schema marks keys x-angelia-secret: the API refuses them in
// a PUT and masks them for every caller.
func TestSecretKeys(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"url":{"type":"string"},"token":{"type":"string","x-angelia-secret":true}}}`)
	e := newEnv(t, func(o *Options) {
		o.Plugins = append(o.Plugins, PluginInfo{Kind: "vault-hook", Interface: "sink", Runtime: "wasm", Version: "1.0.0", ConfigSchema: schema})
		// The webhook schema of the built-in sink marks no key.
	})
	k := store.Sink{
		ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Name: "v", Kind: "vault-hook", ManagedBy: store.ManagedByFile,
		ConfigEncrypted: []byte(`{"url":"https://v.example.com/x","token":"s3cr3t"}`),
	}
	for _, sc := range []Scope{ScopeRead, ScopeAdmin} {
		j := e.srv.sinkJSONOf(k, sc)
		if j.Config["token"] != Masked || len(j.MaskedFields) != 1 || j.MaskedFields[0] != "token" {
			t.Errorf("%s: %+v", sc, j)
		}
		b, _ := json.Marshal(j)
		if strings.Contains(string(b), "s3cr3t") {
			t.Errorf("%s leaks the secret: %s", sc, b)
		}
	}
	invalid, env := e.srv.checkSinkConfig("vault-hook", map[string]any{"token": "x", "token_env": "CI_WEBHOOK_SECRET"}) //nolint:gosec // synthetic
	if len(invalid) != 1 || invalid[0].Pointer != "/config/token" || len(env) != 0 {
		t.Errorf("checkSinkConfig: %v %v", invalid, env)
	}
}

func TestDeleteSubscriptionFailsPending(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	sid := e.subscriptionID("platform-ci")
	ev := e.appendEvent(e.watchID("requests"), event.TypePublished, "pypi", e.now,
		store.Delivery{SubscriptionID: sid, Status: store.DeliveryPending, NextAttemptAt: e.now})
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/subscriptions/platform-ci", tokWrite, nil))
	rows, err := e.st.ListDeliveryPage(context.Background(), store.DeliveryQuery{EventID: ev.ID, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Status != store.DeliveryDead {
		t.Fatalf("%v %+v", err, rows)
	}
}

func TestListPagination(t *testing.T) {
	e := newEnv(t)
	e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/ci-webhook", tokAdmin, sinkPut))
	names := []string{"a", "b", "c", "d", "e"}
	for _, n := range names {
		w := clone(watchPut)
		if n == "c" {
			w["labels"] = map[string]any{"team": "web"}
		}
		e.must(http.StatusCreated, e.do("PUT", "/v1/watches/"+n, tokWrite, w))
	}
	var got []string
	path := "/v1/watches?limit=2"
	for path != "" {
		m := e.must(http.StatusOK, e.do("GET", path, tokRead, nil)).json(t)
		for _, it := range m["items"].([]any) {
			got = append(got, it.(map[string]any)["name"].(string))
		}
		path = ""
		if c, ok := m["next_cursor"].(string); ok {
			path = "/v1/watches?limit=2&cursor=" + c
		}
	}
	if strings.Join(got, ",") != "a,b,c,d,e" {
		t.Errorf("pages: %v", got)
	}
	m := e.must(http.StatusOK, e.do("GET", "/v1/watches?label=team=platform&source=pypi", tokRead, nil)).json(t)
	if n := len(m["items"].([]any)); n != 4 {
		t.Errorf("label filter: %d items", n)
	}
	// A cursor of another list is refused.
	c := e.must(http.StatusOK, e.do("GET", "/v1/watches?limit=1", tokRead, nil)).json(t)["next_cursor"].(string)
	if r := e.do("GET", "/v1/sinks?cursor="+c, tokRead, nil); r.Status != http.StatusBadRequest {
		t.Errorf("cross-list cursor: %d", r.Status)
	}
}
