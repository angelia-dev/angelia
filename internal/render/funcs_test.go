package render

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// AC: a curated func map, no env, no file access. Adding a function means
// updating this list and docs/templates.md.
func TestFuncNamesCurated(t *testing.T) {
	want := []string{
		"and", "contains", "date", "default", "eq", "ge", "gt", "hasPrefix", "hasSuffix", "html",
		"index", "join", "js", "le", "len", "lower", "lt", "ne", "not", "or", "print", "printf",
		"println", "replace", "slice", "split", "toJSON", "trim", "trimPrefix", "trimSuffix",
		"truncate", "upper", "urlquery",
	}
	if got := FuncNames(); !slices.Equal(got, want) {
		t.Errorf("FuncNames() = %q\nwant %q", got, want)
	}
	for _, name := range []string{"env", "expandenv", "getenv", "readFile", "file", "include", "exec", "now", "http"} {
		if slices.Contains(FuncNames(), name) {
			t.Errorf("%s is available", name)
		}
	}
}

func TestFuncs(t *testing.T) {
	tests := []struct{ src, sample, want string }{
		{`{{.data.package | upper}}`, "full", "REQUESTS"},
		{`{{"ABC" | lower}}`, "full", "abc"},
		{`{{"  a b  " | trim}}`, "full", "a b"},
		{`{{.data.version | trimPrefix "v"}}`, "sparse", "0.1.0"},
		{`{{"x.tar.gz" | trimSuffix ".gz"}}`, "full", "x.tar"},
		{`{{contains "/" .data.package}} {{hasPrefix "oct" .data.package}} {{hasSuffix "repo" .data.package}}`, "sparse", "true true true"},
		{`{{replace "-" "_" .data.package}}`, "sparse", "octo_org/octo_repo"},
		{`{{split "/" .data.package | join " / "}}`, "sparse", "octo-org / octo-repo"},
		{`{{index (split "/" .data.package) 1}}`, "sparse", "octo-repo"},
		{`{{join ", " nil}}|{{join "," (split "," "")}}`, "full", "|"},
		{`{{truncate 5 "héllo wörld"}}|{{truncate 20 "héllo"}}|{{truncate 0 "x"}}|{{truncate 1 "xy"}}`, "full", "héll…|héllo||…"},
		{`{{.data.watch.name | default "unnamed"}}`, "sparse", "unnamed"},
		{`{{.data.watch.name | default "unnamed"}}`, "full", "requests"},
		{`{{default "x" ""}} {{default "x" false}}`, "full", "x false"},
		{`{{toJSON .data.watch.labels}}`, "full", `{"repo":"api","team":"platform"}`},
		{`{{toJSON "a<b>&\"c\""}}`, "full", `"a<b>&\"c\""`},
		{`{{toJSON .data.notes}}`, "sparse", `null`},
		{`{{date "2006-01-02 15:04 MST" .data.published_at}}`, "full", "2026-10-27 14:02 UTC"},
		{`{{date "Jan 2" .time}}`, "full", "Oct 30"},
		{`[{{date "2006" .data.published_at}}]`, "sparse", "[]"},
		{`{{html "<a&b>"}} {{js "it's"}} {{urlquery "a b&c"}}`, "full", `&lt;a&amp;b&gt; it\'s a+b%26c`},
		{`{{print "a" 1 2 "b"}}|{{println "x"}}|{{printf "%05.1f %q" 3.14159 "s"}}`, "full", "a1 2b|x\n|003.1 \"s\""},
		{`{{printf "%[2]s %[1]s %%" "a" "b"}}`, "full", "b a %"},
		{`{{len .data.watch.labels}} {{and .data.backport true}} {{or .data.bump "none"}}`, "sparse", "0 false none"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			r, err := render(t, tt.src, Limits{}, tt.sample)
			if err != nil {
				t.Fatal(err)
			}
			if string(r.Body) != tt.want {
				t.Errorf("got %q, want %q", r.Body, tt.want)
			}
		})
	}
}

func TestFuncErrors(t *testing.T) {
	tests := []struct{ src, reason string }{
		{`{{upper 1}}`, "error calling upper: argument: want a string, got int"},
		{`{{trimPrefix "v" .data.backport}}`, "error calling trimPrefix: argument: want a string, got bool"},
		{`{{contains "a" .data.watch}}`, "error calling contains: argument: want a string, got map[string]interface {}"},
		{`{{replace "a" "b" 1}}`, "error calling replace: replace: want a string, got int"},
		{`{{split "," 1}}`, "error calling split: split: want a string, got int"},
		{`{{join "," "abc"}}`, "error calling join: join: want a list, got string"},
		{`{{join "," .data.watch.labels}}`, "error calling join: join: want a list, got map[string]interface {}"},
		{`{{truncate -1 "x"}}`, "error calling truncate: truncate: negative length"},
		{`{{truncate 1 .data.backport}}`, "error calling truncate: truncate: want a string, got bool"},
		{`{{date "2006" "yesterday"}}`, "error calling date: date: not an RFC 3339 timestamp"},
		{`{{date "2006" 1}}`, "error calling date: date: want a string, got int"},
		{`{{toJSON .data}}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			_, err := render(t, tt.src, Limits{}, "full")
			if tt.reason == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var e *Error
			if !errors.As(err, &e) || e.Reason != tt.reason {
				t.Errorf("err = %v, want reason %q", err, tt.reason)
			}
		})
	}
	// join over a JSON list holding a non-string.
	tm := mustCompile(t, `{{join "," .list}}`, Limits{})
	if _, err := tm.Render(context.Background(), []byte(`{"list":["a",1]}`)); err == nil || !strings.Contains(err.Error(), "element 1: want a string, got float64") {
		t.Errorf("join mixed list: %v", err)
	}
	if r, err := tm.Render(context.Background(), []byte(`{"list":["a","b"]}`)); err != nil || string(r.Body) != "a,b" {
		t.Errorf("join JSON list: %q %v", r.Body, err)
	}
	// toJSON of a value encoding/json refuses.
	if _, err := (capper{max: 100}).toJSON(func() {}); err == nil {
		t.Error("toJSON(func) succeeded")
	}
}

func TestFormatBound(t *testing.T) {
	tests := []struct {
		format  string
		largest int
		want    int
	}{
		{"abc", 10, 3},
		{"%s", 10, 12},
		{"%10.5f", 1, 6 + 1 + 15},
		{"%-08d|%+x", 2, 9 + 2 + 8 + 2},
		{"%[1]s%[1]s", 4, 10 + 4 + 1 + 4 + 1},
		{"%", 3, 4},
	}
	for _, tt := range tests {
		if got, err := formatBound(tt.format, tt.largest); err != nil || got != tt.want {
			t.Errorf("formatBound(%q, %d) = %d, %v; want %d", tt.format, tt.largest, got, err, tt.want)
		}
	}
	if _, err := formatBound("%99999999999d", 0); !errors.Is(err, ErrOutputTooLarge) {
		t.Errorf("huge width: %v", err)
	}
	if _, err := formatBound("%.*f", 0); err == nil {
		t.Error("* accepted")
	}
}
