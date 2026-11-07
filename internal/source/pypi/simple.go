package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/source"
	"github.com/angelia-dev/angelia/internal/version/pep440"
)

// simpleAccept asks for PEP 691 JSON and accepts the HTML forms of PEP 503
// and PEP 691 at lower preference, as PEP 691 recommends.
const simpleAccept = "application/vnd.pypi.simple.v1+json, application/vnd.pypi.simple.v1+html;q=0.2, text/html;q=0.01"

// simpleFile is one distribution file of the Simple API: the part of a PEP
// 503 anchor or a PEP 691 / PEP 700 file entry the source reads.
type simpleFile struct {
	Filename   string
	Yanked     bool
	Reason     string
	UploadTime string
}

// listSimple lists releases from {base}/simple/{name}/.
func (s *Source) listSimple(ctx context.Context, name string, st source.FetchState) (source.Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/simple/"+url.PathEscape(name)+"/", nil)
	if err != nil {
		return source.Result{}, fmt.Errorf("pypi: build request for %q: %w", name, err)
	}
	req.Header.Set("Accept", simpleAccept)
	prev := netguard.Validators{ETag: st.ETag, LastModified: st.LastModified}
	res, err := netguard.Fetch(ctx, s.client, req, prev)
	if err != nil {
		return source.Result{}, fmt.Errorf("pypi: fetch %q: %w", name, err)
	}
	state := source.FetchState{ETag: res.Validators.ETag, LastModified: res.Validators.LastModified}
	if res.NotModified {
		return source.Result{State: state, NotModified: true}, nil
	}
	var (
		files    []simpleFile
		declared []string
	)
	if isJSONBody(res.Header.Get("Content-Type"), res.Body) {
		files, declared, err = parseSimpleJSON(res.Body)
		if err != nil {
			return source.Result{}, fmt.Errorf("pypi: decode response for %q: %w", name, err)
		}
	} else {
		files = parseSimpleHTML(ctx, s.log, res.Body)
	}

	// Group by PEP 440 equality when the version parses (so 1.0b0 and
	// 1.0-beta, or 1.0 and 1.0.0, are one release), else by the raw string.
	// Raw is the spelling of the first file.
	type group struct {
		raw    string
		parsed pep440.Version
		ok     bool
		files  []file
	}
	var groups []*group
	find := func(raw string) *group {
		p, err := pep440.Parse(raw)
		for _, g := range groups {
			if (err == nil && g.ok && pep440.Compare(g.parsed, p) == 0) || ((err != nil || !g.ok) && g.raw == raw) {
				return g
			}
		}
		return nil
	}
	for _, f := range files {
		raw, ok := versionOfFilename(name, f.Filename)
		if !ok {
			s.log.DebugContext(ctx, "pypi: skipping link that is not a file of the project", "project", name, "filename", f.Filename)
			continue
		}
		g := find(raw)
		if g == nil {
			g = &group{raw: raw}
			g.parsed, err = pep440.Parse(raw)
			g.ok = err == nil
			groups = append(groups, g)
		}
		g.files = append(g.files, file{UploadTime: f.UploadTime, Yanked: f.Yanked, YankedReason: f.Reason})
	}
	for _, raw := range declared {
		if raw != "" && find(raw) == nil {
			// As on the JSON path, a release without files is not listed.
			s.log.DebugContext(ctx, "pypi: skipping release without files", "project", name, "version", raw)
		}
	}
	versions := make([]source.Version, 0, len(groups))
	for _, g := range groups {
		versions = append(versions, s.version(ctx, name, g.raw, g.files))
	}
	sortNewestFirst(versions)
	return source.Result{Versions: versions, State: state}, nil
}

// isJSONBody tells a PEP 691 JSON body from HTML: by its Content-Type, and
// by its first byte when that is neither (a server that sends none).
func isJSONBody(contentType string, body []byte) bool {
	if mt, _, err := mime.ParseMediaType(contentType); err == nil {
		switch {
		case strings.HasSuffix(mt, "+json"), mt == "application/json":
			return true
		case strings.HasSuffix(mt, "+html"), mt == "text/html":
			return false
		}
	}
	return bytes.HasPrefix(bytes.TrimSpace(body), []byte("{"))
}

// parseSimpleJSON reads a PEP 691 project detail (with the PEP 700 additions
// the source uses): the files, and the versions key when present.
func parseSimpleJSON(body []byte) (files []simpleFile, versions []string, err error) {
	var doc struct {
		Versions []string `json:"versions"`
		Files    *[]struct {
			Filename   string          `json:"filename"`
			Yanked     json.RawMessage `json:"yanked"`
			UploadTime string          `json:"upload-time"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Files == nil {
		return nil, nil, fmt.Errorf("response has no \"files\"")
	}
	for _, f := range *doc.Files {
		sf := simpleFile{Filename: f.Filename, UploadTime: f.UploadTime}
		// PEP 592 / PEP 691: yanked is false, true, or a string (the
		// reason; present means yanked).
		var b bool
		var reason string
		switch {
		case json.Unmarshal(f.Yanked, &b) == nil:
			sf.Yanked = b
		case json.Unmarshal(f.Yanked, &reason) == nil:
			sf.Yanked, sf.Reason = true, reason
		}
		files = append(files, sf)
	}
	return files, doc.Versions, nil
}

// versionOfFilename returns the version of a distribution file of project
// name (its PEP 503 normalized name), from the filename: sdists
// ({name}-{version}.tar.gz, .zip and the other archive forms) and wheels
// ({name}-{version}(-{build})?-{python}-{abi}-{platform}.whl). Files of
// another project and other kinds of link are not versions.
func versionOfFilename(name, filename string) (string, bool) {
	if base, ok := strings.CutSuffix(filename, ".whl"); ok {
		parts := strings.Split(base, "-")
		if len(parts) < 5 || len(parts) > 6 || Normalize(parts[0]) != name || parts[1] == "" {
			return "", false
		}
		return parts[1], true
	}
	var stem string
	for _, ext := range []string{".tar.gz", ".zip", ".tgz", ".tar.bz2", ".tar.xz"} {
		if s, ok := strings.CutSuffix(filename, ext); ok {
			stem = s
			break
		}
	}
	// The project name may itself contain "-", so find the cut where the
	// left part normalizes to the project name.
	for i := range len(stem) {
		if stem[i] == '-' && i+1 < len(stem) && Normalize(stem[:i]) == name {
			rest := stem[i+1:]
			if !startsVersion(rest) {
				return "", false
			}
			return rest, true
		}
	}
	return "", false
}

// startsVersion reports whether s begins like a version: a digit (an epoch
// "1!2.0" starts with one too), or "v" and a digit. It keeps "bar-1.0" out of
// the files of project "foo" when the file is "foo-bar-1.0.tar.gz".
func startsVersion(s string) bool {
	s = strings.TrimPrefix(s, "v")
	return s != "" && s[0] >= '0' && s[0] <= '9'
}

// parseSimpleHTML reads the anchors of a PEP 503 page: the link text is the
// filename (the last segment of the href when the text is empty), and
// data-yanked marks a yanked file (its value is the reason). It is a small
// scanner, not an HTML parser: comments are skipped, tag and attribute names
// are case-insensitive, values may be quoted or bare, and a malformed anchor
// ends the scan with what was read so far.
func parseSimpleHTML(ctx context.Context, log *slog.Logger, body []byte) []simpleFile {
	src := string(body)
	low := asciiLower(src)
	var out []simpleFile
	for i := 0; i < len(src); {
		j := strings.IndexByte(src[i:], '<')
		if j < 0 {
			break
		}
		i += j
		if strings.HasPrefix(src[i:], "<!--") {
			end := strings.Index(src[i+4:], "-->")
			if end < 0 {
				break
			}
			i += 4 + end + 3
			continue
		}
		if !strings.HasPrefix(low[i:], "<a") || i+2 >= len(src) || !isSpace(src[i+2]) {
			i++
			continue
		}
		attrs, next, ok := parseAttrs(src, i+2)
		if !ok {
			log.DebugContext(ctx, "pypi: malformed anchor in the Simple API page, stopping the scan", "offset", i, "links", len(out))
			break
		}
		closeAt := indexAnchorClose(low[next:])
		if closeAt < 0 {
			closeAt = len(src) - next
		}
		text := strings.TrimSpace(html.UnescapeString(stripTags(src[next : next+closeAt])))
		i = next + closeAt
		href, hasHref := attrs["href"]
		if !hasHref {
			continue
		}
		if text == "" {
			text = filenameOfHref(href)
		}
		if text == "" {
			continue
		}
		f := simpleFile{Filename: text}
		if reason, yanked := attrs["data-yanked"]; yanked {
			f.Yanked, f.Reason = true, reason
		}
		out = append(out, f)
	}
	return out
}

// parseAttrs reads the attributes of a start tag from src[i:] up to its ">".
// Names are lower-cased, values unescaped; a name without a value maps to "".
// ok is false when the tag does not end (an unterminated quote or no ">").
func parseAttrs(src string, i int) (attrs map[string]string, next int, ok bool) {
	attrs = map[string]string{}
	for i < len(src) {
		for i < len(src) && (isSpace(src[i]) || src[i] == '/') {
			i++
		}
		if i >= len(src) {
			return nil, 0, false
		}
		if src[i] == '>' {
			return attrs, i + 1, true
		}
		start := i
		for i < len(src) && !isSpace(src[i]) && src[i] != '=' && src[i] != '>' && src[i] != '/' {
			i++
		}
		name := asciiLower(src[start:i])
		for i < len(src) && isSpace(src[i]) {
			i++
		}
		if i >= len(src) || src[i] != '=' {
			attrs[name] = ""
			continue
		}
		i++
		for i < len(src) && isSpace(src[i]) {
			i++
		}
		if i >= len(src) {
			return nil, 0, false
		}
		var val string
		if q := src[i]; q == '"' || q == '\'' {
			end := strings.IndexByte(src[i+1:], q)
			if end < 0 {
				return nil, 0, false
			}
			val = src[i+1 : i+1+end]
			i += end + 2
		} else {
			start := i
			for i < len(src) && !isSpace(src[i]) && src[i] != '>' {
				i++
			}
			val = src[start:i]
		}
		if _, dup := attrs[name]; !dup {
			attrs[name] = html.UnescapeString(val)
		}
	}
	return nil, 0, false
}

// indexAnchorClose returns the index of "</a>" or "</a" followed by white
// space, not of "</abbr>", or -1.
func indexAnchorClose(low string) int {
	for off := 0; ; {
		i := strings.Index(low[off:], "</a")
		if i < 0 {
			return -1
		}
		i += off
		if j := i + 3; j >= len(low) || low[j] == '>' || low[j] == '/' || isSpace(low[j]) {
			return i
		}
		off = i + 3
	}
}

func filenameOfHref(href string) string {
	href, _, _ = strings.Cut(href, "#")
	href, _, _ = strings.Cut(href, "?")
	if i := strings.LastIndexByte(href, '/'); i >= 0 {
		href = href[i+1:]
	}
	if u, err := url.PathUnescape(href); err == nil {
		return u
	}
	return href
}

// stripTags drops any markup inside an anchor's text.
func stripTags(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '<')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		j := strings.IndexByte(s[i:], '>')
		if j < 0 {
			return b.String()
		}
		s = s[i+j+1:]
	}
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' }

// asciiLower lower-cases ASCII letters only, so the result has the same
// length as s and indexes line up.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
