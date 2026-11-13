package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The problem types of spec docs/mcp.md "Problem types": failures with no
// REST problem.
const (
	problemTypeBase     = "https://github.com/angelia-dev/spec/blob/main/docs/mcp.md#problem-"
	slugInvalidArgs     = "invalid-arguments"
	slugNoVersion       = "no-version"
	slugWatchExists     = "watch-exists"
	slugAPIUnreachable  = "api-unreachable"
	maxResponseBytes    = 16 << 20
	restProblemNotFound = "not-found"
)

var problemKinds = map[string]struct {
	status int
	title  string
}{
	slugInvalidArgs:    {http.StatusBadRequest, "Invalid arguments"},
	slugNoVersion:      {http.StatusNotFound, "No version"},
	slugWatchExists:    {http.StatusConflict, "Watch exists"},
	slugAPIUnreachable: {http.StatusBadGateway, "API unreachable"},
}

// toolError is a failed tool call: an RFC 9457 problem as JSON, either the
// REST API's problem byte for byte or one of the types above.
type toolError struct {
	body   []byte
	status int
	slug   string // the fragment of the type after "#problem-"
}

type problemDoc struct {
	Type   string       `json:"type"`
	Title  string       `json:"title"`
	Status int          `json:"status"`
	Detail string       `json:"detail,omitempty"`
	Errors []fieldError `json:"errors,omitempty"`
}

func newProblem(slug, format string, a ...any) *toolError {
	return buildProblem(slug, fmt.Sprintf(format, a...), nil)
}

func invalidArguments(errs ...fieldError) *toolError {
	n := "s"
	if len(errs) == 1 {
		n = ""
	}
	return buildProblem(slugInvalidArgs, fmt.Sprintf("the arguments have %d error%s", len(errs), n), errs)
}

func buildProblem(slug, detail string, errs []fieldError) *toolError {
	k := problemKinds[slug]
	// A struct of strings and ints always encodes.
	b, _ := json.Marshal(problemDoc{Type: problemTypeBase + slug, Title: k.title, Status: k.status, Detail: detail, Errors: errs})
	return &toolError{body: b, status: k.status, slug: slug}
}

// call reads the response of a REST call. On a 2xx status it decodes the
// body into out (nil skips it) and returns the Angelia-Change header. A
// problem response becomes the tool error as the API sent it; anything else
// (a failed connection, a body the API does not send) is api-unreachable,
// whose detail never holds the token: the client sends it only as a header.
func call(resp *http.Response, err error, out any) (string, *toolError) {
	if err != nil {
		return "", newProblem(slugAPIUnreachable, "the request to the API failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", newProblem(slugAPIUnreachable, "reading the API response failed: %v", err)
	}
	if len(body) > maxResponseBytes {
		return "", newProblem(slugAPIUnreachable, "the API response is larger than %d bytes", maxResponseBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var p problemDoc
		if json.Unmarshal(body, &p) != nil || p.Type == "" || p.Title == "" {
			return "", newProblem(slugAPIUnreachable, "the API answered %d without a problem document", resp.StatusCode)
		}
		_, slug, _ := strings.Cut(p.Type, "#problem-")
		return "", &toolError{body: bytes.TrimSpace(body), status: resp.StatusCode, slug: slug}
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return "", newProblem(slugAPIUnreachable, "the API answered %d with a body that is not the expected JSON", resp.StatusCode)
		}
	}
	return resp.Header.Get("Angelia-Change"), nil
}
