package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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
}

// A kind's config_schema marks values x-angelia-secret at any depth: the API
// refuses them in a PUT and masks them for every caller.
func TestSecretKeys(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{
		"url":{"type":"string"},
		"token":{"type":"string","x-angelia-secret":true},
		"auth":{"type":"object","properties":{"password":{"type":"string","x-angelia-secret":true},"user":{"type":"string"}}},
		"targets":{"type":"array","items":{"type":"object","properties":{"key":{"x-angelia-secret":true}}}},
		"extra":{"allOf":[{"properties":{"pin":{"x-angelia-secret":true}}}]}}}`)
	e := newEnv(t, func(o *Options) {
		o.Plugins = append(o.Plugins, PluginInfo{Kind: "vault-hook", Interface: "sink", Runtime: "wasm", Version: "1.0.0", ConfigSchema: schema})
	})
	k := store.Sink{
		ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Name: "v", Kind: "vault-hook", ManagedBy: store.ManagedByFile,
		ConfigEncrypted: []byte(`{"url":"https://v.example.com/x","token":"s3cr3t","auth":{"user":"bot","password":"s3cr3t"},
			"targets":[{"key":"s3cr3t"},{"key":"s3cr3t"}],"extra":{"pin":"s3cr3t"}}`),
	}
	want := "auth.password,extra.pin,targets.0.key,targets.1.key,token"
	for _, sc := range []Scope{ScopeRead, ScopeAdmin} {
		j, err := e.srv.sinkJSONOf(k, sc)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(j.MaskedFields, ","); got != want {
			t.Errorf("%s: masked %s, want %s", sc, got, want)
		}
		b, _ := json.Marshal(j)
		if strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), `"user":"bot"`) {
			t.Errorf("%s: %s", sc, b)
		}
	}
	invalid, env := e.srv.checkSinkConfig("vault-hook", map[string]any{ //nolint:gosec // synthetic values
		"token": "x", "auth": map[string]any{"password": "x"}, "token_env": "CI_WEBHOOK_SECRET",
	})
	if len(invalid) != 2 || invalid[0].Pointer != "/config/auth/password" || invalid[1].Pointer != "/config/token" || len(env) != 0 {
		t.Errorf("checkSinkConfig: %v %v", invalid, env)
	}
	// A stored configuration that is not JSON is an error, not an empty one.
	k.ConfigEncrypted = []byte("{")
	if _, err := e.srv.sinkJSONOf(k, ScopeAdmin); err == nil {
		t.Error("a broken configuration was read")
	}
}

// Masking fails closed: below admin, a kind without a config_schema has every
// string masked except *_env names, and a non-http(s) URL keeps only its
// scheme. The built-in webhook kind gets the spec's schema.
func TestMaskingFailsClosed(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Plugins = append(o.Plugins, PluginInfo{Kind: "chat", Interface: "sink", Runtime: "wasm", Version: "1.0.0"})
	})
	k := store.Sink{
		ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Name: "c", Kind: "chat", ManagedBy: store.ManagedByFile,
		ConfigEncrypted: []byte(`{"url":"slack://hook:s3cr3t@T0/B0/X","token":"s3cr3t","secret_env":"CI_WEBHOOK_SECRET","n":3,"opts":{"room":"ops"}}`),
	}
	j, err := e.srv.sinkJSONOf(k, ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(j.MaskedFields, ","); got != "opts.room,token,url" {
		t.Errorf("masked %s", got)
	}
	if b, _ := json.Marshal(j.Config); string(b) != `{"n":3,"opts":{"room":"[REDACTED]"},"secret_env":"CI_WEBHOOK_SECRET","token":"[REDACTED]","url":"[REDACTED]"}` {
		t.Errorf("config %s", b)
	}
	if j, _ = e.srv.sinkJSONOf(k, ScopeAdmin); j.Config["token"] != "s3cr3t" || len(j.MaskedFields) != 0 {
		t.Errorf("admin: %+v", j)
	}
	for in, want := range map[string]string{
		"slack://hook:s3cr3t@T0/B0/X":            "slack://",
		"smtp://u:p@mail.example.com:25/?to=x":   "smtp://",
		"https://u:p@ci.example.com/hooks/x?k=v": "https://ci.example.com",
		"not a url://x":                          "[redacted]",
		"plain":                                  "plain",
	} {
		if got := redactURLs(in); got != want {
			t.Errorf("redactURLs(%q) = %v, want %s", in, got, want)
		}
	}
	webhook, complete := e.srv.configSchema("webhook")
	sch, _ := json.Marshal(webhook)
	if !complete {
		t.Error("the webhook schema is not complete")
	}
	if !strings.Contains(string(sch), `"secret_env"`) || strings.Contains(string(sch), "$ref") {
		t.Errorf("webhook config_schema: %s", sch)
	}
}

// Every *_env key is checked against the allowlist at any depth, and must be
// a string.
func TestEnvAllowlistNested(t *testing.T) {
	e := newEnv(t)
	invalid, env := e.srv.checkSinkConfig("webhook", map[string]any{ //nolint:gosec // variable names, not secrets
		"secret_env": "CI_WEBHOOK_SECRET",
		"auth":       map[string]any{"token_env": "ANGELIA_DATABASE_DSN"},
		"list":       []any{map[string]any{"key_env": "HOME"}},
		"bad_env":    42,
	})
	var ptrs []string
	for _, f := range env {
		ptrs = append(ptrs, f.Pointer)
	}
	if strings.Join(ptrs, ",") != "/config/auth/token_env,/config/list/0/key_env" {
		t.Errorf("env: %v", env)
	}
	if len(invalid) != 1 || invalid[0].Pointer != "/config/bad_env" {
		t.Errorf("invalid: %v", invalid)
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

// Each watch keeps the interval it asked for; the target is checked at the
// smallest one (spec docs/api.md decision 10).
func TestWatchIntervals(t *testing.T) {
	e := newEnv(t)
	get := func(name string) (string, string) {
		t.Helper()
		m := e.must(http.StatusOK, e.do("GET", "/v1/watches/"+name, tokRead, nil)).json(t)
		return m["interval"].(string), m["status"].(map[string]any)["effective_interval"].(string)
	}
	slow := clone(watchPut)
	slow["interval"] = "2h"
	e.must(http.StatusCreated, e.do("PUT", "/v1/watches/slow", tokWrite, slow))
	if i, eff := get("slow"); i != "2h" || eff != "2h" {
		t.Fatalf("slow alone: %s %s", i, eff)
	}
	e.must(http.StatusCreated, e.do("PUT", "/v1/watches/fast", tokWrite, watchPut)) // 30m, same target
	if i, eff := get("slow"); i != "2h" || eff != "30m" {
		t.Errorf("slow with fast: %s %s", i, eff)
	}
	// Changing only the interval is an update.
	fast := clone(watchPut)
	fast["interval"] = "45m"
	if r := e.must(http.StatusOK, e.do("PUT", "/v1/watches/fast", tokWrite, fast)); r.Header.Get("Angelia-Change") != "updated" {
		t.Errorf("interval change: %v", r.Header)
	}
	if i, eff := get("fast"); i != "45m" || eff != "45m" {
		t.Errorf("fast at 45m: %s %s", i, eff)
	}
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/watches/fast", tokWrite, nil))
	if _, eff := get("slow"); eff != "2h" {
		t.Errorf("after the delete: %s", eff)
	}
}

func TestTruncateKeepsCharacters(t *testing.T) {
	if got := truncate("ééé", 3); got != "é..." {
		t.Errorf("truncate = %q", got)
	}
	if got := truncateUTF8("ab", 5); got != "ab" {
		t.Errorf("truncateUTF8 = %q", got)
	}
	r := newEnv(t).do("GET", "/v1/watches?"+strings.Repeat("é", 40)+"=1", tokRead, nil)
	if r.Status != 400 || !utf8.Valid(r.Body) || strings.Contains(string(r.Body), `�`) {
		t.Errorf("%d %s", r.Status, r.Body)
	}
}

func sinkKindEnv(t *testing.T, kind, schema string) *env {
	t.Helper()
	return newEnv(t, func(o *Options) {
		o.Plugins = append(o.Plugins, PluginInfo{Kind: kind, Interface: "sink", Runtime: "wasm", Version: "1", ConfigSchema: json.RawMessage(schema)})
	})
}

// A secret reached through a local $ref or a patternProperties entry is
// masked for every caller and refused in a PUT (review of #71).
func TestSecretThroughRefAndPattern(t *testing.T) {
	e := sinkKindEnv(t, "vault-hook", `{"type":"object",
		"$defs":{"cred":{"type":"string","x-angelia-secret":true},"box":{"$ref":"#/definitions/inner"}},
		"definitions":{"inner":{"type":"object","properties":{"pin":{"$ref":"#/$defs/cred"}}}},
		"properties":{"url":{"type":"string"},"token":{"$ref":"#/$defs/cred"},"vault":{"$ref":"#/$defs/box"},
			"auth":{"type":"object","patternProperties":{"^p":{"type":"string","x-angelia-secret":true}}}}}`)
	k := store.Sink{
		ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Name: "v", Kind: "vault-hook", ManagedBy: store.ManagedByFile,
		ConfigEncrypted: []byte(`{"url":"https://v.example.com/x","token":"s3cr3tREF","vault":{"pin":"s3cr3tDEEP"},"auth":{"pw":"s3cr3tPAT","user":"bot"}}`),
	}
	for _, sc := range []Scope{ScopeRead, ScopeAdmin} {
		j, err := e.srv.sinkJSONOf(k, sc)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(j)
		if strings.Contains(string(b), "s3cr3t") || !strings.Contains(string(b), `"user":"bot"`) {
			t.Errorf("%s: %s", sc, b)
		}
		if got := strings.Join(j.MaskedFields, ","); got != "auth.pw,token,vault.pin" {
			t.Errorf("%s: masked %s", sc, got)
		}
	}
	invalid, _ := e.srv.checkSinkConfig("vault-hook", map[string]any{"token": "x", "auth": map[string]any{"pw": "y", "user": "z"}})
	var ptrs []string
	for _, f := range invalid {
		ptrs = append(ptrs, f.Pointer)
	}
	if strings.Join(ptrs, ",") != "/config/auth/pw,/config/token" {
		t.Errorf("PUT check: %v", invalid)
	}
}

// A schema with a construct the masking cannot follow, or a reference it
// cannot resolve, counts as unknown below admin: every string except *_env
// values is masked.
func TestUnclassifiableSchemaFailsClosed(t *testing.T) {
	for name, schema := range map[string]string{
		"if/then":     `{"properties":{"mode":{"type":"string"}},"if":{"properties":{"mode":{"const":"a"}}},"then":{"properties":{"key":{"x-angelia-secret":true}}}}`,
		"remote ref":  `{"properties":{"key":{"$ref":"https://schemas.example.com/cred.json"}}}`,
		"missing ref": `{"properties":{"key":{"$ref":"#/$defs/nothing"}}}`,
		"cycle":       `{"$defs":{"a":{"properties":{"next":{"$ref":"#/$defs/a"}}}},"properties":{"key":{"$ref":"#/$defs/a"}}}`,
		"unevaluated": `{"properties":{"mode":{"type":"string"}},"unevaluatedProperties":{"x-angelia-secret":true}}`,
		"dependent":   `{"dependentSchemas":{"mode":{"properties":{"key":{"x-angelia-secret":true}}}}}`,
	} {
		e := sinkKindEnv(t, "odd", schema)
		if _, complete := e.srv.configSchema("odd"); complete {
			t.Errorf("%s: complete", name)
		}
		k := store.Sink{
			ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Name: "o", Kind: "odd", ManagedBy: store.ManagedByFile,
			ConfigEncrypted: []byte(`{"mode":"a","key":"s3cr3t","key_env":"CI_WEBHOOK_SECRET"}`),
		}
		j, err := e.srv.sinkJSONOf(k, ScopeRead)
		if err != nil {
			t.Fatal(err)
		}
		if j.Config["key"] != Masked || j.Config["mode"] != Masked || j.Config["key_env"] != "CI_WEBHOOK_SECRET" {
			t.Errorf("%s: %v", name, j.Config)
		}
	}
	if got := redactURLs("//h.example.com/secret"); got != "[redacted]" {
		t.Errorf("scheme-relative: %v", got)
	}
}
