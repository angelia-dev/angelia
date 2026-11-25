package source

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNotesBytes bounds Notes.Body, in bytes of UTF-8: the limit of the
// release event schema (schemas/release/v1.json, notes.body).
const MaxNotesBytes = 16384

// maxCommentPasses bounds the comment removal passes of NewNotes.
const maxCommentPasses = 16

// Notes is the release notes text of one version, as the event's `notes`
// object carries it. A Source that has no notes leaves Version.Notes nil.
// Build Notes with NewNotes, which cleans and bounds the text.
//
// The body is untrusted upstream content. See ADR 0036.
type Notes struct {
	// Format names the markup of Body: "markdown" or "text".
	Format string `json:"format"`
	// Body is at most MaxNotesBytes bytes of valid UTF-8.
	Body string `json:"body"`
	// Truncated is true when Body is a prefix of the cleaned upstream text.
	Truncated bool `json:"truncated"`
}

// NewNotes returns the notes for an upstream body, or nil when nothing is
// left after cleaning. It is the one place that cleans and bounds notes text
// (ADR 0036, D32 and D34); sources and enrichers call it and nothing else.
//
// Cleaning, in this order:
//  1. invalid UTF-8 becomes U+FFFD;
//  2. "\r\n" and a lone "\r" become "\n";
//  3. control characters other than "\n" and "\t" are removed (C0, DEL, C1);
//  4. HTML comments ("<!-- ... -->", possibly over several lines) are removed
//     when they open outside a fenced code block, through the first "-->"
//     even if fence lines lie in between (as CommonMark and the forges
//     render it); a "<!--" inside a fence is kept, and so is an unclosed
//     one. Removal repeats until a pass removes nothing; after 16 passes
//     every remaining closed comment is removed, fences ignored;
//  5. leading and trailing white space is trimmed.
//
// The result is then cut to MaxNotesBytes at a character boundary, Truncated
// telling whether it was cut, and trimmed again at the end. The returned
// body never shares memory with body, so that a cut 500 KB upstream text is
// not kept alive by a few kilobytes of it. NewNotes is idempotent on Body.
func NewNotes(format, body string) *Notes {
	s := strings.ToValidUTF8(body, "�")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	// Removing a comment can change the fence structure around it, so
	// repeat until a pass removes nothing, at most maxCommentPasses times to
	// bound the work on hostile input. Fail closed: if the text has not
	// settled by then, remove every closed comment, fences ignored.
	settled := false
	for range maxCommentPasses {
		next := stripHTMLComments(s)
		if len(next) == len(s) {
			settled = true
			break
		}
		s = next
	}
	if !settled {
		s = removeComments(s)
	}
	s = strings.TrimSpace(s)
	truncated := false
	if len(s) > MaxNotesBytes {
		cut := MaxNotesBytes
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s, truncated = strings.TrimSpace(s[:cut]), true
	}
	if s == "" {
		return nil
	}
	return &Notes{Format: format, Body: strings.Clone(s), Truncated: truncated}
}

// stripHTMLComments removes the comments that open outside a fenced code
// block. As in CommonMark (an HTML block of type 2 runs to the first "-->"),
// a comment that opens outside a fence is removed through its first "-->"
// even when fence lines lie in between; a "<!--" inside a fence is kept, and
// so is an unclosed "<!--". A fence opens on a line of its own, so the text
// that follows a removed comment on its line is not tested for a fence.
func stripHTMLComments(s string) string {
	if !strings.Contains(s, "<!--") {
		return s
	}
	var (
		out    []byte
		fence  byte // the open fence's character, 0 outside a fence
		fenceN int  // the open fence's length
		mid    bool // s starts in the middle of a line, after a comment
	)
	for len(s) > 0 {
		line, rest := s, ""
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			line, rest = s[:i+1], s[i+1:]
		}
		if fence != 0 {
			out = append(out, line...)
			if fenceCloses(line, fence, fenceN) {
				fence = 0
			}
			s, mid = rest, false
			continue
		}
		if c, n := fenceOpen(line); c != 0 && !mid {
			out = append(out, line...)
			fence, fenceN = c, n
			s = rest
			continue
		}
		i := strings.Index(line, "<!--")
		if i < 0 {
			out = append(out, line...)
			s, mid = rest, false
			continue
		}
		j := strings.Index(s[i+4:], "-->")
		if j < 0 {
			// Nothing closes this comment, nor any later one.
			out = append(out, s...)
			break
		}
		out = append(out, line[:i]...)
		s, mid = s[i+4+j+3:], true
		if joinsComment(out, s) {
			out = append(out, ' ')
		}
	}
	return string(out)
}

// removeComments removes every closed "<!-- ... -->" from s. When the text
// around a removed comment would join into a new "<!--", it keeps one space
// between them, so that one pass is enough for text like "<!<!-- a -->-- b".
func removeComments(s string) string {
	var out []byte
	for {
		i := strings.Index(s, "<!--")
		if i < 0 {
			break
		}
		j := strings.Index(s[i+4:], "-->")
		if j < 0 {
			break
		}
		out = append(out, s[:i]...)
		s = s[i+4+j+3:]
		if joinsComment(out, s) {
			out = append(out, ' ')
		}
	}
	return string(append(out, s...))
}

// joinsComment reports whether "<!--" would appear across the join of head
// and tail.
func joinsComment(head []byte, tail string) bool {
	h := string(head[max(0, len(head)-3):])
	t := tail[:min(len(tail), 3)]
	return strings.Contains(h+t, "<!--")
}

// fenceOpen reports whether line opens a fenced code block: up to three
// spaces, then three or more backticks or tildes (a backtick fence has no
// backtick in its info string). It returns the fence character and length.
func fenceOpen(line string) (byte, int) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return 0, 0
	}
	c := t[0]
	n := 0
	for n < len(t) && t[n] == c {
		n++
	}
	if n < 3 || (c == '`' && strings.IndexByte(t[n:], '`') >= 0) {
		return 0, 0
	}
	return c, n
}

// fenceCloses reports whether line closes a fence opened with n characters c:
// up to three spaces, at least n of c, and nothing else but white space.
func fenceCloses(line string, c byte, n int) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	k := 0
	for k < len(t) && t[k] == c {
		k++
	}
	return k >= n && strings.TrimSpace(t[k:]) == ""
}
