# ADR 0035: Watch-file resources move to the API in place, and the startup apply skips adopted names

- **Status:** Accepted (maintainer, 2026-11-19, in the project thread: "the database is the source of truth", the `POST /v1/adoptions` name, phases 1 and 2)
- **Date:** 2026-11-19
- **Story:** angelia-dev/angelia#287

## Context

The admin UI ([ADR 0033](0033-admin-web-ui.md)) edits watches, sinks and subscriptions through the
REST API. Rows loaded by `angelia serve --watches` are `managed_by: file`: the API refuses to change
them (spec `docs/api.md` item 3) because the startup apply would overwrite or delete what the API
wrote. An instance configured only through a watch file therefore shows a read-only UI.

The documented way out ([cli.md](../cli.md#moving-from-serve---watches-to-apply)) removes the objects
from the file, restarts, and creates them again through the API. That gives them new ids, starts a
new baseline (a release found in the gap emits nothing) and dead-letters the pending deliveries of
the removed subscriptions.

The UI should not write the watch file instead: it is the operator's file, often mounted read-only
into the container (Coolify, Kubernetes), shared by every replica with Postgres, read only at startup,
and it would lose its comments on every save.

## Decision

1. **The database is the source of truth for what the UI edits.** YAML stays the exchange format: the
   UI shows each object and the whole configuration as watch-file YAML and exports it; `angelia
   apply` reads it back.
2. **`POST /v1/adoptions`** (`admin`, spec 0.10.0, item 55) changes the owner of listed rows from `file`
   to `api` in place, in one API transaction: ids, targets, known versions, `baseline_done`, cooldown
   holds and pending deliveries are kept. It is all or nothing, has `dry_run`, and writes an audit
   record (`action: adopt`).
3. **Ownership stays consistent** (spec item 4: no reference across owners): a subscription is adopted
   only with its file sink, and a file sink only with every file subscription that uses it; otherwise
   `422` with a pointer per object.
4. **The instance remembers adopted names** in a new table `adoption (kind, name, adopted_at)`
   (migration 00023 on both engines, additive). The startup apply (`watchfile.Apply`) reads it first:
   - a file entry whose kind and name were adopted is **skipped** (`Skip` change, logged at `WARN`:
     "moved to the API with POST /v1/adoptions; remove it from the watch file") instead of failing on
     the taken name;
   - a file subscription whose sink was adopted is skipped too, since it could only reference an API
     row;
   - a record whose name the file no longer declares is deleted, so the name behaves as any other
     name again (a later file entry with it fails as taken while the API row exists).
5. **The UI** gets a Configure page: a three-column patch bay (watches, routes, sinks), an editor
   drawer with Form and YAML tabs reusing the existing forms, a banner and a one-time "Move to the UI"
   dialog (admin) that downloads the file objects as YAML and adopts them all.

## Consequences

### Positive

- An instance set up with a watch file becomes editable from the UI without losing state or a release.
- A restart before the operator cleans the file is safe: the process starts and says what it skipped.
- No change to the `Source`/`Sink` interfaces, the event schema or existing endpoints; the spec change
  is additive.

### Negative

- Until the operator removes the entries, the watch file no longer describes the instance; the
  startup `WARN` lines and the UI banner are the only reminders.
- A new table and three store methods on both engines.

### Open questions

- Phase 3 of the proposal: editing the whole configuration as YAML in the UI with a server-side plan
  (`POST /v1/plans`, reusing the apply code). Not in this story.
- Whether the UI should offer to adopt only part of the file (the API allows it; the dialog moves
  everything, which always satisfies the dependency rules).

## Alternatives considered

- **Write the watch file from the API.** Rejected: see Context (read-only mounts, replicas, comments,
  startup-only reads).
- **Let any API write take a file row over.** Rejected: an ordinary edit would silently take a row from
  the file, and the next startup would fight it.
- **Delete and recreate (the old documented path).** Rejected as the default: new ids, a new baseline
  and dead deliveries.
- **Skip every file entry whose name an API row has, without records.** Rejected: a typo or a name clash
  in the file would be silently ignored instead of failing as today.

## References

- Spec `docs/api.md` items 2 to 4 and 55; `openapi/angelia.v1.yaml` `adoptResources`.
- [watch-file.md §4](../watch-file.md#4-what-apply-does), [admin-ui.md](../admin-ui.md), [cli.md](../cli.md).
