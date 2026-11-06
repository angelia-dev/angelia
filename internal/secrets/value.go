package secrets

import (
	"bytes"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
)

// Redacted is how a Value prints, logs and encodes.
const Redacted = "[REDACTED]"

// Value holds a plaintext secret: a stored secret's value, or a token read
// from the environment. It prints as [REDACTED] under every fmt verb, in
// slog and in JSON or text encodings, so a struct holding one can be logged
// safely. The zero Value is empty.
type Value struct {
	b []byte
}

// NewValue returns a Value holding a copy of b.
func NewValue(b []byte) Value { return Value{b: bytes.Clone(b)} }

// NewValueString returns a Value holding s.
func NewValueString(s string) Value { return Value{b: []byte(s)} }

// Reveal returns a copy of the plaintext, so that a caller cannot change the
// held value. Call it only where a request is built.
func (v Value) Reveal() []byte { return bytes.Clone(v.b) }

// Len is the length of the plaintext in bytes.
func (v Value) Len() int { return len(v.b) }

// IsZero reports whether the value is empty.
func (v Value) IsZero() bool { return len(v.b) == 0 }

// Equal compares the plaintext with b in constant time (for equal lengths).
func (v Value) Equal(b []byte) bool { return subtle.ConstantTimeCompare(v.b, b) == 1 }

// String implements fmt.Stringer: it never prints the value.
func (Value) String() string { return Redacted }

// GoString implements fmt.GoStringer: it never prints the value.
func (Value) GoString() string { return Redacted }

// LogValue implements slog.LogValuer: it never logs the value.
func (Value) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON encodes the value as "[REDACTED]".
func (Value) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }

// MarshalText encodes the value as [REDACTED].
func (Value) MarshalText() ([]byte, error) { return []byte(Redacted), nil }

// Format implements fmt.Formatter: every verb prints [REDACTED].
func (Value) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, Redacted) }
