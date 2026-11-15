package apitoken

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/ulid"
)

// specExample is the example token of the spec (docs/api.md) and of ADR 0030
// T1: the right shape and a valid checksum, not a real token. It is the test
// vector of the checksum encoding.
const specExample = "ang_01JCZ8W3T6Q9N2H4K7M5R0V8XA_" +
	"fakefakefakefakefakefakefakefakefakefakefakefakefake" + "7qt7acq"

func TestSpecExampleParses(t *testing.T) {
	id, err := Parse(specExample)
	if err != nil {
		t.Fatalf("Parse(spec example): %v", err)
	}
	if id != "01JCZ8W3T6Q9N2H4K7M5R0V8XA" {
		t.Fatalf("id = %q", id)
	}
	if got := checksum(specExample[:Len-checkLen]); got != "7qt7acq" {
		t.Fatalf("checksum = %q, want 7qt7acq", got)
	}
	if len(specExample) != 90 || Len != 90 {
		t.Fatalf("length %d, Len %d", len(specExample), Len)
	}
}

func TestGenerate(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for range 200 {
		id, tok := Generate(now)
		if len(tok) != Len || !strings.HasPrefix(tok, Prefix+id+"_") || !ulid.Valid(id) {
			t.Fatalf("token %q, id %q", tok, id)
		}
		got, err := Parse(tok)
		if err != nil || got != id {
			t.Fatalf("Parse(Generate()) = %q, %v; want %q", got, err, id)
		}
		// 32 bytes in 52 characters: the last secret character carries one
		// bit and four zero bits.
		if c := tok[Len-checkLen-1]; c != 'a' && c != 'q' {
			t.Fatalf("last secret character %q", c)
		}
		if !config.LooksLikeToken(tok) || !config.LooksLikeToken("token: "+tok) {
			t.Fatal("config.LooksLikeToken does not recognise a generated token")
		}
		if seen[tok] {
			t.Fatal("duplicate token")
		}
		seen[tok] = true
	}
	// The id carries the creation time.
	if id, _ := Generate(now); id[:10] != ulid.New(now)[:10] {
		t.Fatalf("id %q is not of %s", id, now)
	}
}

func TestParseRejects(t *testing.T) {
	ok := specExample
	body := ok[:Len-checkLen]
	withCheck := func(b string) string { return b + checksum(b) }
	// Mutations that keep a valid checksum: only the strict parse refuses
	// them.
	strict := map[string]string{
		"wrong prefix":      withCheck("anx_" + body[4:]),
		"upper-case prefix": withCheck("ANG_" + body[4:]),
		"id lower case":     withCheck(Prefix + strings.ToLower(body[4:30]) + body[30:]),
		"id first char 8":   withCheck(Prefix + "8" + body[5:]),
		"id with I":         withCheck(Prefix + "I" + body[5:]),
		"id with U":         withCheck(Prefix + body[4:29] + "U" + body[30:]),
		"id with L":         withCheck(Prefix + body[4:29] + "L" + body[30:]),
		"id with O":         withCheck(Prefix + body[4:29] + "O" + body[30:]),
		"separator":         withCheck(body[:30] + "-" + body[31:]),
		"secret upper case": withCheck(body[:31] + "F" + body[32:]),
		"secret digit 1":    withCheck(body[:31] + "1" + body[32:]),
		"secret digit 8":    withCheck(body[:31] + "8" + body[32:]),
		"secret padding":    withCheck(body[:len(body)-1] + "="),
		"non-ASCII":         withCheck(body[:31] + "é" + body[33:]),
	}
	for name, tok := range strict {
		if len(tok) != Len {
			t.Fatalf("%s: test token has length %d", name, len(tok))
		}
	}
	other := map[string]string{
		"empty":             "",
		"too short":         ok[:Len-1],
		"too long":          ok + "a",
		"check upper case":  body + strings.ToUpper(ok[Len-checkLen:]),
		"check mismatch":    body + "aaaaaaa",
		"typo in secret":    body[:40] + "x" + body[41:] + ok[Len-checkLen:],
		"typo in id":        body[:10] + "Z" + body[11:] + ok[Len-checkLen:],
		"surrounding space": " " + ok[:Len-1],
	}
	for _, cases := range []map[string]string{strict, other} {
		for name, tok := range cases {
			id, err := Parse(tok)
			if !errors.Is(err, ErrMalformed) || id != "" {
				t.Errorf("%s: Parse = %q, %v; want ErrMalformed", name, id, err)
			}
			if err != nil && strings.Contains(err.Error(), "fake") {
				t.Errorf("%s: the error shows the token", name)
			}
		}
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
}
