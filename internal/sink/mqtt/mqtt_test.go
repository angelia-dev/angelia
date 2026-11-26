package mqtt

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/render"
	"github.com/angelia-dev/angelia/internal/secrets"
	"github.com/angelia-dev/angelia/internal/sink"
)

const (
	testPassword = "s3cret-Broker-Passw0rd"
	testUsername = "relbus-user"
	testPayload  = `{"canary":"payload-6f1d"}`
)

// ---- fake broker ----------------------------------------------------------

// session is what the fake broker received on one connection.
type session struct {
	protocol     string
	level        byte
	flags        byte
	keepAlive    uint16
	clientID     string
	username     string
	password     string
	published    bool
	topic        string
	qos          byte
	retain       bool
	id           uint16
	payload      []byte
	disconnected bool
	err          error
}

// script says how the broker answers. connack and puback are written after
// CONNECT and after PUBLISH; nil means no answer (the broker waits until
// the client closes the connection).
type script struct {
	connack []byte
	puback  []byte
}

var (
	accepted = []byte{0x20, 0x02, 0x00, 0x00}
	puback1  = []byte{0x40, 0x02, 0x00, 0x01}
)

type broker struct {
	t        *testing.T
	ln       net.Listener
	port     string
	conns    atomic.Int64
	sessions chan session
	done     chan struct{} // closed when the accept loop returns
	mu       sync.Mutex
	raw      bytes.Buffer // every byte received on TCP, before any TLS
}

// newBroker listens on 127.0.0.1, with TLS when cert is not nil.
func newBroker(t *testing.T, sc script, cert *tls.Certificate) *broker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &broker{t: t, sessions: make(chan session, 16), done: make(chan struct{})}
	b.ln = ln
	_, b.port, _ = net.SplitHostPort(ln.Addr().String())
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		defer close(b.done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			b.conns.Add(1)
			c = &recConn{Conn: c, b: b}
			if cert != nil {
				c = tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12})
			}
			go b.serve(c, sc)
		}
	}()
	return b
}

type recConn struct {
	net.Conn
	b *broker
}

func (c *recConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.b.mu.Lock()
	c.b.raw.Write(p[:n])
	c.b.mu.Unlock()
	return n, err
}

// closedConns closes the listener, waits until the accept loop has
// returned, and gives the number of connections accepted.
func (b *broker) closedConns() int64 {
	_ = b.ln.Close()
	<-b.done
	return b.conns.Load()
}

func (b *broker) rawBytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.raw.Bytes())
}

func readPacket(r *bufio.Reader) (first byte, body []byte, err error) {
	if first, err = r.ReadByte(); err != nil {
		return 0, nil, err
	}
	n, err := readRemainingLength(r)
	if err != nil {
		return 0, nil, err
	}
	body = make([]byte, n)
	_, err = io.ReadFull(r, body)
	return first, body, err
}

func str(b []byte) (string, []byte) {
	n := int(binary.BigEndian.Uint16(b))
	return string(b[2 : 2+n]), b[2+n:]
}

func (b *broker) serve(c net.Conn, sc script) {
	defer func() { _ = c.Close() }()
	var s session
	defer func() { b.sessions <- s }()
	r := bufio.NewReader(c)
	first, body, err := readPacket(r)
	if err != nil || first != 0x10 {
		s.err = errors.New("no CONNECT")
		return
	}
	s.protocol, body = str(body)
	s.level, s.flags, s.keepAlive = body[0], body[1], binary.BigEndian.Uint16(body[2:4])
	body = body[4:]
	s.clientID, body = str(body)
	if s.flags&0x80 != 0 {
		s.username, body = str(body)
	}
	if s.flags&0x40 != 0 {
		s.password, _ = str(body)
	}
	if sc.connack == nil {
		_, _ = io.Copy(io.Discard, r)
		return
	}
	_, _ = c.Write(sc.connack)
	for {
		first, body, err := readPacket(r)
		if err != nil {
			return
		}
		switch first >> 4 {
		case typePublish:
			s.published = true
			s.qos, s.retain = (first>>1)&3, first&1 == 1
			s.topic, body = str(body)
			if s.qos > 0 {
				s.id, body = binary.BigEndian.Uint16(body), body[2:]
			}
			s.payload = body
			if s.qos > 0 {
				if sc.puback == nil {
					_, _ = io.Copy(io.Discard, r)
					return
				}
				_, _ = c.Write(sc.puback)
			}
		case typeDisconnect:
			s.disconnected = true
			return
		}
	}
}

func (b *broker) session(t *testing.T) session {
	t.Helper()
	select {
	case s := <-b.sessions:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the broker saw no session")
		return session{}
	}
}

// ---- helpers ----------------------------------------------------------------

var loopback = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

func guarded(t *testing.T, allow []netip.Prefix) *netguard.Client {
	t.Helper()
	fast := netguard.Limit{Rate: 1000, Burst: 1000}
	nc, err := netguard.New(netguard.Config{Allow: allow, FallbackLimit: netguard.HostLimit{Anonymous: fast, Authenticated: fast}})
	if err != nil {
		t.Fatal(err)
	}
	return nc
}

// testCert returns a self-signed certificate for 127.0.0.1, ::1 and
// localhost, and a pool that trusts it.
func testCert(t *testing.T) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "angelia test broker"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func request(t *testing.T, cfg map[string]any, secretsByPtr map[string]secrets.Value) sink.Request {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ev := render.Samples()[0].Envelope
	evJSON, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	return sink.Request{
		Config:   raw,
		Event:    sink.ReleaseEvent{JSON: evJSON, Envelope: ev},
		Rendered: sink.Rendered{Body: []byte(testPayload), ContentType: sink.CloudEventsContentType},
		Secrets:  secretsByPtr,
		Attempt:  1,
	}
}

func deliver(t *testing.T, s *Sink, req sink.Request) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	res, err := s.Deliver(ctx, req)
	if res.StatusCode != 0 {
		t.Fatalf("StatusCode %d, want 0", res.StatusCode)
	}
	return err
}

// assertNoLeak checks an error carries none of the sensitive values.
func assertNoLeak(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{testPassword, testUsername, testPayload, "canary", "angelia/pypi", "requests"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q contains %q", err, s)
		}
	}
}

func wantClass(t *testing.T, err error, permanent bool, contains string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil, want an error containing %q", contains)
	}
	if sink.IsPermanent(err) != permanent {
		t.Fatalf("%v: permanent = %v, want %v", err, !permanent, permanent)
	}
	if !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %q does not contain %q", err, contains)
	}
	if !strings.HasPrefix(err.Error(), "mqtt: ") {
		t.Fatalf("error %q lacks the mqtt: prefix", err)
	}
	assertNoLeak(t, err)
}

var clientIDRE = regexp.MustCompile(`^[0-9A-Za-z]{23}$`)

// ---- tests ------------------------------------------------------------------

// AC: QoS 1 over TLS with a stored password: CONNECT is MQTT 3.1.1 with a
// clean session, keep-alive 30 and a fresh 23-character client id; the
// password travels only inside CONNECT over TLS; PUBLISH carries the
// rendered topic, the payload, QoS 1, retain and packet id 1; success on
// PUBACK, then DISCONNECT.
func TestDeliverQoS1TLS(t *testing.T) {
	cert, pool := testCert(t)
	b := newBroker(t, script{connack: accepted, puback: puback1}, cert)
	s := New(guarded(t, loopback), Options{RootCAs: pool})
	req := request(t, map[string]any{ //nolint:gosec // G101: synthetic test credentials.
		"url": "mqtts://127.0.0.1:" + b.port, "topic": "angelia/{{.data.source}}/{{.data.package}}",
		"retain": true, "username": testUsername, "password_ref": "mqtt-broker", "client_id_prefix": "relbus",
	}, map[string]secrets.Value{PasswordRefPointer: secrets.NewValueString(testPassword)})
	if err := deliver(t, s, req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	got := b.session(t)
	if got.protocol != "MQTT" || got.level != 4 || got.flags != 0xc2 || got.keepAlive != 30 {
		t.Fatalf("CONNECT protocol %q level %d flags %#x keep-alive %d", got.protocol, got.level, got.flags, got.keepAlive)
	}
	if !clientIDRE.MatchString(got.clientID) || !strings.HasPrefix(got.clientID, "relbus") {
		t.Fatalf("client id %q", got.clientID)
	}
	if got.username != testUsername || got.password != testPassword {
		t.Fatalf("credentials %q / %q", got.username, got.password)
	}
	if !got.published || got.topic != "angelia/pypi/requests" || got.qos != 1 || !got.retain || got.id != 1 || string(got.payload) != testPayload {
		t.Fatalf("PUBLISH %+v", got)
	}
	if !got.disconnected {
		t.Fatal("no DISCONNECT after PUBACK")
	}
	raw := b.rawBytes()
	for _, s := range []string{testPassword, testUsername, testPayload, "angelia/pypi"} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatalf("%q crossed the wire in clear", s)
		}
	}
	// A second delivery uses a new connection and a new client id.
	if err := deliver(t, s, req); err != nil {
		t.Fatalf("second Deliver: %v", err)
	}
	if again := b.session(t); again.clientID == got.clientID {
		t.Fatalf("client id reused: %q", again.clientID)
	}
	if b.conns.Load() != 2 {
		t.Fatalf("%d connections, want one per attempt", b.conns.Load())
	}
}

// AC: QoS 0 over plain mqtt:// to a host of http.allow_http_hosts, by name
// (localhost resolves to 127.0.0.1 and ::1, both allowed): success once
// PUBLISH and DISCONNECT are written, without PUBACK.
func TestDeliverQoS0Plain(t *testing.T) {
	b := newBroker(t, script{connack: accepted}, nil)
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"LocalHost"}})
	req := request(t, map[string]any{"url": "mqtt://localhost:" + b.port, "topic": "releases", "qos": 0}, nil)
	if err := deliver(t, s, req); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	got := b.session(t)
	if got.flags != 0x02 || got.username != "" || got.password != "" {
		t.Fatalf("anonymous CONNECT flags %#x", got.flags)
	}
	if !clientIDRE.MatchString(got.clientID) || !strings.HasPrefix(got.clientID, DefaultClientIDPrefix) {
		t.Fatalf("client id %q", got.clientID)
	}
	if !got.published || got.qos != 0 || got.retain || got.id != 0 || got.topic != "releases" || string(got.payload) != testPayload {
		t.Fatalf("PUBLISH %+v", got)
	}
	if !got.disconnected {
		t.Fatal("no DISCONNECT")
	}
}

// AC: mqtt:// to a host not in http.allow_http_hosts is permanent and
// nothing is dialled.
func TestDeliverPlaintextRefused(t *testing.T) {
	b := newBroker(t, script{connack: accepted}, nil)
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"other.example.net"}})
	req := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases"}, nil)
	err := deliver(t, s, req)
	wantClass(t, err, true, "plain mqtt:// is not allowed for 127.0.0.1; add it to http.allow_http_hosts or use mqtts://")
	if b.conns.Load() != 0 {
		t.Fatal("the broker was dialled")
	}
}

// AC: an SSRF refusal is permanent and the broker is never reached.
func TestDeliverSSRFRefused(t *testing.T) {
	b := newBroker(t, script{connack: accepted}, nil)
	s := New(guarded(t, nil), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	err := deliver(t, s, request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases"}, nil))
	wantClass(t, err, true, "destination not allowed by the SSRF guard (http.allow_networks)")
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("%v does not wrap ErrBlocked", err)
	}
	if n := b.closedConns(); n != 0 {
		t.Fatalf("the broker was reached %d times", n)
	}
}

// AC: CONNACK 1, 4 and 5 are permanent; 2 and 3 retried; an unknown code
// is a protocol error, retried. No message leaks a credential.
func TestDeliverConnackCodes(t *testing.T) {
	for _, tc := range []struct {
		rc        byte
		permanent bool
		class     string
	}{
		{1, true, "unacceptable protocol version"},
		{2, false, "identifier rejected"},
		{3, false, "server unavailable"},
		{4, true, "bad user name or password"},
		{5, true, "not authorized"},
		{6, false, "protocol error"},
	} {
		b := newBroker(t, script{connack: []byte{0x20, 0x02, 0x00, tc.rc}}, nil)
		s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
		req := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "angelia/{{.data.source}}", "username": testUsername, "password_env": "MQTT_PASSWORD"},
			map[string]secrets.Value{PasswordPointer: secrets.NewValueString(testPassword)})
		err := deliver(t, s, req)
		wantClass(t, err, tc.permanent, tc.class)
		if !strings.HasPrefix(err.Error(), "mqtt: mqtt://127.0.0.1:"+b.port+": ") {
			t.Fatalf("CONNACK %d: error %q", tc.rc, err)
		}
		if got := b.session(t); got.published {
			t.Fatalf("CONNACK %d: published anyway", tc.rc)
		}
	}
}

// AC: no CONNACK or PUBACK before the deadline is a retried timeout.
func TestDeliverTimeouts(t *testing.T) {
	for _, tc := range []struct {
		sc    script
		class string
	}{
		{script{}, "timeout waiting for CONNACK"},
		{script{connack: accepted}, "timeout waiting for PUBACK"},
	} {
		b := newBroker(t, tc.sc, nil)
		s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
		req := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases", "timeout": "1s"}, nil)
		start := time.Now()
		_, err := s.Deliver(t.Context(), req)
		wantClass(t, err, false, tc.class)
		if d := time.Since(start); d < 900*time.Millisecond || d > 4*time.Second {
			t.Fatalf("%s after %v, want the 1s timeout", tc.class, d)
		}
	}
}

// The attempt's context bounds the attempt too, and ending it unblocks a
// read at once.
func TestDeliverContextEnds(t *testing.T) {
	b := newBroker(t, script{}, nil)
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	req := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases"}, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.Deliver(ctx, req)
	wantClass(t, err, false, "timeout waiting for CONNACK")
	if time.Since(start) > 2*time.Second {
		t.Fatal("the context deadline was not applied")
	}

	ctx, cancel = context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	start = time.Now()
	_, err = s.Deliver(ctx, req)
	if err == nil || sink.IsPermanent(err) || time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled: %v after %v", err, time.Since(start))
	}
}

// AC: malformed packets from the broker are retried protocol errors; a
// connection closed early is a retried transport error.
func TestDeliverMalformed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		sc    script
		class string
	}{
		{"PUBLISH instead of CONNACK", script{connack: []byte{0x30, 0x03, 0x00, 0x01, 'a'}}, "protocol error in CONNACK: unexpected packet type 3"},
		{"remaining length of 5 bytes", script{connack: []byte{0x20, 0xff, 0xff, 0xff, 0xff, 0x01}}, "protocol error in CONNACK: remaining length longer than 4 bytes"},
		{"packet over 64 KiB", script{connack: []byte{0x20, 0x81, 0x80, 0x04}}, "protocol error in CONNACK: packet of 65537 bytes"},
		{"session present", script{connack: []byte{0x20, 0x02, 0x01, 0x00}}, "protocol error in CONNACK: CONNACK with session present"},
		{"long CONNACK", script{connack: []byte{0x20, 0x03, 0x00, 0x00, 0x00}}, "protocol error in CONNACK: malformed CONNACK"},
		{"CONNACK instead of PUBACK", script{connack: accepted, puback: accepted}, "protocol error in PUBACK: expected PUBACK"},
		{"PUBACK for another id", script{connack: accepted, puback: []byte{0x40, 0x02, 0x00, 0x02}}, "protocol error in PUBACK: PUBACK for another packet identifier"},
		{"truncated CONNACK", script{connack: []byte{0x20, 0x02, 0x00}}, "waiting for CONNACK"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBroker(t, tc.sc, nil)
			s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
			req := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases", "timeout": "1s"}, nil)
			err := deliver(t, s, req)
			wantClass(t, err, false, tc.class)
		})
	}
}

// A broker that closes the connection after CONNECT is a retried
// "connection reset".
func TestDeliverClosedByBroker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _, _ = readPacket(bufio.NewReader(c))
			_ = c.Close()
		}
	}()
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	err = deliver(t, s, request(t, map[string]any{"url": "mqtt://" + ln.Addr().String(), "topic": "releases"}, nil))
	wantClass(t, err, false, "connection reset waiting for CONNACK")
}

// AC: connection refused is retried.
func TestDeliverConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	err = deliver(t, s, request(t, map[string]any{"url": "mqtt://" + addr, "topic": "releases"}, nil))
	wantClass(t, err, false, "connection refused (connect)")
}

// AC: a TLS handshake failure (an untrusted certificate, or a broker that
// does not speak TLS) is retried, and nothing is published.
func TestDeliverTLSFailures(t *testing.T) {
	cert, _ := testCert(t)
	b := newBroker(t, script{connack: accepted, puback: puback1}, cert)
	s := New(guarded(t, loopback), Options{}) // system roots only
	req := request(t, map[string]any{"url": "mqtts://127.0.0.1:" + b.port, "topic": "releases", "username": testUsername, "password_ref": "p"},
		map[string]secrets.Value{PasswordRefPointer: secrets.NewValueString(testPassword)})
	err := deliver(t, s, req)
	wantClass(t, err, false, "TLS handshake failed")
	if bytes.Contains(b.rawBytes(), []byte(testPassword)) {
		t.Fatal("the password was sent before the handshake succeeded")
	}

	plain := newBroker(t, script{connack: accepted}, nil)
	_, pool := testCert(t)
	s = New(guarded(t, loopback), Options{RootCAs: pool})
	err = deliver(t, s, request(t, map[string]any{"url": "mqtts://127.0.0.1:" + plain.port, "topic": "releases", "timeout": "1s"}, nil))
	if err == nil || sink.IsPermanent(err) {
		t.Fatalf("TLS to a plain broker: %v", err)
	}
	if got := plain.session(t); got.published {
		t.Fatal("published over a failed handshake")
	}
}

// AC: an unresolved or empty password is retried; the password is read
// only from Request.Secrets.
func TestDeliverPasswordSecrets(t *testing.T) {
	b := newBroker(t, script{connack: accepted, puback: puback1}, nil)
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	cfg := map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases", "username": testUsername, "password_env": "MQTT_PASSWORD"}
	t.Setenv("MQTT_PASSWORD", "from-the-environment")
	wantClass(t, deliver(t, s, request(t, cfg, nil)), false, "password_env MQTT_PASSWORD: not resolved")
	wantClass(t, deliver(t, s, request(t, cfg, map[string]secrets.Value{PasswordPointer: secrets.NewValue(nil)})), false, "password_env MQTT_PASSWORD: empty password")
	long := secrets.NewValue(make([]byte, 65536))
	wantClass(t, deliver(t, s, request(t, cfg, map[string]secrets.Value{PasswordPointer: long})), true, "longer than 65535")
	if b.conns.Load() != 0 {
		t.Fatal("dialled without a usable password")
	}
	// A username without a password sends only the user name.
	delete(cfg, "password_env")
	if err := deliver(t, s, request(t, cfg, nil)); err != nil {
		t.Fatal(err)
	}
	if got := b.session(t); got.flags != 0x82 || got.username != testUsername {
		t.Fatalf("flags %#x user %q", got.flags, got.username)
	}
}

// AC: an invalid configuration, a topic template that fails, and a
// rendered topic that is not a valid topic name are permanent.
func TestDeliverPermanentConfigAndTopic(t *testing.T) {
	b := newBroker(t, script{connack: accepted, puback: puback1}, nil)
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	url := "mqtt://127.0.0.1:" + b.port
	for _, tc := range []struct {
		cfg   map[string]any
		class string
	}{
		{map[string]any{"url": url, "topic": "t", "qos": 2}, "invalid configuration: qos"},
		{map[string]any{"url": url}, "invalid configuration: topic"},
		{map[string]any{"url": url, "topic": "{{"}, "topic template"},
		{map[string]any{"url": url, "topic": "{{.data.nosuchfield.x}}"}, "topic template"},
		{map[string]any{"url": url, "topic": "a/{{.data.source}}/+"}, "must not contain +, # or U+0000"},
		{map[string]any{"url": url, "topic": "$SYS/{{.data.source}}"}, "must not start with $"},
	} {
		_, err := s.Deliver(t.Context(), request(t, tc.cfg, nil))
		if err == nil || !sink.IsPermanent(err) || !strings.Contains(err.Error(), tc.class) {
			t.Errorf("%v: got %v, want permanent %q", tc.cfg, err, tc.class)
		}
	}
	if b.conns.Load() != 0 {
		t.Fatal("the broker was dialled")
	}
}

// When Rendered carries no body (a request built without the dispatcher),
// the CloudEvents JSON is the payload.
func TestDeliverDefaultPayload(t *testing.T) {
	b := newBroker(t, script{connack: accepted, puback: puback1}, nil)
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	req := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases"}, nil)
	req.Rendered = sink.Rendered{}
	req.Event.JSON = nil
	if err := deliver(t, s, req); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if got := b.session(t); json.Unmarshal(got.payload, &env) != nil || env["id"] != req.Event.Envelope.ID {
		t.Fatalf("payload %s", got.payload)
	}
}

// AC: the sink leaks no goroutine, on success and on failure.
func TestDeliverNoGoroutineLeak(t *testing.T) {
	b := newBroker(t, script{connack: accepted, puback: puback1}, nil)
	hang := newBroker(t, script{}, nil)
	s := New(guarded(t, loopback), Options{AllowPlaintextHosts: []string{"127.0.0.1"}})
	ok := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + b.port, "topic": "releases"}, nil)
	stuck := request(t, map[string]any{"url": "mqtt://127.0.0.1:" + hang.port, "topic": "releases"}, nil)
	_ = deliver(t, s, ok) // warm up lazily started runtime goroutines
	b.session(t)
	base := runtime.NumGoroutine()
	for range 5 {
		if err := deliver(t, s, ok); err != nil {
			t.Fatal(err)
		}
		b.session(t)
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(20*time.Millisecond, cancel)
		_, _ = s.Deliver(ctx, stuck)
		hang.session(t)
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines %d > %d\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClientID(t *testing.T) {
	a, b := clientID("angelia"), clientID("angelia")
	if !clientIDRE.MatchString(a) || !strings.HasPrefix(a, "angelia") || a == b {
		t.Fatalf("client ids %q %q", a, b)
	}
	if got := clientID("abcdefghijklmno"); len(got) != 23 {
		t.Fatalf("15-character prefix: %q", got)
	}
}

func TestNewPanicsOnNilDialer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New(nil) did not panic")
		}
	}()
	New(nil, Options{})
}

func TestKind(t *testing.T) {
	if New(guarded(t, nil), Options{}).Kind() != "mqtt" {
		t.Fatal("kind")
	}
}
