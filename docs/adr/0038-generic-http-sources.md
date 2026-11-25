# ADR 0038: Generic HTTP sources take their URL from the watch, behind netguard and the admin scope

- **Status:** Proposed
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#97
- **Answers:** the open question of [ADR 0006](0006-source-target-and-fetchstate.md) ("whether `http-json` and `oci` need typed coordinates") for `http-json`: no, flat strings suffice.
- **Amends:** decision D9 of [watch-file.md §7](../watch-file.md#7-decisions-all-approved-as-recommended) (new decision D18 there).

## Context

[architecture.md §3.3](../architecture.md#33-source-plugins-detect) lists two built-in sources for upstreams
that are not a known registry: `http-json` ("URL + JSONPath/jq") and `http-regex`. They are the first sources
whose URL is chosen by the watch rather than by the instance configuration, so any principal that can create a
watch can make the server fetch a URL and copy strings out of the response into `version`, which the API
(`GET /v1/watches/{name}/versions`, MCP `latest_version`) and every event expose.

netguard already refuses loopback, private, link-local and other special ranges unless the operator lists them
in `http.allow_networks`, at URL level per hop and at dial time. But `allow_networks` is one list for all
sources and sinks: an operator who lists `192.168.1.0/24` for a private registry also lets every watch of these
kinds reach every host on that network. A watch is created from the watch file (operator), the REST API with a
`write` token, MCP `add_watch` (`write`) and `angelia apply`.

The decisions D50 to D57 of #97 were accepted by the maintainer as recommended on 2026-11-25.

## Decision

1. **Kinds and coordinates (D51, D54).** `http-json`: `name`, `url`, `jsonpath`, optional `extract`
   (`values`, the default, or `keys`). `http-regex`: `name`, `url`, `regex`. Any other key is refused. The
   coordinates stay a flat string map ([ADR 0006](0006-source-target-and-fetchstate.md)). `name` (1 to 128
   bytes, printable, no white space) is the package identity: `data.package` and the CloudEvents `subject`
   (`http-json:<name>@<version>`) through the existing fallback of `event.Package`; `purl` and `urls.*` stay
   `null`. Both kinds implement `source.Canonicalizer` on the URL only
   ([ADR 0021](0021-source-canonical-target.md)): scheme and host lower-cased, default port removed, an empty
   path written `/`, path escaping and query kept as written.
2. **URL rules (D52)**, `internal/source/internal/generic.ValidateURL`, in `Validate` (no I/O): absolute, at
   most 2048 bytes, `https://`, or `http://` only for a host in `http.allow_http_hosts` (case-insensitive); no
   user information, no fragment, no white space or control character, no IP host in a non-canonical form
   (`netguard.NonCanonicalIP`). Every request goes through the injected netguard client; the source keeps a
   shallow copy of it whose `CheckRedirect` follows at most **5** redirects and refuses a hop to plain
   `http://` outside `allow_http_hosts`, with user information or to an IP host in a non-canonical form, then
   runs the client's own check. The watch
   file and the API cannot widen `allow_networks` or `allow_http_hosts`.
3. **Admin scope (D52).** Through the REST API (and so MCP, whose tools are REST calls), a `PUT
   /v1/watches/{name}` that creates a watch whose source is of kind `http-json` or `http-regex`, or that
   changes the `source` or `coordinates` of a watch when the stored or the new source is of either kind,
   needs the `admin` scope: otherwise `403` with `required_scope: admin`, the shape of `*_ref` keys
   ([ADR 0024 §2](0024-secrets-encrypted-at-rest.md)). Changing only `labels`, `policy`, `scheme` or `interval`
   keeps the `write` scope. The watch file is operator-controlled and unaffected.
4. **Extraction (D50).** `http-json` uses an in-house subset of RFC 9535 JSONPath, standard library only:
   `$`, `.name`, `['name']` / `["name"]` with the RFC escapes, `[n]` and `[-n]`, `[*]`, `.*`, at most 512
   bytes and 32 segments. Filters, `..`, slices, unions and functions are refused with the byte offset. Object
   wildcards yield members in sorted name order, so `List` is deterministic. The JSON is decoded with
   `UseNumber`, so `1.10` stays `1.10`; strings and numbers are versions, other types are skipped. With
   `extract: keys` the selected nodes must be objects and their member names are the versions. `http-regex`
   uses Go `regexp` (RE2, linear time), pattern at most 1024 bytes, compiled in `Validate`. RE2's time is linear
   in the input with a factor of the program size (`(a|aa|aaa){1,999}b` took 4.8 s on 64 KiB in review), and a
   Go `regexp` call cannot be cancelled, so the cost is bounded twice:
   - `Validate` refuses a repetition count above 100 and a program of more than **500** instructions
     (`regexp/syntax`: parse, simplify, compile).
   - `List` matches the body in **segments of at most 64 KiB**, each cut after its last newline (or at 64 KiB
     when a line is longer), and checks the context between segments; a done context fails the check with its
     error. A match never spans two segments, so a version must sit within one line of at most 64 KiB, also
     with `(?s)` or `\n` in the pattern; `^` and `\A` match at the start of every segment. The caps of item 5
     and the de-duplication apply across segments.

   Measured on the worst accepted pattern found, `(a|aa|aaa){1,49}b` (492 instructions), on 2 MiB of `a`:
   about 160 ms per segment, about 5 s for the whole body; with a 200 ms deadline the match stops after about
   330 ms. The race detector slows matching about 30 times.

   No capture group means the whole match, one group means that group, several need a group named `version`.
5. **Caps (D53).** A body over **2 MiB** is refused with an error wrapping `netguard.ErrBodyTooLarge`. Raw
   values that are empty, over 256 bytes, invalid UTF-8 or that contain a control character are skipped and
   counted in a debug log; duplicates keep their first position; the order is the document's. More than
   **1,000** distinct versions, or more than 10,000 extracted values before de-duplication, is an error
   ("narrow the expression"); zero usable values is an error, so the watch's status shows the problem.
6. **Conditional requests (D56).** `If-None-Match` / `If-Modified-Since` through `netguard.Fetch`, and
   `FetchState.Cursor` = `sha256:<hex>` of the extracted values joined by `\n`; a 200 whose list hashes to the
   incoming cursor is `NotModified`.
7. **Scheme (D55).** The default stays `lexical` (`schemes.ForSourceKind`); users set `scheme:` on the watch.
8. **No instance keys, no authentication (D57).** `sources.http-json` and `sources.http-regex` accept no key,
   and a named instance of either kind is refused, by `config check` and `serve`. Watch-chosen URLs are
   public-only until a follow-up designs a credential bound to hosts.

The contract test suite gains `sourcetest.Fixtures.ServerURLPlaceholder`, replaced by the fake server's URL in
every coordinate value, because the server URL is inside the target. `source.Source` does not change.

## Consequences

### Positive

- Any JSON endpoint or HTML page becomes watchable without a plugin, with the same SSRF guard, limiter and
  retries as every other source.
- A `write` token (a CI bot, a prompt-injected agent using MCP) cannot turn the server into a read oracle for
  hosts on the operator's allowed networks.
- The JSONPath subset is a strict subset of RFC 9535, so a later full implementation keeps every watch valid.
- Endpoints that send no validators still skip the diff transaction when nothing changed.

### Negative

- Creating these watches through the API needs an admin token, also for `angelia apply` of a file with such
  watches.
- `name` is part of the target: two watches of one URL with different names are two targets and two fetches.
- No per-version metadata (`PublishedAt`, prerelease flag, release URL) and no authenticated upstreams.
- Under the default `lexical` scheme `previous_version` and `bump` are null and `constraint` never matches.

### Open questions

- Authentication bound to hosts (D57 b), relative expressions for per-version metadata, and a `jq` key are
  follow-ups.
- Whether operators want a per-host opt-in for these kinds (`sources.http-json.allowed_hosts`, D52 c).

## Alternatives considered

- **`theory/jsonpath` (MIT, full RFC 9535).** A pre-1.0 dependency with one maintainer for selectors the PRD
  use cases do not need; it stays the fallback if filters are wanted.
- **jq through `gojq`, JMESPath.** jq brings several transitive modules and programs that can loop; both
  JMESPath implementations are unmaintained.
- **No admin rule (D52 b).** Leaves the read oracle open on allowed internal networks.
- **Per-host opt-in in the instance config (D52 c), or URLs only in the instance config (D52 d).** Stronger or
  more literal to watch-file D9, but every new upstream needs a config edit and, for (d), a restart.
- **Truncating at 1,000 versions, empty result on zero matches (D53 b).** The diff engine assumes a complete
  list ([ADR 0010](0010-diff-engine-known-set-and-baseline.md)), and a silent empty result reads as "no new
  release".
- **Default scheme `semver` (D55 b), host and path as package (D54 b).** A guess for an unknown upstream; a
  package that leaks internal host names and changes when the URL moves.

## References

- Issue angelia-dev/angelia#97 (decisions D50–D57), epic angelia-dev/angelia#21.
- [docs/generic-sources.md](../generic-sources.md), [internal/source/README.md](../../internal/source/README.md).
- RFC 9535 (JSONPath), sections 2.3.1 to 2.3.4 and 2.5.
