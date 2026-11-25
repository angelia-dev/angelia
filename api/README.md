# Vendored API contract

These files are copied unchanged from the [spec repository](https://github.com/angelia-dev/spec).
`openapi/angelia.v1.yaml`, `schemas/release/v1.json` and `mcp/tools.v1.json` are at commit `9ec8b68`
("docs(schemas): list maven among the known version schemes (spec 0.10.1)") on the spec repository's
branch `story/96-maven-scheme`. Spec 0.10.1 changes descriptions only: `maven` becomes a known value of
`data.scheme` and of the watch `scheme` (OpenAPI and MCP); no schema rule changes.
Before that, spec 0.10.0 (commit `da75b65`, "feat(openapi): adopt watch-file resources with POST
/v1/adoptions (spec 0.10.0) (#21)"), since spec 0.9.0, added `adoptResources` (`POST /v1/adoptions`,
`admin`) with the schemas
`AdoptionObject`, `AdoptionRequest`, `AdoptedObject` and `AdoptionResult`, and the matching `ManagedBy`
description (spec docs/api.md item 55). The MCP tool contract and the event schemas are unchanged by
0.10.0.
Before that, spec 0.9.0 (commit `dbb439e`, "feat(openapi): API gaps found by the admin UI (spec 0.9.0)
(#20)"), since spec 0.8.0, added `order=desc` and `before` to `listEvents` with
`EventPage.next_before`; the `sink`, `since` and `until` filters of `listDeliveries` and
`Delivery.sink`; `listAccess` (`GET /v1/access`, `admin`); `Me.session_id`; `ETag` on `GET` and `PUT`
of watches, sinks and subscriptions with the optional `If-Match` and `If-None-Match: *` headers and the
`precondition-failed` problem (`412`); the `JsonSchema` type of `Plugin.config_schema`;
`previewTemplate` (`POST /v1/template-previews`, `admin`); and corrects the event stream's text on
`EventSource` (spec docs/api.md decisions 47 to 54). The MCP tool contract and the event schemas are
unchanged by 0.9.0.
Before that, spec 0.8.0 (commit `a5d861a`, "feat(openapi): /v1/me and browser sessions under
/v1/sessions (spec 0.8.0) (#19)") added `getMe` (`GET /v1/me`, `read`), the `admin` operations `listSessions`
and `revokeSession` under `/v1/sessions`, the schemas `Me`, `Session`, `SessionPage`, `SessionUser`,
`UiRole` and `PrincipalId`, the `sessionCookie` security scheme (documentation only: every operation
still declares `bearerAuth`), the `csrf` problem, and widens `TokenActor` to a session principal
(`github:<id>`). The other files are at commit `874e3a8` ("feat(openapi): API tokens under /v1/tokens
(spec 0.7.0) (#18)"), unchanged by 0.8.0: the MCP tool contract and the event schemas (no MCP tool
manages sessions).

| Here | In the spec repository |
| --- | --- |
| `openapi/angelia.v1.yaml` | `openapi/angelia.v1.yaml` |
| `schemas/release/envelope/v1.json` | `schemas/release/envelope/v1.json` |
| `schemas/release/v1.json` | `schemas/release/v1.json` |
| `mcp/tools.v1.json` | `mcp/tools.v1.json` |

The layout mirrors the spec repository so that the relative `$ref` from the OpenAPI document to
the envelope schema resolves. The spec repository is the source of truth: never edit these files
here. To update, copy the files from a newer spec commit, record the commit above, run
`go generate ./internal/api/ ./internal/apiclient/` and commit the result.

`openapi/embed.go` embeds the document so that the server validates requests and enforces scopes
from the specification itself (`internal/api`). `mcp/embed.go` embeds the MCP tool contract: the MCP
server (`internal/mcp`) serves its input and output schemas, and a golden test requires the server's
`tools/list` to equal it ([ADR 0029](../docs/adr/0029-mcp-server-go-sdk-rest-client.md)). Licence:
Apache-2.0 OR CC-BY-4.0, as in the spec repository.
