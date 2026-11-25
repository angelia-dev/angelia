// Package mqtt is the "mqtt" sink (spec MqttSinkConfig, docs/mqtt.md): it
// publishes each delivery to an MQTT broker with a publish-only MQTT 3.1.1
// client of its own (standard library only, decision D76 of #93).
//
// Every attempt opens one connection through netguard.Client.DialContext,
// which applies the SSRF guard on the address actually connected and the
// host's rate limit (ADR 0039), then sends CONNECT (clean session, a fresh
// client id), PUBLISH and DISCONNECT and closes it. mqtts:// wraps the
// connection in TLS 1.2 or later, verified against the instance roots;
// mqtt:// is cleartext and allowed only for a host in http.allow_http_hosts.
// With QoS 1 an attempt succeeds on PUBACK, with QoS 0 once the packets are
// written. The payload is the rendered message (CloudEvents JSON by
// default); the topic is a template.
//
// The sink never reads the environment or the store: the core resolves
// password_env and password_ref into sink.Request.Secrets, and the password
// is sent only inside CONNECT. It logs nothing, uses no goroutine of its
// own, and its errors are "mqtt: <scheme>://<host>:<port>: <class>": never
// the user name, the password, the topic or the payload.
package mqtt

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/sink"
)

// Dialer opens TCP connections. *netguard.Client implements it; the sink
// never dials any other way.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

var _ Dialer = (*netguard.Client)(nil)

// Options configures a Sink.
type Options struct {
	// RootCAs verifies broker certificates: the instance pool, system roots
	// plus http.ca_file (config.LoadCAPool). Nil means the system roots.
	RootCAs *x509.CertPool
	// AllowPlaintextHosts are the hosts mqtt:// may be used for: the
	// instance's http.allow_http_hosts, matched case-insensitively.
	AllowPlaintextHosts []string
}

// Sink is the mqtt sink. It is safe for concurrent use.
type Sink struct {
	dialer    Dialer
	roots     *x509.CertPool
	plainOK   map[string]bool
	newClient func(prefix string) string
}

var _ sink.Sink = (*Sink)(nil)

// New returns an mqtt sink dialling through d, which must be the netguard
// client. It panics if d is nil.
func New(d Dialer, opts Options) *Sink {
	if d == nil {
		panic("mqtt.New: nil dialer")
	}
	plain := make(map[string]bool, len(opts.AllowPlaintextHosts))
	for _, h := range opts.AllowPlaintextHosts {
		plain[normHost(h)] = true
	}
	return &Sink{dialer: d, roots: opts.RootCAs, plainOK: plain, newClient: clientID}
}

func normHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// Kind implements sink.Sink.
func (*Sink) Kind() string { return Kind }

// Validate implements sink.Sink. It checks the configuration, including
// that the topic template compiles and renders a valid topic on the sample
// events; it does no I/O. A password over mqtt:// is allowed here: whether
// mqtt:// may be used at all is decided at delivery.
func (*Sink) Validate(cfg sink.Config) error {
	_, err := parseConfig(cfg, true)
	return err
}

// clientID is prefix followed by random letters and digits, 23 characters
// in all.
func clientID(prefix string) string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	n := clientIDLen - len(prefix)
	b := make([]byte, n)
	_, _ = rand.Read(b) // never fails (crypto/rand)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return prefix + string(b)
}

// Deliver implements sink.Sink: one connection, one publish. It is bounded
// by min(timeout, the deadline of ctx).
func (s *Sink) Deliver(ctx context.Context, req sink.Request) (sink.Result, error) {
	cfg, err := parseConfig(req.Config, false)
	if err != nil {
		return sink.Result{}, sink.Permanent(err)
	}
	host := cfg.host
	if !cfg.tls && !s.plainOK[normHost(host)] {
		return sink.Result{}, sink.Permanent(&failure{
			origin: cfg.origin,
			class:  "plain mqtt:// is not allowed for " + host + "; add it to http.allow_http_hosts or use mqtts://",
		})
	}

	envelope := req.Event.JSON
	if len(envelope) == 0 {
		if envelope, err = json.Marshal(req.Event.Envelope); err != nil {
			return sink.Result{}, sink.Permanent(&failure{origin: cfg.origin, class: "the event cannot be encoded"})
		}
	}
	topic, err := renderTopic(ctx, cfg.Topic, envelope)
	if err != nil {
		if ctx.Err() != nil {
			return sink.Result{}, &failure{origin: cfg.origin, class: sink.TransportClass(ctx.Err()), err: ctx.Err()}
		}
		return sink.Result{}, sink.Permanent(&failure{origin: cfg.origin, class: "topic template: " + err.Error()})
	}
	payload := req.Rendered.Body
	if payload == nil && !req.Rendered.Templated {
		payload = envelope
	}

	cp := connectPacket{clientID: s.newClient(cfg.prefix), username: cfg.Username, hasUsername: cfg.Username != ""}
	if cfg.password {
		ref, ptr := cfg.passwordRef()
		v, ok := req.Secrets[ptr]
		if !ok {
			return sink.Result{}, &failure{origin: cfg.origin, class: ref + ": not resolved"}
		}
		if v.IsZero() {
			// Retried: the operator fixes it by setting the value.
			return sink.Result{}, &failure{origin: cfg.origin, class: ref + ": empty password"}
		}
		if v.Len() > maxStringLen {
			return sink.Result{}, sink.Permanent(&failure{origin: cfg.origin, class: ref + ": password longer than 65535 bytes"})
		}
		cp.password, cp.hasPassword = v.Reveal(), true
	}
	connect, err := encodeConnect(cp)
	clear(cp.password)
	if err != nil {
		return sink.Result{}, sink.Permanent(&failure{origin: cfg.origin, class: "invalid CONNECT"})
	}
	defer clear(connect)
	publish, err := encodePublish(topic, payload, cfg.qos, cfg.Retain)
	if err != nil {
		return sink.Result{}, sink.Permanent(&failure{origin: cfg.origin, class: "invalid PUBLISH"})
	}

	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	if err := s.publish(ctx, cfg, connect, publish); err != nil {
		return sink.Result{}, err
	}
	return sink.Result{}, nil
}

// renderTopic renders the topic template on the event and checks the
// result.
func renderTopic(ctx context.Context, src string, envelope []byte) (string, error) {
	t, err := render.Compile(src, render.Limits{})
	if err != nil {
		return "", err
	}
	r, err := t.Render(ctx, envelope)
	if err != nil {
		return "", err
	}
	if err := CheckTopic(string(r.Body)); err != nil {
		return "", fmt.Errorf("the result %w", err)
	}
	return string(r.Body), nil
}

// publish runs one connection: dial, TLS, CONNECT/CONNACK, PUBLISH, PUBACK
// for QoS 1, DISCONNECT. Every read and write has the deadline of ctx, and
// ending ctx unblocks them; the connection is closed on every path.
func (s *Sink) publish(ctx context.Context, cfg parsed, connect, publish []byte) error {
	fail := func(phase string, err error) error { return classify(cfg.origin, cfg.host, phase, err) }
	conn, err := s.dialer.DialContext(ctx, "tcp", cfg.address)
	if err != nil {
		return fail("connect", err)
	}
	defer func() { _ = conn.Close() }()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// Ending ctx (the dispatcher's lease) unblocks any read or write at
	// once. AfterFunc starts a goroutine only if ctx ends first.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	if cfg.tls {
		tc := tls.Client(conn, &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: cfg.host,
			RootCAs:    s.roots,
		})
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail("TLS handshake", err)
		}
		conn = tc
	}
	r := bufio.NewReaderSize(conn, 64)

	if _, err := conn.Write(connect); err != nil {
		return fail("CONNECT", err)
	}
	p, err := decode(r)
	if err != nil {
		return fail("CONNACK", err)
	}
	if p.typ != typeConnack {
		return fail("CONNACK", protocolError("expected CONNACK"))
	}
	if err := connackError(cfg.origin, p.returnCode); err != nil {
		return err
	}
	if _, err := conn.Write(publish); err != nil {
		return fail("PUBLISH", err)
	}
	if cfg.qos == 1 {
		p, err := decode(r)
		if err != nil {
			return fail("PUBACK", err)
		}
		if p.typ != typePuback {
			return fail("PUBACK", protocolError("expected PUBACK"))
		}
		if p.id != packetID {
			return fail("PUBACK", protocolError("PUBACK for another packet identifier"))
		}
		// Delivered: a failed DISCONNECT changes nothing.
		_, _ = conn.Write(encodeDisconnect())
		return nil
	}
	if _, err := conn.Write(encodeDisconnect()); err != nil {
		return fail("DISCONNECT", err)
	}
	return nil
}

// connackError classifies a CONNACK return code (decision D79): 1, 4 and 5
// are permanent, 2 and 3 retried, anything else a protocol error.
func connackError(origin string, rc byte) error {
	switch rc {
	case connAccepted:
		return nil
	case connBadProtocol:
		return sink.Permanent(&failure{origin: origin, class: "connection refused: unacceptable protocol version (the broker does not speak MQTT 3.1.1)"})
	case connIDRejected:
		return &failure{origin: origin, class: "connection refused: identifier rejected"}
	case connServerUnavailable:
		return &failure{origin: origin, class: "connection refused: server unavailable"}
	case connBadCredentials:
		return sink.Permanent(&failure{origin: origin, class: "connection refused: bad user name or password"})
	case connNotAuthorized:
		return sink.Permanent(&failure{origin: origin, class: "connection refused: not authorized"})
	}
	return &failure{origin: origin, class: "protocol error: CONNACK with an unknown return code", err: errProtocol}
}

// classify turns a dial, TLS or I/O error into a sink error. An SSRF
// refusal and an address no retry can fix are permanent and name the host
// only; a protocol error and any transport error are retried, with a fixed
// description that never holds the dialled address.
func classify(origin, host, phase string, err error) error {
	switch {
	case errors.Is(err, netguard.ErrBlocked):
		return sink.Permanent(&failure{origin: origin, class: "destination not allowed by the SSRF guard (http.allow_networks): " + host, err: err})
	case errors.Is(err, netguard.ErrInvalidURL):
		return sink.Permanent(&failure{origin: origin, class: "invalid address (host not allowed)", err: err})
	case errors.Is(err, errProtocol):
		msg := strings.TrimPrefix(err.Error(), errProtocol.Error()+": ")
		return &failure{origin: origin, class: "protocol error in " + phase + ": " + msg, err: err}
	}
	class := sink.TransportClass(err)
	switch phase {
	case "TLS handshake":
		if class == "request failed" {
			class = "TLS handshake failed"
		}
	case "CONNACK", "PUBACK":
		if class == "timeout" {
			class = "timeout waiting for " + phase
		} else {
			class += " waiting for " + phase
		}
	default:
		class += " (" + phase + ")"
	}
	return &failure{origin: origin, class: class, err: err}
}

// failure is a failed attempt: "mqtt: <scheme>://<host>:<port>: <class>".
// Unwrap gives the cause; Error never repeats it.
type failure struct {
	origin, class string
	err           error
}

func (e *failure) Error() string { return "mqtt: " + e.origin + ": " + e.class }
func (e *failure) Unwrap() error { return e.err }
