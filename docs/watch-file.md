# Watch file

> **Phase 0 format: approved, but may still change before 1.0; every change is recorded as a decision in this document.**

Story: angelia-dev/angelia#48. Status: **format approved by the maintainer (decisions D1 to D17 in the
last section), implemented in `internal/watchfile`.** It becomes a public contract in Phase 1
(`angelia apply`), so names and semantics change only through a decision recorded here.

Until the REST API exists, `angelia serve --watches watches.yaml` loads **watches, sinks and
subscriptions** from one YAML file and reconciles them into the store. A complete example is in
[examples/watches.yaml](../examples/watches.yaml).

## 1. Relation to the instance configuration

| | Instance config ([configuration.md](configuration.md)) | Watch file (this document) |
|---|---|---|
| Describes | one Angelia process: listen address, database, roles, log, HTTP, per-source base URL and token | the data it manages: what to watch, where to deliver |
| Selected by | `--config` / `ANGELIA_CONFIG` | `--watches` / `ANGELIA_WATCHES` |
| Env overrides | `ANGELIA_*` per key | none; the file is declarative data |
| Secrets | `token_env` names a variable | `*_env` keys name a variable (same rule) |

- The two files never share keys. The watch file does not repeat `database.dsn`, `roles` or a source's
  `base_url`.
- Source credentials (for example a GitHub token) stay in the instance config under
  `sources.<kind>.token_env` or `sources.<kind>.auth` (a stored secret, see
  [private-registries.md](private-registries.md)). A watch cannot choose its own token or base URL: decision
  D9 is unchanged. A watch of a private registry is an ordinary watch of that source; the registry is the
  `base_url` of the kind in the instance configuration, or of a named instance
  (`sources.npm-internal: {kind: npm, base_url: ...}`, selected with `source: npm-internal`, see
  [private-registries.md](private-registries.md#several-registries-of-one-kind)).
- Same conventions as the instance config: unknown keys are errors, reported as
  `<file>: line N: ...` (the issue requires path and line); the file must contain exactly one YAML
  document; nothing is interpolated (`${VAR}` is not expanded, decision D10); values that look like
  tokens (`ghp_`, `gho_`, `ghs_`, `github_pat_`, `glpat-`, `npm_`, `pypi-`, `whsec_` prefixes) are
  rejected without being echoed.
- The file is applied by processes that run the `api` role (or `all`), once at startup, before the
  scheduler starts leasing. `--watches` on a process without the `api` role is an error.

## 2. Format

Top level:

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `version` | integer | yes | none | must be `1` |
| `sinks` | list of sink | no | `[]` | names unique |
| `watches` | list of watch | no | `[]` | names unique |
| `subscriptions` | list of subscription | no | `[]` | names unique |

An empty file (all three lists empty or absent) is valid and **deletes everything the file manages**
(see section 4). A file without a `version` key is an error, so an accidentally empty or truncated file
is not mistaken for "delete everything".

### 2.1 Names

Every sink, watch and subscription has a `name`: the stable key used by apply. Pattern
`^[a-z0-9][a-z0-9._-]{0,62}$`. Names are unique per list (a sink and a watch may share a name), and
also across owners: apply refuses a name that a row created through the API already has (migration
00011, spec `docs/api.md` decision 2).

### 2.2 `watches[]`

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `name` | string | yes | none | section 2.1 |
| `source` | string | yes | none | a registered source: a kind (`pypi`, `npm`, `crates`, `github-release`, `github-tag`, `gitlab-release`) or the name of a named source instance of the instance configuration (`sources.<name>.kind`); an unknown name is an error |
| `coordinates` | map string to string | yes | none | flat map (ADR 0006); validated by that source's `Validate` (no I/O), which owns normalisation |
| `interval` | duration | no | `1h` | Go syntax (`30m`, `6h`); at least `5m`, at most `720h` |
| `scheme` | string | no | by source kind | one of `semver`, `pep440`, `calver`, `lexical` (#68) |
| `labels` | map string to string | no | `{}` | at most 32 entries; key `^[A-Za-z0-9][A-Za-z0-9._/-]{0,62}$`, value at most 253 characters, no control characters |
| `policy` | policy | no | all defaults | section 2.3 |

- `coordinates` keys are source-owned. Defined by the source stories, not here. For the built-in
  sources: `pypi`, `npm` and `crates`: `name` (scoped npm names such as `@types/node` allowed);
  `github-release` and `github-tag`: `repo` as `owner/name` (for example `cli/cli`);
  `gitlab-release`: `project`, the path with namespace (`group/subgroup/project`) or the numeric id; each in the
  form `Source.Validate` accepts.
- A named instance behaves as its kind everywhere except where the registry is chosen: `coordinates`
  validation, the default `scheme`, the events (`data.source`, purl type) and `filter.source` use the kind.
  The `source` of a stored watch, and of the API, is the name the watch gave; the target remembers the kind
  it was created for and is refused if the name later becomes a source of another kind
  ([private-registries.md](private-registries.md#what-a-name-is-bound-to)).
- `scheme` overrides the default chosen by source kind (`internal/version/schemes.ForSourceKind`).
  The loader checks the value against `schemes.Names()`, so it accepts `calver` as soon as #68 merges.
  `maven` is not accepted (Phase 1).
- Two watches may point at the same `source` + `coordinates`; they share one target (coalescing,
  architecture.md section 3.2) and differ only by labels, scheme and policy. The target's interval is the
  smallest interval among the watches that use it.

### 2.3 `watches[].policy`

Keys follow architecture.md section 3.4 and PRD F4.

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `prerelease` | string | no | `none` | `none`, `only` or `include` |
| `include` | list of string | no | `[]` (all) | each a valid Go (RE2) regex, matched against the raw version; at most 16 |
| `exclude` | list of string | no | `[]` | same; exclude wins over include |
| `constraint` | string | no | empty (any) | range such as `">=2 <3"`, checked by the scheme in use ([syntax](policy.md#constraint)) |
| `bump` | list of string | no | `[]` (all) | any of `major`, `minor`, `patch` |
| `cooldown` | duration | no | `0` | Go syntax, at most `8760h` |
| `baseline` | boolean | no | `true` | when true, versions known at the first check are recorded without events |

The loader validates each policy structurally (types, enums, regex compilation, durations), then
decodes the stored form with the policy engine (`policy.Decode`) and compiles it with the watch's scheme
(`policy.Compile`). A `constraint` with a syntax error, or with an operand the scheme cannot parse, fails
at load time with the file, line and path of the key. The policy is stored as `watch.policy_json`.

### 2.4 `sinks[]`

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `name` | string | yes | none | section 2.1 |
| `kind` | string | yes | none | a registered sink kind: `webhook`, `github-dispatch` or `shoutrrr` |
| `config` | map | yes (may be `{}` if the kind allows) | none | validated by that sink's `Validate`; unknown keys are errors |

`webhook` config:

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `url` | string | yes | none | absolute `https://` URL (or `http://`, see D12), no userinfo, no query string holding secrets; the SSRF guard (netguard) applies at delivery time, the allowlist stays in the instance config |
| `secret_env` | string | no | none | name of an environment variable holding the Standard Webhooks signing secret ([ADR 0005](adr/0005-standard-webhooks-signing.md)); a `secret:` key is an unknown key and an error. **Security restriction:** a name starting with `ANGELIA_` is refused, because those variables hold the instance's own settings, such as the secrets key |
| `secret_ref` | string | no | none | name of a [stored secret](configuration.md#stored-secrets) holding the signing secret (`^[a-z0-9][a-z0-9._-]{0,62}$`); at most one of `secret_env` and `secret_ref` |
| `timeout` | duration | no | `10s` | `1s` to `2m` |

If neither `secret_env` nor `secret_ref` is set the webhook is delivered unsigned and `config check` warns.
A `secret_ref` naming a secret that is not stored is a warning at apply; deliveries fail, and are retried,
until it is stored (`angelia secret set`). A stored secret bound to
hosts is sent only to those hosts: a `url` outside them fails each delivery before it is sent, and it is retried. A `secret_ref` value signs
exactly as a `secret_env` value. See [Stored secrets](configuration.md#stored-secrets). If the named
variable is empty or unset at startup, apply succeeds with a warning and delivery fails visibly until it is
set (same rule as `token_env` in configuration.md). The variable holds one to three `whsec_` secrets
separated by spaces, for rotation; a `timeout` above the dispatcher's 15 s deliver timeout has no effect
and `config check` warns. Signing, rotation, responses and the receiver's side: [webhooks.md](webhooks.md).

`github-dispatch` config ([github-dispatch.md](github-dispatch.md); `additionalProperties: false`):

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `mode` | string | no | `repository_dispatch` | `repository_dispatch` or `workflow_dispatch` |
| `repo` | string | yes | none | `owner/name` |
| `api_base_url` | string | no | `https://api.github.com` | absolute `https://` URL with no userinfo, query or fragment; GitHub Enterprise Server: `https://host/api/v3`. The token is sent only here (`x-angelia-destination`) |
| `token_env` | string | exactly one of the two | none | name of an environment variable holding the GitHub token; a name starting with `ANGELIA_` is refused |
| `token_ref` | string | exactly one of the two | none | name of a [stored secret](configuration.md#stored-secrets) holding the token (`^[a-z0-9][a-z0-9._-]{0,62}$`) |
| `event_type` | string (template) | no | `angelia.release.published` | `repository_dispatch` only; the result is 1 to 100 characters |
| `workflow` | string | yes for `workflow_dispatch` | none | `workflow_dispatch` only; a file name (`release.yml`) or a numeric id |
| `ref` | string | yes for `workflow_dispatch` | none | `workflow_dispatch` only; a branch or tag |
| `inputs` | map of string (template) | no | none | `workflow_dispatch` only; at most 25; a name must not end in `_env` or `_ref` |
| `timeout` | duration | no | `10s` | `1s` to `2m`; above the dispatcher's 15 s deliver timeout has no effect and `config check` warns |

`event_type` and the values of `inputs` are [templates](templates.md), checked when the file loads like a
subscription template. A missing token reference is an error, and an unset `token_env` variable is a warning, as
for the webhook. The defaults are written into the stored configuration.

`shoutrrr` config ([shoutrrr.md](shoutrrr.md); `additionalProperties: false`):

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `url_ref` | string | exactly one of the two | none | name of a [stored secret](configuration.md#stored-secrets) whose value is the whole shoutrrr service URL (`^[a-z0-9][a-z0-9._-]{0,62}$`); bind it with `--host` to the hosts the service sends to |
| `url_env` | string | exactly one of the two | none | name of an environment variable holding the URL; a name starting with `ANGELIA_` is refused |
| `service` | string | no | none | `slack`, `discord`, `telegram`, `ntfy`, `gotify` or `matrix`; the URL's scheme must equal it at delivery |
| `timeout` | duration | no | `10s` | `1s` to `2m`; above the dispatcher's 15 s deliver timeout has no effect and `config check` warns |

The URL is never in the file. It is read and checked at every delivery (allowed services, `https` or
`http.allow_http_hosts`, the SSRF guard, the secret's hosts); a failure is retried. An unset `url_env` variable
is a warning, as for the webhook.

### 2.5 `subscriptions[]`

| Key | Type | Required | Default | Validation |
|---|---|---|---|---|
| `name` | string | yes | none | section 2.1 |
| `sink` | string | yes | none | the `name` of a sink **in the same file** |
| `filter` | filter | no | `{}` (matches every event) | below |
| `policy` | subscription policy | no | all inherited | section 2.6 |
| `template` | string | no | empty (default CloudEvents JSON) | Go `text/template` over the event (`.data.package`, ...) that must render the sample events within the limits; fields, functions and limits: [templates.md](templates.md) |

**Template check (story #114, [ADR 0016](adr/0016-message-templates-text-template-bounded.md); maintainer:
please confirm).** A template used to be accepted when it parsed. It is now also run against the full
and sparse sample events ([templates.md](templates.md#checked-when-the-configuration-loads)), so these
templates, accepted before, are now rejected at load time:

1. a misspelt or unknown key (`{{.data.pakage}}`);
2. a nullable field read without a guard (`{{.data.notes.body}}`, `{{.data.urls.release | len}}`):
   every event fails it today for `notes`;
3. a label read as a field (`{{.data.watch.labels.team}}`): the sparse sample has no labels; use
   `{{index .data.watch.labels "team"}}`;
4. `print`, `printf`, `println`, `html`, `js` or `urlquery` given a list or an object;
5. `define`, `block`, `template` or `call`, and functions outside the curated set (`env`, ...);
6. a template over 32 KiB, or whose sample render takes over 250 ms or writes over 256 KiB.

`filter` (all given keys must match; PRD F6 lists ecosystem, labels, bump, package glob):

| Key | Type | Default | Validation |
|---|---|---|---|
| `source` | list of string | any | registered source kinds ("ecosystem" in the PRD); the kind of an event, not the name of a named instance |
| `labels` | map string to string | none | the event's watch must carry all of these labels with equal values |
| `bump` | list of string | any | `major`, `minor`, `patch` |
| `package` | string | any | glob (`path.Match` syntax) on the package name |
| `type` | list of string | `[release.published]` | `release.published`, `release.yanked`, `release.prerelease` |

The filter is stored as `subscription.filter_json`. `bump` narrows the watch's `bump` (section 2.6).

### 2.6 `subscriptions[].policy`

Story: angelia-dev/angelia#69, [ADR 0014](adr/0014-subscription-policy-overrides.md). Two consumers of
one watch can want different policies (CI wants every patch, chat only minors after 72 h).

| Key | Type | Default | Validation |
|---|---|---|---|
| `prerelease` | string | absent = inherit the watch | `none`, `only` or `include` |
| `cooldown` | duration | `0` (no extra hold) | Go syntax; clamped to `0` to `8760h` and cut to whole seconds |

With `filter.bump` these are the three keys a subscription can use to narrow the watch policy. `bump`
stays in `filter`; there is no `policy.bump`.

**A subscription can only narrow.** It receives the events its watch policy already admitted, then
applies its own rules, so it can only receive less, and later:

| Key | Effective rule |
|---|---|
| `bump` | the version passes the watch's `bump` **and** the subscription's. Listing a bump the watch drops matches nothing. |
| `prerelease` | the subscription's mode is intersected with what the watch admitted. `include` keeps what the watch admitted; `none` and `only` keep the matching part. `include` on a watch with `none` still gets no pre-release, and `only` on a watch with `none` gets nothing. |
| `cooldown` | counted from the **same start** as the watch's cooldown, not added to it: the effective hold is `max(watch, subscription)`. A value below the watch's changes nothing. A held delivery is released when it ends. |

Because a subscription is not tied to one watch, a value that does nothing for one watch is right for
another, so such values are **not errors**: syntax mistakes (unknown key, bad mode, unparseable
duration) fail at load time, but a widening or useless value is clamped (negative or over-long
cooldown) or intersected away at evaluation (a cooldown below the watch's, a prerelease mode the
watch already excludes), never rejected. Evaluation is
`subscription.Policy.Evaluate`; a `release.yanked` event is never delayed or dropped by `policy.*`.

A cooldown hold is a pending delivery with `next_attempt_at` in the future, so it survives restarts.
Editing a subscription's `filter` or `policy` decides its **held** deliveries again (queued deliveries
are not touched): one the new filter no longer matches (for example `filter.bump` narrowed), or that
the new `prerelease` mode excludes, becomes `dead` with a reason; one whose new hold has passed is
released at once; one whose hold is still ahead is moved to the new time. The filter is matched against
the input recorded when the delivery was held.

## 3. How a file maps onto the store

Tables from `internal/store/migrations/sqlite/00001_initial_schema.sql`.

| File | Rows |
|---|---|
| `watches[]` | one `target` (upserted by `kind` + canonical `coordinates_json`, shared; coordinates are first put in the source's canonical form, [ADR 0021](adr/0021-source-canonical-target.md)) and one `watch` (`policy_json` holds the policy plus `scheme`; `interval` is stored on the watch, `watch.interval_ms`, and the target holds the effective one, the smallest among its watches; `labels_json`) |
| `sinks[]` | one `sink` (`config_json`; the secret *reference* `secret_env` or `secret_ref` is stored, never a value) |
| `subscriptions[]` | one `subscription` (`filter_json`, `sink_id`, `template`) |

**Gaps in the schema of migration 00001** (decision D5):

1. No table has a stable `name`, and no row records that it came from the file. Row ids are ULIDs.
2. No column holds `interval`; architecture.md says `next_check_at` is computed from `interval`.

Resolved by one additive migration, `00004_watch_file_ownership.sql`: `name TEXT` and
`managed_by TEXT NOT NULL DEFAULT 'api'` on `watch`, `sink` and `subscription` (unique on
`(managed_by, name)`; unnamed rows never collide), and `interval_ms INTEGER` on `target`. Nothing is
dropped or rewritten; existing rows become `api` rows. Migration `00011_unique_names.sql` (#185) later
made `name` unique per table whoever owns the row; it fails, listing the duplicates, when a file row and
an API row already share a name, and rewrites nothing. Migration `00012_watch_interval.sql` adds
`interval_ms` on `watch`: the interval the watch asked for.

## 4. What apply does

Apply runs at startup, in **one transaction**, and is all-or-nothing: the whole file is validated first
(every error is reported, each as `file: line N: path: message`, for example
`watches.yaml: line 14: watches[1].policy.prerelease: must be one of none, only, include`), and the
store is untouched if any entry is invalid. The process then exits non-zero instead of serving with a
partial configuration.

Reconciliation, per kind (sinks, watches, subscriptions), keyed on `(managed_by = 'file', name)`:

| Situation | Action |
|---|---|
| name in file, no row | **create** (new ULID id; for a watch, `baseline_done = 0`) |
| name in file, row exists, content differs | **update** in place; id, history and version set kept |
| name in file, row exists, identical | no-op (the second run changes nothing, which is how "idempotent" is tested) |
| row managed by file, name not in file | **delete** |
| row not managed by file (`managed_by = 'api'`) | never touched |
| name in file, row with that name managed by the API | apply and plan fail: `sink "hook": name taken by a sink managed by the API` |

Order inside the transaction: sinks, watches, subscriptions (create/update), then deletes in reverse
dependency order (subscriptions, watches, sinks), then targets no watch uses are deleted.

Details:

- **Changing `source` or `coordinates`** of a named watch points it at another target, resets
  `baseline_done` to 0 and keeps the id. Changing only `labels`, `policy`, `scheme` or `interval` keeps
  the baseline (D7).
- **Renaming** is delete + create: new id, new baseline, no link to the old events.
- **Deleting a watch**: the `watch` row goes away and no new events or deliveries are created for it.
  History is kept (ADR 0008: `event` and `delivery` rows carry no foreign key). The `target` and its
  `version` rows are deleted when no other watch uses it.
- **Deleting a subscription** (or its sink): all its `pending` deliveries, held or not, become `dead`
  with `last_error` `subscription removed` (they would never be sent, and stay visible); `in_flight`
  finish; `delivered` and `dead` rows stay as history. This is how "removing a watch removes its
  future deliveries" is met when the watch's subscription is removed with it; deliveries already
  queued for a *kept* subscription are sent (D8).
- A subscription whose `sink` names a missing sink is a validation error, not a dangling row.
- A sink cannot be removed while a kept subscription references it (validation error).
- Secrets: apply never reads the value of a `*_env` variable into the database.

`angelia config check --watches FILE` runs the same validation with no database and no network.

### 4.1 Implementation notes

- `watchfile.Parse` validates the whole file (every error, ordered by line); `watchfile.Apply` runs the
  reconciliation in one `Store.InConfigTx` transaction; `watchfile.PlanFile` computes the same plan
  without writing. Both return a plan of `create`, `update`, `delete` and `unchanged` lines.
- Each watch stores the interval it asked for in `watch.interval_ms`. A target's `interval_ms` is the
  effective interval: the smallest among the watches on it, the API's included (those without a stored
  interval do not count), kept in step on every apply. Lowering it also pulls `next_check_at` forward to
  at most one interval from now. A new target is due immediately. When the file stops watching a target
  that API watches still use, its `interval_ms` becomes the smallest among those API watches. A change
  of the effective interval shows in the plan as `update target <id> (interval …)`. Apply uses
  `ConfigTx.RecomputeTargetInterval`, which the REST API (#71) calls after its own watch writes.
- The first apply after upgrading to migration 00012 records each file watch's interval once: watches
  stored before it have no `watch.interval_ms`, so the plan shows them as
  `update watch <name> (interval 1h0m0s (recorded))`. Later applies show them unchanged.
- `policy_json` holds the policy with every default filled in plus `scheme`; `filter_json` likewise. This
  keeps the no-op comparison exact. Watch and subscription rows are compared by value, never by text.
- Webhook URLs are never echoed in errors. A query parameter whose name contains `token`, `secret`, `key`,
  `password`, `passwd`, `auth` or `sig` is rejected.
- Any string that looks like a credential (prefixes above) is rejected in labels, URLs and `*_env` values.
  Names and coordinates are exempt, because registry names such as `npm_foo` are legitimate.

## 5. Conflicts with the existing documents

1. The issue says the format goes in `docs/config.md`; the instance config doc is
   `docs/configuration.md`. The format lives in `docs/watch-file.md`, linked from configuration.md (D1).
2. architecture.md section 3.1 describes `angelia apply -f watches.yaml` going through the API; Phase 0
   uses `angelia serve --watches` straight into the store, as the issue says. The file format is meant to
   be the same one.
3. architecture.md section 3.4 says policy applies "per watch or subscription"; the watch carries the
   full policy, a subscription only the narrowing keys of section 2.6 (D6, amended by #69).
4. The store had no `name`, `managed_by` or `interval` columns; migration 00004 adds them (section 3).
5. PRD F3 lists Maven as a scheme; #68 added calver and not Maven. The `scheme` enum follows #68.
6. `schemes.Names()` now lists `calver`, so `scheme: calver` is accepted.

## 6. JSON Schema

The issue does not ask for one, so none is shipped. For Phase 1, the right home is the **spec repository** (`schemas/watch-file/v1.json`, JSON Schema
2020-12, valid and invalid examples validated in CI, as AGENTS.md of that repository requires), because it
is a public contract consumers use for editor completion and CI linting. Any change there is Human-led.

## 7. Decisions (all approved as recommended)

| # | Decision | Alternatives | Recommendation |
|---|---|---|---|
| D1 | File name and doc home: `docs/watch-file.md`, flag `--watches`, env `ANGELIA_WATCHES` | `docs/config.md` as the issue says; put watches under the instance config | Keep a separate file and doc; link from configuration.md |
| D2 | Top-level keys `sinks`, `watches`, `subscriptions` and `version: 1` (required) | `apiVersion`/`kind` Kubernetes style; no version key | Plain keys plus required `version`; cheap to evolve and guards against empty files |
| D3 | Stable key is a human `name` per object | use ULID `id` in the file; name = id; derive from coordinates | `name`; ULIDs are generated, and a file author needs a key they can type |
| D4 | `coordinates` (flat string map) as the key for source parameters | `target:` (architecture wording); per-kind typed keys such as `package:`/`repo:`; `purl` strings | `coordinates`: matches the issue, ADR 0006 and `coordinates_json`; the loader stays generic |
| D5 | Add migration `00004` (`name`, `managed_by` on watch/sink/subscription, `interval_ms` on target) | deterministic ids derived from names (no migration); store name and interval inside `*_json` | Migration: explicit, supports the Phase 1 API coexisting with file-managed rows |
| D6 | `policy` on the watch only; subscription has `filter` only. **Amended by #69 (ADR 0014):** a subscription also has `policy` with `prerelease` and `cooldown`, which only narrow | policy also on subscriptions (architecture.md 3.4) | Watch only in Phase 0; add later without breaking the format |
| D7 | Changing source/coordinates resets the baseline; other edits keep it | never reset; always reset on any edit | As proposed, to avoid a flood of events for the new target |
| D8 | Deleted subscription sets its pending deliveries to `dead` with a reason (**amended by #69, maintainer, 2026-11-01**; first approved as "removes"); deleted watch only stops new events; history is kept | delete the watch's pending deliveries too; keep everything pending | As proposed (confirmed: "its future deliveries" means the pending deliveries of a removed subscription); `dead` instead of deletion keeps them visible; dead rows are pruned with their event like any history |
| D9 | No per-watch token; credentials come from the instance config `token_env` | `token_env` on a watch or sink | None in Phase 0; add `auth_env` later if needed |
| D10 | No `${VAR}` interpolation; only `*_env` references | interpolate env into any value | No interpolation: avoids accidental inline secrets and keeps the file reviewable |
| D11 | Bounds: `interval` 5m to 720h default 1h; `labels` at most 32; name pattern in 2.1 | other limits | Accept; the 5m floor protects upstream rate limits (netguard has per-host limits anyway) |
| D12 | Webhook `url` must be `https://` | allow `http://` too (needed for local tests and homelabs) | `https` by default; `http` only if the instance config allowlist permits the host. The loader takes the allowlist as an option, filled from the instance key `http.allow_http_hosts` (#108, ADR 0023) |
| D13 | `baseline` defaults to `true` | default `false` | `true`: no event flood on first run |
| D14 | Sink `secret_env` stores the reference, resolved at delivery | read the value at apply and store it encrypted (needs the Phase 1 secrets key) | Reference in Phase 0. With encryption at rest ([ADR 0024](adr/0024-secrets-encrypted-at-rest.md)) a sink may instead name a stored secret with `secret_ref`; the file still holds only the reference |
| D15 | Whole-file validation, all-or-nothing, process exits non-zero on error | skip invalid entries and continue | All-or-nothing; a half-applied GitOps file is worse than a crash |
| D16 | Filter keys `source`, `labels`, `bump`, `package`, `type`; `source` instead of the PRD's "ecosystem" | `ecosystem` | `source`: it is the same word as the watch key and the CloudEvents attribute |
| D17 | `scheme` is a top-level watch key, stored in `policy_json` | inside `policy` | Top-level, as #68 specifies; storage detail is internal |
