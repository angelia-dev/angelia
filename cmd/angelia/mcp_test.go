package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	contract "github.com/angelia-dev/angelia/api/mcp"
	"github.com/angelia-dev/angelia/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/store"
)

// mcpPipe runs `angelia mcp` in process with stdin and stdout on pipes and
// speaks newline-delimited JSON-RPC to it, as an agent host does. It does
// not use the SDK: only internal/mcp imports it (ADR 0029).
type mcpPipe struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *bufio.Reader
	stdout bytes.Buffer // every frame read
	stderr bytes.Buffer // read only after done
	done   chan error
	id     int
}

func startMCP(t *testing.T, args ...string) *mcpPipe {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	p := &mcpPipe{t: t, in: inW, out: bufio.NewReader(outR), done: make(chan error, 1)}
	root := newRootCmd()
	root.SetIn(inR)
	root.SetOut(outW)
	root.SetErr(&p.stderr)
	root.SetArgs(append([]string{"mcp"}, args...))
	go func() {
		p.done <- root.ExecuteContext(t.Context())
		_ = outW.Close()
	}()
	return p
}

func (p *mcpPipe) send(msg map[string]any) {
	p.t.Helper()
	msg["jsonrpc"] = "2.0"
	b, err := json.Marshal(msg)
	if err != nil {
		p.t.Fatal(err)
	}
	if _, err := p.in.Write(append(b, '\n')); err != nil {
		p.t.Fatal(err)
	}
}

// request sends a request and returns its result, failing on a JSON-RPC error.
func (p *mcpPipe) request(method string, params any) map[string]any {
	p.t.Helper()
	p.id++
	p.send(map[string]any{"id": p.id, "method": method, "params": params})
	for {
		line, err := p.out.ReadBytes('\n')
		if err != nil {
			p.t.Fatalf("%s: read stdout: %v", method, err)
		}
		p.stdout.Write(line)
		var m struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Result  map[string]any  `json:"result"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(line, &m); err != nil || m.JSONRPC != "2.0" {
			p.t.Fatalf("stdout holds a line that is not a JSON-RPC frame: %q", line)
		}
		if m.ID == nil || *m.ID != p.id {
			continue // a notification
		}
		if m.Error != nil {
			p.t.Fatalf("%s: JSON-RPC error %s", method, m.Error)
		}
		return m.Result
	}
}

// call runs a tool and returns its structured content.
func (p *mcpPipe) call(name string, args map[string]any) map[string]any {
	p.t.Helper()
	res := p.request("tools/call", map[string]any{"name": name, "arguments": args})
	if res["isError"] == true {
		p.t.Fatalf("%s: tool error %v", name, res["content"])
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc == nil {
		p.t.Fatalf("%s: no structured content: %v", name, res)
	}
	return sc
}

// close ends stdin, as the host does, and waits for the command.
func (p *mcpPipe) close() error {
	p.t.Helper()
	_ = p.in.Close()
	select {
	case err := <-p.done:
		return err
	case <-time.After(10 * time.Second):
		p.t.Fatal("angelia mcp did not stop after stdin closed")
		return nil
	}
}

func TestMCPStdio(t *testing.T) {
	isolate(t)
	srv, st := newE2EServerStore(t)
	t.Setenv(envToken, e2eAdmin) // the token through the environment, as a host passes it

	p := startMCP(t, "--server", srv.URL)
	init := p.request("initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test-host", "version": "0"},
	})
	if info, _ := init["serverInfo"].(map[string]any); info["name"] != "angelia" {
		t.Errorf("initialize: %v", init)
	}
	if caps, _ := init["capabilities"].(map[string]any); caps["tools"] == nil {
		t.Errorf("no tools capability: %v", init)
	}
	p.send(map[string]any{"method": "notifications/initialized"})

	list := p.request("tools/list", map[string]any{})
	// The wire-level golden check (spec D11): the raw tools member of the
	// tools/list result, as read from stdout, must equal the tools member of
	// the vendored contract. The contract is that member only: protocol
	// members of the result (nextCursor, cacheScope, ttlMs, ...) stay outside
	// it. internal/mcp's TestToolsListMatchesContract checks the same through
	// the SDK client's types.
	var contractFile struct {
		Tools any `json:"tools"`
	}
	if err := json.Unmarshal(contract.Tools, &contractFile); err != nil {
		t.Fatal(err)
	}
	gotTools, _ := json.Marshal(list["tools"])
	wantTools, _ := json.Marshal(contractFile.Tools)
	if string(gotTools) != string(wantTools) {
		t.Errorf("tools/list on the wire differs from api/mcp/tools.v1.json:\ngot  %s\nwant %s", gotTools, wantTools)
	}
	var names []string
	for _, tl := range list["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if strings.Join(names, ",") != "ack_event,add_watch,events_since,latest_version,list_watches" {
		t.Errorf("tools %v", names)
	}

	added := p.call("add_watch", map[string]any{"name": "requests", "source": "pypi", "coordinates": map[string]any{"name": "requests"}})
	if added["change"] != "created" {
		t.Errorf("add_watch: %v", added)
	}
	id := added["watch"].(map[string]any)["id"].(string)
	w, err := st.GetWatch(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddVersion(t.Context(), store.Version{TargetID: w.TargetID, Raw: "2.32.3", Normalized: "2.32.3", FirstSeenAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := p.call("latest_version", map[string]any{"watch": "requests"}); got["version"] != "2.32.3" {
		t.Errorf("latest_version: %v", got)
	}
	if got := p.call("list_watches", map[string]any{}); len(got["items"].([]any)) != 1 {
		t.Errorf("list_watches: %v", got)
	}

	a, err := apiclient.New(apiclient.Options{Server: srv.URL, Token: e2eAdmin})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := a.PutConsumer(t.Context(), "release-bot", nil, apiclient.ConsumerWrite{}) //nolint:bodyclose // Decode closes it
	if _, err := apiclient.Decode(resp, err, nil); err != nil {
		t.Fatal(err)
	}
	if got := p.call("events_since", map[string]any{"consumer": "release-bot"}); got["has_more"] != false || len(got["items"].([]any)) != 0 {
		t.Errorf("events_since: %v", got)
	}
	if got := p.call("ack_event", map[string]any{"consumer": "release-bot", "seq": 0}); got["consumer"] != "release-bot" || got["acked_seq"] != 0.0 {
		t.Errorf("ack_event: %v", got)
	}

	if err := p.close(); err != nil {
		t.Fatalf("angelia mcp: %v (stderr %s)", err, p.stderr.String())
	}
	if strings.Contains(p.stdout.String(), e2eAdmin) || strings.Contains(p.stderr.String(), e2eAdmin) {
		t.Error("the token was printed")
	}
	if !strings.Contains(p.stderr.String(), "serving MCP on stdio") {
		t.Errorf("stderr %q", p.stderr.String())
	}
}

// TestMCPConnection: `angelia mcp` reads its connection like the other
// client commands, with the same refusals.
func TestMCPConnection(t *testing.T) {
	t.Run("loose client config", func(t *testing.T) {
		home := isolate(t)
		path := filepath.Join(home, ".config", "angelia", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("server: http://127.0.0.1:1\ntoken: "+testToken+"\n"), 0o644); err != nil { //nolint:gosec // the loose mode under test
			t.Fatal(err)
		}
		out, errOut, err := runCLI(t, "mcp")
		if err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Errorf("err %v", err)
		}
		if out != "" || strings.Contains(errOut, testToken) {
			t.Errorf("stdout %q, stderr %q", out, errOut)
		}
	})
	t.Run("token over plain http", func(t *testing.T) {
		isolate(t)
		_, _, err := runCLI(t, "mcp", "--server", "http://angelia.invalid", "--token", testToken)
		if err == nil || !strings.Contains(err.Error(), "--insecure-http") || strings.Contains(err.Error(), testToken) {
			t.Errorf("err %v", err)
		}
		p := startMCP(t, "--server", "http://angelia.invalid", "--token", testToken, "--insecure-http")
		if err := p.close(); err != nil {
			t.Errorf("with --insecure-http: %v", err)
		}
	})
}
