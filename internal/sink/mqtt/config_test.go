package mqtt

import (
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/sink"
)

func TestValidate(t *testing.T) {
	s := New(guarded(t, nil), Options{})
	for _, raw := range []string{
		`{"url": "mqtts://broker.example.net", "topic": "angelia/{{.data.source}}/{{.data.package}}"}`,
		`{"url": "mqtts://broker.example.net:8883", "topic": "releases/{{.data.package}}", "qos": 1, "retain": true, "username": "angelia", "password_ref": "mqtt-broker", "client_id_prefix": "relbus", "timeout": "20s"}`,
		`{"url": "mqtt://mosquitto.lan:1883", "topic": "angelia/releases", "qos": 0, "username": "angelia", "password_env": "MQTT_PASSWORD"}`,
		`{"url": "MQTTS://[2001:db8::1]:8883", "topic": "x", "username": "only-a-user"}`,
		`{"url": "mqtts://broker.example.net", "topic": "{{with .data.previous_version}}{{.}}{{else}}first{{end}}", "timeout": "2m"}`,
	} {
		if err := s.Validate(sink.Config(raw)); err != nil {
			t.Errorf("Validate(%s): %v", raw, err)
		}
	}
	for _, tc := range []struct{ raw, want string }{
		{`{"url": "mqtt+ws://broker.example.net", "topic": "t"}`, "scheme must be mqtts:// or mqtt://"},
		{`{"url": "https://broker.example.net", "topic": "t"}`, "scheme must be"},
		{`{"url": "mqtts://broker.example.net/angelia", "topic": "t"}`, "must not have a path"},
		{`{"url": "mqtts://broker.example.net/", "topic": "t"}`, "must not have a path"},
		{`{"url": "mqtts://user:secret@broker.example.net", "topic": "t"}`, "credentials in the URL"},
		{`{"url": "mqtts://broker.example.net?x=1", "topic": "t"}`, "query"},
		{`{"url": "mqtts://broker.example.net#f", "topic": "t"}`, "fragment"},
		{`{"url": "mqtts://broker.example.net:0", "topic": "t"}`, "port"},
		{`{"url": "mqtts://broker.example.net:70000", "topic": "t"}`, "port"},
		{`{"url": "mqtts://broker.example.net:", "topic": "t"}`, "port"},
		{`{"url": "mqtts://2130706433", "topic": "t"}`, "non-canonical"},
		{`{"url": "mqtts://bücher.example", "topic": "t"}`, "punycode"},
		{`{"url": "mqtts:broker", "topic": "t"}`, "must be mqtts://host"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "qos": 2}`, "qos must be 0 or 1"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "qos": 0.5}`, "want an object"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "password_ref": "mqtt-broker"}`, "need username"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "username": "u", "password_env": "A", "password_ref": "b"}`, "at most one"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "username": "u", "password_env": "1BAD"}`, "password_env must be"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "username": "u", "password_ref": "Bad Name"}`, "password_ref must be"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "username": "` + strings.Repeat("u", 256) + `"}`, "username must be"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "username": "a\u0000b"}`, "username must be"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "client_id_prefix": "angelia-release-bus"}`, "client_id_prefix"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "client_id_prefix": "a-b"}`, "client_id_prefix"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "timeout": "500ms"}`, "timeout"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "timeout": "3m"}`, "timeout"},
		{`{"url": "mqtts://broker.example.net", "topic": "t", "password": "hunter2"}`, "want an object"},
		{`{"url": "mqtts://broker.example.net", "topic": ""}`, "topic must be 1 to 1024"},
		{`{"url": "mqtts://broker.example.net", "topic": "` + strings.Repeat("t", 1025) + `"}`, "topic must be 1 to 1024"},
		{`{"url": "mqtts://broker.example.net", "topic": "{{.data.package"}`, "does not parse"},
		{`{"url": "mqtts://broker.example.net", "topic": "a/#"}`, "must not contain +, # or U+0000"},
		{`{"url": "mqtts://broker.example.net", "topic": "{{.data.nosuch.x}}"}`, "sample event"},
		{`{"url": "mqtts://broker.example.net", "topic": "$SYS/x"}`, "must not start with $"},
		{`{"url": "mqtts://broker.example.net"}`, "topic"},
		{`{"topic": "t"}`, "url"},
		{`{"url": "mqtts://broker.example.net", "topic": "t"} {}`, "trailing data"},
		{`[]`, "want an object"},
	} {
		err := s.Validate(sink.Config(tc.raw))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Validate(%.80s) = %v, want %q", tc.raw, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), "secret@") || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error echoes the configuration: %v", err)
		}
	}
}

func TestParseConfigDefaults(t *testing.T) {
	p, err := parseConfig(sink.Config(`{"url": "mqtts://Broker.example.net", "topic": "t"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if p.qos != 1 || p.Retain || p.prefix != "angelia" || p.timeout != 10*time.Second || !p.tls || p.password {
		t.Fatalf("defaults %+v", p)
	}
	if p.address != "broker.example.net:8883" || p.origin != "mqtts://broker.example.net:8883" {
		t.Fatalf("address %q origin %q", p.address, p.origin)
	}
	p, err = parseConfig(sink.Config(`{"url": "mqtt://[::1]", "topic": "t", "qos": 0}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if p.qos != 0 || p.tls || p.address != "[::1]:1883" || p.origin != "mqtt://[::1]:1883" {
		t.Fatalf("plain %+v", p)
	}
}

func TestCheckTopic(t *testing.T) {
	for _, ok := range []string{"a", "a/b/c", "/leading", "ünïcode/✓", strings.Repeat("x", 1024)} {
		if err := CheckTopic(ok); err != nil {
			t.Errorf("CheckTopic(%.20q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 1025), "a/+/b", "a/#", "a\x00b", "$SYS/x", "\xff"} {
		if err := CheckTopic(bad); err == nil {
			t.Errorf("CheckTopic(%.20q) accepted", bad)
		}
	}
}
