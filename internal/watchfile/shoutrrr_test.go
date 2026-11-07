package watchfile

import (
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/shoutrrr"
)

func shoutrrrSink(config string) string {
	return base + "sinks:\n  - name: chat\n    kind: shoutrrr\n    config: " + config + "\n"
}

// ADR 0026 §1: the file names the URL secret, never holds it; the stored
// configuration has the default timeout and passes the sink's Validate.
func TestShoutrrrStoredConfig(t *testing.T) {
	for yaml, want := range map[string]string{
		`{url_ref: slack-releases}`:                       `{"url_ref":"slack-releases","timeout":"10s"}`,
		`{url_env: CHAT_URL, service: ntfy, timeout: 5s}`: `{"url_env":"CHAT_URL","service":"ntfy","timeout":"5s"}`,
	} {
		f := mustParse(t, shoutrrrSink(yaml))
		if got := string(f.Sinks[0].Config); got != want {
			t.Errorf("%s: config %s, want %s", yaml, got, want)
		}
		if err := (&shoutrrr.Sink{}).Validate(sink.Config(f.Sinks[0].Config)); err != nil {
			t.Errorf("%s: the sink rejects what the file accepted: %v", yaml, err)
		}
	}
}

func TestShoutrrrRejected(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no url":       {`{service: slack}`, "set exactly one of url_ref and url_env"},
		"both":         {`{url_ref: a, url_env: B}`, "config.url_ref: set at most one of url_env and url_ref"},
		"inline url":   {`{url: 'slack://hook:x@webhook'}`, `config.url: unknown key "url"`},
		"url in env":   {`{url_env: 'ntfy://ntfy.sh/x'}`, "config.url_env: must be an environment variable name"},
		"bad ref":      {`{url_ref: 'Bad Name'}`, "config.url_ref: must be the name of a stored secret"},
		"bad service":  {`{url_ref: a, service: smtp}`, "config.service: must be one of slack, discord, telegram, ntfy, gotify, matrix"},
		"timeout":      {`{url_ref: a, timeout: 3m}`, "config.timeout: must be between 1s and 2m0s"},
		"reserved env": {`{url_env: ANGELIA_SECRETS_KEY}`, "names starting with ANGELIA_ are always refused"},
	} {
		_, err := Parse([]byte(shoutrrrSink(tc.yaml)), "w.yaml", opts(t))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}
