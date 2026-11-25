# ADR 00xx: The go-proxy source reads the GOPROXY protocol with golang.org/x/mod

- **Status:** Proposed
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#95 (sub-stories 95-A and 95-B)

## Context

Epic #21 asks for a `go-proxy` source: the tagged versions of one Go module, from `proxy.golang.org` or any
server that speaks the [GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol). The code already treats
the kind as known: `schemes.ForSourceKind("go-proxy")` is the coercing SemVer scheme (ADR 0007) and
`internal/event/purl.go` maps it to `pkg:golang/<module path>@<version>` with the case kept. The decisions
D90 to D93 below were accepted by the maintainer on 2026-11-25. D94 (retractions) was accepted on the same day and is recorded below (95-B).

## Decision

**D90, dependency.** Use `golang.org/x/mod` v0.43.0 (BSD-3-Clause, Go team): `module.CheckPath`,
`module.EscapePath`, `module.EscapeVersion`, `module.IsPseudoVersion` and `semver`. 95-B adds `modfile`. They
are the functions `cmd/go` uses for the three rules that must match it exactly (path validity, case
encoding, `retract` syntax). The packages import only the standard library and `x/mod`, so no transitive module is linked (`go mod tidy`
only moves `golang.org/x/mod` in `go.sum` from v0.42.0 to v0.43.0). It is listed in architecture.md
section 6.

**D91, coordinate.** One key, `module`, the module path as in `go.mod`. `Validate` uses `module.CheckPath` and
also refuses a version suffix, a scheme, a leading or trailing `/` and more than 300 characters. There is no
`Canonicalizer`: module paths are case-sensitive, so `BurntSushi/toml` and `burntsushi/toml` are two
modules. The path is escaped with `module.EscapePath` (and versions with `module.EscapeVersion`) only in
request URLs. A wrong-case path gets the proxy's 404, reported as "module not found (module paths are
case-sensitive)".

**D92, what `List` returns.** The lines of `GET {base}/{module}/@v/list` that are full SemVer versions with a
`v` prefix (`+incompatible` included) and not pseudo-versions, de-duplicated, sorted by SemVer precedence
newest first, ties by raw string. For each version new since the cursor (none on the first scan, a baseline),
at most 50 per call, `GET .../@v/{version}.info` gives `Time` as `PublishedAt`; a failed lookup leaves the
version undated with a WARN, a 429 (or a `Retry-After`) or a cancelled context fails the call and the cursor
does not advance. The cursor is JSON with the sorted names reported so far (as `github-tag`). `URL` is the
pkg.go.dev page for the default proxy only. An empty 200 body is a module with no tagged version: no versions
and no error; 404 and 410 are errors. There are no conditional requests (the proxy sends no `ETag` or
`Last-Modified`). Untagged commits (pseudo-versions) are not reported.

**D93, private proxies and credentials.** The default `base_url` is `https://proxy.golang.org`; a named
instance (`kind: go-proxy`) takes any `base_url`, with a path. `go-proxy` joins `credentialKinds` and the
`token_env` case of `sourceClient`, so `auth` and `token_env` go through `registryauth.Wrap` like `npm` and
`pypi` (ADR 0024: host binding, clear-text and proxy rules). A watch selects the instance by name. Not
supported: a `GOPROXY` list, `direct`, `off`, `file://`, `GOPRIVATE`, the checksum database, `.netrc`, and a
`GOPRIVATE`-like refusal list. The leak of a private module name to the public proxy is documented in
private-registries.md instead.

**D94, retractions, deprecation and major-version modules.** Each check also requests `@latest`. When its
version differs from the one in the cursor, `@v/{latest}.mod` is parsed with `modfile.ParseLax` for `retract`
directives (single versions and intervals, `low == high` for a single one) and the `// Deprecated:` comment.
Both are kept in the cursor with the latest version. A listed version inside an interval is `Yanked: true` with
`Metadata["retracted"]` (the rationale, at most 256 bytes); a deprecated module sets `Metadata["deprecated"]`
on every version. A yanked new version never emits, and the retraction of a known version becomes
`release.yanked` through the existing yank path. If `@latest` is a 404, a 410 or a pseudo-version, the latest
is the highest release of the list that is not `+incompatible` (else the highest version). If `@latest` or the
`.mod` cannot be read (5xx), the previous retractions stay and the call logs a WARN, asking again next time;
a `.mod` that cannot be parsed logs a WARN and is not asked for again; a 429 or a cancelled context fails the
call. A major-version module (`.../v2`) is a separate module and needs its own watch.

## Consequences

### Positive

- One new module, from the Go team, reusing the reference implementation of the rules.
- No change to `source.Source`, `Canonicalizer`, the event schema, the OpenAPI spec or the plugin ABI, and no
  migration. The instance configuration grows additively (`go-proxy` takes `token_env` and `auth`).

### Negative

- Asking the public proxy for a private module name discloses it (documented, not prevented).
- Every check costs one more request (`@latest`), and a `.mod` only when the latest version changes.
- A date costs one request per new version; cooldown still counts from detection, because `.info` `Time` is
  set by the publisher (`go-proxy` is not in `trustedPublishedAt`).

### Open questions

- A `GOPRIVATE`-like list on the built-in source can be added later without breaking anything.
- A major-version module (`.../v2`) is a separate watch; a hint for a new major version is a follow-up.

## Alternatives considered

- **In-house path checking, escaping and `go.mod` reader** (about 210 lines and a fuzz test): acceptable if
  the dependency had been refused.
- **A `Canonicalizer` that lower-cases the host part**: would merge modules Go keeps apart.
- **Reporting `@latest` of an untagged module**: would emit an event per commit.
- **No retractions** (D94 b): `go-sqlite3`'s accidental `v2.0.x+incompatible` tags would look like the newest releases forever.
- **Probing `<module>/v<N+1>` for a new major module** (D94 c): mixes two modules in one watch, and its events would carry the wrong `data.package`.
- **No `.info` lookups**: saves a request per new version but loses the date notifications show.

## References

- [GOPROXY protocol](https://go.dev/ref/mod#goproxy-protocol), [module paths](https://go.dev/ref/mod#go-mod-file-module)
- [ADR 0006](0006-source-target-and-fetchstate.md), [ADR 0007](0007-version-schemes-in-house-with-lexical-fallback.md),
  [ADR 0010](0010-diff-engine-known-set-and-baseline.md), [ADR 0021](0021-source-canonical-target.md),
  [ADR 0024](0024-secrets-encrypted-at-rest.md)
