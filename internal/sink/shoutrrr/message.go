package shoutrrr

import (
	"context"
	"errors"
	"fmt"

	"github.com/angelia-dev/angelia/internal/event"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/sink"
)

// DefaultTemplates returns the plain-text messages sent for a subscription
// without a template, per event type (ADR 0026 §6, #120 criterion 2),
// rendered by the template engine (ADR 0016). docs/templates.md shows them.
func DefaultTemplates() map[string]string {
	return map[string]string{
		event.TypePublished: `{{.data.package}} {{.data.version}} released` +
			`{{with .data.bump}} ({{.}}){{end}}{{with .data.previous_version}}, previous {{.}}{{end}}` +
			`{{with .data.urls.release}}` + "\n" + `{{.}}{{end}}`,
		event.TypePrerelease: `{{.data.package}} {{.data.version}} pre-release published` +
			`{{with .data.previous_version}}, previous {{.}}{{end}}` +
			`{{with .data.urls.release}}` + "\n" + `{{.}}{{end}}`,
		event.TypeYanked: `{{.data.package}} {{.data.version}} was yanked upstream; do not upgrade to it` +
			`{{with .data.urls.release}}` + "\n" + `{{.}}{{end}}`,
	}
}

// fallbackTemplate is used for an event type that has no default (one a
// later spec version adds).
const fallbackTemplate = `{{.data.package}} {{.data.version}}: {{.type}}`

// message is the text to send: the subscription's rendered template, or the
// default for the event's type.
func message(ctx context.Context, req sink.Request) (string, error) {
	if req.Rendered.Templated {
		if len(req.Rendered.Body) == 0 {
			return "", errors.New("the template rendered an empty message")
		}
		return string(req.Rendered.Body), nil
	}
	src, ok := DefaultTemplates()[req.Event.Envelope.Type]
	if !ok {
		src = fallbackTemplate
	}
	t, err := render.Compile(src, render.Limits{})
	if err != nil {
		return "", fmt.Errorf("default message: %w", err)
	}
	r, err := t.Render(ctx, req.Event.JSON)
	if err != nil {
		return "", fmt.Errorf("default message: %w", err)
	}
	return string(r.Body), nil
}
