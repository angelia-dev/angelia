package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"text/template"
	"text/template/parse"
	"time"
)

// Default limits (ADR 0016).
const (
	DefaultTimeout          = time.Second
	DefaultMaxOutputBytes   = 256 << 10
	DefaultMaxTemplateBytes = 32 << 10
	// CheckTimeout bounds each sample render of Template.Check, so a slow
	// template cannot hold up configuration loading.
	CheckTimeout = 250 * time.Millisecond
)

// Content types of a Rendered body.
const (
	// ContentTypeCloudEvents is the default body: the event in CloudEvents
	// JSON structured mode.
	ContentTypeCloudEvents = "application/cloudevents+json; charset=utf-8"
	// ContentTypeJSON is a template output that is valid JSON.
	ContentTypeJSON = "application/json; charset=utf-8"
	// ContentTypeText is any other template output.
	ContentTypeText = "text/plain; charset=utf-8"
)

// templateName names the parsed template; it shows in no message we return.
const templateName = "message"

// tickFunc is the function Compile calls at the start of every range
// iteration to enforce the deadline. A template may call it too: it prints
// nothing.
const tickFunc = "angeliaTick"

// Error kinds; an *Error unwraps to one of them.
var (
	// ErrParse: the template does not parse.
	ErrParse = errors.New("render: template does not parse")
	// ErrNotAllowed: the template uses something Compile rejects (size,
	// define, template, block).
	ErrNotAllowed = errors.New("render: template not allowed")
	// ErrExec: execution failed (missing key, nil value, function error).
	ErrExec = errors.New("render: template execution failed")
	// ErrTimeout: execution ran past its deadline or its context ended.
	ErrTimeout = errors.New("render: template execution timed out")
	// ErrOutputTooLarge: the output or a string a function builds would pass
	// MaxOutputBytes.
	ErrOutputTooLarge = errors.New("render: template output too large")
	// ErrInvalidEvent: the event to render is not a JSON object.
	ErrInvalidEvent = errors.New("render: event is not a JSON object")
)

// Error is a template error. Its message gives the position and the reason
// but never quotes the template text, which may hold something the
// configuration's credential check did not catch.
type Error struct {
	// Line and Col locate the error in the template, 0 when unknown.
	Line, Col int
	// Reason says what went wrong.
	Reason string
	kind   error
}

func (e *Error) Error() string {
	switch {
	case e.Line > 0 && e.Col > 0:
		return fmt.Sprintf("line %d, column %d: %s", e.Line, e.Col, e.Reason)
	case e.Line > 0:
		return fmt.Sprintf("line %d: %s", e.Line, e.Reason)
	}
	return e.Reason
}

// Unwrap returns the error kind (ErrParse, ErrExec, ...).
func (e *Error) Unwrap() error { return e.kind }

// Limits bound a template. A zero field takes its default.
type Limits struct {
	Timeout          time.Duration
	MaxOutputBytes   int
	MaxTemplateBytes int
}

func (l Limits) withDefaults() Limits {
	if l.Timeout <= 0 {
		l.Timeout = DefaultTimeout
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if l.MaxTemplateBytes <= 0 {
		l.MaxTemplateBytes = DefaultMaxTemplateBytes
	}
	return l
}

// Rendered is the body a sink delivers.
type Rendered struct {
	Body []byte
	// ContentType is ContentTypeCloudEvents without a template; with one,
	// ContentTypeJSON when Body is valid JSON and ContentTypeText otherwise.
	ContentType string
	// Templated tells whether a subscription template produced Body.
	Templated bool
}

// Template is a compiled subscription template. It is safe for concurrent
// use.
type Template struct {
	tmpl   *template.Template
	limits Limits
}

// Compile parses src as a message template. src must not be empty: a
// subscription without a template renders the default (Render with a nil
// Template).
func Compile(src string, l Limits) (*Template, error) {
	l = l.withDefaults()
	if src == "" {
		return nil, &Error{Reason: "template is empty", kind: ErrNotAllowed}
	}
	if len(src) > l.MaxTemplateBytes {
		return nil, &Error{Reason: fmt.Sprintf("template is %d bytes, at most %d", len(src), l.MaxTemplateBytes), kind: ErrNotAllowed}
	}
	t, err := template.New(templateName).Option("missingkey=error").Funcs(funcMap(nil, l.MaxOutputBytes)).Parse(src)
	if err != nil {
		return nil, parseError(err)
	}
	if len(t.Templates()) > 1 {
		return nil, &Error{Reason: "define and block are not allowed", kind: ErrNotAllowed}
	}
	if t.Tree != nil && t.Root != nil {
		if err := instrument(t.Tree, t.Root); err != nil {
			return nil, err
		}
	}
	return &Template{tmpl: t, limits: l}, nil
}

// tickNode is an action calling tickFunc, shared by every instrumented range
// body: execution only reads nodes.
var tickNode = func() parse.Node {
	t := template.Must(template.New("tick").Funcs(template.FuncMap{tickFunc: noTick}).Parse("{{" + tickFunc + "}}"))
	return t.Root.Nodes[0]
}()

func noTick() (string, error) { return "", nil }

// instrument prepends tickNode to every range body under n and rejects
// template invocations, so every loop iteration checks the deadline and no
// recursion can bypass it.
func instrument(tr *parse.Tree, n parse.Node) error {
	switch n := n.(type) {
	case *parse.ListNode:
		if n == nil {
			return nil
		}
		for _, c := range n.Nodes {
			if err := instrument(tr, c); err != nil {
				return err
			}
		}
	case *parse.TemplateNode:
		e := &Error{Reason: "the template action is not allowed", kind: ErrNotAllowed}
		loc, _ := tr.ErrorContext(n) // "NAME:LINE:COL"
		if m := locRE.FindStringSubmatch(loc); m != nil {
			e.Line, _ = strconv.Atoi(m[1])
			e.Col, _ = strconv.Atoi(m[2])
		}
		return e
	case *parse.RangeNode:
		if err := branch(tr, &n.BranchNode); err != nil {
			return err
		}
		n.List.Nodes = append([]parse.Node{tickNode}, n.List.Nodes...)
	case *parse.IfNode:
		return branch(tr, &n.BranchNode)
	case *parse.WithNode:
		return branch(tr, &n.BranchNode)
	}
	return nil
}

func branch(tr *parse.Tree, b *parse.BranchNode) error {
	if err := instrument(tr, b.List); err != nil {
		return err
	}
	return instrument(tr, b.ElseList)
}

// Render renders the event envelope (the JSON of an event.Envelope, as
// stored in event.payload_json) with t, or returns the envelope unchanged as
// the CloudEvents body when t is nil.
func Render(ctx context.Context, t *Template, envelope []byte) (Rendered, error) {
	if t == nil {
		if !isObject(envelope) {
			return Rendered{}, ErrInvalidEvent
		}
		return Rendered{Body: bytes.Clone(envelope), ContentType: ContentTypeCloudEvents}, nil
	}
	return t.Render(ctx, envelope)
}

// Render executes t on the event envelope JSON.
func (t *Template) Render(ctx context.Context, envelope []byte) (Rendered, error) {
	g := newGuard(t.limits.Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(g.deadline) {
		g.deadline, g.ctxDeadline = d, true
	}
	g.done = ctx.Done()
	return t.render(g, envelope)
}

func (t *Template) render(g *guard, envelope []byte) (Rendered, error) {
	var data map[string]any
	if err := json.Unmarshal(envelope, &data); err != nil || data == nil {
		return Rendered{}, ErrInvalidEvent
	}
	c, err := t.tmpl.Clone()
	if err != nil {
		return Rendered{}, fmt.Errorf("render: clone template: %w", err)
	}
	c.Funcs(funcMap(g, t.limits.MaxOutputBytes))
	w := &cappedWriter{max: t.limits.MaxOutputBytes, g: g}
	if err := c.Execute(w, data); err != nil {
		return Rendered{}, t.execError(err, g)
	}
	body := w.buf.Bytes()
	return Rendered{Body: body, ContentType: contentType(body), Templated: true}, nil
}

// contentType is ContentTypeJSON for an object or array that is valid JSON,
// ContentTypeText for anything else (a bare "true" or "42" is text).
func contentType(body []byte) string {
	b := bytes.TrimSpace(body)
	if len(b) > 0 && (b[0] == '{' || b[0] == '[') && json.Valid(b) {
		return ContentTypeJSON
	}
	return ContentTypeText
}

func isObject(b []byte) bool {
	var m map[string]json.RawMessage
	return json.Unmarshal(b, &m) == nil && m != nil
}

// guard ends an execution at its deadline or when done (a context's Done,
// nil for none) is closed.
type guard struct {
	done     <-chan struct{}
	deadline time.Time
	timeout  time.Duration // for the error message
	// ctxDeadline: deadline is the caller's context deadline, earlier than
	// the template timeout. ctxEnded: execution stopped because the
	// caller's context ended, not because the template ran too long.
	ctxDeadline, ctxEnded bool
}

func newGuard(timeout time.Duration) *guard {
	return &guard{deadline: time.Now().Add(timeout), timeout: timeout}
}

func (g *guard) check() error {
	select {
	case <-g.done:
		g.ctxEnded = true
		return ErrTimeout
	default:
	}
	if !time.Now().Before(g.deadline) {
		g.ctxEnded = g.ctxDeadline
		return ErrTimeout
	}
	return nil
}

func (g *guard) tick() (string, error) { return "", g.check() }

// cappedWriter collects the output, failing past max bytes or the deadline.
type cappedWriter struct {
	buf bytes.Buffer
	max int
	g   *guard
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if err := w.g.check(); err != nil {
		return 0, err
	}
	if int64(w.buf.Len())+int64(len(p)) > int64(w.max) {
		return 0, ErrOutputTooLarge
	}
	return w.buf.Write(p)
}
