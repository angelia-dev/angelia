package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/angelia-dev/angelia/internal/telemetry"
	"github.com/angelia-dev/angelia/internal/telemetry/telemetrytest"
)

func env(kv map[string]string) func(string) string { return func(k string) string { return kv[k] } }

// AC: the OTLP exporter is off by default and enabled by standard OTEL_*
// variables.
func TestEnabled(t *testing.T) {
	const ep = "OTEL_EXPORTER_OTLP_ENDPOINT"
	for name, tc := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"nothing set is off":               {nil, false},
		"endpoint enables":                 {map[string]string{ep: "http://collector:4318"}, true},
		"traces endpoint enables":          {map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://collector:4318/v1/traces"}, true},
		"blank endpoint is off":            {map[string]string{ep: "  "}, false},
		"exporter otlp with endpoint":      {map[string]string{"OTEL_TRACES_EXPORTER": "otlp", ep: "http://c:4318"}, true},
		"exporter none wins":               {map[string]string{"OTEL_TRACES_EXPORTER": "none", ep: "http://c:4318"}, false},
		"exporter console is not ours":     {map[string]string{"OTEL_TRACES_EXPORTER": "console", ep: "http://c:4318"}, false},
		"sdk disabled wins":                {map[string]string{"OTEL_SDK_DISABLED": "true", ep: "http://c:4318"}, false},
		"sdk disabled false does not":      {map[string]string{"OTEL_SDK_DISABLED": "false", ep: "http://c:4318"}, true},
		"exporter without endpoint is off": {map[string]string{"OTEL_TRACES_EXPORTER": "otlp"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := telemetry.Enabled(env(tc.env)); got != tc.want {
				t.Errorf("Enabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSetupOffInstallsNothing(t *testing.T) {
	before := otel.GetTracerProvider()
	stop, err := telemetry.Setup(t.Context(), env(nil), "dev")
	if err != nil {
		t.Fatal(err)
	}
	if otel.GetTracerProvider() != before {
		t.Error("Setup without OTEL_* variables replaced the tracer provider")
	}
	if err := stop(t.Context()); err != nil {
		t.Error(err)
	}
	_, span := telemetry.Start(t.Context(), "x")
	if span.SpanContext().IsValid() {
		t.Error("spans are recorded while tracing is off")
	}
	span.End()
}

func TestSetupRejectsGRPC(t *testing.T) {
	_, err := telemetry.Setup(t.Context(), env(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
	}), "dev")
	if err == nil || !strings.Contains(err.Error(), "grpc") {
		t.Fatalf("grpc protocol: %v, want an error naming it", err)
	}
}

// AC: with the standard variables set, spans reach the collector over
// OTLP/HTTP.
func TestSetupExportsOverOTLPHTTP(t *testing.T) {
	var gotPath, gotHeader atomic.Value
	var size atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r.Body)
		gotPath.Store(r.URL.Path)
		gotHeader.Store(r.Header.Get("X-Test"))
		size.Store(int64(b.Len()))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "x-test=yes")
	t.Setenv("OTEL_SERVICE_NAME", "angelia-test")

	stop, err := telemetry.Setup(t.Context(), nil, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("tracer provider is %T, want the SDK's", otel.GetTracerProvider())
	}
	_, span := telemetry.Start(t.Context(), "angelia.check")
	span.End()
	if err := stop(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if p, _ := gotPath.Load().(string); p != "/v1/traces" {
		t.Errorf("collector got path %q, want /v1/traces", p)
	}
	if h, _ := gotHeader.Load().(string); h != "yes" {
		t.Errorf("OTEL_EXPORTER_OTLP_HEADERS not honoured: header %q", h)
	}
	if size.Load() == 0 {
		t.Error("empty export")
	}
}

func TestTraceParentRoundTrip(t *testing.T) {
	telemetrytest.Install(t)
	ctx, span := telemetry.Start(t.Context(), "check")
	defer span.End()
	tp := telemetry.TraceParent(ctx)
	if tp == "" {
		t.Fatal("no traceparent for a live span")
	}
	links := telemetry.LinkFromTraceParent(t.Context(), tp)
	if len(links) != 1 || links[0].SpanContext.TraceID() != span.SpanContext().TraceID() ||
		links[0].SpanContext.SpanID() != span.SpanContext().SpanID() {
		t.Fatalf("link %v does not name the span %v", links, span.SpanContext())
	}
	for _, bad := range []string{"", "garbage", "00-xyz-abc-01"} {
		if l := telemetry.LinkFromTraceParent(t.Context(), bad); l != nil {
			t.Errorf("LinkFromTraceParent(%q) = %v, want nil", bad, l)
		}
	}
	if tp := telemetry.TraceParent(t.Context()); tp != "" {
		t.Errorf("traceparent without a span: %q", tp)
	}
}

func TestRecordErrorKeepsOnlyTheType(t *testing.T) {
	rec := telemetrytest.Install(t)
	_, span := telemetry.Start(t.Context(), "x")
	telemetry.RecordError(span, errors.New("Get https://h/hook/SECRET: boom"), "failed")
	span.End()
	dump := telemetrytest.Dump(rec.Ended())
	if strings.Contains(dump, "SECRET") || !strings.Contains(dump, "*errors.errorString") {
		t.Errorf("span export: %s", dump)
	}
}

func TestLogHandlerAddsTraceIDs(t *testing.T) {
	telemetrytest.Install(t)
	var buf bytes.Buffer
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil))).With("k", "v")
	ctx, span := telemetry.Start(t.Context(), "x")
	log.InfoContext(ctx, "inside")
	span.End()
	log.InfoContext(t.Context(), "outside")
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if !strings.Contains(lines[0], `"trace_id":"`+span.SpanContext().TraceID().String()+`"`) ||
		!strings.Contains(lines[0], `"span_id":"`+span.SpanContext().SpanID().String()+`"`) {
		t.Errorf("line inside a span: %s", lines[0])
	}
	if strings.Contains(lines[1], "trace_id") {
		t.Errorf("line outside a span has a trace id: %s", lines[1])
	}
}
