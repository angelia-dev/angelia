// Package telemetrytest records spans in memory for tests.
package telemetrytest

import (
	"fmt"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

// Install returns a tracer provider that records every span in memory, and
// the recorder. Pass the provider to the role under test; nothing is global,
// so tests may run in parallel.
func Install(t testing.TB) (*tracetest.SpanRecorder, trace.TracerProvider) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	return rec, sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
}

// Dump renders everything a span exports (name, attributes, events, links,
// status) as one string, for asserting that a secret is absent.
func Dump(spans []sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	for _, s := range spans {
		fmt.Fprintf(&b, "%s %v %v %v %v\n", s.Name(), s.Attributes(), s.Events(), s.Links(), s.Status())
	}
	return b.String()
}
