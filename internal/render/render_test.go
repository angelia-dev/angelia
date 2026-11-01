package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleJSON(t *testing.T, name string) []byte {
	t.Helper()
	for _, s := range Samples() {
		if s.Name == name {
			b, err := json.Marshal(s.Envelope)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
	}
	t.Fatalf("no sample %q", name)
	return nil
}

func mustCompile(t *testing.T, src string, l Limits) *Template {
	t.Helper()
	tm, err := Compile(src, l)
	if err != nil {
		t.Fatalf("Compile(%q): %v", src, err)
	}
	return tm
}

func render(t *testing.T, src string, l Limits, sample string) (Rendered, error) {
	t.Helper()
	return mustCompile(t, src, l).Render(context.Background(), sampleJSON(t, sample))
}

// AC: default = CloudEvents JSON when no template is set.
func TestRenderDefaultIsCloudEventsJSON(t *testing.T) {
	env := sampleJSON(t, "full")
	r, err := Render(context.Background(), nil, env)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.Body, env) {
		t.Errorf("body = %s, want the envelope unchanged", r.Body)
	}
	if r.ContentType != ContentTypeCloudEvents || r.Templated {
		t.Errorf("content type %q templated %v", r.ContentType, r.Templated)
	}
	env[0] = 'X' // the body is a copy
	if r.Body[0] != '{' {
		t.Error("body aliases the input")
	}
	for _, bad := range []string{"", "null", "[]", "{", `"x"`} {
		if _, err := Render(context.Background(), nil, []byte(bad)); !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("Render(nil, %q) = %v, want ErrInvalidEvent", bad, err)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	tests := []struct {
		name, src, sample, want, ct string
	}{
		{
			"spec field names", `{{.data.source}}:{{.data.package}}@{{.data.version}} ({{.type}})`, "full",
			"pypi:requests@2.33.0 (dev.angelia.release.published.v1)", ContentTypeText,
		},
		{"labels", `{{.data.watch.labels.team}}`, "full", "platform", ContentTypeText},
		{"range over labels", `{{range $k, $v := .data.watch.labels}}{{$k}}={{$v}};{{end}}`, "full", "repo=api;team=platform;", ContentTypeText},
		{"with guards null", `{{with .data.previous_version}}from {{.}}{{else}}first{{end}}`, "sparse", "first", ContentTypeText},
		{"json output", `{"text": {{printf "%s %s" .data.package .data.version | toJSON}}}`, "full", `{"text": "requests 2.33.0"}`, ContentTypeJSON},
		{"null printed", `{{.data.bump}}`, "sparse", "<no value>", ContentTypeText},
		{"if on bool", `{{if .data.prerelease}}pre{{else}}stable{{end}}`, "full", "stable", ContentTypeText},
		{"break in range", `{{range split "," "a,b,c"}}{{if eq . "b"}}{{break}}{{end}}{{.}}{{end}}`, "full", "a", ContentTypeText},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := render(t, tt.src, Limits{}, tt.sample)
			if err != nil {
				t.Fatal(err)
			}
			if string(r.Body) != tt.want || r.ContentType != tt.ct || !r.Templated {
				t.Errorf("got %q %q %v, want %q %q", r.Body, r.ContentType, r.Templated, tt.want, tt.ct)
			}
		})
	}
}

func TestRenderErrors(t *testing.T) {
	tests := []struct {
		name, src, sample string
		kind              error
		line, col         int
		reason            string
	}{
		{"missing key", "ok\n{{.data.nosuch}}", "full", ErrExec, 2, 7, `map has no entry for key "nosuch"`},
		{"nil field", `{{.data.notes.body}}`, "sparse", ErrExec, 1, 7, "nil pointer evaluating interface {}.body"},
		{"function error", `{{upper .data.prerelease}}`, "full", ErrExec, 1, 2, "error calling upper: argument: want a string, got bool"},
		{"call disabled", `{{call .data.package}}`, "full", ErrExec, 1, 2, "error calling call: call is not available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := render(t, tt.src, Limits{}, tt.sample)
			var e *Error
			if !errors.As(err, &e) || !errors.Is(err, tt.kind) {
				t.Fatalf("err = %v, want *Error of kind %v", err, tt.kind)
			}
			if e.Line != tt.line || e.Col != tt.col || e.Reason != tt.reason {
				t.Errorf("got line %d col %d reason %q, want %d %d %q", e.Line, e.Col, e.Reason, tt.line, tt.col, tt.reason)
			}
		})
	}
	if _, err := mustCompile(t, "x", Limits{}).Render(context.Background(), []byte("[1]")); !errors.Is(err, ErrInvalidEvent) {
		t.Errorf("non-object event: %v", err)
	}
}

func TestCompileRejects(t *testing.T) {
	big := strings.Repeat("x", 101)
	tests := []struct {
		name, src string
		kind      error
		msg       string
	}{
		{"empty", "", ErrNotAllowed, "template is empty"},
		{"too large", big, ErrNotAllowed, "template is 101 bytes, at most 100"},
		{"parse error", "a\n{{ .Foo ", ErrParse, "line 2: unclosed action"},
		{"unknown function env", `{{env "HOME"}}`, ErrParse, `line 1: function "env" not defined`},
		{"unknown function readFile", `{{readFile "/etc/passwd"}}`, ErrParse, `line 1: function "readFile" not defined`},
		{"unknown function now", `{{now}}`, ErrParse, `line 1: function "now" not defined`},
		{"define", `{{define "a"}}x{{end}}y`, ErrNotAllowed, "define and block are not allowed"},
		{"block", `{{block "a" .}}x{{end}}`, ErrNotAllowed, "define and block are not allowed"},
		{"template", "x\n  {{if true}}{{template \"a\"}}{{end}}", ErrNotAllowed, "line 2, column 24: the template action is not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile(tt.src, Limits{MaxTemplateBytes: 100})
			if !errors.Is(err, tt.kind) || err.Error() != tt.msg {
				t.Errorf("err = %v, want %q (%v)", err, tt.msg, tt.kind)
			}
		})
	}
}

// Error messages give a position and a reason but never the template text.
func TestErrorsDoNotQuoteTemplate(t *testing.T) {
	const marker = "SECRETMARKER"
	srcs := []string{
		`{{"` + marker + `" | .data.nosuch}}`,
		`{{.data.notes.body}}{{"` + marker + `"}}`,
		`{{"` + marker + `"}}{{upper .data.backport}}`,
	}
	for _, src := range srcs {
		tm, err := Compile(src, Limits{})
		if err == nil {
			err = tm.Check()
		}
		if err == nil {
			t.Fatalf("%q: no error", src)
		}
		if strings.Contains(err.Error(), marker) {
			t.Errorf("%q: error quotes the template: %v", src, err)
		}
	}
	_, err := Compile(`{{ "`+marker+`" `, Limits{})
	if err == nil || strings.Contains(err.Error(), marker) {
		t.Errorf("parse error quotes the template: %v", err)
	}
}

// AC: execution has a timeout.
func TestRenderTimeout(t *testing.T) {
	srcs := map[string]string{
		"range over int, empty body": `{{range 2000000000}}{{end}}`,
		"nested ranges over data":    `{{range split "" .data.package}}{{range split "" $.data.package}}{{range 100000000}}{{end}}{{end}}{{end}}`,
		"range in else":              `{{if false}}{{else}}{{range 2000000000}}{{continue}}{{end}}{{end}}`,
		"range in with":              `{{with .data}}{{range 2000000000}}{{end}}{{end}}`,
	}
	for name, src := range srcs {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			_, err := render(t, src, Limits{Timeout: 50 * time.Millisecond}, "full")
			if !errors.Is(err, ErrTimeout) {
				t.Fatalf("err = %v, want ErrTimeout", err)
			}
			if el := time.Since(start); el > 2*time.Second {
				t.Errorf("took %s", el)
			}
			if err.Error() != "execution took longer than 50ms" {
				t.Errorf("message %q", err)
			}
		})
	}
}

func TestRenderContextEnds(t *testing.T) {
	tm := mustCompile(t, `{{range 2000000000}}{{end}}`, Limits{Timeout: time.Hour})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := tm.Render(ctx, sampleJSON(t, "full")); !errors.Is(err, ErrTimeout) {
		t.Errorf("deadline: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err := tm.Render(ctx, sampleJSON(t, "full")); !errors.Is(err, ErrTimeout) {
		t.Errorf("canceled: %v", err)
	}
	// Without a range the writer checks the context.
	if _, err := mustCompile(t, "text", Limits{}).Render(ctx, sampleJSON(t, "full")); !errors.Is(err, ErrTimeout) {
		t.Errorf("canceled, no range: %v", err)
	}
}

// AC: execution has an output size cap.
func TestRenderOutputCap(t *testing.T) {
	l := Limits{MaxOutputBytes: 1000}
	tests := map[string]string{
		"output":          `{{range 2000}}x{{end}}`,
		"doubling printf": `{{$x := "ab"}}{{range 40}}{{$x = printf "%s%s" $x $x}}{{end}}`,
		"doubling print":  `{{$x := "ab"}}{{range 40}}{{$x = print $x $x}}{{end}}`,
		"doubling js":     `{{$x := "\\"}}{{range 40}}{{$x = js $x}}{{end}}`,
		"replace growth":  `{{$x := "a"}}{{range 40}}{{$x = replace "a" "aa" $x}}{{end}}`,
		"join growth":     `{{$x := "ab"}}{{range 40}}{{$x = join $x (split "" $x)}}{{end}}`,
		"printf width":    `{{printf "%999999d" 1 | len}}`,
		"printf repeats":  `{{$x := printf "%600s" ""}}{{printf "%[1]s%[1]s" $x | len}}`,
		"many args":       `{{$x := printf "%600s" ""}}{{print $x $x | len}}`,
		"upper expands":   `{{$x := printf "%500s" "" | replace " " "ɐ"}}{{len $x}} {{upper $x | len}}`,
	}
	for name, src := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := render(t, src, l, "full")
			if !errors.Is(err, ErrOutputTooLarge) {
				t.Fatalf("err = %v, want ErrOutputTooLarge", err)
			}
			if !strings.HasSuffix(err.Error(), "output larger than 1000 bytes") {
				t.Errorf("message %q", err)
			}
		})
	}
	r, err := render(t, `{{range 1000}}x{{end}}`, l, "full")
	if err != nil || len(r.Body) != 1000 {
		t.Errorf("exactly at the cap: %d bytes, %v", len(r.Body), err)
	}
}

func TestPrintfStar(t *testing.T) {
	_, err := render(t, `{{printf "%*d" 5 1}}`, Limits{}, "full")
	if !errors.Is(err, ErrExec) || !strings.Contains(err.Error(), "* width") {
		t.Errorf("err = %v", err)
	}
}

func TestDefaultLimits(t *testing.T) {
	l := Limits{}.withDefaults()
	if l.Timeout != DefaultTimeout || l.MaxOutputBytes != DefaultMaxOutputBytes || l.MaxTemplateBytes != DefaultMaxTemplateBytes {
		t.Errorf("defaults %+v", l)
	}
	if _, err := Compile(strings.Repeat("x", DefaultMaxTemplateBytes+1), Limits{}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("default template cap: %v", err)
	}
}

// A compiled template is shared by concurrent deliveries.
func TestRenderConcurrent(t *testing.T) {
	tm := mustCompile(t, `{{range .data.watch.labels}}{{.}}{{end}}`, Limits{})
	env := sampleJSON(t, "full")
	done := make(chan error)
	for range 8 {
		go func() {
			for range 50 {
				r, err := tm.Render(context.Background(), env)
				if err == nil && string(r.Body) != "apiplatform" {
					err = errors.New(string(r.Body))
				}
				if err != nil {
					done <- err
					return
				}
			}
			done <- nil
		}()
	}
	for range 8 {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}
}
