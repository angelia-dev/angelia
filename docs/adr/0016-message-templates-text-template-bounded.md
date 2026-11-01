# ADR 0016: Message templates are Go text/template over the event JSON, with a curated function set, an instrumented deadline and a size cap, checked against sample events at load time

- **Status:** Proposed
- **Date:** 2026-11-01
- **Story:** angelia-dev/angelia#114

## Context

A subscription's `template` (watch file §2.5, column `subscription.template`) shapes the body its sink
delivers. [architecture.md §3.5](../architecture.md#35-dispatcher-and-sink-plugins-deliver) says
"Go `text/template` (or CEL) for message bodies. Default payload = CloudEvents JSON", and §4 already tells
template authors to write `.data.source`. The watch file story only checked that a template parses and left
"the available fields" to this story. The issue asks for validation at configuration time with a sample
event, a timeout and an output cap, a curated function map without environment or file access, and the
CloudEvents JSON body when no template is set.

Templates come from whoever controls the configuration, which is trusted, but they run inside the
dispatcher on every delivery: a careless or hostile template must not hang a dispatcher or exhaust its
memory. `text/template` has no cancellation and no output limit. Its loops (`range` over an integer, over
data, nested) can run for hours without writing a byte, and a variable reassigned in a loop
(`{{$x = printf "%s%s" $x $x}}`) doubles in size on each iteration.

## Decision

1. **Engine: `text/template`** from the standard library (no new dependency). CEL was not chosen: it is an
   expression language (good for filters, poor for producing a multi-line body) and would be a new
   dependency outside §6.
2. **Data: the event's JSON object.** The dot is `event.payload_json` decoded into `map[string]any`, so a
   template uses the spec's wire names (`.type`, `.subject`, `.data.package`, `.data.watch.labels.team`), as
   architecture.md §4 already shows. Nothing new is named: the template data model *is* the event contract,
   and additive spec fields appear without code changes. `missingkey=error`: a misspelt key fails instead of
   printing `<no value>`.
3. **Functions** (`render.FuncNames`, [templates.md](../templates.md#functions)): `default`, `upper`, `lower`,
   `trim`, `trimPrefix`, `trimSuffix`, `contains`, `hasPrefix`, `hasSuffix`, `replace`, `split`, `join`,
   `truncate`, `toJSON`, `date`, plus the builtins. No environment, file, network or clock access (no `now`,
   so a retry renders the same bytes). The builtins that build strings (`print`, `printf`, `println`, `html`,
   `js`, `urlquery`) are replaced by size-checked versions; `call` is disabled. String functions take their
   subject last (pipeline order) and treat JSON null as `""`.
4. **Not allowed:** `define`, `block` and `template`. A message template does not need them, and recursive
   invocation is the only loop that would escape point 5.
5. **Timeout: 1 s, enforced inside the loops.** `Compile` walks the parse tree and prepends a call to an
   internal function to every `range` body; at execution that function fails once the deadline (or the
   context's deadline or cancellation) has passed, which stops `text/template`. The output writer checks it
   too. With point 4 every iteration of every loop checks the deadline, so no goroutine is left running past
   it.
6. **Size caps: 256 KiB of output, 32 KiB of template.** The output writer fails past the cap, and every
   function that builds a string checks the size *before* building it (for `printf`: format length, explicit
   widths and precisions, and the largest argument once per verb; `*` widths are refused). So no value, not
   only the output, can grow past the cap.
7. **Load-time check against two sample events** (`render.Samples`, `Template.Check`, no I/O and bounded by the timeout, so it takes no context): **full** (every
   nullable field set) and **sparse** (every nullable field null, no labels). A template must render both
   within the limits. This rejects unguarded null fields (`{{.data.notes.body}}`, which every event fails
   today) before the first delivery, at the cost of requiring `with`/`if` guards.
8. **Errors never quote the template text**, which may hold a credential the configuration's prefix check
   missed: `render.Error` keeps the line, column and `text/template`'s reason, and drops the
   `at <…>` fragment. The watch file reports them as
   `subscriptions[0].template: on the sparse sample event: line 1, column 7: nil pointer evaluating …`.
9. **Output.** `render.Render(ctx, nil, envelope)` returns the stored envelope unchanged, content type
   `application/cloudevents+json; charset=utf-8`. A template's output is `application/json; charset=utf-8`
   when it is valid JSON, `text/plain; charset=utf-8` otherwise; `Rendered.Templated` tells sinks which case
   they have. The package is `internal/render`; the dispatcher (#107) compiles each subscription's template
   and passes the `Rendered` to the sink.

## Consequences

### Positive

- Template authors learn one vocabulary, the spec's; templates are reproducible across retries.
- A dispatcher cannot be hung or blown up by a template; failures are ordinary delivery errors.
- Bad templates fail at `apply`, with a position, not at 3 a.m. on the first matching release.

### Negative

- The instrumentation relies on `text/template/parse` node types (public, stable API) and adds one function
  call per loop iteration.
- `define`/`template` are gone; reusable snippets need a later design if anyone asks.
- The sparse sample makes guards mandatory even for fields a given watch always fills.
- Each render clones the compiled template (to bind its deadline), a small allocation per delivery.

### Open questions

- Limits are constants, not configuration keys (a public contract change); revisit if a sink needs more.
- Whether a sink may override the sniffed content type is left to each sink story.
- The template's `json.Unmarshal` uses `float64` for numbers; the v1 envelope has none.

## Alternatives considered

- **CEL** (cel-go). Bounded by design (cost limits), but an expression language, a new dependency, and the
  issue's "Human needed" says to ask before choosing it. Not chosen.
- **A goroutine with a timer, abandoned on timeout.** The goroutine keeps burning CPU until the loop ends,
  which can be never. Not chosen.
- **Static limits only** (reject large integer literals, limit range nesting). Data-driven loops
  (`split` of a 16 KiB notes body, nested) still explode; not enough on its own.
- **A Go struct as the data** (`.Package`, `.Version`). Invents a second naming of the contract that must
  be kept in sync with the spec; architecture.md §4 already documents `.data.source`. Not chosen.
- **Sprig.** Large function set including `env` and `expandenv`, would need curation anyway, and is a new
  dependency. Not chosen.

## References

- [templates.md](../templates.md), [watch-file.md §2.5](../watch-file.md#25-subscriptions)
- [ADR 0015](0015-release-event-outbox-in-house.md) (the stored envelope), [ADR 0004](0004-cloudevents-purl-event-contract.md)
- angelia-dev/spec `docs/release-event-v1.md`
