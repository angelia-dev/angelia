# Vendored API contract

These files are copied unchanged from the [spec repository](https://github.com/angelia-dev/spec)
at commit `292e6e2` ("feat(openapi): write-only stored secrets and *_ref references (#10)", the
spec pull request of angelia-dev/angelia#75, merged on the spec repository's `main`). Only `openapi/angelia.v1.yaml` changed since
`836646a` ("feat(openapi): REST API v1 specification draft (#7)"):

| Here | In the spec repository |
| --- | --- |
| `openapi/angelia.v1.yaml` | `openapi/angelia.v1.yaml` |
| `schemas/release/envelope/v1.json` | `schemas/release/envelope/v1.json` |
| `schemas/release/v1.json` | `schemas/release/v1.json` |

The layout mirrors the spec repository so that the relative `$ref` from the OpenAPI document to
the envelope schema resolves. The spec repository is the source of truth: never edit these files
here. To update, copy the files from a newer spec commit, record the commit above, run
`go generate ./internal/api/` and commit the result.

`openapi/embed.go` embeds the document so that the server validates requests and enforces scopes
from the specification itself (`internal/api`). Licence: Apache-2.0 OR CC-BY-4.0, as in the spec
repository.
