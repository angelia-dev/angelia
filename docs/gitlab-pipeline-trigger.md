# GitLab pipeline trigger sink

The `gitlab-pipeline-trigger` sink starts a GitLab CI/CD pipeline when Angelia sees a release, on gitlab.com or on a
self-managed GitLab. It is the GitLab twin of the [GitHub dispatch sink](github-dispatch.md): one POST per
delivery to the project's pipeline trigger endpoint, with the release as pipeline **variables** or pipeline
**inputs**.

Configuration keys: [watch-file.md §2.4](watch-file.md#24-sinks). The contract is `GitlabPipelineTriggerSinkConfig`
in the [API specification](https://github.com/angelia-dev/spec/blob/main/docs/api.md) (decision 56).

## Setup

1. Create a **pipeline trigger token** in the target project: *Settings > CI/CD > Pipeline trigger tokens*. A trigger
   token can only start pipelines in its own project and acts with the permissions of the user who created it (a
   Maintainer or Owner), which is far less than a personal or project access token with the `api` scope.
2. Give the token to Angelia without putting it in a file: in an environment variable of the `angelia` process
   (`token_env`), or stored in the instance, encrypted at rest (`token_ref`, see
   [Stored secrets](configuration.md#stored-secrets)). Exactly one of the two. A sink created through the API
   may name only a variable listed in [`api.allowed_env`](configuration.md).

   ```
   angelia secret set gitlab-trigger-token --from-file - --host gitlab.example.com
   ```

3. Declare the sink and a subscription:

   ```yaml
   sinks:
     - name: build-on-release
       kind: gitlab-pipeline-trigger
       config:
         project: platform/deploys/app      # a full path, or a numeric project id
         ref: main                          # the branch or tag the pipeline runs on
         token_ref: gitlab-trigger-token
         # base_url: https://gitlab.com      # the default
   subscriptions:
     - name: requests-to-ci
       sink: build-on-release
   ```

The sink calls `POST {base_url}/api/v4/projects/{project}/trigger/pipeline`. The token is sent **only in the JSON
body** of that request, never in the URL, a header, a log line or an error, and only to `base_url`. A stored token
bound with `--host` is checked against that host before every delivery, and the sink follows no redirect.

`base_url` is the instance root (`https://gitlab.example.com`), or with a relative URL root
(`https://example.com/gitlab`). A trailing `/api/v4` is accepted and ignored. It must be `https://`; a plain
`http://` URL is delivered only to a host listed in `http.allow_http_hosts` ([configuration.md](configuration.md)),
checked at every delivery, and `config check` warns that the token then travels in cleartext. A self-managed
GitLab on a private network also needs `http.allow_networks`, as for any sink.

## What GitLab receives

`ref` is a literal, not a template.

### Default variables

When neither `variables` nor `inputs` is set (an empty map counts as unset), the pipeline gets these variables (a `null` field is the empty
string):

| Variable | Value |
|---|---|
| `ANGELIA_EVENT_ID` | the event id (a ULID), the same on every attempt and replay |
| `ANGELIA_EVENT_TYPE` | the short event type: `release.published`, `release.prerelease` or `release.yanked` |
| `ANGELIA_SOURCE` | the source kind (`pypi`, `npm`, `github-release`, ...) |
| `ANGELIA_PACKAGE` | the package or repository |
| `ANGELIA_VERSION` | the version as the registry wrote it |
| `ANGELIA_PREVIOUS_VERSION` | the previous version, or empty when unknown |
| `ANGELIA_BUMP` | `major`, `minor`, `patch` or empty |
| `ANGELIA_PURL` | the package URL, or empty |
| `ANGELIA_RELEASE_URL` | the link to the release, or empty |
| `ANGELIA_WATCH` | the name of the watch (its id if it has no name) |
| `ANGELIA_IDEMPOTENCY_KEY` | `<event id>:<subscription id>`, the same on every attempt and replay |
| `ANGELIA_TRUNCATED` | `true`, and only present when a value was cut (below) |

The subscription's own `template` does not apply to this sink; `config check` warns when one is set.

### `variables` and `inputs`

Set `variables` and/or `inputs` to choose exactly what the pipeline gets. Each value is a [template](templates.md)
rendered with the same context as a subscription template, to a string:

```yaml
config:
  project: platform/deploys/app
  ref: main
  token_env: GITLAB_TRIGGER_TOKEN
  inputs:
    package: '{{.data.package}}'
    version: '{{.data.version}}'
    event_id: '{{.id}}'
  variables:
    RELEASE_URL: '{{with .data.urls.release}}{{.}}{{end}}'
```

When either map is set, **only** those entries are sent: the `ANGELIA_*` variables are not added. At most 50 entries
each; a variable name matches `^[A-Za-z0-9_]{1,255}$`, an input name `^[A-Za-z0-9_-]{1,255}$`, and neither may end
in `_env` or `_ref` (Angelia reads every such key of a sink configuration as the name of a variable or of a stored
secret). A value is a template of at most 4096 bytes.

Every value is a string. An input declared `type: number` or `type: boolean` in the pipeline is refused by GitLab
with `400`; declare it as a string input.

### Truncation

Every value, default or configured, is cut to at most **2048 bytes**, at a UTF-8 character boundary. With the
default variables `ANGELIA_TRUNCATED=true` is added when any was cut; configured values are cut with no marker.
Real release data is far below this limit.

### Variables or inputs on gitlab.com

**Pipeline variables are restricted by default on new gitlab.com projects**: since GitLab 17.7 the project setting
*Minimum role to use pipeline variables* is `no_one_allowed` for new projects in new namespaces on gitlab.com
(`maintainer` on self-managed). A trigger that sets variables then fails with `400 Insufficient permissions to set
pipeline variables`, which Angelia reports with a hint. GitLab recommends **inputs** instead, and inputs are the
better default there: configure `inputs` (GitLab 18.1 and later; every input must be declared in the pipeline's
`spec:inputs`), or lower the project's minimum role. On a self-managed instance the default variables usually work
as they are.

## Example pipeline

`.gitlab-ci.yml` of the target project, run by every trigger and reading inputs:

```yaml
spec:
  inputs:
    package:
      default: ''
    version:
      default: ''
---
workflow:
  rules:
    - if: $CI_PIPELINE_SOURCE == "trigger"

react:
  script:
    - echo "release $[[ inputs.package ]] $[[ inputs.version ]]"
```

With the default variables instead, read them as ordinary CI variables:

```yaml
react:
  rules:
    - if: $CI_PIPELINE_SOURCE == "trigger"
  script:
    - echo "$ANGELIA_PACKAGE $ANGELIA_VERSION from $ANGELIA_SOURCE (previous $ANGELIA_PREVIOUS_VERSION, bump $ANGELIA_BUMP)"
    - echo "release $ANGELIA_RELEASE_URL, event $ANGELIA_EVENT_ID of watch $ANGELIA_WATCH"
```

`TRIGGER_PAYLOAD`, a file variable GitLab sets in triggered pipelines, holds the request's `variables` and `inputs`.

Do not paste event values into a shell line in a real pipeline: they come from a registry, and an attacker who controls
a package name or version controls them. Quote the variable (`"$ANGELIA_VERSION"`) and never `eval` it.

## Deduplication

GitLab has no idempotency key. A delivery is one request, and a retry after a timeout that GitLab had already
accepted creates a **second pipeline**. When a pipeline must run once per release, deduplicate on
`ANGELIA_IDEMPOTENCY_KEY` (or on an input that carries the event id, such as `event_id: '{{.id}}'`, which is the same on every attempt and replay),
for example by skipping the job when a pipeline for that key already ran.

## Responses and retries

| GitLab answers | Angelia |
|---|---|
| `201` (any `2xx`) | delivered |
| `401` or `403` | **permanent**: the trigger token is not accepted or lacks access |
| `404` | **permanent**: project not found, or a token without access |
| `400` | **permanent**: GitLab refused the ref, variables or inputs. When the first 4 KiB of the body say that setting pipeline variables is not permitted, the hint says to use `inputs` or lower the project's minimum role (the body is searched, never stored) |
| any other `4xx` except `408` and `429`, such as `422` | **permanent** |
| `408`, `429`, `5xx` | retried with backoff, not before the larger of `Retry-After` and `RateLimit-Reset` (a Unix time, at most one hour ahead) |
| a redirect | **permanent**: the sink follows none; set `base_url` to the final host |
| a timeout, a reset, a DNS or TLS failure | retried |
| a destination refused by the SSRF guard | **permanent**; `last_error` names the host only |

`last_error` names the status and a hint, never the token, the project path or the response body
(`gitlab-pipeline-trigger: https://gitlab.com: HTTP 404 Not Found (project not found or token without access)`).
A permanent failure is a dead delivery: fix the cause, then [replay](cli.md) it.

The watch file and the API both write the defaults into the stored configuration (`base_url: https://gitlab.com`, `timeout: 10s`), so a
host-bound `token_ref` is resolved for the host the sink really sends to; a stored sink with no `base_url` has no
destination, and its bound token is refused and retried.

An unset `token_env` variable, a stored token that is missing or bound to another host, or a token with white
space is retried: the operator fixes it by setting the value. A template that fails to render is permanent; the error names the failing variable or input, never the template text.
