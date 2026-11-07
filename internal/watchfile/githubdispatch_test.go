package watchfile

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/githubdispatch"
)

func ghSink(config string) string {
	return base + "sinks:\n  - name: ci\n    kind: github-dispatch\n    config: " + config + "\n"
}

// The stored configuration carries the defaults, among them api_base_url,
// the key a stored token is bound to (x-angelia-destination), and passes the
// sink's own Validate.
func TestGithubDispatchStoredConfig(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{
			"minimal", `{repo: octo-org/octo-repo, token_env: GH_TOKEN}`,
			`{"mode":"repository_dispatch","repo":"octo-org/octo-repo","api_base_url":"https://api.github.com","token_env":"GH_TOKEN","event_type":"angelia.release.published","timeout":"10s"}`,
		},
		{
			"token_ref and enterprise", `{repo: o/r, token_ref: gh-token, api_base_url: 'https://ghe.example.com/api/v3', event_type: 'rel-{{.data.source}}', timeout: 30s}`,
			`{"mode":"repository_dispatch","repo":"o/r","api_base_url":"https://ghe.example.com/api/v3","token_ref":"gh-token","event_type":"rel-{{.data.source}}","timeout":"30s"}`,
		},
		{
			"workflow_dispatch", `{mode: workflow_dispatch, repo: o/r, token_env: T, workflow: release.yml, ref: main, inputs: {version: '{{.data.version}}', n: ''}}`,
			`{"mode":"workflow_dispatch","repo":"o/r","api_base_url":"https://api.github.com","token_env":"T","workflow":"release.yml","ref":"main","inputs":{"n":"","version":"{{.data.version}}"},"timeout":"10s"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := mustParse(t, ghSink(tc.yaml))
			if got := string(f.Sinks[0].Config); got != tc.want {
				t.Fatalf("config\n got %s\nwant %s", got, tc.want)
			}
			if err := (*githubdispatch.Sink)(nil).Validate(sink.Config(f.Sinks[0].Config)); err != nil {
				t.Fatalf("the sink rejects what the file accepted: %v", err)
			}
		})
	}
}

func TestGithubDispatchRejected(t *testing.T) {
	wf := "mode: workflow_dispatch, repo: o/r, token_env: T"
	for _, tc := range []struct{ name, yaml, want string }{
		{"no repo", `{token_env: T}`, `config.repo: missing required key "repo"`},
		{"bad repo", `{repo: octo, token_env: T}`, "config.repo: must be owner/name"},
		{"no token", `{repo: o/r}`, "set one of token_env and token_ref"},
		{"both tokens", `{repo: o/r, token_env: T, token_ref: t}`, "config.token_ref: set at most one of token_env and token_ref"},
		{"inline token", `{repo: o/r, token: ghp_x}`, `config.token: unknown key "token"`},
		{"token_env holds the token", `{repo: o/r, token_env: 'not a name!'}`, "config.token_env: must be an environment variable name"},
		{"token_env reserved", `{repo: o/r, token_env: ANGELIA_SECRETS_KEY}`, "names starting with ANGELIA_ are always refused"},
		{"token_ref bad", `{repo: o/r, token_ref: 'Bad Name'}`, "config.token_ref: must be the name of a stored secret"},
		{"http base", `{repo: o/r, token_env: T, api_base_url: 'http://ghe.example.com'}`, "config.api_base_url: must be an https:// URL"},
		{"base with userinfo", `{repo: o/r, token_env: T, api_base_url: 'https://u:p@ghe.example.com'}`, "credentials in the URL"},
		{"base with query", `{repo: o/r, token_env: T, api_base_url: 'https://ghe.example.com/x?y=1'}`, "must not have a query"},
		{"bad mode", `{repo: o/r, token_env: T, mode: push}`, "config.mode: must be one of repository_dispatch, workflow_dispatch"},
		{"event_type result too long", `{repo: o/r, token_env: T, event_type: '` + strings.Repeat("{{.data.package}}", 20) + `'}`, "the result must be 1 to 100"},
		{"workflow key in repository mode", `{repo: o/r, token_env: T, workflow: a.yml}`, "config.workflow: only for mode workflow_dispatch"},
		{"inputs in repository mode", `{repo: o/r, token_env: T, inputs: {a: b}}`, "config.inputs: only for mode workflow_dispatch"},
		{"event_type in workflow mode", `{` + wf + `, workflow: a.yml, ref: main, event_type: x}`, "config.event_type: only for mode repository_dispatch"},
		{"workflow mode without workflow", `{` + wf + `, ref: main}`, `config.workflow: missing required key "workflow"`},
		{"workflow mode without ref", `{` + wf + `, workflow: a.yml}`, `config.ref: missing required key "ref"`},
		{"workflow with slash", `{` + wf + `, workflow: a/b.yml, ref: main}`, "config.workflow: must be a workflow file name"},
		{"input _env", `{` + wf + `, workflow: a.yml, ref: main, inputs: {deploy_env: prod}}`, "config.inputs.deploy_env: an input name must not end in _env or _ref"},
		{"input template", `{` + wf + `, workflow: a.yml, ref: main, inputs: {a: '{{.data.pakage}}'}}`, "config.inputs.a: on the full sample event"},
		{"input not a string", `{` + wf + `, workflow: a.yml, ref: main, inputs: {a: 1}}`, "config.inputs.a: must be a string"},
		{"event_type template", `{repo: o/r, token_env: T, event_type: '{{.data.pakage}}'}`, "config.event_type: on the full sample event"},
		{"event_type syntax", `{repo: o/r, token_env: T, event_type: '{{'}`, "config.event_type: does not parse as a Go template"},
		{"event_type literal too long", `{repo: o/r, token_env: T, event_type: ` + strings.Repeat("x", 101) + `}`, "config.event_type: on the full sample event, the result must be 1 to 100 characters"},
		{"event_type empty", `{repo: o/r, token_env: T, event_type: ''}`, "config.event_type: must be 1 to 100 characters"},
		{"timeout", `{repo: o/r, token_env: T, timeout: 1h}`, "config.timeout: must be between 1s and 2m0s"},
		{"unknown key", `{repo: o/r, token_env: T, branch: main}`, `config.branch: unknown key "branch"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(ghSink(tc.yaml)), "w.yaml", opts(t))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestGithubDispatchWarnsOnUnsetToken(t *testing.T) {
	o := opts(t)
	o.LookupEnv = func(string) (string, bool) { return "", false }
	f, err := Parse([]byte(ghSink(`{repo: o/r, token_env: GH_TOKEN, timeout: 2m}`)), "w.yaml", o)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Warnings) != 2 || !strings.Contains(f.Warnings[0], "GH_TOKEN is not set") || !strings.Contains(f.Warnings[1], "deliver timeout") {
		t.Fatalf("warnings: %q", f.Warnings)
	}
}

// The sink and subscription example of docs/github-dispatch.md load as a
// watch file.
func TestGithubDispatchDocsExampleLoads(t *testing.T) {
	doc, err := os.ReadFile("../../docs/github-dispatch.md")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile("(?s)```yaml\n( *sinks:.*?)```")
	m := re.FindStringSubmatch(string(doc))
	if m == nil {
		t.Fatal("no watch file example in docs/github-dispatch.md")
	}
	// The example sits in a list item: take the indentation off.
	indent := len(m[1]) - len(strings.TrimLeft(m[1], " "))
	var lines []string
	for _, l := range strings.Split(m[1], "\n") {
		lines = append(lines, strings.TrimPrefix(l, strings.Repeat(" ", indent)))
	}
	f := mustParse(t, base+strings.Join(lines, "\n")+"watches: []\n")
	if len(f.Sinks) != 1 || f.Sinks[0].Kind != "github-dispatch" || len(f.Subscriptions) != 1 {
		t.Fatalf("parsed %+v", f)
	}
}

// An invalid mode gives one error, not a second one per mode-specific key.
func TestGithubDispatchBadModeOneError(t *testing.T) {
	_, err := Parse([]byte(ghSink(`{repo: o/r, token_env: T, mode: push, workflow: a.yml, ref: main, event_type: x}`)), "w.yaml", opts(t))
	var es Errors
	if !errors.As(err, &es) || len(es) != 1 || !strings.Contains(es[0].Msg, "must be one of") {
		t.Fatalf("errors: %v", err)
	}
}

// A subscription template means nothing to a github-dispatch sink: warn.
func TestGithubDispatchSubscriptionTemplateWarns(t *testing.T) {
	doc := ghSink(`{repo: o/r, token_env: T}`) + "  - {name: w, kind: webhook, config: {url: 'https://h.example.org/x', secret_env: S}}\nsubscriptions:\n" +
		"  - {name: a, sink: ci, template: 'x'}\n  - {name: b, sink: ci}\n  - {name: c, sink: w, template: 'x'}\n"
	f := mustParse(t, doc)
	var got []string
	for _, w := range f.Warnings {
		if strings.Contains(w, "github-dispatch sink") {
			got = append(got, w)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "subscriptions[0].template: template has no effect on a github-dispatch sink; use event_type or inputs") {
		t.Fatalf("warnings: %q", f.Warnings)
	}
}
