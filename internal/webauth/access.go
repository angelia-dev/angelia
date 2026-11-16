package webauth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
)

// PinStore pins a login to the subject seen at its first match
// ([store.SessionStore] has it).
type PinStore interface {
	PinUIIdentity(ctx context.Context, provider, login, subject string, at time.Time) (string, error)
}

// accessList evaluates ui.access (ADR 0033 §3). Today every entry is a
// GitHub entry: github_id matches the subject, github_user the login
// case-insensitively, pinned to the subject of its first match.
type accessList struct {
	entries []config.UIAccess
	pins    PinStore
	now     func() time.Time
	log     *slog.Logger

	mu sync.Mutex
	// pinned caches the pins read or written: a pin never changes, so a
	// cached one saves a store call on every request.
	pinned map[string]string
}

func newAccessList(entries []config.UIAccess, pins PinStore, now func() time.Time, log *slog.Logger) *accessList {
	return &accessList{entries: entries, pins: pins, now: now, log: log, pinned: map[string]string{}}
}

// match is the outcome of an access check.
type match struct {
	// role is the highest role an entry gives, "" for none.
	role string
	// mismatch is true when a github_user entry matched the login but the
	// login is pinned to another subject: that entry gives nothing.
	mismatch bool
	// pinnedTo is the other subject of a mismatch, for the log.
	pinnedTo string
}

// role returns the role that ui.access gives an identity now. An error is a
// store failure.
func (a *accessList) role(ctx context.Context, provider, subject, login string) (match, error) {
	var m match
	if provider != GitHubName {
		return m, nil
	}
	for _, e := range a.entries {
		grants := false
		switch {
		case e.GitHubID != 0:
			grants = githubSubject(e.GitHubID) == subject
		case e.GitHubUser != "" && strings.EqualFold(e.GitHubUser, login):
			pinned, err := a.pin(ctx, provider, strings.ToLower(login), subject)
			if err != nil {
				return match{}, err
			}
			if pinned != subject {
				m.mismatch, m.pinnedTo = true, pinned
				continue
			}
			grants = true
		}
		if grants && roleRank(e.Role) > roleRank(m.role) {
			m.role = e.Role
		}
	}
	return m, nil
}

// pin returns the subject that login is pinned to, pinning it to subject if
// it is not pinned yet.
func (a *accessList) pin(ctx context.Context, provider, login, subject string) (string, error) {
	key := provider + "\x00" + login
	a.mu.Lock()
	got, ok := a.pinned[key]
	a.mu.Unlock()
	if ok {
		return got, nil
	}
	got, err := a.pins.PinUIIdentity(ctx, provider, login, subject, a.now())
	if err != nil {
		return "", fmt.Errorf("webauth: pin %s login: %w", provider, err)
	}
	if got == subject {
		a.log.DebugContext(ctx, "ui access entry pinned", "provider", provider, "login", login, "subject", subject)
	}
	a.mu.Lock()
	a.pinned[key] = got
	a.mu.Unlock()
	return got, nil
}
