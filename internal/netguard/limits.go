package netguard

import (
	"errors"
	"fmt"
	"maps"
	"slices"
)

// Origins of an effective limit (ADR 0027 §8), the values of the origin
// label of the host limit gauges.
const (
	// OriginBuiltin is a limit of DefaultHostLimits or DefaultFallbackLimit.
	OriginBuiltin = "builtin"
	// OriginConfig is a limit set for one host (Config.HostLimits,
	// http.host_limits.hosts).
	OriginConfig = "config"
	// OriginDefault is a fallback set by the operator (Config.FallbackLimit,
	// http.host_limits.default).
	OriginDefault = "default"
)

// EffectiveLimit is one line of the limits table a Client applies: the rate
// limits of a host after the cap on built-in budgets, with the origin of
// each bucket's limit.
type EffectiveLimit struct {
	// Host is a lower-case host name of the table, or OtherHost for the
	// fallback that applies to every unlisted host.
	Host                string
	Authenticated       Limit
	Anonymous           Limit
	AuthenticatedOrigin string
	AnonymousOrigin     string
	// Fallback marks the last line, the limit of every unlisted host. Use
	// it rather than Host: a configured host may be named "other" too.
	Fallback bool
}

// IsBuiltinBudgetHost reports whether host has a built-in limit in
// DefaultHostLimits: a published budget that a configured limit can only
// lower (ADR 0027 §4).
func IsBuiltinBudgetHost(host string) bool {
	_, ok := DefaultHostLimits()[normHost(host)]
	return ok
}

// capLimit returns the lower of l and builtin, rate by rate and burst by
// burst, and whether the result is the built-in limit.
func capLimit(l, builtin Limit) (Limit, bool) {
	out := Limit{Rate: min(l.Rate, builtin.Rate), Burst: min(l.Burst, builtin.Burst)}
	return out, out == builtin
}

// EffectiveLimits returns the limits table that New builds from hosts
// (Config.HostLimits) and fallback (Config.FallbackLimit, zero for
// DefaultFallbackLimit), sorted by host, with the fallback last as
// OtherHost. A host of DefaultHostLimits keeps its built-in limit unless the
// entry is lower: an entry may lower a built-in budget, never raise it
// (ADR 0027 §4). The entry's MaxBodyBytes and Timeout are not part of the
// table. It fails on the limits New refuses.
func EffectiveLimits(hosts map[string]HostLimit, fallback HostLimit) ([]EffectiveLimit, error) {
	table, _, _, err := resolveLimits(hosts, fallback)
	return table, err
}

// resolveLimits validates hosts and fallback and returns the table, the
// limits keyed by normalised host for the limiter, and the fallback to use.
func resolveLimits(hosts map[string]HostLimit, fallback HostLimit) ([]EffectiveLimit, map[string]HostLimit, HostLimit, error) {
	fallbackOrigin := OriginDefault
	if fallback == (HostLimit{}) {
		fallback, fallbackOrigin = DefaultFallbackLimit(), OriginBuiltin
	}
	if fallback.MaxBodyBytes != 0 {
		return nil, nil, HostLimit{}, errors.New("netguard: fallback limit: MaxBodyBytes must be zero (use Config.MaxBodyBytes)")
	}
	if err := fallback.validate(); err != nil {
		return nil, nil, HostLimit{}, fmt.Errorf("netguard: fallback limit: %w", err)
	}

	builtins := DefaultHostLimits()
	limits := DefaultHostLimits()
	byHost := map[string]EffectiveLimit{}
	for h, l := range builtins {
		byHost[h] = EffectiveLimit{
			Host: h, Authenticated: l.Authenticated, Anonymous: l.Anonymous,
			AuthenticatedOrigin: OriginBuiltin, AnonymousOrigin: OriginBuiltin,
		}
	}
	for _, h := range slices.Sorted(maps.Keys(hosts)) {
		l := hosts[h]
		if err := l.validate(); err != nil {
			return nil, nil, HostLimit{}, fmt.Errorf("netguard: limit for %s: %w", h, err)
		}
		h = normHost(h)
		e := EffectiveLimit{
			Host: h, Authenticated: l.Authenticated, Anonymous: l.Anonymous,
			AuthenticatedOrigin: OriginConfig, AnonymousOrigin: OriginConfig,
		}
		if b, ok := builtins[h]; ok {
			var isBuiltin bool
			if l.Authenticated, isBuiltin = capLimit(l.Authenticated, b.Authenticated); isBuiltin {
				e.AuthenticatedOrigin = OriginBuiltin
			}
			if l.Anonymous, isBuiltin = capLimit(l.Anonymous, b.Anonymous); isBuiltin {
				e.AnonymousOrigin = OriginBuiltin
			}
			e.Authenticated, e.Anonymous = l.Authenticated, l.Anonymous
		}
		limits[h] = l
		byHost[h] = e
	}

	table := make([]EffectiveLimit, 0, len(byHost)+1)
	for _, h := range slices.Sorted(maps.Keys(byHost)) {
		table = append(table, byHost[h])
	}
	table = append(table, EffectiveLimit{
		Host: OtherHost, Authenticated: fallback.Authenticated, Anonymous: fallback.Anonymous,
		AuthenticatedOrigin: fallbackOrigin, AnonymousOrigin: fallbackOrigin, Fallback: true,
	})
	return table, limits, fallback, nil
}

// Limits returns the limits table c applies (see EffectiveLimits). The
// caller may modify the result.
func (c *Client) Limits() []EffectiveLimit { return slices.Clone(c.table) }
