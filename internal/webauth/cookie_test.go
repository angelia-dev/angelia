package webauth

import (
	"crypto/sha256"
	"strings"
	"testing"
	"time"
)

func TestCookieFormat(t *testing.T) {
	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	id, v := NewCookie(now)
	if len(v) != CookieLen || CookieLen != 84 || !strings.HasPrefix(v, "angs_"+id+"_") {
		t.Fatalf("%d %q", len(v), v[:31])
	}
	got, err := ParseCookie(v)
	if err != nil || got != id {
		t.Fatalf("ParseCookie = %q, %v", got, err)
	}
	_, v2 := NewCookie(now)
	if v2 == v {
		t.Fatal("two cookies are equal")
	}
	for name, bad := range map[string]string{
		"empty":       "",
		"prefix":      "angx_" + v[5:],
		"short":       v[:len(v)-1],
		"long":        v + "a",
		"upper":       v[:len(v)-1] + "A",
		"digit 1":     v[:len(v)-1] + "1",
		"no sep":      v[:31] + "a" + v[32:],
		"id lower":    "angs_" + strings.ToLower(id) + v[31:],
		"token shape": "ang_" + v[5:] + "x",
	} {
		if _, err := ParseCookie(bad); err == nil {
			t.Errorf("%s: accepted", name)
		} else if bad != "" && strings.Contains(err.Error(), bad) {
			t.Errorf("%s: the error holds the value", name)
		}
	}
	h := HashCookie(v)
	if h == sha256.Sum256([]byte(v)) || h != sha256.Sum256([]byte("angelia ui session v1\x00"+v)) {
		t.Fatal("the hash is not domain-separated as specified")
	}
}
