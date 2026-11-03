# Product Requirements: Angelia

Status: draft v0.1 (2026-10-30). Owner: Marco. Companion doc: `architecture.md`.

## 1. Problem

Teams and agents need to **react** when something upstream ships: a library on PyPI or npm, a GitHub release, a container tag. Today the options are:

- **Dependency bots** (Renovate, Dependabot) only act on *your own manifests*, and their output is a PR. They can't say "tell my pipeline or agent that X shipped".
- **Self-hosted watchers** each cover one slice: Argus (GitHub and URLs), github-release-monitor (forges only), Diun (containers only), nvchecker (many sources, but a CLI with no notifications).
- **Multi-registry trackers** (NewReleases.io, Release Alert, Release Bell, releases.sh) are **SaaS**, can't be self-hosted, and their payloads are built for humans.

Nobody offers a **self-hosted, multi-ecosystem service that emits a structured, versioned event** that CI and agents can consume reliably.

## 2. Vision

> **The self-hosted release event bus.** Declare what you care about; get one clean, signed, machine-readable event per new version, delivered reliably to your CI, your chat, or your agents.

## 3. Target users

| Persona | Need | Example |
|---|---|---|
| **Platform / DevOps engineer** | Trigger pipelines when upstream ships | New `kubernetes` minor → run compatibility suite via `repository_dispatch` |
| **Agent builder** | Feed agents with fresh, structured upstream changes | Agent polls `events_since` over MCP, reads release notes, opens a migration PR |
| **Homelab / self-hoster** | Know when apps they run have updates, without SaaS | Ping ntfy when Home Assistant or Immich release |
| **Library maintainer** | Watch dependents and dependencies across ecosystems | Test against every new `pydantic` pre-release |
| **Security / supply-chain team** | Enforce cooldowns, track yanks, keep audit trails | Emit only after 72h without a yank; log every event |

## 4. Market data: why build now

**Demand signals**
- The package universe keeps growing: ~12.2M packages and ~165M versions across the 8 major registries (npm ~6.0M, Go ~2.5M, PyPI ~944k, crates.io ~364k), per ecosyste.ms data on 23 Oct 2026 ([safeguard.sh](https://safeguard.sh/resources/blog/open-source-package-ecosystems-compared-size-growth-risk)). Nobody can follow upstream by hand.
- CI is the reaction layer, and it is growing fast: 11.5B GitHub Actions minutes on public projects in 2025 (+35% YoY); 180M+ developers on GitHub ([Octoverse 2025](https://github.blog/news-insights/octoverse/octoverse-a-new-developer-joins-github-every-second-as-ai-leads-typescript-to-1/)).
- Agents are now active in repositories: 1M+ PRs created by Copilot's coding agent in May–Sep 2025, and 1.1M+ public repos import an LLM SDK (+178% YoY) (same source). Agents need **machine-readable triggers**, not emails.
- Interest in generic "watch and notify" is high: changedetection.io has ~35k★ and Diun ~4.9k★, even though neither understands versions across ecosystems.

**Timing signals (supply chain)**
- Sonatype counted ~454,600 new malicious packages in 2025, more than 99% of them on npm, including worm campaigns such as Shai-Hulud that compromised 500+ packages in days ([Sonatype 2026](https://www.sonatype.com/state-of-the-software-supply-chain/2026/open-source-malware)).
- The response is the **cooldown**. Between Sept 2025 and Aug 2026, pnpm, Yarn, Bun, uv, pip, npm, Bundler, Poetry, Deno and Cargo all shipped minimum-release-age settings ([nesbitt.io](https://nesbitt.io/2026/03/04/package-managers-need-to-cool-down.html)). These work at install time only; there is **no event source** that says "this version just passed its cooldown and was not yanked". That is a gap Angelia fills natively.

**Competitive gap** (details in the prior-art report)

| | Self-hosted OSS | Multi-ecosystem | Daemon + reliable delivery | Structured event schema | CI-native sinks | Agent API (MCP / pull) |
|---|---|---|---|---|---|---|
| Argus | ✅ | ❌ (GitHub, Forgejo, URL) | ✅ (no retries/replay) | ❌ | webhook only | ❌ |
| github-release-monitor | ✅ | ❌ (forges) | partial | ❌ | ❌ | ❌ |
| nvchecker | ✅ | ✅ (44) | ❌ (CLI) | JSON logs | ❌ | ❌ |
| Diun | ✅ | ❌ (containers) | ✅ | partial | webhook, script | ❌ |
| Anitya | ✅ (heavy) | ✅ | ✅ (AMQP bus) | Fedora messages | ❌ | ❌ |
| NewReleases.io / Release Alert | ❌ SaaS | ✅ | ✅ | ❌ | webhook | ❌ |
| releases.sh | ❌ SaaS | partial | ✅ | partial | webhook | ✅ |
| **Angelia** | ✅ | ✅ | ✅ | ✅ CloudEvents | ✅ | ✅ |

**Go decision:** the gap is real, and the demand drivers (agents, supply-chain cooldowns, CI automation) all got stronger in 2025–2026. Risk: Argus or nvchecker could add the missing pieces. Mitigation: we compete on the **contract** (event schema, delivery guarantees, agent surface), not on source count. We could even wrap nvchecker for long-tail sources.

## 5. Goals and non-goals

**Goals (v1.0)**
- G1: Watch PyPI, npm, crates.io, GitHub/GitLab releases and tags, OCI registries, Go modules, Maven, and generic HTTP JSON/regex.
- G2: Detect **every** new version (not just "latest changed"), with correct ordering per ecosystem.
- G3: Emit a stable, documented CloudEvents event with purl, bump type, pre-release and yank flags, and notes.
- G4: Deliver at-least-once with retries, dead-letter and replay; signed webhooks.
- G5: First-class CI sinks (GitHub dispatch, GitLab trigger) and chat sinks (Shoutrrr / Apprise).
- G6: Agent surface: MCP server plus a cursor-based pull API plus SSE.
- G7: Runs as one container with SQLite; scales with Postgres.
- G8: Config as code (YAML, `apply`) plus REST API.

**Non-goals**
- Opening PRs or updating manifests (Renovate's job; we trigger it instead).
- Vulnerability scanning (we can *enrich* with OSV later, not replace scanners).
- Hosted SaaS (maybe later, not in scope).
- Watching arbitrary web pages for text changes (changedetection.io's job).

## 6. Functional requirements

| ID | Requirement | Priority |
|---|---|---|
| F1 | CRUD for watches via API, CLI and YAML; labels on watches | Must |
| F2 | Built-in sources: pypi, npm, crates, github-release, github-tag, gitlab-release, oci, go-proxy, maven, http-json, http-regex | Must (first 6 in MVP) |
| F3 | Version schemes: SemVer, PEP 440, Maven, calver, lexical fallback | Must |
| F4 | Policies: include/exclude regex, pre-release mode, constraint ranges, bump filter, cooldown, baseline on first run | Must |
| F5 | Events: `release.published`, `release.yanked`, `release.prerelease`; immutable log with cursor | Must |
| F6 | Subscriptions: filter (source kind, labels, bump, package glob) → sink + template | Must |
| F7 | Sinks: webhook (Standard Webhooks signing), github-dispatch, gitlab-trigger, shoutrrr, apprise, exec | Must |
| F8 | Sinks: NATS, MQTT, Kafka | Should |
| F9 | Delivery retries, DLQ, replay by event or time range | Must |
| F10 | MCP server: `list_watches`, `latest_version`, `events_since`, `add_watch`, `ack_event` | Must |
| F11 | Manifest import: `requirements.txt`, `pyproject.toml`, `package.json`, `go.mod`, `Cargo.toml`, `Dockerfile` | Should |
| F12 | Release notes enrichment (GitHub release body, changelog URL) | Should |
| F13 | Third-party plugins (WASM) with SDK and conformance kit | Should (v1.x) |
| F14 | Web UI: watches, events timeline, deliveries, DLQ | Could (v1.x) |
| F15 | OSV advisory enrichment and `release.advisory` event | Could |
| F16 | Private registries and mirrors (custom base URLs, auth) | Must |

## 7. Non-functional requirements

- **Footprint:** idle RSS < 40 MB; 10k watches on 1 vCPU / 256 MB.
- **Freshness:** P95 time from upstream publish to event < interval + 2 min.
- **Reliability:** no lost events across crashes (transactional outbox); idempotency keys on every delivery.
- **Politeness:** per-host rate limits, ETag/conditional requests, shared fetch for identical targets.
- **Security:** secrets encrypted at rest, SSRF guard, signed releases, SBOM, OpenSSF Scorecard ≥ 7.
- **Operability:** Prometheus metrics, OTel traces, structured logs, `/healthz` and `/readyz`.
- **License:** Apache-2.0 (permissive, patent grant, friendly to companies embedding it). The event schema is also published under CC-BY-4.0.

## 8. Success metrics

- Phase 0: all six benchmarks in `architecture.md` §6 pass.
- 3 months after v0.1 public: 500★, 10 external contributors, 3 community plugins, at least one published "agent reacts to release" example in the wild.
- Time to first event for a new user: < 5 minutes (`docker run` + 1 YAML + 1 webhook URL).

## 9. Roadmap

| Phase | Scope | Exit criteria |
|---|---|---|
| **0. Spike (2–3 weeks)** | Go skeleton; pypi, npm, github-release sources; SQLite; webhook sink; benchmarks; WASM plugin prototype; event schema draft | Benchmarks pass, language and plugin choices confirmed |
| **1. MVP v0.1** | 6 sources, policies, outbox + retries, webhook / github-dispatch / shoutrrr sinks, REST + YAML apply, Docker image, docs site, GitHub Action consumer | One-container install works end-to-end; dogfooded on Marco's own projects |
| **2. v0.5 Agents** | MCP server, SSE, pull API, release notes enrichment, Postgres mode, Helm chart, plugin SDK (WASM) | An agent example repo reacts to events without a public endpoint |
| **3. v1.0** | All sources in F2, manifest import, UI, DLQ replay UI, conformance kit, contrib plugin index, spec v1 frozen | Spec stability promise; 1.0 release |

## 10. Risks

| Risk | Mitigation |
|---|---|
| Upstream rate limits (GitHub especially) | Tokens, ETags, shared fetches, GitHub App auth option |
| Version ordering bugs per ecosystem | Conformance tests against registry ordering of the top 1k packages |
| Scope creep into Renovate or SaaS territory | Non-goals above; "detect → normalize → deliver" as the guiding rule |
| An existing project (Argus, nvchecker) fills the gap first | Lead on the open spec; consider collaboration (nvchecker as a source adapter) |
| Plugin security | WASM sandbox, host allowlist, signed plugins later |

## 11. Name shortlist

The family theme fits horae (Greek mythology): the messenger who announces news. GitHub availability checked on 2026-10-30; "looks free" means the GitHub profile URL returned not-found.

| Name | Meaning | GitHub org options |
|---|---|---|
| **Angelia** ⭐ | Greek spirit of messages and tidings (daughter of Hermes) | `angelia` taken (dormant user); `angelia-dev` and `angelia-io` look free |
| **Pheme** | Greek goddess of fame and news, "she who spreads the word" | `pheme` taken (empty org); `pheme-dev` looks free. Clashes with the PHEME rumour-detection research dataset |
| **Release Beacon** | Descriptive, easy to search | `releasebeacon` looks free |
| **verbeacon** | "version beacon", short and unique | `verbeacon` looks free |

Rejected: Argus (taken by release-argus), Hermes, Iris, Heimdall (crowded), Tidings (`tidings` and `tidings-dev` taken), Nuntius (taken), ReleaseWire (company), Upwatch (taken).

Before deciding, also check PyPI, npm, Docker Hub and domain names (`angelia.dev`), and search for trademarks.
