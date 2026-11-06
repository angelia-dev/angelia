package secrets

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckStartup(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	// No key and no rows: starts.
	if err := CheckStartup(ctx, st, nil); err != nil {
		t.Fatalf("empty, no key: %v", err)
	}
	if got := Summary(nil, nil); got != "secrets: no key configured (0 stored)" {
		t.Fatalf("Summary = %q", got)
	}
	old := ring(t, 1)
	put(t, st, old, "01A", "a", canary)
	put(t, st, old, "01B", "b", canary)
	put(t, st, old, "01C", "c", canary)

	// Rows and no key: refused, with no plaintext fallback.
	err := CheckStartup(ctx, st, nil)
	if !errors.Is(err, ErrStartup) || err.Error() != "secrets: 3 stored secrets but no key configured; set secrets.key_file" {
		t.Fatalf("rows, no key: %v", err)
	}
	// Rows under a key id that is not configured: refused, listing it.
	err = CheckStartup(ctx, st, ring(t, 2))
	if !errors.Is(err, ErrStartup) || !strings.Contains(err.Error(), old.PrimaryID()+" (3 rows)") {
		t.Fatalf("unknown key id: %v", err)
	}
	for _, k := range []*Keyring{old, ring(t, 2, 1)} {
		if err := CheckStartup(ctx, st, k); err != nil {
			t.Fatalf("configured key: %v", err)
		}
	}
	if err := CheckStartup(ctx, failingCounter{}, old); err == nil {
		t.Fatal("store error ignored")
	}
}

type failingCounter struct{}

func (failingCounter) CountSecretsByKeyID(context.Context) (map[string]int64, error) {
	return nil, errors.New("disk on fire")
}

func TestStatusAndSummary(t *testing.T) {
	k := ring(t, 2, 1)
	p, prev := k.IDs()[0], k.IDs()[1]
	counts := map[string]int64{prev: 2, "ffffffffffffffff": 1}
	got := Status(counts, k)
	want := []KeyStatus{{p, 0, "primary"}, {prev, 2, "previous"}, {"ffffffffffffffff", 1, ""}}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("Status = %+v", got)
	}
	if s := Status(map[string]int64{"aa": 1}, nil); len(s) != 1 || s[0].Role != "" {
		t.Fatalf("Status without keys = %+v", s)
	}
	if s := Summary(counts, k); s != "secrets: key "+p+" and 1 previous key (3 stored, 3 under other keys: run angelia secrets rotate)" {
		t.Fatalf("Summary = %q", s)
	}
	if s := Summary(map[string]int64{p: 1}, ring(t, 2)); s != "secrets: key "+p+" (1 stored)" {
		t.Fatalf("Summary = %q", s)
	}
}
