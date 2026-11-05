package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// maxResponseBytes bounds what a non-stream response may hold: a page is at
// most 1000 events, a few MiB.
const maxResponseBytes = 64 << 20

// DefaultTimeout is the time one non-stream request may take.
const DefaultTimeout = 30 * time.Second

// Options configure New.
type Options struct {
	// Server is the base URL of the instance, e.g. http://localhost:8080.
	Server string
	// Token is the API token, sent as "Authorization: Bearer <token>". Empty
	// sends no Authorization header.
	Token string
	// HTTPClient performs the requests, including event streams. Nil uses a
	// client with DefaultTimeout for requests and none for streams.
	HTTPClient *http.Client
	// UserAgent is sent with every request. Empty means "angelia".
	UserAgent string
}

// API is the API client: the generated Client plus bearer authentication,
// problem details errors and the event stream reader. Methods of the
// embedded generated client return the raw *http.Response; use Decode (or the
// typed helpers of this package) to read one.
type API struct {
	*Client
	stream *Client
}

// New builds an API client.
func New(o Options) (*API, error) {
	if o.Server == "" {
		return nil, errors.New("apiclient: no server URL")
	}
	ua := o.UserAgent
	if ua == "" {
		ua = "angelia"
	}
	edit := func(_ context.Context, req *http.Request) error {
		req.Header.Set("User-Agent", ua)
		if o.Token != "" {
			req.Header.Set("Authorization", "Bearer "+o.Token)
		}
		return nil
	}
	reqHC, streamHC := o.HTTPClient, o.HTTPClient
	if o.HTTPClient == nil {
		reqHC = &http.Client{Timeout: DefaultTimeout}
		streamHC = &http.Client{}
	}
	api, err := NewClient(o.Server, WithHTTPClient(reqHC), WithRequestEditorFn(edit))
	if err != nil {
		return nil, fmt.Errorf("apiclient: %w", err)
	}
	stream, err := NewClient(o.Server, WithHTTPClient(streamHC), WithRequestEditorFn(edit))
	if err != nil {
		return nil, fmt.Errorf("apiclient: %w", err)
	}
	return &API{Client: api, stream: stream}, nil
}

// Error is an API error: a non-2xx response, with the RFC 9457 problem
// details when the server sent them.
type Error struct {
	// Status is the HTTP status code.
	Status int
	// Problem is the parsed problem document; nil when the body was not one.
	Problem *Problem
	// Body is the start of the response body when Problem is nil.
	Body string
}

func (e *Error) Error() string {
	if e.Problem == nil {
		if e.Body != "" {
			return fmt.Sprintf("server returned %d: %s", e.Status, e.Body)
		}
		return fmt.Sprintf("server returned %d %s", e.Status, http.StatusText(e.Status))
	}
	p := e.Problem
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s", e.Status, p.Title)
	if t := problemSlug(p.Type); t != "" {
		fmt.Fprintf(&b, " (%s)", t)
	}
	if p.Detail != nil && *p.Detail != "" {
		fmt.Fprintf(&b, ": %s", *p.Detail)
	}
	if p.Errors != nil {
		for _, pe := range *p.Errors {
			b.WriteString("\n  - ")
			b.WriteString(describeProblemError(pe))
		}
	}
	return b.String()
}

// Type is the problem type slug ("validation-failed", "managed-by-file", ...),
// or "" when the server sent none. Switch on it, never on the title.
func (e *Error) Type() string {
	if e.Problem == nil {
		return ""
	}
	return problemSlug(e.Problem.Type)
}

// problemSlug is the fragment of the problem type URI, which names the type.
func problemSlug(uri string) string {
	if _, frag, ok := strings.Cut(uri, "#problem-"); ok {
		return frag
	}
	return ""
}

func describeProblemError(pe ProblemError) string {
	if pe.Pointer == "" {
		return pe.Detail
	}
	return pe.Pointer + ": " + pe.Detail
}

// Result carries what a decoded response holds besides its body.
type Result struct {
	// Change is the Angelia-Change header of a write: created, updated,
	// unchanged or deleted. Empty on reads.
	Change string
}

// Decode reads resp, closes its body, and returns an *Error for a non-2xx
// status. On success it decodes the JSON body into out (nil ignores the body).
// Pass the (resp, err) pair of a generated method straight in.
func Decode(resp *http.Response, err error, out any) (Result, error) {
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Result{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, newError(resp.StatusCode, body)
	}
	res := Result{Change: resp.Header.Get("Angelia-Change")}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return res, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return res, fmt.Errorf("decode response: %w", err)
	}
	return res, nil
}

func newError(status int, body []byte) *Error {
	e := &Error{Status: status}
	var p Problem
	if json.Unmarshal(body, &p) == nil && p.Type != "" {
		e.Problem = &p
		return e
	}
	s := strings.TrimSpace(string(bytes.ToValidUTF8(body, nil)))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	e.Body = s
	return e
}
