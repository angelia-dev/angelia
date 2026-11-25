package gitlabtrigger

import (
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink"
)

// AC of 90-B (configuration): every rule of parseConfig has a case. A message
// never echoes a token, a URL's credentials or template text.
func TestValidate(t *testing.T) {
	s := New(guardedNoServer(t), Options{})
	long := strings.Repeat("x", 256)
	entries := func(n int) string {
		var b []string
		for i := range n {
			b = append(b, `"K`+strings.Repeat("a", i%20)+string(rune('A'+i/20))+`":"v"`)
		}
		return strings.Join(b, ",")
	}
	const base = `"project":"g/p","ref":"main","token_env":"GL_TOKEN"`
	for _, tc := range []struct {
		name, cfg, want string // want "" is valid
	}{
		{"minimal", `{` + base + `}`, ""},
		{"token_ref", `{"project":"g/p","ref":"main","token_ref":"gl.token"}`, ""},
		{"numeric project", `{"project":"4711","ref":"main","token_env":"T"}`, ""},
		{"subgroups", `{"project":"a/b/c/d","ref":"v1.0","token_env":"T"}`, ""},
		{"full", `{"project":"g/p","ref":"main","base_url":"https://git.example.com/gitlab/api/v4/","token_ref":"t","variables":{"A_B":"{{.data.version}}"},"inputs":{"a-b":"{{.data.package}}"},"timeout":"2m"}`, ""},
		{"http base", `{` + base + `,"base_url":"http://git.example.com"}`, ""},
		{"timeout min", `{` + base + `,"timeout":"1s"}`, ""},
		{"50 variables", `{` + base + `,"variables":{` + entries(50) + `}}`, ""},

		{"not an object", `[]`, "want an object"},
		{"unknown key", `{` + base + `,"mode":"x"}`, "want an object"},
		{"token key", `{` + base + `,"token":"glptt-secret"}`, "want an object"},
		{"no project", `{"ref":"main","token_env":"T"}`, "project must be a path"},
		{"single segment project", `{"project":"proj","ref":"main","token_env":"T"}`, "project must be a path"},
		{"project dotdot", `{"project":"g/../x","ref":"main","token_env":"T"}`, "project must be a path"},
		{"project empty segment", `{"project":"g//x","ref":"main","token_env":"T"}`, "project must be a path"},
		{"project encoded", `{"project":"g%2Fx","ref":"main","token_env":"T"}`, "project must be a path"},
		{"project 21 segments", `{"project":"` + strings.Repeat("a/", 20) + `a","ref":"main","token_env":"T"}`, "project must be a path"},
		{"no ref", `{"project":"g/p","token_env":"T"}`, "ref must be a branch or tag"},
		{"ref too long", `{"project":"g/p","ref":"` + long + `","token_env":"T"}`, "ref must be a branch or tag"},
		{"ref control", `{"project":"g/p","ref":"a\nb","token_env":"T"}`, "ref must be a branch or tag"},
		{"ref template is literal", `{"project":"g/p","ref":"{{.data.version}}","token_env":"T"}`, ""}, // a literal: braces are allowed, never rendered
		{"no token", `{"project":"g/p","ref":"main"}`, "exactly one of token_env and token_ref"},
		{"both tokens", `{"project":"g/p","ref":"main","token_env":"T","token_ref":"t"}`, "exactly one of token_env and token_ref"},
		{"token_env not a name", `{"project":"g/p","ref":"main","token_env":"1bad"}`, "token_env must be an environment variable name"},
		{"token_env is a token", `{"project":"g/p","ref":"main","token_env":"glptt-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "token_env must be an environment variable name"},
		{"token_ref not a name", `{"project":"g/p","ref":"main","token_ref":"Bad Name"}`, "token_ref must be the name of a stored secret"},
		{"ftp base", `{` + base + `,"base_url":"ftp://git.example.com"}`, "base_url must be an absolute https://"},
		{"relative base", `{` + base + `,"base_url":"/gitlab"}`, "base_url must be an absolute"},
		{"userinfo base", `{` + base + `,"base_url":"https://u:p@git.example.com"}`, "credentials in the URL"},
		{"query base", `{` + base + `,"base_url":"https://git.example.com/?x=1"}`, "must not have a query"},
		{"empty query base", `{` + base + `,"base_url":"https://git.example.com/?"}`, "must not have a query"},
		{"fragment base", `{` + base + `,"base_url":"https://git.example.com/#x"}`, "must not have a fragment"},
		{"octal ip base", `{` + base + `,"base_url":"https://0177.0.0.1"}`, "non-canonical"},
		{"glptt token in path", `{` + base + `,"base_url":"https://git.example.com/glptt-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "looks like a credential"},
		{"token in path", `{` + base + `,"base_url":"https://git.example.com/glpat-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`, "looks like a credential"},
		{"timeout low", `{` + base + `,"timeout":"500ms"}`, "timeout must be a duration from"},
		{"timeout high", `{` + base + `,"timeout":"3m"}`, "timeout must be a duration from"},
		{"timeout junk", `{` + base + `,"timeout":"soon"}`, "timeout must be a duration from"},
		{"51 variables", `{` + base + `,"variables":{` + entries(51) + `}}`, "at most 50 variables"},
		{"51 inputs", `{` + base + `,"inputs":{` + entries(51) + `}}`, "at most 50 inputs"},
		{"variable _env", `{` + base + `,"variables":{"DEPLOY_env":"x"}}`, "must not end in _env or _ref"},
		{"variable _ref", `{` + base + `,"variables":{"GIT_ref":"x"}}`, "must not end in _env or _ref"},
		{"input _ref", `{` + base + `,"inputs":{"git-ref_ref":"x"}}`, "must not end in _env or _ref"},
		{"variable hyphen", `{` + base + `,"variables":{"A-B":"x"}}`, "variable names are 1 to 255"},
		{"variable empty name", `{` + base + `,"variables":{"":"x"}}`, "variable names are 1 to 255"},
		{"variable name too long", `{` + base + `,"variables":{"` + long + `":"x"}}`, "variable names are 1 to 255"},
		{"input dot", `{` + base + `,"inputs":{"a.b":"x"}}`, "input names are 1 to 255"},
		{"variable template syntax", `{` + base + `,"variables":{"A":"{{"}}`, "does not parse"},
		{"variable template misspelt key", `{` + base + `,"variables":{"A":"{{.data.pakage}}"}}`, "a value of variables"},
		{"input template misspelt key", `{` + base + `,"inputs":{"a":"{{.data.pakage}}"}}`, "a value of inputs"},
		{"input too long", `{` + base + `,"inputs":{"a":"` + strings.Repeat("x", 4097) + `"}}`, "at most 4096 bytes"},
		{"variable 4096 ok", `{` + base + `,"variables":{"A":"` + strings.Repeat("x", 4096) + `"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := s.Validate(sink.Config(tc.cfg))
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("err = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if err != nil {
				for _, leak := range []string{"glptt", "glpat", "u:p", "{{"} {
					if strings.Contains(err.Error(), leak) {
						t.Errorf("error echoes %q: %v", leak, err)
					}
				}
			}
		})
	}
}

// D72: base_url keeps its relative root, and loses a trailing /api/v4 and
// slashes, in any case; the defaults are filled in.
func TestParseConfigNormalizesBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"":                                  "https://gitlab.com",
		"https://gitlab.com/":               "https://gitlab.com",
		"https://git.example.com/api/v4":    "https://git.example.com",
		"https://git.example.com/API/V4/":   "https://git.example.com",
		"https://example.com/gitlab":        "https://example.com/gitlab",
		"https://example.com/gitlab/api/v4": "https://example.com/gitlab",
		"https://example.com:8443":          "https://example.com:8443",
	} {
		cfg := `{"project":"g/p","ref":"main","token_env":"T"`
		if in != "" {
			cfg += `,"base_url":"` + in + `"`
		}
		p, err := parseConfig(sink.Config(cfg+`}`), false)
		if err != nil || p.base.String() != want || p.timeout != DefaultTimeout {
			t.Errorf("%q: base %v, timeout %v, err %v; want %s", in, p.base, p.timeout, err, want)
		}
	}
}

func TestHelpers(t *testing.T) {
	if err := CheckBaseURL("https://gitlab.com"); err != nil {
		t.Error(err)
	}
	if err := CheckTemplate("\xff", 10); err == nil {
		t.Error("invalid UTF-8 accepted")
	}
	if err := CheckTemplate("", 10); err != nil {
		t.Error("empty value refused")
	}
	if err := CheckRef("a\x00b"); err == nil {
		t.Error("control character in ref accepted")
	}
	if err := CheckVariableName("OK_1"); err != nil {
		t.Error(err)
	}
	if err := CheckInputName("ok-1_x"); err != nil {
		t.Error(err)
	}
}
