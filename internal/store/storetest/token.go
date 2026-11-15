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

// API tokens (story #72, ADR 0030 T3).

func testToken(id, name, scope string) store.APIToken {
	return store.APIToken{
		ID: id, Name: name, Scope: scope, Hash: bytes.Repeat([]byte{id[len(id)-1]}, 32),
		CreatedAt: t0, CreatedBy: "local",
	}
}

func ptr(t time.Time) *time.Time { return &t }

// Create, read back, the conflicts on id and active name, and validation.
func testAPITokenCRUD(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()

	a := testToken("01TOKENA", "ci", store.TokenScopeWrite)
	a.ExpiresAt = ptr(t0.Add(90 * 24 * time.Hour))
	// Fields a create ignores: a new token is unused and active.
	in := a
	in.LastUsedAt, in.RevokedAt, in.RevokedBy = ptr(t0), ptr(t0), "x"
	must(t, s.CreateAPIToken(ctx, in))
	got, err := s.APITokenByID(ctx, a.ID)
	must(t, err)
	if !reflect.DeepEqual(got, a) {
		t.Fatalf("APITokenByID = %+v, want %+v", got, a)
	}
	_, err = s.APITokenByID(ctx, "01NOPE")
	wantErr(t, err, store.ErrNotFound, "unknown id")

	// The id is unique, revoked or not; the name among active tokens only.
	wantErr(t, s.CreateAPIToken(ctx, testToken("01TOKENA", "other", "read")), store.ErrConflict, "same id")
	wantErr(t, s.CreateAPIToken(ctx, testToken("01TOKENB", "ci", "read")), store.ErrConflict, "same active name")
	ok, err := s.RevokeAPIToken(ctx, a.ID, t0.Add(time.Hour), "01ADMIN")
	must(t, err)
	if !ok {
		t.Fatal("first revoke reported false")
	}
	must(t, s.CreateAPIToken(ctx, testToken("01TOKENB", "ci", "read")))
	wantErr(t, s.CreateAPIToken(ctx, testToken("01TOKENA", "again", "read")), store.ErrConflict, "id of a revoked token")

	for name, bad := range map[string]func(*store.APIToken){
		"empty id":      func(x *store.APIToken) { x.ID = "" },
		"empty name":    func(x *store.APIToken) { x.Name = "" },
		"empty creator": func(x *store.APIToken) { x.CreatedBy = "" },
		"scope":         func(x *store.APIToken) { x.Scope = "root" },
		"short hash":    func(x *store.APIToken) { x.Hash = x.Hash[:31] },
		"zero created":  func(x *store.APIToken) { x.CreatedAt = time.Time{} },
	} {
		x := testToken("01TOKENX", "x", "read")
		bad(&x)
		wantErr(t, s.CreateAPIToken(ctx, x), store.ErrInvalid, name)
	}
	n, err := s.CountAPITokens(ctx)
	must(t, err)
	if n != 2 {
		t.Fatalf("CountAPITokens = %d, want 2", n)
	}
}

// Revoking twice reports false the second time and keeps the first
// revocation; an unknown id is ErrNotFound.
func testAPITokenRevoke(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateAPIToken(ctx, testToken("01TOKENA", "a", "admin")))
	ok, err := s.RevokeAPIToken(ctx, "01TOKENA", t0.Add(time.Minute), "01TOKENA")
	must(t, err)
	if !ok {
		t.Fatal("revoke reported false")
	}
	ok, err = s.RevokeAPIToken(ctx, "01TOKENA", t0.Add(time.Hour), "local")
	must(t, err)
	if ok {
		t.Fatal("second revoke reported true")
	}
	got, err := s.APITokenByID(ctx, "01TOKENA")
	must(t, err)
	if got.RevokedAt == nil || !got.RevokedAt.Equal(t0.Add(time.Minute)) || got.RevokedBy != "01TOKENA" {
		t.Fatalf("after two revokes: %v %q", got.RevokedAt, got.RevokedBy)
	}
	_, err = s.RevokeAPIToken(ctx, "01NOPE", t0, "local")
	wantErr(t, err, store.ErrNotFound, "revoke unknown")
	_, err = s.RevokeAPIToken(ctx, "01TOKENA", time.Time{}, "local")
	wantErr(t, err, store.ErrInvalid, "zero time")
	_, err = s.RevokeAPIToken(ctx, "01TOKENA", t0, "")
	wantErr(t, err, store.ErrInvalid, "empty revoker")
}

// Pages by id; revoked rows only with includeRevoked.
func testListAPITokens(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	for _, id := range []string{"01TOKENC", "01TOKENA", "01TOKENB", "01TOKEND"} {
		must(t, s.CreateAPIToken(ctx, testToken(id, "n"+id, "read")))
	}
	_, err := s.RevokeAPIToken(ctx, "01TOKENB", t0, "local")
	must(t, err)
	ids := func(ts []store.APIToken) (out []string) {
		for _, x := range ts {
			out = append(out, x.ID)
		}
		return out
	}
	page, err := s.ListAPITokens(ctx, "", 2, true)
	must(t, err)
	if got := ids(page); !reflect.DeepEqual(got, []string{"01TOKENA", "01TOKENB"}) {
		t.Fatalf("first page = %v", got)
	}
	page, err = s.ListAPITokens(ctx, "01TOKENB", 10, true)
	must(t, err)
	if got := ids(page); !reflect.DeepEqual(got, []string{"01TOKENC", "01TOKEND"}) {
		t.Fatalf("second page = %v", got)
	}
	page, err = s.ListAPITokens(ctx, "", 10, false)
	must(t, err)
	if got := ids(page); !reflect.DeepEqual(got, []string{"01TOKENA", "01TOKENC", "01TOKEND"}) {
		t.Fatalf("active = %v", got)
	}
	if page[0].Hash == nil || page[0].Name != "n01TOKENA" {
		t.Fatalf("row = %+v", page[0])
	}
	_, err = s.ListAPITokens(ctx, "", 0, true)
	wantErr(t, err, store.ErrInvalid, "zero limit")
}

// CountAPITokens counts every row; CountActiveAdminTokens excludes revoked,
// expired and non-admin rows (ADR 0030 T4).
func testCountAPITokens(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	now := t0.Add(24 * time.Hour)
	count := func() (all, admins int64) {
		t.Helper()
		all, err := s.CountAPITokens(ctx)
		must(t, err)
		admins, err = s.CountActiveAdminTokens(ctx, now)
		must(t, err)
		return all, admins
	}
	if all, admins := count(); all != 0 || admins != 0 {
		t.Fatalf("empty table: %d %d", all, admins)
	}
	must(t, s.CreateAPIToken(ctx, testToken("01TOKENR", "reader", "read")))
	must(t, s.CreateAPIToken(ctx, testToken("01TOKENW", "writer", "write")))
	revoked := testToken("01TOKENV", "revoked", "admin")
	must(t, s.CreateAPIToken(ctx, revoked))
	_, err := s.RevokeAPIToken(ctx, revoked.ID, t0, "local")
	must(t, err)
	expired := testToken("01TOKENE", "expired", "admin")
	expired.ExpiresAt = ptr(now) // expires_at > now is active; equal is expired
	must(t, s.CreateAPIToken(ctx, expired))
	if all, admins := count(); all != 4 || admins != 0 {
		t.Fatalf("no active admin: all %d, active admins %d", all, admins)
	}
	later := testToken("01TOKENL", "later", "admin")
	later.ExpiresAt = ptr(now.Add(time.Millisecond))
	must(t, s.CreateAPIToken(ctx, later))
	must(t, s.CreateAPIToken(ctx, testToken("01TOKENF", "forever", "admin")))
	if all, admins := count(); all != 6 || admins != 2 {
		t.Fatalf("all %d, active admins %d; want 6 and 2", all, admins)
	}
}

// TouchAPITokens never moves last_used_at backwards and ignores unknown ids.
func testTouchAPITokens(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	must(t, s.CreateAPIToken(ctx, testToken("01TOKENA", "a", "read")))
	must(t, s.CreateAPIToken(ctx, testToken("01TOKENB", "b", "read")))
	lastUsed := func(id string) *time.Time {
		t.Helper()
		got, err := s.APITokenByID(ctx, id)
		must(t, err)
		return got.LastUsedAt
	}
	if lastUsed("01TOKENA") != nil {
		t.Fatal("a new token has a last use")
	}
	must(t, s.TouchAPITokens(ctx, map[string]time.Time{"01TOKENA": t0.Add(time.Hour), "01NOPE": t0}))
	must(t, s.TouchAPITokens(ctx, map[string]time.Time{"01TOKENA": t0.Add(time.Minute), "01TOKENB": t0}))
	if got := lastUsed("01TOKENA"); got == nil || !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last use moved backwards: %v", got)
	}
	if got := lastUsed("01TOKENB"); got == nil || !got.Equal(t0) {
		t.Fatalf("last use of b = %v", got)
	}
	must(t, s.TouchAPITokens(ctx, nil))
}

// Two creates of the same active name, or of the same id (two replicas
// bootstrapping one token, ADR 0030 T4), race on the unique indexes: exactly
// one wins and the others get ErrConflict.
func testAPITokenCreateRace(t *testing.T, open opener) {
	s := open(t)
	ctx := t.Context()
	const n = 8
	for _, sameID := range []bool{true, false} {
		var wg sync.WaitGroup
		errs := make([]error, n)
		for i := range n {
			wg.Go(func() {
				tok := testToken("01RACE"+"ABCDEFGH"[i:i+1], "race", "admin")
				if sameID {
					tok = testToken("01BOOT", "bootstrap", "admin")
				}
				errs[i] = s.CreateAPIToken(ctx, tok)
			})
		}
		wg.Wait()
		won := 0
		for _, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, store.ErrConflict):
				t.Fatalf("sameID=%v: %v", sameID, err)
			}
		}
		if won != 1 {
			t.Fatalf("sameID=%v: %d creates won, want 1", sameID, won)
		}
	}
}
