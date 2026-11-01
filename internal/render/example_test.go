package render_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/angelia-dev/angelia/internal/render"
)

func fullSample() []byte {
	b, err := json.Marshal(render.Samples()[0].Envelope)
	if err != nil {
		panic(err)
	}
	return b
}

// A chat message.
func ExampleTemplate_Render() {
	t, err := render.Compile(`{{.data.package}} {{.data.version}} is out{{with .data.previous_version}} (was {{.}}){{end}}
{{with .data.urls.release}}{{.}}{{end}}`, render.Limits{})
	if err != nil {
		panic(err)
	}
	if err := t.Check(); err != nil {
		panic(err)
	}
	r, err := t.Render(context.Background(), fullSample())
	if err != nil {
		panic(err)
	}
	fmt.Println(r.ContentType)
	fmt.Println(string(r.Body))
	// Output:
	// text/plain; charset=utf-8
	// requests 2.33.0 is out (was 2.32.5)
	// https://pypi.org/project/requests/2.33.0/
}

// A JSON body: toJSON quotes and escapes every value.
func ExampleTemplate_Render_json() {
	t, err := render.Compile(`{"text": {{printf "%s %s released" .data.package .data.version | toJSON}}, "team": {{toJSON .data.watch.labels.team}}}`, render.Limits{})
	if err != nil {
		panic(err)
	}
	r, err := t.Render(context.Background(), fullSample())
	if err != nil {
		panic(err)
	}
	fmt.Println(r.ContentType)
	fmt.Println(string(r.Body))
	// Output:
	// application/json; charset=utf-8
	// {"text": "requests 2.33.0 released", "team": "platform"}
}

// Without a template the body is the CloudEvents JSON event.
func ExampleRender_default() {
	r, err := render.Render(context.Background(), nil, fullSample())
	if err != nil {
		panic(err)
	}
	fmt.Println(r.ContentType, r.Templated)
	// Output:
	// application/cloudevents+json; charset=utf-8 false
}
