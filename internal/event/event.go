package event

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/source/crates"
	"github.com/angelia-dev/angelia/internal/source/pypi"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// CloudEvents attributes of the release events (spec docs/release-event-v1.md).
const (
	SpecVersion     = "1.0"
	DataContentType = "application/json"
	// DataSchema is the $id of the spec's schemas/release/v1.json.
	DataSchema = "https://github.com/angelia-dev/spec/schemas/release/v1.json"

	TypePublished  = "dev.angelia.release.published.v1"
	TypePrerelease = "dev.angelia.release.prerelease.v1"
	TypeYanked     = "dev.angelia.release.yanked.v1"

	// MaxNotesBytes bounds notes.body, in bytes of UTF-8.
	MaxNotesBytes = 16384
)

// ShortType returns the short type that subscription filters use: the type
// without "dev.angelia." and ".v<major>" ("release.published").
func ShortType(typ string) string {
	s := strings.TrimPrefix(typ, "dev.angelia.")
	if i := strings.LastIndex(s, ".v"); i > 0 {
		s = s[:i]
	}
	return s
}

// Envelope is a release event in CloudEvents 1.0 JSON structured mode, as
// the spec's envelope profile defines it. It has no extension attributes.
// Marshalled with encoding/json it is the event's wire form.
type Envelope struct {
	SpecVersion     string `json:"specversion"`
	Type            string `json:"type"`
	Source          string `json:"source"`
	ID              string `json:"id"`
	Subject         string `json:"subject"`
	Time            string `json:"time"`
	DataContentType string `json:"datacontenttype"`
	DataSchema      string `json:"dataschema"`
	Data            Data   `json:"data"`
}

// Data is the event payload, schemas/release/v1.json. Every field is always
// written: a nil pointer is JSON null, never an omitted key. Timestamps are
// RFC 3339 strings in UTC with at most millisecond precision.
type Data struct {
	Source              string  `json:"source"`
	Package             string  `json:"package"`
	Version             string  `json:"version"`
	NormalizedVersion   string  `json:"normalized_version"`
	PreviousVersion     *string `json:"previous_version"`
	Scheme              string  `json:"scheme"`
	Bump                *string `json:"bump"`
	Backport            bool    `json:"backport"`
	Prerelease          bool    `json:"prerelease"`
	Yanked              bool    `json:"yanked"`
	PublishedAt         *string `json:"published_at"`
	DetectedAt          string  `json:"detected_at"`
	CooldownSatisfiedAt *string `json:"cooldown_satisfied_at"`
	URLs                URLs    `json:"urls"`
	Notes               *Notes  `json:"notes"`
	Watch               Watch   `json:"watch"`
	PURL                *string `json:"purl"`
}

// URLs are the links of a release; nil is unknown.
type URLs struct {
	Release   *string `json:"release"`
	Changelog *string `json:"changelog"`
	Artifact  *string `json:"artifact"`
}

// Notes is the release notes text. No source fetches notes yet (PRD F12), so
// events carry null notes; NewNotes is the constructor to use then.
type Notes struct {
	Format    string `json:"format"`
	Body      string `json:"body"`
	Truncated bool   `json:"truncated"`
}

// NewNotes returns notes whose body is cut to MaxNotesBytes at a UTF-8
// character boundary, Truncated telling whether it was cut.
func NewNotes(format, body string) *Notes {
	n := &Notes{Format: format, Body: body}
	if len(body) <= MaxNotesBytes {
		return n
	}
	cut := MaxNotesBytes
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	n.Body, n.Truncated = body[:cut], true
	return n
}

// encodePayload marshals env into the stored payload of its event. A NUL
// character in any string, which json.Marshal writes as the escape \u0000,
// becomes U+FFFD (written \ufffd), the character json.Marshal already puts
// in place of invalid UTF-8. Postgres refuses \u0000 in a document that its
// subject index reads (ADR 0028 §7), so both engines store the same, valid
// payload. A registry string with a NUL is malformed anyway.
func encodePayload(env Envelope) ([]byte, error) {
	b, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("event: encode %s: %w", env.ID, err)
	}
	return replaceEscapedNUL(b), nil
}

// replaceEscapedNUL replaces every \u0000 escape of the JSON text b with
// \ufffd. It walks escapes pair by pair, so an escaped backslash followed by
// the text "u0000" is left alone.
func replaceEscapedNUL(b []byte) []byte {
	const nul, repl = `\u0000`, `\ufffd`
	if !bytes.Contains(b, []byte(nul)) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] != '\\':
			out = append(out, b[i])
		case bytes.HasPrefix(b[i:], []byte(nul)):
			out = append(out, repl...)
			i += len(nul) - 1
		case i+1 < len(b):
			out = append(out, b[i], b[i+1]) // any other escape pair
			i++
		default:
			out = append(out, b[i])
		}
	}
	return out
}

// subjectKey is the subject as encodePayload stores it, for lookups by
// subject.
func subjectKey(subject string) string { return strings.ReplaceAll(subject, "\x00", "\uFFFD") }

// Watch identifies the watch that produced the event.
type Watch struct {
	ID     string            `json:"id"`
	Name   *string           `json:"name"`
	Labels map[string]string `json:"labels"`
}

// Input is what Build needs for one event.
type Input struct {
	// ID is the event id, a ULID; empty means a new one for Time.
	ID string
	// Instance is the configuration's instance.name (config.Instance.Name).
	Instance string
	// Target is the watch's target: Kind and Coordinates are read.
	Target store.Target
	// RegistryURL is the configured sources.<kind>.base_url when the watch
	// uses a non-default registry, "" for the public one. It becomes the
	// purl's repository_url qualifier.
	RegistryURL string
	Watch       store.Watch
	// Release is what the watch policy emitted (policy.Decision.Emit).
	Release policy.Release
	// Time is when the event is emitted: the end of the check.
	Time time.Time
}

var (
	instanceRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)
	urlRE      = regexp.MustCompile(`^https?://[^\s/?#]+([/?#]\S*)?$`)
	// tokenRE is the spec's open set token (data.source, data.scheme).
	tokenRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// labelKeyRE and the limits below are the spec's watch.labels rules.
	labelKeyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`)

	// ErrInvalid is wrapped by Build errors: no event valid under the spec can
	// be built from the input.
	ErrInvalid = errors.New("event: invalid input")
)

// Spec limits of watch.labels.
const (
	maxLabels        = 32
	maxLabelValueLen = 253
)

// Build maps a release a watch emits to its event: release.prerelease when
// the version is a pre-release, release.published otherwise. It does no I/O.
// An input from which no event valid under the spec can be built is an error
// wrapping ErrInvalid: an instance name, watch id, source kind or scheme of
// the wrong form, a blank package, no version or time, or labels outside the
// spec's limits.
func Build(in Input) (Envelope, error) {
	r := in.Release
	kind := in.Target.Kind
	pkg := Package(kind, in.Target.Coordinates)
	switch {
	case !instanceRE.MatchString(in.Instance):
		return Envelope{}, fmt.Errorf("%w: instance name %q", ErrInvalid, in.Instance)
	case !ulid.Valid(in.Watch.ID):
		return Envelope{}, fmt.Errorf("%w: watch id %q is not a ULID", ErrInvalid, in.Watch.ID)
	case !tokenRE.MatchString(kind):
		return Envelope{}, fmt.Errorf("%w: source kind %q", ErrInvalid, kind)
	case blank(pkg):
		return Envelope{}, fmt.Errorf("%w: target %s has no package name", ErrInvalid, in.Target.ID)
	case r.Raw == "" || r.DetectedAt.IsZero() || in.Time.IsZero():
		return Envelope{}, fmt.Errorf("%w: release without version, detection or emission time", ErrInvalid)
	case !tokenRE.MatchString(r.Scheme):
		return Envelope{}, fmt.Errorf("%w: scheme %q", ErrInvalid, r.Scheme)
	}
	if err := checkLabels(in.Watch.Labels); err != nil {
		return Envelope{}, err
	}
	id := in.ID
	if id == "" {
		id = ulid.New(in.Time)
	}
	typ := TypePublished
	if r.Prerelease {
		typ = TypePrerelease
	}
	normalized := r.Normalized
	if normalized == "" {
		normalized = r.Raw // a version no scheme parsed: the lexical form
	}
	var name *string
	if in.Watch.Name != "" {
		name = &in.Watch.Name
	}
	labels := maps.Clone(in.Watch.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	return Envelope{
		SpecVersion:     SpecVersion,
		Type:            typ,
		Source:          "angelia://" + in.Instance + "/watches/" + in.Watch.ID,
		ID:              id,
		Subject:         kind + ":" + pkg + "@" + r.Raw,
		Time:            timestamp(in.Time),
		DataContentType: DataContentType,
		DataSchema:      DataSchema,
		Data: Data{
			Source:              kind,
			Package:             pkg,
			Version:             r.Raw,
			NormalizedVersion:   normalized,
			PreviousVersion:     nonEmpty(r.Previous),
			Scheme:              r.Scheme,
			Bump:                nonEmpty(string(r.Bump)),
			Backport:            r.Backport,
			Prerelease:          r.Prerelease,
			Yanked:              false, // a version first seen yanked is never emitted
			PublishedAt:         optTimestamp(r.PublishedAt),
			DetectedAt:          timestamp(r.DetectedAt),
			CooldownSatisfiedAt: optTimestamp(r.CooldownSatisfiedAt),
			URLs: URLs{
				Release:   validURL(r.URL),
				Changelog: validURL(r.NotesURL),
				Artifact:  validURL(artifactURL(kind, r.Metadata)),
			},
			Notes: nil, // until notes enrichment (PRD F12)
			Watch: Watch{ID: in.Watch.ID, Name: name, Labels: labels},
			PURL:  PURL(kind, pkg, r.Raw, r.Metadata, in.RegistryURL),
		},
	}, nil
}

// packageKeys are the coordinates that name the package, by source kind.
var packageKeys = map[string]string{
	"npm": "name", "github-release": "repo", "github-tag": "repo", "gitlab-release": "project",
}

// fallbackKeys name the package of kinds without an entry in packageKeys
// (sources not built yet, plugins), tried in order.
var fallbackKeys = []string{"name", "package", "module", "image", "repo", "project"}

// Package returns data.package, the package name as the source normalised
// it: the PEP 503 name for pypi, the lower-case name for crates, owner/repo
// for GitHub, the project path for GitLab, the name for npm. Other kinds use
// the first of the coordinates name, package, module, image, repo, project;
// a target with none of them gets its coordinate values in key order,
// joined by "/".
func Package(kind string, coords map[string]string) string {
	switch kind {
	case pypi.Kind:
		return pypi.Normalize(coords["name"])
	case crates.Kind:
		return crates.Normalize(coords["name"])
	}
	if k, ok := packageKeys[kind]; ok {
		return coords[k]
	}
	for _, k := range fallbackKeys {
		if v := coords[k]; v != "" {
			return v
		}
	}
	var vals []string
	for _, k := range slices.Sorted(maps.Keys(coords)) {
		vals = append(vals, coords[k])
	}
	return strings.Join(vals, "/")
}

// artifactURL is the one obvious download of a release: the npm tarball.
func artifactURL(kind string, md map[string]any) string {
	if kind != "npm" {
		return ""
	}
	s, _ := md["tarball"].(string)
	return s
}

// timestamp formats t as the spec wants: UTC, Z, at most milliseconds.
func timestamp(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format(time.RFC3339Nano)
}

func optTimestamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := timestamp(t)
	return &s
}

func nonEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// isSpace is ECMA-262 \s, which the spec patterns use: Unicode white space
// plus U+FEFF. Go's regexp \s is ASCII only.
func isSpace(r rune) bool { return unicode.IsSpace(r) || r == '\uFEFF' }

// blank reports whether s has no character but white space (spec pattern \S).
func blank(s string) bool { return strings.IndexFunc(s, func(r rune) bool { return !isSpace(r) }) < 0 }

func checkLabels(labels map[string]string) error {
	if len(labels) > maxLabels {
		return fmt.Errorf("%w: %d labels, at most %d", ErrInvalid, len(labels), maxLabels)
	}
	for k, v := range labels {
		if !labelKeyRE.MatchString(k) {
			return fmt.Errorf("%w: label key %q", ErrInvalid, k)
		}
		if utf8.RuneCountInString(v) > maxLabelValueLen || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("%w: value of label %q", ErrInvalid, k)
		}
	}
	return nil
}

// validURL keeps an absolute http(s) URL without whitespace (ECMA \s) that
// net/url parses; anything else is unknown (null) rather than an event that
// fails the schema.
func validURL(s string) *string {
	if !urlRE.MatchString(s) || strings.ContainsFunc(s, isSpace) {
		return nil
	}
	if _, err := url.Parse(s); err != nil {
		return nil
	}
	return &s
}
