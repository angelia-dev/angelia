package shoutrrr

import (
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/angelia-dev/angelia/internal/netguard"
	"github.com/angelia-dev/angelia/internal/secrets"
)

// errUnpinned is the error of a request to a destination outside the
// checked origins. It means the derivation is out of step with the library.
var errUnpinned = errors.New("shoutrrr: request to an unchecked destination refused")

// pin is the transport of the client a service gets for one attempt (ADR
// 0026 §4): it sends through the netguard client's transport only the
// requests whose scheme and canonical host:port are a checked origin, and
// records what it saw, which the outcome is classified from.
type pin struct {
	next http.RoundTripper
	// allowed holds "scheme://host:port" of each checked origin.
	allowed []string
	now     func() time.Time

	mu sync.Mutex
	// requests counts the requests sent; refused, those refused unsent.
	requests, refused int
	// last is the outcome of the latest request.
	last observed
}

// observed is what one request ended with: a response status (and its
// Retry-After), or a transport error, and the origin it went to.
type observed struct {
	origin     string
	status     int
	retryAfter time.Duration
	err        error
}

func pinKey(scheme, hostport string) string { return strings.ToLower(scheme) + "://" + hostport }

func (p *pin) RoundTrip(req *http.Request) (*http.Response, error) {
	key := ""
	if hp, err := secrets.Destination(req.URL); err == nil {
		key = pinKey(req.URL.Scheme, hp)
	}
	if key == "" || !slices.Contains(p.allowed, key) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		p.mu.Lock()
		p.refused++
		p.mu.Unlock()
		return nil, errUnpinned
	}
	resp, err := p.next.RoundTrip(req) //nolint:gosec // G704: next is the netguard client's transport, pinned to the checked origins above.
	o := observed{origin: strings.ToLower(req.URL.Scheme) + "://" + req.URL.Host, err: err}
	if resp != nil {
		o.status = resp.StatusCode
		o.retryAfter = netguard.ParseRetryAfter(resp.Header.Get("Retry-After"), p.now())
	}
	p.mu.Lock()
	p.requests++
	p.last = o
	p.mu.Unlock()
	return resp, err
}

// seen returns what the pin observed.
func (p *pin) seen() (requests, refused int, last observed) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests, p.refused, p.last
}

// pinnedClient returns a client sending through base's transport, pinned
// to allowed, that follows no redirect (ADR 0017 §7).
func pinnedClient(base *http.Client, allowed []string, now func() time.Time) (*http.Client, *pin) {
	next := base.Transport
	if next == nil { // as for any *http.Client; production passes netguard's
		next = http.DefaultTransport
	}
	p := &pin{next: next, allowed: allowed, now: now}
	return &http.Client{
		Transport:     p,
		Timeout:       base.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, p
}
