package manifest

import (
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/source/pypi"
)

// MaxIncludeDepth is how deep `-r` includes may nest (the first file is 0).
const MaxIncludeDepth = 8

var (
	commentRE = regexp.MustCompile(`(^|\s)#.*$`)
	pyNameRE  = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?`)
	// Per-requirement options that follow the requirement on its line.
	reqOptRE = regexp.MustCompile(`\s--?[A-Za-z]`)
)

// optionKind classifies a pip option line (docs/cli.md, "Import").
type optionKind int

const (
	optUnknown optionKind = iota
	optInclude
	optConstraint
	optEditable
	optIndex
	optSilent
)

var requirementOptions = map[string]optionKind{
	"-r": optInclude, "--requirement": optInclude,
	"-c": optConstraint, "--constraint": optConstraint,
	"-e": optEditable, "--editable": optEditable,
	"-i": optIndex, "--index-url": optIndex, "--extra-index-url": optIndex,
	"-f": optIndex, "--find-links": optIndex, "--trusted-host": optIndex,
	"--hash": optSilent, "--pre": optSilent, "--no-binary": optSilent,
	"--only-binary": optSilent, "--prefer-binary": optSilent,
}

// ParseRequirements reads the requirements file name from fsys, and the files
// it includes with -r. dev marks every dependency of the file (and of its
// includes) as a dev dependency. The grammar is the documented subset of
// pip's: docs/cli.md, "Import".
func ParseRequirements(fsys fs.FS, name string, dev bool) (*Result, error) {
	p := &reqParser{fsys: fsys, dev: dev, res: &Result{}, done: map[string]bool{}}
	if err := p.file(path.Clean(name), 0, nil, Origin{}); err != nil {
		return nil, err
	}
	return p.res, nil
}

type reqParser struct {
	fsys fs.FS
	dev  bool
	res  *Result
	done map[string]bool // files already parsed, by cleaned path
}

func (p *reqParser) warn(o Origin, skip bool, format string, a ...any) {
	p.res.Warnings = append(p.res.Warnings, Warning{Origin: o, Skip: skip, Msg: fmt.Sprintf(format, a...)})
}

// file parses one requirements file. stack holds the files being included,
// from is the -r line that includes it (zero for the first file).
func (p *reqParser) file(name string, depth int, stack []string, from Origin) error {
	at := ""
	if from.File != "" {
		at = from.String() + ": "
	}
	if depth > MaxIncludeDepth {
		return fmt.Errorf("%s%s: -r includes nest deeper than %d files", at, name, MaxIncludeDepth)
	}
	for _, s := range stack {
		if s == name {
			return fmt.Errorf("%sinclude cycle: %s -> %s", at, strings.Join(stack, " -> "), name)
		}
	}
	if p.done[name] { // included twice: read once
		return nil
	}
	stack = append(stack, name)
	raw, err := readFile(p.fsys, name)
	if err != nil {
		return fmt.Errorf("%s%w", at, err)
	}
	p.done[name] = true
	indexWarned := false
	for _, ll := range logicalLines(strings.TrimPrefix(string(raw), "\ufeff")) {
		line := strings.TrimSpace(commentRE.ReplaceAllString(ll.text, ""))
		if line == "" {
			continue
		}
		o := Origin{File: name, Line: ll.line}
		if strings.HasPrefix(line, "-") {
			if err := p.option(o, line, depth, stack, &indexWarned); err != nil {
				return err
			}
			continue
		}
		p.requirement(o, line)
	}
	return nil
}

type logicalLine struct {
	text string
	line int // first physical line
}

// logicalLines joins lines that end in a backslash.
func logicalLines(s string) []logicalLine {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var out []logicalLine
	var cur strings.Builder
	start := 0
	for i, l := range strings.Split(s, "\n") {
		if cur.Len() == 0 {
			start = i + 1
		}
		if strings.HasSuffix(l, "\\") && !strings.HasPrefix(strings.TrimSpace(l), "#") { // a comment line never continues
			cur.WriteString(strings.TrimSuffix(l, "\\"))
			continue
		}
		cur.WriteString(l)
		out = append(out, logicalLine{text: cur.String(), line: start})
		cur.Reset()
	}
	if cur.Len() > 0 { // a backslash on the last line
		out = append(out, logicalLine{text: cur.String(), line: start})
	}
	return out
}

// splitOption splits "-r file", "--requirement=file" and "-rfile" into the
// option and its value.
func splitOption(line string) (opt, val string) {
	if i := strings.IndexAny(line, " \t="); i >= 0 && strings.HasPrefix(line, "--") {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	if strings.HasPrefix(line, "--") {
		return line, ""
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i], strings.TrimSpace(line[i+1:])
	}
	if len(line) > 2 { // -rfile
		return line[:2], line[2:]
	}
	return line, ""
}

func (p *reqParser) option(o Origin, line string, depth int, stack []string, indexWarned *bool) error {
	opt, val := splitOption(line)
	switch requirementOptions[opt] {
	case optInclude:
		if val == "" {
			return fmt.Errorf("%s: %s needs a file", o, opt)
		}
		if i := strings.Index(val, "://"); i > 0 && i <= 6 {
			return fmt.Errorf("%s: %s %s: includes are read from the file system only, angelia import makes no network request", o, opt, val)
		}
		if strings.Contains(val, "${") {
			return fmt.Errorf("%s: %s %s: variables are not expanded", o, opt, val)
		}
		inc := val
		if filepath.IsAbs(inc) {
			inc = filepath.ToSlash(inc)
		} else if !path.IsAbs(inc) {
			inc = path.Join(path.Dir(o.File), inc)
		}
		inc = path.Clean(inc)
		return p.file(inc, depth+1, stack, o)
	case optConstraint:
		p.warn(o, true, "skipped %s: constraint files pin versions, they do not declare dependencies", clip(line))
	case optEditable:
		p.warn(o, true, "skipped %s: editable requirements are not registry dependencies", clip(line))
	case optIndex:
		if !*indexWarned {
			*indexWarned = true
			p.warn(o, false, "%s ignored (and the other index options of this file): angelia import reads no index settings; map the PyPI watches to a named source with --source pypi=NAME", opt)
		}
	case optSilent:
	default:
		p.warn(o, true, "skipped unknown option %s", clip(opt))
	}
	return nil
}

// requirement parses one PEP 508 requirement line.
func (p *reqParser) requirement(o Origin, line string) {
	if loc := reqOptRE.FindStringIndex(line); loc != nil { // --hash=... and friends
		line = strings.TrimSpace(line[:loc[0]])
	}
	end := strings.IndexAny(line, " \t[<>=!~;@(,")
	if end < 0 {
		end = len(line)
	}
	token := line[:end]
	switch {
	case strings.Contains(token, "$"):
		p.warn(o, true, "skipped %q: the name holds a variable, which angelia import does not expand", clip(line))
		return
	case strings.Contains(line, "://") || hasVCSPrefix(token):
		p.warn(o, true, "skipped %q: URL and VCS requirements are not registry dependencies", clip(line))
		return
	case strings.ContainsAny(token, `/\`) || strings.HasPrefix(token, ".") || strings.HasPrefix(token, "~") ||
		strings.HasSuffix(token, ".whl") || strings.HasSuffix(token, ".zip") || strings.HasSuffix(token, ".tar.gz"):
		p.warn(o, true, "skipped %q: a local path is not a registry dependency", clip(line))
		return
	}
	name := pyNameRE.FindString(line)
	if name == "" {
		p.warn(o, true, "skipped %q: not a requirement", clip(line))
		return
	}
	rest := strings.TrimSpace(line[len(name):])
	if strings.HasPrefix(rest, "[") { // extras are ignored
		i := strings.Index(rest, "]")
		if i < 0 {
			p.warn(o, true, "skipped %q: unterminated extras", clip(line))
			return
		}
		rest = strings.TrimSpace(rest[i+1:])
	}
	if strings.HasPrefix(rest, "@") {
		p.warn(o, true, "skipped %q: direct references are not registry dependencies", clip(line))
		return
	}
	if rest != "" && !strings.ContainsAny(rest[:1], "<>=!~(;") {
		p.warn(o, true, "skipped %q: cannot read the version specifier", clip(line))
		return
	}
	spec, _, _ := strings.Cut(rest, ";") // the marker is ignored
	typ := TypeRuntime
	if p.dev {
		typ = TypeDev
	}
	p.res.Deps = append(p.res.Deps, Dependency{Name: name, Kind: KindPyPI, Type: typ, Pin: pyPin(spec), Origin: o})
}

func hasVCSPrefix(token string) bool {
	for _, p := range []string{"git+", "hg+", "svn+", "bzr+", "file:"} {
		if strings.HasPrefix(token, p) {
			return true
		}
	}
	return false
}

// pyPin returns the version of an exact `==` clause of a specifier.
func pyPin(spec string) string {
	spec = strings.Trim(strings.TrimSpace(spec), "()")
	for _, c := range strings.Split(spec, ",") {
		c = strings.TrimSpace(c)
		for _, op := range []string{"===", "=="} {
			if v, ok := strings.CutPrefix(c, op); ok {
				v = strings.TrimSpace(v)
				if v != "" && !strings.Contains(v, "*") {
					return v
				}
			}
		}
	}
	return ""
}

// canonicalPyPI is the stored coordinate of a PyPI dependency.
func canonicalPyPI(name string) string { return pypi.Normalize(name) }

// clip shortens a line echoed in a warning, so that a file that is not a
// requirements file is not dumped into logs.
func clip(s string) string {
	const limit = 80
	n := limit
	if len(s) <= limit {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
