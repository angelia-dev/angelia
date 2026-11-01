package subscription

import (
	"testing"
	"time"
)

var (
	start = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	hour  = time.Hour
)

func ev(typ string, pre bool, watchMode string, watchCooldown time.Duration) Event {
	return Event{Type: typ, Prerelease: pre, WatchPrerelease: watchMode, CooldownStart: start, WatchCooldown: watchCooldown}
}

// AC: prerelease narrows the watch policy, and cannot widen it.
func TestEvaluatePrerelease(t *testing.T) {
	const pub = "release.published"
	for _, c := range []struct {
		name       string
		sub, watch string
		pre        bool
		want       Verdict
	}{
		{"inherit keeps a final", "", "include", false, Admit},
		{"inherit keeps a pre-release", "", "include", true, Admit},
		{"none narrows include: pre-release dropped", "none", "include", true, Drop},
		{"none narrows include: final kept", "none", "include", false, Admit},
		{"only narrows include: final dropped", "only", "include", false, Drop},
		{"only narrows include: pre-release kept", "only", "include", true, Admit},
		{"include on a none watch does not widen", "include", "none", true, Drop},
		{"include on a none watch keeps finals", "include", "none", false, Admit},
		{"only on a none watch: nothing is delivered (final)", "only", "none", false, Drop},
		{"only on a none watch: nothing is delivered (pre-release)", "only", "none", true, Drop},
		{"none on an only watch: nothing is delivered", "none", "only", true, Drop},
		{"same mode narrows nothing", "none", "none", false, Admit},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := Policy{Prerelease: c.sub}.Evaluate(ev(pub, c.pre, c.watch, 0), start)
			if got.Verdict != c.want {
				t.Fatalf("verdict = %v, want %v", got.Verdict, c.want)
			}
		})
	}
}

// AC: cooldown narrows (later), measured from the same start as the watch's.
func TestEvaluateCooldown(t *testing.T) {
	const pub = "release.published"
	p := Policy{Cooldown: 72 * hour}

	d := p.Evaluate(ev(pub, false, "none", 0), start.Add(hour))
	if d.Verdict != Hold || !d.Until.Equal(start.Add(72*hour)) {
		t.Fatalf("held = %+v, want hold until start+72h", d)
	}
	// With a watch cooldown of 24h the hold is max, not the sum.
	d = p.Evaluate(ev(pub, false, "none", 24*hour), start.Add(24*hour))
	if d.Verdict != Hold || !d.Until.Equal(start.Add(72*hour)) {
		t.Fatalf("max(watch, sub) = %+v, want start+72h and not start+96h", d)
	}
	if d := p.Evaluate(ev(pub, false, "none", 0), start.Add(72*hour)); d.Verdict != Admit {
		t.Fatalf("at the end of the hold = %+v, want admit", d)
	}
	// A subscription cooldown below the watch's cannot shorten it (clamped).
	if d := (Policy{Cooldown: hour}).Evaluate(ev(pub, false, "none", 24*hour), start.Add(24*hour)); d.Verdict != Admit {
		t.Fatalf("shorter than the watch's = %+v, want admit", d)
	}
	if d := (Policy{Cooldown: -hour}).Evaluate(ev(pub, false, "none", 0), start); d.Verdict != Admit {
		t.Fatalf("negative cooldown = %+v, want admit", d)
	}
	// Above the maximum it is cut, not rejected.
	d = (Policy{Cooldown: 10000 * hour}).Evaluate(ev(pub, false, "none", 0), start)
	if d.Verdict != Hold || !d.Until.Equal(start.Add(MaxCooldown)) {
		t.Fatalf("over the maximum = %+v", d)
	}
	// Unknown start fails closed: counted from now.
	e := ev(pub, false, "none", 0)
	e.CooldownStart = time.Time{}
	if d := p.Evaluate(e, start); d.Verdict != Hold || !d.Until.Equal(start.Add(72*hour)) {
		t.Fatalf("unknown start = %+v, want hold for the full cooldown", d)
	}
	// A pre-release that the subscription drops is dropped, not held.
	if d := (Policy{Prerelease: "none", Cooldown: 72 * hour}).Evaluate(ev(pub, true, "include", 0), start); d.Verdict != Drop {
		t.Fatalf("drop wins over hold: %+v", d)
	}
}

// AC: yanked events ignore policy.*.
func TestEvaluateYankedIgnoresPolicy(t *testing.T) {
	p := Policy{Prerelease: "only", Cooldown: 72 * hour}
	if d := p.Evaluate(ev("release.yanked", false, "none", 0), start); d.Verdict != Admit {
		t.Fatalf("yanked = %+v, want admit", d)
	}
}

func TestPolicyJSON(t *testing.T) {
	b, err := Policy{Prerelease: "none", Cooldown: 72 * hour}.MarshalJSON()
	if err != nil || string(b) != `{"prerelease":"none","cooldown":"72h0m0s"}` {
		t.Fatalf("marshal = %s %v", b, err)
	}
	p, err := PolicyFromJSON(b)
	if err != nil || p.Prerelease != "none" || p.Cooldown != 72*hour {
		t.Fatalf("round trip = %+v %v", p, err)
	}
	for _, in := range []string{"", "null", "{}"} {
		if p, err := PolicyFromJSON([]byte(in)); err != nil || p != (Policy{}) {
			t.Errorf("%q = %+v %v, want zero", in, p, err)
		}
	}
	// Stored values out of range are clamped, never an error.
	if p, err := PolicyFromJSON([]byte(`{"cooldown":"99999h"}`)); err != nil || p.Cooldown != MaxCooldown {
		t.Errorf("over the maximum = %+v %v", p, err)
	}
	for _, in := range []string{`{"bump":["major"]}`, `{"prerelease":"all"}`, `{"cooldown":"soon"}`, `[]`} {
		if _, err := PolicyFromJSON([]byte(in)); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
	if Admit.String() != "admit" || Hold.String() != "hold" || Drop.String() != "drop" || Verdict(9).String() == "" {
		t.Error("Verdict.String")
	}
}

// AC: bump narrows the watch policy. The subscription only sees what the watch
// admitted, so listing a bump the watch drops matches nothing, and listing
// fewer bumps than the watch admits receives less.
func TestBumpNarrowsWatchPolicy(t *testing.T) {
	// A watch with bump: [minor, patch] admitted these events.
	admitted := []string{"minor", "patch"}
	matches := func(f Filter) (got []string) {
		for _, b := range admitted {
			if f.Match(Input{Type: "release.published", Bump: b}) {
				got = append(got, b)
			}
		}
		return got
	}
	if got := matches(Filter{}); len(got) != 2 {
		t.Errorf("inherit = %v, want both", got)
	}
	if got := matches(Filter{Bump: []string{"minor"}}); len(got) != 1 || got[0] != "minor" {
		t.Errorf("bump [minor] = %v, want [minor]", got)
	}
	if got := matches(Filter{Bump: []string{"major"}}); len(got) != 0 {
		t.Errorf("bump [major] must not widen the watch: %v", got)
	}
}
