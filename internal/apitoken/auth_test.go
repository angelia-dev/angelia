package apitoken

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/apitoken/format"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

var t0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "angelia.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// create stores a new token and returns it.
func create(t *testing.T, s store.TokenStore, name, scope string, edit func(*store.APIToken)) (id, tok string) {
	t.Helper()
	id, tok = Generate(t0)
	row := store.APIToken{ID: id, Name: name, Scope: scope, Hash: Hash(tok), CreatedAt: t0, CreatedBy: "local"}
	if edit != nil {
		edit(&row)
	}
	if err := s.CreateAPIToken(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	return id, tok
}

func wantReason(t *testing.T, err error, want Reason, wantID string) {
	t.Helper()
	if !errors.Is(err, api.ErrUnauthenticated) {
		t.Fatalf("err = %v, want ErrUnauthenticated", err)
	}
	r, id, ok := FailureReason(err)
	if !ok || r != want || id != wantID {
		t.Fatalf("reason = %q %q %v, want %q %q", r, id, ok, want, wantID)
	}
}

func TestAuthenticate(t *testing.T) {
	s := openStore(t)
	ctx := t.Context()
	c := &clock{t: t0.Add(time.Hour)}
	a := NewAuthenticator(s, nil, WithClock(c.now))

	readID, readTok := create(t, s, "reader", "read", nil)
	_, adminTok := create(t, s, "admin", "admin", nil)
	revID, revTok := create(t, s, "gone", "write", nil)
	if _, err := s.RevokeAPIToken(ctx, revID, t0, "local"); err != nil {
		t.Fatal(err)
	}
	expID, expTok := create(t, s, "old", "write", func(r *store.APIToken) { e := t0.Add(time.Hour); r.ExpiresAt = &e })
	_, laterTok := create(t, s, "later", "write", func(r *store.APIToken) { e := t0.Add(time.Hour + time.Millisecond); r.ExpiresAt = &e })

	p, err := a.Authenticate(ctx, readTok)
	if err != nil || p != (api.Principal{ID: readID, Name: "reader", Scope: api.ScopeRead}) {
		t.Fatalf("read token: %+v, %v", p, err)
	}
	if p, err = a.Authenticate(ctx, adminTok); err != nil || p.Scope != api.ScopeAdmin {
		t.Fatalf("admin token: %+v, %v", p, err)
	}
	if p, err = a.Authenticate(ctx, laterTok); err != nil || p.Scope != api.ScopeWrite {
		t.Fatalf("token expiring later: %+v, %v", p, err)
	}

	_, err = a.Authenticate(ctx, revTok)
	wantReason(t, err, ReasonRevoked, revID)
	_, err = a.Authenticate(ctx, expTok)
	wantReason(t, err, ReasonExpired, expID)

	// A malformed token, or one with a typo, fails before any store access.
	_, err = a.Authenticate(ctx, "ang_team")
	wantReason(t, err, ReasonMalformed, "")
	typo := byte('x')
	if readTok[50] == typo {
		typo = 'y'
	}
	_, err = a.Authenticate(ctx, readTok[:50]+string(typo)+readTok[51:])
	wantReason(t, err, ReasonMalformed, "")

	// A well-formed token with an unknown id.
	_, unknownTok := Generate(t0)
	_, err = a.Authenticate(ctx, unknownTok)
	wantReason(t, err, ReasonUnknown, "")

	// The id of a stored token with another secret: unknown, whatever the
	// row's state, so a guess learns nothing about the row.
	for _, id := range []string{readID, revID, expID} {
		forged := forge(id)
		_, err = a.Authenticate(ctx, forged)
		wantReason(t, err, ReasonUnknown, "")
		if strings.Contains(err.Error(), id) {
			t.Fatalf("the error of a hash mismatch names the id: %v", err)
		}
	}
}

// forge makes a well-formed token with a valid checksum for id and a secret
// that is not the stored one.
func forge(id string) string { return format.Format(id, [32]byte{}) }

// The error never shows the token.
func TestAuthErrorHidesToken(t *testing.T) {
	s := openStore(t)
	a := NewAuthenticator(s, nil)
	_, tok := create(t, s, "a", "read", nil)
	for _, bad := range []string{tok + "x", forge("01JCZ8W3T6Q9N2H4K7M5R0V8XA"), specExample} {
		_, err := a.Authenticate(t.Context(), bad)
		if err == nil || strings.Contains(err.Error(), bad[31:]) {
			t.Fatalf("error %v", err)
		}
	}
}

// fakeStore counts reads and touches, and can fail or block.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]store.APIToken
	reads    int
	touches  []map[string]time.Time
	readErr  error
	touchErr error
	// touching, when set, is closed on the first touch, which then waits
	// for release.
	touching, release chan struct{}
	// touched, when set, gets the result of every touch (without blocking).
	touched chan error
}

func (f *fakeStore) APITokenByID(_ context.Context, id string) (store.APIToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return store.APIToken{}, f.readErr
	}
	r, ok := f.rows[id]
	if !ok {
		return store.APIToken{}, store.ErrNotFound
	}
	return r, nil
}

func (f *fakeStore) TouchAPITokens(_ context.Context, used map[string]time.Time) error {
	if f.touching != nil {
		close(f.touching)
		f.touching = nil
		<-f.release
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.touchErr
	if err == nil {
		f.touches = append(f.touches, used)
	}
	if f.touched != nil {
		select {
		case f.touched <- err:
		default:
		}
	}
	return err
}

func (f *fakeStore) add(name, scope string) (id, tok string) {
	id, tok = Generate(t0)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]store.APIToken{}
	}
	f.rows[id] = store.APIToken{ID: id, Name: name, Scope: scope, Hash: Hash(tok), CreatedAt: t0, CreatedBy: "local"}
	return id, tok
}

func TestMalformedCostsNoStoreRead(t *testing.T) {
	f := &fakeStore{}
	a := NewAuthenticator(f, nil)
	for _, tok := range []string{"", "ang_team", "Bearer x", specExample[:Len-1] + "b"} {
		_, err := a.Authenticate(t.Context(), tok)
		wantReason(t, err, ReasonMalformed, "")
	}
	if f.reads != 0 {
		t.Fatalf("%d store reads for malformed tokens", f.reads)
	}
	// The spec example is well formed, so it costs one read.
	_, err := a.Authenticate(t.Context(), specExample)
	wantReason(t, err, ReasonUnknown, "")
	if f.reads != 1 {
		t.Fatalf("%d store reads, want 1", f.reads)
	}
}

// A store failure is a server error, not a 401.
func TestStoreFailureIsNotUnauthenticated(t *testing.T) {
	f := &fakeStore{readErr: errors.New("disk on fire")}
	_, err := NewAuthenticator(f, nil).Authenticate(t.Context(), specExample)
	if err == nil || errors.Is(err, api.ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
	if _, _, ok := FailureReason(err); ok {
		t.Fatal("a store failure has a failure reason")
	}
}

func TestUnknownScopeIsAServerError(t *testing.T) {
	f := &fakeStore{}
	id, tok := f.add("a", "read")
	r := f.rows[id]
	r.Scope = "root"
	f.rows[id] = r
	_, err := NewAuthenticator(f, nil).Authenticate(t.Context(), tok)
	if err == nil || errors.Is(err, api.ErrUnauthenticated) {
		t.Fatalf("err = %v", err)
	}
}

// Last use is recorded for successful authentications only, keyed by id, and
// written at most once per token per TouchInterval (ADR 0030 T6).
func TestLastUseCoalescing(t *testing.T) {
	f := &fakeStore{}
	c := &clock{t: t0}
	a := NewAuthenticator(f, nil, WithClock(c.now))
	ctx := t.Context()
	idA, tokA := f.add("a", "read")
	idB, tokB := f.add("b", "read")

	// Failures add nothing.
	for _, bad := range []string{"x", forge(idA), specExample} {
		if _, err := a.Authenticate(ctx, bad); err == nil {
			t.Fatal("bad token accepted")
		}
	}
	if n := len(a.pendingIDs()); n != 0 {
		t.Fatalf("%d uses recorded after failures", n)
	}
	if err := a.Flush(ctx, false); err != nil || len(f.touches) != 0 {
		t.Fatalf("flush with nothing recorded: %v %v", f.touches, err)
	}

	// Many uses of one token are one entry.
	for range 100 {
		c.add(time.Second)
		if _, err := a.Authenticate(ctx, tokA); err != nil {
			t.Fatal(err)
		}
	}
	if p := a.pendingIDs(); len(p) != 1 || !p[idA].Equal(c.now()) {
		t.Fatalf("pending = %v", p)
	}
	if err := a.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(f.touches) != 1 || !f.touches[0][idA].Equal(c.now()) || len(f.touches[0]) != 1 {
		t.Fatalf("touches = %v", f.touches)
	}

	// Within the interval A is held back; B, never written, goes at once.
	c.add(time.Minute)
	for _, tok := range []string{tokA, tokB} {
		if _, err := a.Authenticate(ctx, tok); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(f.touches) != 2 || len(f.touches[1]) != 1 || !f.touches[1][idB].Equal(c.now()) {
		t.Fatalf("touches = %v", f.touches)
	}
	// After the interval A goes too.
	c.add(TouchInterval - time.Minute)
	if err := a.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	if len(f.touches) != 3 || len(f.touches[2]) != 1 || !f.touches[2][idA].Equal(t0.Add(100*time.Second+time.Minute)) {
		t.Fatalf("touches = %v", f.touches)
	}

	// A failed write keeps the use for the next flush.
	c.add(TouchInterval)
	if _, err := a.Authenticate(ctx, tokB); err != nil {
		t.Fatal(err)
	}
	f.touchErr = errors.New("busy")
	if err := a.Flush(ctx, false); err == nil {
		t.Fatal("flush error lost")
	}
	f.touchErr = nil
	if err := a.Flush(ctx, false); err != nil || len(f.touches) != 4 || len(f.touches[3]) != 1 {
		t.Fatalf("retry: %v %v", f.touches, err)
	}

	// Shutdown writes every recorded use, whatever the interval.
	c.add(time.Second)
	if _, err := a.Authenticate(ctx, tokB); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(ctx, false); err != nil || len(f.touches) != 4 {
		t.Fatalf("within the interval: %v %v", f.touches, err)
	}
	if err := a.Flush(ctx, true); err != nil || len(f.touches) != 5 {
		t.Fatalf("final flush: %v %v", f.touches, err)
	}
	if len(a.pendingIDs()) != 0 {
		t.Fatal("uses left after the final flush")
	}
	// Old write times are forgotten, so the map stays small.
	c.add(TouchInterval)
	if err := a.Flush(ctx, false); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	n := len(a.written)
	a.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d write times kept", n)
	}
}

// The store write of a flush runs outside the Authenticator's lock:
// authentication goes on while a touch is blocked, and a use recorded during
// the write stays pending.
func TestFlushDoesNotBlockAuthentication(t *testing.T) {
	f := &fakeStore{touching: make(chan struct{}), release: make(chan struct{})}
	c := &clock{t: t0}
	a := NewAuthenticator(f, nil, WithClock(c.now))
	ctx := t.Context()
	id, tok := f.add("a", "read")
	if _, err := a.Authenticate(ctx, tok); err != nil {
		t.Fatal(err)
	}
	touching := f.touching
	done := make(chan error, 1)
	go func() { done <- a.Flush(ctx, false) }()
	<-touching
	c.add(time.Second)
	authed := make(chan error, 1)
	go func() { _, err := a.Authenticate(ctx, tok); authed <- err }()
	select {
	case err := <-authed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Authenticate waited for the touch")
	}
	close(f.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p := a.pendingIDs(); !p[id].Equal(t0.Add(time.Second)) {
		t.Fatalf("the use during the write was lost: %v", p)
	}
}

func TestRunFlushesAndFinalFlush(t *testing.T) {
	f := &fakeStore{touched: make(chan error, 1)}
	a := NewAuthenticator(f, slog.New(slog.DiscardHandler))
	id, tok := f.add("a", "read")
	if _, err := a.Authenticate(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, time.Millisecond, time.Second) }()
	if err := <-f.touched; err != nil {
		t.Fatal(err)
	}
	// A use inside the interval is written by the final flush only.
	if _, err := a.Authenticate(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.touches) != 2 || f.touches[1][id].IsZero() {
		t.Fatalf("touches = %v", f.touches)
	}
}

func TestRunRefusesNonPositiveTick(t *testing.T) {
	a := NewAuthenticator(&fakeStore{}, nil)
	for _, tick := range []time.Duration{0, -time.Second} {
		if err := a.Run(t.Context(), tick, time.Second); err == nil {
			t.Fatalf("Run with tick %s: no error", tick)
		}
	}
}

// Run logs a failed flush at a tick and keeps going; at shutdown it logs a
// failed final flush with a message of its own.
func TestRunLogsFailures(t *testing.T) {
	f := &fakeStore{touchErr: errors.New("busy"), touched: make(chan error, 1)}
	rec := &recorder{first: make(chan string, 1)}
	a := NewAuthenticator(f, slog.New(slog.NewTextHandler(rec, nil)))
	_, tok := f.add("a", "read")
	if _, err := a.Authenticate(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx, time.Millisecond, time.Second) }()
	if err := <-f.touched; err == nil {
		t.Fatal("the first touch succeeded")
	}
	// ctx is still live, so the first record is a per-tick one.
	first := <-rec.first
	if !strings.Contains(first, `msg="`+msgFlushFailed+`"`) || strings.Contains(first, "at shutdown") ||
		!strings.Contains(first, `err="apitoken: record last use: busy"`) {
		t.Fatalf("tick record = %s", first)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	last := rec.lines[len(rec.lines)-1]
	if !strings.Contains(last, `msg="`+msgShutdownFlushFailed+`"`) {
		t.Fatalf("last record = %s, want the shutdown one", last)
	}
	for _, l := range rec.lines {
		if strings.Contains(l, tok) {
			t.Fatalf("a record shows the token: %s", l)
		}
	}
}

// recorder keeps every log record and sends the first one to first.
type recorder struct {
	mu    sync.Mutex
	lines []string
	first chan string
}

func (r *recorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, string(p))
	if len(r.lines) == 1 {
		r.first <- string(p)
	}
	return len(p), nil
}

// End to end on SQLite: a successful use reaches last_used_at.
func TestLastUseReachesStore(t *testing.T) {
	s := openStore(t)
	c := &clock{t: t0.Add(time.Hour)}
	a := NewAuthenticator(s, nil, WithClock(c.now))
	id, tok := create(t, s, "a", "read", nil)
	if _, err := a.Authenticate(t.Context(), tok); err != nil {
		t.Fatal(err)
	}
	if err := a.Flush(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	row, err := s.APITokenByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastUsedAt == nil || !row.LastUsedAt.Equal(c.now()) {
		t.Fatalf("last_used_at = %v", row.LastUsedAt)
	}
}

// The reasons are internal/api's closed set, and every AuthError says its
// reason to the server through api.AuthFailure.
func TestFailureReasonForServer(t *testing.T) {
	got := []string{string(ReasonMissing), string(ReasonMalformed), string(ReasonUnknown), string(ReasonRevoked), string(ReasonExpired)}
	if !slices.Equal(got, api.AuthReasons()) {
		t.Fatalf("reasons %v, api %v", got, api.AuthReasons())
	}
	var f api.AuthFailure
	if err := error(&AuthError{Reason: ReasonRevoked, ID: "X"}); !errors.As(err, &f) {
		t.Fatal("AuthError is not an api.AuthFailure")
	}
	if r, id := f.FailureReason(); r != "revoked" || id != "X" {
		t.Fatalf("FailureReason = %s, %s", r, id)
	}
}
