package webauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"strings"
	"time"

	"github.com/angelia-dev/angelia/internal/ulid"
)

// The session cookie's value (ADR 0033 §5) is
//
//	angs_<id>_<secret>
//
// where id is the session's ULID (26 upper-case Crockford base32
// characters) and secret 32 random bytes as 52 lower-case RFC 4648 base32
// characters without padding: 84 characters. The store keeps
// SHA-256("angelia ui session v1" || 0x00 || value), ADR 0030 T2's reasoning:
// the secret has 256 bits, so a fast hash is enough, and the domain keeps the
// hash apart from any other SHA-256 of the same bytes.

// CookiePrefix starts every session cookie value.
const CookiePrefix = "angs_"

const (
	idLen     = 26
	secretLen = 52
	// CookieLen is the length of a session cookie value.
	CookieLen = len(CookiePrefix) + idLen + 1 + secretLen
)

// hashDomain separates the session hash from any other SHA-256.
const hashDomain = "angelia ui session v1\x00"

// errMalformed is the error of ParseCookie. Its message never holds the
// value.
var errMalformed = errors.New("webauth: malformed session cookie")

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewCookie returns a new session id, for the instant now, and the cookie
// value that carries it. The secret is 32 bytes from crypto/rand.
func NewCookie(now time.Time) (id, value string) {
	var secret [32]byte
	_, _ = rand.Read(secret[:]) // crypto/rand.Read does not fail
	id = ulid.New(now)
	return id, CookiePrefix + id + "_" + b32.EncodeToString(secret[:])
}

// ParseCookie checks a cookie value strictly and returns its session id. It
// reads nothing but the string, so a malformed value costs no store access.
func ParseCookie(v string) (string, error) {
	if len(v) != CookieLen || !strings.HasPrefix(v, CookiePrefix) {
		return "", errMalformed
	}
	id := v[len(CookiePrefix) : len(CookiePrefix)+idLen]
	rest := v[len(CookiePrefix)+idLen:]
	if !ulid.Valid(id) || rest[0] != '_' {
		return "", errMalformed
	}
	for i := 1; i < len(rest); i++ {
		if c := rest[i]; (c < 'a' || c > 'z') && (c < '2' || c > '7') {
			return "", errMalformed
		}
	}
	return id, nil
}

// HashCookie is the stored form of a session cookie value.
func HashCookie(v string) [32]byte { return sha256.Sum256([]byte(hashDomain + v)) }
