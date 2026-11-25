package mqtt

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MQTT 3.1.1 control packet types (OASIS mqtt-v3.1.1 §2.2.1), in the high
// nibble of the first byte.
const (
	typeConnect    = 1
	typeConnack    = 2
	typePublish    = 3
	typePuback     = 4
	typeDisconnect = 14
)

const (
	protocolLevel = 4 // MQTT 3.1.1
	// keepAlive is the keep-alive of CONNECT, in seconds. A connection lives
	// for one delivery attempt, so no PINGREQ is ever sent.
	keepAlive = 30
	// packetID is the identifier of the one QoS 1 PUBLISH of a connection.
	packetID = 1
	// maxIncoming caps the remaining length of a packet read from the
	// broker. A publisher only reads CONNACK and PUBACK (2 bytes each).
	maxIncoming = 64 << 10
	// maxStringLen is the longest UTF-8 string or binary field of MQTT.
	maxStringLen = 65535
	// maxRemainingLength is the largest remaining length four varint
	// bytes can carry (§2.2.3).
	maxRemainingLength = 268435455
)

// CONNACK return codes (§3.2.2.3).
const (
	connAccepted          = 0
	connBadProtocol       = 1
	connIDRejected        = 2
	connServerUnavailable = 3
	connBadCredentials    = 4
	connNotAuthorized     = 5
)

// errProtocol is wrapped by every decoding error: the broker sent
// something a 3.1.1 publisher does not expect. Its messages never quote the
// bytes received.
var errProtocol = errors.New("protocol error")

func protocolError(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{errProtocol}, a...)...)
}

// connectPacket is what CONNECT carries. CleanSession is always set and no
// will is sent.
type connectPacket struct {
	clientID    string
	username    string
	hasUsername bool
	password    []byte
	hasPassword bool
}

// appendRemainingLength appends n as a variable byte integer (§2.2.3).
func appendRemainingLength(b []byte, n int) []byte {
	for {
		d := byte(n % 128) //nolint:gosec // G115: n % 128 is below 128.
		n /= 128
		if n > 0 {
			d |= 0x80
		}
		b = append(b, d)
		if n == 0 {
			return b
		}
	}
}

func appendString(b []byte, s []byte) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s))) //nolint:gosec // G115: callers bound every field to maxStringLen.
	return append(b, s...)
}

// encodeConnect returns the CONNECT packet (§3.1).
func encodeConnect(p connectPacket) ([]byte, error) {
	if len(p.clientID) > maxStringLen || len(p.username) > maxStringLen || len(p.password) > maxStringLen {
		return nil, errors.New("CONNECT field too long")
	}
	if p.hasPassword && !p.hasUsername {
		return nil, errors.New("a password needs a user name") // §3.1.2.9
	}
	flags := byte(0x02) // clean session
	if p.hasUsername {
		flags |= 0x80
	}
	if p.hasPassword {
		flags |= 0x40
	}
	vh := make([]byte, 0, 10+2+len(p.clientID)+2+len(p.username)+2+len(p.password))
	vh = appendString(vh, []byte("MQTT"))
	vh = append(vh, protocolLevel, flags)
	vh = binary.BigEndian.AppendUint16(vh, keepAlive)
	vh = appendString(vh, []byte(p.clientID))
	if p.hasUsername {
		vh = appendString(vh, []byte(p.username))
	}
	if p.hasPassword {
		vh = appendString(vh, p.password)
	}
	out := appendRemainingLength([]byte{typeConnect << 4}, len(vh))
	return append(out, vh...), nil
}

// encodePublish returns a PUBLISH packet (§3.3) with packet identifier
// packetID when qos is 1.
func encodePublish(topic string, payload []byte, qos byte, retain bool) ([]byte, error) {
	if len(topic) == 0 || len(topic) > maxStringLen {
		return nil, errors.New("PUBLISH topic length out of range")
	}
	if qos > 1 {
		return nil, errors.New("PUBLISH QoS must be 0 or 1")
	}
	first := byte(typePublish<<4) | qos<<1
	if retain {
		first |= 0x01
	}
	n := 2 + len(topic) + len(payload)
	if qos > 0 {
		n += 2
	}
	if n > maxRemainingLength {
		return nil, errors.New("PUBLISH payload too large")
	}
	out := appendRemainingLength([]byte{first}, n)
	out = appendString(out, []byte(topic))
	if qos > 0 {
		out = binary.BigEndian.AppendUint16(out, packetID)
	}
	return append(out, payload...), nil
}

// encodeDisconnect returns the DISCONNECT packet (§3.14).
func encodeDisconnect() []byte { return []byte{typeDisconnect << 4, 0} }

// packet is a decoded CONNACK or PUBACK.
type packet struct {
	typ        byte
	returnCode byte   // CONNACK
	id         uint16 // PUBACK
}

// readRemainingLength reads a variable byte integer of at most four bytes.
func readRemainingLength(r io.ByteReader) (int, error) {
	n, mult := 0, 1
	for i := range 4 {
		b, err := r.ReadByte()
		if err != nil {
			if i > 0 && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		n += int(b&0x7f) * mult
		if b&0x80 == 0 {
			return n, nil
		}
		mult *= 128
	}
	return 0, protocolError("remaining length longer than 4 bytes")
}

// decode reads one packet from r and accepts only a well-formed CONNACK or
// PUBACK. Anything else is an error wrapping errProtocol; a short read is
// the reader's error (io.EOF, io.ErrUnexpectedEOF, a timeout).
func decode(r *bufio.Reader) (packet, error) {
	first, err := r.ReadByte()
	if err != nil {
		return packet{}, err
	}
	n, err := readRemainingLength(r)
	if err != nil {
		return packet{}, err
	}
	if n > maxIncoming {
		return packet{}, protocolError("packet of %d bytes exceeds the %d byte limit", n, maxIncoming)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return packet{}, err
	}
	typ, flags := first>>4, first&0x0f
	switch typ {
	case typeConnack:
		if flags != 0 || n != 2 {
			return packet{}, protocolError("malformed CONNACK")
		}
		if body[0]&0xfe != 0 {
			return packet{}, protocolError("CONNACK with reserved flags set")
		}
		if body[0]&0x01 != 0 {
			return packet{}, protocolError("CONNACK with session present for a clean session")
		}
		return packet{typ: typ, returnCode: body[1]}, nil
	case typePuback:
		if flags != 0 || n != 2 {
			return packet{}, protocolError("malformed PUBACK")
		}
		return packet{typ: typ, id: binary.BigEndian.Uint16(body)}, nil
	default:
		return packet{}, protocolError("unexpected packet type %d", typ)
	}
}
