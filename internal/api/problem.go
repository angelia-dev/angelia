package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// ProblemTypeBase is the prefix of every problem type URI (spec docs/api.md
// decision 18); the slug follows it.
const ProblemTypeBase = "https://github.com/angelia-dev/spec/blob/main/docs/api.md#problem-"

// Problem slugs (spec docs/api.md, "Problem types").
const (
	SlugBadRequest           = "bad-request"
	SlugUnauthorized         = "unauthorized"
	SlugForbidden            = "forbidden"
	SlugNotFound             = "not-found"
	SlugManagedByFile        = "managed-by-file"
	SlugCrossOwnerReference  = "cross-owner-reference"
	SlugSinkInUse            = "sink-in-use"
	SlugSecretInUse          = "secret-in-use"
	SlugSecretsKeyMissing    = "secrets-key-missing"
	SlugSecretHostBinding    = "secret-host-binding-required"
	SlugNotReplayable        = "not-replayable"
	SlugCursorExpired        = "cursor-expired"
	SlugPayloadTooLarge      = "payload-too-large"
	SlugUnsupportedMediaType = "unsupported-media-type"
	SlugEnvNotAllowed        = "env-not-allowed"
	SlugValidationFailed     = "validation-failed"
	SlugInternalError        = "internal-error"
	SlugUnavailable          = "unavailable"
)

// problemKinds gives each slug its status and fixed title.
var problemKinds = map[string]struct {
	status int
	title  string
}{
	SlugBadRequest:           {http.StatusBadRequest, "Bad request"},
	SlugUnauthorized:         {http.StatusUnauthorized, "Unauthorized"},
	SlugForbidden:            {http.StatusForbidden, "Forbidden"},
	SlugNotFound:             {http.StatusNotFound, "Not found"},
	SlugManagedByFile:        {http.StatusConflict, "Managed by the watch file"},
	SlugCrossOwnerReference:  {http.StatusConflict, "Cross-owner reference"},
	SlugSinkInUse:            {http.StatusConflict, "Sink in use"},
	SlugSecretInUse:          {http.StatusConflict, "Secret in use"},
	SlugSecretsKeyMissing:    {http.StatusUnprocessableEntity, "Secrets key missing"},
	SlugSecretHostBinding:    {http.StatusUnprocessableEntity, "Secret host binding required"},
	SlugNotReplayable:        {http.StatusConflict, "Not replayable"},
	SlugCursorExpired:        {http.StatusGone, "Cursor expired"},
	SlugPayloadTooLarge:      {http.StatusRequestEntityTooLarge, "Payload too large"},
	SlugUnsupportedMediaType: {http.StatusUnsupportedMediaType, "Unsupported media type"},
	SlugEnvNotAllowed:        {http.StatusUnprocessableEntity, "Environment variable not allowed"},
	SlugValidationFailed:     {http.StatusUnprocessableEntity, "Validation failed"},
	SlugInternalError:        {http.StatusInternalServerError, "Internal error"},
	SlugUnavailable:          {http.StatusServiceUnavailable, "Unavailable"},
}

// ProblemDetail is an RFC 9457 problem detail. It is also the error the handlers
// return: the strict server's error handler writes it as the response.
// Detail must never hold a secret or a full sink URL.
type ProblemDetail struct {
	Type          string               `json:"type"`
	Title         string               `json:"title"`
	Status        int                  `json:"status"`
	Detail        string               `json:"detail,omitempty"`
	Instance      string               `json:"instance,omitempty"`
	Errors        []ProblemDetailError `json:"errors,omitempty"`
	RequiredScope string               `json:"required_scope,omitempty"`
	OldestSeq     *int64               `json:"oldest_seq,omitempty"`

	header http.Header
}

// ProblemDetailError is one entry of Problem.Errors.
type ProblemDetailError struct {
	Pointer string `json:"pointer"`
	Detail  string `json:"detail"`
}

// problem returns the problem of a slug with a formatted detail.
func problem(slug, format string, a ...any) *ProblemDetail {
	k, ok := problemKinds[slug]
	if !ok {
		k = problemKinds[SlugInternalError]
		slug = SlugInternalError
	}
	return &ProblemDetail{Type: ProblemTypeBase + slug, Title: k.title, Status: k.status, Detail: fmt.Sprintf(format, a...)}
}

// validationProblem returns a 422 listing every error.
func validationProblem(what string, errs []fieldError) *ProblemDetail {
	p := problem(SlugValidationFailed, "the %s has %d error%s", what, len(errs), plural(len(errs)))
	for _, e := range errs {
		p.Errors = append(p.Errors, ProblemDetailError(e))
	}
	return p
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func (p *ProblemDetail) Error() string { return p.Title + ": " + p.Detail }

// withHeader sets a response header of the problem.
func (p *ProblemDetail) withHeader(k, v string) *ProblemDetail {
	if p.header == nil {
		p.header = http.Header{}
	}
	p.header.Set(k, v)
	return p
}

// write sends the problem; instance defaults to the request path.
func (p *ProblemDetail) write(w http.ResponseWriter, r *http.Request) {
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(p) // a Problem always encodes
	h := w.Header()
	for k, v := range p.header {
		h[k] = v
	}
	h.Set("Content-Type", "application/problem+json")
	h.Set("Content-Length", strconv.Itoa(b.Len()))
	w.WriteHeader(p.Status)
	_, _ = w.Write(b.Bytes())
}
