package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
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

// FlushTimeout bounds the final flush of buffered spans at shutdown. `angelia
// serve` takes it from the drain budget when tracing is on.
const FlushTimeout = 2 * time.Second

// errorLogInterval is the least time between two "export failed" log lines.
const errorLogInterval = 30 * time.Second

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

// Setup builds a tracer provider that exports over OTLP/HTTP when [Enabled]
// says so, and returns it with the function that flushes and stops it. When
// tracing is off it returns a no-op provider and a shutdown that does nothing.
// The provider is not installed globally: the roles take it in their Options
// and spans below theirs inherit it through the context.
//
// The one process-global piece is OpenTelemetry's error handler (the SDK has
// no per-provider alternative). When tracing is on, Setup installs one that
// logs export failures through log at Warn, at most once per 30 s with the
// number of errors dropped meanwhile. Call Setup once, from the command.
//
// The exporter, sampler and resource read the standard variables themselves:
// OTEL_EXPORTER_OTLP_[TRACES_]ENDPOINT, _HEADERS, _TIMEOUT, _COMPRESSION,
// _CERTIFICATE, _INSECURE, OTEL_TRACES_SAMPLER[_ARG], OTEL_SERVICE_NAME and
// OTEL_RESOURCE_ATTRIBUTES. Only http/protobuf is supported; asking for grpc
// is an error rather than a silent fallback. version is the service.version
// resource attribute.
func Setup(ctx context.Context, getenv func(string) string, version string, log *slog.Logger) (tp trace.TracerProvider, shutdown func(context.Context) error, err error) {
	off := func(context.Context) error { return nil }
	if getenv == nil {
		getenv = os.Getenv
	}
	if !Enabled(getenv) {
		return noop.NewTracerProvider(), off, nil
	}
	if p := protocol(getenv); p != "http/protobuf" {
		return noop.NewTracerProvider(), off, fmt.Errorf("telemetry: OTLP protocol %q is not supported, only http/protobuf", p)
	}
	exp, err := otlptracehttp.New(ctx)
	if err != nil {
		return noop.NewTracerProvider(), off, fmt.Errorf("telemetry: create OTLP exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", ServiceName), attribute.String("service.version", version)),
		resource.WithFromEnv(),
	)
	if err != nil && !errors.Is(err, resource.ErrPartialResource) {
		return noop.NewTracerProvider(), off, fmt.Errorf("telemetry: resource: %w", err)
	}
	sdk := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	if log != nil {
		otel.SetErrorHandler(newErrorHandler(log, errorLogInterval, time.Now))
	}
	return sdk, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), FlushTimeout)
		defer cancel()
		return sdk.Shutdown(ctx)
	}, nil
}

// errorHandler logs OpenTelemetry errors (a collector that is down, a full
// queue) as slog warnings, at most once per interval.
type errorHandler struct {
	log      *slog.Logger
	interval time.Duration
	now      func() time.Time

	mu      sync.Mutex
	last    time.Time
	dropped int
}

func newErrorHandler(log *slog.Logger, interval time.Duration, now func() time.Time) *errorHandler {
	return &errorHandler{log: log, interval: interval, now: now}
}

// Handle implements [otel.ErrorHandler].
func (h *errorHandler) Handle(err error) {
	h.mu.Lock()
	t := h.now()
	if !h.last.IsZero() && t.Sub(h.last) < h.interval {
		h.dropped++
		h.mu.Unlock()
		return
	}
	dropped := h.dropped
	h.last, h.dropped = t, 0
	h.mu.Unlock()
	h.log.Warn("telemetry export failed", "error", err, "dropped", dropped)
}

// Start starts a span named name as a child of the span in ctx. It uses the
// tracer provider of that span, so it is a no-op when ctx carries none.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return trace.SpanFromContext(ctx).TracerProvider().Tracer(TracerName).Start(ctx, name, trace.WithAttributes(attrs...))
}

// StartRoot starts a span from tp that begins a new trace even if ctx carries
// a span, with optional links to other spans. A nil tp records nothing.
func StartRoot(ctx context.Context, tp trace.TracerProvider, name string, links []trace.Link, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	return tp.Tracer(TracerName).Start(ctx, name,
		trace.WithNewRoot(), trace.WithLinks(links...), trace.WithAttributes(attrs...))
}

// RecordError marks span as failed. It records a coarse class of err as
// error.type, never its text, because the text of a transport or sink error
// can quote a URL, a header or a token: "timeout", "canceled", or the Go type.
// msg is shown as the status description; pass only text that is safe to
// export (a redacted message, or a fixed phrase).
func RecordError(span trace.Span, err error, msg string) {
	RecordErrorAs(span, err, "", msg)
}

// RecordErrorAs is RecordError with a caller-chosen class, for errors only the
// caller can recognise (a permanent sink error). An empty class falls back to
// the generic one.
func RecordErrorAs(span trace.Span, err error, class, msg string) {
	if err == nil {
		return
	}
	if class == "" {
		class = errorClass(err)
	}
	span.SetAttributes(attribute.String("error.type", class))
	span.SetStatus(codes.Error, msg)
}

func errorClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	}
	return fmt.Sprintf("%T", err)
}

// TraceParent returns the W3C traceparent of the span in ctx, or "" when
// there is none.
func TraceParent(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)
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
