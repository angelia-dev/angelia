package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/angelia-dev/angelia/internal/version"
	"github.com/angelia-dev/angelia/internal/version/schemes"
)

// Limits on a policy, as in docs/watch-file.md §2.3.
const (
	// MaxPatterns bounds include and exclude, each.
	MaxPatterns = 16
	// MaxCooldown bounds cooldown (one year).
	MaxCooldown = 8760 * time.Hour
)

// ErrInvalid is wrapped by every policy validation error.
var ErrInvalid = errors.New("invalid policy")

// Prerelease is the pre-release mode of a watch.
type Prerelease string

// Pre-release modes.
const (
	// PrereleaseNone drops pre-releases (the default).
	PrereleaseNone Prerelease = "none"
	// PrereleaseOnly drops final releases.
	PrereleaseOnly Prerelease = "only"
	// PrereleaseInclude keeps both.
	PrereleaseInclude Prerelease = "include"
)

// bumps are the values the bump filter accepts.
var bumps = []version.Bump{version.BumpMajor, version.BumpMinor, version.BumpPatch}

// Policy is a watch's policy as stored in watch.policy_json. Its JSON keys
// are the YAML keys of a watch file's `policy` block (docs/watch-file.md
// §2.3), plus the watch's `scheme` override, which the watch file stores in
// the same column. A key that is absent takes its default.
type Policy struct {
	// Prerelease is none (default), only or include.
	Prerelease Prerelease `json:"prerelease"`
	// Include keeps only raw versions matching at least one RE2 pattern
	// (unanchored). Empty keeps all.
	Include []string `json:"include,omitempty"`
	// Exclude drops raw versions matching any RE2 pattern. Exclude wins
	// over include.
	Exclude []string `json:"exclude,omitempty"`
	// Constraint is a range such as ">=2 <3", compared with the watch's
	// scheme (see ParseConstraint). Empty matches any version.
	Constraint string `json:"constraint,omitempty"`
	// Bump keeps only versions whose bump is listed (major, minor, patch).
	// Empty keeps all.
	Bump []version.Bump `json:"bump,omitempty"`
	// Cooldown holds a version until it is older than this. Zero emits at
	// once.
	Cooldown Duration `json:"cooldown"`
	// Baseline records the versions known at a watch's first check without
	// events. Default true. The diff engine applies the baseline; false is
	// accepted but not yet honoured (see docs/policy.md).
	Baseline bool `json:"baseline"`
	// Scheme overrides the source kind's version scheme ("" for the
	// default); one of schemes.Names().
	Scheme string `json:"scheme,omitempty"`
}

// Default returns the policy of a watch without a policy block.
func Default() Policy { return Policy{Prerelease: PrereleaseNone, Baseline: true} }

// Decode parses and validates a stored policy. Empty input, "null" and
// "{}" give Default(). Unknown keys are errors, as in the watch file.
func Decode(raw json.RawMessage) (Policy, error) {
	p := Default()
	if t := bytes.TrimSpace(raw); len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return p, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if dec.More() {
		return Policy{}, fmt.Errorf("%w: trailing data after the policy object", ErrInvalid)
	}
	if err := p.Validate(); err != nil {
		return Policy{}, err
	}
	return p, nil
}

// Validate checks every key that can be checked without a version scheme:
// enums, regex compilation, bump values, cooldown bounds and the syntax of
// the constraint. Operands of the constraint are checked by Compile.
func (p Policy) Validate() error {
	var errs []error
	bad := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...))
	}
	switch p.Prerelease {
	case PrereleaseNone, PrereleaseOnly, PrereleaseInclude:
	default:
		bad("prerelease: %q is not one of none, only, include", version.Truncate(string(p.Prerelease), 32))
	}
	for _, list := range []struct {
		key  string
		pats []string
	}{{"include", p.Include}, {"exclude", p.Exclude}} {
		if len(list.pats) > MaxPatterns {
			bad("%s: %d patterns, at most %d", list.key, len(list.pats), MaxPatterns)
		}
		for i, pat := range list.pats {
			if _, err := regexp.Compile(pat); err != nil {
				bad("%s[%d]: %v", list.key, i, err)
			}
		}
	}
	for i, b := range p.Bump {
		if !slices.Contains(bumps, b) {
			bad("bump[%d]: %q is not one of major, minor, patch", i, version.Truncate(string(b), 32))
		}
	}
	if d := time.Duration(p.Cooldown); d < 0 || d > MaxCooldown {
		bad("cooldown: %s is outside 0 to %s", d, MaxCooldown)
	}
	if _, err := ParseConstraint(p.Constraint); err != nil {
		errs = append(errs, fmt.Errorf("constraint: %w", err))
	}
	if p.Scheme != "" && !slices.Contains(schemes.Names(), p.Scheme) {
		bad("scheme: %q is not one of %v", version.Truncate(p.Scheme, 32), schemes.Names())
	}
	return errors.Join(errs...)
}

// Duration is a time.Duration written as a Go duration string ("72h").
type Duration time.Duration

// MarshalJSON writes d as a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

// UnmarshalJSON reads a Go duration string such as "72h" or "90m".
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("cooldown: want a duration string such as \"72h\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("cooldown: %w", err)
	}
	*d = Duration(v)
	return nil
}
