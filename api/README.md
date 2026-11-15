# Vendored API contract

These files are copied unchanged from the [spec repository](https://github.com/angelia-dev/spec)
at commit `874e3a8` ("feat(openapi): API tokens under /v1/tokens (spec 0.7.0) (#18)", the spec pull
request of angelia-dev/angelia#72, merged on the spec repository's `main`). Since `fa3e413`
("feat(mcp): MCP tool contract v1 (spec 0.6.0) (#17)"), spec 0.7.0 adds the four `admin` operations
under `/v1/tokens` (`listTokens`, `createToken`, `getToken`, `revokeToken`), the schemas `Token`,
`TokenCreate`, `TokenCreated`, `TokenPage`, `TokenScope` and `TokenActor`, the `ang_` token format in
the `bearerAuth` description, and their examples. The MCP tool contract and the event schemas are
unchanged (no MCP tool manages tokens).

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
