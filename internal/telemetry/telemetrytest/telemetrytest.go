// Package telemetrytest records spans in memory for tests.
package telemetrytest

import (
	"fmt"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
)

// Install makes the global tracer provider record every span in memory until
// the test ends. Tests that call it must not run in parallel.
func Install(t testing.TB) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })
	return rec
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
