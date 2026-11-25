package watchfile

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/sink"
	"github.com/angelia-dev/angelia/internal/sink/mqtt"
)

func mqttSink(config string) string {
	return base + "sinks:\n  - name: broker\n    kind: mqtt\n    config: " + config + "\n"
}

func mqttOpts(t *testing.T) Options {
	o := opts(t)
	o.AllowHTTPHosts = []string{"mosquitto.lan"}
	return o
}

// AC (#93, 93-D): the defaults are written into the stored configuration,
// which the sink's own Validate accepts.
func TestMQTTStoredConfig(t *testing.T) {
	for yaml, want := range map[string]string{ //nolint:gosec // G101: secret names, not secrets.
		`{url: 'mqtts://broker.example.net', topic: 'angelia/{{.data.source}}/{{.data.package}}'}`:                                                                             `{"url":"mqtts://broker.example.net","topic":"angelia/{{.data.source}}/{{.data.package}}","qos":1,"client_id_prefix":"angelia","timeout":"10s"}`,
		`{url: 'mqtts://broker.example.net:8883', topic: releases, qos: 0, retain: true, username: angelia, password_ref: mqtt-broker, client_id_prefix: relbus, timeout: 5s}`: `{"url":"mqtts://broker.example.net:8883","topic":"releases","qos":0,"retain":true,"username":"angelia","password_ref":"mqtt-broker","client_id_prefix":"relbus","timeout":"5s"}`,
		`{url: 'mqtt://mosquitto.lan', topic: releases}`:                                                                                                                       `{"url":"mqtt://mosquitto.lan","topic":"releases","qos":1,"client_id_prefix":"angelia","timeout":"10s"}`,
	} {
		f, err := Parse([]byte(mqttSink(yaml)), "w.yaml", mqttOpts(t))
		if err != nil {
			t.Fatalf("%s: %v", yaml, err)
		}
		if got := string(f.Sinks[0].Config); got != want {
			t.Errorf("%s:\nconfig %s\nwant   %s", yaml, got, want)
		}
		if err := (&mqtt.Sink{}).Validate(sink.Config(f.Sinks[0].Config)); err != nil {
			t.Errorf("%s: the sink rejects what the file accepted: %v", yaml, err)
		}
	}
}

func TestMQTTRejected(t *testing.T) {
	for name, tc := range map[string]struct{ yaml, want string }{
		"no url":         {`{topic: t}`, `config.url: missing`},
		"no topic":       {`{url: 'mqtts://b.example.net'}`, `config.topic: missing`},
		"websockets":     {`{url: 'mqtt+ws://b.example.net', topic: t}`, "config.url: the scheme must be mqtts:// or mqtt://"},
		"path":           {`{url: 'mqtts://b.example.net/x', topic: t}`, "config.url: the URL must not have a path"},
		"userinfo":       {`{url: 'mqtts://u:p@b.example.net', topic: t}`, "config.url: credentials in the URL are not allowed"},
		"plain":          {`{url: 'mqtt://b.example.net', topic: t}`, "plain mqtt:// is not allowed for b.example.net; add it to http.allow_http_hosts or use mqtts://"},
		"qos 2":          {`{url: 'mqtts://b.example.net', topic: t, qos: 2}`, "config.qos: must be 0 or 1"},
		"qos string":     {`{url: 'mqtts://b.example.net', topic: t, qos: '1'}`, "config.qos: must be 0 or 1"},
		"no username":    {`{url: 'mqtts://b.example.net', topic: t, password_ref: p}`, "password_env and password_ref need username"},
		"both passwords": {`{url: 'mqtts://b.example.net', topic: t, username: u, password_env: P, password_ref: p}`, "set at most one of password_env and password_ref"},
		"inline pw":      {`{url: 'mqtts://b.example.net', topic: t, username: u, password: x}`, `config.password: unknown key "password"`},
		"topic wildcard": {`{url: 'mqtts://b.example.net', topic: 'a/#'}`, "config.topic: on the full sample event, the result must not contain +, # or U+0000"},
		"topic parse":    {`{url: 'mqtts://b.example.net', topic: '{{.data'}`, "config.topic: does not parse"},
		"prefix":         {`{url: 'mqtts://b.example.net', topic: t, client_id_prefix: 'a-b'}`, "config.client_id_prefix: must be 1 to 15 letters or digits"},
		"timeout":        {`{url: 'mqtts://b.example.net', topic: t, timeout: 3m}`, "config.timeout: must be between 1s and 2m0s"},
		"retain":         {`{url: 'mqtts://b.example.net', topic: t, retain: 'yes'}`, "config.retain: must be true or false"},
		"long username":  {`{url: 'mqtts://b.example.net', topic: t, username: '` + strings.Repeat("u", 256) + `'}`, "config.username: must be 1 to 255 bytes"},
	} {
		_, err := Parse([]byte(mqttSink(tc.yaml)), "w.yaml", mqttOpts(t))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want %q", name, err, tc.want)
		}
	}
}

func TestMQTTWarnings(t *testing.T) {
	f, err := Parse([]byte(mqttSink(`{url: 'mqtt://mosquitto.lan', topic: t, username: u, password_env: MQTT_PASSWORD, timeout: 30s}`)), "w.yaml", mqttOpts(t))
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(f.Warnings, "\n")
	for _, want := range []string{"the password is sent in cleartext over mqtt://", "30s is above the dispatcher's deliver timeout"} {
		if !strings.Contains(all, want) {
			t.Errorf("warnings %q lack %q", all, want)
		}
	}
	f = mustParse(t, mqttSink(`{url: 'mqtts://b.example.net', topic: t, username: u, password_ref: p}`))
	if len(f.Warnings) != 0 {
		t.Fatalf("unexpected warnings %q", f.Warnings)
	}
}

// The example of docs/mqtt.md loads.
func TestMQTTDocsExampleLoads(t *testing.T) {
	doc, err := os.ReadFile("../../docs/mqtt.md")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)```yaml\n( *sinks:.*?)```").FindStringSubmatch(string(doc))
	if m == nil {
		t.Fatal("no watch file example in docs/mqtt.md")
	}
	f := mustParse(t, base+m[1]+"watches: []\n")
	if len(f.Sinks) != 1 || f.Sinks[0].Kind != mqtt.Kind {
		t.Fatalf("parsed %+v", f.Sinks)
	}
	if err := (&mqtt.Sink{}).Validate(sink.Config(f.Sinks[0].Config)); err != nil {
		t.Fatal(err)
	}
}
