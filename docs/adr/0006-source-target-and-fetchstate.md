# ADR 0006: Source Target, FetchState and error contract

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#50
- **Amended by:** [ADR 0021](0021-source-canonical-target.md) (optional `Canonicalizer`: the canonical target is the coalescing key)

## Context

[architecture.md §3.3](../architecture.md#33-source-plugins-detect) defines the `Source` interface but names
`Target` and `FetchState` without defining them, and does not say what `Validate` returns or how `List` reports
cancellation. The data model ([§5](../architecture.md#5-data-model-simplified)) stores a target as
`coordinates_json` and `fetch_state_json`. Sources are "dumb" and the core must treat both values as data.
`Source` is a contract: changing it needs an ADR (AGENTS.md).

## Decision

- `Target` is `{Kind string; Coordinates map[string]string}`. Coordinates are flat strings by design: they are
  easy to store, compare and use as a coalescing key (50 watches, one fetch). The **source owns normalisation**
  (case folding, `_`/`-` in PyPI names, trimming); the core never interprets coordinates.
- `FetchState` is `{ETag, LastModified, Cursor string}`, opaque to the core. `Cursor` is source-owned and may hold
  JSON for sources whose pagination state does not fit a string token. The zero value means "no state".
- `Validate` does no I/O and returns an error wrapping the sentinel `source.ErrInvalidTarget`, so the API can map
  it to a 4xx.
- `List` wraps context errors with `%w`, so `errors.Is(err, context.Canceled)` and `context.DeadlineExceeded` work.
- Sources receive their `*http.Client` by injection through their constructor; they never use
  `http.DefaultClient`. This lets the netguard client (#49) and the contract suite's loopback guard be
  substituted.
- Contract tests live in `internal/source/sourcetest` and every source must pass them.

## Consequences

### Positive

- One uniform, serialisable shape for all sources; the core stays ignorant of per-source details.
- Callers can distinguish bad input from upstream failure and from cancellation.

### Negative

- Flat string coordinates cannot express nested configuration; a source needing it must encode it in a string.
- Sources must each implement normalisation consistently, or coalescing misses duplicates.

### Open questions

- Whether `http-json` and `oci` need typed coordinates; revisit when those sources are written.

## Alternatives considered

- **`Coordinates map[string]any` or `json.RawMessage`.** More expressive, but pushes decoding into the core or
  every caller, and makes comparison and coalescing keys harder. Rejected for v1.
- **`FetchState` as `[]byte` / `json.RawMessage`.** Fully opaque, but loses debuggability of ETag and
  Last-Modified in the store and logs. A source can still put JSON in `Cursor`.
- **`FetchState` as `map[string]string`.** Untyped; no gain over three named fields plus a free-form cursor.

## References

- [architecture.md §3.3](../architecture.md#33-source-plugins-detect), [§5](../architecture.md#5-data-model-simplified)
- angelia-dev/angelia#50, #49
