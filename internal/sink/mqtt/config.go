package mqtt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

// Kind is the registry key of this sink.
const Kind = "mqtt"

// URL schemes and their default ports.
const (
	SchemeTLS   = "mqtts"
	SchemePlain = "mqtt"
	PortTLS     = "8883"
	PortPlain   = "1883"
)

// Defaults and bounds of the configuration (spec MqttSinkConfig).
const (
	DefaultQoS            = 1
	DefaultClientIDPrefix = "angelia"
	DefaultTimeout        = 10 * time.Second
	MinTimeout            = time.Second
	MaxTimeout            = 2 * time.Minute
	// MaxTopicBytes bounds the topic template and the rendered topic.
	MaxTopicBytes = 1024
	// MaxUsernameBytes bounds username.
	MaxUsernameBytes = 255
	// clientIDLen is the length of every client id: the longest a 3.1.1
	// server must accept (§3.1.3.1).
	clientIDLen = 23
)

// Pointers of the password references in sink.Request.Secrets.
const (
	PasswordPointer    = "/password_env"
	PasswordRefPointer = "/password_ref"
)

var clientIDPrefixRE = regexp.MustCompile(`^[0-9A-Za-z]{1,15}$`)

// Config is the configuration of an mqtt sink (spec MqttSinkConfig,
// docs/mqtt.md). It holds the name of the variable or of the stored secret
// with the password, never the password.
type Config struct {
	URL            string `json:"url"`
	Topic          string `json:"topic"`
	QoS            *int   `json:"qos,omitempty"`
	Retain         bool   `json:"retain,omitempty"`
	Username       string `json:"username,omitempty"`
	PasswordEnv    string `json:"password_env,omitempty"`
	PasswordRef    string `json:"password_ref,omitempty"`
	ClientIDPrefix string `json:"client_id_prefix,omitempty"`
	Timeout        string `json:"timeout,omitempty"`
}

// CheckURL checks url: mqtts://host[:port] or mqtt://host[:port], with no
// userinfo, path, query or fragment, and a host that is ASCII and, if an IP
// address, in canonical form. The message never echoes the URL.
func CheckURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	switch {
	case err != nil || u.Opaque != "" || u.Host == "" || u.Hostname() == "":
		return nil, errors.New("must be mqtts://host[:port] or mqtt://host[:port]")
	case u.Scheme != SchemeTLS && u.Scheme != SchemePlain:
		return nil, errors.New("the scheme must be mqtts:// or mqtt:// (WebSockets are not supported)")
	case u.User != nil:
		return nil, errors.New("credentials in the URL are not allowed; use username and password_env or password_ref")
	case u.Path != "" || u.RawPath != "":
		return nil, errors.New("the URL must not have a path; the topic goes in topic")
	case u.RawQuery != "" || u.ForceQuery || strings.Contains(s, "?"):
		return nil, errors.New("the URL must not have a query")
	case u.Fragment != "" || strings.Contains(s, "#"):
		return nil, errors.New("the URL must not have a fragment")
	case netguard.NonCanonicalIP(u.Hostname()):
		return nil, errors.New("the host is an IP address in a non-canonical form; write it as a.b.c.d")
	}
	for i := range len(u.Hostname()) {
		if c := u.Hostname()[i]; c >= 0x80 || c <= 0x20 {
			return nil, errors.New("the host must be ASCII; write it as punycode")
		}
	}
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return nil, errors.New("the port must be 1 to 65535")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, errors.New("the port must be 1 to 65535")
	}
	return u, nil
}

// CheckTopic checks a rendered topic: 1 to MaxTopicBytes bytes of valid
// UTF-8, no wildcard (+ or #) or U+0000, and not starting with $ (reserved
// by brokers). The message never quotes the topic.
func CheckTopic(s string) error {
	switch {
	case len(s) == 0 || len(s) > MaxTopicBytes:
		return fmt.Errorf("must be 1 to %d bytes", MaxTopicBytes)
	case !utf8.ValidString(s):
		return errors.New("must be valid UTF-8")
	case strings.ContainsAny(s, "+#\x00"):
		return errors.New("must not contain +, # or U+0000")
	case strings.HasPrefix(s, "$"):
		return errors.New("must not start with $ (reserved by brokers)")
	}
	return nil
}

// CheckTopicTemplate compiles the topic template and renders it on the
// sample events, checking each result with CheckTopic. The error gives a
// position and a reason, never the template text.
func CheckTopicTemplate(src string) error {
	if src == "" || len(src) > MaxTopicBytes {
		return fmt.Errorf("must be 1 to %d bytes", MaxTopicBytes)
	}
	if !utf8.ValidString(src) {
		return errors.New("must be valid UTF-8")
	}
	t, err := render.Compile(src, render.Limits{})
	if err != nil {
		return fmt.Errorf("does not parse as a Go template: %w", err)
	}
	return t.CheckOutput(func(out []byte) error { return CheckTopic(string(out)) })
}

// parsed is a validated Config with its defaults applied.
type parsed struct {
	Config
	host     string // lower case, without brackets
	address  string // host:port to dial
	origin   string // scheme://host:port, for errors
	tls      bool
	qos      byte
	prefix   string
	timeout  time.Duration
	password bool // password_env or password_ref is set
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("mqtt: invalid configuration: "+format, a...)
}

// parseConfig validates raw. deep also compiles the topic template and
// renders it on the sample events, as Validate does when a sink is stored;
// Deliver compiles the template it renders.
func parseConfig(raw sink.Config, deep bool) (parsed, error) {
	var p parsed
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p.Config); err != nil {
		return p, invalid("want an object with url and topic, and optionally qos, retain, username, password_env or password_ref, client_id_prefix and timeout")
	}
	if dec.More() {
		return p, invalid("trailing data after the object")
	}
	u, err := CheckURL(p.URL)
	if err != nil {
		return p, invalid("url %v", err)
	}
	p.tls = u.Scheme == SchemeTLS
	port := u.Port()
	if port == "" {
		port = PortPlain
		if p.tls {
			port = PortTLS
		}
	}
	p.host = strings.ToLower(u.Hostname())
	p.address = net.JoinHostPort(p.host, port)
	p.origin = u.Scheme + "://" + p.address
	if deep {
		if err := CheckTopicTemplate(p.Topic); err != nil {
			return p, invalid("topic %v", err)
		}
	} else if p.Topic == "" || len(p.Topic) > MaxTopicBytes {
		return p, invalid("topic must be 1 to %d bytes", MaxTopicBytes)
	}
	p.qos = DefaultQoS
	if p.QoS != nil {
		if *p.QoS != 0 && *p.QoS != 1 {
			return p, invalid("qos must be 0 or 1")
		}
		p.qos = byte(*p.QoS) //nolint:gosec // G115: 0 or 1.
	}
	if p.Username != "" && (len(p.Username) > MaxUsernameBytes || !utf8.ValidString(p.Username) || strings.ContainsRune(p.Username, 0)) {
		return p, invalid("username must be 1 to %d bytes of UTF-8 without U+0000", MaxUsernameBytes)
	}
	switch {
	case p.PasswordEnv != "" && p.PasswordRef != "":
		return p, invalid("set at most one of password_env and password_ref")
	case (p.PasswordEnv != "" || p.PasswordRef != "") && p.Username == "":
		return p, invalid("password_env and password_ref need username")
	case p.PasswordEnv != "" && (!config.IsEnvName(p.PasswordEnv) || config.LooksLikeToken(p.PasswordEnv)):
		return p, invalid("password_env must be an environment variable name")
	case p.PasswordRef != "" && !secrets.ValidName(p.PasswordRef):
		return p, invalid("password_ref must be the name of a stored secret")
	}
	p.password = p.PasswordEnv != "" || p.PasswordRef != ""
	p.prefix = DefaultClientIDPrefix
	if p.ClientIDPrefix != "" {
		if !clientIDPrefixRE.MatchString(p.ClientIDPrefix) {
			return p, invalid("client_id_prefix must be 1 to 15 letters or digits")
		}
		p.prefix = p.ClientIDPrefix
	}
	p.timeout = DefaultTimeout
	if p.Timeout != "" {
		d, err := time.ParseDuration(p.Timeout)
		if err != nil || d < MinTimeout || d > MaxTimeout {
			return p, invalid("timeout must be a duration from %v to %v", MinTimeout, MaxTimeout)
		}
		p.timeout = d
	}
	return p, nil
}

// passwordRef names the password reference for errors, "password_env NAME"
// or "password_ref NAME", and gives its key in sink.Request.Secrets.
func (p parsed) passwordRef() (ref, ptr string) {
	if p.PasswordEnv != "" {
		return "password_env " + p.PasswordEnv, PasswordPointer
	}
	return "password_ref " + p.PasswordRef, PasswordRefPointer
}
