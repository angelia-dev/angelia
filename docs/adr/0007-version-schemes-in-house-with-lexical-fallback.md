# ADR 0007: Version schemes are in-house, with a lexical fallback

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#54

## Context

The normalizer parses, orders and classifies versions per scheme, chosen by source kind and
overridable per watch ([architecture.md §3.4](../architecture.md#34-normalizer-and-policy-engine),
[prd.md §6](../prd.md#6-functional-requirements) F3). Benchmark 6 in
[architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions) asks for zero
ordering mismatches against the registries. §6 proposes `Masterminds/semver` for SemVer and in-house
PEP 440 and Maven comparators. Upstream data is untrusted: one odd tag must not stop a watch.

## Decision

1. **Interface.** `internal/version` defines `Scheme` (`Name`, `Parse`, `Compare`, `IsPrerelease`,
   `Bump(prev, next)`) and `Bump` (`major`, `minor`, `patch`, `prerelease`, `other`). All functions
   are pure. Numbers are compared as digit strings, so there is no size limit and no overflow.
   `Compare` returning 0 is not identity (`1.0` and `1.0.0` in PEP 440, build metadata in SemVer):
   known-version sets are keyed by the raw upstream string, never by the parsed value.
2. **Schemes.** `internal/version/semver` (SemVer 2.0.0, plus the npm loose prefix `=`/`v`),
   `internal/version/pep440` (the full specification, checked against the test vectors of
   `packaging` 26.3) and `internal/version/lexical` (natural string order). All three are in-house;
   **no third-party SemVer library** is added for now.
3. **Coercion of short tags.** `semver.Scheme{Coerce: true}` / `semver.ParseLoose` complete a core of
   one or two numbers like npm's `semver.coerce` (`3.12` → 3.12.0, `v2` → 2.0.0; pre-release and
   build parts are kept, so `3.12-alpine` is a pre-release of 3.12.0). A coerced version's `String()`
   is its original (trimmed) tag. Coercion is on by default for `oci`, `github-tag` and `go-proxy`,
   where short tags are common; `npm`, `crates`, `github-release` and `gitlab-release` stay strict.
   Without it, `1.2` would fall back to lexical and sort below every SemVer version.
4. **Selection and fallback.** `internal/version/schemes.Select(kind, override, logger)` picks the
   scheme (PyPI → pep440; npm, crates, GitHub/GitLab releases → strict semver; OCI, Git tags,
   Go proxy → coercing semver; anything else → lexical) and wraps it: a version the scheme rejects
   is parsed as lexical and logged at `DEBUG`. Fallback versions rank below every version the
   scheme can parse and are ordered lexically among themselves, so a mixed set keeps a total order.
   Only a blank string is an error. Callers detect a fallback with `v.Scheme() == "lexical"`; the
   fetch loop (#56) logs one aggregated `WARN` per fetch with `logger.With("watch", id)`.
5. **Bump.** The most significant changed component; between pre-releases of the same release it
   is `prerelease`; a pre-release followed by its release counts as the level that release introduces
   (`2.0.0-rc.1 → 2.0.0` is `major`, as npm's `semver.diff`); `other` when next is not greater than
   prev. The caller passes as `prev` the **greatest known version below next**, not the latest: with
   2.0.0 known, the backport 1.9.4 is compared with 1.9.3 and classified `patch`.

## Consequences

### Positive

- No new dependency. Exact control over the strictness of each scheme: Masterminds' loose parser
  coerces everywhere (we coerce only for tag-based sources) and rejects numbers above 64 bits that
  SemVer allows.
- Parsers are fuzzed (`make fuzz`), including transitivity of mixed orderings; unparseable input never
  panics and never stops a watch.

### Negative

- We own the SemVer code and its correctness (about 200 lines plus tests).
- Constraint ranges (`constraint: ">=2 <3"`, story for the policy engine) need their own parser or
  can still adopt `Masterminds/semver` constraints then.
- A per-watch override `scheme: semver` on a kind whose default is not SemVer gets strict SemVer.

### Open questions

- Maven and calver schemes (Phase 1); until then Maven sources use lexical order.
- Conformance against real registry ordering (benchmark 6) is a separate story.

## Alternatives considered

- **`Masterminds/semver/v3`.** Allowed by §6 and by the story. Not chosen for the reasons above;
  it remains an option for constraint parsing.
- **Fail the watch on an unparseable version.** Rejected: one bad tag would block every new release.
- **Warn once per unparseable version.** Rejected: a source with many odd tags would flood the log
  on every poll; the fetch loop aggregates instead.

## References

- SemVer 2.0.0: https://semver.org
- PyPA version specifiers: https://packaging.python.org/en/latest/specifications/version-specifiers/
- npm `semver.coerce` and `semver.diff`: https://github.com/npm/node-semver
- `internal/version/pep440/testdata/NOTICE` for the ported `packaging` test vectors and their licence.
