# ADR 0029: The MCP server uses the official Go SDK, lives in `internal/mcp`, and every tool is a REST call made with the caller's token

- **Status:** Proposed (records the maintainer decisions D8–D13 of 2026-11-10)
- **Date:** 2026-11-13
- **Story:** angelia-dev/angelia#81 (sub-story 81-A; 81-B adds the HTTP transport)

## Context

PRD F10 and [architecture.md §3.1](../architecture.md#31-api) make an MCP server the agent surface of
Angelia, with the tools `list_watches`, `latest_version`, `events_since`, `add_watch` and `ack_event`, over
stdio and streamable HTTP. [§6](../architecture.md#6-language-and-framework-phase-0-assumptions) names
the official MCP Go SDK but not its transitive modules. The tools are a public contract, which belongs in
the spec repository. The REST API already settles authentication, scopes, validation and problem
responses ([ADR 0022](0022-rest-api-in-house-spec-driven-validation.md)); the MCP server must not grow a
second set of rules. The maintainer answered D8–D13 of #81 on 2026-11-10 with the recommendations of the
issue; this ADR records them and the design that follows.

## Decision

1. **D8, dependency.** `github.com/modelcontextprotocol/go-sdk` v1.8.0 (2026-09-04). The modules this adds
   to the binary (`go list -deps ./cmd/angelia`, before and after):

   | Module | Version | Licence | Why |
   | --- | --- | --- | --- |
   | `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | MIT, moving to Apache-2.0 (the LICENSE file describes the transition; new code is Apache-2.0) | direct |
   | `github.com/google/jsonschema-go` | v0.4.3 | MIT | SDK; also direct: argument validation (decision 6) |
   | `github.com/segmentio/encoding` | v0.5.4 | MIT | SDK (JSON) |
   | `github.com/segmentio/asm` | v1.2.1 | **MIT No Attribution (MIT-0)** | segmentio/encoding asks for v1.1.3 (MIT); MVS selects v1.2.1, which `pressly/goose/v3` already required in the module graph. See the open question. |
   | `github.com/yosida95/uritemplate/v3` | v3.0.2 | BSD-3-Clause | SDK (resource templates) |
   | `golang.org/x/oauth2` | v0.37.0 | BSD-3-Clause | SDK `auth`/`oauthex`; v0.37.0 is the version `nicholas-fedor/shoutrrr` already required |
   | `golang.org/x/time` | v0.15.0 | BSD-3-Clause | SDK (rate limit of log notifications) |

   `github.com/golang-jwt/jwt/v5` v5.3.1 (MIT) is in the module graph and `go.sum`, but only the SDK's
   `oauthex` tests import it: it is not linked into the binary and `go.mod` does not list it. Bearer token
   verification stays ours (`internal/api.Authenticator`); nothing calls the SDK's OAuth code.
2. **One package.** `internal/mcp` is the only package that imports the SDK
   (`TestOnlyThisPackageImportsTheSDK` walks the repository). It builds an `*mcp.Server` from an
   `*apiclient.API` and exports `NewServer` and `Serve` (stdio); 81-B mounts the same server over HTTP.
3. **D9, transports and reach.** stdio is `angelia mcp`, a REST client with the CLI's connection
   (`--server`, `--token`, `ANGELIA_SERVER`, `ANGELIA_TOKEN`, the client config file and its `chmod 600`
   check, the `--insecure-http` rule). It never opens the database. Streamable HTTP is `POST /mcp` on the
   `api` role (81-B), outside `/v1/`, with no new role. Either way every tool call is one or more REST calls
   with the caller's token, so authentication, scopes, validation and the access log are the REST API's.
4. **D10, HTTP auth and sessions** (81-B): the existing bearer API tokens, no MCP OAuth; stateless JSON
   responses; a cross-origin `Origin` gets `403`.
5. **D11, contract.** Unprefixed PRD names. The contract is `mcp/tools.v1.json` of the spec repository,
   vendored unchanged to `api/mcp/tools.v1.json` (spec commit `fa3e413`, spec 0.6.0; see
   [api/README.md](../../api/README.md)) and embedded by `api/mcp`. The server takes each tool's input and
   output schemas from the embedded file, as the REST server takes its rules from the embedded OpenAPI
   document; names, titles, descriptions and annotations are in Go. `TestToolsListMatchesContract`
   serialises `tools/list` as a client receives it and requires it to be JSON-equal to the vendored file,
   so a change on either side fails until the other follows on purpose. Every tool sets all four
   annotation hints explicitly: the SDK always emits `readOnlyHint` and `idempotentHint` but leaves out a
   nil `destructiveHint` or `openWorldHint`.
6. **Argument validation.** Tools are registered with the low-level `Server.AddTool`, not the typed
   `mcp.AddTool`, whose schema failure is a free-text tool error. `internal/mcp` validates the arguments
   against the contract's `inputSchema` before the handler runs, with `jsonschema-go`. jsonschema-go stops
   at the first failure and names schema paths, so the validator walks the object levels itself
   (`required`, `properties`, `additionalProperties`, `min`/`maxProperties`, `dependentSchemas`) and hands
   each value to jsonschema-go, then checks the whole schema once more. The result is the problem
   `invalid-arguments` with `{pointer, detail}` items, and no REST call is made.
7. **Results and errors** (spec `docs/mcp.md`). Success: `structuredContent` plus the same JSON in one text
   block. A REST problem response is a tool result with `isError: true` and the problem body, byte for
   byte, as its only text block (extension members such as `required_scope`, `oldest_seq` and `errors`
   included). Failures with no REST problem use the MCP problem types `invalid-arguments`, `no-version`,
   `watch-exists` and `api-unreachable`. JSON-RPC errors are left to the SDK for malformed requests and
   unknown tools.
8. **D12, `latest_version`.** `GET /v1/watches/{name}/versions?yanked=false` (plus `prerelease=false`
   unless `include_prerelease`), greatest first; the first item without `removed_at` wins, at most 10
   pages deep. The tool re-checks `yanked` and `prerelease` on each item.
9. **D13, `add_watch` is create-only.** `GET` the name; `404 not-found` → `PUT`, whose `Angelia-Change`
   decides (`created` or `unchanged` pass, `updated` means another client created a different watch in
   between: `watch-exists`); found → `PUT ?dry_run=true`, where `unchanged` passes and `updated` is
   `watch-exists` with nothing written. Defaults are compared by the server, not by the tool.
10. **Limits.** `list_watches` sends `limit` 50 unless given (1–100), `events_since` 20 (1–100). REST
    responses are read up to 16 MiB.

## Consequences

### Positive

- One rule set: scopes, validation and problems come from the REST API, so the MCP server cannot drift
  from it, and 81-B only has to forward the caller's token.
- The protocol (version negotiation, JSON-RPC framing, transports) is the SDK's job, not ours.
- The golden test makes any change to the tool surface visible and deliberate.

### Negative

- Seven new modules in the binary, including OAuth code we do not call. The binary grows from 36.6 MB to
  39.6 MB (`CGO_ENABLED=0`, linux/amd64).
- `add_watch` has a create race of one round trip until `PUT` supports `If-None-Match: *` (spec
  follow-up); the tool reports it as `watch-exists` rather than `created`.
- `latest_version` may need up to 10 REST calls when many versions were removed upstream.

### Open questions

- **`segmentio/asm` v1.2.1 is MIT-0**, which is not in the AGENTS.md list (Apache-2.0, MIT, BSD, ISC,
  MPL-2.0). MIT-0 is MIT without the attribution condition. It cannot be avoided while `pressly/goose/v3`
  v3.29.0 requires v1.2.1 (MVS). Recommendation: accept MIT-0 for this module and add it to the list.
- The SDK logs to the logger it is given; `angelia mcp` passes a stderr text logger at `info`.

## Alternatives considered

- **In-house JSON-RPC and MCP subset (D8 b).** No dependency, but we would track a moving protocol
  (2025-06-18, 2026-07-28, ...) ourselves.
- **stdio opening the database (D9 b).** No token needed, but SQLite and the same host only, and a second
  path that bypasses scopes and validation.
- **Typed `mcp.AddTool`.** Less code, but its validation error is free text and its schemas come from Go
  types, not from the contract.
- **Schemas in Go.** A second copy of about 600 lines of JSON Schema to keep in step with the contract;
  the golden test already pins what the server serves.

## References

- angelia-dev/angelia#81; spec `mcp/tools.v1.json` and `docs/mcp.md` at `fa3e413` (spec 0.6.0).
- [docs/mcp.md](../mcp.md), [docs/cli.md](../cli.md#mcp-server).
- [ADR 0019](0019-prometheus-metrics-on-the-listen-address.md) and
  [ADR 0020](0020-opentelemetry-traces-link-checks-to-deliveries.md): precedent for listing modules.
