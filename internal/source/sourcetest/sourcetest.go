// Package sourcetest is the contract test suite every source.Source must
// pass. A source's own test builds the source against a fake server
// (httptest) or fixtures and calls Run:
//
//	func TestContract(t *testing.T) {
//		srv := httptest.NewServer(fakeUpstream())
//		t.Cleanup(srv.Close)
//		sourcetest.Run(t, pypi.New(srv.URL, srv.Client()), sourcetest.Fixtures{...})
//	}
package sourcetest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
)

// Fixtures describes the upstream the source under test is wired to.
type Fixtures struct {
	// Valid is a target the source accepts and that resolves to at least one
	// version on the fake upstream.
	Valid source.Target
	// Invalid lists targets Validate must reject (bad or missing coordinates,
	// wrong kind). At least one is required.
	Invalid []source.Target
	// WantRaw, if set, is the exact Raw list List must return, in order.
	WantRaw []string
	// Conditional is true when the fake upstream supports conditional
	// requests (ETag, Last-Modified or cursor). The suite then checks the
	// NotModified path.
	Conditional bool
	// HasPublishedAt is true when the fake upstream provides publication
	// dates; every version must then carry a non-zero PublishedAt.
	HasPublishedAt bool
}

// Run executes the contract suite against src as subtests of t. It must not
// be used with t.Parallel: it swaps the default HTTP transport to enforce
// that nothing leaves loopback.
func Run(t *testing.T, src source.Source, fx Fixtures) {
	t.Helper()
	guardNetwork(t)
	checks := []struct {
		name string
		fn   func(context.Context, source.Source, Fixtures) error
	}{
		{"Kind", checkKind},
		{"ValidateAccepts", checkValidateAccepts},
		{"ValidateRejects", checkValidateRejects},
		{"ListDeterministic", checkDeterministic},
		{"ListMatchesFixture", checkWantRaw},
		{"ListHonoursContext", checkContext},
		{"NotModified", checkNotModified},
		{"PublishedAt", checkPublishedAt},
	}
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.fn(ctx, src, fx); err != nil {
				t.Error(err)
			}
		})
	}
}

func list(ctx context.Context, src source.Source, t source.Target, s source.FetchState) (source.Result, error) {
	res, err := src.List(ctx, t, s)
	if err != nil {
		return res, fmt.Errorf("List(%v): %w", t, err)
	}
	return res, nil
}

func checkKind(_ context.Context, src source.Source, fx Fixtures) error {
	if src.Kind() == "" {
		return errors.New("Kind() is empty")
	}
	if fx.Valid.Kind != src.Kind() {
		return fmt.Errorf("fixture Valid.Kind = %q, source Kind() = %q", fx.Valid.Kind, src.Kind())
	}
	return nil
}

func checkValidateAccepts(_ context.Context, src source.Source, fx Fixtures) error {
	if err := src.Validate(fx.Valid); err != nil {
		return fmt.Errorf("Validate rejects the valid fixture %v: %w", fx.Valid, err)
	}
	return nil
}

func checkValidateRejects(_ context.Context, src source.Source, fx Fixtures) error {
	if len(fx.Invalid) == 0 {
		return errors.New("fixtures list no Invalid targets")
	}
	var errs []error
	for _, bad := range fx.Invalid {
		err := src.Validate(bad)
		switch {
		case err == nil:
			errs = append(errs, fmt.Errorf("Validate accepts invalid target %v", bad))
		case !errors.Is(err, source.ErrInvalidTarget):
			errs = append(errs, fmt.Errorf("Validate(%v) error does not wrap ErrInvalidTarget: %w", bad, err))
		}
	}
	return errors.Join(errs...)
}

func checkDeterministic(ctx context.Context, src source.Source, fx Fixtures) error {
	a, err := list(ctx, src, fx.Valid, source.FetchState{})
	if err != nil {
		return err
	}
	if a.NotModified {
		return errors.New("List with empty state returned NotModified")
	}
	if len(a.Versions) == 0 {
		return errors.New("List returned no versions for the valid fixture")
	}
	for i, v := range a.Versions {
		if v.Raw == "" {
			return fmt.Errorf("version %d has an empty Raw", i)
		}
	}
	b, err := list(ctx, src, fx.Valid, source.FetchState{})
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(a, b) {
		return fmt.Errorf("List is not deterministic:\nfirst:  %+v\nsecond: %+v", a, b)
	}
	return nil
}

func checkWantRaw(ctx context.Context, src source.Source, fx Fixtures) error {
	if fx.WantRaw == nil {
		return nil
	}
	res, err := list(ctx, src, fx.Valid, source.FetchState{})
	if err != nil {
		return err
	}
	got := make([]string, len(res.Versions))
	for i, v := range res.Versions {
		got[i] = v.Raw
	}
	if !slices.Equal(got, fx.WantRaw) {
		return fmt.Errorf("versions = %v, want %v", got, fx.WantRaw)
	}
	return nil
}

func checkContext(parent context.Context, src source.Source, fx Fixtures) error {
	ctx, cancel := context.WithCancel(parent)
	cancel()
	_, err := src.List(ctx, fx.Valid, source.FetchState{})
	if err == nil {
		return errors.New("List ignores a cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		return fmt.Errorf("List on a cancelled context must wrap context.Canceled: %w", err)
	}
	return nil
}

func checkNotModified(ctx context.Context, src source.Source, fx Fixtures) error {
	if !fx.Conditional {
		return nil
	}
	first, err := list(ctx, src, fx.Valid, source.FetchState{})
	if err != nil {
		return err
	}
	if first.State.IsZero() {
		return errors.New("fixture is Conditional but List returned a zero State")
	}
	second, err := list(ctx, src, fx.Valid, first.State)
	if err != nil {
		return err
	}
	if !second.NotModified {
		return errors.New("List with the previous State did not report NotModified")
	}
	if len(second.Versions) != 0 {
		return fmt.Errorf("NotModified result carries %d versions", len(second.Versions))
	}
	if second.State.IsZero() {
		return errors.New("NotModified result dropped the State; the next call would refetch")
	}
	return nil
}

func checkPublishedAt(ctx context.Context, src source.Source, fx Fixtures) error {
	if !fx.HasPublishedAt {
		return nil
	}
	res, err := list(ctx, src, fx.Valid, source.FetchState{})
	if err != nil {
		return err
	}
	for _, v := range res.Versions {
		if v.PublishedAt.IsZero() {
			return fmt.Errorf("version %q has no PublishedAt although upstream provides it", v.Raw)
		}
	}
	return nil
}

// guardNetwork makes the default HTTP transport refuse any host that is not
// loopback (httptest servers), so a source that escapes its fake server fails
// instead of reaching a live registry. The transport is restored on cleanup.
func guardNetwork(t *testing.T) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = loopbackOnly{next: orig}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

type loopbackOnly struct{ next http.RoundTripper }

func (l loopbackOnly) RoundTrip(r *http.Request) (*http.Response, error) {
	if !isLoopback(r.URL.Hostname()) {
		return nil, fmt.Errorf("sourcetest: network access to %q blocked, use the fake server", r.URL.Host)
	}
	return l.next.RoundTrip(r)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
