package secrets

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/store"
)

func TestRotate(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	old := ring(t, 1)
	for i, n := range []string{"a", "b", "c", "d", "e"} {
		put(t, st, old, "01ROW"+string(rune('A'+i)), n, canary+n)
	}
	both := ring(t, 2, 1)
	put(t, st, both, "01ROWZ", "z", canary+"z") // already under the new key
	var logs strings.Builder
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	rep, err := Rotate(ctx, st, both, RotateOptions{DryRun: true, Logger: log})
	if err != nil || rep.Resealed != 0 || rep.From[old.PrimaryID()] != 5 {
		t.Fatalf("dry run: %+v %v", rep, err)
	}
	if counts, _ := st.CountSecretsByKeyID(ctx); counts[old.PrimaryID()] != 5 {
		t.Fatalf("dry run wrote: %v", counts)
	}

	rep, err = Rotate(ctx, st, both, RotateOptions{Batch: 2, Logger: log})
	if err != nil || rep.Resealed != 5 || rep.From[old.PrimaryID()] != 5 || rep.PrimaryID != both.PrimaryID() {
		t.Fatalf("rotate: %+v %v", rep, err)
	}
	counts, err := st.CountSecretsByKeyID(ctx)
	if err != nil || len(counts) != 1 || counts[both.PrimaryID()] != 6 {
		t.Fatalf("counts after rotate: %v %v", counts, err)
	}
	// Every value survives, and opens with the new key alone.
	r := &Resolver{Store: st, Keys: ring(t, 2)}
	for _, n := range []string{"a", "b", "c", "d", "e", "z"} {
		b, err := r.Resolve(ctx, n, nil)
		if err != nil || string(b.Value.Reveal()) != canary+n {
			t.Fatalf("%s after rotate: %v", n, err)
		}
	}
	row, _ := st.SecretByName(ctx, "a")
	if row.Revision != 2 {
		t.Fatalf("revision %d", row.Revision)
	}
	// Rerunning is idempotent.
	rep, err = Rotate(ctx, st, both, RotateOptions{Logger: log})
	if err != nil || rep.Resealed != 0 {
		t.Fatalf("rerun: %+v %v", rep, err)
	}
	out := logs.String()
	noLeak(t, "rotate log", out, master(1), master(2))
	if !strings.Contains(out, `"audit":true`) || !strings.Contains(out, `"action":"secrets.rotate"`) || !strings.Contains(out, `"resealed":5`) {
		t.Fatalf("audit record: %s", out)
	}
	if _, err := Rotate(ctx, st, nil, RotateOptions{}); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("no key: %v", err)
	}
}

func TestRotateRollsBackOnError(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	put(t, st, ring(t, 1), "01A", "a", canary)
	put(t, st, ring(t, 3), "01B", "b", canary) // a key the keyring lacks
	keys := ring(t, 2, 1)
	_, err := Rotate(ctx, st, keys, RotateOptions{})
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	noLeak(t, "rotate error", err.Error())
	if counts, _ := st.CountSecretsByKeyID(ctx); counts[ring(t, 1).PrimaryID()] != 1 {
		t.Fatalf("a failed batch kept writes: %v", counts)
	}
}

// losingStore makes UpdateSecret lose to a concurrent write: always for the
// rows in lose, once for the rows in loseOnce.
type losingStore struct {
	store.SecretStore
	lose, loseOnce map[string]bool
	tries          map[string]int
}

func (l *losingStore) InSecretTx(ctx context.Context, fn func(store.SecretTx) error) error {
	return l.SecretStore.InSecretTx(ctx, func(tx store.SecretTx) error { return fn(&losingTx{SecretTx: tx, l: l}) })
}

type losingTx struct {
	store.SecretTx
	l *losingStore
}

func (x *losingTx) UpdateSecret(ctx context.Context, s store.Secret) (bool, error) {
	x.l.tries[s.Name]++
	if x.l.lose[s.Name] || (x.l.loseOnce[s.Name] && x.l.tries[s.Name] == 1) {
		return false, nil
	}
	return x.SecretTx.UpdateSecret(ctx, s)
}

func TestRotateRetriesFencedUpdates(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	old := ring(t, 1)
	put(t, st, old, "01A", "a", canary)
	put(t, st, old, "01B", "b", canary)
	put(t, st, old, "01C", "c", canary)
	ls := &losingStore{SecretStore: st, lose: map[string]bool{"b": true}, loseOnce: map[string]bool{"c": true}, tries: map[string]int{}}
	rep, err := Rotate(ctx, ls, ring(t, 2, 1), RotateOptions{})
	if !errors.Is(err, ErrRotateIncomplete) || rep.Resealed != 2 || len(rep.Skipped) != 1 || rep.Skipped[0] != "b" {
		t.Fatalf("rotate: %+v %v", rep, err)
	}
	if ls.tries["b"] != 3 || ls.tries["c"] != 2 || ls.tries["a"] != 1 {
		t.Fatalf("tries %v", ls.tries)
	}
	counts, _ := st.CountSecretsByKeyID(ctx)
	if counts[old.PrimaryID()] != 1 || counts[ring(t, 2).PrimaryID()] != 2 {
		t.Fatalf("counts %v", counts)
	}
}
