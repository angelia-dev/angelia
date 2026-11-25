package source

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

func TestNewNotesCleaning(t *testing.T) {
	fence := "```"
	tests := []struct {
		name, in, want string
		nilOut         bool
	}{
		{name: "crlf", in: "a\r\nb\r\nc", want: "a\nb\nc"},
		{name: "lone cr", in: "a\rb\r\rc", want: "a\nb\n\nc"},
		{name: "c0 removed keeps newline and tab", in: "a\x00b\x07c\x1b[31m\td\ne\x7f", want: "abc[31m\td\ne"},
		{name: "c1 removed", in: "a\u0080b\u0085c\u009fd", want: "abcd"},
		{name: "printable and rtl kept", in: "ok � \u202e שלום \U0001F600", want: "ok � \u202e שלום \U0001F600"},
		{name: "invalid utf8", in: "a\xffb", want: "a�b"},
		{name: "comment", in: "a <!-- hidden --> b", want: "a  b"},
		{name: "two comments one line", in: "a<!--x-->b<!--y-->c", want: "abc"},
		{name: "multiline comment", in: "a\n<!--\nsecret\nlines\n-->\nb", want: "a\n\nb"},
		{name: "comment in backtick fence kept", in: "a\n" + fence + "html\n<!-- keep -->\n" + fence + "\nb <!-- drop -->", want: "a\n" + fence + "html\n<!-- keep -->\n" + fence + "\nb"},
		{name: "comment in tilde fence kept", in: "~~~~\n<!-- keep -->\n~~~~\n<!-- drop -->x", want: "~~~~\n<!-- keep -->\n~~~~\nx"},
		{name: "shorter fence does not close", in: "````\n" + fence + "\n<!-- keep -->\n````\n<!-- drop -->", want: "````\n" + fence + "\n<!-- keep -->\n````"},
		{name: "unclosed fence runs to the end", in: fence + "\n<!-- keep -->", want: fence + "\n<!-- keep -->"},
		{name: "unclosed comment kept", in: "a <!-- never closed\nb", want: "a <!-- never closed\nb"},
		{name: "comment spans a fence", in: "a\n<!-- open\n" + fence + "\nPAYLOAD\n" + fence + "\nclose -->\nb", want: "a\n\nb"},
		{name: "fence inside a comment does not hide the next comment", in: "x\n<!--\n" + fence + "\n-->\n<!-- PAYLOAD -->", want: "x"},
		{name: "joined comment is not rebuilt", in: "<!<!-- a -->-- b -->x", want: "<! -- b -->x"},
		{name: "trim", in: " \n\t a \n\n", want: "a"},
		{name: "empty", in: "", nilOut: true},
		{name: "blank", in: " \r\n\t ", nilOut: true},
		{name: "only comment", in: "<!-- x -->\n", nilOut: true},
		{name: "only controls", in: "\x00\x01", nilOut: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NewNotes("markdown", tt.in)
			if tt.nilOut {
				if got != nil {
					t.Fatalf("NewNotes = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("NewNotes = nil")
			}
			if got.Body != tt.want || got.Truncated || got.Format != "markdown" {
				t.Fatalf("NewNotes = %+v, want body %q", got, tt.want)
			}
		})
	}
}

func TestNewNotesCap(t *testing.T) {
	exact := NewNotes("text", strings.Repeat("a", MaxNotesBytes))
	if len(exact.Body) != MaxNotesBytes || exact.Truncated {
		t.Fatalf("exact: len %d truncated %v", len(exact.Body), exact.Truncated)
	}
	over := NewNotes("text", strings.Repeat("a", MaxNotesBytes+1))
	if len(over.Body) != MaxNotesBytes || !over.Truncated {
		t.Fatalf("over: len %d truncated %v", len(over.Body), over.Truncated)
	}
	// Cleaning runs before the cut: comments and CRs do not count.
	cleaned := NewNotes("text", strings.Repeat("a\r\n", 100)+"<!--"+strings.Repeat("x", 20000)+"-->")
	if cleaned.Truncated || len(cleaned.Body) != 100*2-1 {
		t.Fatalf("cleaning before the cut: len %d truncated %v", len(cleaned.Body), cleaned.Truncated)
	}
}

func TestNewNotesCutsAtCharacterBoundary(t *testing.T) {
	for _, ch := range []string{"é", "€", "\U0001F600"} { // 2, 3 and 4 bytes
		for pad := 0; pad <= len(ch); pad++ {
			in := strings.Repeat("a", MaxNotesBytes-pad) + strings.Repeat(ch, 3)
			n := NewNotes("text", in)
			if n == nil || !utf8.ValidString(n.Body) || len(n.Body) > MaxNotesBytes {
				t.Fatalf("%q pad %d: bad body (len %d)", ch, pad, len(n.Body))
			}
			if len(in) > MaxNotesBytes && !n.Truncated {
				t.Fatalf("%q pad %d: want truncated", ch, pad)
			}
			if !strings.HasPrefix(in, n.Body) {
				t.Fatalf("%q pad %d: body is not a prefix", ch, pad)
			}
		}
	}
}

// A cut body must not keep the (much larger) input alive.
func TestNewNotesDoesNotRetainInput(t *testing.T) {
	in := strings.Repeat("a", 500_000)
	n := NewNotes("markdown", in)
	if !n.Truncated || len(n.Body) != MaxNotesBytes {
		t.Fatalf("len %d truncated %v", len(n.Body), n.Truncated)
	}
	start := uintptr(unsafe.Pointer(unsafe.StringData(in))) //nolint:gosec // asserts the body does not alias the input backing array
	p := uintptr(unsafe.Pointer(unsafe.StringData(n.Body))) //nolint:gosec // asserts the body does not alias the input backing array
	if p >= start && p < start+uintptr(len(in)) {
		t.Fatal("body points into the input")
	}
	// Same for a body that shrank to nothing like its input.
	small := NewNotes("markdown", " "+strings.Repeat("a", 10)+strings.Repeat(" ", 1000))
	if small.Body != strings.Repeat("a", 10) {
		t.Fatalf("body %q", small.Body)
	}
}

// nestedComments is the reviewer's input: layers of fences that each flip
// the fence state of the next pass, around a hidden payload.
func nestedComments(depth int) string {
	ticks := func(n int) string { return strings.Repeat("`", n) }
	s := "<!-- PAYLOAD -->" + ticks(3) + "\n"
	for k := 1; k <= depth; k++ {
		s = "<!---->" + ticks(3+k) + "\n" + ticks(3+k) + "\n" + s + ticks(3+k) + "\n"
	}
	return s
}

// visibleComment reports whether s has a closed comment that opens outside a
// fenced code block, the property NewNotes guarantees.
func visibleComment(s string) bool {
	var fence byte
	var fenceN int
	for len(s) > 0 {
		line, rest := s, ""
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			line, rest = s[:i+1], s[i+1:]
		}
		switch {
		case fence != 0:
			if fenceCloses(line, fence, fenceN) {
				fence = 0
			}
		default:
			if c, n := fenceOpen(line); c != 0 {
				fence, fenceN = c, n
			} else if i := strings.Index(line, "<!--"); i >= 0 && strings.Contains(s[i+4:], "-->") {
				return true
			}
		}
		s = rest
	}
	return false
}

func TestNewNotesNestedFencesFailClosed(t *testing.T) {
	for _, depth := range []int{1, 5, 15, 16, 17, 18, 40} {
		in := nestedComments(depth)
		n := NewNotes("markdown", in)
		if n == nil {
			continue
		}
		if visibleComment(n.Body) {
			t.Fatalf("depth %d: a closed comment survives outside a fence:\n%s", depth, n.Body)
		}
		if again := NewNotes("markdown", n.Body); again == nil || again.Body != n.Body {
			t.Fatalf("depth %d: not idempotent", depth)
		}
		if depth >= maxCommentPasses && strings.Contains(n.Body, "<!-- PAYLOAD -->") {
			t.Fatalf("depth %d: payload survives", depth)
		}
	}
}

func FuzzNewNotes(f *testing.F) {
	for _, s := range []string{"", "a\r\nb", "<!-- x -->y", "```\n<!-- x -->\n```", "<!<!-- a -->-- b -->", "<!-- o\n```\nP\n```\nc -->", nestedComments(17), "\x00\xff\u0085", strings.Repeat("é", 9000)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		n := NewNotes("markdown", in)
		if n == nil {
			return
		}
		if !utf8.ValidString(n.Body) {
			t.Fatalf("invalid UTF-8: %q", n.Body)
		}
		if len(n.Body) > MaxNotesBytes {
			t.Fatalf("body of %d bytes", len(n.Body))
		}
		if n.Body == "" {
			t.Fatal("empty body must be nil")
		}
		for _, r := range n.Body {
			if r != '\n' && r != '\t' && unicode.IsControl(r) {
				t.Fatalf("control character %U in %q", r, n.Body)
			}
		}
		if visibleComment(n.Body) {
			t.Fatalf("closed comment outside a fence: %q", n.Body)
		}
		again := NewNotes("markdown", n.Body)
		if again == nil || again.Body != n.Body {
			t.Fatalf("not idempotent: %q then %+v", n.Body, again)
		}
	})
}

// Layers of joined comments must not cost a pass per layer.
func TestNewNotesHostileNesting(t *testing.T) {
	in := strings.Repeat("<!", 50_000) + "<!-- a -->" + strings.Repeat("-- b -->", 50_000)
	if n := NewNotes("markdown", in); n == nil || len(n.Body) > MaxNotesBytes {
		t.Fatalf("unexpected result %v", n)
	}
}
