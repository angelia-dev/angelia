# Vendored API contract

These files are copied unchanged from the [spec repository](https://github.com/angelia-dev/spec)
at commit `da249d8` ("feat(openapi): shoutrrr sink config with a whole-URL secret (#14)", the spec
pull request of angelia-dev/angelia#120, merged on the spec repository's `main`). Only `openapi/angelia.v1.yaml` changed since `670a586` ("feat(openapi):
github-dispatch sink config (#12)"): version 0.3.1 documents the github-dispatch input names and
payload size guard (#13), and version 0.4.0 adds `ShoutrrrSinkConfig` to `x-angelia-kind-config` and
the `x-angelia-secret-destination` annotation.

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
