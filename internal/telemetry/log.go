package telemetry

import (
	"context"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

// logHandler adds trace_id and span_id to every record whose context carries
// a valid span, so a log line can be found from its trace and the other way
// round. Use the *Context logging methods to pass the span's context.
type logHandler struct{ slog.Handler }

// NewLogHandler wraps h so that records logged with a context that carries a
// span get trace_id and span_id attributes. Other records pass through.
func NewLogHandler(h slog.Handler) slog.Handler { return logHandler{h} }

// Handle implements [slog.Handler].
func (l logHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return l.Handler.Handle(ctx, r)
}

// WithAttrs implements [slog.Handler].
func (l logHandler) WithAttrs(a []slog.Attr) slog.Handler { return logHandler{l.Handler.WithAttrs(a)} }

// WithGroup implements [slog.Handler].
func (l logHandler) WithGroup(g string) slog.Handler { return logHandler{l.Handler.WithGroup(g)} }
