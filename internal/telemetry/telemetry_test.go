package telemetry_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	tp, stop, err := telemetry.Setup(t.Context(), env(nil), "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := stop(t.Context()); err != nil {
		t.Error(err)
	}
	_, span := telemetry.StartRoot(t.Context(), tp, "x", nil)
	if span.SpanContext().IsValid() {
		t.Error("spans are recorded while tracing is off")
	}
	span.End()
}

func TestSetupRejectsGRPC(t *testing.T) {
	_, _, err := telemetry.Setup(t.Context(), env(map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc",
	}), "dev", nil)
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

	tp, stop, err := telemetry.Setup(t.Context(), nil, "v1.2.3", slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := tp.(*sdktrace.TracerProvider); !ok {
		t.Fatalf("tracer provider is %T, want the SDK's", tp)
	}
	_, span := telemetry.StartRoot(t.Context(), tp, "angelia.check", nil)
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
	t.Parallel()
	_, tp := telemetrytest.Install(t)
	ctx, span := telemetry.StartRoot(t.Context(), tp, "check", nil)
	defer span.End()
	parent := telemetry.TraceParent(ctx)
	if parent == "" {
		t.Fatal("no traceparent for a live span")
	}
	links := telemetry.LinkFromTraceParent(t.Context(), parent)
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
	t.Parallel()
	rec, tp := telemetrytest.Install(t)
	_, span := telemetry.StartRoot(t.Context(), tp, "x", nil)
	telemetry.RecordError(span, errors.New("Get https://h/hook/SECRET: boom"), "failed")
	span.End()
	dump := telemetrytest.Dump(rec.Ended())
	if strings.Contains(dump, "SECRET") || !strings.Contains(dump, "*errors.errorString") {
		t.Errorf("span export: %s", dump)
	}
}

func TestLogHandlerAddsTraceIDs(t *testing.T) {
	t.Parallel()
	_, tp := telemetrytest.Install(t)
	var buf bytes.Buffer
	log := slog.New(telemetry.NewLogHandler(slog.NewJSONHandler(&buf, nil))).With("k", "v")
	ctx, span := telemetry.StartRoot(t.Context(), tp, "x", nil)
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

func TestErrorClass(t *testing.T) {
	t.Parallel()
	rec, tp := telemetrytest.Install(t)
	for _, err := range []error{
		fmt.Errorf("get https://h/SECRET: %w", context.DeadlineExceeded),
		fmt.Errorf("get https://h/SECRET: %w", context.Canceled),
		errors.New("SECRET"),
	} {
		_, span := telemetry.StartRoot(t.Context(), tp, "x", nil)
		telemetry.RecordError(span, err, "failed")
		span.End()
	}
	_, span := telemetry.StartRoot(t.Context(), tp, "x", nil)
	telemetry.RecordErrorAs(span, errors.New("SECRET"), "permanent", "failed")
	span.End()
	var got []string
	for _, s := range rec.Ended() {
		for _, a := range s.Attributes() {
			got = append(got, a.Value.AsString())
		}
	}
	want := []string{"timeout", "canceled", "*errors.errorString", "permanent"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("error.type = %v, want %v", got, want)
	}
}

// OpenTelemetry errors (an unreachable collector) become slog warnings, at
// most one per interval with a count of the ones dropped.
func TestErrorHandlerRateLimits(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	h := telemetry.NewErrorHandlerForTest(slog.New(slog.NewJSONHandler(&buf, nil)), 30*time.Second, func() time.Time { return now })
	h.Handle(errors.New("first"))
	now = now.Add(time.Second)
	h.Handle(errors.New("second"))
	h.Handle(errors.New("third"))
	now = now.Add(31 * time.Second)
	h.Handle(errors.New("fourth"))
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d log lines, want 2:\n%s", len(lines), buf.String())
	}
	for i, want := range []string{`"error":"first"`, `"error":"fourth"`} {
		if !strings.Contains(lines[i], want) || !strings.Contains(lines[i], `"msg":"telemetry export failed"`) || !strings.Contains(lines[i], `"level":"WARN"`) {
			t.Errorf("line %d = %s, want %s", i, lines[i], want)
		}
	}
	if !strings.Contains(lines[0], `"dropped":0`) || !strings.Contains(lines[1], `"dropped":2`) {
		t.Errorf("dropped counts: %s", buf.String())
	}
}
