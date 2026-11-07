# Message templates (draft)

Story: angelia-dev/angelia#114. Decisions: [ADR 0016](adr/0016-message-templates-text-template-bounded.md).
Status: **draft**. The template key is the watch file's `subscriptions[].template`
([watch-file.md §2.5](watch-file.md#25-subscriptions)); this page defines what a template sees and
can call. It becomes a public contract in Phase 1. Code: `internal/render`.

A subscription's `template` shapes the body its sink delivers. It is a Go
[`text/template`](https://pkg.go.dev/text/template). Without one, the body is the event itself, in
CloudEvents JSON structured mode (content type `application/cloudevents+json; charset=utf-8`).

```yaml
subscriptions:
  - name: chat
    sink: team-chat
    template: |
      {{.data.package}} {{.data.version}} is out{{with .data.previous_version}} (was {{.}}){{end}}
```

## What a template sees

The dot is the **event**, as the JSON object of the [release event v1 spec](https://github.com/angelia-dev/spec/blob/main/docs/release-event-v1.md):
the same names as on the wire, lower case with underscores.

| Write | Value |
|---|---|
| `.type` | `dev.angelia.release.published.v1` or `dev.angelia.release.prerelease.v1` |
| `.id`, `.subject`, `.time`, `.source` | CloudEvents attributes (`.source` is a URI, not the source kind) |
| `.data.source` | source kind: `pypi`, `npm`, `github-release`, ... |
| `.data.package`, `.data.version`, `.data.normalized_version`, `.data.scheme` | strings, always set |
| `.data.previous_version`, `.data.bump`, `.data.purl` | string or **null** |
| `.data.prerelease`, `.data.backport`, `.data.yanked` | booleans |
| `.data.published_at`, `.data.cooldown_satisfied_at` | RFC 3339 timestamp or **null** |
| `.data.detected_at` | RFC 3339 timestamp |
| `.data.urls.release`, `.data.urls.changelog`, `.data.urls.artifact` | URL or **null** |
| `.data.notes` | **null** today; later an object with `.format`, `.body`, `.truncated` |
| `.data.watch.id`, `.data.watch.name` (or null), `.data.watch.labels` (a map, may be empty) | the watch |

Rules:

- **A misspelt key is an error**, not `<no value>`: `{{.data.pakage}}` fails the configuration check.
- **Null fields need a guard.** `{{.data.notes.body}}` fails when `notes` is null; write
  `{{with .data.notes}}{{.body}}{{end}}`. A null printed directly gives `<no value>`; prefer `with`
  or `default`. The string functions below treat null as `""`.
- **Read a label with `index`.** A watch may have no labels, so `.data.watch.labels.team` fails
  (on the sparse sample event at load time, and on any watch without that label). Write
  `{{index .data.watch.labels "team" | default "none"}}`: `index` gives null for a missing label.
- Fields added by a later minor version of the spec appear under their spec name.

## Functions

Only these functions exist. None reads the environment, files, the network or the clock, so a
template's output depends on the event alone. Functions that take a value last work in a pipeline:
`{{.data.version | trimPrefix "v"}}`.

| Function | Example | Result |
|---|---|---|
| `default d v` | `{{.data.watch.name \| default "unnamed"}}` | `v`, or `d` when `v` is null or `""` |
| `upper s`, `lower s`, `trim s` | `{{.data.package \| upper}}` | case changed, spaces trimmed |
| `trimPrefix p s`, `trimSuffix p s` | `{{.data.version \| trimPrefix "v"}}` | `s` without the prefix or suffix |
| `contains sub s`, `hasPrefix p s`, `hasSuffix p s` | `{{if hasPrefix "@" .data.package}}` | boolean |
| `replace old new s` | `{{replace "/" "-" .data.package}}` | every `old` replaced |
| `split sep s` | `{{index (split "/" .data.package) 1}}` | list of strings |
| `join sep list` | `{{split "/" .data.package \| join " / "}}` | one string; elements must be strings |
| `truncate n s` | `{{truncate 80 .subject}}` | at most `n` characters, the last one `…` when cut |
| `toJSON v` | `{{toJSON .data.package}}` | `v` as JSON (strings quoted and escaped); use it for every value in a JSON body |
| `date layout ts` | `{{date "2006-01-02" .data.published_at}}` | timestamp in UTC with a [Go layout](https://pkg.go.dev/time#pkg-constants); null gives `""` |
| `print`, `printf`, `println` | `{{printf "%s@%s" .data.package .data.version}}` | as in Go, for strings, numbers, booleans and null only (use `join` or `toJSON` for lists and objects); `*` widths are refused |
| `html`, `js`, `urlquery` | `{{urlquery .data.package}}` | escaped for HTML, JavaScript, a URL query; same arguments as `print` |
| `and`, `or`, `not`, `eq`, `ne`, `lt`, `le`, `gt`, `ge`, `len`, `index`, `slice` | `{{if eq .data.bump "major"}}` | the `text/template` builtins |

`call`, `define`, `template` and `block` are not available.

## Limits

| Limit | Value | When it is passed |
|---|---|---|
| Template size | 32 KiB | the configuration is rejected |
| Execution time | 1 s per delivery, 250 ms per sample event at load time | the delivery attempt fails, or the configuration is rejected |
| Output size | 256 KiB (also any single string a function builds) | the delivery attempt fails |

## Content type

A template's body is sent as `application/json; charset=utf-8` when, without surrounding white space,
it starts with `{` or `[` and is valid JSON; as `text/plain; charset=utf-8` otherwise (so `true` or
`42` is text). A sink may set its own (a chat service that takes text).

## Checked when the configuration loads

Every template is compiled and run against two sample events when the watch file is applied:
**full** (every nullable field set, notes and labels included) and **sparse** (a first version with
every nullable field null, no name and no labels). A template that fails either one, or passes a
limit, is rejected with its line and column, for example
`subscriptions[0].template: on the sparse sample event: line 1, column 7: cannot read field "body" of a null value`.
Error messages never quote the template text: the reason is one of a fixed set (`unknown function`,
`undefined variable`, `unexpected token`, `unclosed action`, `unknown key`, ...), and a name is shown
only when it is a function name, or at most 24 characters of `[a-z_][a-z0-9_]*` with no run of 4 digits (so not a hex hash or token).

Templates accepted before this check (they only had to parse) can now be rejected; see
[watch-file.md §2.5](watch-file.md#25-subscriptions).

## Default chat messages

A [`shoutrrr`](shoutrrr.md) sink whose subscription has no template sends plain text, not the CloudEvents
JSON. The defaults, per event type (`internal/sink/shoutrrr.DefaultTemplates()`; not a schema contract):

| Event type | Template |
|---|---|
| `release.published` | `{{.data.package}} {{.data.version}} released{{with .data.bump}} ({{.}}){{end}}{{with .data.previous_version}}, previous {{.}}{{end}}` and, on a second line, the release URL when there is one |
| `release.prerelease` | `{{.data.package}} {{.data.version}} pre-release published{{with .data.previous_version}}, previous {{.}}{{end}}` and the release URL |
| `release.yanked` | `{{.data.package}} {{.data.version}} was yanked upstream; do not upgrade to it` and the release URL |
| any later type | `{{.data.package}} {{.data.version}}: {{.type}}` |

## Examples

A chat line:

```gotemplate
{{.data.package}} {{.data.version}}{{if .data.prerelease}} (pre-release){{end}} is out{{with .data.previous_version}}, was {{.}}{{end}}.
Team: {{index .data.watch.labels "team" | default "none"}}
{{with .data.urls.release}}{{.}}{{end}}
```

A JSON body for a webhook or a CI trigger; `toJSON` makes every value valid JSON:

```gotemplate
{"event_type": "dependency-release", "client_payload": {"package": {{toJSON .data.package}}, "version": {{toJSON .data.version}}, "bump": {{toJSON .data.bump}}, "labels": {{toJSON .data.watch.labels}}}}
```

Markdown with the release notes when there are some, and the labels:

```gotemplate
**{{.data.package}} {{.data.version}}**{{with .data.published_at}} published {{date "2 Jan 2006" .}}{{end}}
{{range $k, $v := .data.watch.labels}}`{{$k}}={{$v}}` {{end}}
{{with .data.notes}}{{truncate 500 .body}}{{else}}{{with .data.urls.changelog}}Changelog: {{.}}{{end}}{{end}}
```
