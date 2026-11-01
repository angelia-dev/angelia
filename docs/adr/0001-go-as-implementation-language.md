# ADR 0001: Go is the implementation language (provisional until Phase 0 benchmarks)

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#43

## Context

The workload is I/O-bound: thousands of small HTTPS calls per hour, JSON parsing and DB writes. Raw CPU
speed is not the bottleneck; memory footprint, concurrency model, single-binary packaging and contributor
reach are ([architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions)).
The Go recommendation is marked **[P0]**: an assumption to validate in Phase 0, not a settled fact.

The comparison in §6 is between Go and Rust. Rust wins on idle RSS (about 5-15 MB against 15-30 MB) and
has the best WASM host (`wasmtime`). Go wins on concurrency ceremony, ready-made pieces (Diun, Argus,
Shoutrrr, CloudEvents SDK, official MCP Go SDK, River, sqlc), contributor pool and iteration speed.
Both produce a single static binary. Footprint targets come from [prd.md §7](../prd.md#7-non-functional-requirements).

## Decision

Angelia is implemented in **Go** (version per `go.mod`), built with `CGO_ENABLED=0`
([AGENTS.md](../../AGENTS.md)). The stack in architecture.md §6 is the working assumption for
Phase 0 stories; a dependency outside it needs maintainer approval.

This decision is **provisional**. It is confirmed or reopened by the Phase 0 report using the benchmarks
below ([prd.md §9](../prd.md#9-roadmap): "Benchmarks pass, language and plugin choices confirmed").

### Thresholds that would reopen this decision

architecture.md §6 says to revisit Go "only if Phase 0 benchmarks miss the targets", and
[prd.md §8](../prd.md#8-success-metrics) requires all six to pass. **Missing any one of the six Phase 0
targets reopens this ADR.** The pass/fail targets, as written in architecture.md §6:

1. 10,000 watches over 5 ecosystems on 1 vCPU / 256 MB: P95 check lag < 2 min, RSS < 100 MB, CPU < 20%.
2. Idle (100 watches): RSS < 40 MB, binary < 40 MB, image < 50 MB (distroless).
3. GitHub: 2,000 repos on a single token without hitting 429/403 (ETag hit rate > 90%).
4. WASM source plugin round-trip overhead < 5 ms per call vs native.
5. Kill -9 during dispatch: zero lost deliveries, duplicates only within the idempotency key.
6. Version ordering: 0 mismatches against registry ordering on the top 1,000 PyPI and npm packages.


## Consequences

### Positive

- Goroutines make many concurrent HTTP polls simple.
- Most of the stack already exists in Go (`chi`, `sqlc`, `goose`, `cloudevents/sdk-go`,
  `modelcontextprotocol/go-sdk`, `shoutrrr`, `extism/go-sdk` on `wazero`).
- Large contributor pool, low onboarding barrier, fast compiles.
- Pure-Go dependencies keep multi-arch single-binary and distroless builds simple.

### Negative

- Idle RSS about 10-15 MB higher than Rust; architecture.md §6 judges this immaterial for a self-hosted service.
- The WASM host (`wazero`) is less capable than `wasmtime`.
- No CGO: pure-Go replacements are needed wherever a C library would be the usual choice (see ADR 0003 for SQLite).

### Open questions

- A miss on benchmark 3 to 6 may be an application bug and not a language limit (3, 5 and 6 test the
  application rather than the language). Should such a miss be diagnosed before reopening the language
  choice? The docs do not say; the rule above reopens on any miss. Maintainer to confirm.
- The docs compare only Go and Rust; no other language was evaluated.

## Alternatives considered

- **Rust.** Smaller idle footprint and the best WASM runtime (`wasmtime`), but a smaller contributor pool,
  higher onboarding barrier, slower compiles and fewer ready-made pieces (no Shoutrrr equivalent). The
  10-15 MB memory advantage does not matter for this workload (architecture.md §6).
- **Go with CGO dependencies** (for example a C-based SQLite driver). Rejected by the project rule that
  `CGO_ENABLED=0` must build, which keeps static multi-arch binaries and distroless images simple
  ([AGENTS.md](../../AGENTS.md); architecture.md §6 table, "Single static binary").

## References

- [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions)
- The go/no-go belongs to the Phase 0 report story (angelia-dev/angelia#110), not to this ADR.
- [prd.md §7](../prd.md#7-non-functional-requirements), [§8](../prd.md#8-success-metrics), [§9](../prd.md#9-roadmap)
