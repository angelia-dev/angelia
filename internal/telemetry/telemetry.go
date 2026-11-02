package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// TracerName is the instrumentation scope of every span Angelia starts.
const TracerName = "github.com/angelia-dev/angelia"

// ServiceName is the service.name used unless OTEL_SERVICE_NAME or
// OTEL_RESOURCE_ATTRIBUTES set one.
const ServiceName = "angelia"

// shutdownTimeout bounds the final flush of buffered spans.
const shutdownTimeout = 5 * time.Second

// Enabled reports whether env asks for an OTLP trace exporter: the SDK is not
// disabled (OTEL_SDK_DISABLED), the exporter list (OTEL_TRACES_EXPORTER) is
// empty or names otlp, and an endpoint is set (OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT). Without an endpoint the answer is no:
// the SDK default of localhost:4318 would make every plain install try to
// export.
func Enabled(getenv func(string) string) bool {
	if strings.EqualFold(strings.TrimSpace(getenv("OTEL_SDK_DISABLED")), "true") {
		return false
	}
	if exp := strings.TrimSpace(getenv("OTEL_TRACES_EXPORTER")); exp != "" && exp != "otlp" {
		return false
	}
	return strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) != "" ||
		strings.TrimSpace(getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")) != ""
}

// protocol returns the OTLP protocol env asks for, "http/protobuf" by default.
func protocol(getenv func(string) string) string {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v
		}
	}
	return "http/protobuf"
}

// Setup installs a global tracer provider that exports over OTLP/HTTP when
// [Enabled] says so, and returns the function that flushes and stops it. When
// tracing is off it installs nothing and the returned function does nothing.
//
// The exporter, sampler and resource read the standard variables themselves:
// OTEL_EXPORTER_OTLP_[TRACES_]ENDPOINT, _HEADERS, _TIMEOUT, _COMPRESSION,
// _CERTIFICATE, _INSECURE, OTEL_TRACES_SAMPLER[_ARG], OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES. Only http/protobuf is supported; asking for grpc
// is an error rather than a silent fallback. version is the service.version
// resource attribute.
func Setup(ctx context.Context, getenv func(string) string, version string) (shutdown func(context.Context) error, err error) {
	off := func(context.Context) error { return nil }
	if getenv == nil {
		getenv = os.Getenv
	}
	if !Enabled(getenv) {
		return off, nil
	}
	if p := protocol(getenv); p != "http/protobuf" {
		return off, fmt.Errorf("telemetry: OTLP protocol %q is not supported, only http/protobuf", p)
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return off, fmt.Errorf("telemetry: create OTLP exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", ServiceName), attribute.String("service.version", version)),
		resource.WithFromEnv(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return off, fmt.Errorf("telemetry: resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		otel.SetTracerProvider(noop.NewTracerProvider())
		return tp.Shutdown(ctx)
	}, nil
}

// Start starts a span named name as a child of the span in ctx, if any.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(TracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartRoot starts a span that begins a new trace even if ctx carries a span,
// with optional links to other spans.
func StartRoot(ctx context.Context, name string, links []trace.Link, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return otel.Tracer(TracerName).Start(ctx, name,
		trace.WithNewRoot(), trace.WithLinks(links...), trace.WithAttributes(attrs...))
}

// RecordError marks span as failed. It records the Go type of err, never its
// text, because the text of a transport or sink error can quote a URL, a
// header or a token. msg is shown as the status description; pass only text
// that is safe to export (a redacted message, or a fixed phrase).
func RecordError(span trace.Span, err error, msg string) {
	if err == nil {
		return
	}
	span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
	span.SetStatus(codes.Error, msg)
}

// TraceParent returns the W3C traceparent of the span in ctx, or "" when
// there is none or it is not sampled-in/valid.
func TraceParent(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(trace.ContextWithSpanContext(ctx, sc), carrier)
	return carrier.Get("traceparent")
}

// LinkFromTraceParent returns a link to the span a traceparent names, or nil
// when tp is empty or malformed.
func LinkFromTraceParent(ctx context.Context, tp string) []trace.Link {
	if tp == "" {
		return nil
	}
	ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": tp})
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []trace.Link{{SpanContext: sc}}
}
