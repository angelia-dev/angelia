# ADR 0036: Release notes on source.Version

- **Status:** Proposed (D31, D34 and D37 are for the maintainer; the answers below are the recommended ones of the spec)
- **Date:** 2026-11-25
- **Story:** angelia-dev/angelia#83
- **Amends:** [ADR 0006](0006-source-target-and-fetchstate.md)

## Context

The release event has had a `notes` field since spec 0.1 (`format`, `body` at most 16384 bytes of UTF-8,
`truncated`), and every event carries it as `null` ([architecture.md §4](../architecture.md#4-event-contract-v1)).
PRD F12 asks for release notes enrichment. The `github-release` source already downloads each release's `body`
in the list response and ignores it; `gitlab-release` does the same with `description`.

`source.Version` ([architecture.md §3.3](../architecture.md#33-source-plugins-detect)) has no field for notes,
and `Source` is a contract: changing it needs an ADR (AGENTS.md). The text is written by whoever can publish a
release upstream, and flows to webhooks, chat sinks and LLM agents, so what the core does with it is a security
decision.

## Decision

The spec's decisions D30 to D39, with the recommended answers:

- **D30. Sources.** `github-release` (the release body) and `gitlab-release` (the release description): the same
  zero-cost mechanism. Not `github-tag`, `pypi`, `npm`, `crates`: PyPI and npm are #84. GitLab: done in the
  same story (83-C).
- **D31. Contract.** `source.Version` gains `Notes *source.Notes` (`Format`, `Body` strings, `Truncated` bool).
  `policy.Release` gains `Notes *source.Notes` with `json:"notes,omitempty"`, and `event.Build` maps it to
  `data.notes`. It is additive: a source that leaves it `nil` behaves as before, and a `pending_release.release_json`
  row without the key decodes with `nil` (no migration). The `Source` interface methods, the plugin wire types
  (`wireVersion`) and the sinks do not change; WASM plugins keep `notes: null`.
- **D32. Cap.** `source.MaxNotesBytes = 16384`, cut at a UTF-8 character boundary with `truncated: true`.
  `source.NewNotes` is the only implementation of the cut; `event.MaxNotesBytes`, `event.Notes` and
  `event.NewNotes` are aliases of it. The kept prefix is copied (`strings.Clone`), so the decoded body (up to
  125,000 characters on GitHub) is not retained by the `[]Version` of a 10-page scan.
- **D33. Format.** The body as written, `format: markdown`, for both forges. No conversion, no `body_html`. A
  body that is empty or only white space after cleaning gives `nil`, never an empty object.
- **D34. Cleaning (security).** `source.NewNotes` normalizes `\r\n` and `\r` to `\n`; removes C0, DEL and C1
  control characters except `\n` and `\t`; replaces invalid UTF-8 with U+FFFD; removes HTML comments
  (`<!-- ... -->`) that open outside a fenced code block, because GitHub and GitLab do not render them and they are the
  easiest place to hide a prompt-injection payload that a reviewer never sees and an agent reads; and trims.
  A comment is removed through its first `-->` even when fence lines lie in between, as in CommonMark (an HTML
  block of type 2 runs to the first `-->`); a `<!--` inside a fence is kept, and so is an unclosed one. Cleaning runs before the
  cut. It does not strip other HTML, neutralize mentions or scan for secrets: that rewrites upstream content and
  duplicates what each consumer must do for its own medium. **`notes.body` is untrusted upstream content:
  consumers escape it for their medium and treat it as data, never as instructions.** Removal repeats until a pass removes nothing, because removing a comment can change the
  fence structure around it; a join that would rebuild `<!--` keeps one space. The work is bounded to 16
  linear passes, and **fail closed**: if the text has not settled by then, every remaining closed comment is
  removed with fences ignored. So the result never has a closed comment outside a fence, and `NewNotes` is
  idempotent on its body (both are fuzz properties). Known limits of this line heuristic: raw HTML blocks other
  than comments are not parsed, and link-reference comments (`[//]: # (...)`) are not removed.
- **D35. Cost.** Decode `body` and `description` from the pages `List` already fetches: no extra request, no
  extra byte, a `304` stays free. Tests assert the request counts.
- **D36. Which body.** The body at the check that first saw the version. A version held by a cooldown keeps it in
  `release_json`; editing a release later changes nothing (events are immutable). Refreshing is a possible
  follow-up.
- **D37. Opt-out.** None: always on, no config key. Notes of a private repository reach every sink that sends the
  whole event, as its version and URL already do. Templates control what goes to chat; `github-dispatch` does not
  send notes. A per-watch `notes: false` would be its own Human-led story (YAML and OpenAPI contract).
- **D38. Spec text.** Spec 0.10.1 changes only the descriptions of `notes` (schema and
  `docs/release-event-v1.md`); `api/schemas/release/v1.json` is vendored from it.
- **D39. Constraint for #84.** #83 builds no enricher interface. #84 adds its post-policy step and fills `notes`
  **only when the source left it `nil`**, and builds it with `source.NewNotes`, so the cleaning and the cap stay
  in one place.

## Consequences

### Positive

- Events carry release notes for GitHub and GitLab sources at no request cost; agents and templates get
  `data.notes` with a bound and a documented trust level.
- One cleaning and cut implementation, fuzzed (`FuzzNewNotes`).
- No migration, dependency, config key or outbound host.

### Negative

- Webhook bodies grow by up to about 16 KiB plus JSON escaping; `events_since` over MCP returns up to about
  330 KiB of notes at its default limit of 20.
- Notes of a private repository reach sinks that send the whole event (D37).
- The notes are a snapshot: an edit after detection is not reflected (D36).
- Removing comments changes the text compared with the raw body, as the release page already does.
- The comment rule is a line heuristic, not a Markdown parser: raw HTML blocks are not parsed, and
  `[//]: # (...)` link-reference comments stay in the text (a consumer's own escaping still applies).

### Open questions

- Opt-out per watch (D37 b), refresh after an edit (D36 b), smarter cut points (D32 b), notes in the plugin ABI
  and for `github-tag` annotated messages: follow-up issues if users ask.

## Alternatives considered

- **`Metadata["notes"]`** (D31 b): untyped, invisible in the contract, and it lets any plugin write into `notes`.
- **A post-policy enricher fetching one body per emitted version** (D31 c, D35 b): one request per event against
  the rate limit; it is the design #84 needs, not this story.
- **Full sanitizing in the core** (D34 c): rewrites upstream content and duplicates what consumers must do.
- **Markdown to text, or `body_html`** (D33 b, c): lossy or a dependency, or a changed response that the spec
  forbids passing through.

## References

- angelia-dev/angelia#83 (decisions D30 to D39), #84, epic #17; PRD F12 and G3.
- [ADR 0006](0006-source-target-and-fetchstate.md), [ADR 0016](0016-message-templates-text-template-bounded.md),
  [ADR 0018](0018-yank-events-reference-the-original.md); architecture.md §3.3 and §4.
- `internal/source/notes.go`; spec 0.10.1 (`schemas/release/v1.json`, `docs/release-event-v1.md`).
