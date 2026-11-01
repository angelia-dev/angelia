# ADR 0006: Version schemes are in-house, with a lexical fallback

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
2. **Schemes.** `internal/version/semver` (SemVer 2.0.0, plus the npm loose prefix `=`/`v`),
   `internal/version/pep440` (the full specification, checked against the test vectors of
   `packaging` 26.3) and `internal/version/lexical` (natural string order). All three are in-house;
   **no third-party SemVer library** is added for now.
3. **Selection and fallback.** `internal/version/schemes.Select(kind, override, logger)` picks the
   scheme (PyPI → pep440; npm, crates, Go proxy, GitHub, GitLab, OCI → semver; anything else →
   lexical) and wraps it: a version the scheme rejects is logged at `WARN` and parsed as lexical.
   Fallback versions rank below every version the scheme can parse and are ordered lexically among
   themselves, so a mixed set keeps a total order. Only a blank string is an error.
4. **Bump.** The most significant changed component; between pre-releases of the same release it
   is `prerelease`; a pre-release followed by its release counts as the level that release introduces
   (`2.0.0-rc.1 → 2.0.0` is `major`, as npm's `semver.diff`); `other` when next is not greater than prev.

## Consequences

### Positive

- No new dependency. Exact control over the strictness of each scheme (Masterminds' loose parser
  accepts `1.2` as `1.2.0`, which npm does not, and rejects numbers above 64 bits that SemVer allows).
- Parsers are fuzzed (`make fuzz`); unparseable input never panics and never stops a watch.

### Negative

- We own the SemVer code and its correctness (about 150 lines plus tests).
- Constraint ranges (`constraint: ">=2 <3"`, story for the policy engine) need their own parser or
  can still adopt `Masterminds/semver` constraints then.

### Open questions

- Maven and calver schemes (Phase 1); until then Maven sources use lexical order.
- Conformance against real registry ordering (benchmark 6) is a separate story.

## Alternatives considered

- **`Masterminds/semver/v3`.** Allowed by §6 and by the story. Not chosen for the reasons above;
  it remains an option for constraint parsing.
- **Fail the watch on an unparseable version.** Rejected: one bad tag would block every new release.

## References

- SemVer 2.0.0: https://semver.org
- PyPA version specifiers: https://packaging.python.org/en/latest/specifications/version-specifiers/
- `internal/version/pep440/testdata/NOTICE` for the ported `packaging` test vectors and their licence.
