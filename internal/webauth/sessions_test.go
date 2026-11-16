package webauth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/api"
	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// clock is a settable test clock.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// countingStore wraps the sqlite store to count and fail calls.
type countingStore struct {
	*sqlite.Store
	mu        sync.Mutex
	touches   []map[string]time.Time
	failTouch error
	failRead  error
	failPin   error
	failMake  error
	pins      int
}

func (s *countingStore) TouchUISessions(ctx context.Context, seen map[string]time.Time) error {
	s.mu.Lock()
	fail := s.failTouch
	if fail == nil {
		cp := map[string]time.Time{}
		for k, v := range seen {
			cp[k] = v
		}
		s.touches = append(s.touches, cp)
	}
	s.mu.Unlock()
	if fail != nil {
		return fail
	}
	return s.Store.TouchUISessions(ctx, seen)
}

func (s *countingStore) UISessionByID(ctx context.Context, id string) (store.UISession, error) {
	if s.failRead != nil {
		return store.UISession{}, s.failRead
	}
	return s.Store.UISessionByID(ctx, id)
}

func (s *countingStore) PinUIIdentity(ctx context.Context, provider, login, subject string, at time.Time) (string, error) {
	s.mu.Lock()
	s.pins++
	s.mu.Unlock()
	if s.failPin != nil {
		return "", s.failPin
	}
	return s.Store.PinUIIdentity(ctx, provider, login, subject, at)
}

func (s *countingStore) CreateUISession(ctx context.Context, row store.UISession) error {
	if s.failMake != nil {
		return s.failMake
	}
	return s.Store.CreateUISession(ctx, row)
}

func (s *countingStore) nTouches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.touches)
}

func newTestStore(t *testing.T) *countingStore {
	t.Helper()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "angelia.db"), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &countingStore{Store: s}
}

// testUI is an enabled ui section: alice (id 1001) is an admin by login,
// bob (id 1002) an editor by id, and the "viewers" login a viewer.
func testUI() config.UI {
	return config.UI{
		Enabled: true, PublicURL: "https://angelia.example.com",
		Access: []config.UIAccess{
			{GitHubUser: "Alice", Role: config.UIRoleAdmin},
			{GitHubID: 1002, Role: config.UIRoleEditor},
			{GitHubUser: "carol", Role: config.UIRoleViewer},
		},
		Session: config.UISession{IdleTimeout: time.Hour, MaxLifetime: 24 * time.Hour},
	}
}

var (
	alice = Identity{Provider: GitHubName, Subject: "1001", Login: "alice", Name: "Alice", AvatarURL: "https://avatars.example/a"}
	bob   = Identity{Provider: GitHubName, Subject: "1002", Login: "bob"}
	carol = Identity{Provider: GitHubName, Subject: "1003", Login: "carol"}
	mal   = Identity{Provider: GitHubName, Subject: "6666", Login: "ALICE"}
	eve   = Identity{Provider: GitHubName, Subject: "1005", Login: "eve"}
)

func newTestSessions(t *testing.T, ui config.UI) (*Sessions, *countingStore, *clock, *syncBuf) {
	t.Helper()
	st, c := newTestStore(t), newClock()
	logs := &syncBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewSessions(st, ui, log, WithClock(c.Now)), st, c, logs
}

func signIn(t *testing.T, s *Sessions, id Identity) (string, store.UISession) {
	t.Helper()
	cookie, row, m, err := s.signIn(t.Context(), id, "Mozilla/5.0\x01 test")
	if err != nil || cookie == "" {
		t.Fatalf("signIn(%s) = %q, %+v, %v", id.Login, cookie, m, err)
	}
	return cookie, row
}

func wantReason(t *testing.T, err error, reason string) {
	t.Helper()
	var ae *AuthError
	if !errors.As(err, &ae) || ae.Reason != reason || !errors.Is(err, api.ErrUnauthenticated) {
		t.Fatalf("err = %v, want reason %s", err, reason)
	}
}

func TestSessionsAuthenticate(t *testing.T) {
	s, _, c, _ := newTestSessions(t, testUI())
	if s.IdleTimeout() != time.Hour+IdleSlack {
		t.Fatal(s.IdleTimeout())
	}
	cookie, row := signIn(t, s, alice)
	if row.UserAgent != "Mozilla/5.0 test" || row.Login != "alice" || !row.ExpiresAt.Equal(c.Now().Add(24*time.Hour)) {
		t.Fatalf("%+v", row)
	}
	c.Add(time.Minute)
	p, err := s.AuthenticateSession(t.Context(), cookie)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "github:1001" || p.Name != "alice" || p.Scope != api.ScopeAdmin || p.Session == nil ||
		p.Session.ID != row.ID || p.Session.Role != config.UIRoleAdmin || p.Session.AvatarURL != alice.AvatarURL {
		t.Fatalf("%+v %+v", p, p.Session)
	}
	bc, _ := signIn(t, s, bob)
	if p, err := s.AuthenticateSession(t.Context(), bc); err != nil || p.Scope != api.ScopeWrite {
		t.Fatalf("bob: %+v %v", p, err)
	}
	cc, _ := signIn(t, s, carol)
	if p, err := s.AuthenticateSession(t.Context(), cc); err != nil || p.Scope != api.ScopeRead {
		t.Fatalf("carol: %+v %v", p, err)
	}

	// Malformed, unknown id, wrong secret: none of them names the cookie.
	_, other := NewCookie(c.Now())
	wrong := cookie[:len(cookie)-1] + map[bool]string{true: "b", false: "a"}[strings.HasSuffix(cookie, "a")]
	for reason, v := range map[string]string{
		api.AuthReasonMalformed: "angs_nope",
		api.AuthReasonUnknown:   other,
	} {
		_, err := s.AuthenticateSession(t.Context(), v)
		wantReason(t, err, reason)
		if strings.Contains(err.Error(), v) {
			t.Fatal("the error holds the cookie")
		}
	}
	_, err = s.AuthenticateSession(t.Context(), wrong)
	wantReason(t, err, api.AuthReasonUnknown)
	var ae *AuthError
	if !errors.As(err, &ae) || ae.ID != "" {
		t.Fatal(err)
	}
	if reason, id := ae.FailureReason(); reason != api.AuthReasonUnknown || id != "" {
		t.Fatal(reason, id)
	}
}

func TestSessionsEnd(t *testing.T) {
	t.Run("idle", func(t *testing.T) {
		s, _, c, _ := newTestSessions(t, testUI())
		cookie, row := signIn(t, s, alice)
		// Used within the idle timeout, the session lives on although
		// nothing was written yet (the pending use counts).
		c.Add(50 * time.Minute)
		if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
		c.Add(50 * time.Minute)
		if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
		// Within the slack of a use on another replica, it lives on.
		c.Add(time.Hour + IdleSlack - time.Second)
		if _, err := s.CheckSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
		c.Add(time.Second)
		_, err := s.AuthenticateSession(t.Context(), cookie)
		wantReason(t, err, api.AuthReasonExpired)
		var ae *AuthError
		if !errors.As(err, &ae) || ae.ID != row.ID || !strings.Contains(err.Error(), row.ID) {
			t.Fatal(err)
		}
	})
	t.Run("used on another replica", func(t *testing.T) {
		// Replica B serves every request; A only sees the store, which B
		// writes up to TouchInterval plus FlushTick after the use.
		st, c := newTestStore(t), newClock()
		a := NewSessions(st, testUI(), nil, WithClock(c.Now))
		b := NewSessions(st, testUI(), nil, WithClock(c.Now))
		cookie, _ := signIn(t, a, alice)
		if _, err := b.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
		if err := b.Flush(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		// B's later uses are coalesced: the store keeps the first one.
		for range 11 {
			c.Add(5 * time.Minute)
			if _, err := b.AuthenticateSession(t.Context(), cookie); err != nil {
				t.Fatal(err)
			}
		}
		// 55 minutes after the stored use, B's last use was just now and
		// its write is still pending: A must not end the session.
		c.Add(5*time.Minute + FlushTick - time.Second)
		if _, err := a.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatalf("ended early on another replica: %v", err)
		}
	})
	t.Run("open stream only", func(t *testing.T) {
		// The request that opened a stream counts as a use; the stream's
		// re-checks do not, so the session ends at its idle timeout (plus
		// the replica slack) however long the stream stays open.
		s, st, c, _ := newTestSessions(t, testUI())
		cookie, row := signIn(t, s, alice)
		c.Add(time.Minute)
		if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
		opened := c.Now()
		var err error
		for range 200 {
			c.Add(30 * time.Second)
			if _, err = s.CheckSession(t.Context(), cookie); err != nil {
				break
			}
			if ferr := s.Flush(t.Context(), false); ferr != nil {
				t.Fatal(ferr)
			}
		}
		wantReason(t, err, api.AuthReasonExpired)
		if ended := c.Now().Sub(opened); ended < time.Hour+IdleSlack || ended > time.Hour+IdleSlack+30*time.Second {
			t.Fatalf("ended %s after the last use", ended)
		}
		r, _ := st.UISessionByID(t.Context(), row.ID)
		if !r.LastSeenAt.Equal(opened) {
			t.Fatalf("a re-check moved last_seen_at: %v, want %v", r.LastSeenAt, opened)
		}
	})
	t.Run("max lifetime", func(t *testing.T) {
		s, _, c, _ := newTestSessions(t, testUI())
		cookie, _ := signIn(t, s, alice)
		for range 24 {
			c.Add(59 * time.Minute)
			if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
				break
			}
		}
		c.Add(time.Hour)
		_, err := s.AuthenticateSession(t.Context(), cookie)
		wantReason(t, err, api.AuthReasonExpired)
	})
	t.Run("max lifetime lowered", func(t *testing.T) {
		st, c := newTestStore(t), newClock()
		s := NewSessions(st, testUI(), nil, WithClock(c.Now))
		cookie, _ := signIn(t, s, alice)
		ui := testUI()
		ui.Session.MaxLifetime = 30 * time.Minute
		s2 := NewSessions(st, ui, nil, WithClock(c.Now))
		c.Add(31 * time.Minute)
		if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
		_, err := s2.AuthenticateSession(t.Context(), cookie)
		wantReason(t, err, api.AuthReasonExpired)
	})
	t.Run("revoked", func(t *testing.T) {
		s, st, c, _ := newTestSessions(t, testUI())
		cookie, row := signIn(t, s, alice)
		if _, err := st.RevokeUISession(t.Context(), row.ID, c.Now(), "github:1001"); err != nil {
			t.Fatal(err)
		}
		_, err := s.AuthenticateSession(t.Context(), cookie)
		wantReason(t, err, api.AuthReasonRevoked)
	})
	t.Run("removed from access", func(t *testing.T) {
		st, c := newTestStore(t), newClock()
		s := NewSessions(st, testUI(), nil, WithClock(c.Now))
		bc, _ := signIn(t, s, bob)
		ui := testUI()
		ui.Access = ui.Access[:1]
		s2 := NewSessions(st, ui, nil, WithClock(c.Now))
		_, err := s2.AuthenticateSession(t.Context(), bc)
		wantReason(t, err, api.AuthReasonRevoked)
		// A role change applies at the next request.
		ui.Access = append(ui.Access, config.UIAccess{GitHubID: 1002, Role: config.UIRoleViewer})
		s3 := NewSessions(st, ui, nil, WithClock(c.Now))
		if p, err := s3.AuthenticateSession(t.Context(), bc); err != nil || p.Scope != api.ScopeRead {
			t.Fatalf("%+v %v", p, err)
		}
	})
	t.Run("store failure", func(t *testing.T) {
		s, st, _, _ := newTestSessions(t, testUI())
		cookie, _ := signIn(t, s, alice)
		boom := errors.New("boom")
		st.failRead = boom
		if _, err := s.AuthenticateSession(t.Context(), cookie); !errors.Is(err, boom) || errors.Is(err, api.ErrUnauthenticated) {
			t.Fatal(err)
		}
		st.failRead = nil
		// The pin is cached: a failing pin store only matters for a new login.
		st.failPin = boom
		if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := s.signIn(t.Context(), carol, ""); !errors.Is(err, boom) {
			t.Fatal(err)
		}
		st.failPin = nil
		st.failMake = boom
		if _, _, _, err := s.signIn(t.Context(), alice, ""); !errors.Is(err, boom) {
			t.Fatal(err)
		}
	})
}

func TestSessionsAccessPinning(t *testing.T) {
	s, st, _, _ := newTestSessions(t, testUI())
	signIn(t, s, alice)
	// Another account that took the login "alice" gets nothing.
	cookie, _, m, err := s.signIn(t.Context(), mal, "")
	if err != nil || cookie != "" || !m.mismatch || m.pinnedTo != "1001" || m.role != "" {
		t.Fatalf("%q %+v %v", cookie, m, err)
	}
	// Nobody else is let in either.
	if cookie, _, m, err := s.signIn(t.Context(), eve, ""); err != nil || cookie != "" || m.mismatch {
		t.Fatalf("%q %+v %v", cookie, m, err)
	}
	// Another provider matches no entry.
	if m, err := s.access.role(t.Context(), "gitlab", "1001", "alice", ""); err != nil || m.role != "" {
		t.Fatal(m, err)
	}
	// A mismatched login entry does not hide another entry's grant.
	ui := testUI()
	ui.Access = append(ui.Access, config.UIAccess{GitHubID: 6666, Role: config.UIRoleViewer})
	s2 := NewSessions(st, ui, nil)
	cookie, _, m, err = s2.signIn(t.Context(), mal, "")
	if err != nil || cookie == "" || !m.mismatch || m.role != config.UIRoleViewer {
		t.Fatalf("%q %+v %v", cookie, m, err)
	}
	// The pins are cached per process.
	before := st.pins
	if _, err := s2.access.role(t.Context(), GitHubName, "6666", "alice", ""); err != nil || st.pins != before {
		t.Fatal(err, st.pins, before)
	}
}

func TestSessionsSignOut(t *testing.T) {
	s, st, _, _ := newTestSessions(t, testUI())
	cookie, row := signIn(t, s, alice)
	if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
		t.Fatal(err)
	}
	got, changed, err := s.signOut(t.Context(), cookie)
	if err != nil || !changed || got.ID != row.ID {
		t.Fatal(got, changed, err)
	}
	r, err := st.UISessionByID(t.Context(), row.ID)
	if err != nil || r.RevokedAt == nil || r.RevokedBy != "github:1001" {
		t.Fatalf("%+v %v", r, err)
	}
	if _, changed, err := s.signOut(t.Context(), cookie); err != nil || changed {
		t.Fatal(changed, err)
	}
	if _, changed, err := s.signOut(t.Context(), "junk"); err != nil || changed {
		t.Fatal(changed, err)
	}
	st.failRead = errors.New("boom")
	if _, _, err := s.signOut(t.Context(), cookie); err == nil {
		t.Fatal("store failure hidden")
	}
	st.failRead = nil
	_ = st.Close()
	if _, _, err := s.signOut(t.Context(), cookie); err == nil {
		t.Fatal("closed store accepted")
	}
}

func TestSessionsFlush(t *testing.T) {
	s, st, c, _ := newTestSessions(t, testUI())
	cookie, row := signIn(t, s, alice)
	use := func() {
		t.Helper()
		if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(t.Context(), false); err != nil || st.nTouches() != 0 {
		t.Fatal("empty flush wrote", err)
	}
	c.Add(time.Minute)
	use()
	if err := s.Flush(t.Context(), false); err != nil || st.nTouches() != 1 {
		t.Fatal(err, st.nTouches())
	}
	r, _ := st.UISessionByID(t.Context(), row.ID)
	if !r.LastSeenAt.Equal(c.Now()) {
		t.Fatal(r.LastSeenAt)
	}
	// Within TouchInterval of the last write: coalesced.
	c.Add(time.Minute)
	use()
	use()
	if err := s.Flush(t.Context(), false); err != nil || st.nTouches() != 1 {
		t.Fatal(err, st.nTouches())
	}
	c.Add(TouchInterval)
	if err := s.Flush(t.Context(), false); err != nil || st.nTouches() != 2 {
		t.Fatal(err, st.nTouches())
	}
	// A failed write keeps the use for the next flush.
	c.Add(time.Minute)
	use()
	st.failTouch = errors.New("boom")
	if err := s.Flush(t.Context(), true); err == nil {
		t.Fatal("failure hidden")
	}
	st.failTouch = nil
	if err := s.Flush(t.Context(), true); err != nil || st.nTouches() != 3 {
		t.Fatal(err, st.nTouches())
	}
	// Written entries are forgotten after TouchInterval without use.
	c.Add(2 * TouchInterval)
	if err := s.Flush(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	n := len(s.written) + len(s.pending)
	s.mu.Unlock()
	if n != 0 {
		t.Fatal("state kept", n)
	}
	// Sign-out forgets a pending use.
	use()
	if _, _, err := s.signOut(t.Context(), cookie); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(t.Context(), true); err != nil || st.nTouches() != 3 {
		t.Fatal(err, st.nTouches())
	}
}

func TestSessionsRun(t *testing.T) {
	s, st, c, logs := newTestSessions(t, testUI())
	if err := s.Run(t.Context(), 0, time.Second); err == nil {
		t.Fatal("zero tick accepted")
	}
	cookie, _ := signIn(t, s, alice)
	c.Add(time.Minute)
	if _, err := s.AuthenticateSession(t.Context(), cookie); err != nil {
		t.Fatal(err)
	}
	st.failTouch = errors.New("boom")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- s.Run(ctx, time.Millisecond, time.Second) }()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "ui session last use not recorded") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if !strings.Contains(out, "not recorded at shutdown") || strings.Contains(out, cookie) {
		t.Fatal(out)
	}
	// The shutdown flush writes the use.
	st.failTouch = nil
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx, time.Hour, time.Second); err != nil || st.nTouches() != 1 {
		t.Fatal(err, st.nTouches())
	}
}

// syncBuf is a log buffer safe to read while a goroutine logs.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Tokens that a signed-in user created outlive their access: the api role
// warns of the active ones whose creator ui.access no longer lists.
func TestSessionsOrphanTokens(t *testing.T) {
	s, st, c, logs := newTestSessions(t, testUI())
	// alice (github_user "Alice") is listed once pinned by a sign-in; bob
	// by his id. dave (1004) and the login "carol" pinned to 1003 are not
	// listed below.
	signIn(t, s, alice)
	signIn(t, s, carol)
	past := c.Now().Add(-time.Hour)
	for i, tk := range []struct {
		by      string
		expires *time.Time
		revoked bool
	}{
		{by: "github:1001"},
		{by: "github:1002"},
		{by: "github:1003"},
		{by: "github:1004"},
		{by: "local"},
		{by: "bootstrap"},
		{by: "01JD0000000000000000000000"},
		{by: "github:1004", expires: &past},
		{by: "github:1004", revoked: true},
	} {
		id := fmt.Sprintf("01JD00000000000000000000%02d", i)
		err := st.CreateAPIToken(t.Context(), store.APIToken{
			ID: id, Name: fmt.Sprintf("t%d", i), Scope: store.TokenScopeRead, Hash: bytes.Repeat([]byte{byte(i)}, 32),
			CreatedAt: past.Add(-time.Hour), CreatedBy: tk.by, ExpiresAt: tk.expires,
		})
		if err != nil {
			t.Fatal(err)
		}
		if tk.revoked {
			if _, err := st.RevokeAPIToken(t.Context(), id, past, "local"); err != nil {
				t.Fatal(err)
			}
		}
	}
	// carol is dropped from the list: her token and dave's are reported.
	ui := testUI()
	ui.Access = ui.Access[:2]
	s2 := NewSessions(st, ui, slog.New(slog.NewTextHandler(logs, nil)), WithClock(c.Now))
	ids, err := s2.OrphanTokens(t.Context(), st)
	if err != nil || strings.Join(ids, ",") != "01JD0000000000000000000002,01JD0000000000000000000003" {
		t.Fatalf("%v %v", ids, err)
	}
	s2.WarnOrphanTokens(t.Context(), st)
	out := logs.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "ui.access no longer lists") ||
		!strings.Contains(out, "token_ids=\"[01JD0000000000000000000002 01JD0000000000000000000003]\"") {
		t.Fatal(out)
	}
	// Nothing to report, nothing logged; a failed read is logged.
	ui = testUI()
	ui.Access = append(ui.Access, config.UIAccess{GitHubID: 1004, Role: config.UIRoleViewer})
	s3 := NewSessions(st, ui, slog.New(slog.NewTextHandler(logs, nil)), WithClock(c.Now))
	before := len(logs.String())
	s3.WarnOrphanTokens(t.Context(), st)
	if len(logs.String()) != before {
		t.Fatalf("all listed, still warned: %s", logs.String()[before:])
	}
	_ = st.Close()
	s3.WarnOrphanTokens(t.Context(), st)
	if !strings.Contains(logs.String(), "not checked against ui.access") {
		t.Fatal(logs.String())
	}
}

func TestCutUserAgent(t *testing.T) {
	if got := cutUserAgent(strings.Repeat("é", 300) + "\n"); len([]rune(got)) != maxUserAgent {
		t.Fatal(len([]rune(got)))
	}
	if got := cutUserAgent("a\x7fb\xffc"); got != "abc" {
		t.Fatalf("%q", got)
	}
}

func TestRoleScope(t *testing.T) {
	for role, want := range map[string]api.Scope{"viewer": api.ScopeRead, "editor": api.ScopeWrite, "admin": api.ScopeAdmin, "root": api.ScopeNone} {
		if RoleScope(role) != want {
			t.Fatal(role)
		}
	}
	ui := testUI()
	if SessionCookieName(ui) != SessionCookie {
		t.Fatal(SessionCookieName(ui))
	}
	ui.PublicURL = "http://127.0.0.1:8080"
	if SessionCookieName(ui) != SessionCookieLocal {
		t.Fatal(SessionCookieName(ui))
	}
	ui.GitHub.ClientID = "x"
	ui.OIDC = []config.UIOIDC{{Name: "authentik"}, {Name: "google"}}
	if len(LoginResults()) != 7 || strings.Join(ProviderNames(ui), ",") != "github,authentik,google" {
		t.Fatal(LoginResults(), ProviderNames(ui))
	}
	ui.GitHub.ClientID = ""
	if strings.Join(ProviderNames(ui), ",") != "authentik,google" {
		t.Fatal(ProviderNames(ui))
	}
}
