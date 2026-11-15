// Package apitoken holds Angelia's API tokens (story #72, ADR 0030): the
// token format (T1), its hash at rest (T2) and the [api.Authenticator] that
// resolves a bearer token to its principal (T10) and records last use (T6).
//
// A token is
//
//	ang_<id>_<secret><check>
//
// where id is the token's ULID (26 upper-case Crockford base32 characters),
// secret is 32 random bytes as 52 lower-case RFC 4648 base32 characters
// without padding, and check is the CRC-32 (IEEE) of the ASCII bytes of
// everything before it, as 4 big-endian bytes in lower-case base32 without
// padding (7 characters). The token is 90 characters long.
package apitoken

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/ulid"
)

// Prefix starts every token of this format. A later format gets a new
// prefix (ADR 0030 T1).
const Prefix = "ang_"

// Lengths of the parts of a token.
const (
	idLen     = 26
	secretLen = 52
	checkLen  = 7
	// Len is the length of a token.
	Len = len(Prefix) + idLen + 1 + secretLen + checkLen
)

// hashDomain separates the token hash from any other SHA-256 of the same
// bytes (ADR 0030 T2).
const hashDomain = "angelia api token v1\x00"

// ErrMalformed is returned by Parse for a string that is not a token of this
// format: wrong prefix, length or alphabet, an id that is not a strict ULID,
// or a checksum mismatch. Its message never contains the string.
var ErrMalformed = errors.New("apitoken: malformed token")

// b32 is lower-case RFC 4648 base32 without padding.
var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Generate returns a new token and its id, the token's ULID for the instant
// now. The secret is 32 bytes from crypto/rand.
func Generate(now time.Time) (id, token string) {
	var secret [32]byte
	_, _ = rand.Read(secret[:]) // crypto/rand.Read does not fail
	id = ulid.New(now)
	body := Prefix + id + "_" + b32.EncodeToString(secret[:])
	return id, body + checksum(body)
}

// Parse checks token strictly and returns its id. It reads nothing but the
// string, so a malformed token costs no store access (ADR 0030 T1). The
// secret part is checked for its alphabet only, as the T1 pattern
// [a-z2-7]{59} does: its last character is not required to be one a 32-byte
// encoding can end with.
func Parse(token string) (id string, err error) {
	if len(token) != Len || !strings.HasPrefix(token, Prefix) {
		return "", ErrMalformed
	}
	id = token[len(Prefix) : len(Prefix)+idLen]
	rest := token[len(Prefix)+idLen:]
	if !ulid.Valid(id) || rest[0] != '_' {
		return "", ErrMalformed
	}
	for i := 1; i < len(rest); i++ {
		if c := rest[i]; (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return "", ErrMalformed
		}
	}
	body := token[:Len-checkLen]
	if checksum(body) != token[Len-checkLen:] {
		return "", ErrMalformed
	}
	return id, nil
}

// Hash is the stored form of a token (ADR 0030 T2):
// SHA-256("angelia api token v1" || 0x00 || token), 32 bytes.
func Hash(token string) []byte {
	h := sha256.New()
	h.Write([]byte(hashDomain))
	h.Write([]byte(token))
	return h.Sum(nil)
}

// checksum is the CRC-32 (IEEE) of body as 4 big-endian bytes in lower-case
// base32 without padding: 7 characters.
func checksum(body string) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], crc32.ChecksumIEEE([]byte(body)))
	return b32.EncodeToString(b[:])
}
