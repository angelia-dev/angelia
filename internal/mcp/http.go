package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/angelia-dev/angelia/internal/apiclient"
)

// inProcessBase is the base URL of the REST calls the HTTP transport makes in
// memory. The in-process transport ignores the host; it only names the
// calls.
const inProcessBase = "http://angelia.in-process"

// maxBodyBytes is the REST API's request body limit (api.MaxBodyBytes). The
// api role enforces it before the request reaches this handler; the SDK's
// own limit is set to the same value as a second guard.
const maxBodyBytes = 1 << 20

// HTTPOptions configure NewHTTPHandler.
type HTTPOptions struct {
	// REST carries the tools' REST calls to the API, in memory
	// (api.Options.MCP hands it over). Each call goes through the REST
	// pipeline: authentication, scopes, validation and the access log.
	REST http.RoundTripper
	// Version is the server version sent in initialize.
	Version string
	// Logger receives the SDK's warnings and errors. Nil discards them.
	Logger *slog.Logger
}

// NewHTTPHandler returns the Streamable HTTP handler of POST /mcp (ADR 0029,
// D9 and D10): stateless, JSON responses, the SDK's localhost DNS-rebinding
// protection on. The api role authenticates the request, refuses a
// cross-origin Origin and bounds the body before it gets here.
//
// Every request gets its own MCP server, whose REST calls carry exactly the
// request's Authorization header: nothing is cached across requests, so a
// revoked token fails on its next call.
func NewHTTPHandler(o HTTPOptions) (http.Handler, error) {
	if o.REST == nil {
		return nil, errors.New("mcp: no REST transport")
	}
	ts, err := newToolset()
	if err != nil {
		return nil, err
	}
	log := quiet(o.Logger)
	getServer := func(r *http.Request) *sdk.Server {
		hc := &http.Client{
			Transport: forwardAuth{rt: o.REST, auth: r.Header.Values("Authorization")},
			Timeout:   apiclient.DefaultTimeout,
		}
		a, err := apiclient.New(apiclient.Options{Server: inProcessBase, HTTPClient: hc, UserAgent: "angelia-mcp"})
		if err != nil {
			return nil // a constant URL: cannot happen; the SDK answers 400
		}
		return ts.server(a, o.Version, log)
	}
	return sdk.NewStreamableHTTPHandler(getServer, &sdk.StreamableHTTPOptions{
		Stateless:           true,
		JSONResponse:        true,
		Logger:              log,
		MaxRequestBodyBytes: maxBodyBytes,
	}), nil
}

// forwardAuth sends each REST call with exactly the MCP request's
// Authorization header values.
type forwardAuth struct {
	rt   http.RoundTripper
	auth []string
}

func (f forwardAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Del("Authorization")
	for _, v := range f.auth {
		r.Header.Add("Authorization", v)
	}
	return f.rt.RoundTrip(r) //nolint:wrapcheck // a transport passes errors through
}

// quiet keeps the SDK's warnings and errors and drops its per-request info
// records (session connected, initialized, ended), which over stateless HTTP
// would repeat the access log once per request.
func quiet(l *slog.Logger) *slog.Logger {
	if l == nil {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(minLevel{Handler: l.Handler(), min: slog.LevelWarn})
}

type minLevel struct {
	slog.Handler
	min slog.Level
}

func (h minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h minLevel) WithAttrs(as []slog.Attr) slog.Handler {
	return minLevel{Handler: h.Handler.WithAttrs(as), min: h.min}
}

func (h minLevel) WithGroup(name string) slog.Handler {
	return minLevel{Handler: h.Handler.WithGroup(name), min: h.min}
}
