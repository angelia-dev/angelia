// Package sourcetest is the contract test suite every source.Source must
// pass. A source's own test describes a fake upstream and calls Run; Run owns
// the httptest server and hands the constructor a client that can reach
// nothing else:
//
//	func TestContract(t *testing.T) {
//		sourcetest.Run(t, nil, sourcetest.Fixtures{
//			Handler: fakeUpstream(),
//			New: func(baseURL string, c *http.Client) source.Source {
//				return pypi.New(baseURL, c)
//			},
//			Valid: ...,
//		})
//	}
package sourcetest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/source"
)

// Fixtures describes the upstream the source under test is wired to.
type Fixtures struct {
	// Handler, with New, makes Run own a fake upstream: it starts an httptest
	// server for Handler, builds the source with New(srv.URL, client) and
	// passes Run a client from Client, so the suite can assert that Validate
	// sends no request and that nothing leaves the server. Leave both unset
	// for sources that need no HTTP (the in-memory Fake) and pass the source
	// to Run instead.
	Handler http.Handler
	New     func(baseURL string, c *http.Client) source.Source

	// Valid is a target the source accepts and that resolves to at least one
	// version on the fake upstream.
	Valid source.Target
	// Invalid lists targets Validate must reject (bad or missing coordinates,
	// wrong kind). At least one is required.
	Invalid []source.Target
	// Missing, if set, is a valid target that does not exist upstream; List
	// must return an error for it (not an empty result).
	Missing *source.Target
	// WantRaw, if set, is the exact Raw list List must return, in order.
	WantRaw []string
	// Conditional is true when the fake upstream supports conditional
	// requests (ETag, Last-Modified or cursor). The suite then checks the
	// NotModified path.
	Conditional bool
	// Change, if set together with Conditional, mutates the upstream so that
	// Valid has new versions (and a new ETag or cursor). The suite then
	// checks that the state from before the change no longer yields
	// NotModified. It runs last, after every other check, because it alters
	// the upstream.
	Change func()
	// HasPublishedAt is true when the fake upstream provides publication
	// dates; every version must then carry a non-zero PublishedAt.
	HasPublishedAt bool
	// Aliases, if set, are other spellings of Valid (different case, say)
	// that name the same upstream object. The source must then implement
	// source.Canonicalizer, and every alias must have the canonical target of
	// Valid. See ADR 0021.
	Aliases []source.Target
}

// Run executes the contract suite as subtests of t. Pass src directly for a
// source without HTTP, or leave src nil and set Fixtures.Handler and
// Fixtures.New so Run owns the fake server and its guarded client.
func Run(t *testing.T, src source.Source, fx Fixtures) {
	t.Helper()
	var client *http.Client
	switch {
	case src != nil && fx.New != nil:
		t.Fatal("sourcetest: pass either src or Fixtures.New, not both")
	case src == nil && (fx.New == nil || fx.Handler == nil):
		t.Fatal("sourcetest: pass src, or set both Fixtures.Handler and Fixtures.New")
	case src == nil:
		srv := httptest.NewServer(fx.Handler)
		t.Cleanup(srv.Close)
		client = Client(t, srv)
		src = fx.New(srv.URL, client)
	}
	checks := []struct {
		name string
		fn   func(context.Context, source.Source, Fixtures) error
	}{
		{"Kind", checkKind},
		{"ValidateAccepts", checkValidateAccepts},
		{"ValidateRejects", checkValidateRejects},
		{"ValidateNoRequests", func(ctx context.Context, s source.Source, f Fixtures) error {
			return checkNoRequests(ctx, s, f, client)
		}},
		{"Canonical", func(ctx context.Context, s source.Source, f Fixtures) error {
			return checkCanonical(ctx, s, f, client)
		}},
		{"ListDeterministic", checkDeterministic},
		{"ListConcurrent", checkConcurrent},
		{"ListMatchesFixture", checkWantRaw},
		{"ListMissing", checkMissing},
		{"ListHonoursContext", checkContext},
		{"NotModified", checkNotModified},
		{"PublishedAt", checkPublishedAt},
		{"ChangeInvalidatesState", checkChange}, // last: mutates the upstream
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

func checkNoRequests(_ context.Context, src source.Source, fx Fixtures, c *http.Client) error {
	if c == nil {
		return nil // no owned server: cannot count
	}
	before := Requests(c)
	_ = src.Validate(fx.Valid)
	for _, bad := range fx.Invalid {
		_ = src.Validate(bad)
	}
	if n := Requests(c) - before; n != 0 {
		return fmt.Errorf("Validate sent %d HTTP request(s); it must do no I/O", n)
	}
	return nil
}

// checkCanonical checks a source.Canonicalizer: the canonical target of
// Valid and of each alias keeps the kind, passes Validate, is a fixed point
// (canonicalizing it again changes nothing), is the same for every alias and
// costs no request. A source without Canonicalizer passes unless the
// fixtures list Aliases.
func checkCanonical(_ context.Context, src source.Source, fx Fixtures, c *http.Client) error {
	cz, ok := src.(source.Canonicalizer)
	if !ok {
		if len(fx.Aliases) > 0 {
			return errors.New("fixtures list Aliases but the source does not implement source.Canonicalizer")
		}
		return nil
	}
	before := 0
	if c != nil {
		before = Requests(c)
	}
	want, err := canonicalOf(cz, src, fx.Valid)
	if err != nil {
		return err
	}
	var errs []error
	for _, alias := range fx.Aliases {
		if err := src.Validate(alias); err != nil {
			errs = append(errs, fmt.Errorf("Validate rejects alias %v: %w", alias, err))
			continue
		}
		got, err := canonicalOf(cz, src, alias)
		switch {
		case err != nil:
			errs = append(errs, err)
		case !reflect.DeepEqual(got, want):
			errs = append(errs, fmt.Errorf("Canonical(%v) = %v, want %v (the canonical target of Valid)", alias, got, want))
		}
	}
	if c != nil {
		if n := Requests(c) - before; n != 0 {
			errs = append(errs, fmt.Errorf("Canonical sent %d HTTP request(s); it must do no I/O", n))
		}
	}
	return errors.Join(errs...)
}

// canonicalOf returns cz.Canonical(t) after checking that it keeps the kind,
// passes Validate, is a fixed point and leaves t unchanged.
func canonicalOf(cz source.Canonicalizer, src source.Source, t source.Target) (source.Target, error) {
	arg := source.Target{Kind: t.Kind, Coordinates: maps.Clone(t.Coordinates)} // the fixture stays intact
	got, err := cz.Canonical(arg)
	if err != nil {
		return got, fmt.Errorf("Canonical(%v): %w", t, err)
	}
	if !reflect.DeepEqual(arg, t) {
		return got, fmt.Errorf("Canonical(%v) mutated its argument to %v", t, arg)
	}
	if got.Kind != t.Kind {
		return got, fmt.Errorf("Canonical(%v) changed the kind to %q", t, got.Kind)
	}
	if err := src.Validate(got); err != nil {
		return got, fmt.Errorf("Validate rejects Canonical(%v) = %v: %w", t, got, err)
	}
	again, err := cz.Canonical(got)
	if err != nil {
		return got, fmt.Errorf("Canonical(%v) (second pass): %w", got, err)
	}
	if !reflect.DeepEqual(again, got) {
		return got, fmt.Errorf("Canonical is not idempotent: %v, then %v", got, again)
	}
	return got, nil
}

func checkConcurrent(ctx context.Context, src source.Source, fx Fixtures) error {
	want, err := list(ctx, src, fx.Valid, source.FetchState{})
	if err != nil {
		return err
	}
	const workers = 8
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := list(ctx, src, fx.Valid, source.FetchState{})
			switch {
			case err != nil:
				errs[i] = err
			case !reflect.DeepEqual(got, want):
				errs[i] = fmt.Errorf("concurrent List differs:\ngot:  %+v\nwant: %+v", got, want)
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func checkMissing(ctx context.Context, src source.Source, fx Fixtures) error {
	if fx.Missing == nil {
		return nil
	}
	res, err := src.List(ctx, *fx.Missing, source.FetchState{})
	if err == nil {
		return fmt.Errorf("List of the missing target returned no error (%d versions)", len(res.Versions))
	}
	return nil
}

func checkChange(ctx context.Context, src source.Source, fx Fixtures) error {
	if !fx.Conditional || fx.Change == nil {
		return nil
	}
	before, err := list(ctx, src, fx.Valid, source.FetchState{})
	if err != nil {
		return err
	}
	fx.Change()
	after, err := list(ctx, src, fx.Valid, before.State)
	if err != nil {
		return err
	}
	if after.NotModified {
		return errors.New("List with the pre-change State reported NotModified after the upstream changed")
	}
	if len(after.Versions) == 0 {
		return errors.New("List after the upstream changed returned no versions")
	}
	return nil
}
