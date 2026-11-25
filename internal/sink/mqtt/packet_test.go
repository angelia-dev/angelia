package mqtt

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"testing"
)

// Byte vectors from the OASIS MQTT 3.1.1 specification (mqtt-v3.1.1-os).

// §2.2.3, Table 2.4: the bounds of each remaining length size.
func TestRemainingLengthVectors(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want []byte
	}{
		{0, []byte{0x00}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{16383, []byte{0xff, 0x7f}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{2097151, []byte{0xff, 0xff, 0x7f}},
		{2097152, []byte{0x80, 0x80, 0x80, 0x01}},
		{268435455, []byte{0xff, 0xff, 0xff, 0x7f}},
		{321, []byte{0xc1, 0x02}}, // the worked example of §2.2.3
	} {
		if got := appendRemainingLength(nil, tc.n); !bytes.Equal(got, tc.want) {
			t.Errorf("encode %d = % x, want % x", tc.n, got, tc.want)
		}
		got, err := readRemainingLength(bytes.NewReader(tc.want))
		if err != nil || got != tc.n {
			t.Errorf("decode % x = %d, %v; want %d", tc.want, got, err, tc.n)
		}
	}
	if _, err := readRemainingLength(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff, 0x01})); !errors.Is(err, errProtocol) {
		t.Fatalf("five-byte remaining length: %v, want errProtocol", err)
	}
	if _, err := readRemainingLength(bytes.NewReader([]byte{0x80})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated remaining length: %v", err)
	}
}

// §3.1.2.1 to §3.1.2.10 (Figure 3.6, the variable header example) with the
// flags this client uses: user name, password, clean session, keep-alive
// 30; §3.1.3 for the payload order (client id, user name, password).
func TestEncodeConnect(t *testing.T) {
	got, err := encodeConnect(connectPacket{clientID: "angeliaAbc", username: "u", hasUsername: true, password: []byte("pw"), hasPassword: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x10, 29, // CONNECT, remaining length
		0x00, 0x04, 'M', 'Q', 'T', 'T', // protocol name
		0x04,       // protocol level 4
		0xc2,       // user name, password, clean session
		0x00, 0x1e, // keep alive 30
		0x00, 0x0a, 'a', 'n', 'g', 'e', 'l', 'i', 'a', 'A', 'b', 'c',
		0x00, 0x01, 'u',
		0x00, 0x02, 'p', 'w',
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("CONNECT\n got % x\nwant % x", got, want)
	}
	anon, err := encodeConnect(connectPacket{clientID: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x10, 13, 0, 4, 'M', 'Q', 'T', 'T', 4, 0x02, 0, 30, 0, 1, 'a'}; !bytes.Equal(anon, want) {
		t.Fatalf("anonymous CONNECT % x, want % x", anon, want)
	}
	if _, err := encodeConnect(connectPacket{clientID: "a", hasPassword: true}); err == nil {
		t.Fatal("a password without a user name was encoded (§3.1.2.9)")
	}
	if _, err := encodeConnect(connectPacket{clientID: "a", username: string(make([]byte, 65536)), hasUsername: true}); err == nil {
		t.Fatal("a 64 KiB user name was encoded")
	}
}

// §3.3.2 (Figure 3.11: topic "a/b", then the packet identifier) and §3.3.1
// (fixed header flags: QoS in bits 2-1, retain in bit 0).
func TestEncodePublish(t *testing.T) {
	got, err := encodePublish("a/b", []byte("hi"), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x32, 9, 0x00, 0x03, 'a', '/', 'b', 0x00, 0x01, 'h', 'i'}; !bytes.Equal(got, want) {
		t.Fatalf("QoS 1 PUBLISH % x, want % x", got, want)
	}
	got, err = encodePublish("a/b", []byte("hi"), 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x31, 7, 0x00, 0x03, 'a', '/', 'b', 'h', 'i'}; !bytes.Equal(got, want) {
		t.Fatalf("QoS 0 retained PUBLISH % x, want % x", got, want)
	}
	big, err := encodePublish("t", make([]byte, 200), 1, false)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(big[:3], []byte{0x32, 0xcd, 0x01}) { // 2+1+2+200 = 205
		t.Fatalf("PUBLISH header % x", big[:3])
	}
	for _, tc := range []struct {
		topic string
		qos   byte
	}{{"", 1}, {"t", 2}} {
		if _, err := encodePublish(tc.topic, nil, tc.qos, false); err == nil {
			t.Errorf("encodePublish(%q, qos %d) accepted", tc.topic, tc.qos)
		}
	}
}

// §3.14: DISCONNECT is two bytes.
func TestEncodeDisconnect(t *testing.T) {
	if got := encodeDisconnect(); !bytes.Equal(got, []byte{0xe0, 0x00}) {
		t.Fatalf("DISCONNECT % x", got)
	}
}

func decodeBytes(b []byte) (packet, error) { return decode(bufio.NewReader(bytes.NewReader(b))) }

// §3.2 (CONNACK) and §3.4 (PUBACK).
func TestDecode(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
		want packet
	}{
		{"CONNACK accepted", []byte{0x20, 0x02, 0x00, 0x00}, packet{typ: typeConnack}},
		{"CONNACK bad credentials", []byte{0x20, 0x02, 0x00, 0x04}, packet{typ: typeConnack, returnCode: 4}},
		{"PUBACK 1", []byte{0x40, 0x02, 0x00, 0x01}, packet{typ: typePuback, id: 1}},
		{"PUBACK 513", []byte{0x40, 0x02, 0x02, 0x01}, packet{typ: typePuback, id: 513}},
	} {
		got, err := decodeBytes(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v", tc.name, got, err)
		}
	}
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"CONNACK session present", []byte{0x20, 0x02, 0x01, 0x00}},
		{"CONNACK reserved ack flags", []byte{0x20, 0x02, 0x02, 0x00}},
		{"CONNACK header flags", []byte{0x21, 0x02, 0x00, 0x00}},
		{"CONNACK too long", []byte{0x20, 0x03, 0x00, 0x00, 0x00}},
		{"PUBACK too short", []byte{0x40, 0x01, 0x00}},
		{"PUBACK header flags", []byte{0x42, 0x02, 0x00, 0x01}},
		{"PUBLISH", []byte{0x30, 0x03, 0x00, 0x01, 'a'}},
		{"PINGRESP", []byte{0xd0, 0x00}},
		{"reserved type 0", []byte{0x00, 0x00}},
		{"remaining length 5 bytes", []byte{0x20, 0xff, 0xff, 0xff, 0xff, 0x01}},
		{"remaining length over 64 KiB", []byte{0x20, 0x81, 0x80, 0x04}},
	} {
		if _, err := decodeBytes(tc.in); !errors.Is(err, errProtocol) {
			t.Errorf("%s: got %v, want errProtocol", tc.name, err)
		}
	}
	for _, in := range [][]byte{{}, {0x20}, {0x20, 0x02, 0x00}} {
		if _, err := decodeBytes(in); err == nil || errors.Is(err, errProtocol) {
			t.Errorf("% x: got %v, want a read error", in, err)
		}
	}
}

func FuzzDecode(f *testing.F) {
	for _, seed := range [][]byte{
		{0x20, 0x02, 0x00, 0x00},
		{0x40, 0x02, 0x00, 0x01},
		{0x20, 0x02, 0x01, 0x05},
		{0x20, 0xff, 0xff, 0xff, 0x7f},
		{0x30, 0x00},
		{0xe0, 0x00},
		{},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		p, err := decodeBytes(in)
		if err != nil {
			return
		}
		switch p.typ {
		case typeConnack:
			if len(in) < 4 || in[0] != 0x20 || in[2] != 0 {
				t.Fatalf("accepted a malformed CONNACK % x", in)
			}
		case typePuback:
			if len(in) < 4 || in[0] != 0x40 {
				t.Fatalf("accepted a malformed PUBACK % x", in)
			}
		default:
			t.Fatalf("decoded type %d from % x", p.typ, in)
		}
	})
}
