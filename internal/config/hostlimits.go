package config

import (
	"fmt"
	"maps"
	"math"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/angelia-dev/angelia/internal/netguard"
)

// Bounds of http.host_limits (ADR 0027 §2).
const (
	MinHostRPS      = 1.0 / 3600 // one request per hour
	MaxHostRPS      = 100.0
	MaxHostBurst    = 200
	MaxDefaultRPS   = 20.0
	MaxDefaultBurst = 20
	// publicRegistryRPS is the rate above which a public registry entry
	// is warned about (ADR 0027 §6).
	publicRegistryRPS = 20.0
)

// HostLimits is http.host_limits: per-host rate limits of the netguard
// client (ADR 0027). Without it the built-in limits apply unchanged.
type HostLimits struct {
	// Default replaces the built-in fallback (5 rps, burst 5) for every
	// host not listed in Hosts nor in netguard.DefaultHostLimits.
	Default *RateLimit `yaml:"default"`
	// Hosts maps exact host names (lower case, no scheme, port, path,
	// brackets or wildcard; a trailing dot is ignored) to their limit. A
	// host with a built-in budget (netguard.DefaultHostLimits) can only be
	// lowered.
	Hosts map[string]RateLimit `yaml:"hosts"`
}

// RateLimit is one entry of http.host_limits. It applies to both the
// authenticated and the anonymous bucket of a host.
type RateLimit struct {
	// RPS is the refill rate in requests per second. Required.
	RPS *float64 `yaml:"rps"`
	// Burst is the bucket size. Default ceil(RPS), at least 1.
	Burst *BurstSize `yaml:"burst"`
}

// BurstSize is an integer that refuses a YAML float or string: yaml.v3
// would truncate burst: 2.5 to 2 (ADR 0027 §2).
type BurstSize int

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *BurstSize) UnmarshalYAML(n *yaml.Node) error {
	var v int
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" || n.Decode(&v) != nil {
		return fmt.Errorf("line %d: burst: must be an integer", n.Line)
	}
	*b = BurstSize(v)
	return nil
}

// Limit returns the token bucket of r, with the default burst filled in.
// Validate has checked r.
func (r RateLimit) Limit() netguard.Limit {
	var l netguard.Limit
	if r.RPS != nil {
		l.Rate = *r.RPS
	}
	if r.Burst != nil {
		l.Burst = int(*r.Burst)
	} else {
		l.Burst = max(1, int(math.Ceil(l.Rate)))
	}
	return l
}

// Netguard returns the limits for netguard.Config: HostLimits keyed by
// normalised host name, and FallbackLimit, zero when Default is not set
// (netguard then keeps its built-in fallback). netguard caps the hosts
// with a built-in budget.
func (h HostLimits) Netguard() (hosts map[string]netguard.HostLimit, fallback netguard.HostLimit) {
	if len(h.Hosts) > 0 {
		hosts = make(map[string]netguard.HostLimit, len(h.Hosts))
		for name, r := range h.Hosts {
			l := r.Limit()
			hosts[normLimitHost(name)] = netguard.HostLimit{Authenticated: l, Anonymous: l}
		}
	}
	if h.Default != nil {
		l := h.Default.Limit()
		fallback = netguard.HostLimit{Authenticated: l, Anonymous: l}
	}
	return hosts, fallback
}

// normLimitHost is netguard's key for a host: lower case, no trailing dot.
func normLimitHost(h string) string { return strings.TrimSuffix(strings.ToLower(h), ".") }

// validate returns one error per problem, hosts in sorted order.
func (h HostLimits) validate() []error {
	var errs []error
	if h.Default != nil {
		errs = append(errs, h.Default.validate("http.host_limits.default", MaxDefaultRPS, MaxDefaultBurst)...)
	}
	seen := map[string][]string{}
	for _, name := range slices.Sorted(maps.Keys(h.Hosts)) {
		if msg := hostLimitHostProblem(name); msg != "" {
			errs = append(errs, fmt.Errorf("http.host_limits.hosts %q: %s", name, msg))
		} else {
			errs = append(errs, h.Hosts[name].validate("http.host_limits.hosts."+name, MaxHostRPS, MaxHostBurst)...)
		}
		n := normLimitHost(name)
		seen[n] = append(seen[n], name)
	}
	for _, n := range slices.Sorted(maps.Keys(seen)) {
		if names := seen[n]; len(names) > 1 {
			quoted := make([]string, len(names))
			for i, v := range names {
				quoted[i] = strconv.Quote(v)
			}
			errs = append(errs, fmt.Errorf("http.host_limits.hosts: %s is listed more than once (%s); keep one entry", n, strings.Join(quoted, ", ")))
		}
	}
	return errs
}

func (r RateLimit) validate(key string, maxRPS float64, maxBurst int) []error {
	var errs []error
	switch {
	case r.RPS == nil:
		errs = append(errs, fmt.Errorf("%s.rps: required, a number of requests per second", key))
	case math.IsNaN(*r.RPS) || math.IsInf(*r.RPS, 0) || *r.RPS < MinHostRPS || *r.RPS > maxRPS:
		errs = append(errs, fmt.Errorf("%s.rps: must be a number from 1/3600 (one request per hour) to %s, got %s", key, formatNumber(maxRPS), formatNumber(*r.RPS)))
	}
	if r.Burst != nil && (*r.Burst < 1 || int(*r.Burst) > maxBurst) {
		errs = append(errs, fmt.Errorf("%s.burst: must be an integer from 1 to %d, got %d", key, maxBurst, *r.Burst))
	}
	return errs
}

// hostLimitHostProblem says what is wrong with a key of http.host_limits.hosts,
// naming the expected spelling, or returns "" for a host name, an IPv4
// address or an IPv6 address without brackets (as url.URL.Hostname reports
// it), lower case, optionally with a trailing dot (ADR 0027 §3).
func hostLimitHostProblem(h string) string {
	if h == "" || h == "." {
		return "must be a host name, an IPv4 address or an IPv6 address without brackets"
	}
	if strings.Contains(h, "*") || strings.HasPrefix(h, ".") {
		return "wildcards are not supported; list each host name exactly, such as pypi.org"
	}
	if a, err := netip.ParseAddr(h); err == nil && a.Is6() {
		switch {
		case a.Zone() != "":
			return "write the IPv6 address without a zone, such as " + a.WithZone("").String()
		case h != strings.ToLower(h):
			return "write the host in lower case, such as " + strings.ToLower(h)
		case a.String() != h:
			// Matching is by the exact spelling of url.URL.Hostname, so a
			// non-canonical spelling would rarely match (ADR 0027 §3).
			return "write the IPv6 address in its canonical form, such as " + a.String()
		}
		return ""
	}
	if strings.ContainsAny(h, ":/[]@?#") || h != strings.ToLower(h) {
		return "write the host name alone, in lower case, without scheme, port, path or brackets, such as " + suggestHost(h)
	}
	if !hostRE.MatchString(strings.TrimSuffix(h, ".")) {
		return "must be a host name, an IPv4 address or an IPv6 address without brackets"
	}
	return ""
}

// suggestHost guesses the intended host of a malformed entry, for the error.
func suggestHost(h string) string {
	s := strings.ToLower(h)
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if strings.HasPrefix(s, "[") {
		if end := strings.Index(s, "]"); end > 0 {
			return s[1:end]
		}
	}
	if host, _, ok := strings.Cut(s, ":"); ok && strings.Count(s, ":") == 1 {
		s = host
	}
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return "pypi.org"
	}
	return s
}

// builtinBudgets describes the published budget behind each host of
// netguard.DefaultHostLimits, for the warning on a value above it.
var builtinBudgets = map[string]string{
	"api.github.com": "5,000/h with a token",
	"gitlab.com":     "2,000/min with a token",
	"crates.io":      "its crawler policy, 1/s",
}

// Warnings returns the findings of ADR 0027 §6 on a valid h: values above a
// built-in budget (capped), a rate above 20 on a host of publicHosts (the
// hosts of the built-in sources' default base URLs), a default above the
// built-in fallback, hosts that are not in usedHosts (the hosts of the
// configured sources, the built-in sources' defaults and the plugins'
// allowed hosts), and a host named "other", which shares the metrics label
// of the unlisted hosts.
func (h HostLimits) Warnings(publicHosts, usedHosts []string) []string {
	var out []string
	builtins := netguard.DefaultHostLimits()
	for _, name := range slices.Sorted(maps.Keys(h.Hosts)) {
		key := "http.host_limits.hosts." + name
		host := normLimitHost(name)
		l := h.Hosts[name].Limit()
		if b, ok := builtins[host]; ok {
			// The entry applies to both buckets; the anonymous budget is
			// the lower one and is capped silently. The warning compares
			// with the highest built-in limit, the authenticated one.
			top := b.Authenticated
			if l.Rate > top.Rate {
				out = append(out, fmt.Sprintf("%s: rps %s is above the built-in %s (%s); capped at the built-in limit", key, formatNumber(l.Rate), formatRate(top.Rate), builtinBudgets[host]))
			}
			if l.Burst > top.Burst {
				out = append(out, fmt.Sprintf("%s: burst %d is above the built-in %d (%s); capped at the built-in limit", key, l.Burst, top.Burst, builtinBudgets[host]))
			}
		}
		if l.Rate > publicRegistryRPS && slices.Contains(publicHosts, host) {
			out = append(out, fmt.Sprintf("%s: rps %s on a public registry; read its usage policy, over %s rps can get this instance blocked", key, formatNumber(l.Rate), formatNumber(publicRegistryRPS)))
		}
		if !slices.Contains(usedHosts, host) {
			out = append(out, key+": no configured source uses this host")
		}
		if host == netguard.OtherHost {
			out = append(out, fmt.Sprintf("%s: the netguard metrics report this host as host=%q, the label of every unlisted host; its series cannot be told apart from theirs", key, netguard.OtherHost))
		}
	}
	if h.Default != nil {
		fb := netguard.DefaultFallbackLimit().Anonymous
		if l := h.Default.Limit(); l.Rate > fb.Rate {
			out = append(out, fmt.Sprintf("http.host_limits.default: rps %s is above the built-in %s and applies to every unlisted host, webhook hosts included", formatNumber(l.Rate), formatNumber(fb.Rate)))
		}
	}
	return out
}

// formatNumber prints a configured value as written.
func formatNumber(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// FormatRate prints an effective rate with three significant digits, as
// config check lists it ("1.39", "0.0164", "50").
func FormatRate(v float64) string { return formatRate(v) }

func formatRate(v float64) string { return strconv.FormatFloat(v, 'g', 3, 64) }
