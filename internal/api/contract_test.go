package api

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Contract tests generated from the specification's examples: every example
// validates against its schema; each operation, run against a store seeded
// with the examples' data, answers with a declared status, a body valid
// against the declared schema and, where the data allows, the example itself.

// rawOp returns the operation object of the specification.
func rawOp(d *specDoc, op *operation) map[string]any {
	paths := d.root["paths"].(map[string]any)
	return paths[op.Path].(map[string]any)[strings.ToLower(op.Method)].(map[string]any)
}

// responseContent returns the media-type objects of a declared response.
func responseContent(d *specDoc, op *operation, status int) map[string]any {
	responses := rawOp(d, op)["responses"].(map[string]any)
	resp, _ := d.deref(responses[strconv.Itoa(status)]).(map[string]any)
	c, _ := resp["content"].(map[string]any)
	return c
}

// examplesOf returns the example values of a media-type object, by name.
func examplesOf(d *specDoc, media map[string]any) map[string]any {
	out := map[string]any{}
	exs, _ := media["examples"].(map[string]any)
	for name, ex := range exs {
		if m, ok := d.deref(ex).(map[string]any); ok {
			out[name] = m["value"]
		}
	}
	return out
}

// toJSON turns a YAML-decoded value into what encoding/json decodes (with
// json.Number), so that the validator sees JSON types.
func toJSON(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSpecExamplesValidate(t *testing.T) {
	d := newEnv(t).srv.spec
	n := 0
	for _, op := range d.ops {
		if op.Body != nil {
			rb := d.deref(rawOp(d, op)["requestBody"]).(map[string]any)
			media := rb["content"].(map[string]any)["application/json"].(map[string]any)
			for name, ex := range examplesOf(d, media) {
				var errs []fieldError
				d.validate(op.Body, toJSON(t, ex), "", &errs)
				if len(errs) > 0 {
					t.Errorf("%s request example %s: %v", op.ID, name, errs)
				}
				n++
			}
		}
		for status := range op.Statuses {
			for mt, m := range responseContent(d, op, status) {
				media := m.(map[string]any)
				if !strings.Contains(mt, "json") {
					continue
				}
				for name, ex := range examplesOf(d, media) {
					var errs []fieldError
					d.validate(media["schema"], toJSON(t, ex), "", &errs)
					if len(errs) > 0 {
						t.Errorf("%s %d example %s: %v", op.ID, status, name, errs)
					}
					n++
				}
			}
		}
	}
	if n < 40 {
		t.Fatalf("only %d examples checked", n)
	}
}

// The validator must reject what the schemas forbid, or the contract tests
// prove nothing.
func TestValidatorRejects(t *testing.T) {
	d := newEnv(t).srv.spec
	for _, tc := range []struct {
		schema, value string
		pointer       string
	}{
		{"Watch", `{}`, "/coordinates"},
		{"WatchWrite", `{"source":"pypi","coordinates":{"name":"x"},"extra":1}`, "/extra"},
		{"WatchWrite", `{"source":"pypi","coordinates":{}}`, "/coordinates"},
		{"WatchWrite", `{"source":"PyPI","coordinates":{"name":"x"}}`, "/source"},
		{"WatchWrite", `{"source":"pypi","coordinates":{"name":"x"},"policy":{"prerelease":"all"}}`, "/policy/prerelease"},
		{"WatchWrite", `{"source":"pypi","coordinates":{"name":"x"},"policy":{"bump":["major","major"]}}`, "/policy/bump/1"},
		{"WatchWrite", `{"source":"pypi","coordinates":{"name":"x"},"labels":{"a/b~":"x"}}`, "/labels/a~1b~0"},
		{"WatchWrite", `{"source":"pypi","coordinates":{"name":3}}`, "/coordinates/name"},
		{"SubscriptionWrite", `{"sink":"x","filter":{"type":[]}}`, "/filter/type"},
		{"SubscriptionWrite", `{"sink":"x","template":true}`, "/template"},
		{"Delivery", `{"id":"x"}`, "/id"},
		{"Seq", `-1`, ""},
		{"Seq", `1.5`, ""},
		{"ReplayRequest", `{"since":"2026-11-02T00:00:00Z"}`, ""},
		{"ReplayRequest", `{"event":"01JBX2A7C8D9E0F1G2H3J4K5M6","status":[]}`, "/status"},
		{"WatchStatus", `{"baseline_done":true,"effective_interval":"1h","last_checked_at":"yesterday","next_check_at":null,"error_count":0}`, "/last_checked_at"},
		{"Labels", `{"k":"` + strings.Repeat("v", 254) + `"}`, "/k"},
	} {
		v, err := decodeJSON([]byte(tc.value))
		if err != nil {
			t.Fatal(err)
		}
		var errs []fieldError
		d.validate(d.schema(tc.schema), v, "", &errs)
		if !slices.ContainsFunc(errs, func(e fieldError) bool { return e.Pointer == tc.pointer }) {
			t.Errorf("%s %s: errors %v, want one at %q", tc.schema, tc.value, errs, tc.pointer)
		}
	}
}

// contract checks one response against the operation's declaration.
type contract struct {
	t       *testing.T
	e       *env
	covered map[string]bool
}

// call runs an operation and validates the response against the
// specification. It returns the decoded body.
func (c *contract) call(opID string, wantStatus int, path, token string, body any, hdr ...string) (result, any) {
	c.t.Helper()
	op := c.e.srv.spec.operation(opID)
	if op == nil {
		c.t.Fatalf("no operation %s", opID)
	}
	r := c.e.do(op.Method, path, token, body, hdr...)
	if r.Status != wantStatus {
		c.t.Fatalf("%s %s: status %d, want %d: %s\nlog:\n%s", opID, path, r.Status, wantStatus, r.Body, c.e.log.String())
	}
	if !op.Statuses[r.Status] {
		c.t.Fatalf("%s: status %d is not declared", opID, r.Status)
	}
	c.covered[opID] = true
	content := responseContent(c.e.srv.spec, op, r.Status)
	if len(r.Body) == 0 {
		if len(content) != 0 {
			c.t.Fatalf("%s %d: empty body, but the spec declares one", opID, r.Status)
		}
		return r, nil
	}
	ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	media, ok := content[ct].(map[string]any)
	if !ok {
		c.t.Fatalf("%s %d: Content-Type %q is not declared", opID, r.Status, ct)
	}
	v, err := decodeJSON(r.Body)
	if err != nil {
		c.t.Fatalf("%s: %v", opID, err)
	}
	var errs []fieldError
	c.e.srv.spec.validate(media["schema"], v, "", &errs)
	if len(errs) > 0 {
		c.t.Fatalf("%s %d: response breaks the schema: %v\n%s", opID, r.Status, errs, r.Body)
	}
	return r, v
}

// example returns a named example of the specification.
func (c *contract) example(name string) any {
	c.t.Helper()
	ex, ok := c.e.srv.spec.lookup("#/components/examples/" + name).(map[string]any)
	if !ok {
		c.t.Fatalf("no example %s", name)
	}
	return toJSON(c.t, ex["value"])
}

// sameAsExample compares a response with an example, ignoring the keys
// named in ignore at any depth (ids the store generates) and comparing
// timestamps by instant (the server always sends milliseconds).
func (c *contract) sameAsExample(name string, got any, ignore ...string) {
	c.t.Helper()
	want := c.example(name)
	strip(want, ignore)
	strip(got, ignore)
	if !jsonSame(want, got) {
		gb, _ := json.Marshal(got)
		wb, _ := json.Marshal(want)
		c.t.Errorf("response differs from example %s:\n got %s\nwant %s", name, gb, wb)
	}
}

func strip(v any, keys []string) {
	switch x := v.(type) {
	case map[string]any:
		for _, k := range keys {
			delete(x, k)
		}
		for _, e := range x {
			strip(e, keys)
		}
	case []any:
		for _, e := range x {
			strip(e, keys)
		}
	}
}

func jsonSame(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if !jsonSame(v, y[k]) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonSame(x[i], y[i]) {
				return false
			}
		}
		return true
	case string:
		y, ok := b.(string)
		if !ok {
			return false
		}
		tx, e1 := time.Parse(time.RFC3339Nano, x)
		ty, e2 := time.Parse(time.RFC3339Nano, y)
		if e1 == nil && e2 == nil {
			return tx.Equal(ty)
		}
		return x == y
	case json.Number:
		y, ok := b.(json.Number)
		return ok && x.String() == y.String()
	}
	return reflect.DeepEqual(a, b)
}

func (c *contract) exampleBody(name string) map[string]any {
	return c.example(name).(map[string]any)
}

// seedExamples writes the data of the examples: the sink, watch and
// subscription through the API (from the request examples), and the events,
// versions, deliveries and target state directly in the store.
func seedExamples(c *contract) {
	e := c.e
	ctx := context.Background()
	// PUT with the request examples; the 201 responses match the examples.
	r, v := c.call("putSink", 201, "/v1/sinks/ci-webhook", tokAdmin, c.exampleBody("SinkPut"))
	if r.Header.Get("Angelia-Change") != "created" || r.Header.Get("Location") != "/v1/sinks/ci-webhook" {
		c.t.Errorf("putSink headers %v", r.Header)
	}
	c.sameAsExample("Sink", v, "id")
	e.now = time.Date(2026, 10, 30, 16, 0, 0, 0, time.UTC)
	_, v = c.call("putWatch", 201, "/v1/watches/requests", tokWrite, c.exampleBody("WatchPut"))
	c.sameAsExample("WatchCreated", v, "id")
	_, v = c.call("putSubscription", 201, "/v1/subscriptions/platform-ci", tokWrite, c.exampleBody("SubscriptionPut"))
	c.sameAsExample("Subscription", v, "id")
	e.now = time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)

	// The target as the Watch example shows it.
	w, err := e.st.WatchByName(ctx, "requests")
	if err != nil {
		c.t.Fatal(err)
	}
	tg, err := e.st.GetTarget(ctx, w.TargetID)
	if err != nil {
		c.t.Fatal(err)
	}
	tg.LastCheckedAt = time.Date(2026, 10, 30, 17, 0, 2, 512e6, time.UTC)
	tg.NextCheckAt = time.Date(2026, 10, 30, 17, 29, 41, 0, time.UTC)
	if err := e.st.UpdateTarget(ctx, tg); err != nil {
		c.t.Fatal(err)
	}
	if err := e.st.MarkBaselineDone(ctx, w.ID); err != nil {
		c.t.Fatal(err)
	}
	// Versions of the VersionPage example, and an older one so that the
	// page of two has a next page.
	for _, v := range []store.Version{
		{Raw: "2.33.0", Normalized: "2.33.0", PublishedAt: time.Date(2026, 10, 27, 14, 2, 11, 0, time.UTC), FirstSeenAt: time.Date(2026, 10, 27, 14, 15, 0, 0, time.UTC)},
		{Raw: "2.32.5", Normalized: "2.32.5", PublishedAt: time.Date(2026, 8, 18, 9, 12, 40, 0, time.UTC), FirstSeenAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)},
		{Raw: "2.31.0", Normalized: "2.31.0", FirstSeenAt: time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC)},
	} {
		v.TargetID = tg.ID
		if _, err := e.st.AddVersion(ctx, v); err != nil {
			c.t.Fatal(err)
		}
	}
	// The two events of the EventPage example, with the deliveries of the
	// DeliveryPage example on the first.
	page := c.exampleBody("EventPage")
	subID := e.subscriptionID("platform-ci")
	for i, it := range page["items"].([]any) {
		rec := it.(map[string]any)
		env := rec["event"].(map[string]any)
		payload, _ := json.Marshal(env)
		ev := store.Event{
			ID: env["id"].(string), WatchID: w.ID, Type: env["type"].(string), Payload: payload,
			CreatedAt: e.now,
		}
		if ref, ok := rec["ref_event_id"].(string); ok {
			ev.RefEventID = ref
		}
		var ds []store.Delivery
		if i == 0 {
			ds = []store.Delivery{
				{
					ID: "01JBX2A9QWERTY0123456789AB", SubscriptionID: subID, Status: store.DeliveryDead, Attempts: 1,
					LastError: "webhook: https://ci.example.com: unexpected status 404 Not Found",
				},
				{
					ID: "01JBX2B0ASDFGH0123456789CD", SubscriptionID: "01JBX0ZZ9Y8X7W6V5T4S3R2Q1P", Status: store.DeliveryDead,
					LastError: "subscription removed",
				},
			}
		}
		if err := e.st.AppendEvent(ctx, ev, ds); err != nil {
			c.t.Fatal(err)
		}
	}
}

// TestContract runs every operation of the specification.
func TestContract(t *testing.T) {
	e := newEnv(t, func(o *Options) {
		o.Plugins = append(o.Plugins, PluginInfo{
			Kind: "crates", Interface: "source", Runtime: "wasm", Version: "0.3.1", ABI: "angelia:host/v0",
			ConfigSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`),
			AllowedHosts: []PluginHostInfo{
				{Host: "crates.io", Origin: "manifest"},
				{Host: "crates-mirror.example.com", Origin: "operator", CredentialPorts: []int{443, 8443}},
			},
		})
	})
	c := &contract{t: t, e: e, covered: map[string]bool{}}
	seedExamples(c)

	// Watches.
	_, v := c.call("getWatch", 200, "/v1/watches/requests", tokRead, nil)
	c.sameAsExample("Watch", v, "id")
	_, v = c.call("listWatches", 200, "/v1/watches?source=pypi&label=team%3Dplatform", tokRead, nil)
	c.sameAsExample("WatchPage", v, "id")
	r, v := c.call("putWatch", 200, "/v1/watches/requests", tokWrite, c.exampleBody("WatchPut"))
	if r.Header.Get("Angelia-Change") != "unchanged" {
		t.Errorf("second PUT: %s", r.Header.Get("Angelia-Change"))
	}
	c.sameAsExample("Watch", v, "id")
	_, v = c.call("listWatchVersions", 200, "/v1/watches/requests/versions?limit=2", tokRead, nil)
	c.sameAsExample("VersionPage", v, "next_cursor")

	// Events.
	_, v = c.call("listEvents", 200, "/v1/events", tokRead, nil)
	c.sameAsExample("EventPage", v, "seq", "next_after")
	_, v = c.call("getEvent", 200, "/v1/events/01JBX2A7C8D9E0F1G2H3J4K5M6", tokRead, nil)
	c.sameAsExample("EventRecord", v, "seq")

	// Sinks: an admin sees the URL, a read token its scheme and host.
	_, v = c.call("getSink", 200, "/v1/sinks/ci-webhook", tokAdmin, nil)
	c.sameAsExample("Sink", v, "id")
	_, v = c.call("getSink", 200, "/v1/sinks/ci-webhook", tokRead, nil)
	c.sameAsExample("SinkRedacted", v, "id")
	_, v = c.call("listSinks", 200, "/v1/sinks?kind=webhook", tokRead, nil)
	c.sameAsExample("SinkPage", v, "id")

	// Subscriptions.
	_, v = c.call("getSubscription", 200, "/v1/subscriptions/platform-ci", tokRead, nil)
	c.sameAsExample("Subscription", v, "id")
	_, v = c.call("listSubscriptions", 200, "/v1/subscriptions?sink=ci-webhook", tokRead, nil)
	c.sameAsExample("SubscriptionPage", v, "id")

	// Deliveries.
	_, v = c.call("listDeliveries", 200, "/v1/deliveries?status=dead", tokRead, nil)
	c.sameAsExample("DeliveryPage", v, "subscription_id", "idempotency_key", "next_cursor")
	_, v = c.call("getDelivery", 200, "/v1/deliveries/01JBX2A9QWERTY0123456789AB", tokRead, nil)
	c.sameAsExample("DeliveryDead", v, "subscription_id", "idempotency_key")
	_, v = c.call("createReplay", 200, "/v1/replays", tokWrite, c.exampleBody("ReplayByEvent"))
	c.sameAsExample("ReplayResult", v, "subscription_id", "idempotency_key")
	_, v = c.call("replayDelivery", 409, "/v1/deliveries/01JBX2A9QWERTY0123456789AB/replay", tokWrite, map[string]any{})
	c.sameAsExample("ProblemNotReplayable", v)
	_, _ = c.call("createReplay", 200, "/v1/replays", tokWrite, c.exampleBody("ReplayBySubscription"))

	// Plugins.
	_, v = c.call("getPlugin", 200, "/v1/plugins/source/crates", tokRead, nil)
	c.sameAsExample("PluginWasm", v)
	_, v = c.call("listPlugins", 200, "/v1/plugins", tokRead, nil)
	items := v.(map[string]any)["items"].([]any)
	want := c.exampleBody("PluginPage")["items"].([]any)
	for _, w := range want {
		kind := w.(map[string]any)["kind"]
		i := slices.IndexFunc(items, func(x any) bool { return x.(map[string]any)["kind"] == kind })
		if i < 0 || !jsonSame(w, items[i]) {
			t.Errorf("plugin %v: %v", kind, items)
		}
	}

	// Problems with the examples' wording.
	_, v = c.call("listWatches", 400, "/v1/watches?cursor=eyJuIjoicmVxdWVzdHMifQ", tokRead, nil)
	c.sameAsExample("ProblemBadRequest", v)
	_, v = c.call("listWatches", 401, "/v1/watches", "", nil)
	c.sameAsExample("ProblemUnauthorized", v)
	_, v = c.call("putWatch", 403, "/v1/watches/requests", tokRead, c.exampleBody("WatchPut"))
	c.sameAsExample("ProblemForbidden", v)
	_, v = c.call("deleteSink", 409, "/v1/sinks/ci-webhook", tokAdmin, nil)
	c.sameAsExample("ProblemSinkInUse", v)
	_, v = c.call("putSink", 422, "/v1/sinks/ci-webhook", tokAdmin, map[string]any{
		"kind": "webhook", "config": map[string]any{"url": "https://ci.example.com/hooks/angelia", "secret_env": "OTHER_SECRET"},
	})
	c.sameAsExample("ProblemEnvNotAllowed", v)
	bad := c.exampleBody("WatchPut")
	bad["interval"] = "1m"
	bad["policy"].(map[string]any)["prerelease"] = "sometimes"
	_, v = c.call("putWatch", 422, "/v1/watches/requests", tokWrite, bad)
	c.sameAsExample("ProblemValidation", v, "detail") // the errors' details word the same rules differently
	r = e.do("PUT", "/v1/watches/requests", tokWrite, "source: pypi", "Content-Type", "application/yaml")
	c.covered["putWatch"] = true
	if r.Status != 415 || !jsonSame(c.example("ProblemUnsupportedMediaType"), toJSON(t, r.json(t))) {
		t.Errorf("415: %d %s", r.Status, r.Body)
	}

	// Deletes, in reverse dependency order, with dry runs first.
	r, _ = c.call("deleteSubscription", 204, "/v1/subscriptions/platform-ci?dry_run=true", tokWrite, nil)
	if r.Header.Get("Angelia-Change") != "deleted" {
		t.Errorf("dry-run delete: %v", r.Header)
	}
	c.call("getSubscription", 200, "/v1/subscriptions/platform-ci", tokRead, nil)
	c.call("deleteSubscription", 204, "/v1/subscriptions/platform-ci", tokWrite, nil)
	c.call("getSubscription", 404, "/v1/subscriptions/platform-ci", tokRead, nil)
	c.call("deleteSink", 204, "/v1/sinks/ci-webhook", tokAdmin, nil)
	c.call("deleteWatch", 204, "/v1/watches/requests", tokWrite, nil)
	_, v = c.call("getWatch", 404, "/v1/watches/requests", tokRead, nil)
	c.sameAsExample("ProblemNotFound", v)

	// The stream answers with its own test (TestEventStream in events_test.go).
	c.covered["streamEvents"] = true
	for _, op := range e.srv.spec.ops {
		if !c.covered[op.ID] {
			t.Errorf("operation %s is not covered", op.ID)
		}
	}
}

func TestValidatorEdges(t *testing.T) {
	e := newEnv(t)
	d := e.srv.spec
	check := func(schema any, value string) []fieldError {
		t.Helper()
		v, err := decodeJSON([]byte(value))
		if err != nil {
			t.Fatal(err)
		}
		var errs []fieldError
		d.validate(schema, v, "", &errs)
		return errs
	}
	// An integral number is an integer whatever its spelling; a huge
	// exponent is out of range, quickly.
	for _, v := range []string{`1`, `1.0`, `1e2`, `100E-2`} {
		if errs := check(d.schema("Seq"), v); len(errs) != 0 {
			t.Errorf("Seq %s: %v", v, errs)
		}
	}
	for _, v := range []string{`1.5`, `1e999999999`, `9223372036854775808`} {
		if errs := check(d.schema("Seq"), v); len(errs) == 0 {
			t.Errorf("Seq %s accepted", v)
		}
	}
	unique := map[string]any{"type": "array", "uniqueItems": true}
	if errs := check(unique, `[1, 1.0, {"a":1,"b":2}, {"b":2,"a":1.0}]`); len(errs) != 2 || errs[0].Pointer != "/1" || errs[1].Pointer != "/3" {
		t.Errorf("uniqueItems: %v", errs)
	}
	if errs := check(unique, `[9007199254740993, 9007199254740992]`); len(errs) != 0 {
		t.Errorf("uniqueItems on large integers: %v", errs)
	}
	// format: date-time
	for v, ok := range map[string]bool{`"2026-11-03T10:00:00Z"`: true, `"2026-11-03T10:00:00.123Z"`: true, `"2026-13-45T00:00:00Z"`: false, `"yesterday"`: false} {
		if errs := check(d.schema("Timestamp"), v); (len(errs) == 0) != ok {
			t.Errorf("Timestamp %s: %v", v, errs)
		}
	}
	r := e.do("POST", "/v1/replays", tokWrite, map[string]any{"subscription": "x", "since": "2026-13-45T00:00:00Z", "until": "2026-11-04T00:00:00Z"})
	if r.Status != 422 || !strings.Contains(string(r.Body), `"pointer":"/since"`) {
		t.Errorf("bad since: %d %s", r.Status, r.Body)
	}
	// A pattern that does not compile is a broken specification.
	bad := []byte("security: [{bearerAuth: [read]}]\npaths:\n  /v1/x:\n    get:\n      operationId: x\n      responses: {'200': {description: ok}}\n" +
		"components:\n  schemas:\n    Bad: {type: string, pattern: '(['}\n")
	if _, err := parseSpec(bad); err == nil || !strings.Contains(err.Error(), "Bad") {
		t.Errorf("parseSpec: %v", err)
	}
}
