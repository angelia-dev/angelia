package webauth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/store"
)

// PinStore pins a login to the subject seen at its first match
// ([store.SessionStore] has it).
type PinStore interface {
	PinUIIdentity(ctx context.Context, provider, login, subject string, at time.Time) (string, error)
	UIIdentityPin(ctx context.Context, provider, login string) (string, error)
}

// accessList evaluates ui.access (ADR 0033 §3). A GitHub entry's github_id
// matches the subject, its github_user the login case-insensitively, pinned
// to the subject of its first match. An OIDC entry (#268) matches its
// provider's subject, or a verified email case-insensitively, pinned the
// same way.
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

// role returns the role that ui.access gives an identity now. email is the
// identity's verified email at sign-in, "" on later requests: an email
// entry then grants through its pin only. An error is a store failure.
func (a *accessList) role(ctx context.Context, provider, subject, login, email string) (match, error) {
	if provider != GitHubName {
		return a.oidcRole(ctx, provider, subject, email)
	}
	var m match
	for _, e := range a.entries {
		if e.Provider != "" {
			continue
		}
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

// oidcRole is role for an OIDC provider's identity. An email entry whose
// address is the verified email pins it to the subject at its first match;
// any other email entry grants if its address is already pinned to the
// subject, so that it follows the account it first matched, as a
// github_user entry does.
func (a *accessList) oidcRole(ctx context.Context, provider, subject, email string) (match, error) {
	var m match
	for _, e := range a.entries {
		if e.Provider != provider {
			continue
		}
		grants := false
		switch {
		case e.Subject != "":
			grants = e.Subject == subject
		case e.Email != "" && email != "" && strings.EqualFold(e.Email, email):
			pinned, err := a.pin(ctx, provider, emailPinKey(email), subject)
			if err != nil {
				return match{}, err
			}
			if pinned != subject {
				m.mismatch, m.pinnedTo = true, pinned
				continue
			}
			grants = true
		case e.Email != "" && roleRank(e.Role) > roleRank(m.role):
			pinned, ok, err := a.readPin(ctx, provider, emailPinKey(e.Email))
			if err != nil {
				return match{}, err
			}
			grants = ok && pinned == subject
		}
		if grants && roleRank(e.Role) > roleRank(m.role) {
			m.role = e.Role
		}
	}
	return m, nil
}

// readPin returns the subject a key is pinned to, without pinning it. A
// pin found is cached; a missing one is read again next time, since a
// sign-in on another replica may create it.
func (a *accessList) readPin(ctx context.Context, provider, key string) (string, bool, error) {
	k := provider + "\x00" + key
	a.mu.Lock()
	got, ok := a.pinned[k]
	a.mu.Unlock()
	if ok {
		return got, true, nil
	}
	got, err := a.pins.UIIdentityPin(ctx, provider, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("webauth: read %s pin: %w", provider, err)
	}
	a.mu.Lock()
	a.pinned[k] = got
	a.mu.Unlock()
	return got, true, nil
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

// listsSubject reports whether an entry of ui.access gives the subject of a
// provider a role, without pinning anything: a github_id or subject entry
// by its id, a github_user or email entry only through its pin. A login or
// address nobody signed in with yet is pinned to no one, so it lists no
// subject.
func (a *accessList) listsSubject(ctx context.Context, provider, subject string) (bool, error) {
	if provider != GitHubName {
		for _, e := range a.entries {
			switch {
			case e.Provider != provider || roleRank(e.Role) == 0:
			case e.Subject != "":
				if e.Subject == subject {
					return true, nil
				}
			case e.Email != "":
				pinned, ok, err := a.readPin(ctx, provider, emailPinKey(e.Email))
				if err != nil {
					return false, err
				}
				if ok && pinned == subject {
					return true, nil
				}
			}
		}
		return false, nil
	}
	for _, e := range a.entries {
		switch {
		case e.Provider != "" || roleRank(e.Role) == 0:
		case e.GitHubID != 0:
			if githubSubject(e.GitHubID) == subject {
				return true, nil
			}
		case e.GitHubUser != "":
			pinned, err := a.pins.UIIdentityPin(ctx, provider, strings.ToLower(e.GitHubUser))
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return false, fmt.Errorf("webauth: read %s pin: %w", provider, err)
			case pinned == subject:
				return true, nil
			}
		}
	}
	return false, nil
}
