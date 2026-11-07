package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/store"
)

// leakCanary is a synthetic secret value: it must never leave the store
// sealed.
const leakCanary = "whsec_CANARYsecretCANARYsecret0123456789"

// testMaster is the synthetic master key of withKeys.
var testMaster = bytes.Repeat([]byte{0x42}, secrets.KeySize)

func testKeyring() *secrets.Keyring {
	k, err := secrets.NewKeyring(testMaster)
	if err != nil {
		panic(err)
	}
	return k
}

// withKeys configures a secrets key.
func withKeys(o *Options) { o.Secrets = testKeyring() }

// noSecretLeak fails when out holds the canary, its base64 or the key.
func noSecretLeak(t *testing.T, what, out string) {
	t.Helper()
	for _, bad := range []string{
		leakCanary, base64.StdEncoding.EncodeToString([]byte(leakCanary)), "CANARY",
		base64.StdEncoding.EncodeToString(testMaster),
	} {
		if strings.Contains(out, bad) {
			t.Fatalf("%s leaks the secret: %s", what, out)
		}
	}
}

func TestSecretLifecycle(t *testing.T) {
	e := newEnv(t, withKeys)
	ctx := context.Background()
	// Every response body is checked for the value, as the log is below.
	do := func(method, path, token string, b any) result {
		t.Helper()
		r := e.do(method, path, token, b)
		noSecretLeak(t, method+" "+path+" response", string(r.Body))
		return r
	}

	r := e.must(http.StatusCreated, do("PUT", "/v1/secrets/ci-signing", tokAdmin, map[string]any{"value": leakCanary}))
	if r.Header.Get("Angelia-Change") != "created" || r.Header.Get("Location") != "/v1/secrets/ci-signing" {
		t.Fatalf("headers %v", r.Header)
	}
	m := r.json(t)
	if m["name"] != "ci-signing" || m["key_id"] != testKeyring().PrimaryID() || len(m["hosts"].([]any)) != 0 {
		t.Fatalf("created: %s", r.Body)
	}
	// The stored row is sealed: the plaintext is not in it, and it opens
	// to the value with the row's id and name.
	row, err := e.st.SecretByName(ctx, "ci-signing")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(row.Ciphertext, []byte(leakCanary)) {
		t.Fatal("the database holds the plaintext")
	}
	v, err := testKeyring().Open(row.ID, row.Name, row.Ciphertext)
	if err != nil || string(v.Reveal()) != leakCanary {
		t.Fatalf("stored value: %v", err)
	}

	// The same value again is unchanged; a new value or new hosts update.
	r = e.must(http.StatusOK, do("PUT", "/v1/secrets/ci-signing", tokAdmin, map[string]any{"value": leakCanary}))
	if r.Header.Get("Angelia-Change") != "unchanged" {
		t.Fatalf("same value: %v", r.Header)
	}
	r = e.must(http.StatusOK, do("PUT", "/v1/secrets/ci-signing", tokAdmin, map[string]any{
		"value_base64": base64.StdEncoding.EncodeToString([]byte(leakCanary + "2")), "hosts": []any{"ci.example.com"},
	}))
	if r.Header.Get("Angelia-Change") != "updated" || r.json(t)["hosts"].([]any)[0] != "ci.example.com" {
		t.Fatalf("update: %v %s", r.Header, r.Body)
	}
	// A dry run changes nothing.
	e.must(http.StatusOK, do("PUT", "/v1/secrets/ci-signing?dry_run=true", tokAdmin, map[string]any{"value": "other"}))
	e.must(http.StatusCreated, do("PUT", "/v1/secrets/dry?dry_run=true", tokAdmin, map[string]any{"value": "other"}))
	e.must(http.StatusNotFound, do("GET", "/v1/secrets/dry", tokAdmin, nil))
	row2, _ := e.st.SecretByName(ctx, "ci-signing")
	if v, _ := testKeyring().Open(row2.ID, row2.Name, row2.Ciphertext); string(v.Reveal()) != leakCanary+"2" || row2.Revision != 2 {
		t.Fatalf("dry run wrote: revision %d", row2.Revision)
	}

	// Referenced by a sink: listed, and the delete is refused.
	e.must(http.StatusCreated, do("PUT", "/v1/sinks/ci", tokAdmin, map[string]any{"kind": "webhook", "config": map[string]any{
		"url": "https://ci.example.com/hook", "secret_ref": "ci-signing",
	}}))
	r = e.must(http.StatusOK, do("GET", "/v1/secrets/ci-signing", tokAdmin, nil))
	refs := r.json(t)["referenced_by"].([]any)
	if len(refs) != 1 || refs[0].(map[string]any)["name"] != "ci" || refs[0].(map[string]any)["pointer"] != "/config/secret_ref" {
		t.Fatalf("referenced_by: %s", r.Body)
	}
	r = do("DELETE", "/v1/secrets/ci-signing", tokAdmin, nil)
	if r.Status != http.StatusConflict || r.problemType(t) != SlugSecretInUse || !strings.Contains(string(r.Body), "sink ci (/config/secret_ref)") {
		t.Fatalf("delete in use: %d %s", r.Status, r.Body)
	}
	e.must(http.StatusNoContent, do("DELETE", "/v1/sinks/ci", tokAdmin, nil))
	e.must(http.StatusNoContent, do("DELETE", "/v1/secrets/ci-signing", tokAdmin, nil))
	e.must(http.StatusNotFound, do("DELETE", "/v1/secrets/ci-signing", tokAdmin, nil))

	// The access log and the audit records: no value (responses: see do).
	logs := e.log.String()
	noSecretLeak(t, "log", logs)
	for _, want := range []string{`"action":"secret.create"`, `"action":"secret.update"`, `"action":"secret.delete"`, `"secret":"ci-signing"`, `"principal":"tok-admin"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("audit log lacks %s: %s", want, logs)
		}
	}
	if n := strings.Count(logs, `"audit":true`); n != 3 {
		t.Errorf("%d audit records, want 3 (create, update, delete; none for unchanged or dry runs)", n)
	}
}

// lostUpdates makes the first n fenced secret updates lose, as a
// concurrent writer would on a store whose transactions do not lock.
type lostUpdates struct {
	store.APIStore
	n *int
}

func (l lostUpdates) InAPITx(ctx context.Context, fn func(store.APITx) error) error {
	return l.APIStore.InAPITx(ctx, func(tx store.APITx) error { return fn(lostUpdateTx{tx, l.n}) })
}

type lostUpdateTx struct {
	store.APITx
	n *int
}

func (l lostUpdateTx) UpdateSecret(ctx context.Context, s store.Secret) (bool, error) {
	if *l.n > 0 {
		*l.n--
		return false, nil
	}
	return l.APITx.UpdateSecret(ctx, s)
}

// NIT4 (#75 review): a PUT whose fenced update loses runs again on the new
// row, up to putSecretAttempts times, instead of failing at once.
func TestPutSecretLostUpdate(t *testing.T) {
	if putSecretAttempts != 3 {
		t.Fatalf("putSecretAttempts = %d, want 3 (ADR 0024 amendment)", putSecretAttempts)
	}
	lose := 0
	e := newEnv(t, withKeys, func(o *Options) { o.Store = lostUpdates{o.Store, &lose} })
	e.must(http.StatusCreated, e.do("PUT", "/v1/secrets/s", tokAdmin, map[string]any{"value": "v1"}))
	lose = 2 // two losses, then the third attempt wins
	r := e.must(http.StatusOK, e.do("PUT", "/v1/secrets/s", tokAdmin, map[string]any{"value": "v2"}))
	if r.Header.Get("Angelia-Change") != "updated" || lose != 0 {
		t.Fatalf("retried update: %v, %d losses left", r.Header, lose)
	}
	row, _ := e.st.SecretByName(context.Background(), "s")
	if v, _ := testKeyring().Open(row.ID, row.Name, row.Ciphertext); string(v.Reveal()) != "v2" || row.Revision != 2 {
		t.Fatalf("stored revision %d", row.Revision)
	}
	lose = 3 // every attempt loses
	r = e.do("PUT", "/v1/secrets/s", tokAdmin, map[string]any{"value": "v3"})
	if r.Status != http.StatusInternalServerError || lose != 0 {
		t.Fatalf("always losing: %d %s, %d losses left", r.Status, r.Body, lose)
	}
	if n := strings.Count(e.log.String(), `"action":"secret.update"`); n != 1 {
		t.Errorf("%d update audit records, want 1", n)
	}
}

func TestSecretListPages(t *testing.T) {
	e := newEnv(t, withKeys)
	for _, n := range []string{"c", "a", "b"} {
		e.must(http.StatusCreated, e.do("PUT", "/v1/secrets/"+n, tokAdmin, map[string]any{"value": leakCanary}))
	}
	var names []string
	path := "/v1/secrets?limit=2"
	for range 3 {
		r := e.must(http.StatusOK, e.do("GET", path, tokAdmin, nil))
		noSecretLeak(t, "list", string(r.Body))
		m := r.json(t)
		for _, it := range m["items"].([]any) {
			names = append(names, it.(map[string]any)["name"].(string))
		}
		next, _ := m["next_cursor"].(string)
		if next == "" {
			break
		}
		path = "/v1/secrets?limit=2&cursor=" + next
	}
	if strings.Join(names, ",") != "a,b,c" {
		t.Fatalf("pages: %v", names)
	}
	r := e.do("GET", "/v1/secrets?cursor=eyJLIjoiayJ9", tokAdmin, nil) // a sink cursor
	if r.Status != http.StatusBadRequest {
		t.Fatalf("foreign cursor: %d", r.Status)
	}
}

// Acceptance criterion "secrets are write-only in API output": no scope,
// flag or parameter returns a value, and a write below admin is refused.
func TestSecretScopes(t *testing.T) {
	e := newEnv(t, withKeys)
	e.must(http.StatusCreated, e.do("PUT", "/v1/secrets/s", tokAdmin, map[string]any{"value": leakCanary}))
	for _, tok := range []string{tokRead, tokWrite} {
		for _, req := range [][2]string{{"GET", "/v1/secrets"}, {"GET", "/v1/secrets/s"}, {"PUT", "/v1/secrets/s"}, {"DELETE", "/v1/secrets/s"}} {
			var body any
			if req[0] == "PUT" {
				body = map[string]any{"value": "x"}
			}
			r := e.do(req[0], req[1], tok, body)
			if r.Status != http.StatusForbidden || r.json(t)["required_scope"] != "admin" {
				t.Fatalf("%s %s with %s: %d %s", req[0], req[1], tok, r.Status, r.Body)
			}
		}
	}
	for _, path := range []string{"/v1/secrets/s", "/v1/secrets/s?reveal=true", "/v1/secrets", "/v1/secrets?include_values=true"} {
		r := e.do("GET", path, tokAdmin, nil)
		noSecretLeak(t, path, string(r.Body))
	}
}

// Missing key (ADR 0024 §4): a write gets 422 secrets-key-missing and
// stores nothing.
func TestSecretWriteWithoutKey(t *testing.T) {
	e := newEnv(t)
	r := e.do("PUT", "/v1/secrets/s", tokAdmin, map[string]any{"value": leakCanary})
	if r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugSecretsKeyMissing {
		t.Fatalf("no key: %d %s", r.Status, r.Body)
	}
	noSecretLeak(t, "problem", string(r.Body))
	if n, err := e.st.CountSecretsByKeyID(context.Background()); err != nil || len(n) != 0 {
		t.Fatalf("stored without a key: %v %v", n, err)
	}
	noSecretLeak(t, "log", e.log.String())
}

func TestSecretValidation(t *testing.T) {
	e := newEnv(t, withKeys)
	big := strings.Repeat("A", secrets.MaxValueBytes+1)
	for name, tc := range map[string]struct {
		body    any
		pointer string
	}{
		"both":         {map[string]any{"value": leakCanary, "value_base64": base64.StdEncoding.EncodeToString([]byte(leakCanary))}, ""},
		"none":         {map[string]any{"hosts": []any{}}, ""},
		"bad base64":   {map[string]any{"value_base64": leakCanary + "!"}, "/value_base64"},
		"url base64":   {map[string]any{"value_base64": "-_-_"}, "/value_base64"},
		"empty":        {map[string]any{"value": ""}, "/value"},
		"too large":    {map[string]any{"value": leakCanary + big}, "/value"},
		"wildcard":     {map[string]any{"value": leakCanary, "hosts": []any{"*.example.com"}}, "/hosts/0"},
		"upper case":   {map[string]any{"value": leakCanary, "hosts": []any{"ok.example.com", "CI.example.com"}}, "/hosts/1"},
		"url host":     {map[string]any{"value": leakCanary, "hosts": []any{"https://ci.example.com"}}, "/hosts/0"},
		"name differs": {map[string]any{"value": leakCanary, "name": "other"}, "/name"},
		"unknown key":  {map[string]any{"value": leakCanary, "reveal": true}, "/reveal"},
	} {
		r := e.do("PUT", "/v1/secrets/s", tokAdmin, tc.body)
		if r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugValidationFailed {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
			continue
		}
		noSecretLeak(t, name, string(r.Body))
		found := false
		for _, fe := range r.json(t)["errors"].([]any) {
			if fe.(map[string]any)["pointer"] == tc.pointer {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: no error at %q: %s", name, tc.pointer, r.Body)
		}
	}
	r := e.do("PUT", "/v1/secrets/s", tokAdmin, map[string]any{"value": leakCanary + big})
	if !strings.Contains(string(r.Body), fmt.Sprintf("is %d bytes", len(leakCanary)+len(big))) {
		t.Errorf("too large: the size is not reported: %s", r.Body)
	}
	// Exactly 64 KiB and any bytes are accepted.
	raw := make([]byte, secrets.MaxValueBytes)
	for i := range raw {
		raw[i] = byte(i)
	}
	e.must(http.StatusCreated, e.do("PUT", "/v1/secrets/bin", tokAdmin, map[string]any{"value_base64": base64.StdEncoding.EncodeToString(raw)}))
	row, _ := e.st.SecretByName(context.Background(), "bin")
	if v, err := testKeyring().Open(row.ID, row.Name, row.Ciphertext); err != nil || !v.Equal(raw) {
		t.Fatalf("binary value: %v", err)
	}
	if n, _ := e.st.CountSecretsByKeyID(context.Background()); n[testKeyring().PrimaryID()] != 1 {
		t.Fatalf("invalid bodies stored rows: %v", n)
	}
	noSecretLeak(t, "log", e.log.String())
}

func TestSinkSecretRef(t *testing.T) {
	e := newEnv(t, withKeys)
	// A reference to a secret that does not exist yet is accepted.
	body := map[string]any{"kind": "webhook", "config": map[string]any{"url": "https://ci.example.com/hook", "secret_ref": "later"}}
	e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/ci", tokAdmin, body))
	// Below admin, *_ref values are names and are returned like *_env.
	r := e.must(http.StatusOK, e.do("GET", "/v1/sinks/ci", tokRead, nil))
	cfg := r.json(t)["config"].(map[string]any)
	if cfg["secret_ref"] != "later" || cfg["url"] != "https://ci.example.com" {
		t.Fatalf("read view: %s", r.Body)
	}
	// A write token cannot write a sink at all (admin).
	r = e.do("PUT", "/v1/sinks/ci", tokWrite, body)
	if r.Status != http.StatusForbidden || r.json(t)["required_scope"] != "admin" {
		t.Fatalf("write token: %d %s", r.Status, r.Body)
	}
	for _, bad := range []any{"Not A Name", 3, leakCanary + "/x"} {
		r = e.do("PUT", "/v1/sinks/ci", tokAdmin, map[string]any{"kind": "webhook", "config": map[string]any{"url": "https://ci.example.com/hook", "secret_ref": bad}})
		if r.Status != http.StatusUnprocessableEntity || !strings.Contains(string(r.Body), "/config/secret_ref") {
			t.Fatalf("secret_ref %v: %d %s", bad, r.Status, r.Body)
		}
		noSecretLeak(t, "problem", string(r.Body))
	}
	r = e.do("PUT", "/v1/sinks/ci", tokAdmin, map[string]any{"kind": "webhook", "config": map[string]any{ //nolint:gosec // names, not secrets
		"url": "https://ci.example.com/hook", "secret_ref": "a", "secret_env": "CI_WEBHOOK_SECRET",
	}})
	if r.Status != http.StatusUnprocessableEntity || !strings.Contains(string(r.Body), "at most one") {
		t.Fatalf("both references: %d %s", r.Status, r.Body)
	}
}

// SR4 (#75 review): for a kind whose config_schema is absent, so no schema
// covers its keys (a plugin kind, written through the watch file: the API
// enum admits only built-in kinds), the API's own rules still apply. A
// read token sees every *_ref name as written and every other string
// masked, and a *_ref that is not a secret name is refused at its pointer.
func TestSchemalessKindRefs(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Plugins = append(o.Plugins, PluginInfo{Kind: "chat", Interface: "sink", Runtime: "wasm", Version: "1.0.0"})
	})
	k := store.Sink{
		ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Name: "c", Kind: "chat", ManagedBy: store.ManagedByFile,
		ConfigEncrypted: []byte(`{"room":"ops","token_ref":"chat-token","auth":{"key_ref":"chat-key","user":"bot"}}`),
	}
	j, err := e.srv.sinkJSONOf(k, ScopeRead)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(j.Config); string(b) != `{"auth":{"key_ref":"chat-key","user":"[REDACTED]"},"room":"[REDACTED]","token_ref":"chat-token"}` {
		t.Errorf("read view: %s", b)
	}
	if got := strings.Join(j.MaskedFields, ","); got != "auth.user,room" {
		t.Errorf("masked %s", got)
	}
	for ptr, cfg := range map[string]map[string]any{
		"/config/token_ref":     {"token_ref": "Not A Name"},
		"/config/auth/key_ref":  {"auth": map[string]any{"key_ref": 3}},
		"/config/list/0/id_ref": {"list": []any{map[string]any{"id_ref": leakCanary + "/x"}}},
		"":                      {"token_ref": "chat-token", "auth": map[string]any{"key_ref": "chat-key"}},
	} {
		invalid, env := e.srv.checkSinkConfig("chat", cfg)
		switch {
		case ptr == "" && (len(invalid) != 0 || len(env) != 0):
			t.Errorf("valid names refused: %v %v", invalid, env)
		case ptr != "" && (len(invalid) != 1 || invalid[0].Pointer != ptr || len(env) != 0):
			t.Errorf("%s: %v %v", ptr, invalid, env)
		case ptr != "":
			noSecretLeak(t, "detail", invalid[0].Detail)
		}
	}
}

// ADR 0024 §2: a write that adds, removes or changes a *_ref key needs
// admin, whatever the endpoint's scope. Watch and subscription bodies have
// no *_ref key yet (their schemas are closed), so the rule is checked on
// the shapes a future reference takes.
func TestCheckRefScope(t *testing.T) {
	asScope := func(sc Scope) context.Context {
		return context.WithValue(context.Background(), principalKey{}, Principal{ID: "p", Scope: sc})
	}
	obj := func(js string) any {
		v, err := decodeJSON([]byte(js))
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for name, tc := range map[string]struct {
		before, after string
		forbidden     bool
	}{
		"no refs":           {`{"policy":{"bump":["major"]}}`, `{"policy":{"bump":["minor"]}}`, false},
		"unchanged ref":     {`{"auth":{"token_ref":"reg"},"interval":"1h"}`, `{"auth":{"token_ref":"reg"},"interval":"2h"}`, false},
		"added":             {`{"auth":{}}`, `{"auth":{"token_ref":"reg"}}`, true},
		"changed":           {`{"auth":{"token_ref":"reg"}}`, `{"auth":{"token_ref":"other"}}`, true},
		"removed":           {`{"auth":{"token_ref":"reg"}}`, `{"auth":{}}`, true},
		"moved":             {`{"auth":{"token_ref":"reg"}}`, `{"auth2":{"token_ref":"reg"}}`, true},
		"in array":          {`{"targets":[{"token_ref":"a"}]}`, `{"targets":[{"token_ref":"b"}]}`, true},
		"deleted":           {`{"auth":{"password_ref":"p"}}`, `null`, true},
		"label named _ref":  {`{"labels":{"git_ref":"main"}}`, `{"labels":{"git_ref":"dev"}}`, false},
		"coordinate _ref":   {`{"coordinates":{"tag_ref":"a"}}`, `{"coordinates":{"tag_ref":"b"}}`, false},
		"filter labels":     {`{"filter":{"labels":{"x_ref":"a"}}}`, `{"filter":{"labels":{"x_ref":"b"}}}`, false},
		"created with none": {`null`, `{"policy":{}}`, false},
	} {
		before, after := obj(tc.before), obj(tc.after)
		err := checkRefScope(asScope(ScopeWrite), before, after)
		if (err != nil) != tc.forbidden {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if err != nil {
			var p *ProblemDetail
			if !errors.As(err, &p) || p.Status != http.StatusForbidden || p.RequiredScope != "admin" {
				t.Errorf("%s: %v", name, err)
			}
		}
		if err := checkRefScope(asScope(ScopeAdmin), before, after); err != nil {
			t.Errorf("%s as admin: %v", name, err)
		}
	}
}

// The write endpoints apply the rule: their bodies carry no *_ref key
// today, so a write token keeps its scope for them.
func TestWriteScopeWithoutRefsUnaffected(t *testing.T) {
	e := newEnv(t, withKeys)
	e.seedConfig()
	e.must(http.StatusOK, e.do("PUT", "/v1/watches/requests", tokWrite, watchPut))
	e.must(http.StatusOK, e.do("PUT", "/v1/subscriptions/platform-ci", tokWrite, subscriptionPut))
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/subscriptions/platform-ci", tokWrite, nil))
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/watches/requests", tokWrite, nil))
}

func TestCheckStoredSinks(t *testing.T) {
	secretKind := PluginInfo{
		Kind: "chat", Interface: "sink", Runtime: "wasm", Version: "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","x-angelia-secret":true},"token_ref":{"type":"string"}}}`),
	}
	e := newEnv(t, func(o *Options) { o.Plugins = append(o.Plugins, secretKind) })
	ctx := context.Background()
	if err := e.srv.CheckStoredSinks(ctx); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if err := e.st.CreateSink(ctx, store.Sink{ID: "01A", Kind: "chat", Name: "ok", ConfigEncrypted: []byte(`{"token_ref":"chat"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.CheckStoredSinks(ctx); err != nil {
		t.Fatalf("references only: %v", err)
	}
	if err := e.st.CreateSink(ctx, store.Sink{ID: "01B", Kind: "chat", Name: "old", ConfigEncrypted: []byte(`{"token":"` + leakCanary + `"}`)}); err != nil {
		t.Fatal(err)
	}
	err := e.srv.CheckStoredSinks(ctx)
	if err == nil || !strings.Contains(err.Error(), "sink old /config/token") {
		t.Fatalf("literal secret: %v", err)
	}
	noSecretLeak(t, "error", err.Error())
}

// The package-level check is what a role that opens the store without
// serving the API (scheduler, dispatcher) runs before it starts.
func TestCheckStoredSinksWithoutServer(t *testing.T) {
	secretKind := PluginInfo{
		Kind: "chat", Interface: "sink", Runtime: "wasm", Version: "1.0.0",
		ConfigSchema: json.RawMessage(`{"type":"object","properties":{"token":{"type":"string","x-angelia-secret":true}}}`),
	}
	e := newEnv(t)
	ctx := context.Background()
	plugins := []PluginInfo{secretKind, {Kind: "webhook", Interface: "sink", Runtime: "builtin", Version: "0.1.0"}}
	if err := CheckStoredSinks(ctx, e.st, plugins); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if err := e.st.CreateSink(ctx, store.Sink{ID: "01B", Kind: "chat", Name: "old", ConfigEncrypted: []byte(`{"token":"` + leakCanary + `"}`)}); err != nil {
		t.Fatal(err)
	}
	err := CheckStoredSinks(ctx, e.st, plugins)
	if err == nil || !strings.Contains(err.Error(), "sink old /config/token") {
		t.Fatalf("literal secret: %v", err)
	}
	noSecretLeak(t, "error", err.Error())
	if err := CheckStoredSinks(ctx, nil, plugins); err == nil {
		t.Fatal("nil store")
	}
}
