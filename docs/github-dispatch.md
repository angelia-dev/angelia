# GitHub dispatch sink

The `github-dispatch` sink starts a GitHub Actions workflow when Angelia sees a release. It calls one of two
GitHub APIs in one repository:

- `repository_dispatch` (the default) sends an event your workflow listens for with
  `on: repository_dispatch`;
- `workflow_dispatch` runs one named workflow on a branch or tag, with inputs.

Configuration keys: [watch-file.md §2.4](watch-file.md#24-sinks). The contract is `GithubDispatchSinkConfig` in the
[API specification](https://github.com/angelia-dev/spec/blob/main/docs/api.md) (decision 38).

## Setup

1. Create a token. A fine-grained personal access token or a GitHub App installation token works. It needs
   `contents: write` on the target repository for `repository_dispatch`, and `actions: write` for
   `workflow_dispatch`. Angelia does not mint installation tokens from an App key.
2. Give the token to Angelia without putting it in a file: in an environment variable of the `angelia` process
   (`token_env`), or stored in the instance, encrypted at rest (`token_ref`, see
   [Stored secrets](configuration.md#stored-secrets)). Exactly one of the two. A sink created through the API
   may name only a variable listed in [`api.allowed_env`](configuration.md).

   ```
   angelia secret set gh-dispatch-token --from-file token.txt --host api.github.com
   ```

3. Declare the sink and a subscription:

   ```yaml
   sinks:
     - name: build-on-release
       kind: github-dispatch
       config:
         repo: octo-org/octo-app
         token_ref: gh-dispatch-token
         # event_type: angelia.release.published     # the default
   subscriptions:
     - name: requests-to-ci
       sink: build-on-release
   ```

The token is sent only to `api_base_url`, over `https://`; a stored token bound with `--host` is checked against
that host before every delivery, and the sink follows no redirect. For GitHub Enterprise Server set
`api_base_url: https://ghe.example.com/api/v3` (and bind the token to `ghe.example.com`). A private
GitHub Enterprise address also needs `http.allow_networks` ([configuration.md](configuration.md)).

## What GitHub receives

### repository_dispatch

`event_type` is a [template](templates.md) (default `angelia.release.published`) rendered with the same context as a
subscription template, so `event_type: 'release.{{.data.source}}'` gives `release.pypi`. The result must be 1 to
100 characters. `client_payload` is a **fixed, flat subset** of the event, never the whole of it:

| Key | Value |
|---|---|
| `event_id` | the event id (a ULID), the same on every attempt and replay |
| `source` | the source kind (`pypi`, `npm`, `github-release`, ...) |
| `package` | the package or repository |
| `version` | the version as the registry wrote it |
| `previous_version` | the previous version, or `null` when unknown |
| `bump` | `major`, `minor`, `patch` or `null` |
| `purl` | the package URL, or `null` |
| `release_url` | the link to the release, or `null` |
| `watch` | the name of the watch (its id if it has no name) |
| `truncated` | `true`, and only present when a value was cut (below) |

That is at most 10 top-level keys, GitHub's limit. The subscription's own `template` does not apply to this sink.

### workflow_dispatch

`mode: workflow_dispatch` needs `workflow` (a file name such as `release.yml`, or its numeric id) and `ref` (a
branch or tag), and takes `inputs`: each value is a template, rendered to a string.

```yaml
config:
  mode: workflow_dispatch
  repo: octo-org/octo-app
  token_env: GH_DISPATCH_TOKEN
  workflow: bump.yml
  ref: main
  inputs:
    package: '{{.data.package}}'
    version: '{{.data.version}}'
```

GitHub accepts at most 25 inputs. The workflow must declare every input under `on.workflow_dispatch.inputs`, or
GitHub answers `422`. An input name must not end in `_env` or `_ref`: Angelia reads every such key of a sink
configuration as the name of a variable or of a stored secret.

### Truncation

Payloads are cut, deterministically, so that GitHub never refuses them for size:

- Every string of `client_payload` is cut to at most **2048 bytes**, at a UTF-8 character boundary. If any string
  was cut, `truncated` is `true`; otherwise the key is absent.
- Each value of `inputs` is cut to at most **1024 bytes**, at a UTF-8 boundary, with no marker: inputs are plain
  strings.
- If escaping made the JSON body pass 60000 bytes (control characters take six each), the strings are cut further
  to half, again and again, until it fits. GitHub's limit is 65535 characters.

Real release data is far below these limits; they matter for hostile or broken registry data.

## Example workflow

This workflow runs for every release Angelia reports and reads the payload:

```yaml
name: react-to-release
on:
  repository_dispatch:
    types: [angelia.release.published]

jobs:
  react:
    runs-on: ubuntu-latest
    steps:
      - name: Show the release
        run: |
          echo "${{ github.event.client_payload.package }} ${{ github.event.client_payload.version }}"
          echo "from ${{ github.event.client_payload.source }}, previous ${{ github.event.client_payload.previous_version }}"
          echo "bump: ${{ github.event.client_payload.bump }}"
          echo "release: ${{ github.event.client_payload.release_url }}"
          echo "event ${{ github.event.client_payload.event_id }} of watch ${{ github.event.client_payload.watch }}"
```

Do not paste `client_payload` values straight into a shell script in a real workflow: they come from a registry
and an attacker who controls a package name or version controls them. Pass them through `env:` and quote the
variable, as in
[GitHub's guidance on script injection](https://docs.github.com/en/actions/security-for-github-actions/security-guides/security-hardening-for-github-actions#understanding-the-risk-of-script-injections):

```yaml
      - name: Bump
        env:
          PACKAGE: ${{ github.event.client_payload.package }}
          VERSION: ${{ github.event.client_payload.version }}
        run: ./scripts/bump.sh "$PACKAGE" "$VERSION"
```

The same payload keys are available as `github.event.client_payload.*` only for `repository_dispatch`. With
`workflow_dispatch` the inputs are `github.event.inputs.NAME` (or `inputs.NAME`).

GitHub only runs a `repository_dispatch` workflow from the default branch of the repository.

## Responses and retries

| GitHub answers | Angelia |
|---|---|
| `204` | delivered (both modes) |
| `404` (repository, workflow, or a token without access) and `422` (invalid payload or inputs) | **permanent**: the delivery is dead at once |
| `5xx` | retried with backoff |
| `403` or `429` caused by a rate limit (`Retry-After`, or `x-ratelimit-remaining: 0`) | retried, not before `Retry-After` or `x-ratelimit-reset` (at most one hour ahead) |
| any other `4xx`, such as `401` (bad token) or a plain `403` (missing permission) | **permanent** |
| a redirect | **permanent**: the sink follows none; set `api_base_url` to the final host |
| a timeout, a reset, a DNS or TLS failure | retried |

`last_error` names the status and a hint, never the token or the response body (`github-dispatch:
https://api.github.com: HTTP 404 Not Found (repository, workflow or token without access)`). A `404` often means the
token does not reach the repository; a `422` on `workflow_dispatch` usually means an input the workflow does not
declare, or a `ref` that does not exist. A permanent failure is a dead delivery: fix the cause, then
[replay](cli.md) it.

An unset `token_env` variable, a stored token that is missing or bound to another host, or a token with white
space is retried: the operator fixes it by setting the value.

A delivery is one request: `repository_dispatch` has no idempotency key, so a retry after a timeout can start the
workflow twice. Use `client_payload.event_id` to deduplicate in the workflow when that matters.
