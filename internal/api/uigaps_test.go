package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/store"
)

// The API gaps the admin UI found (story #274, spec 0.9.0).

// eventPage decodes a GET /v1/events response with next_before.
type descPage struct {
	Items []struct {
		Seq int64 `json:"seq"`
	} `json:"items"`
	NextAfter  int64  `json:"next_after"`
	HasMore    bool   `json:"has_more"`
	NextBefore *int64 `json:"next_before"`
}

func (e *env) descPage(query string) descPage {
	e.t.Helper()
	r := e.must(http.StatusOK, e.do("GET", "/v1/events?"+query, tokRead, nil))
	var p descPage
	if err := json.Unmarshal(r.Body, &p); err != nil {
		e.t.Fatal(err)
	}
	if !strings.Contains(string(r.Body), `"next_before":`) {
		e.t.Fatalf("next_before is missing: %s", r.Body)
	}
	return p
}

func (p descPage) seqs() []int64 {
	out := make([]int64, len(p.Items))
	for i, it := range p.Items {
		out[i] = it.Seq
	}
	return out
}

func TestEventsNewestFirst(t *testing.T) {
	e := newEnv(t)
	// An empty log: nothing scanned, no cursor.
	p := e.descPage("order=desc")
	if len(p.Items) != 0 || p.NextAfter != 0 || p.HasMore || p.NextBefore != nil {
		t.Fatalf("empty log: %+v", p)
	}
	e.seedConfig()
	wid := e.watchID("requests")
	var seqs []int64
	for i := range 5 {
		typ := event.TypePublished
		if i == 2 {
			typ = event.TypeYanked
		}
		seqs = append(seqs, e.appendEvent(wid, typ, "pypi", e.now.Add(time.Duration(i)*time.Minute)).Seq)
	}
	// Commit order keeps next_before null.
	if p := e.descPage("limit=2"); p.NextBefore != nil || fmt.Sprint(p.seqs()) != fmt.Sprint(seqs[:2]) {
		t.Fatalf("asc page: %+v", p)
	}
	// Newest first, paged with before = next_before.
	p = e.descPage("order=desc&limit=2")
	if fmt.Sprint(p.seqs()) != fmt.Sprint([]int64{seqs[4], seqs[3]}) || p.NextAfter != seqs[4] || !p.HasMore || p.NextBefore == nil || *p.NextBefore != seqs[3] {
		t.Fatalf("first page: %+v", p)
	}
	p = e.descPage(fmt.Sprintf("order=desc&limit=2&before=%d", *p.NextBefore))
	if fmt.Sprint(p.seqs()) != fmt.Sprint([]int64{seqs[2], seqs[1]}) || p.NextAfter != seqs[2] || !p.HasMore || *p.NextBefore != seqs[1] {
		t.Fatalf("second page: %+v", p)
	}
	p = e.descPage(fmt.Sprintf("order=desc&limit=2&before=%d", *p.NextBefore))
	if fmt.Sprint(p.seqs()) != fmt.Sprint([]int64{seqs[0]}) || p.HasMore || *p.NextBefore != seqs[0] {
		t.Fatalf("last page: %+v", p)
	}
	// Below the oldest event: an empty page that keeps the cursor, not 410.
	p = e.descPage(fmt.Sprintf("order=desc&before=%d", seqs[0]))
	if len(p.Items) != 0 || p.HasMore || *p.NextBefore != seqs[0] || p.NextAfter != seqs[0]-1 {
		t.Fatalf("below the oldest: %+v", p)
	}
	// The filters apply as in commit order.
	p = e.descPage("order=desc&type=release.yanked")
	if fmt.Sprint(p.seqs()) != fmt.Sprint([]int64{seqs[2]}) || p.HasMore || *p.NextBefore != seqs[0] {
		t.Fatalf("filtered: %+v", p)
	}
	// The next_after of the first page follows new events without a gap.
	head := e.descPage("order=desc&limit=1").NextAfter
	next := e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(time.Hour)).Seq
	if p := e.descPage(fmt.Sprintf("after=%d", head)); fmt.Sprint(p.seqs()) != fmt.Sprint([]int64{next}) {
		t.Fatalf("follow from next_after: %+v", p)
	}
	for _, q := range []string{"order=desc&after=1", "order=desc&after=0", "order=desc&consumer=bot", "before=3", "order=asc&before=3", "order=sideways"} {
		r := e.do("GET", "/v1/events?"+q, tokRead, nil)
		if r.Status != http.StatusBadRequest || r.problemType(t) != SlugBadRequest {
			t.Errorf("%s: %d %s", q, r.Status, r.Body)
		}
	}
}

// The scan is bounded backwards too: a page can be empty with has_more.
func TestEventsNewestFirstBoundedScan(t *testing.T) {
	e := newEnv(t, func(o *Options) { o.EventScan = 2 })
	e.seedConfig()
	wid := e.watchID("requests")
	var seqs []int64
	for i := range 4 {
		seqs = append(seqs, e.appendEvent(wid, event.TypePublished, "pypi", e.now.Add(time.Duration(i)*time.Minute)).Seq)
	}
	p := e.descPage("order=desc&limit=1&type=release.yanked")
	if len(p.Items) != 0 || !p.HasMore || *p.NextBefore != seqs[2] || p.NextAfter != seqs[3] {
		t.Fatalf("bounded: %+v", p)
	}
}

func TestDeliveryFiltersAndSink(t *testing.T) {
	e, evs := deliveryEnv(t) // events at now, now+1h, now+2h
	ts := func(d time.Duration) string { return url.QueryEscape(e.now.Add(d).Format(timestampLayout)) }
	for _, tc := range []struct {
		query string
		n     int
	}{
		{"sink=ci-webhook", 3},
		{"sink=nothing", 0},
		{"sink=ci-webhook&subscription=platform-ci", 3},
		{"sink=ci-webhook&subscription=nothing", 0},
		{"since=" + ts(time.Hour), 2},
		{"until=" + ts(time.Hour), 1},
		{"since=" + ts(30*time.Minute) + "&until=" + ts(90*time.Minute), 1},
		{"since=" + ts(3*time.Hour), 0},
	} {
		m := e.must(http.StatusOK, e.do("GET", "/v1/deliveries?"+tc.query, tokRead, nil)).json(t)
		if n := len(m["items"].([]any)); n != tc.n {
			t.Errorf("%q: %d items, want %d", tc.query, n, tc.n)
		}
	}
	if d := e.deliveryOf(evs[1]); d["sink"] != "ci-webhook" || d["subscription_name"] != "platform-ci" {
		t.Fatalf("delivery: %v", d)
	}
	d := e.must(http.StatusOK, e.do("GET", "/v1/deliveries/"+e.deliveryOf(evs[0])["id"].(string), tokRead, nil)).json(t)
	if d["sink"] != "ci-webhook" {
		t.Fatalf("getDelivery: %v", d)
	}
	for _, q := range []string{"since=" + ts(time.Hour) + "&until=" + ts(time.Hour), "since=" + ts(time.Hour) + "&until=" + ts(0), "since=yesterday"} {
		if r := e.do("GET", "/v1/deliveries?"+q, tokRead, nil); r.Status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", q, r.Status, r.Body)
		}
	}
	// Once the subscription is deleted, the sink is null too.
	e.must(http.StatusNoContent, e.do("DELETE", "/v1/subscriptions/platform-ci", tokWrite, nil))
	if d := e.deliveryOf(evs[1]); d["sink"] != nil || d["subscription_name"] != nil {
		t.Fatalf("delivery of a deleted subscription: %v", d)
	}
	if m := e.must(http.StatusOK, e.do("GET", "/v1/deliveries?sink=ci-webhook", tokRead, nil)).json(t); len(m["items"].([]any)) != 0 {
		t.Fatalf("sink filter after the delete: %v", m)
	}
}

func TestMeSessionID(t *testing.T) {
	e := newEnv(t, withSessions(newFakeSessions()))
	m := e.must(http.StatusOK, e.do("GET", "/v1/me", "", nil, cookieHdr(sessRead)...)).json(t)
	if m["session_id"] != "01JD08F2K5M8P1R4T7W0Y3B6C9" {
		t.Fatalf("session: %v", m)
	}
	r := e.must(http.StatusOK, e.do("GET", "/v1/me", tokRead, nil))
	if m := r.json(t); m["session_id"] != nil || !strings.Contains(string(r.Body), `"session_id":null`) {
		t.Fatalf("token: %s", r.Body)
	}
}

func TestListAccess(t *testing.T) {
	access := []config.UIAccess{
		{GitHubUser: "MMornati", Role: "admin"},
		{GitHubID: 5550001, Role: "editor"},
		{GitHubUser: "octo-viewer", Role: "viewer"},
	}
	e := newEnv(t, func(o *Options) { o.UIAccess = access })
	if _, err := e.st.PinUIIdentity(context.Background(), "github", "mmornati", "139323", e.now); err != nil {
		t.Fatal(err)
	}
	r := e.must(http.StatusOK, e.do("GET", "/v1/access", tokAdmin, nil))
	want := `{"items":[` +
		`{"provider":"github","kind":"user","login":"mmornati","subject":"139323","pinned":true,"role":"admin"},` +
		`{"provider":"github","kind":"user","login":null,"subject":"5550001","pinned":false,"role":"editor"},` +
		`{"provider":"github","kind":"user","login":"octo-viewer","subject":null,"pinned":false,"role":"viewer"}]}`
	if strings.TrimSpace(string(r.Body)) != want {
		t.Fatalf("access:\n got %s\nwant %s", r.Body, want)
	}
	// Listing pins nobody.
	if _, err := e.st.UIIdentityPin(context.Background(), "github", "octo-viewer"); err == nil {
		t.Fatal("listing pinned a login")
	}
	if r := e.do("GET", "/v1/access", tokWrite, nil); r.Status != http.StatusForbidden {
		t.Fatalf("write token: %d", r.Status)
	}
	empty := newEnv(t)
	if r := empty.must(http.StatusOK, empty.do("GET", "/v1/access", tokAdmin, nil)); strings.TrimSpace(string(r.Body)) != `{"items":[]}` {
		t.Fatalf("no access list: %s", r.Body)
	}
}

func TestTemplatePreview(t *testing.T) {
	e := newEnv(t)
	e.seedConfig()
	ev := e.appendEvent(e.watchID("requests"), event.TypePublished, "pypi", e.now)
	tmpl := `{"text": "{{.data.package}} {{.data.version}}"}`
	preview := func(body map[string]any) map[string]any {
		t.Helper()
		return e.must(http.StatusOK, e.do("POST", "/v1/template-previews", tokAdmin, body)).json(t)
	}
	for name, tc := range map[string]struct {
		body        map[string]any
		output, typ string
	}{
		"default sample": {map[string]any{"template": tmpl}, `{"text": "requests 2.33.0"}`, "application/json; charset=utf-8"},
		"sparse sample":  {map[string]any{"template": "{{.data.package}}", "sample": "sparse"}, "octo-org/octo-repo", "text/plain; charset=utf-8"},
		"stored event":   {map[string]any{"template": "{{.id}}", "event_id": ev.ID}, ev.ID, "text/plain; charset=utf-8"},
		"given event":    {map[string]any{"template": "{{.data.package}}", "event": map[string]any{"data": map[string]any{"package": "flask"}}}, "flask", "text/plain; charset=utf-8"},
	} {
		m := preview(tc.body)
		if m["output"] != tc.output || m["content_type"] != tc.typ || m["error"] != nil {
			t.Errorf("%s: %v", name, m)
		}
	}
	for name, tc := range map[string]struct {
		template, kind string
		line           any
	}{
		"missing key": {`{"text": "{{.data.pakage}}"}`, "exec", float64(1)},
		"parse":       {"{{", "parse", float64(1)},
		"not allowed": {`{{define "x"}}{{end}}`, "not_allowed", nil},
		"timeout":     {"{{range 1000000000}}{{end}}", "timeout", nil},
	} {
		start := time.Now()
		m := preview(map[string]any{"template": tc.template})
		er, _ := m["error"].(map[string]any)
		if m["output"] != nil || m["content_type"] != nil || er == nil || er["kind"] != tc.kind || er["line"] != tc.line {
			t.Errorf("%s: %v", name, m)
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("%s took %v", name, d)
		}
	}
	// A reason never quotes the template text.
	r := e.must(http.StatusOK, e.do("POST", "/v1/template-previews", tokAdmin, map[string]any{"template": "{{.data.ghp_SECRETvalue1234}}"}))
	if strings.Contains(string(r.Body), "SECRET") {
		t.Fatalf("the reason quotes the template: %s", r.Body)
	}
	// A given event that is not an object is refused by the schema.
	for name, body := range map[string]map[string]any{
		"event and event_id":  {"template": tmpl, "event": map[string]any{}, "event_id": ev.ID},
		"event_id and sample": {"template": tmpl, "event_id": ev.ID, "sample": "full"},
		"event not an object": {"template": tmpl, "event": "x"},
		"empty template":      {"template": ""},
		"unknown member":      {"template": tmpl, "secret_ref": "x"},
	} {
		r := e.do("POST", "/v1/template-previews", tokAdmin, body)
		if r.Status != http.StatusUnprocessableEntity || r.problemType(t) != SlugValidationFailed {
			t.Errorf("%s: %d %s", name, r.Status, r.Body)
		}
	}
	if r := e.do("POST", "/v1/template-previews", tokAdmin, map[string]any{"template": tmpl, "event_id": "01JBX2A7C8D9E0F1G2H3J4K5M6"}); r.Status != http.StatusNotFound {
		t.Fatalf("unknown event: %d %s", r.Status, r.Body)
	}
	if r := e.do("POST", "/v1/template-previews", tokWrite, map[string]any{"template": tmpl}); r.Status != http.StatusForbidden {
		t.Fatalf("write token: %d", r.Status)
	}
}

// etagOfResponse returns the ETag of a response, failing if it has none.
func etagOfResponse(t *testing.T, r result) string {
	t.Helper()
	tag := r.Header.Get("ETag")
	if !strings.HasPrefix(tag, `"`) || !strings.HasSuffix(tag, `"`) || len(tag) < 3 {
		t.Fatalf("ETag %q (status %d)", tag, r.Status)
	}
	return tag
}

func TestETagsAndPreconditions(t *testing.T) {
	type resource struct {
		path, token string
		body        map[string]any
		change      func(map[string]any) map[string]any
	}
	clone := func(m map[string]any) map[string]any {
		b, _ := json.Marshal(m)
		var out map[string]any
		_ = json.Unmarshal(b, &out)
		return out
	}
	for _, rs := range []resource{
		{"/v1/sinks/ci-webhook", tokAdmin, sinkPut, func(m map[string]any) map[string]any {
			m["config"].(map[string]any)["url"] = "https://ci.example.com/hooks/other"
			return m
		}},
		{"/v1/watches/requests", tokWrite, watchPut, func(m map[string]any) map[string]any { m["interval"] = "1h"; return m }},
		{"/v1/subscriptions/platform-ci", tokWrite, subscriptionPut, func(m map[string]any) map[string]any { m["template"] = "{{.id}}"; return m }},
	} {
		t.Run(rs.path, func(t *testing.T) {
			e := newEnv(t)
			if !strings.Contains(rs.path, "sinks") {
				e.must(http.StatusCreated, e.do("PUT", "/v1/sinks/ci-webhook", tokAdmin, sinkPut))
			}
			// If-Match on a missing resource fails; If-None-Match: * creates.
			r := e.do("PUT", rs.path, rs.token, rs.body, "If-Match", "*")
			if r.Status != http.StatusPreconditionFailed || r.problemType(t) != SlugPreconditionFailed {
				t.Fatalf("If-Match * on a missing resource: %d %s", r.Status, r.Body)
			}
			created := etagOfResponse(t, e.must(http.StatusCreated, e.do("PUT", rs.path, rs.token, rs.body, "If-None-Match", "*")))
			// GET, any scope, and an unchanged PUT give the same tag.
			if got := etagOfResponse(t, e.must(http.StatusOK, e.do("GET", rs.path, tokRead, nil))); got != created {
				t.Fatalf("GET ETag %s, PUT ETag %s", got, created)
			}
			if got := etagOfResponse(t, e.must(http.StatusOK, e.do("GET", rs.path, tokAdmin, nil))); got != created {
				t.Fatalf("admin GET ETag %s, PUT ETag %s", got, created)
			}
			r = e.must(http.StatusOK, e.do("PUT", rs.path, rs.token, rs.body, "If-Match", created))
			if etagOfResponse(t, r) != created || r.Header.Get("Angelia-Change") != "unchanged" {
				t.Fatalf("unchanged PUT: %v", r.Header)
			}
			r = e.do("PUT", rs.path, rs.token, rs.body, "If-None-Match", "*")
			if r.Status != http.StatusPreconditionFailed || r.problemType(t) != SlugPreconditionFailed {
				t.Fatalf("If-None-Match * on an existing resource: %d %s", r.Status, r.Body)
			}
			// A change gives a new tag; the old one no longer matches,
			// a weak copy of the current one never does, a list does.
			changed := rs.change(clone(rs.body))
			updated := etagOfResponse(t, e.must(http.StatusOK, e.do("PUT", rs.path, rs.token, changed, "If-Match", `"other", `+created)))
			if updated == created {
				t.Fatal("the ETag did not change with the resource")
			}
			for _, im := range []string{created, "W/" + updated} {
				r := e.do("PUT", rs.path, rs.token, rs.body, "If-Match", im)
				if r.Status != http.StatusPreconditionFailed {
					t.Fatalf("If-Match %s: %d %s", im, r.Status, r.Body)
				}
				// Nothing was written.
				if got := etagOfResponse(t, e.must(http.StatusOK, e.do("GET", rs.path, tokRead, nil))); got != updated {
					t.Fatalf("a refused PUT changed the resource: %s", got)
				}
			}
			// A dry run checks the precondition too.
			if r := e.do("PUT", rs.path+"?dry_run=true", rs.token, rs.body, "If-Match", created); r.Status != http.StatusPreconditionFailed {
				t.Fatalf("dry run: %d", r.Status)
			}
			for _, bad := range []string{"abc", `"unterminated`} {
				if r := e.do("PUT", rs.path, rs.token, rs.body, "If-Match", bad); r.Status != http.StatusBadRequest {
					t.Fatalf("If-Match %s: %d", bad, r.Status)
				}
			}
			if r := e.do("PUT", rs.path, rs.token, rs.body, "If-None-Match", created); r.Status != http.StatusBadRequest {
				t.Fatalf("If-None-Match with a tag: %d", r.Status)
			}
			// DELETE: a stale tag fails and keeps the resource.
			if r := e.do("DELETE", rs.path, tokAdmin, nil, "If-Match", created); r.Status != http.StatusPreconditionFailed {
				t.Fatalf("DELETE with a stale tag: %d %s", r.Status, r.Body)
			}
			e.must(http.StatusOK, e.do("GET", rs.path, tokRead, nil))
			e.must(http.StatusNoContent, e.do("DELETE", rs.path, tokAdmin, nil, "If-Match", updated))
			// 404 comes before the precondition.
			if r := e.do("DELETE", rs.path, tokAdmin, nil, "If-Match", updated); r.Status != http.StatusNotFound {
				t.Fatalf("DELETE of a missing resource: %d", r.Status)
			}
		})
	}
}

// The watch's tag ignores its status, and the other refusals come before a
// failed precondition.
func TestETagIgnoresStatusAndOrder(t *testing.T) {
	e := newEnv(t)
	e.applyFile(fileConfig)
	e.seedConfig()
	tag := etagOfResponse(t, e.must(http.StatusOK, e.do("GET", "/v1/watches/requests", tokRead, nil)))
	w, err := e.st.WatchByName(context.Background(), "requests")
	if err != nil {
		t.Fatal(err)
	}
	tg, err := e.st.GetTarget(context.Background(), w.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	tg.LastCheckedAt, tg.ErrorCount, tg.NextCheckAt = e.now, 3, e.now.Add(time.Hour)
	if got := watchETag(w, tg); got != tag {
		t.Fatalf("status changed the tag: %s != %s", got, tag)
	}
	w.BaselineDone = !w.BaselineDone
	if got := watchETag(w, tg); got != tag {
		t.Fatal("the baseline changed the tag")
	}
	stale := `"stale"`
	for _, tc := range []struct {
		method, path, token string
		body                any
		status              int
	}{
		{"PUT", "/v1/watches/file-watch", tokWrite, watchPut, http.StatusConflict},
		{"DELETE", "/v1/sinks/ci-webhook", tokAdmin, nil, http.StatusConflict},
		{"PUT", "/v1/subscriptions/api-to-file", tokWrite, map[string]any{"sink": "file-hook"}, http.StatusConflict},
		{"PUT", "/v1/watches/requests", tokWrite, map[string]any{"source": "pypi"}, http.StatusUnprocessableEntity},
		{"PUT", "/v1/watches/requests", tokRead, watchPut, http.StatusForbidden},
	} {
		if r := e.do(tc.method, tc.path, tc.token, tc.body, "If-Match", stale); r.Status != tc.status {
			t.Errorf("%s %s: %d, want %d: %s", tc.method, tc.path, r.Status, tc.status, r.Body)
		}
	}
	// Stored JSON is compared in canonical form: key order does not matter.
	k := store.Sink{ID: "01JBX2A7C8D9E0F1G2H3J4K5M6", Name: "x", Kind: "webhook", ConfigEncrypted: []byte(`{"a":1,"b":[2]}`)}
	k2 := k
	k2.ConfigEncrypted = []byte(` {"b": [2], "a": 1}`)
	if sinkETag(k) != sinkETag(k2) {
		t.Fatal("the sink tag depends on the JSON spelling")
	}
}

func TestEntityTags(t *testing.T) {
	for in, want := range map[string]string{
		`*`:                       `[*]`,
		`"a"`:                     `["a"]`,
		`"a", W/"b" ,"c,d"`:       `["a" W/"b" "c,d"]`,
		"\"a\"\t,\t\"b\"":         `["a" "b"]`,
		`""`:                      `[""]`,
		`W/"x"`:                   `[W/"x"]`,
		`"a",  "b",W/"c"  ,  "d"`: `["a" "b" W/"c" "d"]`,
	} {
		if got := fmt.Sprint(entityTags(in)); got != want {
			t.Errorf("entityTags(%s) = %s, want %s", in, got, want)
		}
	}
}
