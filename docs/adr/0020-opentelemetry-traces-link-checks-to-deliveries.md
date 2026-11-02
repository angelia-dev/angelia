# ADR 0020: Traces are OpenTelemetry over OTLP/HTTP, enabled only by `OTEL_*` variables; a check is one trace and each delivery attempt a linked trace

- **Status:** Proposed
- **Date:** 2026-11-02
- **Story:** angelia-dev/angelia#118

ADR 0019 is reserved for #117 (Prometheus metrics).

## Context

[architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions) lists "OpenTelemetry traces" and
[prd.md §7](../prd.md#7-non-functional-requirements) wants OTel traces and structured logs. Issue #118: trace
check, normalize, event and delivery; correlate `slog` lines with trace ids; OTLP exporter off by default and
enabled by the standard `OTEL_*` variables; one trace per check, linked to delivery spans; no secrets in
span attributes.

A delivery is sent by the dispatcher, a separate role that may run in another process and, for a retry or
a held delivery, hours after the check. It cannot be a child of the check span in time, and there is no
request to carry a header on. The only thing that connects the two is the `delivery` row.

## Decision

1. **Modules.** The official OpenTelemetry Go modules, all Apache-2.0: `go.opentelemetry.io/otel`,
   `.../otel/trace`, `.../otel/sdk` and `.../otel/exporters/otlp/otlptrace/otlptracehttp`. No contrib
   instrumentation (`otelhttp`, `otelslog`): the few spans we need are written by hand in
   `internal/telemetry`, which keeps URLs and headers out of them. The exporter pulls `google.golang.org/grpc`,
   `grpc-gateway`, `protobuf` and the OTLP protos as transitive modules (grpc was already a spike-only
   dependency, [ADR 0013](0013-go-plugin-subprocess-prototype.md)); nothing under `internal/` or `cmd/`
   imports grpc.
2. **Off by default.** `telemetry.Setup` installs a tracer provider only when `OTEL_SDK_DISABLED` is not
   `true`, `OTEL_TRACES_EXPORTER` is empty or `otlp`, and `OTEL_EXPORTER_OTLP_ENDPOINT` or
   `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is set. Without an endpoint nothing is exported (the SDK's
   `localhost:4318` default would make every install try). Exporter, sampler and resource read the other
   standard variables themselves. Only `http/protobuf` is supported; `grpc` is an error at start. There is
   no `angelia.yaml` key, so the YAML contract is unchanged. The OTLP endpoint is operator-configured and is
   not subject to the SSRF guard, like the database DSN.
3. **Without a provider, spans are no-ops.** Code starts spans through `telemetry.Start`/`StartRoot`, which use
   the OpenTelemetry global provider (a no-op until `Setup` runs). The global is the library's registry,
   not Angelia state.
4. **One trace per check.** The scheduler starts `angelia.check` as a new root for every check. Below it:
   `angelia.http` for each request through the netguard client and `angelia.commit` for `event.Commit`
   (diff, policy and emit run inside it). Attributes: target id, source kind, counts of events and
   deliveries, error counts.
5. **A delivery attempt is its own trace, linked to the check.** The dispatcher starts `angelia.delivery`
   as a new root with a span link to the span that created the delivery row (the commit span, in the
   check's trace). The link is carried in a new nullable column `delivery.traceparent` (migration 00010,
   additive, W3C `traceparent` format), written by `Emit` and `EmitYanks` from the context and read when
   the delivery is leased. A trace is therefore not stretched over hours of backoff, and the link works
   across processes. Retries are separate spans linked to the same check.
6. **No secrets in spans.** Spans carry ids, kinds, counts, the HTTP method, the host (`server.address`)
   and status codes. They never carry a URL, query, header, sink configuration, event payload or error
   text: `telemetry.RecordError` keeps the Go type of the error as `error.type` and a fixed status
   description. A test runs the scheduler, `event.Commit` and the dispatcher end to end with secrets in a
   sink URL, a sink config and error messages and asserts none reaches any span, event, link or status.
7. **Log correlation.** `telemetry.NewLogHandler` wraps the `slog` handler built by `angelia serve`; a
   record logged with a context that carries a span (the `*Context` methods) gets `trace_id` and
   `span_id`.

## Consequences

### Positive

- Tracing costs nothing when off and needs no Angelia configuration when on.
- From a log line or a failed delivery, an operator can open the check trace that caused it.

### Negative

- New dependency tree (OTel SDK, OTLP/HTTP exporter, protobuf, grpc-gateway), roughly 15 modules.
- One more column on `delivery`; the Postgres engine (#85) must add it too.
- Delivery traces are separate traces; a UI that wants one waterfall must follow links.

### Open questions

- Only the netguard client and the check/commit/delivery steps are instrumented. Per-source spans and
  sink-specific attributes can be added when more sources and sinks exist.
- `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` could be supported later by adding `otlptracegrpc`.
- Log lines emitted without a context (`log.Info`) carry no trace ids; some existing call sites in the
  scheduler and dispatcher use the plain methods and could move to the `*Context` ones.

## Alternatives considered

- **Parent the delivery span on the check span.** Needs the same persisted context, and gives traces that
  last as long as the longest retry. Links keep each trace short.
- **Correlate by event id only (no migration).** Cheap, but not a link: a collector cannot join the two
  traces by an attribute.
- **Put `traceparent` in the CloudEvents envelope** (the distributed tracing extension). It would also
  reach consumers, but changes the public event contract; not done here.
- **`otelhttp` and `otelslog`.** Fewer lines of ours, but `otelhttp` records the full URL by default, which
  can contain secrets; more modules.

## References

- [architecture.md §6](../architecture.md#6-language-and-framework-phase-0-assumptions), [prd.md §7](../prd.md#7-non-functional-requirements)
- [ADR 0015](0015-release-event-outbox-in-house.md) (outbox), [ADR 0017](0017-dispatcher-delivery-leases-and-retries.md) (dispatcher)
- OpenTelemetry: [OTLP exporter configuration](https://opentelemetry.io/docs/specs/otel/protocol/exporter/), [SDK environment variables](https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/)
