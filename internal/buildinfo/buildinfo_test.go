package buildinfo

import "testing"

func TestDefaultsWithoutLdflags(t *testing.T) {
	if version != "dev" || commit != "none" || date != "unknown" {
		t.Skipf("build metadata injected via -ldflags: %s", String())
	}
	if Version() != "dev" || Commit() != "none" || Date() != "unknown" {
		t.Fatalf("getters = %q %q %q, want dev none unknown", Version(), Commit(), Date())
	}
	want := "dev (commit none, built unknown)"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestGettersReturnInjectedValues(t *testing.T) {
	oldV, oldC, oldD := version, commit, date
	t.Cleanup(func() { version, commit, date = oldV, oldC, oldD })

	version, commit, date = "v1.2.3", "abc1234", "2026-01-02T03:04:05Z"
	if Version() != "v1.2.3" || Commit() != "abc1234" || Date() != "2026-01-02T03:04:05Z" {
		t.Fatalf("getters = %q %q %q", Version(), Commit(), Date())
	}
	want := "v1.2.3 (commit abc1234, built 2026-01-02T03:04:05Z)"
	if got := String(); got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
