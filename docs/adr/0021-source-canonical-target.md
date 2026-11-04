# ADR 0021: Sources canonicalise targets through an optional `Canonicalizer` interface

- **Status:** Accepted (2026-11-04, maintainer decision on #181)
- **Date:** 2026-11-04
- **Story:** angelia-dev/angelia#134
- **Amends:** [ADR 0006](0006-source-target-and-fetchstate.md)

## Context

[ADR 0006](0006-source-target-and-fetchstate.md) says that the **source owns normalisation** of coordinates
and that `(kind, coordinates_json)` is the coalescing key: 50 watches on one upstream object cost one fetch
([architecture.md §3.2](../architecture.md#32-scheduler-and-workers)). The unique index `target_kind_coordinates` and
`watchfile` apply both key targets on the coordinates exactly as stored.

But the `Source` interface ([§3.3](../architecture.md#33-source-plugins-detect)) has no method that returns
the normalised coordinates. `Validate` only accepts or rejects. So the core stored coordinates as the user
wrote them, and `{name: Requests}` and `{name: requests}` on PyPI became two target rows and two fetches of
the same project. The pypi and crates sources already normalised names inside `List` and `event.Package`,
but not the key.

`Source` is a contract; changing it needs an ADR (AGENTS.md).

## Decision

1. **An additive, optional interface** in `internal/source`:

   ```go
   type Canonicalizer interface {
       Canonical(t Target) (Target, error)
   }
   ```

   The three methods of `Source` are unchanged. A source whose coordinates can name one upstream object in
   several spellings implements `Canonicalizer`; a source whose coordinates are canonical as written does not.

2. **Rules for `Canonical`.** It is called only on a target that `Validate` accepted. It does no I/O, keeps
   `Kind`, does not mutate its argument, is idempotent (`Canonical(Canonical(t)) == Canonical(t)`), and its
   result passes `Validate`. A rejection wraps `ErrInvalidTarget`. Coordinates the source does not
   normalise are kept as they are.

3. **The core calls it through `source.Canonical(src, t)`**, which falls back to a copy of `t` for a source
   without `Canonicalizer`, never shares the coordinates map with its input or the source, and rejects a
   result with another `Kind`.

4. **Where it is applied.** Targets are created only by `watchfile` apply. The watch-file parser validates a
   watch's coordinates and then replaces them with `source.Canonical`, so `Watch.Coordinates`, the
   coalescing key of apply, the `FindTarget` lookup and the stored `coordinates_json` are all canonical.
   A watch that is re-spelled (`Requests` to `requests`) stays on its target: no move, no baseline reset.
   Any later path that creates targets (REST API, MCP) must call `source.Canonical` the same way.

5. **The contract suite** (`sourcetest.Run`) gains a `Canonical` check: for a `Canonicalizer`, the canonical
   form of `Fixtures.Valid` keeps the kind, passes `Validate`, is a fixed point, leaves its argument intact
   and costs no request. `Fixtures.Aliases` lists other spellings of `Valid` that must canonicalise to the
   same target. A `Canonicalizer` whose fixtures list no `Aliases` fails, and so does listing aliases for a
   source without `Canonicalizer`.

6. **Built-in sources**, implemented only where the registry's own rule is clear:

   | Kind | Canonical | Rule |
   |---|---|---|
   | `pypi` | `name` trimmed and PEP 503-normalised (`Zope.Interface` → `zope-interface`) | [PEP 503, normalized names](https://peps.python.org/pep-0503/#normalized-names): PyPI treats these spellings as one project |
   | `crates` | `name` lower-cased; `-` and `_` kept apart | crates.io does not allow two names that differ only in case or in `-`/`_`, and the sparse index files are named by the lower-case name ([Cargo registry index](https://doc.rust-lang.org/cargo/reference/registry-index.html#index-files)). But the sparse index resolves only the separator spelling the crate was published with, so folding `-`/`_` would need I/O to find it; that is why they are kept apart |
   | `npm` | none: as written | the registry is case-sensitive and legacy packages with capitals (`JSONStream`) are distinct from their lower-case spelling; only new names must be lower case ([package.json `name`](https://docs.npmjs.com/cli/configuring-npm/package-json#name)) |
   | `github-release`, `github-tag` | none for now | see Open questions |
   | `gitlab-release` | none for now | see Open questions |
   | WASM plugins | none | the plugin ABI has no canonical call; adding one is an ABI change |

7. **No migration.** No release has shipped, so no deployment holds data worth rewriting. Merging target rows
   that differ only in spelling would rewrite and merge data (known sets, fetch state, watches), which is a
   maintainer decision. A store built before this change keeps its rows; on the next apply, a file watch
   whose stored target is not canonical is moved to the canonical target like any change of coordinates
   (the plan reports `source/coordinates (baseline reset)`), and the old target is deleted when no watch
   uses it. The baseline reset has a cost. With `baseline: true` (the default), releases published since
   the old target's last check, and any pending cooldown holds, are dropped. With `baseline: false`, the
   whole history is re-emitted under new event ids, and receivers avoid duplicates only if they
   deduplicate on `subject` + `type`. Only development stores are affected, since nothing has shipped.

## Consequences

### Positive

- Spellings of one PyPI project or crate share one target row and one fetch, as ADR 0006 intended.
- The `Source` interface is unchanged: existing sources, test fakes and plugins keep compiling and keep
  their behaviour.
- `data.package` does not change for pypi and crates, because `event.Package` already used the same
  normalisation, with one fix: a space-padded pypi name (`" requests "`) used to give `data.package`
  `" requests "` and now gives `"requests"`, like every other spelling.

### Negative

- Canonicalisation is opt-in, so a new source that forgets it silently loses coalescing. The contract
  suite requires `Aliases` from every `Canonicalizer`, but cannot detect a source that should implement
  it and does not.
- A store made by a development build before this change may hold non-canonical targets; the first apply
  after upgrading moves those watches with a baseline reset: with `baseline: true`, releases since the
  last check and pending cooldown holds are dropped; with `baseline: false`, the history is re-emitted
  under new event ids.

### Open questions

- **GitHub and GitLab.** github.com and GitLab resolve owner, repository and project paths
  case-insensitively, and the purl spec lower-cases the `github` and `gitlab` types (`event/purl.go`
  already does). But `data.package` for these kinds is the coordinate as written, and subscription
  `package` filters match it case-sensitively. Lower-casing the stored coordinate would change
  `data.package` and could stop existing filters from matching. Left to the maintainer: (a) keep as
  written (current), (b) lower-case the coordinate and accept the change in `data.package`, or
  (c) lower-case the key only and keep the display spelling elsewhere (needs a schema change).
- Whether a later story should merge pre-existing duplicate target rows with a migration.

## Alternatives considered

- **`Validate` returns the canonical target** (`Validate(Target) (Target, error)`). One call instead of two,
  but it changes an existing method of the contract, breaks every source, fake and plugin adapter, and
  forces sources with nothing to normalise to echo their input. Rejected.
- **The core normalises** (lower-casing every coordinate). Wrong for npm (case-sensitive) and crates
  (`-`/`_` are distinct), and contradicts ADR 0006: the core never interprets coordinates. Rejected.
- **A database-level case-insensitive index.** Same objection, and cannot express PEP 503 separator folding.

## References

- [ADR 0006](0006-source-target-and-fetchstate.md), [ADR 0009](0009-scheduler-target-leases-and-backoff.md)
- [architecture.md §3.3](../architecture.md#33-source-plugins-detect), [watch-file.md §3](../watch-file.md)
- angelia-dev/angelia#134 (found while reviewing #51, #52, #53)
