package main

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Check is one pass/fail line of the report.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Examples are up to maxExamples offending items, for the report.
	Examples []string `json:"examples,omitempty"`
}

// Tally is what the verification counted.
type Tally struct {
	Events            int `json:"events"`
	Deliveries        int `json:"deliveries"`
	Received          int `json:"received"`           // requests at the receiver
	Distinct          int `json:"distinct"`           // distinct (event, subscription) pairs received
	Duplicates        int `json:"duplicates"`         // received - distinct
	DuplicatedPairs   int `json:"duplicated_pairs"`   // pairs received more than once
	RetriedDeliveries int `json:"retried_deliveries"` // delivery rows with attempts > 1
	BaselineVersions  int `json:"baseline_versions"`
	ExpectedVersions  int `json:"expected_versions"` // from the registry: published after the baseline, before the freeze
	Expected          int `json:"expected"`          // ExpectedVersions x subscriptions
	NewVersions       int `json:"new_versions"`
	// Unfinished are the deliveries still pending or in flight at the end:
	// the drain did not finish, so they are neither delivered nor lost.
	Unfinished int `json:"unfinished"`
}

const maxExamples = 10

type checker struct {
	checks []Check
}

func (c *checker) add(name, okDetail string, bad []string) {
	ch := Check{Name: name, OK: len(bad) == 0, Detail: okDetail}
	if !ch.OK {
		sort.Strings(bad)
		ch.Detail = fmt.Sprintf("%d problems", len(bad))
		ch.Examples = bad[:min(len(bad), maxExamples)]
	}
	c.checks = append(c.checks, ch)
}

// Truth is what the fake registry says about the run, independent of
// angelia's database.
type Truth struct {
	// Packages is every watched package with its version count in the
	// first response angelia got (its baseline) and at the freeze.
	Packages []PackageTruth
	// Sinks are the sink names of the workload, one subscription each.
	Sinks []string
}

// PackageTruth is one package as the registry served it.
type PackageTruth struct {
	Name         string
	First, Final int
}

// Verify compares the registry, the database and what the receiver got.
// The checks, in the order of the pipeline:
//
//  1. the database is sound (PRAGMA integrity_check, foreign_key_check);
//  2. it holds the workload's watches and subscriptions;
//  3. every package's recorded versions are exactly the registry's at the
//     freeze, and its baseline the registry's first response (no version
//     lost before it was recorded);
//  4. every version first seen after the baseline has exactly one
//     release.published event, and no other version has one (the
//     transactional outbox: no state change without its event);
//  5. every event has exactly one delivery per subscription;
//  6. no delivery is dead (given up: lost for its consumer);
//  7. every new version the registry published before the freeze reached
//     every sink, judged from the registry and the requests alone;
//  8. every finished delivery of the database was received at least once;
//     deliveries still pending or in flight are counted in
//     Tally.Unfinished, which the drain check judges;
//  9. every request carries the idempotency key of its event and
//     subscription, so duplicates share it, and duplicates have the same
//     body;
//  10. every request was signed with the run's secret;
//  11. every request belongs to a delivery of the database.
func Verify(s *Snapshot, receipts []Receipt, truth Truth) ([]Check, Tally) {
	var c checker
	var t Tally

	c.add("database integrity (integrity_check, foreign_key_check)", "ok", slices.Clone(s.Integrity))

	var wl []string
	byName := map[string]bool{}
	for _, p := range s.Packages {
		byName[p] = true
	}
	for _, p := range truth.Packages {
		if !byName[p.Name] {
			wl = append(wl, "no watch for "+p.Name)
		}
	}
	if len(s.Packages) != len(truth.Packages) {
		wl = append(wl, fmt.Sprintf("%d watches, want %d", len(s.Packages), len(truth.Packages)))
	}
	sinks := map[string]bool{}
	for _, name := range s.Subscriptions {
		sinks[name] = true
	}
	for _, name := range truth.Sinks {
		if !sinks[name] {
			wl = append(wl, "no subscription for sink "+name)
		}
	}
	if len(s.Subscriptions) != len(truth.Sinks) {
		wl = append(wl, fmt.Sprintf("%d subscriptions, want %d", len(s.Subscriptions), len(truth.Sinks)))
	}
	c.add("watches and subscriptions match the workload", fmt.Sprintf("%d watches, %d subscriptions", len(truth.Packages), len(truth.Sinks)), wl)

	// 3 and 4: versions and events.
	type pv struct{ pkg, ver string }
	newVersions := map[pv]bool{}
	var vBad []string
	for _, p := range truth.Packages {
		vs := s.Versions[p.Name]
		if len(vs) == 0 {
			vBad = append(vBad, p.Name+": no versions recorded")
			continue
		}
		base := vs[0].FirstSeen
		for _, v := range vs {
			base = min(base, v.FirstSeen)
		}
		seen, baseline := map[int]bool{}, 0
		for _, v := range vs {
			k, ok := minorOf(v.Raw)
			if !ok || k >= p.Final {
				vBad = append(vBad, fmt.Sprintf("%s: version %q not in the registry (%d versions at the freeze)", p.Name, v.Raw, p.Final))
				continue
			}
			seen[k] = true
			if v.FirstSeen > base {
				newVersions[pv{p.Name, v.Raw}] = true
				t.NewVersions++
			} else {
				baseline++
			}
		}
		for k := range p.Final {
			if !seen[k] {
				vBad = append(vBad, fmt.Sprintf("%s: version 1.%d.0 published before the freeze, not recorded", p.Name, k))
			}
		}
		if baseline != p.First {
			vBad = append(vBad, fmt.Sprintf("%s: baseline of %d versions, the registry's first response had %d", p.Name, baseline, p.First))
		}
		t.BaselineVersions += baseline
		t.ExpectedVersions += max(0, p.Final-p.First)
	}
	c.add("versions recorded match the registry at the freeze",
		fmt.Sprintf("%d packages: %d baseline and %d new versions, as published", len(truth.Packages), t.BaselineVersions, t.ExpectedVersions), vBad)

	var evBad []string
	evByID := map[string]StoredEvent{}
	got := map[pv]int{}
	for _, e := range s.Events {
		evByID[e.ID] = e
		if e.Type != "dev.angelia.release.published.v1" {
			evBad = append(evBad, fmt.Sprintf("event %s: unexpected type %s", e.ID, e.Type))
			continue
		}
		if s.Packages[e.WatchID] != e.Package {
			evBad = append(evBad, fmt.Sprintf("event %s: watch %s is not package %s", e.ID, e.WatchID, e.Package))
		}
		k := pv{e.Package, e.Version}
		got[k]++
		if !newVersions[k] {
			evBad = append(evBad, fmt.Sprintf("event %s: %s %s is not a new recorded version", e.ID, e.Package, e.Version))
		}
	}
	for k := range newVersions {
		if got[k] != 1 {
			evBad = append(evBad, fmt.Sprintf("%s %s: %d events, want 1", k.pkg, k.ver, got[k]))
		}
	}
	t.Events = len(s.Events)
	c.add("one event per new version", fmt.Sprintf("%d events for %d new versions (%d baseline versions without events)", t.Events, t.NewVersions, t.BaselineVersions), evBad)

	// 3 and 4: deliveries.
	type pair struct{ event, sub string }
	rows := map[pair]StoredDelivery{}
	var dBad, dead []string
	for _, d := range s.Deliveries {
		k := pair{d.EventID, d.SubscriptionID}
		if _, dup := rows[k]; dup {
			dBad = append(dBad, fmt.Sprintf("event %s subscription %s: two delivery rows", d.EventID, d.SubscriptionID))
		}
		rows[k] = d
		if _, ok := s.Subscriptions[d.SubscriptionID]; !ok {
			dBad = append(dBad, fmt.Sprintf("delivery %s: unknown subscription %s", d.ID, d.SubscriptionID))
		}
		switch d.Status {
		case "delivered":
		case "dead":
			dead = append(dead, fmt.Sprintf("delivery %s (%s %s): dead after %d attempts: %s",
				d.ID, evByID[d.EventID].Package, evByID[d.EventID].Version, d.Attempts, d.LastError))
		default:
			t.Unfinished++
		}
		if d.Attempts > 1 {
			t.RetriedDeliveries++
		}
	}
	for _, e := range s.Events {
		for sub := range s.Subscriptions {
			if _, ok := rows[pair{e.ID, sub}]; !ok {
				dBad = append(dBad, fmt.Sprintf("event %s (%s %s): no delivery for subscription %s", e.ID, e.Package, e.Version, sub))
			}
		}
	}
	t.Deliveries = len(s.Deliveries)
	c.add("one delivery per event and subscription", fmt.Sprintf("%d deliveries = %d events x %d subscriptions", t.Deliveries, t.Events, len(s.Subscriptions)), dBad)
	c.add("no delivery given up (dead)", fmt.Sprintf("%d delivered, %d dead, %d unfinished", t.Deliveries-len(dead)-t.Unfinished, len(dead), t.Unfinished), dead)

	// 5 to 8: what the receiver got.
	subBySink := map[string]string{}
	for id, sinkName := range s.Subscriptions {
		subBySink[sinkName] = id
	}
	bodies := map[pair][32]byte{}
	count := map[pair]int{}
	var keyBad, sigBad, unexpected []string
	for _, r := range receipts {
		sub, ok := subBySink[r.Sink]
		k := pair{r.EventID, sub}
		if _, known := rows[k]; !ok || !known {
			unexpected = append(unexpected, fmt.Sprintf("sink %s event %q (webhook-id %q)", r.Sink, r.EventID, r.WebhookID))
			continue
		}
		if want := r.EventID + ":" + sub; r.WebhookID != want {
			keyBad = append(keyBad, fmt.Sprintf("event %s subscription %s: webhook-id %q, want %q", r.EventID, sub, r.WebhookID, want))
		}
		if h, seen := bodies[k]; seen && h != r.BodyHash {
			keyBad = append(keyBad, fmt.Sprintf("event %s subscription %s: a duplicate with a different body", r.EventID, sub))
		}
		bodies[k] = r.BodyHash
		count[k]++
		if r.SigError != "" {
			sigBad = append(sigBad, fmt.Sprintf("event %s sink %s: %s", r.EventID, r.Sink, r.SigError))
		}
	}
	type ps struct{ pkg, ver, sink string }
	gotReg := map[ps]bool{}
	for _, r := range receipts {
		gotReg[ps{r.Package, r.Version, r.Sink}] = true
	}
	// A delivery the drain left unfinished is not lost (yet): the drain
	// check judges it.
	for _, d := range s.Deliveries {
		if d.Status == "pending" || d.Status == "in_flight" {
			e := evByID[d.EventID]
			gotReg[ps{e.Package, e.Version, s.Subscriptions[d.SubscriptionID]}] = true
		}
	}
	var regLost []string
	for _, p := range truth.Packages {
		for k := p.First; k < p.Final; k++ {
			for _, sk := range truth.Sinks {
				t.Expected++
				if v := fmt.Sprintf("1.%d.0", k); !gotReg[ps{p.Name, v, sk}] {
					regLost = append(regLost, fmt.Sprintf("%s %s to %s", p.Name, v, sk))
				}
			}
		}
	}
	c.add("every release published before the freeze reached every sink (registry)",
		fmt.Sprintf("%d of %d expected (%d new versions x %d sinks)", t.Expected-len(regLost), t.Expected, t.ExpectedVersions, len(truth.Sinks)), regLost)

	var lost []string
	for k, d := range rows {
		if count[k] == 0 && (d.Status == "delivered" || d.Status == "dead") {
			lost = append(lost, fmt.Sprintf("delivery %s: event %s (%s %s) subscription %s (%s), status %s, %d attempts",
				d.ID, k.event, evByID[k.event].Package, evByID[k.event].Version, k.sub, s.Subscriptions[k.sub], d.Status, d.Attempts))
		}
	}
	t.Received, t.Distinct = len(receipts), len(count)
	for _, n := range count {
		if n > 1 {
			t.DuplicatedPairs++
			t.Duplicates += n - 1
		}
	}
	c.add("every delivery in the database received at least once (zero lost)", fmt.Sprintf("%d of %d finished deliveries received", len(rows)-t.Unfinished-len(lost), len(rows)-t.Unfinished), lost)
	c.add("duplicates only within the idempotency key", fmt.Sprintf("%d duplicate requests over %d deliveries, each with its delivery's key and body", t.Duplicates, t.DuplicatedPairs), keyBad)
	c.add("every request signed (Standard Webhooks)", fmt.Sprintf("%d requests verified", len(receipts)-len(sigBad)-len(unexpected)), sigBad)
	c.add("no request outside the database's deliveries", "none", unexpected)
	return c.checks, t
}

// minorOf returns k for the fake registry's version "1.k.0".
func minorOf(raw string) (int, bool) {
	rest, ok := strings.CutPrefix(raw, "1.")
	if !ok {
		return 0, false
	}
	mid, ok := strings.CutSuffix(rest, ".0")
	if !ok {
		return 0, false
	}
	k, err := strconv.Atoi(mid)
	return k, err == nil && k >= 0
}
