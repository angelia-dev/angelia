package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Standard Webhooks headers (spec "standard-webhooks.md").
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// SecretPrefix starts every serialised secret.
const SecretPrefix = "whsec_"

// Sender-side secret rules (ADR 0023 section 2).
const (
	MinSecretBytes = 24
	MaxSecretBytes = 64
	// MaxSecrets is the number of secrets one value may hold: current,
	// previous and one emergency key during a rotation.
	MaxSecrets = 3
)

// DefaultTolerance is the largest accepted difference between a delivery's
// timestamp and the receiver's clock.
const DefaultTolerance = 5 * time.Minute

var (
	// ErrInvalidSecret is returned for a secret that does not follow the
	// rules. Its message never holds the secret.
	ErrInvalidSecret = errors.New("invalid secret")
	// ErrMissingHeaders is returned when webhook-id, webhook-timestamp or
	// webhook-signature is absent or empty.
	ErrMissingHeaders = errors.New("webhook: missing webhook-id, webhook-timestamp or webhook-signature header")
	// ErrTimestamp is returned when webhook-timestamp is not a decimal Unix
	// time, or is outside the tolerance.
	ErrTimestamp = errors.New("webhook: webhook-timestamp invalid or outside the tolerance")
	// ErrNoMatchingSignature is returned when no v1 signature matches any
	// secret.
	ErrNoMatchingSignature = errors.New("webhook: no matching signature")
)

// secretRule is the message of every sender-side parsing error. It never
// includes the value or a decode error (which can quote the input).
const secretRule = "want 1-3 whsec_ secrets of 24-64 bytes, padded standard base64" //nolint:gosec // G101: a rule, not a credential.

// ParseSecrets parses a secret value: one to three space-separated secrets,
// each "whsec_" plus strict padded standard base64 of 24 to 64 bytes, not a
// single repeated byte, no duplicates. Leading and trailing white space,
// such as the newline of a mounted file, is ignored. It returns the HMAC
// keys in the order given. Errors wrap ErrInvalidSecret and never quote the
// value.
func ParseSecrets(value string) ([][]byte, error) {
	fields := strings.Fields(strings.TrimSpace(value))
	if len(fields) == 0 || len(fields) > MaxSecrets {
		return nil, invalidSecret(secretRule)
	}
	keys := make([][]byte, 0, len(fields))
	for _, f := range fields {
		rest, ok := strings.CutPrefix(f, SecretPrefix)
		if !ok {
			return nil, invalidSecret(secretRule)
		}
		key, err := base64.StdEncoding.Strict().DecodeString(rest)
		if err != nil || len(key) < MinSecretBytes || len(key) > MaxSecretBytes {
			return nil, invalidSecret(secretRule)
		}
		if oneByte(key) {
			return nil, invalidSecret("a key of one repeated byte looks like a placeholder; generate one with `angelia webhook secret new`")
		}
		for _, k := range keys {
			if hmac.Equal(k, key) {
				return nil, invalidSecret("the same secret is listed twice")
			}
		}
		keys = append(keys, key)
	}
	return keys, nil
}

type secretError struct{ reason string }

func (e secretError) Error() string { return ErrInvalidSecret.Error() + " (" + e.reason + ")" }
func (e secretError) Unwrap() error { return ErrInvalidSecret }

func invalidSecret(reason string) error { return secretError{reason} }

func oneByte(b []byte) bool {
	for _, c := range b[1:] {
		if c != b[0] {
			return false
		}
	}
	return true
}

// Sign returns the webhook-signature header value for a message: one
// "v1,<base64 HMAC-SHA256>" entry per secret, in order, joined by a space.
// The signed content is id + "." + timestamp + "." + body, with timestamp
// the exact webhook-timestamp header string. secrets follow the
// sender-side rules of ParseSecrets, each element holding one secret.
func Sign(secrets []string, id, timestamp string, body []byte) (string, error) {
	if len(secrets) == 0 || len(secrets) > MaxSecrets {
		return "", invalidSecret(secretRule)
	}
	keys, err := ParseSecrets(strings.Join(secrets, " "))
	if err != nil {
		return "", err
	}
	if len(keys) != len(secrets) {
		return "", invalidSecret(secretRule)
	}
	return sign(keys, id, timestamp, body), nil
}

func sign(keys [][]byte, id, timestamp string, body []byte) string {
	entries := make([]string, len(keys))
	for i, k := range keys {
		entries[i] = "v1," + base64.StdEncoding.EncodeToString(mac(k, id, timestamp, body))
	}
	return strings.Join(entries, " ")
}

func mac(key []byte, id, timestamp string, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(id))
	h.Write([]byte{'.'})
	h.Write([]byte(timestamp))
	h.Write([]byte{'.'})
	h.Write(body)
	return h.Sum(nil)
}

// Verify checks a delivery as a receiver would: the three headers must be
// present, webhook-timestamp must be ASCII digits fitting an int64 within
// tolerance of now (inclusive at both ends), and one v1 entry of
// webhook-signature must match one of secrets. Secrets may carry the
// "whsec_" prefix or not, and be any non-empty standard base64 key, like
// the reference libraries. Entries that are not "v1," or not valid base64
// are ignored. Errors wrap ErrMissingHeaders, ErrTimestamp,
// ErrNoMatchingSignature or ErrInvalidSecret.
func Verify(secrets []string, h http.Header, body []byte, now time.Time, tolerance time.Duration) error {
	id, ts, sigs := h.Get(HeaderID), h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if id == "" || ts == "" || sigs == "" {
		return ErrMissingHeaders
	}
	if len(secrets) == 0 {
		return invalidSecret("no secret")
	}
	keys := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		k, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, SecretPrefix))
		if err != nil || len(k) == 0 {
			return invalidSecret("want non-empty standard base64, optionally prefixed with whsec_")
		}
		keys = append(keys, k)
	}
	if strings.Trim(ts, "0123456789") != "" {
		return ErrTimestamp
	}
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrTimestamp
	}
	if d := now.Sub(time.Unix(secs, 0)); d > tolerance || d < -tolerance {
		return ErrTimestamp
	}
	for _, entry := range strings.Split(sigs, " ") {
		b64, ok := strings.CutPrefix(entry, "v1,")
		if !ok {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		for _, k := range keys {
			if hmac.Equal(got, mac(k, id, ts, body)) {
				return nil
			}
		}
	}
	return ErrNoMatchingSignature
}
