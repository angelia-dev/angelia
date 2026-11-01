# Benchmark 6: version ordering against registry order

Phase 0 benchmark 6 ([architecture.md §6](../../docs/architecture.md#6-language-and-framework-phase-0-assumptions),
story angelia-dev/angelia#55): our version schemes ([ADR 0007](../../docs/adr/0007-version-schemes-in-house-with-lexical-fallback.md))
must order real version lists exactly as each registry's own tooling does.

```
make bench-ordering            # = go run ./bench/ordering; offline, reads testdata/ only
make bench-ordering-snapshot   # rebuild the snapshots from the live registries
```

CI runs `make bench-ordering` in the `bench-ordering` job, which is part of **CI OK**.
`go test ./bench/...` runs the same comparison too.

## What is compared

| Registry | Versions | Reference order ("oracle") | Our scheme |
|---|---|---|---|
| PyPI | every version of the project, from the Simple API (PEP 691 JSON, PEP 700 `versions`) | `packaging.version.Version` (what pip and pypi.org use) | `schemes.Select("pypi")`: PEP 440 with lexical fallback |
| npm | the version keys of the abbreviated packument | node-semver `semver.compare`, strict (npm picks versions with `semver.rcompare`) | `schemes.Select("npm")`: strict SemVer with lexical fallback |

The snapshot scripts compute the oracle order when they fetch, and store it. Versions of equal
precedence form one group: `1.0` and `1.0.0` in PEP 440, `1.0.0+a` and `1.0.0+b` in SemVer
(build metadata never counts for precedence, as in npm).

For each package the tool reports three kinds of mismatch:

- `rejected`: the oracle accepts a version, our scheme does not (it would fall back to lexical
  and rank below every valid version);
- `accepted`: the oracle rejects a version, our scheme parses it;
- `order`: two versions that are adjacent in the oracle's order compare differently with our
  scheme (`<` expected, got `=` or `>`; or `=` expected inside a group). The versions are then
  shuffled deterministically and sorted with our `Compare`; when the result differs from the
  oracle's order, the report prints both orders around the first difference (`-json` writes
  them in full).

Versions the oracle rejects are not ordered: PyPI has 103 of them in this snapshot (legacy
strings such as `0.1-bulbasaur` or `1.0beta5prerelease`, which pip ≥ 24.1 ignores); our scheme
must reject them too (`accepted` otherwise), and our fallback ranks them below every valid version.

## Result (snapshot of 2026-11-01)

| Registry | Packages | Versions | Mismatches |
|---|---|---|---|
| PyPI (packaging 26.3) | 508 | 59,877 | 0 |
| npm (node-semver 7.7.4) | 421 | 148,745 | 0 |

## Package set

The issue asks for the top 1,000 packages of each registry. Neither download ranking was
reachable when the snapshot was built: the agent sandbox could reach only `pypi.org` and
`registry.npmjs.org`, not `api.npmjs.org` (npm download counts) nor the public "top PyPI
packages" dataset or BigQuery. So the lists in [`packages/`](packages/) are **curated**, not a
ranking:

- the most-downloaded packages of each registry as commonly listed (for example boto3,
  urllib3, requests, numpy; lodash, react, chalk, @types/node), and the dependencies they pull in;
- packages with unusual version histories on purpose: on PyPI legacy and calendar versions
  (setuptools, pytz, certifi, pywin32, paramiko, boto), pre-, post- and dev-releases (django,
  numpy, hypothesis, pillow, traitlets, kubernetes); on npm long pre-release trains and nightly
  or canary builds (typescript, next, react, prisma, @prisma/client, playwright, electron, vue,
  webpack, @angular/core).

Every listed package existed when the snapshot was built (`meta.missing` is empty). To run the
benchmark on a true top-1,000 set, replace the lists (one name per line) and rebuild the
snapshot where the registries are reachable; nothing else changes. Both snapshots together stay
well under 1 MB compressed (about 630 KB).

## Snapshot format and reproducibility

`testdata/<registry>.jsonl.gz` is gzip'd JSON lines. Line 1 is
`{"meta": {"registry", "source", "oracle", "generated", "packages", "versions", "missing"}}`;
each further line is `{"name", "order": [[version, ...], ...], "invalid": [...]}`, sorted by name.
The gzip header has no timestamp, so rebuilding on the same day from unchanged registry data
gives identical bytes (checked when this snapshot was made). Registries only add versions, so a
later rebuild may add versions and packages but should not change the order of existing ones.

Rebuilding needs Python 3 with `packaging` (`pip install packaging==26.3`) and Node.js with npm
(node-semver is taken from npm's bundled copy, or from `SEMVER_PATH`). The scripts use only
those and their standard libraries, and contact only `pypi.org` and `registry.npmjs.org`.

## Known mismatches

[`known-mismatches.json`](known-mismatches.json) lists mismatches the maintainer has accepted, each
with an explanation and an issue reference. A listed mismatch does not fail the run; an
unlisted one does, and so does an entry that no longer matches anything (remove it). The list is
empty: the comparators match both registries on this snapshot.

Do not add an entry, or change a comparator to follow a registry quirk, without the maintainer's
decision (issue #55, "Human needed?").
