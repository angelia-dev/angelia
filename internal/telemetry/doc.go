// Package telemetry is Angelia's OpenTelemetry tracing (story #118, ADR 0020).
//
// Tracing is off by default. [Setup] turns it on only when the standard
// OpenTelemetry environment variables ask for an OTLP exporter, and returns a
// shutdown function that flushes the spans. The rest of the code starts spans
// through [Start], which is a no-op until a provider is installed, so
// instrumented packages need no configuration and no nil checks.
//
// One check is one trace: the scheduler starts the root span, and fetch,
// commit and emit are its children. A delivery happens later and maybe in
// another process, so it is a trace of its own that carries a span link to the
// check; [TraceParent] and [LinkFromTraceParent] carry the context through
// the delivery row.
//
// Span attributes are ids, kinds, counts and status codes. They never hold a
// URL, a header, a sink configuration or an error message that might quote
// one; [RecordError] keeps only the type of an error unless the caller passes
// an already redacted message.
package telemetry
