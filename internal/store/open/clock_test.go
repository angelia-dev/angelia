package open

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// skewedClock is the host clock off by d.
func skewedClock(d time.Duration) func() time.Time {
	return func() time.Time { return time.Now().Add(d) }
}

// AC 86-A (D22): on Postgres the start-up check compares the host clock, here
// an injected one, with the database clock: silent within 10 s, a warning
// above, a refusal wrapping ErrClockSkew above 15 s, in either direction.
func TestCheckClockOnPostgres(t *testing.T) {
	dsn, _ := pgDSN(t)
	st, err := Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, c := range []struct {
		offset     time.Duration
		warn, fail bool
	}{
		{0, false, false},
		{5 * time.Second, false, false},
		{-12 * time.Second, true, false},
		{12 * time.Second, true, false},
		{20 * time.Second, false, true},
		{-20 * time.Second, false, true},
		{-time.Hour, false, true},
	} {
		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		err := CheckClock(t.Context(), dsn, st, skewedClock(c.offset), log)
		if got := errors.Is(err, ErrClockSkew); got != c.fail || (err != nil && !got) {
			t.Errorf("host clock %v: %v, want refusal %v", c.offset, err, c.fail)
		}
		if got := strings.Contains(logs.String(), "level=WARN"); got != c.warn {
			t.Errorf("host clock %v: warning %v, want %v: %s", c.offset, got, c.warn, logs.String())
		}
	}
}

// SQLite runs on one host: no check, whatever the clock.
func TestCheckClockOnSQLiteIsANoOp(t *testing.T) {
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "a.db")
	st, err := Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := CheckClock(t.Context(), dsn, st, skewedClock(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
}

// A failed measurement is an error, not a pass.
func TestCheckClockMeasurementErrors(t *testing.T) {
	dsn, _ := pgDSN(t)
	st, err := Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CheckClock(ctx, dsn, st, nil, nil); err == nil || errors.Is(err, ErrClockSkew) {
		t.Errorf("cancelled measurement: %v", err)
	}
	// A clock that runs backwards between the two reads of a sample.
	var calls int
	back := func() time.Time {
		calls++
		return time.Unix(0, 0).Add(-time.Duration(calls) * time.Second)
	}
	if err := CheckClock(t.Context(), dsn, st, back, nil); err == nil || !strings.Contains(err.Error(), "backwards") {
		t.Errorf("backwards clock: %v", err)
	}
}

// hidden wraps a store and hides the engine's ClockSkew method.
type hidden struct{ Store }

// On Postgres the check fails closed: a store that cannot measure the skew is
// refused, not passed. A DSN that names no engine is an error too.
func TestCheckClockFailsClosedOnPostgres(t *testing.T) {
	dsn, _ := pgDSN(t)
	st, err := Open(t.Context(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := CheckClock(t.Context(), dsn, hidden{st}, nil, nil); err == nil || !strings.Contains(err.Error(), "cannot run") {
		t.Fatalf("wrapped Postgres store: %v, want a refusal", err)
	}
	if err := CheckClock(t.Context(), "nonsense", st, nil, nil); err == nil {
		t.Fatal("a DSN without an engine passed the check")
	}
}
