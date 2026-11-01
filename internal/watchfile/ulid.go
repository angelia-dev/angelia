package watchfile

import (
	"crypto/rand"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a ULID string: 48 bits of Unix milliseconds then 80 random
// bits, Crockford base32, 26 characters.
func newULID(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli()) //nolint:gosec // timestamps are positive
	for i := 5; i >= 0; i-- {
		b[i] = byte(ms)
		ms >>= 8
	}
	_, _ = rand.Read(b[6:]) // crypto/rand.Read does not fail
	var out [26]byte
	// 128 bits as 26 groups of 5 bits, the first group holding 3 bits.
	var acc uint32
	var bits uint
	pos := 25
	for i := 15; i >= 0; i-- {
		acc |= uint32(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[pos] = crockford[acc&31]
			pos--
			acc >>= 5
			bits -= 5
		}
	}
	if bits > 0 {
		out[pos] = crockford[acc&31]
	}
	return string(out[:])
}
