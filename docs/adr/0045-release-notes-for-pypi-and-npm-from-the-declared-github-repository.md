# ADR 0045: Release notes for PyPI and npm from the declared GitHub repository

- **Status:** Proposed (D83 and D84 are for the maintainer, who accepted the recommended answers on 2026-11-25; the
  live measurement below is still to be run)
- **Date:** 2026-11-26
- **Story:** angelia-dev/angelia#84
- **Builds on:** [ADR 0036](0036-release-notes-on-source-version.md) (`source.Version.Notes`, `source.NewNotes`)

## Context

Notes exist for `github-release` and `gitlab-release` since ADR 0036. PyPI and npm events still carry
`notes: null`, and `urls.changelog` is `null` for both, although almost every popular package names its source
repository in the metadata the sources already download ([architecture.md §10, Q3](../architecture.md#10-open-questions-for-phase-0),
PRD F12). The #82 agent reads `data.notes.body` and needs the repository to open a migration pull request against.

Two facts shape the design. Events are immutable, so notes must be known before `event.Commit` writes them. And
`event.Commit` decides which versions are new inside one store transaction, where a network call would hold the
write lock. Package metadata is written by the publisher, so any package can name any repository.

## Decision

The spec's decisions D80 to D84, with the recommended answers (accepted 2026-11-25).

- **D80. Discovery costs no request.** `pypi` decodes `info.project_urls` and `info.home_page` from the JSON
  response it already fetches; `npm` decodes the top-level `repository` of the full packument. `repolink.Parse`
  normalizes a link to `https://host/owner/name` for `github.com` and `gitlab.com` only (`https`, `http`, `git`,
  `git+https`, `git+ssh`, `ssh`, `git@host:path`, `github:` and `gitlab:` shorthands, and for npm a bare
  `owner/repo`); userinfo other than the ssh user `git`, ports, other hosts, one-segment paths and `..` are
  refused, `.git`, trailing slashes, deeper paths, queries and fragments are dropped (`FuzzParse`: never panics,
  the URL parses back to the same `Repo`). The source keeps the result in `Metadata["repository_url"]` (and npm's
  `directory` in `repository_directory`) on **every** version of the check: the registries describe the latest
  release only, and the link is applied as declared. PyPI labels are compared after PEP 753 normalization;
  repository precedence `source`, `sourcecode`, `repository`, `code`, `github`, `gitlab`, `homepage`, `home`,
  then any other label that parses, then `home_page`; a changelog (`changelog`, `releasenotes`, `changes`,
  `history`, `whatsnew`, `news`; an absolute `https` URL of any host) becomes `NotesURL` of the **newest**
  version only. The Simple API and the abbreviated packument have no links, so those watches get none.
- **D81. One request per check that finds new versions.** `detect.Checker` has an optional `Notes` step
  between `List` and `event.Commit`, outside the transaction. It runs only for a `200` (not a `304`), only when
  the target already has a known set (read with `ListVersions`; the first check is the baseline and emits
  nothing), and only for versions that are new to the known set, not yanked and among the 5 newest. The filler
  sends **one** `GET /repos/{owner}/{repo}/releases?per_page=30` and matches all of them. The step has a 15 s
  deadline, so `netguard`'s limiter refuses a request whose slot would pass it instead of making the check wait
  (an empty anonymous bucket costs the check nothing). No ETag is kept. A failure never fails the check: the
  event goes out with `notes: null` and `urls.changelog` as the source set it. Requests per release: 1 (plus 1
  public-repository check per repository per day with a token).
- **D82. Exact tag match.** For version `v` the tags tried are `v`, `v{v}`, then for every package name `n`:
  `n@v`, `n-v{v}`, `n-{v}`, `n_v{v}`. `n` is the npm name (scoped and bare) or the PyPI project name (as
  `info.name` reports it and PEP 503-normalized). Drafts are skipped, the first match wins, only the first
  30 releases are searched. A wrong body is worse than none: `foo-v1.2.0` never matches package `baz`, `1.2.30`
  never matches `1.2.3`. The name forms are tried for every package, not only when npm gives a `directory`
  (PyPI has no such field, and a tag is matched exactly, so trying them is harmless). A matched release gives
  `source.NewNotes("markdown", body)` and `NotesURL` = its `html_url`, which overrides a declared changelog (the
  full text is there); an empty body gives `Notes == nil` and keeps `NotesURL`. A version whose source already
  set `Notes` is not touched (ADR 0036, D39).
- **D83. `urls.repository` in the event.** A fourth required, nullable key of `urls` (spec 0.15.0): `pypi` and
  `npm` from `repository_url`, `github-release` and `github-tag` from the repository coordinate, `gitlab-release`
  from the project path, the forge kinds on the public base only (a GitHub Enterprise or self-managed GitLab
  base gives `null`: the public forge's page would be wrong); every other kind, plugins included, gives `null`.
  It is "as declared, not verified". `gitlab.com` links from PyPI and npm fill `urls.repository` but get no notes.
- **D84. Trust, token, opt-out (security).** The step uses the HTTP client and token of the built-in
  `github-release` source (`token_env` or stored `auth`), **only when its base URL is the default
  `https://api.github.com`**; otherwise it is off and one Info log at start says so (a GitHub Enterprise token is
  never sent to `api.github.com`). Without a token the request is anonymous (60 per hour, deadline-bounded).
  With a token or a stored credential, the first request for a repository is `GET /repos/{owner}/{repo}`: the
  repository is read only when `private` is `false`, so a package that names a private repository the token can
  read cannot copy its release notes into events. The answer is cached in memory for 24 h, at most 4,096
  entries, oldest evicted. Owner and name come from `repolink.Parse`; no URL from package metadata is ever
  requested. Always on, no config key. `notes`, `urls.changelog` and `urls.repository` from this step are
  declared by the package, not verified; the notes text is untrusted as in ADR 0036.

Wiring: `repolink.Filler` implements `detect.NotesFiller`; `cmd/angelia/roles.go` builds it. The metric
`angelia_notes_enrichments_total{source, result}` counts one enrichment per check (results `matched`,
`no_match`, `no_repository`, `not_github`, `private`, `rate_limited`, `error`; every pair starts at 0).
No dependency, migration, config key, outbound host or `Source` interface change.

## Consequences

### Positive

- PyPI and npm events carry `urls.repository`, a declared changelog and, when the repository has a release
  whose tag matches, notes and the release page, at one request per release at most.
- The cost is visible (a metric per result) and bounded (deadline, 5 versions, 30 releases, cached public check).
- Nothing changes for a watch whose package declares no GitHub repository.

### Negative

- Named instances of `pypi` and `npm` (`sources.<name>` with `kind:`, for example a private mirror) are enriched
  too: the Checker hands the step the base kind. A private index that names a GitHub repository gets notes from
  public GitHub like any package, and the metric does not tell instances apart.
- A monorepo that publishes more than 30 releases between two checks, or tags a package in a form D82 does not
  list, misses its notes. A repository without GitHub releases (`@types/node`, `DefinitelyTyped`) gives none.
- The link of the latest release is applied to older versions of the same check; a moved repository gives
  those versions the new location.
- With a token, a repository is checked once a day (one request per repository per day); without one the 60
  requests per hour are shared with anonymous `github-release` watches.
- Notes, changelog and repository are chosen by the publisher of the package.

### Open questions

- **Measurement.** The spec asks for a live run over 20 PyPI and 20 npm packages
  (`go test -tags live -run TestLiveMeasurement ./internal/source/repolink`, anonymous or with
  `ANGELIA_LIVE_GITHUB_TOKEN`) and says to reconsider D81 (d) when fewer than about 30% of the versions match. The
  build session had no access to `api.github.com` (its proxy refused the host), so only discovery was measured
  (`ANGELIA_LIVE_DISCOVERY_ONLY=1`, 2026-11-26): **38 of 40 packages declare a `github.com` repository** (the two
  without are `protobuf` and `grpcio-status`); `react` and `react-dom` point at `react/react`, `pydantic-core` at
  `pydantic/pydantic`, `@babel/core` at `babel/babel`, `@types/node` at `DefinitelyTyped/DefinitelyTyped`. The hit
  rate of the matching is **to be recorded here by the maintainer** from one run with network access, before this
  ADR is accepted.
- A per-instance switch (`enrich.notes`), GitLab, Codeberg and `CHANGELOG.md` notes, paginating releases, crates
  (which has a `repository` field) are follow-ups.

## Alternatives considered

- **Per-version metadata** (`/pypi/{name}/{version}/json`): exact for moved repositories, one more request per
  version; npm has it per version in the same document for no gain. A curated package-to-repository file: stale.
- **A per-version `GET /releases/tags/{tag}`**: up to three requests per version, most of them `404`.
- **Fetching inside the sources** on every `200`: the source does not know which versions are new, and npm's
  packument changes on every dist-tag move and deprecation. It would also change what `List` costs and how it fails.
- **Discovery only**: meets the epic only for `github-release`.
- **Comparing versions after stripping prefixes**: `foo-v1.2.0` and `bar-v1.2.0` would both match package `baz`.
- **Always anonymous**: no private-repository risk, but 60 requests per hour per IP shared with every
  anonymous watch. A config switch with its own token: a YAML contract change nobody asked for. Verifying the
  link both ways: one or two more requests per package and no standard place to look.
- **A purl `vcs_url` qualifier** for the repository link: it changes the purl consumers join SBOMs and OSV on.

## References

- Issue #84 and its decisions D80 to D84; [ADR 0036](0036-release-notes-on-source-version.md);
  [ADR 0010](0010-diff-engine-known-set-and-baseline.md); [ADR 0027](0027-configurable-per-host-rate-limits.md).
- Spec 0.15.0: `urls.repository`, spec `docs/api.md` item 60.
- [architecture.md §3.3, §4, §10](../architecture.md), [prd.md](../prd.md) F12,
  [metrics](../operations/metrics.md).
