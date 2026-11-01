package wasm

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
)

// Wire types of the draft plugin ABI v0, shared with the Rust crate
// bench/plugins/abi. JSON over Extism memory; bodies are base64.

type wireState struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
	Cursor       string `json:"cursor,omitempty"`
}

type wireListInput struct {
	Target source.Target `json:"target"`
	State  wireState     `json:"state"`
}

type wireVersion struct {
	Raw         string         `json:"raw"`
	PublishedAt string         `json:"published_at,omitempty"`
	Prerelease  *bool          `json:"prerelease,omitempty"`
	Yanked      bool           `json:"yanked"`
	URL         string         `json:"url,omitempty"`
	NotesURL    string         `json:"notes_url,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type wireListOutput struct {
	Versions    []wireVersion `json:"versions"`
	State       wireState     `json:"state"`
	NotModified bool          `json:"not_modified"`
}

type wireValidateOutput struct {
	Reason string `json:"reason,omitempty"`
}

type wireValidators struct {
	ETag         string `json:"etag,omitempty"`
	LastModified string `json:"last_modified,omitempty"`
}

type wireHTTPRequest struct {
	Method     string            `json:"method"`
	URL        string            `json:"url"`
	Headers    map[string]string `json:"headers"`
	Body       []byte            `json:"body"` // base64 in JSON
	Validators *wireValidators   `json:"validators"`
}

type wireHTTPError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type wireHTTPResponse struct {
	Status      int                 `json:"status"`
	Headers     map[string][]string `json:"headers,omitempty"`
	Body        []byte              `json:"body,omitempty"` // base64 in JSON
	Validators  wireValidators      `json:"validators"`
	NotModified bool                `json:"not_modified"`
	Error       *wireHTTPError      `json:"error,omitempty"`
}

type wireLogEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// Error codes of wireHTTPError.
const (
	codeHostNotAllowed  = "host_not_allowed"  // not in allowed_hosts, first hop or redirect
	codeBlocked         = "blocked"           // refused by the netguard SSRF guard
	codeNotAllowedHere  = "not_allowed_here"  // network access during validate
	codeTooManyRequests = "too_many_requests" // per-call request budget exhausted
	codeBadRequest      = "bad_request"       // malformed request from the plugin
	codeTooLarge        = "too_large"         // response body over the cap
	codeCanceled        = "canceled"          // the call's context ended
	codeTransport       = "transport"         // any other error before a response
)

// wireTarget returns t with non-nil coordinates, so the plugin always gets
// a JSON object.
func wireTarget(t source.Target) source.Target {
	if t.Coordinates == nil {
		t.Coordinates = map[string]string{}
	}
	return t
}

// errPluginOutput is wrapped when a plugin returns output the host rejects.
var errPluginOutput = errors.New("wasm: invalid plugin output")

func toResult(out []byte, in source.FetchState) (source.Result, error) {
	var w wireListOutput
	if err := json.Unmarshal(out, &w); err != nil {
		return source.Result{}, fmt.Errorf("%w: %w", errPluginOutput, err)
	}
	res := source.Result{
		State:       source.FetchState{ETag: w.State.ETag, LastModified: w.State.LastModified, Cursor: w.State.Cursor},
		NotModified: w.NotModified,
	}
	if w.NotModified {
		if len(w.Versions) != 0 {
			return source.Result{}, fmt.Errorf("%w: not_modified with %d versions", errPluginOutput, len(w.Versions))
		}
		if res.State.IsZero() {
			res.State = in // keep the incoming state, as built-in sources do
		}
		return res, nil
	}
	if len(w.Versions) > 0 {
		res.Versions = make([]source.Version, len(w.Versions))
	}
	for i, v := range w.Versions {
		if v.Raw == "" {
			return source.Result{}, fmt.Errorf("%w: version %d has an empty raw", errPluginOutput, i)
		}
		var pub time.Time
		if v.PublishedAt != "" {
			t, err := time.Parse(time.RFC3339Nano, v.PublishedAt)
			if err != nil {
				return source.Result{}, fmt.Errorf("%w: version %q: published_at: %w", errPluginOutput, v.Raw, err)
			}
			pub = t.UTC()
		}
		res.Versions[i] = source.Version{
			Raw:         v.Raw,
			PublishedAt: pub,
			Prerelease:  v.Prerelease,
			Yanked:      v.Yanked,
			URL:         v.URL,
			NotesURL:    v.NotesURL,
			Metadata:    v.Metadata,
		}
	}
	return res, nil
}
