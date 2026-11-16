package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/angelia-dev/angelia/internal/render"
)

// POST /v1/template-previews (story #274, spec docs/api.md decision 53):
// render a message template against one event, with the engine deliveries
// use, and store or send nothing. The engine has no network, file,
// environment or clock access, and the template sees the event only, so no
// secret reference can be resolved. The run is bounded more strictly than a
// delivery.

// Preview limits: a sample check's time (render.CheckTimeout, 250 ms) and
// the delivery output limit (256 KiB).
var previewLimits = render.Limits{Timeout: render.CheckTimeout, MaxOutputBytes: render.DefaultMaxOutputBytes}

type templatePreviewJSON struct {
	Output      *string            `json:"output"`
	ContentType *string            `json:"content_type"`
	Error       *templateErrorJSON `json:"error"`
}

type templateErrorJSON struct {
	Kind   string `json:"kind"`
	Line   *int   `json:"line"`
	Column *int   `json:"column"`
	Reason string `json:"reason"`
}

// templateErrorKinds names the render error kinds in the TemplateError
// schema.
var templateErrorKinds = []struct {
	err  error
	kind string
}{
	{render.ErrParse, "parse"},
	{render.ErrNotAllowed, "not_allowed"},
	{render.ErrExec, "exec"},
	{render.ErrTimeout, "timeout"},
	{render.ErrOutputTooLarge, "output_too_large"},
	{render.ErrInvalidEvent, "invalid_event"},
}

// templateError describes a render error. render.Error never quotes the
// template text; any other error (none is expected) gets a fixed reason.
func templateError(err error) *templateErrorJSON {
	out := &templateErrorJSON{Kind: "exec", Reason: "template execution failed"}
	for _, k := range templateErrorKinds {
		if errors.Is(err, k.err) {
			out.Kind = k.kind
			break
		}
	}
	var re *render.Error
	if errors.As(err, &re) {
		out.Reason = truncateUTF8(re.Reason, 1024)
		if re.Line > 0 {
			l := re.Line
			out.Line = &l
		}
		if re.Col > 0 {
			c := re.Col
			out.Column = &c
		}
	} else if errors.Is(err, render.ErrInvalidEvent) {
		out.Reason = "the event is not a JSON object"
	}
	return out
}

// previewEvent returns the envelope a preview renders: the request's
// event, an event of the log, or a built-in sample (full by default).
func (s *Server) previewEvent(ctx context.Context, req *TemplatePreviewRequest, raw []byte) ([]byte, error) {
	switch {
	case req.Event != nil:
		var body struct {
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, problem(SlugBadRequest, "the request body is not valid JSON")
		}
		return body.Event, nil
	case req.EventId != nil:
		e, err := s.opts.Store.GetEvent(ctx, *req.EventId)
		if err != nil {
			return nil, storeProblem(err, "event", *req.EventId)
		}
		return e.Payload, nil
	}
	name := "full"
	if req.Sample != nil {
		name = string(*req.Sample)
	}
	for _, smp := range render.Samples() {
		if smp.Name == name {
			b, err := json.Marshal(smp.Envelope)
			if err != nil {
				return nil, err //nolint:wrapcheck // a marshalling error of a built-in value is a bug
			}
			return b, nil
		}
	}
	return nil, problem(SlugBadRequest, "no sample event %q", name)
}

// PreviewTemplate implements StrictServerInterface.
func (s *Server) PreviewTemplate(ctx context.Context, req PreviewTemplateRequestObject) (PreviewTemplateResponseObject, error) {
	if req.Body == nil {
		return nil, problem(SlugBadRequest, "the request body is required")
	}
	set := 0
	for _, b := range []bool{req.Body.Event != nil, req.Body.EventId != nil, req.Body.Sample != nil} {
		if b {
			set++
		}
	}
	if set > 1 {
		// The schema's not already refuses it; kept for any other caller.
		return nil, validationProblem("template preview", []fieldError{{Pointer: "", Detail: "must not have more than one of event, event_id and sample"}})
	}
	envelope, err := s.previewEvent(ctx, req.Body, stateFrom(ctx).body)
	if err != nil {
		return nil, err
	}
	out := templatePreviewJSON{}
	t, err := render.Compile(req.Body.Template, previewLimits)
	if err == nil {
		var r render.Rendered
		if r, err = t.Render(ctx, envelope); err == nil {
			body := strings.ToValidUTF8(string(r.Body), "�")
			out.Output, out.ContentType = &body, &r.ContentType
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err() //nolint:wrapcheck // the client went away
		}
		out.Error = templateError(err)
	}
	return jsonOK(out), nil
}

func (r response) VisitPreviewTemplateResponse(w http.ResponseWriter) error { return r.write(w) }
