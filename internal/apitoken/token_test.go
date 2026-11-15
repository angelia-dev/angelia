package apitoken

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
)

// specExample is the spec's example token (see internal/apitoken/format).
const specExample = "ang_01JCZ8W3T6Q9N2H4K7M5R0V8XA_" +
	"fakefakefakefakefakefakefakefakefakefakefakefakefake" + "7qt7acq"

// The re-exports are the format's, and config refuses what Generate makes.
func TestFormatReexports(t *testing.T) {
	id, tok := Generate(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC))
	if got, err := Parse(tok); err != nil || got != id || len(tok) != Len || tok[:len(Prefix)] != Prefix {
		t.Fatalf("Parse(Generate()) = %q, %v", got, err)
	}
	if !config.LooksLikeToken(tok) || !config.LooksLikeToken(specExample) {
		t.Fatal("config.LooksLikeToken does not recognise a token")
	}
	if _, err := Parse("ang_team"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Parse(ang_team) = %v", err)
	}
}

func TestHash(t *testing.T) {
	h := Hash(specExample)
	want := sha256.Sum256([]byte("angelia api token v1\x00" + specExample))
	if !bytes.Equal(h, want[:]) || len(h) != 32 {
		t.Fatalf("Hash = %x, want %x", h, want)
	}
	plain := sha256.Sum256([]byte(specExample))
	if bytes.Equal(h, plain[:]) {
		t.Fatal("the hash has no domain separation")
	}
	if dummy := Hash(Prefix + "dummy"); !bytes.Equal(dummy, dummyHash[:]) {
		t.Fatal("dummyHash is not a token hash")
	}
}
