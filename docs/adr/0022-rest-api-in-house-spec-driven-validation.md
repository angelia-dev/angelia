# ADR 0022: REST API server: in-house spec-driven validation and routing

- **Status:** Accepted (2026-11-05, maintainer decision on #189)
- **Date:** 2026-11-04
- **Story:** angelia-dev/angelia#71

## Context

The REST API implements the OpenAPI document of `angelia-dev/spec` (`openapi/angelia.v1.yaml`, with its
accepted decisions in the spec's `docs/api.md`). [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions)
names `net/http` + `chi` and `oapi-codegen` (spec-first). It names no OpenAPI or JSON Schema validation
library. Several needs must be met:

- Every request must be checked against the spec. A `422 validation-failed` lists *every* error, each
  with a JSON Pointer, and the schema errors of a PUT body come together with the errors of the watch
  file's own rules (the spec's errors section, decision 19).
- Authentication (`401`) and the scope (`403`) are decided before a `404` or a validation error
  (decision 23), so the server must know the operation and its scope before any handler runs.
- The responses have a fixed form: timestamps always have milliseconds, and nullable members are
  `null`, never left out.

oapi-codegen v2.8.2 with `chi-server` and `strict-server` gives routing, typed request objects and one
`StrictServerInterface`. Its own body decoding stops at the first type error and answers 400. Its
`embedded-spec` option links `getkin/kin-openapi`, which §6 does not list.

## Decision

1. **The vendored spec drives validation at run time.** `api/openapi/angelia.v1.yaml` is embedded
   (`api/openapi/embed.go`) and parsed once at start (`internal/api/spec.go`). A small in-house
   validator (`internal/api/schema.go`) covers the JSON Schema 2020-12 keywords the spec uses: local
   `$ref`, `type`, `enum`, string, number, object and array keywords, `allOf`, `anyOf`, `oneOf`, and
   `format: date-time`. A spec with a pattern that does not compile fails at start. The validator checks
   query, path and header parameters and JSON bodies before the generated layer runs. For the PUT
   operations of watches, sinks and subscriptions, schema errors are held back and merged with the
   resource's own rules, so one `422` lists both.
2. **Routing and scopes come from the same document.** `ServeHTTP` matches the method and path against
   the spec's operations (literal segments win), reads each operation's `bearerAuth` scope, and decides
   401/403/404/405 before handing the request to the chi router that oapi-codegen generates.
   `TestScopesFromSpec` holds a table of every operation's scope, to catch a spec change.
3. **oapi-codegen generates types and the strict interface only.** `internal/api/oapi-codegen.yaml`
   generates `models`, `chi-server` and `strict-server` without `embedded-spec`. A user template,
   `internal/api/strict-interface.tmpl`, replaces v2.8.2's `strict/strict-interface.tmpl`. It keeps the
   request objects, the response object interfaces and `StrictServerInterface`, and leaves out the
   per-status response types. Every handler answers with the one `response` type of `models.go`, which
   writes the fixed forms above, so those types were dead code (about 1,200 statements).
   The override is pinned to v2.8.2. `generate.go` holds the version, and `TestGeneratorPinned` checks
   that the `go:generate` line, the generated header and the template name the same release. An upgrade
   needs a re-diff of the template against the new release.
4. **Errors are RFC 9457 problems** with the spec's type URIs. They are written by the strict layer's
   error handler from a `*ProblemDetail` error.

## Consequences

### Positive

- No new dependency beyond `chi` and `oapi-codegen/runtime`, which §6 names. kin-openapi and its
  transitive modules stay out of the binary.
- A spec update is a vendored-file change plus `go generate`. Validation, routing and scopes follow it
  without hand-written tables.
- A `422` lists every error, and the wording matches the spec's examples (the contract tests check it).

### Negative

- The validator must be maintained. A keyword the spec starts to use is ignored until it is added. The
  contract tests catch this only for the keywords the spec's examples exercise.
- `format` is checked for `date-time` only. `uri` and `uri-reference` are left to the resource's own
  rules, which are stricter.
- The template override couples the build to the internals of one oapi-codegen release.

### Open questions

- **Event cursor order on Postgres.** `next_after` is the highest `seq` scanned. On SQLite, writes are
  serialized, so `seq` order is commit order, and a cursor never skips an event committed later with a
  smaller `seq`. `TestEventPaginationConcurrentWriters` checks this. On Postgres a sequence value is
  taken at insert, not at commit, so this no-skip promise does not hold yet. The question is the same
  one [ADR 0008](0008-store-history-rows-and-event-cursor.md) leaves open, and it blocks the pull API
  and the stream on Postgres until it is answered (for example, by a commit-ordered cursor or a
  visibility horizon).
- Whether `masked_fields` should list a nested secret as a dotted path (`auth.password`, as now) or as
  a JSON Pointer.

## Alternatives considered

- **kin-openapi (`openapi3filter`).** A complete validator, but §6 does not list it, it brings its own
  transitive modules, and its error texts and pointers differ from the spec's examples.
- **The strict server's own decoding as the only check.** It stops at the first error, answers 400 for
  type errors, and does not check patterns, enums or bounds.
- **Hand-written routes and a scope table.** Duplicates the spec and drifts from it.
- **Using the generated per-status response types.** They encode the generated models with
  `encoding/json` defaults, so `time.Time` values lose the fixed milliseconds, and the generator, not
  the server, decides how a nullable member is written.

## References

- Spec `docs/api.md` decisions 11, 15, 18, 19, 23 and 26; `openapi/angelia.v1.yaml` at 836646a.
- [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions),
  [ADR 0008](0008-store-history-rows-and-event-cursor.md).
- Issue #71.
