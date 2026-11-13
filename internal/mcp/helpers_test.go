package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/apiclient"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// Synthetic test tokens.
const (
	tokRead  = "mcp-read-token-00000000000000001"
	tokWrite = "mcp-write-token-0000000000000002"
)

type fakeAuth struct{}

func (fakeAuth) Authenticate(_ context.Context, token string) (api.Principal, error) {
	for t, p := range map[string]api.Principal{
		tokRead:  {ID: "read", Scope: api.ScopeRead},
		tokWrite: {ID: "write", Scope: api.ScopeWrite},
	} {
		if subtle.ConstantTimeCompare([]byte(t), []byte(token)) == 1 {
			return p, nil
		}
	}
	return api.Principal{}, fmt.Errorf("token: %w", api.ErrUnauthenticated)
}

// realAPI runs the REST API on an empty SQLite store with fakeAuth.
func realAPI(t *testing.T) (*httptest.Server, *sqlite.Store) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "api.db"), log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg := source.NewRegistry()
	if err := reg.Register(pypi.New(pypi.DefaultBaseURL, &http.Client{})); err != nil {
		t.Fatal(err)
	}
	h, err := api.New(api.Options{
		Store: st, Auth: fakeAuth{}, Sources: reg, Logger: log, API: config.API{},
		Plugins: []api.PluginInfo{{Kind: "pypi", Interface: "source", Runtime: "builtin", Version: "0.1.0"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, st
}

// connect starts the MCP server against the REST API at url, with token,
// over the SDK's in-memory transports, and returns the client session.
func connect(t *testing.T, url, token string) *sdk.ClientSession {
	t.Helper()
	a, err := apiclient.New(apiclient.Options{Server: url, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := NewServer(Options{API: a, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ct, st := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// fakeAPI serves handler as the REST API.
func fakeAPI(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error %v", name, err)
	}
	return res
}

func resultText(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("want one content block, got %d", len(res.Content))
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content is %T, not text", res.Content[0])
	}
	return tc.Text
}

// success checks a successful result: structuredContent that validates
// against the contract's outputSchema, and the same JSON in one text block.
// It returns the structured content.
func success(t *testing.T, name string, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	text := resultText(t, res)
	if res.IsError {
		t.Fatalf("%s: tool error %s", name, text)
	}
	var fromText, structured map[string]any
	if err := json.Unmarshal([]byte(text), &fromText); err != nil {
		t.Fatalf("%s: text block is not JSON: %v", name, err)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &structured); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(fromText, structured) {
		t.Errorf("%s: text %s differs from structured content %s", name, text, b)
	}
	c, err := loadContract()
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(c[name].OutputSchema, &s); err != nil {
		t.Fatal(err)
	}
	r, err := s.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(structured); err != nil {
		t.Errorf("%s: result does not match the output schema: %v\n%s", name, err, text)
	}
	return structured
}

// failure checks a tool error and returns its problem.
func failure(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	text := resultText(t, res)
	if !res.IsError {
		t.Fatalf("want a tool error, got %s", text)
	}
	if res.StructuredContent != nil {
		t.Errorf("a tool error has structured content %v", res.StructuredContent)
	}
	var p map[string]any
	if err := json.Unmarshal([]byte(text), &p); err != nil {
		t.Fatalf("tool error text is not JSON: %v: %s", err, text)
	}
	return p
}

func jsonEqual(a, b any) bool {
	x, err1 := json.Marshal(a)
	y, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

const mcpProblem = "https://github.com/angelia-dev/spec/blob/main/docs/mcp.md#problem-"
