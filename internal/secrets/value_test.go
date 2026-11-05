package secrets

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestValueRedacts(t *testing.T) {
	v := NewValueString(canary)
	type holder struct {
		Token Value
		P     *Value
	}
	h := holder{Token: v, P: &v}
	for _, f := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d"} {
		noLeak(t, f, fmt.Sprintf(f, v))
		noLeak(t, f+" in struct", fmt.Sprintf(f, h))
	}
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	noLeak(t, "json", string(b))
	txt, _ := v.MarshalText()
	noLeak(t, "text", string(txt))
	var sb strings.Builder
	slog.New(slog.NewJSONHandler(&sb, nil)).Info("x", "v", v, "h", h)
	slog.New(slog.NewTextHandler(&sb, nil)).Info("x", "v", v, "h", h)
	noLeak(t, "slog", sb.String())
	if !strings.Contains(sb.String(), Redacted) {
		t.Fatalf("slog: %s", sb.String())
	}
}

func TestValueRevealCopies(t *testing.T) {
	src := []byte("abc")
	v := NewValue(src)
	src[0] = 'x'
	got := v.Reveal()
	if string(got) != "abc" {
		t.Fatalf("NewValue did not copy: %q", got)
	}
	got[0] = 'y'
	if string(v.Reveal()) != "abc" || v.Len() != 3 || v.IsZero() || !v.Equal([]byte("abc")) || v.Equal([]byte("abd")) {
		t.Fatal("Reveal returned the held bytes")
	}
	if !(Value{}).IsZero() {
		t.Fatal("zero value is not zero")
	}
}
