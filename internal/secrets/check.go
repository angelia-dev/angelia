package secrets

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// Counter counts the stored secrets per key id.
type Counter interface {
	CountSecretsByKeyID(ctx context.Context) (map[string]int64, error)
}

// ErrStartup is wrapped by the errors of CheckStartup.
var ErrStartup = errors.New("secrets")

// CheckStartup is the startup check of every role (ADR 0024 §4): with
// stored secrets and no key, or with rows sealed by a key id that is not
// configured, the instance must not start. There is no plaintext fallback.
func CheckStartup(ctx context.Context, st Counter, keys *Keyring) error {
	counts, err := st.CountSecretsByKeyID(ctx)
	if err != nil {
		return fmt.Errorf("secrets: count stored secrets: %w", err)
	}
	return checkCounts(counts, keys)
}

func checkCounts(counts map[string]int64, keys *Keyring) error {
	var total int64
	for _, n := range counts {
		total += n
	}
	if total == 0 {
		return nil
	}
	if keys == nil {
		return fmt.Errorf("%w: %d stored secret%s but no key configured; set secrets.key_file", ErrStartup, total, pluralS(total))
	}
	var unknown []string
	for _, id := range slices.Sorted(maps.Keys(counts)) {
		if !keys.Has(id) {
			unknown = append(unknown, fmt.Sprintf("%s (%d row%s)", id, counts[id], pluralS(counts[id])))
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("%w: stored secrets are sealed with key ids that are not configured: %s; add their key files to secrets.previous_key_files",
			ErrStartup, strings.Join(unknown, ", "))
	}
	return nil
}

// KeyStatus is one line of `angelia secrets status`: a key id, how many
// rows it sealed, and whether it is configured.
type KeyStatus struct {
	KeyID string
	Rows  int64
	// Role is "primary", "previous" or "" (not configured).
	Role string
}

// Status lists every configured key id and every key id that sealed a row,
// the primary first, then the previous keys, then the unknown ones.
func Status(counts map[string]int64, keys *Keyring) []KeyStatus {
	var out []KeyStatus
	seen := map[string]bool{}
	if keys != nil {
		for i, id := range keys.IDs() {
			role := "previous"
			if i == 0 {
				role = "primary"
			}
			out = append(out, KeyStatus{KeyID: id, Rows: counts[id], Role: role})
			seen[id] = true
		}
	}
	for _, id := range slices.Sorted(maps.Keys(counts)) {
		if !seen[id] {
			out = append(out, KeyStatus{KeyID: id, Rows: counts[id]})
		}
	}
	return out
}

// Summary is the one-line report of `angelia config check`.
func Summary(counts map[string]int64, keys *Keyring) string {
	var total int64
	for _, n := range counts {
		total += n
	}
	if keys == nil {
		return fmt.Sprintf("secrets: no key configured (%d stored)", total)
	}
	prev := len(keys.IDs()) - 1
	s := fmt.Sprintf("secrets: key %s", keys.PrimaryID())
	if prev > 0 {
		s += fmt.Sprintf(" and %d previous key%s", prev, pluralS(int64(prev)))
	}
	s += fmt.Sprintf(" (%d stored", total)
	if other := total - counts[keys.PrimaryID()]; other > 0 {
		s += fmt.Sprintf(", %d under other keys: run angelia secrets rotate", other)
	}
	return s + ")"
}

func pluralS(n int64) string {
	if n == 1 {
		return ""
	}
	return "s"
}
