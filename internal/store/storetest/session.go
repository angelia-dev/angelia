package storetest

import (
	"bytes"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
)

// Browser sessions of the admin UI and identity pins (story #262, ADR 0033
// §5).

func testSession(id string, created time.Time) store.UISession {
	return store.UISession{
		ID: id, Hash: bytes.Repeat([]byte{id[len(id)-1]}, 32), Provider: "github", Subject: "139323",
		Login: "mmornati", Name: "Marco", AvatarURL: "https://avatars.example/u/1", UserAgent: "test/1",
		CreatedAt: created, LastSeenAt: created, ExpiresAt: created.Add(7 * 24 * time.Hour),
	}
}

// Create, read back, the conflict on the id, and validation.
func testUISessionCRUD(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	a := testSession("01SESSA", t0)
	in := a
	in.RevokedAt, in.RevokedBy = ptr(t0), "x" // ignored: a new session is active
	must(t, s.CreateUISession(ctx, in))
	got, err := s.UISessionByID(ctx, a.ID)
	must(t, err)
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("UISessionByID = %+v, want %+v", got, a)
	}
	_, err = s.UISessionByID(ctx, "01NOPE")
	wantErr(t, err, store.ErrNotFound, "unknown id")
	wantErr(t, s.CreateUISession(ctx, testSession("01SESSA", t0)), store.ErrConflict, "same id")

	// Unknown optional fields are "".
	b := testSession("01SESSB", t0)
	b.Name, b.AvatarURL, b.UserAgent = "", "", ""
	must(t, s.CreateUISession(ctx, b))
	got, err = s.UISessionByID(ctx, b.ID)
	must(t, err)
	if !reflect.DeepEqual(got, b) {
		t.Fatalf("empty optional fields: %+v", got)
	}

	for name, bad := range map[string]func(*store.UISession){
		"empty id":       func(x *store.UISession) { x.ID = "" },
		"empty provider": func(x *store.UISession) { x.Provider = "" },
		"empty subject":  func(x *store.UISession) { x.Subject = "" },
		"empty login":    func(x *store.UISession) { x.Login = "" },
		"short hash":     func(x *store.UISession) { x.Hash = x.Hash[:31] },
		"zero created":   func(x *store.UISession) { x.CreatedAt = time.Time{} },
		"zero last seen": func(x *store.UISession) { x.LastSeenAt = time.Time{} },
		"zero expiry":    func(x *store.UISession) { x.ExpiresAt = time.Time{} },
	} {
		x := testSession("01SESSX", t0)
		bad(&x)
		wantErr(t, s.CreateUISession(ctx, x), store.ErrInvalid, name)
	}
}

// Revoking twice reports false the second time and keeps the first
// revocation; an unknown id is ErrNotFound.
func testUISessionRevoke(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateUISession(ctx, testSession("01SESSA", t0)))
	ok, err := s.RevokeUISession(ctx, "01SESSA", t0.Add(time.Minute), "github:139323")
	must(t, err)
	if !ok {
		t.Fatal("revoke reported false")
	}
	ok, err = s.RevokeUISession(ctx, "01SESSA", t0.Add(time.Hour), "01ADMIN")
	must(t, err)
	if ok {
		t.Fatal("second revoke reported true")
	}
	got, err := s.UISessionByID(ctx, "01SESSA")
	must(t, err)
	if got.RevokedAt == nil || !got.RevokedAt.Equal(t0.Add(time.Minute)) || got.RevokedBy != "github:139323" {
		t.Fatalf("after two revokes: %v %q", got.RevokedAt, got.RevokedBy)
	}
	_, err = s.RevokeUISession(ctx, "01NOPE", t0, "local")
	wantErr(t, err, store.ErrNotFound, "revoke unknown")
	_, err = s.RevokeUISession(ctx, "01SESSA", time.Time{}, "local")
	wantErr(t, err, store.ErrInvalid, "zero time")
	_, err = s.RevokeUISession(ctx, "01SESSA", t0, "")
	wantErr(t, err, store.ErrInvalid, "empty revoker")
}

// Pages by id; ended rows (revoked, expired, idle) only with IncludeEnded.
func testListUISessions(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	now := t0.Add(24 * time.Hour)
	idleSince := now.Add(-12 * time.Hour)
	active := testSession("01SESSA", t0)
	active.LastSeenAt = now.Add(-time.Hour)
	revoked := testSession("01SESSB", t0)
	revoked.LastSeenAt = now.Add(-time.Hour)
	expired := testSession("01SESSC", t0)
	expired.LastSeenAt, expired.ExpiresAt = now.Add(-time.Hour), now // expires_at > now is active
	idle := testSession("01SESSD", t0)
	idle.LastSeenAt = idleSince // last_seen_at > idle_since is active
	active2 := testSession("01SESSE", t0)
	active2.LastSeenAt = idleSince.Add(time.Millisecond)
	for _, x := range []store.UISession{active2, idle, expired, revoked, active} {
		must(t, s.CreateUISession(ctx, x))
	}
	_, err := s.RevokeUISession(ctx, revoked.ID, t0, "local")
	must(t, err)
	ids := func(xs []store.UISession) (out []string) {
		for _, x := range xs {
			out = append(out, x.ID)
		}
		return out
	}
	list := func(q store.UISessionQuery) []string {
		t.Helper()
		q.Now, q.IdleSince = now, idleSince
		page, err := s.ListUISessions(ctx, q)
		must(t, err)
		return ids(page)
	}
	if got := list(store.UISessionQuery{Limit: 10}); !reflect.DeepEqual(got, []string{"01SESSA", "01SESSE"}) {
		t.Fatalf("active = %v", got)
	}
	if got := list(store.UISessionQuery{Limit: 1, AfterID: "01SESSA"}); !reflect.DeepEqual(got, []string{"01SESSE"}) {
		t.Fatalf("active after A = %v", got)
	}
	if got := list(store.UISessionQuery{Limit: 2, IncludeEnded: true}); !reflect.DeepEqual(got, []string{"01SESSA", "01SESSB"}) {
		t.Fatalf("first page of all = %v", got)
	}
	if got := list(store.UISessionQuery{Limit: 10, IncludeEnded: true, AfterID: "01SESSB"}); !reflect.DeepEqual(got, []string{"01SESSC", "01SESSD", "01SESSE"}) {
		t.Fatalf("second page of all = %v", got)
	}
	_, err = s.ListUISessions(ctx, store.UISessionQuery{})
	wantErr(t, err, store.ErrInvalid, "zero limit")
}

// TouchUISessions never moves last_seen_at backwards and ignores unknown ids.
func testTouchUISessions(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateUISession(ctx, testSession("01SESSA", t0)))
	must(t, s.CreateUISession(ctx, testSession("01SESSB", t0)))
	lastSeen := func(id string) time.Time {
		t.Helper()
		got, err := s.UISessionByID(ctx, id)
		must(t, err)
		return got.LastSeenAt
	}
	must(t, s.TouchUISessions(ctx, map[string]time.Time{"01SESSA": t0.Add(time.Hour), "01NOPE": t0}))
	must(t, s.TouchUISessions(ctx, map[string]time.Time{"01SESSA": t0.Add(time.Minute), "01SESSB": t0.Add(-time.Hour)}))
	if got := lastSeen("01SESSA"); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last seen moved backwards: %v", got)
	}
	if got := lastSeen("01SESSB"); !got.Equal(t0) {
		t.Fatalf("last seen of b moved before its creation: %v", got)
	}
	must(t, s.TouchUISessions(ctx, nil))
}

// The first pin of a login holds; later pins of it, concurrent or not,
// return the first subject. Logins are per provider.
func testPinUIIdentity(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	got, err := s.PinUIIdentity(ctx, "github", "mmornati", "139323", t0)
	must(t, err)
	if got != "139323" {
		t.Fatalf("first pin = %q", got)
	}
	got, err = s.PinUIIdentity(ctx, "github", "mmornati", "999", t0.Add(time.Hour))
	must(t, err)
	if got != "139323" {
		t.Fatalf("second pin replaced the first: %q", got)
	}
	if got, err := s.UIIdentityPin(ctx, "github", "mmornati"); err != nil || got != "139323" {
		t.Fatalf("read pin = %q, %v", got, err)
	}
	if _, err := s.UIIdentityPin(ctx, "github", "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read of no pin: %v", err)
	}
	got, err = s.PinUIIdentity(ctx, "other", "mmornati", "999", t0)
	must(t, err)
	if got != "999" {
		t.Fatalf("pin of another provider = %q", got)
	}
	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			results[i], errs[i] = s.PinUIIdentity(ctx, "github", "octocat", "10"+"ABCDEFGH"[i:i+1], t0)
		})
	}
	wg.Wait()
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("concurrent pin %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("concurrent pins disagree: %v", results)
		}
	}
	for name, args := range map[string][3]string{
		"empty provider": {"", "a", "1"}, "empty login": {"github", "", "1"}, "empty subject": {"github", "a", ""},
	} {
		_, err := s.PinUIIdentity(ctx, args[0], args[1], args[2], t0)
		if !errors.Is(err, store.ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	_, err = s.PinUIIdentity(ctx, "github", "a", "1", time.Time{})
	wantErr(t, err, store.ErrInvalid, "zero time")
}
