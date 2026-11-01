package version

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCompareDigits(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"0", "0", 0},
		{"7", "007", 0},
		{"9", "10", -1},
		{"10", "9", 1},
		{"123", "124", -1},
		{"18446744073709551616", "18446744073709551615", 1},
		{"000", "0", 0},
	}
	for _, tt := range tests {
		if got := CompareDigits(tt.a, tt.b); got != tt.want {
			t.Errorf("CompareDigits(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestTrimZeros(t *testing.T) {
	for in, want := range map[string]string{"0": "0", "000": "0", "007": "7", "10": "10", "": ""} {
		if got := TrimZeros(in); got != want {
			t.Errorf("TrimZeros(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsDigits(t *testing.T) {
	for in, want := range map[string]bool{"": false, "0": true, "0123": true, "1a": false, "-1": false, "١": false} {
		if got := IsDigits(in); got != want {
			t.Errorf("IsDigits(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestNatural(t *testing.T) {
	ordered := []string{
		"", "1", "1.0", "1.0-rc1", "1.0.1", "1.1", "1.1.0", "1.01.2", "1.9", "1.10", "1.10a", "2", "10", "a", "b1", "b2", "b10",
	}
	for i, a := range ordered {
		for j, b := range ordered {
			want := 0
			switch {
			case i < j:
				want = -1
			case i > j:
				want = 1
			}
			if got := Natural(a, b); got != want {
				t.Errorf("Natural(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
	// Numerically equal strings are distinct, ordered bytewise.
	if Natural("1.01", "1.1") != -1 || Natural("1.1", "1.01") != 1 {
		t.Error("Natural must break numeric ties bytewise")
	}
}

func TestParseError(t *testing.T) {
	err := error(&ParseError{Scheme: "semver", Raw: "1.2", Reason: "want MAJOR.MINOR.PATCH"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("ParseError must wrap ErrInvalid")
	}
	if got, want := err.Error(), `semver: "1.2": want MAJOR.MINOR.PATCH: invalid version`; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	long := &ParseError{Scheme: "x", Raw: strings.Repeat("9", 500), Reason: "r"}
	if len(long.Error()) > 120 || !strings.Contains(long.Error(), "...") {
		t.Errorf("long input not truncated: %q", long.Error())
	}
}

func FuzzNatural(f *testing.F) {
	f.Add("1.10", "1.9")
	f.Add("1.01", "1.1")
	f.Add("a1b", "a01b")
	f.Fuzz(func(t *testing.T, a, b string) {
		ab, ba := Natural(a, b), Natural(b, a)
		if ab != -ba {
			t.Fatalf("Natural(%q, %q) = %d, reverse = %d", a, b, ab, ba)
		}
		if (ab == 0) != (a == b) {
			t.Fatalf("Natural(%q, %q) = %d", a, b, ab)
		}
	})
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10!", 10, "exactly10!"},
		{"abcdefghijkl", 5, "abcde..."},
		{"aé", 2, "a..."},              // é is two bytes: do not split it
		{"日本語", 4, "日..."},             // 3-byte runes
		{"\xff\xfe\xfd", 1, "\xff..."}, // invalid UTF-8 bytes count as rune starts
	}
	for _, tt := range tests {
		if got := Truncate(tt.in, tt.n); got != tt.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", tt.in, tt.n, got, tt.want)
		}
		if got := Truncate(tt.in, tt.n); !utf8.ValidString(got) && utf8.ValidString(tt.in) {
			t.Errorf("Truncate(%q, %d) = %q is not valid UTF-8", tt.in, tt.n, got)
		}
	}
}
