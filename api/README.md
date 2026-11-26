# Vendored API contract

These files are copied unchanged from the [spec repository](https://github.com/angelia-dev/spec).
`openapi/angelia.v1.yaml`, `schemas/release/v1.json`, `schemas/release/envelope/v1.json` and `mcp/tools.v1.json` are at commit `fede249`
("fix(release): keep generated clients open to new event types") on the spec repository's branch `story/102-open-event-types`, spec 0.16.0. Spec 0.16.0 opens the event-type set (core angelia-dev/angelia#102, sub-story 102-A, spec docs/api.md item 61): the envelope's `type` is the pattern `^dev\.angelia\.[a-z0-9.]+\.v[0-9]+$` with the release rules kept per release type, `EventRecord.event` is a `oneOf` keyed on `type` with the release envelope as its only branch, `ShortEventType` stays an enum for requests and grows in minor releases, and the MCP `events_since` and the docs say that consumers must ignore events whose type they do not know. It also carries spec 0.14.0 (`ExecSinkConfig` under `SinkConfig.x-angelia-kind-config`, spec docs/api.md item 59), which changes no generated Go code.
Before that, `openapi/angelia.v1.yaml`, `schemas/release/v1.json` and `mcp/tools.v1.json` are at commit `5534e1d`
("feat(openapi): mqtt sink config (0.13.0) (#27)", spec 0.13.0) on the spec repository's `main`. Spec 0.13.0 adds `MqttSinkConfig` under `SinkConfig.x-angelia-kind-config` (spec docs/api.md item 58); the MCP tool contract and the event schemas are unchanged by 0.13.0.
Before that, `openapi/angelia.v1.yaml`, `schemas/release/v1.json` and `mcp/tools.v1.json` are at commit `e013106`
("docs(openapi): http-json and http-regex watches (0.12.0) (#26)", spec 0.12.0) on the spec repository's `main`. Spec 0.12.0 documents the `http-json` and `http-regex` watch coordinates and the `admin` scope needed to create or retarget such a watch (spec docs/api.md item 57), in the OpenAPI document and the MCP `add_watch` tool; no schema rule changes.
Before that, `openapi/angelia.v1.yaml`, `schemas/release/v1.json` and `mcp/tools.v1.json` are at commit `87249d1`
("feat(openapi): gitlab-pipeline-trigger sink config (0.11.0) (#24)", spec 0.11.0) on the spec repository's `main`. Spec 0.11.0 adds `GitlabPipelineTriggerSinkConfig` under `SinkConfig.x-angelia-kind-config` (spec docs/api.md item 56); the MCP tool contract and the event schemas are unchanged by 0.11.0.
Before that, `openapi/angelia.v1.yaml`, `schemas/release/v1.json` and `mcp/tools.v1.json` are at commit `8e98902`
("docs(schemas): list maven among the known version schemes (#25)", spec 0.10.2) on the spec repository's
`main`. Spec 0.10.2 changes descriptions only: `maven` becomes a known value of `data.scheme` and of the
watch `scheme` (OpenAPI and MCP). Spec 0.10.1 (commit `a8250b0`, #22) changed descriptions only too: which
sources fill `notes`, and that its text is untrusted. No schema rule changes in either.
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
