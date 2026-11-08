package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/angelia-dev/angelia/internal/config"
	"github.com/angelia-dev/angelia/internal/netguard"
)

// builtinHostLimitsListing is what config check prints without
// http.host_limits.
const builtinHostLimitsListing = "http host limits (per process):\n" +
	"  api.github.com           1.39 rps, burst 10 with a token; 0.0164 rps, burst 1 without (built-in)\n" +
	"  crates.io                1 rps, burst 1 (built-in)\n" +
	"  gitlab.com               33.2 rps, burst 10 with a token; 8.17 rps, burst 10 without (built-in)\n" +
	"  other hosts              5 rps, burst 5 (built-in default)\n"

const hostLimitsCfg = `http:
  host_limits:
    default: {rps: 10}
    hosts:
      127.0.0.1: {rps: 50, burst: 50}
      api.github.com: {rps: 3, burst: 5}
      crates.io: {rps: 0.5}
      pypi.org: {rps: 30}
      nexus.example: {rps: 1}
sources:
  npm:
    base_url: http://127.0.0.1:4873
`

// ADR 0027 §6: config check prints the effective table with origins and
// the four kinds of warning.
func TestConfigCheckHostLimits(t *testing.T) {
	t.Setenv("ANGELIA_PLUGINS_DIR", filepath.Join(t.TempDir(), "absent"))
	out, errb, err := runCheck(t, "--config", writeCfg(t, hostLimitsCfg))
	if err != nil {
		t.Fatalf("err = %v, stderr %q", err, errb)
	}
	want := "configuration OK\n" +
		"http host limits (per process):\n" +
		"  127.0.0.1                50 rps, burst 50 (config)\n" +
		"  api.github.com           1.39 rps, burst 5 with a token; 0.0164 rps, burst 1 without (config, capped at built-in)\n" +
		"  crates.io                0.5 rps, burst 1 (config)\n" +
		"  gitlab.com               33.2 rps, burst 10 with a token; 8.17 rps, burst 10 without (built-in)\n" +
		"  nexus.example            1 rps, burst 1 (config)\n" +
		"  pypi.org                 30 rps, burst 30 (config)\n" +
		"  other hosts              10 rps, burst 10 (config default)\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("out =\n%s\nwant it to start with\n%s", out, want)
	}
	for _, w := range []string{
		"warning: http.host_limits.hosts.api.github.com: rps 3 is above the built-in 1.39 (5,000/h with a token); capped at the built-in limit\n",
		"warning: http.host_limits.hosts.pypi.org: rps 30 on a public registry; read its usage policy, over 20 rps can get this instance blocked\n",
		"warning: http.host_limits.hosts.nexus.example: no configured source uses this host\n",
		"warning: http.host_limits.default: rps 10 is above the built-in 5 and applies to every unlisted host, webhook hosts included\n",
	} {
		if !strings.Contains(errb, w) {
			t.Errorf("stderr lacks %q:\n%s", w, errb)
		}
	}
	for _, quiet := range []string{"hosts.127.0.0.1", "hosts.crates.io"} {
		if strings.Contains(errb, quiet) {
			t.Errorf("unexpected warning on %s:\n%s", quiet, errb)
		}
	}
}

// A plugin's allowed hosts count as used: no typo warning.
func TestConfigCheckHostLimitsPluginHostIsUsed(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "cargo-registry", `{"kind":"cargo-registry","allowed_hosts":["crates.io"]}`, "allowed_hosts: [crates-mirror.example.com]\n")
	t.Setenv("ANGELIA_PLUGINS_DIR", dir)
	p := writeCfg(t, "http:\n  host_limits:\n    hosts:\n      crates-mirror.example.com: {rps: 2}\n")
	_, errb, err := runCheck(t, "--config", p)
	if err != nil || strings.Contains(errb, "no configured source uses") {
		t.Fatalf("err = %v, stderr %q", err, errb)
	}
}

// ADR 0027 §6: out-of-range values, malformed hosts and duplicates fail
// config check with the key named.
func TestConfigCheckHostLimitsErrors(t *testing.T) {
	for cfg, want := range map[string]string{
		"    hosts: {pypi.org: {rps: 101}}":                        "http.host_limits.hosts.pypi.org.rps: must be a number from 1/3600",
		"    hosts: {pypi.org: {rps: 1, burst: 500}}":              "http.host_limits.hosts.pypi.org.burst: must be an integer from 1 to 200",
		"    default: {rps: 21}":                                   "http.host_limits.default.rps",
		"    hosts: {\"*.example.com\": {rps: 1}}":                 "wildcards are not supported",
		"    hosts: {\"pypi.org:443\": {rps: 1}}":                  "such as pypi.org",
		"    hosts: {pypi.org: {rps: 1}, pypi.org.: {rps: 2}}":     "listed more than once",
		"    hosts: {pypi.org: {rps: 1, authenticated: {rps: 2}}}": `unknown key "authenticated"`,
	} {
		_, errb, err := runCheck(t, "--config", writeCfg(t, "http:\n  host_limits:\n"+cfg+"\n"))
		if err == nil || !strings.Contains(errb, "invalid configuration") || !strings.Contains(errb, want) {
			t.Errorf("%s: err = %v, stderr %q, want %q", cfg, err, errb, want)
		}
	}
}

// ADR 0027 §1: serve passes the limits to the one netguard client, with the
// built-in budgets capped.
func TestNewNetClientUsesHostLimits(t *testing.T) {
	cfg, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, hostLimitsCfg)})
	if err != nil {
		t.Fatal(err)
	}
	c, err := newNetClient(cfg, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]netguard.EffectiveLimit{}
	for _, e := range c.Limits() {
		got[e.Host] = e
	}
	if e := got["127.0.0.1"]; e.Anonymous != (netguard.Limit{Rate: 50, Burst: 50}) || e.Authenticated != e.Anonymous {
		t.Errorf("127.0.0.1 = %+v", e)
	}
	if e := got["api.github.com"]; e.Authenticated.Rate != netguard.DefaultHostLimits()["api.github.com"].Authenticated.Rate || e.Authenticated.Burst != 5 {
		t.Errorf("api.github.com = %+v, want the built-in rate and burst 5", e)
	}
	if e := got[netguard.OtherHost]; e.Anonymous != (netguard.Limit{Rate: 10, Burst: 10}) || e.AnonymousOrigin != netguard.OriginDefault {
		t.Errorf("other = %+v", e)
	}

	// Without the key the client has exactly the built-in table.
	def, err := config.Load(t.Context(), config.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c, err = newNetClient(def, slog.New(slog.DiscardHandler), nil)
	if err != nil {
		t.Fatal(err)
	}
	builtin, _ := netguard.EffectiveLimits(nil, netguard.HostLimit{})
	if !slices.Equal(c.Limits(), builtin) {
		t.Errorf("Limits() = %+v, want the built-in table %+v", c.Limits(), builtin)
	}
}

// ADR 0027 §8: serve exposes the effective limits as gauges.
func TestServeMetricsHostLimitGauges(t *testing.T) {
	db := "sqlite://" + filepath.ToSlash(filepath.Join(t.TempDir(), "a.db"))
	code, body := serveAndGetCfg(t, "database: {dsn: \""+db+"\"}\nhttp:\n  host_limits:\n    hosts:\n      127.0.0.1: {rps: 50, burst: 50}\n",
		"/metrics", "--roles=scheduler")
	if code != 200 {
		t.Fatalf("GET /metrics = %d", code)
	}
	for _, want := range []string{
		`angelia_netguard_host_limit_rate{bucket="anonymous",host="127.0.0.1",origin="config"} 50`,
		`angelia_netguard_host_limit_burst{bucket="authenticated",host="127.0.0.1",origin="config"} 50`,
		`angelia_netguard_host_limit_rate{bucket="anonymous",host="other",origin="builtin"} 5`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}

// ADR 0027 §5: the watch file cannot set host limits; the key is unknown
// at the top level and in an object.
func TestWatchFileRefusesHostLimits(t *testing.T) {
	for _, wf := range []string{
		"version: 1\nhttp:\n  host_limits:\n    hosts: {pypi.org: {rps: 100}}\n",
		"version: 1\nhost_limits:\n  hosts: {pypi.org: {rps: 100}}\n",
		"version: 1\nwatches:\n  - {name: r, source: pypi, coordinates: {name: requests}, host_limits: {rps: 100}}\n",
	} {
		p := filepath.Join(t.TempDir(), "watches.yaml")
		if err := os.WriteFile(p, []byte(wf), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ANGELIA_CONFIG", "")
		_, errb, err := runCheck(t, "--watches", p)
		if err == nil || !strings.Contains(errb, "unknown key") || !strings.Contains(errb, "host_limits") && !strings.Contains(errb, `"http"`) {
			t.Errorf("%q: err = %v, stderr %q", wf, err, errb)
		}
	}
}

// ADR 0027 §5: a plugin's grants.yaml cannot set host limits.
func TestGrantsRefuseHostLimits(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "cargo-registry", `{"kind":"cargo-registry","allowed_hosts":["crates.io"]}`, "host_limits:\n  hosts: {crates.io: {rps: 100}}\n")
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_PLUGINS_DIR", dir)
	_, errb, err := runCheck(t)
	if err == nil || !strings.Contains(errb, "invalid plugins") || !strings.Contains(errb, `unknown key "host_limits"`) {
		t.Fatalf("err = %v, stderr %q", err, errb)
	}
}

func TestBuiltinSourceHosts(t *testing.T) {
	want := []string{"api.github.com", "crates.io", "gitlab.com", "index.crates.io", "pypi.org", "registry.npmjs.org"}
	if got := builtinSourceHosts(); !slices.Equal(got, want) {
		t.Errorf("builtinSourceHosts() = %v, want %v", got, want)
	}
}

// ADR 0027 §6: serve logs the same warnings at start; plugin hosts count as
// used there too.
func TestServeHostLimitWarnings(t *testing.T) {
	dir := t.TempDir()
	writePlugin(t, dir, "cargo-registry", `{"kind":"cargo-registry","allowed_hosts":["crates-mirror.example.com"]}`, "")
	cfg, err := config.Load(t.Context(), config.Options{
		Path:    writeCfg(t, "http:\n  host_limits:\n    hosts:\n      crates-mirror.example.com: {rps: 2}\n      typo.example: {rps: 2}\n"),
		Environ: []string{"ANGELIA_PLUGINS_DIR=" + dir},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := serveHostLimitWarnings(t.Context(), cfg, nil)
	if want := []string{"http.host_limits.hosts.typo.example: no configured source uses this host"}; !slices.Equal(got, want) {
		t.Errorf("warnings = %v, want %v", got, want)
	}
}

// The documented example of docs/configuration.md is valid, and the
// documented listing is what config check prints for 127.0.0.1 at 50 rps.
func TestHostLimitsDocumentedExamples(t *testing.T) {
	example := docBlock(t, "### Per-host rate limits", "```yaml")
	if _, err := config.Load(t.Context(), config.Options{Path: writeCfg(t, example)}); err != nil {
		t.Errorf("documented example: %v", err)
	}
	want := docBlock(t, "### Per-host rate limits", "```text")
	t.Setenv("ANGELIA_CONFIG", "")
	t.Setenv("ANGELIA_PLUGINS_DIR", filepath.Join(t.TempDir(), "absent"))
	out, errb, err := runCheck(t, "--config", writeCfg(t, "http:\n  host_limits:\n    hosts:\n      127.0.0.1: {rps: 50, burst: 50}\n"))
	if err != nil {
		t.Fatalf("err = %v, stderr %q", err, errb)
	}
	if !strings.Contains(out, want) {
		t.Errorf("out =\n%s\ndocumented\n%s", out, want)
	}
}
