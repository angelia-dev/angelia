# Architecture

Status: draft v0.1 (2026-10-30). Project name: **Angelia**, GitHub org `angelia-dev` (see `prd.md` for the name rationale). Everything marked **[P0]** is an assumption we validate in Phase 0.

## 1. What the system does, in one sentence

Watch upstream artifacts (packages, releases, images), turn every new version into an **immutable, normalized release event**, and **deliver** it reliably to subscribers (CI, chat, agents).

The pipeline has three verbs: **detect → normalize → deliver**. Anything that decides *what to do* with a new version (open a PR, run tests, ask an agent) stays out of the core and lives with the consumer.

## 2. Review of the proposed split

The initial proposal was: a core API, verifier batches, an event "batch" that fans out to hooks, and plugins for both verifiers and notifiers. The split of responsibilities is right. Five changes:

| # | Proposal | Challenge | Decision |
|---|---|---|---|
| 1 | Separate components (core, verifier, dispatcher) | Self-hosters want **one container**. Separate services from day one mean a broker, service discovery and 3 images to version. | **One binary, several roles.** `angelia serve --roles=api,scheduler,worker,dispatcher` (default `all`). Same code scales out by running more `worker` or `dispatcher` replicas. Internal boundaries are Go packages with interfaces, not network hops. |
| 2 | Verifier as **batches** over the list | A cron "loop over everything" creates bursts, hits rate limits, and makes one slow registry delay all others. | **Continuous scheduler.** Each watch has `next_check_at`; workers lease due watches from the DB. Per-host token buckets, jitter, backoff. Batching happens *inside* a source plugin when the upstream API supports bulk calls. |
| 3 | Plugin `verify()` returns "new or not" | Pushes semver, pre-release and filter logic into every plugin, so each one does it differently. | **Sources are dumb.** `List(ctx, target, cursor) → []Version + new cursor/ETag`. The core does diff, version parsing, filtering, cooldown and policy, the same way for every ecosystem. |
| 4 | Event batch "prepares all the events to emit" | Mixing the event and its deliveries makes retries and replay hard. | **Event ≠ delivery.** One immutable `ReleaseEvent` row; N `Delivery` rows (one per matching subscription), each with its own retry state. Written in the **same transaction** as the state change (transactional outbox). |
| 5 | Notifier plugin with `notify()` | Correct. Missing: signing, idempotency, templating, and pull-based consumers (agents rarely have a public URL). | **Sinks** implement `Deliver(ctx, event, rendered) → result`. Plus **pull channels** in the core: cursor API, SSE stream, MCP server. |

## 3. Component view

```
                  ┌──────────────────────────── angelia (single binary) ───────────────────────────┐
 YAML / API ─────▶│  API (REST + SSE + MCP)                                                         │
                  │      │ CRUD watches, subscriptions, sinks; query versions; read event stream    │
                  │      ▼                                                                          │
                  │  Store (SQLite | Postgres)  ◀──────────────┐                                    │
                  │      ▲  watches, state, events, deliveries │                                    │
                  │      │                                     │                                    │
                  │  Scheduler ──lease due watches──▶ Workers ─┤──▶ Source plugins ──HTTP──▶ PyPI,  │
                  │                                     │      │      (pypi, npm, github,…)    npm, │
                  │                                     ▼      │                               GH…  │
                  │                         Normalizer + Policy engine                              │
                  │                     (parse version, semver delta, filters,                      │
                  │                      cooldown, dedup) → ReleaseEvent + Deliveries (1 tx)        │
                  │                                                                                 │
                  │  Dispatcher ──lease due deliveries──▶ Sink plugins ──▶ webhook, GH dispatch,    │
                  │                         retries, backoff, DLQ             GitLab, Apprise, NATS │
                  └─────────────────────────────────────────────────────────────────────────────────┘
```

### 3.1 API
- REST + JSON, contract-first with **OpenAPI 3.1**; the CLI and UI are generated clients.
- Resources: `watches`, `versions` (read-only, per watch), `events`, `subscriptions`, `sinks`, `deliveries`, `plugins`.
- `GET /v1/events?after=<cursor>&watch=…&kind=…`: the pull API. Cursors are a monotonic integer sequence assigned in commit order (`event.seq`), so a consumer can resume after downtime without skipping an event; the ULID stays the public event id ([ADR 0008](adr/0008-store-history-rows-and-event-cursor.md)).
- `GET /v1/events/stream`: SSE, same filters.
- **MCP server** (stdio and streamable HTTP) exposing tools such as `list_watches`, `latest_version`, `events_since`, `add_watch`, `ack_event`. This is the agent surface.
- Auth: API tokens with scopes (read / write / admin). OIDC later.
- Config-as-code: `angelia apply -f watches.yaml` is idempotent and upserts via the API (GitOps).

### 3.2 Scheduler and workers
- `target.next_check_at` is computed from `interval` (default 1h) ± jitter and adaptive backoff on errors.
- Leasing: `SELECT … FOR UPDATE SKIP LOCKED` on Postgres; a single-process lease on SQLite. No external queue is required.
- **Per-host rate limiter** (e.g. `api.github.com`: 5,000 req/h with a token, 60 unauthenticated). Conditional requests (`ETag` / `If-None-Match`) are stored per target, in `target.fetch_state_json`, so every watch on that target shares them. A 304 costs almost nothing and on GitHub does not count against the limit.
- Coalescing: 50 watches on the same upstream object (e.g. `pypi:requests` used by 50 subscriptions) mean **one** fetch. Watches reference a shared `target`.

### 3.3 Source plugins (detect)
```go
type Source interface {
    Kind() string                                     // "pypi", "npm", "github-release", "oci", ...
    Validate(target Target) error                     // config check at create time
    List(ctx context.Context, t Target, s FetchState) (Result, error)
}
type Result struct {
    Versions []Version   // raw upstream versions, newest first if known
    State    FetchState  // ETag, Last-Modified, cursor: opaque to the core
    NotModified bool
}
type Version struct {
    Raw         string            // "v2.3.0-rc.1"
    PublishedAt time.Time
    Prerelease  *bool             // upstream flag if available (GitHub, PyPI yanked, npm dist-tags)
    Yanked      bool
    URL         string            // release page
    NotesURL    string
    Metadata    map[string]any    // digest, tarball url, dist-tags, …
}
```
Built-in for v1: `pypi`, `npm`, `crates`, `github-release`, `github-tag`, `gitlab-release`, `oci` (Docker Hub, GHCR, any OCI registry), `go-proxy`, `maven`, `http-json` (URL + JSONPath/jq) and `http-regex`.

### 3.4 Normalizer and policy engine
- Version parsing per scheme: SemVer 2, PEP 440, Maven, calver, and a "string order" fallback. The scheme is chosen by source kind and can be overridden per watch.
- Diff: known-version set vs fetched set gives the **new versions** (not just "latest changed"). This catches backports such as `1.9.4` released after `2.0.0`.
- Policy per watch or subscription: `include` / `exclude` regex, `prerelease: none|only|include`, `constraint: ">=2 <3"`, `bump: [major, minor]`, **`cooldown: 72h`** (emit only once the version is older than N, and drop it if yanked in between), first-run behaviour (`baseline` = record without emitting). Evaluation order and semantics: [policy.md](policy.md), [ADR 0012](adr/0012-policy-evaluation-order-and-cooldown.md).
- Output: `ReleaseEvent` (see §4) and one `Delivery` per matching subscription, in a single DB transaction.

### 3.5 Dispatcher and sink plugins (deliver)
```go
type Sink interface {
    Kind() string                                      // "webhook", "github-dispatch", "apprise", ...
    Validate(cfg SinkConfig) error
    Deliver(ctx context.Context, e ReleaseEvent, r Rendered) (DeliveryResult, error)
}
```
- At-least-once delivery. Each delivery carries `idempotency-key = event.id + subscription.id`.
- Retries with exponential backoff and jitter (default 8 attempts over ~24h), then **dead-letter** state, visible in the API and replayable.
- Webhooks are signed following the **Standard Webhooks** spec (`webhook-id`, `webhook-timestamp`, `webhook-signature` HMAC-SHA256).
- Templating: Go `text/template` over the event JSON for message bodies, with a curated function set, a timeout and an output cap ([templates.md](templates.md), [ADR 0016](adr/0016-message-templates-text-template-bounded.md)). Default payload = CloudEvents JSON.
- Built-in for v1: `webhook`, `github-dispatch` (`repository_dispatch` / `workflow_dispatch`), `gitlab-pipeline-trigger`, `apprise` (via an `apprise-api` sidecar, which gives 100+ services at once), `shoutrrr` (Go-native: Slack, Discord, Telegram, ntfy, Gotify, SMTP, …), `nats`, `mqtt`, `exec`.

### 3.6 Third-party plugins
Built-ins are compiled in. For third-party plugins there are three options, to compare in Phase 0:

| Option | Pros | Cons |
|---|---|---|
| **WASM** (Extism on `wazero`, pure Go) | Sandboxed, any language (Rust, Go, JS, Python via extism-py), single file, no extra process | HTTP must go through host functions; younger ecosystem |
| **gRPC subprocess** (`hashicorp/go-plugin`) | Battle-tested (Terraform, Vault), full language freedom | One process per plugin, versioned protobuf, heavier for self-hosters |
| **exec / JSON-over-stdio** | Trivial: any script works | Slow per call, weak sandboxing |

**[P0] assumption:** WASM via Extism for sources and sinks, with host-provided `http_request` (rate-limited and logged by the core), plus `exec` as an escape hatch. A plugin manifest declares `kind`, `config schema` (JSON Schema) and `allowed_hosts`.

## 4. The event contract (the product)

CloudEvents 1.0, JSON structured mode. The contract lives in its own repo, [angelia-dev/spec](https://github.com/angelia-dev/spec), and is versioned with SemVer (draft, pre-1.0: it may still change in a 0.x release). The three event types share one `data` schema, [`schemas/release/v1.json`](https://github.com/angelia-dev/spec/blob/main/schemas/release/v1.json), inside the envelope profile [`schemas/release/envelope/v1.json`](https://github.com/angelia-dev/spec/blob/main/schemas/release/envelope/v1.json). The field-by-field reference is [`docs/release-event-v1.md`](https://github.com/angelia-dev/spec/blob/main/docs/release-event-v1.md); this section is a summary and the spec wins if they differ.

```json
{
  "specversion": "1.0",
  "id": "01JBX2A7C8D9E0F1G2H3J4K5M6",
  "source": "angelia://angelia/watches/01JBX1Z3M4N5P6Q7R8S9T0V1W2",
  "type": "dev.angelia.release.published.v1",
  "subject": "pypi:requests@2.33.0",
  "time": "2026-10-30T17:00:02.512Z",
  "datacontenttype": "application/json",
  "dataschema": "https://github.com/angelia-dev/spec/schemas/release/v1.json",
  "data": {
    "source": "pypi",
    "package": "requests",
    "version": "2.33.0",
    "normalized_version": "2.33.0",
    "previous_version": "2.32.5",
    "scheme": "pep440",
    "bump": "minor",
    "backport": false,
    "prerelease": false,
    "yanked": false,
    "published_at": "2026-10-27T14:02:11Z",
    "detected_at": "2026-10-30T17:00:00Z",
    "cooldown_satisfied_at": "2026-10-30T14:02:11Z",
    "urls": {
      "release": "https://pypi.org/project/requests/2.33.0/",
      "changelog": "https://github.com/psf/requests/blob/main/HISTORY.md",
      "artifact": null
    },
    "notes": null,
    "watch": {
      "id": "01JBX1Z3M4N5P6Q7R8S9T0V1W2",
      "name": "requests",
      "labels": { "team": "platform", "repo": "api" }
    },
    "purl": "pkg:pypi/requests@2.33.0"
  }
}
```

- **Envelope.** `source` is `angelia://<instance>/watches/<watch_id>`: `<instance>` is the `instance.name` of the [instance configuration](configuration.md) (default `angelia`) and `<watch_id>` is the watch's bare ULID, equal to `data.watch.id`. `id` is a ULID generated once when the event is written; every delivery attempt, retry and replay carries the same id (at-least-once delivery, consumers deduplicate on it). `subject` is `<source kind>:<package>@<version>` with the raw version. `time` is the **emission** time (the end of the check that emitted the event), not `detected_at`: with a cooldown it is later.
- **`data.source`** is the source kind of the watch (`pypi`, `npm`, `github-release`, a plugin's kind, ...). It is not the CloudEvents `source` attribute; in templates write `.data.source`.
- **Versions.** `version` is the raw upstream string; `normalized_version` is the scheme's normalised form; `previous_version` is the greatest earlier version of the same scheme, and `bump` is the change from it (both `null` when there is nothing to compare with). `backport` is `true` when a greater version was already known (`1.9.4` after `2.0.0`).
- **Always present.** Every documented field is always present: a field with no value is `null`, never omitted. For example `notes` is `null` until notes enrichment exists (PRD F12), and `urls.artifact` is `null` for PyPI. `watch.name` is `null` for a watch created through the API without a name; `watch.labels` is `{}` when empty.
- **Open objects.** Producers emit only the documented fields and no CloudEvents extension attributes. Consumers must ignore fields and extension attributes they do not know (a new optional field is a minor change). The values of `data.source`, `scheme` and `notes.format` are open sets; `bump` and `type` are closed enums.
- **Types.** `release.published`, `release.prerelease` and `release.yanked` (one `data` schema, with a few rules that depend on the type, such as `yanked` being true only on `release.yanked`); later `release.advisory` (OSV enrichment). Packages are identified with **purl** so consumers can join with SBOMs, OSV and Renovate.

## 5. Data model (simplified)

```
target(id, kind, coordinates_json, fetch_state_json, last_checked_at, next_check_at, error_count)
watch(id, target_id, policy_json, labels_json, baseline_done)
version(target_id, raw, normalized, published_at, prerelease, yanked, first_seen_at, removed_at)   -- known set, never shrinks (ADR 0010)
event(seq, id ULID, watch_id, type, payload_json, created_at, ref_event_id)            -- immutable log; ref_event_id: the original of a release.yanked (ADR 0018)
subscription(id, filter_json, sink_id, template)
sink(id, kind, config_json_encrypted)
delivery(id, event_id, subscription_id, status, attempts, next_attempt_at, last_error)
pending_release(watch_id, target_id, raw, held_at, release_json)                       -- versions held for cooldown, PK (watch_id, target_id, raw) (ADR 0012)
```
Retention: `event` and `delivery` rows are pruned after N days (default 90). `version` is kept.

Store conventions ([ADR 0008](adr/0008-store-history-rows-and-event-cursor.md)): `event.seq` is the pull-API cursor. History rows (`event`, `delivery`) keep `watch_id` and `subscription_id` without a foreign key, so a watch, subscription or sink can be deleted while its history is retained. Foreign keys remain between config tables and from `delivery` to `event`. Timestamps are unix milliseconds in UTC (`INTEGER` on SQLite, `BIGINT` on Postgres). Delivery status is one of `pending`, `in_flight`, `delivered`, `dead`.

## 6. Language and framework (Phase 0 assumptions)

The workload is **I/O-bound** (thousands of small HTTPS calls per hour, JSON parsing, DB writes). Raw CPU speed is not the bottleneck; memory footprint, concurrency model, single-binary packaging and contributor reach are.

| Criterion | Go | Rust |
|---|---|---|
| Idle RSS for this workload | ~15–30 MB | ~5–15 MB |
| Concurrency for many HTTP polls | goroutines, trivial | tokio, very good, more ceremony |
| Single static binary, multi-arch images | yes (`CGO_ENABLED=0`) | yes (musl), slower builds |
| WASM plugin host | `wazero` (pure Go, no CGO) + Extism SDK | `wasmtime`, best in class |
| Ecosystem fit | Diun, Argus, Shoutrrr, CloudEvents SDK, official MCP Go SDK, River queue, sqlc | Strong but fewer ready-made pieces (no Shoutrrr equivalent) |
| Contributor pool and onboarding | large, low barrier | smaller, higher barrier |
| Iteration speed | fast compiles | slower compiles |

**Recommendation [P0]: Go.** The memory gap (~10–15 MB) does not matter for a self-hosted service, and Go gets us further, faster, with more contributors. Revisit only if Phase 0 benchmarks miss the targets below.

Proposed stack [P0]:
- HTTP: `net/http` + `chi`, OpenAPI via `oapi-codegen` (spec-first).
- Process supervision: `golang.org/x/sync/errgroup` (BSD-3), named by the `serve --roles` story (#45).
- DB: `sqlc` + migrations (`goose`); SQLite through `modernc.org/sqlite` (no CGO) by default, Postgres (`pgx`) for HA. Job leasing on Postgres via `River`, or a small in-house lease table usable on both.
- Versions: `Masterminds/semver` + in-house PEP 440 and Maven comparators (test them against each registry's own ordering). See [ADR 0007](adr/0007-version-schemes-in-house-with-lexical-fallback.md): SemVer is in-house too for now.
- Events: `cloudevents/sdk-go`; event ids are ULIDs generated by the caller with `oklog/ulid/v2` (Apache-2.0). Neither is used yet: the event envelope, ULIDs and purls are in-house ([ADR 0015](adr/0015-release-event-outbox-in-house.md)). MCP: `modelcontextprotocol/go-sdk`.
- Sinks: `nicholas-fedor/shoutrrr` (maintained fork), and `apprise-api` as a sidecar for the long tail.
- Plugins: `extism/go-sdk` on `wazero`.
- Spike-only, not shipped: direct deps `hashicorp/go-plugin` (MPL-2.0), `hashicorp/go-hclog` (MIT) and `google.golang.org/grpc` (Apache-2.0), with their transitive deps (`hashicorp/yamux`, `oklog/run`, `fatih/color`, …; MPL-2.0, Apache-2.0, MIT, BSD; `google.golang.org/protobuf` was already indirect), for the go-plugin prototype of #60 under `bench/plugins/goplugin` only. Nothing under `internal/` or `cmd/` may import them unless [ADR 0013](adr/0013-go-plugin-subprocess-prototype.md) is accepted with go-plugin as the choice.
- Observability: OpenTelemetry traces, Prometheus `/metrics`, `slog` JSON logs.
- UI (later): SvelteKit or React, static build served by the binary. Separate repo.

**Phase 0 benchmarks (pass/fail):**
1. 10,000 watches over 5 ecosystems on 1 vCPU / 256 MB: P95 check lag < 2 min, RSS < 100 MB, CPU < 20%.
2. Idle (100 watches): RSS < 40 MB, binary < 40 MB, image < 50 MB (distroless).
3. GitHub: 2,000 repos on a single token without hitting 429/403 (ETag hit rate > 90%).
4. WASM source plugin round-trip overhead < 5 ms per call vs native.
5. Kill -9 during dispatch: zero lost deliveries, duplicates only within the idempotency key.
6. Version ordering: 0 mismatches against registry ordering on the top 1,000 PyPI and npm packages.

## 7. Deployment

- `docker run ghcr.io/angelia-dev/angelia` with SQLite on a volume: the "homelab" mode.
- Docker Compose with Postgres plus an optional `apprise-api` sidecar.
- Helm chart: `api` and `dispatcher` Deployments, `worker` HPA, Postgres external.
- Air-gapped and proxy support: per-source base URLs (Artifactory, Nexus, private PyPI and npm mirrors).

## 8. Security

- Secrets (tokens, webhook secrets) are encrypted at rest with a key from env or file (age or AES-GCM); they are never returned by the API.
- SSRF protection on `http-*` sources and webhook sinks: deny private ranges by default, allowlist to opt in.
- WASM plugins only reach hosts declared in their manifest.
- Signed releases (cosign), SBOM published, OpenSSF Scorecard. A supply-chain tool has to practice what it preaches.

## 9. Repository layout (GitHub org)

Start with **few repos**. A multi-repo split too early slows down everything in the pre-1.0 phase.

| Repo | Content | When |
|---|---|---|
| `angelia` | Monorepo: server, CLI, built-in sources and sinks, migrations, Dockerfile, compose examples | Phase 0 |
| `spec` | CloudEvents types, JSON Schemas, OpenAPI, versioned and published | Phase 0 |
| `plugin-sdk` | Go + Rust + JS SDKs for WASM plugins, template plugin, conformance test kit | Phase 2 |
| `actions` | GitHub Action `angelia/on-release` (consume events, parse payload) and a reusable workflow | Phase 1 |
| `helm-charts` | Helm chart | Phase 1 |
| `ui` | Web UI | Phase 2 |
| `docs` | Website (Docusaurus/Hugo), docs, blog | Phase 1 |
| `plugins-contrib` | Community plugins registry (index file and CI that runs the conformance kit) | Phase 3 |

## 10. Open questions for Phase 0

1. WASM vs go-plugin: does Extism's HTTP host function cover auth headers, pagination and ETags cleanly?
2. SQLite-only single node: how far does it go? (Target: 10k watches.)
3. Release-notes enrichment: fetch from GitHub releases for PyPI and npm packages that link a repo? Cost vs value.
4. Shoutrrr vs Apprise as the default chat layer: Shoutrrr is native, Apprise has more targets. Probably both.
5. Do we need a hosted "public index" (shared cache of upstream versions) to spare rate limits across instances? Out of scope for v1, noted.
