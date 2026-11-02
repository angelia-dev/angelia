// Package ulid generates ULIDs (https://github.com/ulid/spec): 48 bits of
// Unix milliseconds then 80 random bits, written as 26 characters of
// Crockford base32, upper case. It is the id format of watches, events and
// deliveries. The standard library is enough for this, so no ULID module is
// used (architecture.md §6 names oklog/ulid/v2 as an option).
package ulid

import (
	"crypto/rand"
	"time"
)

const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// New returns a ULID for the instant now. Two ids of the same millisecond
// differ in their random part and are not ordered.
func New(now time.Time) string {
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

// Valid reports whether s is a ULID in canonical form: 26 upper-case
// Crockford base32 characters, the first at most 7 (pattern
// ^[0-7][0-9A-HJKMNP-TV-Z]{25}$ of the event schema).
func Valid(s string) bool {
	if len(s) != 26 || s[0] < '0' || s[0] > '7' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z' || c == 'I' || c == 'L' || c == 'O' || c == 'U') {
			return false
		}
	}
	return true
}
