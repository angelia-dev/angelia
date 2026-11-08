package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/netguard"
)

func loadLimits(t *testing.T, yaml string) (*Config, error) {
	t.Helper()
	return load(t, writeFile(t, "http:\n  host_limits:\n"+yaml))
}

// ADR 0027 §1: the documented shape parses; burst defaults to ceil(rps),
// and to at least 1.
func TestHostLimitsParse(t *testing.T) {
	cfg, err := loadLimits(t, `
    default: {rps: 4, burst: 6}
    hosts:
      nexus.internal.example: {rps: 40, burst: 40}
      pypi.org: {rps: 10, burst: 20}
      registry.npmjs.org: {rps: 10}
      tiny.example: {rps: 0.25}
      odd.example: {rps: 2.5}
      api.github.com: {rps: 0.5, burst: 5}
      "::1": {rps: 1}
      mirror.example.: {rps: 3}
`)
	if err != nil {
		t.Fatal(err)
	}
	hosts, fallback := cfg.HTTP.HostLimits.Netguard()
	want := map[string]netguard.Limit{
		"nexus.internal.example": {Rate: 40, Burst: 40},
		"pypi.org":               {Rate: 10, Burst: 20},
		"registry.npmjs.org":     {Rate: 10, Burst: 10},
		"tiny.example":           {Rate: 0.25, Burst: 1},
		"odd.example":            {Rate: 2.5, Burst: 3},
		"api.github.com":         {Rate: 0.5, Burst: 5},
		"::1":                    {Rate: 1, Burst: 1},
		"mirror.example":         {Rate: 3, Burst: 3},
	}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %+v", hosts)
	}
	for h, l := range want {
		// One limit per entry, for both buckets (ADR 0027 §1, question 5).
		if got := hosts[h]; got.Authenticated != l || got.Anonymous != l || got.MaxBodyBytes != 0 || got.Timeout != 0 {
			t.Errorf("%s = %+v, want %+v for both buckets", h, got, l)
		}
	}
	if fallback.Anonymous != (netguard.Limit{Rate: 4, Burst: 6}) || fallback.Authenticated != fallback.Anonymous {
		t.Errorf("fallback = %+v", fallback)
	}
	if _, err := netguard.EffectiveLimits(hosts, fallback); err != nil {
		t.Errorf("netguard refuses a valid config: %v", err)
	}
}

// ADR 0027 §2: without the key nothing changes.
func TestHostLimitsAbsent(t *testing.T) {
	cfg, err := load(t, writeFile(t, "listen: :9\n"))
	if err != nil {
		t.Fatal(err)
	}
	hosts, fallback := cfg.HTTP.HostLimits.Netguard()
	if hosts != nil || fallback != (netguard.HostLimit{}) {
		t.Errorf("Netguard() = %v, %+v; want nil and the zero fallback (built-in)", hosts, fallback)
	}
	if w := cfg.HTTP.HostLimits.Warnings(nil, nil); len(w) != 0 {
		t.Errorf("warnings without the key: %v", w)
	}
}

// ADR 0027 §2: the bounds, inclusive, for named hosts and for default.
func TestHostLimitsBounds(t *testing.T) {
	for _, tc := range []struct {
		yaml string
		ok   bool
		want string
	}{
		{"    hosts: {a.example: {rps: 0.000277777777777778}}", true, ""}, // 1/3600
		{"    hosts: {a.example: {rps: 0.00027}}", false, "http.host_limits.hosts.a.example.rps: must be a number from 1/3600 (one request per hour) to 100, got 0.00027"},
		{"    hosts: {a.example: {rps: 100, burst: 200}}", true, ""},
		{"    hosts: {a.example: {rps: 100.5}}", false, "hosts.a.example.rps: must be a number from 1/3600 (one request per hour) to 100, got 100.5"},
		{"    hosts: {a.example: {rps: 1, burst: 201}}", false, "hosts.a.example.burst: must be an integer from 1 to 200, got 201"},
		{"    hosts: {a.example: {rps: 1, burst: 1}}", true, ""},
		{"    hosts: {a.example: {rps: 1, burst: 0}}", false, "hosts.a.example.burst: must be an integer from 1 to 200, got 0"},
		{"    hosts: {a.example: {rps: 1, burst: -3}}", false, "burst: must be an integer from 1 to 200, got -3"},
		{"    hosts: {a.example: {rps: 0}}", false, "hosts.a.example.rps: must be a number"},
		{"    hosts: {a.example: {rps: -1}}", false, "got -1"},
		{"    hosts: {a.example: {rps: .nan}}", false, "got NaN"},
		{"    hosts: {a.example: {rps: .inf}}", false, "got +Inf"},
		{"    hosts: {a.example: {burst: 5}}", false, "hosts.a.example.rps: required"},
		{"    hosts: {a.example: }", false, "hosts.a.example.rps: required"},
		{"    default: {rps: 20, burst: 20}", true, ""},
		{"    default: {rps: 0.000277777777777778, burst: 1}", true, ""},
		{"    default: {rps: 20.01}", false, "http.host_limits.default.rps: must be a number from 1/3600 (one request per hour) to 20, got 20.01"},
		{"    default: {rps: 5, burst: 21}", false, "http.host_limits.default.burst: must be an integer from 1 to 20, got 21"},
		{"    default: {rps: 0.0001}", false, "http.host_limits.default.rps"},
		{"    default: {burst: 2}", false, "http.host_limits.default.rps: required"},
		// Default burst ceil(rps) stays inside the bounds at the ceilings.
		{"    hosts: {a.example: {rps: 100}}\n    default: {rps: 20}", true, ""},
	} {
		_, err := loadLimits(t, tc.yaml+"\n")
		switch {
		case tc.ok && err != nil:
			t.Errorf("%q: unexpected error %v", tc.yaml, err)
		case !tc.ok && err == nil:
			t.Errorf("%q: want an error", tc.yaml)
		case !tc.ok && !strings.Contains(err.Error(), tc.want):
			t.Errorf("%q: error %q does not contain %q", tc.yaml, err, tc.want)
		}
	}
}

// ADR 0027 §2: values are numbers. A string rate, a fractional burst and
// unknown keys are errors, with the file and line.
func TestHostLimitsTypesAndUnknownKeys(t *testing.T) {
	for _, tc := range []struct{ yaml, want string }{
		{`    hosts: {a.example: {rps: "10/s"}}`, "line 3"},
		{`    hosts: {a.example: {rps: "10"}}`, "line 3"},
		{`    hosts: {a.example: {rps: 10, burst: 2.5}}`, "line 3"},
		{`    hosts: {a.example: {rps: 10, burst: "5"}}`, "line 3"},
		{`    hosts: {a.example: {rps: 10, per: 1h}}`, `unknown key "per"`},
		{`    hosts: {a.example: {rps: 10, authenticated: {rps: 1}}}`, `unknown key "authenticated"`},
		{`    default: {rps: 1, max_body_bytes: 10}`, `unknown key "max_body_bytes"`},
		{`    pypi.org: {rps: 10}`, `unknown key "pypi.org"`}, // hosts go under hosts
		{`    hosts: [pypi.org]`, "line 3"},
	} {
		_, err := loadLimits(t, tc.yaml+"\n")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want it to contain %q", tc.yaml, err, tc.want)
		}
	}
}

// ADR 0027 §3: exact host names; scheme, port, path, brackets, upper case
// and wildcards are refused with the expected spelling.
func TestHostLimitsHostSpelling(t *testing.T) {
	for _, h := range []string{"pypi.org", "pypi.org.", "127.0.0.1", "::1", "fd00::5", "::ffff:192.0.2.1", "default", "other", "xn--bcher-kva.example", "a-b.c"} {
		if _, err := loadLimits(t, "    hosts: {\""+h+"\": {rps: 1}}\n"); err != nil {
			t.Errorf("%q refused: %v", h, err)
		}
	}
	for h, want := range map[string]string{
		"https://pypi.org":     "without scheme, port, path or brackets, such as pypi.org",
		"pypi.org:443":         "such as pypi.org",
		"pypi.org/simple":      "such as pypi.org",
		"[::1]":                "such as ::1",
		"[::1]:8080":           "such as ::1",
		"PyPI.org":             "in lower case",
		"FD00::5":              "in lower case, such as fd00::5",
		"fe80::1%eth0":         "without a zone, such as fe80::1",
		"*.example.com":        "wildcards are not supported",
		".example.com":         "wildcards are not supported",
		"user@pypi.org":        "such as pypi.org",
		"under_score.example":  "must be a host name",
		"-bad.example":         "must be a host name",
		"http://127.0.0.1:500": "such as 127.0.0.1",
		"0:0::1":               "in its canonical form, such as ::1",
		"fd00:0:0::5":          "in its canonical form, such as fd00::5",
		".":                    "must be a host name",
	} {
		_, err := loadLimits(t, "    hosts: {\""+h+"\": {rps: 1}}\n")
		if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "http.host_limits.hosts") {
			t.Errorf("%q: err = %v, want it to contain %q", h, err, want)
		}
	}
}

// ADR 0027 §6: the same host twice after normalisation is an error.
func TestHostLimitsDuplicateAfterNormalisation(t *testing.T) {
	_, err := loadLimits(t, "    hosts:\n      pypi.org: {rps: 1}\n      pypi.org.: {rps: 2}\n")
	if err == nil || !strings.Contains(err.Error(), `pypi.org is listed more than once ("pypi.org", "pypi.org.")`) {
		t.Errorf("err = %v", err)
	}
	// Upper case is refused on its own and still counted as a duplicate.
	_, err = loadLimits(t, "    hosts:\n      PyPI.org: {rps: 1}\n      pypi.org.: {rps: 2}\n")
	if err == nil || !strings.Contains(err.Error(), "listed more than once") || !strings.Contains(err.Error(), "lower case") {
		t.Errorf("err = %v", err)
	}
}

// ADR 0027 §1 and §5: there is no environment override.
func TestHostLimitsNoEnvironmentOverride(t *testing.T) {
	cfg, err := load(t, "", "ANGELIA_HTTP_HOST_LIMITS={\"hosts\":{\"pypi.org\":{\"rps\":100}}}",
		"ANGELIA_HTTP_HOST_LIMITS_DEFAULT_RPS=20", "ANGELIA_HTTP_HOST_LIMITS_HOSTS=pypi.org")
	if err != nil {
		t.Fatal(err)
	}
	if hosts, fallback := cfg.HTTP.HostLimits.Netguard(); hosts != nil || fallback != (netguard.HostLimit{}) {
		t.Errorf("environment set host limits: %v %+v", hosts, fallback)
	}
}

// ADR 0027 §6: the four warnings.
func TestHostLimitsWarnings(t *testing.T) {
	public := []string{"pypi.org", "registry.npmjs.org", "index.crates.io", "crates.io", "api.github.com", "gitlab.com"}
	used := append(slices.Clone(public), "nexus.internal.example")
	cfg, err := loadLimits(t, `
    default: {rps: 10}
    hosts:
      api.github.com: {rps: 3, burst: 20}
      gitlab.com: {rps: 30}
      crates.io: {rps: 0.5}
      pypi.org: {rps: 30}
      registry.npmjs.org: {rps: 20}
      nexus.internal.example: {rps: 80}
      nexus.example: {rps: 1}
`)
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.HTTP.HostLimits.Warnings(public, used)
	want := []string{
		"http.host_limits.hosts.api.github.com: rps 3 is above the built-in 1.39 (5,000/h with a token); capped at the built-in limit",
		"http.host_limits.hosts.api.github.com: burst 20 is above the built-in 10 (5,000/h with a token); capped at the built-in limit",
		"http.host_limits.hosts.gitlab.com: burst 30 is above the built-in 10 (2,000/min with a token); capped at the built-in limit",
		"http.host_limits.hosts.gitlab.com: rps 30 on a public registry; read its usage policy, over 20 rps can get this instance blocked",
		"http.host_limits.hosts.nexus.example: no configured source uses this host",
		"http.host_limits.hosts.pypi.org: rps 30 on a public registry; read its usage policy, over 20 rps can get this instance blocked",
		"http.host_limits.default: rps 10 is above the built-in 5 and applies to every unlisted host, webhook hosts included",
	}
	if !slices.Equal(got, want) {
		t.Errorf("warnings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// Lowering a built-in budget, a default at or below 5 and a moderate rate
// on a public registry are not warned about.
func TestHostLimitsNoWarnings(t *testing.T) {
	cfg, err := loadLimits(t, `
    default: {rps: 5, burst: 5}
    hosts:
      api.github.com: {rps: 0.5, burst: 5}
      crates.io: {rps: 1, burst: 1}
      pypi.org: {rps: 20, burst: 40}
`)
	if err != nil {
		t.Fatal(err)
	}
	if w := cfg.HTTP.HostLimits.Warnings([]string{"pypi.org"}, []string{"pypi.org", "api.github.com", "crates.io"}); len(w) != 0 {
		t.Errorf("warnings: %v", w)
	}
}

// The warning on a built-in budget compares with the highest built-in
// bucket; that is the authenticated one for every built-in host.
func TestBuiltinBudgetsDescribed(t *testing.T) {
	for h, l := range netguard.DefaultHostLimits() {
		if builtinBudgets[h] == "" {
			t.Errorf("%s has no budget description", h)
		}
		if l.Authenticated.Rate < l.Anonymous.Rate || l.Authenticated.Burst < l.Anonymous.Burst {
			t.Errorf("%s: authenticated %+v is below anonymous %+v", h, l.Authenticated, l.Anonymous)
		}
	}
}

func TestFormatRate(t *testing.T) {
	for v, want := range map[float64]string{50: "50", 5: "5", 4990.0 / 3600: "1.39", 59.0 / 3600: "0.0164", 100: "100", 0.5: "0.5"} {
		if got := FormatRate(v); got != want {
			t.Errorf("FormatRate(%v) = %q, want %q", v, got, want)
		}
	}
}

// A host named "other" is accepted, with a warning: it shares the metrics
// label of every unlisted host.
func TestHostLimitsOtherHostWarns(t *testing.T) {
	cfg, err := loadLimits(t, "    hosts:\n      other: {rps: 2}\n")
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.HTTP.HostLimits.Warnings(nil, []string{"other"})
	want := []string{`http.host_limits.hosts.other: the netguard metrics report this host as host="other", the label of every unlisted host; its series cannot be told apart from theirs`}
	if !slices.Equal(got, want) {
		t.Errorf("warnings = %v, want %v", got, want)
	}
}
