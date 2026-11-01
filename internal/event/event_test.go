package event

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/policy"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/version"
)

const (
	watchID = "01JBX1Z3M4N5P6Q7R8S9T0V1W2"
	eventID = "01JBX2A7C8D9E0F1G2H3J4K5M6"
)

var (
	detected = time.Date(2026, 10, 30, 17, 0, 0, 0, time.UTC)
	emitted  = detected.Add(2512*time.Millisecond + 999*time.Microsecond)
)

func target(kind string, coords map[string]string) store.Target {
	return store.Target{ID: "t1", Kind: kind, Coordinates: coords}
}

// validEvent fails the test unless env, marshalled, validates against the
// vendored envelope schema (which references the data schema) and its data
// against the data schema.
func validEvent(t *testing.T, s *schemaSet, env Envelope) map[string]any {
	t.Helper()
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return validJSON(t, s, b)
}

func validJSON(t *testing.T, s *schemaSet, b []byte) map[string]any {
	t.Helper()
	v := decode(t, b).(map[string]any)
	if errs := s.validate(envelopeSchemaID, v); len(errs) > 0 {
		t.Fatalf("envelope fails the spec schema: %v\n%s", errs, b)
	}
	if errs := s.validate(dataSchemaID, v["data"]); len(errs) > 0 {
		t.Fatalf("data fails the spec schema: %v\n%s", errs, b)
	}
	return v
}

// The architecture.md §4 example, built from a policy release.
func TestBuildPyPIExample(t *testing.T) {
	s := loadSchemas(t)
	env, err := Build(Input{
		ID: eventID, Instance: "angelia",
		Target: target("pypi", map[string]string{"name": "Requests"}),
		Watch:  store.Watch{ID: watchID, Name: "requests", Labels: map[string]string{"team": "platform", "repo": "api"}},
		Release: policy.Release{
			Raw: "2.33.0", Normalized: "2.33.0", Scheme: "pep440", Previous: "2.32.5", Bump: version.BumpMinor,
			PublishedAt: time.Date(2026, 10, 27, 14, 2, 11, 0, time.UTC), DetectedAt: detected,
			URL: "https://pypi.org/project/requests/2.33.0/", NotesURL: "https://github.com/psf/requests/blob/main/HISTORY.md",
			Metadata:            map[string]any{"project": "requests", "project_name": "requests"},
			CooldownSatisfiedAt: time.Date(2026, 10, 30, 14, 2, 11, 0, time.UTC),
		},
		Time: emitted,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(env)
	want := `{"specversion":"1.0","type":"dev.angelia.release.published.v1",` +
		`"source":"angelia://angelia/watches/01JBX1Z3M4N5P6Q7R8S9T0V1W2","id":"01JBX2A7C8D9E0F1G2H3J4K5M6",` +
		`"subject":"pypi:requests@2.33.0","time":"2026-10-30T17:00:02.512Z","datacontenttype":"application/json",` +
		`"dataschema":"https://github.com/angelia-dev/spec/schemas/release/v1.json","data":{"source":"pypi",` +
		`"package":"requests","version":"2.33.0","normalized_version":"2.33.0","previous_version":"2.32.5",` +
		`"scheme":"pep440","bump":"minor","backport":false,"prerelease":false,"yanked":false,` +
		`"published_at":"2026-10-27T14:02:11Z","detected_at":"2026-10-30T17:00:00Z",` +
		`"cooldown_satisfied_at":"2026-10-30T14:02:11Z","urls":{"release":"https://pypi.org/project/requests/2.33.0/",` +
		`"changelog":"https://github.com/psf/requests/blob/main/HISTORY.md","artifact":null},"notes":null,` +
		`"watch":{"id":"01JBX1Z3M4N5P6Q7R8S9T0V1W2","name":"requests","labels":{"repo":"api","team":"platform"}},` +
		`"purl":"pkg:pypi/requests@2.33.0"}}`
	if string(got) != want {
		t.Fatalf("event =\n%s\nwant\n%s", got, want)
	}
	validEvent(t, s, env)
}

// Acceptance criterion 1 of #106: generated events validate against the
// spec schema, for every kind and with every nullable field null.
func TestBuildValidatesAgainstSpecSchema(t *testing.T) {
	s := loadSchemas(t)
	base := policy.Release{Raw: "1.0.0", Normalized: "1.0.0", Scheme: "semver", DetectedAt: detected}
	cases := []struct {
		name      string
		tgt       store.Target
		watch     store.Watch
		rel       func(r *policy.Release)
		typ, purl string // purl "" means null
		subject   string
	}{
		{
			name: "first version, nothing known, no name, nil labels", tgt: target("pypi", map[string]string{"name": "Foo_Bar"}),
			typ: TypePublished, subject: "pypi:foo-bar@1.0.0",
		},
		{
			name: "npm scoped pre-release with tarball", tgt: target("npm", map[string]string{"name": "@angular/core"}),
			rel: func(r *policy.Release) {
				r.Raw, r.Normalized, r.Prerelease, r.Previous, r.Bump = "18.0.0-rc.1", "18.0.0-rc.1", true, "18.0.0-rc.0", version.BumpPrerelease
				r.Metadata = map[string]any{"tarball": "https://registry.npmjs.org/@angular/core/-/core-18.0.0-rc.1.tgz"}
			},
			typ: TypePrerelease, purl: "pkg:npm/%40angular/core@18.0.0-rc.1", subject: "npm:@angular/core@18.0.0-rc.1",
		},
		{
			name: "github release tag", tgt: target("github-release", map[string]string{"repo": "cli/cli"}),
			rel: func(r *policy.Release) { r.Raw = "v2.62.0"; r.URL = "https://github.com/cli/cli/releases/tag/v2.62.0" },
			typ: TypePublished, purl: "pkg:github/cli/cli@v2.62.0", subject: "github-release:cli/cli@v2.62.0",
		},
		{
			name: "crates backport", tgt: target("crates", map[string]string{"name": "Serde"}),
			rel: func(r *policy.Release) {
				r.Raw, r.Backport, r.Previous, r.Bump = "1.0.215", true, "1.0.214", version.BumpPatch
			},
			typ: TypePublished, purl: "pkg:cargo/serde@1.0.215", subject: "crates:serde@1.0.215",
		},
		{
			name: "plugin kind, no normalized form, bad URL dropped", tgt: target("my-plugin", map[string]string{"name": "thing"}),
			watch: store.Watch{ID: watchID, Name: "w", Labels: map[string]string{"a.b/c": "x y"}},
			rel: func(r *policy.Release) {
				r.Normalized, r.Scheme = "", "lexical"
				r.URL, r.NotesURL = "ftp://example.org/x", "https://example.org/has space"
			},
			typ: TypePublished, subject: "my-plugin:thing@1.0.0",
		},
		{
			name: "gitlab numeric project id", tgt: target("gitlab-release", map[string]string{"project": "278964"}),
			typ: TypePublished, subject: "gitlab-release:278964@1.0.0",
		},
		{
			name: "sub-millisecond times", tgt: target("npm", map[string]string{"name": "left-pad"}),
			rel: func(r *policy.Release) {
				r.PublishedAt = time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("x", 3600))
				r.DetectedAt = detected.Add(time.Microsecond)
				r.CooldownSatisfiedAt = detected.Add(-time.Nanosecond)
			},
			typ: TypePublished, purl: "pkg:npm/left-pad@1.0.0", subject: "npm:left-pad@1.0.0",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := base
			if c.rel != nil {
				c.rel(&r)
			}
			w := c.watch
			if w.ID == "" {
				w.ID = watchID
			}
			env, err := Build(Input{Instance: "ci.example-1", Target: c.tgt, Watch: w, Release: r, Time: emitted})
			if err != nil {
				t.Fatal(err)
			}
			v := validEvent(t, s, env)
			if env.Type != c.typ || env.Subject != c.subject || env.Source != "angelia://ci.example-1/watches/"+watchID {
				t.Errorf("type, subject, source = %s, %s, %s", env.Type, env.Subject, env.Source)
			}
			if got := v["data"].(map[string]any)["purl"]; (c.purl == "" && got != nil) || (c.purl != "" && got != c.purl) {
				t.Errorf("purl = %v, want %q", got, c.purl)
			}
			if env.Data.Watch.Labels == nil || (w.Name == "") != (env.Data.Watch.Name == nil) {
				t.Errorf("watch = %+v", env.Data.Watch)
			}
			if (r.Previous == "") != (env.Data.Bump == nil && env.Data.PreviousVersion == nil) {
				t.Errorf("previous %v, bump %v", env.Data.PreviousVersion, env.Data.Bump)
			}
		})
	}
}

func TestBuildIDsAndTime(t *testing.T) {
	r := policy.Release{Raw: "1.0.0", Normalized: "1.0.0", Scheme: "semver", DetectedAt: detected}
	in := Input{
		Instance: "angelia", Target: target("npm", map[string]string{"name": "x"}),
		Watch: store.Watch{ID: watchID}, Release: r, Time: emitted,
	}
	a, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Build(in)
	if a.ID == b.ID || a.ID[:10] != b.ID[:10] || a.Time != "2026-10-30T17:00:02.512Z" {
		t.Errorf("ids %s %s, time %s: want distinct ULIDs of the emission time", a.ID, b.ID, a.Time)
	}
	if a.Data.Notes != nil || a.Data.Yanked {
		t.Error("notes must be null and yanked false")
	}
}

func TestBuildRejectsInputsTheSchemaRejects(t *testing.T) {
	ok := Input{
		Instance: "angelia", Target: target("npm", map[string]string{"name": "x"}), Watch: store.Watch{ID: watchID},
		Release: policy.Release{Raw: "1", DetectedAt: detected}, Time: emitted,
	}
	for name, mod := range map[string]func(*Input){
		"instance upper case": func(in *Input) { in.Instance = "Angelia" },
		"instance empty":      func(in *Input) { in.Instance = "" },
		"watch id not ULID":   func(in *Input) { in.Watch.ID = "w1" },
		"no package":          func(in *Input) { in.Target.Coordinates = nil },
		"blank package":       func(in *Input) { in.Target.Coordinates = map[string]string{"name": " "} },
		"no version":          func(in *Input) { in.Release.Raw = "" },
		"no detection time":   func(in *Input) { in.Release.DetectedAt = time.Time{} },
		"no emission time":    func(in *Input) { in.Time = time.Time{} },
	} {
		in := ok
		mod(&in)
		if _, err := Build(in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := Build(ok); err != nil {
		t.Fatal(err)
	}
}

// Acceptance criterion 3 of #106: purl per kind, from the spec's table.
func TestPURL(t *testing.T) {
	pyName := func(n string) map[string]any { return map[string]any{"project_name": n} }
	for _, c := range []struct {
		kind, pkg, raw string
		md             map[string]any
		registry       string
		want           string // "" means nil
	}{
		// pypi: the name as PyPI reports it, lower case, "_" → "-", "." kept.
		{"pypi", "requests", "2.33.0", pyName("requests"), "", "pkg:pypi/requests@2.33.0"},
		{"pypi", "typing-extensions", "4.12.2", pyName("typing_extensions"), "", "pkg:pypi/typing-extensions@4.12.2"},
		{"pypi", "zope-interface", "7.1.1", pyName("zope.interface"), "", "pkg:pypi/zope.interface@7.1.1"},
		{"pypi", "django", "5.1", pyName("Django"), "", "pkg:pypi/django@5.1"},
		{"pypi", "a-b", "1!2.0+local.1", pyName("A__B"), "", "pkg:pypi/a--b@1%212.0%2Blocal.1"},
		{"pypi", "requests", "2.33.0", nil, "", ""}, // name not reported: not guessed
		{"pypi", "requests", "1.0", pyName("requests"), "https://mirror.example.org/pypi", "pkg:pypi/requests@1.0?repository_url=https://mirror.example.org/pypi"},
		// npm: the scope is the namespace, "@" encoded.
		{"npm", "@angular/core", "17.3.0", nil, "", "pkg:npm/%40angular/core@17.3.0"},
		{"npm", "left-pad", "1.3.0", nil, "", "pkg:npm/left-pad@1.3.0"},
		{"npm", "x", "1.0.0+build.5", nil, "", "pkg:npm/x@1.0.0%2Bbuild.5"},
		{"crates", "serde", "1.0.215", nil, "", "pkg:cargo/serde@1.0.215"},
		// github: owner and repo lower-cased, version the tag.
		{"github-release", "cli/cli", "v2.62.0", nil, "", "pkg:github/cli/cli@v2.62.0"},
		{"github-release", "BurntSushi/ripgrep", "14.1.1", nil, "", "pkg:github/burntsushi/ripgrep@14.1.1"},
		{"github-tag", "golang/go", "go1.23.3", nil, "", "pkg:github/golang/go@go1.23.3"},
		{"github-release", "o/r", "release/1.0", nil, "", "pkg:github/o/r@release%2F1.0"},
		{"gitlab-release", "gitlab-org/gitlab-runner", "v17.5.0", nil, "", "pkg:gitlab/gitlab-org/gitlab-runner@v17.5.0"},
		{"gitlab-release", "Group/Sub/Proj", "v1", nil, "https://gitlab.example.org", "pkg:gitlab/group/sub/proj@v1?repository_url=https://gitlab.example.org"},
		{"gitlab-release", "278964", "v1", nil, "", ""},
		{"oci", "docker.io/library/node", "22.11.0", nil, "", "pkg:oci/node?repository_url=docker.io/library/node&tag=22.11.0"},
		{"go-proxy", "github.com/BurntSushi/toml", "v1.4.0", nil, "", "pkg:golang/github.com/BurntSushi/toml@v1.4.0"},
		{"maven", "org.slf4j:slf4j-api", "2.0.16", nil, "", "pkg:maven/org.slf4j/slf4j-api@2.0.16"},
		{"maven", "no-artifact", "1", nil, "", ""},
		{"http-json", "x", "1", nil, "", ""},
		{"http-regex", "x", "1", nil, "", ""},
		{"my-plugin", "x", "1", nil, "", ""},
	} {
		got := PURL(c.kind, c.pkg, c.raw, c.md, c.registry)
		if (got == nil) != (c.want == "") || (got != nil && *got != c.want) {
			g := "<nil>"
			if got != nil {
				g = *got
			}
			t.Errorf("PURL(%s, %s, %s) = %s, want %q", c.kind, c.pkg, c.raw, g, c.want)
		}
	}
}

func TestPackage(t *testing.T) {
	for _, c := range []struct {
		kind   string
		coords map[string]string
		want   string
	}{
		{"pypi", map[string]string{"name": "Zope.Interface"}, "zope-interface"},
		{"crates", map[string]string{"name": "Serde"}, "serde"},
		{"npm", map[string]string{"name": "@angular/core"}, "@angular/core"},
		{"github-tag", map[string]string{"repo": "golang/go"}, "golang/go"},
		{"gitlab-release", map[string]string{"project": "a/b/c"}, "a/b/c"},
		{"go-proxy", map[string]string{"module": "github.com/gorilla/mux"}, "github.com/gorilla/mux"},
		{"my-plugin", map[string]string{"zone": "b", "area": "a"}, "a/b"},
		{"my-plugin", nil, ""},
	} {
		if got := Package(c.kind, c.coords); got != c.want {
			t.Errorf("Package(%s, %v) = %q, want %q", c.kind, c.coords, got, c.want)
		}
	}
}

func TestNewNotes(t *testing.T) {
	short := NewNotes("markdown", "## 1.0")
	if short.Body != "## 1.0" || short.Truncated {
		t.Errorf("short notes = %+v", short)
	}
	exact := NewNotes("text", strings.Repeat("a", MaxNotesBytes))
	if len(exact.Body) != MaxNotesBytes || exact.Truncated {
		t.Errorf("notes of exactly the limit: %d bytes, truncated %v", len(exact.Body), exact.Truncated)
	}
	// "é" is two bytes; the limit falls inside the last one.
	long := NewNotes("text", strings.Repeat("a", MaxNotesBytes-1)+"éé")
	if !long.Truncated || len(long.Body) != MaxNotesBytes-1 || !utf8.ValidString(long.Body) {
		t.Errorf("cut notes: %d bytes, truncated %v, valid %v", len(long.Body), long.Truncated, utf8.ValidString(long.Body))
	}
}

func TestShortType(t *testing.T) {
	for typ, want := range map[string]string{
		TypePublished: "release.published", TypePrerelease: "release.prerelease", TypeYanked: "release.yanked",
	} {
		if got := ShortType(typ); got != want {
			t.Errorf("ShortType(%s) = %s", typ, got)
		}
	}
}
