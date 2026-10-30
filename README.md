# Angelia

> *Angelia* (ἀγγελία): ancient Greek for "message, news". The spirit of announcements and a daughter of Hermes.

**The self-hosted release event bus.** Angelia watches upstream artifacts (PyPI, npm, crates.io, GitHub/GitLab releases, OCI images, …), detects every new version, and delivers one clean, signed, machine-readable event (CloudEvents) to your CI, your chat, or your AI agents.

Detect → normalize → deliver.

## Status

**Pre-alpha / Phase 0.** Nothing here is usable yet. Part of the [angelia-dev](https://github.com/angelia-dev) organization. Design docs:

- [docs/prd.md](docs/prd.md): problem, market, goals, requirements, roadmap
- [docs/architecture.md](docs/architecture.md): components, event contract, language choice, Phase 0 benchmarks

## Planned repositories (GitHub org `angelia-dev`)

| Repo | Purpose |
|---|---|
| `angelia` | Core server, CLI, built-in sources and sinks |
| `spec` | Event schema (CloudEvents types, JSON Schema) and OpenAPI |
| `actions` | GitHub Action to consume Angelia events |
| `helm-charts` | Kubernetes deployment |
| `docs` | Website and documentation |
| `plugin-sdk` | WASM plugin SDKs and conformance kit (later) |
| `ui` | Web UI (later) |

## License

[Apache-2.0](LICENSE)
