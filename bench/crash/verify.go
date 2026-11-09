package main

import (
	"fmt"
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

// Verify compares the database with what the receiver got. The checks, in
// the order of the pipeline:
//
//  1. every package's versions are 1.0.0 to 1.k.0 with no gap (no version
//     lost before it was recorded);
//  2. every version first seen after the package's baseline check has
//     exactly one release.published event, and no other version has one
//     (the transactional outbox: no state change without its event);
//  3. every event has exactly one delivery per subscription;
//  4. no delivery is dead (given up: lost for its consumer);
//  5. every finished delivery was received at least once (no delivery
//     lost); deliveries still pending or in flight are counted in
//     Tally.Unfinished, which the drain check judges;
//  6. every request carries the idempotency key of its event and
//     subscription, so duplicates share it, and duplicates have the same
//     body;
//  7. every request was signed with the run's secret;
//  8. every request belongs to a delivery of the database.
func Verify(s *Snapshot, receipts []Receipt) ([]Check, Tally) {
	var c checker
	var t Tally

	// 1 and 2: versions and events.
	type pv struct{ pkg, ver string }
	newVersions := map[pv]bool{}
	var gaps []string
	pkgs := make([]string, 0, len(s.Versions))
	for p := range s.Versions {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		vs := s.Versions[p]
		seen := map[int]bool{}
		maxK, base := -1, vs[0].FirstSeen
		for _, v := range vs {
			base = min(base, v.FirstSeen)
		}
		for _, v := range vs {
			k, ok := minorOf(v.Raw)
			if !ok {
				gaps = append(gaps, fmt.Sprintf("%s: unexpected version %q", p, v.Raw))
				continue
			}
			seen[k], maxK = true, max(maxK, k)
			if v.FirstSeen > base {
				newVersions[pv{p, v.Raw}] = true
				t.NewVersions++
			} else {
				t.BaselineVersions++
			}
		}
		for k := 0; k <= maxK; k++ {
			if !seen[k] {
				gaps = append(gaps, fmt.Sprintf("%s: version 1.%d.0 missing (have up to 1.%d.0)", p, k, maxK))
			}
		}
	}
	c.add("versions recorded without gaps", fmt.Sprintf("%d packages", len(pkgs)), gaps)

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
	c.add("every delivery received at least once (zero lost)", fmt.Sprintf("%d of %d finished deliveries received", len(rows)-t.Unfinished-len(lost), len(rows)-t.Unfinished), lost)
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
