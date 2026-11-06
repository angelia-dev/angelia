package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"github.com/angelia-dev/angelia/internal/store"
)

// rotateAttempts bounds the tries of one row whose fenced update keeps
// losing to concurrent writes (Postgres; on SQLite a batch holds the write
// lock and never loses).
const rotateAttempts = 3

// ErrRotateIncomplete is returned when some rows could not be re-sealed
// because concurrent writes kept winning; rerun the rotation.
var ErrRotateIncomplete = errors.New("secrets: rotation incomplete")

// RotateOptions controls Rotate.
type RotateOptions struct {
	// DryRun reports what would be re-sealed and writes nothing.
	DryRun bool
	// Batch is the number of rows per transaction; <= 0 means one
	// transaction for every row.
	Batch int
	// Logger receives the audit record of the run; nil discards it.
	Logger *slog.Logger
}

// RotateReport says what a rotation did.
type RotateReport struct {
	// From counts, per old key id, the rows re-sealed (or, in a dry run,
	// that would be).
	From map[string]int
	// Resealed is the number of rows re-sealed.
	Resealed int
	// Skipped names the rows that kept losing to concurrent writes.
	Skipped []string
	// PrimaryID is the key every row is sealed with afterwards.
	PrimaryID string
}

// Rotate re-seals under the primary key every stored secret sealed with
// another configured key (ADR 0024 §5). Each batch is one transaction: a
// crash rolls it back and leaves every row under a configured key, and a
// rerun is idempotent. Every row update is fenced by the row's revision; a
// row that loses is read again and retried, at most three times, then left
// alone and reported. Values are never logged.
func Rotate(ctx context.Context, st store.SecretStore, keys *Keyring, o RotateOptions) (RotateReport, error) {
	if keys == nil {
		return RotateReport{}, fmt.Errorf("%w: set secrets.key_file to the new key and secrets.previous_key_files to the old ones", ErrKeyMissing)
	}
	log := o.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rep := RotateReport{From: map[string]int{}, PrimaryID: keys.PrimaryID()}
	rows, err := st.ListSecrets(ctx)
	if err != nil {
		return rep, fmt.Errorf("secrets: rotate: %w", err)
	}
	var todo []store.Secret
	for _, r := range rows {
		if r.KeyID != keys.PrimaryID() {
			todo = append(todo, r)
		}
	}
	if o.DryRun {
		for _, r := range todo {
			rep.From[r.KeyID]++
		}
		return rep, nil
	}
	batch := o.Batch
	if batch <= 0 {
		batch = max(len(todo), 1)
	}
	for start := 0; start < len(todo); start += batch {
		chunk := todo[start:min(start+batch, len(todo))]
		var done []store.Secret
		var skipped []string
		err := st.InSecretTx(ctx, func(tx store.SecretTx) error {
			done, skipped = nil, nil
			for _, r := range chunk {
				ok, from, err := reseal(ctx, tx, keys, r.ID)
				if err != nil {
					return err
				}
				switch {
				case !ok:
					skipped = append(skipped, r.Name)
				case from != "":
					done = append(done, store.Secret{Name: r.Name, KeyID: from})
				}
			}
			return nil
		})
		if err != nil {
			audit(ctx, log, rep, "failed")
			return rep, fmt.Errorf("secrets: rotate: %w", err)
		}
		for _, d := range done {
			rep.From[d.KeyID]++
			rep.Resealed++
		}
		rep.Skipped = append(rep.Skipped, skipped...)
	}
	if len(rep.Skipped) > 0 {
		audit(ctx, log, rep, "incomplete")
		return rep, fmt.Errorf("%w: %d secret%s changed concurrently and kept their key: %v; run angelia secrets rotate again",
			ErrRotateIncomplete, len(rep.Skipped), pluralS(int64(len(rep.Skipped))), rep.Skipped)
	}
	audit(ctx, log, rep, "done")
	return rep, nil
}

// reseal re-seals one row under the primary key. ok is false when the row
// kept losing to concurrent writes; from is the key id it was sealed with,
// "" when nothing was to do (gone, or already under the primary).
func reseal(ctx context.Context, tx store.SecretTx, keys *Keyring, id string) (ok bool, from string, err error) {
	for range rotateAttempts {
		cur, err := tx.GetSecret(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			return true, "", nil // deleted meanwhile
		}
		if err != nil {
			return false, "", err
		}
		if cur.KeyID == keys.PrimaryID() {
			return true, "", nil
		}
		v, err := keys.Open(cur.ID, cur.Name, cur.Ciphertext)
		if err != nil {
			return false, "", err
		}
		env, kid, err := keys.Seal(cur.ID, cur.Name, v.b)
		if err != nil {
			return false, "", err
		}
		next := cur
		next.KeyID, next.Ciphertext = kid, env
		updated, err := tx.UpdateSecret(ctx, next)
		if err != nil {
			return false, "", err
		}
		if updated {
			return true, cur.KeyID, nil
		}
	}
	return false, "", nil
}

// audit writes the audit record of a rotation run: counts and key ids only.
func audit(ctx context.Context, log *slog.Logger, rep RotateReport, outcome string) {
	from := slices.Sorted(maps.Keys(rep.From))
	log.LogAttrs(ctx, slog.LevelInfo, "secrets audit",
		slog.Bool("audit", true), slog.String("action", "secrets.rotate"), slog.String("outcome", outcome),
		slog.String("key_id", rep.PrimaryID), slog.Any("from_key_ids", from),
		slog.Int("resealed", rep.Resealed), slog.Any("skipped", rep.Skipped))
}
