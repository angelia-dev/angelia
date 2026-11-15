package apitoken

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/store"
	"github.com/angelia-dev/angelia/internal/store/pgtest"
	"github.com/angelia-dev/angelia/internal/store/postgres"
	"github.com/angelia-dev/angelia/internal/store/sqlite"
)

// writeBootstrap writes content to a new file with mode perm.
func writeBootstrap(t *testing.T, content string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bootstrap-token")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, perm); err != nil {
		t.Fatal(err)
	}
	return p
}

// logBuffer is a JSON logger into a buffer that is safe for concurrent use.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func newLog() (*slog.Logger, *logBuffer) {
	b := &logBuffer{}
	return slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})), b
}

// noSecret fails when any output holds the token or its secret part.
func noSecret(t *testing.T, tok string, outs ...string) {
	t.Helper()
	secret := tok[len(Prefix)+27:]
	for _, o := range outs {
		if strings.Contains(o, tok) || strings.Contains(o, secret) || strings.Contains(o, secret[:20]) {
			t.Fatalf("the token leaked: %s", o)
		}
	}
}

// tokenStores runs fn on each engine with a function that opens another
// store on the same database, as a second replica would.
func tokenStores(t *testing.T, fn func(t *testing.T, open func() store.TokenStore)) {
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "angelia.db")
		fn(t, func() store.TokenStore {
			s, err := sqlite.Open(t.Context(), path, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		})
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := pgtest.DSN(t)
		fn(t, func() store.TokenStore {
			s, err := postgres.Open(t.Context(), dsn, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		})
	})
}

// The token is stored once as admin "bootstrap"; a restart is a no-op; a
// revoked bootstrap token never comes back; nothing logs the token.
func TestBootstrapOnceAndNeverAgain(t *testing.T) {
	tokenStores(t, func(t *testing.T, open func() store.TokenStore) {
		ctx := t.Context()
		st := open()
		id, tok := Generate(t0)
		path := writeBootstrap(t, tok+"\n", 0o600)
		log, logs := newLog()

		if err := Bootstrap(ctx, st, path, t0, log); err != nil {
			t.Fatal(err)
		}
		row, err := st.APITokenByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Name != "bootstrap" || row.Scope != "admin" || row.CreatedBy != "bootstrap" || !bytes.Equal(row.Hash, Hash(tok)) || !row.CreatedAt.Equal(t0) {
			t.Fatalf("stored %+v", row)
		}
		if !strings.Contains(logs.String(), "bootstrap admin token stored") || !strings.Contains(logs.String(), id) {
			t.Fatalf("logs: %s", logs)
		}

		// Restart: nothing changes, nothing is logged at WARN.
		if err := Bootstrap(ctx, st, path, t0.Add(time.Hour), log); err != nil {
			t.Fatal(err)
		}
		if n, err := st.CountAPITokens(ctx); err != nil || n != 1 {
			t.Fatalf("after restart: %d tokens, %v", n, err)
		}

		// Revoked: a restart does not bring it back.
		if _, err := st.RevokeAPIToken(ctx, id, t0.Add(2*time.Hour), "local"); err != nil {
			t.Fatal(err)
		}
		if err := Bootstrap(ctx, st, path, t0.Add(3*time.Hour), log); err != nil {
			t.Fatal(err)
		}
		row, err = st.APITokenByID(ctx, id)
		if err != nil || row.RevokedAt == nil {
			t.Fatalf("after revoke and restart: %+v %v", row, err)
		}
		if n, err := st.CountActiveAdminTokens(ctx, t0.Add(4*time.Hour)); err != nil || n != 0 {
			t.Fatalf("active admins after revoke and restart: %d, %v", n, err)
		}
		if strings.Contains(logs.String(), `"level":"WARN"`) {
			t.Fatalf("unexpected warning: %s", logs)
		}
		noSecret(t, tok, logs.String())
	})
}

// A file whose token is not in a non-empty table is ignored, with an INFO.
func TestBootstrapIgnoredWhenTokensExist(t *testing.T) {
	tokenStores(t, func(t *testing.T, open func() store.TokenStore) {
		st := open()
		create(t, st, "me", "admin", nil)
		id, tok := Generate(t0)
		log, logs := newLog()
		if err := Bootstrap(t.Context(), st, writeBootstrap(t, tok, 0o400), t0, log); err != nil {
			t.Fatal(err)
		}
		if _, err := st.APITokenByID(t.Context(), id); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("the file's token was stored: %v", err)
		}
		if !strings.Contains(logs.String(), "api.bootstrap_token_file ignored") || !strings.Contains(logs.String(), id) {
			t.Fatalf("logs: %s", logs)
		}
		noSecret(t, tok, logs.String())
	})
}

// Two replicas with the same file start at once on an empty database: one
// inserts, the others get ErrConflict, which is "already done"; all start.
func TestBootstrapReplicasRace(t *testing.T) {
	tokenStores(t, func(t *testing.T, open func() store.TokenStore) {
		id, tok := Generate(t0)
		path := writeBootstrap(t, tok, 0o600)
		const replicas = 4
		stores := make([]store.TokenStore, replicas)
		for i := range stores {
			stores[i] = open()
		}
		log, logs := newLog()
		start := make(chan struct{})
		errs := make([]error, replicas)
		var wg sync.WaitGroup
		for i, st := range stores {
			wg.Go(func() {
				<-start
				errs[i] = Bootstrap(t.Context(), st, path, t0, log)
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("replica %d: %v", i, err)
			}
		}
		if n, err := stores[0].CountAPITokens(t.Context()); err != nil || n != 1 {
			t.Fatalf("%d tokens, %v", n, err)
		}
		if _, err := stores[1].APITokenByID(t.Context(), id); err != nil {
			t.Fatal(err)
		}
		if c := strings.Count(logs.String(), "bootstrap admin token stored"); c != 1 {
			t.Fatalf("stored %d times: %s", c, logs)
		}
		noSecret(t, tok, logs.String())
	})
}

// The insert's ErrConflict, which the race above may or may not reach,
// is "already done": forced here by a store whose count lags.
func TestBootstrapConflictIsDone(t *testing.T) {
	st := openStore(t)
	_, tok := Generate(t0)
	path := writeBootstrap(t, tok, 0o600)
	if err := Bootstrap(t.Context(), st, path, t0, nil); err != nil {
		t.Fatal(err)
	}
	log, logs := newLog()
	if err := Bootstrap(t.Context(), zeroCount{st}, path, t0, log); err != nil {
		t.Fatalf("conflict: %v", err)
	}
	if !strings.Contains(logs.String(), "already stored by another process") {
		t.Fatalf("logs: %s", logs)
	}
}

type zeroCount struct{ store.TokenStore }

func (zeroCount) CountAPITokens(context.Context) (int64, error) { return 0, nil }

// Store errors fail the start.
func TestBootstrapStoreErrors(t *testing.T) {
	_, tok := Generate(t0)
	path := writeBootstrap(t, tok, 0o600)
	boom := errors.New("boom")
	for name, st := range map[string]store.TokenStore{
		"count":  failing{count: boom},
		"byid":   failing{n: 1, byID: boom},
		"create": failing{create: boom},
	} {
		if err := Bootstrap(t.Context(), st, path, t0, nil); !errors.Is(err, boom) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := WarnIfNoAdmin(t.Context(), failing{count: boom}, t0, nil); !errors.Is(err, boom) {
		t.Errorf("WarnIfNoAdmin: %v", err)
	}
	if err := Bootstrap(t.Context(), failing{count: boom}, "", t0, nil); err != nil {
		t.Errorf("no file: %v", err)
	}
}

type failing struct {
	store.TokenStore
	n                   int64
	count, byID, create error
}

func (f failing) CountAPITokens(context.Context) (int64, error) { return f.n, f.count }
func (f failing) CountActiveAdminTokens(context.Context, time.Time) (int64, error) {
	return 0, f.count
}

func (f failing) APITokenByID(context.Context, string) (store.APIToken, error) {
	return store.APIToken{}, f.byID
}
func (f failing) CreateAPIToken(context.Context, store.APIToken) error { return f.create }

// A malformed file fails the start without echoing its content; so do a
// directory, a missing file and a file others can write. A file others can
// read is accepted with a warning that holds no secret.
func TestReadBootstrapFile(t *testing.T) {
	id, tok := Generate(t0)
	for _, content := range []string{tok, tok + "\n", tok + "\r\n"} {
		gotID, gotTok, warn, err := ReadBootstrapFile(writeBootstrap(t, content, 0o600))
		if err != nil || gotID != id || gotTok != tok || warn != "" {
			t.Fatalf("%q: %q %v %q", content, gotID, err, warn)
		}
	}
	last := "a"
	if strings.HasSuffix(tok, last) {
		last = "b"
	}
	bad := map[string]string{ //nolint:gosec // G101: synthetic test tokens
		"two newlines":    tok + "\n\n",
		"leading space":   " " + tok,
		"bad checksum":    tok[:len(tok)-1] + last,
		"empty":           "",
		"two tokens":      tok + "\n" + tok + "\n",
		"not a token":     "hunter2-but-not-a-token-at-all\n",
		"lower-case id":   strings.ToLower(tok),
		"secrets key":     "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\n",
		"oversized token": strings.Repeat(tok, 100),
	}
	for name, content := range bad {
		_, _, _, err := ReadBootstrapFile(writeBootstrap(t, content, 0o600))
		if err == nil || !strings.Contains(err.Error(), "not an API token") {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if content != "" && strings.Contains(err.Error(), strings.TrimSpace(content)[:10]) {
			t.Errorf("%s: the error echoes the content: %v", name, err)
		}
		noSecret(t, tok, err.Error())
	}
	if _, _, _, err := ReadBootstrapFile(t.TempDir()); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: %v", err)
	}
	if _, _, _, err := ReadBootstrapFile(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing: %v", err)
	}
	// A symbolic link to the file is followed (Kubernetes secret volumes).
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(writeBootstrap(t, tok, 0o400), link); err != nil {
		t.Fatal(err)
	}
	if gotID, _, _, err := ReadBootstrapFile(link); err != nil || gotID != id {
		t.Errorf("symlink: %q %v", gotID, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, perm := range []os.FileMode{0o644, 0o640, 0o604} {
		gotID, _, warn, err := ReadBootstrapFile(writeBootstrap(t, tok, perm))
		if err != nil || gotID != id || !strings.Contains(warn, "readable by group or others") || !strings.Contains(warn, "chmod 600") {
			t.Errorf("mode %04o: %q %v %q", perm, gotID, err, warn)
		}
		noSecret(t, tok, warn)
	}
	for _, perm := range []os.FileMode{0o666, 0o620, 0o602} {
		_, _, _, err := ReadBootstrapFile(writeBootstrap(t, tok, perm))
		if err == nil || !strings.Contains(err.Error(), "writable by group or others") {
			t.Errorf("mode %04o: %v", perm, err)
		}
	}
}

// A world-readable bootstrap file still bootstraps, with one WARN that holds
// no secret; a malformed one fails Bootstrap before any store access.
func TestBootstrapFileModeAndMalformed(t *testing.T) {
	st := openStore(t)
	_, tok := Generate(t0)
	log, logs := newLog()
	if runtime.GOOS != "windows" {
		if err := Bootstrap(t.Context(), st, writeBootstrap(t, tok, 0o644), t0, log); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(logs.String(), `"level":"WARN"`) || !strings.Contains(logs.String(), "readable by group or others") {
			t.Fatalf("logs: %s", logs)
		}
		noSecret(t, tok, logs.String())
	}
	bad := writeBootstrap(t, tok[:50], 0o600)
	err := Bootstrap(t.Context(), failing{count: errors.New("store touched")}, bad, t0, log)
	if err == nil || !strings.Contains(err.Error(), "not an API token") {
		t.Fatalf("malformed: %v", err)
	}
	noSecret(t, tok, err.Error(), logs.String())
	if strings.Contains(err.Error(), tok[:50]) {
		t.Fatalf("the error echoes the file: %v", err)
	}
}

// The warning fires while no admin token is active: none, revoked, expired
// or non-admin only; not once one is.
func TestWarnIfNoAdmin(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	warns := func() int {
		log, logs := newLog()
		if err := WarnIfNoAdmin(ctx, st, t0.Add(time.Hour), log); err != nil {
			t.Fatal(err)
		}
		if c := strings.Count(logs.String(), "no active admin API token"); c > 0 {
			if !strings.Contains(logs.String(), "angelia token create --local") || !strings.Contains(logs.String(), `"level":"WARN"`) {
				t.Fatalf("warning: %s", logs)
			}
			return c
		}
		return 0
	}
	if warns() != 1 {
		t.Fatal("empty table: no warning")
	}
	revID, _ := create(t, st, "old", "admin", nil)
	if _, err := st.RevokeAPIToken(ctx, revID, t0, "local"); err != nil {
		t.Fatal(err)
	}
	create(t, st, "expired", "admin", func(r *store.APIToken) { e := t0.Add(time.Minute); r.ExpiresAt = &e })
	create(t, st, "writer", "write", nil)
	if warns() != 1 {
		t.Fatal("revoked, expired and write tokens only: no warning")
	}
	create(t, st, "admin", "admin", nil)
	if warns() != 0 {
		t.Fatal("warning with an active admin token")
	}
}
