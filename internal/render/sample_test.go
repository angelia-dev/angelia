package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/event"
)

// The samples are events of the spec's shape: every key of event.Envelope,
// nothing else, and between them every nullable field both set and null.
func TestSamplesShape(t *testing.T) {
	samples := Samples()
	if len(samples) != 2 || samples[0].Name != "full" || samples[1].Name != "sparse" {
		t.Fatalf("samples %v", samples)
	}
	for _, s := range samples {
		b, err := json.Marshal(s.Envelope)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		var back event.Envelope
		if err := dec.Decode(&back); err != nil || !reflect.DeepEqual(back, s.Envelope) {
			t.Errorf("%s: round trip %v", s.Name, err)
		}
		if event.ShortType(s.Envelope.Type) != "release.published" {
			t.Errorf("%s: type %s", s.Name, s.Envelope.Type)
		}
	}
	nullable := func(d event.Data) []bool {
		return []bool{
			d.PreviousVersion != nil, d.Bump != nil, d.PublishedAt != nil, d.CooldownSatisfiedAt != nil,
			d.URLs.Release != nil, d.URLs.Changelog != nil, d.URLs.Artifact != nil, d.Notes != nil,
			d.Watch.Name != nil, len(d.Watch.Labels) > 0, d.PURL != nil,
		}
	}
	for i, set := range nullable(samples[0].Envelope.Data) {
		if !set {
			t.Errorf("full: nullable field %d is null", i)
		}
	}
	for i, set := range nullable(samples[1].Envelope.Data) {
		if set {
			t.Errorf("sparse: nullable field %d is set", i)
		}
	}
	// Timestamps have the spec's form.
	ts := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,3})?Z$`)
	for _, s := range samples {
		for _, v := range []string{s.Envelope.Time, s.Envelope.Data.DetectedAt} {
			if !ts.MatchString(v) {
				t.Errorf("%s: timestamp %q", s.Name, v)
			}
		}
	}
}

// AC: templates are validated at config time with a sample event.
func TestCheck(t *testing.T) {
	good := []string{
		`{{.data.package}} {{.data.version}}`,
		`{{with .data.notes}}{{.body}}{{end}}`,
		`{{range $k, $v := .data.watch.labels}}{{$k}}{{end}}`,
	}
	for _, src := range good {
		if err := mustCompile(t, src, Limits{}).Check(); err != nil {
			t.Errorf("Check(%q) = %v", src, err)
		}
	}
	bad := []struct {
		src  string
		kind error
		msg  string
	}{
		{`{{.data.pakage}}`, ErrExec, `on the full sample event: line 1, column 7: unknown key "pakage"`},
		{`{{.data.notes.body}}`, ErrExec, `on the sparse sample event: line 1, column 7: cannot read field "body" of a null value`},
		{`{{.data.previous_version | upper}}`, nil, ""}, // null is "" for string functions
		{`{{range 2000000000}}{{end}}`, ErrTimeout, "on the full sample event: execution took longer than 20ms"},
		{`{{range 300}}x{{end}}`, ErrOutputTooLarge, "on the full sample event: output larger than 200 bytes"},
	}
	for _, tt := range bad {
		err := mustCompile(t, tt.src, Limits{Timeout: 20 * time.Millisecond, MaxOutputBytes: 200}).Check()
		if tt.kind == nil {
			if err != nil {
				t.Errorf("Check(%q) = %v", tt.src, err)
			}
			continue
		}
		if !errors.Is(err, tt.kind) {
			t.Errorf("Check(%q) = %v, want %v", tt.src, err, tt.kind)
			continue
		}
		if err.Error() != tt.msg {
			t.Errorf("Check(%q) = %q, want %q", tt.src, err, tt.msg)
		}
	}
}

// Every template in docs/templates.md compiles and passes Check, so the
// documented examples stay correct.
func TestDocExamples(t *testing.T) {
	doc, err := os.ReadFile("../../docs/templates.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)```gotemplate\n(.*?)```").FindAllStringSubmatch(string(doc), -1)
	if len(blocks) < 3 {
		t.Fatalf("found %d gotemplate blocks", len(blocks))
	}
	for _, b := range blocks {
		src := strings.TrimSuffix(b[1], "\n")
		tm, err := Compile(src, Limits{})
		if err != nil {
			t.Errorf("%s\n: %v", src, err)
			continue
		}
		if err := tm.Check(); err != nil {
			t.Errorf("%s\n: %v", src, err)
		}
	}
}
