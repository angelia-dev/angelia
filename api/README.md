# Vendored API contract

These files are copied unchanged from the [spec repository](https://github.com/angelia-dev/spec)
at commit `fa3e413` ("feat(mcp): MCP tool contract v1 (spec 0.6.0) (#17)", the spec pull request of
angelia-dev/angelia#81, merged on the spec repository's `main`). Since `423580c` ("feat(openapi):
consumer cursors and ack (spec 0.5.0) (#16)"), spec 0.6.0 adds the MCP tool contract
`mcp/tools.v1.json` (vendored here as `mcp/tools.v1.json`), and `openapi/angelia.v1.yaml` changes only
its `info.version` to 0.6.0. The schemas are unchanged.

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
