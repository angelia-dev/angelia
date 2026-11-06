package githubdispatch

import (
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink"
)

// Validate applies the rules of the spec's GithubDispatchSinkConfig.
func TestValidate(t *testing.T) {
	s := New(guardedNoServer(t), Options{})
	long := strings.Repeat("x", 256)
	inputs26 := make([]string, 0, 26)
	for i := range 26 {
		inputs26 = append(inputs26, `"i`+string(rune('a'+i))+`":"v"`)
	}
	for _, tc := range []struct {
		name, cfg, want string // want "" is valid
	}{
		{"minimal", `{"repo":"o/r","token_env":"GH_TOKEN"}`, ""},
		{"token_ref", `{"repo":"o/r","token_ref":"gh.token"}`, ""},
		{"full repository_dispatch", `{"mode":"repository_dispatch","repo":"octo-org/octo.repo_x","api_base_url":"https://ghe.example.com/api/v3","token_env":"GH_TOKEN","event_type":"rel.{{.data.source}}","timeout":"2m"}`, ""},
		{"full workflow_dispatch", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"release.yml","ref":"refs/heads/main","inputs":{"v":"{{.data.version}}"},"timeout":"1s"}`, ""},
		{"numeric workflow", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"42","ref":"main"}`, ""},
		{"not an object", `[]`, "want an object"},
		{"unknown key", `{"repo":"o/r","token_env":"T","secret":"x"}`, "want an object"},
		{"no repo", `{"token_env":"T"}`, "repo must be owner/name"},
		{"bad repo", `{"repo":"o/r/x","token_env":"T"}`, "repo must be owner/name"},
		{"repo dotdot", `{"repo":"o/..","token_env":"T"}`, "repo must be owner/name"},
		{"repo dot", `{"repo":"o/.","token_env":"T"}`, "repo must be owner/name"},
		{"bad mode", `{"mode":"push","repo":"o/r","token_env":"T"}`, "mode must be"},
		{"no token", `{"repo":"o/r"}`, "exactly one of token_env and token_ref"},
		{"both tokens", `{"repo":"o/r","token_env":"T","token_ref":"t"}`, "exactly one of token_env and token_ref"},
		{"token_env not a name", `{"repo":"o/r","token_env":"1bad"}`, "token_env must be an environment variable name"},
		{"token_env is a token", `{"repo":"o/r","token_env":"ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "token_env must be an environment variable name"},
		{"token_ref not a name", `{"repo":"o/r","token_ref":"Bad Name"}`, "token_ref must be the name of a stored secret"},
		{"http base", `{"repo":"o/r","token_env":"T","api_base_url":"http://ghe.example.com"}`, "api_base_url must be an https://"},
		{"relative base", `{"repo":"o/r","token_env":"T","api_base_url":"/api"}`, "api_base_url must be an absolute"},
		{"userinfo base", `{"repo":"o/r","token_env":"T","api_base_url":"https://u:p@ghe.example.com"}`, "credentials in the URL"},
		{"query base", `{"repo":"o/r","token_env":"T","api_base_url":"https://ghe.example.com/api?x=1"}`, "must not have a query"},
		{"empty query base", `{"repo":"o/r","token_env":"T","api_base_url":"https://ghe.example.com/api?"}`, "must not have a query"},
		{"fragment base", `{"repo":"o/r","token_env":"T","api_base_url":"https://ghe.example.com/api#x"}`, "must not have a fragment"},
		{"octal ip base", `{"repo":"o/r","token_env":"T","api_base_url":"https://0177.0.0.1"}`, "non-canonical"},
		{"token in path", `{"repo":"o/r","token_env":"T","api_base_url":"https://ghe.example.com/ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "looks like a credential"},
		{"timeout low", `{"repo":"o/r","token_env":"T","timeout":"500ms"}`, "timeout must be a duration from"},
		{"timeout high", `{"repo":"o/r","token_env":"T","timeout":"3m"}`, "timeout must be a duration from"},
		{"timeout junk", `{"repo":"o/r","token_env":"T","timeout":"soon"}`, "timeout must be a duration from"},
		{"workflow in repository mode", `{"repo":"o/r","token_env":"T","workflow":"a.yml"}`, "are for mode workflow_dispatch"},
		{"ref in repository mode", `{"repo":"o/r","token_env":"T","ref":"main"}`, "are for mode workflow_dispatch"},
		{"inputs in repository mode", `{"repo":"o/r","token_env":"T","inputs":{}}`, "are for mode workflow_dispatch"},
		{"event_type in workflow mode", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"m","event_type":"x"}`, "event_type is for mode repository_dispatch"},
		{"workflow mode without workflow", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","ref":"m"}`, "workflow must be 1 to"},
		{"workflow mode without ref", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml"}`, "ref must be a branch or tag"},
		{"workflow with slash", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a/b.yml","ref":"m"}`, "workflow must be a workflow file name"},
		{"workflow dotdot", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"..","ref":"m"}`, "workflow must be a workflow file name"},
		{"workflow too long", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"` + long + `","ref":"m"}`, "workflow must be 1 to 255"},
		{"ref too long", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"` + long + `"}`, "ref must be a branch or tag"},
		{"too many inputs", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"m","inputs":{` + strings.Join(inputs26, ",") + `}}`, "at most 25 inputs"},
		{"input _env", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"m","inputs":{"deploy_env":"prod"}}`, "must not end in _env or _ref"},
		{"input _ref", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"m","inputs":{"git_ref":"main"}}`, "must not end in _env or _ref"},
		{"empty input name", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"m","inputs":{"":"x"}}`, "input names are 1 to"},
		{"input template error", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"m","inputs":{"a":"{{.data.pakage}}"}}`, "an input value"},
		{"input too long", `{"mode":"workflow_dispatch","repo":"o/r","token_env":"T","workflow":"a.yml","ref":"m","inputs":{"a":"` + strings.Repeat("x", 4097) + `"}}`, "at most 4096 bytes"},
		{"event_type misspelt key", `{"repo":"o/r","token_env":"T","event_type":"{{.data.pakage}}"}`, "event_type"},
		{"event_type syntax", `{"repo":"o/r","token_env":"T","event_type":"{{"}`, "does not parse"},
		{"event_type too long a template", `{"repo":"o/r","token_env":"T","event_type":"` + strings.Repeat("x", 1025) + `"}`, "at most 1024 bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Validate(sink.Config(tc.cfg))
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("err = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if err != nil && (strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "u:p")) {
				t.Errorf("error echoes the value: %v", err)
			}
		})
	}
}

func TestHelpers(t *testing.T) {
	if err := CheckEventType("a\x00b"); err == nil {
		t.Error("control character accepted")
	}
	if err := CheckEventType(strings.Repeat("é", 101)); err == nil {
		t.Error("101 characters accepted")
	}
	if !IsLiteral("angelia.release") || IsLiteral("a{{.id}}") {
		t.Error("IsLiteral")
	}
	if err := CheckTemplate("\xff", 10); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
	if err := CheckRef("a\nb"); err == nil {
		t.Error("control character in ref accepted")
	}
}
