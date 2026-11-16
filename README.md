# Angelia

[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/angelia-dev/angelia/badge)](https://scorecard.dev/viewer/?uri=github.com/angelia-dev/angelia)

> *Angelia* (ἀγγελία): ancient Greek for "message, news". The spirit of announcements and a daughter of Hermes.

**The self-hosted release event bus.** Angelia watches upstream artifacts (PyPI, npm, crates.io, GitHub/GitLab releases, OCI images, …), detects every new version, and delivers one clean, signed, machine-readable event (CloudEvents) to your CI, your chat, or your AI agents.

Detect → normalize → deliver.

## Status

**Installable pre-release (v0.1.x).** Signed binaries and a container image are built by the release pipeline. **Push mode is usable:** describe watches, sinks and subscriptions in a [watch file](docs/watch-file.md) and Angelia delivers signed webhooks, GitHub dispatches and chat messages. **The REST API, the CLI client commands, the pull API, SSE and MCP need an API token** ([ADR 0030](docs/adr/0030-api-tokens.md)): create the first one as in [docs/install.md](docs/install.md#4-create-your-first-api-token). The Postgres store, the pull API and MCP are on `main` and ship with the next release. Start with [docs/install.md](docs/install.md). Part of the [angelia-dev](https://github.com/angelia-dev) organization. Design docs:

- [docs/prd.md](docs/prd.md): problem, market, goals, requirements, roadmap
- [docs/architecture.md](docs/architecture.md): components, event contract, language choice, Phase 0 benchmarks
- [docs/ESTIMATION.md](docs/ESTIMATION.md): how stories are sized and which model an agent uses
- [docs/private-registries.md](docs/private-registries.md): private registries and mirrors (Artifactory, Nexus, Verdaccio)
- [docs/admin-ui.md](docs/admin-ui.md): the admin web UI, its roles, GitHub sign-in and deploy notes
- [docs/adr/](docs/adr/README.md): architecture decision records

Work is tracked on the [Angelia Roadmap](https://github.com/orgs/angelia-dev/projects) board. Contributors, human or AI, start with [AGENTS.md](AGENTS.md).

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

## Run it

Follow [docs/install.md](docs/install.md): install a [release](https://github.com/angelia-dev/angelia/releases)
binary and verify it, run it with SQLite (with a systemd unit), and get a first signed webhook delivered.

Container images and Docker Compose examples are in [deploy/compose/](deploy/compose/README.md):
one container with SQLite on a volume, and a full stack with Postgres. The GitHub Container
Registry package is not public yet, so pulling the image needs a login for now.

## Development

Requires Go 1.27 or newer (the toolchain pinned in [go.mod](go.mod); Go downloads it automatically) and golangci-lint v2.15.0 or newer, built with Go 1.27 or newer. Run these from the repository root (`make help` lists them):

| Command | What it does |
|---|---|
| `make build` | Builds `bin/angelia` with `CGO_ENABLED=0` and injects version, commit and build date via `-ldflags` |
| `make test` | Runs `go test -race ./...` |
| `make lint` | Runs `golangci-lint run` and the gofumpt format check (`golangci-lint fmt --diff`), config in [.golangci.yml](.golangci.yml) |
| `make vuln` | Runs `govulncheck` (pinned version) on all packages; needs access to `vuln.go.dev` |
| `make generate` | Runs `go generate ./...` (sqlc, oapi-codegen); commit the output |
| `make clean` | Removes `bin/` |

Print the build info with `./bin/angelia version`.

## License

[Apache-2.0](LICENSE)
