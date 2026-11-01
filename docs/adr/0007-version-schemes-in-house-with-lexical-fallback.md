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

- Maven scheme (Phase 1); until then Maven sources use lexical order. Calver is described below.
- Conformance against real registry ordering (benchmark 6) is a separate story.

## Addendum: calver (story #68)

`internal/version/calver` (`scheme: calver`) is a fourth in-house scheme, selected **explicitly only**.

- **Formats.** `<year>.<n>[.<n>[.<n>]][suffix]` after an optional `v`: a 2-digit (`24.04`, YY.MM) or
  4-digit (`2026.10.1`, YYYY.MM.DD or YYYY.MM.MICRO; `2026.3`, YYYY.N) year, then one to three more
  numbers. Leading zeros are allowed and ignored in comparison (`24.04` == `24.4`, and a missing
  component counts as 0). The components are not validated as dates, since projects use them as
  counters. A suffix starts with a letter or one of `-._+` (`24.04-rc1`, `2026.10b1`,
  `2026.10.1.hotfix`, `2026.10.1+build5`).
- **Order.** Numeric components, then class (pre-release < none < other suffix), then, among
  pre-releases, the marker rank (dev < alpha < beta < pre/rc, as in PEP 440), then the suffix in
  natural order, ignoring case. A version is a pre-release when the lexical heuristic finds a
  marker in its last number plus its suffix (`24.04a`, `2026.10-rc1`). Build metadata after `+` is
  ignored for ordering and for the pre-release class, as in SemVer. A suffix needs at least one
  letter or digit.
- **Bump.** First differing component: year = `major`, second = `minor`, third or fourth = `patch`.
  With the same numbers: `prerelease` when next is a pre-release; a pre-release followed by its
  release gets the level the release introduces by the last non-zero number of next (`24.04-rc1`
  to `24.04` is `minor`, `2026.10.1-rc1` to `2026.10.1` is `patch`), like rule 5; any other suffix
  change is `patch`.
- **Year rollover is `major`** (`2025.12.1` to `2026.1.1`). Open question for the maintainer:
  whether a `bump: [major]` policy on a calver watch should instead see rollovers as `minor`.
- **Mixed year styles.** 2-digit and 4-digit years compare numerically (`24.01` < `2023.12`), so
  a watch should use one style. The scheme is explicit per watch, so this is up to the watch.
- **No auto-detect.** No source kind defaults to calver and tags are never sniffed: `2026.10.1`
  is also valid SemVer and `1.2` is a valid calver only for a 2- or 4-digit year, so guessing per
  tag would give a watch an unstable order. A watch that wants calver says `scheme: calver`;
  `schemes.Select(kind, override, logger)` is the lookup (an unknown name is an error). Tags that
  are not calver fall back to lexical like for any other scheme.

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
