# AGENTS.md — how to work in `angelia`

This repository is built mostly by **AI coding agents** (Claude Code and others) working with a human maintainer. These rules apply to agents and humans alike. `CLAUDE.md` imports this file.

Angelia is the self-hosted release event bus: **detect → normalize → deliver**. Read [docs/architecture.md](docs/architecture.md) and [docs/prd.md](docs/prd.md) before your first story.

## Before you start
1. Work on **one issue at a time**. Only pick issues labelled `agent-ready` whose executor is `Agent` or `Agent + human check` (label `executor:agent` / `executor:agent+check`).
   - `Human-led` (`executor:human-led`): open a **draft PR with a short design note first** (or a Proposed ADR) and wait for the maintainer before writing the implementation.
   - `Human-only` (`executor:human-only`): do not pick it.
2. Check the **Blocked by** list on the issue. Don't start while a blocker is open.
3. Read the issue's **acceptance criteria**, its parent epic, and the doc sections it links.
4. Use the model the issue suggests (project field **Model**) unless the maintainer says otherwise.

## When to stop and ask a human
You decide when a human is needed. Stop, comment on the issue with a precise question (options + your recommendation), add the `human-needed` label and move the item to **Human needed** on the [project board](https://github.com/orgs/angelia-dev/projects) when:
- the issue's **Human needed?** section says so (e.g. naming a public field of the event schema, a go/no-go decision);
- you would change a **public contract**: the CloudEvents schema, the OpenAPI spec, the YAML config format, the plugin ABI, a database migration that drops or rewrites data;
- you would make a **security** decision not settled by an ADR: crypto, secret storage, signing, SSRF rules, auth, sandboxing;
- you would add a **dependency** that is not in [docs/architecture.md §6](docs/architecture.md#6-language-and-framework-phase-0-assumptions), or a licence that is not Apache-2.0/MIT/BSD/ISC/MPL-2.0;
- the acceptance criteria are ambiguous, contradict the docs, or can't be met (e.g. a benchmark misses its target);
- you failed the same CI check three times.
Unclear specs are the top cause of wasted agent work: asking is cheaper than guessing.

## Setup
```
# Go 1.24+ (see go.mod), golangci-lint v2, govulncheck
go mod download
```
Until story "Go module, layout and tooling" is merged, the repository has no Go code yet.

## Commands
```
go build ./...                     # build
go test -race ./...                # tests
go test -race -cover ./...         # tests + coverage (gate: 80% on internal/)
golangci-lint run                  # lint (config in .golangci.yml)
govulncheck ./...                  # known vulnerabilities
go generate ./...                  # sqlc / oapi-codegen, then commit the result
```
CI runs the same checks; the aggregate **CI OK** check must be green before merge.

## Layout (target)
```
cmd/angelia/            main: single binary, `serve --roles=…`, CLI subcommands
internal/config/        config + YAML apply model
internal/store/         SQLite (modernc) / Postgres (pgx) via sqlc; migrations in internal/store/migrations (goose)
internal/scheduler/     due-watch leasing, jitter, backoff
internal/source/<kind>/ Source implementations (pypi, npm, github-release, …)
internal/version/<scheme>/ semver, pep440, maven, calver, lexical
internal/policy/        filters, prerelease mode, constraints, cooldown, baseline
internal/event/         ReleaseEvent, CloudEvents mapping, outbox
internal/dispatch/      delivery leasing, retries, DLQ
internal/sink/<kind>/   Sink implementations (webhook, github-dispatch, shoutrrr, …)
internal/netguard/      SSRF guard, per-host rate limiter, HTTP client
internal/secrets/       encryption at rest
internal/api/           REST (chi + oapi-codegen), SSE, MCP
internal/plugin/        third-party plugin host
bench/                  Phase 0 benchmark harness
docs/adr/               architecture decision records
```

## Working rules
- Branch: `story/<issue-number>-<short-slug>`. One PR per issue; keep it under ~400 changed lines when possible.
- Go: standard library first; `context.Context` first argument on anything doing I/O; errors wrapped with `%w`; `log/slog` only; no global mutable state; no CGO (`CGO_ENABLED=0` must build).
- **Tests first** for behaviour: every acceptance criterion maps to at least one test. Network tests use recorded fixtures (`testdata/`, `httptest`), never live registries in `go test` (live tests go behind the `live` build tag).
- Interfaces from [architecture.md §3](docs/architecture.md#3-component-view) (`Source`, `Sink`) are contracts: changing them needs an ADR.
- Decisions that a story produces go into an ADR in [docs/adr/](docs/adr/README.md) (copy the template).
- Never commit secrets, tokens, personal data or real hostnames other than public registries. Fixtures must be synthetic or public data.
- Do not change `graft.yaml`, `AGENTS.md`, CI permissions or the release pipeline unless the issue asks for it.

## Commits and provenance (Graft)
Conventional Commits (`feat(source/pypi): …`), signed off (`git commit -s`, DCO). Every commit made by an agent also carries Graft trailers in its last paragraph:

```
Graft-Agent: claude-code
Graft-Model: <model id>
Graft-Task: #<issue number>
```

## Review
- Graft posts an intent card and applies [`graft.yaml`](graft.yaml).
- Trivial and small low-risk changes can be approved without a human.
- Changes touching sensitive paths (secrets, netguard, plugin host, webhook signing, API auth, migrations, workflows, Dockerfile, release config), large changes and anything Graft rates `sensitive` need the maintainer.
- Address review comments in new commits; don't force-push over reviewed commits.

## Definition of done
- [ ] All acceptance criteria met and checked in the PR description
- [ ] CI green: the **CI OK** check (build, lint, tests with `-race`, govulncheck)
- [ ] Commits are signed off and carry the Graft trailers
- [ ] Docs, examples and ADRs updated if behaviour or a decision changed
- [ ] No new undeclared network hosts, secrets, dependencies or public-contract changes
- [ ] On the issue, record **actuals**: agent attempts, wall-clock time, whether a human took over. These feed the [estimation](docs/ESTIMATION.md) recalibration.
