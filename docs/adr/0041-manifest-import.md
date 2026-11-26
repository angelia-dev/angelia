# ADR 0041: `angelia import` prints a watch file from dependency manifests, offline

- **Status:** Proposed (decisions D60 to D69 answered with the recommendations of the spec of 2026-11-25; the maintainer sets Accepted at review)
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#98

## Context

PRD F11 asks for watches created from the dependencies a project already declares. Today a user writes one
watch per package by hand. [cli.md](../cli.md#apply) already has config as code (`angelia apply`, with
`--prune -l` for removals), and the watch file ([watch-file.md](../watch-file.md)) is the exchange format.
Opening pull requests or updating manifests is a non-goal (PRD "Non-goals"): that stays Renovate's job. No
public contract may change: no CloudEvents field, no OpenAPI operation, no watch-file key, no migration.

## Decision

1. **D60. The command prints a watch file on stdout** (format v1 unchanged) and does nothing else. It is
   applied with `angelia apply -f -` or loaded with `serve --watches`. It makes no network request, needs no
   server and reads no client configuration. An `--apply` flag can be added later without breaking anything.
2. **D61. Names** are `<project>-<kind>-<slug>`: `--project` is required (`^[a-z0-9][a-z0-9-]{0,23}$`), `kind`
   is the source kind and never the instance name, `slug` is the canonical package name with `@` dropped and
   characters outside `[a-z0-9._-]` replaced by `-`. A name over 63 characters is cut to 54 and gets `-` and
   the first 8 hex digits of SHA-256 of kind, NUL and the canonical name. Two dependencies that map to one
   name are an error naming both.
3. **D62. Labels** are the `-l key=value` pairs (default `project=<project>`) plus `dependency: runtime |
   dev | optional | peer`; a package with several types gets the first of runtime, optional, dev, peer. The
   keys are conventions, not schema: labels are free-form user data in spec v1.
4. **D63. Pins and ranges are ignored.** Every watch keeps `baseline: true`. Pins appear in `-v` only.
5. **D64. Private registries** are mapped with `--source KIND=NAME` onto named instances. The import never
   reads `--index-url`, `.npmrc` or `.pypirc` (it warns about index lines). The server validates the instance
   at `apply`; a watch still cannot carry a URL or credential (watch-file D9).
6. **D65. `requirements.txt`** is read by an in-house parser of a documented subset of pip's grammar (comments,
   continuations, `-r` includes nested to depth 8 with cycle detection, PEP 508 requirements with extras,
   specifiers and markers ignored). Constraint files, editable, local path, VCS, URL and `${VAR}` lines are
   skipped with a warning; index options are ignored with one warning per file; `--strict` makes skips errors.
7. **D66. `package.json`** (story 98-B): `dependencies`, `optionalDependencies` and `devDependencies` by
   default, `peerDependencies` with `--include peer`; non-registry specifiers skipped with a warning; `npm:`
   aliases import the real name.
8. **D67. Subscriptions:** watches only by default; `--subscribe SINK` adds `<project>-deps` on an existing
   sink with the owner labels as `filter.labels`. `apply --prune` does not delete it until angelia-dev/spec#8
   adds owner labels to subscriptions.
9. **D68. Policy defaults and the UI:** only what the flags say is set (`--interval`, `--policy FILE`; the
   scheme is never set). No merge with hand edits: `PUT` replaces the whole object, so a watch tuned by hand
   is left out with `--exclude`. The admin UI edits the API-managed rows that `apply` creates.
10. **D69. Scope:** `requirements.txt` and `package.json` only, standard library and `gopkg.in/yaml.v3`.
    `pyproject.toml` and `Cargo.toml` need a TOML parser (a dependency decision), `go.mod` waits for the
    `go-proxy` source (#95) and `Dockerfile` for the `oci` source (#94): follow-up issues under epic #22.

The generator builds the file with the types of `internal/manifest` and checks it with the watch-file parser
before printing, so what it prints is accepted by `apply` and `serve --watches`.

## Consequences

### Positive

- Offline, reviewable in a pull request, testable with golden files; it reuses apply's plan, dry run,
  idempotence and prune.
- No contract change and no dependency.

### Negative

- Labels `project` and `dependency` become a convention that users filter on.
- Hand edits to an imported watch are overwritten by the next import (documented).
- The grammar is a subset: some requirement lines are skipped.

### Open questions

- 98-C to 98-E: TOML parser choice, `go.mod`, `Dockerfile`.
- Owner labels on subscriptions when angelia-dev/spec#8 lands.

## Alternatives considered

- **Direct API calls from `import`** duplicate apply (ordering, dry run, prune, report) and cannot run without a server.
- **Always hash the name:** no collision, but unreadable names in events and `watch list`.
- **A reserved label namespace** (`angelia.dev/project`): collision-proof, but long to type in every prune and filter.
- **Pin in a label, a constraint from a range, or a baseline at the pin:** stale labels, hidden majors, and a
  contract change plus a flood of first events respectively.
- **Shelling out to `pip`, or a PEP 508 library:** not in the image and network I/O; no library is in architecture.md section 6.
- **An import button in the admin UI** needs a new endpoint (OpenAPI change): a later, human-led story.

## References

- Spec of story #98 (2026-11-25), PRD F11, [cli.md](../cli.md#import), [watch-file.md](../watch-file.md),
  [ADR 0035](0035-adopt-watch-file-resources.md).
