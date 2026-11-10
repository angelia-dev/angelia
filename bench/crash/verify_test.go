package main

import (
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fixture is a clean run: 2 packages with 2 baseline versions and 2 new
// versions each, 2 subscriptions, every delivery delivered and received
// once, and a registry that agrees.
func fixture() (*Snapshot, []Receipt, Truth) {
	s := &Snapshot{
		Packages:      map[string]string{"w0": "pkg-00000", "w1": "pkg-00001"},
		Versions:      map[string][]StoredVersion{},
		Subscriptions: map[string]string{"s0": "hook-0", "s1": "hook-1"},
	}
	truth := Truth{
		Packages: []PackageTruth{{Name: "pkg-00000", First: 2, Final: 4}, {Name: "pkg-00001", First: 2, Final: 4}},
		Sinks:    []string{"hook-0", "hook-1"},
	}
	var rs []Receipt
	n := 0
	for _, w := range []string{"w0", "w1"} {
		pkg := s.Packages[w]
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
			for _, sub := range []string{"s0", "s1"} {
				n++
				s.Deliveries = append(s.Deliveries, StoredDelivery{ID: fmt.Sprintf("d%d", n), EventID: ev, SubscriptionID: sub, Status: "delivered", Attempts: 1})
				rs = append(rs, Receipt{
					Sink: s.Subscriptions[sub], WebhookID: ev + ":" + sub, EventID: ev, Package: pkg, Version: ver,
					BodyHash: sha256.Sum256([]byte(ev)),
				})
			}
		}
	}
	return s, rs, truth
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
	s, rs, truth := fixture()
	checks, tally := Verify(s, rs, truth)
	if f := failed(checks); len(f) != 0 {
		t.Fatalf("failed checks %v: %+v", f, checks)
	}
	if tally.Events != 4 || tally.Deliveries != 8 || tally.Received != 8 || tally.Distinct != 8 || tally.Duplicates != 0 ||
		tally.BaselineVersions != 4 || tally.NewVersions != 4 || tally.ExpectedVersions != 4 || tally.Expected != 8 {
		t.Errorf("tally %+v", tally)
	}
}

// Acceptance criterion 2: duplicates are allowed when they carry the
// delivery's idempotency key and body.
func TestVerifyDuplicatesWithTheirKeyPass(t *testing.T) {
	s, rs, truth := fixture()
	rs = append(rs, rs[0], rs[0], rs[3])
	s.Deliveries[0].Attempts = 3
	checks, tally := Verify(s, rs, truth)
	if f := failed(checks); len(f) != 0 {
		t.Fatalf("failed checks %v", f)
	}
	if tally.Duplicates != 3 || tally.DuplicatedPairs != 2 || tally.RetriedDeliveries != 1 {
		t.Errorf("tally %+v", tally)
	}
}

const (
	cIntegrity  = "database integrity (integrity_check, foreign_key_check)"
	cWorkload   = "watches and subscriptions match the workload"
	cVersions   = "versions recorded match the registry at the freeze"
	cEvents     = "one event per new version"
	cRows       = "one delivery per event and subscription"
	cDead       = "no delivery given up (dead)"
	cRegistry   = "every release published before the freeze reached every sink (registry)"
	cLost       = "every delivery in the database received at least once (zero lost)"
	cKey        = "duplicates only within the idempotency key"
	cSigned     = "every request signed (Standard Webhooks)"
	cUnexpected = "no request outside the database's deliveries"
)

// Acceptance criterion 2: each fault fails exactly the checks that should
// see it, and the registry checks see a loss without trusting the
// database.
func TestVerifyFindsFaults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(s *Snapshot, rs []Receipt, tr *Truth) []Receipt
		want  []string
	}{
		{"lost delivery", func(_ *Snapshot, rs []Receipt, _ *Truth) []Receipt { return rs[1:] }, []string{cRegistry, cLost}},
		{"request with another subscription's key", func(_ *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			rs[0].WebhookID = rs[0].EventID + ":s1"
			return rs
		}, []string{cKey}},
		{"duplicate with another body", func(_ *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			d := rs[0]
			d.BodyHash = sha256.Sum256([]byte("changed"))
			return append(rs, d)
		}, []string{cKey}},
		{"bad signature", func(_ *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			rs[2].SigError = "no matching signature"
			return rs
		}, []string{cSigned}},
		{"unknown request", func(_ *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			return append(rs, Receipt{Sink: "hook-0", EventID: "nope", WebhookID: "nope:s0"})
		}, []string{cUnexpected}},
		{"delivery dead", func(s *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			s.Deliveries[1].Status, s.Deliveries[1].LastError = "dead", "giving up"
			return rs
		}, []string{cDead}},
		{"delivery row missing", func(s *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			s.Deliveries = s.Deliveries[1:]
			return rs
		}, []string{cRows, cUnexpected}},
		{"release published, never recorded", func(_ *Snapshot, rs []Receipt, tr *Truth) []Receipt {
			tr.Packages[0].Final = 5
			return rs
		}, []string{cVersions, cRegistry}},
		{"baseline differs from the registry's first response", func(_ *Snapshot, rs []Receipt, tr *Truth) []Receipt {
			tr.Packages[1].First = 1
			return rs
		}, []string{cVersions, cRegistry}},
		{"event missing for a new version", func(s *Snapshot, rs []Receipt, tr *Truth) []Receipt {
			s.Versions["pkg-00000"] = append(s.Versions["pkg-00000"], StoredVersion{Raw: "1.4.0", FirstSeen: 300})
			tr.Packages[0].Final = 5
			return rs
		}, []string{cEvents, cRegistry}},
		{"event for a baseline version", func(s *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			s.Events = append(s.Events, StoredEvent{ID: "x", WatchID: "w0", Type: "dev.angelia.release.published.v1", Package: "pkg-00000", Version: "1.0.0"})
			for sub, sinkName := range s.Subscriptions {
				s.Deliveries = append(s.Deliveries, StoredDelivery{ID: "x" + sub, EventID: "x", SubscriptionID: sub, Status: "delivered", Attempts: 1})
				rs = append(rs, Receipt{Sink: sinkName, WebhookID: "x:" + sub, EventID: "x", Package: "pkg-00000", Version: "1.0.0"})
			}
			return rs
		}, []string{cEvents}},
		{"version not in the registry", func(s *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			s.Versions["pkg-00001"] = append(s.Versions["pkg-00001"], StoredVersion{Raw: "1.9.0", FirstSeen: 100})
			return rs
		}, []string{cVersions}},
		{"subscription missing", func(_ *Snapshot, rs []Receipt, tr *Truth) []Receipt {
			tr.Sinks = append(tr.Sinks, "hook-2")
			return rs
		}, []string{cWorkload, cRegistry}},
		{"watch missing", func(_ *Snapshot, rs []Receipt, tr *Truth) []Receipt {
			tr.Packages = append(tr.Packages, PackageTruth{Name: "pkg-00002", First: 3, Final: 3})
			return rs
		}, []string{cWorkload, cVersions}},
		{"corrupt database", func(s *Snapshot, rs []Receipt, _ *Truth) []Receipt {
			s.Integrity = []string{"integrity_check: row 3 missing from index"}
			return rs
		}, []string{cIntegrity}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rs, truth := fixture()
			rs = tc.fault(s, rs, &truth)
			checks, _ := Verify(s, rs, truth)
			f := failed(checks)
			slices.Sort(f)
			slices.Sort(tc.want)
			if !slices.Equal(f, tc.want) {
				t.Fatalf("failed %v, want %v", f, tc.want)
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
	s, rs, truth := fixture()
	s.Deliveries[0].Status, s.Deliveries[1].Status = "pending", "in_flight"
	checks, tally := Verify(s, rs[2:], truth)
	if f := failed(checks); len(f) != 0 {
		t.Fatalf("failed checks %v", f)
	}
	if tally.Unfinished != 2 {
		t.Errorf("unfinished %d", tally.Unfinished)
	}
	var r Report
	r.decide(checks, setupChecks(100, 30, busyKills(100), tally, true))
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
