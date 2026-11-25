package watchfile

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/gitlabtrigger"
)

func glSink(config string) string {
	return base + "sinks:\n  - name: ci\n    kind: gitlab-pipeline-trigger\n    config: " + config + "\n"
}

// 90-C: the stored configuration carries the defaults, among them base_url,
// the key a stored token is bound to (x-angelia-destination), and passes the
// sink's own Validate.
func TestGitlabTriggerStoredConfig(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{
			"minimal", `{project: platform/app, ref: main, token_env: GL_TOKEN}`,
			`{"project":"platform/app","ref":"main","base_url":"https://gitlab.com","token_env":"GL_TOKEN","timeout":"10s"}`,
		},
		{
			"numeric project, token_ref, self-managed", `{project: '4711', ref: v1, token_ref: gl-trigger, base_url: 'https://git.example.com/gitlab', timeout: 30s}`,
			`{"project":"4711","ref":"v1","base_url":"https://git.example.com/gitlab","token_ref":"gl-trigger","timeout":"30s"}`,
		},
		{
			"variables and inputs", `{project: a/b/c, ref: main, token_env: T, variables: {V: '{{.data.version}}', E: ''}, inputs: {package: '{{.data.package}}'}}`,
			`{"project":"a/b/c","ref":"main","base_url":"https://gitlab.com","token_env":"T","variables":{"E":"","V":"{{.data.version}}"},"inputs":{"package":"{{.data.package}}"},"timeout":"10s"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParse(t, glSink(tc.yaml))
			if got := string(f.Sinks[0].Config); got != tc.want {
				t.Fatalf("config\n got %s\nwant %s", got, tc.want)
			}
			if err := (*gitlabtrigger.Sink)(nil).Validate(sink.Config(f.Sinks[0].Config)); err != nil {
				t.Fatalf("the sink rejects what the file accepted: %v", err)
			}
		})
	}
}

func TestGitlabTriggerRejected(t *testing.T) {
	const ok = "project: g/p, ref: main, token_env: T"
	for _, tc := range []struct{ name, yaml, want string }{
		{"no project", `{ref: main, token_env: T}`, `config.project: missing required key "project"`},
		{"bad project", `{project: solo, ref: main, token_env: T}`, "config.project: must be a path like group/project"},
		{"project dotdot", `{project: 'g/../x', ref: main, token_env: T}`, "config.project: must be a path like group/project"},
		{"no ref", `{project: g/p, token_env: T}`, `config.ref: missing required key "ref"`},
		{"ref control", `{project: g/p, ref: "a\nb", token_env: T}`, "config.ref: must be a branch or tag name"},
		{"no token", `{project: g/p, ref: main}`, "set one of token_env and token_ref"},
		{"both tokens", `{` + ok + `, token_ref: t}`, "config.token_ref: set at most one of token_env and token_ref"},
		{"inline token", `{project: g/p, ref: main, token: glptt-x}`, `config.token: unknown key "token"`},
		{"token_env holds the token", `{project: g/p, ref: main, token_env: 'not a name!'}`, "config.token_env: must be an environment variable name"},
		{"token_env reserved", `{project: g/p, ref: main, token_env: ANGELIA_SECRETS_KEY}`, "names starting with ANGELIA_ are always refused"},
		{"token_ref bad", `{project: g/p, ref: main, token_ref: 'Bad Name'}`, "config.token_ref: must be the name of a stored secret"},
		{"ftp base", `{` + ok + `, base_url: 'ftp://git.example.com'}`, "config.base_url: must be an absolute https:// URL"},
		{"base with userinfo", `{` + ok + `, base_url: 'https://u:p@git.example.com'}`, "credentials in the URL"},
		{"base with query", `{` + ok + `, base_url: 'https://git.example.com/x?y=1'}`, "must not have a query"},
		{"base with fragment", `{` + ok + `, base_url: 'https://git.example.com/#x'}`, "must not have a fragment"},
		{"timeout", `{` + ok + `, timeout: 1h}`, "config.timeout: must be between 1s and 2m0s"},
		{"unknown key", `{` + ok + `, mode: x}`, `config.mode: unknown key "mode"`},
		{"variable _env", `{` + ok + `, variables: {deploy_env: prod}}`, "config.variables.deploy_env: a variable name must not end in _env or _ref"},
		{"variable hyphen", `{` + ok + `, variables: {a-b: x}}`, "config.variables.a-b: variable names are 1 to 255"},
		{"input _ref", `{` + ok + `, inputs: {git_ref: main}}`, "config.inputs.git_ref: an input name must not end in _env or _ref"},
		{"input dot", `{` + ok + `, inputs: {a.b: x}}`, "config.inputs.a.b: input names are 1 to 255"},
		{"input template", `{` + ok + `, inputs: {a: '{{.data.pakage}}'}}`, "config.inputs.a: on the full sample event"},
		{"variable syntax", `{` + ok + `, variables: {A: '{{'}}`, "config.variables.A: does not parse as a Go template"},
		{"input not a string", `{` + ok + `, inputs: {a: 1}}`, "config.inputs.a: must be a string"},
		{"too many variables", `{` + ok + `, variables: {` + entries(51) + `}}`, "config.variables: at most 50 variables"},
		{"too many inputs", `{` + ok + `, inputs: {` + entries(51) + `}}`, "config.inputs: at most 50 inputs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(glSink(tc.yaml)), "w.yaml", opts(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func entries(n int) string {
	var b []string
	for i := range n {
		b = append(b, "K"+strings.Repeat("a", i%20)+string(rune('A'+i/20))+": v")
	}
	return strings.Join(b, ", ")
}

// An unset token variable, a timeout above the deliver timeout and an http://
// base_url each warn.
func TestGitlabTriggerWarnings(t *testing.T) {
	o := opts(t)
	o.LookupEnv = func(string) (string, bool) { return "", false }
	f, err := Parse([]byte(glSink(`{project: g/p, ref: main, token_env: GL_TOKEN, timeout: 2m, base_url: 'http://git.example.com'}`)), "w.yaml", o)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, w := range f.Warnings {
		got += w + "\n"
	}
	for _, want := range []string{"GL_TOKEN is not set", "deliver timeout", "the trigger token is sent in cleartext over http://"} {
		if !strings.Contains(got, want) {
			t.Errorf("no warning %q in %q", want, f.Warnings)
		}
	}
	if len(f.Warnings) != 3 {
		t.Errorf("warnings: %q", f.Warnings)
	}
	// https and the default: no cleartext warning.
	f = mustParse(t, glSink(`{project: g/p, ref: main, token_ref: t, base_url: 'https://git.example.com'}`))
	if len(f.Warnings) != 0 {
		t.Errorf("warnings: %q", f.Warnings)
	}
}

// The sink and subscription example of docs/gitlab-pipeline-trigger.md load
// as a watch file.
func TestGitlabTriggerDocsExampleLoads(t *testing.T) {
	doc, err := os.ReadFile("../../docs/gitlab-pipeline-trigger.md")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?s)```yaml\n( *sinks:.*?)```")
	m := re.FindStringSubmatch(string(doc))
	if m == nil {
		t.Fatal("no watch file example in docs/gitlab-pipeline-trigger.md")
	}
	indent := len(m[1]) - len(strings.TrimLeft(m[1], " "))
	var lines []string
	for _, l := range strings.Split(m[1], "\n") {
		lines = append(lines, strings.TrimPrefix(l, strings.Repeat(" ", indent)))
	}
	f := mustParse(t, base+strings.Join(lines, "\n")+"watches: []\n")
	if len(f.Sinks) != 1 || f.Sinks[0].Kind != "gitlab-pipeline-trigger" || len(f.Subscriptions) != 1 {
		t.Fatalf("parsed %+v", f)
	}
}

// Duplicate and invalid values give one error each, not a cascade.
func TestGitlabTriggerOneErrorPerKey(t *testing.T) {
	_, err := Parse([]byte(glSink(`{project: solo, ref: main, token_env: T}`)), "w.yaml", opts(t))
	var es Errors
	if !errors.As(err, &es) || len(es) != 1 {
		t.Fatalf("errors: %v", err)
	}
}

// A subscription template means nothing to a gitlab-pipeline-trigger sink: warn.
func TestGitlabTriggerSubscriptionTemplateWarns(t *testing.T) {
	doc := glSink(`{project: g/p, ref: main, token_env: T}`) + "  - {name: w, kind: webhook, config: {url: 'https://h.example.org/x', secret_env: S}}\nsubscriptions:\n" +
		"  - {name: a, sink: ci, template: 'x'}\n  - {name: b, sink: ci}\n  - {name: c, sink: w, template: 'x'}\n"
	f := mustParse(t, doc)
	var got []string
	for _, w := range f.Warnings {
		if strings.Contains(w, "gitlab-pipeline-trigger sink") {
			got = append(got, w)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "subscriptions[0].template: template has no effect on a gitlab-pipeline-trigger sink; use variables or inputs") {
		t.Fatalf("warnings: %q", f.Warnings)
	}
}
