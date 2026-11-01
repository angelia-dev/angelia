package watchfile

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const base = "version: 1\n"

// AC: invalid entries fail with path and line.
func TestParseErrorsCarryFileLineAndPath(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string // substring of one error line
	}{
		{"empty file", "", "w.yaml: line 1: file is empty"},
		{"no version", "watches: []\n", "w.yaml: line 1: version: missing required key"},
		{"wrong version", "version: 2\n", "w.yaml: line 1: version: must be 1"},
		{"version is a string", "version: \"1\"\n", "w.yaml: line 1: version: must be 1"},
		{"unknown top key", base + "wachtes: []\n", `w.yaml: line 2: wachtes: unknown key "wachtes"`},
		{"unknown nested key", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    polcy: {}\n", `line 6: watches[0].polcy: unknown key`},
		{"duplicate key", base + "version: 1\n", "line 2: version: duplicate key"},
		{"multi document", base + "---\nversion: 1\n", "multiple YAML documents"},
		{"bad yaml", "version: [1\n", "invalid YAML"},
		{"not a mapping", "- a\n", "must be a mapping"},
		{"alias", base + "sinks: &s []\nwatches: *s\n", "line 3: watches: must be a list"},
		{"bad prerelease", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    policy:\n      prerelease: sometimes\n", "line 7: watches[0].policy.prerelease: must be one of none, only, include"},
		{"bad name", base + "watches:\n  - name: Bad Name\n    source: pypi\n    coordinates: {name: x}\n", "line 3: watches[0].name: invalid name"},
		{"missing name", base + "watches:\n  - source: pypi\n    coordinates: {name: x}\n", `line 3: watches[0]: missing required key "name"`},
		{"missing source", base + "watches:\n  - name: a\n    coordinates: {name: x}\n", `watches[0].source: missing required key "source"`},
		{"unknown source", base + "watches:\n  - name: a\n    source: cobol\n    coordinates: {name: x}\n", "line 4: watches[0].source: must be one of github-release, npm, pypi"},
		{"missing coordinates", base + "watches:\n  - name: a\n    source: pypi\n", `watches[0].coordinates: missing required key "coordinates"`},
		{"coordinates rejected by the source", base + "watches:\n  - name: a\n    source: github-release\n    coordinates: {repo: cli}\n", "line 5: watches[0].coordinates: coordinate repo must be owner/name"},
		{"coordinate not a string", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: 12}\n", "line 5: watches[0].coordinates.name: must be a string"},
		{"interval too short", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    interval: 1m\n", "line 6: watches[0].interval: must be between 5m0s and 720h0m0s"},
		{"interval unparsable", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    interval: soon\n", "watches[0].interval: invalid duration"},
		{"unknown scheme", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    scheme: maven\n", "line 6: watches[0].scheme: must be one of"},
		{"bad label key", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    labels: {'-bad': v}\n", "line 6: watches[0].labels.-bad: invalid label key"},
		{"label not a string", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    labels: {tier: 1}\n", "watches[0].labels.tier: must be a string"},
		{"bad regex", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    policy:\n      include: ['(']\n", "line 7: watches[0].policy.include[0]: invalid regular expression"},
		{"bad bump", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    policy:\n      bump: [major, huge]\n", "line 7: watches[0].policy.bump[1]: must be one of major, minor, patch"},
		{"bad baseline", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    policy:\n      baseline: maybe\n", "watches[0].policy.baseline: must be true or false"},
		{"cooldown too long", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    policy:\n      cooldown: 9000h\n", "watches[0].policy.cooldown: must be between"},
		{"duplicate watch name", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n  - name: a\n    source: pypi\n    coordinates: {name: y}\n", `line 6: watches[1].name: duplicate name "a" (first used on line 3)`},
		{"unknown sink kind", base + "sinks:\n  - name: s\n    kind: carrier-pigeon\n    config: {}\n", "line 4: sinks[0].kind: must be one of webhook"},
		{"webhook without url", base + "sinks:\n  - name: s\n    kind: webhook\n    config: {}\n", `sinks[0].config.url: missing required key "url"`},
		{"webhook http", base + "sinks:\n  - name: s\n    kind: webhook\n    config: {url: 'http://hook.example.org/x'}\n", "line 5: sinks[0].config.url: must be https://"},
		{"webhook userinfo", base + "sinks:\n  - name: s\n    kind: webhook\n    config: {url: 'https://u:p@hook.example.org/x'}\n", "credentials in the URL"},
		{"webhook secret in query", base + "sinks:\n  - name: s\n    kind: webhook\n    config: {url: 'https://hook.example.org/x?api_token=abc'}\n", "query string must not carry a secret"},
		{"webhook inline secret key", base + "sinks:\n  - name: s\n    kind: webhook\n    config: {url: 'https://hook.example.org/x', secret: hush}\n", `sinks[0].config.secret: unknown key "secret"`},
		{"secret_env holds the secret", base + "sinks:\n  - name: s\n    kind: webhook\n    config: {url: 'https://hook.example.org/x', secret_env: 'not a name!'}\n", "sinks[0].config.secret_env: must be an environment variable name"},
		{"webhook timeout", base + "sinks:\n  - name: s\n    kind: webhook\n    config: {url: 'https://hook.example.org/x', timeout: 1h}\n", "sinks[0].config.timeout: must be between 1s and 2m0s"},
		{"subscription to a missing sink", base + "subscriptions:\n  - name: x\n    sink: nowhere\n", `line 3: subscriptions[0].sink: no sink named "nowhere" in this file`},
		{"bad filter type", base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'https://hook.example.org/x'}}\nsubscriptions:\n  - name: x\n    sink: s\n    filter:\n      type: [release.exploded]\n", "subscriptions[0].filter.type[0]: must be one of release.published"},
		{"bad glob", base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'https://hook.example.org/x'}}\nsubscriptions:\n  - name: x\n    sink: s\n    filter:\n      package: '[a'\n", "subscriptions[0].filter.package: invalid glob"},
		{"interval not whole seconds", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    interval: 10m0.5s\n", "line 6: watches[0].interval: must be a whole number of seconds"},
		{"cooldown not whole seconds", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    policy:\n      cooldown: 1500ms\n", "line 7: watches[0].policy.cooldown: must be a whole number of seconds"},
		{"empty constraint", base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    policy:\n      constraint: ''\n", "line 7: watches[0].policy.constraint: must not be empty"},
		{"too large", base + "# " + strings.Repeat("x", MaxFileSize) + "\n", "larger than the 4 MiB limit"},
		{"bad template", base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'https://hook.example.org/x'}}\nsubscriptions:\n  - name: x\n    sink: s\n    template: '{{ .Foo '\n", "subscriptions[0].template: does not parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc), "w.yaml", opts(t))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err, tt.want)
			}
			var es Errors
			if !errors.As(err, &es) || len(es) == 0 {
				t.Fatalf("error is %T, want Errors", err)
			}
			var one Error
			if !errors.As(err, &one) {
				t.Fatal("errors.As does not find an Error")
			}
		})
	}
}

func TestParseReportsEveryError(t *testing.T) {
	_, err := Parse([]byte(base+"watches:\n  - name: a\n    source: cobol\n    coordinates: {name: x}\n    interval: 1m\n  - name: a\n    source: pypi\n    coordinates: {name: y}\n"), "w.yaml", opts(t))
	var es Errors
	if !errors.As(err, &es) || len(es) < 3 {
		t.Fatalf("want at least 3 errors, got %v", err)
	}
	for i := 1; i < len(es); i++ {
		if es[i].Line < es[i-1].Line {
			t.Fatalf("errors not ordered by line: %v", es)
		}
	}
}

func TestTokenLikeValuesAreRejectedWithoutEcho(t *testing.T) {
	const tok = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"
	for _, doc := range []string{
		base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    labels: {k: " + tok + "}\n",
		base + "watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n    labels: {k: whsec_abcdef}\n",
		base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'https://h.example.org/x', secret_env: " + tok + "}}\n",
		base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'https://h.example.org/hooks/" + tok + "/x'}}\n",
		base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'https://h.example.org/x?room=" + tok + "'}}\n",
		base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'https://h.example.org/x#" + tok + "'}}\n",
		base + "subscriptions:\n  - {name: x, sink: s, template: '" + tok + "'}\n",
	} {
		_, err := Parse([]byte(doc), "w.yaml", opts(t))
		if err == nil {
			t.Fatal("want an error")
		}
		if msg := err.Error(); !strings.Contains(msg, "looks like a credential") && !strings.Contains(msg, "fragment") {
			t.Fatalf("want credential error, got %v", err)
		}
		if strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "whsec_") {
			t.Fatalf("error echoes the value: %v", err)
		}
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	f := mustParse(t, base+"watches:\n  - name: a\n    source: pypi\n    coordinates: {name: x}\n")
	w := f.Watches[0]
	if w.Interval != time.Hour || w.Policy.Prerelease != "none" || !w.Policy.Baseline || w.Policy.Cooldown != 0 || len(w.Labels) != 0 {
		t.Fatalf("defaults: %+v", w)
	}
	want := `{"prerelease":"none","include":[],"exclude":[],"bump":[],"cooldown":"0s","baseline":true,"scheme":"pep440"}`
	if string(w.PolicyJSON()) != want {
		t.Fatalf("policy json = %s", w.PolicyJSON())
	}
}

func TestEmptyListsAreValid(t *testing.T) {
	f := mustParse(t, base+"sinks:\nwatches: []\nsubscriptions: []\n")
	if len(f.Sinks)+len(f.Watches)+len(f.Subscriptions) != 0 {
		t.Fatal("want nothing")
	}
}

func TestSubscriptionDefaultsAndFilter(t *testing.T) {
	f := mustParse(t, base+"sinks:\n  - {name: s, kind: webhook, config: {url: 'https://h.example.org/x'}}\nsubscriptions:\n  - name: x\n    sink: s\n")
	want := `{"source":[],"labels":{},"bump":[],"type":["release.published"]}`
	if got := string(f.Subscriptions[0].FilterJSON()); got != want {
		t.Fatalf("filter json = %s", got)
	}
}

func TestWebhookHTTPAllowlistAndWarnings(t *testing.T) {
	doc := base + "sinks:\n  - {name: s, kind: webhook, config: {url: 'http://hooks.lan/x', secret_env: HOOK}}\n  - {name: u, kind: webhook, config: {url: 'https://h.example.org/x'}}\n"
	o := opts(t)
	o.AllowHTTPHosts = []string{"HOOKS.lan"}
	o.LookupEnv = func(string) (string, bool) { return "", false }
	f, err := Parse([]byte(doc), "w.yaml", o)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Warnings) != 2 || !strings.Contains(f.Warnings[0], "HOOK is not set") || !strings.Contains(f.Warnings[1], "unsigned") {
		t.Fatalf("warnings: %q", f.Warnings)
	}
	if string(f.Sinks[0].Config) != `{"url":"http://hooks.lan/x","secret_env":"HOOK","timeout":"10s"}` {
		t.Fatalf("config: %s", f.Sinks[0].Config)
	}
}

func TestLoad(t *testing.T) {
	if _, err := Load("/nonexistent/w.yaml", opts(t)); err == nil {
		t.Fatal("want error")
	}
	f, err := Load("../../examples/watches.yaml", opts(t))
	if err != nil || f.Version != 1 {
		t.Fatalf("load example: %v", err)
	}
}

func TestULID(t *testing.T) {
	a, b := newULID(clock), newULID(clock)
	if len(a) != 26 || a == b || a[:10] != b[:10] {
		t.Fatalf("ulids: %s %s", a, b)
	}
	if newULID(clock.Add(time.Hour)) <= a {
		t.Fatal("ulid does not sort by time")
	}
}
