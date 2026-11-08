package netguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func same(l Limit) HostLimit { return HostLimit{Authenticated: l, Anonymous: l} }

func tableByHost(t *testing.T, table []EffectiveLimit) map[string]EffectiveLimit {
	t.Helper()
	m := map[string]EffectiveLimit{}
	for _, e := range table {
		m[e.Host] = e
	}
	return m
}

// ADR 0027 §2: without configuration the table is the built-in one, and
// the fallback is 5 rps with a burst of 5, listed last as "other".
func TestEffectiveLimitsDefaults(t *testing.T) {
	table, err := EffectiveLimits(nil, HostLimit{})
	if err != nil {
		t.Fatal(err)
	}
	var hosts []string
	for _, e := range table {
		hosts = append(hosts, e.Host)
	}
	if want := []string{"api.github.com", "crates.io", "gitlab.com", OtherHost}; !slices.Equal(hosts, want) {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
	builtins := DefaultHostLimits()
	for _, e := range table[:3] {
		b := builtins[e.Host]
		if e.Authenticated != b.Authenticated || e.Anonymous != b.Anonymous || e.AuthenticatedOrigin != OriginBuiltin || e.AnonymousOrigin != OriginBuiltin {
			t.Errorf("%s = %+v, want the built-in %+v", e.Host, e, b)
		}
	}
	other := table[3]
	fb := DefaultFallbackLimit()
	if other.Authenticated != fb.Authenticated || other.Anonymous != fb.Anonymous || other.AnonymousOrigin != OriginBuiltin {
		t.Errorf("other = %+v, want the built-in 5 rps / burst 5", other)
	}
}

// A configured fallback and a configured host carry their own origins; host
// names are normalised (lower case, no trailing dot).
func TestEffectiveLimitsConfigured(t *testing.T) {
	table, err := EffectiveLimits(map[string]HostLimit{"Mirror.Example.": same(Limit{Rate: 40, Burst: 40})}, same(Limit{Rate: 2, Burst: 3}))
	if err != nil {
		t.Fatal(err)
	}
	m := tableByHost(t, table)
	if e := m["mirror.example"]; e.Anonymous != (Limit{Rate: 40, Burst: 40}) || e.Authenticated != e.Anonymous ||
		e.AnonymousOrigin != OriginConfig || e.AuthenticatedOrigin != OriginConfig {
		t.Errorf("mirror.example = %+v", e)
	}
	if e := m[OtherHost]; e.Anonymous != (Limit{Rate: 2, Burst: 3}) || e.AnonymousOrigin != OriginDefault {
		t.Errorf("other = %+v, want 2 rps burst 3 from the configured default", e)
	}
	if table[len(table)-1].Host != OtherHost {
		t.Errorf("fallback is not last: %+v", table)
	}
}

// ADR 0027 §4: an entry for a built-in budget host can lower its limits,
// rate by rate and burst by burst, never raise them.
func TestEffectiveLimitsBuiltinBudgetsOnlyLowered(t *testing.T) {
	b := DefaultHostLimits()
	table, err := EffectiveLimits(map[string]HostLimit{
		"api.github.com": same(Limit{Rate: 3, Burst: 20}),    // above both buckets: capped
		"crates.io":      same(Limit{Rate: 0.5, Burst: 1}),   // lower rate
		"gitlab.com":     same(Limit{Rate: 10, Burst: 50}),   // lower rate for one bucket, burst capped
		"pypi.org":       same(Limit{Rate: 100, Burst: 200}), // no built-in budget: as configured
	}, HostLimit{})
	if err != nil {
		t.Fatal(err)
	}
	m := tableByHost(t, table)
	gh := m["api.github.com"]
	if gh.Authenticated != b["api.github.com"].Authenticated || gh.Anonymous != b["api.github.com"].Anonymous {
		t.Errorf("api.github.com = %+v, want capped at the built-in %+v", gh, b["api.github.com"])
	}
	if gh.AuthenticatedOrigin != OriginBuiltin || gh.AnonymousOrigin != OriginBuiltin {
		t.Errorf("api.github.com origins = %s/%s, want builtin: the entry changed nothing", gh.AuthenticatedOrigin, gh.AnonymousOrigin)
	}
	if c := m["crates.io"]; c.Anonymous != (Limit{Rate: 0.5, Burst: 1}) || c.AnonymousOrigin != OriginConfig {
		t.Errorf("crates.io = %+v, want lowered to 0.5 rps", c)
	}
	gl := m["gitlab.com"]
	if want := (Limit{Rate: 10, Burst: 10}); gl.Authenticated != want || gl.AuthenticatedOrigin != OriginConfig {
		t.Errorf("gitlab.com authenticated = %+v (%s), want %+v from config", gl.Authenticated, gl.AuthenticatedOrigin, want)
	}
	if gl.Anonymous != b["gitlab.com"].Anonymous || gl.AnonymousOrigin != OriginBuiltin {
		t.Errorf("gitlab.com anonymous = %+v (%s), want the built-in %+v", gl.Anonymous, gl.AnonymousOrigin, b["gitlab.com"].Anonymous)
	}
	if p := m["pypi.org"]; p.Anonymous != (Limit{Rate: 100, Burst: 200}) {
		t.Errorf("pypi.org = %+v", p)
	}
}

// The cap holds in the client the limiter uses, not only in the table, and
// keeps the entry's body cap and timeout.
func TestNewCapsBuiltinBudgets(t *testing.T) {
	c, err := New(Config{HostLimits: map[string]HostLimit{"API.GitHub.com": {
		Authenticated: Limit{Rate: 1000, Burst: 1000}, Anonymous: Limit{Rate: 1000, Burst: 1000},
		MaxBodyBytes: 1 << 20, Timeout: time.Minute,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	got := c.limiter.limits["api.github.com"]
	b := DefaultHostLimits()["api.github.com"]
	if got.Authenticated != b.Authenticated || got.Anonymous != b.Anonymous {
		t.Errorf("limiter uses %+v, want the built-in %+v", got, b)
	}
	if got.MaxBodyBytes != 1<<20 || got.Timeout != time.Minute {
		t.Errorf("body cap %d and timeout %v were not kept", got.MaxBodyBytes, got.Timeout)
	}
	if lim := c.Limits(); len(lim) != 4 || lim[0].Host != "api.github.com" || lim[0].Authenticated != b.Authenticated {
		t.Errorf("Limits() = %+v", lim)
	}
}

func TestIsBuiltinBudgetHost(t *testing.T) {
	for h, want := range map[string]bool{
		"api.github.com": true, "GitLab.com.": true, "crates.io": true,
		"pypi.org": false, "index.crates.io": false, "github.com": false,
	} {
		if got := IsBuiltinBudgetHost(h); got != want {
			t.Errorf("IsBuiltinBudgetHost(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestEffectiveLimitsRejectsInvalid(t *testing.T) {
	for name, tc := range map[string]struct {
		hosts    map[string]HostLimit
		fallback HostLimit
	}{
		"zero rate":         {hosts: map[string]HostLimit{"x": same(Limit{Rate: 0, Burst: 1})}},
		"zero burst":        {hosts: map[string]HostLimit{"x": same(Limit{Rate: 1, Burst: 0})}},
		"fallback body cap": {fallback: HostLimit{Authenticated: Limit{Rate: 1, Burst: 1}, Anonymous: Limit{Rate: 1, Burst: 1}, MaxBodyBytes: 1}},
		"fallback no rate":  {fallback: HostLimit{Authenticated: Limit{Rate: 1, Burst: 1}}},
	} {
		if _, err := EffectiveLimits(tc.hosts, tc.fallback); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

// frozenClock never moves. Sleep returns at once and records the duration
// under the host the context carries, so the slot each wait was given can
// be checked without sleeping.
type frozenClock struct {
	mu    sync.Mutex
	slept map[string][]time.Duration
}

type hostCtxKey struct{}

func (*frozenClock) Now() time.Time { return fakeEpoch }

func (f *frozenClock) Sleep(ctx context.Context, d time.Duration) error {
	if d > 0 {
		f.mu.Lock()
		h, _ := ctx.Value(hostCtxKey{}).(string)
		f.slept[h] = append(f.slept[h], d)
		f.mu.Unlock()
	}
	return ctx.Err()
}

// Concurrent waits on configured, capped and unlisted hosts at the same
// instant: each host's bucket hands out exactly its burst at once, then one
// slot every 1/rate, whatever the interleaving, and hosts never share a
// bucket. Run with -race; nothing sleeps.
func TestLimiterConcurrentConfiguredHosts(t *testing.T) {
	clock := &frozenClock{slept: map[string][]time.Duration{}}
	c, err := New(Config{Clock: clock, HostLimits: map[string]HostLimit{
		"127.0.0.1":      same(Limit{Rate: 50, Burst: 50}),
		"api.github.com": same(Limit{Rate: 100, Burst: 100}), // capped at 59/3600 and 1 anonymous
	}})
	if err != nil {
		t.Fatal(err)
	}
	const n = 200
	hosts := map[string]Limit{
		"127.0.0.1":      {Rate: 50, Burst: 50},
		"api.github.com": DefaultHostLimits()["api.github.com"].Anonymous,
		"registry.test":  DefaultFallbackLimit().Anonymous,
	}
	var wg sync.WaitGroup
	for h := range hosts {
		ctx := context.WithValue(t.Context(), hostCtxKey{}, h)
		for range n {
			wg.Go(func() {
				if err := c.limiter.wait(ctx, h, false); err != nil {
					t.Errorf("wait %s: %v", h, err)
				}
			})
		}
	}
	wg.Wait()
	for h, lim := range hosts {
		got := slices.Clone(clock.slept[h])
		slices.Sort(got)
		if len(got) != n-lim.Burst {
			t.Fatalf("%s: %d waits slept, want %d (all but the burst of %d)", h, len(got), n-lim.Burst, lim.Burst)
		}
		for i, d := range got {
			k := float64(i + 1) // the k-th request past the burst
			exact := time.Duration(k / lim.Rate * float64(time.Second))
			if d < exact || d > exact+time.Microsecond {
				t.Fatalf("%s: wait #%d = %v, want %v (slot k/rate)", h, i+1, d, exact)
			}
		}
	}
}

// A configured limit sets the bucket the client uses: 50 at once, then one
// every 20 ms.
func TestClientUsesConfiguredHostLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	c, clock := newTestClient(t, func(cfg *Config) {
		cfg.FallbackLimit = HostLimit{}
		cfg.HostLimits = map[string]HostLimit{"127.0.0.1": same(Limit{Rate: 50, Burst: 50})}
	})
	for range 52 {
		if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
			t.Fatal(err)
		}
	}
	if got := clock.Now().Sub(fakeEpoch); got != 40*time.Millisecond {
		t.Errorf("52 requests took %v of fake time, want 40ms (two past the burst of 50 at 50 rps)", got)
	}
}

// ADR 0027 §4: a configured limit is a ceiling, never a floor. A Retry-After
// on a host with a high configured limit still pauses it and the retry
// waits for it.
func TestConfiguredLimitRetryAfterWins(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock, func(w http.ResponseWriter) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	obs := &waitObs{}
	var c *Client
	c, clock = newTestClient(t, func(cfg *Config) {
		cfg.HostLimits = map[string]HostLimit{"127.0.0.1": same(Limit{Rate: 100, Burst: 200})}
		cfg.Observer = obs
	})
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
		t.Fatal(err)
	}
	if gap := (*times)[1].Sub((*times)[0]); gap < 7*time.Second {
		t.Fatalf("retried after %v, want at least the 7s of Retry-After", gap)
	}
	if !slices.Equal(obs.pauses, []string{"127.0.0.1/" + PauseRetryAfter}) {
		t.Errorf("pauses = %v", obs.pauses)
	}
	if !slices.Equal(obs.responses, []string{"127.0.0.1/429", "127.0.0.1/2xx"}) {
		t.Errorf("responses = %v", obs.responses)
	}
}

// ADR 0027 §4: X-RateLimit-* with Remaining at 0 pauses a host with a high
// configured limit until the reset; the next request waits for it.
func TestConfiguredLimitRateLimitHeadersWin(t *testing.T) {
	var clock *fakeClock
	srv, times := retryServer(t, &clock, func(w http.ResponseWriter) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(clock.Now().Add(30*time.Second).Unix(), 10))
		_, _ = io.WriteString(w, "last one")
	})
	obs := &waitObs{}
	var c *Client
	c, clock = newTestClient(t, func(cfg *Config) {
		cfg.HostLimits = map[string]HostLimit{"127.0.0.1": same(Limit{Rate: 100, Burst: 200})}
		cfg.Observer = obs
	})
	for range 2 {
		if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
			t.Fatal(err)
		}
	}
	if gap := (*times)[1].Sub((*times)[0]); gap < 30*time.Second {
		t.Fatalf("second request after %v, want the 30s to the reset", gap)
	}
	if !slices.Equal(obs.pauses, []string{"127.0.0.1/" + PauseRateLimitReset}) {
		t.Errorf("pauses = %v", obs.pauses)
	}
}

// ADR 0027 §4: every retry takes a token. With 1 rps and a burst of 1, the
// retries of 503s are spaced by the bucket even though the backoff is
// shorter.
func TestConfiguredLimitRetriesTakeTokens(t *testing.T) {
	var clock *fakeClock
	unavailable := func(w http.ResponseWriter) { w.WriteHeader(http.StatusServiceUnavailable) }
	srv, times := retryServer(t, &clock, unavailable, unavailable, unavailable)
	obs := &waitObs{}
	var c *Client
	c, clock = newTestClient(t, func(cfg *Config) {
		cfg.HostLimits = map[string]HostLimit{"127.0.0.1": same(Limit{Rate: 1, Burst: 1})}
		cfg.BaseBackoff, cfg.MaxBackoff = time.Millisecond, time.Millisecond
		cfg.Observer = obs
	})
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
		t.Fatal(err)
	}
	if len(*times) != 4 {
		t.Fatalf("%d attempts, want 4", len(*times))
	}
	for i := 1; i < 4; i++ {
		if gap := (*times)[i].Sub((*times)[i-1]); gap < time.Second {
			t.Errorf("attempt %d came %v after the previous one, want at least 1s (one token per attempt)", i+1, gap)
		}
	}
	if len(obs.waits) != 4 {
		t.Errorf("observed %d rate limit waits, want one per attempt", len(obs.waits))
	}
	if want := []string{"127.0.0.1/5xx", "127.0.0.1/5xx", "127.0.0.1/5xx", "127.0.0.1/2xx"}; !slices.Equal(obs.responses, want) {
		t.Errorf("responses = %v, want %v", obs.responses, want)
	}
}

// ADR 0027 §8: a wait refused because the slot is after the deadline is
// reported, still matches context.DeadlineExceeded, and sends nothing. A
// wait ended by a cancelled context is not a refusal.
func TestDeadlineRefusalObserved(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sent.Add(1); _, _ = io.WriteString(w, "ok") }))
	defer srv.Close()
	obs := &waitObs{}
	c, _ := newTestClient(t, func(cfg *Config) {
		cfg.HostLimits = map[string]HostLimit{"127.0.0.1": same(Limit{Rate: 1.0 / 3600, Burst: 1})}
		cfg.Observer = obs
	})
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	_, err := Fetch(ctx, c.HTTPClient(), get(t, srv.URL), Validators{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if n := sent.Load(); n != 1 {
		t.Errorf("%d requests sent, want 1", n)
	}
	if !slices.Equal(obs.refusals, []string{"127.0.0.1"}) {
		t.Errorf("refusals = %v", obs.refusals)
	}

	cctx, ccancel := context.WithCancel(t.Context())
	ccancel()
	_, _ = Fetch(cctx, c.HTTPClient(), get(t, srv.URL), Validators{})
	if len(obs.refusals) != 1 {
		t.Errorf("a cancelled context counted as a deadline refusal: %v", obs.refusals)
	}
}

// Unlisted hosts are "other" in every new observation.
func TestObserverNewHooksFoldUnlistedHosts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	obs := &waitObs{}
	c, _ := newTestClient(t, func(cfg *Config) { cfg.Observer = obs; cfg.MaxAttempts = 1 })
	if _, err := Fetch(t.Context(), c.HTTPClient(), get(t, srv.URL), Validators{}); err == nil {
		t.Fatal("want a status error")
	}
	if !slices.Equal(obs.pauses, []string{OtherHost + "/" + PauseRetryAfter}) || !slices.Equal(obs.responses, []string{OtherHost + "/429"}) {
		t.Errorf("pauses %v, responses %v", obs.pauses, obs.responses)
	}
}

func TestResponseClass(t *testing.T) {
	for code, want := range map[int]string{
		101: ClassError, 200: ClassOK, 204: ClassOK, 301: ClassRedirect, 304: ClassNotModified,
		404: ClassClientError, 403: ClassClientError, 429: ClassTooManyRequests,
		500: ClassServerError, 503: ClassServerError, 600: ClassError,
	} {
		if got := responseClass(&http.Response{StatusCode: code}, nil); got != want {
			t.Errorf("responseClass(%d) = %s, want %s", code, got, want)
		}
	}
	if got := responseClass(nil, fmt.Errorf("dial: refused")); got != ClassError {
		t.Errorf("transport error = %s", got)
	}
}
