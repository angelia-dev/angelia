package ulid

import (
	"regexp"
	"testing"
	"time"
)

var pattern = regexp.MustCompile(`^[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

func TestNew(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	a, b := New(now), New(now)
	if a == b {
		t.Fatalf("two ids of one millisecond are equal: %s", a)
	}
	for _, id := range []string{a, b} {
		if !pattern.MatchString(id) || !Valid(id) {
			t.Errorf("New = %q, not a canonical ULID", id)
		}
	}
	if a[:10] != b[:10] {
		t.Errorf("time parts differ: %s %s", a, b)
	}
	if later := New(now.Add(time.Millisecond)); later[:10] <= a[:10] {
		t.Errorf("a later id sorts first: %s <= %s", later, a)
	}
	// The ULID spec's example time part: 1469918176385 ms is 01ARYZ6S41.
	if got := New(time.UnixMilli(1469918176385))[:10]; got != "01ARYZ6S41" {
		t.Errorf("time part = %s, want 01ARYZ6S41", got)
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"01JBX1Z3M4N5P6Q7R8S9T0V1W2":  true,
		"7ZZZZZZZZZZZZZZZZZZZZZZZZZ":  true,
		"81JBX1Z3M4N5P6Q7R8S9T0V1W2":  false, // overflows 128 bits
		"01jbx1z3m4n5p6q7r8s9t0v1w2":  false, // lower case
		"01JBX1Z3M4N5P6Q7R8S9T0V1WI":  false, // I is not Crockford
		"01JBX1Z3M4N5P6Q7R8S9T0V1W":   false,
		"01JBX1Z3M4N5P6Q7R8S9T0V1W2A": false,
		"01JBX1Z3M4N5P6Q7R8S9T0V1W-":  false,
		"":                            false,
	} {
		if got := Valid(s); got != want || got != pattern.MatchString(s) {
			t.Errorf("Valid(%q) = %v, want %v", s, got, want)
		}
	}
}
