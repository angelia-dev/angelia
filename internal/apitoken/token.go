// Package apitoken holds Angelia's API tokens (story #72, ADR 0030): the
// token format (T1, in internal/apitoken/format and re-exported here), its
// hash at rest (T2) and the [api.Authenticator] that resolves a bearer token
// to its principal (T10) and records last use (T6).
package apitoken

import (
	"crypto/sha256"
	"time"

	"github.com/angelia-dev/angelia/internal/apitoken/format"
)

// Prefix starts every token of this format ([format.Prefix]).
const Prefix = format.Prefix

// Len is the length of a token ([format.Len]).
const Len = format.Len

// ErrMalformed is the error of Parse ([format.ErrMalformed]).
var ErrMalformed = format.ErrMalformed

// Generate returns a new token and its id ([format.Generate]).
func Generate(now time.Time) (id, token string) { return format.Generate(now) }

// Parse checks a token strictly and returns its id ([format.Parse]).
func Parse(token string) (string, error) { return format.Parse(token) }

// hashDomain separates the token hash from any other SHA-256 of the same
// bytes (ADR 0030 T2).
const hashDomain = "angelia api token v1\x00"

// Hash is the stored form of a token (ADR 0030 T2):
// SHA-256("angelia api token v1" || 0x00 || token), 32 bytes.
func Hash(token string) []byte {
	h := hashArray(token)
	return h[:]
}

func hashArray(token string) [32]byte {
	return sha256.Sum256([]byte(hashDomain + token))
}
