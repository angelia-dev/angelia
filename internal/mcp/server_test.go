package mcp

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	contract "github.com/angelia-dev/angelia/api/mcp"
)

// TestToolsListMatchesContract is the golden test of spec D11: tools/list,
// as a client receives it, equals the vendored api/mcp/tools.v1.json.
func TestToolsListMatchesContract(t *testing.T) {
	cs := connect(t, "http://127.0.0.1:1", "")
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(map[string]any{"tools": res.Tools})
	if err != nil {
		t.Fatal(err)
	}
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(contract.Tools, &w); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(g, w) {
		t.Fatalf("tools/list differs from api/mcp/tools.v1.json; update the server or re-vendor the contract on purpose.\ngot:  %s", got)
	}

	// The rules the contract states, spelled out so that a failure says which.
	reads := map[string]bool{"list_watches": true, "latest_version": true, "events_since": true}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		a := tl.Annotations
		if a == nil || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil {
			t.Errorf("%s: annotations %+v lack a hint or are destructive", tl.Name, a)
			continue
		}
		if reads[tl.Name] {
			if !a.ReadOnlyHint || !*a.OpenWorldHint {
				t.Errorf("%s: a read must be read-only and open world", tl.Name)
			}
			if !strings.Contains(tl.Description, untrusted) {
				t.Errorf("%s: the description lacks the untrusted-data sentence", tl.Name)
			}
		} else if a.ReadOnlyHint || !a.IdempotentHint || *a.OpenWorldHint {
			t.Errorf("%s: a write must be idempotent, not read-only, closed world", tl.Name)
		}
	}
	if strings.Join(names, ",") != "ack_event,add_watch,events_since,latest_version,list_watches" {
		t.Errorf("tools %v", names)
	}
}

// TestOnlyThisPackageImportsTheSDK enforces ADR 0029: the SDK stays behind
// internal/mcp.
func TestOnlyThisPackageImportsTheSDK(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	self := filepath.Join(root, "internal", "mcp")
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "testdata" || n == ".claude" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || filepath.Dir(p) == self {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, im := range f.Imports {
			if path, _ := strconv.Unquote(im.Path.Value); strings.HasPrefix(path, "github.com/modelcontextprotocol/") {
				t.Errorf("%s imports %s", p, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestRESTProblemsPassThrough: a REST problem becomes a tool error whose
// text is the problem as the API sent it, extension members included.
func TestRESTProblemsPassThrough(t *testing.T) {
	const base = "https://github.com/angelia-dev/spec/blob/main/docs/api.md#problem-"
	cases := []struct {
		status int
		body   string
	}{
		{400, `{"type":"` + base + `bad-request","title":"Bad request","status":400,"detail":"query parameter limit is not an integer"}`},
		{401, `{"type":"` + base + `unauthorized","title":"Unauthorized","status":401,"detail":"a valid bearer token is required"}`},
		{403, `{"type":"` + base + `forbidden","title":"Forbidden","status":403,"detail":"this operation needs the write scope","required_scope":"write"}`},
		{404, `{"type":"` + base + `not-found","title":"Not found","status":404,"detail":"no watch named requests"}`},
		{410, `{"type":"` + base + `cursor-expired","title":"Cursor expired","status":410,"detail":"events up to 41 were pruned","oldest_seq":42}`},
		{422, `{"type":"` + base + `validation-failed","title":"Validation failed","status":422,"detail":"the query has 1 error","errors":[{"pointer":"/watch/0","detail":"no watch named x"}],"x-future":true}`},
		{503, `{"type":"` + base + `unavailable","title":"Unavailable","status":503,"detail":"the server is shutting down"}`},
	}
	for _, c := range cases {
		t.Run(strconv.Itoa(c.status), func(t *testing.T) {
			srv := fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body + "\n"))
			})
			cs := connect(t, srv.URL, tokRead)
			for name, args := range map[string]any{
				"list_watches":   map[string]any{},
				"events_since":   map[string]any{"after": 3},
				"latest_version": map[string]any{"watch": "requests"},
				"add_watch":      map[string]any{"name": "requests", "source": "pypi", "coordinates": map[string]string{"name": "requests"}},
				"ack_event":      map[string]any{"consumer": "bot", "seq": 1},
			} {
				res := callTool(t, cs, name, args)
				failure(t, res)
				if got := resultText(t, res); got != c.body {
					t.Errorf("%s: text %s, want the problem verbatim %s", name, got, c.body)
				}
			}
		})
	}
}

func TestAPIUnreachable(t *testing.T) {
	notProblem := fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>proxy error</html>"))
	})
	badJSON := fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	})
	closed := fakeAPI(t, func(http.ResponseWriter, *http.Request) {})
	closedURL := closed.URL
	closed.Close()
	for name, url := range map[string]string{"not a problem": notProblem.URL, "bad json": badJSON.URL, "closed": closedURL} {
		t.Run(name, func(t *testing.T) {
			res := callTool(t, connect(t, url, tokRead), "list_watches", map[string]any{})
			p := failure(t, res)
			if p["type"] != mcpProblem+"api-unreachable" || p["status"] != 502.0 || p["title"] != "API unreachable" {
				t.Errorf("problem %v", p)
			}
			if strings.Contains(resultText(t, res), tokRead) {
				t.Error("the token is in the tool error")
			}
		})
	}
}

// goInternals matches what a Go decode error would leak into a detail.
var goInternals = regexp.MustCompile(`json:|unmarshal|Go (struct|value)|int64|float64`)

// TestInvalidArguments: arguments that fail the inputSchema are the tool
// error invalid-arguments with {pointer, detail} items, and no REST call.
func TestInvalidArguments(t *testing.T) {
	var calls atomic.Int32
	srv := fakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTeapot)
	})
	cs := connect(t, srv.URL, tokRead)
	cases := []struct {
		tool     string
		args     any
		pointers []string
	}{
		{"list_watches", map[string]any{"limit": 101}, []string{"/limit"}},
		{"list_watches", map[string]any{"limit": 0, "colour": "red"}, []string{"/colour", "/limit"}},
		{"list_watches", map[string]any{"label": []any{"no-equals-sign"}}, []string{"/label"}},
		{"latest_version", map[string]any{}, []string{"/watch"}},
		{"latest_version", map[string]any{"watch": "Upper"}, []string{"/watch"}},
		{"events_since", map[string]any{"consumer": "bot", "after": 3, "watch": []any{"x"}}, []string{"/after", "/watch"}},
		{"events_since", map[string]any{"limit": 500}, []string{"/limit"}},
		{"events_since", map[string]any{"after": 1.5}, []string{"/after"}},
		{"events_since", map[string]any{"type": []any{"release.deleted"}}, []string{"/type"}},
		{"add_watch", map[string]any{"name": "x", "source": "pypi", "coordinates": map[string]any{}, "policy": map[string]any{"prerelease": "maybe"}}, []string{"/coordinates", "/policy/prerelease"}},
		{"add_watch", map[string]any{"source": "pypi"}, []string{"/name", "/coordinates"}},
		{"ack_event", map[string]any{"consumer": "bot"}, []string{""}},
		{"ack_event", map[string]any{"consumer": "bot", "seq": 1, "event_id": "01JBX2A7C8D9E0F1G2H3J4K5M6"}, []string{""}},
		{"ack_event", map[string]any{"consumer": "bot", "seq": 1e30}, []string{"/seq"}},
		{"events_since", json.RawMessage(`{"after":9223372036854775808}`), []string{"/after"}},
	}
	for _, c := range cases {
		b, _ := json.Marshal(c.args)
		t.Run(c.tool+string(b), func(t *testing.T) {
			p := failure(t, callTool(t, cs, c.tool, c.args))
			if p["type"] != mcpProblem+"invalid-arguments" || p["status"] != 400.0 || p["title"] != "Invalid arguments" {
				t.Errorf("problem %v", p)
			}
			errs, _ := p["errors"].([]any)
			var ptrs []string
			for _, e := range errs {
				m, _ := e.(map[string]any)
				if d, _ := m["detail"].(string); d == "" || strings.HasPrefix(d, "validating") || goInternals.MatchString(d) {
					t.Errorf("detail %q", d)
				}
				ptrs = append(ptrs, m["pointer"].(string))
			}
			if strings.Join(ptrs, ",") != strings.Join(c.pointers, ",") {
				t.Errorf("pointers %q, want %q (%v)", ptrs, c.pointers, errs)
			}
		})
	}
	if n := calls.Load(); n != 0 {
		t.Errorf("%d REST calls for invalid arguments", n)
	}

	// An unknown tool is a protocol error, not a tool error.
	if _, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "delete_everything"}); err == nil {
		t.Error("an unknown tool got no protocol error")
	}
}
