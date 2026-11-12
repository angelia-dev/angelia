# Vendored API contract

These files are copied unchanged from the [spec repository](https://github.com/angelia-dev/spec)
at commit `423580c` ("feat(openapi): consumer cursors and ack (spec 0.5.0) (#16)", the spec pull
request of angelia-dev/angelia#79, sub-story 79-A, merged on the spec repository's `main`). Only
`openapi/angelia.v1.yaml` changed since `c47df92` ("docs(api): a watch source may name a
configured source instance (#15)"): version 0.5.0 adds named consumers (`/v1/consumers`,
`/v1/consumers/{name}`, `POST /v1/consumers/{name}/ack`) and the `consumer` parameter of
`GET /v1/events` and `GET /v1/events/stream`. The schemas are unchanged.

| Here | In the spec repository |
| --- | --- |
| `openapi/angelia.v1.yaml` | `openapi/angelia.v1.yaml` |
| `schemas/release/envelope/v1.json` | `schemas/release/envelope/v1.json` |
| `schemas/release/v1.json` | `schemas/release/v1.json` |

The layout mirrors the spec repository so that the relative `$ref` from the OpenAPI document to
the envelope schema resolves. The spec repository is the source of truth: never edit these files
here. To update, copy the files from a newer spec commit, record the commit above, run
`go generate ./internal/api/ ./internal/apiclient/` and commit the result.

`openapi/embed.go` embeds the document so that the server validates requests and enforces scopes
from the specification itself (`internal/api`). Licence: Apache-2.0 OR CC-BY-4.0, as in the spec
repository.
