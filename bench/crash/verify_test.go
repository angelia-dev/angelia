package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

// fixture is a clean run: 2 packages with 2 baseline versions and 2 new
// versions each, 2 subscriptions, every delivery delivered and received
// once.
func fixture() (*Snapshot, []Receipt) {
	s := &Snapshot{
		Packages:      map[string]string{"w0": "pkg-00000", "w1": "pkg-00001"},
		Versions:      map[string][]StoredVersion{},
		Subscriptions: map[string]string{"s0": "hook-0", "s1": "hook-1"},
	}
	var rs []Receipt
	n := 0
	for w, pkg := range map[string]string{"w0": "pkg-00000", "w1": "pkg-00001"} {
		for k := range 4 {
			seen := int64(100)
			if k >= 2 {
				seen = 200 + int64(k)
			}
			ver := fmt.Sprintf("1.%d.0", k)
			s.Versions[pkg] = append(s.Versions[pkg], StoredVersion{Raw: ver, FirstSeen: seen})
			if k < 2 {
				continue
			}
			ev := fmt.Sprintf("ev-%s-%d", pkg, k)
			s.Events = append(s.Events, StoredEvent{ID: ev, WatchID: w, Type: "dev.angelia.release.published.v1", Package: pkg, Version: ver})
			for sub, sinkName := range s.Subscriptions {
				n++
				s.Deliveries = append(s.Deliveries, StoredDelivery{ID: fmt.Sprintf("d%d", n), EventID: ev, SubscriptionID: sub, Status: "delivered", Attempts: 1})
				rs = append(rs, Receipt{Sink: sinkName, WebhookID: ev + ":" + sub, EventID: ev, BodyHash: sha256.Sum256([]byte(ev))})
			}
		}
	}
	return s, rs
}

func failed(checks []Check) []string {
	var out []string
	for _, c := range checks {
		if !c.OK {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestVerifyCleanRun(t *testing.T) {
	s, rs := fixture()
	checks, tally := Verify(s, rs)
	if f := failed(checks); len(f) != 0 {
		t.Fatalf("failed checks %v: %+v", f, checks)
	}
	if tally.Events != 4 || tally.Deliveries != 8 || tally.Received != 8 || tally.Distinct != 8 || tally.Duplicates != 0 ||
		tally.BaselineVersions != 4 || tally.NewVersions != 4 {
		t.Errorf("tally %+v", tally)
	}
}

// Acceptance criterion 2: duplicates are allowed when they carry the
// delivery's idempotency key and body.
func TestVerifyDuplicatesWithTheirKeyPass(t *testing.T) {
	s, rs := fixture()
	rs = append(rs, rs[0], rs[0], rs[3])
	s.Deliveries[0].Attempts = 3
	checks, tally := Verify(s, rs)
	if f := failed(checks); len(f) != 0 {
		t.Fatalf("failed checks %v", f)
	}
	if tally.Duplicates != 3 || tally.DuplicatedPairs != 2 || tally.RetriedDeliveries != 1 {
		t.Errorf("tally %+v", tally)
	}
}

// Acceptance criterion 2: every check that can find a lost or wrong
// delivery fails on exactly that fault.
func TestVerifyFindsFaults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(s *Snapshot, rs []Receipt) []Receipt
		want  string
	}{
		{"lost delivery", func(_ *Snapshot, rs []Receipt) []Receipt { return rs[1:] }, "every delivery received at least once (zero lost)"},
		{"duplicate with another key", func(_ *Snapshot, rs []Receipt) []Receipt {
			d := rs[0]
			d.WebhookID = "other:key"
			return append(rs, d)
		}, "duplicates only within the idempotency key"},
		{"duplicate with another body", func(_ *Snapshot, rs []Receipt) []Receipt {
			d := rs[0]
			d.BodyHash = sha256.Sum256([]byte("changed"))
			return append(rs, d)
		}, "duplicates only within the idempotency key"},
		{"bad signature", func(_ *Snapshot, rs []Receipt) []Receipt {
			rs[2].SigError = "no matching signature"
			return rs
		}, "every request signed (Standard Webhooks)"},
		{"unknown request", func(_ *Snapshot, rs []Receipt) []Receipt {
			return append(rs, Receipt{Sink: "hook-0", EventID: "nope", WebhookID: "nope:s0"})
		}, "no request outside the database's deliveries"},
		{"delivery dead", func(s *Snapshot, rs []Receipt) []Receipt {
			s.Deliveries[1].Status, s.Deliveries[1].LastError = "dead", "giving up"
			return rs
		}, "no delivery given up (dead)"},
		{"delivered but never received", func(_ *Snapshot, rs []Receipt) []Receipt {
			return rs[:len(rs)-1]
		}, "every delivery received at least once (zero lost)"},
		{"delivery row missing", func(s *Snapshot, rs []Receipt) []Receipt {
			s.Deliveries = s.Deliveries[1:]
			return rs[1:]
		}, "one delivery per event and subscription"},
		{"event missing for a new version", func(s *Snapshot, rs []Receipt) []Receipt {
			s.Versions["pkg-00000"] = append(s.Versions["pkg-00000"], StoredVersion{Raw: "1.4.0", FirstSeen: 300})
			return rs
		}, "one event per new version"},
		{"event for a baseline version", func(s *Snapshot, rs []Receipt) []Receipt {
			s.Events = append(s.Events, StoredEvent{ID: "x", WatchID: "w0", Type: "dev.angelia.release.published.v1", Package: "pkg-00000", Version: "1.0.0"})
			for sub, sinkName := range s.Subscriptions {
				s.Deliveries = append(s.Deliveries, StoredDelivery{ID: "x" + sub, EventID: "x", SubscriptionID: sub, Status: "delivered", Attempts: 1})
				rs = append(rs, Receipt{Sink: sinkName, WebhookID: "x:" + sub, EventID: "x"})
			}
			return rs
		}, "one event per new version"},
		{"version gap", func(s *Snapshot, rs []Receipt) []Receipt {
			s.Versions["pkg-00001"] = append(s.Versions["pkg-00001"], StoredVersion{Raw: "1.9.0", FirstSeen: 100})
			return rs
		}, "versions recorded without gaps"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rs := fixture()
			rs = tc.fault(s, rs)
			checks, _ := Verify(s, rs)
			f := failed(checks)
			if len(f) != 1 || f[0] != tc.want {
				t.Fatalf("failed %v, want only %q", f, tc.want)
			}
			for _, c := range checks {
				if !c.OK && (len(c.Examples) == 0 || !strings.Contains(c.Detail, "problems")) {
					t.Errorf("failed check without examples: %+v", c)
				}
			}
		})
	}
}

// A delivery still pending when the drain ends is neither delivered nor
// lost: it is counted for the drain check, which makes the run incomplete.
func TestVerifyUnfinishedIsNotLost(t *testing.T) {
	s, rs := fixture()
	s.Deliveries[0].Status, s.Deliveries[1].Status = "pending", "in_flight"
	checks, tally := Verify(s, rs[2:])
	if f := failed(checks); len(f) != 0 {
		t.Fatalf("failed checks %v", f)
	}
	if tally.Unfinished != 2 {
		t.Errorf("unfinished %d", tally.Unfinished)
	}
	setup := setupChecks(100, busyKills(100), tally)
	var r Report
	r.decide(checks, setup)
	if r.Verdict != "incomplete" {
		t.Errorf("verdict %s", r.Verdict)
	}
}

func TestMinorOf(t *testing.T) {
	for raw, want := range map[string]int{"1.0.0": 0, "1.42.0": 42} {
		if k, ok := minorOf(raw); !ok || k != want {
			t.Errorf("minorOf(%q) = %d, %v", raw, k, ok)
		}
	}
	for _, raw := range []string{"2.0.0", "1.x.0", "1.1.1", "1.-1.0"} {
		if _, ok := minorOf(raw); ok {
			t.Errorf("minorOf(%q) accepted", raw)
		}
	}
}
