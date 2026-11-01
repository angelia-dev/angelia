package policy

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/angelia-dev/angelia/internal/version"
)

func TestDecodeDefaults(t *testing.T) {
	for _, in := range []string{"", "  ", "null", "{}"} {
		p, err := Decode(json.RawMessage(in))
		if err != nil {
			t.Fatalf("Decode(%q): %v", in, err)
		}
		if !reflect.DeepEqual(p, Default()) {
			t.Errorf("Decode(%q) = %+v, want %+v", in, p, Default())
		}
	}
	if d := Default(); d.Prerelease != PrereleaseNone || !d.Baseline || d.Cooldown != 0 {
		t.Errorf("Default() = %+v", d)
	}
}

// TestDecodeWatchFileKeys decodes a policy_json as the watch file (#48)
// stores it: the YAML keys of `policy` plus the watch's `scheme`.
func TestDecodeWatchFileKeys(t *testing.T) {
	in := `{"prerelease":"include","include":["^v"],"exclude":["-canary","-experimental"],` +
		`"constraint":">=18 <20","bump":["major","minor"],"cooldown":"72h","baseline":false,"scheme":"semver"}`
	p, err := Decode(json.RawMessage(in))
	if err != nil {
		t.Fatal(err)
	}
	want := Policy{
		Prerelease: PrereleaseInclude, Include: []string{"^v"}, Exclude: []string{"-canary", "-experimental"},
		Constraint: ">=18 <20", Bump: []version.Bump{version.BumpMajor, version.BumpMinor},
		Cooldown: Duration(72 * time.Hour), Baseline: false, Scheme: "semver",
	}
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("Decode = %+v\nwant     %+v", p, want)
	}
	// Round trip.
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := Decode(b)
	if err != nil || !reflect.DeepEqual(p2, want) {
		t.Fatalf("round trip %s = %+v, %v", b, p2, err)
	}
	if !strings.Contains(string(b), `"cooldown":"72h0m0s"`) {
		t.Errorf("cooldown encoded as %s", b)
	}
}

func TestDecodeInvalid(t *testing.T) {
	many := `["a"` + strings.Repeat(`,"a"`, MaxPatterns) + `]`
	for name, in := range map[string]string{
		"unknown key":         `{"prereleases":"none"}`,
		"interval not here":   `{"interval":"1h"}`,
		"prerelease enum":     `{"prerelease":"all"}`,
		"prerelease empty":    `{"prerelease":""}`,
		"include regex":       `{"include":["("]}`,
		"exclude regex":       `{"exclude":["[a-"]}`,
		"too many includes":   `{"include":` + many + `}`,
		"too many excludes":   `{"exclude":` + many + `}`,
		"bump enum":           `{"bump":["prerelease"]}`,
		"bump other":          `{"bump":["major","other"]}`,
		"cooldown syntax":     `{"cooldown":"3d"}`,
		"cooldown number":     `{"cooldown":3600}`,
		"cooldown negative":   `{"cooldown":"-1h"}`,
		"cooldown too long":   `{"cooldown":"8761h"}`,
		"constraint syntax":   `{"constraint":">="}`,
		"constraint newline":  `{"constraint":">=1\n<2"}`,
		"scheme":              `{"scheme":"maven"}`,
		"baseline type":       `{"baseline":"yes"}`,
		"not an object":       `["none"]`,
		"trailing data":       `{} {}`,
		"trailing brace":      `{}}`,
		"trailing bracket":    `{"cooldown":"1h"}]`,
		"trailing garbage":    `{} x`,
		"cooldown fraction":   `{"cooldown":"1.5s"}`,
		"cooldown millis":     `{"cooldown":"500ms"}`,
		"include wrong type":  `{"include":"^v"}`,
		"constraint too long": `{"constraint":"` + strings.Repeat("1", MaxConstraintLen+1) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(json.RawMessage(in)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Decode(%s) = %v, want ErrInvalid", in, err)
			}
		})
	}
}

func TestDecodeWholeSeconds(t *testing.T) {
	for in, want := range map[string]time.Duration{`"90s"`: 90 * time.Second, `"1.5h"`: 90 * time.Minute, `"0s"`: 0} {
		p, err := Decode(json.RawMessage(`{"cooldown":` + in + `}  `))
		if err != nil || time.Duration(p.Cooldown) != want {
			t.Errorf("Decode cooldown %s = %v, %v; want %v", in, time.Duration(p.Cooldown), err, want)
		}
	}
}

func TestDecodeLimits(t *testing.T) {
	pats := `["a"` + strings.Repeat(`,"a"`, MaxPatterns-1) + `]`
	in := `{"include":` + pats + `,"exclude":` + pats + `,"cooldown":"8760h","bump":["patch","minor","major"]}`
	if _, err := Decode(json.RawMessage(in)); err != nil {
		t.Fatalf("limits rejected: %v", err)
	}
}

func TestValidateReportsEveryError(t *testing.T) {
	err := Policy{Prerelease: "x", Bump: []version.Bump{"y"}, Cooldown: Duration(-1)}.Validate()
	for _, want := range []string{"prerelease", "bump[0]", "cooldown"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Validate error %v does not mention %s", err, want)
		}
	}
}
