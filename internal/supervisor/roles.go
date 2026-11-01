package supervisor

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

// Role is one long-running part of the process. Run blocks until ctx is
// cancelled (then returns nil or a context error after finishing or releasing
// in-flight work) or until it fails (then returns the error).
type Role interface {
	Name() string
	Run(ctx context.Context) error
}

var allRoles = []string{"api", "scheduler", "worker", "dispatcher"}

// RoleNames returns every role name in start order.
func RoleNames() []string { return slices.Clone(allRoles) }

// Factory builds the Role for a name. Real roles replace the stubs as their
// stories land.
type Factory func(name string) Role

// Select expands names ("all" means every role) into roles built by f,
// deduplicated and in canonical order. Unknown names are an error.
func Select(names []string, f Factory) ([]Role, error) {
	if len(names) > 1 && slices.Contains(names, "all") {
		return nil, errors.New(`"all" cannot be combined with other roles`)
	}
	want := map[string]bool{}
	for _, n := range names {
		if n == "all" {
			for _, r := range allRoles {
				want[r] = true
			}
			continue
		}
		if !slices.Contains(allRoles, n) {
			return nil, fmt.Errorf("unknown role %q (want api, scheduler, worker, dispatcher or all)", n)
		}
		want[n] = true
	}
	if len(want) == 0 {
		return nil, errors.New("no roles selected")
	}
	var out []Role
	for _, n := range allRoles {
		if want[n] {
			out = append(out, f(n))
		}
	}
	return out, nil
}

type stub struct{ name string }

// NewStub returns a placeholder role that blocks until its context is
// cancelled. It stands in for roles whose story is not implemented yet.
func NewStub(name string) Role { return stub{name: name} }

func (s stub) Name() string { return s.name }

func (s stub) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}
